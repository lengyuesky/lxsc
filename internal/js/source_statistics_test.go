package js

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"lxsc/internal/admission"
)

func TestSourceCallWindowsAreIndependentOfRecentLimit(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 32, 0, 0, time.UTC)
	s := sourceCalls{since: now.Add(-25 * time.Hour)}
	for _, at := range []time.Time{now.Add(-25 * time.Hour), now.Add(-2 * time.Hour)} {
		s.record(SourceCall{At: at, Platform: "tx", Action: "lyric", Attempts: 1, Outcome: "success", DurationMS: 50})
	}
	for i := range 150 {
		s.record(SourceCall{At: now.Add(-time.Duration(150-i) * time.Second), Platform: "wy", Action: "musicUrl", Attempts: 2, Outcome: "success", Downgraded: true, DurationMS: 100})
	}
	s.record(SourceCall{At: now, Platform: "wy", Action: "pic", Attempts: 1, Outcome: "failed", ErrorCategory: "invalid_response", DurationMS: 200})
	s.record(SourceCall{At: now, Platform: "wy", Action: "lyric", Attempts: 1, Outcome: "cancelled", ErrorCategory: "cancelled", DurationMS: 400})
	one := s.snapshot(now, time.Hour)
	if one.Summary.Calls != 152 || one.Summary.Success != 150 || one.Summary.Failed != 1 || one.Summary.Cancelled != 1 || one.Summary.Attempts != 302 || one.Summary.Downgrades != 150 || one.Summary.MaxMS != 400 || one.Summary.AverageMS != 102 {
		t.Fatalf("汇总不能按最近 100 条截断，取消应单独计数: %+v", one.Summary)
	}
	if len(one.Recent) != 100 || one.Recent[0].Action != "lyric" || len(one.Trend) != 12 || len(one.Groups) != 3 || one.Total.Calls != 154 {
		t.Fatalf("趋势、分组、最近记录或累计数异常: %+v", one)
	}
	day := s.snapshot(now, 24*time.Hour)
	if day.Summary.Calls != 153 || day.Summary.DurationMS != 15650 || len(day.Trend) != 24 || len(day.Groups) != 4 {
		t.Fatalf("24 小时范围错误: %+v", day)
	}
	var count int64
	for _, point := range day.Trend {
		count += point.Calls
	}
	if count != day.Summary.Calls {
		t.Fatal("趋势与总调用次数不一致")
	}
	one.Summary.Errors["invalid_response"] = 999
	one.Groups[0].Errors["script"] = 999
	one.Recent[0].Song = "修改快照"
	if again := s.snapshot(now, time.Hour); again.Summary.Errors["invalid_response"] != 1 || again.Recent[0].Song != "" || again.Groups[0].Errors["script"] != 0 {
		t.Fatal("修改返回值污染内部记录")
	}
	later := s.snapshot(now.Add(25*time.Hour), 24*time.Hour)
	if later.Summary.Calls != 0 || len(later.Recent) != 0 || later.Total.Calls != 154 {
		t.Fatal("旧窗口未过期，或运行期间累计被清除")
	}
	s.record(SourceCall{At: now.Add(25 * time.Hour), Platform: "kw", Action: "pic", Attempts: 1, Outcome: "success"})
	if got := s.snapshot(now.Add(25*time.Hour), 24*time.Hour); got.Summary.Calls != 1 || len(got.Groups) != 1 {
		t.Fatal("循环时间段复用时混入旧数据")
	}
}

