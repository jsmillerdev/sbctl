// Package update runs the node's own maintenance: the opt-in automatic upgrade inside the
// maintenance window, the reboot an OS patch asks for, and the host-side setup (the systemd
// timer, unattended-upgrades) that goes with them. The daemon, not this package, looks for new
// releases (internal/health.CheckUpdate) and announces them (internal/alerts).
//
// `supavise update run` calls Run from supavise-upgrade.service, which supavise-upgrade.timer
// starts. The upgrade itself is `supavise upgrade --unattended` (the upgrade engine owns what it
// does and what its exit codes mean); this package decides only whether and when to call it.
package update

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/selfupdate"
)

// Exit codes of `supavise upgrade --unattended`.
const (
	ExitOK            = 0 // upgraded, or nothing to do
	ExitRefused       = 2 // a pre-check refused; nothing changed
	ExitRolledBack    = 3 // failed and rolled back
	ExitNeedsOperator = 4 // failed and needs the operator
)

// Meaning says in words what an exit code of `supavise upgrade --unattended` means.
func Meaning(exit int) string {
	switch exit {
	case ExitOK:
		return "upgraded, or nothing to do"
	case ExitRefused:
		return "refused by a pre-check; nothing changed"
	case ExitRolledBack:
		return "failed and rolled back"
	case ExitNeedsOperator:
		return "failed and needs the operator"
	}
	return fmt.Sprintf("unexpected exit status %d", exit)
}

// Deps are the things Run touches, so that a test supplies its own.
type Deps struct {
	Update  config.Update
	Version string // the running Supavise version
	Store   Store
	Log     *slog.Logger
	Now     func() time.Time
	// Latest returns the tag of the newest stable release the node knows of: the daemon's last
	// check (internal/health), or a lookup when there is none. Run uses it only to tell whether a
	// rolled-back release has a successor; the daemon's check is the one that announces releases.
	Latest func(ctx context.Context) (string, error)
	// Upgrade runs `supavise upgrade --unattended` and returns its exit status. The error is for
	// a command that could not run at all.
	Upgrade func(ctx context.Context) (exit int, err error)
	// RebootRequired reports whether an installed OS patch waits for a reboot.
	RebootRequired func(ctx context.Context) bool
	// RebootBlocker says why the node must not restart itself right now, or returns "" when it
	// may: a failed or unhealthy project or service, a base backup running, a lifecycle
	// operation in flight. A nil RebootBlocker blocks every reboot, so that a caller who forgot
	// the gate gets no reboot rather than an ungated one.
	RebootBlocker func(ctx context.Context) string
	// Reboot restarts the machine.
	Reboot func(ctx context.Context) error
}

// The latest moment, counted back from the close of the window, at which an unattended upgrade or
// a reboot may still start. An upgrade of many projects runs for a long time and is not cut off
// when the window closes, so one that starts in the last minutes would run its outage past the
// window the operator chose. The cutoffs are the smaller of a fixed span and a share of the window,
// so that a short window still has a time to start in. A reboot is short (docs/research/09-footprint.md
// measures the cold start), so it may start later than an upgrade.
const (
	upgradeCutoff = time.Hour
	rebootCutoff  = 15 * time.Minute
)

// StartCutoff is how long before the window closes the last unattended upgrade may start.
func StartCutoff(w config.Window) time.Duration { return min(upgradeCutoff, w.Length()/2) }

// RebootCutoff is how long before the window closes the last OS reboot may start.
func RebootCutoff(w config.Window) time.Duration { return min(rebootCutoff, w.Length()/4) }

