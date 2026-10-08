package admin

import (
	"net/http"
	"time"

	"lxsc/internal/js"
)

type sourceStatisticsView struct {
	ID         int64                   `json:"id"`
	Name       string                  `json:"name"`
	Enabled    bool                    `json:"enabled"`
	State      string                  `json:"state"`
	Statistics js.SourceCallStatistics `json:"statistics"`
}

func (s *Server) sourceStatistics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	window := time.Hour
	windowName := r.URL.Query().Get("window")
	switch windowName {
	case "", "1h":
		windowName = "1h"
	case "24h":
		window = 24 * time.Hour
	default:
		fail(w, http.StatusBadRequest, "时间范围仅支持 1h 或 24h")
		return
	}
	sources, err := s.DB.ListSources(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取音源失败")
		return
	}
	now := time.Now()
	items := make([]sourceStatisticsView, 0, len(sources))
	for _, source := range sources {
		state := "unloaded"
		if !source.Enabled {
			state = "disabled"
		} else if status := s.Sources.StatusOf(source.ID); status != nil {
			state = status.State
		}
		items = append(items, sourceStatisticsView{ID: source.ID, Name: source.Name, Enabled: source.Enabled, State: state, Statistics: s.Sources.CallStatisticsOf(source.ID, now, window)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"generatedAt": now, "window": windowName, "recentLimit": 100, "sources": items})
}
