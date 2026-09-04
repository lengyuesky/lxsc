package music

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Line 一行歌词
type Line struct {
	Start int    `json:"start"` // 毫秒
	Value string `json:"value"`
}

// Lyrics 解析后的歌词
type Lyrics struct {
	Lines  []Line // 原文
	Trans  []Line // 翻译
	Roma   []Line // 罗马音
	Offset int
	Synced bool
	Raw    string // 原始 LRC
}

var (
	timeTagRe = regexp.MustCompile(`\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)
	metaTagRe = regexp.MustCompile(`^\[([a-zA-Z]+):(.*)\]$`)
	wordTagRe = regexp.MustCompile(`<\d+,\d+>|\(\d+,\d+\)`)
)

// ParseLRC 解析 LRC 文本
func ParseLRC(lrc string) (lines []Line, offset int, synced bool) {
	lrc = strings.ReplaceAll(lrc, "\r\n", "\n")
	lrc = strings.ReplaceAll(lrc, "\r", "\n")
	for _, raw := range strings.Split(lrc, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if m := metaTagRe.FindStringSubmatch(raw); m != nil {
			if strings.EqualFold(m[1], "offset") {
				if n, err := strconv.Atoi(strings.TrimSpace(m[2])); err == nil {
					offset = n
				}
			}
			continue
		}
		tags := timeTagRe.FindAllStringSubmatchIndex(raw, -1)
		if len(tags) == 0 {
			lines = append(lines, Line{Start: -1, Value: raw})
			continue
		}
		// 文本为最后一个时间标签之后的内容
		text := strings.TrimSpace(raw[tags[len(tags)-1][1]:])
		text = wordTagRe.ReplaceAllString(text, "")
		for _, t := range tags {
			mm, _ := strconv.Atoi(raw[t[2]:t[3]])
			ss, _ := strconv.Atoi(raw[t[4]:t[5]])
			ms := 0
			if t[6] >= 0 {
				frac := raw[t[6]:t[7]]
				switch len(frac) {
				case 1:
					frac += "00"
				case 2:
					frac += "0"
				}
				ms, _ = strconv.Atoi(frac)
			}
			lines = append(lines, Line{Start: mm*60000 + ss*1000 + ms, Value: text})
			synced = true
		}
	}
	if synced {
		// 无时间标签的行放到开头（通常是头部信息）
		sort.SliceStable(lines, func(i, j int) bool {
			a, b := lines[i].Start, lines[j].Start
			if a < 0 {
				a = 0
			}
			if b < 0 {
				b = 0
			}
			return a < b
		})
		for i := range lines {
			if lines[i].Start < 0 {
				lines[i].Start = 0
			}
		}
	} else {
		for i := range lines {
			lines[i].Start = 0
		}
	}
	return lines, offset, synced
}

// ParseLyrics 解析原文/翻译/罗马音
func ParseLyrics(lyric, tlyric, rlyric string) *Lyrics {
	l := &Lyrics{Raw: lyric}
	l.Lines, l.Offset, l.Synced = ParseLRC(lyric)
	if strings.TrimSpace(tlyric) != "" {
		l.Trans, _, _ = ParseLRC(tlyric)
		l.Trans = dropEmpty(l.Trans)
	}
	if strings.TrimSpace(rlyric) != "" {
		l.Roma, _, _ = ParseLRC(rlyric)
		l.Roma = dropEmpty(l.Roma)
	}
	return l
}

func dropEmpty(lines []Line) []Line {
	out := lines[:0]
	for _, ln := range lines {
		if strings.TrimSpace(ln.Value) != "" && ln.Value != "//" {
			out = append(out, ln)
		}
	}
	return out
}

// PlainText 纯文本歌词（去时间标签）
func (l *Lyrics) PlainText() string {
	var sb strings.Builder
	for _, ln := range l.Lines {
		sb.WriteString(ln.Value)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// MergedLRC 生成原文+翻译合并的 LRC（同一时间点两行）
func (l *Lyrics) MergedLRC() string {
	var sb strings.Builder
	trans := map[int]string{}
	for _, t := range l.Trans {
		trans[t.Start] = t.Value
	}
	for _, ln := range l.Lines {
		tag := formatTag(ln.Start)
		sb.WriteString(tag)
		sb.WriteString(ln.Value)
		sb.WriteByte('\n')
		if tv, ok := trans[ln.Start]; ok && tv != "" && ln.Value != "" {
			sb.WriteString(tag)
			sb.WriteString(tv)
			sb.WriteByte('\n')
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatTag(ms int) string {
	if ms < 0 {
		ms = 0
	}
	return "[" + pad2(ms/60000) + ":" + pad2(ms%60000/1000) + "." + pad2(ms%1000/10) + "]"
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
