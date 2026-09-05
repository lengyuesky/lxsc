package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"

	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/settings"
)

// Catalog 汇聚 SDK、音源脚本、数据库与缓存，是 Subsonic 层的唯一数据入口
type Catalog struct {
	DB       *db.DB
	SDK      *js.SDKPool
	Sources  *js.SourceManager
	Settings *settings.Store
	Log      *slog.Logger

	tracks          *lru.LRU[string, *Info]
	albums          *lru.LRU[string, []*Info]
	artists         *lru.LRU[string, string]
	artistRefs      *lru.LRU[string, ArtistRef]
	seenArtists     *lru.LRU[string, string]
	searchedArtists *lru.LRU[string, string]
	artistDetails   *lru.LRU[string, ArtistDetail]
	albumPages      *lru.LRU[string, AlbumPage]
	albumMetadata   *lru.LRU[string, AlbumMeta]
	boardNames      *lru.LRU[string, string]
	failures        *lru.LRU[string, string]
	flight          requestGroup
	observeMu       sync.RWMutex
	observer        func(RemoteRequest)
	remoteCall      func(context.Context, string, ...any) (json.RawMessage, error)
	configMu        sync.Mutex
	urls            *requestCache[urlKey, js.MusicURLResult]
	search          *requestCache[searchKey, []*Info]
	searchCall      func(context.Context, string, ...any) (json.RawMessage, error)
	urlCall         func(context.Context, string, any, string) (*js.MusicURLResult, error)
	lyrics          *lru.LRU[string, *Lyrics]
	generic         *lru.LRU[string, json.RawMessage]
}

// NewCatalog 创建
func NewCatalog(d *db.DB, sdk *js.SDKPool, src *js.SourceManager, st *settings.Store, log *slog.Logger) *Catalog {
	v := st.Get()
	c := &Catalog{
		DB: d, SDK: sdk, Sources: src, Settings: st, Log: log,
		tracks:          lru.NewLRU[string, *Info](5000, nil, time.Hour),
		albums:          lru.NewLRU[string, []*Info](2000, nil, time.Hour),
		artists:         lru.NewLRU[string, string](2000, nil, time.Hour),
		artistRefs:      lru.NewLRU[string, ArtistRef](4000, nil, time.Hour),
		seenArtists:     lru.NewLRU[string, string](2000, nil, time.Hour),
		searchedArtists: lru.NewLRU[string, string](2000, nil, time.Hour),
		artistDetails:   lru.NewLRU[string, ArtistDetail](2000, nil, time.Hour),
		albumPages:      lru.NewLRU[string, AlbumPage](2000, nil, time.Hour),
		albumMetadata:   lru.NewLRU[string, AlbumMeta](4000, nil, time.Hour),
		boardNames:      lru.NewLRU[string, string](2000, nil, 30*time.Minute),
		failures:        lru.NewLRU[string, string](500, nil, 30*time.Second),
		urls:            newRequestCache[urlKey, js.MusicURLResult](2000, time.Duration(v.URLCacheTTL)*time.Second),
		search:          newRequestCache[searchKey, []*Info](500, time.Duration(v.SearchCacheTTL)*time.Second),
		lyrics:          lru.NewLRU[string, *Lyrics](2000, nil, 6*time.Hour),
		generic:         lru.NewLRU[string, json.RawMessage](500, nil, 30*time.Minute),
	}
	c.searchCall = func(ctx context.Context, path string, args ...any) (json.RawMessage, error) {
		if c.SDK == nil {
			return nil, errors.New("sdk 未初始化")
		}
		return c.SDK.CallRaw(ctx, path, args...)
	}
	c.urlCall = func(ctx context.Context, platform string, info any, quality string) (*js.MusicURLResult, error) {
		if c.Sources == nil {
			return nil, errors.New("音源未初始化")
		}
		return c.Sources.MusicURL(ctx, platform, info, quality)
	}
	return c
}

