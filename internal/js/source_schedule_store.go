package js

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"
	"time"
)

// ScheduleStore 只保存调度样本与截止时间，不保存脚本、地址、凭据或运行中的锁。
type ScheduleStore interface {
	SourceSchedules(context.Context) (map[int64][]byte, error)
	SaveSourceSchedule(context.Context, int64, []byte) error
	DeleteSourceSchedule(context.Context, int64) error
}

type schedulePersistence struct {
	store   ScheduleStore
	flushMu sync.Mutex
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

type persistedSample struct {
	At     int64   `json:"at"`
	Weight float64 `json:"weight"`
	MS     int64   `json:"ms"`
}

type persistedScheduleKey struct {
	Platform, Action string
	Samples          []persistedSample
	Consecutive      int
	LastFail         int64
	Cooldown         int64
	RetryUntil       int64
}

type persistedSchedule struct {
	Version     int
	Fingerprint string
	Keys        []persistedScheduleKey
}

func ScriptFingerprint(script string) string {
	hash := sha256.Sum256([]byte(script))
	return hex.EncodeToString(hash[:])
}

func (m *SourceManager) ScriptIdentity(id int64) string {
	if m == nil || m.schedule == nil {
		return ""
	}
	m.schedule.mu.Lock()
	defer m.schedule.mu.Unlock()
	return m.schedule.fingerprints[id]
}

func timeMS(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}

func fromMS(at int64) time.Time {
	if at <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(at)
}

func (g *sourceScheduler) changed(id int64) {
	g.dirty[id] = true
	if p := g.persistence; p != nil {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

func (g *sourceScheduler) register(id int64, fingerprint string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if old, known := g.fingerprints[id]; known && old != fingerprint {
		g.forgetLocked(id)
	}
	g.fingerprints[id] = fingerprint
	g.changed(id)
}

// prune 清除超龄样本，连续十分钟没有新样本时不延续旧失败惩罚。
func (g *sourceScheduler) prune(key scheduleKey, state *sourceScheduleState, now time.Time) {
	var latest time.Time
	changed := false
	for index, sample := range state.samples {
		if sample.at.After(latest) {
			latest = sample.at
		}
		if !sample.at.IsZero() && (sample.at.After(now) || now.Sub(sample.at) > scheduleSampleTTL) {
			state.samples[index] = scheduleSample{}
			changed = true
		}
	}
	if latest.IsZero() && state.consec > 0 {
		latest = state.lastFail
	}
	if !latest.IsZero() && (latest.After(now) || now.Sub(latest) > scheduleSampleTTL) {
		state.consec, state.lastFail, state.cooldown = 0, time.Time{}, time.Time{}
		changed = true
	}
	if changed {
		g.changed(key.sourceID)
	}
}

func (g *sourceScheduler) persisted(id int64, now time.Time) persistedSchedule {
	out := persistedSchedule{Version: 1, Fingerprint: g.fingerprints[id], Keys: []persistedScheduleKey{}}
	keys := map[scheduleKey]bool{}
	for key := range g.states {
		if key.sourceID == id {
			keys[key] = true
		}
	}
	for key, until := range g.retryUntil {
		if key.sourceID == id && now.Before(until) {
			keys[key] = true
		}
	}
	for key := range keys {
		item := persistedScheduleKey{Platform: key.platform, Action: key.action, Samples: []persistedSample{}}
		if state := g.states[key]; state != nil {
			g.prune(key, state, now)
			for index := range len(state.samples) {
				sample := state.samples[(state.next+index)%len(state.samples)]
				if !sample.at.IsZero() {
					item.Samples = append(item.Samples, persistedSample{sample.at.UnixMilli(), sample.weight, sample.ms})
				}
			}
			item.Consecutive, item.LastFail, item.Cooldown = state.consec, timeMS(state.lastFail), timeMS(state.cooldown)
		}
		if until := g.retryUntil[key]; now.Before(until) {
			item.RetryUntil = until.UnixMilli()
		}
		if len(item.Samples) != 0 || item.Consecutive != 0 || now.Before(fromMS(item.Cooldown)) || item.RetryUntil != 0 {
			out.Keys = append(out.Keys, item)
		}
	}
	slices.SortFunc(out.Keys, func(a, b persistedScheduleKey) int {
		if a.Platform < b.Platform || (a.Platform == b.Platform && a.Action < b.Action) {
			return -1
		}
		if a.Platform == b.Platform && a.Action == b.Action {
			return 0
		}
		return 1
	})
	return out
}

func validPersisted(p persistedSchedule) bool {
	if p.Version != 1 || len(p.Fingerprint) != 64 || len(p.Keys) > 32 {
		return false
	}
	for _, key := range p.Keys {
		if !slices.Contains(append(slices.Clone(AllPlatforms), "local"), key.Platform) || !slices.Contains([]string{"musicUrl", "media", "lyric", "pic"}, key.Action) || len(key.Samples) > scheduleSampleWindow || key.Consecutive < 0 || key.Consecutive > 1<<20 {
			return false
		}
		for _, sample := range key.Samples {
			if sample.At <= 0 || math.IsNaN(sample.Weight) || math.IsInf(sample.Weight, 0) || sample.Weight < -1 || sample.Weight > 1 || sample.MS < -1 || sample.MS > int64(24*time.Hour/time.Millisecond) {
				return false
			}
		}
	}
	return true
}

// EnableSchedulePersistence 在音源及目录导入完成后、接收请求前调用。
// fingerprints 包含停用的音源，避免短暂停用丢失未过期经验。
func (m *SourceManager) EnableSchedulePersistence(ctx context.Context, store ScheduleStore, fingerprints map[int64]string) error {
	records, err := store.SourceSchedules(ctx)
	if err != nil {
		return err
	}
	g := m.schedule
	g.mu.Lock()
	if g.persistence != nil {
		g.mu.Unlock()
		return errors.New("音源调度持久化已启用")
	}
	for id, hash := range fingerprints {
		g.fingerprints[id] = hash
	}
	var discard []int64
	now := g.now()
	for id, raw := range records {
		var saved persistedSchedule
		if len(raw) > 256<<10 || json.Unmarshal(raw, &saved) != nil || !validPersisted(saved) || g.fingerprints[id] == "" || saved.Fingerprint != g.fingerprints[id] {
			discard = append(discard, id)
			continue
		}
		for _, item := range saved.Keys {
			key := scheduleKey{id, item.Platform, item.Action}
			state := &sourceScheduleState{consec: item.Consecutive, lastFail: fromMS(item.LastFail), cooldown: fromMS(item.Cooldown)}
			for index, sample := range item.Samples {
				state.samples[index] = scheduleSample{fromMS(sample.At), sample.Weight, sample.MS}
			}
			state.next = len(item.Samples) % scheduleSampleWindow
			g.prune(key, state, now)
			if !now.Before(state.cooldown) {
				state.cooldown = time.Time{}
			} else if state.cooldown.After(now.Add(scheduleMaxCooldown)) {
				state.cooldown = now.Add(scheduleMaxCooldown)
			}
			g.states[key] = state
			if until := fromMS(item.RetryUntil); item.Action == "musicUrl" && now.Before(until) {
				g.retryUntil[key] = minTime(until, now.Add(scheduleMaxCooldown))
			}
		}
	}
	p := &schedulePersistence{store: store, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	g.persistence = p
	g.mu.Unlock()
	for _, id := range discard {
		if err := store.DeleteSourceSchedule(ctx, id); err != nil {
			m.log.Warn("清理无效调度数据失败", "sourceId", id, "err", err)
		}
	}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-p.wake:
			case <-ticker.C:
			}
			flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := m.FlushSchedulePersistence(flushCtx); err != nil {
				m.log.Warn("保存音源调度状态失败", "err", err)
			}
			cancel()
		}
	}()
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (m *SourceManager) FlushSchedulePersistence(ctx context.Context) error {
	g := m.schedule
	g.mu.Lock()
	p := g.persistence
	g.mu.Unlock()
	if p == nil {
		return nil
	}
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	g.mu.Lock()
	payloads := map[int64][]byte{}
	for id := range g.dirty {
		if g.fingerprints[id] == "" {
			payloads[id] = nil
		} else {
			payloads[id], _ = json.Marshal(g.persisted(id, g.now()))
		}
		delete(g.dirty, id)
	}
	g.mu.Unlock()
	var errs []error
	for id, payload := range payloads {
		var err error
		if payload == nil {
			err = p.store.DeleteSourceSchedule(ctx, id)
		} else {
			err = p.store.SaveSourceSchedule(ctx, id, payload)
		}
		if err != nil {
			g.mu.Lock()
			g.dirty[id] = true
			g.mu.Unlock()
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *SourceManager) CloseSchedulePersistence() error {
	g := m.schedule
	g.mu.Lock()
	p := g.persistence
	g.mu.Unlock()
	if p == nil {
		return nil
	}
	p.once.Do(func() { close(p.stop) })
	<-p.done
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return m.FlushSchedulePersistence(ctx)
}
