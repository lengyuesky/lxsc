// Package playback 记录播放交付事实，不从媒体请求推算实际收听时长。
package playback

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"lxsc/internal/db"
)

const FnDuplicateWindow = 5 * time.Second

var playbackKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

func ValidKey(key string) bool { return playbackKeyPattern.MatchString(key) }

type clientKey struct{}
type traceKey struct{}

type Client struct {
	Protocol, Name, Key string
	// Session 是独立随机关联 ID，不是认证票据、设备密钥或其摘要。
	Session string
}

func Bind(r *http.Request, client Client) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), clientKey{}, client))
}

func BindDefault(r *http.Request, client Client) *http.Request {
	if _, ok := r.Context().Value(clientKey{}).(Client); ok {
		return r
	}
	return Bind(r, client)
}

func FromContext(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

func Text(value string, limit int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(value, "")))
	if utf8.RuneCountInString(value) > limit {
		value = string([]rune(value)[:limit])
	}
	return value
}

type sessionID struct {
	user int64
	id   string
}

type pendingMedia struct {
	key string
	at  time.Time
}

type fnState struct {
	track, key string
	started    time.Time
	touched    time.Time
	pending    map[string]pendingMedia
}

type Recorder struct {
	DB       *db.DB
	Log      *slog.Logger
	mu       sync.Mutex
	sessions map[sessionID]*fnState
	now      func() time.Time
}

func New(database *db.DB, log *slog.Logger) *Recorder {
	if database == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{DB: database, Log: log, sessions: map[sessionID]*fnState{}, now: time.Now}
}

func (r *Recorder) session(id sessionID, now time.Time) *fnState {
	if state := r.sessions[id]; state != nil && now.Sub(state.touched) <= 24*time.Hour {
		state.touched = now
		return state
	}
	// 飞牛认证同样限制总会话数。过期及容量淘汰仅影响请求关联，不删除持久统计。
	for key, state := range r.sessions {
		if now.Sub(state.touched) > 24*time.Hour {
			delete(r.sessions, key)
		}
	}
	if len(r.sessions) >= 4096 {
		var oldest sessionID
		var at time.Time
		for key, state := range r.sessions {
			if at.IsZero() || state.touched.Before(at) {
				oldest, at = key, state.touched
			}
		}
		delete(r.sessions, oldest)
	}
	state := &fnState{touched: now, pending: map[string]pendingMedia{}}
	r.sessions[id] = state
	return state
}

func (r *Recorder) mediaKey(userID int64, session, track string, resume bool, now time.Time) (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.session(sessionID{userID, session}, now)
	if state.track == track && state.key != "" && (resume || (now.Sub(state.started) >= 0 && now.Sub(state.started) < FnDuplicateWindow)) {
		return state.key, "playback"
	}
	if pending, ok := state.pending[track]; ok && resume && now.Sub(pending.at) >= 0 && now.Sub(pending.at) <= 30*time.Second {
		return pending.key, "request"
	}
	key := rand.Text()
	for id, pending := range state.pending {
		if now.Sub(pending.at) > 30*time.Second {
			delete(state.pending, id)
		}
	}
	if len(state.pending) >= 16 {
		var oldest string
		var at time.Time
		for id, pending := range state.pending {
			if at.IsZero() || pending.at.Before(at) {
				oldest, at = id, pending.at
			}
		}
		delete(state.pending, oldest)
	}
	state.pending[track] = pendingMedia{key, now}
	return key, "request"
}

func (r *Recorder) FnStart(ctx context.Context, user *db.User, session string, track db.PlaybackTrack, durationMS int64) error {
	if r == nil || user == nil || session == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	state := r.session(sessionID{user.ID, session}, now)
	if state.track == track.ID && state.key != "" && now.Sub(state.started) >= 0 && now.Sub(state.started) < FnDuplicateWindow {
		return nil
	}
	key := rand.Text()
	if pending, ok := state.pending[track.ID]; ok && now.Sub(pending.at) >= 0 && now.Sub(pending.at) <= 30*time.Second {
		key = pending.key
	} else {
		// 失败后的重试仍可以复用这个标识，避免半途故障留下两份统计。
		state.pending[track.ID] = pendingMedia{key, now}
	}
	p := db.PlaybackStart{UserID: user.ID, UserName: Text(user.Name, 128), Protocol: "fnmusic", Client: "飞牛音乐", Kind: "playback", Key: key, Track: cleanTrack(track), At: now.UnixMilli()}
	if err := r.DB.RecordFnPlayback(ctx, p, durationMS); err != nil {
		return err
	}
	state.track, state.key, state.started = track.ID, key, now
	delete(state.pending, track.ID)
	return nil
}

func cleanTrack(track db.PlaybackTrack) db.PlaybackTrack {
	track.Name, track.Singer = Text(track.Name, 300), Text(track.Singer, 300)
	return track
}

