package subsonic

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lxsc/internal/music"
)

// pickQuality 根据 maxBitRate/format 与用户默认音质决定请求音质
func pickQuality(r *http.Request, userQuality string) string {
	if q := param(r, "quality"); q != "" && music.QualityRank(q) > 0 {
		return q
	}
	q := userQuality
	if q == "" {
		q = "320k"
	}
	maxBR := paramInt(r, "maxBitRate", 0)
	format := strings.ToLower(param(r, "format"))
	if format == "mp3" && music.QualityRank(q) > music.QualityRank("320k") {
		q = "320k"
	}
	if maxBR > 0 {
		switch {
		case maxBR <= 128:
			q = "128k"
		case maxBR <= 320 && music.QualityRank(q) > music.QualityRank("320k"):
			q = "320k"
		}
	}
	return q
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	s.serveMedia(w, r, true)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	s.serveMedia(w, r, false)
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, persistPlayback bool) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	id := param(r, "id")
	in, err := s.Catalog.Track(rc.ctx, id)
	if err != nil {
		writeErr(w, r, ErrNotFound, err.Error())
		return
	}
	quality := pickQuality(r, u.Quality)
	ctx, cancel := context.WithTimeout(rc.ctx, 45*time.Second)
	defer cancel()
	res, err := s.Catalog.ResolveURL(ctx, in, quality)
	if err != nil {
		s.Log.Warn("获取直链失败", "id", id, "quality", quality, "err", err)
		writeErr(w, r, ErrGeneric, "无法获取播放地址: "+err.Error())
		return
	}
	persist := func() {
		if !persistPlayback {
			return
		}
		if err := s.Catalog.RememberSync(rc.ctx, []*music.Info{in}); err != nil {
			s.Log.Warn("持久化播放歌曲元数据失败", "id", id, "err", err)
		}
	}
	s.Log.Info("播放", "user", u.Name, "song", in.Name(), "singer", in.Singer(), "source", in.Source(), "quality", res.Quality, "via", res.Source)
	if s.Settings.Get().StreamMode == "proxy" || param(r, "proxy") == "1" {
		if s.proxyStream(w, r, in, res.URL, res.Quality) {
			persist()
		}
		return
	}
	// 重定向成功即可交由客户端直连上游，此时记录播放。
	persist()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, res.URL, http.StatusFound)
}

// refererFor 为部分平台的 CDN 添加 Referer/UA
func refererFor(source string) (referer, ua string) {
	ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	switch source {
	case "wy":
		referer = "https://music.163.com/"
	case "tx":
		referer = "https://y.qq.com/"
	case "kw":
		referer = "https://www.kuwo.cn/"
	case "kg":
		referer = "https://www.kugou.com/"
	case "mg":
		referer = "https://music.migu.cn/"
	}
	return
}

func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, in *music.Info, url, quality string) bool {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeErr(w, r, ErrGeneric, err.Error())
		return false
	}
	ref, ua := refererFor(in.Source())
	req.Header.Set("User-Agent", ua)
	if ref != "" {
		req.Header.Set("Referer", ref)
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		writeErr(w, r, ErrGeneric, "上游请求失败: "+err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		writeErr(w, r, ErrGeneric, "上游返回 "+strconv.Itoa(resp.StatusCode))
		return false
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Content-Type") == "" || strings.HasPrefix(w.Header().Get("Content-Type"), "application/octet") {
		_, _, ct := music.QualityMeta(quality)
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err == nil
}

func (s *Server) getCoverArt(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	var url string
	p, ok := music.ParseID(id)
	if ok {
		switch p.Kind {
		case music.KindTrack:
			if in, err := s.Catalog.Track(rc.ctx, id); err == nil {
				url = s.Catalog.Cover(rc.ctx, in)
			}
		case music.KindOnlineAlbum:
			if meta, ok := s.Catalog.CachedAlbumMeta(id); ok && strings.HasPrefix(meta.Image, "http") {
				url = meta.Image
			} else if locator, parsed := music.ParseOnlineAlbumID(id); parsed && strings.HasPrefix(locator.Image, "http") {
				url = locator.Image
			} else if infos, ok := s.Catalog.CachedAlbum(id); ok && len(infos) > 0 {
				url = s.Catalog.Cover(rc.ctx, infos[0])
			} else if _, _, _, raw, err := s.DB.GetAlbum(rc.ctx, id); err == nil {
				var ids []string
				if jsonUnmarshal(raw, &ids) == nil && len(ids) > 0 {
					if tracks := s.Catalog.Tracks(rc.ctx, ids[:1]); len(tracks) > 0 {
						url = s.Catalog.Cover(rc.ctx, tracks[0])
					}
				}
			}
		case music.KindAlbum:
			// 封面请求不应为了找一张图片触发整张专辑扫描。
			if meta, ok := s.Catalog.CachedAlbumMeta(id); ok && strings.HasPrefix(meta.Image, "http") {
				url = meta.Image
			} else if infos, ok := s.Catalog.CachedAlbum(id); ok && len(infos) > 0 {
				url = s.Catalog.Cover(rc.ctx, infos[0])
			} else if _, parsed := music.ParseID(id); parsed {
				if _, _, _, raw, err := s.DB.GetAlbum(rc.ctx, id); err == nil {
					var ids []string
					if jsonUnmarshal(raw, &ids) == nil && len(ids) > 0 {
						if tracks := s.Catalog.Tracks(rc.ctx, ids[:1]); len(tracks) > 0 {
							url = s.Catalog.Cover(rc.ctx, tracks[0])
						}
					}
				}
			}
		case music.KindArtist:
			if g := s.artistByID(rc, id); g != nil && len(g.Songs) > 0 {
				url = s.Catalog.Cover(rc.ctx, g.Songs[0])
			}
		case music.KindBoard:
			// 榜单目录项没有稳定封面；不要为封面请求隐式扫描榜单歌曲。
		case music.KindPlaylist:
			if pl, err := s.DB.GetPlaylist(rc.ctx, id); err == nil && canReadPlaylist(currentUser(r), pl) && len(pl.TrackIDs) > 0 {
				if infos := s.Catalog.Tracks(rc.ctx, pl.TrackIDs[:1]); len(infos) > 0 {
					url = s.Catalog.Cover(rc.ctx, infos[0])
				}
			}
		}
	} else if strings.HasPrefix(id, "http") {
		url = id
	}
	if url == "" {
		http.Error(w, "cover not found", http.StatusNotFound)
		return
	}
	if strings.HasPrefix(url, "http://") {
		url = "https://" + strings.TrimPrefix(url, "http://")
	}
	if s.Settings.Get().CoverMode == "proxy" {
		s.proxyCover(w, r, url)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *Server) proxyCover(w http.ResponseWriter, r *http.Request, url string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		http.Error(w, "upstream "+resp.Status, http.StatusBadGateway)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}
