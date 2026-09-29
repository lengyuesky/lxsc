package portal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"lxsc/internal/js"
)

func TestSearchStreamAuthenticationValidationAndStates(t *testing.T) {
	f := newPortalFixture(t)
	pool, err := js.NewSDKPool(1, "", `globalThis.__sdk_call=(path,args)=>path.startsWith('tx.') ? Promise.reject(new Error('合成失败，不应暴露内部详情')) : Promise.resolve({list:[{songmid:'one',name:'测试歌曲'}]});`, nil, nil, f.service.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.service.Catalog.SDK = pool
	if status, _ := doJSON(t, http.DefaultClient, "POST", f.server.URL+"/search/stream", map[string]any{"query": "歌曲"}); status != 401 {
		t.Fatal("流式搜索必须鉴权")
	}
	client := f.client(t, "alice")
	for _, body := range []any{map[string]any{"query": " "}, map[string]any{"query": "歌曲", "sources": []string{"bad"}}} {
		if status, _ := doJSON(t, client, "POST", f.server.URL+"/search/stream", body); status != 400 {
			t.Fatalf("错误参数必须在发送流之前返回 400: %d", status)
		}
	}
	for run := 0; run < 2; run++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, "POST", f.server.URL+"/search/stream", bytes.NewBufferString(`{"query":"歌曲","sources":["wy","tx","wy"]}`))
		response, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if response.Header.Get("X-Accel-Buffering") != "no" || response.StatusCode != 200 {
			t.Fatal("必须关闭反向代理缓冲并返回成功流")
		}
		scanner := bufio.NewScanner(response.Body)
		events := make([]map[string]any, 0)
		for scanner.Scan() {
			var event map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		response.Body.Close()
		cancel()
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		if len(events) != 4 || events[0]["type"] != "start" || events[3]["type"] != "done" {
			t.Fatalf("流事件或平台去重错误: %+v", events)
		}
		for _, event := range events[1:3] {
			if event["source"] == "wy" {
				if event["status"] != "ok" || event["cached"] != (run == 1) || len(event["tracks"].([]any)) != 1 {
					t.Fatalf("成功平台结果错误: %+v", event)
				}
			} else if event["status"] != "error" || event["error"] != "平台搜索失败，请稍后重试" {
				t.Fatalf("失败状态必须明确且不泄露内部错误: %+v", event)
			}
		}
	}
	response, err := client.Post(f.server.URL+"/search", "application/json", bytes.NewBufferString(`{"query":"歌曲","sources":["wy"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var tracks []trackView
	if err := json.NewDecoder(response.Body).Decode(&tracks); err != nil || len(tracks) != 1 {
		t.Fatal("原搜索接口必须继续返回数组")
	}
}

func TestSearchStreamSlowReaderReleasesAdmission(t *testing.T) {
	f := newPortalFixture(t)
	pool, err := js.NewSDKPool(1, "", `globalThis.__sdk_call=()=>({list:[{source:'wy',songmid:'one',name:'x'.repeat(8*1024*1024)}]});`, nil, nil, f.service.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.service.Catalog.SDK = pool
	client := f.client(t, "alice")
	u, _ := url.Parse(f.server.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.(*net.TCPConn).SetReadBuffer(1024)
	body := `{"query":"大响应","sources":["wy"]}`
	_, err = fmt.Fprintf(conn, "POST /search/stream HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", u.Host, client.Jar.Cookies(u)[0].String(), len([]byte(body)), body)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(7 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		active := f.service.Catalog.RequestLimits.Stats().Active
		seen = seen || active > 0
		if seen && active == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("不读取响应的客户端必须触发写超时并释放请求名额")
}
