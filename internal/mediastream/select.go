package mediastream

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/chksz"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/playback"
	"lxsc/internal/settings"
)

// 选源只有一条规则：把"音源 × 音质 × 平台"摊平成候选，并发取链并验证，第一个真正能播放的候选胜出；
// 只有全部候选都确认失败才向客户端报告失败。没有分档轮次、总预算或尝试次数上限。
const (
	// prepareLimit 只是防止请求无限占用资源的安全上限，不参与选源；正常失败由候选耗尽结束。
	prepareLimit = 30 * time.Second
	// attemptLimit 是一次尝试（一次取链加一次音频验证）的上限。
	attemptLimit = 8 * time.Second
	// mediaHeaderTimeout 是代理模式下等待上游响应头的上限。
	mediaHeaderTimeout = 3 * time.Second
	// crossSearchLimit 是到其它平台搜索同一首歌的上限。
	crossSearchLimit = 5 * time.Second
	// qualityGrace 是音质优先时，已有可播放结果后为在途的更高音质最多再等的时间；速度优先不等。
	qualityGrace = time.Second
	// fallbackReserve 是启用 ChKSz 兜底时为它预留的时间：音源与换平台选源最多用到 prepareLimit 减去该值，
	// 音源迟迟没有结论时仍来得及兜底。
	fallbackReserve = 8 * time.Second
)

var (
	errMediaUnavailable = errors.New("没有可用的音频响应")
	errInvalidMediaURL  = errors.New("无效的播放地址")
)

// lane 是一个音源脚本对一首曲目的全部候选：从高到低逐档尝试，同一时刻只有一档在途。
// 不同音源互不依赖，所以多条 lane 并发；同一音源的各档共用上游，所以 lane 内顺序执行。
type lane struct {
	track     *music.Info // 取链所用的曲目；换平台时是其它平台上的同一首歌
	source    int64
	qualities []string
	next      int
	// again 收集首轮中被上游以过期类状态（403/404/410）拒绝的档位：
	// 全部候选都失败后，这些档位各向原音源重新取一次；retried 标记第二轮，不再继续重取。
	// 第二轮每档单独一条 lane：它们都已失败过一次，没有理由再让低档排在高档后面等。
	again   []string
	retried bool
}

// attempt 是一次尝试的结果：err 为空表示地址已取得并通过验证。
type attempt struct {
	lane       int
	quality    string
	resolution music.URLResolution
	response   *http.Response
	status     int
	ms         int64
	err        error
	stop       bool // 音源此刻整体不可用，同一 lane 不再尝试更低音质
	again      bool // 新取的地址被上游以过期类状态拒绝，全部候选失败后值得向同一音源再取一次
}

func (a *attempt) close() {
	if a != nil && a.response != nil {
		_ = a.response.Body.Close()
	}
}

func (a *attempt) rank() int { return music.QualityRank(a.resolution.Result.Quality) }

type preparation struct {
	*Service
	request     *http.Request
	quality     string
	proxy, head bool
	concurrency int
	grace       time.Duration
	calls       *atomic.Int32 // 同一请求内各路选源共享的尝试序号
	// failure 是最近一次有意义的失败，全部候选耗尽时作为返回的错误类别。
	failure error
}

