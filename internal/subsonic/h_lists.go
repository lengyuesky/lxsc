package subsonic

import (
	"math/rand"
	"net/http"
	"strings"

	"lxsc/internal/music"
)

func (s *Server) getAlbumList(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	typ := param(r, "type")
	size := paramInt(r, "size", 10)
	offset := paramInt(r, "offset", 0)
	var infos []*music.Info
	switch typ {
	case "recent":
		ids, _ := s.DB.RecentTracks(rc.ctx, u.ID, 500)
		infos = s.Catalog.Tracks(rc.ctx, ids)
	case "frequent":
		ids, _ := s.DB.FrequentTracks(rc.ctx, u.ID, 500)
		infos = s.Catalog.Tracks(rc.ctx, ids)
	case "starred":
		var ids []string
		items, _ := s.DB.ListStarred(rc.ctx, u.ID, "")
		for _, it := range items {
			if strings.HasPrefix(it.ItemID, music.KindTrack+"-") {
				ids = append(ids, it.ItemID)
			}
		}
		infos = s.Catalog.Tracks(rc.ctx, ids)
		// 直接收藏的专辑
		var extra []*albumGroup
		for _, it := range items {
			if strings.HasPrefix(it.ItemID, music.KindAlbum+"-") || strings.HasPrefix(it.ItemID, music.KindOnlineAlbum+"-") {
				var g *albumGroup
				if _, parsed := music.ParseOnlineAlbumID(it.ItemID); parsed {
					g = s.onlineAlbumGroup(rc, it.ItemID)
				} else {
					g = s.albumByID(rc, it.ItemID)
				}
				if g != nil {
					extra = append(extra, g)
				}
			}
		}
		groups := append(groupAlbums(infos), extra...)
		s.writeAlbumList(w, r, rc, groups, offset, size)
		return
	case "random", "newest", "highest", "alphabeticalByName", "alphabeticalByArtist", "byGenre", "byYear":
		// 列表接口只读取用户资料库；不能为了补数量隐式扫描在线榜单。
		infos = s.libraryTracks(rc, u.ID)
		if typ == "byGenre" {
			genre := param(r, "genre")
			filtered := infos[:0:0]
			for _, in := range infos {
				if music.PlatformName(in.Source()) == genre {
					filtered = append(filtered, in)
				}
			}
			infos = filtered
		}
	default:
		infos = s.libraryTracks(rc, u.ID)
	}
	groups := groupAlbums(dedupe(infos))
	if typ == "random" {
		rand.Shuffle(len(groups), func(i, j int) { groups[i], groups[j] = groups[j], groups[i] })
	}
	s.writeAlbumList(w, r, rc, groups, offset, size)
}

func (s *Server) writeAlbumList(w http.ResponseWriter, r *http.Request, rc *reqCtx, groups []*albumGroup, offset, size int) {
	groups = slicePage(groups, offset, size)
	out := make([]M, 0, len(groups))
	for _, g := range groups {
		s.rememberAlbum(rc, g)
		out = append(out, s.albumObj(rc, g))
	}
	name := "albumList2"
	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, ".view"), "getAlbumList") {
		name = "albumList"
	}
	writeOK(w, r, name, M{"album": out})
}

func (s *Server) getRandomSongs(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	size := paramInt(r, "size", 10)
	// 随机歌曲只来自用户资料库；打开该接口不应触发在线榜单扫描。
	infos := s.libraryTracks(rc, u.ID)
	infos = dedupe(infos)
	rand.Shuffle(len(infos), func(i, j int) { infos[i], infos[j] = infos[j], infos[i] })
	if len(infos) > size {
		infos = infos[:size]
	}
	writeOK(w, r, "randomSongs", M{"song": songList(s, rc, infos)})
}

func (s *Server) getSongsByGenre(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	genre := param(r, "genre")
	count := paramInt(r, "count", 10)
	offset := paramInt(r, "offset", 0)
	var infos []*music.Info
	for _, in := range s.libraryTracks(rc, u.ID) {
		if music.PlatformName(in.Source()) == genre {
			infos = append(infos, in)
		}
	}
	infos = slicePage(infos, offset, count)
	writeOK(w, r, "songsByGenre", M{"song": songList(s, rc, infos)})
}

// getOnlineSongLists 非标准端点：列出平台在线歌单（供调试）
func (s *Server) getOnlineSongLists(w http.ResponseWriter, r *http.Request) {
	src := param(r, "source")
	if src == "" {
		src = "wy"
	}
	raw, err := s.Catalog.SongLists(r.Context(), src, param(r, "sort"), param(r, "tag"), paramInt(r, "page", 1))
	if err != nil {
		writeErr(w, r, ErrGeneric, err.Error())
		return
	}
	var v any
	_ = jsonUnmarshal(raw, &v)
	writeOK(w, r, "songLists", v)
}
