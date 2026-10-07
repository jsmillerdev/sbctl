# internal/update

The node's own maintenance: look for a Supavise release, run the opt-in unattended upgrade inside the maintenance window, reboot for an OS patch inside it, and set up the host side of that (the systemd timer, unattended OS security updates). `deploy/README.md`, "Update", is the operator's view; this is the code's.

- `run.go`: `Run` is one pass of `supavise update run`, which `supavise-upgrade.service` executes when `supavise-upgrade.timer` fires. It decides, it does not upgrade: the upgrade is `supavise upgrade --unattended` (`Deps.Upgrade`), whose exit status (0, 2, 3, 4) it maps to what happens next. Every effect goes through `Deps` (clock, release lookup, upgrade, reboot check, reboot, state store), so the tests run a week of ticks against a fake clock.
- `state.go`: the JSON the pass remembers in `<state_dir>/update/state.json` (the release announced, the window that already had its attempt, the last result, the pause, the window of the last reboot).
- `timer.go`: `RenderTimer` writes `supavise-upgrade.timer` from `config.Update`; `deploy/systemd/supavise-upgrade.timer` is its rendering with the defaults and a test keeps them equal.
- `osupdates.go`: the apt configuration (security origins only, no automatic reboot), the needrestart drop-in, and the parsing of needrestart's kernel status.
- `host.go`: the real implementations of the reboot check, the reboot, the release lookup and the call of `supavise upgrade`.

The window grammar and the `[update]` section are in `internal/config` (`window.go`, `update.go`).

## Rules the tests pin

- Notify mode never calls the upgrade. Auto mode calls it only while the window is open, once per window, except that a refusal (exit 2) is tried again at the next tick of the window.
- Exit 3 (rolled back) fails the service unit and waits for the next window. Exit 4, an unnamed exit status and a command that did not run pause automatic upgrades until `supavise update resume`.
- A release is announced (`update_available`) once; a failed check is logged and is not a failure.
- The node reboots for an OS patch only when `os_security_updates` and `os_reboot = "window"` are both set, only inside the window, once per window, and not after a failed upgrade. The record that stops a reboot loop is written before the reboot.

## Not done

- An upgrade that starts inside the window runs to its end even when the window closes first; nothing estimates the duration beforehand.
- The reboot check does not look at microcode, and does not wait for a quiet moment: the window is the quiet moment.
- Events are log lines (`update_available`, `unattended_upgrade_started`, `unattended_upgrade_result`, `unattended_upgrade_skipped`, `os_reboot`, `update_check_failed`). Sending them anywhere is the alerting package's job, wired at merge.
