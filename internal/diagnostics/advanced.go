package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/db"
	"lxsc/internal/logbuf"
	"lxsc/internal/settings"
)

type ProtocolRequest struct {
	Endpoint string            `json:"endpoint"`
	Method   string            `json:"method"`
	Format   string            `json:"format"`
	Params   map[string]string `json:"params"`
}

type ProtocolResult struct {
	Status         int               `json:"status"`
	Headers        map[string]string `json:"headers"`
	Body           any               `json:"body,omitempty"`
	Truncated      bool              `json:"truncated"`
	Events         []Event           `json:"events,omitempty"`
	Mode           string            `json:"mode,omitempty"`
	SourceID       int64             `json:"sourceId,omitempty"`
	Quality        string            `json:"quality,omitempty"`
	Cached         bool              `json:"cached"`
	Format         string            `json:"format,omitempty"`
	ProtocolStatus string            `json:"protocolStatus,omitempty"`
	ProtocolCode   *int              `json:"protocolCode,omitempty"`
	Count          *int              `json:"count,omitempty"`
	Lines          *int              `json:"lines,omitempty"`
}

type ProtocolProbeFunc func(context.Context, *db.User, ProtocolRequest) (ProtocolResult, error)

func (s *Server) advancedRoutes(r chi.Router) {
	r.Get("/inspect/{kind}", s.inspect)
	r.Post("/inspect/logs", s.inspectLogs)
	r.Post("/maintenance", s.maintain)
	r.Post("/probe/protocol", func(w http.ResponseWriter, r *http.Request) { s.protocolProbe(w, r, false) })
	r.Post("/probe/playback", func(w http.ResponseWriter, r *http.Request) { s.protocolProbe(w, r, true) })
}

func (s *Server) inspect(w http.ResponseWriter, r *http.Request) {
	if !requireScopes(w, r, "inspect") {
		return
	}
	var result any
	switch chi.URLParam(r, "kind") {
	case "settings":
		result = s.Settings.Get()
	case "runtime":
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		result = map[string]any{"version": s.Version, "goVersion": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(), "goroutines": runtime.NumGoroutine(), "heapBytes": memory.Alloc, "sysBytes": memory.Sys, "uptimeSeconds": int64(time.Since(s.StartAt).Seconds()), "configuration": s.RuntimeInfo}
	case "performance":
		result = map[string]any{"database": s.DB.ConnectionStats(), "listening": s.DB.ListeningQueries.Snapshot(), "listeningWorkload": s.DB.ListeningWorkload(), "music": s.Catalog.Performance()}
	case "sources":
		sources, err := s.DB.ListSources(r.Context())
		if err != nil {
			failure(w, 503, "sources_unavailable")
			return
		}
		items := make([]map[string]any, 0, len(sources))
		for _, source := range sources {
			digest := sha256.Sum256([]byte(source.Script))
			item := map[string]any{"id": source.ID, "name": source.Name, "version": source.Version, "enabled": source.Enabled, "priority": source.Priority, "scriptBytes": len(source.Script), "scriptSHA256": hex.EncodeToString(digest[:])}
			if s.Sources != nil {
				item["status"] = s.Sources.StatusOf(source.ID)
			}
			items = append(items, item)
		}
		result = items
	case "logs":
		result = s.filteredLogs(300, "", "")
	default:
		failure(w, 404, "unknown_inspection")
		return
	}
	if r.Context().Err() != nil {
		failure(w, 408, "debug_cancelled_or_expired")
		return
	}
	output(w, 200, map[string]any{"data": RedactValue(result), "redacted": true, "perspective": "server_only"})
}

func (s *Server) filteredLogs(limit int, level, contains string) []logbuf.Entry {
	items := []logbuf.Entry{}
	if s.Logs == nil {
		return items
	}
	for _, item := range s.Logs.List(1000) {
		item.Message = RedactText(item.Message)
		if (level == "" || item.Level == level) && (contains == "" || strings.Contains(item.Message, contains)) {
			items = append(items, item)
		}
	}
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	return items
}

func (s *Server) inspectLogs(w http.ResponseWriter, r *http.Request) {
	if !requireScopes(w, r, "inspect") {
		return
	}
	values, ok := objectBody(w, r, "limit", "level", "contains")
	limit, level, contains := 300, "", ""
	if !ok || (values["limit"] != nil && json.Unmarshal(values["limit"], &limit) != nil) || (values["level"] != nil && json.Unmarshal(values["level"], &level) != nil) || (values["contains"] != nil && json.Unmarshal(values["contains"], &contains) != nil) || limit < 1 || limit > 1000 || len(contains) > 200 || (level != "" && oneOf(level, "DEBUG", "INFO", "WARN", "ERROR") == "") {
		failure(w, 400, "invalid_log_filter")
		return
	}
	if r.Context().Err() != nil {
		failure(w, 408, "debug_cancelled_or_expired")
		return
	}
	output(w, 200, map[string]any{"data": s.filteredLogs(limit, level, contains), "redacted": true, "perspective": "server_only"})
}

