package subsonic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

const probePreviewLimit = 64 << 10

// 调试只调用明确的只读协议接口；不经过播放器登录，也不开放评分、收藏、队列等写入。
func (s *Server) probeHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"ping": s.ping, "getLicense": s.getLicense, "getOpenSubsonicExtensions": s.getOpenSubsonicExtensions,
		"getUser": s.getUser, "getUsers": s.getUsers, "getScanStatus": s.getScanStatus,
		"getMusicFolders": s.getMusicFolders, "getIndexes": s.getIndexes, "getArtists": s.getArtists,
		"getMusicDirectory": s.getMusicDirectory, "getGenres": s.getGenres, "getArtist": s.getArtist,
		"getArtistInfo": s.getArtistInfo, "getArtistInfo2": s.getArtistInfo,
		"getAlbum": s.getAlbum, "getAlbumInfo": s.getAlbumInfo, "getAlbumInfo2": s.getAlbumInfo,
		"getSong": s.getSong, "getTopSongs": s.getTopSongs, "getSimilarSongs": s.getSimilarSongs, "getSimilarSongs2": s.getSimilarSongs,
		"getAlbumList": s.getAlbumList, "getAlbumList2": s.getAlbumList, "getRandomSongs": s.getRandomSongs,
		"getSongsByGenre": s.getSongsByGenre, "getNowPlaying": s.getNowPlaying,
		"getStarred": s.getStarred, "getStarred2": s.getStarred,
		"search": s.search, "search2": s.search, "search3": s.search,
		"getPlaylists": s.getPlaylists, "getPlaylist": s.getPlaylist,
		"getLyrics": s.getLyrics, "getLyricsBySongId": s.getLyricsBySongID,
		"getPlayQueue": s.getPlayQueue, "getBookmarks": s.getBookmarks,
		"getInternetRadioStations": s.getInternetRadioStations, "getPodcasts": s.getPodcasts, "getNewestPodcasts": s.getPodcasts,
		"getShares": s.getShares, "getVideos": s.getVideos, "getCoverArt": s.getCoverArt,
		"getSongLists": s.getOnlineSongLists,
	}
}

func (s *Server) ProbeEndpoints() []string {
	endpoints := []string{"stream", "download"}
	for endpoint := range s.probeHandlers() {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)
	return endpoints
}

type probeCapture struct {
	headers   http.Header
	status    int
	body      bytes.Buffer
	truncated bool
}

func (w *probeCapture) Header() http.Header { return w.headers }
func (w *probeCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *probeCapture) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	length := min(len(data), probePreviewLimit-w.body.Len())
	_, _ = w.body.Write(data[:length])
	w.truncated = w.truncated || length < len(data)
	return len(data), nil
}

