package music

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lxsc/internal/httpguard"
	"lxsc/internal/js"
)

const maxCustomLyricsBytes = 256 << 10

// TemplateValues 只提供歌曲元数据，不包含播放器账号、认证参数或原始请求头。
func (i *Info) TemplateValues() map[string]string {
	return map[string]string{
		"title": i.Name(), "artist": i.Singer(), "album": i.Album(),
		"id": i.Key(), "source": i.Source(), "trackId": i.TrackID(),
		"duration": strconv.Itoa(i.Duration()),
	}
}

func (c *Catalog) customLyric(ctx context.Context, in *Info, template string) (*Lyrics, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	release, err := c.customLimits.Acquire(ctx, 0)
	if err != nil {
		return nil, err
	}
	defer release()
	address, err := httpguard.ExpandTemplate(template, in.TemplateValues())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("自定义歌词接口地址无效")
	}
	req.Header.Set("Accept", "application/json, text/plain, application/x-lrc")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.customHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("自定义歌词接口请求失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("自定义歌词接口返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxCustomLyricsBytes {
		return nil, errors.New("自定义歌词响应过大")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCustomLyricsBytes+1))
	if err != nil || len(body) > maxCustomLyricsBytes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("自定义歌词读取失败或响应过大")
	}
	return parseCustomLyrics(body, resp.Header.Get("Content-Type"))
}

func parseCustomLyrics(body []byte, contentType string) (*Lyrics, error) {
	bad := errors.New("自定义歌词接口未返回可用歌词")
	contentType = strings.ToLower(strings.Split(contentType, ";")[0])
	body = bytes.TrimSpace(bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF")))
	// LrcAPI /lyrics 按文档返回 text/html，但正文是 LRC；按实际内容拒绝 HTML 错误页。
	if len(body) == 0 || !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 || strings.Contains(contentType, "xml") || strings.Contains(contentType, "javascript") || body[0] == '<' || strings.Contains(http.DetectContentType(body), "html") {
		return nil, bad
	}
	result := js.LyricResult{Lyric: string(body)}
	// LRC 时间标签同样以 [ 开始，不能按首字符把它误判为 JSON 数组。
	if strings.Contains(contentType, "json") || bytes.ContainsAny(body[:1], "{\"") || (body[0] == '[' && json.Valid(body)) {
		result = lyricJSON(body, 0)
	}
	if lyrics := usableLyrics(result); lyrics != nil {
		return lyrics, nil
	}
	return nil, bad
}

func lyricJSON(raw json.RawMessage, depth int) js.LyricResult {
	if depth > 3 {
		return js.LyricResult{}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return js.LyricResult{Lyric: text}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return js.LyricResult{}
	}
	field := func(names ...string) string {
		for _, name := range names {
			if value := object[name]; len(value) > 0 {
				var text string
				if json.Unmarshal(value, &text) == nil && strings.TrimSpace(text) != "" {
					return text
				}
				var nested struct {
					Lyric string `json:"lyric"`
				}
				if json.Unmarshal(value, &nested) == nil && strings.TrimSpace(nested.Lyric) != "" {
					return nested.Lyric
				}
			}
		}
		return ""
	}
	result := js.LyricResult{
		Lyric:  field("lyric", "lrc", "syncedLyrics", "lyrics", "plainLyrics"),
		TLyric: field("tlyric", "tLyric", "translatedLyrics"),
		RLyric: field("rlyric", "rLyric", "romanizedLyrics"),
	}
	if usableLyrics(result) == nil && len(object["data"]) > 0 {
		return lyricJSON(object["data"], depth+1)
	}
	return result
}
