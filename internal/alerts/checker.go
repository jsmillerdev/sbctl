package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/health"
	"github.com/jsmillerdev/supavise/internal/notice"
)

// Checker is the daemon's periodic look at the node. Every [alerts] check_interval_seconds it
// takes a health report and turns it into conditions: the disk is low, a project's backup failed
// or went stale, a project or a shared service does not answer, a certificate is close to
// expiring. A condition that has lasted [alerts] unhealthy_after_seconds is sent (a restart or
// an upgrade must not page anyone), and one that clears is sent as resolved. Once a day it also
// asks for the newest release and raises update_available once per version.
//
// While an upgrade runs or a maintenance window is open the checker raises and resolves
// nothing: the operator caused that downtime, and the upgrade reports its own events.
type Checker struct {
	Notifier *Notifier
	// Report returns the current health of the node (health.Monitor.Fresh).
	Report func(ctx context.Context) (*health.Report, error)
	Cfg    *config.Config
	// CheckUpdate asks for the newest release and records it (health.CheckUpdate). Nil turns
	// the update check off.
	CheckUpdate func(ctx context.Context, now time.Time) (*health.UpdateRecord, error)
	// UpdateInterval is the pause between update checks; zero means 24 hours.
	UpdateInterval time.Duration
	Log            *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// firstSeen is when each condition was first observed in a row.
	firstSeen map[string]time.Time
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

// startupDelay lets projects start before the first check.
const startupDelay = 30 * time.Second

// Run checks until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	wait := startupDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		c.Once(ctx)
		wait = c.Cfg.Alerts.Check()
	}
}

// condition is one thing that is wrong now.
type condition struct {
	key, kind, severity, ref, title, detail string
}

// owned are the kinds of condition the checker raises and resolves; the other kinds in the
// notifier's state belong to whoever raised them.
func owned(kind string) bool {
	switch kind {
	case KindDiskLow, KindBackupFailed, KindProjectUnhealthy, KindCertificateExpiring, KindNodeUnhealthy:
		return true
	}
	return false
}

// Once runs one check: conditions first, then the update check when it is due.
func (c *Checker) Once(ctx context.Context) {
	now := c.now()
	c.checkUpdate(ctx, now)

	paths := c.Cfg.Paths()
	if _, running := notice.UpgradeRunning(paths, now); running {
		c.firstSeen = nil
		return
	}
	if m, err := notice.ReadMaintenance(paths); err == nil && m != nil && m.InProgress(now) {
		c.firstSeen = nil
		return
	}
	rep, err := c.Report(ctx)
	if err != nil {
		c.log().Warn("alert check: no health report", "error", err)
		return
	}
	conds := conditionsOf(rep)
	debounce := c.Cfg.Alerts.Debounce()
	if c.firstSeen == nil {
		c.firstSeen = map[string]time.Time{}
	}
	current := map[string]bool{}
	for _, cd := range conds {
		current[cd.key] = true
		first, seen := c.firstSeen[cd.key]
		if !seen {
			first = now
			c.firstSeen[cd.key] = now
		}
		if now.Sub(first) < debounce {
			continue
		}
		// Notify sends it once and again only after the repeat interval.
		if err := c.Notifier.Notify(ctx, Event{Kind: cd.kind, Severity: cd.severity, Ref: cd.ref, Key: cd.key, Title: cd.title, Detail: cd.detail}); err != nil {
			c.log().Warn("alert not delivered; it is tried again at the next check", "kind", cd.kind, "ref", cd.ref, "error", err)
		}
	}
	for k := range c.firstSeen {
		if !current[k] {
			delete(c.firstSeen, k)
		}
	}
	active, err := c.Notifier.Active()
	if err != nil {
		c.log().Warn("alert check: cannot read the alert state", "error", err)
		return
	}
	for key, a := range active {
		if !owned(a.Kind) || current[key] {
			continue
		}
		ev := Event{Kind: a.Kind, Severity: a.Severity, Ref: a.Ref, Key: key, Title: a.Title, Resolved: true,
			Detail: "This is over."}
		if err := c.Notifier.Notify(ctx, ev); err != nil {
			c.log().Warn("recovery not delivered; it is tried again at the next check", "kind", a.Kind, "ref", a.Ref, "error", err)
		}
	}
}

