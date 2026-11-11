package js

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func replacementScript(tag string) string {
	return fmt.Sprintf(`// @name %s
let count=0;
lx.on(lx.EVENT_NAMES.request,()=>Promise.resolve('https://media.invalid/%s/'+(++count)));
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`, tag, tag)
}

func TestPreparedSourceKeepsOldWorkerUntilSaved(t *testing.T) {
	ctx := context.Background()
	manager := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(manager.UnloadAll)
	if _, err := manager.Load(ctx, 1, replacementScript("old")); err != nil {
		t.Fatal(err)
	}
	old := manager.loaded[1].worker
	request := func(want string) {
		t.Helper()
		result, err := manager.MusicURLForSources(ctx, "wy", nil, "320k", []int64{1})
		if err != nil || result.URL != "https://media.invalid/"+want {
			t.Fatalf("原音源被提前停止或计数被重置: %+v %v", result, err)
		}
	}
	request("old/1")
	prepared, err := manager.Prepare(ctx, 1, replacementScript("new"))
	if err != nil {
		t.Fatal(err)
	}
	request("old/2")
	blocked := errors.New("数据库保存失败")
	if _, err := prepared.Apply(ctx, func() error { return blocked }); !errors.Is(err, blocked) {
		t.Fatal(err)
	}
	prepared.Close()
	if old.stopped.Load() || !prepared.source.worker.stopped.Load() {
		t.Fatal("失败后未保留旧实例或候选实例泄漏")
	}
	request("old/3")
	prepared, err = manager.Prepare(ctx, 1, replacementScript("new"))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	saved := false
	status, err := prepared.Apply(ctx, func() error {
		if manager.loaded[1].worker != old || old.stopped.Load() {
			return errors.New("保存前旧实例已丢失")
		}
		saved = true
		return nil
	})
	if err != nil || !saved || status.ID != 1 || status.State != "ready" || !old.stopped.Load() {
		t.Fatal("验证后的实例未正确切换", status, err)
	}
	prepared.Close()
	request("new/1")
}

func TestPreparedSourceRejectsInvalidAndStaleCandidates(t *testing.T) {
	ctx := context.Background()
	manager := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(manager.UnloadAll)
	if _, err := manager.Load(ctx, 1, replacementScript("old")); err != nil {
		t.Fatal(err)
	}
	old := manager.loaded[1]
	for _, script := range []string{
		`throw new Error('脚本失败')`,
		`lx.send(lx.EVENT_NAMES.inited,{sources:null});`,
		`lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{}});`,
	} {
		if prepared, err := manager.Prepare(ctx, 1, script); err == nil {
			prepared.Close()
			t.Fatal("无效候选被接受")
		}
		if manager.loaded[1] != old || old.worker.stopped.Load() {
			t.Fatal("验证失败修改了旧实例")
		}
	}
	prepared, err := manager.Prepare(ctx, 1, replacementScript("stale"))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if _, err := manager.Load(ctx, 1, replacementScript("manual")); err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := prepared.Apply(ctx, func() error { called = true; return nil }); !errors.Is(err, ErrSourceChanged) || called {
		t.Fatal("过期候选覆盖了显式重载", err)
	}
	result, err := manager.MusicURLForSources(ctx, "wy", nil, "320k", []int64{1})
	if err != nil || !strings.Contains(result.URL, "/manual/") {
		t.Fatal("当前实例已被过期更新替换")
	}
}
