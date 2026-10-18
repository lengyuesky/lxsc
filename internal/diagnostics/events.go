// Package diagnostics 提供与原始日志隔离的限权、限时播放诊断。
package diagnostics

import (
	"regexp"
	"sync"
	"time"

	"lxsc/internal/httpguard"
)

// Event 只接受固定字段；不得加入 URL、用户名、歌曲文本或自由文本错误。
type Event struct {
	RequestID        string    `json:"requestId,omitempty"`
	BytesWritten     *int64    `json:"bytesWritten,omitempty"`
	ResponseBytes    *int64    `json:"responseBytes,omitempty"`
	Endpoint         string    `json:"endpoint,omitempty"`
	Method           string    `json:"method,omitempty"`
	Client           string    `json:"client,omitempty"`
	Format           string    `json:"format,omitempty"`
	BoardID          string    `json:"boardId,omitempty"`
	BoardIDs         []string  `json:"boardIds,omitempty"`
	Result           string    `json:"result,omitempty"`
	Count            *int      `json:"count,omitempty"`
	Lines            *int      `json:"lines,omitempty"`
	Synced           *bool     `json:"synced,omitempty"`
	ProtocolCode     *int      `json:"protocolCode,omitempty"`
	Time             time.Time `json:"time"`
	Stage            string    `json:"stage"`
	TrackID          string    `json:"trackId,omitempty"`
	Platform         string    `json:"platform,omitempty"`
	Quality          string    `json:"quality,omitempty"`
	RequestedQuality string    `json:"requestedQuality,omitempty"`
	Attempt          int       `json:"attempt,omitempty"`
	BudgetMS         int64     `json:"budgetMs,omitempty"`
	Validation       string    `json:"validation,omitempty"`
	Mode             string    `json:"mode,omitempty"`
	Cached           bool      `json:"cached"`
	CoverOrigin      string    `json:"coverOrigin,omitempty"`
	Status           int       `json:"status"`
	Error            string    `json:"error"`
	ElapsedMS        int64     `json:"elapsedMs"`
}

var trackPattern = regexp.MustCompile(`^tr-(wy|tx|kw|kg|mg)-[A-Za-z0-9_-]{1,128}$`)
var boardPattern = regexp.MustCompile(`^lb-(wy|tx|kw|kg|mg)-[A-Za-z0-9_-]{1,128}$`)
var requestPattern = regexp.MustCompile(`^req-[A-Z2-7]{26,64}$`)

func ValidBoardID(id string) bool { return boardPattern.MatchString(id) }

func ValidTrackID(id string) bool { return trackPattern.MatchString(id) }
func oneOf(value string, values ...string) string {
	for _, v := range values {
		if v == value {
			return value
		}
	}
	return ""
}
func Platform(value string) string { return oneOf(value, "wy", "tx", "kw", "kg", "mg") }
func Quality(value string) string {
	return oneOf(value, "128k", "192k", "320k", "flac", "flac24bit", "hires", "atmos", "atmos_plus", "master", "dolby")
}
func Mode(value string) string { return oneOf(value, "redirect", "force_redirect", "proxy") }

// ErrorCode 通过固定类别分类，不返回错误中的原始文本。
func ErrorCode(err error) string { return httpguard.ErrorCode(err) }

// Events 固定容量内存环；零值可用，nil 接收者方便不启用诊断的测试服务。
type Events struct {
	mu      sync.Mutex
	entries []Event
}

