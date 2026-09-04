package backup

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/secret"
)

type davMemory struct {
	mu       sync.Mutex
	files    map[string][]byte
	modified map[string]time.Time
}

func (d *davMemory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if user, password, ok := r.BasicAuth(); !ok || user != "user" || password != "pass" {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	name := filepath.Base(strings.TrimSuffix(r.URL.Path, "/"))
	switch r.Method {
	case "MKCOL":
		w.WriteHeader(http.StatusCreated)
	case "PROPFIND":
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
		fmt.Fprintf(w, `<d:response><d:href>%s/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response>`, r.URL.Path)
		var names []string
		for key := range d.files {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			fmt.Fprintf(w, `<d:response><d:href>%s/%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:getlastmodified>%s</d:getlastmodified><d:resourcetype/></d:prop></d:propstat></d:response>`, strings.TrimSuffix(r.URL.Path, "/"), key, len(d.files[key]), d.modified[key].UTC().Format(http.TimeFormat))
		}
		fmt.Fprint(w, `</d:multistatus>`)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		d.files[name] = body
		d.modified[name] = time.Now()
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		body, ok := d.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	case http.MethodDelete:
		delete(d.files, name)
		delete(d.modified, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestWebDAVBackupAndRetention(t *testing.T) {
	memory := &davMemory{files: map[string][]byte{}, modified: map[string]time.Time{}}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("lxsc-old-%d.lxsc-backup", i)
		memory.files[name] = []byte("old")
		memory.modified[name] = time.Now().Add(time.Duration(-i-1) * time.Hour)
	}
	server := httptest.NewServer(memory)
	defer server.Close()
	dir := t.TempDir()
	database, _ := db.Open(filepath.Join(dir, "lxsc.db"))
	defer database.Close()
	box, _ := secret.Load(dir, "key")
	enc, _ := box.Encrypt("password")
	if _, err := database.CreateUser(context.Background(), "admin", enc, true, "320k"); err != nil {
		t.Fatal(err)
	}
	s := New(database, box, dir, "test", server.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg, err := s.SaveWebDAVConfig(context.Background(), WebDAVConfig{URL: server.URL + "/dav", Username: "user", Password: "pass", Path: "lxsc-backups", Frequency: "daily", Time: "03:00", Weekday: 1, Retention: 3, BackupPassword: "archive-pass"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PasswordConfigured || !cfg.BackupPasswordConfigured || cfg.Password != "" {
		t.Fatalf("密码暴露或状态错误: %+v", cfg)
	}
	storedPassword := database.GetSetting(context.Background(), "backup.webdav.password", "")
	if storedPassword == "pass" {
		t.Fatal("WebDAV 密码不得明文存储")
	}
	if plain, err := box.Decrypt(storedPassword); err != nil || plain != "pass" {
		t.Fatalf("WebDAV 密码加密存储异常: %q %v", plain, err)
	}
	if err := s.TestWebDAV(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.RunWebDAVBackup(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	files, err := s.ListRemote(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("应只保留最近 3 份: %+v", files)
	}
	var generated string
	for _, file := range files {
		if strings.Contains(file.Name, "test") {
			generated = file.Name
		}
	}
	if generated == "" {
		t.Fatalf("未找到新备份: %+v", files)
	}
	memory.mu.Lock()
	encrypted := strings.HasPrefix(string(memory.files[generated]), "age-encryption.org/")
	memory.mu.Unlock()
	if !encrypted {
		t.Fatal("配置备份密码后远程文件应加密")
	}
	download, err := s.DownloadRemote(context.Background(), generated)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(download); err != nil || info.Size() == 0 {
		t.Fatalf("远程下载失败: %v", err)
	}
	_ = os.RemoveAll(filepath.Dir(download))
	if err := s.DeleteRemote(context.Background(), generated); err != nil {
		t.Fatal(err)
	}
	files, _ = s.ListRemote(context.Background())
	if len(files) != 2 {
		t.Fatalf("删除远程文件失败: %+v", files)
	}
}

func TestScheduleDailyWeeklyAndSlotDeduplication(t *testing.T) {
	memory := &davMemory{files: map[string][]byte{}, modified: map[string]time.Time{}}
	server := httptest.NewServer(memory)
	defer server.Close()
	dir := t.TempDir()
	database, _ := db.Open(filepath.Join(dir, "lxsc.db"))
	defer database.Close()
	box, _ := secret.Load(dir, "key")
	enc, _ := box.Encrypt("password")
	_, _ = database.CreateUser(context.Background(), "admin", enc, true, "320k")
	s := New(database, box, dir, "test", server.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	base := WebDAVConfig{URL: server.URL + "/dav", Username: "user", Password: "pass", Path: "backups", Auto: true, Frequency: "daily", Time: now.Format("15:04"), Weekday: int(now.Weekday()), Retention: 0}
	if _, err := s.SaveWebDAVConfig(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	s.checkSchedule(context.Background(), now)
	waitFileCount(t, memory, 1)
	memory.mu.Lock()
	dailyCount := len(memory.files)
	memory.mu.Unlock()
	if dailyCount != 1 {
		t.Fatalf("每日计划未执行: %d", dailyCount)
	}
	s.checkSchedule(context.Background(), now)
	time.Sleep(100 * time.Millisecond)
	memory.mu.Lock()
	duplicateCount := len(memory.files)
	memory.mu.Unlock()
	if duplicateCount != dailyCount {
		t.Fatal("同一每日槽位不应重复执行")
	}

	base.Frequency = "weekly"
	base.Password = ""
	if _, err := s.SaveWebDAVConfig(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	s.checkSchedule(context.Background(), now)
	waitFileCount(t, memory, dailyCount+1)
	memory.mu.Lock()
	weeklyCount := len(memory.files)
	memory.mu.Unlock()
	if weeklyCount != dailyCount+1 {
		t.Fatalf("每周计划未执行: %d", weeklyCount)
	}
}

func waitFileCount(t *testing.T, memory *davMemory, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		memory.mu.Lock()
		count := len(memory.files)
		memory.mu.Unlock()
		if count >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("远程备份数量未达到 %d", want)
}
