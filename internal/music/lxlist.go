package music

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// 洛雪音乐（LX Music）歌单备份文件解析。
// 支持：
//   - 桌面版 .lxmc（gzip 压缩的 JSON）与未压缩的 .json
//   - type = playList_v2：data 为列表数组，歌曲为 v2 结构（含 meta 字段）
//   - type = playList：旧版结构，data 含 defaultList / loveList / userList，歌曲为扁平结构
//   - type = allData / allData_v2：整体备份，从 data 中提取上述列表

const (
	maxLXListRaw     = 32 << 20  // 压缩前最大字节数
	maxLXListInflate = 128 << 20 // 解压后最大字节数
)

// LXPlaylist 解析出的一个歌单
type LXPlaylist struct {
	ID       string  // 洛雪列表 ID（default / love / 平台前缀+哈希）
	Name     string  // 显示名称（已翻译内置列表名）
	Source   string  // 在线歌单来源平台（用户自建列表为空）
	Tracks   []*Info // 已转换为音源脚本可用的扁平结构
	Skipped  int     // 因缺少关键字段或本地歌曲而跳过的数量
	Original int     // 文件中的原始歌曲数
}

// ParseLXList 解析洛雪歌单备份（自动识别 gzip）
func ParseLXList(data []byte) ([]LXPlaylist, error) {
	if len(data) == 0 {
		return nil, errors.New("文件为空")
	}
	if len(data) > maxLXListRaw {
		return nil, errors.New("文件过大")
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("解压失败: %w", err)
		}
		defer zr.Close()
		buf, err := io.ReadAll(io.LimitReader(zr, maxLXListInflate+1))
		if err != nil {
			return nil, fmt.Errorf("解压失败: %w", err)
		}
		if len(buf) > maxLXListInflate {
			return nil, errors.New("文件解压后过大")
		}
		data = buf
	}
	var root struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &root); err != nil {
		return nil, errors.New("不是有效的洛雪歌单文件（JSON 解析失败）")
	}
	if len(root.Data) == 0 {
		return nil, errors.New("不是有效的洛雪歌单文件（缺少 data 字段）")
	}
	var rawLists []rawLXList
	switch root.Type {
	case "playList_v2":
		if err := json.Unmarshal(root.Data, &rawLists); err != nil {
			return nil, errors.New("playList_v2 结构不正确")
		}
	case "playList", "allData", "allData_v2", "":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(root.Data, &obj); err != nil {
			// 有些导出直接把列表数组放在 data 里
			if err2 := json.Unmarshal(root.Data, &rawLists); err2 != nil {
				return nil, errors.New("无法识别的歌单文件结构")
			}
			break
		}
		rawLists = collectLXLists(obj)
	default:
		return nil, fmt.Errorf("不支持的文件类型: %s", root.Type)
	}
	if len(rawLists) == 0 {
		return nil, errors.New("文件中没有歌单")
	}
	out := make([]LXPlaylist, 0, len(rawLists))
	for i, rl := range rawLists {
		pl := LXPlaylist{ID: rl.ID, Name: lxListName(rl.ID, rl.Name, i), Source: rl.Source, Original: len(rl.List)}
		pl.Tracks = make([]*Info, 0, len(rl.List))
		for _, item := range rl.List {
			in := lxSongToInfo(item)
			if in == nil {
				pl.Skipped++
				continue
			}
			pl.Tracks = append(pl.Tracks, in)
		}
		out = append(out, pl)
	}
	return out, nil
}

type rawLXList struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Source string            `json:"source"`
	List   []json.RawMessage `json:"list"`
}

// collectLXLists 从旧版 / 整体备份的 data 对象中提取列表
func collectLXLists(obj map[string]json.RawMessage) []rawLXList {
	var out []rawLXList
	one := func(key, id string) {
		raw, ok := obj[key]
		if !ok {
			return
		}
		var l rawLXList
		if err := json.Unmarshal(raw, &l); err != nil {
			return
		}
		if l.ID == "" {
			l.ID = id
		}
		out = append(out, l)
	}
	many := func(key string) {
		raw, ok := obj[key]
		if !ok {
			return
		}
		var ls []rawLXList
		if err := json.Unmarshal(raw, &ls); err == nil {
			out = append(out, ls...)
		}
	}
	one("defaultList", "default")
	one("loveList", "love")
	many("userList")
	many("playList") // allData_v2 中的 v2 列表
	return out
}

// lxListName 内置列表的 i18n 键转中文
func lxListName(id, name string, index int) string {
	switch name {
	case "list__name_default":
		return "默认列表"
	case "list__name_love":
		return "我的收藏"
	case "list__name_temp":
		return "临时列表"
	}
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	switch id {
	case "default":
		return "默认列表"
	case "love":
		return "我的收藏"
	case "temp":
		return "临时列表"
	}
	return fmt.Sprintf("导入列表 %d", index+1)
}

// lxSongToInfo 把洛雪歌曲（v2 含 meta，或旧版扁平结构）转换为 Info；无法使用时返回 nil
func lxSongToInfo(raw json.RawMessage) *Info {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil
	}
	source := anyToString(m["source"])
	if !IsPlatform(source) {
		return nil // local 等本地歌曲无法在服务端播放
	}
	meta, hasMeta := m["meta"].(map[string]any)
	if !hasMeta {
		// 旧版结构已是扁平 musicInfo
		in := FromMap(m)
		if in.Key() == "" {
			return nil
		}
		return in
	}
	// v2 → 旧版扁平结构（对应洛雪 toOldMusicInfo）
	out := map[string]any{
		"name":      anyToString(m["name"]),
		"singer":    anyToString(m["singer"]),
		"source":    source,
		"songmid":   metaValue(meta, "songId"),
		"interval":  anyToString(m["interval"]),
		"albumId":   metaValue(meta, "albumId"),
		"albumName": anyToString(meta["albumName"]),
		"img":       anyToString(meta["picUrl"]),
		"typeUrl":   map[string]any{},
	}
	if q, ok := meta["qualitys"]; ok {
		out["types"] = q
	} else {
		out["types"] = []any{}
	}
	if q, ok := meta["_qualitys"]; ok {
		out["_types"] = q
	} else {
		out["_types"] = map[string]any{}
	}
	switch source {
	case "kg":
		out["hash"] = anyToString(meta["hash"])
	case "tx":
		out["strMediaMid"] = anyToString(meta["strMediaMid"])
		out["albumMid"] = anyToString(meta["albumMid"])
		out["songId"] = metaValue(meta, "id")
	case "mg":
		out["copyrightId"] = anyToString(meta["copyrightId"])
		out["lrcUrl"] = anyToString(meta["lrcUrl"])
		out["mrcUrl"] = anyToString(meta["mrcUrl"])
		out["trcUrl"] = anyToString(meta["trcUrl"])
	}
	in := FromMap(out)
	if in.Key() == "" {
		return nil
	}
	return in
}

// metaValue 保留原始类型（数字/字符串），nil 转为空串
func metaValue(meta map[string]any, key string) any {
	v, ok := meta[key]
	if !ok || v == nil {
		return ""
	}
	return v
}
