package subsonic

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"lxsc/internal/admission"
	"lxsc/internal/assets"
	"lxsc/internal/js"
	"lxsc/internal/music"
)

func searchRequest(f directoryTestServer, method, endpoint string, values url.Values) *httptest.ResponseRecorder {
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, "/rest/"+endpoint+".view", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, "/rest/"+endpoint+".view?"+values.Encode(), nil)
	}
	req = req.WithContext(withUser(req.Context(), f.user))
	w := httptest.NewRecorder()
	f.server.search(w, req)
	return w
}

func jsonSearchResult(t *testing.T, w *httptest.ResponseRecorder, endpoint string) map[string]any {
	t.Helper()
	var body map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err, w.Body)
	}
	root := body["subsonic-response"]
	if root["status"] != "ok" {
		t.Fatal("搜索失败", w.Body)
	}
	key := strings.Replace(endpoint, "search", "searchResult", 1)
	return root[key].(map[string]any)
}

func searchOnly(kind, query, format string) url.Values {
	values := url.Values{"query": {query}, "f": {format}, "c": {"amcfy"}, "songCount": {"0"}, "artistCount": {"0"}, "albumCount": {"0"}, "songOffset": {"9000"}}
	values.Set(kind+"Count", "20")
	return values
}

func TestSearchArtistsAndAlbumsWithoutSongResults(t *testing.T) {
	f := newDirectoryTestServer(t)
	var mu sync.Mutex
	paths := []string{}
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		switch path {
		case "wy.extendSearch.searchSinger":
			if args[0] != "周杰伦" || args[1] != 1 {
				t.Errorf("艺术家搜索参数错误: %v", args)
			}
			return json.RawMessage(`{"list":[{"id":6452,"name":"周杰伦","picUrl":"https://img.example/artist.jpg","albumSize":15}]}`), nil
		case "wy.extendSearch.searchAlbum":
			if args[0] != "范特西" || args[1] != 1 {
				t.Errorf("专辑搜索参数错误: %v", args)
			}
			return json.RawMessage(`{"list":[{"id":18903,"name":"范特西","artistName":"周杰伦","artistId":6452,"size":10,"picUrl":"https://img.example/album.jpg"}]}`), nil
		default:
			return nil, fmt.Errorf("搜索列表不能加载歌曲或详情: %s", path)
		}
	})
	before, err := f.database.Statistics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"search", "search2", "search3"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, kind := range []string{"artist", "album"} {
				query := "wy:周杰伦"
				if kind == "album" {
					query = "wy:范特西"
				}
				for _, format := range []string{"json", "xml"} {
					t.Run(endpoint+"/"+method+"/"+kind+"/"+format, func(t *testing.T) {
						w := searchRequest(f, method, endpoint, searchOnly(kind, query, format))
						if w.Code != 200 {
							t.Fatal(w.Code, w.Body)
						}
						if format == "xml" {
							var body struct {
								Status string `xml:"status,attr"`
							}
							if err := xml.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Status != "ok" || !strings.Contains(w.Body.String(), "<"+kind+" ") || strings.Contains(w.Body.String(), "<song ") {
								t.Fatal("XML 必须包含所请求的实体，且不依赖歌曲", err, w.Body)
							}
							return
						}
						result := jsonSearchResult(t, w, endpoint)
						if len(result["song"].([]any)) != 0 || len(result[kind].([]any)) != 1 {
							t.Fatal("歌曲关闭后艺术家和专辑仍需返回结果", w.Body)
						}
						item := result[kind].([]any)[0].(map[string]any)
						if kind == "artist" {
							locator, ok := music.ParseSingerDirectoryID(item["id"].(string))
							if !ok || locator.ID != "6452" || locator.Name != "周杰伦" || item["albumCount"] != float64(15) {
								t.Fatal("艺术家定位或数量错误", item)
							}
						} else {
							locator, ok := music.ParseOnlineAlbumID(item["id"].(string))
							if !ok || locator.ID != "18903" || locator.Name != "范特西" || item["songCount"] != float64(10) || item["artist"] != "周杰伦" {
								t.Fatal("专辑字段映射错误", item)
							}
						}
					})
				}
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatal("分类搜索应独立缓存，不反复请求歌曲或目录", paths)
	}
	after, err := f.database.Statistics(context.Background())
	if err != nil || before.Tracks != after.Tracks || before.Albums != after.Albums || before.Artists != after.Artists {
		t.Fatal("搜索摘要不应写入资料库", before, after, err)
	}
}

