package urlcache

import (
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
		if err := s.Put(Record{TrackID: id, Quality: "320k", URL: "https://cdn.example/" + id, ResolvedQuality: "320k", CreatedAt: now.UnixMilli(), ExpiresAt: expires, LastUsedAt: int64(i)}); err != nil {
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
