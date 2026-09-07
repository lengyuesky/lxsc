package subsonic

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/secret"
)

func (s *Server) playlistObj(rc *reqCtx, p *db.Playlist, songs []*music.Info) M {
	dur := 0
	for _, in := range songs {
		dur += in.Duration()
	}
	m := M{
		"id": p.ID, "name": p.Name, "comment": p.Comment, "owner": p.Owner, "public": p.Public,
		"songCount": p.Count, "duration": dur, "created": fmtTime(p.CreatedAt), "changed": fmtTime(p.UpdatedAt),
	}
	if len(songs) > 0 {
		m["coverArt"] = songs[0].TrackID()
	} else {
		m["coverArt"] = p.ID
	}
	return m
}

const virtualPlaylistTime = "2000-01-01T00:00:00Z"

// boardPlaylistObj 构造只包含榜单摘要的虚拟歌单，不读取榜单歌曲。
func boardPlaylistObj(board music.Board) (M, bool) {
	source := strings.TrimSpace(board.Source)
	bangID := strings.TrimSpace(board.BangID)
	boardName := strings.TrimSpace(board.Name)
	if !music.IsPlatform(source) || bangID == "" || bangID == "0" || boardName == "" {
		return nil, false
	}
	id := music.BoardID(source, bangID)
	return M{
		"id": id, "name": music.PlatformName(source) + " · " + boardName,
		"comment": "在线榜单，打开后加载歌曲", "owner": "榜单", "public": true,
		// 榜单摘要没有可靠的歌曲数量；不为计算数量提前请求榜单内容。
		"songCount": 0, "duration": 0,
		"created": virtualPlaylistTime, "changed": virtualPlaylistTime,
		"coverArt": id,
	}, true
}

// boardPlaylistObjects 并行读取各平台榜单名称，保持配置的平台顺序。
// 每个平台只请求榜单目录，不请求任何榜单歌曲。
func (s *Server) boardPlaylistObjects(rc *reqCtx) []M {
	display := newBoardVisibility(s.Settings.Get())
	sources := display.sources
	if len(sources) == 0 {
		return nil
	}
	type result struct {
		index  int
		boards []music.Board
		err    error
	}
	results := make([]result, len(sources))
	var wg sync.WaitGroup
	for index, source := range sources {
		wg.Add(1)
		go func(index int, source string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(rc.ctx, 10*time.Second)
			defer cancel()
			boards, err := s.Catalog.Boards(ctx, source)
			results[index] = result{index: index, boards: boards, err: err}
		}(index, source)
	}
	wg.Wait()

	out := make([]M, 0)
	seen := map[string]bool{}
	for _, result := range results {
		if result.err != nil {
			if s.Log != nil {
				s.Log.Warn("读取榜单名称失败", "source", sources[result.index], "err", result.err)
			}
			continue
		}
		for _, board := range result.boards {
			if !display.allows(board) {
				continue
			}
			obj, ok := boardPlaylistObj(board)
			boardID, _ := obj["id"].(string)
			if !ok || boardID == "" || seen[boardID] {
				continue
			}
			seen[boardID] = true
			out = append(out, obj)
		}
	}
	return out
}

func (s *Server) getPlaylists(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	list, err := s.DB.ListPlaylists(rc.ctx, u.ID)
	if err != nil {
		writeErr(w, r, ErrGeneric, err.Error())
		return
	}
	out := make([]M, 0, len(list))
	for _, p := range list {
		full, err := s.DB.GetPlaylist(rc.ctx, p.ID)
		var songs []*music.Info
		if err == nil && len(full.TrackIDs) > 0 {
			songs = s.Catalog.Tracks(rc.ctx, full.TrackIDs[:1])
		}
		out = append(out, s.playlistObj(rc, p, songs))
	}
	// 榜单只在这里加载名称；不会为了生成歌单摘要请求榜单歌曲。
	out = append(out, s.boardPlaylistObjects(rc)...)
	writeOK(w, r, "playlists", M{"playlist": out})
}

