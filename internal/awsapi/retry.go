package awsapi

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	defaultMaxAttempts  = 5
	defaultIMDSAttempts = 3
	defaultRetryBackoff = 200 * time.Millisecond
	// maxRetryBackoff is the longest a wait grows to by doubling. A larger Config.RetryBackoff is
	// the longest wait instead.
	maxRetryBackoff = 5 * time.Second
)

// retrier decides how long to wait between the sends of one request.
type retrier struct {
	attempts int
	backoff  time.Duration
	jitter   func() float64 // a number in [0, 1)
	sleep    func(context.Context, time.Duration) error
}

func newRetrier(attempts int, backoff time.Duration) *retrier {
	return &retrier{attempts: attempts, backoff: backoff, jitter: rand.Float64, sleep: sleepContext}
}

// delay is the wait after the nth failed attempt, counting from 1. The schedule starts at the
// base and doubles with each failure up to maxRetryBackoff; the wait is a random point in the
// upper half of it. Clients that failed together then do not retry together, and a wait is never
// shorter than half of its schedule.
func (r *retrier) delay(n int) time.Duration {
	limit := max(maxRetryBackoff, r.backoff)
	d := r.backoff
	for i := 1; i < n && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	return d/2 + time.Duration(r.jitter()*float64(d/2))
}

// wait sleeps for the delay after the nth failed attempt. It returns the context's error when the
// context ends first.
func (r *retrier) wait(ctx context.Context, n int) error { return r.sleep(ctx, r.delay(n)) }

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
