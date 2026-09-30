package db

import (
	"context"
	"errors"
	"time"
)

func (d *DB) SmartTrackIDs(ctx context.Context, userID int64, kind, singer string) ([]string, error) {
	var query string
	var args []any
	switch kind {
	case "frequent", "month":
		from, to := smartDateRange(kind, time.Now())
		query = `SELECT s.track_id FROM listening_sessions s JOIN listening_days d ON d.session_id=s.id WHERE s.user_id=? AND d.day>=? AND d.day<=? GROUP BY s.track_id HAVING SUM(d.milliseconds)>0 ORDER BY SUM(d.milliseconds) DESC,MAX(s.started_at) DESC,s.track_id LIMIT 100`
		args = []any{userID, from, to}
	case "rediscover":
		query = `SELECT s.item_id FROM stars s WHERE s.user_id=? AND s.kind='track' AND NOT EXISTS(SELECT 1 FROM history h WHERE h.user_id=s.user_id AND h.track_id=s.item_id AND h.played_at>=?) AND NOT EXISTS(SELECT 1 FROM listening_sessions ls JOIN listening_days ld ON ld.session_id=ls.id WHERE ls.user_id=s.user_id AND ls.track_id=s.item_id AND ld.day>=? AND ld.milliseconds>0) ORDER BY s.created_at,s.item_id LIMIT 100`
		args = []any{userID, now() - 30*86400, time.Now().In(ListeningZone).AddDate(0, 0, -29).Format("2006-01-02")}
	case "singer":
		query = `SELECT s.item_id FROM stars s JOIN tracks t ON t.id=s.item_id WHERE s.user_id=? AND s.kind='track' AND instr(lower(t.singer),lower(?))>0 ORDER BY s.created_at DESC,s.item_id LIMIT 100`
		args = []any{userID, singer}
	default:
		return nil, errors.New("不支持的智能歌单")
	}
	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func smartDateRange(kind string, at time.Time) (string, string) {
	today := at.In(ListeningZone)
	from := today.AddDate(0, 0, -29)
	if kind == "month" {
		from = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, ListeningZone)
	}
	return from.Format("2006-01-02"), today.Format("2006-01-02")
}

// HistoryRetention removes only raw history. Listening sessions and deduplication stay intact.
// Each batch is bounded to avoid holding the sole business writer for a long time.
func (d *DB) HistoryRetention(ctx context.Context, days int, apply bool) (int64, error) {
	if days < 90 || days > 3650 {
		return 0, errors.New("保留时间必须为90–3650天")
	}
	cutoff := now() - int64(days)*86400
	if !apply {
		var n int64
		err := d.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM history WHERE played_at<?`, cutoff).Scan(&n)
		return n, err
	}
	result, err := d.sql.ExecContext(ctx, `DELETE FROM history WHERE id IN (SELECT id FROM history WHERE played_at<? ORDER BY played_at LIMIT 10000)`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
