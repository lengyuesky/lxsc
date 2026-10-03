package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func fullBoardKey(source, id string) string { return "board-full|" + source + "|" + id }

// FullBoardTracks 按平台分页读完榜单，不按固定歌曲数截断。
// 只有完整结果才能进入整榜缓存；空页、重复页和失败不能伪装成完整榜单。
func (c *Catalog) FullBoardTracks(ctx context.Context, source, id string) ([]*Info, error) {
	if !IsPlatform(source) || !validPlatformID(id) {
		return nil, errors.New("无效的榜单ID")
	}
	key := fullBoardKey(source, id)
	if raw, ok := c.generic.Get(key); ok {
		return c.parseList(ctx, raw, source)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	all := make([]map[string]any, 0)
	seen := map[string]bool{}
	expected := -1
	offset := 0
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := c.cachedCallRaw(ctx, fmt.Sprintf("board|%s|%s|%d", source, id, page), source+".leaderboard.getList", id, page)
		if err != nil {
			return nil, err
		}
		var result struct {
			List  []map[string]any `json:"list"`
			Total *int             `json:"total"`
			Limit int              `json:"limit"`
			Page  *int             `json:"page"`
		}
		if json.Unmarshal(raw, &result) != nil || result.List == nil || result.Limit < 0 || (result.Total != nil && *result.Total < 0) {
			return nil, errors.New("平台返回了无效的榜单分页")
		}
		if result.Page != nil && *result.Page != page {
			return nil, errors.New("平台返回了错误的榜单页码")
		}
		if page == 1 && result.Total != nil {
			expected = *result.Total
		}
		if page > 1 && expected >= 0 && (result.Total == nil || *result.Total != expected) {
			return nil, errors.New("榜单在读取期间发生变化，请重试")
		}
		if len(result.List) == 0 && expected > offset {
			return nil, errors.New("平台提前返回空页，未取得完整榜单")
		}
		if expected == 0 && len(result.List) > 0 {
			return nil, errors.New("榜单总数与歌曲数据不一致")
		}
		added := 0
		for _, item := range result.List {
			if item == nil {
				continue
			}
			if _, ok := item["source"]; !ok {
				item["source"] = source
			}
			in := FromMap(item)
			if in.Source() != source || !validPlatformID(in.Key()) {
				continue
			}
			if seen[in.TrackID()] {
				continue
			}
			seen[in.TrackID()] = true
			all = append(all, item)
			added++
		}
		if page > 1 && len(result.List) > 0 && added == 0 {
			return nil, errors.New("平台重复返回同一榜单页，未取得完整榜单")
		}
		// SDK 可能过滤不可用歌曲，因此按原始分页容量判断是否读到末页。
		step := max(result.Limit, len(result.List))
		if step > int(^uint(0)>>1)-offset {
			return nil, errors.New("榜单分页数量无效")
		}
		offset += step
		complete := expected >= 0 && offset >= expected
		if expected < 0 {
			complete = result.Limit == 0 || len(result.List) < result.Limit
		}
		if complete {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			raw, err := json.Marshal(map[string]any{"list": all})
			if err != nil {
				return nil, err
			}
			c.generic.Add(key, raw)
			return c.parseList(ctx, raw, source)
		}
		if len(result.List) == 0 || added == 0 || result.Limit == 0 {
			return nil, errors.New("平台未返回完整榜单，请稍后重试")
		}
	}
}
