package music

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"lxsc/internal/settings"
)

type marshalProbe struct{ calls *int }

func (p marshalProbe) MarshalJSON() ([]byte, error) { *p.calls++; return []byte(`"测试"`), nil }

func TestTemporaryCacheDoesNotSerializeDatabaseRows(t *testing.T) {
	c := NewCatalog(nil, nil, nil, &settings.Store{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	calls := 0
	info := FromMap(map[string]any{"source": "wy", "songmid": "1", "name": "歌曲", "extra": marshalProbe{&calls}})
	c.Cache([]*Info{nil, info})
	if calls != 0 {
		t.Fatal("临时缓存不应执行JSON序列化")
	}
	if got, ok := c.tracks.Get(info.TrackID()); !ok || got != info {
		t.Fatal("歌曲未缓存")
	}
	c.rememberRows([]*Info{info})
	if calls != 1 {
		t.Fatal("持久化路径仍需生成JSON")
	}
}

// BenchmarkTemporaryCache 对比原有落库行准备路径和纯内存路径，数据完全合成。
func BenchmarkTemporaryCache(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "memory-only"
		if legacy {
			name = "legacy-rows"
		}
		b.Run(name, func(b *testing.B) {
			c := NewCatalog(nil, nil, nil, &settings.Store{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			infos := make([]*Info, 100)
			for i := range infos {
				infos[i] = FromMap(map[string]any{"source": "wy", "songmid": fmt.Sprint(i), "name": "基准歌曲", "singer": "歌手", "albumName": "专辑", "types": []any{map[string]any{"type": "320k"}}})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if legacy {
					c.rememberRows(infos)
					c.rememberArtistRefs(infos, false)
				} else {
					c.Cache(infos)
				}
			}
		})
	}
}
