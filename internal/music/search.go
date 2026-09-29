package music

import (
	"context"
	"encoding/json"
	"errors"
	"lxsc/internal/admission"
	"strings"
	"time"
)

// SearchOptions 搜索参数
type SearchOptions struct {
	Sources []string
	Page    int
	Limit   int
}

type searchKey struct {
	query   string
	sources string
	page    int
	limit   int
}

// Search 聚合搜索：并发请求各平台，按平台顺序交错合并。
func (c *Catalog) Search(ctx context.Context, query string, opts SearchOptions) []*Info {
	list, _ := c.SearchChecked(ctx, query, opts)
	return list
}

// SearchChecked 让 HTTP 调用方区分没有结果与所有平台都因过载被拒绝。
func (c *Catalog) SearchChecked(ctx context.Context, query string, opts SearchOptions) ([]*Info, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}
	if opts.Limit <= 0 {
		opts.Limit = c.Settings.Get().SearchLimit
	}
	if len(opts.Sources) == 0 {
		opts.Sources = c.Settings.Get().SearchSources
	}
	opts.Sources = append([]string(nil), opts.Sources...)
	lists := make(map[string][]*Info, len(opts.Sources))
	var busy, succeeded bool
	c.SearchProgress(ctx, query, opts, func(result SearchPlatformResult) {
		lists[result.Source] = result.Tracks
		busy = busy || errors.Is(result.Err, admission.ErrBusy)
		succeeded = succeeded || result.Err == nil
	})
	var merged []*Info
	for index := 0; ; index++ {
		added := false
		for _, source := range opts.Sources {
			if list := lists[source]; index < len(list) {
				merged = append(merged, list[index])
				added = true
			}
		}
		if !added {
			break
		}
	}
	if busy && !succeeded {
		return merged, admission.ErrBusy
	}
	return merged, nil
}

// SearchPlatformResult 表示一个平台的完整结果；失败不会伪装成空结果。
type SearchPlatformResult struct {
	Source   string
	Tracks   []*Info
	Err      error
	Cached   bool
	Duration time.Duration
}

// SearchProgress 串行通知调用方，成功平台单独缓存并共享在途请求。
// 调用方取消或到达预算时立即交付已完成结果，最后一个等待者退出后取消上游。
func (c *Catalog) SearchProgress(ctx context.Context, query string, opts SearchOptions, notify func(SearchPlatformResult)) {
	query = strings.TrimSpace(query)
	if query == "" || ctx.Err() != nil {
		return
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}
	if opts.Limit <= 0 {
		opts.Limit = c.Settings.Get().SearchLimit
	}
	if len(opts.Sources) == 0 {
		opts.Sources = c.Settings.Get().SearchSources
	}
	sources := make([]string, 0, len(opts.Sources))
	seen := make(map[string]bool)
	for _, source := range opts.Sources {
		if IsPlatform(source) && !seen[source] {
			sources = append(sources, source)
			seen[source] = true
		}
	}
	ch := make(chan SearchPlatformResult, len(sources))
	for _, source := range sources {
		go func(s string) {
			started := time.Now()
			finish := c.searchMetrics[s].Start()
			key := searchKey{query: query, sources: s, page: opts.Page, limit: opts.Limit}
			result, err := c.search.load(ctx, key, nil, 15*time.Second, func(cctx context.Context) ([]*Info, bool, error) {
				var out struct {
					List []map[string]any `json:"list"`
				}
				raw, err := c.searchCall(cctx, s+".musicSearch.search", query, opts.Page, opts.Limit)
				if err == nil {
					err = json.Unmarshal(raw, &out)
				}
				if err != nil {
					if c.Log != nil {
						c.Log.Debug("搜索失败", "source", s, "err", err)
					}
					return nil, false, err
				}
				list := make([]*Info, 0, len(out.List))
				for _, m := range out.List {
					if m == nil {
						continue
					}
					if _, ok := m["source"]; !ok {
						m["source"] = s
					}
					list = append(list, FromMap(m))
				}
				return list, len(list) > 0, nil
			})
			finish(err)
			ch <- SearchPlatformResult{Source: s, Tracks: result.value, Err: err, Cached: result.cached, Duration: time.Since(started)}
		}(source)
	}
	deliver := func(result SearchPlatformResult) {
		delete(seen, result.Source)
		if result.Err == nil {
			c.Cache(result.Tracks)
			c.rememberArtistRefs(result.Tracks, true)
		}
		if notify != nil {
			notify(result)
		}
	}
	for len(seen) > 0 {
		select {
		case result := <-ch:
			deliver(result)
		case <-ctx.Done():
			// 同时就绪的成功结果不能被超时分支丢弃。
			for {
				select {
				case result := <-ch:
					deliver(result)
				default:
					for _, source := range sources {
						if seen[source] {
							deliver(SearchPlatformResult{Source: source, Err: ctx.Err()})
						}
					}
					return
				}
			}
		}
	}
}
