// Package alerts tells the operator when the node needs them: a disk running low, a backup
// that failed or went stale, a project that does not answer, a certificate that will not
// renew, a release to install. It sends JSON webhooks (optionally signed) and email through the
// node's [mail] relay, de-duplicates what is still wrong, caps the notifications per hour,
// and runs a checker in the daemon (Checker) that turns health reports into events.
//
// Other parts of the node call Notify when something they own happens (an upgrade started,
// succeeded or failed). Every event is also logged, under the message key "alert", whether or
// not a destination is configured.
package alerts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// Options configure a Notifier.
type Options struct {
	Log *slog.Logger
	// HTTP sends the webhooks; nil means a client with a 15 second timeout.
	HTTP *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Mailer replaces the SMTP delivery (tests).
	Mailer func(ctx context.Context, to []string, subject, text string) error
}

// Notifier delivers events. It is safe for concurrent use, and several processes of one node
// (the daemon and a CLI run) can share it through the state file.
type Notifier struct {
	cfg    config.Alerts
	mail   config.Mail
	node   string
	log    *slog.Logger
	http   *http.Client
	now    func() time.Time
	st     *store
	logMu  sync.Mutex
	logged map[string]time.Time // when each condition was last logged
	mailer func(ctx context.Context, to []string, subject, text string) error
	wg     sync.WaitGroup // the deliveries NotifyDetached has in flight
}

// New returns a Notifier for the node cfg describes.
func New(cfg *config.Config, o Options) *Notifier {
	n := &Notifier{cfg: cfg.Alerts, mail: cfg.Mail, node: cfg.BaseDomain(), log: o.Log, http: o.HTTP, now: o.Now, mailer: o.Mailer,
		st: newStore(cfg.StateDir)}
	if n.node == "" {
		n.node = "supavise"
	}
	if n.log == nil {
		n.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if n.http == nil {
		n.http = &http.Client{Timeout: 15 * time.Second}
	}
	if n.now == nil {
		n.now = time.Now
	}
	return n
}

// Configured reports whether any destination exists.
func (n *Notifier) Configured() bool { return len(n.cfg.Webhooks) > 0 || len(n.cfg.Recipients()) > 0 }

var defaultNotifier atomic.Pointer[Notifier]

// SetDefault sets the Notifier that the package-level Notify uses.
func SetDefault(n *Notifier) { defaultNotifier.Store(n) }

// Configure builds a Notifier for cfg and makes it the default. A command that raises events
// (supavise upgrade) calls it once after loading the config.
func Configure(cfg *config.Config, log *slog.Logger) *Notifier {
	n := New(cfg, Options{Log: log})
	SetDefault(n)
	return n
}

// Notify sends ev through the default Notifier. Without one (nothing called SetDefault or
// Configure) it does nothing and returns nil: raising an event never fails the work that raised it.
func Notify(ctx context.Context, ev Event) error {
	if n := defaultNotifier.Load(); n != nil {
		return n.Notify(ctx, ev)
	}
	return nil
}

// detachedTimeout bounds one NotifyBounded or NotifyDetached delivery: a webhook is retried once
// after two seconds and SMTP can take 40 seconds. A caller does not wait longer than this for an
// alert, and a lost one is logged, never an error of the work that raised it.
const detachedTimeout = 90 * time.Second

// detach is a context that does not end with ctx and ends after detachedTimeout: the request or
// the SIGTERM that is stopping the work does not also drop the message that tells the operator
// about it.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), detachedTimeout)
}

// NotifyBounded is Notify on a context that outlives ctx's cancellation and ends after 90 seconds.
// It returns what Notify returns.
func (n *Notifier) NotifyBounded(ctx context.Context, ev Event) error {
	ctx, cancel := detach(ctx)
	defer cancel()
	return n.Notify(ctx, ev)
}

// NotifyDetached delivers ev on a goroutine of its own, on NotifyBounded's kind of context, so that
// a slow webhook does not hold up the request or the upgrade that raised the event. A failure to
// send is logged ("could not send the alert") and never reaches the caller. Drain waits for the
// deliveries still in flight.
func (n *Notifier) NotifyDetached(ctx context.Context, ev Event) {
	n.NotifyDetachedFunc(ctx, ev, func(err error) {
		n.log.Warn("could not send the alert", "kind", ev.Kind, "ref", ev.Ref, "error", err)
	})
}

