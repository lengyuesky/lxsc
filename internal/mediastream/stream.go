package mediastream

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
	"lxsc/internal/music"
	"lxsc/internal/playback"
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
	u := currentUser(r)
	if u == nil {
		fail(w, r, ErrWrongAuth, "未登录或会话已过期")
		return
	}
	id := param(r, "id")
	quality := PickQuality(r, u.Quality)
	mode := s.Settings.Get().StreamMode
	var trace *playback.Trace
	if persistPlayback && !headersOnly(r.Context()) && s.Catalog.Playback != nil {
		r, trace = s.Catalog.Playback.Begin(r, u, id, quality, mode)
		defer trace.Finish()
	}
	originalFail := fail
	fail = func(w http.ResponseWriter, r *http.Request, code int, message string) {
		trace.Outcome("failed", 0)
		originalFail(w, r, code, message)
	}
	ctx, cancel := context.WithTimeout(r.Context(), mediaRecoveryTimeout)
	defer cancel()
	release, err := s.Catalog.AcquireRequest(ctx, u.ID)
	if err != nil {
		if r.Context().Err() == nil {
			fail(w, r, ErrBusy, admission.ErrBusy.Error())
		}
		return
	}
	defer release()
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
	trace.Identify(db.PlaybackTrack{ID: id, Name: in.Name(), Singer: in.Singer(), Platform: in.Source()})
	// 普通模式在本站完成 HEAD 预检，避免客户端继续向只支持 GET 的音源发 HEAD。
	// 强制 302 仍遵守管理员选择的重定向契约。
	headMetadata := headOnly && mode != "force_redirect"
	// 强制 302 优先于客户端的 proxy 参数，播放和下载均禁止服务器转发。
	proxy := mode != "force_redirect" && (mode == "proxy" || param(r, "proxy") == "1")
	if abort, _ := r.Context().Value(abortTransferKey{}).(bool); abort && !proxy && nonzeroMediaRange(r) {
		if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
			event.Error = "invalid_response"
		}
		fail(w, r, ErrGeneric, "直链下载中断后请重新下载整份文件")
		return
	}
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
	// 预算只覆盖音频准备与片段验证，不能截断已经开始的整首音频传输。
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
	trace.Selected(res.Platform, res.TrackID, res.Result.SourceID, res.Result.Source, res.Result.Quality, "", res.Cached)
	if abort, _ := r.Context().Value(abortTransferKey{}).(bool); abort && proxy && !headOnly && resp != nil && !downloadRangeResponseMatches(r, resp) {
		_ = resp.Body.Close()
		if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
			event.Error = "invalid_response"
		}
		fail(w, r, ErrGeneric, "下载文件已改变，请重新下载")
		return
	}
	if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
		event.Validation = "unverified"
		if res.Validated {
			event.Validation = "validated"
		}
	}
	if observe, _ := r.Context().Value(mediaDescriptionKey{}).(func(MediaDescription) error); observe != nil && resp != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) {
		length, known := fullMediaLength(resp)
		description := MediaDescription{ContentType: resp.Header.Get("Content-Type"), Length: length, KnownLength: known, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), ResourceDigest: sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", res.Result.SourceID, res.Result.Quality, res.Result.URL)))}
		if err := observe(description); err != nil {
			_ = resp.Body.Close()
			if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
				event.Error = "invalid_response"
			}
			fail(w, r, ErrGeneric, "下载文件已改变，请重新下载")
			return
		}
	}
	if headMetadata {
		s.WriteHead(w, r, res, resp)
		return
	}
	if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil && proxy {
		event.Mode = "proxy"
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
		trace.Outcome("interrupted", 0)
		return
	}
	// HEAD 只校验链接，不将预检记为播放。
	if !headOnly {
		s.Log.Info("播放", "user", u.Name, "song", in.Name(), "singer", in.Singer(), "source", res.Platform, "quality", res.Result.Quality, "via", res.Result.Source)
	}
	persist()
	w.Header().Set("Cache-Control", "no-store")
	s.mediaRequestEvent(r, "redirect", id, res.Platform, res.Result.Quality, res.Cached, http.StatusFound, time.Now(), nil)
	trace.Delivery("302")
	trace.Outcome("success", http.StatusFound)
	recordMediaResponse(r, http.StatusFound)
	http.Redirect(w, r, res.Result.URL, http.StatusFound)
}

