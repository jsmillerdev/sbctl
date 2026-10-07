# deploy/systemd

systemd units for sbctl, embedded in the binary (`embed.go`) and installed by
`sbctl system install-units` (needs root) or by the installer.

| File | What |
|---|---|
| `sb-postgres@.service`, `sb-gotrue@.service`, `sb-postgrest@.service` | per-project templates; the instance is the ref |
| `sb-supavisor`, `sb-realtime`, `sb-storage`, `sb-pgmeta`, `sb-studio` | fleet singletons |
| `sb-imgproxy`, `sb-edge-runtime` | optional singletons |
| `sb-edge-bundle@<ref>` | one-shot, started by the API for each upload of Edge Function sources (one instance per project, so each has **its own uid and its own module cache**, `/var/cache/sb-edge-bundle/<ref>`, which `sbctl projects delete` removes): runs `edge-runtime bundle` under **a uid of its own** (`DynamicUser=yes`, the one exception to `User=sbctl`) in a sandbox that sees only the upload's sources (read-only), one output directory and the artifacts, with no loopback, no instance metadata and no other process in `/proc` (see `internal/functions/README.md`) |
| `sbctl.slice` | every unit runs in it, so `systemctl status sbctl.slice` shows the total |
| `sbctl.service` | the daemon (`sbctl serve`: Management API, edge proxy, lifecycle engine; starts active projects at boot) |
| `50-sbctl.rules` | polkit rule: the `sbctl` user may start, stop and tune `sb-*` units and `sbctl.service` (manage-units only; enabling units and daemon-reload need root, through `install-units`) |
| `sb-basebackup@.service` and `.timer`, `sb-basebackup-prune.service` and `.timer` | nightly base backup per project and the node-wide retention prune. The daemon and the lifecycle engine start the timer instances over D-Bus (they are not enabled for boot); `install-units` writes `sb-basebackup@.timer` with `backup.base_backup_on_calendar` |

Every service unit but `sb-edge-bundle` runs as `User=sbctl`, reads `/var/lib/sbctl/projects/<ref>/<svc>.env`
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

**The exception, and why it is one.** `sb-edge-bundle@<ref>.service` runs another person's source code through a bundler that opens whatever paths the code names, so it is the one unit whose input chooses file paths, and a mount namespace is not enough for it: under the `sbctl` uid an upload could import `/proc/<pid>/root/...` of any other unit and carry that unit's files, every project's `functions-env.json` included, out in its bundle. It therefore runs under a dynamic uid, with `ProtectProc=invisible` and `ProcSubset=pid` (they hide only other users' processes, so they work for it and not for the `sbctl` units), reading the sources the daemon hands over and writing two files the daemon prepares. `tests/linux/functions-smoke.sh` probes it in the real unit. The unit is a template with one instance per project (`sb-edge-bundle@<ref>.service`, `CacheDirectory=sb-edge-bundle/%i`), because a module cache is shared by everything bundled with it: with one cache for the node, an upload of project B could import what an upload of project A had made the bundler download (a private npm package fetched with A's `.npmrc`). Each instance's cache is private to its own uid and is deleted with the project; the smoke test plants a package through A's upload and checks that B's upload cannot import it. **The Edge Runtime does not need the same treatment against its user workers**: they are isolates inside its process, with no path to name under `/proc` (see `functions-main/README.md`, "Isolation, as measured"); the runtime stays in the one trust domain of the node, and a compromise of the runtime itself (a V8 escape) is the case the future one-uid-per-project hardening below addresses.

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
(`sbctl wal push`) reads `config.toml` for the backend. **That makes `/etc/sbctl/config.toml`
readable by every project's Postgres, and with it whatever secrets it holds:
`backup.s3_secret_access_key` (and the access key id) and the DNS-01 provider credentials in
`tls.credentials`.** A tenant that can run code in its cluster can read them, so anyone who
runs untrusted SQL extensions or hands out `postgres`-role access should keep S3 and DNS
credentials out of `config.toml`: use an instance profile or a bucket policy scoped to the
backup prefix for S3, and the HTTP-01 challenge instead of DNS-01. (A backup-only file that the
Postgres units can read, with `config.toml` hidden from them, is the fix; it is not done.) `internal/units`
(`TestTemplatesContainment`) pins this shape, and `tests/linux/systemd-smoke.sh` checks it
from inside the namespaces of a real node (CI, amd64 and arm64).

**The writable artifact directory is a persistence path.** `artifacts/postgres/` is writable
in every project's Postgres namespace and shared by all clusters, the system cluster that holds
the registry included, and `artifacts` unpacks owned by the `sbctl` user (`artifacts.handOver`).
One cluster can replace the Postgres binaries that every other cluster, and the control
plane's own database, run at their next restart. The only reason it is writable is the
launcher's one `chmod +x` of `share/supabase-cli/config/pgsodium_getkey.sh` under `set -e`,
which fails on a read-only or foreign-owned tree even when the bit is already set. Fix
(recorded, not done, because it needs iteration on Linux): keep the artifact tree root-owned
and read-only, give each cluster a private writable copy of only `share/supabase-cli/config`
(a per-project `BindPaths=` rendered into the unit's drop-in by the plane, since the template
cannot know the artifact tag), drop `handOver`, and send the upstream change that makes the
chmod conditional. This is part of the same trust model as above: one trust domain per node.

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

**Cloud metadata.** On AWS (deploy/cloudformation/sbctl.yaml) the instance role holds write
access to the whole backup bucket (every project's WAL and base backups) and Route 53 changes,
and IMDSv2's hop limit of 1 does not stop a process on the instance itself. The tenant-facing
units (GoTrue, PostgREST, postgres-meta, Realtime, Supavisor, Studio, imgproxy, edge runtime)
therefore carry `IPAddressDeny=169.254.169.254`. Two exceptions need the role and keep access:
`sbctl.service` and the backup units, and `sb-postgres@` (its `archive_command` runs
`sbctl wal push` inside the postmaster, which signs S3 requests with the role), plus
`sb-storage` when `storage_backend = "s3"` relies on the role. A Postgres extension or a
Storage bug could still reach the credentials, so the AWS template should be paired, once
workstream J runs user code in Edge Functions, with a bucket policy or a separate role for
Storage and with static credentials for the WAL archiver (`--s3-credentials-file`). Workstream
J: keep `IPAddressDeny=169.254.169.254` on `sb-edge-runtime.service` and on any unit that runs
tenant code; `sb-edge-bundle@<ref>.service` (it reads tenant code's imports) has it too, and in addition
denies loopback, so a bundler cannot reach the Management API or a database, and runs under its own uid. It cannot be started
with `systemd-run` by the `sbctl` user: the polkit rule grants `manage-units` only for names that
start with `sb-`, and polkit receives no unit name for a transient unit, so the sandbox is a fixed
unit file, not a transient one.
