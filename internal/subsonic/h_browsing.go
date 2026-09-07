package subsonic

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"lxsc/internal/music"
)

func (s *Server) getMusicFolders(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "musicFolders", M{"musicFolder": []M{{"id": 1, "name": "在线音乐"}}})
}

func (s *Server) getGenres(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	infos := s.libraryTracks(rc, currentUser(r).ID)
	counts := map[string]int{}
	albums := map[string]map[string]bool{}
	for _, in := range infos {
		g := music.PlatformName(in.Source())
		counts[g]++
		if albums[g] == nil {
			albums[g] = map[string]bool{}
		}
		albums[g][in.AlbumSubID()] = true
	}
	var out []M
	for _, p := range s.Settings.Get().SearchSources {
		name := music.PlatformName(p)
		out = append(out, M{"value": name, "songCount": counts[name], "albumCount": len(albums[name])})
	}
	writeOK(w, r, "genres", M{"genre": out})
}

// getIndexes / getArtists：资料库中的歌手
func (s *Server) getIndexes(w http.ResponseWriter, r *http.Request) {
	s.artistsIndex(w, r, "indexes")
}

func (s *Server) getArtists(w http.ResponseWriter, r *http.Request) {
	s.artistsIndex(w, r, "artists")
}

func (s *Server) artistsIndex(w http.ResponseWriter, r *http.Request, name string) {
	rc := s.newReqCtx(r)
	infos := s.libraryTracks(rc, currentUser(r).ID)
	groups := groupArtists(infos)
	// 也包含直接收藏的歌手
	for id := range rc.starred {
		if strings.HasPrefix(id, music.KindArtist+"-") {
			found := false
			for _, g := range groups {
				if g.ID == id {
					found = true
					break
				}
			}
			if !found {
				if _, n, _, err := s.DB.GetArtist(rc.ctx, id); err == nil {
					groups = append(groups, &artistGroup{ID: id, Name: n, Albums: map[string]bool{}})
				}
			}
		}
	}
	sortByName(groups)
	byLetter := map[string][]M{}
	var letters []string
	for _, g := range groups {
		l := indexLetter(g.Name)
		if _, ok := byLetter[l]; !ok {
			letters = append(letters, l)
		}
		obj := s.artistObj(rc, g)
		if name == "indexes" {
			obj = M{"id": g.ID, "name": g.Name, "coverArt": g.ID, "albumCount": len(g.Albums)}
			if st, ok := rc.starred[g.ID]; ok {
				obj["starred"] = fmtTime(st)
			}
		}
		byLetter[l] = append(byLetter[l], obj)
	}
	sort.Strings(letters)
	idx := make([]M, 0, len(letters))
	for _, l := range letters {
		idx = append(idx, M{"name": l, "artist": byLetter[l]})
	}
	writeOK(w, r, name, M{"index": idx, "lastModified": 0, "ignoredArticles": ""})
}

