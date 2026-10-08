package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jsmillerdev/supavise/internal/alerts"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
)

// upgradeAlertTimeout bounds one delivery: a webhook is retried once after two seconds and SMTP can
// take 40 seconds.
const upgradeAlertTimeout = 90 * time.Second

// upgradeAlerter turns the events of project upgrades (lifecycle.Options.UpgradeNotify) into
// alerts. lifecycle cannot import alerts (alerts reads the node's health through lifecycle), so the
// daemon passes this hook in. Delivery runs on a goroutine of its own: a slow webhook must not hold
// up the upgrade that raised the event, and a failure to send is logged, never the upgrade's.
type upgradeAlerter struct {
	notifier *alerts.Notifier
	log      *slog.Logger
	wg       sync.WaitGroup
}

func newUpgradeAlerter(n *alerts.Notifier, log *slog.Logger) *upgradeAlerter {
	return &upgradeAlerter{notifier: n, log: log}
}

func (a *upgradeAlerter) notify(ctx context.Context, n lifecycle.UpgradeNotice) {
	ev := upgradeEvent(n)
	// The upgrade's context may end with the request that started it; the message still goes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upgradeAlertTimeout)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		if err := a.notifier.Notify(ctx, ev); err != nil {
			a.log.Warn("could not send the upgrade alert", "kind", ev.Kind, "ref", ev.Ref, "error", err)
		}
	}()
}

// wait lets the deliveries in flight finish, up to upgradeAlertTimeout, when the daemon stops.
func (a *upgradeAlerter) wait() {
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(upgradeAlertTimeout):
	}
}

// upgradeEvent is the alert for one event of a project's upgrade. They are announcements: each is
// sent when it happens, and neither an announced maintenance window nor a running node upgrade
// holds them back (those quiet the checker's project_unhealthy and node_unhealthy only).
func upgradeEvent(n lifecycle.UpgradeNotice) alerts.Event {
	changes := n.Changes
	if changes == "" {
		changes = "no service changes recorded"
	}
	ev := alerts.Event{Ref: n.Ref}
	switch n.Event {
	case lifecycle.EventUpgradeStarted:
		ev.Kind, ev.Severity = alerts.KindUpgradeStarted, alerts.SeverityInfo
		ev.Title = fmt.Sprintf("Project %s upgrade started", n.Ref)
		ev.Detail = fmt.Sprintf("%s. The project's services restart; it is offline for about a minute.", changes)
	case lifecycle.EventUpgradeSucceeded:
		ev.Kind, ev.Severity = alerts.KindUpgradeSucceeded, alerts.SeverityInfo
		ev.Title = fmt.Sprintf("Project %s upgraded", n.Ref)
		ev.Detail = changes + "."
		if n.Seconds > 0 {
			ev.Detail += fmt.Sprintf(" It took %d seconds.", n.Seconds)
		}
		if n.Settled {
			ev.Detail += " The daemon closed the record: the process that ran the upgrade had stopped after the project reached the target versions."
		}
	default:
		ev.Kind, ev.Severity = alerts.KindUpgradeFailed, alerts.SeverityWarning
		ev.Title = fmt.Sprintf("Project %s upgrade failed", n.Ref)
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s", n.ErrorCode, n.Cause)
		if n.Outcome != "" {
			fmt.Fprintf(&b, " (%s)", n.Outcome)
		}
		if n.Unresolved() {
			ev.Severity = alerts.SeverityCritical
			b.WriteString("\nThe project is not healthy. `supavise projects versions " + n.Ref + "` shows what it runs")
			if n.BackupID != 0 {
				fmt.Fprintf(&b, "; the pre-upgrade backup %d is the way back for the data (`supavise backups restore`)", n.BackupID)
			}
			b.WriteString(".")
		} else if n.BackupID != 0 {
			fmt.Fprintf(&b, "\nThe pre-upgrade backup is %d.", n.BackupID)
		}
		ev.Detail = b.String()
	}
	return ev
}
