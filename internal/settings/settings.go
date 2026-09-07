// Package settings 管理运行时可修改的设置（存于 SQLite settings 表，内存缓存）
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"lxsc/internal/db"
)

// ErrInvalidSetting 表示设置值不合法，可由 HTTP 层映射为 400。
var ErrInvalidSetting = errors.New("设置值不合法")

// parseTTL 同时限制 Duration 和当前平台 int 的范围，不截断小数。
func parseTTL(value string) (int, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n > uint64((1<<63-1)/time.Second) || n > uint64(^uint(0)>>1) {
		return 0, ErrInvalidSetting
	}
	return int(n), nil
}

// Values 运行时设置
type Values struct {
	SearchSources    []string            `json:"searchSources"`    // 聚合搜索的平台及顺序
	StreamMode       string              `json:"streamMode"`       // 播放方式：redirect / force_redirect / proxy
	CoverMode        string              `json:"coverMode"`        // redirect / proxy
	URLCacheTTL      int                 `json:"urlCacheTTL"`      // 直链缓存秒数
	SearchCacheTTL   int                 `json:"searchCacheTTL"`   // 搜索缓存秒数
	DefaultQuality   string              `json:"defaultQuality"`   // 新用户默认音质
	ShowBoards       bool                `json:"showBoards"`       // 是否在在线音乐目录和歌单中展示榜单
	BoardSources     []string            `json:"boardSources"`     // 展示榜单的平台
	BoardSelections  map[string][]string `json:"boardSelections"`  // 缺少平台表示全部，空数组表示不展示
	BoardLimit       int                 `json:"boardLimit"`       // 旧字段，仅后端兼容读取
	BoardTrackLimit  int                 `json:"boardTrackLimit"`  // 旧字段，仅后端兼容读取
	ArtistSongLimit  int                 `json:"artistSongLimit"`  // 歌手页最多歌曲数
	ArtistAlbumLimit int                 `json:"artistAlbumLimit"` // 歌手页最多专辑数
	SearchLimit      int                 `json:"searchLimit"`      // 每平台每次搜索条数
	PublicPlaylists  bool                `json:"publicPlaylists"`  // 新建歌单默认公开
	ServerName       string              `json:"serverName"`
}

// Defaults 默认值
func Defaults() Values {
	return Values{
		SearchSources:    []string{"wy", "tx", "kw", "kg", "mg"},
		StreamMode:       "redirect",
		CoverMode:        "redirect",
		URLCacheTTL:      900,
		SearchCacheTTL:   600,
		DefaultQuality:   "320k",
		ShowBoards:       true,
		BoardSources:     []string{"wy", "tx", "kw", "kg", "mg"},
		BoardSelections:  map[string][]string{},
		BoardLimit:       3,
		BoardTrackLimit:  30,
		ArtistSongLimit:  30,
		ArtistAlbumLimit: 12,
		SearchLimit:      20,
		ServerName:       "lxsc",
	}
}

// Store 设置存储
type Store struct {
	mu  sync.RWMutex
	db  *db.DB
	cur Values
}

// New 从数据库加载
func New(ctx context.Context, d *db.DB) (*Store, error) {
	s := &Store{db: d, cur: Defaults()}
	all, err := d.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	s.cur = apply(s.cur, all)
	return s, nil
}

func apply(v Values, m map[string]string) Values {
	for k, val := range m {
		switch k {
		case "searchSources":
			v.SearchSources = splitList(val)
		case "streamMode":
			if val == "proxy" || val == "redirect" || val == "force_redirect" {
				v.StreamMode = val
			}
		case "coverMode":
			if val == "proxy" || val == "redirect" {
				v.CoverMode = val
			}
		case "urlCacheTTL":
			if n, err := parseTTL(val); err == nil {
				v.URLCacheTTL = n
			}
		case "searchCacheTTL":
			if n, err := parseTTL(val); err == nil {
				v.SearchCacheTTL = n
			}
		case "defaultQuality":
			if val != "" {
				v.DefaultQuality = val
			}
		case "showBoards":
			v.ShowBoards = val == "true" || val == "1"
		case "boardSources":
			v.BoardSources = splitList(val)
		case "boardSelections":
			if selections, err := parseBoardSelections([]byte(val)); err == nil {
				v.BoardSelections = selections
			}
		case "boardLimit":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 20 {
				v.BoardLimit = n
			}
		case "boardTrackLimit":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 100 {
				v.BoardTrackLimit = n
			}
		case "artistSongLimit":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 100 {
				v.ArtistSongLimit = n
			}
		case "artistAlbumLimit":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 50 {
				v.ArtistAlbumLimit = n
			}
		case "searchLimit":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 100 {
				v.SearchLimit = n
			}
		case "publicPlaylists":
			v.PublicPlaylists = val == "true" || val == "1"
		case "serverName":
			if val != "" {
				v.ServerName = val
			}
		}
	}
	return v
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Get 当前设置快照
func (s *Store) Get() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneValues(s.cur)
}

func cloneValues(v Values) Values {
	v.SearchSources = append([]string(nil), v.SearchSources...)
	v.BoardSources = append([]string(nil), v.BoardSources...)
	selections := make(map[string][]string, len(v.BoardSelections))
	for source, ids := range v.BoardSelections {
		// 保留显式空数组，不能将它序列化成 null 或丢掉平台键。
		selections[source] = append([]string{}, ids...)
	}
	v.BoardSelections = selections
	return v
}

// Update 以 JSON 对象更新（仅更新提供的字段），并持久化
func (s *Store) Update(ctx context.Context, patch map[string]json.RawMessage) (Values, error) {
	m := map[string]string{}
	for k, raw := range patch {
		if k == "boardSelections" {
			selections, err := parseBoardSelections(raw)
			if err != nil {
				return s.Get(), err
			}
			encoded, _ := json.Marshal(selections)
			m[k] = string(encoded)
			continue
		}
		if k == "urlCacheTTL" || k == "searchCacheTTL" {
			value := strings.TrimSpace(string(raw))
			var str string
			if json.Unmarshal(raw, &str) == nil {
				value = str
			}
			n, err := parseTTL(value)
			if err != nil {
				return s.Get(), fmt.Errorf("%w: %s 必须是范围内的非负整数秒数", ErrInvalidSetting, k)
			}
			m[k] = strconv.Itoa(n)
			continue
		}
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			m[k] = str
			continue
		}
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			m[k] = strconv.FormatBool(b)
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil {
			m[k] = strconv.Itoa(int(n))
			continue
		}
		var arr []string
		if err := json.Unmarshal(raw, &arr); err == nil {
			m[k] = strings.Join(arr, ",")
			continue
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := apply(s.cur, m)
	if err := s.db.SetSettings(ctx, m); err != nil {
		return cloneValues(s.cur), err
	}
	s.cur = next
	return cloneValues(s.cur), nil
}
