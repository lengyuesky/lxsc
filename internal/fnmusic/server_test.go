package fnmusic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

type fixture struct {
	server *Server
	db     *db.DB
	box    *secret.Box
	auth   *webauth.Manager
	users  map[string]*db.User
	routes http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	box, err := secret.Load(dir, "fnmusic-test-key")
	if err != nil {
		t.Fatal(err)
	}
	users := map[string]*db.User{}
	for _, name := range []string{"alice", "bob", "admin"} {
		password, err := box.Encrypt("synthetic-password")
		if err != nil {
			t.Fatal(err)
		}
		u, err := database.CreateUser(ctx, name, password, name == "admin", "320k")
		if err != nil {
			t.Fatal(err)
		}
		users[name] = u
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	infos := []*music.Info{}
	for _, key := range []string{"one", "two", "public"} {
		infos = append(infos, music.FromMap(map[string]any{"source": "wy", "songmid": key, "name": key, "singer": "测试歌手", "albumName": "测试专辑", "interval": "00:45"}))
	}
	if err := catalog.RememberSync(ctx, infos); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id, user, track string
		public          bool
	}{{"pl-alice", "alice", "tr-wy-one", false}, {"pl-bob", "bob", "tr-wy-two", false}, {"pl-public", "bob", "tr-wy-public", true}} {
		if err := database.CreatePlaylistFull(ctx, item.id, users[item.user].ID, item.id, "", item.public, []string{item.track}); err != nil {
			t.Fatal(err)
		}
	}
	auth := &webauth.Manager{DB: database, Secret: box}
	server, err := New(ctx, database, catalog, store, auth, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	routes := chi.NewRouter()
	routes.Get("/trimcon", server.Probe)
	routes.Mount("/music", server.Routes())
	return &fixture{server, database, box, auth, users, routes}
}

func request(f *fixture, method, path, token string, body any) *httptest.ResponseRecorder {
	var content []byte
	if body != nil {
		content, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(content))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "music-token", Value: token})
	}
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	return w
}

func payload(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Code != 0 {
		t.Fatalf("协议请求失败，HTTP %d，code %d", w.Code, result.Code)
	}
	return result.Data
}

