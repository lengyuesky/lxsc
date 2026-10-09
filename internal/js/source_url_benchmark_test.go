package js

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"lxsc/internal/assets"
)

// 三个候选、首选永不返回、第二个立即成功；不依赖公网或真实音源。
// 单轮运行足够对比首播等待，初始化不计入时间。
func BenchmarkMusicURLSlowPrimary(b *testing.B) {
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		b.Fatal(err)
	}
	m := NewSourceManager(string(prelude), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(m.UnloadAll)
	for index, body := range []string{
		`globalThis.__lx_request=()=>new Promise(()=>{});`,
		`globalThis.__lx_request=()=>({url:'https://media.invalid/ready'});`,
		`globalThis.__lx_request=()=>({url:'https://media.invalid/unused'});`,
	} {
		script := `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});` + body
		if _, err := m.Load(b.Context(), int64(index+1), index, script); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for range b.N {
		ctx, cancel := context.WithTimeout(b.Context(), 45*time.Second)
		result, err := m.MusicURL(ctx, "wy", nil, "320k")
		cancel()
		if err != nil || result.SourceID != 2 {
			b.Fatalf("备用源未成功: %+v %v", result, err)
		}
	}
}
