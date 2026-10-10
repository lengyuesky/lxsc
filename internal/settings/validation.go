package settings

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"lxsc/internal/httpguard"
)

// ValidatePatch 供调试等入口在记录维护操作前使用；与 Update 共用全部校验规则。
func ValidatePatch(patch map[string]json.RawMessage) error {
	_, err := normalizePatch(patch)
	return err
}

// normalizePatch 先完整校验再落库，不能把非法值静默保存或部分发布。
// 整数的十进制字符串、平台逗号列表和布尔字符串继续兼容旧管理调用。
func normalizePatch(patch map[string]json.RawMessage) (map[string]string, error) {
	if patch == nil {
		return nil, fmt.Errorf("%w: 必须提供设置对象", ErrInvalidSetting)
	}
	values := make(map[string]string, len(patch))
	for key, raw := range patch {
		invalid := func(reason string) (map[string]string, error) {
			return nil, fmt.Errorf("%w: %s %s", ErrInvalidSetting, key, reason)
		}
		if !json.Valid(raw) || strings.TrimSpace(string(raw)) == "null" {
			return invalid("不能是空值或无效 JSON")
		}
		switch key {
		case "customLyricsURL", "customCoverURL":
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return invalid("必须是接口地址字符串")
			}
			value = strings.TrimSpace(value)
			if _, err := httpguard.ParseTemplate(value); value != "" && err != nil {
				return invalid(httpguard.ErrInvalidTemplate.Error())
			}
			values[key] = value
		case "boardSelections":
			selections, err := parseBoardSelections(raw)
			if err != nil {
				return nil, err
			}
			encoded, _ := json.Marshal(selections)
			values[key] = string(encoded)
		case "urlCacheTTL", "searchCacheTTL", "boardLimit", "boardTrackLimit", "artistSongLimit", "artistAlbumLimit", "searchLimit":
			value := strings.TrimSpace(string(raw))
			var text string
			if json.Unmarshal(raw, &text) == nil {
				value = text
			}
			var number int
			var err error
			switch key {
			case "urlCacheTTL":
				number, err = parseURLCacheTTL(value)
			case "searchCacheTTL":
				number, err = parseTTL(value)
			default:
				number, err = strconv.Atoi(value)
				maximum := 100
				if key == "boardLimit" {
					maximum = 20
				} else if key == "artistAlbumLimit" {
					maximum = 50
				}
				if number < 1 || number > maximum {
					err = ErrInvalidSetting
				}
			}
			if err != nil {
				return invalid("必须是允许范围内的整数")
			}
			values[key] = strconv.Itoa(number)
		case "showBoards", "publicPlaylists":
			value := strings.TrimSpace(string(raw))
			var text string
			if json.Unmarshal(raw, &text) == nil {
				value = text
			}
			if value != "true" && value != "false" && value != "1" && value != "0" {
				return invalid("必须是布尔值")
			}
			values[key] = strconv.FormatBool(value == "true" || value == "1")
		case "searchSources", "boardSources":
			var sources []string
			var text string
			if json.Unmarshal(raw, &text) == nil {
				sources = splitList(text)
			} else if json.Unmarshal(raw, &sources) != nil {
				return invalid("必须是平台数组或逗号分隔的平台列表")
			}
			seen := make(map[string]bool, len(sources))
			unique := make([]string, 0, 5)
			for _, source := range sources {
				source = strings.TrimSpace(source)
				if !slices.Contains([]string{"wy", "tx", "kw", "kg", "mg"}, source) {
					return invalid("包含未知平台")
				}
				if !seen[source] {
					seen[source] = true
					unique = append(unique, source)
				}
			}
			values[key] = strings.Join(unique, ",")
		case "streamMode", "coverMode", "defaultQuality", "serverName":
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return invalid("必须是字符串")
			}
			allowed := []string{"redirect", "proxy"}
			switch key {
			case "streamMode":
				allowed = append(allowed, "force_redirect")
			case "defaultQuality":
				allowed = []string{"128k", "192k", "320k", "flac", "flac24bit", "hires", "atmos", "atmos_plus", "master", "dolby"}
			case "serverName":
				if strings.TrimSpace(value) == "" || len(value) > 200 {
					return invalid("不能为空且不能超过 200 字节")
				}
				values[key] = value
				continue
			}
			if !slices.Contains(allowed, value) {
				return invalid("不是支持的选项")
			}
			values[key] = value
		default:
			return invalid("是未知设置")
		}
	}
	return values, nil
}