// Prepare 为一次播放请求选出可播放的音频。
// 顺序固定为：已验证的缓存 → 歌曲所在平台的全部音源和音质 → 其它平台上的同一首歌 → ChKSz API 兜底（已配置密钥时）。
// 近期已确认所在平台全部失败并换平台播放成功的曲目，先复用那首替代曲目；近期靠兜底播放成功的曲目，先走兜底。
// 所在平台近期连续多首歌都确认全部失败时，第二、三步同时进行，先得到可播放音频的一路胜出。
// 返回错误且请求未被取消时，上述候选均已逐一确认失败。
func (s *Service) Prepare(ctx context.Context, r *http.Request, in *music.Info, quality string, proxy, headMetadata bool) (result music.URLResolution, response *http.Response, resultErr error) {
	finish := s.Catalog.MediaPreparation.Start()
	defer func() { finish(resultErr) }()
	if s.Catalog.Sources == nil {
		return result, nil, errMediaUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, prepareLimit)
	defer cancel()
	p := &preparation{Service: s, request: r, quality: quality, proxy: proxy, head: headMetadata, concurrency: s.sourceConcurrency(), calls: new(atomic.Int32), failure: errMediaUnavailable}
	if s.Settings.Get().SourceSelection != "speed" {
		p.grace = qualityGrace
	}
	var winner *selection
	var err error
	remember := !headersOnly(ctx)
	if substitute := s.Catalog.PlaybackSubstitute(in, quality, !proxy); remember && substitute != nil {
		started := time.Now()
		if winner, err = p.pick(ctx, []*music.Info{substitute}); winner != nil {
			s.mediaRequestEvent(r, "cross_source", in.TrackID(), winner.track.Source(), winner.resolution.Result.Quality, true, winner.status, started, nil)
		} else if err == nil {
			s.Catalog.ForgetPlaybackSubstitute(in, !proxy)
		}
	}
	// 兜底接口按次计额度，只在音源结论之后调用；近期靠兜底播放成功的曲目直接复用结论。
	fallback := s.Catalog.FallbackEnabled()
	fallbackTried, viaFallback := false, false
	if track := s.Catalog.PlaybackFallback(in, quality, !proxy); winner == nil && err == nil && remember && fallback && track != nil {
		fallbackTried = true
		if winner, err = p.fallback(ctx, in, track); winner != nil {
			viaFallback = true
		} else if err == nil {
			s.Catalog.ForgetPlaybackFallback(in, !proxy)
		}
	}
	found, confirmed := false, false
	if winner == nil && err == nil {
		sourceCtx := ctx
		if fallback && !fallbackTried {
			var stop context.CancelFunc
			sourceCtx, stop = context.WithTimeout(ctx, prepareLimit-fallbackReserve)
			defer stop()
		}
		if s.Catalog.HomePlatformFailing(in.Source(), !proxy) {
			winner, found, err = p.hedge(sourceCtx, in)
		} else if winner, err = p.home(sourceCtx, in); winner == nil && err == nil {
			winner, err = p.crossSource(sourceCtx, in)
			found = winner != nil
		}
		// 只有音源和平台都确认失败才记住兜底结论；被预留时间截断的选源没有结论，下次仍先试音源。
		confirmed = winner == nil && err == nil && sourceCtx.Err() == nil
	}
	// 音源全部确认失败，或被预留时间截断时兜底；客户端离开、本机繁忙或缺少解码器时不兜底。
	if winner == nil && fallback && !fallbackTried && ctx.Err() == nil && (err == nil || errors.Is(err, context.DeadlineExceeded)) {
		if rescued, rescueErr := p.fallback(ctx, in, nil); rescued != nil {
			winner, err, viaFallback = rescued, nil, true
		} else if err == nil {
			err = rescueErr
		}
	}
	if winner == nil {
		switch {
		case r.Context().Err() != nil:
			return result, nil, r.Context().Err()
		case err != nil:
			return result, nil, err
		case ctx.Err() != nil:
			return result, nil, ctx.Err()
		}
		return result, nil, p.failure
	}
	result = winner.resolution
	// 超出文件末尾的 Range 原样交给客户端，不能据此认定音频已验证。
	result.Validated = !headersOnly(ctx) && winner.status != http.StatusRequestedRangeNotSatisfiable
	if result.Validated {
		track := winner.track
		switch {
		case viaFallback:
			// 兜底地址不进入音源直链缓存，也不计入音源评分。
			if confirmed {
				s.Catalog.RememberPlaybackFallback(in, quality, !proxy, track)
			}
		default:
			result = s.Catalog.PublishPlaybackURL(track, quality, result)
			result.Validated = true
			s.Catalog.ReportPlaybackOutcome(result, track.Source(), true, winner.ms)
			if found {
				s.Catalog.RememberPlaybackSubstitute(in, quality, !proxy, track)
			}
		}
	}
	return result, winner.response, nil
}

