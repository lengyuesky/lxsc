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

type mediaErrorWriter func(http.ResponseWriter, *http.Request, int, string)

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, persistPlayback bool) {
	s.serveMediaWithError(w, r, persistPlayback, writeErr)
}

func (s *Server) serveMediaWithError(w http.ResponseWriter, r *http.Request, persistPlayback bool, fail mediaErrorWriter) {
	rc := s.newReqCtx(r)
	u := currentUser(r)
	id := param(r, "id")
	in, err := s.Catalog.Track(rc.ctx, id)
	if err != nil {
		fail(w, r, ErrNotFound, err.Error())
		return
	}
	quality := pickQuality(r, u.Quality)
	remaining := 45 * time.Second
	resolve := func(failed *music.URLResolution) (music.URLResolution, error) {
		started := time.Now()
		ctx, cancel := context.WithTimeout(rc.ctx, remaining)
		defer func() {
			cancel()
			remaining -= time.Since(started)
		}()
		if failed != nil {
			return s.Catalog.RefreshPlaybackURL(ctx, in, *failed)
		}
		return s.Catalog.ResolvePlaybackURL(ctx, in, quality)
	}
	res, err := resolve(nil)
	if err != nil {
		s.Log.Warn("获取直链失败", "id", id, "quality", quality)
		if r.Context().Err() == nil {
			fail(w, r, ErrGeneric, "无法获取播放地址")
		}
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
	mode := s.Settings.Get().StreamMode
	// 强制 302 优先于客户端的 proxy 参数，播放和下载均禁止服务器转发。
	if mode != "force_redirect" && (mode == "proxy" || param(r, "proxy") == "1") {
		if s.proxyStream(w, r, in, res, func(failed music.URLResolution) (music.URLResolution, error) { return resolve(&failed) }, fail) {
			persist()
		}
		return
	}
	if res.Cached {
		status, checkErr := s.Catalog.CheckPlaybackURL(rc.ctx, res, func(ctx context.Context) (int, error) {
			return s.checkMedia(ctx, in, res.Result.URL)
		})
		if rc.ctx.Err() != nil {
			return
		}
		if checkErr == nil && expiredMediaStatus(status) {
			s.Log.Info("缓存直链失效，尝试刷新", "id", id, "status", status)
			res, err = resolve(&res)
			if err != nil {
				s.Log.Warn("直链刷新失败", "id", id)
				if rc.ctx.Err() == nil {
					fail(w, r, ErrGeneric, "重新获取播放地址失败")
				}
				return
			}
		} else if checkErr != nil || (status != http.StatusOK && status != http.StatusPartialContent) {
			// 校验网络与客户端网络可能不同，不能据此删除直链或阻止客户端尝试。
			s.Log.Info("直链校验结果不确定，继续客户端直连", "id", id, "status", status)
		} else {
			s.Log.Debug("缓存直链校验成功", "id", id, "status", status)
		}
	}
	if rc.ctx.Err() != nil {
		return
	}
	// 重定向成功即可交由客户端直连上游，此时记录播放。
	s.Log.Info("播放", "user", u.Name, "song", in.Name(), "singer", in.Singer(), "source", in.Source(), "quality", res.Result.Quality, "via", res.Result.Source)
	persist()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, res.Result.URL, http.StatusFound)
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

func expiredMediaStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone
}

// checkMedia 使用最小 Range 校验，不读取或转发音频正文，也不继承客户端凭据。
func (s *Server) checkMedia(ctx context.Context, in *music.Info, address string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, err
	}
	ref, ua := refererFor(in.Source())
	req.Header.Set("User-Agent", ua)
	if ref != "" {
		req.Header.Set("Referer", ref)
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// openMedia 只请求响应头，不向客户端提交内容，为限次恢复保留空间。
func (s *Server) openMedia(r *http.Request, in *music.Info, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	ref, ua := refererFor(in.Source())
	req.Header.Set("User-Agent", ua)
	if ref != "" {
		req.Header.Set("Referer", ref)
	}
	for _, h := range []string{"Range", "If-Range"} {
		if value := r.Header.Get(h); value != "" {
			req.Header.Set(h, value)
		}
	}
	return s.HTTP.Do(req)
}

func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, in *music.Info, resolution music.URLResolution, refresh func(music.URLResolution) (music.URLResolution, error), fail mediaErrorWriter) bool {
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		if r.Context().Err() != nil {
			return false
		}
		var err error
		resp, err = s.openMedia(r, in, resolution.Result.URL)
		if err != nil {
			// http 错误可能包含签名 URL，只记录阶段，不输出原始错误。
			s.Log.Warn("代理上游请求失败", "id", in.TrackID(), "attempt", attempt+1)
			if r.Context().Err() == nil {
				fail(w, r, ErrGeneric, "上游请求失败")
			}
			return false
		}
		if attempt == 0 && expiredMediaStatus(resp.StatusCode) {
			_ = resp.Body.Close()
			s.Log.Info("直链失效，尝试刷新", "id", in.TrackID(), "status", resp.StatusCode)
			resolution, err = refresh(resolution)
			if err != nil {
				s.Log.Warn("直链刷新失败", "id", in.TrackID())
				if r.Context().Err() == nil {
					fail(w, r, ErrGeneric, "重新获取播放地址失败")
				}
				return false
			}
			continue
		}
		break
	}
	defer resp.Body.Close()
	if r.Context().Err() != nil {
		return false
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		s.Log.Warn("代理上游响应失败", "id", in.TrackID(), "status", resp.StatusCode)
		fail(w, r, ErrGeneric, "上游返回 "+strconv.Itoa(resp.StatusCode))
		return false
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable && (w.Header().Get("Content-Type") == "" || strings.HasPrefix(w.Header().Get("Content-Type"), "application/octet")) {
		_, _, ct := music.QualityMeta(resolution.Result.Quality)
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, err := io.Copy(w, resp.Body)
	if err != nil || r.Context().Err() != nil {
		s.Log.Info("代理传输中断", "id", in.TrackID())
		return false
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return false
	}
	s.Log.Info("播放", "user", currentUser(r).Name, "song", in.Name(), "singer", in.Singer(), "source", in.Source(), "quality", resolution.Result.Quality, "via", resolution.Result.Source)
	return true
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
