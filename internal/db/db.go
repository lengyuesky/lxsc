// Package db 封装 SQLite 数据访问
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// DB 数据库句柄
type DB struct {
	sql  *sql.DB
	path string
}

// Stats 是概览与备份清单使用的数据统计。
type Stats struct {
	Users     int   `json:"users"`
	Playlists int   `json:"playlists"`
	Tracks    int   `json:"tracks"`
	Albums    int   `json:"albums"`
	Artists   int   `json:"artists"`
	Sources   int   `json:"sources"`
	Stars     int   `json:"stars"`
	History   int   `json:"history"`
	SizeBytes int64 `json:"sizeBytes"`
}

// ErrNotFound 记录不存在
var ErrNotFound = errors.New("not found")

// Open 打开（或创建）数据库并初始化表结构
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	s, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s.SetMaxOpenConns(1)
	s.SetConnMaxLifetime(0)
	for _, stmt := range strings.Split(schema, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.Exec(stmt); err != nil {
			s.Close()
			return nil, fmt.Errorf("初始化表结构失败: %w (%s)", err, stmt)
		}
	}
	return &DB{sql: s, path: path}, nil
}

// Snapshot 使用 SQLite VACUUM INTO 生成包含 WAL 中最新数据的一致快照。
func (d *DB) Snapshot(ctx context.Context, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	_ = os.Remove(target)
	// 使用独立连接执行快照，避免占用业务连接池中唯一的连接。
	snapshotDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", filepath.ToSlash(d.path)))
	if err != nil {
		return err
	}
	defer snapshotDB.Close()
	snapshotDB.SetMaxOpenConns(1)
	_, err = snapshotDB.ExecContext(ctx, `VACUUM INTO ?`, target)
	return err
}

// Statistics 返回核心数据量和数据库占用空间。
func (d *DB) Statistics(ctx context.Context) (Stats, error) {
	var out Stats
	for table, target := range map[string]*int{
		"users": &out.Users, "playlists": &out.Playlists, "tracks": &out.Tracks,
		"albums": &out.Albums, "artists": &out.Artists, "sources": &out.Sources,
		"stars": &out.Stars, "history": &out.History,
	} {
		if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(target); err != nil {
			return out, err
		}
	}
	if info, err := os.Stat(d.path); err == nil {
		out.SizeBytes = info.Size()
	}
	return out, nil
}

// Close 关闭
func (d *DB) Close() error { return d.sql.Close() }

func now() int64 { return time.Now().Unix() }

// ---------- settings ----------

// GetSetting 读取设置，不存在返回 def
func (d *DB) GetSetting(ctx context.Context, key, def string) string {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

// SetSetting 写入设置
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SetSettings 原子写入一批设置，任一写入失败都会回滚。
func (d *DB) SetSettings(ctx context.Context, values map[string]string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, values[key]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AllSettings 读取全部设置
func (d *DB) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// ---------- users ----------

// User 用户
type User struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	PasswordEnc string `json:"-"`
	IsAdmin     bool   `json:"isAdmin"`
	Quality     string `json:"quality"`
	CreatedAt   int64  `json:"createdAt"`
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var admin int
	if err := row.Scan(&u.ID, &u.Name, &u.PasswordEnc, &admin, &u.Quality, &u.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.IsAdmin = admin == 1
	return &u, nil
}

const userCols = `id, name, password_enc, is_admin, quality, created_at`

// GetUserByName 按用户名查找
func (d *DB) GetUserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(d.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE name = ?`, name))
}

// GetUserByID 按 ID 查找
func (d *DB) GetUserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(d.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// ListUsers 列出全部用户
func (d *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CreateUser 创建用户
func (d *DB) CreateUser(ctx context.Context, name, passwordEnc string, isAdmin bool, quality string) (*User, error) {
	if quality == "" {
		quality = "320k"
	}
	res, err := d.sql.ExecContext(ctx, `INSERT INTO users(name, password_enc, is_admin, quality, created_at) VALUES(?,?,?,?,?)`, name, passwordEnc, boolInt(isAdmin), quality, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return d.GetUserByID(ctx, id)
}

// UpdateUser 更新用户（passwordEnc 为空则不改口令）
func (d *DB) UpdateUser(ctx context.Context, id int64, name, passwordEnc string, isAdmin bool, quality string) error {
	if passwordEnc != "" {
		_, err := d.sql.ExecContext(ctx, `UPDATE users SET name=?, password_enc=?, is_admin=?, quality=? WHERE id=?`, name, passwordEnc, boolInt(isAdmin), quality, id)
		return err
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE users SET name=?, is_admin=?, quality=? WHERE id=?`, name, boolInt(isAdmin), quality, id)
	return err
}

// UpdateUserQuality 更新用户的默认播放音质。
func (d *DB) UpdateUserQuality(ctx context.Context, id int64, quality string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE users SET quality=? WHERE id=?`, quality, id)
	return err
}

// DeleteUser 删除用户
func (d *DB) DeleteUser(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	return err
}

// CountUsers 用户数
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// GetUserByAPIKey 按 API Key 查找用户
func (d *DB) GetUserByAPIKey(ctx context.Context, key string) (*User, error) {
	return scanUser(d.sql.QueryRowContext(ctx, `SELECT u.id, u.name, u.password_enc, u.is_admin, u.quality, u.created_at FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.key = ?`, key))
}

// CreateAPIKey 创建 API Key
func (d *DB) CreateAPIKey(ctx context.Context, userID int64, key, label string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO api_keys(key, user_id, label, created_at) VALUES(?,?,?,?)`, key, userID, label, now())
	return err
}

