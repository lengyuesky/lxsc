package subsonic

import (
	"database/sql"
	"errors"
	"lxsc/internal/db"
	"lxsc/internal/music"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) savePlayQueue(w http.ResponseWriter, r *http.Request) {
	ids := params(r, "id")
	if len(ids) > db.MaxPlaylistTracks {
		writeErr(w, r, ErrGeneric, "播放队列最多2000首")
		return
	}
	current := param(r, "current")
	found := current == ""
	for _, id := range ids {
		if parsed, ok := music.ParseID(id); !ok || parsed.Kind != music.KindTrack || !music.IsPlatform(parsed.Source) || parsed.Key == "" || len(id) > 1024 {
			writeErr(w, r, ErrGeneric, "歌曲ID无效")
			return
		}
		if id == current {
			found = true
		}
	}
	position := int64(0)
	if raw := param(r, "position"); raw != "" {
		var err error
		position, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || position < 0 {
			writeErr(w, r, ErrGeneric, "播放位置无效")
			return
		}
	}
	if !found {
		writeErr(w, r, ErrGeneric, "当前歌曲不在队列中")
		return
	}
	client := param(r, "c")
	if len(client) > 200 {
		writeErr(w, r, ErrGeneric, "客户端名称过长")
		return
	}
	if err := s.DB.SavePlayQueue(r.Context(), currentUser(r).ID, db.PlayQueue{IDs: ids, Current: current, Position: position, ChangedBy: client}); err != nil {
		writeErr(w, r, ErrGeneric, "保存播放队列失败")
		return
	}
	writeOK(w, r, "", nil)
}
func (s *Server) getPlayQueue(w http.ResponseWriter, r *http.Request) {
	q, err := s.DB.GetPlayQueue(r.Context(), currentUser(r).ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOK(w, r, "", nil)
		return
	}
	if err != nil {
		writeErr(w, r, ErrGeneric, "读取播放队列失败")
		return
	}
	rc := s.newReqCtx(r)
	entries := make([]M, 0, len(q.IDs))
	for _, id := range q.IDs {
		if in, err := s.Catalog.Track(r.Context(), id); err == nil {
			m := s.songObj(rc, in)
			m["id"] = id
			entries = append(entries, m)
		} else {
			entries = append(entries, M{"id": id, "title": "无法读取的歌曲", "isDir": false})
		}
	}
	out := M{"entry": entries, "position": q.Position, "username": currentUser(r).Name, "changed": fmtTime(q.ChangedAt), "changedBy": q.ChangedBy}
	if q.Current != "" {
		out["current"] = q.Current
	}
	writeOK(w, r, "playQueue", out)
}
func (s *Server) setRating(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(param(r, "id"))
	rating, err := strconv.Atoi(param(r, "rating"))
	if id == "" || len(id) > 1024 || err != nil || rating < 0 || rating > 5 {
		writeErr(w, r, ErrGeneric, "评分必须为0–5，且必须提供歌曲ID")
		return
	}
	if parsed, ok := music.ParseID(id); !ok || parsed.Kind != music.KindTrack || !music.IsPlatform(parsed.Source) || parsed.Key == "" || len(id) > 1024 {
		writeErr(w, r, ErrGeneric, "仅支持歌曲评分")
		return
	}
	if err = s.DB.SetRating(r.Context(), currentUser(r).ID, id, rating); err != nil {
		writeErr(w, r, ErrGeneric, "保存评分失败")
		return
	}
	writeOK(w, r, "", nil)
}
func (s *Server) getNowPlaying(w http.ResponseWriter, r *http.Request) {
	list, err := s.DB.NowPlaying(r.Context(), currentUser(r).ID)
	if err != nil {
		writeErr(w, r, ErrGeneric, "读取正在播放失败")
		return
	}
	rc := s.newReqCtx(r)
	entries := []M{}
	for _, item := range list {
		in, err := s.Catalog.Track(r.Context(), item.TrackID)
		if err != nil {
			continue
		}
		m := s.songObj(rc, in)
		m["username"] = item.Username
		m["minutesAgo"] = int(time.Now().Unix()-item.UpdatedAt) / 60
		m["playerName"] = item.Client
		m["playerId"] = 0
		entries = append(entries, m)
	}
	writeOK(w, r, "nowPlaying", M{"entry": entries})
}
