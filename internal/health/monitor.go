package health

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Monitor shares one health check between everyone who asks: the public /healthz, the
// operator's detailed endpoint and the alert checker. A report younger than TTL is reused, and
// callers that arrive while a check runs wait for that one instead of starting another, so
// anyone who can reach /healthz cannot make the node probe its fifty projects fifty times.
type Monitor struct {
	check func(ctx context.Context) (*Report, error)
	ttl   time.Duration
	now   func() time.Time

	flight singleflight.Group
	mu     sync.Mutex
	last   *Report
	lastAt time.Time
}

// NewMonitor returns a Monitor over check. A ttl of zero or less reuses nothing.
func NewMonitor(check func(ctx context.Context) (*Report, error), ttl time.Duration) *Monitor {
	return &Monitor{check: check, ttl: ttl, now: time.Now}
}

// Report returns the cached report when it is younger than the TTL, otherwise a fresh one. A
// check that fails or outlasts ctx returns the last report when there is one, and the error
// otherwise.
func (m *Monitor) Report(ctx context.Context) (*Report, error) {
	m.mu.Lock()
	last, at := m.last, m.lastAt
	m.mu.Unlock()
	if last != nil && m.ttl > 0 && m.now().Sub(at) < m.ttl {
		return last, nil
	}
	return m.Fresh(ctx)
}

// Fresh runs a check now (joining one that is already running) and caches the result.
func (m *Monitor) Fresh(ctx context.Context) (*Report, error) {
	ch := m.flight.DoChan("check", func() (any, error) {
		// The run belongs to every waiter, not to the one that started it: it ends on its own
		// budget, and a caller that gives up does not cancel it for the others.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		r, err := m.check(rctx)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.last, m.lastAt = r, m.now()
		m.mu.Unlock()
		return r, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return m.fallback(res.Err)
		}
		return res.Val.(*Report), nil
	case <-ctx.Done():
		return m.fallback(ctx.Err())
	}
}

func (m *Monitor) fallback(err error) (*Report, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// An old answer is worse than none: a check that has been failing for minutes must not
	// leave /healthz saying what it said before.
	if m.last != nil && m.now().Sub(m.lastAt) < maxStale {
		return m.last, nil
	}
	return nil, err
}

// maxStale is how old a report may be and still stand in for a check that failed or timed out.
const maxStale = 2 * time.Minute
