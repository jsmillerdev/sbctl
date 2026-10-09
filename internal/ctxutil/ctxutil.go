// Package ctxutil holds the context helpers the packages share.
package ctxutil

import (
	"context"
	"time"
)

// Sleep waits for d or until ctx is done, whichever is first, and returns ctx.Err() in the second
// case. The timer is stopped on the way out, so an early return leaves nothing behind.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
