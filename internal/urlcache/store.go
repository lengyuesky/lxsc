// Package urlcache 保存可重建的直链缓存，数据库独立于业务数据和备份。
package urlcache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const Filename = "url-cache.db"

// Record 使用绝对时间，ExpiresAt 为 0 表示不按时间过期。
type Record struct {
	TrackID, Quality, URL, ResolvedQuality, Source string
	SourceID                                       int64
	CreatedAt, ExpiresAt, LastUsedAt               int64
}

type Store struct {
	db   *sql.DB
	path string
}

// ResetFiles 仅在缓存连接打开前调用，备份恢复和回滚均丢弃可重建的缓存。
func ResetFiles(dataDir string) error {
	var errs []error
	for _, suffix := range []string{"", "-wal", "-shm", ".invalid"} {
		if err := os.Remove(filepath.Join(dataDir, Filename) + suffix); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Open 打开独立数据库。上次持久化出错时，先丢弃旧库再重新启用。
func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, Filename)
	if _, err := os.Stat(path + ".invalid"); err == nil {
		if err := ResetFiles(dataDir); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	database, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(250)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	s := &Store{db: database, path: path}
	// 首次连接需要初始化 SQLite、启用 WAL 并执行多条建表/迁移语句。
	// 这发生在接收请求之前，不能套用播放路径单次 I/O 的一秒预算；
	// 慢磁盘或竞态检测下的调度延迟不应让启动误判为缓存损坏。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version int
	if err := database.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		_ = s.Discard()
		return nil, fmt.Errorf("读取直链缓存版本失败: %w", err)
	}
	statements := []string{`CREATE TABLE IF NOT EXISTS cache_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`}
	if version != 2 {
		// 旧格式只有脚本名，不能推断可信 ID；只重建独立缓存，不迁移业务数据库。
		statements = append(statements, `DROP TABLE IF EXISTS playback_urls`, `DELETE FROM cache_meta`)
	}
	statements = append(statements,
		`CREATE TABLE IF NOT EXISTS playback_urls (
			track_id TEXT NOT NULL, quality TEXT NOT NULL, url TEXT NOT NULL,
			resolved_quality TEXT NOT NULL, source TEXT NOT NULL, source_id INTEGER NOT NULL,
			created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL,
			PRIMARY KEY (track_id, quality))`,
		`PRAGMA user_version = 2`,
	)
	for _, stmt := range statements {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			_ = s.Discard()
			return nil, fmt.Errorf("初始化直链缓存失败: %w", err)
		}
	}
	return s, nil
}

// 独立缓存不应长时间阻塞播放或设置保存。
func ioContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Second)
}

// Load 清理过期及超容量记录，并按从旧到新的使用顺序恢复。
func (s *Store) Load(fingerprint string, capacity int, now time.Time) ([]Record, error) {
	ctx, cancel := ioContext()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT value FROM cache_meta WHERE key='fingerprint'`).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if old != fingerprint {
		if err := reset(ctx, tx, fingerprint); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playback_urls WHERE source_id <= 0 OR (expires_at > 0 AND expires_at <= ?)`, now.UnixMilli()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playback_urls WHERE rowid IN (
		SELECT rowid FROM playback_urls ORDER BY last_used_at DESC, track_id DESC, quality DESC LIMIT -1 OFFSET ?)`, capacity); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT track_id, quality, url, resolved_quality, source, source_id, created_at, expires_at, last_used_at
		FROM playback_urls ORDER BY last_used_at, track_id, quality`)
	if err != nil {
		return nil, err
	}
	var records []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.TrackID, &r.Quality, &r.URL, &r.ResolvedQuality, &r.Source, &r.SourceID, &r.CreatedAt, &r.ExpiresAt, &r.LastUsedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		records = append(records, r)
	}
	readErr := rows.Err()
	_ = rows.Close()
	if readErr != nil {
		return nil, readErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return records, nil
}

func reset(ctx context.Context, tx *sql.Tx, fingerprint string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM playback_urls`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO cache_meta(key,value) VALUES('fingerprint',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fingerprint)
	return err
}

func (s *Store) Reset(fingerprint string) error {
	ctx, cancel := ioContext()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := reset(ctx, tx, fingerprint); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Put(r Record) error {
	ctx, cancel := ioContext()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO playback_urls
		(track_id,quality,url,resolved_quality,source,source_id,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(track_id,quality) DO UPDATE SET url=excluded.url, resolved_quality=excluded.resolved_quality,
		source=excluded.source, source_id=excluded.source_id, created_at=excluded.created_at, expires_at=excluded.expires_at, last_used_at=excluded.last_used_at`,
		r.TrackID, r.Quality, r.URL, r.ResolvedQuality, r.Source, r.SourceID, r.CreatedAt, r.ExpiresAt, r.LastUsedAt)
	return err
}

func (s *Store) Touch(trackID, quality string, now time.Time) error {
	return s.TouchBatch([]TouchUpdate{{TrackID: trackID, Quality: quality, At: now}})
}

type TouchUpdate struct {
	TrackID, Quality string
	At               time.Time
}

// 只推进现存记录的使用时间；迟到的更新不能复活已删除记录或倒退新直链的时间。
func (s *Store) TouchBatch(updates []TouchUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	ctx, cancel := ioContext()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE playback_urls SET last_used_at=? WHERE track_id=? AND quality=? AND last_used_at<?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, update := range updates {
		at := update.At.UnixNano()
		if _, err := stmt.ExecContext(ctx, at, update.TrackID, update.Quality, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Delete(trackID, quality string) error {
	ctx, cancel := ioContext()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM playback_urls WHERE track_id=? AND quality=?`, trackID, quality)
	return err
}

// Discard 出错后停止访问旧库；标记保证下次启动先清空再恢复持久化。
func (s *Store) Discard() error {
	markErr := os.WriteFile(s.path+".invalid", []byte("直链缓存需要重建\n"), 0o600)
	return errors.Join(markErr, s.db.Close())
}

func (s *Store) Close() error { return s.db.Close() }