// fallback 通过 ChKSz API 兜底取链并验证，第一个通过验证的地址胜出。
// track 非空时只试这一首（复用近期结论）；否则先试所在平台，仍失败再试其它平台上最匹配的一首受支持曲目。
// 额度按次计，曲目和音质依次尝试而不并发；返回 (nil, nil) 表示兜底也已失败。
// 兜底不改变 p.failure：失败类别保留音源的结论，兜底的失败只记入诊断与播放日志。
func (p *preparation) fallback(ctx context.Context, in, track *music.Info) (*selection, error) {
	tracks := []*music.Info{track}
	if track == nil {
		tracks = []*music.Info{in}
	}
	for index := 0; index < len(tracks); index++ {
		candidate := tracks[index]
		if music.FallbackSupports(candidate) {
			winner, stop, err := p.tryFallback(ctx, candidate)
			if winner != nil || err != nil || stop {
				return winner, err
			}
		}
		if track == nil && index == 0 {
			searchCtx, cancel := context.WithTimeout(ctx, crossSearchLimit)
			for _, substitute := range p.Catalog.FindCrossSourceCandidates(searchCtx, in) {
				if music.FallbackSupports(substitute) {
					tracks = append(tracks, substitute)
					break
				}
			}
			cancel()
		}
	}
	return nil, ctx.Err()
}

// tryFallback 为一首曲目取兜底地址：先取请求的音质，接口明确表示该档不可用时再取 320k。
// stop 表示兜底接口此刻整体不可用（密钥失效、限流），不再尝试其它曲目。
func (p *preparation) tryFallback(ctx context.Context, track *music.Info) (winner *selection, stop bool, err error) {
	quality := music.SelectQuality(p.quality, track.Qualities())
	qualities := []string{quality}
	if music.QualityRank(quality) > music.QualityRank("320k") {
		qualities = append(qualities, "320k")
	}
	for _, quality := range qualities {
		out, err := p.fallbackAttempt(ctx, track, quality)
		switch {
		case out != nil:
			return &selection{*out, track}, false, nil
		case ctx.Err() != nil:
			return nil, false, ctx.Err()
		case errors.Is(err, admission.ErrBusy), errors.Is(err, httpguard.ErrDecoderUnavailable):
			return nil, false, err
		case errors.Is(err, chksz.ErrAuth), errors.Is(err, chksz.ErrRateLimited), errors.Is(err, chksz.ErrKeyInURL):
			return nil, true, nil
		case !errors.Is(err, chksz.ErrNoURL):
			// 地址验证失败或接口故障：更低音质通常是同一个问题，换下一首曲目。
			return nil, false, nil
		}
	}
	return nil, false, nil
}

// fallbackAttempt 是一次兜底尝试：取地址并按实际交付方式验证。缓存的地址失效时向接口重新取一次。
func (p *preparation) fallbackAttempt(ctx context.Context, track *music.Info, quality string) (*attempt, error) {
	ctx, cancel := context.WithTimeout(ctx, attemptLimit)
	defer cancel()
	for refreshed := false; ; refreshed = true {
		number := int(p.calls.Add(1))
		started := time.Now()
		resolution, err := p.Catalog.ResolveFallbackURL(ctx, track, quality)
		p.mediaAttemptEvent(ctx, p.request, "fallback_api", track, quality, resolution, 0, started, err, number, music.FallbackSourceID)
		if err != nil {
			return nil, err
		}
		out := p.verify(ctx, track, resolution, music.FallbackSourceID)
		if out.err == nil {
			return &out, nil
		}
		out.close()
		if ctx.Err() != nil || errors.Is(out.err, httpguard.ErrDecoderUnavailable) {
			return nil, out.err
		}
		if out.definite() || resolution.Cached {
			p.Catalog.InvalidatePlaybackURL(resolution)
		}
		if !resolution.Cached || refreshed {
			return nil, out.err
		}
	}
}

