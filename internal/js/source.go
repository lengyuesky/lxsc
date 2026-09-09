package js

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// 平台与音质常量（与桌面版 preload 保持一致）
var (
	AllPlatforms    = []string{"kw", "kg", "tx", "wy", "mg"}
	AllQualities    = []string{"128k", "320k", "flac", "flac24bit"}
	supportActions  = []string{"musicUrl", "lyric", "pic"}
	qualityRank     = map[string]int{"128k": 1, "320k": 2, "flac": 3, "flac24bit": 4, "hires": 4}
	ErrNoSource     = errors.New("没有可用的音源支持该平台")
	ErrNoQuality    = errors.New("音源不支持所请求的音质")
	ErrScriptFailed = errors.New("音源脚本返回失败")
)

// ScriptMeta 脚本头注释元信息
type ScriptMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      string `json:"author"`
	Homepage    string `json:"homepage"`
}

var metaRe = regexp.MustCompile(`(?m)^\s*(?:\*|//)?\s*@(name|description|version|author|homepage)\s+(.+?)\s*$`)

// ParseScriptMeta 解析脚本头部 @name 等元信息
func ParseScriptMeta(script string) ScriptMeta {
	head := script
	if len(head) > 4000 {
		head = head[:4000]
	}
	var m ScriptMeta
	for _, mt := range metaRe.FindAllStringSubmatch(head, -1) {
		v := strings.TrimSpace(mt[2])
		switch mt[1] {
		case "name":
			if m.Name == "" {
				m.Name = v
			}
		case "description":
			if m.Description == "" {
				m.Description = v
			}
		case "version":
			if m.Version == "" {
				m.Version = v
			}
		case "author":
			if m.Author == "" {
				m.Author = v
			}
		case "homepage":
			if m.Homepage == "" {
				m.Homepage = v
			}
		}
	}
	return m
}

// PlatformCap 脚本对某平台的能力
type PlatformCap struct {
	Type     string   `json:"type"`
	Actions  []string `json:"actions"`
	Qualitys []string `json:"qualitys"`
}

// SourceStatus 对外展示的音源状态
type SourceStatus struct {
	ID        int64                  `json:"id"`
	Name      string                 `json:"name"`
	Version   string                 `json:"version"`
	Priority  int                    `json:"priority"`
	State     string                 `json:"state"` // loading / ready / error
	Error     string                 `json:"error,omitempty"`
	Platforms map[string]PlatformCap `json:"platforms"`
	LoadedAt  time.Time              `json:"loadedAt"`
	Alert     string                 `json:"alert,omitempty"`
	Logs      []string               `json:"logs,omitempty"`
}

// loadedSource 已加载的脚本实例
type loadedSource struct {
	id        int64
	meta      ScriptMeta
	priority  int
	worker    *Worker
	platforms map[string]PlatformCap
	state     string
	err       string
	alert     string
	loadedAt  time.Time
	logs      *ringLog
}

// SourceManager 管理所有音源脚本
type SourceManager struct {
	mu       sync.RWMutex
	loaded   map[int64]*loadedSource
	prelude  string
	http     *http.Client
	httpIns  *http.Client
	log      *slog.Logger
	CallTime time.Duration
}

// NewSourceManager 创建管理器
func NewSourceManager(prelude string, secure, insecure *http.Client, log *slog.Logger) *SourceManager {
	return &SourceManager{loaded: map[int64]*loadedSource{}, prelude: prelude, http: secure, httpIns: insecure, log: log, CallTime: 20 * time.Second}
}

