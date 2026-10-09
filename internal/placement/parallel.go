package placement

import (
	"context"
	"sync"
)

// DefaultConcurrency is how many replicas a node starts or observes at once, and how many projects it
// probes at once for its report. With [replicas] default = "all" a follower holds a replica of every
// project: one after the other, a reboot would wait for each start (up to ten minutes) in turn and a
// report would wait for each probe (several seconds).
const DefaultConcurrency = 4

// EachLimit runs fn for every item, at most limit at a time, and returns when all have returned. It
// starts no more once ctx has ended.
func EachLimit[T any](ctx context.Context, limit int, items []T, fn func(T)) {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(it)
		}()
	}
	wg.Wait()
}
