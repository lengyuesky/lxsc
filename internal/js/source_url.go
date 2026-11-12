package js

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lxsc/internal/admission"
)

// MusicURLSourceTimeout 是向一个音源取链（含音源内降档）的上限。
const MusicURLSourceTimeout = 4 * time.Second

type exactMusicURLKey struct{}

// WithExactMusicURL 要求只请求指定音质，不在音源内向下降档。
// 播放选源用它逐个音源、逐档取链，并自行验证音频后决定是否换档或换源。
func WithExactMusicURL(ctx context.Context) context.Context {
	return context.WithValue(ctx, exactMusicURLKey{}, true)
}

func exactMusicURL(ctx context.Context) bool {
	exact, _ := ctx.Value(exactMusicURLKey{}).(bool)
	return exact
}

// musicURLCandidates 按调度顺序逐个请求音源，返回第一个取得的地址。
// 这里不并发也不验证音频：播放时的换源、降档和竞速只在播放选源一处实现，
// 多音源路径只服务调试与管理端"能否取到地址"的检查。
func (m *SourceManager) musicURLCandidates(ctx context.Context, candidates []*loadedSource, platform string, musicInfo any, quality string) (*MusicURLResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	quality = CanonicalQuality(quality)
	var lastErr error = ErrNoSource
	for _, source := range candidates {
		result, err := m.callMusicURLSource(ctx, source, platform, musicInfo, quality)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if ctx.Err() != nil || errors.Is(err, admission.ErrBusy) {
			return nil, err
		}
	}
	return nil, lastErr
}

// callMusicURLSource 调用一个音源并记录统计、健康度与调度评分。
func (m *SourceManager) callMusicURLSource(parent context.Context, source *loadedSource, platform string, musicInfo any, quality string) (*MusicURLResult, error) {
	ctx, cancel := context.WithTimeout(parent, min(m.CallTime, MusicURLSourceTimeout))
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
	m.schedule.record(source.id, platform, "musicUrl", classifyOutcome(parent, err), result != nil && result.Quality != quality, time.Since(started).Milliseconds(), source.fingerprint)
	var sourceErr *SourceRequestError
	if errors.As(err, &sourceErr) && errors.Is(err, ErrSourceRateLimited) {
		m.BackoffMusicURL(source.id, platform, sourceErr.RetryAfter(), source.fingerprint)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source.meta.Name, err)
	}
	return result, nil
}