func TestSourceCallClassificationAndBoundedMetadata(t *testing.T) {
	s := &sourceCalls{since: time.Now()}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, admission.ErrBusy, errInvalidSourceResponse, errors.New("https://private.invalid/?token=secret")} {
		s.begin()
		s.finish(time.Now().Add(-time.Millisecond), SourceCall{Platform: "wy", Action: "musicUrl", Attempts: 1, RequestedQuality: "https://private.invalid/?token=secret"}, map[string]any{"name": strings.Repeat("歌曲", 10000), "singer": "测试歌手", "picUrl": "secret", "meta": map[string]any{"token": "secret"}}, err)
	}
	stats := s.snapshot(time.Now(), time.Hour)
	if stats.Summary.Calls != 6 || stats.Summary.Success != 1 || stats.Summary.Failed != 4 || stats.Summary.Cancelled != 1 || stats.InFlight != 0 {
		t.Fatalf("调用分类异常: %+v", stats.Summary)
	}
	for _, category := range sourceCallErrors {
		if stats.Summary.Errors[category] != 1 {
			t.Fatal("错误分类缺失", category)
		}
	}
	raw, err := json.Marshal(stats)
	if err != nil || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "private.invalid") {
		t.Fatal("统计不能暴露原始错误、签名地址或完整音乐参数", err)
	}
	if len(stats.Recent[0].Song) > 240 || !utf8.ValidString(stats.Recent[0].Song) || stats.Recent[0].Singer != "测试歌手" {
		t.Fatal("元数据未安全截断")
	}
}

const callStatisticsPrelude = `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl','lyric','pic'],qualitys:['320k','128k']},tx:{type:'music',actions:['musicUrl'],qualitys:['128k']}}});`

func newCallStatisticsManager(t *testing.T) *SourceManager {
	t.Helper()
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	return m
}

func TestSourceCallTracksFallbackDowngradesAndAllActions(t *testing.T) {
	m := newCallStatisticsManager(t)
	ctx := context.Background()
	for i, body := range []string{
		`globalThis.__lx_request=()=>{throw new Error('https://private.invalid/?token=secret')}`,
		`globalThis.__lx_request=({action,info})=>{if(action==='lyric')return {lyric:'[00:00]歌词正文'};if(action==='pic')return '';if(info.type==='320k')throw new Error('当前音质不可用');return {url:'https://media.invalid/?token=secret'}}`,
	} {
		if _, err := m.Load(ctx, int64(i+1), i, "// @name 同名音源\n"+callStatisticsPrelude+body); err != nil {
			t.Fatal(err)
		}
	}
	result, err := m.MusicURL(ctx, "wy", map[string]any{"name": "测试歌曲", "singer": "测试歌手"}, "320k")
	if err != nil || result.SourceID != 2 || result.Quality != "128k" {
		t.Fatal("未按原有规则降级或换源", result, err)
	}
	if _, err := m.Lyric(ctx, "wy", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pic(ctx, "wy", nil); !errors.Is(err, errInvalidSourceResponse) {
		t.Fatal("无效封面应算失败", err)
	}
	first := m.CallStatisticsOf(1, time.Now(), time.Hour)
	second := m.CallStatisticsOf(2, time.Now(), time.Hour)
	if first.Summary.Calls != 3 || first.Summary.Failed != 3 || first.Summary.Attempts != 4 || first.Summary.Errors["script"] != 3 {
		t.Fatalf("失败音源应该独立记录一次取链而非两次音质尝试: %+v", first)
	}
	if second.Summary.Calls != 3 || second.Summary.Success != 2 || second.Summary.Failed != 1 || second.Summary.Downgrades != 1 || second.Summary.Attempts != 4 || second.Summary.Errors["invalid_response"] != 1 {
		t.Fatalf("备用音源调用数据错误: %+v", second)
	}
	if len(second.Recent) != 3 || second.Recent[2].Song != "测试歌曲" || second.Recent[2].RequestedQuality != "320k" || second.Recent[2].Quality != "128k" || !second.Recent[2].Downgraded {
		t.Fatal("未记录最终音质、歌曲或降级信息", second.Recent)
	}
	raw, _ := json.Marshal(second)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "歌词正文") {
		t.Fatal("成功结果正文不应保存到统计")
	}
	if _, err := m.MusicURLForSources(ctx, "tx", nil, "128k", []int64{2}); err != nil {
		t.Fatal(err)
	}
	if got := m.CallStatisticsOf(2, time.Now(), time.Hour); len(got.Groups) != 4 || got.Summary.Calls != 4 {
		t.Fatal("同一脚本的平台统计未分开")
	}
}

