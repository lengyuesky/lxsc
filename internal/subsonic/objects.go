package subsonic

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"lxsc/internal/music"
)

type reqCtx struct {
	ctx     context.Context
	starred map[string]int64
	plays   map[string]int
}

func (s *Server) newReqCtx(r *http.Request) *reqCtx {
	rc := &reqCtx{ctx: r.Context()}
	if u := currentUser(r); u != nil {
		rc.starred, _ = s.DB.StarredSet(r.Context(), u.ID)
	}
	return rc
}

const generatedMetadataTime = "2000-01-01T00:00:00Z"

func fmtTime(ts int64) string {
	if ts <= 0 {
		return generatedMetadataTime
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

// songObj 构造 Child(song) 对象。
func (s *Server) songObj(rc *reqCtx, in *music.Info) M {
	return s.songObjWithAlbum(rc, in, "")
}

// songObjWithAlbum 允许在线专辑目录覆盖歌曲所属专辑 ID，保持目录父子身份一致。
func (s *Server) songObjWithAlbum(rc *reqCtx, in *music.Info, albumOverride string) M {
	id := in.TrackID()
	albumID := in.AlbumSubID()
	if albumOverride != "" {
		albumID = albumOverride
	}
	artistID := in.ArtistSubID()
	if s.Catalog != nil {
		// 歌曲对象只记录歌手定位；不能把单首歌曲误当成完整专辑缓存。
		s.Catalog.CacheArtist(artistID, in.PrimarySinger())
	}
	dur := in.Duration()
	q := in.BestQuality()
	if q == "" {
		q = "320k"
	}
	bitRate, suffix, ct := music.QualityMeta(q)
	size := int64(bitRate) * 1000 / 8 * int64(dur)
	m := M{
		"id":                 id,
		"parent":             albumID,
		"isDir":              false,
		"title":              in.Name(),
		"album":              in.Album(),
		"artist":             in.Singer(),
		"albumId":            albumID,
		"artistId":           artistID,
		"coverArt":           id,
		"duration":           dur,
		"bitRate":            bitRate,
		"size":               size,
		"suffix":             suffix,
		"contentType":        ct,
		"track":              1,
		"isVideo":            false,
		"type":               "music",
		"mediaType":          "song",
		"path":               music.PlatformName(in.Source()) + "/" + safePath(in.Singer()) + "/" + safePath(in.Album()) + "/" + safePath(in.Name()) + "." + suffix,
		"created":            fmtTime(0),
		"genre":              music.PlatformName(in.Source()),
		"musicBrainzId":      "",
		"artists":            []M{{"id": artistID, "name": in.PrimarySinger()}},
		"albumArtists":       []M{{"id": artistID, "name": in.PrimarySinger()}},
		"displayArtist":      in.Singer(),
		"displayAlbumArtist": in.PrimarySinger(),
	}
	if rc != nil {
		if ts, ok := rc.starred[id]; ok {
			m["starred"] = fmtTime(ts)
		}
		if n, ok := rc.plays[id]; ok {
			m["playCount"] = n
		}
	}
	return m
}

func safePath(s string) string {
	s = strings.NewReplacer("/", "／", "\\", "＼").Replace(s)
	if s == "" {
		return "_"
	}
	return s
}

// albumGroup 由歌曲派生的专辑
type albumGroup struct {
	ID     string
	Name   string
	Artist string
	Source string
	Cover  string
	Songs  []*music.Info
}

// groupAlbums 按专辑 ID 聚合歌曲（保持首次出现顺序）
func groupAlbums(infos []*music.Info) []*albumGroup {
	var out []*albumGroup
	idx := map[string]*albumGroup{}
	for _, in := range infos {
		id := in.AlbumSubID()
		g, ok := idx[id]
		if !ok {
			g = &albumGroup{ID: id, Name: in.Album(), Artist: in.PrimarySinger(), Source: in.Source(), Cover: in.TrackID()}
			if g.Name == "" {
				g.Name = "未知专辑"
			}
			idx[id] = g
			out = append(out, g)
		}
		g.Songs = append(g.Songs, in)
	}
	return out
}

// albumObj AlbumID3
func (s *Server) albumObj(rc *reqCtx, g *albumGroup) M {
	if s.Catalog != nil {
		s.Catalog.CacheArtist(music.ArtistID(g.Artist), g.Artist)
	}
	dur := 0
	for _, in := range g.Songs {
		dur += in.Duration()
	}
	artistID := music.ArtistID(g.Artist)
	m := M{
		"id":            g.ID,
		"name":          g.Name,
		"title":         g.Name,
		"album":         g.Name,
		"artist":        g.Artist,
		"artistId":      artistID,
		"coverArt":      g.ID,
		"songCount":     len(g.Songs),
		"duration":      dur,
		"created":       fmtTime(0),
		"isDir":         true,
		"parent":        artistID,
		"genre":         music.PlatformName(g.Source),
		"artists":       []M{{"id": artistID, "name": g.Artist}},
		"displayArtist": g.Artist,
		"isCompilation": false,
		"musicBrainzId": "",
	}
	if rc != nil {
		if ts, ok := rc.starred[g.ID]; ok {
			m["starred"] = fmtTime(ts)
		}
	}
	return m
}

// artistGroup 由歌曲派生的歌手
type artistGroup struct {
	ID     string
	Name   string
	Cover  string
	Albums map[string]bool
	Songs  []*music.Info
}

func groupArtists(infos []*music.Info) []*artistGroup {
	var out []*artistGroup
	idx := map[string]*artistGroup{}
	for _, in := range infos {
		name := in.PrimarySinger()
		if name == "" {
			name = "未知歌手"
		}
		id := music.ArtistID(name)
		g, ok := idx[id]
		if !ok {
			g = &artistGroup{ID: id, Name: name, Cover: in.TrackID(), Albums: map[string]bool{}}
			idx[id] = g
			out = append(out, g)
		}
		g.Albums[in.AlbumSubID()] = true
		g.Songs = append(g.Songs, in)
	}
	return out
}

func (s *Server) artistObj(rc *reqCtx, g *artistGroup) M {
	if s.Catalog != nil {
		s.Catalog.CacheArtist(g.ID, g.Name)
	}
	m := M{
		"id":            g.ID,
		"name":          g.Name,
		"coverArt":      g.ID,
		"albumCount":    len(g.Albums),
		"musicBrainzId": "",
		"sortName":      g.Name,
	}
	if rc != nil {
		if ts, ok := rc.starred[g.ID]; ok {
			m["starred"] = fmtTime(ts)
		}
	}
	return m
}

func songList(s *Server, rc *reqCtx, infos []*music.Info) []M {
	return songListWithAlbum(s, rc, infos, "")
}

func songListWithAlbum(s *Server, rc *reqCtx, infos []*music.Info, albumID string) []M {
	out := make([]M, 0, len(infos))
	for _, in := range infos {
		out = append(out, s.songObjWithAlbum(rc, in, albumID))
	}
	return out
}

// dedupe 按 TrackID 去重
func dedupe(infos []*music.Info) []*music.Info {
	seen := map[string]bool{}
	out := infos[:0:0]
	for _, in := range infos {
		id := in.TrackID()
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, in)
	}
	return out
}

// libraryTracks 用户"资料库"= 收藏歌曲 + 歌单歌曲 + 最近播放
func (s *Server) libraryTracks(rc *reqCtx, userID int64) []*music.Info {
	ctx := rc.ctx
	var ids []string
	for id := range rc.starred {
		if strings.HasPrefix(id, music.KindTrack+"-") {
			ids = append(ids, id)
		}
	}
	pls, _ := s.DB.ListPlaylists(ctx, userID)
	for _, p := range pls {
		full, err := s.DB.GetPlaylist(ctx, p.ID)
		if err == nil {
			ids = append(ids, full.TrackIDs...)
		}
	}
	recent, _ := s.DB.RecentTracks(ctx, userID, 200)
	ids = append(ids, recent...)
	seen := map[string]bool{}
	uniq := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	return s.Catalog.Tracks(ctx, uniq)
}

func sortByName(groups []*artistGroup) {
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
}

// indexLetter 索引字母：英文取首字母，中文归入 "#"（简化处理）
func indexLetter(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "#"
	}
	c := name[0]
	switch {
	case c >= 'a' && c <= 'z':
		return strings.ToUpper(string(c))
	case c >= 'A' && c <= 'Z':
		return string(c)
	}
	return "#"
}