// getMusicDirectory 按层懒加载在线榜单与完整歌手目录；原生专辑和歌手 ID 继续保持兼容。
func (s *Server) getMusicDirectory(w http.ResponseWriter, r *http.Request) {
	id := param(r, "id")
	rc := s.newReqCtx(r)
	p, ok := music.ParseID(id)
	if id == "" || id == "1" {
		id = "1"
		p = music.ParsedID{Kind: music.KindDir, Source: "root"}
		ok = true
	}
	if !ok {
		writeErr(w, r, ErrNotFound, "directory not found")
		return
	}
	stage := p.Kind + ":" + p.Source
	w.Header().Set("X-LXSC-Directory-Stage", stage)
	if s.Log != nil {
		s.Log.Debug("在线目录请求", "id", id, "stage", stage)
	}

	switch p.Kind {
	case music.KindDir:
		display := newBoardVisibility(s.Settings.Get())
		if p.Source == "root" {
			children := []M{{"id": music.ArtistCategoryID("root"), "parent": "1", "isDir": true, "title": "歌手", "name": "歌手"}}
			for _, src := range display.sources {
				children = append(children, M{"id": "dir-" + src, "parent": "1", "isDir": true, "title": music.PlatformName(src) + "榜单", "name": music.PlatformName(src) + "榜单"})
			}
			writeOK(w, r, "directory", M{"id": "1", "name": "在线音乐", "child": children})
			return
		}
		if !display.includesSource(p.Source) {
			writeErr(w, r, ErrNotFound, "directory not found")
			return
		}
		boards, err := s.Catalog.Boards(rc.ctx, p.Source)
		if err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		children := make([]M, 0, len(boards))
		for _, board := range boards {
			if !display.allows(board) {
				continue
			}
			children = append(children, M{"id": music.BoardID(p.Source, board.BangID), "parent": id, "isDir": true, "title": board.Name, "name": board.Name})
		}
		writeOK(w, r, "directory", M{"id": id, "parent": "1", "name": music.PlatformName(p.Source) + "榜单", "child": children})
	case music.KindBoard:
		// 旧 lb ID 不受当前目录开关影响，直接深链仍可访问；不附带扫描榜单名称。
		name := p.Key
		infos, err := s.Catalog.BoardTracks(rc.ctx, p.Source, p.Key, 1)
		if err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		writeOK(w, r, "directory", M{"id": id, "parent": "dir-" + p.Source, "name": name, "child": songList(s, rc, infos)})
	case music.KindArtistDir:
		s.writeArtistCategoryDirectory(w, r, rc, p.Source)
	case music.KindArtistName:
		category, name, parsed := music.ParseArtistNameID(id)
		if !parsed {
			writeErr(w, r, ErrNotFound, "directory not found")
			return
		}
		platforms := s.Catalog.EnabledPlatforms()
		children := make([]M, 0, len(platforms))
		for _, source := range uniquePlatforms(platforms) {
			ref, _ := s.Catalog.KnownArtistRef(source, name)
			childID := music.SingerDirectoryID(source, name, ref.ID, id)
			children = append(children, M{"id": childID, "parent": id, "isDir": true, "title": music.PlatformName(source), "name": music.PlatformName(source)})
		}
		writeOK(w, r, "directory", M{"id": id, "parent": music.ArtistCategoryID(category), "name": name, "child": children})
	case music.KindSingerDir:
		ref, parsed := music.ParseSingerDirectoryID(id)
		if !parsed {
			writeErr(w, r, ErrNotFound, "directory not found")
			return
		}
		detail, err := s.Catalog.ArtistDetail(rc.ctx, music.ArtistRef{Source: ref.Source, ID: ref.ID, Name: ref.Name})
		if err != nil {
			if s.Log != nil {
				s.Log.Warn("平台歌手解析失败", "source", ref.Source, "artist", ref.Name, "err", err)
			}
			writeErr(w, r, ErrGeneric, "无法读取"+music.PlatformName(ref.Source)+"歌手目录: "+err.Error())
			return
		}
		// 歌手入口只暴露第一页；后续页由当前页按需串行展开，避免预先制造全部页元数据。
		pageID := music.ArtistAlbumPageID(detail.Source, detail.Name, detail.ID, 1, id)
		children := []M{{"id": pageID, "parent": id, "isDir": true, "title": artistPageTitle(1), "name": artistPageTitle(1)}}
		writeOK(w, r, "directory", M{"id": id, "parent": ref.Parent, "name": music.PlatformName(detail.Source) + " · " + detail.Name, "child": children})
	case music.KindArtistAlbumPage:
		page, parsed := music.ParseArtistAlbumPageID(id)
		if !parsed {
			writeErr(w, r, ErrNotFound, "directory not found")
			return
		}
		ref := music.ArtistRef{Source: page.Source, ID: page.ID, Name: page.Name}
		result, err := s.Catalog.ArtistAlbums(rc.ctx, ref, page.Page)
		if err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		singerParent := page.SingerParent
		if singerParent == "" {
			singerParent = music.SingerDirectoryID(page.Source, page.Name, page.ID)
		}
		parent := singerParent
		if page.Page > 1 {
			parent = music.ArtistAlbumPageID(page.Source, page.Name, page.ID, page.Page-1, singerParent)
		}
		children := make([]M, 0, len(result.List)+1)
		for _, album := range result.List {
			albumID := music.OnlineAlbumID(album.Source, album.ID, album.Name, album.Artist, album.Image, id)
			s.Catalog.CacheAlbumMetaForID(albumID, album)
			children = append(children, albumMetaObj(albumID, id, album))
		}
		if result.HasMore {
			nextID := music.ArtistAlbumPageID(page.Source, page.Name, page.ID, page.Page+1, singerParent)
			children = append(children, M{"id": nextID, "parent": id, "isDir": true, "title": artistPageTitle(page.Page + 1), "name": artistPageTitle(page.Page + 1)})
		}
		writeOK(w, r, "directory", M{"id": id, "parent": parent, "name": artistPageTitle(page.Page), "child": children})
	case music.KindOnlineAlbum:
		locator, parsed := music.ParseOnlineAlbumID(id)
		if !parsed {
			writeErr(w, r, ErrNotFound, "album not found")
			return
		}
		infos, meta, err := s.Catalog.AlbumSongsFor(rc.ctx, locator)
		if err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		name, artist := meta.Name, meta.Artist
		if name == "" {
			name = locator.Name
		}
		if artist == "" {
			artist = locator.Artist
		}
		parent := locator.Parent
		if parent == "" {
			parent = music.ArtistID(artist)
		}
		writeOK(w, r, "directory", M{"id": id, "parent": parent, "name": name, "child": songListWithAlbum(s, rc, infos, id)})
	case music.KindAlbum:
		g := s.albumByID(rc, id)
		if g == nil {
			writeErr(w, r, ErrNotFound, "album not found")
			return
		}
		writeOK(w, r, "directory", M{"id": id, "parent": music.ArtistID(g.Artist), "name": g.Name, "child": songList(s, rc, g.Songs)})
	case music.KindArtist:
		g := s.artistByID(rc, id)
		if g == nil {
			writeErr(w, r, ErrNotFound, "artist not found")
			return
		}
		var children []M
		for _, album := range groupAlbums(g.Songs) {
			obj := s.albumObj(rc, album)
			obj["title"] = album.Name
			children = append(children, obj)
		}
		writeOK(w, r, "directory", M{"id": id, "parent": "1", "name": g.Name, "child": children})
	default:
		writeErr(w, r, ErrNotFound, "directory not found")
	}
}

