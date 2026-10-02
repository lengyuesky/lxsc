package subsonic

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lxsc/internal/js"
	"lxsc/internal/music"
)

func lyricTestServer(t *testing.T, script string) directoryTestServer {
	t.Helper()
	f := newDirectoryTestServer(t)
	pool, err := js.NewSDKPool(1, "", script, &http.Client{}, &http.Client{}, f.server.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.server.Catalog.SDK = pool
	f.server.Catalog.Cache([]*music.Info{music.FromMap(map[string]any{
		"source": "wy", "songmid": "lyric", "name": "测试歌曲", "singer": "测试歌手",
	})})
	return f
}

const lyricTestSDK = `globalThis.__sdk_call = (path, args) => {
  if (path !== 'wy.getLyric' || args[0].songmid !== 'lyric') throw new Error('不应重新搜索或切换歌曲版本');
  return {lyric:'[offset:500]\n[00:01.50]第一行\n[00:03.20]第二行',tlyric:'[00:01.50]First line'};
}`

func TestTraditionalLyricsUsesKnownTrackWithoutSearch(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, format := range []string{"json", "xml"} {
			t.Run(method+"/"+format, func(t *testing.T) {
				f := lyricTestServer(t, lyricTestSDK)
				params := url.Values{"f": {format}, "artist": {"测试歌手"}, "title": {"测试歌曲"}, "c": {"Amcfy Music"}}
				target := "/rest/getLyrics"
				body := ""
				if method == http.MethodGet {
					target += "?" + params.Encode()
				} else {
					body = params.Encode()
				}
				req := httptest.NewRequest(method, target, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req = req.WithContext(withUser(req.Context(), f.user))
				rec := httptest.NewRecorder()
				f.server.getLyrics(rec, req)
				var value string
				if format == "json" {
					var result struct {
						Response struct {
							Lyrics struct{ Value string } `json:"lyrics"`
						} `json:"subsonic-response"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					value = result.Response.Lyrics.Value
				} else {
					var result struct {
						Lyrics string `xml:"lyrics"`
					}
					if err := xml.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					value = result.Lyrics
				}
				for _, want := range []string{"[offset:500]", "[00:01.50]第一行", "[00:01.50]First line", "[00:03.20]第二行"} {
					if !strings.Contains(value, want) {
						t.Fatalf("歌词缺少 %q: %s", want, rec.Body.String())
					}
				}
			})
		}
	}
}

func TestStructuredLyricsAndTransientEmptyRetry(t *testing.T) {
	f := lyricTestServer(t, `let calls=0; globalThis.__sdk_call = () => ++calls === 1
  ? {lyric:'[ti:只有元信息]\n[00:00.00]'}
  : {lxlyric:'[00:01.50]<0,100>第一行',tlyric:'[00:01.50]First line'};`)
	request := func() []any {
		t.Helper()
		root := f.boardRequest(t, f.server.getLyricsBySongID, "/rest/getLyricsBySongId?f=json&id=tr-wy-lyric&c=Amcfy")
		return root["lyricsList"].(map[string]any)["structuredLyrics"].([]any)
	}
	if lyrics := request(); len(lyrics) != 0 {
		t.Fatalf("无正文不能作为成功歌词缓存: %+v", lyrics)
	}
	lyrics := request()
	if len(lyrics) != 2 {
		t.Fatalf("空结果后应重试，兼容仅逐字歌词并保留翻译: %+v", lyrics)
	}
	primary := lyrics[0].(map[string]any)
	line := primary["line"].([]any)[0].(map[string]any)
	if primary["synced"] != true || line["start"] != float64(1500) || line["value"] != "第一行" {
		t.Fatalf("同步歌词格式错误: %+v", primary)
	}
}

func TestTraditionalLyricsFindsPersistedTrackAfterRestart(t *testing.T) {
	f := lyricTestServer(t, lyricTestSDK)
	in, err := f.server.Catalog.LocalTrack(context.Background(), "tr-wy-lyric")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.Catalog.RememberSync(context.Background(), []*music.Info{in}); err != nil {
		t.Fatal(err)
	}
	f.server.Catalog.PurgeMetadataCaches()
	root := f.boardRequest(t, f.server.getLyrics, "/rest/getLyrics?f=json&"+url.Values{"artist": {"测试歌手"}, "title": {"测试歌曲"}}.Encode())
	if !strings.Contains(root["lyrics"].(map[string]any)["value"].(string), "第一行") {
		t.Fatalf("重启后的已保存歌曲不应依赖在线搜索: %+v", root)
	}
}
