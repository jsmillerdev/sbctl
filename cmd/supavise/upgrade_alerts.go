package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/alerts"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/nodeupgrade"
)

// upgradeAlertTimeout bounds one delivery. A webhook is retried once after two seconds and SMTP can
// take 40 seconds; the upgrade does not wait longer than this for an alert, and a lost alert is
// logged, never an error of the upgrade.
const upgradeAlertTimeout = 90 * time.Second

// upgradeNotifier returns the Notify hook of `supavise upgrade` and `supavise rollback`. They run
// outside the daemon (as root, from a terminal or from supavise-upgrade.service), so they build a
// Notifier from the node's config the way the daemon does and share its state file: the same
// destinations, the same hourly cap and the same record of what was sent (internal/alerts, "Root
// and the supavise user"). Events go out even when the daemon is down, which is when they matter.
//
// Maintenance windows and the upgrade marker quiet the daemon's checker for restarted projects and
// services; they do not touch these events. An upgrade that fails inside an announced window is
// still told, and so is the one the window's own timer started.
func upgradeNotifier(cfg *config.Config, log *slog.Logger) func(context.Context, nodeupgrade.Event) {
	return notifyWith(alerts.New(cfg, alerts.Options{Log: log}), log)
}

func notifyWith(n *alerts.Notifier, log *slog.Logger) func(context.Context, nodeupgrade.Event) {
	return func(ctx context.Context, ev nodeupgrade.Event) {
		// A SIGTERM that stops the upgrade must not also drop the message that says why.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upgradeAlertTimeout)
		defer cancel()
		if err := n.Notify(ctx, upgradeAlert(ev)); err != nil {
			log.Warn("could not send the upgrade alert", "event", ev.Kind, "error", err)
		}
	}
}

// upgradeAlert is the alert that tells the operator about ev.
func upgradeAlert(ev nodeupgrade.Event) alerts.Event {
	what, target := "Upgrade", ev.To
	if ev.Rollback {
		what = "Rollback"
	}
	by := ""
	if ev.Unattended {
		by = " by the maintenance window"
	}
	var a alerts.Event
	switch ev.Kind {
	case nodeupgrade.EventStarted:
		a.Kind, a.Severity = alerts.KindUpgradeStarted, alerts.SeverityInfo
		a.Title = fmt.Sprintf("%s to %s started", what, target)
		a.Detail = fmt.Sprintf("Supavise %s -> %s started%s. %s", ev.From, ev.To, by, projectsLine(ev, "move"))
	case nodeupgrade.EventSucceeded:
		a.Kind, a.Severity = alerts.KindUpgradeSucceeded, alerts.SeverityInfo
		a.Title = fmt.Sprintf("Node is on %s", target)
		if ev.Rollback {
			a.Title = fmt.Sprintf("Node rolled back to %s", target)
		}
		a.Detail = fmt.Sprintf("Supavise %s -> %s finished%s. %s", ev.From, ev.To, by, projectsLine(ev, "moved"))
	case nodeupgrade.EventRefused:
		a.Kind, a.Severity = alerts.KindUpgradeFailed, alerts.SeverityWarning
		a.Title = fmt.Sprintf("%s to %s did not go ahead", what, target)
		a.Detail = fmt.Sprintf("Supavise %s -> %s was stopped%s before anything changed, and the node still runs %s. Cause: %s", ev.From, ev.To, by, ev.From, ev.Cause)
	case nodeupgrade.EventRolledBack:
		a.Kind, a.Severity = alerts.KindUpgradeFailed, alerts.SeverityWarning
		a.Title = fmt.Sprintf("Upgrade to %s failed and was rolled back", target)
		a.Detail = fmt.Sprintf("Supavise %s -> %s failed%s; the node runs %s again. %sCause: %s", ev.From, ev.To, by, ev.BackTo, haltedLine(ev), ev.Cause)
		a.Detail += "\nRun `supavise status` to check the node."
		if ev.Unattended {
			a.Detail += " Automatic upgrades skip this release until a newer one exists; `sudo supavise update resume` lifts that."
		}
	case nodeupgrade.EventNeedsOperator:
		a.Kind, a.Severity = alerts.KindUpgradeFailed, alerts.SeverityCritical
		a.Title = fmt.Sprintf("%s to %s failed: the node needs you", what, target)
		a.Detail = fmt.Sprintf("Supavise %s -> %s failed%s and the node is not back on %s. %sCause: %s", ev.From, ev.To, by, ev.From, haltedLine(ev), ev.Cause)
		a.Detail += "\nRun `supavise status`, then see `supavise rollback` and deploy/README.md, \"Upgrading the node\"."
		if ev.Unattended {
			a.Detail += " Automatic upgrades are paused until `sudo supavise update resume`."
		}
		if ev.Rollback {
			a.Detail = fmt.Sprintf("Supavise %s -> %s failed and the node needs the operator. Cause: %s\nRun `supavise status`.", ev.From, ev.To, ev.Cause)
		}
	}
	return a
}

func projectsLine(ev nodeupgrade.Event, verb string) string {
	switch {
	case ev.Projects == 0:
		return "No project " + verb + "."
	case ev.Rollback && ev.Kind == nodeupgrade.EventStarted:
		return fmt.Sprintf("%d project(s) go back to the releases they ran.", ev.Projects)
	case ev.Rollback:
		return fmt.Sprintf("%d project(s) went back to the releases they ran.", ev.Projects)
	case verb == "move":
		return fmt.Sprintf("%d project(s) move, one at a time after the canary.", ev.Projects)
	}
	return fmt.Sprintf("%d project(s) %s.", ev.Projects, verb)
}

func haltedLine(ev nodeupgrade.Event) string {
	if ev.Halted == "" {
		return ""
	}
	return fmt.Sprintf("The rollout halted at project %s. ", ev.Halted)
}

// haltedRef finds the project in the worker's error: "rollout halted: REF failed (...)" for a
// rollout of several projects, "REF failed: ..." for one. A ref is twenty lower-case letters.
var haltedRef = regexp.MustCompile(`(?:rollout halted: |supavise: )([a-z]{20}) failed`)

// haltTee passes the worker's stderr through and remembers the project its rollout halted at. The
// worker (`supavise projects upgrade --all`) names it in its error; that line is the only place the
// ref is told.
type haltTee struct {
	w    io.Writer
	line strings.Builder
	ref  string
}

func (t *haltTee) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	for _, b := range p[:n] {
		if b == '\n' {
			t.scan()
			continue
		}
		if t.line.Len() < 4096 { // a line that long is not the error
			t.line.WriteByte(b)
		}
	}
	return n, err
}

func (t *haltTee) scan() {
	s := t.line.String()
	t.line.Reset()
	if m := haltedRef.FindStringSubmatch(s); m != nil {
		t.ref = m[1]
	}
}

// Ref is the project the rollout halted at, or "".
func (t *haltTee) Ref() string {
	t.scan() // a last line without its newline
	return t.ref
}
