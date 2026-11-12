package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const PlaybackRetention = 30 * 24 * time.Hour
const PlaybackDetailLimit = 100

var ErrPlaybackLog = errors.New("播放日志参数无效或播放标识与原曲目不一致")

var playbackSchema = []string{
	`CREATE TABLE IF NOT EXISTS playback_logs(
	 id INTEGER PRIMARY KEY AUTOINCREMENT,
	 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	 user_name TEXT NOT NULL, playback_key TEXT NOT NULL,
	 protocol TEXT NOT NULL, client TEXT NOT NULL, kind TEXT NOT NULL,
	 track_id TEXT NOT NULL, name TEXT NOT NULL, singer TEXT NOT NULL,
	 requested_platform TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL DEFAULT '',
	 resolved_track_id TEXT NOT NULL DEFAULT '',
	 source_id INTEGER NOT NULL DEFAULT 0, source_name TEXT NOT NULL DEFAULT '',
	 requested_quality TEXT NOT NULL DEFAULT '', quality TEXT NOT NULL DEFAULT '',
	 mode TEXT NOT NULL DEFAULT '', delivery TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT 'pending',
	 status INTEGER NOT NULL DEFAULT 0, cached INTEGER NOT NULL DEFAULT 0,
	 started_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	 requests INTEGER NOT NULL DEFAULT 0, completed_sequence INTEGER NOT NULL DEFAULT 0,
	 detail_count INTEGER NOT NULL DEFAULT 0,
	 UNIQUE(user_id,protocol,client,playback_key))`,
	`CREATE INDEX IF NOT EXISTS idx_playback_time ON playback_logs(started_at DESC,id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_user_time ON playback_logs(user_id,started_at DESC,id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_source_time ON playback_logs(source_id,started_at DESC,id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_outcome ON playback_logs(outcome)`,
	`CREATE TABLE IF NOT EXISTS playback_events(
	 id INTEGER PRIMARY KEY AUTOINCREMENT,
	 log_id INTEGER NOT NULL REFERENCES playback_logs(id) ON DELETE CASCADE,
	 request_sequence INTEGER NOT NULL, event_index INTEGER NOT NULL,
	 at INTEGER NOT NULL, stage TEXT NOT NULL, source_id INTEGER NOT NULL DEFAULT 0,
	 source_name TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL DEFAULT '', quality TEXT NOT NULL DEFAULT '',
	 status INTEGER NOT NULL DEFAULT 0, cached INTEGER NOT NULL DEFAULT 0,
	 error TEXT NOT NULL DEFAULT '', elapsed_ms INTEGER NOT NULL DEFAULT 0,
	 UNIQUE(log_id,request_sequence,event_index))`,
	`CREATE INDEX IF NOT EXISTS idx_playback_events_log ON playback_events(log_id,id)`,
	`CREATE INDEX IF NOT EXISTS idx_playback_events_source ON playback_events(source_id,log_id)`,
	`CREATE TABLE IF NOT EXISTS source_schedules(source_id INTEGER PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,payload BLOB NOT NULL)`,
}

type PlaybackTrack struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Singer   string `json:"singer"`
	Platform string `json:"platform"`
}

// PlaybackStart 的 Key 是独立的播放关联标识，不能使用认证票据或票据摘要。
type PlaybackStart struct {
	UserID                 int64
	UserName, Key          string
	Protocol, Client, Kind string
	Track                  PlaybackTrack
	At                     int64
}

type PlaybackHandle struct{ ID, UserID, Sequence int64 }

type PlaybackResult struct {
	Track                            PlaybackTrack
	Platform, ResolvedTrackID        string
	SourceID                         int64
	SourceName, RequestedQuality     string
	Quality, Mode, Delivery, Outcome string
	Status                           int
	Cached                           bool
}

type PlaybackEvent struct {
	Request    int64  `json:"request"`
	At         int64  `json:"at"`
	Stage      string `json:"stage"`
	SourceID   int64  `json:"sourceId"`
	SourceName string `json:"sourceName"`
	Platform   string `json:"platform"`
	Quality    string `json:"quality"`
	Status     int    `json:"status"`
	Cached     bool   `json:"cached"`
	Error      string `json:"error"`
	ElapsedMS  int64  `json:"elapsedMs"`
}