// Load 加载（或重载）一个脚本；返回其能力
func (m *SourceManager) Load(ctx context.Context, id int64, priority int, script string) (*SourceStatus, error) {
	m.Unload(id)
	meta := ParseScriptMeta(script)
	if meta.Name == "" {
		meta.Name = fmt.Sprintf("音源#%d", id)
	}
	ls := &loadedSource{id: id, meta: meta, priority: priority, platforms: map[string]PlatformCap{}, state: "loading", loadedAt: time.Now(), logs: newRingLog(200)}
	inited := make(chan string, 1)
	w, err := New(Options{
		Name:         fmt.Sprintf("source-%d", id),
		Logger:       m.log,
		HTTP:         m.http,
		HTTPInsecure: m.httpIns,
		Prelude:      m.prelude,
		OnInited: func(p string) {
			select {
			case inited <- p:
			default:
			}
		},
		OnUpdateAlert: func(p string) {
			var d struct {
				Log     string `json:"log"`
				Updated string `json:"updateUrl"`
			}
			_ = json.Unmarshal([]byte(p), &d)
			ls.alert = p
			m.log.Warn("音源提示更新", "source", meta.Name, "info", p)
		},
		OnConsole: func(level, msg string) { ls.logs.add(fmt.Sprintf("[%s] %s", level, msg)) },
	})
	if err != nil {
		return nil, err
	}
	ls.worker = w
	m.mu.Lock()
	m.loaded[id] = ls
	m.mu.Unlock()

	fail := func(e error) (*SourceStatus, error) {
		ls.state = "error"
		ls.err = e.Error()
		m.log.Error("音源加载失败", "source", meta.Name, "err", e)
		return m.statusOf(ls), e
	}

	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := w.CallJSON(loadCtx, "__lx_setScriptInfo", map[string]any{
		"name": meta.Name, "description": meta.Description, "version": meta.Version, "author": meta.Author, "homepage": meta.Homepage, "rawScript": script,
	}); err != nil {
		return fail(err)
	}
	if err := w.RunScript(loadCtx, fmt.Sprintf("source_%d.js", id), script); err != nil {
		return fail(fmt.Errorf("脚本执行出错: %w", err))
	}
	select {
	case p := <-inited:
		var d struct {
			Sources map[string]json.RawMessage `json:"sources"`
		}
		if err := json.Unmarshal([]byte(p), &d); err != nil || d.Sources == nil {
			return fail(errors.New("inited 数据缺少 sources"))
		}
		for _, pf := range append(append([]string{}, AllPlatforms...), "local") {
			raw, ok := d.Sources[pf]
			if !ok {
				continue
			}
			cap, ok := parsePlatformCap(raw)
			if !ok {
				continue
			}
			ls.platforms[pf] = cap
		}
		if len(ls.platforms) == 0 {
			return fail(errors.New("脚本未声明任何受支持的平台"))
		}
	case <-time.After(3 * time.Second):
		return fail(errors.New("初始化超时，请确保脚本调用了 lx.send('inited', ...)"))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	ls.state = "ready"
	m.log.Info("音源已加载", "source", meta.Name, "version", meta.Version, "platforms", platformKeys(ls.platforms))
	return m.statusOf(ls), nil
}

func parsePlatformCap(raw json.RawMessage) (PlatformCap, bool) {
	var obj struct {
		Type     string   `json:"type"`
		Actions  []string `json:"actions"`
		Qualitys []string `json:"qualitys"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && (obj.Type == "music" || obj.Type == "") {
		cap := PlatformCap{Type: "music"}
		if len(obj.Actions) == 0 {
			cap.Actions = []string{"musicUrl"}
		} else {
			for _, a := range obj.Actions {
				for _, s := range supportActions {
					if a == s {
						cap.Actions = append(cap.Actions, a)
					}
				}
			}
		}
		for _, q := range obj.Qualitys {
			if _, ok := qualityRank[q]; ok {
				cap.Qualitys = append(cap.Qualitys, q)
			}
		}
		if len(cap.Qualitys) == 0 {
			cap.Qualitys = []string{"128k", "320k"}
		}
		return cap, true
	}
	// 兼容 sources: { kw: ['128k','320k'] } 形式
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		cap := PlatformCap{Type: "music", Actions: []string{"musicUrl"}}
		for _, q := range arr {
			if _, ok := qualityRank[q]; ok {
				cap.Qualitys = append(cap.Qualitys, q)
			}
		}
		if len(cap.Qualitys) == 0 {
			cap.Qualitys = []string{"128k", "320k"}
		}
		return cap, true
	}
	return PlatformCap{}, false
}

func platformKeys(m map[string]PlatformCap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Unload 卸载脚本
func (m *SourceManager) Unload(id int64) {
	m.mu.Lock()
	ls, ok := m.loaded[id]
	if ok {
		delete(m.loaded, id)
	}
	m.mu.Unlock()
	if ok && ls.worker != nil {
		ls.worker.Stop()
	}
}

// UnloadAll 卸载全部
func (m *SourceManager) UnloadAll() {
	m.mu.Lock()
	all := m.loaded
	m.loaded = map[int64]*loadedSource{}
	m.mu.Unlock()
	for _, ls := range all {
		if ls.worker != nil {
			ls.worker.Stop()
		}
	}
}

// SetPriority 调整优先级
func (m *SourceManager) SetPriority(id int64, priority int) {
	m.mu.Lock()
	if ls, ok := m.loaded[id]; ok {
		ls.priority = priority
	}
	m.mu.Unlock()
}

func (m *SourceManager) statusOf(ls *loadedSource) *SourceStatus {
	return &SourceStatus{ID: ls.id, Name: ls.meta.Name, Version: ls.meta.Version, Priority: ls.priority, State: ls.state, Error: ls.err, Platforms: ls.platforms, LoadedAt: ls.loadedAt, Alert: ls.alert, Logs: ls.logs.list()}
}

// Status 所有已加载音源状态
func (m *SourceManager) Status() []*SourceStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*SourceStatus, 0, len(m.loaded))
	for _, ls := range m.loaded {
		out = append(out, m.statusOf(ls))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// StatusOf 单个音源状态
func (m *SourceManager) StatusOf(id int64) *SourceStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ls, ok := m.loaded[id]; ok {
		return m.statusOf(ls)
	}
	return nil
}

// candidates 返回支持 platform/action 的已就绪脚本，按优先级排序
func (m *SourceManager) candidates(platform, action string) []*loadedSource {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*loadedSource
	for _, ls := range m.loaded {
		if ls.state != "ready" {
			continue
		}
		cap, ok := ls.platforms[platform]
		if !ok {
			continue
		}
		for _, a := range cap.Actions {
			if a == action {
				out = append(out, ls)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].priority != out[j].priority {
			return out[i].priority < out[j].priority
		}
		return out[i].id < out[j].id
	})
	return out
}

// SupportedPlatforms 当前所有就绪脚本支持的平台合集
func (m *SourceManager) SupportedPlatforms() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := map[string]bool{}
	for _, ls := range m.loaded {
		if ls.state != "ready" {
			continue
		}
		for p := range ls.platforms {
			set[p] = true
		}
	}
	var out []string
	for _, p := range AllPlatforms {
		if set[p] {
			out = append(out, p)
		}
	}
	return out
}

// MusicURLResult 取直链结果
type MusicURLResult struct {
	URL      string `json:"url"`
	Quality  string `json:"type"`
	Source   string `json:"source"` // 使用的脚本名
	SourceID int64  `json:"-"`      // 由管理器赋值，脚本输出和对外 JSON 均不能携带该身份
}

// MusicURLSourceIDs 按当前优先级返回支持取链的真实脚本 ID，不使用平台代号或脚本名代替。
func (m *SourceManager) MusicURLSourceIDs(platform string) []int64 {
	ids := []int64{}
	for _, source := range m.candidates(platform, "musicUrl") {
		ids = append(ids, source.id)
	}
	return ids
}

// MusicURL 依次尝试各音源脚本获取直链；quality 不被支持时向下降级。
func (m *SourceManager) MusicURL(ctx context.Context, platform string, musicInfo any, quality string) (*MusicURLResult, error) {
	return m.MusicURLForSources(ctx, platform, musicInfo, quality, nil)
}

// MusicURLForSources 仅在给定脚本集合内按原优先级/音质降级取链；nil 表示不限制。
// 此处不校验音频响应，播放层独立决定是否刷新或换源，调试解析不会隐式探测媒体。
func (m *SourceManager) MusicURLForSources(ctx context.Context, platform string, musicInfo any, quality string, sourceIDs []int64) (*MusicURLResult, error) {
	var lastErr error = ErrNoSource
	for _, ls := range m.candidates(platform, "musicUrl") {
		if sourceIDs != nil && !slices.Contains(sourceIDs, ls.id) {
			continue
		}
		lastErr = ErrScriptFailed
		for _, q := range downgradeChain(quality, ls.platforms[platform].Qualitys) {
			cctx, cancel := context.WithTimeout(ctx, m.CallTime)
			out, err := ls.worker.CallJSON(cctx, "__lx_request", map[string]any{
				"source": platform, "action": "musicUrl",
				"info": map[string]any{"type": q, "musicInfo": musicInfo},
			})
			cancel()
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", ls.meta.Name, err)
				// 脚本错误可能包含签名地址，普通取链日志也不输出错误原文。
				m.log.Debug("取直链失败", "source", ls.meta.Name, "platform", platform, "quality", q)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			var r MusicURLResult
			if err := json.Unmarshal(out, &r); err != nil || r.URL == "" {
				lastErr = fmt.Errorf("%s: 返回数据无效", ls.meta.Name)
				continue
			}
			r.Quality = q
			r.Source = ls.meta.Name
			r.SourceID = ls.id
			return &r, nil
		}
	}
	return nil, lastErr
}

// LyricResult 歌词结果
type LyricResult struct {
	Lyric   string `json:"lyric"`
	TLyric  string `json:"tlyric"`
	RLyric  string `json:"rlyric"`
	LxLyric string `json:"lxlyric"`
}

// Lyric 通过脚本获取歌词（若脚本支持）
func (m *SourceManager) Lyric(ctx context.Context, platform string, musicInfo any) (*LyricResult, error) {
	cands := m.candidates(platform, "lyric")
	if len(cands) == 0 {
		return nil, ErrNoSource
	}
	var lastErr error = ErrScriptFailed
	for _, ls := range cands {
		cctx, cancel := context.WithTimeout(ctx, m.CallTime)
		out, err := ls.worker.CallJSON(cctx, "__lx_request", map[string]any{"source": platform, "action": "lyric", "info": map[string]any{"musicInfo": musicInfo}})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		var r LyricResult
		if err := json.Unmarshal(out, &r); err != nil || r.Lyric == "" {
			lastErr = errors.New("歌词数据无效")
			continue
		}
		return &r, nil
	}
	return nil, lastErr
}

// Pic 通过脚本获取封面
func (m *SourceManager) Pic(ctx context.Context, platform string, musicInfo any) (string, error) {
	cands := m.candidates(platform, "pic")
	if len(cands) == 0 {
		return "", ErrNoSource
	}
	var lastErr error = ErrScriptFailed
	for _, ls := range cands {
		cctx, cancel := context.WithTimeout(ctx, m.CallTime)
		out, err := ls.worker.CallJSON(cctx, "__lx_request", map[string]any{"source": platform, "action": "pic", "info": map[string]any{"musicInfo": musicInfo}})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		var s string
		if err := json.Unmarshal(out, &s); err != nil || s == "" {
			lastErr = errors.New("封面数据无效")
			continue
		}
		return s, nil
	}
	return "", lastErr
}

// downgradeChain 从请求音质开始，按脚本支持列表向下降级
func downgradeChain(want string, supported []string) []string {
	rank := qualityRank[want]
	if rank == 0 {
		rank = qualityRank["320k"]
	}
	set := map[string]bool{}
	for _, q := range supported {
		set[q] = true
	}
	var out []string
	for _, q := range []string{"flac24bit", "flac", "320k", "128k"} {
		if qualityRank[q] <= rank && set[q] {
			out = append(out, q)
		}
	}
	if len(out) == 0 && set["128k"] {
		out = append(out, "128k")
	}
	return out
}

// ringLog 简单环形日志
type ringLog struct {
	mu   sync.Mutex
	buf  []string
	size int
}

func newRingLog(n int) *ringLog { return &ringLog{size: n} }

func (r *ringLog) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(s) > 2000 {
		s = s[:2000] + "…"
	}
	r.buf = append(r.buf, time.Now().Format("15:04:05 ")+s)
	if len(r.buf) > r.size {
		r.buf = r.buf[len(r.buf)-r.size:]
	}
}

func (r *ringLog) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.buf...)
}
