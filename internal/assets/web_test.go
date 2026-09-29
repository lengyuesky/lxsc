package assets

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestWebCacheVersionsCompressionAndRevalidation(t *testing.T) {
	handler, err := WebHandler()
	if err != nil {
		t.Fatal(err)
	}
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest("GET", "/", nil))
	match := regexp.MustCompile(`src="(api.js\?v=[0-9a-f]{64})"`).FindStringSubmatch(index.Body.String())
	if len(match) != 2 || index.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("入口应自动引用内容版本并重新验证")
	}
	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest("GET", "/"+match[1], nil))
	if !strings.Contains(plain.Header().Get("Cache-Control"), "immutable") {
		t.Fatal("版本化资源应该长期缓存")
	}
	r := httptest.NewRequest("GET", "/"+match[1], nil)
	r.Header.Set("If-None-Match", plain.Header().Get("ETag"))
	revalidated := httptest.NewRecorder()
	handler.ServeHTTP(revalidated, r)
	if revalidated.Code != 304 || revalidated.Body.Len() != 0 {
		t.Fatal("条件请求应返回304")
	}
	r = httptest.NewRequest("GET", "/"+match[1], nil)
	r.Header.Set("Accept-Encoding", "gzip")
	compressed := httptest.NewRecorder()
	handler.ServeHTTP(compressed, r)
	reader, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(data, plain.Body.Bytes()) || compressed.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Fatal("压缩资源或ETag错误", err)
	}
	stale := httptest.NewRecorder()
	handler.ServeHTTP(stale, httptest.NewRequest("GET", "/api.js?v=old", nil))
	if stale.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("旧版本URL不能缓存新内容一年")
	}
}

func TestEncodingNegotiation(t *testing.T) {
	for _, value := range []string{"", "br", "gzip;q=0", "*;q=0", "gzip;q=bad", "gzip;q=0, *;q=1"} {
		if acceptsGzip(value) {
			t.Error("不应发送gzip", value)
		}
	}
	for _, value := range []string{"gzip", "br, gzip;q=0.5", "*"} {
		if !acceptsGzip(value) {
			t.Error("应发送gzip", value)
		}
	}
}

func TestWebScriptPolicyAndIntegrity(t *testing.T) {
	handler, err := WebHandler()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "'strict-dynamic'") || !strings.Contains(policy, "script-src-attr 'none'") || strings.Contains(policy, "unsafe-eval") {
		t.Fatal("脚本策略未生效", policy)
	}
	scripts := regexp.MustCompile(`<script src="([^"?]+)\?v=[a-f0-9]+" integrity="(sha256-[^"]+)"`).FindAllStringSubmatch(response.Body.String(), -1)
	if len(scripts) < 15 {
		t.Fatalf("脚本缺少内容完整性校验：%d", len(scripts))
	}
	for _, script := range scripts {
		source, err := Web.ReadFile("web/" + script[1])
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(source)
		hash := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
		if hash != script[2] || !strings.Contains(policy, "'"+hash+"'") {
			t.Fatal("脚本与策略哈希不一致", script[1])
		}
	}
	html, _ := Web.ReadFile("web/index.html")
	if regexp.MustCompile(`\son[a-z]+\s*=`).Match(html) {
		t.Fatal("页面仍包含内联事件")
	}
}