type PlaybackLog struct {
	ID                int64  `json:"id"`
	UserID            int64  `json:"userId"`
	UserName          string `json:"userName"`
	Protocol          string `json:"protocol"`
	Client            string `json:"client"`
	Kind              string `json:"kind"`
	TrackID           string `json:"trackId"`
	Name              string `json:"name"`
	Singer            string `json:"singer"`
	RequestedPlatform string `json:"requestedPlatform"`
	Platform          string `json:"platform"`
	ResolvedTrackID   string `json:"resolvedTrackId"`
	SourceID          int64  `json:"sourceId"`
	SourceName        string `json:"sourceName"`
	RequestedQuality  string `json:"requestedQuality"`
	Quality           string `json:"quality"`
	Mode              string `json:"mode"`
	Delivery          string `json:"delivery"`
	Outcome           string `json:"outcome"`
	Status            int    `json:"status"`
	Cached            bool   `json:"cached"`
	StartedAt         int64  `json:"startedAt"`
	UpdatedAt         int64  `json:"updatedAt"`
	Requests          int64  `json:"requests"`
	DetailCount       int64  `json:"detailCount"`
}

func validPlaybackStart(p PlaybackStart) bool {
	return p.UserID > 0 && p.At > 0 && p.Key != "" && len(p.Key) <= 256 && p.Track.ID != "" && len(p.Track.ID) <= 1200 && len(p.Client) <= 256 &&
		(p.Protocol == "web" || p.Protocol == "subsonic" || p.Protocol == "fnmusic") && (p.Kind == "playback" || p.Kind == "request")
}

func beginPlayback(ctx context.Context, tx *sql.Tx, p PlaybackStart, request bool) (PlaybackHandle, error) {
	h := PlaybackHandle{UserID: p.UserID}
	count := 0
	if request {
		count = 1
	}
	err := tx.QueryRowContext(ctx, `INSERT INTO playback_logs(user_id,user_name,playback_key,protocol,client,kind,track_id,name,singer,requested_platform,started_at,updated_at,requests,outcome)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(user_id,protocol,client,playback_key) DO UPDATE SET
	kind=CASE WHEN excluded.kind='playback' THEN 'playback' ELSE playback_logs.kind END,
	requests=playback_logs.requests+excluded.requests,updated_at=MAX(playback_logs.updated_at,excluded.updated_at)
	WHERE playback_logs.track_id=excluded.track_id RETURNING id,requests`,
		p.UserID, p.UserName, p.Key, p.Protocol, p.Client, p.Kind, p.Track.ID, p.Track.Name, p.Track.Singer, p.Track.Platform, p.At, p.At, count,
		map[bool]string{true: "pending", false: "reported"}[request]).Scan(&h.ID, &h.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrPlaybackLog
	}
	return h, err
}

func (d *DB) BeginPlayback(ctx context.Context, p PlaybackStart) (PlaybackHandle, error) {
	if !validPlaybackStart(p) {
		return PlaybackHandle{}, ErrPlaybackLog
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return PlaybackHandle{}, err
	}
	defer tx.Rollback()
	h, err := beginPlayback(ctx, tx, p, true)
	if err != nil {
		return h, err
	}
	return h, tx.Commit()
}