func canReadPlaylist(u *db.User, p *db.Playlist) bool {
	return u != nil && (u.IsAdmin || p.UserID == u.ID || p.Public)
}

func canEditPlaylist(u *db.User, p *db.Playlist) bool {
	return u != nil && (u.IsAdmin || p.UserID == u.ID)
}

func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	p, ok := music.ParseID(id)
	if ok && p.Kind == music.KindBoard {
		if !music.IsPlatform(p.Source) || strings.TrimSpace(p.Key) == "" || p.Key == "0" {
			writeErr(w, r, ErrNotFound, "榜单不存在")
			return
		}
		// 歌单列表阶段只缓存榜单名称；歌曲只在请求具体榜单详情时读取。
		boardName, _ := s.Catalog.BoardName(p.Source, p.Key)
		if boardName == "" {
			boardName = p.Key
		}
		name := music.PlatformName(p.Source) + " · " + boardName
		infos, err := s.Catalog.BoardTracks(rc.ctx, p.Source, p.Key, 1)
		if err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		dur := 0
		for _, in := range infos {
			dur += in.Duration()
		}
		writeOK(w, r, "playlist", M{"id": id, "name": name, "comment": "在线榜单，只读", "owner": "榜单", "public": true, "songCount": len(infos), "duration": dur, "created": virtualPlaylistTime, "changed": virtualPlaylistTime, "coverArt": id, "entry": songList(s, rc, infos)})
		return
	}
	if ok && p.Kind == music.KindSongList {
		infos, meta, err := s.Catalog.SongListDetail(rc.ctx, p.Source, p.Key, 1)
		if err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		name := id
		if meta != nil {
			if info, ok := meta["info"].(map[string]any); ok {
				if n, ok := info["name"].(string); ok {
					name = n
				}
			}
		}
		writeOK(w, r, "playlist", M{"id": id, "name": name, "owner": music.PlatformName(p.Source), "public": true, "songCount": len(infos), "created": virtualPlaylistTime, "changed": virtualPlaylistTime, "coverArt": id, "entry": songList(s, rc, infos)})
		return
	}
	pl, err := s.DB.GetPlaylist(rc.ctx, id)
	if err != nil {
		writeErr(w, r, ErrNotFound, "playlist not found")
		return
	}
	if !canReadPlaylist(currentUser(r), pl) {
		writeErr(w, r, ErrNotAuthorized, "not authorized to view playlist")
		return
	}
	songs := s.Catalog.Tracks(rc.ctx, pl.TrackIDs)
	obj := s.playlistObj(rc, pl, songs)
	obj["entry"] = songList(s, rc, songs)
	writeOK(w, r, "playlist", obj)
}

func (s *Server) ensureTracks(rc *reqCtx, ids []string) []string {
	var out []string
	var infos []*music.Info
	for _, id := range ids {
		if in, err := s.Catalog.Track(rc.ctx, id); err == nil {
			out = append(out, id)
			infos = append(infos, in)
		} else {
			s.Log.Warn("歌单加入了未知歌曲", "id", id, "err", err)
		}
	}
	if err := s.Catalog.RememberSync(rc.ctx, infos); err != nil {
		s.Log.Warn("持久化歌单歌曲元数据失败", "err", err)
	}
	return out
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	id := param(r, "playlistId")
	if isVirtualBoardID(id) {
		writeErr(w, r, ErrNotAuthorized, "在线榜单为只读歌单")
		return
	}
	name := param(r, "name")
	songIDs := s.ensureTracks(rc, params(r, "songId"))
	if id != "" {
		pl, err := s.DB.GetPlaylist(rc.ctx, id)
		if err != nil {
			writeErr(w, r, ErrNotFound, "playlist not found")
			return
		}
		if !canEditPlaylist(u, pl) {
			writeErr(w, r, ErrNotAuthorized, "not owner")
			return
		}
		if err := s.DB.ReplacePlaylistTracks(rc.ctx, id, songIDs); err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		if name != "" {
			_ = s.DB.UpdatePlaylistMeta(rc.ctx, id, &name, nil, nil)
		}
	} else {
		if name == "" {
			writeErr(w, r, ErrMissingParam, "name required")
			return
		}
		id = "pl-" + secret.RandomToken(9)
		if err := s.DB.CreatePlaylist(rc.ctx, id, u.ID, name, songIDs); err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
		if s.Settings.Get().PublicPlaylists {
			pub := true
			_ = s.DB.UpdatePlaylistMeta(rc.ctx, id, nil, nil, &pub)
		}
	}
	pl, _ := s.DB.GetPlaylist(rc.ctx, id)
	songs := s.Catalog.Tracks(rc.ctx, pl.TrackIDs)
	obj := s.playlistObj(rc, pl, songs)
	obj["entry"] = songList(s, rc, songs)
	writeOK(w, r, "playlist", obj)
}

