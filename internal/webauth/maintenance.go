package webauth

import (
	"context"
	"crypto/sha256"
	"time"
)

type loginAttempt struct {
	count   int
	expires time.Time
}

// allowLogin 在鉴权前预占名额，避免并发错误密码绕过限制。
func (m *Manager) allowLogin(name string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attempts == nil {
		m.attempts = make(map[[32]byte]loginAttempt)
	}
	for key, v := range m.attempts {
		if !now.Before(v.expires) {
			delete(m.attempts, key)
		}
	}
	key := sha256.Sum256([]byte(name))
	v, exists := m.attempts[key]
	if !exists {
		if len(m.attempts) >= 4096 {
			return false
		}
		v.expires = now.Add(time.Minute)
	}
	if v.count >= 10 {
		return false
	}
	v.count++
	m.attempts[key] = v
	return true
}

func (m *Manager) pruneLocked(now time.Time) {
	m.sessions.Range(func(key, value any) bool {
		if !now.Before(value.(session).expires) {
			m.sessions.Delete(key)
		}
		return true
	})
	for key, v := range m.attempts {
		if !now.Before(v.expires) {
			delete(m.attempts, key)
		}
	}
}

func (m *Manager) evictOldestLocked(userID int64, limit int) bool {
	count := 0
	var oldest any
	var expires time.Time
	m.sessions.Range(func(key, value any) bool {
		ss := value.(session)
		if userID != 0 && ss.userID != userID {
			return true
		}
		count++
		if oldest == nil || ss.expires.Before(expires) {
			oldest, expires = key, ss.expires
		}
		return true
	})
	if count < limit {
		return false
	}
	m.sessions.Delete(oldest)
	return true
}

// RevokeUser 撤销指定用户的全部网页登录会话。
func (m *Manager) RevokeUser(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions.Range(func(key, value any) bool {
		if value.(session).userID == userID {
			m.sessions.Delete(key)
		}
		return true
	})
}

// Maintain 由服务生命周期管理，不为每次登录创建常驻协程。
func (m *Manager) Maintain(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			m.pruneLocked(now)
			m.mu.Unlock()
		}
	}
}
