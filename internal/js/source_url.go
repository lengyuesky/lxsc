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
)

// 常见的快速取链仍只访问一个音源；慢请求最多增加一个并行备用。
// 调用结束前取消并收回所有工作，避免切歌后遗留 HTTP、计数或脚本任务。
func (m *SourceManager) musicURLCandidates(ctx context.Context, candidates []*loadedSource, platform string, musicInfo any, quality string) (*MusicURLResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoSource
	}
	// 单源和定向刷新不需要并行调度。
	if len(candidates) == 1 {
		return m.callMusicURLSource(ctx, candidates[0], platform, musicInfo, quality, 1)
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	type outcome struct {
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
			result, err := m.callMusicURLSource(ctx, candidates[index], platform, musicInfo, quality, len(candidates)-index)
			results <- outcome{result, err}
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
	for active > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case completed := <-results:
			active--
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if completed.err == nil {
				return completed.result, nil
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
	if busy {
		return nil, admission.ErrBusy
	}
	return nil, lastErr
}

func (m *SourceManager) callMusicURLSource(ctx context.Context, source *loadedSource, platform string, musicInfo any, quality string, remaining int) (*MusicURLResult, error) {
	// 同一音源的全部降级尝试共用预算，并为后续媒体校验留出时间。
	budget := m.CallTime
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
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source.meta.Name, err)
	}
	return result, nil
}
