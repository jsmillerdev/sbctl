# internal/update

The node's own maintenance: run the opt-in unattended upgrade inside the maintenance window, reboot for an OS patch inside it, and set up the host side of that (the systemd timer, unattended OS security updates). The daemon, not this package, looks for releases (`internal/health.CheckUpdate`, recorded in `update.json`) and raises `update_available` (`internal/alerts`); `update run` reads that record only to tell whether a rolled-back release has a successor. `deploy/README.md`, "Update", is the operator's view; this is the code's.

- `run.go`: `Run` is one pass of `supavise update run`, which `supavise-upgrade.service` executes when `supavise-upgrade.timer` fires. It decides and does not upgrade: the upgrade is `supavise upgrade --unattended` (`Deps.Upgrade`), whose exit status (0, 2, 3, 4) it maps to what happens next. Every effect goes through `Deps` (clock, latest-release lookup, upgrade, reboot check, reboot, state store), so the tests run a week of ticks against a fake clock.
- `state.go`: the JSON the pass remembers in `/var/lib/supavise-upgrade/state.json` (the unit's `StateDirectory`): the window that already had its attempt, the in-progress record of a running upgrade, the last result, the pause, the rolled-back release, the window of the last reboot. The directory is outside the state directory on purpose: the service runs as root and its memory decides whether the node upgrades and reboots, so a unit running as `supavise` must not be able to write it. The store refuses a directory that is a symlink, owned by another user, or writable by group or others.
- `timer.go`: `RenderTimer` writes `supavise-upgrade.timer` from `config.Update` (the window, nothing else: no release check); `deploy/systemd/supavise-upgrade.timer` is its rendering with the defaults and a test keeps them equal.
- `osupdates.go`: the apt configuration (security origins only, no automatic reboot), the needrestart drop-in, and the parsing of needrestart's kernel status.
- `host.go`: the real implementations of the reboot check, the reboot gate (`HostRebootBlocker`), the reboot, the host lock, the release lookup and the call of `supavise upgrade`. `lock_unix.go` is the flock behind `Store.Lock` and the host lock.

The window grammar and the `[update]` section are in `internal/config` (`window.go`, `update.go`).

## Rules the tests pin

- Notify mode never calls the upgrade. Auto mode calls it only while the window is open, once per window, except that a refusal (exit 2) is tried again at the next tick of the window.
- No upgrade starts in the last `StartCutoff` of the window (the smaller of 1 h and half the window; `unattended_upgrade_skipped reason=window_closing`).
- Exit 3 (rolled back) fails the service unit and remembers the release (`State.RolledBack`). The next window looks for a newer release and skips this one until it finds one or `supavise update resume` runs (`reason=rolled_back_release`); a failed lookup keeps the skip. Exit 4, an unnamed exit status and a command that did not run pause automatic upgrades until `supavise update resume`.
- **An upgrade that never reported is not run again.** Before it calls the upgrade, `Run` saves `State.InProgress` (window, start time, version) and clears it when the command returns. A pass that finds the record set knows the last upgrade was cut short (a crash, an OOM kill, a power cut): it logs `unattended_upgrade_interrupted`, pauses automatic upgrades as for exit 4 (`State.Blocked`), fails the unit, and starts neither an upgrade nor a reboot until `supavise update resume`. `Store.Lock` (an flock next to the state file) keeps a pass started by hand from reading a live pass's record as a crash.
- **A stopped service lets the upgrade end.** `RunUpgrade` passes SIGTERM, not SIGKILL, when its context is cancelled (systemd stopping the unit, a shutdown) and waits `UpgradeStopTimeout` (50 min) for the exit status. `supavise-upgrade.service` has `KillMode=mixed`, `TimeoutStopSec=1h` and no start timeout, so a 50-project upgrade that runs for hours is not cut off; `supavise upgrade` bounds its own steps.
- `update run` never announces a release or looks for one on a schedule. `--dry-run` (and the hidden `--at`, which implies it) swaps the upgrade and the reboot for log lines and works on a copy of the record.
- The node reboots for an OS patch only when `os_security_updates` and `os_reboot = "window"` are both set, only inside the window, once per window, and not after a failed or refused upgrade. `rebootDeferral` runs just before the reboot and reads the clock again (an upgrade can outlast the window): it defers when the window closed or `RebootCutoff` (the smaller of 15 min and a quarter of the window) is left, and when `Deps.RebootBlocker` names a reason (supavise.service down, a failed unit, a base backup running, a project in any status but ACTIVE_HEALTHY, INACTIVE or REMOVED, the host lock held by a running `supavise upgrade` or `self-update`, or no gate configured). A deferral logs `os_reboot_deferred` with the reason and does not use up the window's reboot. The record that stops a reboot loop is written before the reboot.

## Limits

- An upgrade that starts inside the window still runs to its end when the window closes first; the cutoff bounds the overrun, and nothing estimates the duration beforehand.
- The reboot gate reads project statuses from the registry (`supavise projects list --json`), which only moves between healthy and unhealthy when something runs a health check. It catches in-flight operations and projects already marked bad, not a project that died unnoticed. It does not call `supavise status` (`health.CheckNode`).
- The reboot check does not look at microcode.
- `HostLockPath` (`/run/supavise-maintenance.lock`) is the flock the reboot gate checks. `supavise upgrade` and `supavise self-update` take it (`update.LockHost`) from before the first change until the end.
- Events are log lines (`unattended_upgrade_started`, `unattended_upgrade_result`, `unattended_upgrade_skipped`, `unattended_upgrade_interrupted`, `os_reboot`, `os_reboot_deferred`, `update_check_failed`). They go to the journal (`journalctl -u supavise-upgrade`); `internal/alerts` does not forward them.

## Tests

`go test ./internal/update/` (the `test` job of `ci.yml`) runs `update run` passes against a fake clock, the timer and the apt and needrestart files. The `install-e2e` job of `linux.yml` covers the timer and `update config` on a real node, and the `os-updates` job the apt configuration.