func (s *Server) maintain(w http.ResponseWriter, r *http.Request) {
	if !requireScopes(w, r, "maintain", "inspect") {
		return
	}
	values, ok := objectBody(w, r, "operation", "target", "sourceId", "settings")
	var operation string
	if !ok || json.Unmarshal(values["operation"], &operation) != nil || oneOf(operation, "cache_clear", "source_reload", "settings_update") == "" {
		failure(w, 400, "invalid_maintenance_operation")
		return
	}
	if r.Context().Err() != nil {
		failure(w, 408, "maintenance_cancelled_check_current_state")
		return
	}
	tokenID, _ := r.Context().Value(tokenIDKey{}).(string)
	var result any
	switch operation {
	case "cache_clear":
		var target string
		if len(values) != 2 || json.Unmarshal(values["target"], &target) != nil || oneOf(target, "urls", "metadata", "all") == "" {
			failure(w, 400, "invalid_cache_target")
			return
		}
		s.Tokens.RecordOperation(tokenID, operation)
		if target == "urls" || target == "all" {
			s.Catalog.InvalidateURLs()
		}
		if target == "metadata" || target == "all" {
			s.Catalog.PurgeMetadataCaches()
		}
		result = map[string]any{"cleared": target}
	case "source_reload":
		var id int64
		if len(values) != 2 || json.Unmarshal(values["sourceId"], &id) != nil || id <= 0 || s.Sources == nil {
			failure(w, 400, "invalid_source_id")
			return
		}
		source, err := s.DB.GetSource(r.Context(), id)
		if err != nil || !source.Enabled {
			failure(w, 404, "enabled_source_not_found")
			return
		}
		s.Tokens.RecordOperation(tokenID, operation)
		status, err := s.Sources.Load(r.Context(), id, source.Priority, source.Script)
		s.Catalog.InvalidateURLs()
		if err != nil {
			failure(w, 502, "source_reload_failed")
			return
		}
		result = status
	case "settings_update":
		var patch map[string]json.RawMessage
		if len(values) != 2 || json.Unmarshal(values["settings"], &patch) != nil || len(patch) == 0 || !validSettingsPatch(s, patch) {
			failure(w, 400, "invalid_settings_patch")
			return
		}
		s.Tokens.RecordOperation(tokenID, operation)
		updated, err := s.Settings.Update(r.Context(), patch)
		if err != nil {
			failure(w, 400, "settings_update_failed")
			return
		}
		s.Catalog.RefreshTTL()
		result = updated
	}
	if r.Context().Err() != nil {
		failure(w, 408, "maintenance_cancelled_check_current_state")
		return
	}
	output(w, 200, map[string]any{"ok": true, "operation": operation, "data": RedactValue(result)})
}

func validSettingsPatch(s *Server, patch map[string]json.RawMessage) bool {
	encoded, err := json.Marshal(patch)
	var typed settings.Values
	if err != nil || json.Unmarshal(encoded, &typed) != nil {
		return false
	}
	raw, _ := json.Marshal(s.Settings.Get())
	var current map[string]json.RawMessage
	_ = json.Unmarshal(raw, &current)
	for key, value := range patch {
		if _, ok := current[key]; !ok || string(value) == "null" {
			return false
		}
		if key == "streamMode" || key == "coverMode" {
			var mode string
			if json.Unmarshal(value, &mode) != nil || (key == "streamMode" && Mode(mode) == "") || (key == "coverMode" && oneOf(mode, "redirect", "proxy") == "") {
				return false
			}
		}
		if key == "defaultQuality" && Quality(typed.DefaultQuality) == "" {
			return false
		}
		if key == "serverName" && (strings.TrimSpace(typed.ServerName) == "" || len(typed.ServerName) > 200) {
			return false
		}
		if key == "searchSources" || key == "boardSources" {
			var sources []string
			if json.Unmarshal(value, &sources) != nil || len(sources) > 5 {
				return false
			}
			seen := map[string]bool{}
			for _, source := range sources {
				if Platform(source) == "" || seen[source] {
					return false
				}
				seen[source] = true
			}
		}
		if maximum, bounded := map[string]int{"boardLimit": 20, "boardTrackLimit": 100, "artistSongLimit": 100, "artistAlbumLimit": 50, "searchLimit": 100}[key]; bounded {
			var number int
			if json.Unmarshal(value, &number) != nil || number < 1 || number > maximum {
				return false
			}
		}
	}
	return true
}

