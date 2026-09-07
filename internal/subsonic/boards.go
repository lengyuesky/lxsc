package subsonic

import (
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

// boardVisibility 将一次请求的设置快照转换为目录和歌单共用的展示规则。
type boardVisibility struct {
	sources []string
	allowed map[string]map[string]bool
}

func newBoardVisibility(v settings.Values) boardVisibility {
	display := boardVisibility{allowed: map[string]map[string]bool{}}
	if !v.ShowBoards {
		return display
	}
	for _, source := range uniquePlatforms(v.BoardSources) {
		ids, custom := v.BoardSelections[source]
		if custom && len(ids) == 0 {
			continue
		}
		var selected map[string]bool
		if custom {
			selected = make(map[string]bool, len(ids))
			for _, id := range ids {
				selected[id] = true
			}
		}
		display.sources = append(display.sources, source)
		display.allowed[source] = selected
	}
	return display
}

func (v boardVisibility) includesSource(source string) bool {
	_, ok := v.allowed[source]
	return ok
}

func (v boardVisibility) allows(board music.Board) bool {
	selected, ok := v.allowed[board.Source]
	return ok && (selected == nil || selected[board.BangID])
}
