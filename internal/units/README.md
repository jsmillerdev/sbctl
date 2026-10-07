# internal/units

Renders and drives the units that run Supabase's native artifacts. The `Supervisor`
interface has two backends.

- **systemd** (`systemd_linux.go`, production): D-Bus through
  `github.com/coreos/go-systemd/v22/dbus`, pure Go, no `systemctl`. The unit templates in
  `deploy/systemd/` are static. `Render` writes only `<state_dir>/projects/<ref>/<svc>.env`
  (0600) and `<svc>.run` (the launcher script the template executes), and applies per-unit
  `MemoryMax` and `CPUQuota` as persistent drop-ins with `SetUnitProperties` (systemd writes
  them under `/etc/systemd/system.control`). No daemon-reload is needed for a new project;
  `Reload` exists for after the templates change. `Remove` reverts the drop-ins.
  The templates are an allowlist (`TestTemplatesContainment`): every unit hides `/etc/sbctl`, sees an empty tmpfs where `/var/lib/sbctl` is plus its own paths, and denies the cloud instance metadata addresses (`IPAddressDeny`); a project's Postgres also gets its WAL relay directory read-only and no backup directory (`deploy/systemd/README.md`).
  Running as the `sbctl` user needs the polkit rule `deploy/systemd/50-sbctl.rules`, which grants `manage-units` on `sb-*` units only. Daemon-reload and enabling the system units for boot need root and happen in `sbctl system install-units`; on delete, `Remove` sets MemoryMax and CPUQuota back to infinity and leaves the inert drop-in.
- **exec** (`exec.go`, development and tests): runs `<svc>.run` as a detached child in its
  own session, logs to `<state_dir>/logs/<unit>.log`, keeps `<state_dir>/run/<unit>.pid`
  (with the process start time as a guard against pid reuse) so that a later `sbctl`
  invocation can query and stop what an earlier one started. Postgres gets SIGINT (fast
  shutdown) then SIGQUIT; everything else SIGTERM to the process group; stragglers get
  SIGKILL. It does not enforce limits, restart crashed units or start at boot.

`cfg.Supervisor` (`systemd` or `exec`) picks the backend through `units.New`. On non-Linux
builds `NewSystemd` returns an error, so the package builds and vets everywhere.

Both backends read the same `Spec`, so the exec backend exercises the real launchers,
environment files and quoting. `FormatEnv` writes `KEY="value"` lines with `\` and `"`
escaped (valid for systemd `EnvironmentFile=` and for `ParseEnv`).

## Test

```
go test ./internal/units/
```

Covers env round trips, launcher script rendering, the templates against the file layout,
installing templates, and the exec backend with fake launchers (stop signals, crash
detection, kill fallback, a second process stopping the first one's units).
The systemd backend is Linux-only and is exercised by `tests/linux/systemd-smoke.sh` on a
CI VM, not here.

## Not done

- The systemd backend compiles for linux/amd64 and linux/arm64 but has not been run
  (no Linux in development).
- No `MemoryHigh` or `TasksMax`; only `MemoryMax` and `CPUQuota`, as the project record
  specifies.
- Journald is the only log sink on Linux; `Exec.Tail` is the only log reader.
