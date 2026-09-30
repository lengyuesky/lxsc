package portal

import (
	"context"
	"database/sql"
	"errors"
	"lxsc/internal/db"
	"lxsc/internal/music"
	"net/http"
	"time"
)

func (s *Server) getSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	sub, err := s.DB.Subscription(r.Context(), p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 200, nil)
		return
	}
	if err != nil {
		fail(w, 500, "读取订阅失败")
		return
	}
	writeJSON(w, 200, sub)
}
func (s *Server) saveSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	var body struct {
		Source string `json:"source"`
		Input  string `json:"input"`
		Auto   bool   `json:"auto"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	var id string
	if body.Source != "" {
		var err error
		id, err = music.ParseOnlinePlaylistID(body.Source, body.Input)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
	}
	if err := s.DB.SaveSubscription(r.Context(), p.ID, currentUser(r), body.Source, id, body.Auto); err != nil {
		playlistWriteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) syncSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	var body struct {
		Preview          bool   `json:"preview"`
		ExpectedRevision string `json:"expectedRevision"`
		Version          int64  `json:"version"`
		RemoteRevision   string `json:"remoteRevision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	sub, err := s.DB.Subscription(r.Context(), p.ID)
	if err != nil {
		fail(w, 404, "请先保存订阅来源")
		return
	}
	if !body.Preview && (body.ExpectedRevision != db.TracksRevision(p.TrackIDs) || body.Version != sub.Version) {
		playlistWriteError(w, db.ErrPlaylistConflict)
		return
	}
	release, ok := s.admitSearch(w, r)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	remote, err := s.Catalog.FetchOnlinePlaylist(ctx, sub.Source, sub.RemoteID)
	if err != nil {
		fail(w, 502, "读取源歌单失败，本地歌单未变更")
		return
	}
	ids, rows, seen, removed := subscriptionChanges(p, sub, remote)
	if body.Preview {
		writeJSON(w, 200, map[string]any{"added": len(rows), "removed": removed, "truncated": remote.Truncated, "skipped": remote.Skipped, "expectedRevision": db.TracksRevision(p.TrackIDs), "version": sub.Version, "remoteRevision": db.TracksRevision(seen)})
		return
	}
	if body.RemoteRevision != db.TracksRevision(seen) {
		fail(w, 409, "源歌单已变化，请重新预览")
		return
	}
	if remote.Truncated > 0 || remote.Skipped > 0 {
		fail(w, 409, "源歌单未完整读取，请使用手动导入确认内容")
		return
	}
	saved, err := s.DB.ApplySubscription(ctx, sub, currentUser(r), body.ExpectedRevision, ids, rows, seen)
	if err != nil {
		playlistWriteError(w, err)
		return
	}
	writeJSON(w, 200, s.detail(ctx, currentUser(r), saved))
}
func subscriptionChanges(p *db.Playlist, sub *db.Subscription, remote *music.OnlinePlaylist) (ids []string, rows []db.Track, seen []string, removed int) {
	ids = append([]string{}, p.TrackIDs...)
	known := map[string]bool{}
	previous := map[string]bool{}
	remoteIDs := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	for _, id := range sub.Seen {
		previous[id] = true
	}
	for _, in := range remote.Tracks {
		id := in.TrackID()
		if remoteIDs[id] {
			continue
		}
		remoteIDs[id] = true
		seen = append(seen, id)
		if known[id] || previous[id] {
			continue
		}
		known[id] = true
		ids = append(ids, id)
		rows = append(rows, db.Track{ID: id, Source: in.Source(), Name: in.Name(), Singer: in.Singer(), Album: in.Album(), JSON: in.JSON()})
	}
	for id := range previous {
		if !remoteIDs[id] {
			removed++
		}
	}
	return
}

// StartSubscriptions returns a stop function that cancels and joins all work before DB shutdown.
func (s *Server) StartSubscriptions(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			s.runSubscriptions(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
func (s *Server) runSubscriptions(ctx context.Context) {
	ids, err := s.DB.DueSubscriptions(ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		sub, err := s.DB.Subscription(ctx, id)
		if err != nil || !sub.Auto {
			continue
		}
		p, err := s.DB.GetPlaylist(ctx, id)
		if err != nil {
			continue
		}
		actor, err := s.DB.GetUserByID(ctx, p.UserID)
		if err != nil {
			continue
		}
		func() {
			task, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			release, err := s.Catalog.AcquireRequest(task, actor.ID)
			if err == nil {
				defer release()
				var remote *music.OnlinePlaylist
				remote, err = s.Catalog.FetchOnlinePlaylist(task, sub.Source, sub.RemoteID)
				if err == nil {
					if remote.Truncated > 0 || remote.Skipped > 0 {
						err = errors.New("incomplete playlist")
					} else {
						ids, rows, seen, _ := subscriptionChanges(p, sub, remote)
						_, err = s.DB.ApplySubscription(task, sub, actor, db.TracksRevision(p.TrackIDs), ids, rows, seen)
					}
				}
			}
			if err != nil {
				_ = s.DB.SubscriptionFailed(ctx, sub)
			}
		}()
	}
}
