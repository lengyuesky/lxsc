package subsonic

import (
	"bytes"
	"strings"
	"testing"
)

func TestXML(t *testing.T) {
	var buf bytes.Buffer
	writeXML(&buf, "root", M{"a": 1, "child": []M{{"id": "x", "value": "文本 & <b>"}}, "list": []int{1, 2}, "obj": M{"k": "v"}})
	s := buf.String()
	for _, want := range []string{`<root a="1">`, `<child id="x">文本 &amp; &lt;b&gt;</child>`, `<list>1</list><list>2</list>`, `<obj k="v"/>`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestParseQuery(t *testing.T) {
	s := &Server{}
	_ = s
	cases := map[string][]string{"wy:晴天": {"晴天", "wy"}, "TX：晴天": {"晴天", "tx"}, "local:abc": {"abc", "local"}, "晴天": {"晴天", "all"}}
	for in, want := range cases {
		q, src, local := parsePrefix(in)
		got := "all"
		if local {
			got = "local"
		} else if len(src) == 1 {
			got = src[0]
		}
		if q != want[0] || got != want[1] {
			t.Fatalf("%s => %s %s", in, q, got)
		}
	}
}
