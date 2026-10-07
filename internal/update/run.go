// Package update runs the node's own maintenance: the periodic look for a new Supavise release,
// the opt-in automatic upgrade inside the maintenance window, the reboot an OS patch asks for,
// and the host-side setup (the systemd timer, unattended-upgrades) that goes with them.
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

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/selfupdate"
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
	// Latest returns the tag of the newest stable release.
	Latest func(ctx context.Context) (string, error)
	// Upgrade runs `supavise upgrade --unattended` and returns its exit status. The error is for
	// a command that could not run at all.
	Upgrade func(ctx context.Context) (exit int, err error)
	// RebootRequired reports whether an installed OS patch waits for a reboot.
	RebootRequired func(ctx context.Context) bool
	// Reboot restarts the machine.
	Reboot func(ctx context.Context) error
}

// Run does one pass of the node's maintenance. It never does anything outside the maintenance
// window except check for and announce a release; in notify mode it never upgrades. The
// events it logs have stable messages (update_available, unattended_upgrade_started,
// unattended_upgrade_result, unattended_upgrade_skipped, os_reboot, update_check_failed) for
// whatever collects them.
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
	opened, _, inWindow := win.Occurrence(now)

	// 1. Look for a release and announce it once.
	if every, on, _ := d.Update.CheckEvery(); on && now.Sub(st.CheckedAt) >= every-time.Minute {
		latest, err := d.Latest(ctx)
		if err != nil {
			d.Log.Warn("update_check_failed", "error", err.Error())
		} else {
			st.CheckedAt, st.Latest = now, latest
			if selfupdate.Newer(latest, d.Version) && st.Notified != latest {
				d.Log.Warn("update_available", "installed", d.Version, "latest", latest,
					"mode", d.Update.Mode, "window", win.String(), "next_window", nextWindow(win, now))
				st.Notified = latest
			}
		}
	}

	// 2. In auto mode, inside the window, run the upgrade: once per window unless it was refused.
	var upgradeErr error
	upgradeFailed := false
	if d.Update.Mode == config.UpdateAuto && inWindow {
		switch {
		case st.Blocked != "":
			d.Log.Warn("unattended_upgrade_skipped", "reason", "blocked", "detail", st.Blocked,
				"fix", "run `supavise status`, fix the node, then `sudo supavise update resume`")
		case st.Window.Equal(opened):
			// This window already had its attempt.
		default:
			d.Log.Info("unattended_upgrade_started", "installed", d.Version, "window", opened.Format(time.RFC3339))
			exit, runErr := d.Upgrade(ctx)
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
				d.Log.Info("unattended_upgrade_result", "exit", exit, "outcome", "ok", "detail", res.Meaning)
			case ExitRefused:
				// Not marked: the next tick of this window tries again, which is the point of a
				// refusal that changed nothing (a backup was running, a project was unhealthy).
				d.Log.Warn("unattended_upgrade_result", "exit", exit, "outcome", "refused", "detail", res.Meaning)
			case ExitRolledBack:
				st.Window = opened
				upgradeFailed = true
				d.Log.Error("unattended_upgrade_result", "exit", exit, "outcome", "rolled_back", "detail", res.Meaning)
				upgradeErr = fmt.Errorf("unattended upgrade failed and was rolled back (supavise upgrade exit %d)", exit)
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

	// 3. An OS patch that needs a reboot gets it inside the window, once per window, unless the
	// upgrade just failed (a person should see the node before it restarts).
	if d.Update.RebootsInWindow() && inWindow && !upgradeFailed && st.Blocked == "" && !st.RebootWindow.Equal(opened) && d.RebootRequired(ctx) {
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

	if st != before {
		if err := d.Store.Save(st); err != nil {
			return err
		}
	}
	return upgradeErr
}

func nextWindow(w config.Window, now time.Time) string {
	if t, ok := w.Next(now); ok {
		return t.Format(time.RFC3339)
	}
	return ""
}

// Resume clears the pause that a failed unattended upgrade left, so the next window may try again.
func Resume(s Store) (was string, err error) {
	st, err := s.Load()
	if err != nil {
		return "", err
	}
	was = st.Blocked
	st.Blocked = ""
	return was, s.Save(st)
}