// ---------- sources ----------

// Source 音源脚本记录
type Source struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      string `json:"author"`
	Homepage    string `json:"homepage"`
	Script      string `json:"-"`
	Enabled     bool   `json:"enabled"`
	Priority    int    `json:"priority"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
}

const sourceCols = `id, name, description, version, author, homepage, script, enabled, priority, created_at, updated_at`

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	var s Source
	var enabled int
	if err := row.Scan(&s.ID, &s.Name, &s.Description, &s.Version, &s.Author, &s.Homepage, &s.Script, &enabled, &s.Priority, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.Enabled = enabled == 1
	return &s, nil
}

// ListSources 按优先级列出音源
func (d *DB) ListSources(ctx context.Context) ([]*Source, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSource 获取音源
func (d *DB) GetSource(ctx context.Context, id int64) (*Source, error) {
	return scanSource(d.sql.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM sources WHERE id=?`, id))
}

// CreateSource 新增音源
func (d *DB) CreateSource(ctx context.Context, s *Source) (*Source, error) {
	t := now()
	res, err := d.sql.ExecContext(ctx, `INSERT INTO sources(name, description, version, author, homepage, script, enabled, priority, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		s.Name, s.Description, s.Version, s.Author, s.Homepage, s.Script, boolInt(s.Enabled), s.Priority, t, t)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return d.GetSource(ctx, id)
}

// UpdateSource 更新音源（script 为空则不更新脚本）
func (d *DB) UpdateSource(ctx context.Context, s *Source) error {
	if s.Script != "" {
		_, err := d.sql.ExecContext(ctx, `UPDATE sources SET name=?, description=?, version=?, author=?, homepage=?, script=?, enabled=?, priority=?, updated_at=? WHERE id=?`,
			s.Name, s.Description, s.Version, s.Author, s.Homepage, s.Script, boolInt(s.Enabled), s.Priority, now(), s.ID)
		return err
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE sources SET name=?, enabled=?, priority=?, updated_at=? WHERE id=?`, s.Name, boolInt(s.Enabled), s.Priority, now(), s.ID)
	return err
}

// DeleteSource 删除音源
func (d *DB) DeleteSource(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id)
	return err
}

// ---------- tracks ----------

// Track 歌曲元数据
type Track struct {
	ID        string
	Source    string
	Name      string
	Singer    string
	Album     string
	JSON      []byte
	UpdatedAt int64
}

// MetadataCleanup 是清理无引用元数据的结果。
type MetadataCleanup struct {
	Tracks  int `json:"tracks"`
	Albums  int `json:"albums"`
	Artists int `json:"artists"`
}

