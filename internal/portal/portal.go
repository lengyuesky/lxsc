// Package portal 提供普通用户与管理员使用的歌单管理网页及 JSON API。
package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

const (
	maxJSONBody       = 2 << 20
	maxImportBody     = 40 << 20
	maxPlaylistTracks = db.MaxPlaylistTracks
)

// Server 用户歌单管理服务。
type Server struct {
	DB       *db.DB
	Catalog  *music.Catalog
	Settings *settings.Store
	Secret   *secret.Box
	Log      *slog.Logger
	Auth     *webauth.Manager
	Stream   func(http.ResponseWriter, *http.Request, *db.User)
}

type userContextKey struct{}

type userView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"isAdmin"`
	Quality string `json:"quality"`
}

type playlistView struct {
	ID        string `json:"id"`
	UserID    int64  `json:"userId"`
	Owner     string `json:"owner"`
	Name      string `json:"name"`
	Comment   string `json:"comment"`
	Public    bool   `json:"public"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	SongCount int    `json:"songCount"`
	CanEdit   bool   `json:"canEdit"`
}

type trackView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Singer      string   `json:"singer"`
	Album       string   `json:"album"`
	Source      string   `json:"source"`
	Duration    int      `json:"duration"`
	Qualities   []string `json:"qualities,omitempty"`
	Unavailable bool     `json:"unavailable,omitempty"`
}

type playlistDetail struct {
	playlistView
	Tracks         []trackView `json:"tracks"`
	TracksRevision string      `json:"tracksRevision"`
}

// Routes 返回挂载到 /api/app 的 API 路由。
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requireUser)
	r.Get("/users", s.listUsers)
	r.Put("/profile", s.updateProfile)
	r.Get("/playlists", s.listPlaylists)
	r.Post("/playlists", s.createPlaylist)
	r.Get("/playlists/{id}", s.getPlaylist)
	r.Put("/playlists/{id}", s.updatePlaylist)
	r.Put("/playlists/{id}/tracks", s.replacePlaylistTracks)
	r.Post("/playlists/{id}/tracks", s.addPlaylistTrack)
	r.Post("/playlists/import", s.importPlaylists)
	r.Delete("/playlists/{id}", s.deletePlaylist)
	r.Post("/search", s.search)
	r.Get("/stream", s.stream)
	r.Post("/listening/progress", s.listeningProgress)
	r.Get("/listening/stats", s.listeningStats)
	return r
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("请求体只能包含一个 JSON 对象")
	}
	return nil
}

func toUserView(u *db.User) userView {
	return userView{ID: u.ID, Name: u.Name, IsAdmin: u.IsAdmin, Quality: u.Quality}
}

func currentUser(r *http.Request) *db.User {
	u, _ := r.Context().Value(userContextKey{}).(*db.User)
	return u
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := s.sessionUser(r)
		if u == nil {
			fail(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, u)))
	})
}

func (s *Server) sessionUser(r *http.Request) *db.User {
	return s.Auth.User(r)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.IsAdmin {
		fail(w, http.StatusForbidden, "仅管理员可查看用户列表")
		return
	}
	users, err := s.DB.ListUsers(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]userView, 0, len(users))
	for _, item := range users {
		out = append(out, toUserView(item))
	}
	writeJSON(w, http.StatusOK, out)
}

func validQuality(quality string) bool {
	switch quality {
	case "128k", "320k", "flac", "flac24bit":
		return true
	default:
		return false
	}
}

func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Quality string `json:"quality"`
	}
	if err := decodeJSON(w, r, &body); err != nil || !validQuality(body.Quality) {
		fail(w, http.StatusBadRequest, "不支持的默认音质")
		return
	}
	u := currentUser(r)
	if err := s.DB.UpdateUserQuality(r.Context(), u.ID, body.Quality); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	u.Quality = body.Quality
	writeJSON(w, http.StatusOK, toUserView(u))
}

func canEditPlaylist(u *db.User, p *db.Playlist) bool {
	return u != nil && (u.IsAdmin || p.UserID == u.ID)
}

func canReadPlaylist(u *db.User, p *db.Playlist) bool {
	return canEditPlaylist(u, p) || p.Public
}

func makePlaylistView(u *db.User, p *db.Playlist) playlistView {
	return playlistView{
		ID: p.ID, UserID: p.UserID, Owner: p.Owner, Name: p.Name, Comment: p.Comment,
		Public: p.Public, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		SongCount: p.Count, CanEdit: canEditPlaylist(u, p),
	}
}

