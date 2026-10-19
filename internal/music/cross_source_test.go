package music

import "testing"

func crossInfo(source, name, singer, interval string) *Info {
	return FromMap(map[string]any{"source": source, "name": name, "singer": singer, "interval": interval, "songmid": source + "-1"})
}

func TestCrossSourceMatchRequiresSameSong(t *testing.T) {
	base := crossInfo("tx", "曾经是情侣 (Live)", "梁博", "04:30")
	cases := []struct {
		name  string
		want  *Info
		match bool
	}{
		{"同名同歌手跨平台", crossInfo("wy", "曾经是情侣 (Live)", "梁博", "04:31"), true},
		{"全角与修饰词归一化", crossInfo("kw", "曾经是情侣（live）", "梁博", ""), true},
		{"多歌手写法差异", crossInfo("kg", "曾经是情侣 (Live)", "梁博/某某", "04:30"), true},
		{"不同歌名", crossInfo("wy", "日落大道", "梁博", "04:30"), false},
		{"不同歌手", crossInfo("wy", "曾经是情侣 (Live)", "张三", "04:30"), false},
		{"时长差距过大", crossInfo("wy", "曾经是情侣 (Live)", "梁博", "05:00"), false},
		{"同平台不算兜底", crossInfo("tx", "曾经是情侣 (Live)", "梁博", "04:30"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := crossSourceMatch(base, tc.want); got != tc.match {
				t.Fatalf("crossSourceMatch=%v，期望 %v", got, tc.match)
			}
		})
	}
}

func TestNormalizeCrossTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"曾经是情侣 (Live)", "曾经是情侣"},
		{"曾经是情侣（LIVE）", "曾经是情侣"},
		{"Song　Name  (Remastered)", "song name"},
		{"冻结 (DJ 阿若版)", "冻结"},
	} {
		if got := normalizeCrossTitle(tc.in); got != tc.want {
			t.Fatalf("normalizeCrossTitle(%q)=%q，期望 %q", tc.in, got, tc.want)
		}
	}
}
