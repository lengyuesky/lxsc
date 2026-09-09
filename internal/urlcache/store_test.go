package urlcache

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDropsExpiredAndExcessRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reset("测试上下文"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, id := range []string{"过期", "淘汰", "保留", "永久"} {
		expires := now.Add(time.Hour).UnixMilli()
		if i == 0 {
			expires = now.UnixMilli()
		} else if i == 3 {
			expires = 0
		}
		if err := s.Put(Record{TrackID: id, Quality: "320k", URL: "https://cdn.example/" + id, ResolvedQuality: "320k", SourceID: 22, CreatedAt: now.UnixMilli(), ExpiresAt: expires, LastUsedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Load("测试上下文", 2, now)
	if err != nil || len(rows) != 2 || rows[0].TrackID != "保留" || rows[1].TrackID != "永久" {
		t.Fatalf("恢复应保留未过期且最近使用的条目: %+v %v", rows, err)
	}
	rows, err = s.Load("其他上下文", 2, now)
	if err != nil || len(rows) != 0 {
		t.Fatal("不匹配的上下文必须清空")
	}
}

func TestLegacySourceNameCacheIsRebuilt(t *testing.T) {
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite", filepath.Join(dir, Filename))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE cache_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO cache_meta VALUES ('fingerprint', '旧上下文')`,
		`CREATE TABLE playback_urls (track_id TEXT, quality TEXT, url TEXT, resolved_quality TEXT, source TEXT, created_at INTEGER, expires_at INTEGER, last_used_at INTEGER, PRIMARY KEY (track_id, quality))`,
		`INSERT INTO playback_urls VALUES ('旧歌曲', '320k', 'https://media.invalid/old', '320k', '同名脚本', 1, 0, 1)`,
	} {
		if _, err := legacy.Exec(statement); err != nil {
			_ = legacy.Close()
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Load("旧上下文", 10, time.Now())
	if err != nil || len(rows) != 0 {
		_ = s.Close()
		t.Fatalf("只有名字的旧缓存不得猜测脚本ID后复用：%+v %v", rows, err)
	}
	if err := s.Put(Record{TrackID: "新歌曲", Quality: "320k", URL: "https://media.invalid/new", ResolvedQuality: "320k", Source: "同名脚本", SourceID: 22}); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err = s.Load("旧上下文", 10, time.Now())
	if err != nil || len(rows) != 1 || rows[0].SourceID != 22 {
		t.Fatalf("新缓存格式重启后必须保留可信身份：%+v %v", rows, err)
	}
}

func TestLoadDropsUnknownSourceIdentity(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Reset("测试上下文"); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{0, -1, 22} {
		if err := s.Put(Record{TrackID: []string{"未知", "无效", "有效"}[i], Quality: "320k", URL: "https://media.invalid/audio", SourceID: id}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Load("测试上下文", 10, time.Now())
	if err != nil || len(rows) != 1 || rows[0].SourceID != 22 {
		t.Fatalf("缺失身份不能进入持久缓存恢复：%+v %v", rows, err)
	}
}

func TestCorruptDatabaseIsMarkedForRebuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte("损坏的缓存"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir); err == nil {
		_ = s.Close()
		t.Fatal("损坏的缓存数据库不能被复用")
	}
	if _, err := os.Stat(filepath.Join(dir, Filename) + ".invalid"); err != nil {
		t.Fatal("缓存故障必须留下重建标记")
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("下次启动应重建缓存: %v", err)
	}
	defer s.Close()
	rows, err := s.Load("测试上下文", 2000, time.Now())
	if err != nil || len(rows) != 0 {
		t.Fatalf("重建后应为空缓存: %+v %v", rows, err)
	}
}
