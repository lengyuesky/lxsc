// Package admin 提供管理页面与 /api/admin 接口
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/backup"
	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/logbuf"
	"lxsc/internal/music"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

// Server 管理服务
type Server struct {
	DB       *db.DB
	Sources  *js.SourceManager
	Catalog  *music.Catalog
	Settings *settings.Store
	Secret   *secret.Box
	Logs     *logbuf.Buffer
	Log      *slog.Logger
	HTTP     *http.Client
	Version  string
	StartAt  time.Time
	Auth     *webauth.Manager
	Backup   *backup.Service
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// Routes 挂载
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requireAdmin)
	r.Get("/status", s.status)
	r.Get("/logs", s.logs)
	r.Get("/backups/status", s.backupStatus)
	r.Post("/backups/export", s.exportBackup)
	r.Post("/backups/inspect", s.inspectBackup)
	r.Post("/backups/import", s.importBackup)
	r.Delete("/backups/pending", s.cancelPendingBackup)
	r.Get("/backups/webdav/config", s.getWebDAVConfig)
	r.Put("/backups/webdav/config", s.putWebDAVConfig)
	r.Post("/backups/webdav/test", s.testWebDAV)
	r.Get("/backups/webdav/files", s.listWebDAVFiles)
	r.Post("/backups/webdav/run", s.runWebDAVBackup)
	r.Get("/backups/webdav/files/{name}", s.downloadWebDAVFile)
	r.Post("/backups/webdav/files/{name}/restore", s.restoreWebDAVFile)
	r.Delete("/backups/webdav/files/{name}", s.deleteWebDAVFile)
	r.Get("/settings", s.getSettings)
	r.Put("/settings", s.putSettings)
	r.Post("/metadata/cleanup", s.cleanupMetadata)
	r.Get("/users", s.listUsers)
	r.Post("/users", s.createUser)
	r.Put("/users/{id}", s.updateUser)
	r.Delete("/users/{id}", s.deleteUser)
	r.Post("/users/{id}/apikey", s.createAPIKey)
	r.Get("/sources", s.listSources)
	r.Post("/sources", s.createSource)
	r.Post("/sources/import", s.importSource)
	r.Put("/sources/{id}", s.updateSource)
	r.Delete("/sources/{id}", s.deleteSource)
	r.Post("/sources/{id}/reload", s.reloadSource)
	r.Post("/sources/{id}/test", s.testSource)
	r.Get("/sources/{id}/script", s.sourceScript)
	r.Post("/search", s.searchTest)
	return r
}

// ---------- 认证 ----------

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := s.Auth.User(r)
		if u == nil {
			fail(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		if !u.IsAdmin {
			fail(w, http.StatusForbidden, "仅管理员可访问")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, u)))
	})
}

type ctxUser struct{}

// ---------- 状态 ----------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	users, _ := s.DB.CountUsers(r.Context())
	stats, _ := s.DB.Statistics(r.Context())
	allSources, _ := s.DB.ListSources(r.Context())
	health := map[string]int{"total": len(allSources)}
	for _, source := range allSources {
		if !source.Enabled {
			health["disabled"]++
			continue
		}
		status := s.Sources.StatusOf(source.ID)
		if status != nil && status.State == "ready" {
			health["ready"]++
		} else {
			health["error"]++
		}
	}
	writeJSON(w, 200, map[string]any{
		"version":      s.Version,
		"uptime":       int(time.Since(s.StartAt).Seconds()),
		"goroutines":   runtime.NumGoroutine(),
		"memMB":        float64(ms.Alloc) / 1024 / 1024,
		"sysMB":        float64(ms.Sys) / 1024 / 1024,
		"users":        users,
		"sources":      s.Sources.Status(),
		"platforms":    s.Sources.SupportedPlatforms(),
		"data":         stats,
		"sourceHealth": health,
		"backup":       s.Backup.Status(),
	})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 200
	}
	writeJSON(w, 200, s.Logs.List(n))
}

// ---------- 设置 ----------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Settings.Get())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	v, err := s.Settings.Update(r.Context(), patch)
	if err != nil {
		if errors.Is(err, settings.ErrInvalidSetting) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		fail(w, 500, err.Error())
		return
	}
	s.Catalog.RefreshTTL()
	writeJSON(w, 200, v)
}

func (s *Server) cleanupMetadata(w http.ResponseWriter, r *http.Request) {
	result, err := s.DB.CleanupUnreferencedMetadata(r.Context())
	if err != nil {
		fail(w, 500, "清理元数据失败: "+err.Error())
		return
	}
	s.Catalog.PurgeMetadataCaches()
	if err := s.DB.Compact(r.Context()); err != nil {
		s.Log.Warn("压缩数据库失败", "err", err)
		writeJSON(w, 200, map[string]any{"cleanup": result, "warning": "元数据已清理，但数据库空间暂未完全回收: " + err.Error()})
		return
	}
	s.Log.Info("已清理无引用元数据", "tracks", result.Tracks, "albums", result.Albums, "artists", result.Artists)
	writeJSON(w, 200, map[string]any{"cleanup": result})
}

