package subsonic

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/simplelru"
	"lxsc/internal/admission"
)

const (
	coverConcurrent       = 8
	coverQueueLimit       = 64
	coverUserConcurrent   = 4
	coverUserQueueLimit   = 32
	coverCacheBytes       = 16 << 20
	coverCacheEntries     = 128
	coverCacheTTL         = 15 * time.Minute
	coverStaleTTL         = 24 * time.Hour
	coverRequestTimeout   = 9 * time.Second
	coverOperationTimeout = 8 * time.Second
	coverQueueTimeout     = 2 * time.Second
	coverMaxWaiters       = 256
	coverMaxSharedWaiters = 64
)

type cachedCover struct {
	image   coverImage
	expires time.Time
}

type coverCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	permit  *admission.Permit
	waiters int
	image   coverImage
	err     error
}

// coverWork 把字节预算、在途合并和等待者取消放在同一锁内；只为新的上游任务预占名额。
type coverWork struct {
	mu           sync.Mutex
	entries      *simplelru.LRU[string, cachedCover]
	calls        map[string]*coverCall
	bytes        int
	waiters      int
	hits         uint64
	misses       uint64
	shared       uint64
	rejected     uint64
	failed       uint64
	cancelled    uint64
	stale        uint64
	placeholders uint64
}

func newCoverWork() *coverWork {
	c := &coverWork{calls: make(map[string]*coverCall)}
	c.entries, _ = simplelru.NewLRU[string, cachedCover](coverCacheEntries, func(_ string, value cachedCover) {
		c.bytes -= len(value.image.data)
	})
	return c
}

func (c *coverWork) putLocked(key string, image coverImage) {
	if len(image.data) == 0 || len(image.data) > coverCacheBytes {
		return
	}
	if old, ok := c.entries.Peek(key); ok {
		c.bytes -= len(old.image.data)
	}
	c.entries.Add(key, cachedCover{image: image, expires: time.Now().Add(coverCacheTTL)})
	c.bytes += len(image.data)
	for c.bytes > coverCacheBytes {
		c.entries.RemoveOldest()
	}
}

func (c *coverWork) load(ctx context.Context, key string, user int64, gate *admission.Gate, fetch func(context.Context) (coverImage, error)) (coverImage, bool, error) {
	if err := ctx.Err(); err != nil {
		return coverImage{}, false, err
	}
	c.mu.Lock()
	if entry, ok := c.entries.Get(key); ok {
		if time.Now().Before(entry.expires) {
			c.hits++
			c.mu.Unlock()
			return entry.image, true, nil
		}
		if time.Now().After(entry.expires.Add(coverStaleTTL)) {
			c.entries.Remove(key)
		}
	}
	call := c.calls[key]
	if c.waiters >= coverMaxWaiters || (call != nil && call.waiters >= coverMaxSharedWaiters) {
		c.rejected++
		c.mu.Unlock()
		return coverImage{}, false, admission.ErrBusy
	}
	if call == nil {
		permit, err := gate.Reserve(user)
		if err != nil {
			c.mu.Unlock()
			return coverImage{}, false, err
		}
		// 一位等待者的取消不能中止其他人仍在等待的同一张图片。
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coverOperationTimeout)
		call = &coverCall{done: make(chan struct{}), cancel: cancel, permit: permit}
		c.calls[key] = call
		c.misses++
		go c.run(workCtx, key, call, fetch)
	} else {
		c.shared++
	}
	call.waiters++
	c.waiters++
	c.mu.Unlock()
	defer c.leave(key, call)
	select {
	case <-ctx.Done():
		return coverImage{}, false, ctx.Err()
	case <-call.done:
		if err := ctx.Err(); err != nil {
			return coverImage{}, false, err
		}
		return call.image, false, call.err
	}
}

func (c *coverWork) leave(key string, call *coverCall) {
	c.mu.Lock()
	call.waiters--
	c.waiters--
	abandoned := call.waiters == 0 && c.calls[key] == call
	if abandoned {
		delete(c.calls, key)
	}
	c.mu.Unlock()
	if abandoned {
		call.cancel()
	}
}

func (c *coverWork) run(ctx context.Context, key string, call *coverCall, fetch func(context.Context) (coverImage, error)) {
	var picture coverImage
	var err error
	defer func() {
		if recover() != nil {
			err = errors.New("封面请求异常")
		}
		// 返回结果前归还名额；实际读取停止前不能提前释放正在使用的上游额度。
		call.permit.Release()
		call.cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.calls[key] == call {
			delete(c.calls, key)
			if err == nil {
				c.putLocked(key, picture)
			}
		}
		if errors.Is(err, context.Canceled) {
			c.cancelled++
		} else if err != nil {
			c.failed++
		}
		call.image, call.err = picture, err
		close(call.done)
	}()
	queueCtx, cancel := context.WithTimeout(ctx, coverQueueTimeout)
	err = call.permit.Wait(queueCtx)
	cancel()
	if err != nil {
		return
	}
	picture, err = fetch(ctx)
	if err == nil {
		err = ctx.Err()
	}
}

func (c *coverWork) staleImage(key string) (coverImage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries.Get(key)
	if !ok {
		return coverImage{}, false
	}
	if time.Now().After(entry.expires.Add(coverStaleTTL)) {
		c.entries.Remove(key)
		return coverImage{}, false
	}
	c.stale++
	return entry.image, true
}

func (c *coverWork) placeholder() {
	c.mu.Lock()
	c.placeholders++
	c.mu.Unlock()
}

func (s *Server) CoverPerformance() any {
	s.initCoverClient()
	c := s.coverCache
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{
		"admission": s.coverLimits.Stats(), "delivery": s.coverDelivery.Stats(), "userLimit": coverUserConcurrent, "userQueueLimit": coverUserQueueLimit,
		"cacheEntries": c.entries.Len(), "cacheBytes": c.bytes,
		"maxCacheEntries": coverCacheEntries, "maxCacheBytes": coverCacheBytes,
		"inFlight": len(c.calls), "waiting": c.waiters, "maxWaiting": coverMaxWaiters,
		"hits": c.hits, "misses": c.misses, "shared": c.shared, "waiterRejected": c.rejected,
		"failed": c.failed, "cancelled": c.cancelled, "staleServed": c.stale, "placeholders": c.placeholders,
	}
}
