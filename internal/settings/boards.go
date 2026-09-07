package settings

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseBoardSelections 只校验结构和稳定标识，不依赖平台当前能否返回榜单。
func parseBoardSelections(raw []byte) (map[string][]string, error) {
	var selections map[string][]string
	if err := json.Unmarshal(raw, &selections); err != nil || selections == nil {
		return nil, fmt.Errorf("%w: 榜单选择必须是以平台为键、榜单 ID 数组为值的对象", ErrInvalidSetting)
	}
	for source, ids := range selections {
		switch source {
		case "wy", "tx", "kw", "kg", "mg":
		default:
			return nil, fmt.Errorf("%w: 不支持的榜单平台 %q", ErrInvalidSetting, source)
		}
		if ids == nil {
			return nil, fmt.Errorf("%w: %s 的榜单选择必须是数组，清空请使用 []", ErrInvalidSetting, source)
		}
		unique := make([]string, 0, len(ids))
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" || id == "0" {
				return nil, fmt.Errorf("%w: %s 的榜单 ID 不能为空或为 0", ErrInvalidSetting, source)
			}
			if !seen[id] {
				seen[id] = true
				unique = append(unique, id)
			}
		}
		selections[source] = unique
	}
	return selections, nil
}
