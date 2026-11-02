package coverart

import (
	"errors"

	"lxsc/internal/admission"
	"lxsc/internal/diagnostics"
)

// RecordOutcome retains failure classification even when a temporary image returns HTTP 200.
func RecordOutcome(event *diagnostics.Event, origin string, cached bool, err error) {
	if event == nil {
		return
	}
	event.CoverOrigin, event.Cached = origin, cached
	event.Error = diagnostics.ErrorCode(err)
	if errors.Is(err, admission.ErrBusy) {
		event.Error = "busy"
	}
	event.Result = "ok"
	if err != nil {
		event.Result = "failed"
	}
	if origin == "placeholder" || origin == "stale" {
		event.Result = "unavailable"
	}
}
