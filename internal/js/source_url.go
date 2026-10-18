package js

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"lxsc/internal/admission"
)

const (
	musicURLFallbackDelay   = 1500 * time.Millisecond
	maxConcurrentURLSources = 2
	MusicURLSourceTimeout   = 4 * time.Second
	musicURLQualityWait     = 200 * time.Millisecond
)

type musicURLPolicyKey struct{}
type musicURLPolicy struct {
	strategy string
	exact    bool
}

// WithMusicURLPolicy 在共享工作创建前读取策略，不把单个等待者的截止带入共享任务。
func WithMusicURLPolicy(ctx context.Context, strategy string, exact bool) context.Context {
	return context.WithValue(ctx, musicURLPolicyKey{}, musicURLPolicy{strategy, exact})
}

func urlPolicy(ctx context.Context) musicURLPolicy {
	p, _ := ctx.Value(musicURLPolicyKey{}).(musicURLPolicy)
	return p
}

// 常见的快速取链仍只访问一个音源；慢请求最多增加一个并行备用。
// 调用结束前取消并收回所有工作，避免切歌后遗留 HTTP、计数或脚本任务。
func (m *SourceManager) musicURLCandidates(ctx context.Context, candidates []*loadedSource, platform string, musicInfo any, quality string) (*MusicURLResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoSource
	}
	quality = CanonicalQuality(quality)
	// 单源不存在跨源同档竞争，保留一次逻辑调用中的尝试和降级统计。
	if policy := urlPolicy(ctx); len(candidates) > 1 && !policy.exact && policy.strategy != "speed" {
		excluded := map[int64]bool{}
		var lastErr error = ErrNoQuality
		for _, q := range QualityChain(quality) {
			var matching []*loadedSource
			for _, candidate := range candidates {
				if _, ok := scriptQuality(q, candidate.platforms[platform].Qualitys); ok && !excluded[candidate.id] {
					matching = append(matching, candidate)
				}
			}
			if len(matching) == 0 {
				continue
			}
			result, err := m.raceMusicURLCandidates(WithMusicURLPolicy(ctx, "quality", true), matching, platform, musicInfo, q, excluded)
			if err == nil {
				return result, nil
			}
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, admission.ErrBusy) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
		}
		return nil, lastErr
	}
	return m.raceMusicURLCandidates(ctx, candidates, platform, musicInfo, quality, nil)
}

func (m *SourceManager) raceMusicURLCandidates(ctx context.Context, candidates []*loadedSource, platform string, musicInfo any, quality string, excluded map[int64]bool) (*MusicURLResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoSource
	}
	// 单源和定向刷新不需要并行调度；父请求同时作为预算与归属判断。
	if len(candidates) == 1 {
		result, err := m.callMusicURLSource(ctx, ctx, candidates[0], platform, musicInfo, quality, 1)
		if excluded != nil && sourceWideFailure(err) {
			excluded[candidates[0].id] = true
		}
		return result, err
	}
	// callCtx 只由本函数在收尾或赢家出现时取消；外层 ctx 保持原样，
	// 用于区分"用户取消"与"其他音源先成功导致的取消"。
	callCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	type outcome struct {
		id     int64
		result *MusicURLResult
		err    error
	}
	results := make(chan outcome, maxConcurrentURLSources)
	next, active := 0, 0
	start := func() {
		index := next
		next++
		active++
		workers.Go(func() {
			result, err := m.callMusicURLSource(callCtx, ctx, candidates[index], platform, musicInfo, quality, len(candidates)-index)
			results <- outcome{candidates[index].id, result, err}
		})
	}
	timer := time.NewTimer(musicURLFallbackDelay)
	defer timer.Stop()
	var fallback <-chan time.Time
	busy := false
	armFallback := func() {
		timer.Stop()
		fallback = nil
		if !busy && next < len(candidates) && active < maxConcurrentURLSources {
			timer.Reset(musicURLFallbackDelay)
			fallback = timer.C
		}
	}
	start()
	armFallback()
	var lastErr error = ErrNoSource
	var best *MusicURLResult
	qualityTimer := time.NewTimer(time.Hour)
	defer qualityTimer.Stop()
	qualityTimer.Stop()
	var qualityDeadline <-chan time.Time
	for active > 0 {
		select {
		case <-ctx.Done():
			if best != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return best, nil
			}
			return nil, ctx.Err()
		case <-qualityDeadline:
			return best, nil
		case completed := <-results:
			active--
			if excluded != nil && sourceWideFailure(completed.err) {
				excluded[completed.id] = true
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if completed.err == nil {
				if QualityRank(completed.result.Quality) >= QualityRank(quality) {
					return completed.result, nil
				}
				if best == nil || QualityRank(completed.result.Quality) > QualityRank(best.Quality) {
					best = completed.result
				}
				if qualityDeadline == nil {
					qualityTimer.Reset(musicURLQualityWait)
					qualityDeadline = qualityTimer.C
				}
				if next < len(candidates) && active < maxConcurrentURLSources && !busy {
					start()
				}
				armFallback()
				continue
			}
			lastErr = completed.err
			busy = busy || errors.Is(completed.err, admission.ErrBusy)
			// 明确失败立即尝试下一个；繁忙时不扩大请求，仍允许在途音源完成。
			if !busy && next < len(candidates) {
				start()
			}
			armFallback()
		case <-fallback:
			start()
			armFallback()
		}
	}
	if best != nil {
		return best, nil
	}
	if busy {
		return nil, admission.ErrBusy
	}
	return nil, lastErr
}

func (m *SourceManager) callMusicURLSource(ctx, parent context.Context, source *loadedSource, platform string, musicInfo any, quality string, remaining int) (*MusicURLResult, error) {
	// 同一音源的全部降级尝试共用预算，并为后续媒体校验留出时间。
	budget := min(m.CallTime, MusicURLSourceTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/time.Duration(remaining+1))
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	started := time.Now()
	source.calls.begin()
	result, attempts, err := m.musicURLFromSource(ctx, source, platform, musicInfo, quality)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	call := SourceCall{Platform: platform, Action: "musicUrl", RequestedQuality: quality, Attempts: attempts}
	if result != nil {
		call.Quality, call.Downgraded = result.Quality, result.Quality != quality
	}
	source.calls.finish(started, call, musicInfo, err)
	source.health.record(started, err, result != nil && result.Quality != quality)
	m.schedule.record(source.id, platform, "musicUrl", classifyOutcome(parent, ctx, err), result != nil && result.Quality != quality, time.Since(started).Milliseconds())
	var sourceErr *SourceRequestError
	if errors.As(err, &sourceErr) && errors.Is(err, ErrSourceRateLimited) {
		m.BackoffMusicURL(source.id, platform, sourceErr.RetryAfter())
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source.meta.Name, err)
	}
	return result, nil
}
