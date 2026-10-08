package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lxsc/internal/admission"
)

type metadataSearchKey struct {
	searchKey
	kind string
}

type metadataSearchResult struct {
	Artists []ArtistRef
	Albums  []AlbumMeta
	HasMore bool
}

// SearchArtists 使用平台歌手搜索，不依赖同名歌曲是否出现在歌曲搜索结果中。
func (c *Catalog) SearchArtists(ctx context.Context, query string, opts SearchOptions) ([]ArtistRef, error) {
	artists, _, err := c.SearchArtistPage(ctx, query, opts)
	return artists, err
}

// SearchAlbums 只搜索专辑摘要，专辑歌曲仍在用户打开专辑时加载。
func (c *Catalog) SearchAlbums(ctx context.Context, query string, opts SearchOptions) ([]AlbumMeta, error) {
	albums, _, err := c.SearchAlbumPage(ctx, query, opts)
	return albums, err
}

// SearchArtistPage 同时返回是否仍有平台可能存在下一页，避免从去重后的条数猜测页长。
func (c *Catalog) SearchArtistPage(ctx context.Context, query string, opts SearchOptions) ([]ArtistRef, bool, error) {
	result, err := c.searchMetadata(ctx, query, "artist", opts)
	return result.Artists, result.HasMore, err
}

// SearchAlbumPage 使用原始平台页长判断是否继续读取。
func (c *Catalog) SearchAlbumPage(ctx context.Context, query string, opts SearchOptions) ([]AlbumMeta, bool, error) {
	result, err := c.searchMetadata(ctx, query, "album", opts)
	return result.Albums, result.HasMore, err
}

func (c *Catalog) searchMetadata(ctx context.Context, query, kind string, opts SearchOptions) (metadataSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || ctx.Err() != nil {
		return metadataSearchResult{}, ctx.Err()
	}
	if len(opts.Sources) == 0 {
		opts.Sources = c.Settings.Get().SearchSources
	}
	sources := uniquePlatformNames(opts.Sources)
	if len(sources) == 0 {
		return metadataSearchResult{}, nil
	}
	page, limit := max(1, opts.Page), opts.Limit
	if limit <= 0 {
		limit = c.Settings.Get().SearchLimit
	}
	limit = min(50, max(1, limit))
	type sourceResult struct {
		source string
		result metadataSearchResult
		err    error
	}
	ch := make(chan sourceResult, len(sources))
	for _, source := range sources {
		go func() {
			if source != "wy" && source != "tx" {
				// 歌曲回退已有自己的缓存和工作额度，避免嵌套占用同一个准入队列。
				result, err := c.searchSourceMetadata(ctx, source, query, kind, page, limit)
				ch <- sourceResult{source, result, err}
				return
			}
			key := metadataSearchKey{searchKey: searchKey{query: query, sources: source, page: page, limit: limit}, kind: kind}
			result, err := c.metadataSearch.load(ctx, key, nil, 15*time.Second, func(callCtx context.Context) (metadataSearchResult, bool, error) {
				value, err := c.searchSourceMetadata(callCtx, source, query, kind, page, limit)
				return value, len(value.Artists)+len(value.Albums) > 0, err
			})
			ch <- sourceResult{source, result.value, err}
		}()
	}
	results := map[string]metadataSearchResult{}
	var lastErr error
	busy := false
	collect := func(result sourceResult) {
		if result.err != nil {
			lastErr = result.err
			busy = busy || errors.Is(result.err, admission.ErrBusy)
			return
		}
		results[result.source] = result.result
	}
receive:
	for remaining := len(sources); remaining > 0; remaining-- {
		select {
		case result := <-ch:
			collect(result)
		case <-ctx.Done():
			// 已完成平台仍可返回，慢平台不能抹掉可用结果。
			lastErr = ctx.Err()
			for {
				select {
				case result := <-ch:
					collect(result)
				default:
					break receive
				}
			}
		}
	}
	var out metadataSearchResult
	for _, result := range results {
		out.HasMore = out.HasMore || result.HasMore
	}
	seen := map[string]bool{}
	for index := 0; ; index++ {
		added := false
		for _, source := range sources {
			result := results[source]
			if kind == "artist" && index < len(result.Artists) {
				added = true
				ref := result.Artists[index]
				key := strings.ToLower(strings.TrimSpace(ref.Name))
				if !seen[key] {
					seen[key] = true
					out.Artists = append(out.Artists, ref)
				}
				c.cacheSearchArtist(ref)
			}
			if kind == "album" && index < len(result.Albums) {
				added = true
				album := result.Albums[index]
				key := album.Source + "|" + album.ID
				if !seen[key] {
					seen[key] = true
					out.Albums = append(out.Albums, album)
				}
				c.CacheAlbumMeta(album)
				if album.Artist != "" {
					c.cacheSearchArtist(ArtistRef{Source: album.Source, ID: album.ArtistID, Name: album.Artist})
				}
			}
		}
		if !added {
			break
		}
	}
	if len(results) == 0 {
		if busy {
			return out, admission.ErrBusy
		}
		return out, lastErr
	}
	return out, nil
}

