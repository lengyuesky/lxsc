package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/db"
	"lxsc/internal/js"
)

const maxSourceScriptBytes = 8 << 20

var (
	sourceUpdateLocation     = time.FixedZone("Asia/Shanghai", 8*60*60)
	sourceRetryDelays        = []time.Duration{5 * time.Minute, 30 * time.Minute}
	errSourceUpdateBusy      = errors.New("该音源正在检查更新")
	errSourceUpdateCapacity  = errors.New("更新任务繁忙，请稍后重试")
	errSourceUpdateStopped   = errors.New("音源更新服务已停止")
	errSourceUpdateCancelled = errors.New("音源设置已变化或请求已取消，本次更新未继续")
	errSourceURLMissing      = errors.New("请先填写来源 URL")
)

type sourceUpdateControl struct {
	mu      sync.Mutex
	running atomic.Bool
	cancel  context.CancelFunc
}

type sourceUpdateRuntime struct {
	mu              sync.Mutex
	controls        map[int64]*sourceUpdateControl
	slots           chan struct{}
	root            context.Context
	cancel          context.CancelFunc
	workers         sync.WaitGroup
	started, closed bool
	done            chan struct{}
	now             func() time.Time
}

func (s *Server) sourceUpdates() *sourceUpdateRuntime {
	s.sourceUpdatesOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.sourceUpdatesRuntime = &sourceUpdateRuntime{controls: map[int64]*sourceUpdateControl{}, slots: make(chan struct{}, 2), root: ctx, cancel: cancel, now: time.Now}
	})
	return s.sourceUpdatesRuntime
}

func (runtime *sourceUpdateRuntime) control(id int64) *sourceUpdateControl {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	control := runtime.controls[id]
	if control == nil {
		control = &sourceUpdateControl{}
		runtime.controls[id] = control
	}
	return control
}

func (s *Server) lockSourceControl(id int64) *sourceUpdateControl {
	control := s.sourceUpdates().control(id)
	control.mu.Lock()
	if control.cancel != nil {
		control.cancel()
	}
	return control
}

// sourcePlanSlot 返回最近一次北京时间 03:00 的日期及下一次检查时间。
func sourcePlanSlot(now time.Time) (string, time.Time) {
	local := now.In(sourceUpdateLocation)
	due := time.Date(local.Year(), local.Month(), local.Day(), 3, 0, 0, 0, sourceUpdateLocation)
	if now.Before(due) {
		due = due.AddDate(0, 0, -1)
	}
	return due.Format("2006-01-02"), due.AddDate(0, 0, 1)
}

func sourceCheckDue(source *db.Source, now time.Time) bool {
	if !source.Enabled || !source.AutoUpdate || source.URL == "" {
		return false
	}
	slot, _ := sourcePlanSlot(now)
	return source.UpdateSlot != slot || (source.UpdateResult == "failed" && source.UpdateAttempts < 3 && source.RetryAt <= now.Unix())
}

func sourceNextCheck(source *db.Source, now time.Time) int64 {
	if !source.Enabled || !source.AutoUpdate || source.URL == "" {
		return 0
	}
	if sourceCheckDue(source, now) {
		return now.Unix()
	}
	_, next := sourcePlanSlot(now)
	if source.UpdateResult == "failed" && source.UpdateAttempts < 3 && source.RetryAt > now.Unix() && source.RetryAt < next.Unix() {
		return source.RetryAt
	}
	return next.Unix()
}

func sourceAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	parsed, err := url.Parse(address)
	if err != nil || len(address) > 4096 || parsed == nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.Fragment != "" {
		return "", errors.New("来源 URL 无效，仅支持完整的 HTTP(S) 地址")
	}
	return address, nil
}

type sourceDownloadError struct {
	message string
	cause   error
}

func (e *sourceDownloadError) Error() string { return e.message }
func (e *sourceDownloadError) Unwrap() error { return e.cause }

