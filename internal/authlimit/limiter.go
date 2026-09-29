// Package authlimit 限制失败认证；成功请求释放预占额度，不消耗每分钟失败预算。
package authlimit

import (
	"crypto/sha256"
	"net"
	"sync"
	"time"
)

type entry struct {
	failed, pending int
	expires         time.Time
}

// Limiter 零值可用，只保存固定长度摘要，最多 4096 个账户/IP 键。
type Limiter struct {
	mu        sync.Mutex
	entries   map[[32]byte]*entry
	nextPrune time.Time
}

// Begin 先预占失败额度，防止并发错误认证绕过限制；finish 必须且只需调用一次。
func (l *Limiter) Begin(identity, address string, now time.Time) (finish func(bool), allowed bool) {
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	keys := [2][32]byte{sha256.Sum256([]byte("account:" + identity)), sha256.Sum256([]byte("ip:" + address))}
	limits := [2]int{20, 60}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[[32]byte]*entry)
	}
	if !now.Before(l.nextPrune) {
		for key, e := range l.entries {
			if e.pending == 0 && !now.Before(e.expires) {
				delete(l.entries, key)
			}
		}
		l.nextPrune = now.Add(time.Minute)
	}
	var entries [2]*entry
	missing := 0
	for i, key := range keys {
		e := l.entries[key]
		if e != nil && e.pending == 0 && !now.Before(e.expires) {
			delete(l.entries, key)
			e = nil
		}
		if e == nil {
			missing++
			e = &entry{expires: now.Add(time.Minute)}
		}
		if e.failed+e.pending >= limits[i] {
			return nil, false
		}
		entries[i] = e
	}
	if len(l.entries)+missing > 4096 {
		return nil, false
	}
	for i, key := range keys {
		entries[i].pending++
		l.entries[key] = entries[i]
	}
	var once sync.Once
	return func(success bool) {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			for i, e := range entries {
				e.pending--
				if !success {
					e.failed++
				}
				if e.pending == 0 && e.failed == 0 {
					delete(l.entries, keys[i])
				}
			}
		})
	}, true
}