// fork 为同一请求的另一路选源创建独立的失败记录，共享尝试序号。
func (p *preparation) fork() *preparation {
	return &preparation{Service: p.Service, request: p.request, quality: p.quality, proxy: p.proxy, head: p.head, concurrency: p.concurrency, grace: p.grace, calls: p.calls, failure: errMediaUnavailable}
}

// home 在歌曲所在平台选源，并记录有结论的结果，供后续请求判断该平台是否整体持续失败。
// 刚确认过全部失败、本次直接跳过的曲目不重复计数。
func (p *preparation) home(ctx context.Context, in *music.Info) (*selection, error) {
	record := !headersOnly(ctx) && !p.Catalog.PlaybackExhausted(in, p.quality, !p.proxy)
	winner, err := p.pick(ctx, []*music.Info{in})
	if record {
		switch {
		case winner != nil:
			p.Catalog.RecordHomePlatform(in.Source(), !p.proxy, true)
		case err == nil && ctx.Err() == nil:
			p.Catalog.RecordHomePlatform(in.Source(), !p.proxy, false)
		}
	}
	return winner, err
}

// crossSource 到其它平台搜索同一首歌并选源。
func (p *preparation) crossSource(ctx context.Context, in *music.Info) (*selection, error) {
	started := time.Now()
	searchCtx, stop := context.WithTimeout(ctx, crossSearchLimit)
	substitutes := p.Catalog.FindCrossSourceCandidates(searchCtx, in)
	p.mediaRequestEvent(p.request, "cross_source", in.TrackID(), in.Source(), p.quality, false, 0, started, searchCtx.Err())
	stop()
	winner, err := p.pick(ctx, substitutes)
	if winner != nil {
		p.mediaRequestEvent(p.request, "cross_source", in.TrackID(), winner.track.Source(), winner.resolution.Result.Quality, false, winner.status, started, nil)
	}
	return winner, err
}

// hedge 在所在平台整体持续失败时让所在平台与换平台同时选源：先得到可播放音频的一路胜出，另一路随即取消。
// 两路各自独立确认失败，都确认失败才报告失败；found 表示由其它平台胜出。
func (p *preparation) hedge(ctx context.Context, in *music.Info) (winner *selection, found bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		winner  *selection
		err     error
		failure error
		cross   bool
	}
	results := make(chan outcome, 2)
	run := func(cross bool) {
		q := p.fork()
		out := outcome{cross: cross}
		if cross {
			out.winner, out.err = q.crossSource(ctx, in)
		} else {
			out.winner, out.err = q.home(ctx, in)
		}
		out.failure = q.failure
		results <- out
	}
	go run(false)
	go run(true)
	for pending := 2; pending > 0; pending-- {
		out := <-results
		if out.winner != nil {
			if pending == 2 {
				// 落败一路随返回取消，在后台结束并关闭它可能已拿到的响应。
				go func() {
					if late := <-results; late.winner != nil {
						late.winner.close()
					}
				}()
			}
			return out.winner, out.cross, nil
		}
		if err == nil {
			err = out.err
		}
		// 报告的失败类别优先取所在平台的。
		if out.failure != errMediaUnavailable && (!out.cross || p.failure == errMediaUnavailable) {
			p.failure = out.failure
		}
	}
	return nil, false, err
}

// selection 在 attempt 之外记住胜出候选所属的曲目，换平台时缓存与评分都记在实际播放的曲目上。
type selection struct {
	attempt
	track *music.Info
}

