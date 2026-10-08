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

const (
	sourceCallBucketWidth = 5 * time.Minute
	sourceCallBucketCount = 288 // 最多保留 24 小时；汇总不受明细条数限制。
	sourceCallRecentLimit = 100
)

var errInvalidSourceResponse = errors.New("音源返回数据无效")

// SourceCall 只保存调用所需的白名单信息，不保留直链、歌词正文或原始错误。
type SourceCall struct {
	At               time.Time `json:"at"`
	Platform         string    `json:"platform"`
	Action           string    `json:"action"`
	Song             string    `json:"song,omitempty"`
	Singer           string    `json:"singer,omitempty"`
	RequestedQuality string    `json:"requestedQuality,omitempty"`
	Quality          string    `json:"quality,omitempty"`
	Attempts         int       `json:"attempts"`
	DurationMS       int64     `json:"durationMs"`
	Outcome          string    `json:"outcome"` // success / failed / cancelled
	ErrorCategory    string    `json:"errorCategory,omitempty"`
	Downgraded       bool      `json:"downgraded"`
}

type SourceCallSummary struct {
	Calls      int64            `json:"calls"`
	Success    int64            `json:"success"`
	Failed     int64            `json:"failed"`
	Cancelled  int64            `json:"cancelled"`
	Downgrades int64            `json:"downgrades"`
	Attempts   int64            `json:"attempts"`
	DurationMS int64            `json:"durationMs"`
	AverageMS  int64            `json:"averageMs"`
	MaxMS      int64            `json:"maxMs"`
	Errors     map[string]int64 `json:"errors"`
}

type SourceCallGroup struct {
	Platform string `json:"platform"`
	Action   string `json:"action"`
	SourceCallSummary
}

type SourceCallPoint struct {
	At time.Time `json:"at"`
	SourceCallSummary
}

type SourceCallStatistics struct {
	Since         time.Time         `json:"since"`
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
	BucketMinutes int               `json:"bucketMinutes"`
	InFlight      int64             `json:"inFlight"`
	Total         SourceCallSummary `json:"total"`
	Summary       SourceCallSummary `json:"summary"`
	Groups        []SourceCallGroup `json:"groups"`
	Trend         []SourceCallPoint `json:"trend"`
	Recent        []SourceCall      `json:"recent"`
}

var sourceCallErrors = [...]string{"timeout", "busy", "cancelled", "invalid_response", "script"}

type sourceCallCounters struct {
	calls, success, downgrades, attempts, durationMS, maxMS int64
	errors                                                  [len(sourceCallErrors)]int64
}

func (c *sourceCallCounters) add(other sourceCallCounters) {
	c.calls += other.calls
	c.success += other.success
	c.downgrades += other.downgrades
	c.attempts += other.attempts
	c.durationMS += other.durationMS
	c.maxMS = max(c.maxMS, other.maxMS)
	for i, count := range other.errors {
		c.errors[i] += count
	}
}

func (c sourceCallCounters) summary() SourceCallSummary {
	s := SourceCallSummary{Calls: c.calls, Success: c.success, Downgrades: c.downgrades, Attempts: c.attempts, DurationMS: c.durationMS, MaxMS: c.maxMS, Errors: map[string]int64{}}
	for i, count := range c.errors {
		if count > 0 {
			s.Errors[sourceCallErrors[i]] = count
		}
	}
	s.Cancelled = s.Errors["cancelled"]
	s.Failed = s.Calls - s.Success - s.Cancelled
	if s.Calls > 0 {
		s.AverageMS = s.DurationMS / s.Calls
	}
	return s
}

type sourceCallKey struct{ platform, action string }
type sourceCallBucket struct {
	at     time.Time
	groups map[sourceCallKey]sourceCallCounters
}

// sourceCalls 的存储上限固定，记录时不访问数据库，也不阻塞其他音源。
type sourceCalls struct {
	mu       sync.Mutex
	since    time.Time
	inFlight int64
	total    sourceCallCounters
	buckets  [sourceCallBucketCount]sourceCallBucket
	recent   [sourceCallRecentLimit]SourceCall
	next     int
}

func (s *sourceCalls) begin() {
	s.mu.Lock()
	s.inFlight++
	s.mu.Unlock()
}

func sourceCallCategory(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, admission.ErrBusy):
		return "busy"
	case errors.Is(err, errInvalidSourceResponse):
		return "invalid_response"
	case err != nil:
		return "script"
	default:
		return ""
	}
}

