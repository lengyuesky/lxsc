package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
)

// Migrations only add derived state or new tables; existing credentials remain valid.
func migrate(s *sql.DB) error {
	tx, err := s.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version > 4 {
		return fmt.Errorf("数据库版本 %d 高于当前程序支持版本", version)
	}
	if version < 1 {
		for _, statement := range []string{
			`ALTER TABLE api_keys ADD COLUMN id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE api_keys ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,
			`CREATE TABLE play_queues(user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE, ids TEXT NOT NULL, current_id TEXT NOT NULL, position INTEGER NOT NULL, changed_at INTEGER NOT NULL, changed_by TEXT NOT NULL)`,
			`CREATE TABLE ratings(user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, item_id TEXT NOT NULL, rating INTEGER NOT NULL CHECK(rating BETWEEN 1 AND 5), PRIMARY KEY(user_id,item_id))`,
			`CREATE TABLE now_playing(user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, client TEXT NOT NULL, track_id TEXT NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(user_id,client))`,
		} {
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
		rows, err := tx.Query(`SELECT key FROM api_keys`)
		if err != nil {
			return err
		}
		var keys []string
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, key)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, key := range keys {
			digest := keyDigest(key)
			if _, err = tx.Exec(`UPDATE api_keys SET key=?,id=? WHERE key=?`, digest, digest, key); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`CREATE UNIQUE INDEX idx_api_keys_id ON api_keys(id)`); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO schema_migrations(version) VALUES(1)`); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err = tx.Exec(`CREATE INDEX idx_listening_user_track ON listening_sessions(user_id,track_id)`); err != nil {
			return err
		}
		if _, err = tx.Exec(`CREATE TABLE playlist_subscriptions(playlist_id TEXT PRIMARY KEY REFERENCES playlists(id) ON DELETE CASCADE, source TEXT NOT NULL, remote_id TEXT NOT NULL, auto INTEGER NOT NULL, version INTEGER NOT NULL, seen TEXT NOT NULL DEFAULT '[]', next_at INTEGER NOT NULL DEFAULT 0, last_at INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '')`); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO schema_migrations(version) VALUES(2)`); err != nil {
			return err
		}
	}
	if version < 3 {
		for _, statement := range []string{
			`CREATE TABLE playlist_track_history(id INTEGER PRIMARY KEY AUTOINCREMENT, playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE, created_at INTEGER NOT NULL, ids TEXT NOT NULL)`,
			`CREATE INDEX idx_playlist_track_history ON playlist_track_history(playlist_id,id DESC)`,
			`INSERT INTO schema_migrations(version) VALUES(3)`,
		} {
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	if version < 4 {
		for _, statement := range []string{
			`CREATE TABLE board_snapshots(board_key TEXT PRIMARY KEY, raw TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
			`INSERT INTO schema_migrations(version) VALUES(4)`,
		} {
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func keyDigest(key string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(key))) }
