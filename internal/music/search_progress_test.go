package music

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchProgressBeforeSlowPlatformAndCancellation(t *testing.T) {
	c := newStabilityCatalog(t)
	var fastCalls atomic.Int32
	slowStarted, slowCanceled := make(chan struct{}), make(chan struct{})
	c.searchCall = func(ctx context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path == "wy.musicSearch.search" {
			fastCalls.Add(1)
			return json.RawMessage(`{"list":[{"songmid":"one","name":"歌曲"}]}`), nil
		}
		close(slowStarted)
		<-ctx.Done()
		close(slowCanceled)
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan SearchPlatformResult, 2)
	done := make(chan struct{})
	go func() {
		c.SearchProgress(ctx, "歌曲", SearchOptions{Sources: []string{"wy", "tx"}}, func(result SearchPlatformResult) { results <- result })
		close(done)
	}()
	select {
	case result := <-results:
		if result.Source != "wy" || len(result.Tracks) != 1 || result.Err != nil {
			t.Fatalf("必须先交付成功平台: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("快平台被慢平台阻塞")
	}
	<-slowStarted
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("搜索取消未及时退出")
	}
	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("无人等待时上游未取消")
	}
	result := <-results
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("取消必须提供失败状态: %v", result.Err)
	}
	if len(c.Search(context.Background(), "歌曲", SearchOptions{Sources: []string{"wy"}})) != 1 || fastCalls.Load() != 1 {
		t.Fatal("成功平台应独立缓存")
	}
}

func TestSearchBudgetReturnsCompletedResults(t *testing.T) {
	c := newStabilityCatalog(t)
	fastReturned := make(chan struct{})
	c.searchCall = func(ctx context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path == "wy.musicSearch.search" {
			return json.RawMessage(`{"list":[{"songmid":"one"}]}`), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var results []SearchPlatformResult
	c.SearchProgress(ctx, "关键词", SearchOptions{Sources: []string{"wy", "tx"}}, func(result SearchPlatformResult) {
		if result.Source == "wy" {
			close(fastReturned)
		}
		results = append(results, result)
	})
	select {
	case <-fastReturned:
	default:
		t.Fatal("预算内的结果被丢失")
	}
	if len(results) != 2 || results[0].Source != "wy" || !errors.Is(results[1].Err, context.DeadlineExceeded) {
		t.Fatalf("超时结果错误: %+v", results)
	}
}