func (s *sourceCalls) finish(start time.Time, call SourceCall, musicInfo any, err error) {
	call.At = time.Now()
	call.DurationMS = max(0, call.At.Sub(start).Milliseconds())
	call.ErrorCategory = sourceCallCategory(err)
	call.Outcome = "success"
	if call.ErrorCategory == "cancelled" {
		call.Outcome = "cancelled"
	} else if err != nil {
		call.Outcome = "failed"
	}
	// 防止非标准音质与任意 musicInfo 内容进入统计记录。
	if _, ok := qualityRank[call.RequestedQuality]; !ok {
		call.RequestedQuality = ""
	}
	if info, ok := musicInfo.(map[string]any); ok {
		call.Song = sourceCallText(info["name"])
		call.Singer = sourceCallText(info["singer"])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	if call.Attempts > 0 {
		s.record(call)
	}
}

func sourceCallText(value any) string {
	text, _ := value.(string)
	// 先限制字节数，再修复 UTF-8，避免为超长平台字段分配大切片。
	if len(text) > 240 {
		text = text[:240]
	}
	return strings.Clone(strings.TrimSpace(strings.ToValidUTF8(text, "")))
}

// record 在持锁时调用；以调用完成时间归入固定时间段。
func (s *sourceCalls) record(call SourceCall) {
	c := sourceCallCounters{calls: 1, attempts: int64(call.Attempts), durationMS: call.DurationMS, maxMS: call.DurationMS}
	if call.Outcome == "success" {
		c.success = 1
	}
	if call.Downgraded {
		c.downgrades = 1
	}
	for i, category := range sourceCallErrors {
		if call.ErrorCategory == category {
			c.errors[i] = 1
		}
	}
	s.total.add(c)
	s.recent[s.next] = call
	s.next = (s.next + 1) % len(s.recent)
	at := call.At.Truncate(sourceCallBucketWidth)
	bucket := &s.buckets[(at.Unix()/int64(sourceCallBucketWidth/time.Second))%sourceCallBucketCount]
	if !bucket.at.Equal(at) {
		*bucket = sourceCallBucket{at: at, groups: map[sourceCallKey]sourceCallCounters{}}
	}
	key := sourceCallKey{call.Platform, call.Action}
	group := bucket.groups[key]
	group.add(c)
	bucket.groups[key] = group
}

func (s *sourceCalls) snapshot(now time.Time, window time.Duration) SourceCallStatistics {
	// 所有来源使用同一时间边界，当前五分钟段尚未结束。
	end := now.Truncate(sourceCallBucketWidth).Add(sourceCallBucketWidth)
	from := end.Add(-window)
	step := sourceCallBucketWidth
	if window > time.Hour {
		step = time.Hour
	}
	out := SourceCallStatistics{From: from, To: now, BucketMinutes: int(step / time.Minute), Groups: []SourceCallGroup{}, Trend: []SourceCallPoint{}, Recent: []SourceCall{}}
	var summary sourceCallCounters
	groups := map[sourceCallKey]sourceCallCounters{}
	points := make([]sourceCallCounters, int(window/step))
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Since, out.InFlight, out.Total = s.since, s.inFlight, s.total.summary()
	for _, bucket := range s.buckets {
		if bucket.at.Before(from) || !bucket.at.Before(end) {
			continue
		}
		index := int(bucket.at.Sub(from) / step)
		for key, count := range bucket.groups {
			summary.add(count)
			points[index].add(count)
			group := groups[key]
			group.add(count)
			groups[key] = group
		}
	}
	out.Summary = summary.summary()
	for key, count := range groups {
		out.Groups = append(out.Groups, SourceCallGroup{Platform: key.platform, Action: key.action, SourceCallSummary: count.summary()})
	}
	slices.SortFunc(out.Groups, func(a, b SourceCallGroup) int {
		if n := strings.Compare(a.Platform, b.Platform); n != 0 {
			return n
		}
		return strings.Compare(a.Action, b.Action)
	})
	for i, count := range points {
		out.Trend = append(out.Trend, SourceCallPoint{At: from.Add(time.Duration(i) * step), SourceCallSummary: count.summary()})
	}
	for i := 0; i < len(s.recent); i++ {
		call := s.recent[(s.next-1-i+len(s.recent))%len(s.recent)]
		if call.At.Before(from) || call.At.After(now) {
			continue
		}
		out.Recent = append(out.Recent, call)
	}
	return out
}

// CallStatisticsOf 返回运行期统计；重载和停用保留记录，删除或重启后清除。
func (m *SourceManager) CallStatisticsOf(id int64, now time.Time, window time.Duration) SourceCallStatistics {
	if window != time.Hour && window != 24*time.Hour {
		window = time.Hour
	}
	m.mu.RLock()
	s := m.statistics[id]
	m.mu.RUnlock()
	if s == nil {
		s = &sourceCalls{since: now}
	}
	return s.snapshot(now, window)
}

// ForgetCallStatistics 只在已成功删除脚本后调用。
func (m *SourceManager) ForgetCallStatistics(id int64) {
	m.mu.Lock()
	delete(m.statistics, id)
	m.mu.Unlock()
}
