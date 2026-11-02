package fnmusic

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/simplelru"
	"lxsc/internal/music"
)

const (
	searchGrantTTL          = 30 * time.Minute
	searchGrantLimit        = 8192
	searchSessionGrantLimit = 2048
)

type searchResource struct {
	track  *music.Info
	album  *music.AlbumMeta
	artist *music.ArtistRef
}

type searchGrantKey struct {
	session [32]byte
	kind    string
	guid    string
}

type searchGrant struct {
	resource searchResource
	expires  time.Time
}

// Search grants are session-scoped, bounded and volatile; GUIDs or global caches are not authority.
type searchStore struct {
	mu      sync.Mutex
	entries *simplelru.LRU[searchGrantKey, searchGrant]
	counts  map[[32]byte]int
}

func (s *searchStore) grant(ctx context.Context, kind, guid string, resource searchResource, now time.Time) {
	session, ok := ctx.Value(sessionKey{}).([32]byte)
	if !ok || !validGUID(guid) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.counts = map[[32]byte]int{}
		s.entries, _ = simplelru.NewLRU[searchGrantKey, searchGrant](searchGrantLimit, func(key searchGrantKey, _ searchGrant) {
			s.counts[key.session]--
			if s.counts[key.session] == 0 {
				delete(s.counts, key.session)
			}
		})
	}
	key := searchGrantKey{session, kind, guid}
	if !s.entries.Contains(key) {
		if s.counts[session] >= searchSessionGrantLimit {
			for _, candidate := range s.entries.Keys() {
				entry, _ := s.entries.Peek(candidate)
				if !now.Before(entry.expires) {
					s.entries.Remove(candidate)
					continue
				}
				if candidate.session == session {
					s.entries.Remove(candidate)
					break
				}
			}
		}
		s.counts[session]++
	}
	s.entries.Add(key, searchGrant{resource, now.Add(searchGrantTTL)})
}

func (s *searchStore) get(ctx context.Context, kind, guid string, now time.Time) (searchResource, bool) {
	session, ok := ctx.Value(sessionKey{}).([32]byte)
	if !ok || !validGUID(guid) {
		return searchResource{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		return searchResource{}, false
	}
	key := searchGrantKey{session, kind, guid}
	entry, ok := s.entries.Get(key)
	if !ok {
		return searchResource{}, false
	}
	if !now.Before(entry.expires) {
		s.entries.Remove(key)
		return searchResource{}, false
	}
	return entry.resource, true
}

func (s *searchStore) revoke(session [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries != nil {
		for _, key := range s.entries.Keys() {
			if key.session == session {
				s.entries.Remove(key)
			}
		}
	}
}
