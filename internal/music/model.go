// Package music 提供歌曲元数据模型、ID 编码、聚合搜索、歌词与直链解析
package music

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Info 平台歌曲元数据（扁平结构，与 SDK 搜索结果一致，可直接传给音源脚本）
type Info struct {
	Raw map[string]any
}

// ParseInfo 从 JSON 构造
func ParseInfo(b []byte) (*Info, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("empty music info")
	}
	return &Info{Raw: m}, nil
}

// FromMap 从 map 构造
func FromMap(m map[string]any) *Info { return &Info{Raw: m} }

func (i *Info) str(k string) string {
	if i == nil || i.Raw == nil {
		return ""
	}
	return anyToString(i.Raw[k])
}

func anyToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}

// Source 平台
func (i *Info) Source() string { return i.str("source") }

// Name 歌名
func (i *Info) Name() string { return i.str("name") }

// Singer 歌手（多歌手以、分隔）
func (i *Info) Singer() string { return i.str("singer") }

// SingerID 第一位歌手的平台 ID；旧元数据没有该字段时返回空。
func (i *Info) SingerID() string {
	for _, key := range []string{"singerId", "artistId", "singerMid", "artistMid"} {
		if value := i.str(key); value != "" && value != "0" {
			return value
		}
	}
	return ""
}

// PrimarySinger 第一位歌手
func (i *Info) PrimarySinger() string {
	s := i.Singer()
	for _, sep := range []string{"、", "/", "&", ","} {
		if idx := strings.Index(s, sep); idx > 0 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}

// Album 专辑名
func (i *Info) Album() string { return i.str("albumName") }

// AlbumID 平台专辑 ID
func (i *Info) AlbumID() string { return i.str("albumId") }

// SongMid 平台歌曲 ID
func (i *Info) SongMid() string { return i.str("songmid") }

// Hash 酷狗 hash
func (i *Info) Hash() string { return i.str("hash") }

// Img 封面
func (i *Info) Img() string {
	if v := i.str("img"); v != "" {
		return v
	}
	return i.str("picUrl")
}

// Key 用于 Track ID 的平台内唯一键（kg 使用 hash）
func (i *Info) Key() string {
	if i.Source() == "kg" && i.Hash() != "" {
		return strings.ToUpper(i.Hash())
	}
	return i.SongMid()
}

// TrackID Subsonic 歌曲 ID
func (i *Info) TrackID() string { return TrackID(i.Source(), i.Key()) }

// Duration 时长（秒）
func (i *Info) Duration() int {
	if v, ok := i.Raw["_interval"].(float64); ok && v > 0 {
		return int(v)
	}
	return parseInterval(i.str("interval"))
}

func parseInterval(s string) int {
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// Qualities 可用音质列表
func (i *Info) Qualities() []string {
	var out []string
	if arr, ok := i.Raw["types"].([]any); ok {
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				if t := anyToString(m["type"]); t != "" {
					out = append(out, t)
				}
			}
		}
	}
	if len(out) == 0 {
		if m, ok := i.Raw["_types"].(map[string]any); ok {
			for k := range m {
				out = append(out, k)
			}
		}
	}
	return out
}

// BestQuality 最高音质
func (i *Info) BestQuality() string {
	best, rank := "", 0
	for _, q := range i.Qualities() {
		if r := QualityRank(q); r > rank {
			best, rank = q, r
		}
	}
	return best
}

// QualityRank 音质等级
func QualityRank(q string) int {
	switch q {
	case "128k":
		return 1
	case "192k":
		return 2
	case "320k":
		return 3
	case "flac":
		return 4
	case "flac24bit", "hires":
		return 5
	case "master", "atmos", "atmos_plus", "dolby":
		return 6
	}
	return 0
}

// SelectQuality 在歌曲已知音质中选择不高于目标音质的最高档。
// 元数据未提供音质列表，或没有可向下降级的档位时，保留目标音质交给音源解析器处理。
func SelectQuality(want string, available []string) string {
	if QualityRank(want) == 0 {
		want = "320k"
	}
	if len(available) == 0 {
		return want
	}
	set := make(map[string]bool, len(available))
	for _, quality := range available {
		if quality == "hires" {
			quality = "flac24bit"
		}
		set[quality] = true
	}
	wantRank := QualityRank(want)
	for _, quality := range []string{"flac24bit", "flac", "320k", "128k"} {
		if set[quality] && QualityRank(quality) <= wantRank {
			return quality
		}
	}
	return want
}

// QualityMeta 音质对应的比特率/后缀/MIME
func QualityMeta(q string) (bitRate int, suffix, contentType string) {
	switch q {
	case "128k":
		return 128, "mp3", "audio/mpeg"
	case "192k":
		return 192, "mp3", "audio/mpeg"
	case "320k":
		return 320, "mp3", "audio/mpeg"
	case "flac":
		return 1000, "flac", "audio/flac"
	case "flac24bit", "hires":
		return 2304, "flac", "audio/flac"
	}
	return 320, "mp3", "audio/mpeg"
}

// JSON 序列化
func (i *Info) JSON() []byte {
	b, _ := json.Marshal(i.Raw)
	return b
}

// AlbumSubID 专辑 Subsonic ID
func (i *Info) AlbumSubID() string {
	return AlbumID(i.Source(), i.AlbumID(), i.Album(), i.PrimarySinger())
}

// ArtistSubID 歌手 Subsonic ID
func (i *Info) ArtistSubID() string { return ArtistID(i.PrimarySinger()) }
