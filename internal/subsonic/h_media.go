package subsonic

import (
	"context"
	"net/http"
	"strings"
	"time"

	"lxsc/internal/music"
)

func (s *Server) getCoverArt(w http.ResponseWriter, r *http.Request) {
	w, finishCover := captureCoverResponse(w, r)
	defer finishCover()
	customURL := s.Settings.Get().CustomCoverURL
	ctx, cancel := context.WithTimeout(r.Context(), coverRequestTimeout)
	defer cancel()
	rc := s.newReqCtx(r.WithContext(ctx))
	id := param(r, "id")
	var url string
	var fields map[string]string
	coverFor := func(in *music.Info) string {
		fields = in.TemplateValues()
		ctx := rc.ctx
		cancel := func() {}
		if customURL != "" {
			ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		}
		defer cancel()
		return s.Catalog.Cover(ctx, in)
	}
	p, ok := music.ParseID(id)
	if ok {
		switch p.Kind {
		case music.KindTrack:
			if in, err := s.Catalog.Track(rc.ctx, id); err == nil {
				url = coverFor(in)
			}
		case music.KindOnlineAlbum:
			if meta, ok := s.Catalog.CachedAlbumMeta(id); ok && strings.HasPrefix(meta.Image, "http") {
				url = meta.Image
			} else if locator, parsed := music.ParseOnlineAlbumID(id); parsed && strings.HasPrefix(locator.Image, "http") {
				url = locator.Image
			} else if infos, ok := s.Catalog.CachedAlbum(id); ok && len(infos) > 0 {
				url = coverFor(infos[0])
			} else if _, _, _, raw, err := s.DB.GetAlbum(rc.ctx, id); err == nil {
				var ids []string
				if jsonUnmarshal(raw, &ids) == nil && len(ids) > 0 {
					if tracks := s.Catalog.Tracks(rc.ctx, ids[:1]); len(tracks) > 0 {
						url = coverFor(tracks[0])
					}
				}
			}
		case music.KindAlbum:
			// 封面请求不应为了找一张图片触发整张专辑扫描。
			if meta, ok := s.Catalog.CachedAlbumMeta(id); ok && strings.HasPrefix(meta.Image, "http") {
				url = meta.Image
			} else if infos, ok := s.Catalog.CachedAlbum(id); ok && len(infos) > 0 {
				url = coverFor(infos[0])
			} else if _, parsed := music.ParseID(id); parsed {
				if _, _, _, raw, err := s.DB.GetAlbum(rc.ctx, id); err == nil {
					var ids []string
					if jsonUnmarshal(raw, &ids) == nil && len(ids) > 0 {
						if tracks := s.Catalog.Tracks(rc.ctx, ids[:1]); len(tracks) > 0 {
							url = coverFor(tracks[0])
						}
					}
				}
			}
		case music.KindArtist:
			if g := s.artistByID(rc, id); g != nil && len(g.Songs) > 0 {
				url = coverFor(g.Songs[0])
			}
		case music.KindSingerDir:
			if locator, parsed := music.ParseSingerDirectoryID(id); parsed {
				if ref, ok := s.Catalog.KnownArtistRef(locator.Source, locator.Name); ok {
					url = ref.Avatar
				}
			}
		case music.KindBoard:
			// 列表和详情都公布榜单 ID 作为 coverArt。没有平台独立封面时
			// 返回本站内置榜单图，不为一张封面扫描歌曲或依赖外部图源。
			if music.IsPlatform(p.Source) && strings.TrimSpace(p.Key) != "" && p.Key != "0" {
				recordCoverOutcome(r, "board", false, nil)
				serveBoardCover(w, r)
				return
			}
		case music.KindPlaylist:
			if pl, err := s.DB.GetPlaylist(rc.ctx, id); err == nil && canReadPlaylist(currentUser(r), pl) && len(pl.TrackIDs) > 0 {
				if infos := s.Catalog.Tracks(rc.ctx, pl.TrackIDs[:1]); len(infos) > 0 {
					url = coverFor(infos[0])
				}
			}
		}
	} else if strings.HasPrefix(id, "http") {
		url = id
	}
	if fields == nil && ok {
		// 专辑／歌手已有的摘要也可用于回退，不为封面扫描完整歌曲列表。
		switch p.Kind {
		case music.KindOnlineAlbum, music.KindAlbum:
			if meta, found := s.Catalog.CachedAlbumMeta(id); found {
				fields = map[string]string{"album": meta.Name, "artist": meta.Artist, "source": meta.Source}
			} else if locator, parsed := music.ParseOnlineAlbumID(id); parsed {
				fields = map[string]string{"album": locator.Name, "artist": locator.Artist, "source": locator.Source}
			}
		case music.KindSingerDir:
			if locator, parsed := music.ParseSingerDirectoryID(id); parsed {
				fields = map[string]string{"artist": locator.Name, "source": locator.Source}
			}
		}
	}
	if fields != nil && ok {
		switch p.Kind {
		case music.KindOnlineAlbum, music.KindAlbum:
			delete(fields, "title")
		case music.KindArtist, music.KindSingerDir:
			delete(fields, "title")
			delete(fields, "album")
		}
	}
	if strings.HasPrefix(url, "http://") {
		url = "https://" + strings.TrimPrefix(url, "http://")
	}
	if customURL != "" && fields != nil {
		s.serveCover(ctx, w, r, url, customURL, fields)
		return
	}
	if url == "" {
		if fields != nil {
			s.serveCover(ctx, w, r, "", "", fields)
			return
		}
		http.Error(w, "cover not found", http.StatusNotFound)
		return
	}
	if s.Settings.Get().CoverMode == "proxy" {
		s.serveCover(ctx, w, r, url, "", fields)
		return
	}
	recordCoverOutcome(r, "redirect", false, nil)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.Redirect(w, r, url, http.StatusFound)
}
