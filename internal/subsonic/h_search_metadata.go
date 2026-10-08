package subsonic

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/music"
)

func (s *Server) searchOnline(w http.ResponseWriter, r *http.Request, rc *reqCtx, query string, sources []string, songCount, songOffset, albumCount, albumOffset, artistCount, artistOffset int) {
	sources = uniquePlatforms(sources)
	budget := 15 * time.Second
	if len(sources) > 1 {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(rc.ctx, budget)
	defer cancel()
	limit := s.Settings.Get().SearchLimit
	var songs []*music.Info
	var albums []music.AlbumMeta
	var artists []music.ArtistRef
	var songErr, albumErr, artistErr error
	var tasks sync.WaitGroup
	// 三类请求各自使用 count/offset；关闭歌曲结果不会关闭艺术家或专辑搜索。
	tasks.Go(func() {
		songs, songErr = onlineSearchPage(ctx, sources, limit, songCount, songOffset, func(opts music.SearchOptions) ([]*music.Info, error) {
			return s.Catalog.SearchChecked(ctx, query, opts)
		}, func(info *music.Info) string { return info.TrackID() })
	})
	tasks.Go(func() {
		albums, albumErr = metadataSearchWindow(ctx, sources, limit, albumCount, albumOffset, func(opts music.SearchOptions) ([]music.AlbumMeta, bool, error) {
			return s.Catalog.SearchAlbumPage(ctx, query, opts)
		}, func(album music.AlbumMeta) string { return album.Source + "|" + album.ID })
	})
	tasks.Go(func() {
		artists, artistErr = metadataSearchWindow(ctx, sources, limit, artistCount, artistOffset, func(opts music.SearchOptions) ([]music.ArtistRef, bool, error) {
			return s.Catalog.SearchArtistPage(ctx, query, opts)
		}, func(artist music.ArtistRef) string { return strings.ToLower(artist.Name) })
	})
	tasks.Wait()
	if err := errors.Join(songErr, albumErr, artistErr); err != nil && len(songs)+len(albums)+len(artists) == 0 {
		if errors.Is(err, admission.ErrBusy) {
			writeErr(w, r, ErrBusy, admission.ErrBusy.Error())
		} else {
			writeErr(w, r, ErrGeneric, "在线搜索暂时不可用，请稍后重试")
		}
		return
	}
	albumObjs := make([]M, 0, len(albums))
	for _, album := range albums {
		albumObjs = append(albumObjs, s.onlineAlbumObj(rc, album, ""))
	}
	artistObjs := make([]M, 0, len(artists))
	for _, artist := range artists {
		artistObjs = append(artistObjs, s.onlineArtistObj(rc, artist))
	}
	s.writeSearch(w, r, songList(s, rc, songs), albumObjs, artistObjs)
}

// metadataSearchWindow 按实际实体数分页。少结果平台、去重和歌曲回退都会使
// 每页数量减少，不能再用“平台数 × 歌曲页长”推算艺术家或专辑偏移。
func metadataSearchWindow[T any](ctx context.Context, sources []string, limit, count, offset int, fetch func(music.SearchOptions) ([]T, bool, error), id func(T) string) ([]T, error) {
	if count <= 0 || len(sources) == 0 {
		return nil, nil
	}
	limit = min(50, max(1, limit))
	var list []T
	seen := map[string]bool{}
	for page := 1; page <= 100; page++ {
		if err := ctx.Err(); err != nil {
			return slicePage(list, offset, count), err
		}
		items, hasMore, err := fetch(music.SearchOptions{Sources: sources, Page: page, Limit: limit})
		if err != nil {
			return slicePage(list, offset, count), err
		}
		added := 0
		for _, item := range items {
			key := id(item)
			if !seen[key] {
				seen[key] = true
				list = append(list, item)
				added++
			}
		}
		if len(list) >= offset+count || added == 0 || !hasMore {
			return slicePage(list, offset, count), nil
		}
	}
	// 有界读取，防止异常分页或任意大偏移触发无限扫描。
	return slicePage(list, offset, count), errors.New("搜索分页超过读取上限")
}

// onlineSearchPage 保留每次至多三个上游页的预算，按本类结果的偏移分页。
func onlineSearchPage[T any](ctx context.Context, sources []string, limit, count, offset int, fetch func(music.SearchOptions) ([]T, error), id func(T) string) ([]T, error) {
	if count <= 0 || len(sources) == 0 {
		return nil, nil
	}
	limit = min(50, max(1, limit, (count+len(sources)-1)/len(sources)))
	perPage := limit * len(sources)
	startPage, endPage := offset/perPage+1, (offset+count-1)/perPage+1
	var list []T
	seen := map[string]bool{}
	var lastErr error
	for page := startPage; page <= endPage && page <= startPage+2 && ctx.Err() == nil; page++ {
		items, err := fetch(music.SearchOptions{Sources: sources, Page: page, Limit: limit})
		if err != nil {
			lastErr = err
			break
		}
		for _, item := range items {
			key := id(item)
			if !seen[key] {
				seen[key] = true
				list = append(list, item)
			}
		}
	}
	if len(list) == 0 && ctx.Err() != nil {
		lastErr = ctx.Err()
	}
	return slicePage(list, offset-(startPage-1)*perPage, count), lastErr
}

func (s *Server) onlineArtistObj(rc *reqCtx, ref music.ArtistRef) M {
	id := music.SingerDirectoryID(ref.Source, ref.Name, ref.ID)
	obj := M{"id": id, "name": ref.Name, "title": ref.Name, "isDir": true, "coverArt": id, "albumCount": max(0, ref.AlbumSize), "sortName": ref.Name, "musicBrainzId": ""}
	s.Catalog.CacheArtist(id, ref.Name)
	if ts := rc.starred[id]; ts > 0 {
		obj["starred"] = fmtTime(ts)
	}
	return obj
}

func (s *Server) onlineAlbumObj(rc *reqCtx, album music.AlbumMeta, parent string) M {
	artistID := music.SingerDirectoryID(album.Source, album.Artist, album.ArtistID)
	if album.Artist == "" {
		artistID = ""
	}
	if parent == "" {
		parent = artistID
	}
	id := music.OnlineAlbumID(album.Source, album.ID, album.Name, album.Artist, album.Image, parent)
	s.Catalog.CacheAlbumMetaForID(id, album)
	obj := albumMetaObj(id, parent, album)
	obj["artistId"], obj["duration"], obj["musicBrainzId"], obj["isCompilation"] = artistID, 0, "", false
	if artistID != "" {
		obj["artists"] = []M{{"id": artistID, "name": album.Artist}}
	}
	if ts := rc.starred[id]; ts > 0 {
		obj["starred"] = fmtTime(ts)
	}
	return obj
}

// getOnlineArtist 只查询搜索结果定位的平台，不跨平台扫歌，也不提前加载专辑歌曲。
func (s *Server) getOnlineArtist(w http.ResponseWriter, r *http.Request, rc *reqCtx, id string, locator music.SingerLocator) {
	ref := music.ArtistRef{Source: locator.Source, ID: locator.ID, Name: locator.Name}
	if known, ok := s.Catalog.KnownArtistRef(ref.Source, ref.Name); ok {
		ref.Avatar, ref.AlbumSize = known.Avatar, known.AlbumSize
	}
	page, err := s.Catalog.ArtistAlbums(rc.ctx, ref, 1)
	if err != nil {
		writeErr(w, r, ErrGeneric, "暂时无法读取艺术家专辑，请稍后重试")
		return
	}
	albums := make([]M, 0, min(len(page.List), s.Settings.Get().ArtistAlbumLimit))
	for _, album := range slicePage(page.List, 0, s.Settings.Get().ArtistAlbumLimit) {
		albums = append(albums, s.onlineAlbumObj(rc, album, id))
	}
	obj := s.onlineArtistObj(rc, ref)
	obj["id"], obj["album"], obj["albumCount"] = id, albums, len(albums)
	writeOK(w, r, "artist", obj)
}
