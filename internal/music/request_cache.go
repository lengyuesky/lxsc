package music

import (
	"context"
	"fmt"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// cacheToken 只用于标识某次解析，不能根据 URL 是否相同判断缓存新旧。
type cacheToken struct {
	generation uint64
	serial     uint64
}

type cachedResult[V any] struct {
	value  V
	token  cacheToken
	cached bool
}

type timedEntry[V any] struct {
	result  cachedResult[V]
	created time.Time
	expires time.Time
}

// cachePersistence 的操作在缓存锁内执行，保证失效后旧请求不能重新落盘。
type cachePersistence[K comparable, V any] interface {
	put(K, timedEntry[V]) error
	touch(K, time.Time) error
	remove(K) error
	clear() error
	disable(string)
	close() error
}

type flightKey[K comparable] struct {
	key        K
	generation uint64
}

type cacheCall[V any] struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	result  cachedResult[V]
	err     error
}

// requestCache 将过期、代次隔离、条件失效和在途合并放在同一同步边界内。
// 固定 LRU 不创建清理协程；过期项在读取或容量淘汰时移除。
type requestCache[K comparable, V any] struct {
	mu         sync.Mutex
	entries    *lru.Cache[K, timedEntry[V]]
	calls      map[flightKey[K]]*cacheCall[V]
	ttl        time.Duration
	generation uint64
	serial     uint64
	now        func() time.Time
	capacity   int
	persistent cachePersistence[K, V]
}

func newRequestCache[K comparable, V any](capacity int, ttl time.Duration) *requestCache[K, V] {
	entries, err := lru.New[K, timedEntry[V]](capacity)
	if err != nil {
		panic(err)
	}
	return &requestCache[K, V]{entries: entries, calls: make(map[flightKey[K]]*cacheCall[V]), ttl: ttl, now: time.Now, capacity: capacity}
}

func (c *requestCache[K, V]) persistLocked(stage string, fn func(cachePersistence[K, V]) error) {
	if c.persistent != nil {
		if err := fn(c.persistent); err != nil {
			c.persistent.disable(stage)
			c.persistent = nil
		}
	}
}

func (c *requestCache[K, V]) removeLocked(key K) {
	c.entries.Remove(key)
	c.persistLocked("删除", func(p cachePersistence[K, V]) error { return p.remove(key) })
}

func (c *requestCache[K, V]) clearLocked() {
	c.generation++
	c.entries.Purge()
	c.persistLocked("清理", func(p cachePersistence[K, V]) error { return p.clear() })
}

func (c *requestCache[K, V]) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.persistent == nil {
		return nil
	}
	p := c.persistent
	c.persistent = nil
	return p.close()
}

func (c *requestCache[K, V]) configure(ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl != ttl {
		c.ttl = ttl
		c.clearLocked()
	}
}

func (c *requestCache[K, V]) purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
}

// load 中 failed 非空表示条件刷新。迟到的失败不能删除新结果。
// fn 的布尔结果决定是否缓存；即使不缓存，也会把结果交给当前等待者。
func (c *requestCache[K, V]) load(ctx context.Context, key K, failed *cacheToken, timeout time.Duration, fn func(context.Context) (V, bool, error)) (cachedResult[V], error) {
	if err := ctx.Err(); err != nil {
		return cachedResult[V]{}, err
	}
	c.mu.Lock()
	if c.ttl != 0 {
		if entry, ok := c.entries.Get(key); ok {
			if (entry.expires.IsZero() || c.now().Before(entry.expires)) && (failed == nil || entry.result.token != *failed) {
				result := entry.result
				result.cached = true
				c.persistLocked("更新使用时间", func(p cachePersistence[K, V]) error { return p.touch(key, c.now()) })
				c.mu.Unlock()
				return result, nil
			}
			c.removeLocked(key)
		}
	}
	fk := flightKey[K]{key: key, generation: c.generation}
	call, ok := c.calls[fk]
	if !ok {
		remoteCtx, cancel := context.WithTimeout(context.Background(), timeout)
		c.serial++
		call = &cacheCall[V]{done: make(chan struct{}), cancel: cancel, result: cachedResult[V]{token: cacheToken{generation: c.generation, serial: c.serial}}}
		c.calls[fk] = call
		go c.execute(remoteCtx, fk, call, fn)
	}
	call.waiters++
	c.mu.Unlock()
	defer c.leave(fk, call)

	select {
	case <-ctx.Done():
		return cachedResult[V]{}, ctx.Err()
	case <-call.done:
		if err := ctx.Err(); err != nil {
			return cachedResult[V]{}, err
		}
		return call.result, call.err
	}
}

func (c *requestCache[K, V]) execute(ctx context.Context, key flightKey[K], call *cacheCall[V], fn func(context.Context) (V, bool, error)) {
	var value V
	var cacheable bool
	var err error
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("共享上游请求异常")
		}
		if ctx.Err() != nil {
			// 搜索可在预算结束时交付部分结果，但不得缓存。
			cacheable = false
		}
		call.cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		call.result.value, call.err = value, err
		// 全部等待者取消时旧任务已被移除，不能污染后来同键的新任务。
		if c.calls[key] == call {
			delete(c.calls, key)
			if err == nil && cacheable && c.ttl != 0 && key.generation == c.generation {
				now := c.now()
				entry := timedEntry[V]{result: call.result, created: now}
				if c.ttl > 0 {
					entry.expires = now.Add(c.ttl)
				}
				if c.entries.Len() == c.capacity && !c.entries.Contains(key.key) {
					oldest, _, _ := c.entries.GetOldest()
					c.removeLocked(oldest)
				}
				c.entries.Add(key.key, entry)
				c.persistLocked("保存", func(p cachePersistence[K, V]) error { return p.put(key.key, entry) })
			}
		}
		close(call.done)
	}()
	value, cacheable, err = fn(ctx)
}

func (c *requestCache[K, V]) leave(key flightKey[K], call *cacheCall[V]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	call.waiters--
	if call.waiters == 0 {
		call.cancel()
		if c.calls[key] == call {
			delete(c.calls, key)
		}
	}
}
