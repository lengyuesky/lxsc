package js

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func TestSlowSourceReservesFallbackBudget(t *testing.T) {
	manager := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(manager.UnloadAll)
	manager.CallTime = 100 * time.Millisecond
	pre := `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k','128k']}}});`
	for i, body := range []string{`globalThis.__lx_request=()=>new Promise(()=>{});`, `globalThis.__lx_request=()=>({url:'https://media.invalid/ok'});`} {
		if _, err := manager.Load(context.Background(), int64(i+1), i, pre+body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	result, err := manager.MusicURLForSources(ctx, "wy", map[string]any{"songmid": "one"}, "flac", nil)
	if err != nil || result.SourceID != 2 {
		t.Fatalf("首源三档音质持续超时，健康备用源未成功接替：结果=%+v，错误=%v", result, err)
	}
}
