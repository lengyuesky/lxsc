package subsonic

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

func (s *Server) star(w http.ResponseWriter, r *http.Request) {
	s.setStar(w, r, true)
}

func (s *Server) unstar(w http.ResponseWriter, r *http.Request) {
	s.setStar(w, r, false)
}

func (s *Server) setStar(w http.ResponseWriter, r *http.Request, on bool) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	ids := append(append(params(r, "id"), params(r, "albumId")...), params(r, "artistId")...)
	for _, id := range ids {
		if id == "" {
			continue
		}
		if on {
			kind := kindOf(id)
			switch kind {
			case "track":
				in, err := s.Catalog.Track(rc.ctx, id)
				if err != nil {
					writeErr(w, r, ErrGeneric, "读取收藏歌曲失败")
					return
				}
				if err := s.Catalog.RememberSync(rc.ctx, []*music.Info{in}); err != nil {
					s.Log.Warn("持久化收藏歌曲元数据失败", "id", id, "err", err)
					writeErr(w, r, ErrGeneric, "读取或保存收藏信息失败")
					return
				}
			case "album":
				var g *albumGroup
				if _, parsed := music.ParseOnlineAlbumID(id); parsed {
					g = s.onlineAlbumGroup(rc, id)
				} else {
					g = s.albumByID(rc, id)
				}
				if g == nil {
					writeErr(w, r, ErrGeneric, "读取或保存收藏信息失败")
					return
				}
				if err := s.Catalog.RememberSync(rc.ctx, g.Songs); err != nil {
					s.Log.Warn("持久化收藏专辑歌曲失败", "id", id, "err", err)
					writeErr(w, r, ErrGeneric, "读取或保存收藏信息失败")
					return
				}
				ids := make([]string, 0, len(g.Songs))
				for _, in := range g.Songs {
					ids = append(ids, in.TrackID())
				}
				js, _ := jsonMarshal(ids)
				if err := s.DB.UpsertAlbum(rc.ctx, id, g.Source, g.Name, g.Artist, js); err != nil {
					writeErr(w, r, ErrGeneric, "读取或保存收藏信息失败")
					return
				}
			case "artist":
				name, ok := s.Catalog.CachedArtist(id)
				if locator, parsed := music.ParseSingerDirectoryID(id); parsed {
					name = locator.Name
					ok = true
				} else if _, artistName, parsed := music.ParseArtistNameID(id); parsed {
					name = artistName
					ok = true
				}
				if !ok {
					if _, storedName, _, err := s.DB.GetArtist(rc.ctx, id); err == nil {
						name = storedName
					}
				}
				if name == "" {
					writeErr(w, r, ErrGeneric, "读取或保存收藏信息失败")
					return
				}
				js, _ := jsonMarshal(M{"name": name})
				if err := s.DB.UpsertArtist(rc.ctx, id, "", name, js); err != nil {
					writeErr(w, r, ErrGeneric, "读取或保存收藏信息失败")
					return
				}
			}
			if err := s.DB.Star(rc.ctx, u.ID, id, kind); err != nil {
				writeErr(w, r, ErrGeneric, "保存收藏失败")
				return
			}
		} else {
			if err := s.DB.Unstar(rc.ctx, u.ID, id); err != nil {
				writeErr(w, r, ErrGeneric, "取消收藏失败")
				return
			}
		}
	}
	writeOK(w, r, "", nil)
}

func (s *Server) getStarred(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	items, err := s.DB.ListStarred(rc.ctx, u.ID, "")
	if err != nil {
		writeErr(w, r, ErrGeneric, "读取收藏失败")
		return
	}
	var trackIDs []string
	var albums []M
	var artists []M
	for _, it := range items {
		switch it.Kind {
		case "album":
			var g *albumGroup
			if _, parsed := music.ParseOnlineAlbumID(it.ItemID); parsed {
				g = s.onlineAlbumGroup(rc, it.ItemID)
			} else {
				g = s.albumByID(rc, it.ItemID)
			}
			if g != nil {
				albums = append(albums, s.albumObj(rc, g))
			}
		case "artist":
			if _, name, _, err := s.DB.GetArtist(rc.ctx, it.ItemID); err == nil {
				obj := M{"id": it.ItemID, "name": name, "coverArt": it.ItemID, "albumCount": 0, "starred": fmtTime(it.CreatedAt)}
				artists = append(artists, obj)
			}
		default:
			trackIDs = append(trackIDs, it.ItemID)
		}
	}
	songs := s.Catalog.Tracks(rc.ctx, trackIDs)
	name := "starred2"
	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, ".view"), "getStarred") {
		name = "starred"
	}
	if albums == nil {
		albums = []M{}
	}
	if artists == nil {
		artists = []M{}
	}
	writeOK(w, r, name, M{"song": songList(s, rc, songs), "album": albums, "artist": artists})
}

func (s *Server) scrobble(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	submission := param(r, "submission")
	if submission == "false" {
		id, client := param(r, "id"), param(r, "c")
		if len(client) > 200 {
			writeErr(w, r, ErrGeneric, "客户端名称过长")
			return
		}
		if parsed, ok := music.ParseID(id); !ok || parsed.Kind != music.KindTrack || !music.IsPlatform(parsed.Source) || parsed.Key == "" || len(id) > 1024 {
			writeErr(w, r, ErrGeneric, "歌曲ID无效")
			return
		}
		if err := s.DB.SetNowPlaying(r.Context(), u.ID, client, id); err != nil {
			writeErr(w, r, ErrGeneric, "保存正在播放失败")
			return
		}

		writeOK(w, r, "", nil)
		return
	}
	ids := params(r, "id")
	times := params(r, "time")
	for i, id := range ids {
		if !strings.HasPrefix(id, music.KindTrack+"-") {
			continue
		}
		now := time.Now()
		playedAt := now.UnixMilli()
		hasTimestamp, validTimestamp := false, true
		if i < len(times) {
			ms, err := strconv.ParseInt(times[i], 10, 64)
			validTimestamp = err == nil && ms > 0
			if validTimestamp {
				playedAt, hasTimestamp = ms, true
			}
		}
		track := db.ListeningTrack{ID: id, Name: "未知歌曲"}
		var durationMS int64
		// scrobble 可能先于 stream 到达；此时将歌曲视为实际播放并持久化元数据。
		if in, err := s.Catalog.Track(r.Context(), id); err == nil {
			track.Name, track.Singer = in.Name(), in.Singer()
			durationMS = int64(in.Duration()) * 1000
			if err := s.Catalog.RememberSync(r.Context(), []*music.Info{in}); err != nil && s.Log != nil {
				s.Log.Warn("持久化 scrobble 歌曲元数据失败", "id", id, "err", err)
			}
		}
		if validTimestamp {
			if err := s.DB.AddClientListening(r.Context(), u.ID, param(r, "c"), track, durationMS, playedAt, hasTimestamp, now); err != nil {
				if s.Log != nil {
					s.Log.Error("保存客户端听歌统计失败", "err", err)
				}
				writeErr(w, r, ErrGeneric, "保存听歌统计失败")
				return
			}
		}
		if err := s.DB.AddHistory(r.Context(), u.ID, id, playedAt/1000); err != nil {
			writeErr(w, r, ErrGeneric, "保存播放历史失败")
			return
		}
	}
	writeOK(w, r, "", nil)
}