// CompletePlayback 原子追加本次请求明细。较早请求的迟到结果不能覆盖较新请求。
func (d *DB) CompletePlayback(ctx context.Context, h PlaybackHandle, result PlaybackResult, events []PlaybackEvent, at time.Time) error {
	if h.ID <= 0 || h.UserID <= 0 || h.Sequence <= 0 || len(events) > PlaybackDetailLimit {
		return ErrPlaybackLog
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner int64
	if err = tx.QueryRowContext(ctx, `SELECT user_id FROM playback_logs WHERE id=?`, h.ID).Scan(&owner); err != nil {
		return err
	}
	if owner != h.UserID {
		return ErrPlaybackLog
	}
	added := int64(0)
	for index, e := range events {
		v, err := tx.ExecContext(ctx, `INSERT INTO playback_events(log_id,request_sequence,event_index,at,stage,source_id,source_name,platform,quality,status,cached,error,elapsed_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(log_id,request_sequence,event_index) DO NOTHING`,
			h.ID, h.Sequence, index, e.At, e.Stage, e.SourceID, e.SourceName, e.Platform, e.Quality, e.Status, e.Cached, e.Error, e.ElapsedMS)
		if err != nil {
			return err
		}
		n, _ := v.RowsAffected()
		added += n
	}
	if _, err = tx.ExecContext(ctx, `UPDATE playback_logs SET detail_count=detail_count+?,updated_at=MAX(updated_at,?) WHERE id=?`, added, at.UnixMilli(), h.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE playback_logs SET
	name=?,singer=?,requested_platform=?,platform=?,resolved_track_id=?,source_id=?,source_name=?,
	requested_quality=?,quality=?,mode=?,delivery=?,outcome=?,status=?,cached=?,completed_sequence=?
	WHERE id=? AND completed_sequence<=?`, result.Track.Name, result.Track.Singer, result.Track.Platform, result.Platform, result.ResolvedTrackID,
		result.SourceID, result.SourceName, result.RequestedQuality, result.Quality, result.Mode, result.Delivery, result.Outcome, result.Status, result.Cached, h.Sequence, h.ID, h.Sequence); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM playback_events WHERE log_id=? AND id NOT IN (SELECT id FROM playback_events WHERE log_id=? ORDER BY id DESC LIMIT ?)`, h.ID, h.ID, PlaybackDetailLimit); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordFnPlayback 把接受的开始事件、日志归类及客户端估算放在同一事务中。
// 重试使用同一个独立 Key，统计、历史和日志均不重复；不回填升级前的记录。
func (d *DB) RecordFnPlayback(ctx context.Context, p PlaybackStart, durationMS int64) error {
	if !validPlaybackStart(p) || p.Protocol != "fnmusic" || p.Kind != "playback" {
		return ErrPlaybackLog
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	h, err := beginPlayback(ctx, tx, p, false)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE playback_logs SET name=?,singer=?,requested_platform=? WHERE id=?`, p.Track.Name, p.Track.Singer, p.Track.Platform, h.ID); err != nil {
		return err
	}
	var enabled int64
	if err = tx.QueryRowContext(ctx, `SELECT enabled_at FROM listening_meta WHERE id=1`).Scan(&enabled); err != nil {
		return err
	}
	if p.At < enabled {
		return tx.Commit()
	}
	unknown := durationMS <= 0
	durationMS = max(0, durationMS)
	insert, err := tx.ExecContext(ctx, `INSERT INTO listening_sessions(user_id,session_key,source,track_id,name,singer,started_at,unknown_duration) VALUES(?,?,'client',?,?,?,?,?) ON CONFLICT(user_id,source,session_key) DO NOTHING`,
		p.UserID, "fnmusic:"+p.Key, p.Track.ID, p.Track.Name, p.Track.Singer, p.At, unknown)
	if err != nil {
		return err
	}
	n, _ := insert.RowsAffected()
	if n == 0 {
		return tx.Commit()
	}
	id, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	day := time.UnixMilli(p.At).In(ListeningZone).Format("2006-01-02")
	if _, err = tx.ExecContext(ctx, `INSERT INTO listening_days(session_id,day,milliseconds) VALUES(?,?,?)`, id, day, durationMS); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO history(user_id,track_id,played_at) VALUES(?,?,?)`, p.UserID, p.Track.ID, p.At/1000); err != nil {
		return err
	}
	return d.commitListening(tx)
}

const playbackColumns = `id,user_id,user_name,protocol,client,kind,track_id,name,singer,requested_platform,platform,resolved_track_id,source_id,source_name,requested_quality,quality,mode,delivery,outcome,status,cached,started_at,updated_at,requests,detail_count`

type playbackScanner interface{ Scan(...any) error }

func scanPlayback(row playbackScanner, p *PlaybackLog) error {
	return row.Scan(&p.ID, &p.UserID, &p.UserName, &p.Protocol, &p.Client, &p.Kind, &p.TrackID, &p.Name, &p.Singer, &p.RequestedPlatform, &p.Platform, &p.ResolvedTrackID,
		&p.SourceID, &p.SourceName, &p.RequestedQuality, &p.Quality, &p.Mode, &p.Delivery, &p.Outcome, &p.Status, &p.Cached, &p.StartedAt, &p.UpdatedAt, &p.Requests, &p.DetailCount)
}

type PlaybackQuery struct {
	UserID                                   int64
	SourceID                                 int64
	Query, Client, Delivery, Outcome, Cursor string
	Days, Limit                              int
}

type PlaybackSource struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type PlaybackPage struct {
	Items         []PlaybackLog    `json:"items"`
	NextCursor    string           `json:"nextCursor"`
	Sources       []PlaybackSource `json:"sources"`
	Clients       []string         `json:"clients"`
	GeneratedAt   int64            `json:"generatedAt"`
	RetentionDays int              `json:"retentionDays"`
}

type playbackCursor struct{ At, ID int64 }

func playbackWhere(userID int64, cutoff int64) (string, []any) {
	where, args := ` WHERE started_at>=?`, []any{cutoff}
	if userID > 0 {
		where += ` AND user_id=?`
		args = append(args, userID)
	}
	return where, args
}

// RecentPlayback 的全站查询只供已经通过管理员鉴权的入口调用。
func (d *DB) RecentPlayback(ctx context.Context, q PlaybackQuery, at time.Time) (PlaybackPage, error) {
	out := PlaybackPage{Items: []PlaybackLog{}, Sources: []PlaybackSource{}, Clients: []string{}, GeneratedAt: at.UnixMilli(), RetentionDays: 30}
	if q.UserID < 0 || q.SourceID < 0 || len(q.Query) > 800 || len(q.Client) > 256 || len(q.Cursor) > 128 || q.Limit < 0 || q.Limit > 100 {
		return out, ErrPlaybackLog
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Days == 0 || q.Days > 30 {
		q.Days = 30
	}
	if q.Days < 1 {
		return out, ErrPlaybackLog
	}
	base, baseArgs := playbackWhere(q.UserID, at.Add(-time.Duration(q.Days)*24*time.Hour).UnixMilli())
	where, args := base, append([]any{}, baseArgs...)
	if q.Query != "" {
		query := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(q.Query))
		where += ` AND (name LIKE ? ESCAPE '\' OR singer LIKE ? ESCAPE '\')`
		args = append(args, "%"+query+"%", "%"+query+"%")
	}
	if q.SourceID > 0 {
		where += ` AND (source_id=? OR EXISTS(SELECT 1 FROM playback_events e WHERE e.log_id=playback_logs.id AND e.source_id=?))`
		args = append(args, q.SourceID, q.SourceID)
	}
	for _, filter := range []struct{ column, value string }{{"client", q.Client}, {"delivery", q.Delivery}, {"outcome", q.Outcome}} {
		if filter.value != "" {
			where += ` AND ` + filter.column + `=?`
			args = append(args, filter.value)
		}
	}
	if q.Cursor != "" {
		var cursor playbackCursor
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.At <= 0 || cursor.ID <= 0 {
			return out, ErrPlaybackLog
		}
		where += ` AND (started_at<? OR (started_at=? AND id<?))`
		args = append(args, cursor.At, cursor.At, cursor.ID)
	}
	args = append(args, q.Limit+1)
	rows, err := d.read.QueryContext(ctx, `SELECT `+playbackColumns+` FROM playback_logs`+where+` ORDER BY started_at DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item PlaybackLog
		if err = scanPlayback(rows, &item); err != nil {
			rows.Close()
			return out, err
		}
		out.Items = append(out.Items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Items) > q.Limit {
		out.Items = out.Items[:q.Limit]
		last := out.Items[len(out.Items)-1]
		raw, _ := json.Marshal(playbackCursor{At: last.StartedAt, ID: last.ID})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	// 选项也受同一用户范围与留存限制，不能借筛选器泄漏别人的客户端或音源。
	rows, err = d.read.QueryContext(ctx, `SELECT source_id,MAX(source_name) FROM (
	SELECT source_id,source_name FROM playback_logs`+base+` AND source_id>0
	UNION ALL SELECT e.source_id,e.source_name FROM playback_logs JOIN playback_events e ON e.log_id=playback_logs.id`+base+` AND e.source_id>0)
	GROUP BY source_id ORDER BY source_id LIMIT 300`, append(append([]any{}, baseArgs...), baseArgs...)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var source PlaybackSource
		if err = rows.Scan(&source.ID, &source.Name); err != nil {
			rows.Close()
			return out, err
		}
		out.Sources = append(out.Sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = d.read.QueryContext(ctx, `SELECT DISTINCT client FROM playback_logs`+base+` ORDER BY client LIMIT 200`, baseArgs...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var client string
		if err = rows.Scan(&client); err != nil {
			return out, err
		}
		out.Clients = append(out.Clients, client)
	}
	return out, rows.Err()
}

type PlaybackDetail struct {
	PlaybackLog
	Events []PlaybackEvent `json:"events"`
}

func (d *DB) PlaybackDetail(ctx context.Context, id, userID int64, at time.Time) (PlaybackDetail, error) {
	out := PlaybackDetail{Events: []PlaybackEvent{}}
	if id <= 0 || userID < 0 {
		return out, ErrPlaybackLog
	}
	where, args := playbackWhere(userID, at.Add(-PlaybackRetention).UnixMilli())
	args = append(args, id)
	if err := scanPlayback(d.read.QueryRowContext(ctx, `SELECT `+playbackColumns+` FROM playback_logs`+where+` AND id=?`, args...), &out.PlaybackLog); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return out, err
	}
	rows, err := d.read.QueryContext(ctx, `SELECT request_sequence,at,stage,source_id,source_name,platform,quality,status,cached,error,elapsed_ms FROM playback_events WHERE log_id=? ORDER BY id LIMIT ?`, id, PlaybackDetailLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e PlaybackEvent
		if err = rows.Scan(&e.Request, &e.At, &e.Stage, &e.SourceID, &e.SourceName, &e.Platform, &e.Quality, &e.Status, &e.Cached, &e.Error, &e.ElapsedMS); err != nil {
			return out, err
		}
		out.Events = append(out.Events, e)
	}
	return out, rows.Err()
}

func (d *DB) CleanupPlayback(ctx context.Context, at time.Time) (int64, error) {
	result, err := d.sql.ExecContext(ctx, `DELETE FROM playback_logs WHERE id IN (SELECT id FROM playback_logs WHERE started_at<? ORDER BY started_at,id LIMIT 1000)`, at.Add(-PlaybackRetention).UnixMilli())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RecoverPlayback 在单实例启动、接收请求前执行。旧进程未记录结束结果时不伪造成功。
func (d *DB) RecoverPlayback(ctx context.Context, at time.Time) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE playback_logs SET outcome='unknown',updated_at=MAX(updated_at,?) WHERE outcome='pending'`, at.UnixMilli())
	return err
}

func (d *DB) SourceSchedules(ctx context.Context) (map[int64][]byte, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT source_id,payload FROM source_schedules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]byte{}
	for rows.Next() {
		var id int64
		var payload []byte
		if err = rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		out[id] = payload
	}
	return out, rows.Err()
}

func (d *DB) SaveSourceSchedule(ctx context.Context, id int64, payload []byte) error {
	if id <= 0 || len(payload) > 256<<10 {
		return fmt.Errorf("音源调度数据无效")
	}
	_, err := d.sql.ExecContext(ctx, `INSERT INTO source_schedules(source_id,payload) SELECT id,? FROM sources WHERE id=? ON CONFLICT(source_id) DO UPDATE SET payload=excluded.payload`, payload, id)
	return err
}

func (d *DB) DeleteSourceSchedule(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM source_schedules WHERE source_id=?`, id)
	return err
}

func (d *DB) PlaybackSourceName(ctx context.Context, id int64) (string, error) {
	var name string
	err := d.read.QueryRowContext(ctx, `SELECT name FROM sources WHERE id=?`, id).Scan(&name)
	return name, err
}
