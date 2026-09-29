package portal

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/secret"
)

// importOnlinePlaylist 仅导入无需平台登录即可访问的歌单，不接受平台 Cookie。
func (s *Server) importOnlinePlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source           string  `json:"source"`
		Input            string  `json:"input"`
		Preview          bool    `json:"preview"`
		Target           string  `json:"target"`
		OwnerID          int64   `json:"ownerId"`
		Public           *bool   `json:"public"`
		ExpectedRevision *string `json:"expectedRevision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	id, err := music.ParseOnlinePlaylistID(body.Source, body.Input)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	u := currentUser(r)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var target *db.Playlist
	ownerID := u.ID
	// 写入权限在发出上游请求前检查；追加提交时还会在事务内复查。
	if !body.Preview {
		if body.Target != "" {
			target, err = s.DB.GetPlaylist(ctx, body.Target)
			if err != nil {
				playlistWriteError(w, err)
				return
			}
			if !canEditPlaylist(u, target) {
				playlistWriteError(w, db.ErrPlaylistForbidden)
				return
			}
			if body.ExpectedRevision != nil && *body.ExpectedRevision != db.TracksRevision(target.TrackIDs) {
				playlistWriteError(w, db.ErrPlaylistConflict)
				return
			}
			if len(target.TrackIDs) > maxPlaylistTracks {
				playlistWriteError(w, db.ErrPlaylistLimit)
				return
			}
		} else if body.OwnerID != 0 {
			if body.OwnerID < 0 {
				fail(w, http.StatusBadRequest, "ownerId 无效")
				return
			}
			if !u.IsAdmin && body.OwnerID != u.ID {
				fail(w, http.StatusForbidden, "不能替其他用户创建歌单")
				return
			}
			if _, err := s.DB.GetUserByID(ctx, body.OwnerID); err != nil {
				fail(w, http.StatusBadRequest, "归属用户不存在")
				return
			}
			ownerID = body.OwnerID
		}
	}
	release, ok := s.admitSearch(w, r)
	if !ok {
		return
	}
	defer release()
	pl, err := s.Catalog.FetchOnlinePlaylist(ctx, body.Source, id)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fail(w, http.StatusGatewayTimeout, "读取歌单超时，请稍后重试")
		} else {
			fail(w, http.StatusBadGateway, "读取歌单失败，请确认歌单公开、链接正确且平台可访问后重试")
		}
		return
	}
	if len(pl.Tracks) == 0 {
		fail(w, http.StatusBadRequest, "歌单中没有可导入的歌曲，请确认歌单公开且非空")
		return
	}
	if body.Preview {
		writeJSON(w, http.StatusOK, map[string]any{
			"lists": []importListView{{Index: 0, Name: pl.Name, Source: pl.Source, Count: len(pl.Tracks), Skipped: pl.Skipped}},
			"total": pl.Total, "truncated": pl.Truncated,
		})
		return
	}
	ids := []string{}
	seen := map[string]bool{}
	if target != nil {
		ids = append(ids, target.TrackIDs...)
		for _, id := range ids {
			seen[id] = true
		}
	}
	rows := []db.Track{}
	truncated := pl.Truncated
	for _, in := range pl.Tracks {
		id := in.TrackID()
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(ids) >= maxPlaylistTracks {
			truncated++
			continue
		}
		ids = append(ids, id)
		rows = append(rows, db.Track{ID: id, Source: in.Source(), Name: in.Name(), Singer: in.Singer(), Album: in.Album(), JSON: in.JSON()})
	}
	var saved *db.Playlist
	if target != nil {
		revision := db.TracksRevision(target.TrackIDs)
		saved, err = s.DB.ReplacePlaylistTracksChecked(ctx, target.ID, u, ids, rows, &revision)
	} else {
		name := []rune(pl.Name)
		if len(name) > 100 {
			name = name[:100]
		}
		public := s.Settings.Get().PublicPlaylists
		if body.Public != nil {
			public = *body.Public
		}
		newID := music.KindPlaylist + "-" + secret.RandomToken(9)
		comment := "从" + music.PlatformName(pl.Source) + "导入\n" + pl.URL
		err = s.DB.CreatePlaylistWithMetadata(ctx, newID, ownerID, strings.TrimSpace(string(name)), comment, public, ids, rows)
		if err == nil {
			saved, err = s.DB.GetPlaylist(ctx, newID)
		}
	}
	if err != nil {
		playlistWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"playlists": []playlistView{makePlaylistView(u, saved)}, "added": len(rows), "skipped": pl.Skipped, "truncated": truncated,
	})
}
