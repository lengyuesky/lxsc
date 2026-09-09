package music

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/js"
)

func TestSelectedCacheLateResultDoesNotReplaceNewerSource(t *testing.T) {
	c := newRequestCache[string, int](2, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.load(ctx, "歌曲", nil, time.Second, func(context.Context) (int, bool, error) { return 1, true, nil }); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		result, err := c.loadSelected(ctx, "歌曲", nil, "B", func(v int) bool { return v == 2 }, time.Second, func(ctx context.Context) (int, bool, error) {
			close(started)
			select {
			case <-release:
				return 2, true, nil
			case <-ctx.Done():
				return 0, false, ctx.Err()
			}
		})
		if err == nil && result.value != 2 {
			err = errors.New("B请求必须取得自己的候选，不能错误共享C")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("B请求未启动")
	}
	newer, err := c.loadSelected(ctx, "歌曲", nil, "C", func(v int) bool { return v == 3 }, time.Second, func(context.Context) (int, bool, error) { return 3, true, nil })
	if err != nil || newer.value != 3 {
		t.Fatalf("C请求失败：%+v %v", newer, err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := c.load(ctx, "歌曲", nil, time.Second, func(context.Context) (int, bool, error) { return 99, true, nil })
	if err != nil || !got.cached || got.value != 3 || got.token != newer.token {
		t.Fatalf("较早B请求晚到不能覆盖较新C缓存：%+v %v", got, err)
	}
}

func TestSelectedCacheNewerFallbackCanReplaceEarlierPublication(t *testing.T) {
	c := newRequestCache[string, int](2, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	startA, startB := make(chan struct{}), make(chan struct{})
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	defer func() {
		for _, ch := range []chan struct{}{releaseA, releaseB} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	launch := func(selection string, value int, started, release chan struct{}) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := c.loadSelected(ctx, "歌曲", nil, selection, func(v int) bool { return v == value }, time.Second, func(ctx context.Context) (int, bool, error) {
				close(started)
				select {
				case <-release:
					return value, true, nil
				case <-ctx.Done():
					return 0, false, ctx.Err()
				}
			})
			done <- err
		}()
		return done
	}
	a := launch("A", 1, startA, releaseA)
	<-startA
	b := launch("B", 2, startB, releaseB)
	<-startB
	close(releaseA)
	if err := <-a; err != nil {
		t.Fatal(err)
	}
	close(releaseB)
	if err := <-b; err != nil {
		t.Fatal(err)
	}
	got, err := c.load(ctx, "歌曲", nil, time.Second, func(context.Context) (int, bool, error) { return 99, true, nil })
	if err != nil || !got.cached || got.value != 2 {
		t.Fatalf("较新备选B必须能替换先完成的旧A：%+v %v", got, err)
	}
}

func TestSelectedCacheCoalescingIsolationAndCancellation(t *testing.T) {
	c := newRequestCache[string, int](2, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	remoteA, remoteB := make(chan context.Context, 1), make(chan context.Context, 1)
	var callsA, callsB atomic.Int32
	call := func(value int, calls *atomic.Int32, remote chan context.Context) func(context.Context) (int, bool, error) {
		return func(ctx context.Context) (int, bool, error) {
			calls.Add(1)
			remote <- ctx
			select {
			case <-release:
				return value, true, nil
			case <-ctx.Done():
				return 0, false, ctx.Err()
			}
		}
	}
	launch := func(ctx context.Context, selection string, value int, fn func(context.Context) (int, bool, error)) <-chan error {
		done := make(chan error, 1)
		go func() {
			result, err := c.loadSelected(ctx, "歌曲", nil, selection, func(v int) bool { return v == value }, 5*time.Second, fn)
			if err == nil && result.value != value {
				err = errors.New("不同候选集合错误共享取链结果")
			}
			done <- err
		}()
		return done
	}
	fnA, fnB := call(1, &callsA, remoteA), call(2, &callsB, remoteB)
	first := launch(firstCtx, "A", 1, fnA)
	sharedA := <-remoteA
	second := launch(ctx, "A", 1, fnA)
	third := launch(ctx, "B", 2, fnB)
	sharedB := <-remoteB
	waitCacheWaiters(t, c, 3)
	cancelFirst()
	if !errors.Is(<-first, context.Canceled) || sharedA.Err() != nil || sharedB.Err() != nil {
		t.Fatal("单等待者取消不能影响同集合其他等待者或其他集合")
	}
	// 让两个仍存活的调用正常完成；无缓存档位也必须只合并相同集合。
	unblock()
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if err := <-third; err != nil {
		t.Fatal(err)
	}
	if callsA.Load() != 1 || callsB.Load() != 1 || c.entries.Len() != 0 {
		t.Fatal("集合合并次数或不缓存边界错误")
	}
}

func TestPlaybackCandidateOrderCoalesces(t *testing.T) {
	c := newStabilityCatalog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c.urlCall = func(ctx context.Context, _ string, _ any, quality string, ids []int64) (*js.MusicURLResult, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
			return nil, errors.New("用于合并的候选集合应排序去重")
		}
		select {
		case <-release:
			return &js.MusicURLResult{URL: "https://media.invalid/a", SourceID: 1, Quality: quality}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	results := make(chan error, 2)
	go func() {
		_, err := c.ResolvePlaybackURLForSources(ctx, stabilityTrack(), "320k", []int64{2, 1, 2}, nil)
		results <- err
	}()
	<-started
	go func() {
		_, err := c.ResolvePlaybackURLForSources(ctx, stabilityTrack(), "320k", []int64{1, 2}, nil)
		results <- err
	}()
	waitCacheWaiters(t, c.urls, 2)
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("相同候选集合的不同顺序不能重复发起取链")
	}
}

func TestPlaybackFallbackIdentityPersistenceAndConditionalInvalidation(t *testing.T) {
	c := newStabilityCatalog(t)
	ctx := context.Background()
	dir := t.TempDir()
	var calls atomic.Int32
	call := func(_ context.Context, _ string, _ any, quality string, ids []int64) (*js.MusicURLResult, error) {
		calls.Add(1)
		id := int64(1)
		if len(ids) > 0 {
			id = ids[0]
		}
		return &js.MusicURLResult{URL: "https://media.invalid/同一地址", Source: "同名脚本", SourceID: id, Quality: quality}, nil
	}
	c.urlCall = call
	attachTestURLCache(t, c, dir)
	old, err := c.ResolvePlaybackURLForSources(ctx, stabilityTrack(), "320k", []int64{2, 1, 2}, nil)
	if err != nil || old.Result.SourceID != 1 {
		t.Fatalf("初次候选错误：%+v %v", old, err)
	}
	fresh, err := c.ResolvePlaybackURLForSources(ctx, stabilityTrack(), "320k", []int64{2}, nil)
	if err != nil || fresh.Result.SourceID != 2 || fresh.Cached || fresh.token == old.token {
		t.Fatalf("排除原源必须产生不同真实ID的新版本：%+v %v", fresh, err)
	}
	c.InvalidatePlaybackURL(old)
	reused, err := c.ResolvePlaybackURLForSources(ctx, stabilityTrack(), "320k", []int64{1, 2}, &old)
	if err != nil || !reused.Cached || reused.token != fresh.token || calls.Load() != 2 {
		t.Fatal("迟到旧失败不能删除新版本或再次调用坏源")
	}
	if err := c.CloseURLCache(); err != nil {
		t.Fatal(err)
	}
	restarted := NewCatalog(c.DB, nil, nil, c.Settings, c.Log)
	restarted.urlCall = call
	attachTestURLCache(t, restarted, dir)
	got, err := restarted.ResolvePlaybackURL(ctx, stabilityTrack(), "320k")
	if err != nil || !got.Cached || got.Result.SourceID != 2 || calls.Load() != 2 {
		t.Fatalf("重启必须复用成功替代且保留真实身份：%+v %v", got, err)
	}
	// 另一个明确只选择A的请求仍可使用A，排除不能演变为全局拉黑。
	other, err := restarted.ResolvePlaybackURLForSources(ctx, stabilityTrack(), "320k", []int64{1}, nil)
	if err != nil || other.Result.SourceID != 1 || calls.Load() != 3 {
		t.Fatal("请求内排除不得成为全局拉黑")
	}
	if _, err := c.DB.GetTrack(ctx, stabilityTrack().TrackID()); err == nil {
		t.Fatal("取链与持久直链缓存不能记录播放")
	}
}
