package js

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"lxsc/internal/admission"
)

// newTestClock 返回可推进的调度器与时钟。
func newTestClock() (*sourceScheduler, *time.Time) {
	g := newSourceScheduler()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	return g, &now
}

func TestClassifyOutcome(t *testing.T) {
	scriptErr := errors.New("脚本失败")
	newParent := func() func() (context.Context, context.CancelFunc) {
		return func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }
	}
	for _, tc := range []struct {
		name   string
		parent func() (context.Context, context.CancelFunc)
		budget func(context.Context) (context.Context, context.CancelFunc)
		err    error
		want   scheduleOutcome
	}{
		{"成功", newParent(), func(p context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(p) }, nil, scheduleSuccess},
		{"用户取消", newParent(), func(p context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(p) }, context.Canceled, scheduleIgnore},
		{"繁忙", newParent(), func(p context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(p) }, admission.ErrBusy, scheduleIgnore},
		{"赢家出现", newParent(), func(p context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(p) }, context.Canceled, scheduleSlow},
		{"本源超时", newParent(), func(p context.Context) (context.Context, context.CancelFunc) { return context.WithTimeout(p, 0) }, context.DeadlineExceeded, scheduleStrongFail},
		{"返回无效", newParent(), func(p context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(p) }, errInvalidSourceResponse, scheduleStrongFail},
		{"脚本错误", newParent(), func(p context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(p) }, scriptErr, scheduleStrongFail},
	} {
		parent, parentCancel := tc.parent()
		budget, budgetCancel := tc.budget(parent)
		switch tc.name {
		case "用户取消":
			parentCancel()
		case "赢家出现":
			budgetCancel()
		}
		if got := classifyOutcome(parent, budget, tc.err); got != tc.want {
			t.Errorf("%s 分类错误: got=%v want=%v", tc.name, got, tc.want)
		}
		parentCancel()
		budgetCancel()
	}
}

func TestSchedulerColdStartAndIgnoreLeaveNoTrace(t *testing.T) {
	g, now := newTestClock()
	if score, cooling := g.rank(1, "wy", "musicUrl"); score != 0 || cooling {
		t.Fatalf("无样本必须为 (0,false): %v %v", score, cooling)
	}
	g.record(1, "wy", "musicUrl", scheduleIgnore, false, 100)
	*now = now.Add(time.Minute)
	g.record(1, "wy", "musicUrl", scheduleIgnore, false, 100)
	if len(g.states) != 0 {
		t.Fatalf("ignore 不应创建状态: %d", len(g.states))
	}
}

func TestSchedulerScoresLatencyAndFailure(t *testing.T) {
	g, _ := newTestClock()
	// 快源：成功 200ms；慢源：成功 3s；坏源：一半强失败。
	for range 20 {
		g.record(1, "wy", "musicUrl", scheduleSuccess, false, 200)
		g.record(2, "wy", "musicUrl", scheduleSuccess, false, 3000)
	}
	for range 10 {
		g.record(3, "wy", "musicUrl", scheduleSuccess, false, 200)
		g.record(3, "wy", "musicUrl", scheduleStrongFail, false, -1)
	}
	fast, _ := g.rank(1, "wy", "musicUrl")
	slow, _ := g.rank(2, "wy", "musicUrl")
	bad, _ := g.rank(3, "wy", "musicUrl")
	if !(fast > slow && slow > bad) {
		t.Fatalf("快源应优于慢源、慢源应优于高失败源: fast=%v slow=%v bad=%v", fast, slow, bad)
	}
	if fast > 1 || bad < -1 {
		t.Fatalf("评分越界: fast=%v bad=%v", fast, bad)
	}
}

func TestSchedulerWindowAndTTL(t *testing.T) {
	g, now := newTestClock()
	for range scheduleSampleWindow + 5 {
		g.record(1, "wy", "musicUrl", scheduleSuccess, false, 100)
	}
	// 环形覆盖后仍只统计窗口内样本；100ms 成功的评分 = 1 - 0.01。
	if score, _ := g.rank(1, "wy", "musicUrl"); score < 0.98 || score > 1 {
		t.Fatalf("成功样本评分异常: %v", score)
	}
	*now = now.Add(scheduleSampleTTL + time.Second)
	if score, _ := g.rank(1, "wy", "musicUrl"); score != 0 {
		t.Fatalf("超龄样本必须丢弃: %v", score)
	}
}