func TestSearchTypesUseIndependentOffsetsAndZeroCounts(t *testing.T) {
	f := newDirectoryTestServer(t)
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := js.NewSDKPool(1, string(prelude), `globalThis.__sdk_call=(path,args)=>({list:Array.from({length:args[2]},(_,i)=>({source:'wy',songmid:'song-'+((args[1]-1)*args[2]+i),name:'歌曲'+i,singer:'歌手',albumName:'专辑'}))})`, &http.Client{}, &http.Client{}, f.server.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.server.Catalog.SDK = pool
	var mu sync.Mutex
	paths := []string{}
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		page, limit := args[1].(int), args[2].(int)
		items := make([]M, 0, limit)
		for i := range limit {
			index := (page-1)*limit + i
			items = append(items, M{"id": fmt.Sprintf("id-%d", index), "name": fmt.Sprintf("名称%d", index), "artistName": "歌手", "artistId": "artist", "albumSize": 1, "size": 10})
		}
		return json.Marshal(M{"list": items})
	})
	values := url.Values{"query": {"wy:名称"}, "f": {"json"}, "songCount": {"1"}, "songOffset": {"1"}, "artistCount": {"1"}, "artistOffset": {"2"}, "albumCount": {"1"}, "albumOffset": {"3"}}
	result := jsonSearchResult(t, searchRequest(f, "GET", "search3", values), "search3")
	if result["song"].([]any)[0].(map[string]any)["id"] != "tr-wy-song-1" || result["artist"].([]any)[0].(map[string]any)["name"] != "名称2" || result["album"].([]any)[0].(map[string]any)["name"] != "名称3" {
		t.Fatal("三类偏移相互干扰", result)
	}
	for _, kind := range []string{"artist", "album"} {
		values = searchOnly(kind, "wy:名称", "json")
		values.Set(kind+"Count", "2")
		values.Set(kind+"Offset", "19")
		result = jsonSearchResult(t, searchRequest(f, "GET", "search3", values), "search3")
		list := result[kind].([]any)
		if len(list) != 2 || list[0].(map[string]any)["name"] != "名称19" || list[1].(map[string]any)["name"] != "名称20" {
			t.Fatal("跨上游页的艺术家/专辑分页错误", kind, list)
		}
	}
	mu.Lock()
	before := len(paths)
	mu.Unlock()
	values = url.Values{"query": {"wy:不应请求"}, "f": {"json"}, "songCount": {"0"}, "artistCount": {"0"}, "albumCount": {"0"}}
	result = jsonSearchResult(t, searchRequest(f, "GET", "search3", values), "search3")
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != before || len(result["song"].([]any))+len(result["artist"].([]any))+len(result["album"].([]any)) != 0 {
		t.Fatal("零数量不能继续执行分类搜索")
	}
}

