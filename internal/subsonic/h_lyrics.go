package subsonic

import (
	"net/http"

	"lxsc/internal/music"
)

func (s *Server) getLyrics(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	var in *music.Info
	if id != "" {
		in, _ = s.Catalog.Track(rc.ctx, id)
	}
	if in == nil {
		artist, title := param(r, "artist"), param(r, "title")
		if title != "" {
			res := s.Catalog.Search(rc.ctx, title+" "+artist, music.SearchOptions{Limit: 5})
			if len(res) > 0 {
				in = res[0]
			}
		}
	}
	if in == nil {
		writeOK(w, r, "lyrics", M{"value": ""})
		return
	}
	l, err := s.Catalog.Lyric(rc.ctx, in)
	if err != nil {
		writeOK(w, r, "lyrics", M{"artist": in.Singer(), "title": in.Name(), "value": ""})
		return
	}
	writeOK(w, r, "lyrics", M{"artist": in.Singer(), "title": in.Name(), "value": l.MergedLRC()})
}

func lineObjs(lines []music.Line) []M {
	out := make([]M, 0, len(lines))
	for _, ln := range lines {
		out = append(out, M{"start": ln.Start, "value": ln.Value})
	}
	return out
}

func (s *Server) getLyricsBySongID(w http.ResponseWriter, r *http.Request) {
	rc := s.newReqCtx(r)
	id := param(r, "id")
	in, err := s.Catalog.Track(rc.ctx, id)
	if err != nil {
		writeErr(w, r, ErrNotFound, err.Error())
		return
	}
	l, err := s.Catalog.Lyric(rc.ctx, in)
	if err != nil {
		writeOK(w, r, "lyricsList", M{"structuredLyrics": []M{}})
		return
	}
	var structured []M
	structured = append(structured, M{
		"lang": "xxx", "synced": l.Synced, "displayArtist": in.Singer(), "displayTitle": in.Name(), "offset": l.Offset, "line": lineObjs(l.Lines),
	})
	if len(l.Trans) > 0 {
		structured = append(structured, M{"lang": "zho", "synced": true, "displayArtist": in.Singer(), "displayTitle": in.Name() + " (翻译)", "offset": l.Offset, "line": lineObjs(l.Trans)})
	}
	if len(l.Roma) > 0 {
		structured = append(structured, M{"lang": "rom", "synced": true, "displayArtist": in.Singer(), "displayTitle": in.Name() + " (罗马音)", "offset": l.Offset, "line": lineObjs(l.Roma)})
	}
	writeOK(w, r, "lyricsList", M{"structuredLyrics": structured})
}
