// Package metrics 提供固定桶、无请求内容的进程内性能计数。
package metrics

import (
	"sync"
	"time"
)

var boundsMS = [...]int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000}

type Operation struct {
	mu      sync.Mutex
	active  int64
	count   int64
	failed  int64
	total   time.Duration
	max     time.Duration
	buckets [len(boundsMS) + 1]int64
}

type Bucket struct {
	UpperMS int64 `json:"upperMs"`
	Count   int64 `json:"count"`
}

type Snapshot struct {
	Active    int64    `json:"active"`
	Count     int64    `json:"count"`
	Failed    int64    `json:"failed"`
	AverageMS float64  `json:"averageMs"`
	MaxMS     float64  `json:"maxMs"`
	Buckets   []Bucket `json:"buckets"`
}

// Start 返回必须调用一次的结束函数；只记录耗时和失败数量。
func (o *Operation) Start() func(error) {
	start := time.Now()
	o.mu.Lock()
	o.active++
	o.mu.Unlock()
	return func(err error) {
		elapsed := time.Since(start)
		o.mu.Lock()
		defer o.mu.Unlock()
		o.active--
		o.count++
		o.total += elapsed
		if err != nil {
			o.failed++
		}
		if elapsed > o.max {
			o.max = elapsed
		}
		index := len(boundsMS)
		for i, bound := range boundsMS {
			if elapsed <= time.Duration(bound)*time.Millisecond {
				index = i
				break
			}
		}
		o.buckets[index]++
	}
}

// Snapshot 中 upperMs=-1 是溢出桶，其余是各区间计数，不是累加值。
func (o *Operation) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := Snapshot{Active: o.active, Count: o.count, Failed: o.failed, MaxMS: float64(o.max) / float64(time.Millisecond)}
	if o.count > 0 {
		out.AverageMS = float64(o.total) / float64(time.Millisecond) / float64(o.count)
	}
	for i, count := range o.buckets {
		bound := int64(-1)
		if i < len(boundsMS) {
			bound = boundsMS[i]
		}
		out.Buckets = append(out.Buckets, Bucket{UpperMS: bound, Count: count})
	}
	return out
}