// 下载器可能直接追加续传字节，不能把忽略 Range 的 200 响应当作可追加片段。
func downloadRangeResponseMatches(r *http.Request, response *http.Response) bool {
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return false
	}
	value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	first, last, _ := strings.Cut(value, "-")
	if first == "" || first == "0" {
		if response.StatusCode == http.StatusOK {
			return true
		}
	}
	if response.StatusCode != http.StatusPartialContent {
		return false
	}
	contentRange, ok := strings.CutPrefix(response.Header.Get("Content-Range"), "bytes ")
	span, totalValue, hasTotal := strings.Cut(contentRange, "/")
	startValue, endValue, hasEnd := strings.Cut(span, "-")
	start, startErr := strconv.ParseInt(startValue, 10, 64)
	end, endErr := strconv.ParseInt(endValue, 10, 64)
	total, totalErr := strconv.ParseInt(totalValue, 10, 64)
	wantedStart, wantedErr := strconv.ParseInt(first, 10, 64)
	if !ok || !hasTotal || !hasEnd || startErr != nil || endErr != nil || totalErr != nil || wantedErr != nil || start != wantedStart || start < 0 || end < start || total <= end {
		return false
	}
	wantedEnd := total - 1
	if last != "" {
		requestedEnd, err := strconv.ParseInt(last, 10, 64)
		if err != nil || requestedEnd < start {
			return false
		}
		wantedEnd = min(wantedEnd, requestedEnd)
	}
	if end != wantedEnd {
		return false
	}
	if length := response.Header.Get("Content-Length"); length != "" {
		count, err := strconv.ParseInt(length, 10, 64)
		if err != nil || count != end-start+1 {
			return false
		}
	}
	if start == 0 {
		return true
	}
	validator := r.Header.Get("If-Range")
	if strings.HasPrefix(validator, `"`) {
		return response.Header.Get("ETag") == validator
	}
	expected, err := http.ParseTime(validator)
	actual, actualErr := http.ParseTime(response.Header.Get("Last-Modified"))
	return err == nil && actualErr == nil && expected.Equal(actual)
}

func nonzeroMediaRange(r *http.Request) bool {
	value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	first, _, _ := strings.Cut(value, "-")
	return first != "" && first != "0"
}

// MediaEvent 只向独立结构化缓冲写入白名单，不输出签名地址或脚本身份。
func (s *Service) MediaEvent(stage, id, platform, quality string, cached bool, status int, started time.Time, err error) {
	s.Diagnostics.Add(diagnostics.Event{Stage: stage, TrackID: id, Platform: platform, Quality: quality, Mode: s.Settings.Get().StreamMode, Cached: cached, Status: status, Error: diagnostics.ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds()})
}

func (s *Service) mediaRequestEvent(r *http.Request, stage, id, platform, quality string, cached bool, status int, started time.Time, err error) {
	e := diagnostics.Event{Stage: stage, TrackID: id, Platform: platform, Quality: quality, Mode: s.Settings.Get().StreamMode, Cached: cached, Status: status, Error: diagnostics.ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds()}
	if strings.HasPrefix(stage, "proxy_") {
		e.Mode = "proxy"
	}
	if parent, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); parent != nil {
		e.RequestID = parent.RequestID
	}
	s.Diagnostics.Add(e)
	playback.FromContext(r.Context()).Add(db.PlaybackEvent{At: time.Now().UnixMilli(), Stage: stage, Platform: platform, Quality: quality, Cached: cached, Status: status, Error: e.Error, ElapsedMS: e.ElapsedMS})
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

// inspectMedia 验证有界音频片段后关闭正文，保留真实媒体头供 HEAD/直连使用。
// 授权的只取头调试沿用最小 Range，并明确不作可播放验证。
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
	if headersOnly(ctx) || s.Catalog.AudioCheck != nil {
		req.Header.Set("Range", "bytes=0-0")
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", httpguard.AudioSampleLimit-1))
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	err = s.verifyMedia(ctx, resp)
	_ = resp.Body.Close()
	resp.Body = http.NoBody
	return resp, err
}

