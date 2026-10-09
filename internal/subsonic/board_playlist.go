package subsonic

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

// boardPlaylistObj 是列表和详情共用的摘要；nil infos 表示尚无可用快照。
// 简介不包含瞬时加载状态，版本只随真实内容变化，不随请求、缓存过期或重启变化。
func (s *Server) boardPlaylistObj(rc *reqCtx, board music.Board, infos []*music.Info) (M, error) {
	var content *db.BoardPlaylistContent
	if infos != nil {
		rows := make([]map[string]any, 0, len(infos))
		content = &db.BoardPlaylistContent{Count: len(infos)}
		for _, info := range infos {
			rows = append(rows, info.Raw)
			content.Duration += info.Duration()
		}
		// JSON 排序对象键但保留歌曲顺序：同数量的换歌、重排和元数据更新也能被识别。
		raw, err := json.Marshal(rows)
		if err != nil {
			return nil, err
		}
		content.Hash = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	id := music.BoardID(board.Source, board.BangID)
	meta, err := s.DB.SyncBoardPlaylistMetadata(rc.ctx, id, board.Name, "在线榜单，只读", content)
	if err != nil {
		return nil, err
	}
	name := meta.Name
	if name == "" {
		name = board.BangID
	}
	return M{
		"id": id, "name": music.PlatformName(board.Source) + " · " + name,
		"comment": meta.Comment, "owner": "榜单", "public": true,
		// 从未加载的数量用 1 保留可打开入口；已有摘要继续使用上次真实数量。
		"songCount": meta.Count, "duration": meta.Duration,
		"created": virtualPlaylistTime, "changed": fmtTime(meta.UpdatedAt), "coverArt": id,
	}, nil
}
