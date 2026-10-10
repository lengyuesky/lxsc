package httpguard

import (
	"errors"
	"net/url"
	"strings"
)

var templateFields = []string{"title", "artist", "album", "id", "source", "trackId", "duration"}

var ErrInvalidTemplate = errors.New("接口地址必须是公网 HTTP(S) 地址，参数仅支持 title、artist、album、id、source、trackId、duration")

// ParseTemplate 只允许在路径或查询值中填写占位符，不能用歌曲信息改变请求主机。
func ParseTemplate(value string) (*url.URL, error) {
	if len(value) > 2048 || strings.TrimSpace(value) != value {
		return nil, ErrInvalidTemplate
	}
	u, err := url.Parse(value)
	if err != nil || u == nil || u.User != nil || strings.ContainsAny(u.Host, "{}") {
		return nil, ErrInvalidTemplate
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, ErrInvalidTemplate
	}
	valid := func(text string) bool {
		for _, field := range templateFields {
			text = strings.ReplaceAll(text, "{"+field+"}", "value")
		}
		return !strings.ContainsAny(text, "{}")
	}
	if !valid(u.Path) {
		return nil, ErrInvalidTemplate
	}
	for key, values := range query {
		if strings.ContainsAny(key, "{}") {
			return nil, ErrInvalidTemplate
		}
		for _, item := range values {
			if !valid(item) {
				return nil, ErrInvalidTemplate
			}
		}
	}
	if !ValidTarget(u) {
		return nil, ErrInvalidTemplate
	}
	return u, nil
}

// ExpandTemplate 对路径和查询参数分别编码；仅填地址时追加通用歌曲参数。
func ExpandTemplate(value string, fields map[string]string) (string, error) {
	u, err := ParseTemplate(value)
	if err != nil {
		return "", err
	}
	used := false
	path := strings.NewReplacer("%7b", "%7B", "%7d", "%7D").Replace(u.EscapedPath())
	query := u.Query()
	var pathPairs, queryPairs []string
	for _, field := range templateFields {
		tag := "{" + field + "}"
		escapedTag := url.PathEscape(tag)
		if strings.Contains(path, escapedTag) {
			used = true
		}
		pathPairs = append(pathPairs, escapedTag, url.PathEscape(fields[field]))
		queryPairs = append(queryPairs, tag, fields[field])
		for _, values := range query {
			for _, item := range values {
				if strings.Contains(item, tag) {
					used = true
				}
			}
		}
	}
	// 单次替换，歌曲文本本身包含占位符时必须保持为普通文本。
	path = strings.NewReplacer(pathPairs...).Replace(path)
	replaceQuery := strings.NewReplacer(queryPairs...)
	for key, values := range query {
		for i, item := range values {
			values[i] = replaceQuery.Replace(item)
		}
		query[key] = values
	}
	u.Path, err = url.PathUnescape(path)
	if err != nil {
		return "", ErrInvalidTemplate
	}
	u.RawPath = path
	if !used {
		// LrcAPI 的通用请求只包含这三个字段；其他平台标识须显式配置占位符。
		for _, field := range []string{"title", "artist", "album"} {
			if !query.Has(field) && fields[field] != "" {
				query.Set(field, fields[field])
			}
		}
	}
	u.RawQuery = query.Encode()
	if !ValidTarget(u) {
		return "", ErrInvalidTemplate
	}
	return u.String(), nil
}