// rememberRows 写入内存缓存并生成待落库的行
func (c *Catalog) rememberRows(infos []*Info) []db.Track {
	rows := make([]db.Track, 0, len(infos))
	for _, in := range infos {
		if in == nil || in.Source() == "" || in.Key() == "" {
			continue
		}
		id := in.TrackID()
		c.tracks.Add(id, in)
		rows = append(rows, db.Track{ID: id, Source: in.Source(), Name: in.Name(), Singer: in.Singer(), Album: in.Album(), JSON: in.JSON()})
	}
	return rows
}

// Cache 仅缓存临时歌曲元数据，不写入数据库。
// 在线搜索、榜单和专辑浏览结果属于临时数据，避免客户端扫描时撑大持久化存储。
func (c *Catalog) Cache(infos []*Info) {
	if len(infos) == 0 {
		return
	}
	c.rememberRows(infos)
	c.rememberArtistRefs(infos, false)
}

// CacheAlbum 缓存临时专辑，不写入数据库。
func (c *Catalog) CacheAlbum(id string, infos []*Info) {
	if id == "" || len(infos) == 0 {
		return
	}
	c.Cache(infos)
	merged := make([]*Info, 0, len(infos))
	seen := map[string]bool{}
	if current, ok := c.albums.Get(id); ok {
		for _, in := range current {
			if in != nil && !seen[in.TrackID()] {
				seen[in.TrackID()] = true
				merged = append(merged, in)
			}
		}
	}
	for _, in := range infos {
		if in != nil && !seen[in.TrackID()] {
			seen[in.TrackID()] = true
			merged = append(merged, in)
		}
	}
	c.albums.Add(id, merged)
}

// CachedAlbum 读取临时专辑缓存。
func (c *Catalog) CachedAlbum(id string) ([]*Info, bool) {
	return c.albums.Get(id)
}

// CacheArtist 缓存歌手 ID 与名称映射，不写入数据库。
func (c *Catalog) CacheArtist(id, name string) {
	if id != "" && name != "" {
		c.artists.Add(id, name)
	}
}

// CachedArtist 读取临时歌手名称。
func (c *Catalog) CachedArtist(id string) (string, bool) {
	return c.artists.Get(id)
}

