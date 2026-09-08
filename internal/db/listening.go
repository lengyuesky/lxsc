package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ListeningZone 统一按北京时间归日，不依赖服务器或浏览器本地时区。
var ListeningZone = time.FixedZone("Asia/Shanghai", 8*60*60)
var ErrListeningProgress = errors.New("听歌进度无效或与原会话不一致")
var listeningKey = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

// ListeningProgress 使用各日累计值，使重试和乱序上报保持幂等。
type ListeningProgress struct {
	UserID    int64            `json:"userId"`
	SessionID string           `json:"sessionId"`
	TrackID   string           `json:"trackId"`
	StartedAt int64            `json:"startedAt"`
	Days      map[string]int64 `json:"days"`
}

type ListeningTrack struct{ ID, Name, Singer string }

func (d *DB) ListeningEnabledAt(ctx context.Context) (int64, error) {
	var at int64
	err := d.sql.QueryRowContext(ctx, `SELECT enabled_at FROM listening_meta WHERE id=1`).Scan(&at)
	return at, err
}

// SaveListeningProgress 在同一事务中校验归属并合并每日最大累计值。
func (d *DB) SaveListeningProgress(ctx context.Context, p ListeningProgress, track ListeningTrack, at time.Time) error {
	if !listeningKey.MatchString(p.SessionID) || p.TrackID == "" || p.TrackID != track.ID || p.UserID <= 0 || len(p.Days) == 0 || len(p.Days) > 367 {
		return ErrListeningProgress
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled int64
	if err = tx.QueryRowContext(ctx, `SELECT enabled_at FROM listening_meta WHERE id=1`).Scan(&enabled); err != nil {
		return err
	}
	nowMS := at.UnixMilli()
	if p.StartedAt < enabled || p.StartedAt > nowMS || nowMS-p.StartedAt > int64(366*24*time.Hour/time.Millisecond) {
		return ErrListeningProgress
	}
	for day, ms := range p.Days {
		date, e := time.ParseInLocation("2006-01-02", day, ListeningZone)
		if e != nil || date.Format("2006-01-02") != day || ms < 0 {
			return ErrListeningProgress
		}
		begin, end := max(date.UnixMilli(), p.StartedAt), min(date.AddDate(0, 0, 1).UnixMilli(), nowMS)
		if end < begin || ms > end-begin {
			return ErrListeningProgress
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO listening_sessions(user_id,session_key,source,track_id,name,singer,started_at) VALUES(?,?,'web',?,?,?,?) ON CONFLICT(user_id,source,session_key) DO NOTHING`, p.UserID, p.SessionID, p.TrackID, track.Name, track.Singer, p.StartedAt)
	if err != nil {
		return err
	}
	var id, started int64
	var trackID string
	if err = tx.QueryRowContext(ctx, `SELECT id,track_id,started_at FROM listening_sessions WHERE user_id=? AND source='web' AND session_key=?`, p.UserID, p.SessionID).Scan(&id, &trackID, &started); err != nil {
		return err
	}
	if trackID != p.TrackID || started != p.StartedAt {
		return ErrListeningProgress
	}
	for day, ms := range p.Days {
		// 零时长心跳不算一次收听，客户端缺失时长的记录另行处理。
		if ms == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO listening_days(session_id,day,milliseconds) VALUES(?,?,?) ON CONFLICT(session_id,day) DO UPDATE SET milliseconds=MAX(milliseconds,excluded.milliseconds)`, id, day, ms); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddClientListening 只接收正式 scrobble；有原始时间戳时按用户、客户端和歌曲去重。
func (d *DB) AddClientListening(ctx context.Context, userID int64, client string, track ListeningTrack, durationMS, playedAt int64, hasTimestamp bool, at time.Time) error {
	enabled, err := d.ListeningEnabledAt(ctx)
	if err != nil {
		return err
	}
	if playedAt < enabled || playedAt > at.UnixMilli() {
		return nil
	}
	key := rand.Text()
	if hasTimestamp {
		encoded, _ := json.Marshal([]any{client, track.ID, playedAt})
		key = fmt.Sprintf("%x", sha256.Sum256(encoded))
	}
	unknown := 0
	if durationMS <= 0 {
		durationMS = 0
		unknown = 1
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO listening_sessions(user_id,session_key,source,track_id,name,singer,started_at,unknown_duration) VALUES(?,?,'client',?,?,?,?,?) ON CONFLICT(user_id,source,session_key) DO NOTHING`, userID, key, track.ID, track.Name, track.Singer, playedAt, unknown)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return tx.Commit()
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	day := time.UnixMilli(playedAt).In(ListeningZone).Format("2006-01-02")
	if _, err = tx.ExecContext(ctx, `INSERT INTO listening_days(session_id,day,milliseconds) VALUES(?,?,?)`, id, day, durationMS); err != nil {
		return err
	}
	return tx.Commit()
}

type ListeningTotals struct {
	WebMS           int64 `json:"webMs"`
	ClientMS        int64 `json:"clientMs"`
	TotalMS         int64 `json:"totalMs"`
	Plays           int   `json:"plays"`
	UnknownDuration int   `json:"unknownDuration"`
}
type ListeningDay struct {
	Day string `json:"day"`
	ListeningTotals
}
type ListeningRank struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Singer string `json:"singer,omitempty"`
	ListeningTotals
}
type ListeningStats struct {
	EnabledAt int64  `json:"enabledAt"`
	From      string `json:"from"`
	To        string `json:"to"`
	Timezone  string `json:"timezone"`
	ListeningTotals
	Tracks      int             `json:"tracks"`
	ActiveDays  int             `json:"activeDays"`
	CountedDays int             `json:"countedDays"`
	AverageMS   int64           `json:"averageMs"`
	Daily       []ListeningDay  `json:"daily"`
	TopTracks   []ListeningRank `json:"topTracks"`
	TopUsers    []ListeningRank `json:"topUsers"`
}

const listeningSums = `COALESCE(SUM(CASE WHEN s.source='web' THEN d.milliseconds ELSE 0 END),0),COALESCE(SUM(CASE WHEN s.source='client' THEN d.milliseconds ELSE 0 END),0),COUNT(DISTINCT s.id),COUNT(DISTINCT CASE WHEN s.unknown_duration=1 THEN s.id END)`

// ListeningStatistics 的 userID=0 只供已经通过管理员鉴权的全站查询使用。
func (d *DB) ListeningStatistics(ctx context.Context, userID int64, days int, at time.Time) (ListeningStats, error) {
	out := ListeningStats{Timezone: "Asia/Shanghai", Daily: []ListeningDay{}, TopTracks: []ListeningRank{}, TopUsers: []ListeningRank{}}
	if days != 7 && days != 30 && days != 90 && days != 365 {
		return out, ErrListeningProgress
	}
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT enabled_at FROM listening_meta WHERE id=1`).Scan(&out.EnabledAt); err != nil {
		return out, err
	}
	today := at.In(ListeningZone)
	end := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, ListeningZone)
	begin := end.AddDate(0, 0, 1-days)
	out.From, out.To = begin.Format("2006-01-02"), end.Format("2006-01-02")
	enabledDay := time.UnixMilli(out.EnabledAt).In(ListeningZone).Format("2006-01-02")
	dailyIndex := map[string]int{}
	for day := begin; !day.After(end); day = day.AddDate(0, 0, 1) {
		label := day.Format("2006-01-02")
		dailyIndex[label] = len(out.Daily)
		out.Daily = append(out.Daily, ListeningDay{Day: label})
		if label >= enabledDay {
			out.CountedDays++
		}
	}
	from := ` FROM listening_days d JOIN listening_sessions s ON s.id=d.session_id JOIN users u ON u.id=s.user_id WHERE d.day>=? AND d.day<=?`
	args := []any{out.From, out.To}
	if userID > 0 {
		from += ` AND s.user_id=?`
		args = append(args, userID)
	}
	err = tx.QueryRowContext(ctx, `SELECT `+listeningSums+`,COUNT(DISTINCT s.track_id),COUNT(DISTINCT d.day)`+from, args...).Scan(&out.WebMS, &out.ClientMS, &out.Plays, &out.UnknownDuration, &out.Tracks, &out.ActiveDays)
	if err != nil {
		return out, err
	}
	out.TotalMS = out.WebMS + out.ClientMS
	if out.CountedDays > 0 {
		out.AverageMS = out.TotalMS / int64(out.CountedDays)
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.day,`+listeningSums+from+` GROUP BY d.day ORDER BY d.day`, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v ListeningDay
		if err = rows.Scan(&v.Day, &v.WebMS, &v.ClientMS, &v.Plays, &v.UnknownDuration); err != nil {
			rows.Close()
			return out, err
		}
		v.TotalMS = v.WebMS + v.ClientMS
		if i, ok := dailyIndex[v.Day]; ok {
			out.Daily[i] = v
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	readRank := func(user bool) ([]ListeningRank, error) {
		fields, group := `s.track_id,MAX(s.name),MAX(s.singer)`, `s.track_id`
		if user {
			fields, group = `CAST(u.id AS TEXT),u.name,''`, `u.id`
		}
		rows, e := tx.QueryContext(ctx, `SELECT `+fields+`,`+listeningSums+from+` GROUP BY `+group+` ORDER BY SUM(d.milliseconds) DESC,COUNT(DISTINCT s.id) DESC,`+group+` LIMIT 10`, args...)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		ranks := []ListeningRank{}
		for rows.Next() {
			var v ListeningRank
			if e = rows.Scan(&v.ID, &v.Name, &v.Singer, &v.WebMS, &v.ClientMS, &v.Plays, &v.UnknownDuration); e != nil {
				return nil, e
			}
			v.TotalMS = v.WebMS + v.ClientMS
			ranks = append(ranks, v)
		}
		return ranks, rows.Err()
	}
	if out.TopTracks, err = readRank(false); err != nil {
		return out, err
	}
	if userID == 0 {
		if out.TopUsers, err = readRank(true); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}
