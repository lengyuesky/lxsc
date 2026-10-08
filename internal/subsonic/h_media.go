package subsonic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
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
	headOnly := r.Method == http.MethodHead
	persistPlayback = persistPlayback && !headOnly
	ctx, cancel := context.WithTimeout(r.Context(), mediaRecoveryTimeout)
	defer cancel()
	u := currentUser(r)
	release, err := s.Catalog.AcquireRequest(ctx, u.ID)
	if err != nil {
		if r.Context().Err() == nil {
			fail(w, r, ErrBusy, admission.ErrBusy.Error())
		}
		return
	}
	defer release()
	id := param(r, "id")
	metadataStarted := time.Now()
	in, err := s.Catalog.Track(ctx, id)
	s.mediaEvent("metadata", id, "", "", false, 0, metadataStarted, err)
	if err != nil {
		if errors.Is(err, admission.ErrBusy) {
			fail(w, r, ErrBusy, err.Error())
			return
		}
		if r.Context().Err() == nil {
			fail(w, r, ErrNotFound, err.Error())
		}
		return
	}
	quality := pickQuality(r, u.Quality)
	mode := s.Settings.Get().StreamMode
	// 普通模式在本站完成 HEAD 预检，避免客户端继续向只支持 GET 的音源发 HEAD。
	// 强制 302 仍遵守管理员选择的重定向契约。
	headMetadata := headOnly && mode != "force_redirect"
	// 强制 302 优先于客户端的 proxy 参数，播放和下载均禁止服务器转发。
	proxy := mode != "force_redirect" && (mode == "proxy" || param(r, "proxy") == "1")
	if proxy && !headMetadata {
		transferRelease, err := s.Catalog.MediaLimits.Acquire(ctx, u.ID)
		if err != nil {
			if r.Context().Err() == nil {
				fail(w, r, ErrBusy, admission.ErrBusy.Error())
			}
			return
		}
		defer transferRelease()
	}
	res, resp, err := s.prepareMedia(ctx, r, in, quality, proxy, headMetadata)
	release()
	// 预算只覆盖取得可用响应头之前的工作，不能截断已经开始的整首音频传输。
	cancel()
	if err != nil {
		if errors.Is(err, admission.ErrBusy) {
			fail(w, r, ErrBusy, err.Error())
			return
		}
		s.Log.Warn("获取可用音频失败", "id", id, "quality", quality)
		if r.Context().Err() == nil {
			fail(w, r, ErrGeneric, "无法获取可用的播放地址")
		}
		return
	}
	if headMetadata {
		s.writeMediaHead(w, r, res, resp)
		return
	}
	persist := func() {
		if !persistPlayback {
			return
		}
		if err := s.Catalog.RememberSync(r.Context(), []*music.Info{in}); err != nil {
			s.Log.Warn("持久化播放歌曲元数据失败", "id", id, "err", err)
		}
	}
	if proxy {
		if s.proxyStream(w, r, in, res, resp) {
			persist()
		}
		return
	}
	if r.Context().Err() != nil {
		return
	}
	// HEAD 只校验链接，不将预检记为播放。
	if !headOnly {
		s.Log.Info("播放", "user", u.Name, "song", in.Name(), "singer", in.Singer(), "source", in.Source(), "quality", res.Result.Quality, "via", res.Result.Source)
	}
	persist()
	w.Header().Set("Cache-Control", "no-store")
	s.mediaEvent("redirect", id, in.Source(), res.Result.Quality, res.Cached, http.StatusFound, time.Now(), nil)
	recordMediaResponse(r, http.StatusFound)
	http.Redirect(w, r, res.Result.URL, http.StatusFound)
}

// mediaEvent 只向独立结构化缓冲写入白名单，不输出签名地址或脚本身份。
func (s *Server) mediaEvent(stage, id, platform, quality string, cached bool, status int, started time.Time, err error) {
	s.Diagnostics.Add(diagnostics.Event{Stage: stage, TrackID: id, Platform: platform, Quality: quality, Mode: s.Settings.Get().StreamMode, Cached: cached, Status: status, Error: diagnostics.ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds()})
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
	resp, err := s.inspectMedia(ctx, in, address)
	if resp == nil {
		return 0, err
	}
	return resp.StatusCode, err
}

// inspectMedia 在响应头到达后立即关闭正文，保留真实媒体头供 HEAD 预检使用。
func (s *Server) inspectMedia(ctx context.Context, in *music.Info, address string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || req.URL.Host == "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return nil, errInvalidMediaURL
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
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	_ = resp.Body.Close()
	resp.Body = http.NoBody
	return resp, httpguard.CheckMediaResponse(resp)
}

// openMedia 取得响应头后解除恢复预算，但始终保留父请求取消和正文关闭的生命周期。
func (s *Server) openMedia(ctx context.Context, r *http.Request, in *music.Info, address string) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bodyCtx, cancel := context.WithCancel(r.Context())
	req, err := http.NewRequestWithContext(bodyCtx, http.MethodGet, address, nil)
	if err != nil || req.URL.Host == "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		cancel()
		return nil, errInvalidMediaURL
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
	stop := context.AfterFunc(ctx, cancel)
	resp, err := s.HTTP.Do(req)
	if !stop() {
		err = ctx.Err()
	}
	if err == nil {
		err = bodyCtx.Err()
	}
	if err != nil {
		cancel()
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	resp.Body = &mediaResponseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type mediaResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *mediaResponseBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

// proxyStream 仅提交已经选定的响应；416 或 copy 开始后的错误都不重新播放。
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, in *music.Info, resolution music.URLResolution, resp *http.Response) bool {
	defer resp.Body.Close()
	if r.Context().Err() != nil {
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
	recordMediaResponse(r, resp.StatusCode)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return false
	}
	copyStarted := time.Now()
	_, err := httpguard.CopyIdle(w, resp.Body, 30*time.Second)
	s.mediaEvent("proxy_copy", in.TrackID(), in.Source(), resolution.Result.Quality, resolution.Cached, resp.StatusCode, copyStarted, err)
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
		case music.KindSingerDir:
			if locator, parsed := music.ParseSingerDirectoryID(id); parsed {
				if ref, ok := s.Catalog.KnownArtistRef(locator.Source, locator.Name); ok {
					url = ref.Avatar
				}
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