func uniquePlatforms(sources []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		if music.IsPlatform(source) && !seen[source] {
			seen[source] = true
			out = append(out, source)
		}
	}
	return out
}

func artistPageTitle(page int) string {
	return "专辑第 " + strconv.Itoa(page) + " 页（每页 50）"
}

func albumMetaObj(id, parent string, album music.AlbumMeta) M {
	return M{"id": id, "parent": parent, "isDir": true, "title": album.Name, "name": album.Name, "album": album.Name,
		"artist": album.Artist, "coverArt": id, "songCount": album.SongCount, "created": generatedMetadataTime,
		"genre": music.PlatformName(album.Source)}
}

func (s *Server) writeArtistCategoryDirectory(w http.ResponseWriter, r *http.Request, rc *reqCtx, category string) {
	if category == "root" {
		children := []M{
			{"id": music.ArtistCategoryID("seen"), "parent": music.ArtistCategoryID("root"), "isDir": true, "title": "已见歌手", "name": "已见歌手"},
			{"id": music.ArtistCategoryID("search"), "parent": music.ArtistCategoryID("root"), "isDir": true, "title": "搜索歌手", "name": "搜索歌手"},
			{"id": music.ArtistCategoryID("starred"), "parent": music.ArtistCategoryID("root"), "isDir": true, "title": "收藏歌手", "name": "收藏歌手"},
		}
		writeOK(w, r, "directory", M{"id": music.ArtistCategoryID("root"), "parent": "1", "name": "歌手", "child": children})
		return
	}
	var names []string
	switch category {
	case "seen":
		names = append(names, s.Catalog.SeenArtistNames()...)
		for _, group := range groupArtists(s.libraryTracks(rc, currentUser(r).ID)) {
			names = append(names, group.Name)
		}
	case "search":
		names = s.Catalog.SearchedArtistNames()
	case "starred":
		items, _ := s.DB.ListStarred(rc.ctx, currentUser(r).ID, "artist")
		for _, item := range items {
			if _, name, _, err := s.DB.GetArtist(rc.ctx, item.ItemID); err == nil && name != "" {
				names = append(names, name)
			}
		}
	default:
		writeErr(w, r, ErrNotFound, "directory not found")
		return
	}
	names = uniqueNames(names)
	children := make([]M, 0, len(names))
	for _, name := range names {
		childID := music.ArtistNameID(category, name)
		children = append(children, M{"id": childID, "parent": music.ArtistCategoryID(category), "isDir": true, "title": name, "name": name})
	}
	writeOK(w, r, "directory", M{"id": music.ArtistCategoryID(category), "parent": music.ArtistCategoryID("root"), "name": artistCategoryName(category), "child": children})
}

