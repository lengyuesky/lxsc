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
	"lxsc/internal/settings"
)

const (
	// mediaRecoveryTimeout 是播放请求取得可用响应头的总预算。
	// 实测"最高档失效、降到 flac 才成功"的极端链路需要 ~7s，总预算必须留出这条路径。
	mediaRecoveryTimeout = 9 * time.Second
	// mediaHeadTimeout 是客户端 HEAD 预检的独立预算：预检失败要尽快回 502，不能与 GET 一样跑满总预算。
	mediaHeadTimeout = 4 * time.Second
	// 分档预算：首档等得久一些（多数请求首档即中），后续档快速迭代，避免高档位失败时耗尽总预算。
	mediaFirstQualityTimeout = 2500 * time.Millisecond
	mediaNextQualityTimeout  = 1500 * time.Millisecond
	// mediaCandidateTimeout 是单个候选（一次取链 + 一次媒体校验）的上限。
	mediaCandidateTimeout = 2 * time.Second
	// mediaSpeedLaneTimeout 是 speed 策略下单个音源源内降档的总预算，需容纳多档尝试。
	mediaSpeedLaneTimeout = 4 * time.Second
	mediaHeaderTimeout    = 3 * time.Second
	mediaQualityWait      = 200 * time.Millisecond
	// maxMediaAttempts 限制一次播放准备的取链次数；并发后每档会同时消耗多次。
	maxMediaAttempts = 24
	// crossSourceTimeout 限制跨平台兜底的总时长：它只是最后一档，不能反客为主拖长播放等待。
	crossSourceTimeout = 4 * time.Second
)

var (
	errMediaUnavailable = errors.New("没有可用的音频响应")
	errInvalidMediaURL  = errors.New("无效的播放地址")
)

