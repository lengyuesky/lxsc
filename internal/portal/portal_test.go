package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

type portalFixture struct {
	server  *httptest.Server
	db      *db.DB
	users   map[string]*db.User
	service *Server
	mux     *http.ServeMux
}

func newPortalFixture(t *testing.T) *portalFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	box, err := secret.Load(dir, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	users := map[string]*db.User{}
	for _, item := range []struct {
		name  string
		admin bool
	}{{"admin", true}, {"alice", false}, {"bob", false}} {
		enc, _ := box.Encrypt("pw-" + item.name)
		u, err := database.CreateUser(ctx, item.name, enc, item.admin, "320k")
		if err != nil {
			t.Fatal(err)
		}
		users[item.name] = u
	}
	tracks := []db.Track{
		{ID: "tr-wy-1", Source: "wy", Name: "歌曲一", Singer: "歌手", Album: "专辑", JSON: []byte(`{"source":"wy","songmid":"1","name":"歌曲一","singer":"歌手","albumName":"专辑"}`)},
		{ID: "tr-tx-2", Source: "tx", Name: "歌曲二", Singer: "歌手", Album: "专辑", JSON: []byte(`{"source":"tx","songmid":"2","name":"歌曲二","singer":"歌手","albumName":"专辑"}`)},
	}
	if err := database.UpsertTracks(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	authManager := &webauth.Manager{DB: database, Secret: box}
	service := &Server{DB: database, Catalog: catalog, Settings: store, Secret: box, Log: log, Auth: authManager}
	authService := &webauth.Server{Auth: authManager, Settings: store}
	mux := http.NewServeMux()
	mux.Handle("/auth/", http.StripPrefix("/auth", authService.Routes()))
	mux.Handle("/", service.Routes())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &portalFixture{server: ts, db: database, users: users, service: service, mux: mux}
}

func (f *portalFixture) client(t *testing.T, username string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	status, _ := doJSON(t, client, http.MethodPost, f.server.URL+"/auth/login", map[string]any{"username": username, "password": "pw-" + username})
	if status != http.StatusOK {
		t.Fatalf("用户 %s 登录失败，状态码 %d", username, status)
	}
	return client
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func requestStatus(t *testing.T, client *http.Client, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestPortalPlaylistPermissionsAndOrdering(t *testing.T) {
	fixture := newPortalFixture(t)
	admin := fixture.client(t, "admin")
	alice := fixture.client(t, "alice")
	bob := fixture.client(t, "bob")

	status, created := doJSON(t, admin, http.MethodPost, fixture.server.URL+"/playlists", map[string]any{
		"name": "Bob 私有", "comment": "管理员代建", "public": false, "ownerId": fixture.users["bob"].ID,
	})
	if status != http.StatusOK {
		t.Fatalf("管理员代建失败: %d %+v", status, created)
	}
	playlistID, _ := created["id"].(string)
	if playlistID == "" || int64(created["userId"].(float64)) != fixture.users["bob"].ID {
		t.Fatalf("代建归属错误: %+v", created)
	}

	status, _ = doJSON(t, alice, http.MethodGet, fixture.server.URL+"/playlists/"+playlistID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("普通用户不应查看他人私有歌单，状态码 %d", status)
	}
	status, bobDetail := doJSON(t, bob, http.MethodGet, fixture.server.URL+"/playlists/"+playlistID, nil)
	if status != http.StatusOK || bobDetail["canEdit"] != true {
		t.Fatalf("拥有者应可编辑歌单: %d %+v", status, bobDetail)
	}

	order := []string{"tr-tx-2", "tr-wy-1", "tr-tx-2"}
	status, ordered := doJSON(t, bob, http.MethodPut, fixture.server.URL+"/playlists/"+playlistID+"/tracks", map[string]any{"trackIds": order})
	if status != http.StatusOK {
		t.Fatalf("保存歌曲顺序失败: %d %+v", status, ordered)
	}
	tracks, ok := ordered["tracks"].([]any)
	if !ok || len(tracks) != len(order) {
		t.Fatalf("歌曲数量或重复项丢失: %+v", ordered)
	}
	for i, raw := range tracks {
		if raw.(map[string]any)["id"] != order[i] {
			t.Fatalf("歌曲顺序错误: %+v", tracks)
		}
	}

	status, _ = doJSON(t, bob, http.MethodPut, fixture.server.URL+"/playlists/"+playlistID+"/tracks", map[string]any{"trackIds": []string{"bad-id"}})
	if status != http.StatusBadRequest {
		t.Fatalf("非法歌曲 ID 应返回 400，实际为 %d", status)
	}
	stored, err := fixture.db.GetPlaylist(context.Background(), playlistID)
	if err != nil || len(stored.TrackIDs) != len(order) {
		t.Fatalf("失败更新不应改动原歌单: %+v err=%v", stored, err)
	}

	status, publicCreated := doJSON(t, alice, http.MethodPost, fixture.server.URL+"/playlists", map[string]any{"name": "Alice 公开", "public": true})
	if status != http.StatusOK {
		t.Fatalf("创建公开歌单失败: %d %+v", status, publicCreated)
	}
	publicID := publicCreated["id"].(string)
	status, readonly := doJSON(t, bob, http.MethodGet, fixture.server.URL+"/playlists/"+publicID, nil)
	if status != http.StatusOK || readonly["canEdit"] != false {
		t.Fatalf("公开歌单应可只读访问: %d %+v", status, readonly)
	}
	status, _ = doJSON(t, bob, http.MethodPut, fixture.server.URL+"/playlists/"+publicID, map[string]any{"name": "越权修改"})
	if status != http.StatusForbidden {
		t.Fatalf("非拥有者不应修改公开歌单，状态码 %d", status)
	}

	status = requestStatus(t, admin, http.MethodGet, fixture.server.URL+"/users")
	if status != http.StatusOK {
		t.Fatalf("管理员用户列表失败: %d", status)
	}
	status = requestStatus(t, alice, http.MethodGet, fixture.server.URL+"/users")
	if status != http.StatusForbidden {
		t.Fatalf("普通用户不应读取用户列表，状态码 %d", status)
	}
}

func TestPortalLoginFailureAndAuthentication(t *testing.T) {
	fixture := newPortalFixture(t)
	client := &http.Client{}
	status, _ := doJSON(t, client, http.MethodGet, fixture.server.URL+"/auth/me", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("未登录访问应返回 401，实际为 %d", status)
	}
	status, _ = doJSON(t, client, http.MethodPost, fixture.server.URL+"/auth/login", map[string]any{"username": "alice", "password": "错误"})
	if status != http.StatusUnauthorized {
		t.Fatalf("错误密码应返回 401，实际为 %d", status)
	}
}

func TestUpdateProfileQuality(t *testing.T) {
	fixture := newPortalFixture(t)
	alice := fixture.client(t, "alice")
	status, out := doJSON(t, alice, http.MethodPut, fixture.server.URL+"/profile", map[string]any{"quality": "flac"})
	if status != http.StatusOK || out["quality"] != "flac" {
		t.Fatalf("更新默认音质失败: %d %+v", status, out)
	}
	stored, err := fixture.db.GetUserByID(context.Background(), fixture.users["alice"].ID)
	if err != nil || stored.Quality != "flac" {
		t.Fatalf("默认音质未持久化: %+v err=%v", stored, err)
	}
	status, _ = doJSON(t, alice, http.MethodPut, fixture.server.URL+"/profile", map[string]any{"quality": "master"})
	if status != http.StatusBadRequest {
		t.Fatalf("不支持的音质应返回 400，实际为 %d", status)
	}
	stored, _ = fixture.db.GetUserByID(context.Background(), fixture.users["alice"].ID)
	if stored.Quality != "flac" {
		t.Fatalf("失败更新不应改变默认音质: %+v", stored)
	}
}

func doMultipart(t *testing.T, client *http.Client, url string, fields map[string]string, filename string, file []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(file)
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const lxImportSample = `{"type":"playList_v2","data":[
 {"id":"default","name":"list__name_default","list":[
   {"id":"kg_1","name":"晴天","singer":"周杰伦","source":"kg","interval":"04:29","meta":{"songId":20505418,"albumName":"叶惠美","picUrl":"","qualitys":[{"type":"128k"}],"_qualitys":{},"albumId":"1","hash":"ABCDEF"}},
   {"id":"kg_1_dup","name":"晴天","singer":"周杰伦","source":"kg","interval":"04:29","meta":{"songId":20505418,"albumName":"叶惠美","picUrl":"","qualitys":[],"_qualitys":{},"albumId":"1","hash":"abcdef"}},
   {"id":"local_1","name":"本地","singer":"","source":"local","meta":{"filePath":"/x"}}
 ]},
 {"id":"wy_x","name":"随便","source":"wy","list":[
   {"id":"wy_1","name":"歌曲一","singer":"歌手","source":"wy","meta":{"songId":1,"albumName":"专辑","qualitys":[],"_qualitys":{},"albumId":""}},
   {"id":"wy_9","name":"歌曲九","singer":"歌手","source":"wy","meta":{"songId":9,"albumName":"专辑","qualitys":[],"_qualitys":{},"albumId":""}}
 ]}
]}`

func TestImportLXPlaylists(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.client(t, "alice")
	base := f.server.URL + "/playlists/import"

	// 预览
	status, out := doMultipart(t, alice, base, map[string]string{"preview": "1"}, "lx_list.lxmc", []byte(lxImportSample))
	if status != http.StatusOK {
		t.Fatalf("预览失败: %d %v", status, out)
	}
	lists := out["lists"].([]any)
	if len(lists) != 2 || lists[0].(map[string]any)["name"] != "默认列表" || lists[0].(map[string]any)["count"].(float64) != 2 || lists[0].(map[string]any)["skipped"].(float64) != 1 {
		t.Fatalf("预览结果异常: %v", lists)
	}

	// 新建导入，仅第 1 个列表，指定公开
	status, out = doMultipart(t, alice, base, map[string]string{"lists": "0", "public": "1"}, "lx_list.lxmc", []byte(lxImportSample))
	if status != http.StatusOK {
		t.Fatalf("导入失败: %d %v", status, out)
	}
	if out["added"].(float64) != 1 || out["skipped"].(float64) != 1 {
		t.Fatalf("导入统计异常: %v", out)
	}
	created := out["playlists"].([]any)[0].(map[string]any)
	if created["name"] != "默认列表" || created["public"] != true || created["songCount"].(float64) != 1 {
		t.Fatalf("新建歌单异常: %v", created)
	}
	pid := created["id"].(string)
	status, detail := doJSON(t, alice, http.MethodGet, f.server.URL+"/playlists/"+pid, nil)
	if status != http.StatusOK {
		t.Fatal("读取歌单失败")
	}
	track := detail["tracks"].([]any)[0].(map[string]any)
	if track["id"] != "tr-kg-ABCDEF" || track["name"] != "晴天" || track["album"] != "叶惠美" || track["unavailable"] == true {
		t.Fatalf("导入歌曲元数据异常: %v", track)
	}

	// 追加到现有歌单：已有 tr-wy-1 应去重，只新增 tr-wy-9
	status, pl := doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists", map[string]any{"name": "追加目标"})
	if status != http.StatusOK {
		t.Fatal("创建歌单失败")
	}
	target := pl["id"].(string)
	if status, _ = doJSON(t, alice, http.MethodPut, f.server.URL+"/playlists/"+target+"/tracks", map[string]any{"trackIds": []string{"tr-wy-1"}}); status != http.StatusOK {
		t.Fatal("设置初始歌曲失败")
	}
	status, out = doMultipart(t, alice, base, map[string]string{"lists": "1", "target": target}, "lx_list.json", []byte(lxImportSample))
	if status != http.StatusOK || out["added"].(float64) != 1 {
		t.Fatalf("追加导入异常: %d %v", status, out)
	}
	_, detail = doJSON(t, alice, http.MethodGet, f.server.URL+"/playlists/"+target, nil)
	ids := detail["tracks"].([]any)
	if len(ids) != 2 || ids[0].(map[string]any)["id"] != "tr-wy-1" || ids[1].(map[string]any)["id"] != "tr-wy-9" {
		t.Fatalf("追加后歌曲顺序异常: %v", ids)
	}

	// 权限：bob 不能追加到 alice 的歌单，也不能替他人新建
	bob := f.client(t, "bob")
	if status, _ = doMultipart(t, bob, base, map[string]string{"target": target}, "a.json", []byte(lxImportSample)); status != http.StatusForbidden {
		t.Fatalf("越权追加应返回 403，得到 %d", status)
	}
	if status, _ = doMultipart(t, bob, base, map[string]string{"ownerId": strconv.FormatInt(f.users["alice"].ID, 10)}, "a.json", []byte(lxImportSample)); status != http.StatusForbidden {
		t.Fatalf("越权新建应返回 403，得到 %d", status)
	}
	// 管理员可替他人导入
	admin := f.client(t, "admin")
	status, out = doMultipart(t, admin, base, map[string]string{"ownerId": strconv.FormatInt(f.users["bob"].ID, 10), "lists": "1"}, "a.json", []byte(lxImportSample))
	if status != http.StatusOK || out["playlists"].([]any)[0].(map[string]any)["owner"] != "bob" {
		t.Fatalf("管理员替他人导入异常: %d %v", status, out)
	}
	// 错误文件
	if status, _ = doMultipart(t, alice, base, nil, "bad.lxmc", []byte("garbage")); status != http.StatusBadRequest {
		t.Fatalf("无效文件应返回 400，得到 %d", status)
	}
}
