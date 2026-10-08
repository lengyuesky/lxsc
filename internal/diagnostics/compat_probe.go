package diagnostics

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"lxsc/internal/music"
)

// 机器可读指南，AI 先发现能力再诊断；凭据不能作为客户端登录或播放凭据。
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	eventLimit := 200
	if hasScope(requestScopes(r), "inspect") {
		eventLimit = 2000
	}
	output(w, 200, map[string]any{
		"diagnosticsVersion": DiagnosticsVersion,
		"administratorOnly":  true,
		"grantedScopes":      requestScopes(r),
		"maximumScopes":      MaximumScopes(),
		"authentication":     "Authorization: Bearer <temporary-debug-token>",
		"read":               []string{"GET /api/debug/status", "GET /api/debug/events", "GET /api/debug/capabilities"},
		"probe": []map[string]any{
			{"method": "POST", "path": "/api/debug/probe", "body": map[string]string{"trackId": "tr-wy-123456", "quality": "320k"}},
			{"method": "POST", "path": "/api/debug/probe/board", "body": map[string]string{"boardId": "lb-wy-19723756"}},
			{"method": "POST", "path": "/api/debug/probe/lyrics", "body": map[string]string{"trackId": "tr-wy-123456"}},
			{"method": "POST", "path": "/api/debug/probe/protocol", "requiredScopes": []string{"probe", "inspect"}, "body": map[string]any{"endpoint": "getPlaylists", "method": "GET", "format": "json", "params": map[string]string{}}},
			{"method": "POST", "path": "/api/debug/probe/playback", "requiredScopes": []string{"probe", "inspect"}, "body": map[string]string{"trackId": "tr-wy-123456", "method": "HEAD", "quality": "320k"}},
		},
		"inspect":           map[string]any{"requiredScope": "inspect", "endpoints": []string{"GET /api/debug/inspect/runtime", "GET /api/debug/inspect/performance", "GET /api/debug/inspect/settings", "GET /api/debug/inspect/sources", "GET /api/debug/inspect/logs", "POST /api/debug/inspect/logs"}, "logFilter": map[string]any{"limit": 300, "level": "WARN", "contains": ""}},
		"maintenance":       map[string]any{"method": "POST", "path": "/api/debug/maintenance", "requiredScope": "maintain", "examples": []map[string]any{{"operation": "cache_clear", "target": "urls"}, {"operation": "source_reload", "sourceId": 1}, {"operation": "settings_update", "settings": map[string]string{"streamMode": "redirect"}}}},
		"protocolEndpoints": s.ProtocolEndpoints,
		"protocolProbe":     map[string]any{"methods": []string{"GET", "POST", "HEAD"}, "formats": []string{"json", "xml"}, "userId": "可选，管理员可模拟指定现有用户；省略时使用创建者", "credentials": "禁止传入 u/p/t/s/apiKey/Cookie/Authorization，禁止任何协议写入", "media": "HEAD/GET/POST 均只检查媒体响应头，不读取音频正文或记录播放"},
		"workflow":          []string{"确认 status.version 为预期部署版本", "请用户在箭头音乐中打开问题榜单、重试播放并打开歌曲歌词", "读取 events，对比 client、endpoint、method、format、result、count、lines 和 protocolCode；stream/download 的 HEAD 是客户端预检，不是播放，普通302/代理模式成功时应返回200媒体头，强制302除外", "HTTP 200/206 仍需检查 error；non_audio 表示上游返回了非音频类型，缺少媒体头不能证明音频有效", "仅在具备 probe 权限时，用用户指定或事件内的 boardId/trackId 进行针对性探测", "客户端请求记录和主动探测有不同 stage；主动探测成功不证明客户端显示或播放成功"},
		"limits":            map[string]any{"events": eventLimit, "requestsPerMinute": scopeRate(requestScopes(r)), "concurrentProbes": 2, "requestTimeoutSeconds": int(scopeTimeout(requestScopes(r)).Seconds()), "logEntries": 1000, "protocolPreviewBytes": 65536},
		"perspective":       "server_only", "clientLogin": false,
		"privacy": "基础 read/probe 保持固定字段；inspect 可返回设置、音源、处理过的日志和协议预览，可能包含用户或歌曲信息。已识别凭据、脚本正文及 URL 路径/参数被隐藏；自由文本仍仅供管理员授权的可信排查者，不得公开。维护会修改共享状态并记录审计；到期或撤销不回滚已完成操作。",
	})
}

func (s *Server) compatibilityProbe(w http.ResponseWriter, r *http.Request, board bool) {
	if !requireScopes(w, r, "probe") {
		return
	}
	field := "trackId"
	stage := "lyrics_probe"
	if board {
		field = "boardId"
		stage = "board_probe"
	}
	values, ok := objectBody(w, r, field)
	if r.Context().Err() != nil {
		failure(w, 408, "probe_cancelled_or_expired")
		return
	}
	var id string
	if !ok || json.Unmarshal(values[field], &id) != nil || (board && !ValidBoardID(id)) || (!board && !ValidTrackID(id)) {
		failure(w, 400, "invalid_probe_request")
		return
	}
	select {
	case s.probes <- struct{}{}:
		defer func() { <-s.probes }()
	default:
		busy(w, "1")
		return
	}
	started := time.Now()
	parsed, _ := music.ParseID(id)
	event := Event{Time: started.UTC(), Stage: stage, Platform: parsed.Source, Result: "ok", Error: "none"}
	var err error
	sample := []string{}
	if board {
		event.BoardID = id
		_, _, event.Cached = s.Catalog.CachedBoardSummary(parsed.Source, parsed.Key)
		var tracks []*music.Info
		tracks, err = s.Catalog.FullBoardTracks(r.Context(), parsed.Source, parsed.Key)
		if err == nil {
			n := len(tracks)
			event.Count = &n
			for _, track := range tracks {
				if len(sample) < 3 && ValidTrackID(track.TrackID()) {
					sample = append(sample, track.TrackID())
				}
			}
			if n == 0 {
				event.Result = "empty"
			}
		}
	} else {
		event.TrackID = id
		var track *music.Info
		track, err = s.Catalog.Track(r.Context(), id)
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			var lyrics *music.Lyrics
			lyrics, err = s.Catalog.Lyric(ctx, track)
			cancel()
			if err == nil {
				n := len(lyrics.Lines)
				event.Lines = &n
				event.Synced = &lyrics.Synced
				if n == 0 {
					event.Result = "empty"
				}
			}
		}
	}
	event.ElapsedMS = time.Since(started).Milliseconds()
	if err != nil {
		event.Result = "failed"
		event.Error = ErrorCode(err)
	}
	// 撤销、到期和请求取消后不发布已取得的探测结果。
	if r.Context().Err() != nil {
		failure(w, 408, "probe_cancelled_or_expired")
		return
	}
	s.Events.Add(event)
	output(w, 200, map[string]any{"events": []Event{event}, "sampleTrackIds": sample, "perspective": "server_only", "cacheSideEffects": true})
}
