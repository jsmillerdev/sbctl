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

	"github.com/jsmillerdev/supavise/internal/config"
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
		// The hourly cap.
		cutoff := now.Add(-time.Hour)
		kept := s.Sent[:0]
		for _, t := range s.Sent {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		s.Sent = kept
		if len(s.Sent) >= n.cfg.HourlyCap() {
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
		results := n.deliver(ctx, out)
		var failed []error
		ok := 0
		for _, r := range results {
			if r.Err != nil {
				n.log.Warn("alert delivery failed", "destination", r.Destination, "kind", ev.Kind, "error", r.Err)
				failed = append(failed, fmt.Errorf("%s: %w", r.Destination, r.Err))
				continue
			}
			ok++
		}
		if ok == 0 {
			return errors.Join(failed...) // nothing is recorded: the next try sends it
		}
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
	return err
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