// Run does one pass of the node's maintenance. It does nothing outside the maintenance window,
// and in notify mode it never upgrades. (Looking for a release and announcing it belong to the
// daemon: internal/health records the check, internal/alerts raises update_available.) It starts
// neither an upgrade nor a reboot in the last stretch of the window (StartCutoff, RebootCutoff),
// and it reboots only when the node is healthy and quiet (Deps.RebootBlocker). The events it logs
// have stable messages (unattended_upgrade_started, unattended_upgrade_result,
// unattended_upgrade_skipped, unattended_upgrade_interrupted, os_reboot, os_reboot_deferred,
// update_check_failed) for whatever
// collects them.
func Run(ctx context.Context, d Deps) error {
	now := d.Now()
	win, err := d.Update.ParsedWindow()
	if err != nil {
		return err
	}
	st, err := d.Store.Load()
	if err != nil {
		return err
	}
	before := st
	opened, closes, inWindow := win.Occurrence(now)

	// 0. An unattended upgrade that began and left no result was cut short: the node crashed, the
	// kernel killed the process, the power failed, or something killed the service. Nobody knows
	// what state it left, so a person looks before the node tries again.
	if p := st.InProgress; p != nil {
		st.InProgress = nil
		st.Blocked = fmt.Sprintf("the unattended upgrade that started at %s did not report how it ended (the node or the service was stopped part way through)", p.Started.Format(time.RFC3339))
		d.Log.Error("unattended_upgrade_interrupted", "started", p.Started.Format(time.RFC3339), "window", p.Window.Format(time.RFC3339),
			"installed", d.Version, "fix", "run `supavise status` and `supavise upgrade --check`, fix the node, then `sudo supavise update resume`")
		if err := d.Store.Save(st); err != nil {
			return err
		}
		return fmt.Errorf("an unattended upgrade was interrupted; automatic upgrades are paused until `sudo supavise update resume`")
	}

	// 1. In auto mode, inside the window, run the upgrade: once per window unless it was refused.
	var upgradeErr error
	upgradeFailed, upgradeRefused := false, false
	if d.Update.Mode == config.UpdateAuto && inWindow {
		switch {
		case st.Blocked != "":
			d.Log.Warn("unattended_upgrade_skipped", "reason", "blocked", "detail", st.Blocked,
				"fix", "run `supavise status`, fix the node, then `sudo supavise update resume`")
		case st.Window.Equal(opened):
			// This window already had its attempt.
		case closes.Sub(now) < StartCutoff(win):
			d.Log.Warn("unattended_upgrade_skipped", "reason", "window_closing", "window_closes", closes.Format(time.RFC3339),
				"detail", fmt.Sprintf("less than %s of the window is left", StartCutoff(win)))
		case d.skipRolledBack(ctx, &st):
			st.Window = opened // decided for this window; the next one looks again
		default:
			// The record goes to disk before the upgrade starts: no upgrade without the note that
			// lets the next run see it never finished.
			st.InProgress = &Progress{Window: opened, Started: d.Now(), Version: d.Version}
			if err := d.Store.Save(st); err != nil {
				return err
			}
			d.Log.Info("unattended_upgrade_started", "installed", d.Version, "window", opened.Format(time.RFC3339))
			exit, runErr := d.Upgrade(ctx)
			st.InProgress = nil
			if runErr != nil {
				exit = -1
			}
			res := &Result{At: d.Now(), Exit: exit, Meaning: Meaning(exit), Version: d.Version}
			if runErr != nil {
				res.Meaning = "could not run supavise upgrade: " + runErr.Error()
			}
			st.Result = res
			switch exit {
			case ExitOK:
				st.Window = opened
				st.RolledBack = ""
				d.Log.Info("unattended_upgrade_result", "exit", exit, "outcome", "ok", "detail", res.Meaning)
			case ExitRefused:
				// Not marked: the next tick of this window tries again, which is the point of a
				// refusal that changed nothing (a backup was running, a project was unhealthy).
				upgradeRefused = true
				d.Log.Warn("unattended_upgrade_result", "exit", exit, "outcome", "refused", "detail", res.Meaning)
			case ExitRolledBack:
				st.Window = opened
				upgradeFailed = true
				st.RolledBack = d.rolledBackTarget(ctx)
				d.Log.Error("unattended_upgrade_result", "exit", exit, "outcome", "rolled_back", "detail", res.Meaning, "release", st.RolledBack)
				upgradeErr = fmt.Errorf("unattended upgrade failed and was rolled back (supavise upgrade exit %d); automatic upgrades skip that release until a newer one appears or `supavise update resume`", exit)
			default:
				// 4, an exit status the contract does not name, or no run at all: a person looks
				// before the node tries again.
				st.Window = opened
				st.Blocked = fmt.Sprintf("%s at %s", res.Meaning, res.At.Format(time.RFC3339))
				upgradeFailed = true
				d.Log.Error("unattended_upgrade_result", "exit", exit, "outcome", "needs_operator", "detail", res.Meaning)
				upgradeErr = fmt.Errorf("unattended upgrade needs the operator (supavise upgrade exit %d); automatic upgrades are paused until `supavise update resume`", exit)
			}
		}
	}

	// 2. An OS patch that needs a reboot gets it inside the window, once per window. The reboot
	// waits when the upgrade failed or was refused (a person should see the node first, and a
	// refusal means the node is unhealthy or not backed up), when the window is about to close or
	// closed during the upgrade, and while the node is unhealthy or busy.
	if d.Update.RebootsInWindow() && inWindow && !upgradeFailed && st.Blocked == "" && !st.RebootWindow.Equal(opened) && d.RebootRequired(ctx) {
		if reason := d.rebootDeferral(ctx, win, opened, upgradeRefused); reason != "" {
			d.Log.Warn("os_reboot_deferred", "window", opened.Format(time.RFC3339), "reason", reason, "next_try", "the next tick or window")
		} else {
			st.RebootWindow = opened
			if err := d.Store.Save(st); err != nil {
				return err // no reboot without the record that stops a reboot loop
			}
			d.Log.Warn("os_reboot", "window", opened.Format(time.RFC3339), "reason", "an OS update needs a reboot")
			if err := d.Reboot(ctx); err != nil {
				return fmt.Errorf("reboot: %w", err)
			}
			return upgradeErr
		}
	}

	if st != before {
		if err := d.Store.Save(st); err != nil {
			return err
		}
	}
	return upgradeErr
}

