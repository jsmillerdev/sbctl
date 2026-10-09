package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/lifecycle"
)

// upgradeAlerter turns the events of project upgrades (lifecycle.Options.UpgradeNotify) into
// alerts. lifecycle cannot import alerts (alerts reads the node's health through lifecycle), so the
// daemon passes this hook in. Delivery is detached (alerts.Notifier.NotifyDetached): a slow webhook
// must not hold up the upgrade that raised the event, and a failure to send is logged, never the
// upgrade's.
type upgradeAlerter struct {
	notifier *alerts.Notifier
}

// newUpgradeAlerter takes a logger for symmetry with newOAuthAlerter; the notifier logs a failed
// delivery with its own.
func newUpgradeAlerter(n *alerts.Notifier, _ *slog.Logger) *upgradeAlerter {
	return &upgradeAlerter{notifier: n}
}

func (a *upgradeAlerter) notify(ctx context.Context, n lifecycle.UpgradeNotice) {
	a.notifier.NotifyDetached(ctx, upgradeEvent(n))
}

// wait lets the deliveries in flight finish when the daemon stops.
func (a *upgradeAlerter) wait() { a.notifier.Drain() }

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
