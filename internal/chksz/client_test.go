package chksz

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/httpguard"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string, header ...string) *http.Response {
	h := http.Header{"Content-Type": {"application/json"}}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func testClient(fn roundTrip) *Client {
	return New(&http.Client{Transport: fn})
}

// 每个平台按文档拼接口与参数，音质不高于请求档位，响应的实际档位映射回内部档位。
func TestResolveBuildsPlatformRequestsAndParsesQuality(t *testing.T) {
	for _, tc := range []struct {
		platform, id, quality string
		path, param, value    string
		body                  string
		want                  Result
	}{
		{"wy", "1315196858", "flac", "/api/163_music", "level", "lossless",
			`{"code":200,"msg":"success","data":{"id":1315196858,"url":"https://m801.music.126.net/a.flac","level":"exhigh"}}`,
			Result{"https://m801.music.126.net/a.flac", "320k"}},
		{"wy", "1", "192k", "/api/163_music", "level", "standard",
			`{"code":200,"data":{"url":"https://m801.music.126.net/a.mp3"}}`,
			Result{"https://m801.music.126.net/a.mp3", "128k"}},
		{"tx", "0039MnYb0qxYhV", "flac24bit", "/api/qq_music", "size", "hires",
			`{"code":200,"msg":"success","url":"https://qq.example/song.flac","bitrate":"hires"}`,
			Result{"https://qq.example/song.flac", "flac24bit"}},
		{"kg", "48C685F679FFC7CF08B8A8341CA9DB44", "320k", "/api/kugou_music", "size", "320k",
			`{"code":"200","url":"https://kg.example/song.mp3","bitrate":"320k"}`,
			Result{"https://kg.example/song.mp3", "320k"}},
		{"tx", "mid", "master", "/api/qq_music", "size", "master",
			`{"code":200,"url":"http://qq.example/m.flac"}`,
			Result{"http://qq.example/m.flac", "master"}},
	} {
		t.Run(tc.platform+"/"+tc.quality, func(t *testing.T) {
			var seen *url.URL
			c := testClient(func(r *http.Request) (*http.Response, error) {
				seen = r.URL
				return reply(200, tc.body), nil
			})
			got, err := c.Resolve(context.Background(), "secret-key", tc.platform, tc.id, tc.quality)
			if err != nil || got != tc.want {
				t.Fatalf("结果不符：%+v %v", got, err)
			}
			q := seen.Query()
			idParam := "id"
			if tc.platform == "tx" {
				idParam = "mid"
			}
			if seen.Host != "api.chksz.com" || seen.Path != tc.path || q.Get(idParam) != tc.id || q.Get(tc.param) != tc.value || q.Get("apikey") != "secret-key" || q.Get("type") != "json" {
				t.Fatalf("请求不符：%s", seen)
			}
		})
	}
}

func TestResolveRejectsUnusableResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"缺少地址", 200, `{"code":200,"data":{"url":null}}`, ErrNoURL},
		{"业务码失败", 200, `{"code":404,"msg":"无版权"}`, ErrNoURL},
		{"服务故障", 502, `{"code":502}`, ErrUnavailable},
		{"不是 JSON", 200, `<html>`, ErrInvalid},
		{"非 HTTP 地址", 200, `{"code":200,"url":"file:///etc/passwd"}`, ErrInvalid},
		{"地址携带密钥", 200, `{"code":200,"url":"https://proxy.example/play?apikey=secret-key"}`, ErrKeyInURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(func(*http.Request) (*http.Response, error) { return reply(tc.status, tc.body), nil })
			_, err := c.Resolve(context.Background(), "secret-key", "tx", "mid", "320k")
			if !errors.Is(err, tc.want) {
				t.Fatalf("应返回 %v，实际 %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "secret-key") {
				t.Fatal("错误信息不能包含密钥")
			}
		})
	}
	short := testClient(func(*http.Request) (*http.Response, error) {
		return reply(200, `{"code":200,"url":"https://kg.example/k/song.mp3?sign=k"}`), nil
	})
	if _, err := short.Resolve(context.Background(), "k", "kg", "1", "320k"); !errors.Is(err, ErrKeyInURL) {
		t.Fatal("查询参数携带短密钥也应拒绝", err)
	}
	if got, err := short.Resolve(context.Background(), "g", "kg", "1", "320k"); err != nil || got.URL == "" {
		t.Fatal("短密钥只是地址的子串时不应误判", err)
	}
	c := testClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("不支持的平台不应发起请求")
		return nil, nil
	})
	for _, platform := range []string{"kw", "mg", ""} {
		if _, err := c.Resolve(context.Background(), "k", platform, "1", "320k"); !errors.Is(err, ErrUnsupported) {
			t.Fatal(platform, err)
		}
	}
	if _, err := c.Resolve(context.Background(), " ", "wy", "1", "320k"); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
}

// 密钥失效或限流后暂停调用，免得每次播放都消耗额度；更换密钥立即恢复。
func TestAuthAndRateLimitPauseCalls(t *testing.T) {
	now := time.Now()
	var calls atomic.Int32
	status, retry := 401, ""
	c := testClient(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return reply(status, `{"code":401,"msg":"apikey 无效或已失效"}`, "Retry-After", retry), nil
	})
	c.now = func() time.Time { return now }
	if _, err := c.Resolve(context.Background(), "old", "wy", "1", "320k"); !errors.Is(err, ErrAuth) || httpguard.ErrorCode(err) != "source_auth" {
		t.Fatal(err)
	}
	if _, err := c.Resolve(context.Background(), "old", "wy", "2", "320k"); !errors.Is(err, ErrAuth) || calls.Load() != 1 || !c.Blocked("old") {
		t.Fatal("密钥失效后应暂停调用", calls.Load(), err)
	}
	status = 429
	retry = "30"
	if _, err := c.Resolve(context.Background(), "new", "wy", "1", "320k"); !errors.Is(err, ErrRateLimited) || calls.Load() != 2 || httpguard.ErrorCode(err) != "rate_limited" {
		t.Fatal("更换密钥应立即恢复调用", calls.Load(), err)
	}
	now = now.Add(29 * time.Second)
	if !c.Blocked("new") {
		t.Fatal("应遵守 Retry-After")
	}
	now = now.Add(time.Second)
	if c.Blocked("new") {
		t.Fatal("Retry-After 到期后应恢复")
	}
}

// 网络错误不能带出含密钥的请求地址。
func TestTransportErrorsDoNotExposeKey(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })
	_, err := c.Resolve(context.Background(), "secret-key", "wy", "1", "320k")
	if err == nil || strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "apikey") {
		t.Fatalf("错误信息泄露了请求地址：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Resolve(ctx, "secret-key", "wy", "1", "320k"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