func TestSearchResultArtistAlbumAndCoversAreNavigable(t *testing.T) {
	f := newDirectoryTestServer(t)
	var mu sync.Mutex
	paths := []string{}
	caller := func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		switch path {
		case "tx.extendSearch.searchSinger":
			return json.RawMessage(`{"list":[{"id":"artist-mid","name":"测试歌手","picUrl":"https://img.example/artist.jpg","albumSize":1}]}`), nil
		case "tx.extendDetail.getArtistAlbums":
			if args[0] != "artist-mid" {
				t.Errorf("歌手 MID 丢失: %v", args)
			}
			return json.RawMessage(`{"list":[{"id":77,"mid":"album-mid","name":"测试专辑","picUrl":"https://img.example/album.jpg","count":2}],"total":1}`), nil
		case "tx.extendDetail.getAlbumSongs":
			if args[0] != "album-mid" {
				t.Errorf("专辑 MID 丢失: %v", args)
			}
			return json.RawMessage(`{"name":"测试专辑","list":[{"songmid":"one","name":"第一首","singer":"测试歌手","albumName":"测试专辑","albumId":"album-mid"},{"songmid":"two","name":"第二首","singer":"测试歌手","albumName":"测试专辑","albumId":"album-mid"}]}`), nil
		default:
			return nil, fmt.Errorf("意外的目录预加载: %s", path)
		}
	}
	f.server.Catalog.SetRemoteCallerForTest(caller)
	result := jsonSearchResult(t, searchRequest(f, "GET", "search3", searchOnly("artist", "tx:测试歌手", "json")), "search3")
	artist := result["artist"].([]any)[0].(map[string]any)
	artistID := artist["id"].(string)
	call := func(handler handlerFunc, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/rest/test.view?f=json&id="+url.QueryEscape(id), nil)
		req = req.WithContext(withUser(req.Context(), f.user))
		w := httptest.NewRecorder()
		handler(w, req)
		return w
	}
	if w := call(f.server.getCoverArt, artistID); w.Code != 302 || w.Header().Get("Location") != "https://img.example/artist.jpg" {
		t.Fatal("艺术家头像不应依赖歌曲结果", w.Code, w.Body)
	}
	if len(paths) != 1 {
		t.Fatal("搜索或封面触发了歌手专辑扫描", paths)
	}
	// 新建 Catalog 模拟缓存过期或重启；搜索结果 ID 自身必须能定位平台歌手。
	f.server.Catalog = music.NewCatalog(f.database, nil, nil, f.store, f.server.Log)
	f.server.Catalog.SetRemoteCallerForTest(caller)
	w := call(f.server.getArtist, artistID)
	var response map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	artist = response["subsonic-response"]["artist"].(map[string]any)
	albums := artist["album"].([]any)
	if artist["id"] != artistID || len(albums) != 1 || len(paths) != 2 {
		t.Fatal("艺术家详情为空或提前加载了专辑歌曲", w.Body, paths)
	}
	albumID := albums[0].(map[string]any)["id"].(string)
	if w := call(f.server.getCoverArt, albumID); w.Code != 302 || w.Header().Get("Location") != "https://img.example/album.jpg" || len(paths) != 2 {
		t.Fatal("专辑封面触发了歌曲加载", w.Code, paths)
	}
	w = call(f.server.getAlbum, albumID)
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	album := response["subsonic-response"]["album"].(map[string]any)
	if album["id"] != albumID || album["artistId"] != artistID || len(album["song"].([]any)) != 2 || len(paths) != 3 {
		t.Fatal("从搜索结果打开专辑未返回正确歌曲和关联身份", w.Body, paths)
	}
	for _, item := range album["song"].([]any) {
		if item.(map[string]any)["albumId"] != albumID {
			t.Fatal("专辑歌曲的父级身份不一致")
		}
	}
}

func TestMetadataSearchFailuresAreNotSuccessfulEmptyPages(t *testing.T) {
	for _, kind := range []string{"artist", "album"} {
		for _, busy := range []bool{false, true} {
			f := newDirectoryTestServer(t)
			f.server.Catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
				if busy {
					return nil, admission.ErrBusy
				}
				return nil, fmt.Errorf("合成平台故障")
			})
			w := searchRequest(f, "GET", "search3", searchOnly(kind, "wy:测试", "json"))
			if !strings.Contains(w.Body.String(), `"status":"failed"`) || busy && w.Code != 503 {
				t.Fatal("平台故障不能伪装成成功空结果", kind, busy, w.Code, w.Body)
			}
		}
	}
}

func TestMetadataPaginationUsesActualResultsWhenAnotherPlatformFails(t *testing.T) {
	f := newDirectoryTestServer(t)
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"searchSources": json.RawMessage(`["wy","tx"]`)}); err != nil {
		t.Fatal(err)
	}
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		if strings.HasPrefix(path, "tx.") {
			return nil, fmt.Errorf("平台不可用")
		}
		page, limit := args[1].(int), args[2].(int)
		items := make([]M, 0, limit)
		for i := range limit {
			index := (page-1)*limit + i
			items = append(items, M{"id": fmt.Sprint(index + 1), "name": fmt.Sprintf("实体%d", index), "artistName": "歌手", "size": 2})
		}
		return json.Marshal(M{"list": items})
	})
	for _, kind := range []string{"artist", "album"} {
		values := searchOnly(kind, "实体", "json")
		values.Set(kind+"Count", "2")
		values.Set(kind+"Offset", "20")
		result := jsonSearchResult(t, searchRequest(f, "GET", "search3", values), "search3")
		list := result[kind].([]any)
		if len(list) != 2 || list[0].(map[string]any)["name"] != "实体20" || list[1].(map[string]any)["name"] != "实体21" {
			t.Fatal("空缺平台不能让下一页变空或错过有效结果", kind, list)
		}
	}
}
