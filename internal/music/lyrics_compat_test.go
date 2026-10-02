package music

import (
	"testing"

	"lxsc/internal/js"
)

func TestLyricsTextAndOffsetCompatibility(t *testing.T) {
	plain := ParseLyrics("\uFEFF[ti:歌曲]\n第一行\n第二行", "", "")
	if plain.Synced || plain.MergedLRC() != "第一行\n第二行" {
		t.Fatalf("纯文本不能被伪装成全部零时间的同步歌词: %+v", plain)
	}
	synced := ParseLyrics("\uFEFF[offset:-250]\n[00:01.50]歌词", "", "")
	if got := synced.MergedLRC(); got != "[offset:-250]\n[00:01.50]歌词" {
		t.Fatalf("传统客户端必须保留歌词偏移: %q", got)
	}
}

func TestUsableLyricsRejectsEmptyAndFallsBackToWordLyrics(t *testing.T) {
	for _, raw := range []string{"", " \r\n\t", "[ti:歌名]\n[offset:100]", "[00:00.00]\n[00:01.00]"} {
		if got := usableLyrics(js.LyricResult{Lyric: raw}); got != nil {
			t.Fatalf("不应接受没有正文的歌词 %q: %+v", raw, got)
		}
	}
	got := usableLyrics(js.LyricResult{LxLyric: "[00:01.00]<0,100>歌词"})
	if got == nil || got.Lines[0].Value != "歌词" {
		t.Fatalf("仅逐字歌词也应可显示: %+v", got)
	}
}

func TestLyricTrackMatchingRejectsWrongVersion(t *testing.T) {
	in := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "歌曲", "singer": "歌手"})
	if !MatchesLyricTrack(in, " 歌手 ", "歌曲") || MatchesLyricTrack(in, "其他歌手", "歌曲") || MatchesLyricTrack(in, "歌手", "歌曲 (伴奏)") || MatchesLyricTrack(in, "歌手", "") {
		t.Fatal("传统歌词定位必须核对标题和歌手")
	}
}