// rebootDeferral returns why the reboot must wait, or "" when it may go ahead now. It looks at the
// clock again: an upgrade can run for hours, so the time Run started at says nothing about the
// window now.
func (d Deps) rebootDeferral(ctx context.Context, win config.Window, opened time.Time, upgradeRefused bool) string {
	if upgradeRefused {
		return "the unattended upgrade was refused this pass, so the node is not healthy or not backed up"
	}
	at := d.Now()
	open, closes, ok := win.Occurrence(at)
	switch {
	case !ok || !open.Equal(opened):
		return "the maintenance window closed while the node was working"
	case closes.Sub(at) < RebootCutoff(win):
		return fmt.Sprintf("less than %s of the window is left", RebootCutoff(win))
	}
	if d.RebootBlocker == nil {
		return "no node health check is configured"
	}
	return d.RebootBlocker(ctx)
}

// skipRolledBack reports whether the window must not try the release that the last unattended
// upgrade rolled back. A newer release clears the memory and lets the window go on. A release
// lookup that fails keeps the skip: the node cannot tell the release is a new one.
func (d Deps) skipRolledBack(ctx context.Context, st *State) bool {
	if st.RolledBack == "" {
		return false
	}
	latest, err := d.Latest(ctx)
	if err != nil {
		d.Log.Warn("update_check_failed", "error", err.Error())
	} else {
		if selfupdate.Newer(latest, st.RolledBack) {
			st.RolledBack = ""
			return false
		}
	}
	d.Log.Warn("unattended_upgrade_skipped", "reason", "rolled_back_release", "release", st.RolledBack,
		"fix", "wait for a newer release, or run `sudo supavise update resume` to try this one again")
	return true
}

// rolledBackTarget is the release a rolled-back upgrade was after: the newest the node knows of,
// if it is newer than the running version. It is empty when the node knows of none (checks are
// off and GitHub is out of reach), and the next window then tries again.
func (d Deps) rolledBackTarget(ctx context.Context) string {
	if latest, err := d.Latest(ctx); err == nil && selfupdate.Newer(latest, d.Version) {
		return latest
	}
	return ""
}

func nextWindow(w config.Window, now time.Time) string {
	if t, ok := w.Next(now); ok {
		return t.Format(time.RFC3339)
	}
	return ""
}

// Resume clears what stops automatic upgrades after a failure (the pause of an upgrade that needs
// the operator, and the memory of a rolled-back release), so the next window may try again. It
// returns what it cleared, or "" when nothing stopped them.
func Resume(s Store) (was string, err error) {
	st, err := s.Load()
	if err != nil {
		return "", err
	}
	was = st.Blocked
	if st.RolledBack != "" {
		if was != "" {
			was += "; "
		}
		was += "release " + st.RolledBack + " was rolled back"
	}
	st.Blocked, st.RolledBack = "", ""
	return was, s.Save(st)
}
