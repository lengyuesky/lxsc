package subsonic

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
	"lxsc/internal/js"
	"lxsc/internal/music"
)

const (
	mediaRecoveryTimeout      = 12 * time.Second
	mediaHeaderTimeout        = 3 * time.Second
	mediaCandidateTimeout     = 4 * time.Second
	mediaFallbackDelay        = 1500 * time.Millisecond
	mediaQualityWait          = 200 * time.Millisecond
	maxConcurrentMediaSources = 2
	maxMediaAttempts          = 12
)

var (
	errMediaUnavailable = errors.New("没有可用的音频响应")
	errInvalidMediaURL  = errors.New("无效的播放地址")
)

type mediaTask struct {
	id        int64
	quality   string
	exact     bool
	failed    *music.URLResolution
	cached    *music.URLResolution
	refreshed *sync.Map
	fallback  bool
}

type mediaOutcome struct {
	task       mediaTask
	resolution music.URLResolution
	response   *http.Response
	err        error
	status     int
	refresh    *music.URLResolution
	rejected   []string
	headerMS   int64
}

// prepareMedia 竞速“取链 + 媒体响应头”，完整候选快照受总预算、并行数和调用数限制。
// 同档优先策略在所有支持该档的源失败后才降档；正文只交付最终胜出的一个响应。
func (s *Server) prepareMedia(ctx context.Context, r *http.Request, in *music.Info, quality string, proxy, headMetadata bool) (result music.URLResolution, response *http.Response, resultErr error) {
	finish := s.Catalog.MediaPreparation.Start()
	defer func() { finish(resultErr) }()
	if s.Catalog.Sources == nil {
		return result, nil, errMediaUnavailable
	}
	ctx, stopBudget := context.WithTimeout(ctx, mediaRecoveryTimeout)
	defer stopBudget()
	strategy := s.Settings.Get().SourceSelection
	ids := s.Catalog.Sources.MusicURLSourceIDs(in.Source())
	cached, cachedFirst := s.Catalog.CachedPlaybackURL(in, quality)
	cachedFirst = cachedFirst && slices.Contains(ids, cached.Result.SourceID)
	if cachedFirst {
		ids = append([]int64{cached.Result.SourceID}, slices.DeleteFunc(slices.Clone(ids), func(id int64) bool { return id == cached.Result.SourceID })...)
	}
	qualities := js.QualityChain(music.SelectQuality(quality, in.Qualities()))
	if strategy == "speed" {
		qualities = qualities[:1]
	}
	if cachedFirst {
		qualities = append([]string{cached.Result.Quality}, qualities...)
	}
	blocked := map[int64]bool{}
	type candidateKey struct {
		id      int64
		quality string
	}
	attempted := map[candidateKey]bool{}
	rejected := map[string]bool{}
	var uncertain []music.URLResolution
	lastFailure := errMediaUnavailable
	var calls atomic.Int32
	var refreshed sync.Map
	leases := map[int64]func(){}
	defer func() {
		for _, release := range leases {
			release()
		}
	}()
	for round, q := range qualities {
		cachedRound := cachedFirst && round == 0
		var tasks []mediaTask
		for _, id := range ids {
			taskQuality := q
			useCached := cachedRound && id == cached.Result.SourceID
			if cachedRound && !useCached {
				taskQuality = js.QualityChain(music.SelectQuality(quality, in.Qualities()))[0]
			}
			key := candidateKey{id, taskQuality}
			if attempted[key] || blocked[id] || (strategy != "speed" && !s.Catalog.Sources.SupportsMusicURLQuality(id, in.Source(), taskQuality)) {
				continue
			}
			if _, owned := leases[id]; !owned {
				release, ok := s.Catalog.Sources.AcquirePlaybackProbe(id, in.Source())
				if !ok {
					blocked[id] = true
					continue
				}
				leases[id] = release
			}
			attempted[key] = true
			task := mediaTask{id: id, quality: taskQuality, exact: useCached || strategy != "speed", refreshed: &refreshed}
			if useCached {
				task.cached = &cached
			}
			tasks = append(tasks, task)
		}
		if len(tasks) == 0 {
			continue
		}
		winner, failures, err := s.raceMediaCandidates(ctx, r, in, tasks, proxy, headMetadata, &calls)
		for _, failed := range failures {
			if failed.err != nil && !errors.Is(failed.err, context.Canceled) {
				lastFailure = failed.err
			}
			for _, address := range failed.rejected {
				rejected[address] = true
			}
			if failed.status == http.StatusRequestedRangeNotSatisfiable {
				return result, nil, errMediaUnavailable
			}
			if failed.resolution.Result.URL != "" {
				if failed.status != 0 || errors.Is(failed.err, errInvalidMediaURL) || errors.Is(failed.err, httpguard.ErrNonAudioResponse) {
					rejected[failed.resolution.Result.URL] = true
				} else if !proxy && !headMetadata && !errors.Is(failed.err, context.Canceled) {
					uncertain = append(uncertain, failed.resolution)
				}
			}
			// 网络和源级故障不靠重复降档放大；明确过期和未知脚本失败允许同源下一档。
			if failed.status >= 500 || failed.status == 429 || errors.Is(failed.err, context.DeadlineExceeded) || errors.Is(failed.err, httpguard.ErrNonAudioResponse) || errors.Is(failed.err, errInvalidMediaURL) || errors.Is(failed.err, js.ErrInvalidSourceResponse) || errors.Is(failed.err, js.ErrSourceAuth) || errors.Is(failed.err, js.ErrSourceUnavailable) || errors.Is(failed.err, js.ErrSourceRateLimited) || (failed.status == 0 && failed.resolution.Result.URL != "") {
				blocked[failed.task.id] = true
			}
		}
		if winner != nil {
			result = s.Catalog.PublishPlaybackURL(in, quality, winner.resolution)
			result.Validated = true
			if winner.status != http.StatusRequestedRangeNotSatisfiable {
				s.Catalog.ReportPlaybackOutcome(result, in.Source(), true, winner.headerMS)
			}
			return result, winner.response, nil
		}
		if errors.Is(err, admission.ErrBusy) || (err != nil && !errors.Is(err, errMediaUnavailable) && !errors.Is(err, context.DeadlineExceeded)) {
			return result, nil, err
		}
		if ctx.Err() != nil || calls.Load() >= maxMediaAttempts {
			break
		}
	}
	if r.Context().Err() != nil {
		return result, nil, r.Context().Err()
	}
	for _, candidate := range uncertain {
		if !rejected[candidate.Result.URL] {
			return candidate, nil, nil
		}
	}
	if ctx.Err() != nil {
		return result, nil, ctx.Err()
	}
	return result, nil, lastFailure
}

