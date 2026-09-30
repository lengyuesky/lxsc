package js

import (
	"context"
	"encoding/json"
	"errors"
	"lxsc/internal/admission"
	"strings"
	"testing"
	"time"
)

func TestSourceHealthBoundedClassifiedAndRedacted(t *testing.T) {
	var h sourceHealth
	h.record(time.Now().Add(-time.Second), nil, true)
	h.record(time.Now(), context.DeadlineExceeded, false)
	h.record(time.Now(), context.Canceled, false)
	h.record(time.Now(), admission.ErrBusy, false)
	h.record(time.Now(), errors.New("https://private.invalid/?token=secret"), false)
	s := h.snapshot()
	if s.Samples != 5 || s.Success != 1 || s.Downgrades != 1 || s.Errors["timeout"] != 1 || s.Errors["cancelled"] != 1 || s.Errors["busy"] != 1 || s.Errors["script"] != 1 {
		t.Fatalf("健康统计异常: %+v", s)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "private.invalid") {
		t.Fatal("诊断泄露原始错误")
	}
	s.Errors["script"] = 999
	if h.snapshot().Errors["script"] != 1 {
		t.Fatal("快照修改影响内部状态")
	}
	h.mu.Lock()
	h.samples[0].at = time.Now().Add(-2 * time.Hour)
	h.mu.Unlock()
	if h.snapshot().Samples != 4 {
		t.Fatal("一小时前样本未淘汰")
	}
	for i := 0; i < 150; i++ {
		h.record(time.Now(), nil, false)
	}
	if h.snapshot().Samples != 100 {
		t.Fatal("样本数量未受限")
	}
}
