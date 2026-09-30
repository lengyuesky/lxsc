package js

import (
	"context"
	"errors"
	"lxsc/internal/admission"
	"sync"
	"time"
)

type SourceHealth struct {
	Samples    int            `json:"samples"`
	Success    int            `json:"success"`
	AverageMS  int64          `json:"averageMs"`
	Downgrades int            `json:"downgrades"`
	Errors     map[string]int `json:"errors"`
}
type healthSample struct {
	at        time.Time
	ms        int64
	category  string
	downgrade bool
}
type sourceHealth struct {
	mu      sync.Mutex
	samples [100]healthSample
	next    int
}

func (h *sourceHealth) record(start time.Time, err error, downgrade bool) {
	category := ""
	switch {
	case errors.Is(err, context.Canceled):
		category = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
	case errors.Is(err, admission.ErrBusy):
		category = "busy"
	case err != nil:
		category = "script"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples[h.next] = healthSample{time.Now(), time.Since(start).Milliseconds(), category, downgrade}
	h.next = (h.next + 1) % len(h.samples)
}
func (h *sourceHealth) snapshot() SourceHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := SourceHealth{Errors: map[string]int{}}
	for _, sample := range h.samples {
		if sample.at.IsZero() || time.Since(sample.at) > time.Hour {
			continue
		}
		out.Samples++
		out.AverageMS += sample.ms
		if sample.category == "" {
			out.Success++
		} else {
			out.Errors[sample.category]++
		}
		if sample.downgrade {
			out.Downgrades++
		}
	}
	if out.Samples > 0 {
		out.AverageMS /= int64(out.Samples)
	}
	return out
}
