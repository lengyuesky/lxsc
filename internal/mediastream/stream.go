package mediastream

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

// PickQuality 根据 maxBitRate/format 与用户默认音质决定请求音质。
func PickQuality(r *http.Request, userQuality string) string {
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

func (s *Service) Serve(w http.ResponseWriter, r *http.Request, persistPlayback bool, fail ErrorWriter) {
	headOnly := r.Method == http.MethodHead
	persistPlayback = persistPlayback && !headOnly
	ctx, cancel := context.WithTimeout(r.Context(), mediaRecoveryTimeout)
	defer cancel()
	u := currentUser(r)
	if u == nil {
		fail(w, r, ErrWrongAuth, "未登录或会话已过期")
		return
	}
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
	s.mediaRequestEvent(r, "metadata", id, "", "", false, 0, metadataStarted, err)
	if err != nil {
		if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
			event.Error = diagnostics.ErrorCode(err)
		}
		if errors.Is(err, admission.ErrBusy) {
			fail(w, r, ErrBusy, err.Error())
			return
		}
		if r.Context().Err() == nil {
			fail(w, r, ErrNotFound, err.Error())
		}
		return
	}
	quality := PickQuality(r, u.Quality)
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
	res, resp, err := s.Prepare(ctx, r, in, quality, proxy, headMetadata)
	release()
	// 预算只覆盖取得可用响应头之前的工作，不能截断已经开始的整首音频传输。
	cancel()
	if err != nil {
		if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
			event.Error = diagnostics.ErrorCode(err)
		}
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
	if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
		event.Validation = "unverified"
		if res.Validated {
			event.Validation = "validated"
		}
	}
	if headMetadata {
		s.WriteHead(w, r, res, resp)
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
		if s.ProxyStream(w, r, in, res, resp) {
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
	s.mediaRequestEvent(r, "redirect", id, in.Source(), res.Result.Quality, res.Cached, http.StatusFound, time.Now(), nil)
	recordMediaResponse(r, http.StatusFound)
	http.Redirect(w, r, res.Result.URL, http.StatusFound)
}

// MediaEvent 只向独立结构化缓冲写入白名单，不输出签名地址或脚本身份。
func (s *Service) MediaEvent(stage, id, platform, quality string, cached bool, status int, started time.Time, err error) {
	s.Diagnostics.Add(diagnostics.Event{Stage: stage, TrackID: id, Platform: platform, Quality: quality, Mode: s.Settings.Get().StreamMode, Cached: cached, Status: status, Error: diagnostics.ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds()})
}

func (s *Service) mediaRequestEvent(r *http.Request, stage, id, platform, quality string, cached bool, status int, started time.Time, err error) {
	e := diagnostics.Event{Stage: stage, TrackID: id, Platform: platform, Quality: quality, Mode: s.Settings.Get().StreamMode, Cached: cached, Status: status, Error: diagnostics.ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds()}
	if parent, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); parent != nil {
		e.RequestID = parent.RequestID
	}
	s.Diagnostics.Add(e)
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

// inspectMedia 在响应头到达后立即关闭正文，保留真实媒体头供 HEAD 预检使用。
func (s *Service) inspectMedia(ctx context.Context, in *music.Info, address string) (*http.Response, error) {
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
func (s *Service) openMedia(ctx context.Context, r *http.Request, in *music.Info, address string) (*http.Response, error) {
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
func (s *Service) ProxyStream(w http.ResponseWriter, r *http.Request, in *music.Info, resolution music.URLResolution, resp *http.Response) bool {
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
	s.mediaRequestEvent(r, "proxy_copy", in.TrackID(), in.Source(), resolution.Result.Quality, resolution.Cached, resp.StatusCode, copyStarted, err)
	if err != nil || r.Context().Err() != nil {
		if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
			event.Result = "failed"
			if err == nil {
				err = r.Context().Err()
			}
			event.Error = diagnostics.ErrorCode(err)
		}
		s.Log.Info("代理传输中断", "id", in.TrackID())
		return false
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return false
	}
	s.Log.Info("播放", "user", currentUser(r).Name, "song", in.Name(), "singer", in.Singer(), "source", in.Source(), "quality", resolution.Result.Quality, "via", resolution.Result.Source)
	return true
}
