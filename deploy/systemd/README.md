# deploy/systemd

systemd units for sbctl, embedded in the binary (`embed.go`) and installed by
`sbctl system install-units` (needs root) or by the installer.

| File | What |
|---|---|
| `sb-postgres@.service`, `sb-gotrue@.service`, `sb-postgrest@.service` | per-project templates; the instance is the ref |
| `sb-supavisor`, `sb-realtime`, `sb-storage`, `sb-pgmeta`, `sb-studio` | fleet singletons |
| `sb-imgproxy`, `sb-edge-runtime` | optional singletons |
| `sbctl.slice` | every unit runs in it, so `systemctl status sbctl.slice` shows the total |
| `sbctl.service` | the daemon (`sbctl serve`, from the API and proxy workstreams) |
| `50-sbctl.rules` | polkit rule: the `sbctl` user may manage `sb-*` units and `sbctl.service` |
| `sb-basebackup*` | owned by the backup workstream |

Every service unit runs as `User=sbctl`, reads `/var/lib/sbctl/projects/<ref>/<svc>.env`
(0600) and executes `<svc>.run`; both are written by `units.Supervisor.Render`, so the
templates never change per project. `MemoryMax` and `CPUQuota` are per-unit drop-ins applied
over D-Bus. Logs go to journald, selected by unit name (`SyslogIdentifier` equals the
unit).

Hardening: `NoNewPrivileges`, `ProtectSystem=strict` with a `ReadWritePaths` of the
project's own directory (the Postgres template also `-/var/lib/sbctl/backups` for the file
backup backend), `ProtectHome`, `PrivateTmp`, `PrivateDevices` (`/dev/shm` stays
available), kernel protections, empty capability set, `RestrictAddressFamilies`,
`UMask=0027`. `MemoryDenyWriteExecute` is left off for the artifacts (Postgres JIT, the BEAM
and Node map writable and executable memory); `sbctl.service` sets it. Postgres stops on
SIGINT (fast shutdown) with `KillMode=mixed` and 90 s.

The unit paths assume the default `state_dir` of `/var/lib/sbctl`. A test in
`internal/units` checks that the templates and the Go layout agree.

Not verified on Linux in development: the units start in `tests/linux/systemd-smoke.sh`
on a CI VM. If a hardening option breaks an artifact there, relax that option in the
affected template and record why in the template.