func (s *Server) raceMediaCandidates(ctx context.Context, r *http.Request, in *music.Info, tasks []mediaTask, proxy, headMetadata bool, calls *atomic.Int32) (winner *mediaOutcome, failures []mediaOutcome, resultErr error) {
	workCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	results := make(chan mediaOutcome, maxConcurrentMediaSources)
	var best *mediaOutcome
	defer func() {
		cancel()
		workers.Wait()
		for len(results) > 0 {
			outcome := <-results
			if outcome.response != nil {
				outcome.response.Body.Close()
			}
			if outcome.err != nil {
				failures = append(failures, outcome)
			}
		}
	}()
	next, active := 0, 0
	start := func() {
		task := tasks[next]
		task.fallback = next > 0
		next++
		active++
		workers.Go(func() { results <- s.tryMediaCandidate(workCtx, r, in, task, proxy, headMetadata, len(tasks), calls) })
	}
	hedge := time.NewTimer(mediaFallbackDelay)
	defer hedge.Stop()
	qualityTimer := time.NewTimer(time.Hour)
	qualityTimer.Stop()
	defer qualityTimer.Stop()
	var qualityDeadline <-chan time.Time
	var deferred []mediaTask
	busy := false
	start()
	for active > 0 {
		select {
		case <-ctx.Done():
			if best != nil && !errors.Is(ctx.Err(), context.Canceled) {
				return best, failures, nil
			}
			if best != nil && best.response != nil {
				best.response.Body.Close()
			}
			return nil, failures, ctx.Err()
		case <-qualityDeadline:
			return best, failures, nil
		case <-hedge.C:
			if !busy && next < len(tasks) && active < maxConcurrentMediaSources {
				start()
			}
		case outcome := <-results:
			active--
			if outcome.err == nil {
				if !outcome.task.exact && music.QualityRank(outcome.resolution.Result.Quality) < music.QualityRank(outcome.task.quality) {
					if best == nil || music.QualityRank(outcome.resolution.Result.Quality) > music.QualityRank(best.resolution.Result.Quality) {
						if best != nil && best.response != nil {
							best.response.Body.Close()
						}
						best = &outcome
					} else if outcome.response != nil {
						outcome.response.Body.Close()
					}
					if qualityDeadline == nil {
						qualityTimer.Reset(mediaQualityWait)
						qualityDeadline = qualityTimer.C
					}
				} else {
					if best != nil && best.response != nil {
						best.response.Body.Close()
					}
					return &outcome, failures, nil
				}
			} else {
				failures = append(failures, outcome)
				busy = busy || errors.Is(outcome.err, admission.ErrBusy)
				if outcome.status == http.StatusRequestedRangeNotSatisfiable {
					return nil, failures, errMediaUnavailable
				}
				if outcome.refresh != nil {
					deferred = append(deferred, mediaTask{id: outcome.task.id, quality: outcome.task.quality, exact: outcome.task.exact, failed: outcome.refresh, refreshed: outcome.task.refreshed})
				}
			}
			if !busy && next == len(tasks) && active == 0 && best == nil && len(deferred) > 0 {
				tasks = append(tasks, deferred...)
				deferred = nil
			}
			if !busy && next < len(tasks) && calls.Load() < maxMediaAttempts {
				start()
			}
			hedge.Stop()
			if !busy && next < len(tasks) && active < maxConcurrentMediaSources {
				hedge.Reset(mediaFallbackDelay)
			}
		}
	}
	if best != nil {
		return best, failures, nil
	}
	if busy {
		return nil, failures, admission.ErrBusy
	}
	return nil, failures, errMediaUnavailable
}

