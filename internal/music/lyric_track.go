package music

import (
	"context"
	"strings"
)

// MatchesLyricTrack 避免传统歌词接口把搜索首项（翻唱、伴奏等）当作当前歌曲。
func MatchesLyricTrack(in *Info, artist, title string) bool {
	if in == nil || strings.TrimSpace(title) == "" || !strings.EqualFold(strings.TrimSpace(in.Name()), strings.TrimSpace(title)) {
		return false
	}
	artist = strings.TrimSpace(artist)
	return artist == "" || strings.EqualFold(strings.TrimSpace(in.Singer()), artist) || strings.EqualFold(strings.TrimSpace(in.PrimarySinger()), artist)
}

// LocalLyricTrack 按客户端已获得的元数据定位歌曲，优先最近浏览/播放的版本。
// 传统 getLyrics 只有歌名和歌手；重新在线搜索会在平台故障时丢失本可读取的歌词。
func (c *Catalog) LocalLyricTrack(ctx context.Context, artist, title string) *Info {
	if strings.TrimSpace(title) == "" {
		return nil
	}
	cached := c.tracks.Values()
	for i := len(cached) - 1; i >= 0; i-- {
		if MatchesLyricTrack(cached[i], artist, title) {
			return cached[i]
		}
	}
	if c.DB == nil {
		return nil
	}
	rows, err := c.DB.SearchTracks(ctx, strings.TrimSpace(title), 100, 0)
	if err != nil {
		return nil
	}
	for _, row := range rows {
		in, err := ParseInfo(row.JSON)
		if err == nil && MatchesLyricTrack(in, artist, title) {
			c.Cache([]*Info{in})
			return in
		}
	}
	return nil
}
