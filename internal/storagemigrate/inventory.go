package storagemigrate

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"
)

// fileState is what a pass knows about one file: enough to see that it changed.
type fileState struct {
	Path  string
	Size  int64
	MTime int64 // nanoseconds since the epoch
}

// inventory lists the files of one project, sorted by path (byte order, the order of an S3
// listing). Memory is about 100 bytes a file.
type inventory []fileState

func (inv inventory) find(path string) (fileState, bool) {
	i := sort.Search(len(inv), func(i int) bool { return inv[i].Path >= path })
	if i < len(inv) && inv[i].Path == path {
		return inv[i], true
	}
	return fileState{}, false
}

func (inv inventory) totals() (n int, bytes int64) {
	for _, f := range inv {
		bytes += f.Size
	}
	return len(inv), bytes
}

// diff compares what a pass knew with what it sees now. changed has the files that are new or
// differ in size or modification time, gone the paths that disappeared.
func diff(prev, cur inventory) (changed []fileState, gone []string) {
	i, j := 0, 0
	for i < len(prev) || j < len(cur) {
		switch {
		case j == len(cur) || (i < len(prev) && prev[i].Path < cur[j].Path):
			gone = append(gone, prev[i].Path)
			i++
		case i == len(prev) || cur[j].Path < prev[i].Path:
			changed = append(changed, cur[j])
			j++
		default:
			if prev[i].Size != cur[j].Size || prev[i].MTime != cur[j].MTime {
				changed = append(changed, cur[j])
			}
			i++
			j++
		}
	}
	return changed, gone
}

// baselineSlack is how much older than a file the bucket's copy must be before the copy is
// believed to be of its current content when the bucket's clock and the node's may differ.
const baselineSlack = 2 * time.Second

// diffBucket compares the files with a listing of the bucket's copy (prefix already removed from
// the keys), for a pass that has no earlier pass to compare with: a copy that was cut short and is
// continued. A file is current when the bucket has it at the same size, uploaded after the file was
// last written. Keys the files do not have are left alone: without an earlier pass there is no way
// to tell a key this run sent from one that belongs to something else.
func diffBucket(cur inventory, listed []Entry, prefix string) (changed []fileState) {
	i := 0
	for _, f := range cur {
		for i < len(listed) && listed[i].Key[len(prefix):] < f.Path {
			i++
		}
		if i < len(listed) && listed[i].Key[len(prefix):] == f.Path {
			en := listed[i]
			if en.Size == f.Size && !en.ModTime.Before(time.Unix(0, f.MTime).Add(baselineSlack)) {
				continue
			}
		}
		changed = append(changed, f)
	}
	return changed
}

// limiter paces the copy: after it lets n bytes through, the next caller waits until n bytes of
// the rate have gone by. An object bigger than a second of the rate is sent at full speed and the
// wait comes after it, so the average is right without a burst size to tune.
type limiter struct {
	mu    sync.Mutex
	rate  float64 // bytes per second; zero means no limit
	free  time.Time
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func (l *limiter) wait(ctx context.Context, n int64) error {
	if l == nil || l.rate <= 0 || n <= 0 {
		return nil
	}
	l.mu.Lock()
	now := l.now()
	start := l.free
	if start.Before(now) {
		start = now
	}
	l.free = start.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	l.mu.Unlock()
	if d := start.Sub(now); d > 0 {
		return l.sleep(ctx, d)
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func quote(s string) string { return strconv.QuoteToGraphic(s) }