// CleanupUnreferencedMetadata 删除未被歌单、收藏、播放历史或已收藏专辑引用的缓存元数据。
func (d *DB) CleanupUnreferencedMetadata(ctx context.Context) (MetadataCleanup, error) {
	var out MetadataCleanup
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM tracks
		WHERE NOT EXISTS (SELECT 1 FROM playlist_tracks p WHERE p.track_id = tracks.id)
		  AND NOT EXISTS (SELECT 1 FROM stars s WHERE s.kind = 'track' AND s.item_id = tracks.id)
		  AND NOT EXISTS (SELECT 1 FROM history h WHERE h.track_id = tracks.id)
		  AND NOT EXISTS (
			SELECT 1 FROM stars s
			JOIN albums a ON a.id = s.item_id
			JOIN json_each(CASE WHEN json_valid(a.json) THEN a.json ELSE '[]' END) j
			WHERE s.kind = 'album' AND CAST(j.value AS TEXT) = tracks.id
		  )`)
	if err != nil {
		return out, err
	}
	if n, err := res.RowsAffected(); err == nil {
		out.Tracks = int(n)
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM albums
		WHERE NOT EXISTS (SELECT 1 FROM stars s WHERE s.kind = 'album' AND s.item_id = albums.id)`)
	if err != nil {
		return out, err
	}
	if n, err := res.RowsAffected(); err == nil {
		out.Albums = int(n)
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM artists
		WHERE NOT EXISTS (SELECT 1 FROM stars s WHERE s.kind = 'artist' AND s.item_id = artists.id)`)
	if err != nil {
		return out, err
	}
	if n, err := res.RowsAffected(); err == nil {
		out.Artists = int(n)
	}
	if err := tx.Commit(); err != nil {
		return MetadataCleanup{}, err
	}
	return out, nil
}

// Compact 回收 SQLite 空闲页并截断 WAL，实际释放磁盘空间。
func (d *DB) Compact(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	_, err := d.sql.ExecContext(ctx, `VACUUM`)
	return err
}

// UpsertTracks 批量写入歌曲元数据
func (d *DB) UpsertTracks(ctx context.Context, tracks []Track) error {
	if len(tracks) == 0 {
		return nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO tracks(id, source, name, singer, album, json, updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET source=excluded.source, name=excluded.name, singer=excluded.singer, album=excluded.album, json=excluded.json, updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	t := now()
	for _, tr := range tracks {
		if _, err := stmt.ExecContext(ctx, tr.ID, tr.Source, tr.Name, tr.Singer, tr.Album, string(tr.JSON), t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetTrack 读取歌曲元数据
func (d *DB) GetTrack(ctx context.Context, id string) (*Track, error) {
	var t Track
	var js string
	err := d.sql.QueryRowContext(ctx, `SELECT id, source, name, singer, album, json, updated_at FROM tracks WHERE id=?`, id).Scan(&t.ID, &t.Source, &t.Name, &t.Singer, &t.Album, &js, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.JSON = []byte(js)
	return &t, nil
}

// GetTracks 批量读取，保持传入顺序，缺失的跳过
func (d *DB) GetTracks(ctx context.Context, ids []string) ([]*Track, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]*Track, 0, len(ids))
	const chunk = 500
	m := map[string]*Track{}
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[i:end]
		q := `SELECT id, source, name, singer, album, json, updated_at FROM tracks WHERE id IN (?` + strings.Repeat(",?", len(part)-1) + `)`
		args := make([]any, len(part))
		for j, id := range part {
			args[j] = id
		}
		rows, err := d.sql.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t Track
			var js string
			if err := rows.Scan(&t.ID, &t.Source, &t.Name, &t.Singer, &t.Album, &js, &t.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			t.JSON = []byte(js)
			m[t.ID] = &t
		}
		rows.Close()
	}
	for _, id := range ids {
		if t, ok := m[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// SearchTracks 在本地库中按名称/歌手/专辑模糊搜索
func (d *DB) SearchTracks(ctx context.Context, q string, limit, offset int) ([]*Track, error) {
	like := "%" + q + "%"
	rows, err := d.sql.QueryContext(ctx, `SELECT id, source, name, singer, album, json, updated_at FROM tracks WHERE name LIKE ? OR singer LIKE ? OR album LIKE ? ORDER BY updated_at DESC LIMIT ? OFFSET ?`, like, like, like, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Track
	for rows.Next() {
		var t Track
		var js string
		if err := rows.Scan(&t.ID, &t.Source, &t.Name, &t.Singer, &t.Album, &js, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.JSON = []byte(js)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// ---------- albums / artists 缓存 ----------

// UpsertMeta 写入专辑或歌手缓存（table 为 albums 或 artists）
func (d *DB) UpsertAlbum(ctx context.Context, id, source, name, artist string, js []byte) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO albums(id, source, name, artist, json, updated_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, artist=excluded.artist, json=excluded.json, updated_at=excluded.updated_at`, id, source, name, artist, string(js), now())
	return err
}