func (s *Server) tryMediaCandidate(ctx context.Context, r *http.Request, in *music.Info, task mediaTask, proxy, headMetadata bool, sourceCount int, calls *atomic.Int32) mediaOutcome {
	if task.exact {
		return s.tryExactMediaCandidate(ctx, r, in, task, proxy, headMetadata, sourceCount, calls)
	}
	// 速度优先允许源内降档，但降档也必须经过媒体校验；全部尝试共用单源预算。
	ctx, cancel := context.WithTimeout(ctx, mediaCandidateTimeout)
	defer cancel()
	out := mediaOutcome{task: task, err: js.ErrNoQuality}
	var rejected []string
	for index, quality := range js.QualityChain(task.quality) {
		if !s.Catalog.Sources.SupportsMusicURLQuality(task.id, in.Source(), quality) {
			continue
		}
		next := task
		next.quality, next.exact = quality, true
		if index > 0 {
			next.failed = nil
		}
		out = s.tryExactMediaCandidate(ctx, r, in, next, proxy, headMetadata, 1, calls)
		out.task = task
		rejected = append(rejected, out.rejected...)
		out.rejected = rejected
		if out.err == nil || ctx.Err() != nil || out.status == 416 || out.status == 429 || out.status >= 500 || errors.Is(out.err, admission.ErrBusy) || errors.Is(out.err, context.Canceled) || errors.Is(out.err, context.DeadlineExceeded) || errors.Is(out.err, js.ErrSourceAuth) || errors.Is(out.err, js.ErrSourceUnavailable) || errors.Is(out.err, js.ErrSourceRateLimited) || errors.Is(out.err, js.ErrInvalidSourceResponse) || errors.Is(out.err, errInvalidMediaURL) || errors.Is(out.err, httpguard.ErrNonAudioResponse) || (out.status == 0 && out.resolution.Result.URL != "") {
			return out
		}
	}
	return out
}