func (s *Server) protocolProbe(w http.ResponseWriter, r *http.Request, playback bool) {
	if !requireScopes(w, r, "probe", "inspect") {
		return
	}
	fields := []string{"endpoint", "method", "format", "params", "userId"}
	if playback {
		fields = []string{"trackId", "quality", "endpoint", "method", "proxy", "userId"}
	}
	values, ok := objectBody(w, r, fields...)
	request := ProtocolRequest{Method: "GET", Format: "json", Params: map[string]string{}}
	var userID int64
	if !ok || (values["userId"] != nil && json.Unmarshal(values["userId"], &userID) != nil) || userID < 0 {
		failure(w, 400, "invalid_protocol_probe")
		return
	}
	if playback {
		request.Endpoint, request.Method = "stream", "HEAD"
		var id, quality string
		var proxy bool
		if json.Unmarshal(values["trackId"], &id) != nil || !ValidTrackID(id) || (values["quality"] != nil && (json.Unmarshal(values["quality"], &quality) != nil || Quality(quality) == "")) || (values["proxy"] != nil && json.Unmarshal(values["proxy"], &proxy) != nil) {
			failure(w, 400, "invalid_playback_probe")
			return
		}
		request.Params["id"], request.Params["quality"] = id, quality
		if proxy {
			request.Params["proxy"] = "1"
		}
	} else if values["params"] != nil && (json.Unmarshal(values["params"], &request.Params) != nil || request.Params == nil) {
		failure(w, 400, "invalid_protocol_params")
		return
	}
	for key, dest := range map[string]*string{"endpoint": &request.Endpoint, "method": &request.Method, "format": &request.Format} {
		if values[key] != nil && json.Unmarshal(values[key], dest) != nil {
			failure(w, 400, "invalid_protocol_probe")
			return
		}
	}
	if (playback && oneOf(request.Endpoint, "stream", "download") == "") || oneOf(request.Method, "GET", "POST", "HEAD") == "" || oneOf(request.Format, "json", "xml") == "" {
		failure(w, 400, "invalid_protocol_probe")
		return
	}
	if s.ProtocolProbe == nil {
		failure(w, 503, "protocol_probe_unavailable")
		return
	}
	if !slices.Contains(s.ProtocolEndpoints, request.Endpoint) {
		failure(w, 400, "unsupported_protocol_endpoint")
		return
	}
	for key := range request.Params {
		switch strings.ToLower(key) {
		case "u", "p", "t", "s", "apikey", "authorization", "cookie", "callback", "f":
			failure(w, 400, "protocol_credentials_forbidden")
			return
		}
	}
	select {
	case s.probes <- struct{}{}:
		defer func() { <-s.probes }()
	default:
		busy(w, "1")
		return
	}
	user, _ := r.Context().Value(ownerKey{}).(*db.User)
	if userID > 0 {
		var err error
		user, err = s.DB.GetUserByID(r.Context(), userID)
		if err != nil {
			failure(w, 404, "probe_subject_not_found")
			return
		}
	}
	started := time.Now()
	result, err := s.ProtocolProbe(r.Context(), user, request)
	if r.Context().Err() != nil {
		failure(w, 408, "probe_cancelled_or_expired")
		return
	}
	if err != nil {
		failure(w, 400, "unsupported_or_invalid_protocol_probe")
		return
	}
	stage := "protocol_probe"
	if playback {
		stage = "playback_probe"
	}
	event := Event{Stage: stage, Endpoint: request.Endpoint, Method: request.Method, TrackID: request.Params["id"], Status: result.Status, Result: "ok", Error: "none", ElapsedMS: time.Since(started).Milliseconds()}
	event.Format, event.ProtocolCode, event.Count, event.Lines = result.Format, result.ProtocolCode, result.Count, result.Lines
	if result.ProtocolStatus != "" {
		event.Result = result.ProtocolStatus
	}
	if result.Status >= 400 {
		event.Result = "failed"
	}
	if body, ok := result.Body.(map[string]any); ok {
		if response, ok := body["subsonic-response"].(map[string]any); ok && response["status"] == "failed" {
			event.Result = "failed"
			if protocolError, ok := response["error"].(map[string]any); ok {
				if code, ok := protocolError["code"].(float64); ok {
					value := int(code)
					event.ProtocolCode = &value
				}
			}
		}
	}
	s.Events.Add(event)
	output(w, 200, map[string]any{"result": RedactValue(result), "perspective": "server_only", "cacheSideEffects": true, "playbackPersisted": false, "subjectUserId": user.ID})
}
