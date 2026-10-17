package js

import (
	"context"
	"errors"
	"sync"
	"time"

	"lxsc/internal/admission"
)

// 调度结果类别：决定一次调用如何影响音源的动态排序。
type scheduleOutcome uint8

const (
	// scheduleIgnore 用户取消、整体预算终止或服务繁忙：不计样本、不动连续失败。
	scheduleIgnore scheduleOutcome = iota
	// scheduleSuccess 调用成功；音质降级时权重略降。
	scheduleSuccess
	// scheduleStrongFail 超时、脚本失败、返回无效或媒体校验明确拒绝：计入连续失败。
	scheduleStrongFail
	// scheduleSlow 其他音源先成功后本调用被取消：只作为"较慢"弱负面样本。
	scheduleSlow
)

// classifyOutcome 依据调用错误区分调度口径。
// parent 为原始请求 ctx；budgetCtx 为本音源派生的预算 ctx——
// parent 已终止时不惩罚任何源，budgetCtx 被内部取消且 parent 存活说明赢家已出现。
func classifyOutcome(parent, budgetCtx context.Context, err error) scheduleOutcome {
	switch {
	case err == nil:
		return scheduleSuccess
	case parent.Err() != nil:
		return scheduleIgnore
	case errors.Is(err, admission.ErrBusy):
		return scheduleIgnore
	case errors.Is(budgetCtx.Err(), context.Canceled):
		return scheduleSlow
	default:
		return scheduleStrongFail
	}
}

const (
	scheduleSampleWindow = 20               // 每键最多保留样本数
	scheduleSampleTTL    = 10 * time.Minute // 评分时同时丢弃超龄样本
	scheduleFailGrace    = 5 * time.Second   // 失败合并窗口：窗口内并发失败只递增一次连续失败
	scheduleBaseCooldown = 30 * time.Second
	scheduleMaxCooldown  = 5 * time.Minute
	// 降级成功与"较慢"取消的权重；强失败为 -1，普通成功为 +1。
	scheduleDowngradeWeight = 0.9
	scheduleSlowWeight     = -0.25
)

type scheduleKey struct {
	sourceID int64
	platform string
	action   string
}

type scheduleSample struct {
	at     time.Time
	weight float64
	ms     int64 // <0 表示没有延迟样本（如媒体反馈），不进延迟平均
}

// sourceScheduleState 每个音源×平台×动作的调度状态；内存态，重启重置。
type sourceScheduleState struct {
	samples  [scheduleSampleWindow]scheduleSample
	next     int
	consec   int       // 连续强失败次数
	lastFail time.Time // 最近一次强失败（合并窗口判断）
	cooldown time.Time // 冷却截止时间；零值表示未冷却
}

// sourceScheduler 按运行表现为音源排序：滑动窗口评分 + 指数退避冷却。
// 冷却中的音源排最后但不删除，全部源冷却时仍可兜底；到期后回到正常排序层自然获得探测机会。
type sourceScheduler struct {
	mu     sync.Mutex
	states map[scheduleKey]*sourceScheduleState
	now    func() time.Time
}

func newSourceScheduler() *sourceScheduler {
	return &sourceScheduler{states: map[scheduleKey]*sourceScheduleState{}, now: time.Now}
}

func (g *sourceScheduler) stateOf(key scheduleKey) *sourceScheduleState {
	s := g.states[key]
	if s == nil {
		s = &sourceScheduleState{}
		g.states[key] = s
	}
	return s
}

// record 记录一次调用结果；ignore 完全无痕。
func (g *sourceScheduler) record(id int64, platform, action string, outcome scheduleOutcome, downgrade bool, ms int64) {
	if outcome == scheduleIgnore {
		return
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stateOf(scheduleKey{id, platform, action})
	sample := scheduleSample{at: now, ms: ms}
	switch outcome {
	case scheduleSuccess:
		s.consec = 0
		s.cooldown = time.Time{}
		sample.weight = 1
		if downgrade {
			sample.weight = scheduleDowngradeWeight
		}
	case scheduleStrongFail:
		if s.lastFail.IsZero() || now.Sub(s.lastFail) > scheduleFailGrace {
			s.consec++
		}
		s.lastFail = now
		if s.consec >= 2 {
			// 2 次起 30s，逐次翻倍，封顶 5 分钟。
			shift := uint(min(s.consec-2, 20))
			s.cooldown = now.Add(min(scheduleBaseCooldown<<shift, scheduleMaxCooldown))
		}
		sample.weight = -1
		sample.ms = -1
	case scheduleSlow:
		sample.weight = scheduleSlowWeight
	}
	s.samples[s.next] = sample
	s.next = (s.next + 1) % len(s.samples)
}

// rank 返回评分与冷却状态：score = 窗口内平均权重 - 成功延迟惩罚（平均每慢 1 秒扣 0.1，封顶 0.25）。
// 无有效样本时返回 0，由 id 决出先后（加载序）。
func (g *sourceScheduler) rank(id int64, platform, action string) (float64, bool) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.states[scheduleKey{id, platform, action}]
	if s == nil {
		return 0, false
	}
	var sum, succSum float64
	var succCount int
	count := 0
	for _, sample := range s.samples {
		if sample.at.IsZero() || now.Sub(sample.at) > scheduleSampleTTL {
			continue
		}
		count++
		sum += sample.weight
		if sample.weight > 0 && sample.ms >= 0 {
			succSum += float64(sample.ms)
			succCount++
		}
	}
	if count == 0 {
		return 0, now.Before(s.cooldown)
	}
	latency := float64(0)
	if succCount > 0 {
		latency = succSum / float64(succCount) / 10000
	}
	return sum/float64(count) - min(latency, 0.25), now.Before(s.cooldown)
}

// forget 只在音源被成功删除后调用，清理其全部调度键。
func (g *sourceScheduler) forget(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for key := range g.states {
		if key.sourceID == id {
			delete(g.states, key)
		}
	}
}

// ForgetSchedule 与统计清理一样只在删除成功后调用。
func (m *SourceManager) ForgetSchedule(id int64) {
	if m != nil && m.schedule != nil {
		m.schedule.forget(id)
	}
}

// ReportMediaOutcome 媒体校验结果反馈，只携带身份与结论，不携带地址或错误原文。
// 播放层在响应头校验成功或明确拒绝时调用；网络不确定不反馈。
func (m *SourceManager) ReportMediaOutcome(sourceID int64, platform string, ok bool) {
	if m == nil || m.schedule == nil || sourceID <= 0 || platform == "" {
		return
	}
	outcome := scheduleStrongFail
	if ok {
		outcome = scheduleSuccess
	}
	m.schedule.record(sourceID, platform, "musicUrl", outcome, false, -1)
}
