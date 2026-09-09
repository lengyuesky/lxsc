package music

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"lxsc/internal/js"
	"lxsc/internal/urlcache"
)

const URLCheckTimeout = 3 * time.Second

type urlCheckKey struct {
	key   urlKey
	token cacheToken
}

// CheckPlaybackURL 仅合并同一解析版本的在途校验，不缓存校验结果。
func (c *Catalog) CheckPlaybackURL(ctx context.Context, resolution URLResolution, check func(context.Context) (int, error)) (int, error) {
	result, err := c.urlChecks.load(ctx, urlCheckKey{resolution.key, resolution.token}, nil, URLCheckTimeout, func(ctx context.Context) (int, bool, error) {
		status, err := check(ctx)
		return status, false, err
	})
	return result.value, err
}

type urlPersistence struct {
	store       *urlcache.Store
	fingerprint func() (string, error)
	log         *slog.Logger
}

// EnableURLCache 必须在音源加载和目录导入完成后、接收请求前调用。
func (c *Catalog) EnableURLCache(dataDir, proxy string) error {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	c.urls.mu.Lock()
	defer c.urls.mu.Unlock()
	if c.urls.persistent != nil {
		return errors.New("直链持久缓存已启用")
	}
	p := &urlPersistence{fingerprint: func() (string, error) { return c.urlCacheFingerprint(proxy) }, log: c.Log}
	fingerprint, err := p.fingerprint()
	if err != nil {
		return err
	}
	p.store, err = urlcache.Open(dataDir)
	if err != nil {
		return err
	}
	capacity := c.urls.capacity
	if c.urls.ttl == 0 {
		capacity = 0
	}
	records, err := p.store.Load(fingerprint, capacity, c.urls.now())
	if err != nil {
		p.disable("加载")
		return err
	}
	c.urls.generation++
	c.urls.entries.Purge()
	for _, record := range records {
		c.urls.serial++
		entry := timedEntry[js.MusicURLResult]{
			result: cachedResult[js.MusicURLResult]{
				value: js.MusicURLResult{URL: record.URL, Quality: record.ResolvedQuality, Source: record.Source},
				token: cacheToken{generation: c.urls.generation, serial: c.urls.serial},
			},
			created: time.UnixMilli(record.CreatedAt),
		}
		if record.ExpiresAt != 0 {
			entry.expires = time.UnixMilli(record.ExpiresAt)
		}
		c.urls.entries.Add(urlKey{record.TrackID, record.Quality}, entry)
	}
	c.urls.persistent = p
	return nil
}

func (c *Catalog) CloseURLCache() error { return c.urls.close() }

// 指纹不包含每次启动都会变化的加载时间，确保普通重启可以复用缓存。
func (c *Catalog) urlCacheFingerprint(proxy string) (string, error) {
	type sourceState struct {
		ID        int64
		Enabled   bool
		Priority  int
		Script    string
		State     string
		Platforms map[string]js.PlatformCap
	}
	state := struct {
		Version int
		TTL     int
		Proxy   string
		Sources []sourceState
	}{Version: 1, TTL: c.Settings.Get().URLCacheTTL, Proxy: proxy}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sources, err := c.DB.ListSources(ctx)
	if err != nil {
		return "", err
	}
	for _, source := range sources {
		scriptHash := sha256.Sum256([]byte(source.Script))
		value := sourceState{ID: source.ID, Enabled: source.Enabled, Priority: source.Priority, Script: hex.EncodeToString(scriptHash[:])}
		if c.Sources != nil {
			if status := c.Sources.StatusOf(source.ID); status != nil {
				value.State, value.Platforms = status.State, status.Platforms
			}
		}
		state.Sources = append(state.Sources, value)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

func (p *urlPersistence) put(key urlKey, entry timedEntry[js.MusicURLResult]) error {
	var expires int64
	if !entry.expires.IsZero() {
		expires = entry.expires.UnixMilli()
	}
	return p.store.Put(urlcache.Record{
		TrackID: key.trackID, Quality: key.quality, URL: entry.result.value.URL,
		ResolvedQuality: entry.result.value.Quality, Source: entry.result.value.Source,
		CreatedAt: entry.created.UnixMilli(), ExpiresAt: expires, LastUsedAt: entry.created.UnixNano(),
	})
}

func (p *urlPersistence) touch(key urlKey, now time.Time) error {
	return p.store.Touch(key.trackID, key.quality, now)
}

func (p *urlPersistence) remove(key urlKey) error { return p.store.Delete(key.trackID, key.quality) }

func (p *urlPersistence) clear() error {
	fingerprint, err := p.fingerprint()
	if err != nil {
		return err
	}
	return p.store.Reset(fingerprint)
}

func (p *urlPersistence) disable(stage string) {
	err := p.store.Discard()
	if p.log != nil {
		p.log.Warn("直链持久缓存不可用，改用内存缓存", "stage", stage, "resetPending", err == nil)
	}
}

func (p *urlPersistence) close() error { return p.store.Close() }