// NotifyDetachedFunc is NotifyDetached for a caller that reports a failure to send in its own
// words: failed is called, on the delivery's goroutine, with the error Notify returned.
func (n *Notifier) NotifyDetachedFunc(ctx context.Context, ev Event, failed func(error)) {
	ctx, cancel := detach(ctx)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer cancel()
		if err := n.Notify(ctx, ev); err != nil {
			failed(err)
		}
	}()
}

// Drain lets the deliveries of NotifyDetached that are in flight finish, up to 90 seconds, which is
// what the daemon does when it stops.
func (n *Notifier) Drain() {
	done := make(chan struct{})
	go func() { n.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(detachedTimeout):
	}
}

// Notify logs ev and sends it to every destination unless it is a duplicate of a problem
// that was sent less than [alerts] repeat_hours ago, falls below min_severity, or the hourly cap
// is reached. A delivery that fails everywhere is returned, and not recorded, so the next try
// sends it. A resolved event is sent only when the problem was.
func (n *Notifier) Notify(ctx context.Context, ev Event) error {
	if ev.Kind == "" {
		return errors.New("alerts: an event needs a Kind")
	}
	if ev.Time.IsZero() {
		ev.Time = n.now()
	}
	ev.Severity = ev.severity()
	if n.worthLogging(ev) {
		n.log.Warn("alert", "kind", ev.Kind, "severity", ev.Severity, "ref", ev.Ref, "title", ev.Title,
			"detail", ev.Detail, "resolved", ev.Resolved)
	}
	if sev, _ := config.SeverityRank(ev.Severity); sev < n.cfg.Severity() || !n.Configured() {
		return nil
	}
	// The state is locked while the decision is made and recorded, and not while the message is
	// delivered, which can take 40 seconds: a slow webhook must not block the daemon's checker and
	// an upgrade's events on each other. The decision therefore reserves the notification (it is
	// recorded as sent), so a second process that asks meanwhile sees a duplicate; a delivery that
	// fails everywhere gives the reservation back.
	var (
		res  reservation
		send bool
	)
	err := n.st.update(func(s *state) error {
		now := n.now()
		key := ev.key()
		prev, active := s.Active[key]
		switch {
		case ev.Resolved && !active:
			return errSkip
		case !ev.Resolved && !oneShot(ev.Kind) && active && now.Sub(prev.LastSent) < n.cfg.Repeat():
			return errSkip
		}
		// The hourly cap. A critical alert, the upgrade's own events, the failover announcements and update_available are
		// not held back: a burst of conditions must not swallow the one message that says an upgrade failed.
		cutoff := now.Add(-time.Hour)
		kept := s.Sent[:0]
		for _, t := range s.Sent {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		s.Sent = kept
		if len(s.Sent) >= n.cfg.HourlyCap() && !exemptFromCap(ev) {
			if !slices.Contains(s.Held, key) { // saved: the next notification that goes out says how many were held back
				s.Held = append(s.Held, key)
			}
			n.log.Warn("alert held back by the hourly cap", "kind", ev.Kind, "ref", ev.Ref, "cap", n.cfg.HourlyCap())
			return nil
		}
		out := ev
		if len(s.Held) > 0 {
			out.Detail = strings.TrimSpace(out.Detail + fmt.Sprintf("\n(%d earlier alert(s) were held back by the limit of %d per hour; see the daemon's log.)", len(s.Held), n.cfg.HourlyCap()))
		}
		res = reservation{out: out, key: key, prev: prev, active: active, held: slices.Clone(s.Held), at: now}
		send = true
		s.Sent = append(s.Sent, now)
		s.Held = nil
		switch {
		case ev.Resolved:
			delete(s.Active, key)
		case !oneShot(ev.Kind):
			a := ActiveAlert{Kind: ev.Kind, Severity: ev.Severity, Ref: ev.Ref, Title: ev.Title, Detail: ev.Detail, FirstSent: now, LastSent: now}
			if active {
				a.FirstSent = prev.FirstSent
			}
			s.Active[key] = a
		}
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	if err != nil || !send {
		return err
	}
	var failed []error
	ok := 0
	for _, r := range n.deliver(ctx, res.out) {
		if r.Err != nil {
			n.log.Warn("alert delivery failed", "destination", r.Destination, "kind", ev.Kind, "error", r.Err)
			failed = append(failed, fmt.Errorf("%s: %w", r.Destination, r.Err))
			continue
		}
		ok++
	}
	if ok > 0 {
		return nil
	}
	// Nothing arrived: nothing stays recorded, so the next try sends it.
	rerr := n.st.update(func(s *state) error {
		res.release(s)
		return nil
	})
	return errors.Join(append(failed, rerr)...)
}

// reservation is what a decision to send recorded in the state, kept to undo it.
type reservation struct {
	out    Event
	key    string
	prev   ActiveAlert
	active bool
	held   []string
	at     time.Time
}

// release removes the reservation from s: the send time, the active entry (restored to what it
// was) and the held keys (merged with any that others added since).
func (r reservation) release(s *state) {
	if i := slices.IndexFunc(s.Sent, r.at.Equal); i >= 0 {
		s.Sent = slices.Delete(s.Sent, i, i+1)
	}
	if r.active {
		s.Active[r.key] = r.prev
	} else {
		delete(s.Active, r.key)
	}
	for _, k := range r.held {
		if !slices.Contains(s.Held, k) {
			s.Held = append(s.Held, k)
		}
	}
}

// exemptFromCap reports whether ev is sent even when the hourly cap is reached. update_available
// is one message per version and the checker records the version as told once it is sent, so a
// held-back notice would never come again. The three failover announcements are one-shot like the
// upgrade ones: each says what a move that cannot be repeated just did, and a burst of conditions
// must not swallow the message that a failover started, finished or stopped. The other
// failover_* kinds are conditions and stay under the cap.
func exemptFromCap(ev Event) bool {
	switch ev.Kind {
	case KindFailoverStarted, KindFailoverCompleted, KindFailoverFailed, KindUpdateAvailable:
		return true
	}
	return ev.Severity == SeverityCritical || strings.HasPrefix(ev.Kind, "upgrade_")
}

// worthLogging reports whether ev is news for the log. The checker offers every standing
// condition at every check so that reminders and retries happen; logging each offer would write
// the same line fifty times a minute. A condition is logged once per repeat interval, a
// recovery and an announcement every time.
func (n *Notifier) worthLogging(ev Event) bool {
	if ev.Resolved || oneShot(ev.Kind) {
		n.logMu.Lock()
		delete(n.logged, ev.key())
		n.logMu.Unlock()
		return true
	}
	now, key := n.now(), ev.key()
	n.logMu.Lock()
	defer n.logMu.Unlock()
	if at, ok := n.logged[key]; ok && now.Sub(at) < n.cfg.Repeat() {
		return false
	}
	if n.logged == nil {
		n.logged = map[string]time.Time{}
	}
	n.logged[key] = now
	return true
}

// errSkip ends a state update without saving: nothing was sent, nothing changed.
var errSkip = errors.New("alerts: duplicate")

// Active returns the problems that were sent and are not resolved, by key.
func (n *Notifier) Active() (map[string]ActiveAlert, error) {
	s, err := n.st.snapshot()
	if err != nil {
		return nil, err
	}
	return s.Active, nil
}

// Forget drops an active alert without sending anything. The checker uses it when one alert
// stands down in favor of others that say the same thing (a group of unhealthy projects that
// shrinks to single alerts), where a recovery message would be false.
func (n *Notifier) Forget(key string) error {
	err := n.st.update(func(s *state) error {
		if _, ok := s.Active[key]; !ok {
			return errSkip
		}
		delete(s.Active, key)
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	return err
}

// Test sends a test alert to every destination, ignoring de-duplication and the hourly cap,
// and returns one Result per destination (empty when none is configured).
func (n *Notifier) Test(ctx context.Context) ([]Result, error) {
	if !n.Configured() {
		return nil, errNothingConfigured
	}
	ev := Event{
		Kind: KindTest, Severity: SeverityInfo, Time: n.now(),
		Title:  "Test alert from " + n.node,
		Detail: "This is a test from `supavise alerts test`. If you can read it, alerts reach you.",
	}
	return n.deliver(ctx, ev), nil
}