// CachedTracksByArtist 从当前已发送或浏览过的歌曲中查找歌手歌曲，不触发在线扩展搜索。
func (c *Catalog) CachedTracksByArtist(name string, limit int) []*Info {
	var out []*Info
	for _, in := range c.tracks.Values() {
		if in != nil && (in.PrimarySinger() == name || strings.Contains(in.Singer(), name)) {
			out = append(out, in)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

// RememberSync 记录一批需要长期保留的歌曲元数据并同步写入数据库（歌单、收藏等场景）
func (c *Catalog) RememberSync(ctx context.Context, infos []*Info) error {
	if len(infos) == 0 {
		return nil
	}
	if c.DB == nil {
		return errors.New("数据库未初始化")
	}
	return c.DB.UpsertTracks(ctx, c.rememberRows(infos))
}

// Remember 记录一批歌曲元数据（内存 + 异步写数据库）
func (c *Catalog) Remember(ctx context.Context, infos []*Info) {
	if len(infos) == 0 {
		return
	}
	if c.DB == nil {
		return
	}
	rows := c.rememberRows(infos)
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.DB.UpsertTracks(bctx, rows); err != nil && c.Log != nil {
			c.Log.Warn("写入歌曲元数据失败", "err", err)
		}
	}()
}

// LocalTrack 只从内存与数据库读取歌曲，不触发平台兜底。
func (c *Catalog) LocalTrack(ctx context.Context, id string) (*Info, error) {
	if in, ok := c.tracks.Get(id); ok {
		return in, nil
	}
	if c.DB == nil {
		return nil, errors.New("数据库未初始化")
	}
	track, err := c.DB.GetTrack(ctx, id)
	if err != nil {
		return nil, err
	}
	in, err := ParseInfo(track.JSON)
	if err != nil {
		return nil, err
	}
	c.tracks.Add(id, in)
	c.rememberArtistRefs([]*Info{in}, false)
	return in, nil
}

// Track 按 ID 取歌曲元数据：内存 → 数据库 → 平台接口兜底
func (c *Catalog) Track(ctx context.Context, id string) (*Info, error) {
	if in, err := c.LocalTrack(ctx, id); err == nil {
		return in, nil
	}
	p, ok := ParseID(id)
	if !ok || p.Kind != KindTrack || !IsPlatform(p.Source) {
		return nil, fmt.Errorf("无效的歌曲 ID: %s", id)
	}
	in, err := c.fetchInfo(ctx, p.Source, p.Key)
	if err != nil {
		return nil, err
	}
	// 仅获取元数据不代表用户已保存或播放；普通浏览只进入内存缓存。
	c.Cache([]*Info{in})
	// 兜底信息的 key 可能与请求不同（如 kg 用 hash），也按请求 ID 缓存一份。
	c.tracks.Add(id, in)
	return in, nil
}

// Tracks 批量读取（保持顺序，缺失跳过）
func (c *Catalog) Tracks(ctx context.Context, ids []string) []*Info {
	out := make([]*Info, 0, len(ids))
	var miss []string
	positions := map[string][]int{}
	for _, id := range ids {
		if in, ok := c.tracks.Get(id); ok {
			out = append(out, in)
		} else {
			positions[id] = append(positions[id], len(out))
			out = append(out, nil)
			miss = append(miss, id)
		}
	}
	if len(miss) > 0 && c.DB != nil {
		rows, err := c.DB.GetTracks(ctx, miss)
		if err == nil {
			for _, r := range rows {
				if in, err := ParseInfo(r.JSON); err == nil {
					c.tracks.Add(r.ID, in)
					c.rememberArtistRefs([]*Info{in}, false)
					for _, pos := range positions[r.ID] {
						out[pos] = in
					}
				}
			}
		}
	}
	res := out[:0]
	for _, in := range out {
		if in != nil {
			res = append(res, in)
		}
	}
	return res
}

// fetchInfo 通过平台接口按 ID 获取元数据
func (c *Catalog) fetchInfo(ctx context.Context, source, key string) (*Info, error) {
	if len(c.SDK.Workers()) == 0 {
		return nil, errors.New("sdk 未初始化")
	}
	raw, err := c.SDK.CallFn(ctx, "__sdk_info", source, key)
	if err != nil {
		if c.Log != nil {
			c.Log.Debug("按 ID 获取元数据失败", "source", source, "key", key, "err", err)
		}
		return nil, errors.New("找不到歌曲元数据")
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, errors.New("找不到歌曲元数据，请先通过搜索访问该歌曲")
	}
	if _, ok := m["source"]; !ok {
		m["source"] = source
	}
	return FromMap(m), nil
}

type urlKey struct {
	trackID string
	quality string
}

// URLResolution 携带不透明的缓存版本，用于失败后的条件刷新。
// Result 为值拷贝，调用者不能修改缓存内的解析结果。
type URLResolution struct {
	Result js.MusicURLResult
	key    urlKey
	token  cacheToken
}

// ResolveURL 保留已有取链接口。
func (c *Catalog) ResolveURL(ctx context.Context, in *Info, quality string) (*js.MusicURLResult, error) {
	r, err := c.ResolvePlaybackURL(ctx, in, quality)
	if err != nil {
		return nil, err
	}
	return &r.Result, nil
}

// ResolvePlaybackURL 获取直链和对应的版本标识。
func (c *Catalog) ResolvePlaybackURL(ctx context.Context, in *Info, quality string) (URLResolution, error) {
	if quality == "" {
		quality = c.Settings.Get().DefaultQuality
	}
	key := urlKey{trackID: in.TrackID(), quality: SelectQuality(quality, in.Qualities())}
	return c.resolvePlaybackURL(ctx, in, key, nil)
}

// RefreshPlaybackURL 仅失效失败请求所使用的版本，复用其他请求刚刷新的结果。
func (c *Catalog) RefreshPlaybackURL(ctx context.Context, in *Info, failed URLResolution) (URLResolution, error) {
	if failed.key.trackID != in.TrackID() {
		return URLResolution{}, errors.New("直链刷新歌曲不匹配")
	}
	return c.resolvePlaybackURL(ctx, in, failed.key, &failed.token)
}

func (c *Catalog) resolvePlaybackURL(ctx context.Context, in *Info, key urlKey, failed *cacheToken) (URLResolution, error) {
	r, err := c.urls.load(ctx, key, failed, 45*time.Second, func(remoteCtx context.Context) (js.MusicURLResult, bool, error) {
		value, err := c.urlCall(remoteCtx, in.Source(), c.ScriptInfo(in), key.quality)
		if remoteCtx.Err() != nil {
			err = remoteCtx.Err()
		}
		if err != nil {
			return js.MusicURLResult{}, false, err
		}
		if value == nil || value.URL == "" {
			return js.MusicURLResult{}, false, errors.New("音源返回空播放地址")
		}
		return *value, true, nil
	})
	return URLResolution{Result: r.value, key: key, token: r.token}, err
}

// InvalidateURLs 在运行中音源发生变更后使直链及旧代次的在途结果失效。
func (c *Catalog) InvalidateURLs() {
	c.urls.purge()
}

// scriptInfo 传给音源脚本的 musicInfo：扁平字段 + meta 结构（兼容两种脚本写法）
func (c *Catalog) ScriptInfo(in *Info) map[string]any {
	m := make(map[string]any, len(in.Raw)+2)
	for k, v := range in.Raw {
		m[k] = v
	}
	meta := map[string]any{"songId": in.SongMid(), "albumName": in.Album(), "albumId": in.AlbumID(), "picUrl": in.Img()}
	if h := in.Hash(); h != "" {
		meta["hash"] = h
	}
	if v, ok := in.Raw["strMediaMid"]; ok {
		meta["strMediaMid"] = v
	}
	if v, ok := in.Raw["albumMid"]; ok {
		meta["albumMid"] = v
	}
	if v, ok := in.Raw["copyrightId"]; ok {
		meta["copyrightId"] = v
	}
	if v, ok := in.Raw["lrcUrl"]; ok {
		meta["lrcUrl"] = v
	}
	if v, ok := in.Raw["types"]; ok {
		meta["qualitys"] = v
	}
	if v, ok := in.Raw["_types"]; ok {
		meta["_qualitys"] = v
	}
	m["meta"] = meta
	m["id"] = in.TrackID()
	return m
}

// SearchOptions 搜索参数
type SearchOptions struct {
	Sources []string
	Page    int
	Limit   int
}

type searchKey struct {
	query   string
	sources string
	page    int
	limit   int
}

// Search 聚合搜索：并发请求各平台，按平台顺序交错合并。
func (c *Catalog) Search(ctx context.Context, query string, opts SearchOptions) []*Info {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}
	if opts.Limit <= 0 {
		opts.Limit = c.Settings.Get().SearchLimit
	}
	if len(opts.Sources) == 0 {
		opts.Sources = c.Settings.Get().SearchSources
	}
	opts.Sources = append([]string(nil), opts.Sources...)
	encodedSources, _ := json.Marshal(opts.Sources)
	key := searchKey{query: query, sources: string(encodedSources), page: opts.Page, limit: opts.Limit}
	r, err := c.search.load(ctx, key, nil, 15*time.Second, func(remoteCtx context.Context) ([]*Info, bool, error) {
		return c.searchPlatforms(remoteCtx, query, opts)
	})
	if err != nil {
		return nil
	}
	c.Cache(r.value)
	c.rememberArtistRefs(r.value, true)
	return append([]*Info(nil), r.value...)
}

func (c *Catalog) searchPlatforms(ctx context.Context, query string, opts SearchOptions) ([]*Info, bool, error) {
	type res struct {
		idx  int
		list []*Info
		err  error
	}
	ch := make(chan res, len(opts.Sources))
	for i, s := range opts.Sources {
		go func(i int, s string) {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			var out struct {
				List []map[string]any `json:"list"`
			}
			raw, err := c.searchCall(cctx, s+".musicSearch.search", query, opts.Page, opts.Limit)
			if err == nil {
				err = json.Unmarshal(raw, &out)
			}
			if err != nil {
				if c.Log != nil {
					c.Log.Debug("搜索失败", "source", s, "err", err)
				}
				ch <- res{idx: i, err: err}
				return
			}
			list := make([]*Info, 0, len(out.List))
			for _, m := range out.List {
				if m == nil {
					continue
				}
				if _, ok := m["source"]; !ok {
					m["source"] = s
				}
				list = append(list, FromMap(m))
			}
			ch <- res{idx: i, list: list}
		}(i, s)
	}
	lists := make([][]*Info, len(opts.Sources))
	complete := true
	for range opts.Sources {
		r := <-ch
		lists[r.idx] = r.list
		complete = complete && r.err == nil
	}
	var merged []*Info
	for {
		added := false
		for i := range lists {
			if len(lists[i]) > 0 {
				merged = append(merged, lists[i][0])
				lists[i] = lists[i][1:]
				added = true
			}
		}
		if !added {
			break
		}
	}
	return merged, complete && len(merged) > 0, nil
}

// Board 榜单
type Board struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	BangID string `json:"bangid"`
	Source string `json:"source"`
}

