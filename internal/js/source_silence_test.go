package js

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// 连续无响应的音源暂停取链并逐次加长；任何回应都立即清零。
func TestSilentSourceIsSuspendedUntilItAnswers(t *testing.T) {
	g, now := newTestClock()
	silent := func() { g.heard(1, "tx", false, "") }
	silent()
	silent() // 同一批并发请求里的超时只算一次
	if !g.canCall(1, "tx") {
		t.Fatal("一次无响应不应暂停")
	}
	*now = now.Add(time.Minute)
	silent()
	if g.canCall(1, "tx") {
		t.Fatal("连续两次无响应后应暂停取链")
	}
	if !g.canCall(1, "wy") || !g.canCall(2, "tx") {
		t.Fatal("暂停只针对无响应的音源与平台")
	}
	*now = now.Add(silenceBaseSuspend)
	if !g.canCall(1, "tx") {
		t.Fatal("暂停到期后应允许一次恢复探测")
	}
	silent()
	*now = now.Add(silenceBaseSuspend)
	if g.canCall(1, "tx") {
		t.Fatal("恢复探测仍无响应时暂停应加长")
	}
	*now = now.Add(silenceBaseSuspend)
	if !g.canCall(1, "tx") {
		t.Fatal("加长后的暂停应在两倍时长后到期")
	}
	for range 20 {
		silent()
		*now = now.Add(silenceMaxSuspend)
		if !g.canCall(1, "tx") {
			t.Fatal("暂停时长不得超过上限")
		}
	}
	g.heard(1, "tx", true, "")
	silent()
	if !g.canCall(1, "tx") {
		t.Fatal("有回应后应从头计数")
	}
	*now = now.Add(time.Minute)
	silent()
	g.forget(1)
	if !g.canCall(1, "tx") || len(g.silent) != 0 {
		t.Fatal("删除音源后不应保留暂停状态")
	}
}

// 暂停期间不再调用无响应的音源，选源无需为它等满取链上限。
func TestSuspendedSourceIsNotCalled(t *testing.T) {
	manager := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(manager.UnloadAll)
	manager.CallTime = 50 * time.Millisecond
	now := time.Now()
	manager.schedule.now = func() time.Time { return now }
	script := `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});globalThis.__lx_request=()=>new Promise(()=>{});`
	if _, err := manager.Load(context.Background(), 1, script); err != nil {
		t.Fatal(err)
	}
	call := func() error {
		_, err := manager.MusicURLForSources(context.Background(), "wy", map[string]any{"songmid": "one"}, "320k", []int64{1})
		return err
	}
	for range silenceThreshold {
		if err := call(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("无响应的音源应以超时结束：%v", err)
		}
		now = now.Add(time.Minute)
	}
	started := time.Now()
	if err := call(); !errors.Is(err, ErrNoSource) || time.Since(started) > manager.CallTime/2 {
		t.Fatalf("暂停期间应立即返回而不是再等一次超时：%v %v", err, time.Since(started))
	}
	if calls := manager.CallStatisticsOf(1, time.Now(), time.Hour).Total.Calls; calls != silenceThreshold {
		t.Fatalf("暂停期间不应调用音源：%d", calls)
	}
	// 请求方自己放弃的调用无从判断音源是否有回应，不计入。
	now = now.Add(silenceMaxSuspend)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := manager.MusicURLForSources(ctx, "wy", map[string]any{"songmid": "one"}, "320k", []int64{1}); err == nil {
		t.Fatal("应随请求取消而结束")
	}
	if !manager.schedule.canCall(1, "wy") {
		t.Fatal("被请求方取消的调用不应延长暂停")
	}
}
