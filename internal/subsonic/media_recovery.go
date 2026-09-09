package subsonic

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"lxsc/internal/music"
)

const (
	mediaRecoveryTimeout = 45 * time.Second
	mediaHeaderTimeout   = 8 * time.Second
	maxMediaSources      = 3
	maxMediaAttempts     = 2 * maxMediaSources
)

var (
	errMediaUnavailable = errors.New("没有可用的音频响应")
	errInvalidMediaURL  = errors.New("无效的播放地址")
)

// prepareMedia 只在响应提交前恢复：固定候选快照、每源至多一次过期刷新，失败排除仅限本次请求。
// 代理返回尚未读取的响应体；302 只做最小 Range 头校验，不把调试 probe 纳入恢复流程。
func (s *Server) prepareMedia(ctx context.Context, r *http.Request, in *music.Info, quality string, proxy bool) (music.URLResolution, *http.Response, error) {
	if s.Catalog.Sources == nil {
		return music.URLResolution{}, nil, errMediaUnavailable
	}
	sourceIDs := s.Catalog.Sources.MusicURLSourceIDs(in.Source())
	if len(sourceIDs) > maxMediaSources {
		sourceIDs = sourceIDs[:maxMediaSources]
	}
	refreshed := make(map[int64]bool, maxMediaSources)
	// 同一地址可能由不同脚本返回，明确拒绝不能因换了脚本身份而被遗忘。
	rejectedURLs := make(map[string]struct{}, maxMediaAttempts)
	uncertain := make([]music.URLResolution, 0, maxMediaSources)
	var failed *music.URLResolution
	stage := "resolve"
	for attempts := 0; attempts < maxMediaAttempts && len(sourceIDs) > 0 && ctx.Err() == nil; {
		started := time.Now()
		resolution, err := s.Catalog.ResolvePlaybackURLForSources(ctx, in, quality, sourceIDs, failed)
		s.mediaEvent(stage, in.TrackID(), in.Source(), quality, resolution.Cached, 0, started, err)
		if err != nil {
			if failed == nil {
				break
			}
			// 过期刷新解析失败，也应跳过原脚本，而不是提前阻止其他音源。
			id := failed.Result.SourceID
			sourceIDs = slices.DeleteFunc(sourceIDs, func(candidate int64) bool { return candidate == id })
			failed, stage = nil, "source_fallback"
			continue
		}
		failed = nil
		attempts++
		started = time.Now()
		status := 0
		var resp *http.Response
		if proxy {
			headCtx, cancel := context.WithTimeout(ctx, mediaHeaderTimeout)
			resp, err = s.openMedia(headCtx, r, in, resolution.Result.URL)
			cancel()
			if resp != nil {
				status = resp.StatusCode
			}
			s.mediaEvent("proxy_response", in.TrackID(), in.Source(), resolution.Result.Quality, resolution.Cached, status, started, err)
		} else {
			status, err = s.Catalog.CheckPlaybackURL(ctx, resolution, func(checkCtx context.Context) (int, error) {
				return s.checkMedia(checkCtx, in, resolution.Result.URL)
			})
			checkStage := "url_check"
			if resolution.Cached {
				checkStage = "cache_check"
			}
			s.mediaEvent(checkStage, in.TrackID(), in.Source(), resolution.Result.Quality, resolution.Cached, status, started, err)
		}
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return music.URLResolution{}, nil, context.Canceled
		}
		if err == nil && (status == http.StatusOK || status == http.StatusPartialContent || (proxy && status == http.StatusRequestedRangeNotSatisfiable)) {
			return resolution, resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		id := resolution.Result.SourceID
		if err != nil {
			if errors.Is(err, errInvalidMediaURL) {
				s.Catalog.InvalidatePlaybackURL(resolution)
				rejectedURLs[resolution.Result.URL] = struct{}{}
			} else if !proxy {
				// 网络失败不证明客户端必定失败；候选只存于本次请求，最后排除已明确拒绝的地址。
				uncertain = append(uncertain, resolution)
			}
		} else {
			rejectedURLs[resolution.Result.URL] = struct{}{}
			s.Catalog.InvalidatePlaybackURL(resolution)
			if expiredMediaStatus(status) && !refreshed[id] {
				refreshed[id] = true
				failed, stage = &resolution, "refresh"
				continue
			}
			if status == http.StatusRequestedRangeNotSatisfiable {
				// 416 不触发换源，不能将客户端的范围错误变成重新播放。
				return music.URLResolution{}, nil, errMediaUnavailable
			}
		}
		sourceIDs = slices.DeleteFunc(sourceIDs, func(candidate int64) bool { return candidate == id })
		stage = "source_fallback"
	}
	if r.Context().Err() != nil {
		return music.URLResolution{}, nil, r.Context().Err()
	}
	for _, candidate := range uncertain {
		if _, rejected := rejectedURLs[candidate.Result.URL]; !rejected {
			return candidate, nil, nil
		}
	}
	if ctx.Err() != nil {
		return music.URLResolution{}, nil, ctx.Err()
	}
	return music.URLResolution{}, nil, errMediaUnavailable
}
