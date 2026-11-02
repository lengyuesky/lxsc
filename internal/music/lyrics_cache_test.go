package music

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLyricSharedWorkSurvivesOneCancelledWaiter(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	in := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "synthetic"})
	started, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	c.customHTTP = &http.Client{Transport: customLyricTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- r.Context()
		select {
		case <-release:
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[00:01.00]shared lyric"))}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := c.Lyric(ctx, in); first <- err }()
	workCtx := <-started
	go func() { _, err := c.Lyric(context.Background(), in); second <- err }()
	waitCacheWaiters(t, c.lyrics, 2)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) || workCtx.Err() != nil {
		t.Fatal("one metadata waiter cancelled another lyric request", err)
	}
	unblock()
	if err := <-second; err != nil || calls.Load() != 1 {
		t.Fatal("shared lyric work did not finish once", err)
	}
	if lyrics, ok := c.CachedLyric(in); !ok || lyrics.Lines[0].Value != "shared lyric" {
		t.Fatal("successful shared lyrics were not cached")
	}
}

func TestLyricAvailabilityExpiresWithoutStartingRequests(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	in := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "synthetic"})
	var calls atomic.Int32
	c.customHTTP = &http.Client{Transport: customLyricTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[00:01.00]cached lyric"))}, nil
	})}
	if _, err := c.Lyric(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.CachedLyric(in); !ok {
		t.Fatal("usable cached lyric was not observed")
	}
	c.lyrics.mu.Lock()
	c.lyrics.now = func() time.Time { return time.Now().Add(7 * time.Hour) }
	c.lyrics.mu.Unlock()
	if _, ok := c.CachedLyric(in); ok || calls.Load() != 1 {
		t.Fatal("availability lookup reused expired data or fetched upstream")
	}
	if _, err := c.Lyric(context.Background(), in); err != nil || calls.Load() != 2 {
		t.Fatal("expired lyrics could not be fetched again", err)
	}
}

func TestLateLyricResponseCannotReplaceChangedFallback(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	in := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "synthetic"})
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c.customHTTP = &http.Client{Transport: customLyricTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.lrc.cx" {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("[00:01.00]" + r.URL.Host))}, nil
	})}
	old := make(chan error, 1)
	go func() { _, err := c.Lyric(context.Background(), in); old <- err }()
	<-started
	if _, err := c.Settings.Update(context.Background(), map[string]json.RawMessage{"customLyricsURL": json.RawMessage(`"https://changed.example/lyrics"`)}); err != nil {
		t.Fatal(err)
	}
	if lyrics, err := c.Lyric(context.Background(), in); err != nil || lyrics.Lines[0].Value != "changed.example" {
		t.Fatal("new fallback joined an old in-flight request", err)
	}
	unblock()
	if err := <-old; err != nil {
		t.Fatal(err)
	}
	if lyrics, ok := c.CachedLyric(in); !ok || lyrics.Lines[0].Value != "changed.example" {
		t.Fatal("late old response polluted current availability")
	}
}