func (s *Service) verifyMedia(ctx context.Context, response *http.Response) error {
	if err := httpguard.CheckMediaResponse(response); err != nil {
		return err
	}
	if headersOnly(ctx) {
		return nil
	}
	if s.Catalog.AudioCheck != nil {
		return s.Catalog.AudioCheck(ctx, response)
	}
	return httpguard.VerifyAudio(ctx, response)
}

func (s *Service) checkMedia(ctx context.Context, resolution music.URLResolution, in *music.Info) (*http.Response, error) {
	headerOnly := headersOnly(ctx)
	check := func(checkCtx context.Context) (*http.Response, error) {
		// 共享缓存工作有独立取消上下文，显式保留本次验证契约。
		if headerOnly {
			checkCtx = HeaderProbe(checkCtx)
		}
		return s.inspectMedia(checkCtx, in, resolution.Result.URL)
	}
	if headerOnly {
		return s.Catalog.CheckPlaybackURL(ctx, resolution, check)
	}
	return s.Catalog.CheckPlaybackAudio(ctx, resolution, check)
}

func (s *Service) verifyProxyMedia(ctx context.Context, in *music.Info, resolution music.URLResolution, response *http.Response) error {
	if headersOnly(ctx) || s.Catalog.AudioCheck != nil || httpguard.MediaStartsAtZero(response) || (response.StatusCode != 200 && response.StatusCode != 206) {
		return s.verifyMedia(ctx, response)
	}
	// 拖动得到的片段不含文件头，先校验同版本音频起始片段，不能把中间字节误判成坏文件。
	checked, err := s.checkMedia(ctx, resolution, in)
	if err != nil {
		return err
	}
	if checked == nil {
		return httpguard.ErrInvalidAudio
	}
	if contentType := checked.Header.Get("Content-Type"); contentType != "" {
		response.Header.Set("Content-Type", contentType)
	}
	return httpguard.VerifyMediaReadable(ctx, response)
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
	req.Header.Set("Accept-Encoding", "identity")
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
	trace := playback.FromContext(r.Context())
	trace.Delivery("proxy")
	if r.Method == http.MethodHead {
		return false
	}
	copyStarted := time.Now()
	upstream := &mediaReadStatus{ReadCloser: resp.Body}
	_, err := httpguard.CopyIdle(w, upstream, 30*time.Second)
	s.mediaRequestEvent(r, "proxy_copy", in.TrackID(), resolution.Platform, resolution.Result.Quality, resolution.Cached, resp.StatusCode, copyStarted, err)
	if err != nil || r.Context().Err() != nil {
		trace.Outcome("interrupted", resp.StatusCode)
		if event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); event != nil {
			event.Result = "failed"
			if err == nil {
				err = r.Context().Err()
			}
			event.Error = diagnostics.ErrorCode(err)
		}
		if err != nil && upstream.failed.Load() && r.Context().Err() == nil && s.Catalog != nil {
			s.Catalog.InvalidatePlaybackURL(resolution)
			s.Catalog.ReportPlaybackOutcome(resolution, in.Source(), false, time.Since(copyStarted).Milliseconds())
		}
		s.Log.Info("代理传输中断", "id", in.TrackID())
		if abort, _ := r.Context().Value(abortTransferKey{}).(bool); abort {
			panic(http.ErrAbortHandler)
		}
		return false
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		trace.Outcome("failed", resp.StatusCode)
		return false
	}
	trace.Outcome("success", resp.StatusCode)
	s.Log.Info("播放", "user", currentUser(r).Name, "song", in.Name(), "singer", in.Singer(), "source", resolution.Platform, "quality", resolution.Result.Quality, "via", resolution.Result.Source)
	return true
}

// 下游写出失败不应降低音源评分；只记录上游音频读取的明确错误。
type mediaReadStatus struct {
	io.ReadCloser
	failed atomic.Bool
}

func (r *mediaReadStatus) Read(data []byte) (int, error) {
	n, err := r.ReadCloser.Read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		r.failed.Store(true)
	}
	return n, err
}
