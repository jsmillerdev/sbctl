package api

import (
	"strconv"
	"sync"
	"time"
)

// Per-address limits of the OAuth endpoints (design 2.14). They are in memory and per node, in the
// way of the limiter of POST /claim, so a cluster of n nodes lets n times as many requests
// through. Behind them stand the caps of internal/oauth (stored dynamic apps, pending
// authorizations), which count rows and so hold across nodes.
const (
	// oauthRegisterLimit registrations per oauthRegisterWindow per client address. Claude Code
	// registers again on every connect, so the limit is generous.
	oauthRegisterLimit  = 100
	oauthRegisterWindow = 10 * time.Minute
	// oauthAuthorizeLimit authorization requests per oauthAuthorizeWindow per client address.
	oauthAuthorizeLimit  = 60
	oauthAuthorizeWindow = time.Minute
	// oauthFailLimit failed requests per oauthFailWindow per client address, counted together for
	// the token and revoke endpoints, so that guessing a client secret or a code through one
	// endpoint spends the budget of the other.
	oauthFailLimit  = 30
	oauthFailWindow = time.Minute

	// oauthLimiterMaxKeys bounds the memory of one limiter. Windows that ended are dropped first;
	// when every slot is a live window an address nobody has seen yet is not limited (see slot).
	oauthLimiterMaxKeys = 4096
)

// windowLimiter counts events per key in fixed windows that start at a key's first event.
type windowLimiter struct {
	limit  int
	window time.Duration

	mu   sync.Mutex
	keys map[string]*limitWindow
}

type limitWindow struct {
	start time.Time
	n     int
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window}
}

// slot returns the live window of key, starting a new one when the old one ended. The caller holds
// mu. When the table is full of live windows, an unknown key gets a window that is not stored: its
// events count for nothing. A shared overflow bucket would let a flood of junk addresses lock out
// everybody else, and the caps behind the limiters hold either way.
func (l *windowLimiter) slot(key string, now time.Time) *limitWindow {
	if l.keys == nil {
		l.keys = map[string]*limitWindow{}
	}
	w := l.keys[key]
	if w != nil && now.Sub(w.start) < l.window {
		return w
	}
	if w == nil && len(l.keys) >= oauthLimiterMaxKeys {
		for k, o := range l.keys {
			if now.Sub(o.start) >= l.window {
				delete(l.keys, k)
			}
		}
		if len(l.keys) >= oauthLimiterMaxKeys {
			return &limitWindow{start: now}
		}
	}
	w = &limitWindow{start: now}
	l.keys[key] = w
	return w
}

// retryAfter is how long until the window of w ends, at least one second.
func (l *windowLimiter) retryAfter(w *limitWindow, now time.Time) time.Duration {
	if d := w.start.Add(l.window).Sub(now); d > time.Second {
		return d
	}
	return time.Second
}

// allow counts one event of key. ok is false when the event is over the limit; retry is then how
// long until the window ends. An event over the limit is not counted again.
func (l *windowLimiter) allow(key string, now time.Time) (retry time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.slot(key, now)
	if w.n >= l.limit {
		return l.retryAfter(w, now), false
	}
	w.n++
	return 0, true
}

// blocked reports whether key has used up its limit, without counting an event.
func (l *windowLimiter) blocked(key string, now time.Time) (retry time.Duration, blocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.slot(key, now)
	if w.n >= l.limit {
		return l.retryAfter(w, now), true
	}
	return 0, false
}

// fail counts one failure of key.
func (l *windowLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.slot(key, now).n++
}

// oauthLimits are the limiters of the OAuth endpoints of one server.
type oauthLimits struct {
	register  *windowLimiter
	authorize *windowLimiter
	// failures is shared by the token and revoke endpoints.
	failures *windowLimiter
}

func newOAuthLimits() *oauthLimits {
	return &oauthLimits{
		register:  newWindowLimiter(oauthRegisterLimit, oauthRegisterWindow),
		authorize: newWindowLimiter(oauthAuthorizeLimit, oauthAuthorizeWindow),
		failures:  newWindowLimiter(oauthFailLimit, oauthFailWindow),
	}
}

// retryAfterSeconds is the value of a Retry-After header for d, rounded up.
func retryAfterSeconds(d time.Duration) string {
	n := int((d + time.Second - 1) / time.Second)
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}
