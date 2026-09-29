package music

import (
	"lxsc/internal/js"
	"lxsc/internal/metrics"
)

type CacheStats struct {
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	Shared   uint64 `json:"shared"`
	Entries  int    `json:"entries"`
	InFlight int    `json:"inFlight"`
}

func (c *requestCache[K, V]) stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{c.hits, c.misses, c.shared, c.entries.Len(), len(c.calls)}
}

// Performance 只按有限的平台维度统计，不记录关键词、用户或播放地址。
func (c *Catalog) Performance() map[string]any {
	search := make(map[string]metrics.Snapshot, len(c.searchMetrics))
	for source, operation := range c.searchMetrics {
		search[source] = operation.Snapshot()
	}
	out := map[string]any{"search": search, "urlResolve": c.urlMetrics.Snapshot(), "mediaPreparation": c.MediaPreparation.Snapshot(), "urlCache": c.urls.stats(), "searchCache": c.search.stats()}
	out["admission"] = map[string]any{"requests": c.RequestLimits.Stats(), "media": c.MediaLimits.Stats(), "upstream": c.workLimits.Stats(), "javascript": js.WorkloadStats()}
	out["urlPersistence"] = c.urls.persistenceTiming.Snapshot()
	if c.SDK != nil {
		out["sdk"] = c.SDK.Performance()
	}
	return out
}