// GetAlbum 读取专辑缓存
func (d *DB) GetAlbum(ctx context.Context, id string) (source, name, artist string, js []byte, err error) {
	var s string
	err = d.sql.QueryRowContext(ctx, `SELECT source, name, artist, json FROM albums WHERE id=?`, id).Scan(&source, &name, &artist, &s)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return source, name, artist, []byte(s), err
}

// UpsertArtist 写入歌手缓存
func (d *DB) UpsertArtist(ctx context.Context, id, source, name string, js []byte) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO artists(id, source, name, json, updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, json=excluded.json, updated_at=excluded.updated_at`, id, source, name, string(js), now())
	return err
}

// GetArtist 读取歌手缓存
func (d *DB) GetArtist(ctx context.Context, id string) (source, name string, js []byte, err error) {
	var s string
	err = d.sql.QueryRowContext(ctx, `SELECT source, name, json FROM artists WHERE id=?`, id).Scan(&source, &name, &s)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return source, name, []byte(s), err
}

// ---------- playlists ----------

// Playlist 歌单
type Playlist struct {
	ID        string
	UserID    int64
	Owner     string
	Name      string
	Comment   string
	Public    bool
	CreatedAt int64
	UpdatedAt int64
	Count     int
	TrackIDs  []string
}

const playlistListCols = `p.id, p.user_id, u.name, p.name, p.comment, p.public, p.created_at, p.updated_at,
	(SELECT COUNT(*) FROM playlist_tracks t WHERE t.playlist_id = p.id)`

func scanPlaylists(rows *sql.Rows) ([]*Playlist, error) {
	var out []*Playlist
	for rows.Next() {
		var p Playlist
		var pub int
		if err := rows.Scan(&p.ID, &p.UserID, &p.Owner, &p.Name, &p.Comment, &pub, &p.CreatedAt, &p.UpdatedAt, &p.Count); err != nil {
			return nil, err
		}
		p.Public = pub == 1
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ListPlaylists 列出用户可见歌单（自己的 + 公开的）
func (d *DB) ListPlaylists(ctx context.Context, userID int64) ([]*Playlist, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+playlistListCols+`
		FROM playlists p JOIN users u ON u.id = p.user_id
		WHERE p.user_id = ? OR p.public = 1
		ORDER BY CASE WHEN p.user_id = ? THEN 0 ELSE 1 END, p.updated_at DESC, p.created_at DESC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlaylists(rows)
}

// ListAllPlaylists 列出全部歌单；ownerID 大于 0 时仅列出指定用户的歌单
func (d *DB) ListAllPlaylists(ctx context.Context, ownerID int64) ([]*Playlist, error) {
	query := `SELECT ` + playlistListCols + ` FROM playlists p JOIN users u ON u.id = p.user_id`
	var args []any
	if ownerID > 0 {
		query += ` WHERE p.user_id = ?`
		args = append(args, ownerID)
	}
	query += ` ORDER BY p.updated_at DESC, p.created_at DESC`
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlaylists(rows)
}