func TestSchedulerDowngradeWeight(t *testing.T) {
	g, _ := newTestClock()
	for range 20 {
		g.record(1, "wy", "musicUrl", scheduleSuccess, true, 100)
		g.record(2, "wy", "musicUrl", scheduleSuccess, false, 100)
	}
	downgraded, _ := g.rank(1, "wy", "musicUrl")
	full, _ := g.rank(2, "wy", "musicUrl")
	if downgraded >= full {
		t.Fatalf("降级成功必须略低于完整成功: %v %v", downgraded, full)
	}
}

func TestSchedulerCooldownStateMachine(t *testing.T) {
	g, now := newTestClock()
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	if _, cooling := g.rank(1, "wy", "musicUrl"); cooling {
		t.Fatal("单次强失败不触发冷却")
	}
	*now = now.Add(scheduleFailGrace + time.Second)
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	state := g.states[scheduleKey{1, "wy", "musicUrl"}]
	if state.cooldown.Sub(*now) != scheduleBaseCooldown {
		t.Fatalf("连续两次强失败应冷却基础时长: %v", state.cooldown.Sub(*now))
	}
	if _, cooling := g.rank(1, "wy", "musicUrl"); !cooling {
		t.Fatal("连续两次强失败必须进入冷却")
	}
	// 成功立即恢复。
	g.record(1, "wy", "musicUrl", scheduleSuccess, false, 100)
	if _, cooling := g.rank(1, "wy", "musicUrl"); cooling {
		t.Fatal("成功必须清除冷却")
	}
	if state.consec != 0 {
		t.Fatalf("成功必须清零连续失败: %d", state.consec)
	}
	// 下一轮从清零重新累计：第 1 次失败无冷却，第 2 次 30s、每次翻倍、封顶 5 分钟。
	// 每次失败都推进时钟越过合并窗口，确保各自计入连续失败。
	wantDurations := []time.Duration{0, 30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 300 * time.Second}
	for i, want := range wantDurations {
		*now = now.Add(scheduleFailGrace + time.Second)
		g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
		if want == 0 {
			if !state.cooldown.IsZero() {
				t.Fatalf("清零后第 %d 次失败不得冷却: %v", i+1, state.cooldown)
			}
			continue
		}
		if got := state.cooldown.Sub(*now); got != want {
			t.Fatalf("第 %d 次连续失败冷却时长错误: got=%v want=%v", i+1, got, want)
		}
	}
	// 到期解除冷却。
	*now = state.cooldown.Add(time.Second)
	if _, cooling := g.rank(1, "wy", "musicUrl"); cooling {
		t.Fatal("冷却到期后必须解除")
	}
}

func TestSchedulerFailGraceMergesConcurrentFailures(t *testing.T) {
	g, now := newTestClock()
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	state := g.states[scheduleKey{1, "wy", "musicUrl"}]
	// 窗口内的第二次失败只刷新时间，不重复递增。
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	if state.consec != 1 {
		t.Fatalf("合并窗口内不得重复递增连续失败: %d", state.consec)
	}
	if !state.cooldown.IsZero() {
		t.Fatal("单次有效失败不得触发冷却")
	}
	// 超出窗口后递增并触发冷却。
	*now = now.Add(scheduleFailGrace + time.Second)
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	if state.consec != 2 {
		t.Fatalf("超出窗口应递增: %d", state.consec)
	}
	if state.cooldown.IsZero() {
		t.Fatal("连续失败达到两次必须设置冷却")
	}
}

func TestSchedulerSlowIsWeakSignalOnly(t *testing.T) {
	g, now := newTestClock()
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	state := g.states[scheduleKey{1, "wy", "musicUrl"}]
	consecBefore := state.consec
	g.record(1, "wy", "musicUrl", scheduleSlow, false, 800)
	if state.consec != consecBefore {
		t.Fatal("较慢信号不得改变连续失败计数")
	}
	sample := state.samples[(state.next+len(state.samples)-1)%len(state.samples)]
	if sample.weight != scheduleSlowWeight {
		t.Fatalf("较慢信号必须使用固定弱权重: %+v", sample)
	}
	// 冷却中的音源收到慢信号不改变冷却截止。
	*now = now.Add(scheduleFailGrace + time.Second)
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	cooldownBefore := state.cooldown
	g.record(1, "wy", "musicUrl", scheduleSlow, false, 800)
	if !state.cooldown.Equal(cooldownBefore) {
		t.Fatal("弱信号不得延长冷却")
	}
}

