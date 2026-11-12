package subsonic

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/diagnostics"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/testaudio"
)

// exhaustiveScript 在 wy 与 kg 两个平台各支持三档，地址形如 https://media.invalid/<label>/<平台>/<音质>。
func exhaustiveScript(label string) string {
	return fmt.Sprintf(`lx.on(lx.EVENT_NAMES.request,({source,info})=>Promise.resolve('https://media.invalid/%s/'+source+'/'+info.type));
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k','128k']},kg:{type:'music',actions:['musicUrl'],qualitys:['flac','320k','128k']}}});`, label)
}

// withAlternatePlatform 让其它平台的搜索在 kg 上找到同一首歌。
func withAlternatePlatform(t *testing.T, s *Server) {
	t.Helper()
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	sdk := `globalThis.__sdk_call=(path,args)=>Promise.resolve({list:path.startsWith('kg.') ? [{source:'kg',songmid:'alternate',name:'临时歌曲',singer:'测试歌手',types:[{type:'128k'},{type:'320k'},{type:'flac'}]}] : []});`
	pool, err := js.NewSDKPool(1, string(prelude), sdk, &http.Client{}, &http.Client{}, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s.Catalog.SDK = pool
}

type recordedPaths struct {
	mu    sync.Mutex
	paths []string
}

func (r *recordedPaths) add(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
}

func (r *recordedPaths) sorted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.paths)
	slices.Sort(out)
	return out
}

// 向客户端报告失败之前，每个音源、每档音质、每个有这首歌的平台都必须真正试过。
func TestPlaybackFailureMeansEverySourceQualityAndPlatformFailed(t *testing.T) {
	for _, strategy := range []string{"quality", "speed"} {
		t.Run(strategy, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
			addMediaSource(t, s, 2, exhaustiveScript("b"))
			withAlternatePlatform(t, s)
			setSourceSelection(t, s, strategy)
			var seen recordedPaths
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				seen.add(r.URL.Path)
				return &http.Response{StatusCode: 500, Header: make(http.Header), Body: http.NoBody}, nil
			})}
			prepare := func(quality string) error {
				_, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality="+quality), info, quality, false, false)
				if response != nil {
					response.Body.Close()
				}
				return err
			}
			if err := prepare("flac"); err == nil {
				t.Fatal("全部候选失败必须报告失败")
			}
			var want []string
			for _, source := range []string{"a", "b"} {
				for _, platform := range []string{"kg", "wy"} {
					for _, quality := range []string{"128k", "320k", "flac"} {
						want = append(want, "/"+source+"/"+platform+"/"+quality)
					}
				}
			}
			if got := seen.sorted(); !slices.Equal(got, want) {
				t.Fatalf("报告失败前必须试遍所有音源、音质与平台，且每个候选只试一次：\n got=%v\nwant=%v", got, want)
			}
			// 刚确认过的失败直接复用：客户端的重试、HEAD 和降档请求不再重复打扰上游。
			for _, quality := range []string{"flac", "320k", "128k"} {
				started := time.Now()
				if err := prepare(quality); err == nil || time.Since(started) > time.Second {
					t.Fatal("已确认的失败应立即返回", quality, err)
				}
			}
			if got := seen.sorted(); len(got) != len(want) {
				t.Fatalf("确认失败后短时间内不应重复请求上游：%v", got)
			}
			// 音源变更后旧结论作废。
			s.Catalog.InvalidateURLs()
			if err := prepare("128k"); err == nil {
				t.Fatal("仍应失败")
			}
			if got := seen.sorted(); len(got) != len(want)+4 {
				t.Fatalf("结论作废后应重新确认 128k 的四个候选：%d", len(got)-len(want))
			}
		})
	}
}

