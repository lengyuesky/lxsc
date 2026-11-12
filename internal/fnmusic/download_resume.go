package fnmusic

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/simplelru"
	"lxsc/internal/mediastream"
)

type downloadResumeKey struct {
	session [32]byte
	guid    string
}

type downloadResumeValidator struct {
	value       string
	expires     time.Time
	description mediastream.MediaDescription
	ambiguous   bool
}

// 仅保留已交付表示的实体校验条件，不保留音频，也不授予资源访问权限。
type downloadResumeStore struct {
	mu      sync.Mutex
	entries *simplelru.LRU[downloadResumeKey, downloadResumeValidator]
}

func (s *downloadResumeStore) remember(ctx context.Context, guid string, description mediastream.MediaDescription) {
	session, ok := ctx.Value(sessionKey{}).([32]byte)
	if !ok {
		return
	}
	value := description.ETag
	if strings.HasPrefix(value, "W/") || value == "" {
		value = description.LastModified
		if _, err := http.ParseTime(value); err != nil {
			value = ""
		}
	}
	if len(value) > 1024 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries, _ = simplelru.NewLRU[downloadResumeKey, downloadResumeValidator](4096, nil)
	}
	key := downloadResumeKey{session, guid}
	entry, exists := s.entries.Get(key)
	if exists && time.Now().Before(entry.expires) {
		if !sameDownloadRepresentation(entry, description) {
			// 原版没有下载尝试 ID；并发取得不同表示时不能猜测哪个残片属于哪份文件。
			entry.ambiguous = true
			s.entries.Add(key, entry)
		}
		return
	}
	s.entries.Add(key, downloadResumeValidator{value: value, expires: time.Now().Add(30 * time.Minute), description: description})
}

func (s *downloadResumeStore) validator(ctx context.Context, guid string) string {
	entry, ok := s.get(ctx, guid)
	if !ok || entry.ambiguous {
		return ""
	}
	return entry.value
}

func (s *downloadResumeStore) matches(ctx context.Context, guid string, expected downloadResumeValidator, description mediastream.MediaDescription) bool {
	current, ok := s.get(ctx, guid)
	return ok && !current.ambiguous && current == expected && sameDownloadRepresentation(expected, description)
}

func sameDownloadRepresentation(expected downloadResumeValidator, description mediastream.MediaDescription) bool {
	before := expected.description
	if before.ResourceDigest != description.ResourceDigest || actualAudioFormat(before.ContentType) != actualAudioFormat(description.ContentType) {
		return false
	}
	if before.KnownLength && (!description.KnownLength || before.Length != description.Length) {
		return false
	}
	if strings.HasPrefix(expected.value, `"`) {
		return expected.value == description.ETag
	}
	if expected.value != "" {
		wanted, err := http.ParseTime(expected.value)
		actual, actualErr := http.ParseTime(description.LastModified)
		return err == nil && actualErr == nil && wanted.Equal(actual)
	}
	return before == description
}

func (s *downloadResumeStore) get(ctx context.Context, guid string) (downloadResumeValidator, bool) {
	session, ok := ctx.Value(sessionKey{}).([32]byte)
	if !ok {
		return downloadResumeValidator{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		return downloadResumeValidator{}, false
	}
	key := downloadResumeKey{session, guid}
	entry, ok := s.entries.Get(key)
	if !ok {
		return downloadResumeValidator{}, false
	}
	if !time.Now().Before(entry.expires) {
		s.entries.Remove(key)
		return downloadResumeValidator{}, false
	}
	return entry, true
}

func actualAudioFormat(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "audio/mpeg":
		return "mp3"
	case "audio/mp4":
		return "m4a"
	case "audio/ogg":
		return "ogg"
	case "audio/aac":
		return "aac"
	case "audio/ape":
		return "ape"
	case "audio/wavpack":
		return "wv"
	case "audio/aiff":
		return "aiff"
	case "audio/basic":
		return "au"
	}
	return ""
}

func (s *Server) downloadAudioSpec(ctx context.Context, guid string, view map[string]any) {
	entry, ok := s.downloads.get(ctx, guid)
	if !ok || entry.ambiguous {
		return
	}
	spec := view["audioSpec"].(map[string]any)
	for _, field := range []string{"codec", "sampleRate", "bitDepth", "channel", "bitrate", "size"} {
		delete(spec, field)
	}
	format := actualAudioFormat(entry.description.ContentType)
	if format != "" {
		spec["format"] = format
		spec["path"] = "/online/" + guid + "." + format
	}
	if entry.description.KnownLength {
		spec["size"] = entry.description.Length
	}
}