func TestSourceCallCancellationTimeoutAndInFlight(t *testing.T) {
	m := newCallStatisticsManager(t)
	script := callStatisticsPrelude + `globalThis.__lx_request=()=>{console.log('统计测试调用已开始');return new Promise(()=>{})};`
	if _, err := m.Load(context.Background(), 1, 1, script); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.MusicURL(ctx, "wy", nil, "320k"); done <- err }()
	t.Cleanup(cancel)
	deadline := time.Now().Add(time.Second)
	for m.CallStatisticsOf(1, time.Now(), time.Hour).InFlight != 1 || !strings.Contains(strings.Join(m.StatusOf(1).Logs, "\n"), "统计测试调用已开始") {
		if time.Now().After(deadline) {
			t.Fatal("未显示进行中的调用")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// 已取消的请求与不支持的平台不会新增调用。
	_, _ = m.MusicURL(ctx, "wy", nil, "320k")
	_, _ = m.Lyric(ctx, "wy", nil)
	_, _ = m.Pic(ctx, "wy", nil)
	_, _ = m.MusicURL(context.Background(), "kw", nil, "320k")
	m.CallTime = 10 * time.Millisecond
	if _, err := m.MusicURL(context.Background(), "wy", nil, "320k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	stats := m.CallStatisticsOf(1, time.Now(), time.Hour)
	if stats.Summary.Calls != 2 || stats.Summary.Cancelled != 1 || stats.Summary.Errors["timeout"] != 1 || stats.Summary.Attempts != 2 || stats.InFlight != 0 {
		t.Fatalf("取消、超时或进行中数量异常: %+v", stats)
	}
}

func TestSourceCallStatisticsSurviveReloadAndUnload(t *testing.T) {
	m := newCallStatisticsManager(t)
	ctx := context.Background()
	script := callStatisticsPrelude + `globalThis.__lx_request=()=>({url:'https://media.invalid/test'});`
	if _, err := m.Load(ctx, 7, 1, script); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MusicURL(ctx, "wy", nil, "320k"); err != nil {
		t.Fatal(err)
	}
	since := m.CallStatisticsOf(7, time.Now(), time.Hour).Since
	for _, script := range []string{script, `throw new Error('加载失败')`} {
		_, _ = m.Load(ctx, 7, 2, script)
		if got := m.CallStatisticsOf(7, time.Now(), time.Hour); got.Summary.Calls != 1 || !got.Since.Equal(since) {
			t.Fatal("重载清除了调用记录")
		}
	}
	m.Unload(7)
	if got := m.CallStatisticsOf(7, time.Now(), time.Hour); got.Summary.Calls != 1 {
		t.Fatal("停用清除了调用记录")
	}
	m.ForgetCallStatistics(7)
	if got := m.CallStatisticsOf(7, time.Now(), time.Hour); got.Total.Calls != 0 {
		t.Fatal("删除后仍有统计")
	}
}

func TestSourceCallConcurrentRecordingAndSnapshots(t *testing.T) {
	s := &sourceCalls{since: time.Now()}
	var tasks sync.WaitGroup
	for i := range 8 {
		tasks.Go(func() {
			for range 100 {
				s.begin()
				s.finish(time.Now(), SourceCall{Platform: "wy", Action: "musicUrl", Attempts: 1}, nil, nil)
				if i == 0 {
					_ = s.snapshot(time.Now(), 24*time.Hour)
				}
			}
		})
	}
	tasks.Wait()
	if got := s.snapshot(time.Now(), time.Hour); got.Summary.Calls != 800 || got.InFlight != 0 || len(got.Recent) != 100 {
		t.Fatalf("并发记录丢失或明细无界: %+v", got.Summary)
	}
}