// ---------- 用户 ----------

type userBody struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"isAdmin"`
	Quality  string `json:"quality"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.DB.ListUsers(r.Context())
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []*db.User{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var b userBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || strings.TrimSpace(b.Name) == "" || b.Password == "" {
		fail(w, 400, "用户名与密码不能为空")
		return
	}
	enc, err := s.Secret.Encrypt(b.Password)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	u, err := s.DB.CreateUser(r.Context(), strings.TrimSpace(b.Name), enc, b.IsAdmin, b.Quality)
	if err != nil {
		fail(w, 400, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, 200, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var b userBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	cur, err := s.DB.GetUserByID(r.Context(), id)
	if err != nil {
		fail(w, 404, "用户不存在")
		return
	}
	enc := ""
	if b.Password != "" {
		enc, _ = s.Secret.Encrypt(b.Password)
	}
	if b.Name == "" {
		b.Name = cur.Name
	}
	if b.Quality == "" {
		b.Quality = cur.Quality
	}
	me := r.Context().Value(ctxUser{}).(*db.User)
	if me.ID == id && !b.IsAdmin {
		fail(w, 400, "不能取消自己的管理员权限")
		return
	}
	if err := s.DB.UpdateUser(r.Context(), id, b.Name, enc, b.IsAdmin, b.Quality); err != nil {
		fail(w, 400, err.Error())
		return
	}
	u, _ := s.DB.GetUserByID(r.Context(), id)
	writeJSON(w, 200, u)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	me := r.Context().Value(ctxUser{}).(*db.User)
	if me.ID == id {
		fail(w, 400, "不能删除自己")
		return
	}
	if err := s.DB.DeleteUser(r.Context(), id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	key := "lxsc_" + secret.RandomToken(24)
	if err := s.DB.CreateAPIKey(r.Context(), id, key, "admin"); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"apiKey": key})
}

// ---------- 音源 ----------

type sourceView struct {
	*db.Source
	Status *js.SourceStatus `json:"status"`
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.DB.ListSources(r.Context())
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := make([]sourceView, 0, len(list))
	for _, src := range list {
		out = append(out, sourceView{Source: src, Status: s.Sources.StatusOf(src.ID)})
	}
	writeJSON(w, 200, out)
}

func (s *Server) sourceScript(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	src, err := s.DB.GetSource(r.Context(), id)
	if err != nil {
		fail(w, 404, "不存在")
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.js\"", src.Name))
	_, _ = w.Write([]byte(src.Script))
}

// readScript 从 multipart 或 JSON 读取脚本内容
func readScript(r *http.Request) (script, name string, priority int, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err = r.ParseMultipartForm(8 << 20); err != nil {
			return
		}
		f, _, e := r.FormFile("file")
		if e != nil {
			err = e
			return
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 8<<20))
		if e != nil {
			err = e
			return
		}
		script = string(b)
		name = r.FormValue("name")
		priority, _ = strconv.Atoi(r.FormValue("priority"))
		return
	}
	var body struct {
		Script   string `json:"script"`
		Name     string `json:"name"`
		Priority int    `json:"priority"`
	}
	if err = json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		return
	}
	return body.Script, body.Name, body.Priority, nil
}

// AddSource 新增并加载音源（供导入与上传共用）
func (s *Server) AddSource(ctx context.Context, script, name string, priority int) (*db.Source, *js.SourceStatus, error) {
	script = strings.TrimPrefix(script, "\uFEFF")
	if strings.TrimSpace(script) == "" {
		return nil, nil, errors.New("脚本为空")
	}
	meta := js.ParseScriptMeta(script)
	if name == "" {
		name = meta.Name
	}
	if name == "" {
		name = "未命名音源"
	}
	if priority <= 0 {
		priority = 100
	}
	src, err := s.DB.CreateSource(ctx, &db.Source{Name: name, Description: meta.Description, Version: meta.Version, Author: meta.Author, Homepage: meta.Homepage, Script: script, Enabled: true, Priority: priority})
	if err != nil {
		return nil, nil, err
	}
	defer s.Catalog.InvalidateURLs()
	st, lerr := s.Sources.Load(ctx, src.ID, src.Priority, script)
	if lerr != nil {
		s.Log.Warn("音源加载失败（已保存，可稍后重试）", "name", name, "err", lerr)
	}
	return src, st, nil
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	script, name, priority, err := readScript(r)
	if err != nil {
		fail(w, 400, "读取脚本失败: "+err.Error())
		return
	}
	src, st, err := s.AddSource(r.Context(), script, name, priority)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, sourceView{Source: src, Status: st})
}