// HEAD 与 GET 使用同一套选源：HEAD 不会因为省略换平台而误报失败，随后的 GET 直接复用结论。
func TestHeadAndGetShareOneSelectionAcrossPlatforms(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	withAlternatePlatform(t, s)
	var seen recordedPaths
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen.add(r.URL.Path)
		status := 500
		if r.URL.Path == "/a/kg/128k" {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	for index, head := range []bool{true, false} {
		resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, head)
		if response != nil {
			response.Body.Close()
		}
		if err != nil || !resolution.Validated || resolution.Platform != "kg" || resolution.TrackID != "tr-kg-alternate" || resolution.Result.Quality != "128k" {
			t.Fatalf("第 %d 次请求应由 kg 的 128k 胜出：%+v %v", index, resolution, err)
		}
	}
	want := []string{"/a/kg/128k", "/a/kg/320k", "/a/wy/128k", "/a/wy/320k"}
	if got := seen.sorted(); !slices.Equal(got, want) {
		t.Fatalf("HEAD 完成选源后 GET 不应再请求上游：got=%v want=%v", got, want)
	}
}

// 能解码但明显不是整首歌的音频（试听片段、提示音）不算可播放，继续尝试其它音源。
func TestPreviewClipIsNotPlayableAndNextSourceWins(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint(proxy), func(t *testing.T) {
			s, user, _ := newMediaStabilityServer(t, mediaFailoverScript("a"))
			s.Catalog.AudioCheck = nil
			s.Diagnostics = &diagnostics.Events{}
			addMediaFallback(t, s, 22, "b")
			// 逐个尝试，确定性地先遇到 A 的片段。
			setSourceConcurrency(t, s, 1)
			info := music.FromMap(map[string]any{"source": "wy", "songmid": "full", "name": "完整歌曲", "interval": "00:45", "types": []any{map[string]any{"type": "320k"}}})
			s.Catalog.Cache([]*music.Info{info})
			clip, full := testaudio.WAV(), testaudio.Tone(45)
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := clip
				if r.URL.Path == "/b/1" {
					body = full
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/wav"}, "Content-Length": {strconv.Itoa(len(body))}}, Body: &countedMediaBody{Reader: bytes.NewReader(body), reads: &atomic.Int32{}, closed: &atomic.Int32{}}, Request: r}, nil
			})}
			request := mediaStabilityRequest(user, info, "")
			request.Header.Del("Range")
			request.Header.Del("If-Range")
			resolution, response, err := s.prepareMedia(context.Background(), request, info, "320k", proxy, false)
			if response != nil {
				response.Body.Close()
			}
			if err != nil || !resolution.Validated || resolution.Result.SourceID != 22 {
				t.Fatalf("一秒钟的片段不能冒充 45 秒的歌曲：%+v %v", resolution.Result, err)
			}
			rejected := false
			for _, event := range s.Diagnostics.List() {
				rejected = rejected || event.Error == "incomplete_audio"
			}
			if !rejected {
				t.Fatal("诊断必须指出片段不完整")
			}
		})
	}
}

// 音源明确表示整体不可用（认证失效、限流、接口故障）时不再逐档重复同一个错误。
func TestSourceWideFailureSkipsLowerQualities(t *testing.T) {
	for _, code := range []string{"EAUTH", "ERATELIMIT", "EUPSTREAM"} {
		t.Run(code, func(t *testing.T) {
			script := fmt.Sprintf(`lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k','128k']}}});globalThis.__lx_request=()=>Promise.reject(Object.assign(new Error('不得导出的错误'),{code:%q,retryAfter:60}));`, code)
			s, user, info := newMediaStabilityServer(t, script)
			addMediaSource(t, s, 2, mediaQualityScript("b", "320k"))
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
			})}
			resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, false)
			if response != nil {
				response.Body.Close()
			}
			if err != nil || resolution.Result.SourceID != 2 || resolution.Result.Quality != "320k" {
				t.Fatalf("应由另一个音源胜出：%+v %v", resolution.Result, err)
			}
			if attempts := s.Catalog.Sources.CallStatisticsOf(1, time.Now(), time.Hour).Total.Attempts; attempts != 1 {
				t.Fatalf("整体不可用的音源只应调用一次：%d", attempts)
			}
		})
	}
}