func (s *Server) updatePlaylist(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	id := param(r, "playlistId")
	if isVirtualBoardID(id) {
		writeErr(w, r, ErrNotAuthorized, "在线榜单为只读歌单")
		return
	}
	pl, err := s.DB.GetPlaylist(rc.ctx, id)
	if err != nil {
		writeErr(w, r, ErrNotFound, "playlist not found")
		return
	}
	if !canEditPlaylist(u, pl) {
		writeErr(w, r, ErrNotAuthorized, "not owner")
		return
	}
	var name, comment *string
	var public *bool
	if v := param(r, "name"); v != "" {
		name = &v
	}
	if v := param(r, "comment"); v != "" || r.URL.Query().Has("comment") {
		comment = &v
	}
	if v := param(r, "public"); v != "" {
		b := v == "true" || v == "1"
		public = &b
	}
	_ = s.DB.UpdatePlaylistMeta(rc.ctx, id, name, comment, public)
	ids := append([]string(nil), pl.TrackIDs...)
	// 删除按索引（从大到小）
	var removeIdx []int
	for _, v := range params(r, "songIndexToRemove") {
		if n, err := strconv.Atoi(v); err == nil {
			removeIdx = append(removeIdx, n)
		}
	}
	if len(removeIdx) > 0 {
		rm := map[int]bool{}
		for _, n := range removeIdx {
			rm[n] = true
		}
		kept := ids[:0:0]
		for i, tid := range ids {
			if !rm[i] {
				kept = append(kept, tid)
			}
		}
		ids = kept
	}
	added := s.ensureTracks(rc, params(r, "songIdToAdd"))
	if len(added) > 0 {
		ids = append(added, ids...)
	}
	if len(removeIdx) > 0 || len(added) > 0 {
		if err := s.DB.ReplacePlaylistTracks(rc.ctx, id, ids); err != nil {
			writeErr(w, r, ErrGeneric, err.Error())
			return
		}
	}
	writeOK(w, r, "", nil)
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := param(r, "id")
	if isVirtualBoardID(id) {
		writeErr(w, r, ErrNotAuthorized, "在线榜单为只读歌单")
		return
	}
	pl, err := s.DB.GetPlaylist(r.Context(), id)
	if err != nil {
		writeErr(w, r, ErrNotFound, "playlist not found")
		return
	}
	if !canEditPlaylist(u, pl) {
		writeErr(w, r, ErrNotAuthorized, "not owner")
		return
	}
	_ = s.DB.DeletePlaylist(r.Context(), id)
	writeOK(w, r, "", nil)
}

func isVirtualBoardID(id string) bool {
	p, ok := music.ParseID(id)
	return ok && p.Kind == music.KindBoard
}

func kindOf(id string) string {
	switch {
	case strings.HasPrefix(id, music.KindAlbum+"-"), strings.HasPrefix(id, music.KindOnlineAlbum+"-"):
		return "album"
	case strings.HasPrefix(id, music.KindArtist+"-"), strings.HasPrefix(id, music.KindSingerDir+"-"), strings.HasPrefix(id, music.KindArtistName+"-"):
		return "artist"
	}
	return "track"
}
