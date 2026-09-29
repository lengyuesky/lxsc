package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"lxsc/internal/db"
)

var (
	playlistNumber = regexp.MustCompile(`^[0-9]{1,20}$`)
	playlistLink   = regexp.MustCompile(`https?://[^\s<>"'，。]+`)
	wyPlaylistPath = regexp.MustCompile(`^/(?:m/)?playlist/(\d+)(?:/.*)?$`)
	txPlaylistPath = regexp.MustCompile(`^/n/(?:ryqq|yqq)/(?:playlist|playsquare)/(\d+)(?:\.html)?/?$`)
)

// ParseOnlinePlaylistID 只提取已知平台链接中的数字 ID，绝不把用户 URL 交给 SDK 请求。
// 支持完整分享文案；短链接需在浏览器打开后复制完整歌单链接。
func ParseOnlinePlaylistID(source, input string) (string, error) {
	if source != "wy" && source != "tx" {
		return "", errors.New("仅支持网易云音乐和 QQ 音乐歌单")
	}
	input = strings.TrimSpace(input)
	invalid := errors.New("请输入所选平台的完整歌单链接或数字 ID；短链接请先在浏览器打开后复制完整链接")
	if len(input) > 4096 {
		return "", invalid
	}
	if playlistNumber.MatchString(input) && strings.Trim(input, "0") != "" {
		return input, nil
	}
	link := playlistLink.FindString(input)
	u, err := url.Parse(link)
	if err != nil || link == "" || u.User != nil || u.Port() != "" {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	if source == "wy" && host != "music.163.com" && host != "y.music.163.com" {
		return "", invalid
	}
	if source == "tx" && host != "y.qq.com" && host != "i.y.qq.com" && host != "c.y.qq.com" {
		return "", invalid
	}
	// 网易云桌面链接使用片段路由，QQ 的部分移动链接也使用此形式。
	if strings.HasPrefix(u.Fragment, "/") {
		u, err = url.Parse(u.Fragment)
		if err != nil {
			return "", invalid
		}
	}
	var id string
	if source == "wy" {
		if u.Path == "/playlist" || u.Path == "/m/playlist" {
			id = u.Query().Get("id")
		} else if match := wyPlaylistPath.FindStringSubmatch(u.Path); match != nil {
			id = match[1]
		}
	} else {
		if match := txPlaylistPath.FindStringSubmatch(u.Path); match != nil {
			id = match[1]
		} else if u.Path == "/n/m/detail/taoge/index.html" || u.Path == "/n2/m/share/details/taoge.html" || u.Path == "/n/ryqq/playlist" || u.Path == "/qzone/qqmusic.html" {
			for _, key := range []string{"id", "disstid"} {
				if u.Query().Get(key) != "" {
					id = u.Query().Get(key)
					break
				}
			}
		}
	}
	if !playlistNumber.MatchString(id) || strings.Trim(id, "0") == "" {
		return "", invalid
	}
	return id, nil
}

// OnlinePlaylist 是有界抓取、去重后的在线歌单；预览不写数据库。
type OnlinePlaylist struct {
	Name      string
	Source    string
	URL       string
	Tracks    []*Info
	Total     int
	Skipped   int
	Truncated int
}

// FetchOnlinePlaylist 拉取公开歌单，最多保留网页歌单允许的歌曲数。
// 网易云可能分页，也可能一次返回全部；QQ 的第二参数是重试次数而非页码。
func (c *Catalog) FetchOnlinePlaylist(ctx context.Context, source, id string) (*OnlinePlaylist, error) {
	if _, err := ParseOnlinePlaylistID(source, id); err != nil || !playlistNumber.MatchString(id) {
		return nil, errors.New("无效的在线歌单 ID")
	}
	out := &OnlinePlaylist{Source: source, Tracks: []*Info{}}
	if source == "wy" {
		out.URL = "https://music.163.com/#/playlist?id=" + id
	} else {
		out.URL = "https://y.qq.com/n/ryqq/playlist/" + id
	}
	seen := map[string]bool{}
	processed := 0
	// 防止异常分页造成无界上游请求。
	for page := 1; page <= 20; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		args := []any{id}
		if source == "wy" {
			args = append(args, page)
		}
		raw, err := c.cachedCallRaw(ctx, fmt.Sprintf("sl|%s|%s|%d", source, id, page), source+".songList.getListDetail", args...)
		if err != nil {
			return nil, err
		}
		var result struct {
			List  []map[string]any `json:"list"`
			Total *int             `json:"total"`
			Limit int              `json:"limit"`
			Info  *struct {
				Name string `json:"name"`
			} `json:"info"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || result.List == nil || result.Total == nil || *result.Total < 0 || *result.Total > 1000000 || result.Info == nil {
			return nil, errors.New("平台返回了无效的歌单数据")
		}
		if page == 1 {
			out.Name = strings.TrimSpace(result.Info.Name)
			out.Total = *result.Total
			if out.Name == "" {
				out.Name = PlatformName(source) + "歌单 " + id
			}
		} else if out.Total != *result.Total {
			return nil, errors.New("源歌单在读取期间发生变化，请重新导入")
		}
		newIDs := 0
		for _, item := range result.List {
			processed++
			if item == nil {
				out.Skipped++
				continue
			}
			if _, ok := item["source"]; !ok {
				item["source"] = source
			}
			in := FromMap(item)
			if in.Source() != source || !validPlatformID(in.Key()) || len(in.Key()) > 128 || strings.TrimSpace(in.Name()) == "" {
				out.Skipped++
				continue
			}
			if seen[in.TrackID()] {
				continue
			}
			seen[in.TrackID()] = true
			newIDs++
			if len(out.Tracks) < db.MaxPlaylistTracks {
				out.Tracks = append(out.Tracks, in)
			} else {
				out.Truncated++
			}
		}
		if page > 1 && len(result.List) > 0 && newIDs == 0 {
			return nil, errors.New("平台返回了重复分页，请稍后重试")
		}
		if processed >= out.Total {
			c.Cache(out.Tracks)
			return out, nil
		}
		if len(out.Tracks) >= db.MaxPlaylistTracks {
			out.Truncated += out.Total - processed
			c.Cache(out.Tracks)
			return out, nil
		}
		if source == "tx" || len(result.List) == 0 || newIDs == 0 || result.Limit <= 0 {
			return nil, errors.New("平台未返回完整歌单，请确认歌单公开且可访问后重试")
		}
		// SDK 的分页可能过滤已失效歌曲，不能用有效歌曲数判断是否到末页。
		if page*result.Limit >= out.Total {
			out.Skipped += max(0, out.Total-processed)
			c.Cache(out.Tracks)
			return out, nil
		}
	}
	return nil, errors.New("歌单分页过多，请缩小歌单后重试")
}
