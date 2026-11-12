package js

import (
	"context"
	"errors"
	"slices"
	"strings"
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
)

// classifyOutcome 依据调用错误区分调度口径。
// parent 为发起调用的请求 ctx：它已终止（用户取消，或其它候选先胜出）时不惩罚音源。
func classifyOutcome(parent context.Context, err error) scheduleOutcome {
	switch {
	case err == nil:
		return scheduleSuccess
	case parent.Err() != nil:
		return scheduleIgnore
	case errors.Is(err, admission.ErrBusy):
		return scheduleIgnore
	case errors.Is(err, ErrNoQuality):
		return scheduleIgnore
	default:
		return scheduleStrongFail
	}
}

const (
	scheduleSampleWindow = 20               // 每键最多保留样本数
	scheduleSampleTTL    = 10 * time.Minute // 评分时同时丢弃超龄样本
	scheduleFailGrace    = 5 * time.Second  // 失败合并窗口：窗口内并发失败只递增一次连续失败
	scheduleBaseCooldown = 30 * time.Second
	scheduleMaxCooldown  = 5 * time.Minute
	// 降级成功的权重；强失败为 -1，普通成功为 +1。
	scheduleDowngradeWeight = 0.9
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

// sourceScheduleState 每个音源×平台×动作的调度状态，持久化时保留原始样本时间。
type sourceScheduleState struct {
	samples  [scheduleSampleWindow]scheduleSample
	next     int
	consec   int       // 连续强失败次数
	lastFail time.Time // 最近计入的失败批次起点，窗口不能随并发反馈滑动
	cooldown time.Time // 冷却截止时间；零值表示未冷却
}

// sourceScheduler 按运行表现为音源排序：滑动窗口评分 + 指数退避冷却。
// 冷却中的音源排最后但不删除：播放选源仍会尝试它，只是并发名额先给健康的音源。
type sourceScheduler struct {
	mu           sync.Mutex
	states       map[scheduleKey]*sourceScheduleState
	now          func() time.Time
	retryUntil   map[scheduleKey]time.Time
	fingerprints map[int64]string
	dirty        map[int64]bool
	persistence  *schedulePersistence
}

func newSourceScheduler() *sourceScheduler {
	return &sourceScheduler{states: map[scheduleKey]*sourceScheduleState{}, retryUntil: map[scheduleKey]time.Time{}, fingerprints: map[int64]string{}, dirty: map[int64]bool{}, now: time.Now}
}

func (g *sourceScheduler) canCall(id int64, platform string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.now().Before(g.retryUntil[scheduleKey{id, platform, "musicUrl"}])
}

// BackoffMusicURL 尊重受控的 Retry-After；不通过错误字符串猜测限流。
func (m *SourceManager) BackoffMusicURL(id int64, platform string, delay time.Duration, fingerprint ...string) {
	g := m.schedule
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(fingerprint) != 0 && fingerprint[0] != "" && fingerprint[0] != g.fingerprints[id] {
		return
	}
	key := scheduleKey{id, platform, "musicUrl"}
	until := g.now().Add(min(5*time.Minute, max(time.Second, delay)))
	if until.After(g.retryUntil[key]) {
		g.retryUntil[key] = until
		g.changed(id)
	}
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
func (g *sourceScheduler) record(id int64, platform, action string, outcome scheduleOutcome, downgrade bool, ms int64, fingerprint ...string) {
	if outcome == scheduleIgnore {
		return
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(fingerprint) != 0 && fingerprint[0] != "" && fingerprint[0] != g.fingerprints[id] {
		return
	}
	s := g.stateOf(scheduleKey{id, platform, action})
	g.prune(scheduleKey{id, platform, action}, s, now)
	sample := scheduleSample{at: now, ms: ms}
	switch outcome {
	case scheduleSuccess:
		s.consec = 0
		s.cooldown = time.Time{}
		s.lastFail = time.Time{}
		sample.weight = 1
		if downgrade {
			sample.weight = scheduleDowngradeWeight
		}
	case scheduleStrongFail:
		if s.lastFail.IsZero() || now.Sub(s.lastFail) >= scheduleFailGrace {
			s.consec++
			s.lastFail = now
			if s.consec >= 2 {
				// 2 次起 30s，逐次翻倍，封顶 5 分钟。
				shift := uint(min(s.consec-2, 20))
				s.cooldown = now.Add(min(scheduleBaseCooldown<<shift, scheduleMaxCooldown))
			}
		}
		sample.weight = -1
		sample.ms = -1
	}
	s.samples[s.next] = sample
	s.next = (s.next + 1) % len(s.samples)
	g.changed(id)
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
	g.prune(scheduleKey{id, platform, action}, s, now)
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
	g.forgetLocked(id)
	delete(g.fingerprints, id)
	g.changed(id)
}

func (g *sourceScheduler) forgetLocked(id int64) {
	for key := range g.states {
		if key.sourceID == id {
			delete(g.states, key)
		}
	}
	for key := range g.retryUntil {
		if key.sourceID == id {
			delete(g.retryUntil, key)
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
func (m *SourceManager) ReportMediaOutcome(sourceID int64, platform string, ok bool, latencyMS ...int64) {
	m.ReportMediaForScript(sourceID, platform, "", ok, latencyMS...)
}

func (m *SourceManager) ReportMediaForScript(sourceID int64, platform, fingerprint string, ok bool, latencyMS ...int64) {
	if m == nil || m.schedule == nil || sourceID <= 0 || platform == "" {
		return
	}
	outcome := scheduleStrongFail
	if ok {
		outcome = scheduleSuccess
	}
	ms := int64(-1)
	if len(latencyMS) > 0 {
		ms = latencyMS[0]
	}
	m.schedule.record(sourceID, platform, "media", outcome, false, ms, fingerprint)
}

// ScheduleStatus 仅用于完整诊断，取链和有效媒体样本分开列出。
type ScheduleStatus struct {
	Platform            string    `json:"platform"`
	Action              string    `json:"action"`
	Score               float64   `json:"score"`
	Samples             int       `json:"samples"`
	Success             int       `json:"success"`
	Failed              int       `json:"failed"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	CoolingUntil        time.Time `json:"coolingUntil"`
	RetryUntil          time.Time `json:"retryUntil"`
	P50MS               int64     `json:"p50Ms"`
	P95MS               int64     `json:"p95Ms"`
	P99MS               int64     `json:"p99Ms"`
}

func (m *SourceManager) ScheduleSnapshot(id int64) []ScheduleStatus {
	g := m.schedule
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	items := []ScheduleStatus{}
	for key, state := range g.states {
		if key.sourceID != id {
			continue
		}
		g.prune(key, state, now)
		item := ScheduleStatus{Platform: key.platform, Action: key.action, ConsecutiveFailures: state.consec, CoolingUntil: state.cooldown, RetryUntil: g.retryUntil[scheduleKey{id, key.platform, "musicUrl"}]}
		var times []int64
		var sum float64
		for _, sample := range state.samples {
			if sample.at.IsZero() || now.Sub(sample.at) > scheduleSampleTTL {
				continue
			}
			item.Samples++
			sum += sample.weight
			if sample.weight > 0 {
				item.Success++
				if sample.ms >= 0 {
					times = append(times, sample.ms)
				}
			} else if sample.weight == -1 {
				item.Failed++
			}
		}
		if item.Samples > 0 {
			item.Score = sum / float64(item.Samples)
		}
		if len(times) > 0 {
			var total int64
			for _, ms := range times {
				total += ms
			}
			item.Score -= min(float64(total)/float64(len(times))/10000, 0.25)
			slices.Sort(times)
			item.P50MS = times[(len(times)-1)*50/100]
			item.P95MS = times[(len(times)*95+99)/100-1]
			item.P99MS = times[(len(times)*99+99)/100-1]
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b ScheduleStatus) int {
		if a.Platform != b.Platform {
			return strings.Compare(a.Platform, b.Platform)
		}
		return strings.Compare(a.Action, b.Action)
	})
	return items
}
