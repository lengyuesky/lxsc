package music

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"lxsc/internal/js"
)

type customLyricTransport func(*http.Request) (*http.Response, error)

func (f customLyricTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCustomLyricsResponseFormats(t *testing.T) {
	for _, test := range []struct{ contentType, body string }{
		{"text/html; charset=utf-8", "[offset:100]\n[00:01.00]测试歌词"},
		{"text/plain", "[00:01.00]测试歌词"},
		{"application/json", `{"lyric":"[00:01.00]测试歌词","tlyric":"[00:01.00]翻译"}`},
		{"application/json", `{"data":{"lrc":{"lyric":"[00:01.00]测试歌词"}}}`},
		{"application/json", `{"syncedLyrics":"[00:01.00]测试歌词","plainLyrics":"测试歌词"}`},
	} {
		lyrics, err := parseCustomLyrics([]byte(test.body), test.contentType)
		if err != nil || !lyrics.Synced || len(lyrics.Lines) != 1 || lyrics.Lines[0].Value != "测试歌词" {
			t.Fatalf("有效歌词未正确解析：%s，%v", test.contentType, err)
		}
	}
	for _, body := range []string{"", "[ti:仅元数据]", "[00:00.00]", "<html>错误页面</html>", `{"error":"not found"}`, `[{"lyrics":"不能随意选择搜索第一项"}]`, "\x00\x01"} {
		if _, err := parseCustomLyrics([]byte(body), "text/html"); err == nil {
			t.Fatalf("空内容、错误页或未知响应被当成歌词：%q", body)
		}
	}
}

func TestCustomLyricsFallbackCacheAndSettingChange(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	in := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "歌名&x=1", "singer": "A + B", "albumName": "专辑"})
	calls := 0
	c.customHTTP = &http.Client{Transport: customLyricTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.Query().Get("title") != in.Name() || r.URL.Query().Get("artist") != in.Singer() || len(r.URL.Query()) != 3 {
			t.Fatal("自定义接口必须只接收正确编码的歌曲信息")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader("[00:01.00]" + r.URL.Host))}, nil
	})}
	for range 2 {
		lyrics, err := c.Lyric(context.Background(), in)
		if err != nil || lyrics.Lines[0].Value != "api.lrc.cx" {
			t.Fatal("默认公共接口没有在原歌词来源不可用时回退", err)
		}
	}
	if calls != 1 {
		t.Fatal("成功歌词未命中缓存", calls)
	}
	_, err := c.Settings.Update(context.Background(), map[string]json.RawMessage{"customLyricsURL": json.RawMessage(`"https://changed.example/lyrics"`)})
	if err != nil {
		t.Fatal(err)
	}
	lyrics, err := c.Lyric(context.Background(), in)
	if err != nil || lyrics.Lines[0].Value != "changed.example" || calls != 2 {
		t.Fatal("更改地址仍命中了旧接口的歌词", err)
	}
	_, _ = c.Settings.Update(context.Background(), map[string]json.RawMessage{"customLyricsURL": json.RawMessage(`""`)})
	if _, err := c.Lyric(context.Background(), in); err == nil || calls != 2 {
		t.Fatal("关闭后仍调用了自定义接口")
	}
}

func TestCustomLyricsNativeSourceFirstAndEmptyRetry(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	in := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "歌曲", "singer": "歌手"})
	pool, err := js.NewSDKPool(1, "", `globalThis.__sdk_call = () => ({lyric: '[00:01.00]原歌词'})`, &http.Client{}, &http.Client{}, c.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	c.SDK = pool
	calls := 0
	c.customHTTP = &http.Client{Transport: customLyricTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := "[ti:空歌词]"
		if calls > 1 {
			body = "[00:01.00]备用歌词"
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	lyrics, err := c.Lyric(context.Background(), in)
	if err != nil || lyrics.Lines[0].Value != "原歌词" || calls != 0 {
		t.Fatal("原歌词正常时不应访问备用接口", err)
	}
	c.SDK = nil
	c.lyrics.Purge()
	if _, err := c.Lyric(context.Background(), in); err == nil || calls != 1 {
		t.Fatal("空的自定义歌词应失败且允许稍后重试")
	}
	if lyrics, err := c.Lyric(context.Background(), in); err != nil || lyrics.Lines[0].Value != "备用歌词" || calls != 2 {
		t.Fatal("失败结果阻止了重试", err)
	}
}

func TestCustomLyricsLimitsAndCancellation(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	in := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "歌曲"})
	for _, status := range []int{403, 500, 200} {
		c.customHTTP = &http.Client{Transport: customLyricTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxCustomLyricsBytes+1)))}, nil
		})}
		if _, err := c.Lyric(context.Background(), in); err == nil {
			t.Fatal("错误状态或过大响应应失败", status)
		}
	}
	c.customHTTP = &http.Client{Transport: customLyricTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("请求取消后仍访问了备用接口")
		return nil, errors.New("不应调用")
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Lyric(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatal("取消未传播", err)
	}
}