func (s *Server) listPlaylists(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var (
		list []*db.Playlist
		err  error
	)
	if u.IsAdmin {
		var ownerID int64
		if raw := strings.TrimSpace(r.URL.Query().Get("ownerId")); raw != "" {
			ownerID, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || ownerID <= 0 {
				fail(w, http.StatusBadRequest, "ownerId 无效")
				return
			}
			if _, err = s.DB.GetUserByID(r.Context(), ownerID); err != nil {
				fail(w, http.StatusBadRequest, "归属用户不存在")
				return
			}
		}
		list, err = s.DB.ListAllPlaylists(r.Context(), ownerID)
	} else {
		list, err = s.DB.ListPlaylists(r.Context(), u.ID)
		sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]playlistView, 0, len(list))
	for _, p := range list {
		out = append(out, makePlaylistView(u, p))
	}
	writeJSON(w, http.StatusOK, out)
}

func validatePlaylistMeta(name, comment string) (string, string, error) {
	name = strings.TrimSpace(name)
	comment = strings.TrimSpace(comment)
	if utf8.RuneCountInString(name) == 0 || utf8.RuneCountInString(name) > 100 {
		return "", "", errors.New("歌单名称必须为 1–100 个字符")
	}
	if utf8.RuneCountInString(comment) > 1000 {
		return "", "", errors.New("歌单备注不能超过 1000 个字符")
	}
	return name, comment, nil
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body struct {
		Name     string   `json:"name"`
		Comment  string   `json:"comment"`
		Public   *bool    `json:"public"`
		OwnerID  int64    `json:"ownerId"`
		TrackIDs []string `json:"trackIds"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	name, comment, err := validatePlaylistMeta(body.Name, body.Comment)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ownerID := u.ID
	if body.OwnerID > 0 {
		if !u.IsAdmin && body.OwnerID != u.ID {
			fail(w, http.StatusForbidden, "不能替其他用户创建歌单")
			return
		}
		ownerID = body.OwnerID
	}
	if _, err := s.DB.GetUserByID(r.Context(), ownerID); err != nil {
		fail(w, http.StatusBadRequest, "归属用户不存在")
		return
	}
	public := s.Settings.Get().PublicPlaylists
	if body.Public != nil {
		public = *body.Public
	}
	ids := uniqueTrackIDs(body.TrackIDs)
	tracks, ok := s.resolvePlaylistTracks(w, r, ids)
	if !ok {
		return
	}
	id := music.KindPlaylist + "-" + secret.RandomToken(9)
	if err := s.DB.CreatePlaylistWithMetadata(r.Context(), id, ownerID, name, comment, public, ids, tracks); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writePlaylistDetail(w, r, id)
}

func (s *Server) loadPlaylistForRead(w http.ResponseWriter, r *http.Request) (*db.Playlist, bool) {
	p, err := s.DB.GetPlaylist(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusNotFound, "歌单不存在")
		return nil, false
	}
	if !canReadPlaylist(currentUser(r), p) {
		fail(w, http.StatusForbidden, "无权查看该歌单")
		return nil, false
	}
	return p, true
}

func (s *Server) loadPlaylistForEdit(w http.ResponseWriter, r *http.Request) (*db.Playlist, bool) {
	p, err := s.DB.GetPlaylist(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusNotFound, "歌单不存在")
		return nil, false
	}
	if !canEditPlaylist(currentUser(r), p) {
		fail(w, http.StatusForbidden, "无权修改该歌单")
		return nil, false
	}
	return p, true
}

func infoTrackView(in *music.Info) trackView {
	return trackView{ID: in.TrackID(), Name: in.Name(), Singer: in.Singer(), Album: in.Album(), Source: in.Source(), Duration: in.Duration(), Qualities: in.Qualities()}
}

func (s *Server) detail(ctx context.Context, u *db.User, p *db.Playlist) playlistDetail {
	tracks := make([]trackView, 0, len(p.TrackIDs))
	for _, id := range p.TrackIDs {
		in, err := s.Catalog.Track(ctx, id)
		if err != nil {
			tracks = append(tracks, trackView{ID: id, Name: "无法读取的歌曲", Unavailable: true})
			continue
		}
		view := infoTrackView(in)
		view.ID = id // 保留歌单中实际存储的 ID，避免平台兜底元数据改写别名 ID。
		tracks = append(tracks, view)
	}
	return playlistDetail{playlistView: makePlaylistView(u, p), Tracks: tracks, TracksRevision: db.TracksRevision(p.TrackIDs)}
}

func (s *Server) writePlaylistDetail(w http.ResponseWriter, r *http.Request, id string) {
	p, err := s.DB.GetPlaylist(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, "歌单不存在")
		return
	}
	writeJSON(w, http.StatusOK, s.detail(r.Context(), currentUser(r), p))
}

func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForRead(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.detail(r.Context(), currentUser(r), p))
}

func (s *Server) updatePlaylist(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	var body struct {
		Name    *string `json:"name"`
		Comment *string `json:"comment"`
		Public  *bool   `json:"public"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	name, comment := p.Name, p.Comment
	if body.Name != nil {
		name = *body.Name
	}
	if body.Comment != nil {
		comment = *body.Comment
	}
	name, comment, err := validatePlaylistMeta(name, comment)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name != nil {
		body.Name = &name
	}
	if body.Comment != nil {
		body.Comment = &comment
	}
	if err := s.DB.UpdatePlaylistMeta(r.Context(), p.ID, body.Name, body.Comment, body.Public); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writePlaylistDetail(w, r, p.ID)
}

func (s *Server) replacePlaylistTracks(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	var body struct {
		TrackIDs         []string `json:"trackIds"`
		ExpectedRevision *string  `json:"expectedRevision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	if body.ExpectedRevision != nil && *body.ExpectedRevision != db.TracksRevision(p.TrackIDs) {
		playlistWriteError(w, db.ErrPlaylistConflict)
		return
	}
	tracks, ok := s.resolvePlaylistTracks(w, r, body.TrackIDs)
	if !ok {
		return
	}
	updated, err := s.DB.ReplacePlaylistTracksChecked(r.Context(), p.ID, currentUser(r), body.TrackIDs, tracks, body.ExpectedRevision)
	if err != nil {
		playlistWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.detail(r.Context(), currentUser(r), updated))
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeletePlaylist(r.Context(), p.ID); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query   string   `json:"query"`
		Sources []string `json:"sources"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	body.Query = strings.TrimSpace(body.Query)
	if body.Query == "" || utf8.RuneCountInString(body.Query) > 200 {
		fail(w, http.StatusBadRequest, "搜索关键词必须为 1–200 个字符")
		return
	}
	for _, source := range body.Sources {
		if !music.IsPlatform(source) {
			fail(w, http.StatusBadRequest, "不支持的平台: "+source)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	infos := s.Catalog.Search(ctx, body.Query, music.SearchOptions{Sources: body.Sources, Limit: 20})
	out := make([]trackView, 0, len(infos))
	for _, in := range infos {
		out = append(out, infoTrackView(in))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- 洛雪歌单导入 ----------

type importListView struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	Count   int    `json:"count"`
	Skipped int    `json:"skipped,omitempty"`
}

// importPlaylists 导入洛雪音乐歌单备份（.lxmc / .json）。
// multipart 字段：file 必填；preview=1 仅解析返回列表；lists 为要导入的列表序号（逗号分隔，空为全部）；
// target 为追加到的现有歌单 ID（空则每个列表新建一个歌单）；ownerId / public 仅新建时生效。
func (s *Server) importPlaylists(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBody)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		fail(w, http.StatusBadRequest, "上传失败: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "缺少歌单文件")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		fail(w, http.StatusBadRequest, "读取文件失败")
		return
	}
	lists, err := music.ParseLXList(data)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	views := make([]importListView, 0, len(lists))
	for i, pl := range lists {
		views = append(views, importListView{Index: i, Name: pl.Name, Source: pl.Source, Count: len(pl.Tracks), Skipped: pl.Skipped})
	}
	if r.FormValue("preview") == "1" {
		writeJSON(w, http.StatusOK, map[string]any{"lists": views})
		return
	}

	selected, err := parseIndexList(r.FormValue("lists"), len(lists))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	if target := strings.TrimSpace(r.FormValue("target")); target != "" {
		s.importAppend(w, r, u, target, lists, selected)
		return
	}

	ownerID := u.ID
	if raw := strings.TrimSpace(r.FormValue("ownerId")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			fail(w, http.StatusBadRequest, "ownerId 无效")
			return
		}
		if !u.IsAdmin && id != u.ID {
			fail(w, http.StatusForbidden, "不能替其他用户创建歌单")
			return
		}
		if _, err := s.DB.GetUserByID(ctx, id); err != nil {
			fail(w, http.StatusBadRequest, "归属用户不存在")
			return
		}
		ownerID = id
	}
	public := s.Settings.Get().PublicPlaylists
	if v := r.FormValue("public"); v != "" {
		public = v == "1" || v == "true" || v == "on"
	}
	var (
		created   []playlistView
		total     int
		skipped   int
		truncated int
	)
	for _, idx := range selected {
		pl := lists[idx]
		infos, ids := dedupeInfos(pl.Tracks)
		if len(ids) > maxPlaylistTracks {
			truncated += len(ids) - maxPlaylistTracks
			infos, ids = infos[:maxPlaylistTracks], ids[:maxPlaylistTracks]
		}
		if err := s.Catalog.RememberSync(ctx, infos); err != nil {
			fail(w, http.StatusInternalServerError, "保存歌曲信息失败: "+err.Error())
			return
		}
		name := pl.Name
		if utf8.RuneCountInString(name) > 100 {
			name = string([]rune(name)[:100])
		}
		comment := "从洛雪音乐导入"
		if pl.Source != "" {
			comment += "（" + music.PlatformName(pl.Source) + " 在线歌单）"
		}
		id := music.KindPlaylist + "-" + secret.RandomToken(9)
		if err := s.DB.CreatePlaylistFull(ctx, id, ownerID, name, comment, public, ids); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		p, err := s.DB.GetPlaylist(ctx, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		created = append(created, makePlaylistView(u, p))
		total += len(ids)
		skipped += pl.Skipped
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": created, "added": total, "skipped": skipped, "truncated": truncated})
}

// importAppend 把选中列表的歌曲追加到现有歌单
func (s *Server) importAppend(w http.ResponseWriter, r *http.Request, u *db.User, target string, lists []music.LXPlaylist, selected []int) {
	ctx := r.Context()
	p, err := s.DB.GetPlaylist(ctx, target)
	if err != nil {
		fail(w, http.StatusNotFound, "目标歌单不存在")
		return
	}
	if !canEditPlaylist(u, p) {
		fail(w, http.StatusForbidden, "无权修改该歌单")
		return
	}
	var all []*music.Info
	skipped := 0
	for _, idx := range selected {
		all = append(all, lists[idx].Tracks...)
		skipped += lists[idx].Skipped
	}
	existing := make(map[string]bool, len(p.TrackIDs))
	for _, id := range p.TrackIDs {
		existing[id] = true
	}
	ids := append([]string(nil), p.TrackIDs...)
	var infos []*music.Info
	for _, in := range all {
		id := in.TrackID()
		if existing[id] {
			continue
		}
		existing[id] = true
		ids = append(ids, id)
		infos = append(infos, in)
	}
	truncated := 0
	if len(ids) > maxPlaylistTracks {
		truncated = len(ids) - maxPlaylistTracks
		ids = ids[:maxPlaylistTracks]
		infos = infos[:len(infos)-truncated]
	}
	if err := s.Catalog.RememberSync(ctx, infos); err != nil {
		fail(w, http.StatusInternalServerError, "保存歌曲信息失败: "+err.Error())
		return
	}
	if err := s.DB.ReplacePlaylistTracks(ctx, p.ID, ids); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, err = s.DB.GetPlaylist(ctx, p.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": []playlistView{makePlaylistView(u, p)}, "added": len(infos), "skipped": skipped, "truncated": truncated})
}

// dedupeInfos 按歌曲 ID 去重，保持顺序
func dedupeInfos(infos []*music.Info) ([]*music.Info, []string) {
	seen := make(map[string]bool, len(infos))
	out := make([]*music.Info, 0, len(infos))
	ids := make([]string, 0, len(infos))
	for _, in := range infos {
		id := in.TrackID()
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, in)
		ids = append(ids, id)
	}
	return out, ids
}

// parseIndexList 解析 "0,2,3" 形式的序号列表；空表示全部
func parseIndexList(raw string, n int) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i, err := strconv.Atoi(part)
		if err != nil || i < 0 || i >= n {
			return nil, errors.New("列表序号无效: " + part)
		}
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("请至少选择一个列表")
	}
	return out, nil
}