// pick 在给定曲目中选出一个可播放的候选。返回 (nil, nil) 表示这些曲目的全部候选都已确认失败。
func (p *preparation) pick(ctx context.Context, tracks []*music.Info) (*selection, error) {
	remember := !headersOnly(ctx)
	var lanes []*lane
	var selected []*music.Info // 本次真正参与选源的曲目
	for _, track := range tracks {
		if remember {
			if found := p.cached(ctx, track); found != nil {
				return &selection{*found, track}, nil
			}
			// 刚确认过全部失败的曲目不再重复整轮选源。
			if p.Catalog.PlaybackExhausted(track, p.quality, !p.proxy) {
				continue
			}
		}
		selected = append(selected, track)
		lanes = append(lanes, p.lanes(track)...)
	}
	p.order(lanes)
	best, complete, err := p.race(ctx, lanes)
	if best == nil && err == nil && complete {
		var again []*lane
		for _, l := range lanes {
			for _, quality := range l.again {
				again = append(again, &lane{track: l.track, source: l.source, qualities: []string{quality}, retried: true})
			}
		}
		if len(again) > 0 {
			lanes = again
			p.order(lanes)
			best, complete, err = p.race(ctx, lanes)
		}
	}
	if best != nil {
		return &selection{*best, lanes[best.lane].track}, nil
	}
	if err == nil && complete && remember {
		// 只记本次跑过的曲目：直接跳过的曲目若也重新记一遍，客户端持续重试时失败结论会无限续期。
		for _, track := range selected {
			p.Catalog.MarkPlaybackExhausted(track, p.quality, !p.proxy)
		}
	}
	return nil, err
}

// order 在音质优先时把并发名额先给能提供更高音质的 lane，同档保持调度评分顺序。
func (p *preparation) order(lanes []*lane) {
	if p.grace > 0 {
		slices.SortStableFunc(lanes, func(a, b *lane) int {
			return music.QualityRank(b.qualities[0]) - music.QualityRank(a.qualities[0])
		})
	}
}

