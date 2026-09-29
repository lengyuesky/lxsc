package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"
)

// initTrackSearch 在一个事务中建立派生索引并补齐旧数据，失败时不留下半份索引。
// 单独保存稳定整数主键，避免 VACUUM 改变 tracks 的隐含 rowid 后错配歌曲。
func initTrackSearch(s *sql.DB) error {
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tracks_search'`).Scan(&exists); err != nil {
		return err
	}
	_, err = tx.Exec(`
	CREATE TABLE IF NOT EXISTS track_search_keys(id INTEGER PRIMARY KEY, track_id TEXT NOT NULL UNIQUE);
	CREATE VIRTUAL TABLE IF NOT EXISTS tracks_search USING fts5(name, singer, album, tokenize='trigram');
	CREATE INDEX IF NOT EXISTS idx_tracks_updated_id ON tracks(updated_at DESC, id);
	CREATE TRIGGER IF NOT EXISTS tracks_search_insert AFTER INSERT ON tracks BEGIN
		INSERT INTO track_search_keys(track_id) VALUES(new.id);
		INSERT INTO tracks_search(rowid,name,singer,album)
			SELECT id,new.name,new.singer,new.album FROM track_search_keys WHERE track_id=new.id;
	END;
	CREATE TRIGGER IF NOT EXISTS tracks_search_delete AFTER DELETE ON tracks BEGIN
		DELETE FROM tracks_search WHERE rowid=(SELECT id FROM track_search_keys WHERE track_id=old.id);
		DELETE FROM track_search_keys WHERE track_id=old.id;
	END;
	CREATE TRIGGER IF NOT EXISTS tracks_search_update AFTER UPDATE OF id,name,singer,album ON tracks
	WHEN old.id IS NOT new.id OR old.name IS NOT new.name OR old.singer IS NOT new.singer OR old.album IS NOT new.album BEGIN
		DELETE FROM tracks_search WHERE rowid=(SELECT id FROM track_search_keys WHERE track_id=old.id);
		UPDATE track_search_keys SET track_id=new.id WHERE track_id=old.id;
		INSERT INTO tracks_search(rowid,name,singer,album)
			SELECT id,new.name,new.singer,new.album FROM track_search_keys WHERE track_id=new.id;
	END;`)
	if err != nil {
		return fmt.Errorf("创建歌曲搜索索引失败: %w", err)
	}
	if exists == 0 {
		if _, err := tx.Exec(`INSERT INTO track_search_keys(track_id) SELECT id FROM tracks;
			INSERT INTO tracks_search(rowid,name,singer,album)
			SELECT k.id,t.name,t.singer,t.album FROM tracks t JOIN track_search_keys k ON k.track_id=t.id;`); err != nil {
			return fmt.Errorf("初始化歌曲搜索索引失败: %w", err)
		}
	}
	return tx.Commit()
}

func trackSearchQuery(q string, limit, offset int) (string, []any) {
	query := `SELECT id, source, name, singer, album, json, updated_at FROM tracks`
	args := []any{}
	if q != "" {
		query += ` WHERE `
		// 三字片段用于缩小候选集合；原 LIKE 仍负责最终语义校验。
		// 短词、通配符和 NUL 保留原匹配方式，不能因分词规则漏掉结果。
		if utf8.RuneCountInString(q) >= 3 && !strings.ContainsAny(q, "%_\x00") {
			query += `id IN (SELECT k.track_id FROM tracks_search JOIN track_search_keys k ON k.id=tracks_search.rowid WHERE tracks_search MATCH ?) AND `
			args = append(args, `"`+strings.ReplaceAll(q, `"`, `""`)+`"`)
		}
		query += `(name LIKE ? OR singer LIKE ? OR album LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	query += ` ORDER BY updated_at DESC, id LIMIT ? OFFSET ?`
	return query, append(args, max(0, limit), max(0, offset))
}

// SearchTracks 使用三字子串索引；空查询直接按稳定索引分页。
func (d *DB) SearchTracks(ctx context.Context, q string, limit, offset int) ([]*Track, error) {
	query, args := trackSearchQuery(q, limit, offset)
	rows, err := d.read.QueryContext(ctx, query, args...)
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
