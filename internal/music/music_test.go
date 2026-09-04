package music

import "testing"

func TestIDs(t *testing.T) {
	id := TrackID("kg", "ABC-123")
	p, ok := ParseID(id)
	if !ok || p.Kind != KindTrack || p.Source != "kg" || p.Key != "ABC-123" {
		t.Fatalf("parse %s => %+v", id, p)
	}
	if AlbumID("wy", "1", "x", "y") != "al-wy-1" {
		t.Fatal("album id")
	}
	if AlbumID("wy", "", "专辑", "歌手") != AlbumID("tx", "0", "专辑", "歌手") {
		t.Fatal("hash album id should be platform independent")
	}
	if _, ok := ParseID("bogus"); ok {
		t.Fatal("bogus id parsed")
	}
}

func TestInfo(t *testing.T) {
	in, err := ParseInfo([]byte(`{"source":"kg","songmid":123,"hash":"abc","name":"晴天","singer":"周杰伦、A","albumName":"叶惠美","albumId":"9","interval":"04:29","types":[{"type":"128k"},{"type":"flac"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.TrackID() != "tr-kg-ABC" || in.Duration() != 269 || in.PrimarySinger() != "周杰伦" || in.BestQuality() != "flac" || in.AlbumSubID() != "al-kg-9" {
		t.Fatalf("%s %d %s %s %s", in.TrackID(), in.Duration(), in.PrimarySinger(), in.BestQuality(), in.AlbumSubID())
	}
}

func TestSelectQuality(t *testing.T) {
	tests := []struct {
		want      string
		available []string
		expect    string
	}{
		{"flac24bit", []string{"128k", "320k", "flac"}, "flac"},
		{"flac", []string{"128k", "320k"}, "320k"},
		{"320k", []string{"128k"}, "128k"},
		{"128k", []string{"320k"}, "128k"},
		{"flac24bit", nil, "flac24bit"},
		{"unknown", []string{"128k", "320k"}, "320k"},
	}
	for _, test := range tests {
		if got := SelectQuality(test.want, test.available); got != test.expect {
			t.Fatalf("SelectQuality(%q, %v) = %q，期望 %q", test.want, test.available, got, test.expect)
		}
	}
}

func TestLyrics(t *testing.T) {
	l := ParseLyrics("[ti:x]\n[offset:500]\n[00:01.50]第一行\n[00:03.2]第二行<0,100>词\n纯文本", "[00:01.500]line1\n[00:03.20]line2", "")
	if !l.Synced || l.Offset != 500 || len(l.Lines) != 3 {
		t.Fatalf("%+v", l)
	}
	if l.Lines[1].Start != 1500 || l.Lines[1].Value != "第一行" || l.Lines[2].Start != 3200 || l.Lines[2].Value != "第二行词" {
		t.Fatalf("%+v", l.Lines)
	}
	if len(l.Trans) != 2 || l.Trans[0].Start != 1500 {
		t.Fatalf("%+v", l.Trans)
	}
	m := l.MergedLRC()
	if m == "" || !contains(m, "[00:01.50]line1") {
		t.Fatal(m)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
