package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lxsc/internal/js"
)

// RemoteRequest 描述一次真实上游目录请求，可用于日志与实机验证懒加载边界。
type RemoteRequest struct {
	Key  string
	Path string
	Args []any
}

// ArtistRef 是某个平台内可定位的歌手引用。
type ArtistRef struct {
	Source    string `json:"source"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Avatar    string `json:"avatar,omitempty"`
	AlbumSize int    `json:"albumSize,omitempty"`
}

// ArtistDetail 是平台歌手入口所需的轻量详情。
type ArtistDetail struct {
	ArtistRef
	Description string `json:"description,omitempty"`
	MusicSize   int    `json:"musicSize,omitempty"`
}

// AlbumMeta 是不依赖歌曲实体的在线专辑元数据。
type AlbumMeta struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	Name        string `json:"name"`
	Artist      string `json:"artist"`
	ArtistID    string `json:"artistId,omitempty"`
	Image       string `json:"image,omitempty"`
	PublishTime string `json:"publishTime,omitempty"`
	SongCount   int    `json:"songCount,omitempty"`
}

// AlbumPage 是固定 50 条语义的歌手专辑页。
type AlbumPage struct {
	List    []AlbumMeta `json:"list"`
	Page    int         `json:"page"`
	Limit   int         `json:"limit"`
	Total   int         `json:"total"`
	HasMore bool        `json:"hasMore"`
}

type requestCall struct {
	done chan struct{}
	data json.RawMessage
	err  error
}

type requestGroup struct {
	mu sync.Mutex
	m  map[string]*requestCall
}

func (g *requestGroup) do(ctx context.Context, key string, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*requestCall{}
	}
	call, ok := g.m[key]
	if !ok {
		call = &requestCall{done: make(chan struct{})}
		g.m[key] = call
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					call.data = nil
					call.err = fmt.Errorf("在线目录请求异常: %v", recovered)
				}
				close(call.done)
				g.mu.Lock()
				delete(g.m, key)
				g.mu.Unlock()
			}()
			call.data, call.err = fn()
		}()
	}
	g.mu.Unlock()

	select {
	case <-call.done:
		return call.data, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SetRequestObserver 设置真实上游请求观察器；命中缓存或等待 singleflight 不会触发。
func (c *Catalog) SetRequestObserver(observer func(RemoteRequest)) {
	c.observeMu.Lock()
	c.observer = observer
	c.observeMu.Unlock()
}

// SetRemoteCallerForTest 注入目录上游调用，仅供聚焦自动测试使用。
func (c *Catalog) SetRemoteCallerForTest(caller func(context.Context, string, ...any) (json.RawMessage, error)) {
	c.observeMu.Lock()
	c.remoteCall = caller
	c.observeMu.Unlock()
}

func (c *Catalog) observeRequest(request RemoteRequest) {
	c.observeMu.RLock()
	observer := c.observer
	c.observeMu.RUnlock()
	if observer != nil {
		observer(request)
	}
	if c.Log != nil {
		c.Log.Debug("在线目录上游请求", "key", request.Key, "path", request.Path)
	}
}

func (c *Catalog) cachedCallRaw(ctx context.Context, key, path string, args ...any) (json.RawMessage, error) {
	if value, ok := c.generic.Get(key); ok {
		return value, nil
	}
	if message, ok := c.failures.Get(key); ok {
		return nil, errors.New(message)
	}
	return c.flight.do(ctx, key, func() (json.RawMessage, error) {
		if value, ok := c.generic.Get(key); ok {
			return value, nil
		}
		if message, ok := c.failures.Get(key); ok {
			return nil, errors.New(message)
		}
		c.observeMu.RLock()
		caller := c.remoteCall
		c.observeMu.RUnlock()
		if caller == nil {
			if c.SDK == nil {
				return nil, errors.New("sdk 未初始化")
			}
			caller = c.SDK.CallRaw
		}
		request := RemoteRequest{Key: key, Path: path, Args: append([]any(nil), args...)}
		c.observeRequest(request)
		// 共享请求不绑定首个客户端生命周期，任一等待者取消都不会拖累其他请求。
		remoteCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		value, err := caller(remoteCtx, path, args...)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				c.failures.Add(key, err.Error())
			}
			return nil, err
		}
		c.generic.Add(key, value)
		return value, nil
	})
}

func artistCacheKey(source, name string) string {
	return source + "|" + strings.ToLower(strings.TrimSpace(name))
}

func (c *Catalog) rememberArtistRefs(infos []*Info, searched bool) {
	for _, info := range infos {
		if info == nil || info.PrimarySinger() == "" || !IsPlatform(info.Source()) {
			continue
		}
		name := info.PrimarySinger()
		ref := ArtistRef{Source: info.Source(), ID: info.SingerID(), Name: name, Avatar: info.Img()}
		key := artistCacheKey(ref.Source, ref.Name)
		if old, ok := c.artistRefs.Get(key); ok {
			if ref.ID == "" {
				ref.ID = old.ID
			}
			if ref.Avatar == "" {
				ref.Avatar = old.Avatar
			}
		}
		c.artistRefs.Add(key, ref)
		c.seenArtists.Add(strings.ToLower(strings.TrimSpace(name)), name)
		if searched {
			c.searchedArtists.Add(strings.ToLower(strings.TrimSpace(name)), name)
		}
	}
}

// EnabledPlatforms 返回当前可用的平台，目录不会主动请求上游探测能力。
func (c *Catalog) EnabledPlatforms() []string {
	if c.Sources != nil {
		if platforms := c.Sources.SupportedPlatforms(); len(platforms) > 0 {
			return platforms
		}
	}
	if c.Settings != nil {
		if platforms := uniquePlatformNames(c.Settings.Get().SearchSources); len(platforms) > 0 {
			return platforms
		}
	}
	return append([]string(nil), js.AllPlatforms...)
}

func uniquePlatformNames(sources []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		if IsPlatform(source) && !seen[source] {
			seen[source] = true
			out = append(out, source)
		}
	}
	return out
}

// KnownArtistRef 返回会话中已知的平台歌手引用。
func (c *Catalog) KnownArtistRef(source, name string) (ArtistRef, bool) {
	return c.artistRefs.Get(artistCacheKey(source, name))
}

// SeenArtistNames 返回当前进程浏览过的歌手名称。
func (c *Catalog) SeenArtistNames() []string { return sortedUniqueValues(c.seenArtists.Values()) }

// SearchedArtistNames 返回当前进程搜索结果中出现过的歌手名称。
func (c *Catalog) SearchedArtistNames() []string {
	return sortedUniqueValues(c.searchedArtists.Values())
}

func sortedUniqueValues(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validPlatformID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "0"
}

func anyToInt(value any) int {
	text := anyToString(value)
	if text == "" {
		return 0
	}
	valueInt, err := strconv.Atoi(text)
	if err == nil {
		return valueInt
	}
	_, _ = fmt.Sscan(text, &valueInt)
	return valueInt
}

func artistRefFromMap(source string, item map[string]any) ArtistRef {
	ref := ArtistRef{Source: source,
		ID:     anyToString(item["id"]),
		Name:   strings.TrimSpace(anyToString(item["name"])),
		Avatar: anyToString(item["avatar"]),
	}
	if ref.ID == "" {
		ref.ID = anyToString(item["mid"])
	}
	if ref.Name == "" {
		ref.Name = strings.TrimSpace(anyToString(item["singerName"]))
	}
	if ref.Avatar == "" {
		ref.Avatar = anyToString(item["picUrl"])
	}
	if ref.Avatar == "" {
		ref.Avatar = anyToString(item["img"])
	}
	ref.AlbumSize = anyToInt(item["albumSize"])
	if ref.AlbumSize == 0 {
		ref.AlbumSize = anyToInt(item["albumNum"])
	}
	return ref
}

func artistDetailFromMap(source string, fallback ArtistRef, item map[string]any) ArtistDetail {
	ref := artistRefFromMap(source, item)
	detail := ArtistDetail{ArtistRef: ref, Description: anyToString(item["description"]), MusicSize: anyToInt(item["musicSize"])}
	if detail.Description == "" {
		detail.Description = anyToString(item["desc"])
	}
	if detail.AlbumSize == 0 {
		detail.AlbumSize = anyToInt(item["albumNum"])
	}
	if info, ok := item["info"].(map[string]any); ok {
		infoRef := artistRefFromMap(source, info)
		if infoRef.Name != "" {
			ref.Name = infoRef.Name
		}
		if infoRef.ID != "" {
			ref.ID = infoRef.ID
		}
		if infoRef.Avatar != "" {
			ref.Avatar = infoRef.Avatar
		}
		if detail.Description == "" {
			detail.Description = anyToString(info["desc"])
		}
		count, _ := item["count"].(map[string]any)
		if count != nil {
			if detail.MusicSize == 0 {
				detail.MusicSize = anyToInt(count["music"])
			}
			if ref.AlbumSize == 0 {
				ref.AlbumSize = anyToInt(count["album"])
			}
		}
	}
	if ref.ID == "" {
		ref.ID = fallback.ID
	}
	if ref.Name == "" {
		ref.Name = fallback.Name
	}
	if ref.Avatar == "" {
		ref.Avatar = fallback.Avatar
	}
	if ref.Source == "" {
		ref.Source = fallback.Source
	}
	detail.ArtistRef = ref
	return detail
}

// ResolveArtist 在单个平台内精确解析歌手，不会触发跨平台聚合。
func (c *Catalog) ResolveArtist(ctx context.Context, ref ArtistRef) (ArtistRef, error) {
	ref.Name = strings.TrimSpace(ref.Name)
	if !IsPlatform(ref.Source) || ref.Name == "" {
		return ArtistRef{}, errors.New("无效的平台歌手")
	}
	if ref.ID == "" {
		if known, ok := c.KnownArtistRef(ref.Source, ref.Name); ok && known.ID != "" {
			ref = known
		}
	}
	if ref.ID != "" {
		c.artistRefs.Add(artistCacheKey(ref.Source, ref.Name), ref)
		return ref, nil
	}

	if ref.Source == "wy" || ref.Source == "tx" {
		key := "artist-search|" + artistCacheKey(ref.Source, ref.Name)
		raw, err := c.cachedCallRaw(ctx, key, ref.Source+".extendSearch.searchSinger", ref.Name, 1, 50)
		if err != nil {
			return ArtistRef{}, err
		}
		var result struct {
			List []map[string]any `json:"list"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return ArtistRef{}, err
		}
		for _, item := range result.List {
			candidate := artistRefFromMap(ref.Source, item)
			if strings.EqualFold(strings.TrimSpace(candidate.Name), ref.Name) && candidate.ID != "" {
				c.artistRefs.Add(artistCacheKey(candidate.Source, candidate.Name), candidate)
				return candidate, nil
			}
		}
		return ArtistRef{}, fmt.Errorf("%s 未找到精确歌手: %s", PlatformName(ref.Source), ref.Name)
	}

	if ref.Source == "kg" {
		result, err := c.searchSourceSongs(ctx, ref.Source, ref.Name, 1, 50)
		if err != nil {
			return ArtistRef{}, err
		}
		for _, info := range result.List {
			if strings.EqualFold(info.PrimarySinger(), ref.Name) && info.SingerID() != "" {
				resolved := ArtistRef{Source: ref.Source, ID: info.SingerID(), Name: info.PrimarySinger(), Avatar: info.Img()}
				c.artistRefs.Add(artistCacheKey(resolved.Source, resolved.Name), resolved)
				return resolved, nil
			}
		}
		return ArtistRef{}, fmt.Errorf("%s 未找到带 ID 的精确歌手: %s", PlatformName(ref.Source), ref.Name)
	}

	// 酷我和咪咕没有稳定歌手详情接口，名称本身就是平台内精确定位条件。
	c.artistRefs.Add(artistCacheKey(ref.Source, ref.Name), ref)
	return ref, nil
}