// GetPlaylist 读取歌单及其歌曲 ID
func (d *DB) GetPlaylist(ctx context.Context, id string) (*Playlist, error) {
	var p Playlist
	var pub int
	err := d.sql.QueryRowContext(ctx, `SELECT p.id, p.user_id, u.name, p.name, p.comment, p.public, p.created_at, p.updated_at FROM playlists p JOIN users u ON u.id = p.user_id WHERE p.id = ?`, id).
		Scan(&p.ID, &p.UserID, &p.Owner, &p.Name, &p.Comment, &pub, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Public = pub == 1
	rows, err := d.sql.QueryContext(ctx, `SELECT track_id FROM playlist_tracks WHERE playlist_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, err
		}
		p.TrackIDs = append(p.TrackIDs, tid)
	}
	p.Count = len(p.TrackIDs)
	return &p, rows.Err()
}

// CreatePlaylist 创建歌单
func (d *DB) CreatePlaylist(ctx context.Context, id string, userID int64, name string, trackIDs []string) error {
	return d.CreatePlaylistFull(ctx, id, userID, name, "", false, trackIDs)
}

// CreatePlaylistFull 创建带完整元数据的歌单
func (d *DB) CreatePlaylistFull(ctx context.Context, id string, userID int64, name, comment string, public bool, trackIDs []string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO playlists(id, user_id, name, comment, public, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`, id, userID, name, comment, boolInt(public), t, t); err != nil {
		return err
	}
	for i, tid := range trackIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO playlist_tracks(playlist_id, position, track_id) VALUES(?,?,?)`, id, i, tid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplacePlaylistTracks 整体替换歌单歌曲
func (d *DB) ReplacePlaylistTracks(ctx context.Context, id string, trackIDs []string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_tracks WHERE playlist_id = ?`, id); err != nil {
		return err
	}
	for i, tid := range trackIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO playlist_tracks(playlist_id, position, track_id) VALUES(?,?,?)`, id, i, tid); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// UpdatePlaylistMeta 更新名称/备注/公开
func (d *DB) UpdatePlaylistMeta(ctx context.Context, id string, name, comment *string, public *bool) error {
	sets := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if name != nil {
		sets = append(sets, "name=?")
		args = append(args, *name)
	}
	if comment != nil {
		sets = append(sets, "comment=?")
		args = append(args, *comment)
	}
	if public != nil {
		sets = append(sets, "public=?")
		args = append(args, boolInt(*public))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), id)
	res, err := d.sql.ExecContext(ctx, `UPDATE playlists SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePlaylist 删除歌单
func (d *DB) DeletePlaylist(ctx context.Context, id string) error {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM playlists WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- stars ----------

// Star 收藏
func (d *DB) Star(ctx context.Context, userID int64, itemID, kind string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT OR IGNORE INTO stars(user_id, item_id, kind, created_at) VALUES(?,?,?,?)`, userID, itemID, kind, now())
	return err
}

// Unstar 取消收藏
func (d *DB) Unstar(ctx context.Context, userID int64, itemID string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM stars WHERE user_id=? AND item_id=?`, userID, itemID)
	return err
}

// StarredItem 收藏项
type StarredItem struct {
	ItemID    string
	Kind      string
	CreatedAt int64
}

// ListStarred 列出收藏（kind 为空表示全部）
func (d *DB) ListStarred(ctx context.Context, userID int64, kind string) ([]StarredItem, error) {
	var rows *sql.Rows
	var err error
	if kind == "" {
		rows, err = d.sql.QueryContext(ctx, `SELECT item_id, kind, created_at FROM stars WHERE user_id=? ORDER BY created_at DESC`, userID)
	} else {
		rows, err = d.sql.QueryContext(ctx, `SELECT item_id, kind, created_at FROM stars WHERE user_id=? AND kind=? ORDER BY created_at DESC`, userID, kind)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StarredItem
	for rows.Next() {
		var s StarredItem
		if err := rows.Scan(&s.ItemID, &s.Kind, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StarredSet 返回用户收藏 ID → 时间 的映射
func (d *DB) StarredSet(ctx context.Context, userID int64) (map[string]int64, error) {
	items, err := d.ListStarred(ctx, userID, "")
	if err != nil {
		return nil, err
	}
	m := make(map[string]int64, len(items))
	for _, it := range items {
		m[it.ItemID] = it.CreatedAt
	}
	return m, nil
}

// ---------- history ----------

// AddHistory 记录播放
func (d *DB) AddHistory(ctx context.Context, userID int64, trackID string, at int64) error {
	if at <= 0 {
		at = now()
	}
	_, err := d.sql.ExecContext(ctx, `INSERT INTO history(user_id, track_id, played_at) VALUES(?,?,?)`, userID, trackID, at)
	return err
}

// RecentTracks 最近播放（去重）
func (d *DB) RecentTracks(ctx context.Context, userID int64, limit int) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT track_id, MAX(played_at) m FROM history WHERE user_id=? GROUP BY track_id ORDER BY m DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var m int64
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FrequentTracks 最常播放
func (d *DB) FrequentTracks(ctx context.Context, userID int64, limit int) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT track_id, COUNT(*) c FROM history WHERE user_id=? GROUP BY track_id ORDER BY c DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var c int
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PlayCounts 返回歌曲播放次数
func (d *DB) PlayCounts(ctx context.Context, userID int64, ids []string) (map[string]int, error) {
	m := map[string]int{}
	if len(ids) == 0 {
		return m, nil
	}
	q := `SELECT track_id, COUNT(*) FROM history WHERE user_id=? AND track_id IN (?` + strings.Repeat(",?", len(ids)-1) + `) GROUP BY track_id`
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c int
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		m[id] = c
	}
	return m, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
