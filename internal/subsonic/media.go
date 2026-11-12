package subsonic

import (
	"context"
	"net/http"
	"time"

	"lxsc/internal/diagnostics"
	"lxsc/internal/mediastream"
	"lxsc/internal/music"
	"lxsc/internal/playback"
)

const (
	mediaPrepareLimit = mediastream.PrepareLimit
	mediaQualityGrace = mediastream.QualityGrace
)

type mediaErrorWriter = mediastream.ErrorWriter

func (s *Server) mediaService() *mediastream.Service {
	return &mediastream.Service{Catalog: s.Catalog, Settings: s.Settings, HTTP: s.HTTP, Log: s.Log, Diagnostics: s.Diagnostics}
}

func mediaRequest(r *http.Request) *http.Request {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	client := playback.Text(param(r, "c"), 64)
	if client == "" {
		client = "未知客户端"
	}
	r = playback.BindDefault(r, playback.Client{Protocol: "subsonic", Name: client})
	return mediastream.Bind(r, currentUser(r), event)
}

func pickQuality(r *http.Request, userQuality string) string {
	return mediastream.PickQuality(r, userQuality)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request)   { s.serveMedia(w, r, true) }
func (s *Server) download(w http.ResponseWriter, r *http.Request) { s.serveMedia(w, r, false) }

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, persist bool) {
	s.serveMediaWithError(w, r, persist, writeErr)
}

func (s *Server) serveMediaWithError(w http.ResponseWriter, r *http.Request, persist bool, fail mediaErrorWriter) {
	s.mediaService().Serve(w, mediaRequest(r), persist, fail)
}

// 保留协议诊断与旧回归测试入口，取链和媒体正文业务只在共享服务中实现。
func (s *Server) prepareMedia(ctx context.Context, r *http.Request, track *music.Info, quality string, proxy, head bool) (music.URLResolution, *http.Response, error) {
	return s.mediaService().Prepare(ctx, mediaRequest(r), track, quality, proxy, head)
}

func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, track *music.Info, resolution music.URLResolution, response *http.Response) bool {
	return s.mediaService().ProxyStream(w, mediaRequest(r), track, resolution, response)
}

func (s *Server) writeMediaHead(w http.ResponseWriter, r *http.Request, resolution music.URLResolution, response *http.Response) {
	s.mediaService().WriteHead(w, mediaRequest(r), resolution, response)
}

func (s *Server) mediaEvent(stage, id, platform, quality string, cached bool, status int, started time.Time, err error) {
	s.mediaService().MediaEvent(stage, id, platform, quality, cached, status, started, err)
}
