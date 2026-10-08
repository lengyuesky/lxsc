package subsonic

import (
	"net/http"
	"strings"

	"lxsc/internal/admission"
	"lxsc/internal/music"
)

// parseQuery 解析搜索前缀：wy:/tx:/kw:/kg:/mg:/online:/local:
func (s *Server) parseQuery(q string) (clean string, sources []string, local bool) {
	clean, sources, local = parsePrefix(q)
	if sources == nil && !local {
		sources = s.Settings.Get().SearchSources
	} else if len(sources) == 0 && !local {
		sources = s.Settings.Get().SearchSources
	}
	return
}

// parsePrefix 纯解析：返回 sources 为 nil 表示未指定平台
func parsePrefix(q string) (clean string, sources []string, local bool) {
	q = strings.TrimSpace(q)
	if q == `""` || q == "''" {
		q = ""
	}
	lower := strings.ToLower(q)
	cut := func(prefix string) (string, bool) {
		for _, sep := range []string{":", "："} {
			if strings.HasPrefix(lower, prefix+sep) {
				return strings.TrimSpace(q[len(prefix)+len(sep):]), true
			}
		}
		return q, false
	}
	if c, ok := cut("local"); ok {
		return c, nil, true
	}
	for _, p := range []string{"online", "net"} {
		if c, ok := cut(p); ok {
			return c, []string{}, false
		}
	}
	for _, p := range []string{"wy", "tx", "kw", "kg", "mg"} {
		if c, ok := cut(p); ok {
			return c, []string{p}, false
		}
	}
	return q, nil, false
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	release, err := s.Catalog.AcquireRequest(r.Context(), u.ID)
	if err != nil {
		if r.Context().Err() == nil {
			writeErr(w, r, ErrBusy, admission.ErrBusy.Error())
		}
		return
	}
	defer release()
	raw := param(r, "query")
	if raw == "" {
		raw = param(r, "any")
	}
	query, sources, local := s.parseQuery(raw)
	songCount := paramCount(r, "songCount", 20)
	songOffset := paramOffset(r, "songOffset")
	albumCount := paramCount(r, "albumCount", 20)
	albumOffset := paramOffset(r, "albumOffset")
	artistCount := paramCount(r, "artistCount", 20)
	artistOffset := paramOffset(r, "artistOffset")
	windowSize := max(songCount, albumCount, artistCount)
	if query != "" && !local {
		s.searchOnline(w, r, rc, query, sources, songCount, songOffset, albumCount, albumOffset, artistCount, artistOffset)
		return
	}

	var infos []*music.Info
	if query == "" {
		// 空查询：返回资料库内容（部分客户端用空查询列出全部）
		infos = s.libraryTracks(rc, u.ID)
	} else if local {
		rows, err := s.DB.SearchTracks(rc.ctx, query, windowSize, songOffset)
		if err != nil {
			writeErr(w, r, ErrGeneric, "读取本地歌曲失败")
			return
		}
		for _, t := range rows {
			if in, err := music.ParseInfo(t.JSON); err == nil {
				infos = append(infos, in)
			}
		}
	}
	songStart := 0
	if query == "" {
		songStart = songOffset
	}
	songs := slicePage(infos, songStart, songCount)
	albums := groupAlbums(infos)
	for _, a := range albums {
		s.rememberAlbum(rc, a)
	}
	albums = slicePage(albums, albumOffset, albumCount)
	artists := groupArtists(infos)
	for _, a := range artists {
		s.Catalog.CacheArtist(a.ID, a.Name)
	}
	artists = slicePage(artists, artistOffset, artistCount)

	albumObjs := make([]M, 0, len(albums))
	for _, a := range albums {
		albumObjs = append(albumObjs, s.albumObj(rc, a))
	}
	artistObjs := make([]M, 0, len(artists))
	for _, a := range artists {
		artistObjs = append(artistObjs, s.artistObj(rc, a))
	}
	s.writeSearch(w, r, songList(s, rc, songs), albumObjs, artistObjs)
}

func (s *Server) writeSearch(w http.ResponseWriter, r *http.Request, songs, albums, artists []M) {
	name := "searchResult3"
	method := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".view")
	switch method {
	case "search2":
		name = "searchResult2"
	case "search":
		name = "searchResult"
	}
	writeOK(w, r, name, M{"song": songs, "album": albums, "artist": artists})
}

func slicePage[T any](list []T, offset, count int) []T {
	if offset >= len(list) || offset < 0 || count <= 0 {
		return nil
	}
	return list[offset : offset+min(count, len(list)-offset)]
}