func (s *Server) tryExactMediaCandidate(ctx context.Context, r *http.Request, in *music.Info, task mediaTask, proxy, headMetadata bool, sourceCount int, calls *atomic.Int32) mediaOutcome {
	ctx, cancel := context.WithTimeout(ctx, mediaCandidateTimeout)
	defer cancel()
	out := mediaOutcome{task: task}
	failed := task.failed
	for round := 0; round < 2; round++ {
		attempt := calls.Add(1)
		if attempt > maxMediaAttempts {
			out.err = errMediaUnavailable
			return out
		}
		stage := "resolve"
		if task.fallback {
			stage = "source_fallback"
		}
		if failed != nil {
			stage = "refresh"
		}
		started := time.Now()
		var resolution music.URLResolution
		var err error
		if round == 0 && task.cached != nil && failed == nil {
			resolution = *task.cached
		} else if task.exact {
			resolution, err = s.Catalog.ResolvePlaybackCandidate(ctx, in, task.quality, []int64{task.id}, failed)
		} else {
			resolution, err = s.Catalog.ResolvePlaybackURLForSources(ctx, in, task.quality, []int64{task.id}, failed)
		}
		s.mediaAttemptEvent(ctx, r, stage, in, task.quality, resolution, 0, started, err, int(attempt))
		if err != nil {
			out.err = err
			return out
		}
		out.resolution = resolution
		started = time.Now()
		var resp *http.Response
		if proxy && !headMetadata {
			headCtx, stop := context.WithTimeout(ctx, mediaHeaderTimeout)
			resp, err = s.openMedia(headCtx, r, in, resolution.Result.URL)
			stop()
			if resp != nil && err == nil {
				err = httpguard.CheckMediaResponse(resp)
			}
			stage = "proxy_response"
		} else {
			resp, err = s.Catalog.CheckPlaybackURL(ctx, resolution, func(checkCtx context.Context) (*http.Response, error) {
				return s.inspectMedia(checkCtx, in, resolution.Result.URL)
			})
			stage = "url_check"
			if resolution.Cached {
				stage = "cache_check"
			}
		}
		if resp != nil {
			out.status = resp.StatusCode
		}
		if err == nil && !(out.status == 200 || out.status == 206 || (proxy && !headMetadata && out.status == 416)) {
			err = mediaHTTPError(out.status)
		}
		out.headerMS = time.Since(started).Milliseconds()
		s.mediaAttemptEvent(ctx, r, stage, in, resolution.Result.Quality, resolution, out.status, started, err, int(attempt))
		if err == nil {
			out.err = nil
			out.response = resp
			return out
		}
		if resp != nil {
			resp.Body.Close()
		}
		out.err = err
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return out
		}
		if out.status != 0 || errors.Is(err, errInvalidMediaURL) || errors.Is(err, httpguard.ErrNonAudioResponse) {
			out.rejected = append(out.rejected, resolution.Result.URL)
			s.Catalog.InvalidatePlaybackURL(resolution)
			if out.status != 416 {
				s.Catalog.ReportPlaybackOutcome(resolution, in.Source(), false, out.headerMS)
			}
			if out.status == 429 {
				s.Catalog.Sources.BackoffMusicURL(task.id, in.Source(), mediaRetryAfter(resp))
			}
		}
		if expiredMediaStatus(out.status) && round == 0 && failed == nil {
			if task.refreshed != nil {
				if _, used := task.refreshed.LoadOrStore(task.id, true); used {
					return out
				}
			}
			if resolution.Cached || sourceCount == 1 {
				failed = &resolution
				continue
			}
			out.refresh = &resolution
		}
		return out
	}
	return out
}

func mediaRetryAfter(resp *http.Response) time.Duration {
	if resp != nil {
		value := resp.Header.Get("Retry-After")
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
			return time.Duration(min(seconds, 300)) * time.Second
		}
		if date, err := http.ParseTime(value); err == nil && time.Until(date) > 0 {
			return min(time.Until(date), 5*time.Minute)
		}
	}
	return 5 * time.Second
}

type mediaHTTPError int

func (e mediaHTTPError) Error() string { return "媒体响应被拒绝" }
func (e mediaHTTPError) DiagnosticCode() string {
	if expiredMediaStatus(int(e)) {
		return "expired"
	}
	if int(e) == 429 {
		return "rate_limited"
	}
	return "upstream_status"
}

func (s *Server) mediaAttemptEvent(ctx context.Context, r *http.Request, stage string, in *music.Info, quality string, resolution music.URLResolution, status int, started time.Time, err error, attempt int) {
	e := diagnostics.Event{Stage: stage, TrackID: in.TrackID(), Platform: in.Source(), Quality: quality, RequestedQuality: param(r, "quality"), Cached: resolution.Cached, Status: status, Error: diagnostics.ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds(), Attempt: attempt}
	if parent, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event); parent != nil {
		e.RequestID = parent.RequestID
	}
	if deadline, ok := ctx.Deadline(); ok {
		e.BudgetMS = max(0, time.Until(deadline).Milliseconds())
	}
	if stage == "url_check" || stage == "cache_check" || stage == "proxy_response" {
		e.Validation = "failed"
		if err == nil {
			e.Validation = "validated"
		} else if status == 0 && !errors.Is(err, errInvalidMediaURL) {
			e.Validation = "unverified"
		}
	}
	s.Diagnostics.Add(e)
}