// checkUpdate asks for the newest release when the last check is older than the interval and
// raises update_available for a version the operator was not told about yet.
func (c *Checker) checkUpdate(ctx context.Context, now time.Time) {
	if c.CheckUpdate == nil {
		return
	}
	interval := c.UpdateInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	rec, err := health.ReadUpdate(c.Cfg.Paths())
	if err != nil {
		c.log().Warn("update check: cannot read the record", "error", err)
	}
	// An error is retried at the next interval, not at every check: the last attempt stamps the record.
	if rec == nil || now.Sub(rec.CheckedAt) >= interval {
		got, err := c.CheckUpdate(ctx, now)
		if err != nil {
			c.log().Warn("update check failed", "error", err)
		}
		if got != nil {
			rec = got
		}
	}
	if rec == nil || !rec.Available || rec.Latest == "" || rec.NotifiedVersion == rec.Latest {
		return
	}
	ev := Event{
		Kind: KindUpdateAvailable, Severity: SeverityInfo, Key: KindUpdateAvailable + "/" + rec.Latest,
		Title:  "Supavise " + rec.Latest + " is available",
		Detail: fmt.Sprintf("This node runs %s. Run `supavise upgrade --check` to see what changes, then `sudo supavise upgrade` inside your maintenance window.", rec.Installed),
	}
	if err := c.Notifier.Notify(ctx, ev); err != nil {
		c.log().Warn("update notice not delivered; it is tried again at the next check", "version", rec.Latest, "error", err)
		return
	}
	// Without a destination Notify sends nothing and returns nil; the version is then not
	// marked, so it is announced as soon as one is configured.
	if c.Notifier.Configured() {
		if err := health.MarkNotified(c.Cfg, rec.Latest); err != nil {
			c.log().Warn("update check: cannot record the notice", "error", err)
		}
	}
}

// conditionsOf turns a report into the conditions that are wrong now.
func conditionsOf(r *health.Report) []condition {
	var out []condition
	for _, comp := range r.Components {
		if comp.State != health.Warn && comp.State != health.Fail {
			continue
		}
		sev := SeverityWarning
		if comp.State == health.Fail {
			sev = SeverityCritical
		}
		switch comp.Name {
		case "disk":
			out = append(out, condition{key: KindDiskLow + "/", kind: KindDiskLow, severity: sev, title: "Disk space is low", detail: comp.Detail})
		case "certificates":
			out = append(out, condition{key: KindCertificateExpiring + "/", kind: KindCertificateExpiring, severity: sev, title: "A certificate is about to expire", detail: comp.Detail})
		case "system backup":
			out = append(out, condition{key: KindBackupFailed + "/system", kind: KindBackupFailed, severity: SeverityWarning, ref: "system",
				title: "Backups of the system cluster are not current", detail: comp.Detail + " The system cluster holds the registry of every project."})
		default:
			if comp.Critical {
				sev = SeverityCritical
			}
			out = append(out, condition{key: KindNodeUnhealthy + "/" + comp.Name, kind: KindNodeUnhealthy, severity: sev,
				title: comp.Name + " is not healthy", detail: comp.Detail})
		}
	}
	var down []condition
	for _, p := range r.Projects {
		if problems := unhealthyServices(p); problems != "" {
			down = append(down, condition{key: KindProjectUnhealthy + "/" + p.Ref, kind: KindProjectUnhealthy, severity: SeverityCritical, ref: p.Ref,
				title: "Project " + projectLabel(p) + " is not healthy", detail: problems})
		}
		if b := p.Backup; b != nil && (b.Stale || b.LastFailed != "") {
			title, detail := "Backup of project "+projectLabel(p)+" is stale", "The newest completed backup is older than the limit."
			if b.LastFailed != "" {
				title, detail = "Backup of project "+projectLabel(p)+" failed", "The newest backup failed: "+b.LastFailed
			}
			if b.LastCompleted == nil {
				detail += " There is no completed backup."
			}
			out = append(out, condition{key: KindBackupFailed + "/" + p.Ref, kind: KindBackupFailed, severity: SeverityWarning, ref: p.Ref, title: title, detail: detail})
		}
	}
	// A few projects down are told one by one. Many at once mean something shared broke, and
	// fifty messages would bury the one that says what.
	if len(down) > groupAbove {
		refs := make([]string, 0, len(down))
		for _, d := range down {
			refs = append(refs, d.ref)
		}
		shown := refs
		if len(shown) > 10 {
			shown = shown[:10]
		}
		detail := strings.Join(shown, ", ")
		if len(refs) > len(shown) {
			detail += fmt.Sprintf(" and %d more", len(refs)-len(shown))
		}
		down = []condition{{key: KindProjectUnhealthy + "/many", kind: KindProjectUnhealthy, severity: SeverityCritical,
			title: "Many projects are not healthy", detail: fmt.Sprintf("%d projects do not answer: %s. Run `supavise status`.", len(refs), detail)}}
	}
	return append(out, down...)
}

// groupAbove is how many unhealthy projects are reported one by one.
const groupAbove = 5

func projectLabel(p health.ProjectResult) string {
	if p.Name != "" {
		return fmt.Sprintf("%s (%s)", p.Name, p.Ref)
	}
	return p.Ref
}

// unhealthyServices says what is wrong with a project's units and tenants ("" when nothing is).
func unhealthyServices(p health.ProjectResult) string {
	var parts []string
	for _, s := range p.Services {
		if !s.OK {
			parts = append(parts, s.Name+": "+firstNonEmpty(s.Error, strings.ToLower(s.Status)))
		}
	}
	for _, t := range p.Tenants {
		// A service that cannot be asked is reported once, as itself; counting it against
		// every project would raise fifty alerts for one dead service.
		if t.Error == "" && !t.Present {
			parts = append(parts, t.Service+" has no tenant for the project")
		}
	}
	return strings.Join(parts, "; ")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
