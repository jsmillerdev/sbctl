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
| `50-sbctl.rules` | polkit rule: the `sbctl` user may start, stop and tune `sb-*` units and `sbctl.service` (manage-units only; enabling units and daemon-reload need root, through `install-units`) |
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

## Containment and residual risk

Every unit runs as the `sbctl` user (a settled convention), so systemd sandboxing is the
only wall between services. The API and fleet templates mount an empty tmpfs over
`/var/lib/sbctl` (`TemporaryFileSystem=/var/lib/sbctl:ro`) and bring back only the
artifacts (`BindReadOnlyPaths`), their own project directory (`projects/<ref>`, or
`projects/system` for the singletons) and, for singletons, their own
`/var/lib/sbctl/system/<svc>` directory. So they cannot see `backups/` (every project's WAL
and base backups), `certs/` (TLS keys), `run/`, `logs/`, other projects, or another
service's state. `/etc/sbctl` (master key, config), the cluster directory
(`postgres/`: sockets, pgsodium root key) and the sibling services' env files in the same
directory (`InaccessiblePaths`) are hidden as well, so Studio cannot read `supavisor.env` or
`storage.env`. The Postgres template keeps its own project directory and the backups
directory, which `wal push` needs; it is the only template that sees `backups/`.

What remains: all services share one uid, so a compromised service can still reach other
services over loopback TCP, and can read the `.run` launcher scripts of siblings in the
shared `projects/system` directory (they hold no secrets). Because the `sbctl` user owns the
cluster sockets and `pg_hba.conf` trusts `supabase_admin` on them, a process that can see a
socket can connect as `supabase_admin`; the sandbox hides every socket directory from the
API and fleet units, while `sbctl.service` and the Postgres units keep access. The
singleton `system/<svc>` directories are optional (`-` prefix): create one before a service
needs to write there. `tests/linux/systemd-smoke.sh` checks the hiding on a real systemd;
it has not been run yet.