func (s *Server) importSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL      string `json:"url"`
		Name     string `json:"name"`
		Priority int    `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.HasPrefix(body.URL, "http") {
		fail(w, 400, "URL 无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, body.URL, nil)
	req.Header.Set("User-Agent", "lxsc")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		fail(w, 400, "下载失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail(w, 400, "下载失败: "+resp.Status)
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		fail(w, 400, "下载失败: "+err.Error())
		return
	}
	src, st, err := s.AddSource(r.Context(), string(b), body.Name, body.Priority)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, sourceView{Source: src, Status: st})
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	src, err := s.DB.GetSource(r.Context(), id)
	if err != nil {
		fail(w, 404, "不存在")
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Enabled  *bool   `json:"enabled"`
		Priority *int    `json:"priority"`
		Script   *string `json:"script"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	upd := &db.Source{ID: id, Name: src.Name, Enabled: src.Enabled, Priority: src.Priority}
	if body.Name != nil && *body.Name != "" {
		upd.Name = *body.Name
	}
	if body.Enabled != nil {
		upd.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		upd.Priority = *body.Priority
	}
	if body.Script != nil && strings.TrimSpace(*body.Script) != "" {
		upd.Script = *body.Script
		meta := js.ParseScriptMeta(upd.Script)
		upd.Description, upd.Version, upd.Author, upd.Homepage = meta.Description, meta.Version, meta.Author, meta.Homepage
	}
	if err := s.DB.UpdateSource(r.Context(), upd); err != nil {
		fail(w, 500, err.Error())
		return
	}
	invalidateURLs := upd.Enabled != src.Enabled || upd.Priority != src.Priority || upd.Script != ""
	defer func() {
		if invalidateURLs {
			s.Catalog.InvalidateURLs()
		}
	}()
	src, err = s.DB.GetSource(r.Context(), id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	var st *js.SourceStatus
	if src.Enabled {
		if body.Script != nil || s.Sources.StatusOf(id) == nil {
			invalidateURLs = true
			st, _ = s.Sources.Load(r.Context(), id, src.Priority, src.Script)
		} else {
			s.Sources.SetPriority(id, src.Priority)
			st = s.Sources.StatusOf(id)
		}
	} else {
		if s.Sources.StatusOf(id) != nil {
			invalidateURLs = true
		}
		s.Sources.Unload(id)
	}
	writeJSON(w, 200, sourceView{Source: src, Status: st})
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	defer s.Catalog.InvalidateURLs()
	s.Sources.Unload(id)
	if err := s.DB.DeleteSource(r.Context(), id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) reloadSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	src, err := s.DB.GetSource(r.Context(), id)
	if err != nil {
		fail(w, 404, "不存在")
		return
	}
	defer s.Catalog.InvalidateURLs()
	st, lerr := s.Sources.Load(r.Context(), id, src.Priority, src.Script)
	out := map[string]any{"status": st}
	if lerr != nil {
		out["error"] = lerr.Error()
	}
	writeJSON(w, 200, out)
}

// testSource 用一首示例歌曲测试取直链
func (s *Server) testSource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	st := s.Sources.StatusOf(id)
	if st == nil || st.State != "ready" {
		fail(w, 400, "音源未就绪")
		return
	}
	var body struct {
		Platform string `json:"platform"`
		Query    string `json:"query"`
		Quality  string `json:"quality"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Query == "" {
		body.Query = "晴天 周杰伦"
	}
	if body.Quality == "" {
		body.Quality = "128k"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	results := map[string]any{}
	platforms := []string{body.Platform}
	if body.Platform == "" {
		platforms = nil
		for p := range st.Platforms {
			if music.IsPlatform(p) {
				platforms = append(platforms, p)
			}
		}
	}
	for _, p := range platforms {
		infos := s.Catalog.Search(ctx, body.Query, music.SearchOptions{Sources: []string{p}, Limit: 3})
		if len(infos) == 0 {
			results[p] = map[string]any{"ok": false, "error": "搜索无结果"}
			continue
		}
		in := infos[0]
		t := time.Now()
		res, err := s.Sources.MusicURL(ctx, p, s.Catalog.ScriptInfo(in), body.Quality)
		if err != nil {
			results[p] = map[string]any{"ok": false, "song": in.Name() + " - " + in.Singer(), "error": err.Error(), "ms": time.Since(t).Milliseconds()}
			continue
		}
		results[p] = map[string]any{"ok": true, "song": in.Name() + " - " + in.Singer(), "url": res.URL, "quality": res.Quality, "ms": time.Since(t).Milliseconds()}
	}
	writeJSON(w, 200, results)
}

func (s *Server) searchTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query   string   `json:"query"`
		Sources []string `json:"sources"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	infos := s.Catalog.Search(ctx, body.Query, music.SearchOptions{Sources: body.Sources, Limit: 10})
	out := make([]map[string]any, 0, len(infos))
	for _, in := range infos {
		out = append(out, map[string]any{"id": in.TrackID(), "name": in.Name(), "singer": in.Singer(), "album": in.Album(), "source": in.Source(), "qualities": in.Qualities(), "img": in.Img()})
	}
	writeJSON(w, 200, out)
}