func TestSchedulerConcurrentRecordAndRank(t *testing.T) {
	g, _ := newTestClock()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for range 100 {
				g.record(int64(i%4+1), "wy", "musicUrl", scheduleSuccess, i%2 == 0, 100)
				_, _ = g.rank(int64(i%4+1), "wy", "musicUrl")
			}
		})
	}
	wg.Wait()
	for id := range int64(4) {
		if score, cooling := g.rank(id+1, "wy", "musicUrl"); cooling || score <= 0 {
			t.Fatalf("并发后状态异常: id=%d score=%v cooling=%v", id+1, score, cooling)
		}
	}
}

func TestSchedulerForget(t *testing.T) {
	g, _ := newTestClock()
	g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
	g.record(1, "tx", "musicUrl", scheduleSuccess, false, 100)
	g.forget(1)
	if len(g.states) != 0 {
		t.Fatalf("forget 必须清理该源全部键: %d", len(g.states))
	}
	g.record(2, "wy", "musicUrl", scheduleSuccess, false, 100)
	g.forget(1)
	if len(g.states) != 1 {
		t.Fatal("forget 不存在的源不得影响其他源")
	}
}

const scheduleTestPrelude = `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`

func TestManagerDynamicReorderAfterFailures(t *testing.T) {
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	// 注入虚拟时钟，让端到端取链的失败能自然跨越合并窗口。
	fake := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.schedule.now = func() time.Time { return fake }
	broken := scheduleTestPrelude + `globalThis.__lx_request=()=>{throw new Error('渠道故障')};`
	healthy := scheduleTestPrelude + `globalThis.__lx_request=()=>({url:'https://media.invalid/ok'});`
	if _, err := m.Load(context.Background(), 1, broken); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(context.Background(), 2, healthy); err != nil {
		t.Fatal(err)
	}
	if got, want := m.MusicURLSourceIDs("wy"), []int64{1, 2}; !slicesEqual(got, want) {
		t.Fatalf("冷启动必须按加载顺序: %v", got)
	}
	// 反复取链：坏源强失败后评分低于好源，调度器把好源排到前面；
	// 首选在等待窗口内成功时不再启动备用，坏源不再被浪费调用。
	for range 3 {
		result, err := m.MusicURL(context.Background(), "wy", nil, "320k")
		if err != nil || result.SourceID != 2 {
			t.Fatalf("应始终返回好源结果: %+v %v", result, err)
		}
	}
	if got := m.MusicURLSourceIDs("wy"); got[0] != 2 || got[1] != 1 {
		t.Fatalf("动态排序应把好源排前: %v", got)
	}
	// 坏源只被调用过一次（第一次取链），此后不再被首选路径触碰。
	if got := m.CallStatisticsOf(1, time.Now(), time.Hour); got.Total.Calls != 1 {
		t.Fatalf("降级后的坏源不应被重复调用: %d", got.Total.Calls)
	}
	// 冷却兜底：所有源都不可用时仍保留候选身份。
	if got := m.MusicURLSourceIDs("wy"); len(got) != 2 {
		t.Fatal("动态排序不得删除任何候选")
	}
}