func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func artistCategoryName(category string) string {
	switch category {
	case "seen":
		return "已见歌手"
	case "search":
		return "搜索歌手"
	case "starred":
		return "收藏歌手"
	}
	return "歌手"
}

// albumByID 找专辑：内存缓存 → 持久化收藏 → 数据库歌曲 → 平台专辑接口。
func (s *Server) albumByID(rc *reqCtx, id string) *albumGroup {
	p, ok := music.ParseID(id)
	if !ok {
		return nil
	}
	if infos, ok := s.Catalog.CachedAlbum(id); ok && len(infos) > 0 {
		groups := groupAlbums(infos)
		if len(groups) > 0 {
			return groups[0]
		}
	}
	if src, name, artist, js, err := s.DB.GetAlbum(rc.ctx, id); err == nil {
		var ids []string
		if jsonUnmarshal(js, &ids) == nil && len(ids) > 0 {
			infos := s.Catalog.Tracks(rc.ctx, ids)
			if len(infos) > 0 {
				return &albumGroup{ID: id, Name: name, Artist: artist, Source: src, Cover: infos[0].TrackID(), Songs: infos}
			}
		}
	}
	// 尝试从已入库歌曲中聚合
	rows, _ := s.DB.SearchTracks(rc.ctx, "", 2000, 0)
	var songs []*music.Info
	for _, t := range rows {
		in, err := music.ParseInfo(t.JSON)
		if err == nil && in.AlbumSubID() == id {
			songs = append(songs, in)
		}
	}
	if len(songs) > 0 {
		g := groupAlbums(songs)[0]
		s.rememberAlbum(rc, g)
		return g
	}
	// 平台专辑接口
	if music.IsPlatform(p.Source) && p.Key != "" {
		if infos, name, artist := s.fetchAlbum(rc, p.Source, p.Key); len(infos) > 0 {
			g := &albumGroup{ID: id, Name: name, Artist: artist, Source: p.Source, Cover: infos[0].TrackID(), Songs: infos}
			s.rememberAlbum(rc, g)
			return g
		}
	}
	return nil
}

func (s *Server) rememberAlbum(_ *reqCtx, g *albumGroup) {
	// 普通搜索、榜单和歌手页只缓存专辑摘要；歌曲必须等用户打开专辑后才请求。
	meta := music.AlbumMeta{Source: g.Source, Name: g.Name, Artist: g.Artist}
	if len(g.Songs) > 0 {
		meta.ID = g.Songs[0].AlbumID()
		meta.Image = g.Songs[0].Img()
		meta.SongCount = len(g.Songs)
	}
	s.Catalog.CacheAlbumMeta(meta)
}

