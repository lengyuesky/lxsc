package music

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ID 前缀
const (
	KindTrack           = "tr"
	KindAlbum           = "al"
	KindArtist          = "ar"
	KindPlaylist        = "pl"
	KindBoard           = "lb"
	KindSongList        = "sl"
	KindDir             = "dir"
	KindArtistDir       = "ad"
	KindArtistName      = "an"
	KindSingerDir       = "sar"
	KindArtistAlbumPage = "sap"
	KindOnlineAlbum     = "oa"
)

// Hash16 稳定短哈希（hex 前 16 位）
func Hash16(parts ...string) string {
	sum := md5.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

// TrackID 生成歌曲 ID
func TrackID(source, key string) string { return KindTrack + "-" + source + "-" + key }

// AlbumID 生成专辑 ID：有平台专辑 ID 用之，否则用名称哈希
func AlbumID(source, albumID, albumName, artist string) string {
	if albumID != "" && albumID != "0" && source != "" {
		return KindAlbum + "-" + source + "-" + albumID
	}
	return KindAlbum + "-h-" + Hash16(strings.TrimSpace(albumName), strings.TrimSpace(artist))
}

// ArtistID 歌手 ID（按名称哈希，跨平台统一）
func ArtistID(name string) string { return KindArtist + "-h-" + Hash16(strings.TrimSpace(name)) }

// BoardID 榜单 ID
func BoardID(source, bangID string) string { return KindBoard + "-" + source + "-" + bangID }

// SongListID 在线歌单 ID
func SongListID(source, id string) string { return KindSongList + "-" + source + "-" + id }

// ArtistCategoryID 在线歌手分类目录 ID。
func ArtistCategoryID(category string) string { return KindArtistDir + "-" + category }

// ArtistNameID 在线歌手名称目录 ID。
func ArtistNameID(category, name string) string {
	return KindArtistName + "-" + category + "-" + encodeVirtualID(name)
}

// SingerDirectoryID 平台歌手入口 ID，保留平台歌手 ID、显示名称和可选父目录。
func SingerDirectoryID(source, name, artistID string, parent ...string) string {
	payload := struct {
		Name   string `json:"n"`
		ID     string `json:"i,omitempty"`
		Parent string `json:"r,omitempty"`
	}{Name: name, ID: artistID}
	if len(parent) > 0 {
		payload.Parent = parent[0]
	}
	return KindSingerDir + "-" + source + "-" + encodeVirtualID(payload)
}

// ArtistAlbumPageID 歌手专辑分页目录 ID。
func ArtistAlbumPageID(source, name, artistID string, page int, singerParent ...string) string {
	if page < 1 {
		page = 1
	}
	payload := struct {
		Name         string `json:"n"`
		ID           string `json:"i,omitempty"`
		Page         int    `json:"p"`
		SingerParent string `json:"r,omitempty"`
	}{Name: name, ID: artistID, Page: page}
	if len(singerParent) > 0 {
		payload.SingerParent = singerParent[0]
	}
	return KindArtistAlbumPage + "-" + source + "-" + encodeVirtualID(payload)
}

// AlbumLocator 是在线目录中可逆的专辑定位信息。
type AlbumLocator struct {
	Source string `json:"s"`
	ID     string `json:"i,omitempty"`
	Name   string `json:"n"`
	Artist string `json:"a,omitempty"`
	Image  string `json:"p,omitempty"`
	Parent string `json:"r,omitempty"`
}

// OnlineAlbumID 生成不会与旧 al-* ID 混淆的在线专辑 ID。
// parent 可选；省略时仍兼容早期生成的 oa-* ID。
func OnlineAlbumID(source, albumID, name, artist, image string, parent ...string) string {
	locator := AlbumLocator{Source: source, ID: albumID, Name: name, Artist: artist, Image: image}
	if len(parent) > 0 {
		locator.Parent = parent[0]
	}
	return KindOnlineAlbum + "-" + source + "-" + encodeVirtualID(locator)
}

// ParseOnlineAlbumID 解析在线专辑 ID。
func ParseOnlineAlbumID(id string) (AlbumLocator, bool) {
	p, ok := ParseID(id)
	if !ok || p.Kind != KindOnlineAlbum || !IsPlatform(p.Source) {
		return AlbumLocator{}, false
	}
	var locator AlbumLocator
	if !decodeVirtualID(p.Key, &locator) || strings.TrimSpace(locator.Name) == "" {
		return AlbumLocator{}, false
	}
	locator.Source = p.Source
	return locator, true
}

// ParseArtistNameID 解析在线歌手名称目录 ID。
func ParseArtistNameID(id string) (category, name string, ok bool) {
	p, parsed := ParseID(id)
	if !parsed || p.Kind != KindArtistName || p.Source == "" {
		return "", "", false
	}
	if !decodeVirtualID(p.Key, &name) || strings.TrimSpace(name) == "" {
		return "", "", false
	}
	return p.Source, name, true
}

// SingerLocator 是可逆的平台歌手定位信息。
type SingerLocator struct {
	Source string
	Name   string
	ID     string
	Parent string
}

// ArtistAlbumPage 是可逆的专辑分页定位信息。
type ArtistAlbumPage struct {
	SingerLocator
	Page         int
	SingerParent string
}

// ParseSingerDirectoryID 解析平台歌手入口 ID。
func ParseSingerDirectoryID(id string) (SingerLocator, bool) {
	p, ok := ParseID(id)
	if !ok || p.Kind != KindSingerDir || !IsPlatform(p.Source) {
		return SingerLocator{}, false
	}
	var payload struct {
		Name   string `json:"n"`
		ID     string `json:"i"`
		Parent string `json:"r"`
	}
	if !decodeVirtualID(p.Key, &payload) || strings.TrimSpace(payload.Name) == "" {
		return SingerLocator{}, false
	}
	return SingerLocator{Source: p.Source, Name: payload.Name, ID: payload.ID, Parent: payload.Parent}, true
}

// ParseArtistAlbumPageID 解析歌手专辑分页 ID。
func ParseArtistAlbumPageID(id string) (ArtistAlbumPage, bool) {
	p, ok := ParseID(id)
	if !ok || p.Kind != KindArtistAlbumPage || !IsPlatform(p.Source) {
		return ArtistAlbumPage{}, false
	}
	var payload struct {
		Name         string `json:"n"`
		ID           string `json:"i"`
		Page         int    `json:"p"`
		SingerParent string `json:"r"`
	}
	if !decodeVirtualID(p.Key, &payload) || strings.TrimSpace(payload.Name) == "" || payload.Page < 1 {
		return ArtistAlbumPage{}, false
	}
	return ArtistAlbumPage{SingerLocator: SingerLocator{Source: p.Source, Name: payload.Name, ID: payload.ID}, Page: payload.Page, SingerParent: payload.SingerParent}, true
}

func encodeVirtualID(value any) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeVirtualID(value string, target any) bool {
	data, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && json.Unmarshal(data, target) == nil
}

// ParsedID 解析后的 ID
type ParsedID struct {
	Kind   string
	Source string
	Key    string
}

// ParseID 解析 "kind-source-key"，key 允许包含 '-'
func ParseID(id string) (ParsedID, bool) {
	parts := strings.SplitN(id, "-", 3)
	if len(parts) < 2 {
		return ParsedID{}, false
	}
	p := ParsedID{Kind: parts[0], Source: parts[1]}
	if len(parts) == 3 {
		p.Key = parts[2]
	}
	switch p.Kind {
	case KindTrack, KindAlbum, KindArtist, KindPlaylist, KindBoard, KindSongList, KindDir,
		KindArtistDir, KindArtistName, KindSingerDir, KindArtistAlbumPage, KindOnlineAlbum:
		return p, true
	}
	return ParsedID{}, false
}

// IsPlatform 是否为已知平台
func IsPlatform(s string) bool {
	switch s {
	case "kw", "kg", "tx", "wy", "mg":
		return true
	}
	return false
}

// PlatformName 平台中文名
func PlatformName(s string) string {
	switch s {
	case "kw":
		return "酷我"
	case "kg":
		return "酷狗"
	case "tx":
		return "QQ音乐"
	case "wy":
		return "网易云"
	case "mg":
		return "咪咕"
	}
	return s
}