func (c *Catalog) cacheSearchArtist(ref ArtistRef) {
	key := artistCacheKey(ref.Source, ref.Name)
	if old, ok := c.artistRefs.Get(key); ok {
		if ref.ID == "" {
			ref.ID = old.ID
		}
		if ref.Avatar == "" {
			ref.Avatar = old.Avatar
		}
		if ref.AlbumSize == 0 {
			ref.AlbumSize = old.AlbumSize
		}
	}
	c.artistRefs.Add(key, ref)
	c.CacheArtist(ArtistID(ref.Name), ref.Name)
	c.CacheArtist(SingerDirectoryID(ref.Source, ref.Name, ref.ID), ref.Name)
	c.seenArtists.Add(strings.ToLower(ref.Name), ref.Name)
	c.searchedArtists.Add(strings.ToLower(ref.Name), ref.Name)
}

func (c *Catalog) searchSourceMetadata(ctx context.Context, source, query, kind string, page, limit int) (metadataSearchResult, error) {
	var out metadataSearchResult
	if source == "wy" || source == "tx" {
		method := "searchSinger"
		if kind == "album" {
			method = "searchAlbum"
		}
		key := fmt.Sprintf("metadata-search|%s|%s|%s|%d|%d", source, kind, query, page, limit)
		raw, err := c.callRaw(ctx, key, source+".extendSearch."+method, query, page, limit)
		if err != nil {
			return out, err
		}
		var payload struct {
			List []map[string]any `json:"list"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return out, err
		}
		out.HasMore = len(payload.List) >= limit
		for _, item := range payload.List {
			if kind == "artist" {
				ref := artistRefFromMap(source, item)
				if ref.Name != "" && validPlatformID(ref.ID) {
					out.Artists = append(out.Artists, ref)
				}
			} else {
				album := albumMetaFromMap(ArtistRef{Source: source}, item)
				if strings.TrimSpace(album.Name) != "" && validPlatformID(album.ID) {
					out.Albums = append(out.Albums, album)
				}
			}
		}
		return out, nil
	}

	// 其余 SDK 没有歌手/专辑搜索接口；复用歌曲搜索，仅保留对应名称匹配的实体。
	tracks, err := c.SearchChecked(ctx, query, SearchOptions{Sources: []string{source}, Page: page, Limit: limit})
	if err != nil {
		return out, err
	}
	out.HasMore = len(tracks) >= limit
	artistIndexes, albumIndexes := map[string]int{}, map[string]int{}
	artistAlbums := map[string]map[string]bool{}
	seenTracks := map[string]bool{}
	for _, track := range tracks {
		if seenTracks[track.TrackID()] {
			continue
		}
		seenTracks[track.TrackID()] = true
		if kind == "artist" {
			name := strings.TrimSpace(track.PrimarySinger())
			if name != "" && strings.Contains(strings.ToLower(name), strings.ToLower(query)) {
				key := strings.ToLower(name)
				index, ok := artistIndexes[key]
				if !ok {
					index = len(out.Artists)
					artistIndexes[key] = index
					artistAlbums[key] = map[string]bool{}
					out.Artists = append(out.Artists, ArtistRef{Source: source, ID: track.SingerID(), Name: name, Avatar: track.Img()})
				}
				if !artistAlbums[key][track.AlbumSubID()] {
					artistAlbums[key][track.AlbumSubID()] = true
					out.Artists[index].AlbumSize++
				}
			}
		} else if validPlatformID(track.AlbumID()) && track.Album() != "" && strings.Contains(strings.ToLower(track.Album()), strings.ToLower(query)) {
			index, ok := albumIndexes[track.AlbumID()]
			if !ok {
				index = len(out.Albums)
				albumIndexes[track.AlbumID()] = index
				out.Albums = append(out.Albums, AlbumMeta{Source: source, ID: track.AlbumID(), Name: track.Album(), Artist: track.PrimarySinger(), ArtistID: track.SingerID(), Image: track.Img()})
			}
			out.Albums[index].SongCount++
		}
	}
	return out, nil
}