func login(t *testing.T, f *fixture, name string) string {
	t.Helper()
	digest := sha256.Sum256([]byte("synthetic-password"))
	w := request(f, http.MethodPost, "/music/api/v1/user/password-login", "", map[string]string{
		"username": name, "password": hex.EncodeToString(digest[:]), "deviceId": strings.Repeat("1", 32),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("测试登录失败，HTTP %d", w.Code)
	}
	token := payload(t, w)["userToken"].(string)
	if len(token) != 64 {
		t.Fatal("音乐票据格式无效")
	}
	return token
}

func TestInitializationAndStableServerIdentity(t *testing.T) {
	f := newFixture(t)
	w := request(f, http.MethodGet, "/music/api/v1/initialization/state", "", nil)
	if payload(t, w)["initialized"] != true {
		t.Fatal("初始化状态错误")
	}
	second, err := New(context.Background(), f.db, f.server.Catalog, f.server.Settings, f.auth, "new-version")
	if err != nil || second.GUID != f.server.GUID || !validGUID(second.GUID) {
		t.Fatal("服务器标识不稳定")
	}
	config := payload(t, request(f, http.MethodGet, "/music/api/v1/sys/config", "", nil))
	if config["serverVersion"] != compatibilityVersion || config["serverName"] != "lxsc" || config["nasOAuth"] != false {
		t.Fatal("兼容版本或独立服务身份错误")
	}
	for _, path := range []string{"/music/api/v1/user/me", "/music/api/v1/sys/info", "/music/api/v1/playlist/list", "/music/api/v1/track/stream"} {
		if request(f, http.MethodGet, path, "", nil).Code != http.StatusUnauthorized {
			t.Fatalf("未认证访问放行：%s", path)
		}
	}
}

func TestLoginTokenSourcesAndRevocation(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	for _, mode := range []string{"cookie", "raw", "bearer", "both", "conflicting", "duplicate-cookie"} {
		r := httptest.NewRequest(http.MethodGet, "/music/api/v1/user/me", nil)
		if mode == "cookie" || mode == "both" || mode == "conflicting" || mode == "duplicate-cookie" {
			r.AddCookie(&http.Cookie{Name: "music-token", Value: token})
		}
		if mode == "raw" || mode == "both" {
			r.Header.Set("Authorization", token)
		}
		if mode == "bearer" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if mode == "conflicting" {
			r.Header.Set("Authorization", strings.Repeat("0", 64))
		}
		if mode == "duplicate-cookie" {
			r.AddCookie(&http.Cookie{Name: "music-token", Value: strings.Repeat("0", 64)})
		}
		w := httptest.NewRecorder()
		f.routes.ServeHTTP(w, r)
		want := http.StatusOK
		if mode == "conflicting" || mode == "duplicate-cookie" {
			want = http.StatusUnauthorized
		}
		if w.Code != want {
			t.Fatalf("认证来源 %s: HTTP %d", mode, w.Code)
		}
	}
	enc, err := f.box.Encrypt("changed-password")
	if err != nil {
		t.Fatal(err)
	}
	u := f.users["alice"]
	if err := f.db.UpdateUserPassword(context.Background(), u.ID, u.PasswordEnc, enc); err != nil {
		t.Fatal(err)
	}
	if request(f, http.MethodGet, "/music/api/v1/user/me", token, nil).Code != http.StatusUnauthorized {
		t.Fatal("改密后音乐会话仍然有效")
	}
}

func TestFailedLoginAndSessionBounds(t *testing.T) {
	f := newFixture(t)
	body := map[string]string{"username": "alice", "password": strings.Repeat("0", 64)}
	for i := 0; i < 20; i++ {
		if request(f, http.MethodPost, "/music/api/v1/user/password-login", "", body).Code != http.StatusUnauthorized {
			t.Fatal("错误密码未拒绝")
		}
	}
	if request(f, http.MethodPost, "/music/api/v1/user/password-login", "", body).Code != http.StatusTooManyRequests {
		t.Fatal("失败登录没有限频")
	}
	now := time.Now()
	first, err := f.server.sessions.start(f.users["alice"], now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 11; i++ {
		if _, err := f.server.sessions.start(f.users["alice"], now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, valid := f.server.sessions.get(first, now.Add(time.Minute)); valid {
		t.Fatal("没有淘汰最早的第十一个会话")
	}
	if len(f.server.sessions.entries) != 10 {
		t.Fatal("会话数量超限")
	}
	for token := range f.server.sessions.entries {
		if fmt.Sprintf("%x", token) == first {
			t.Fatal("内存保存了原始访问票据")
		}
	}
	last, err := f.server.sessions.start(f.users["bob"], now)
	if err != nil {
		t.Fatal(err)
	}
	if _, valid := f.server.sessions.get(last, now.Add(8*24*time.Hour)); valid {
		t.Fatal("音乐会话过期后仍然有效")
	}
}

func TestLibraryPrivacyAndStableIdentifiers(t *testing.T) {
	f := newFixture(t)
	alice := login(t, f, "alice")
	bob := login(t, f, "bob")
	page := payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/list", alice, nil))
	if page["total"] != float64(2) || strings.Contains(fmt.Sprint(page), "pl-bob") {
		t.Fatal("歌单列表泄漏其他用户的私有歌单")
	}
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", bob, nil))
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/list", bob, nil))
	private := identifier("track", "tr-wy-two")
	privatePlaylist := identifier("playlist", "pl-bob")
	for _, path := range []string{
		"/music/api/v1/track/metadata?guid=" + private,
		"/music/api/v1/track/stream?guid=" + private,
		"/music/api/v1/static/cover?coverId=track_" + private,
		"/music/api/v1/playlist/detail?guid=" + privatePlaylist,
		"/music/api/v1/playlist/batch-detail?guids=" + privatePlaylist,
		"/music/api/v1/track/playlist-detail/list?playlistGUID=" + privatePlaylist,
	} {
		if request(f, http.MethodGet, path, alice, nil).Code != http.StatusNotFound {
			t.Fatalf("猜测标识绕过了权限：%s", path)
		}
	}
	second, err := New(context.Background(), f.db, f.server.Catalog, f.server.Settings, f.auth, "new-version")
	if err != nil {
		t.Fatal(err)
	}
	id, err := second.resolve(context.Background(), "track", private)
	if err != nil || id != "tr-wy-two" {
		t.Fatal("重启后标识映射丢失")
	}
	if _, err := second.resolve(context.Background(), "playlist", private); err == nil {
		t.Fatal("跨类型标识被接受")
	}
	if err := f.db.EnsureClientID(context.Background(), "fnmusic", "track", "other-track", private); err == nil {
		t.Fatal("冲突标识静默覆盖")
	}
}

func TestTrackShapePaginationAndPublicRevocation(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	result := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list?size=1&page=1", token, nil))
	items := result["list"].([]any)
	if result["total"] != float64(2) || len(items) != 1 {
		t.Fatal("授权后分页错误")
	}
	track := items[0].(map[string]any)
	guid := track["guid"].(string)
	if !validGUID(guid) || track["duration"] != float64(45000) {
		t.Fatal("曲目标识或时长错误")
	}
	if _, ok := track["album"].(map[string]any); !ok {
		t.Fatal("专辑不是对象")
	}
	if _, ok := track["genres"].([]any); !ok {
		t.Fatal("风格不是数组")
	}
	spec := track["audioSpec"].(map[string]any)
	if _, estimated := spec["size"]; estimated {
		t.Fatal("把未知文件大小作为真实属性返回")
	}
	metadata := payload(t, request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, token, nil))
	if _, ok := metadata["track"].(map[string]any); !ok {
		t.Fatal("metadata 缺少 track")
	}
	if _, ok := metadata["audioSpec"].(map[string]any); !ok {
		t.Fatal("metadata 缺少 audioSpec")
	}
	if request(f, http.MethodGet, "/music/api/v1/static/cover?coverId=track_"+guid, token, nil).Header().Get("Content-Type") != "image/png" {
		t.Fatal("封面没有返回图片")
	}
	empty := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list?size=500&page=2147483647", token, nil))
	if len(empty["list"].([]any)) != 0 {
		t.Fatal("超大页码错误")
	}
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	public := identifier("track", "tr-wy-public")
	no := false
	if err := f.db.UpdatePlaylistMeta(context.Background(), "pl-public", nil, nil, &no); err != nil {
		t.Fatal(err)
	}
	if request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+public, token, nil).Code != http.StatusNotFound {
		t.Fatal("撤销公开权限后仍能访问旧标识")
	}
}

func TestUnsupportedWritesAndLogout(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	if request(f, http.MethodPost, "/music/api/v1/playlist/delete", token, map[string]string{"guid": "anything"}).Code < 400 {
		t.Fatal("未实现的歌单写入返回假成功")
	}
	if request(f, http.MethodPost, "/music/api/v1/track/transcode", token, map[string]string{}).Code != http.StatusNotImplemented {
		t.Fatal("未实现的转码没有明确拒绝")
	}
	if request(f, http.MethodPost, "/music/api/v1/user/logout", token, nil).Code != http.StatusOK {
		t.Fatal("注销失败")
	}
	if request(f, http.MethodGet, "/music/api/v1/user/me", token, nil).Code != http.StatusUnauthorized {
		t.Fatal("注销后票据仍有效")
	}
}

func TestUnpagedPlaylistsAndAlbumArtistFilter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("extra-%03d", i)
		if err := f.db.CreatePlaylist(ctx, id, f.users["alice"].ID, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	token := login(t, f, "alice")
	result := payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/list?lan=zh-CN", token, nil))
	if result["total"] != float64(103) || len(result["list"].([]any)) != 103 {
		t.Fatal("原版无分页歌单请求被截断")
	}
	paged := payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/list?size=1&page=2", token, nil))
	if paged["total"] != float64(103) || len(paged["list"].([]any)) != 1 {
		t.Fatal("显式歌单分页错误")
	}
	for _, artist := range []string{"甲歌手", "乙歌手"} {
		track := music.FromMap(map[string]any{"source": "wy", "songmid": artist, "name": artist + "的歌", "singer": artist, "albumName": artist + "专辑"})
		if err := f.server.Catalog.RememberSync(ctx, []*music.Info{track}); err != nil {
			t.Fatal(err)
		}
		if err := f.db.CreatePlaylist(ctx, "pl-"+artist, f.users["alice"].ID, artist, []string{track.TrackID()}); err != nil {
			t.Fatal(err)
		}
	}
	filter := identifier("artist", music.ArtistID("甲歌手"))
	albums := payload(t, request(f, http.MethodGet, "/music/api/v1/album/list?artistGUID="+filter, token, nil))
	if albums["total"] != float64(1) || albums["list"].([]any)[0].(map[string]any)["name"] != "甲歌手专辑" {
		t.Fatal("歌手限定专辑没有过滤")
	}
	sorted := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list?sort=title,desc&size=1", token, nil))
	if sorted["sort"] != "title,desc" || sorted["list"].([]any)[0].(map[string]any)["title"] != "甲歌手的歌" {
		t.Fatal("排序没有在分页前应用")
	}
	unknown := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list?sort=createdAt,desc", token, nil))
	if unknown["sort"] != "" {
		t.Fatal("没有真实入库日期却声称已排序")
	}
}

func TestUserRenameDoesNotChangeIdentityAndDeletionRevokes(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	before := payload(t, request(f, http.MethodGet, "/music/api/v1/user/me", token, nil))
	u := f.users["alice"]
	if err := f.db.UpdateUser(context.Background(), u.ID, "renamed", "", false, "320k"); err != nil {
		t.Fatal(err)
	}
	after := payload(t, request(f, http.MethodGet, "/music/api/v1/user/me", token, nil))
	if before["guid"] != after["guid"] || after["name"] != "renamed" {
		t.Fatal("改名改变了音乐用户身份")
	}
	enc, err := f.box.Encrypt("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateUser(context.Background(), "alice", enc, false, "320k"); err != nil {
		t.Fatal(err)
	}
	newToken := login(t, f, "alice")
	newUser := payload(t, request(f, http.MethodGet, "/music/api/v1/user/me", newToken, nil))
	if newUser["guid"] == before["guid"] {
		t.Fatal("复用旧用户名复用了已存在用户的标识")
	}
	if err := f.db.DeleteUser(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}
	if request(f, http.MethodGet, "/music/api/v1/user/me", token, nil).Code != http.StatusUnauthorized {
		t.Fatal("删除用户后旧音乐会话仍有效")
	}
}
