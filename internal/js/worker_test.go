package js

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"lxsc/internal/assets"
)

func loadAsset(t *testing.T, name string) string {
	b, err := assets.JS.ReadFile("js/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestWorker(t *testing.T) *Worker {
	w, err := New(Options{Name: "test", Prelude: loadAsset(t, "prelude.js"), HTTP: &http.Client{Timeout: 60 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	return w
}

func TestPreludeBasics(t *testing.T) {
	w := newTestWorker(t)
	ctx := context.Background()
	out, err := w.Eval(ctx, `[typeof lx.request, typeof Buffer, typeof URL, atob(btoa('hi')), lx.utils.crypto.md5('abc'), Buffer.from('你好').toString('base64'), typeof window]`)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(out))
	var arr []any
	_ = json.Unmarshal(out, &arr)
	if arr[4] != "900150983cd24fb0d6963f7d28e17f72" {
		t.Fatalf("md5 mismatch: %v", arr)
	}
	// AES/RSA 与 lx.utils
	out, err = w.Eval(ctx, `(function(){
		var enc = lx.utils.crypto.aesEncrypt(Buffer.from('hello world'), 'aes-128-cbc', Buffer.from('0CoJUm6Qyw8W8jud'), Buffer.from('0102030405060708'))
		return enc.toString('base64')
	})()`)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"4DSisWgkBZuTDBMRpiz81g=="` {
		t.Fatalf("aes mismatch: %s", out)
	}
	// zlib promise
	out, err = w.Eval(ctx, `lx.utils.zlib.deflate(Buffer.from('aaaaaaaaaa')).then(function(b){ return lx.utils.zlib.inflate(b) }).then(function(b){ return b.toString() })`)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"aaaaaaaaaa"` {
		t.Fatalf("zlib mismatch: %s", out)
	}
}

func TestDummySource(t *testing.T) {
	w := newTestWorker(t)
	inited := make(chan string, 1)
	w.opts.OnInited = func(p string) { inited <- p }
	src, err := os.ReadFile("../../testdata/dummy-source.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.RunScript(context.Background(), "dummy.js", string(src)); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-inited:
		t.Log("inited:", p)
	case <-time.After(3 * time.Second):
		t.Fatal("inited timeout")
	}
	out, err := w.CallJSON(context.Background(), "__lx_request", map[string]any{
		"source": "wy", "action": "musicUrl",
		"info": map[string]any{"type": "128k", "musicInfo": map[string]any{"songmid": "123"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(out))
}

func TestSDKDirectoryMethodsAvailable(t *testing.T) {
	w := newTestWorker(t)
	if err := w.RunScript(context.Background(), "sdk.bundle.js", loadAsset(t, "sdk.bundle.js")); err != nil {
		t.Fatal(err)
	}
	out, err := w.Eval(context.Background(), `[
		typeof __sdk.wy.extendSearch.searchSinger,
		typeof __sdk.wy.extendDetail.getArtistDetail,
		typeof __sdk.wy.extendDetail.getArtistAlbums,
		typeof __sdk.wy.extendDetail.getAlbumSongs,
		typeof __sdk.tx.extendSearch.searchSinger,
		typeof __sdk.tx.extendDetail.getArtistDetail,
		typeof __sdk.tx.extendDetail.getArtistAlbums,
		typeof __sdk.tx.extendDetail.getAlbumSongs,
		typeof __sdk.kg.singer.getInfo,
		typeof __sdk.kg.singer.getAlbumList,
		typeof __sdk.kg.album.getAlbumDetail,
		typeof __sdk.kw.album.getAlbumListDetail,
		typeof __sdk.mg.album.getAlbumDetail
	]`)
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	if err := json.Unmarshal(out, &methods); err != nil {
		t.Fatal(err)
	}
	for index, method := range methods {
		if method != "function" {
			t.Fatalf("目录 SDK 方法 %d 未打包: %v", index, methods)
		}
	}
}

func TestSDKSearchOnline(t *testing.T) {
	if os.Getenv("LXSC_ONLINE_TEST") == "" {
		t.Skip("set LXSC_ONLINE_TEST=1 to run")
	}
	w := newTestWorker(t)
	if err := w.RunScript(context.Background(), "sdk.bundle.js", loadAsset(t, "sdk.bundle.js")); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"wy", "tx", "kw", "kg", "mg"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := w.CallJSON(ctx, "__sdk_call", src+".musicSearch.search", []any{"晴天", 1, 3})
		cancel()
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		s := string(out)
		if len(s) > 400 {
			s = s[:400]
		}
		t.Logf("%s: %s", src, s)
	}
}

func TestSDKMiscOnline(t *testing.T) {
	if os.Getenv("LXSC_ONLINE_TEST") == "" {
		t.Skip("set LXSC_ONLINE_TEST=1 to run")
	}
	w := newTestWorker(t)
	if err := w.RunScript(context.Background(), "sdk.bundle.js", loadAsset(t, "sdk.bundle.js")); err != nil {
		t.Fatal(err)
	}
	call := func(path string, args ...any) string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := w.CallJSON(ctx, "__sdk_call", path, args)
		if err != nil {
			return "ERR: " + err.Error()
		}
		s := string(out)
		if len(s) > 300 {
			s = s[:300]
		}
		return s
	}
	t.Log("wy lyric:", call("wy.getLyric", map[string]any{"songmid": 186016, "source": "wy"}))
	t.Log("tx lyric:", call("tx.getLyric", map[string]any{"songmid": "0039MnYb0qxYhV", "source": "tx"}))
	t.Log("kg lyric:", call("kg.getLyric", map[string]any{"songmid": 20505418, "hash": "B3A52A7A958BF0AED0EBFBA2E9A818B7", "source": "kg", "interval": "04:29", "_interval": 269, "name": "晴天", "singer": "周杰伦"}))
	t.Log("kw lyric:", call("kw.getLyric", map[string]any{"songmid": "228908", "source": "kw"}))
	t.Log("mg lyric:", call("mg.getLyric", map[string]any{"songmid": "3790007", "copyrightId": "60054701923", "source": "mg", "lrcUrl": "https://d.musicapp.migu.cn/data/oss/resource/00/5b/o7/70896c574ad040789a981d164fe24ff9"}))
	t.Log("wy boards:", call("wy.leaderboard.getBoards"))
	t.Log("tx boards:", call("tx.leaderboard.getBoards"))
	t.Log("wy board list:", call("wy.leaderboard.getList", "19723756", 1))
	t.Log("wy songlist tags:", call("wy.songList.getTags"))
	t.Log("wy songlist:", call("wy.songList.getList", "hot", "", 1))
	t.Log("tx pic:", call("tx.getPic", map[string]any{"songmid": "0039MnYb0qxYhV", "albumMid": "000MkMni19ClKG", "source": "tx"}))
	t.Log("wy hot:", call("wy.hotSearch.getList"))
}
