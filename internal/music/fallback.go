package music

import (
	"context"
	"errors"
	"time"

	"lxsc/internal/chksz"
	"lxsc/internal/js"
)

// FallbackSourceID 标识 ChKSz API 兜底给出的地址。它不是音源脚本：
// 不参与音源调度、评分和统计，播放日志按 chksz.SourceName 显示。
const FallbackSourceID int64 = -1

const (
	// fallbackURLTTL 是兜底直链的缓存时间：免费额度按次计，HEAD、GET 与短时间内的重播共用一次调用。
	fallbackURLTTL = 10 * time.Minute
	// fallbackCallLimit 是一次兜底接口调用的上限。
	fallbackCallLimit = 6 * time.Second
	// fallbackTokenSpace 让兜底直链的解析版本与音源直链互不相撞，二者共用媒体校验缓存。
	fallbackTokenSpace = 1
)

func newFallbackURLCache() *requestCache[urlKey, js.MusicURLResult] {
	cache := newRequestCache[urlKey, js.MusicURLResult](500, fallbackURLTTL)
	cache.space = fallbackTokenSpace
	cache.expiry = func(key urlKey, value js.MusicURLResult, created, expires time.Time) time.Time {
		parsed, _ := ParseID(key.trackID)
		return playbackURLExpiryFor(key, value, parsed.Source, created, expires)
	}
	return cache
}

// fallbackCacheTTL 跟随管理员的直链缓存开关；开启时固定为 fallbackURLTTL。
func fallbackCacheTTL(urlTTL int) time.Duration {
	if urlTTL == 0 {
		return 0
	}
	return fallbackURLTTL
}

// FallbackEnabled 报告是否已配置 ChKSz 密钥，且此刻没有因密钥失效或限流暂停调用。
func (c *Catalog) FallbackEnabled() bool {
	key := c.Settings.Get().FallbackAPIKey
	return key != "" && c.Fallback != nil && !c.Fallback.Blocked(key)
}

// FallbackSupports 报告曲目能否交给 ChKSz 取链：只支持网易云、QQ 音乐和酷狗。
func FallbackSupports(in *Info) bool {
	return in != nil && chksz.Supports(in.Source()) && in.Key() != ""
}

// ResolveFallbackURL 通过 ChKSz API 取一首歌在指定音质下的地址，不主动请求音频。
// 同一曲目和音质的并发请求合并为一次调用，成功结果短暂缓存；失效时由调用方 InvalidatePlaybackURL。
func (c *Catalog) ResolveFallbackURL(ctx context.Context, in *Info, quality string) (URLResolution, error) {
	key := urlKey{trackID: in.TrackID(), quality: SelectQuality(quality, in.Qualities())}
	apiKey := c.Settings.Get().FallbackAPIKey
	r, err := c.fallbackURLs.load(ctx, key, nil, fallbackCallLimit, func(remoteCtx context.Context) (js.MusicURLResult, bool, error) {
		if c.Fallback == nil {
			return js.MusicURLResult{}, false, chksz.ErrNotConfigured
		}
		release, err := c.customLimits.Acquire(remoteCtx, 0)
		if err != nil {
			return js.MusicURLResult{}, false, err
		}
		defer release()
		result, err := c.Fallback.Resolve(remoteCtx, apiKey, in.Source(), in.Key(), key.quality)
		if err != nil {
			if c.Log != nil {
				switch {
				case errors.Is(err, chksz.ErrAuth):
					c.Log.Warn("ChKSz API 密钥无效或已失效，暂停兜底取链，请在设置中更换密钥")
				case errors.Is(err, chksz.ErrRateLimited):
					c.Log.Warn("ChKSz API 额度用尽或请求受限，暂停兜底取链")
				case errors.Is(err, chksz.ErrKeyInURL):
					c.Log.Warn("ChKSz API 返回的播放地址包含密钥，为避免交给客户端已拒绝使用", "platform", in.Source())
				}
			}
			return js.MusicURLResult{}, false, err
		}
		return js.MusicURLResult{URL: result.URL, Quality: result.Quality, Source: chksz.SourceName, SourceID: FallbackSourceID}, true, nil
	})
	return URLResolution{Result: r.value, Cached: r.cached, Platform: in.Source(), TrackID: in.TrackID(), key: key, token: r.token}, err
}

// RememberPlaybackFallback 记录"这首曲目的全部音源和平台已确认失败，由 ChKSz 兜底播放成功"。
// track 是实际取链的曲目，可能是其它平台上的同一首歌。
func (c *Catalog) RememberPlaybackFallback(in *Info, quality string, direct bool, track *Info) {
	c.fallbacks.Add(exhaustedKey{in.TrackID(), direct}, playbackSubstitute{QualityRank(SelectQuality(quality, in.Qualities())), track})
}

// PlaybackFallback 返回近期由兜底接口播放成功的曲目；没有可复用的结论时返回 nil。
// 与换平台结论相同，有效期内重播直接走兜底，不再把已确认失败的音源重新等一遍。
func (c *Catalog) PlaybackFallback(in *Info, quality string, direct bool) *Info {
	found, ok := c.fallbacks.Get(exhaustedKey{in.TrackID(), direct})
	if !ok || QualityRank(SelectQuality(quality, in.Qualities())) > found.rank {
		return nil
	}
	return found.track
}

// ForgetPlaybackFallback 在兜底地址不再能播放时作废结论，恢复完整选源。
func (c *Catalog) ForgetPlaybackFallback(in *Info, direct bool) {
	c.fallbacks.Remove(exhaustedKey{in.TrackID(), direct})
}
