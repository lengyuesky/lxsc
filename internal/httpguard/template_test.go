package httpguard

import (
	"net/url"
	"testing"
)

func TestCustomAPITemplateEncodingAndAuthority(t *testing.T) {
	fields := map[string]string{"title": "歌名/{artist}&injected=1?#", "artist": "A + B", "album": "专辑", "id": "id/one", "source": "wy"}
	for _, template := range []string{
		"https://api.example/{id}?name={title}&singer={artist}",
		"https://api.example/%7bid%7d?name=%7Btitle%7D&singer={artist}",
	} {
		address, err := ExpandTemplate(template, fields)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(address)
		if err != nil || u.Host != "api.example" || u.EscapedPath() != "/id%2Fone" || u.Query().Get("name") != fields["title"] || u.Query().Get("singer") != fields["artist"] || len(u.Query()) != 2 || u.Fragment != "" {
			t.Fatalf("歌曲信息被当成 URL 结构或再次展开：%s", address)
		}
	}
	address, err := ExpandTemplate("https://api.lrc.cx/cover?fixed=1", fields)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(address)
	if len(u.Query()) != 4 || u.Query().Get("title") != fields["title"] || u.Query().Get("artist") != fields["artist"] || u.Query().Get("album") != "专辑" {
		t.Fatalf("普通接口应只附加 LrcAPI 的三个参数：%s", address)
	}
	for _, value := range []string{
		"http://127.0.0.1/a", "http://10.0.0.1/a", "http://[::1]/a", "file:///tmp/a",
		"https://user:password@api.example/a", "https://api.example:8080/a", "https://api.example/a#fragment",
		"https://{source}.example/a", "https://api.example/{unknown}", "https://api.example/a?{title}=x",
		"https://api.example/a?title={unknown}", "https://api.example/a?title=%GG", "https://api.example/{{title}}",
	} {
		if _, err := ParseTemplate(value); err == nil {
			t.Errorf("错误地接受了危险地址或未知占位符：%s", value)
		}
	}
}