func (s *Server) downloadSource(parent context.Context, address string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", &sourceDownloadError{"来源 URL 无效", err}
	}
	req.Header.Set("User-Agent", "lxsc")
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		message := "下载失败：连接或响应异常"
		if errors.Is(err, context.DeadlineExceeded) {
			message = "下载音源脚本超时"
		}
		return "", &sourceDownloadError{message, err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败：HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSourceScriptBytes+1))
	if err != nil {
		return "", &sourceDownloadError{"读取音源脚本失败", err}
	}
	if len(body) > maxSourceScriptBytes {
		return "", errors.New("音源脚本超过 8 MiB")
	}
	script := strings.TrimPrefix(string(body), "\uFEFF")
	if strings.TrimSpace(script) == "" {
		return "", errors.New("下载的音源脚本为空")
	}
	return script, nil
}

func (s *Server) StartSourceUpdates(parent context.Context) func() {
	runtime := s.sourceUpdates()
	runtime.mu.Lock()
	if runtime.started || runtime.closed {
		runtime.mu.Unlock()
		return s.StopSourceUpdates
	}
	runtime.started, runtime.done = true, make(chan struct{})
	runtime.mu.Unlock()
	stopParent := context.AfterFunc(parent, runtime.cancel)
	go func() {
		defer close(runtime.done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			s.runSourceUpdates(runtime.root)
			select {
			case <-runtime.root.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { stopParent(); s.StopSourceUpdates() }
}

func (s *Server) StopSourceUpdates() {
	runtime := s.sourceUpdates()
	runtime.mu.Lock()
	runtime.closed = true
	runtime.cancel()
	started, done := runtime.started, runtime.done
	runtime.mu.Unlock()
	if started {
		<-done
	}
	runtime.workers.Wait()
}

func (s *Server) runSourceUpdates(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	sources, err := s.DB.ListSourceSummaries(ctx)
	if err != nil {
		return
	}
	runtime := s.sourceUpdates()
	for _, source := range sources {
		if ctx.Err() != nil {
			return
		}
		if sourceCheckDue(source, runtime.now()) && !runtime.control(source.ID).running.Load() {
			select {
			case runtime.slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			// 登记工作发生在调度线程内，Stop 可先等待线程退出再等待工作结束。
			runtime.mu.Lock()
			if runtime.closed || runtime.root.Err() != nil {
				runtime.mu.Unlock()
				<-runtime.slots
				return
			}
			runtime.workers.Add(1)
			runtime.mu.Unlock()
			go func(id int64) {
				defer runtime.workers.Done()
				_, err := s.checkSourceWithSlot(ctx, id, true, true)
				if err != nil && !errors.Is(err, errSourceUpdateCapacity) && !errors.Is(err, errSourceUpdateBusy) && !errors.Is(err, errSourceUpdateCancelled) && !errors.Is(err, db.ErrSourceConflict) && !errors.Is(err, js.ErrSourceChanged) && s.Log != nil {
					s.Log.Warn("音源自动更新失败", "id", id, "error", err.Error())
				}
			}(source.ID)
		}
	}
}

func (s *Server) performSourceCheck(parent context.Context, id int64, scheduled bool) (*db.Source, error) {
	return s.checkSourceWithSlot(parent, id, scheduled, false)
}

func (s *Server) checkSourceWithSlot(parent context.Context, id int64, scheduled, reserved bool) (*db.Source, error) {
	runtime := s.sourceUpdates()
	if !reserved {
		select {
		case runtime.slots <- struct{}{}:
		default:
			return nil, errSourceUpdateCapacity
		}
	}
	defer func() { <-runtime.slots }()
	runtime.mu.Lock()
	if runtime.closed || runtime.root.Err() != nil {
		runtime.mu.Unlock()
		return nil, errSourceUpdateStopped
	}
	runtime.workers.Add(1)
	runtime.mu.Unlock()
	defer runtime.workers.Done()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	stopRoot := context.AfterFunc(runtime.root, cancel)
	defer stopRoot()
	defer cancel()
	control := runtime.control(id)
	control.mu.Lock()
	if control.running.Load() {
		control.mu.Unlock()
		return nil, errSourceUpdateBusy
	}
	control.running.Store(true)
	control.cancel = cancel
	defer func() {
		control.mu.Lock()
		control.cancel = nil
		control.running.Store(false)
		control.mu.Unlock()
	}()
	source, err := s.DB.GetSource(ctx, id)
	control.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if scheduled && !sourceCheckDue(source, runtime.now()) {
		return source, nil
	}
	if source.URL == "" {
		return nil, errSourceURLMissing
	}
	slot, _ := sourcePlanSlot(runtime.now())
	attempt := 1
	if scheduled && source.UpdateSlot == slot {
		attempt = min(3, max(1, source.UpdateAttempts+1))
	}
	script, problem := s.downloadSource(ctx, source.URL)
	var prepared *js.PreparedSource
	changed := problem == nil && script != strings.TrimPrefix(source.Script, "\uFEFF")
	if changed {
		prepared, err = s.Sources.Prepare(ctx, id, script)
		if err != nil {
			problem = errors.New("新脚本执行或初始化失败，旧脚本已保留")
		}
		if prepared != nil {
			defer prepared.Close()
		}
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if ctx.Err() != nil {
		return nil, errSourceUpdateCancelled
	}
	check := db.SourceCheck{Slot: slot, CheckedAt: runtime.now().Unix(), Attempts: attempt, Result: "unchanged"}
	if problem != nil {
		check.Result, check.Error = "failed", problem.Error()
		if source.Enabled && source.AutoUpdate && attempt < 3 {
			check.RetryAt = runtime.now().Add(sourceRetryDelays[attempt-1]).Unix()
		}
	} else if changed {
		meta := js.ParseScriptMeta(script)
		check.Result = "updated"
		check.Script = &db.Source{Script: script, Description: meta.Description, Version: meta.Version, Author: meta.Author, Homepage: meta.Homepage}
	}
	save := func() error { return s.DB.CompleteSourceCheck(ctx, source, check) }
	if prepared != nil && source.Enabled {
		_, err = prepared.Apply(ctx, save)
	} else {
		err = save()
	}
	if err != nil {
		return nil, err
	}
	if changed && problem == nil {
		s.Catalog.InvalidateURLs()
	}
	if problem != nil {
		return nil, problem
	}
	completed := *source
	completed.LastCheckedAt, completed.UpdateResult, completed.UpdateError = check.CheckedAt, check.Result, check.Error
	completed.UpdateSlot, completed.UpdateAttempts, completed.RetryAt = check.Slot, check.Attempts, check.RetryAt
	completed.UpdateRevision++
	if next := check.Script; next != nil {
		completed.Script, completed.Description, completed.Version, completed.Author, completed.Homepage = next.Script, next.Description, next.Version, next.Author, next.Homepage
		completed.ScriptUpdatedAt, completed.UpdatedAt = check.CheckedAt, check.CheckedAt
	}
	return &completed, nil
}

func (s *Server) checkSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	source, err := s.performSourceCheck(r.Context(), id, false)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, db.ErrNotFound) {
			code = http.StatusNotFound
		}
		if errors.Is(err, errSourceURLMissing) {
			code = http.StatusBadRequest
		}
		if errors.Is(err, errSourceUpdateBusy) || errors.Is(err, errSourceUpdateCancelled) || errors.Is(err, db.ErrSourceConflict) || errors.Is(err, js.ErrSourceChanged) {
			code = http.StatusConflict
		}
		if errors.Is(err, errSourceUpdateCapacity) || errors.Is(err, errSourceUpdateStopped) {
			code = http.StatusServiceUnavailable
		}
		fail(w, code, err.Error())
		return
	}
	writeSourceJSON(w, http.StatusOK, s.sourceView(source))
}