// ProbeProtocol 的调用方必须是已验证管理员的调试服务；user 仅是被模拟的请求主体。
func (s *Server) ProbeProtocol(ctx context.Context, user *db.User, probe diagnostics.ProtocolRequest) (diagnostics.ProtocolResult, error) {
	if user == nil || user.ID <= 0 || (probe.Method != "GET" && probe.Method != "POST" && probe.Method != "HEAD") || (probe.Format != "json" && probe.Format != "xml") {
		return diagnostics.ProtocolResult{}, errors.New("无效协议探测")
	}
	media := probe.Endpoint == "stream" || probe.Endpoint == "download"
	// 不复制 Server 内的锁；只有只读业务依赖和独立诊断缓冲进入探测实例。
	s.initCoverClient()
	local := &Server{DB: s.DB, Catalog: s.Catalog, Settings: s.Settings, Secret: s.Secret, Log: s.Log, HTTP: s.HTTP, Diagnostics: &diagnostics.Events{}, coverHTTP: s.coverHTTP, coverLimits: s.coverLimits, coverCache: s.coverCache}
	handler, ok := local.probeHandlers()[probe.Endpoint]
	if !media && (!ok || probe.Method == "HEAD") {
		return diagnostics.ProtocolResult{}, errors.New("不支持的协议探测")
	}
	params := url.Values{}
	for key, value := range probe.Params {
		if len(key) > 64 || len(value) > 4096 || key == "" {
			return diagnostics.ProtocolResult{}, errors.New("无效协议参数")
		}
		switch strings.ToLower(key) {
		case "u", "p", "t", "s", "apikey", "authorization", "cookie", "callback", "f":
			return diagnostics.ProtocolResult{}, errors.New("探测不接受认证或回调参数")
		}
		params.Set(key, value)
	}
	params.Set("f", probe.Format)
	address := "https://debug.invalid/rest/" + probe.Endpoint + ".view"
	var request *http.Request
	if probe.Method == http.MethodPost {
		request, _ = http.NewRequestWithContext(ctx, probe.Method, address, strings.NewReader(params.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		request, _ = http.NewRequestWithContext(ctx, probe.Method, address+"?"+params.Encode(), nil)
	}
	request = request.WithContext(withUser(request.Context(), user))
	protocol := &diagnostics.Event{Format: probe.Format}
	request = request.WithContext(context.WithValue(request.Context(), clientDiagnosticKey{}, protocol))
	capture := &probeCapture{headers: make(http.Header)}
	result := diagnostics.ProtocolResult{}
	if media {
		local.probePlayback(capture, request, &result)
	} else {
		handler(capture, request)
	}
	if ctx.Err() != nil {
		return diagnostics.ProtocolResult{}, ctx.Err()
	}
	result.Status = capture.status
	if result.Status == 0 {
		result.Status = 200
	}
	result.Headers = map[string]string{}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control", "Location", "Retry-After"} {
		if value := capture.headers.Get(key); value != "" {
			result.Headers[key] = value
		}
	}
	if capture.body.Len() > 0 {
		contentType := capture.headers.Get("Content-Type")
		if strings.HasPrefix(contentType, "application/json") && !capture.truncated {
			_ = json.Unmarshal(capture.body.Bytes(), &result.Body)
		} else if strings.Contains(contentType, "xml") || strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") {
			result.Body = capture.body.String()
		} else {
			result.Body = map[string]any{"binaryBodyOmitted": true, "capturedBytes": capture.body.Len()}
		}
	}
	result.Truncated = capture.truncated
	result.Format, result.ProtocolStatus, result.ProtocolCode = protocol.Format, protocol.Result, protocol.ProtocolCode
	result.Count, result.Lines = protocol.Count, protocol.Lines
	result.Events = local.Diagnostics.List()
	return result, nil
}

func (s *Server) probePlayback(w http.ResponseWriter, r *http.Request, result *diagnostics.ProtocolResult) {
	ctx, cancel := context.WithTimeout(r.Context(), mediaRecoveryTimeout)
	defer cancel()
	user := currentUser(r)
	release, err := s.Catalog.AcquireRequest(ctx, user.ID)
	if err != nil {
		writeErr(w, r, ErrBusy, admission.ErrBusy.Error())
		return
	}
	defer release()
	started := time.Now()
	info, err := s.Catalog.Track(ctx, param(r, "id"))
	s.mediaEvent("metadata", param(r, "id"), "", "", false, 0, started, err)
	if err != nil {
		writeErr(w, r, ErrNotFound, "歌曲不可用")
		return
	}
	mode := s.Settings.Get().StreamMode
	proxy := mode != "force_redirect" && (mode == "proxy" || param(r, "proxy") == "1")
	headMetadata := r.Method == "HEAD" && mode != "force_redirect"
	if proxy && !headMetadata {
		releaseMedia, err := s.Catalog.MediaLimits.Acquire(ctx, user.ID)
		if err != nil {
			writeErr(w, r, ErrBusy, admission.ErrBusy.Error())
			return
		}
		defer releaseMedia()
	}
	resolution, response, err := s.prepareMedia(ctx, r, info, pickQuality(r, user.Quality), proxy, headMetadata)
	if response != nil {
		defer response.Body.Close()
	}
	result.Mode = mode
	if err != nil {
		writeErr(w, r, ErrGeneric, "无法获取可用的播放地址")
		return
	}
	result.SourceID, result.Quality, result.Cached = resolution.Result.SourceID, resolution.Result.Quality, resolution.Cached
	if headMetadata {
		s.writeMediaHead(w, r, resolution, response)
		return
	}
	if proxy {
		for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
			if value := response.Header.Get(key); value != "" {
				w.Header().Set(key, value)
			}
		}
		if value := w.Header().Get("Content-Type"); value == "" || strings.HasPrefix(value, "application/octet") {
			_, _, contentType := music.QualityMeta(resolution.Result.Quality)
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("Cache-Control", "no-store")
		recordMediaResponse(r, response.StatusCode)
		w.WriteHeader(response.StatusCode)
		// GET 也只观察真实代理的响应头，绝不调用 proxyStream/RememberSync。
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	recordMediaResponse(r, http.StatusFound)
	http.Redirect(w, r, resolution.Result.URL, http.StatusFound)
}
