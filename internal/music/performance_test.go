package music

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/js"
)

// BenchmarkConcurrentSearch 使用合成上游，建立多用户重复搜索的可复现基线。
func BenchmarkConcurrentSearch(b *testing.B) {
	c := newStabilityCatalog(b)
	var remoteCalls atomic.Int64
	c.searchCall = func(ctx context.Context, _ string, _ ...any) (json.RawMessage, error) {
		remoteCalls.Add(1)
		timer := time.NewTimer(time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		return json.RawMessage(`{"list":[{"songmid":"1","name":"基准歌曲"}]}`), nil
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if len(c.Search(context.Background(), "基准歌曲", SearchOptions{Sources: []string{"wy", "tx", "kw", "kg", "mg"}})) != 5 {
				b.Error("合成搜索结果缺失")
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(remoteCalls.Load()), "upstream-calls")
}

func BenchmarkPersistentURLCacheColdWrites(b *testing.B) {
	c := newStabilityCatalog(b)
	c.urlCall = func(context.Context, string, any, string, []int64) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://example.invalid/audio", Quality: "320k", SourceID: 1}, nil
	}
	if err := c.EnableURLCache(b.TempDir(), ""); err != nil {
		b.Fatal(err)
	}
	defer c.CloseURLCache()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		track := FromMap(map[string]any{"source": "wy", "songmid": fmt.Sprint(i)})
		if _, err := c.ResolvePlaybackURL(context.Background(), track, "320k"); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	stats := c.urls.persistenceTiming.Snapshot()
	b.ReportMetric(stats.AverageMS, "disk-average-ms")
	b.ReportMetric(stats.MaxMS, "disk-max-ms")
}

// BenchmarkPersistentURLCacheHit 测量启用 SQLite 持久化时连续取链的热门缓存路径。
func BenchmarkPersistentURLCacheHit(b *testing.B) {
	c := newStabilityCatalog(b)
	c.urlCall = func(context.Context, string, any, string, []int64) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://example.invalid/audio", Quality: "320k", SourceID: 1}, nil
	}
	if err := c.EnableURLCache(b.TempDir(), ""); err != nil {
		b.Fatal(err)
	}
	defer c.CloseURLCache()
	track := stabilityTrack()
	if _, err := c.ResolvePlaybackURL(context.Background(), track, "320k"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, err := c.ResolvePlaybackURL(context.Background(), track, "320k")
		if err != nil || !value.Cached {
			b.Fatal("热门缓存命中失败")
		}
	}
}
