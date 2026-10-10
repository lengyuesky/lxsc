// Package diagnostics 提供与原始日志隔离的限权、限时播放诊断。
package diagnostics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"regexp"
	"sync"
	"syscall"
	"time"

	"lxsc/internal/httpguard"
)

// Event 只接受固定字段；不得加入 URL、用户名、歌曲文本或自由文本错误。
type Event struct {
	RequestID     string    `json:"requestId,omitempty"`
	BytesWritten  *int64    `json:"bytesWritten,omitempty"`
	ResponseBytes *int64    `json:"responseBytes,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
	Method        string    `json:"method,omitempty"`
	Client        string    `json:"client,omitempty"`
	Format        string    `json:"format,omitempty"`
	BoardID       string    `json:"boardId,omitempty"`
	BoardIDs      []string  `json:"boardIds,omitempty"`
	Result        string    `json:"result,omitempty"`
	Count         *int      `json:"count,omitempty"`
	Lines         *int      `json:"lines,omitempty"`
	Synced        *bool     `json:"synced,omitempty"`
	ProtocolCode  *int      `json:"protocolCode,omitempty"`
	Time          time.Time `json:"time"`
	Stage         string    `json:"stage"`
	TrackID       string    `json:"trackId,omitempty"`
	Platform      string    `json:"platform,omitempty"`
	Quality       string    `json:"quality,omitempty"`
	Mode          string    `json:"mode,omitempty"`
	Cached        bool      `json:"cached"`
	CoverOrigin   string    `json:"coverOrigin,omitempty"`
	Status        int       `json:"status"`
	Error         string    `json:"error"`
	ElapsedMS     int64     `json:"elapsedMs"`
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

// ErrorCode 通过类型分类；绝不读取或返回 err.Error() 中的签名地址。
func ErrorCode(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, errUnsafeTarget) {
		return "blocked_target"
	}
	if errors.Is(err, errRedirectLimit) {
		return "redirect_limit"
	}
	if errors.Is(err, httpguard.ErrNonAudioResponse) {
		return "non_audio"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.Timeout() {
			return "timeout"
		}
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	if errors.As(err, &cert) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &record) {
		return "tls"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "connect"
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) {
		return "connect"
	}
	return "upstream_error"
}

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
	e.Mode = Mode(e.Mode)
	e.Error = oneOf(e.Error, "none", "busy", "cancelled", "timeout", "dns", "tls", "connect", "blocked_target", "redirect_limit", "non_audio", "upstream_error", "encode_error", "write_error")
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