// fetchAlbum 仅在真实专辑节点被打开时调用对应平台专辑歌曲接口。
func (s *Server) fetchAlbum(rc *reqCtx, source, key string) ([]*music.Info, string, string) {
	infos, meta, err := s.Catalog.AlbumSongs(rc.ctx, source, key)
	if err != nil || len(infos) == 0 {
		return nil, "", ""
	}
	return infos, meta.Name, meta.Artist
}

// onlineAlbumGroup 统一读取在线专辑；持久化收藏命中时不会访问上游。
func (s *Server) onlineAlbumGroup(rc *reqCtx, id string) *albumGroup {
	locator, parsed := music.ParseOnlineAlbumID(id)
	if !parsed {
		return nil
	}
	infos, meta, err := s.Catalog.AlbumSongsFor(rc.ctx, locator)
	if err != nil || len(infos) == 0 {
		return nil
	}
	name, artist := meta.Name, meta.Artist
	if name == "" {
		name = locator.Name
	}
	if artist == "" {
		artist = locator.Artist
	}
	return &albumGroup{ID: id, Name: name, Artist: artist, Source: locator.Source, Cover: infos[0].TrackID(), Songs: infos}
}

// artistByID 找歌手：优先使用本次会话缓存，收藏歌手再从数据库读取。
func (s *Server) artistByID(rc *reqCtx, id string) *artistGroup {
	if locator, ok := music.ParseSingerDirectoryID(id); ok {
		return s.artistByName(rc, locator.Name)
	}
	if name, ok := s.Catalog.CachedArtist(id); ok {
		return s.artistByName(rc, name)
	}
	if _, name, _, err := s.DB.GetArtist(rc.ctx, id); err == nil && name != "" {
		s.Catalog.CacheArtist(id, name)
		return s.artistByName(rc, name)
	}
	return nil
}

// artistByName 只聚合已持久化或本次会话已经见过的歌曲，不再触发五个平台扩展搜索。
func (s *Server) artistByName(rc *reqCtx, name string) *artistGroup {
	id := music.ArtistID(name)
	limit := s.Settings.Get().ArtistSongLimit
	rows, _ := s.DB.SearchTracks(rc.ctx, name, limit*3, 0)
	var songs []*music.Info
	for _, t := range rows {
		in, err := music.ParseInfo(t.JSON)
		if err == nil && (in.PrimarySinger() == name || strings.Contains(in.Singer(), name)) {
			songs = append(songs, in)
		}
	}
	songs = append(songs, s.Catalog.CachedTracksByArtist(name, limit*2)...)
	songs = dedupe(songs)
	if len(songs) > limit {
		songs = songs[:limit]
	}
	g := &artistGroup{ID: id, Name: name, Albums: map[string]bool{}, Songs: songs}
	for _, in := range songs {
		g.Albums[in.AlbumSubID()] = true
	}
	if len(songs) > 0 {
		g.Cover = songs[0].TrackID()
	}
	s.Catalog.CacheArtist(id, name)
	return g
}

func (s *Server) getArtist(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	g := s.artistByID(rc, id)
	if g == nil {
		writeErr(w, r, ErrNotFound, "artist not found")
		return
	}
	obj := s.artistObj(rc, g)
	var albums []M
	groups := slicePage(groupAlbums(g.Songs), 0, s.Settings.Get().ArtistAlbumLimit)
	for _, a := range groups {
		s.rememberAlbum(rc, a)
		albums = append(albums, s.albumObj(rc, a))
	}
	obj["album"] = albums
	obj["albumCount"] = len(albums)
	writeOK(w, r, "artist", obj)
}

