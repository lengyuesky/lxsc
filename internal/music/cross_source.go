package music

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"lxsc/internal/js"
)

// crossDurationTolerance 允许的时长差（秒）。同名翻唱/伴奏与原版通常差得更多。
const crossDurationTolerance = 3

// crossTitleDecorations 是需要剥离的固定修饰词；其余版本说明按括号整体剥除。
var crossTitleDecorations = []string{"(explicit)", "(clean)", "(remaster)", "(remastered)"}

// crossVersionMarkers 括号内含这些字样时整段视为版本说明（重制/现场/伴奏/DJ 版等）。
var crossVersionMarkers = []string{"live", "版", "remaster", "伴奏", "demo", "cover", "翻唱", "instrumental", "acoustic", "mix"}

// stripVersionNotes 去掉括号内的版本说明；括号里是正题的一部分（如 feat.）时保留。
func stripVersionNotes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r := s[i]
		if r != '(' && r != '[' {
			b.WriteByte(r)
			i++
			continue
		}
		closing := byte(')')
		if r == '[' {
			closing = ']'
		}
		end := strings.IndexByte(s[i:], closing)
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		inner := s[i+1 : i+end]
		drop := false
		for _, marker := range crossVersionMarkers {
			if strings.Contains(inner, marker) {
				drop = true
				break
			}
		}
		if !drop {
			b.WriteString(s[i : i+end+1])
		}
		i += end + 1
	}
	return b.String()
}

// normalizeCrossTitle 统一全半角、大小写与常见修饰词，用于同名判定。
func normalizeCrossTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '（':
			b.WriteRune('(')
		case '）':
			b.WriteRune(')')
		case '【':
			b.WriteRune('[')
		case '】':
			b.WriteRune(']')
		case '　':
			b.WriteRune(' ')
		default:
			if unicode.IsSpace(r) {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(r)
		}
	}
	out := stripVersionNotes(b.String())
	for _, decoration := range crossTitleDecorations {
		out = strings.ReplaceAll(out, decoration, "")
	}
	return strings.Join(strings.Fields(out), " ")
}

// crossSourceMatch 判断候选是否为同一首歌：标题归一化后必须完全相等，
// 歌手需有交集，双方时长都已知时必须落在容差内。宁可放弃兜底，也不播放错歌。
func crossSourceMatch(in, candidate *Info) bool {
	if in == nil || candidate == nil || in.Source() == candidate.Source() {
		return false
	}
	title := normalizeCrossTitle(in.Name())
	if title == "" || title != normalizeCrossTitle(candidate.Name()) {
		return false
	}
	if !crossSingerOverlap(in, candidate) {
		return false
	}
	if a, b := in.Duration(), candidate.Duration(); a > 0 && b > 0 {
		diff := a - b
		if diff < 0 {
			diff = -diff
		}
		if diff > crossDurationTolerance {
			return false
		}
	}
	return true
}

// crossSingerOverlap 任一歌手名相等即视为同一演唱者，兼容"张三、李四"与"李四"的写法差异。
func crossSingerOverlap(in, candidate *Info) bool {
	want := splitSingers(in.Singer())
	if len(want) == 0 {
		return true
	}
	for name := range splitSingers(candidate.Singer()) {
		if want[name] {
			return true
		}
	}
	return false
}

func splitSingers(s string) map[string]bool {
	out := map[string]bool{}
	for _, sep := range []string{"、", "/", "&", ",", "，", " feat. ", " feat ", " ft. "} {
		s = strings.ReplaceAll(s, sep, "\x00")
	}
	for _, part := range strings.Split(s, "\x00") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out[part] = true
		}
	}
	return out
}

// FindCrossSourceCandidate 在其它平台按歌名搜索并挑选同一首歌。
// 只返回匹配成功的候选，按"时长最接近、平台优先级靠前"排序后的第一个。
func (c *Catalog) FindCrossSourceCandidate(ctx context.Context, in *Info) *Info {
	if in == nil || strings.TrimSpace(in.Name()) == "" {
		return nil
	}
	query := strings.TrimSpace(in.Name() + " " + in.PrimarySinger())
	platforms := make([]string, 0, len(js.AllPlatforms))
	for _, platform := range js.AllPlatforms {
		if platform != in.Source() {
			platforms = append(platforms, platform)
		}
	}
	list, err := c.SearchChecked(ctx, query, SearchOptions{Sources: platforms, Limit: 20})
	if err != nil {
		return nil
	}
	type match struct {
		info *Info
		gap  int
		pos  int
	}
	var matches []match
	for index, candidate := range list {
		if !crossSourceMatch(in, candidate) {
			continue
		}
		gap := 0
		if a, b := in.Duration(), candidate.Duration(); a > 0 && b > 0 {
			gap = a - b
			if gap < 0 {
				gap = -gap
			}
		}
		matches = append(matches, match{candidate, gap, index})
	}
	if len(matches) == 0 {
		return nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].gap != matches[j].gap {
			return matches[i].gap < matches[j].gap
		}
		return matches[i].pos < matches[j].pos
	})
	return matches[0].info
}