// UnmarshalJSON 兼容不同音源把榜单 ID 编码为数字或字符串。
func (b *Board) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.ID = anyToString(raw["id"])
	b.Name = anyToString(raw["name"])
	b.BangID = anyToString(raw["bangid"])
	if b.BangID == "" {
		b.BangID = anyToString(raw["bangId"])
	}
	return nil
}

// Boards 某平台榜单列表
func (c *Catalog) Boards(ctx context.Context, source string) ([]Board, error) {
	if !IsPlatform(source) {
		return nil, fmt.Errorf("不支持的平台: %s", source)
	}
	key := "boards|" + source
	raw, err := c.cachedCallRaw(ctx, key, source+".leaderboard.getBoards")
	if err != nil {
		return nil, err
	}
	var out struct {
		List []Board `json:"list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	valid := make([]Board, 0, len(out.List))
	seen := make(map[string]bool, len(out.List))
	for _, board := range out.List {
		board.Source = source
		board.Name = strings.TrimSpace(board.Name)
		board.BangID = strings.TrimSpace(board.BangID)
		if board.BangID == "" {
			// 部分旧音源只返回 id；去掉常见的 source__ 前缀后作为榜单参数。
			board.BangID = strings.TrimSpace(board.ID)
			if parts := strings.SplitN(board.BangID, "__", 2); len(parts) == 2 && parts[0] == source {
				board.BangID = parts[1]
			}
		}
		if board.Name == "" || !validPlatformID(board.BangID) {
			continue
		}
		nameKey := boardCacheKey(source, board.BangID)
		if seen[nameKey] {
			continue
		}
		seen[nameKey] = true
		if c.boardNames != nil {
			c.boardNames.Add(nameKey, board.Name)
		}
		valid = append(valid, board)
	}
	return valid, nil
}

// BoardName 返回最近一次榜单目录请求得到的名称，不触发上游请求。
func (c *Catalog) BoardName(source, bangID string) (string, bool) {
	if c == nil || c.boardNames == nil {
		return "", false
	}
	return c.boardNames.Get(boardCacheKey(source, bangID))
}

func boardCacheKey(source, bangID string) string {
	return strings.ToLower(strings.TrimSpace(source)) + "|" + strings.TrimSpace(bangID)
}

// BoardTracks 榜单歌曲
func (c *Catalog) BoardTracks(ctx context.Context, source, bangID string, page int) ([]*Info, error) {
	if page <= 0 {
		page = 1
	}
	key := fmt.Sprintf("board|%s|%s|%d", source, bangID, page)
	raw, err := c.cachedCallRaw(ctx, key, source+".leaderboard.getList", bangID, page)
	if err != nil {
		return nil, err
	}
	return c.parseList(ctx, raw, source)
}

// SongListDetail 在线歌单歌曲
func (c *Catalog) SongListDetail(ctx context.Context, source, id string, page int) ([]*Info, map[string]any, error) {
	if page <= 0 {
		page = 1
	}
	key := fmt.Sprintf("sl|%s|%s|%d", source, id, page)
	raw, err := c.cachedCallRaw(ctx, key, source+".songList.getListDetail", id, page)
	if err != nil {
		return nil, nil, err
	}
	var meta map[string]any
	_ = json.Unmarshal(raw, &meta)
	list, err := c.parseList(ctx, raw, source)
	return list, meta, err
}

// SongLists 在线歌单列表（sortId 例如 hot/new，tagId 为空表示全部）
func (c *Catalog) SongLists(ctx context.Context, source, sortID, tagID string, page int) (json.RawMessage, error) {
	key := fmt.Sprintf("sls|%s|%s|%s|%d", source, sortID, tagID, page)
	return c.cachedCallRaw(ctx, key, source+".songList.getList", sortID, tagID, page)
}

func (c *Catalog) parseList(ctx context.Context, raw json.RawMessage, source string) ([]*Info, error) {
	var out struct {
		List []map[string]any `json:"list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	list := make([]*Info, 0, len(out.List))
	for _, m := range out.List {
		if m == nil {
			continue
		}
		if _, ok := m["source"]; !ok {
			m["source"] = source
		}
		list = append(list, FromMap(m))
	}
	c.Cache(list)
	return list, nil
}

// HotSearch 热搜词
func (c *Catalog) HotSearch(ctx context.Context, source string) []string {
	var out struct {
		List []string `json:"list"`
	}
	if err := c.SDK.Call(ctx, source+".hotSearch.getList", &out); err != nil {
		return nil
	}
	return out.List
}

// Lyric 获取歌词：SDK 优先，失败回退音源脚本；带缓存
func (c *Catalog) Lyric(ctx context.Context, in *Info) (*Lyrics, error) {
	id := in.TrackID()
	if l, ok := c.lyrics.Get(id); ok {
		return l, nil
	}
	var r js.LyricResult
	err := c.SDK.Call(ctx, in.Source()+".getLyric", &r, in.Raw)
	if err != nil || r.Lyric == "" {
		if sr, serr := c.Sources.Lyric(ctx, in.Source(), c.ScriptInfo(in)); serr == nil && sr.Lyric != "" {
			r = *sr
			err = nil
		} else if err == nil {
			err = errors.New("歌词为空")
		}
	}
	if err != nil {
		return nil, err
	}
	l := ParseLyrics(r.Lyric, r.TLyric, r.RLyric)
	c.lyrics.Add(id, l)
	return l, nil
}

// Cover 封面 URL：元数据自带 → SDK getPic → 音源脚本 pic
func (c *Catalog) Cover(ctx context.Context, in *Info) string {
	if u := in.Img(); strings.HasPrefix(u, "http") {
		return u
	}
	key := "pic|" + in.TrackID()
	if v, ok := c.generic.Get(key); ok {
		var s string
		_ = json.Unmarshal(v, &s)
		return s
	}
	var pic string
	if err := c.SDK.Call(ctx, in.Source()+".getPic", &pic, in.Raw); err != nil || !strings.HasPrefix(pic, "http") || strings.Contains(pic, "undefined") {
		pic = ""
		if p, err := c.Sources.Pic(ctx, in.Source(), c.ScriptInfo(in)); err == nil {
			pic = p
		}
	}
	if pic != "" {
		b, _ := json.Marshal(pic)
		c.generic.Add(key, b)
		// 歌曲元数据被搜索、封面和取链请求共享，不能在这里修改 Raw。
		// 封面结果只保存在独立缓存中。
	}
	return pic
}

// PurgeMetadataCaches 清空歌曲及派生元数据缓存，供管理端清理持久化元数据后调用。
func (c *Catalog) PurgeMetadataCaches() {
	c.tracks.Purge()
	c.albums.Purge()
	c.artists.Purge()
	c.artistRefs.Purge()
	c.seenArtists.Purge()
	c.searchedArtists.Purge()
	c.artistDetails.Purge()
	c.albumPages.Purge()
	c.albumMetadata.Purge()
	c.boardNames.Purge()
	c.failures.Purge()
	c.search.purge()
	c.generic.Purge()
}

// RefreshTTL 幂等地应用最新 TTL，仅失效发生变化的缓存。
func (c *Catalog) RefreshTTL() {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	v := c.Settings.Get()
	c.urls.configure(time.Duration(v.URLCacheTTL) * time.Second)
	c.search.configure(time.Duration(v.SearchCacheTTL) * time.Second)
}