func TestManagerCooldownSourceStaysAsFallback(t *testing.T) {
	// 独立验证：强失败次数跨窗口累计到阈值后，源进入冷却并排最后，但仍可被兜底选中。
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	fake := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.schedule.now = func() time.Time { return fake }
	broken := scheduleTestPrelude + `globalThis.__lx_request=()=>{throw new Error('渠道故障')};`
	if _, err := m.Load(context.Background(), 1, broken); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(context.Background(), 2, scheduleTestPrelude+`globalThis.__lx_request=()=>{throw new Error('也坏了')};`); err != nil {
		t.Fatal(err)
	}
	// 两个坏源交替失败：每次取链两者都失败，跨窗口后 consec 达到 2 触发冷却。
	for range 2 {
		_, _ = m.MusicURL(context.Background(), "wy", nil, "320k")
		fake = fake.Add(scheduleFailGrace + time.Second)
	}
	if _, cooling := m.schedule.rank(1, "wy", "musicUrl"); !cooling {
		t.Fatal("跨窗口两次强失败后应进入冷却")
	}
	if _, cooling := m.schedule.rank(2, "wy", "musicUrl"); !cooling {
		t.Fatal("第二个坏源同样应进入冷却")
	}
	got := m.MusicURLSourceIDs("wy")
	if len(got) != 2 {
		t.Fatal("冷却中的音源仍必须保留在候选列表")
	}
	// 两个冷却源同分，按加载序（ID 升序）保持稳定。
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("同分冷却源应按加载序: %v", got)
	}
}

func slicesEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestManagerUserCancellationLeavesNoTrace(t *testing.T) {
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	hang := scheduleTestPrelude + `globalThis.__lx_request=()=>new Promise(()=>{});`
	if _, err := m.Load(context.Background(), 1, hang); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.MusicURL(ctx, "wy", nil, "320k"); done <- err }()
	deadline := time.After(2 * time.Second)
	for m.CallStatisticsOf(1, time.Now(), time.Hour).InFlight != 1 {
		select {
		case <-deadline:
			t.Fatal("调用未开始")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(m.schedule.states) != 0 {
		t.Fatalf("用户取消不得留下调度样本: %d", len(m.schedule.states))
	}
}

func TestManagerSlowPrimaryRecordsWeakSignal(t *testing.T) {
	m := startupSources(t,
		`globalThis.__lx_request=()=>new Promise((resolve,reject)=>setTimeout(()=>reject(new Error('太慢')),30000));`,
		`globalThis.__lx_request=()=>({url:'https://media.invalid/backup'});`,
	)
	result, err := m.MusicURL(context.Background(), "wy", nil, "320k")
	if err != nil || result.SourceID != 2 {
		t.Fatalf("备用源应先成功: %+v %v", result, err)
	}
	// 慢源在 1.5s 后被启动，随后被赢家取消：应记为弱信号而非强失败。
	state := m.schedule.states[scheduleKey{1, "wy", "musicUrl"}]
	if state == nil {
		t.Fatal("慢源被取消后应有调度样本")
	}
	if state.consec != 0 {
		t.Fatalf("弱信号不得计入连续失败: %d", state.consec)
	}
	found := false
	for _, sample := range state.samples {
		if sample.at.IsZero() {
			continue
		}
		if sample.weight == scheduleSlowWeight {
			found = true
		}
		if sample.weight == -1 {
			t.Fatal("被赢家取消的慢源不得记为强失败")
		}
	}
	if !found {
		t.Fatal("慢源应记录较慢弱信号")
	}
}

func TestReportMediaOutcome(t *testing.T) {
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	// 注入虚拟时钟，使第二次拒绝自然跨越合并窗口。
	fake := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.schedule.now = func() time.Time { return fake }
	m.ReportMediaOutcome(0, "wy", true)
	m.ReportMediaOutcome(1, "", true)
	if len(m.schedule.states) != 0 {
		t.Fatal("非法身份的反馈必须忽略")
	}
	m.ReportMediaOutcome(1, "wy", false)
	state := m.schedule.states[scheduleKey{1, "wy", "media"}]
	if state.consec != 1 {
		t.Fatalf("首次媒体拒绝应计入连续失败: %d", state.consec)
	}
	fake = fake.Add(scheduleFailGrace + time.Second)
	m.ReportMediaOutcome(1, "wy", false)
	if _, cooling := m.schedule.rank(1, "wy", "media"); !cooling {
		t.Fatal("跨窗口的第二次媒体拒绝应触发冷却")
	}
	m.ReportMediaOutcome(1, "wy", true)
	if _, cooling := m.schedule.rank(1, "wy", "media"); cooling {
		t.Fatal("媒体成功反馈应恢复排名")
	}
	if got := m.CallStatisticsOf(1, time.Now(), time.Hour); got.Total.Calls != 0 {
		t.Fatal("媒体反馈不得混入调用统计")
	}
}
