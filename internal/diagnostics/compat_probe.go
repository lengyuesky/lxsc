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
	output(w, 200, map[string]any{
		"diagnosticsVersion": 2,
		"authentication":     "Authorization: Bearer <temporary-debug-token>",
		"read":               []string{"GET /api/debug/status", "GET /api/debug/events", "GET /api/debug/capabilities"},
		"probe": []map[string]any{
			{"method": "POST", "path": "/api/debug/probe", "body": map[string]string{"trackId": "tr-wy-123456", "quality": "320k"}},
			{"method": "POST", "path": "/api/debug/probe/board", "body": map[string]string{"boardId": "lb-wy-19723756"}},
			{"method": "POST", "path": "/api/debug/probe/lyrics", "body": map[string]string{"trackId": "tr-wy-123456"}},
		},
		"workflow":    []string{"确认 status.version 为预期部署版本", "请用户在箭头音乐中打开问题榜单并打开歌曲歌词", "读取 events，对比 client、endpoint、format、result、count、lines 和 protocolCode", "仅在具备 probe 权限时，用用户指定或事件内的 boardId/trackId 进行针对性探测", "客户端请求记录和主动探测有不同 stage；主动探测成功不证明客户端显示或播放成功"},
		"limits":      map[string]any{"events": 200, "requestsPerMinute": 30, "concurrentProbes": 2, "requestTimeoutSeconds": 20},
		"perspective": "server_only", "clientLogin": false,
		"privacy": "不返回原始请求、认证参数、用户名、自定义歌单ID、歌曲文本、歌词正文或音频链接；用完由管理员撤销临时凭据",
	})
}

func (s *Server) compatibilityProbe(w http.ResponseWriter, r *http.Request, board bool) {
	scopes, _ := r.Context().Value(scopeKey{}).([]string)
	if len(scopes) != 2 {
		failure(w, 403, "probe_scope_required")
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
