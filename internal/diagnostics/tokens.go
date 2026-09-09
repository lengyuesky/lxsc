package diagnostics

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

const maxTokens = 64
const maxActiveTokens = 16
const callsPerMinute = 30

var errTokenLimit = errors.New("凭据数量已达上限")

// TokenView 不含凭据或摘要；只在内存保存，固定绝对到期。
type TokenView struct {
	ID         string     `json:"id"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	Calls      uint64     `json:"calls"`
	Revoked    bool       `json:"revoked"`
}
type token struct {
	TokenView
	owner       int64
	digest      [32]byte
	ctx         context.Context
	cancel      context.CancelFunc
	timer       *time.Timer
	window      time.Time
	windowCalls int
}
type Audit struct {
	Time   time.Time `json:"time"`
	ID     string    `json:"id"`
	Action string    `json:"action"`
}

// Tokens 所有索引、计数、撤销都使用同一把锁；不持有明文密钥。
type Tokens struct {
	mu      sync.Mutex
	entries map[string]*token
	audit   []Audit
}

func NewTokens() *Tokens { return &Tokens{entries: make(map[string]*token)} }
func (m *Tokens) auditLocked(id, action string) {
	if len(m.audit) == 128 {
		copy(m.audit, m.audit[1:])
		m.audit = m.audit[:127]
	}
	m.audit = append(m.audit, Audit{time.Now().UTC(), id, action})
}
func cloneView(t *token) TokenView {
	v := t.TokenView
	v.Scopes = append([]string{}, v.Scopes...)
	if v.LastUsedAt != nil {
		used := *v.LastUsedAt
		v.LastUsedAt = &used
	}
	return v
}
func (m *Tokens) cleanupLocked(now time.Time) {
	for id, t := range m.entries {
		if !now.Before(t.ExpiresAt) || t.Revoked {
			t.cancel()
			t.timer.Stop()
			if now.Sub(t.ExpiresAt) > time.Hour || len(m.entries) >= maxTokens {
				delete(m.entries, id)
			}
		}
	}
}
func (m *Tokens) Create(owner int64, seconds int64, probe bool) (TokenView, string, error) {
	if seconds != 900 && seconds != 3600 && seconds != 21600 && seconds != 86400 {
		return TokenView{}, "", errors.New("无效有效期")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return TokenView{}, "", err
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return TokenView{}, "", err
	}
	plain := "lxdbg_" + base64.RawURLEncoding.EncodeToString(key)
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(now)
	active := 0
	for _, t := range m.entries {
		if !t.Revoked && now.Before(t.ExpiresAt) {
			active++
		}
	}
	if active >= maxActiveTokens || len(m.entries) >= maxTokens {
		return TokenView{}, "", errTokenLimit
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &token{TokenView: TokenView{ID: hex.EncodeToString(id), Scopes: []string{"read"}, CreatedAt: now, ExpiresAt: now.Add(time.Duration(seconds) * time.Second)}, owner: owner, digest: sha256.Sum256([]byte(plain)), ctx: ctx, cancel: cancel}
	if probe {
		t.Scopes = append(t.Scopes, "probe")
	}
	t.timer = time.AfterFunc(time.Until(t.ExpiresAt), cancel)
	m.entries[t.ID] = t
	m.auditLocked(t.ID, "created")
	return cloneView(t), plain, nil
}
func (m *Tokens) List() ([]TokenView, []Audit) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(time.Now())
	out := make([]TokenView, 0, len(m.entries))
	for _, t := range m.entries {
		out = append(out, cloneView(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	audit := append([]Audit{}, m.audit...)
	return out, audit
}
func (m *Tokens) Revoke(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.entries[id]
	if t == nil {
		return false
	}
	if !t.Revoked {
		t.Revoked = true
		t.cancel()
		t.timer.Stop()
		m.auditLocked(id, "revoked")
	}
	return true
}

// RevokeOwner 在管理接口删除/降权提交后取消在途工作，恢复管理员也不能恢复旧凭据。
func (m *Tokens) RevokeOwner(owner int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.entries {
		if t.owner == owner && !t.Revoked {
			t.Revoked = true
			t.cancel()
			t.timer.Stop()
			m.auditLocked(id, "owner_invalid")
		}
	}
}

// acquire 对通过密钥校验的调用限频；失败尝试不写入审计，避免攻击者撑大内存。
func (m *Tokens) acquire(plain string) (*token, TokenView, int) {
	if len(plain) != 49 {
		return nil, TokenView{}, 401
	}
	digest := sha256.Sum256([]byte(plain))
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(now)
	for _, t := range m.entries {
		if t.digest != digest || t.Revoked || !now.Before(t.ExpiresAt) || t.ctx.Err() != nil {
			continue
		}
		if now.Sub(t.window) >= time.Minute {
			t.window = now
			t.windowCalls = 0
		}
		if t.windowCalls >= callsPerMinute {
			return nil, TokenView{}, 429
		}
		t.windowCalls++
		t.Calls++
		t.LastUsedAt = &now
		m.auditLocked(t.ID, "used")
		return t, cloneView(t), 200
	}
	return nil, TokenView{}, 401
}

// Close 只用于进程/隔离测试退出；所有凭据失效。
func (m *Tokens) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.entries {
		t.cancel()
		t.timer.Stop()
	}
	m.entries = make(map[string]*token)
}