type mediaTask struct {
	id      int64
	quality string
	exact   bool
	// lane 是任务要尝试的档位：精确任务只有 quality 本身；非精确任务为源内降档链，
	// 由调度方按音源能力与本请求已尝试的档位预先算好。
	lane      []string
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

// prepareMedia 并发竞速“取链 + 媒体响应头”，受总预算、并发音源数和调用数限制。
// quality 策略按档位轮次并发，所有支持该档的源失败后才降档；
// speed 策略一轮内所有源并发各自降档。正文只交付最终胜出的一个响应。
func (s *Server) prepareMedia(ctx context.Context, r *http.Request, in *music.Info, quality string, proxy, headMetadata bool) (result music.URLResolution, response *http.Response, resultErr error) {
	finish := s.Catalog.MediaPreparation.Start()
	defer func() { finish(resultErr) }()
	if s.Catalog.Sources == nil {
		return result, nil, errMediaUnavailable
	}
	budget := mediaRecoveryTimeout
	if headMetadata {
		budget = mediaHeadTimeout
	}
	ctx, stopBudget := context.WithTimeout(ctx, budget)
	defer stopBudget()
	strategy, concurrency := s.Settings.Get().SourceSelection, s.sourceConcurrency()
	ids := s.Catalog.Sources.MusicURLSourceIDs(in.Source())
	cached, cachedFirst := s.Catalog.CachedPlaybackURL(in, quality)
	cachedFirst = cachedFirst && slices.Contains(ids, cached.Result.SourceID)
	if cachedFirst {
		ids = append([]int64{cached.Result.SourceID}, slices.DeleteFunc(slices.Clone(ids), func(id int64) bool { return id == cached.Result.SourceID })...)
	}
	// quality 按完整降档链逐档并发；speed 只跑最高档一轮，降档在各源内部完成。
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
	raced := false // 是否已跑过正式竞速档位：缓存轮之后的首个档位仍享有较长预算
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
			// 缓存轮只校验缓存候选：命中时不向其它源并发取链，避免放大上游压力。
			if blocked[id] || (cachedRound && id != cached.Result.SourceID) {
				continue
			}
			task := mediaTask{id: id, quality: q, exact: cachedRound || strategy != "speed", refreshed: &refreshed}
			if task.exact {
				if attempted[candidateKey{id, q}] || (strategy != "speed" && !s.Catalog.Sources.SupportsMusicURLQuality(id, in.Source(), q)) {
					continue
				}
				task.lane = []string{q}
			} else {
				// speed 在源内逐档降级，跳过本请求已尝试过的档位（如缓存轮失败的档位）。
				for _, laneQuality := range js.QualityChain(q) {
					if !attempted[candidateKey{id, laneQuality}] && s.Catalog.Sources.SupportsMusicURLQuality(id, in.Source(), laneQuality) {
						task.lane = append(task.lane, laneQuality)
					}
				}
				if len(task.lane) == 0 {
					continue
				}
			}
			if _, owned := leases[id]; !owned {
				release, ok := s.Catalog.Sources.AcquirePlaybackProbe(id, in.Source())
				if !ok {
					blocked[id] = true
					continue
				}
				leases[id] = release
			}
			for _, laneQuality := range task.lane {
				attempted[candidateKey{id, laneQuality}] = true
			}
			if cachedRound {
				task.cached = &cached
			}
			tasks = append(tasks, task)
		}
		if len(tasks) == 0 {
			continue
		}
		// 每档独立预算：本轮超时只说明这一档不可用，应立即进入下一档，而不是耗尽总预算。
		// 缓存校验与首个竞速档位等得久一些（多数请求在此命中）；speed 只有一轮竞速，直接使用总预算。
		var roundCtx context.Context
		var stopRound context.CancelFunc
		switch {
		case strategy == "speed" && !cachedRound:
			roundCtx, stopRound = context.WithCancel(ctx)
		case !raced:
			roundCtx, stopRound = context.WithTimeout(ctx, mediaFirstQualityTimeout)
		default:
			roundCtx, stopRound = context.WithTimeout(ctx, mediaNextQualityTimeout)
		}
		raced = raced || !cachedRound
		winner, failures, err := s.raceMediaCandidates(roundCtx, r, in, tasks, proxy, headMetadata, concurrency, &calls)
		roundExpired := errors.Is(roundCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		stopRound()
		// 并发下失败按完成先后到达；轮内按调度顺序处理，不确定候选与最终错误不受竞速时序影响。
		slices.SortStableFunc(failures, func(a, b mediaOutcome) int {
			return slices.Index(ids, a.task.id) - slices.Index(ids, b.task.id)
		})
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
			if roundExpired {
				// 本轮是这一档自己的时间用完了，不是源级故障：不让源被判死，留到后续档位继续尝试。
				continue
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
	// 最后一档：原平台所有档位与源都失败时，在其它平台找同一首歌。
	// HEAD 预检保持快速失败（客户端随后会发 GET，由 GET 承担兜底），避免预检把等待拉长。
	if !headMetadata {
		if rescued, rescuedResp, rescueErr := s.crossSourceRescue(ctx, r, in, quality, proxy, &calls); rescued.Result.URL != "" {
			return rescued, rescuedResp, nil
		} else if rescueErr != nil {
			lastFailure = rescueErr
		}
	}
	return result, nil, lastFailure
}

// crossSourceRescue 在原平台整体不可用时，用同名同歌手同长度的候选到其它平台取链。
// 只有原生路径彻底失败才会走到这里，正常播放不会承担额外耗时。
func (s *Server) crossSourceRescue(ctx context.Context, r *http.Request, in *music.Info, quality string, proxy bool, calls *atomic.Int32) (music.URLResolution, *http.Response, error) {
	if s.Catalog == nil || s.Catalog.Sources == nil || ctx.Err() != nil {
		return music.URLResolution{}, nil, nil
	}
	rescueCtx, cancel := context.WithTimeout(r.Context(), crossSourceTimeout)
	defer cancel()
	started := time.Now()
	candidate := s.Catalog.FindCrossSourceCandidate(rescueCtx, in)
	s.Diagnostics.Add(diagnostics.Event{Stage: "cross_source", TrackID: in.TrackID(), Platform: in.Source(), Quality: quality, ElapsedMS: time.Since(started).Milliseconds()})
	if candidate == nil {
		return music.URLResolution{}, nil, nil
	}
	ids := s.Catalog.Sources.MusicURLSourceIDs(candidate.Source())
	if len(ids) == 0 {
		return music.URLResolution{}, nil, errMediaUnavailable
	}
	// 兜底只尝试一档，避免换平台后又陷入长时间降档。
	rescueQuality := music.SelectQuality(quality, candidate.Qualities())
	if js.QualityRank(rescueQuality) > js.QualityRank("320k") {
		rescueQuality = js.QualityChain(rescueQuality)[1]
	}
	if rescueQuality == "" {
		rescueQuality = "320k"
	}
	tasks := make([]mediaTask, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, mediaTask{id: id, quality: rescueQuality, exact: true})
	}
	winner, _, err := s.raceMediaCandidates(rescueCtx, r, candidate, tasks, proxy, false, s.sourceConcurrency(), calls)
	if winner == nil {
		if err == nil {
			err = errMediaUnavailable
		}
		return music.URLResolution{}, nil, err
	}
	// 兜底结果不写入原歌曲的直链缓存：它属于另一个平台的另一条曲目。
	winner.resolution.Validated = true
	s.Diagnostics.Add(diagnostics.Event{Stage: "cross_source", TrackID: in.TrackID(), Platform: candidate.Source(), Quality: winner.resolution.Result.Quality, Status: winner.status, ElapsedMS: time.Since(started).Milliseconds()})
	return winner.resolution, winner.response, nil
}

// sourceConcurrency 返回播放取链同时请求的音源数。
func (s *Server) sourceConcurrency() int {
	if n := s.Settings.Get().SourceConcurrency; n > 0 {
		return n
	}
	return settings.DefaultSourceConcurrency
}

// raceMediaCandidates 按调度顺序同时启动至多 concurrency 个候选，失败立即补位；
// 首个满足请求档位的有效媒体胜出并取消其余候选。
func (s *Server) raceMediaCandidates(ctx context.Context, r *http.Request, in *music.Info, tasks []mediaTask, proxy, headMetadata bool, concurrency int, calls *atomic.Int32) (winner *mediaOutcome, failures []mediaOutcome, resultErr error) {
	concurrency = max(1, concurrency)
	workCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	// 在途任务不超过 concurrency，缓冲足以让收尾时的工作者全部写入而不阻塞。
	results := make(chan mediaOutcome, concurrency)
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
	busy := false
	fill := func() {
		for !busy && next < len(tasks) && active < concurrency && (next == 0 || calls.Load() < maxMediaAttempts) {
			task := tasks[next]
			task.fallback = next > 0
			next++
			active++
			sourceCount := len(tasks) // 启动前取值：主协程补做刷新时会重新赋值 tasks
			workers.Go(func() { results <- s.tryMediaCandidate(workCtx, r, in, task, proxy, headMetadata, sourceCount, calls) })
		}
	}
	qualityTimer := time.NewTimer(time.Hour)
	qualityTimer.Stop()
	defer qualityTimer.Stop()
	var qualityDeadline <-chan time.Time
	var deferred []mediaTask
	fill()
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
					deferred = append(deferred, mediaTask{id: outcome.task.id, quality: outcome.task.quality, exact: outcome.task.exact, lane: outcome.task.lane, failed: outcome.refresh, refreshed: outcome.task.refreshed})
				}
			}
			if !busy && next == len(tasks) && active == 0 && best == nil && len(deferred) > 0 {
				tasks = append(tasks, deferred...)
				deferred = nil
			}
			fill()
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
	// 单档超时只说明这一档慢，单源预算未尽时继续降档。
	ctx, cancel := context.WithTimeout(ctx, mediaSpeedLaneTimeout)
	defer cancel()
	out := mediaOutcome{task: task, err: js.ErrNoQuality}
	var rejected []string
	for index, quality := range task.lane {
		next := task
		next.quality, next.exact = quality, true
		if index > 0 {
			next.failed = nil
		}
		out = s.tryExactMediaCandidate(ctx, r, in, next, proxy, headMetadata, 1, calls)
		out.task = task
		rejected = append(rejected, out.rejected...)
		out.rejected = rejected
		if out.err == nil || ctx.Err() != nil || out.status == 416 || out.status == 429 || out.status >= 500 || errors.Is(out.err, admission.ErrBusy) || errors.Is(out.err, context.Canceled) || errors.Is(out.err, js.ErrSourceAuth) || errors.Is(out.err, js.ErrSourceUnavailable) || errors.Is(out.err, js.ErrSourceRateLimited) || errors.Is(out.err, js.ErrInvalidSourceResponse) || errors.Is(out.err, errInvalidMediaURL) || errors.Is(out.err, httpguard.ErrNonAudioResponse) || (out.status == 0 && out.resolution.Result.URL != "") {
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
			// 每源每请求最多刷新一次（task.refreshed 合并），既不放弃刷新机会，也不放大取链次数。
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
