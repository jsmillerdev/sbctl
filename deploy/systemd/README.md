# deploy/systemd

systemd units for sbctl, embedded in the binary (`embed.go`) and installed by
`sbctl system install-units` (needs root) or by the installer.

| File | What |
|---|---|
| `sb-postgres@.service`, `sb-gotrue@.service`, `sb-postgrest@.service` | per-project templates; the instance is the ref |
| `sb-supavisor`, `sb-realtime`, `sb-storage`, `sb-pgmeta`, `sb-studio` | fleet singletons |
| `sb-imgproxy`, `sb-edge-runtime` | optional singletons |
| `sbctl.slice` | every unit runs in it, so `systemctl status sbctl.slice` shows the total |
| `sbctl.service` | the daemon (`sbctl serve`: Management API, edge proxy, lifecycle engine; starts active projects at boot) |
| `50-sbctl.rules` | polkit rule: the `sbctl` user may start, stop and tune `sb-*` units and `sbctl.service` (manage-units only; enabling units and daemon-reload need root, through `install-units`) |
| `sb-basebackup@.service` and `.timer`, `sb-basebackup-prune.service` and `.timer` | nightly base backup per project and the node-wide retention prune. The daemon and the lifecycle engine start the timer instances over D-Bus (they are not enabled for boot); `install-units` writes `sb-basebackup@.timer` with `backup.base_backup_on_calendar` |

Every service unit runs as `User=sbctl`, reads `/var/lib/sbctl/projects/<ref>/<svc>.env`
(0600, by systemd, before the unit's mount namespace exists) and executes `<svc>.run`; both are
written by `units.Supervisor.Render`, so the templates never change per project. `MemoryMax` and `CPUQuota` are per-unit drop-ins applied
over D-Bus. Logs go to journald, selected by unit name (`SyslogIdentifier` equals the
unit).

Hardening: `NoNewPrivileges`, `ProtectSystem=strict` with a `ReadWritePaths` of the
unit's own state directory only (the allowlist below), `ProtectHome`, `PrivateTmp`, `PrivateDevices` (`/dev/shm` stays
available), kernel protections, empty capability set, `RestrictAddressFamilies`,
`UMask=0027`. `MemoryDenyWriteExecute` is left off for the artifacts (Postgres JIT, the BEAM
and Node map writable and executable memory); `sbctl.service` sets it. Postgres stops on
SIGINT (fast shutdown) with `KillMode=mixed` and 90 s.

The unit paths assume the default `state_dir` of `/var/lib/sbctl`. A test in
`internal/units` checks that the templates and the Go layout agree.

The units start in `tests/linux/systemd-smoke.sh` on a CI VM (Ubuntu 24.04, amd64 and arm64) in
the `linux` workflow. If a hardening option breaks an artifact there, relax that option in the
affected template and record why in the template.

## Trust model: what the sandbox does and does not do

HANDOFF section 1 settles one service user: every unit, Postgres included, runs as `sbctl`,
and so does the daemon. That has a consequence this section states plainly.

**The mount sandbox is not tenant isolation.** All services share one uid, and the kernel
lets a process of a uid open `/proc/<pid>/environ` and `/proc/<pid>/root` of every other
process of that uid. A compromised GoTrue or PostgREST of project A can therefore read
`/proc/<postgres-pid-of-B>/root/...` (B's data directory) and the environment of every other
unit: B's JWT secret and database passwords sit in the environment of B's GoTrue and
PostgREST. A file-read bug in a service reaches the same files (a path traversal can read
`/proc/<pid>/environ`). `TemporaryFileSystem`, `BindPaths` and `InaccessiblePaths` do not stop
any of that, and no unit option available on Ubuntu 22.04 and 24.04 and Debian 12 (systemd 249
to 255) does: `ProtectProc=invisible` hides only other users' processes, and `PrivatePIDs=`
needs systemd 257.

**What it does buy.** The allowlist below keeps a service from reading, by plain path, what it
has no business with: the master key and config (`/etc/sbctl`), TLS keys, other projects' data
and backups, and sibling units' environment files. That stops accidents, a service that wanders
through the file system, and exploit classes that reach only paths and not `/proc`. It is
defense in depth, not a boundary, and the documentation must not promise tenant isolation on
a single node: **treat every project on a node as sharing a trust domain with every other
project and with the control plane.** Postgres is the most exposed process (reachable through
Supavisor, runs C extensions), which is why it gets the same allowlist, and why
`POSTGRES_PASSWORD` (the `supabase_admin` superuser password, read only on the first boot) is
rendered into `postgres.env` only until the cluster is initialized, then removed and the
cluster restarted, so it is not in the postmaster's environment for the life of the cluster.
The other passwords cannot be removed from the environments of the units that need them.

**Allowlist, per template.** `TemporaryFileSystem=/var/lib/sbctl:ro` replaces the state
directory with an empty tmpfs, and the unit gets back only:

| Unit | Read-only | Writable |
|---|---|---|
| `sb-postgres@<ref>` | `artifacts/`, `projects/<ref>/postgres.run` | `projects/<ref>/postgres/`, `artifacts/postgres/` (the launcher chmods a script there on first boot), `backups/<ref>/` (file backend; sbctl creates it) |
| `sb-gotrue@<ref>`, `sb-postgrest@<ref>` | `artifacts/`, `projects/<ref>/<svc>.run` | `projects/<ref>/<svc>/` |
| fleet singletons | `artifacts/`, `projects/system/<svc>.run` | `system/<svc>/` (optional: create it first); Studio also `artifacts/studio/` |

No template binds a project or system directory as a whole, and none needs to read an
environment file: systemd (PID 1) reads `EnvironmentFile=` before it builds the namespace.
The Postgres template hides only `/etc/sbctl/master.key`, because `archive_command`
(`sbctl wal push`) reads `config.toml` for the backend. `internal/units`
(`TestTemplatesContainment`) pins this shape, and `tests/linux/systemd-smoke.sh` checks it
from inside the namespaces of a real node (CI, amd64 and arm64).

Two limits of the file backend follow from the allowlist: it must live under
`/var/lib/sbctl/backups` for the systemd backend (a backend elsewhere needs a drop-in that
adds its path to the Postgres template), and `backups/<ref>` must exist before the unit
starts, which sbctl guarantees at create and start.

## Future hardening: one uid per project

The real fix changes a HANDOFF convention, so it is recorded here and not done: a distinct
uid per project (and one for the fleet and one for the control plane), with the environment
files owned by the project uid and still read by systemd as root, so that the kernel denies
`/proc` access across projects and a compromised project cannot reach another's data or the
master key. Costs: uid allocation and cleanup in `lifecycle`, file ownership in
`internal/units` and in the backup paths, and the installer. (`DynamicUser=` does not fit:
sbctl backs up and restores the data directories, which must keep a stable owner.)
Alternative on systemd 257 or later: `PrivatePIDs=yes` per unit, which hides other units'
processes without new uids. Until one of them ships, the paragraphs above are the supported
trust model.

## Residual notes

The singleton `system/<svc>` directories are optional (`-` prefix): create one before a
service needs to write there (the fleet workstream owns that). `pg_hba.conf` is sbctl's: it
trusts `supabase_admin` on the cluster's private unix socket (a directory only the `sbctl`
user can enter) and requires scram-sha-256 on TCP; the registry is reached through that
socket, so a process that can enter a cluster's socket directory is a superuser there. The
allowlist hides every socket directory from the other units, but not from the daemon, which
needs them.