// Begin 由已认证的共享交付入口调用。未知标识的外部客户端请求明确归为媒体请求。
func (r *Recorder) Begin(request *http.Request, user *db.User, trackID string, quality, mode string) (*http.Request, *Trace) {
	if r == nil || user == nil {
		return request, nil
	}
	client, _ := request.Context().Value(clientKey{}).(Client)
	if client.Protocol == "" {
		client.Protocol, client.Name = "subsonic", "未知客户端"
	}
	client.Name = Text(client.Name, 64)
	kind, key := "request", client.Key
	if key != "" {
		kind = "playback"
	} else if client.Protocol == "fnmusic" && client.Session != "" {
		// 非零 Range 可以关联已知的播放；较晚的完整取流也可能是同曲重播，
		// 先独立记录为媒体请求，随后开始事件再关联，避免永远合并到上一次播放。
		span, valid := strings.CutPrefix(request.Header.Get("Range"), "bytes=")
		first, _, _ := strings.Cut(span, "-")
		offset, err := strconv.ParseInt(first, 10, 64)
		key, kind = r.mediaKey(user.ID, client.Session, trackID, valid && err == nil && offset > 0, r.now())
	} else {
		key = rand.Text()
	}
	track := db.PlaybackTrack{ID: trackID, Name: "未知歌曲"}
	p := db.PlaybackStart{UserID: user.ID, UserName: Text(user.Name, 128), Key: key, Protocol: client.Protocol, Client: client.Name, Kind: kind, Track: track, At: r.now().UnixMilli()}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), time.Second)
	defer cancel()
	h, err := r.DB.BeginPlayback(ctx, p)
	if err != nil {
		r.Log.Warn("保存播放日志失败", "stage", "begin", "err", err)
		return request, nil
	}
	t := &Trace{recorder: r, handle: h, ctx: request.Context(), result: db.PlaybackResult{Track: track, RequestedQuality: quality, Mode: mode, Outcome: "failed"}}
	return request.WithContext(context.WithValue(request.Context(), traceKey{}, t)), t
}

type Trace struct {
	mu          sync.Mutex
	recorder    *Recorder
	handle      db.PlaybackHandle
	ctx         context.Context
	result      db.PlaybackResult
	events      []db.PlaybackEvent
	sourceNames map[int64]string
	closed      bool
}

func (t *Trace) Identify(track db.PlaybackTrack) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.result.Track = cleanTrack(track)
}

func (t *Trace) Selected(platform, trackID string, sourceID int64, sourceName, quality, delivery string, cached bool) {
	if t == nil {
		return
	}
	sourceName = t.SourceName(sourceID, sourceName)
	t.mu.Lock()
	t.result.Platform, t.result.ResolvedTrackID = platform, trackID
	t.result.SourceID, t.result.SourceName = sourceID, Text(sourceName, 128)
	t.result.Quality, t.result.Delivery, t.result.Cached = quality, delivery, cached
	t.mu.Unlock()
	t.checkpoint()
}

// SourceName 保存当次调用时的音源管理名称，使日志能与管理员上传的脚本对应。
func (t *Trace) SourceName(id int64, fallback string) string {
	if t == nil || id <= 0 {
		return fallback
	}
	t.mu.Lock()
	name, exists := t.sourceNames[id]
	t.mu.Unlock()
	if exists {
		return name
	}
	ctx, cancel := context.WithTimeout(t.ctx, 100*time.Millisecond)
	defer cancel()
	if label, err := t.recorder.DB.PlaybackSourceName(ctx, id); err == nil {
		fallback = label
	}
	name = Text(fallback, 128)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sourceNames == nil {
		t.sourceNames = map[int64]string{}
	}
	if previous, exists := t.sourceNames[id]; exists {
		return previous
	}
	t.sourceNames[id] = name
	return name
}

func (t *Trace) Outcome(outcome string, status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.result.Outcome, t.result.Status = outcome, status
}

func (t *Trace) Delivery(delivery string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.result.Delivery = delivery
	t.mu.Unlock()
	t.checkpoint()
}

// 长时间代理传输中也能查看已选择的音源与取链明细，终态由 Finish 更新。
func (t *Trace) checkpoint() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	result, events := t.result, append([]db.PlaybackEvent{}, t.events...)
	result.Outcome = "pending"
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := t.recorder.DB.CompletePlayback(ctx, t.handle, result, events, t.recorder.now()); err != nil {
		t.recorder.Log.Warn("保存播放日志失败", "stage", "progress", "err", err)
	}
}

func (t *Trace) Response(status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.result.Status = status
	if status >= http.StatusBadRequest {
		t.result.Outcome = "failed"
	}
}

func (t *Trace) Add(e db.PlaybackEvent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || len(t.events) >= db.PlaybackDetailLimit {
		return
	}
	if e.At == 0 {
		e.At = t.recorder.now().UnixMilli()
	}
	e.SourceName = Text(e.SourceName, 128)
	t.events = append(t.events, e)
}

func (t *Trace) Finish() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	result, events := t.result, append([]db.PlaybackEvent{}, t.events...)
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := t.recorder.DB.CompletePlayback(ctx, t.handle, result, events, t.recorder.now()); err != nil {
		t.recorder.Log.Warn("保存播放日志失败", "stage", "complete", "err", err)
	}
}

func (r *Recorder) Maintain(ctx context.Context) {
	if r == nil {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		for {
			cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			n, err := r.DB.CleanupPlayback(cleanupCtx, r.now())
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					r.Log.Warn("清理过期播放日志失败", "err", err)
				}
				break
			}
			if n < 1000 || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