func (b *Events) Add(e Event) {
	if b == nil {
		return
	}
	e.Stage = oneOf(e.Stage, "metadata", "resolve", "refresh", "source_fallback", "url_check", "cache_check", "redirect", "proxy_response", "proxy_copy", "probe", "client_response", "board_probe", "lyrics_probe", "protocol_probe", "playback_probe")
	if e.Stage == "" {
		return
	}
	if !ValidTrackID(e.TrackID) {
		e.TrackID = ""
	}
	if !requestPattern.MatchString(e.RequestID) {
		e.RequestID = ""
	}
	e.Endpoint = oneOf(e.Endpoint, "ping", "getLicense", "getOpenSubsonicExtensions", "getUser", "getUsers", "getScanStatus", "getMusicFolders", "getIndexes", "getArtists", "getMusicDirectory", "getGenres", "getArtist", "getArtistInfo", "getArtistInfo2", "getAlbum", "getAlbumInfo", "getAlbumInfo2", "getSong", "getTopSongs", "getSimilarSongs", "getSimilarSongs2", "getAlbumList", "getAlbumList2", "getRandomSongs", "getSongsByGenre", "getNowPlaying", "getStarred", "getStarred2", "search", "search2", "search3", "getPlaylists", "getPlaylist", "getLyrics", "getLyricsBySongId", "getPlayQueue", "getBookmarks", "getInternetRadioStations", "getPodcasts", "getNewestPodcasts", "getShares", "getVideos", "getCoverArt", "getSongLists", "stream", "download")
	e.Method = oneOf(e.Method, "GET", "POST", "HEAD")
	e.Client = oneOf(e.Client, "amcfy", "stream_music", "other", "unknown")
	e.Format = oneOf(e.Format, "json", "xml", "jsonp", "binary")
	e.CoverOrigin = oneOf(e.CoverOrigin, "original", "custom", "stale", "placeholder", "board", "redirect")
	e.Result = oneOf(e.Result, "ok", "failed", "empty", "unavailable")
	if !ValidBoardID(e.BoardID) {
		e.BoardID = ""
	}
	boardIDs := make([]string, 0, min(10, len(e.BoardIDs)))
	for _, id := range e.BoardIDs {
		if ValidBoardID(id) {
			boardIDs = append(boardIDs, id)
		}
		if len(boardIDs) == 10 {
			break
		}
	}
	e.BoardIDs = boardIDs
	// 复制指针字段，调用方不能在入环后修改已校验数据。
	for _, field := range []**int{&e.Count, &e.Lines, &e.ProtocolCode} {
		if *field != nil {
			value := min(1000000, max(0, **field))
			*field = &value
		}
	}
	for _, field := range []**int64{&e.BytesWritten, &e.ResponseBytes} {
		if *field != nil {
			value := min(int64(1<<40), max(0, **field))
			*field = &value
		}
	}
	if e.Synced != nil {
		value := *e.Synced
		e.Synced = &value
	}
	e.Platform = Platform(e.Platform)
	e.Quality = Quality(e.Quality)
	e.RequestedQuality = Quality(e.RequestedQuality)
	e.Attempt = min(100, max(0, e.Attempt))
	e.BudgetMS = min(12000, max(0, e.BudgetMS))
	e.Validation = oneOf(e.Validation, "validated", "unverified", "failed")
	e.Mode = Mode(e.Mode)
	e.Error = oneOf(e.Error, "none", "busy", "cancelled", "timeout", "dns", "tls", "connect", "blocked_target", "redirect_limit", "non_audio", "upstream_error", "encode_error", "write_error", "expired", "rate_limited", "upstream_status", "unsupported_quality", "source_auth", "invalid_response", "source_error")
	if e.Error == "" {
		e.Error = "upstream_error"
	}
	if e.Status < 100 || e.Status > 599 {
		e.Status = 0
	}
	if e.ElapsedMS < 0 {
		e.ElapsedMS = 0
	}
	if e.ElapsedMS > 86400000 {
		e.ElapsedMS = 86400000
	}
	e.Time = time.Now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 2000 {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:1999]
	}
	b.entries = append(b.entries, e)
}
func (b *Events) List() []Event {
	return b.Recent(200)
}

func (b *Events) Recent(limit int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit = min(len(b.entries), min(2000, max(0, limit)))
	out := make([]Event, limit)
	copy(out, b.entries[len(b.entries)-limit:])
	return out
}