func (s *Server) getArtistInfo(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	g := s.artistByID(rc, id)
	name := "artistInfo2"
	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, ".view"), "getArtistInfo") {
		name = "artistInfo"
	}
	out := M{"biography": "", "musicBrainzId": "", "lastFmUrl": ""}
	if g != nil {
		if g.Cover != "" {
			cover := "/rest/getCoverArt.view?id=" + g.Cover
			out["smallImageUrl"] = cover
			out["mediumImageUrl"] = cover
			out["largeImageUrl"] = cover
		}
		var similar []M
		seen := map[string]bool{}
		for _, in := range g.Songs {
			for _, sn := range strings.Split(in.Singer(), "、") {
				sn = strings.TrimSpace(sn)
				if sn == "" || sn == g.Name || seen[sn] {
					continue
				}
				seen[sn] = true
				aid := music.ArtistID(sn)
				s.Catalog.CacheArtist(aid, sn)
				similar = append(similar, M{"id": aid, "name": sn, "albumCount": 0})
				if len(similar) >= s.Settings.Get().ArtistAlbumLimit {
					break
				}
			}
			if len(similar) >= s.Settings.Get().ArtistAlbumLimit {
				break
			}
		}
		if len(similar) > 0 {
			out["similarArtist"] = similar
		}
	}
	writeOK(w, r, name, out)
}

func (s *Server) getAlbum(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	var g *albumGroup
	if _, parsed := music.ParseOnlineAlbumID(id); parsed {
		g = s.onlineAlbumGroup(rc, id)
	} else {
		g = s.albumByID(rc, id)
	}
	if g == nil {
		// 客户端可能把歌曲 ID 当专辑用
		if in, err := s.Catalog.Track(rc.ctx, id); err == nil {
			g = groupAlbums([]*music.Info{in})[0]
		} else {
			writeErr(w, r, ErrNotFound, "album not found")
			return
		}
	}
	obj := s.albumObj(rc, g)
	if locator, online := music.ParseOnlineAlbumID(id); online {
		parent := locator.Parent
		if parent == "" {
			parent = music.ArtistID(g.Artist)
		}
		obj["parent"] = parent
		obj["song"] = songListWithAlbum(s, rc, g.Songs, id)
	} else {
		obj["song"] = songList(s, rc, g.Songs)
	}
	writeOK(w, r, "album", obj)
}

func (s *Server) getAlbumInfo(w http.ResponseWriter, r *http.Request) {
	name := "albumInfo"
	writeOK(w, r, name, M{"notes": "", "musicBrainzId": "", "lastFmUrl": ""})
}

func (s *Server) getSong(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	in, err := s.Catalog.Track(rc.ctx, param(r, "id"))
	if err != nil {
		writeErr(w, r, ErrNotFound, err.Error())
		return
	}
	writeOK(w, r, "song", s.songObj(rc, in))
}

func (s *Server) getTopSongs(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	name := param(r, "artist")
	count := paramInt(r, "count", 50)
	if maxCount := s.Settings.Get().ArtistSongLimit; count > maxCount {
		count = maxCount
	}
	if name == "" {
		writeErr(w, r, ErrMissingParam, "artist required")
		return
	}
	g := s.artistByName(rc, name)
	if g == nil {
		writeOK(w, r, "topSongs", M{"song": []M{}})
		return
	}
	songs := g.Songs
	if len(songs) > count {
		songs = songs[:count]
	}
	writeOK(w, r, "topSongs", M{"song": songList(s, rc, songs)})
}

func (s *Server) getSimilarSongs(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	count := paramInt(r, "count", 50)
	if maxCount := s.Settings.Get().ArtistSongLimit; count > maxCount {
		count = maxCount
	}
	var songs []*music.Info
	p, _ := music.ParseID(id)
	switch p.Kind {
	case music.KindArtist:
		if g := s.artistByID(rc, id); g != nil {
			songs = g.Songs
		}
	default:
		if in, err := s.Catalog.LocalTrack(rc.ctx, id); err == nil {
			if artist := s.artistByName(rc, in.PrimarySinger()); artist != nil {
				songs = artist.Songs
			}
			filtered := songs[:0:0]
			for _, x := range songs {
				if x.TrackID() != in.TrackID() {
					filtered = append(filtered, x)
				}
			}
			songs = filtered
		}
	}
	if len(songs) > count {
		songs = songs[:count]
	}
	name := "similarSongs2"
	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, ".view"), "getSimilarSongs") {
		name = "similarSongs"
	}
	writeOK(w, r, name, M{"song": songList(s, rc, songs)})
}