// lanes 按调度评分顺序列出一首曲目的全部候选：每个支持该平台的音源一条，音质从高到低。
func (p *preparation) lanes(track *music.Info) []*lane {
	var out []*lane
	for _, id := range p.Catalog.Sources.MusicURLSourceIDs(track.Source()) {
		l := &lane{track: track, source: id}
		for _, quality := range js.QualityChain(music.SelectQuality(p.quality, track.Qualities())) {
			// 歌曲本身没有的档位落到它实际拥有的下一档，避免同一档重复尝试。
			quality = music.SelectQuality(quality, track.Qualities())
			if !slices.Contains(l.qualities, quality) && p.Catalog.Sources.SupportsMusicURLQuality(id, track.Source(), quality) {
				l.qualities = append(l.qualities, quality)
			}
		}
		if len(l.qualities) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// cached 复用上一次已验证胜出的地址。地址每次仍要重新验证（短时间内的验证结果由目录层复用），
// 失效时丢弃并回到正常选源。
func (p *preparation) cached(ctx context.Context, track *music.Info) *attempt {
	resolution, ok := p.Catalog.CachedPlaybackURL(track, p.quality)
	if !ok || !slices.Contains(p.Catalog.Sources.MusicURLSourceIDs(track.Source()), resolution.Result.SourceID) {
		return nil
	}
	p.calls.Add(1)
	out := p.verify(ctx, track, resolution, resolution.Result.SourceID)
	if out.err == nil {
		return &out
	}
	out.close()
	if ctx.Err() == nil {
		// 旧地址过期是常态，不计入音源评分；丢弃后由正常选源向音源重新取链。
		p.Catalog.InvalidatePlaybackURL(resolution)
	}
	return nil
}

// race 并发推进各条 lane，直到出现胜者或全部 lane 走完。
// complete 为真表示没有任何候选被取消、超时上限或繁忙打断，失败结论可以确认。
func (p *preparation) race(ctx context.Context, lanes []*lane) (best *attempt, complete bool, err error) {
	raceCtx, stop := context.WithCancel(ctx)
	results := make(chan attempt)
	running, started := 0, 0
	inFlight := map[int]int{} // lane → 在途音质的档位
	defer func() {
		stop()
		// 落败的在途尝试可能已经拿到响应，全部收回并关闭。
		for ; running > 0; running-- {
			late := <-results
			late.close()
		}
	}()
	launch := func(index int) {
		l := lanes[index]
		quality := l.qualities[l.next]
		l.next++
		inFlight[index] = music.QualityRank(quality)
		running++
		go func() { results <- p.try(raceCtx, index, l, quality) }()
	}
	busy := false
	fill := func() {
		for best == nil && !busy && running < p.concurrency && started < len(lanes) {
			launch(started)
			started++
		}
	}
	var grace <-chan time.Time
	fill()
	for running > 0 {
		select {
		case <-ctx.Done():
			if best != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return best, false, nil
			}
			best.close()
			return nil, false, ctx.Err()
		case <-grace:
			return best, false, nil
		case out := <-results:
			running--
			delete(inFlight, out.lane)
			switch {
			case out.err == nil:
				if best == nil || out.rank() > best.rank() {
					best.close()
					best = &out
				} else {
					out.close()
				}
			case errors.Is(out.err, httpguard.ErrDecoderUnavailable):
				// 缺少解码器是部署问题，换源也无法验证。
				best.close()
				return nil, false, out.err
			case errors.Is(out.err, admission.ErrBusy):
				// 本机繁忙时不再扩大请求，仍让在途候选完成。
				busy = true
			default:
				p.failure = out.err
				l := lanes[out.lane]
				if out.again && !l.retried {
					l.again = append(l.again, out.quality)
				}
				if best == nil && !busy && !out.stop && l.next < len(l.qualities) {
					launch(out.lane) // 同一音源的下一档
				}
			}
			if best != nil {
				higher := false
				for _, rank := range inFlight {
					higher = higher || rank > best.rank()
				}
				if !higher || p.grace == 0 {
					return best, false, nil
				}
				if grace == nil {
					timer := time.NewTimer(p.grace)
					defer timer.Stop()
					grace = timer.C
				}
			}
			fill()
		}
	}
	if busy {
		return nil, false, admission.ErrBusy
	}
	return nil, ctx.Err() == nil, nil
}

// try 向一个音源请求一个音质的地址并验证。缓存里的旧地址失效时立即向同一音源重新取一次，
// 除此之外不重试：换音质、换音源和第二轮重取都由 pick 与 race 决定。
func (p *preparation) try(ctx context.Context, index int, l *lane, quality string) attempt {
	ctx, cancel := context.WithTimeout(ctx, attemptLimit)
	defer cancel()
	var stale *music.URLResolution
	for {
		number := int(p.calls.Add(1))
		stage := "resolve"
		if index > 0 {
			stage = "source_fallback"
		}
		if stale != nil || l.retried {
			stage = "refresh"
		}
		started := time.Now()
		resolution, err := p.Catalog.ResolvePlaybackCandidate(ctx, l.track, quality, []int64{l.source}, stale)
		p.mediaAttemptEvent(ctx, p.request, stage, l.track, quality, resolution, 0, started, err, number, l.source)
		if err != nil {
			return attempt{lane: index, quality: quality, err: err, stop: sourceDown(err)}
		}
		out := p.verify(ctx, l.track, resolution, l.source)
		out.lane, out.quality = index, quality
		if out.err == nil {
			return out
		}
		out.close()
		out.response = nil
		if ctx.Err() != nil || errors.Is(out.err, httpguard.ErrDecoderUnavailable) {
			return out
		}
		if out.definite() && !headersOnly(ctx) {
			p.Catalog.InvalidatePlaybackURL(resolution)
			if !resolution.Cached {
				p.Catalog.ReportPlaybackOutcome(resolution, l.track.Source(), false, out.ms)
			}
		}
		if resolution.Cached && stale == nil {
			stale = &resolution
			continue
		}
		out.again = stale == nil && expiredMediaStatus(out.status)
		return out
	}
}

// verify 按实际交付方式验证地址：代理时直接打开客户端请求的范围并保留正文，
// 其余情况只读取并解码文件开头的片段。
func (p *preparation) verify(ctx context.Context, track *music.Info, resolution music.URLResolution, source int64) attempt {
	out := attempt{resolution: resolution}
	started := time.Now()
	stage := "url_check"
	if p.proxy && !p.head {
		stage = "proxy_response"
		headCtx, stop := context.WithTimeout(ctx, mediaHeaderTimeout)
		out.response, out.err = p.openMedia(headCtx, p.request, track, resolution.Result.URL)
		stop()
		if out.response != nil && out.err == nil {
			out.err = p.verifyProxyMedia(ctx, track, resolution, out.response)
		}
	} else {
		if resolution.Cached {
			stage = "cache_check"
		}
		out.response, out.err = p.checkMedia(ctx, resolution, track, !p.proxy)
	}
	if out.response != nil {
		out.status = out.response.StatusCode
	}
	passthrough := p.proxy && !p.head && out.status == http.StatusRequestedRangeNotSatisfiable
	if out.err == nil && out.status != http.StatusOK && out.status != http.StatusPartialContent && !passthrough {
		out.err = mediaHTTPError(out.status)
	}
	out.ms = time.Since(started).Milliseconds()
	p.mediaAttemptEvent(ctx, p.request, stage, track, resolution.Result.Quality, resolution, out.status, started, out.err, int(p.calls.Load()), source)
	if out.status == http.StatusTooManyRequests && source > 0 && ctx.Err() == nil && !headersOnly(ctx) {
		p.Catalog.Sources.BackoffMusicURL(source, track.Source(), mediaRetryAfter(out.response), resolution.Result.ScriptFingerprint)
	}
	// 被限流或连不上音频地址时，更低音质通常指向同一上游，不再逐档重复等待。
	out.stop = out.status == http.StatusTooManyRequests || errors.Is(out.err, context.DeadlineExceeded)
	return out
}

// definite 区分"上游明确给出了不可播放的内容"与"网络不确定"：只有前者才丢弃地址并计入音源评分。
func (a *attempt) definite() bool {
	return a.status != 0 || errors.Is(a.err, errInvalidMediaURL) || errors.Is(a.err, httpguard.ErrNonAudioResponse)
}

// sourceDown 判断取链失败是否说明音源此刻整体不可用：认证失效、限流、接口故障或没有响应。
// 这类失败与音质无关，继续降档只会重复同一个错误；其余失败照常尝试更低音质。
func sourceDown(err error) bool {
	return errors.Is(err, js.ErrNoSource) || errors.Is(err, js.ErrSourceAuth) || errors.Is(err, js.ErrSourceRateLimited) ||
		errors.Is(err, js.ErrSourceUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

// sourceConcurrency 返回同时取链的音源数。
func (s *Service) sourceConcurrency() int {
	if n := s.Settings.Get().SourceConcurrency; n > 0 {
		return n
	}
	return settings.DefaultSourceConcurrency
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

func (s *Service) mediaAttemptEvent(ctx context.Context, r *http.Request, stage string, in *music.Info, quality string, resolution music.URLResolution, status int, started time.Time, err error, attempt int, sourceID int64) {
	e := diagnostics.Event{Stage: stage, TrackID: in.TrackID(), Platform: in.Source(), Quality: quality, RequestedQuality: param(r, "quality"), Cached: resolution.Cached, Status: status, Error: diagnostics.ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds(), Attempt: attempt}
	if parent, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event); parent != nil {
		e.RequestID = parent.RequestID
	}
	if deadline, ok := ctx.Deadline(); ok {
		e.BudgetMS = max(0, time.Until(deadline).Milliseconds())
	}
	if stage == "url_check" || stage == "cache_check" || stage == "proxy_response" {
		e.Validation = "failed"
		if err == nil && (status == http.StatusOK || status == http.StatusPartialContent) {
			e.Validation = "validated"
			if headersOnly(ctx) {
				e.Validation = "headers_only"
			}
		} else if status == 0 && !errors.Is(err, errInvalidMediaURL) {
			e.Validation = "unverified"
		}
	}
	s.Diagnostics.Add(e)
	sourceName := resolution.Result.Source
	if sourceName == "" && s.Catalog.Sources != nil {
		if source := s.Catalog.Sources.StatusOf(sourceID); source != nil {
			sourceName = source.Name
		}
	}
	trace := playback.FromContext(r.Context())
	trace.Add(db.PlaybackEvent{At: time.Now().UnixMilli(), Stage: stage, SourceID: sourceID, SourceName: trace.SourceName(sourceID, sourceName), Platform: in.Source(), Quality: quality, Cached: resolution.Cached, Status: status, Error: e.Error, ElapsedMS: e.ElapsedMS})
}
