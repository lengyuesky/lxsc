package portal

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"lxsc/internal/db"
)

func (s *Server) listeningProgress(w http.ResponseWriter, r *http.Request) {
	var p db.ListeningProgress
	if err := decodeJSON(w, r, &p); err != nil {
		fail(w, 400, "听歌进度格式不正确")
		return
	}
	// 客户端绑定原账号，阻止换号后迟到的请求写入新账号。
	if p.UserID != currentUser(r).ID {
		fail(w, 403, "播放会话所属用户已改变")
		return
	}
	track := db.ListeningTrack{ID: p.TrackID, Name: "未知歌曲"}
	if stored, err := s.DB.GetTrack(r.Context(), p.TrackID); err == nil {
		track.Name, track.Singer = stored.Name, stored.Singer
	} else if s.Catalog != nil {
		in, err := s.Catalog.Track(r.Context(), p.TrackID)
		if err != nil {
			fail(w, 400, "找不到播放歌曲")
			return
		}
		track.Name, track.Singer = in.Name(), in.Singer()
	} else {
		fail(w, 400, "找不到播放歌曲")
		return
	}
	if err := s.DB.SaveListeningProgress(r.Context(), p, track, time.Now()); err != nil {
		if errors.Is(err, db.ErrListeningProgress) {
			fail(w, 400, err.Error())
		} else {
			fail(w, 500, "保存听歌进度失败")
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listeningStats(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	userID := user.ID
	scope := r.URL.Query().Get("scope")
	switch scope {
	case "", "me":
		if r.URL.Query().Get("userId") != "" {
			fail(w, 400, "个人统计不能指定其他用户")
			return
		}
	case "all", "user":
		if !user.IsAdmin {
			fail(w, 403, "仅管理员可查看其他用户或全站统计")
			return
		}
		userID = 0
		if scope == "user" {
			id, err := strconv.ParseInt(r.URL.Query().Get("userId"), 10, 64)
			if err != nil || id <= 0 {
				fail(w, 400, "用户编号无效")
				return
			}
			if _, err = s.DB.GetUserByID(r.Context(), id); err != nil {
				fail(w, 404, "用户不存在")
				return
			}
			userID = id
		}
	default:
		fail(w, 400, "统计范围无效")
		return
	}
	days := 30
	if value := r.URL.Query().Get("days"); value != "" {
		var err error
		days, err = strconv.Atoi(value)
		if err != nil {
			fail(w, 400, "时间范围无效")
			return
		}
	}
	if days != 7 && days != 30 && days != 90 && days != 365 {
		fail(w, 400, "支持近 7、30、90 或 365 天")
		return
	}
	stats, err := s.DB.ListeningStatistics(r.Context(), userID, days, time.Now())
	if err != nil {
		fail(w, 500, "读取听歌统计失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, stats)
}