// ArtistDetail 获取歌手详情；酷我和咪咕保持名称定位，不提前扫描专辑。
func (c *Catalog) ArtistDetail(ctx context.Context, ref ArtistRef) (ArtistDetail, error) {
	resolved, err := c.ResolveArtist(ctx, ref)
	if err != nil {
		return ArtistDetail{}, err
	}
	key := "artist-detail|" + artistCacheKey(resolved.Source, resolved.Name) + "|" + resolved.ID
	if detail, ok := c.artistDetails.Get(key); ok {
		return detail, nil
	}
	if resolved.Source == "kw" || resolved.Source == "mg" {
		detail := ArtistDetail{ArtistRef: resolved}
		c.artistDetails.Add(key, detail)
		return detail, nil
	}

	var raw json.RawMessage
	switch resolved.Source {
	case "wy", "tx":
		raw, err = c.cachedCallRaw(ctx, key, resolved.Source+".extendDetail.getArtistDetail", resolved.ID)
	case "kg":
		raw, err = c.cachedCallRaw(ctx, key, "kg.singer.getInfo", resolved.ID)
	}
	if err != nil {
		return ArtistDetail{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		if err == nil {
			err = errors.New("歌手详情格式无效")
		}
		return ArtistDetail{}, err
	}
	detail := artistDetailFromMap(resolved.Source, resolved, payload)
	if detail.Source == "" {
		detail.Source = resolved.Source
	}
	if detail.ID == "" {
		detail.ID = resolved.ID
	}
	if detail.Name == "" {
		detail.Name = resolved.Name
	}
	c.artistRefs.Add(artistCacheKey(detail.Source, detail.Name), detail.ArtistRef)
	c.artistDetails.Add(key, detail)
	return detail, nil
}

// ArtistAlbums 获取固定 50 条的歌手专辑页。
func (c *Catalog) ArtistAlbums(ctx context.Context, ref ArtistRef, page int) (AlbumPage, error) {
	if page < 1 {
		page = 1
	}
	resolved, err := c.ResolveArtist(ctx, ref)
	if err != nil {
		return AlbumPage{}, err
	}
	key := fmt.Sprintf("artist-albums|%s|%s|%s|%d|50", resolved.Source, strings.ToLower(resolved.Name), resolved.ID, page)
	if result, ok := c.albumPages.Get(key); ok {
		return result, nil
	}
	if resolved.Source == "kw" || resolved.Source == "mg" {
		result, err := c.searchArtistAlbumsFallback(ctx, resolved, page)
		if err == nil {
			c.cacheAlbumPage(key, result)
		}
		return result, err
	}

	var raw json.RawMessage
	switch resolved.Source {
	case "wy", "tx":
		raw, err = c.cachedCallRaw(ctx, key, resolved.Source+".extendDetail.getArtistAlbums", resolved.ID, page, 50)
	case "kg":
		raw, err = c.cachedCallRaw(ctx, key, "kg.singer.getAlbumList", resolved.ID, page, 50)
	default:
		err = errors.New("平台不支持歌手专辑")
	}
	if err != nil {
		return AlbumPage{}, err
	}
	var payload struct {
		List  []map[string]any `json:"list"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return AlbumPage{}, err
	}
	result := AlbumPage{Page: page, Limit: 50, Total: payload.Total}
	for _, item := range payload.List {
		meta := albumMetaFromMap(resolved, item)
		if validPlatformID(meta.ID) && strings.TrimSpace(meta.Name) != "" {
			result.List = append(result.List, meta)
		}
	}
	result.List = dedupeAlbumMetadata(result.List)
	result.HasMore = result.Total > page*50 || (result.Total == 0 && len(payload.List) == 50)
	c.cacheAlbumPage(key, result)
	return result, nil
}

func albumMetaFromMap(ref ArtistRef, item map[string]any) AlbumMeta {
	meta := AlbumMeta{Source: ref.Source, Artist: ref.Name, ArtistID: ref.ID}
	meta.ID = anyToString(item["id"])
	meta.Name = anyToString(item["name"])
	meta.Image = anyToString(item["img"])
	if meta.Image == "" {
		meta.Image = anyToString(item["picUrl"])
	}
	if value := anyToString(item["singer"]); value != "" {
		meta.Artist = value
	}
	meta.PublishTime = anyToString(item["publishTime"])
	if count, err := fmt.Sscan(anyToString(item["total"]), &meta.SongCount); count == 0 || err != nil {
		_, _ = fmt.Sscan(anyToString(item["count"]), &meta.SongCount)
	}
	if info, ok := item["info"].(map[string]any); ok {
		if value := anyToString(info["name"]); value != "" {
			meta.Name = value
		}
		if value := anyToString(info["author"]); value != "" {
			meta.Artist = value
		}
		if value := anyToString(info["img"]); value != "" {
			meta.Image = value
		}
		meta.PublishTime = anyToString(info["publishTime"])
	}
	if ref.Source == "tx" {
		if mid := anyToString(item["mid"]); mid != "" {
			meta.ID = mid
		}
	}
	return meta
}

func (c *Catalog) cacheAlbumPage(key string, page AlbumPage) {
	c.albumPages.Add(key, page)
	for _, album := range page.List {
		c.CacheAlbumMeta(album)
	}
}

func dedupeAlbumMetadata(items []AlbumMeta) []AlbumMeta {
	seen := map[string]bool{}
	out := make([]AlbumMeta, 0, len(items))
	for _, item := range items {
		key := item.Source + "|" + item.ID
		if item.ID == "" {
			key = item.Source + "|" + strings.ToLower(item.Name) + "|" + strings.ToLower(item.Artist)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

type sourceSongResult struct {
	List  []*Info
	Total int
}

func (c *Catalog) searchSourceSongs(ctx context.Context, source, query string, page, limit int) (sourceSongResult, error) {
	key := fmt.Sprintf("source-search|%s|%s|%d|%d", source, strings.ToLower(strings.TrimSpace(query)), page, limit)
	raw, err := c.cachedCallRaw(ctx, key, source+".musicSearch.search", query, page, limit)
	if err != nil {
		return sourceSongResult{}, err
	}
	var payload struct {
		List  []map[string]any `json:"list"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return sourceSongResult{}, err
	}
	result := sourceSongResult{Total: payload.Total}
	for _, item := range payload.List {
		if item == nil {
			continue
		}
		if _, ok := item["source"]; !ok {
			item["source"] = source
		}
		result.List = append(result.List, FromMap(item))
	}
	c.Cache(result.List)
	return result, nil
}

func (c *Catalog) searchArtistAlbumsFallback(ctx context.Context, ref ArtistRef, page int) (AlbumPage, error) {
	const limit = 50
	start, end := (page-1)*limit, page*limit
	var albums []AlbumMeta
	remotePage := 1
	totalSongs := 0
	moreSongs := true
	for len(albums) < end && moreSongs {
		result, err := c.searchSourceSongs(ctx, ref.Source, ref.Name, remotePage, limit)
		if err != nil {
			return AlbumPage{}, err
		}
		if result.Total > 0 {
			totalSongs = result.Total
		}
		for _, info := range result.List {
			// 没有平台专辑 ID 时无法在点击后精确获取歌曲，不能输出死目录节点。
			if !strings.EqualFold(strings.TrimSpace(info.PrimarySinger()), ref.Name) || info.Album() == "" || !validPlatformID(info.AlbumID()) {
				continue
			}
			albums = append(albums, AlbumMeta{ID: info.AlbumID(), Source: ref.Source, Name: info.Album(), Artist: info.PrimarySinger(), ArtistID: ref.ID, Image: info.Img()})
		}
		albums = dedupeAlbumMetadata(albums)
		moreSongs = len(result.List) == limit
		if totalSongs > 0 {
			moreSongs = remotePage*limit < totalSongs
		}
		remotePage++
	}
	result := AlbumPage{Page: page, Limit: limit, Total: len(albums)}
	if start < len(albums) {
		to := end
		if to > len(albums) {
			to = len(albums)
		}
		result.List = append(result.List, albums[start:to]...)
	}
	result.HasMore = len(albums) > end || moreSongs
	return result, nil
}

// CacheAlbumMeta 缓存不依赖歌曲实体的专辑摘要，不写入数据库。
func (c *Catalog) CacheAlbumMeta(meta AlbumMeta) {
	if !IsPlatform(meta.Source) || strings.TrimSpace(meta.Name) == "" {
		return
	}
	if meta.ID != "" {
		c.albumMetadata.Add(AlbumID(meta.Source, meta.ID, meta.Name, meta.Artist), meta)
	}
	c.albumMetadata.Add(OnlineAlbumID(meta.Source, meta.ID, meta.Name, meta.Artist, meta.Image), meta)
}

// CacheAlbumMetaForID 按目录节点 ID 缓存专辑摘要，支持带父目录上下文的 oa-* ID。
func (c *Catalog) CacheAlbumMetaForID(id string, meta AlbumMeta) {
	if id == "" || !IsPlatform(meta.Source) || strings.TrimSpace(meta.Name) == "" {
		return
	}
	c.albumMetadata.Add(id, meta)
	c.CacheAlbumMeta(meta)
}

// CachedAlbumMeta 返回在线歌手目录中已见的专辑元数据。
func (c *Catalog) CachedAlbumMeta(id string) (AlbumMeta, bool) { return c.albumMetadata.Get(id) }

// AlbumSongsFor 仅在打开真实在线专辑节点时获取歌曲。
func (c *Catalog) AlbumSongsFor(ctx context.Context, locator AlbumLocator) ([]*Info, AlbumMeta, error) {
	locator.Source = strings.TrimSpace(locator.Source)
	locator.Name = strings.TrimSpace(locator.Name)
	if !IsPlatform(locator.Source) || locator.Name == "" {
		return nil, AlbumMeta{}, errors.New("无效的在线专辑")
	}
	onlineID := OnlineAlbumID(locator.Source, locator.ID, locator.Name, locator.Artist, locator.Image, locator.Parent)
	if locator.ID != "" {
		if infos, ok := c.CachedAlbum(AlbumID(locator.Source, locator.ID, "", "")); ok && len(infos) > 0 {
			meta, _ := c.CachedAlbumMeta(AlbumID(locator.Source, locator.ID, "", ""))
			if meta.Name == "" {
				meta = AlbumMeta{ID: locator.ID, Source: locator.Source, Name: locator.Name, Artist: locator.Artist, Image: locator.Image, SongCount: len(infos)}
			}
			return infos, meta, nil
		}
	}
	if infos, ok := c.CachedAlbum(onlineID); ok && len(infos) > 0 {
		meta, _ := c.CachedAlbumMeta(onlineID)
		if meta.Name == "" {
			meta = AlbumMeta{ID: locator.ID, Source: locator.Source, Name: locator.Name, Artist: locator.Artist, Image: locator.Image, SongCount: len(infos)}
		}
		return infos, meta, nil
	}
	// 收藏过的在线专辑以完整 oa-* ID 持久化；进程重启后优先从数据库恢复。
	if c.DB != nil {
		if source, name, artist, raw, err := c.DB.GetAlbum(ctx, onlineID); err == nil {
			var ids []string
			if json.Unmarshal(raw, &ids) == nil && len(ids) > 0 {
				infos := c.Tracks(ctx, ids)
				if len(infos) == len(ids) {
					meta := AlbumMeta{ID: locator.ID, Source: source, Name: name, Artist: artist, Image: locator.Image, SongCount: len(infos)}
					if meta.Source == "" {
						meta.Source = locator.Source
					}
					if meta.Name == "" {
						meta.Name = locator.Name
					}
					if meta.Artist == "" {
						meta.Artist = locator.Artist
					}
					c.albumMetadata.Add(onlineID, meta)
					c.CacheAlbum(onlineID, infos)
					return infos, meta, nil
				}
			}
		}
	}
	if !validPlatformID(locator.ID) {
		return nil, AlbumMeta{}, errors.New("专辑缺少平台 ID")
	}
	key := "album-songs|" + locator.Source + "|" + locator.ID
	var path string
	var args []any
	switch locator.Source {
	case "wy", "tx":
		path = locator.Source + ".extendDetail.getAlbumSongs"
		args = []any{locator.ID}
	case "kg":
		path = "kg.album.getAlbumDetail"
		args = []any{locator.ID, 1, 200}
	case "kw":
		path = "kw.album.getAlbumListDetail"
		args = []any{locator.ID, 1}
	case "mg":
		path = "mg.album.getAlbumDetail"
		args = []any{locator.ID, 1}
	default:
		return nil, AlbumMeta{}, errors.New("平台不支持专辑歌曲")
	}
	raw, err := c.cachedCallRaw(ctx, key, path, args...)
	if err != nil {
		return nil, AlbumMeta{}, err
	}
	var payload struct {
		List []map[string]any `json:"list"`
		Name string           `json:"name"`
		Info map[string]any   `json:"info"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, AlbumMeta{}, err
	}
	infos := make([]*Info, 0, len(payload.List))
	for _, item := range payload.List {
		if item == nil {
			continue
		}
		if _, ok := item["source"]; !ok {
			item["source"] = locator.Source
		}
		infos = append(infos, FromMap(item))
	}
	if len(infos) == 0 {
		return nil, AlbumMeta{}, errors.New("专辑没有可用歌曲")
	}
	meta, _ := c.CachedAlbumMeta(onlineID)
	if meta.Name == "" && locator.ID != "" {
		meta, _ = c.CachedAlbumMeta(AlbumID(locator.Source, locator.ID, "", ""))
	}
	if meta.Name == "" {
		meta = AlbumMeta{ID: locator.ID, Source: locator.Source, Name: locator.Name, Artist: locator.Artist, Image: locator.Image}
	}
	// 平台返回的真实名称优先于旧 al-* 路径使用的平台 ID 占位名。
	if payload.Name != "" {
		meta.Name = payload.Name
	}
	if payload.Info != nil {
		if value := anyToString(payload.Info["name"]); value != "" {
			meta.Name = value
		}
		if value := anyToString(payload.Info["author"]); value != "" {
			meta.Artist = value
		}
		if value := anyToString(payload.Info["img"]); value != "" {
			meta.Image = value
		}
	}
	if meta.Name == "" {
		meta.Name = infos[0].Album()
	}
	if meta.Artist == "" {
		meta.Artist = infos[0].PrimarySinger()
	}
	meta.ID = locator.ID
	meta.Source = locator.Source
	meta.SongCount = len(infos)
	c.albumMetadata.Add(onlineID, meta)
	c.CacheAlbumMeta(meta)
	c.CacheAlbum(onlineID, infos)
	// 同时填充旧 al-* 缓存，保证旧客户端保存的专辑 ID 仍可复用。
	if locator.ID != "" {
		c.CacheAlbum(AlbumID(locator.Source, locator.ID, meta.Name, meta.Artist), infos)
	}
	return infos, meta, nil
}

// AlbumSongs 兼容旧 al-* 调用，实际仍只在专辑被打开时请求。
func (c *Catalog) AlbumSongs(ctx context.Context, source, albumID string) ([]*Info, AlbumMeta, error) {
	meta, _ := c.CachedAlbumMeta(AlbumID(source, albumID, "", ""))
	name := meta.Name
	if name == "" {
		// 旧 al-* ID 未必有摘要缓存；这里允许以平台 ID 作为临时显示名继续请求。
		name = albumID
	}
	return c.AlbumSongsFor(ctx, AlbumLocator{Source: source, ID: albumID, Name: name, Artist: meta.Artist, Image: meta.Image})
}
