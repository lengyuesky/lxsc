package subsonic

import (
	"net/http"
	"strings"

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
	raw := param(r, "query")
	if raw == "" {
		raw = param(r, "any")
	}
	query, sources, local := s.parseQuery(raw)
	songCount := paramInt(r, "songCount", 20)
	songOffset := paramInt(r, "songOffset", 0)
	albumCount := paramInt(r, "albumCount", 20)
	albumOffset := paramInt(r, "albumOffset", 0)
	artistCount := paramInt(r, "artistCount", 20)
	artistOffset := paramInt(r, "artistOffset", 0)

	var infos []*music.Info
	if query == "" {
		// 空查询：返回资料库内容（部分客户端用空查询列出全部）
		infos = s.libraryTracks(rc, u.ID)
	} else if local {
		rows, _ := s.DB.SearchTracks(rc.ctx, query, songOffset+songCount, 0)
		for _, t := range rows {
			if in, err := music.ParseInfo(t.JSON); err == nil {
				infos = append(infos, in)
			}
		}
	} else {
		// 在线搜索：按 offset 推算页码（每平台 limit 条），把多页合并
		limit := s.Settings.Get().SearchLimit
		if songCount > limit*len(sources) {
			limit = (songCount + len(sources) - 1) / len(sources)
			if limit > 50 {
				limit = 50
			}
		}
		perPage := limit * len(sources)
		if perPage == 0 {
			perPage = limit
		}
		startPage := songOffset/perPage + 1
		endPage := (songOffset+songCount-1)/perPage + 1
		for page := startPage; page <= endPage && page <= startPage+2; page++ {
			infos = append(infos, s.Catalog.Search(rc.ctx, query, music.SearchOptions{Sources: sources, Page: page, Limit: limit})...)
		}
		infos = dedupe(infos)
		// 相对本次起始页的偏移
		rel := songOffset - (startPage-1)*perPage
		if rel < len(infos) {
			infos = infos[rel:]
		} else {
			infos = nil
		}
	}
	songs := infos
	if len(songs) > songCount {
		songs = songs[:songCount]
	}
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
	name := "searchResult3"
	method := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".view")
	switch method {
	case "search2":
		name = "searchResult2"
	case "search":
		name = "searchResult"
	}
	writeOK(w, r, name, M{"song": songList(s, rc, songs), "album": albumObjs, "artist": artistObjs})
}

func slicePage[T any](list []T, offset, count int) []T {
	if offset >= len(list) || offset < 0 {
		return nil
	}
	end := offset + count
	if end > len(list) || count <= 0 {
		end = len(list)
	}
	return list[offset:end]
}
