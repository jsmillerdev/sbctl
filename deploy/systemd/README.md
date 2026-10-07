# deploy/systemd

systemd units for Supavise, embedded in the binary (`embed.go`) and installed by
`supavise system install-units` (needs root) or by the installer.

| File | What |
|---|---|
| `supavise-postgres@.service`, `supavise-gotrue@.service`, `supavise-postgrest@.service` | per-project templates; the instance is the ref |
| `supavise-supavisor`, `supavise-realtime`, `supavise-storage`, `supavise-pgmeta`, `supavise-studio` | fleet singletons |
| `supavise-imgproxy`, `supavise-edge-runtime` | optional singletons |
| `supavise-edge-bundle@<ref>` | one-shot, started by the API for each upload of Edge Function sources (one instance per project, so each has **its own uid and its own module cache**, `/var/cache/supavise-edge-bundle/<ref>`, which `supavise projects delete` removes): runs `edge-runtime bundle` under **a uid of its own** (`DynamicUser=yes`, the one exception to `User=supavise`) in a sandbox that sees only the upload's sources (read-only), one output directory and the artifacts, with no loopback, no instance metadata and no other process in `/proc` (see `internal/functions/README.md`) |
| `supavise.slice` | every unit runs in it, so `systemctl status supavise.slice` shows the total |
| `supavise.service` | the daemon (`supavise serve`: Management API, edge proxy, lifecycle engine; starts active projects at boot) |
| `50-supavise.rules` | polkit rule: the `supavise` user may start, stop and tune `supavise-*` units and `supavise.service` (manage-units only; enabling units and daemon-reload need root, through `install-units`) |
| `supavise-basebackup@.service` and `.timer`, `supavise-basebackup-prune.service` and `.timer` | nightly base backup per project and the node-wide retention prune. The daemon and the lifecycle engine start the timer instances over D-Bus (they are not enabled for boot); `install-units` writes `supavise-basebackup@.timer` with `backup.base_backup_on_calendar` |

Every service unit but `supavise-edge-bundle` runs as `User=supavise`, reads `/var/lib/supavise/projects/<ref>/<svc>.env`
(0600, by systemd, before the unit's mount namespace exists) and executes `<svc>.run`; both are
written by `units.Supervisor.Render`, so the templates never change per project. `MemoryMax` and `CPUQuota` are per-unit drop-ins applied
over D-Bus. Logs go to journald, selected by unit name (`SyslogIdentifier` equals the
unit).

Hardening: `NoNewPrivileges`, `ProtectSystem=strict` with a `ReadWritePaths` of the
unit's own state directory only (the allowlist below), `ProtectHome`, `PrivateTmp`, `PrivateDevices` (`/dev/shm` stays
available), kernel protections, empty capability set, `RestrictAddressFamilies`,
`UMask=0027`. `MemoryDenyWriteExecute` is left off for the artifacts (Postgres JIT, the BEAM
and Node map writable and executable memory); `supavise.service` sets it. Postgres stops on
SIGINT (fast shutdown) with `KillMode=mixed` and 90 s.

The unit paths assume the default `state_dir` of `/var/lib/supavise`. A test in
`internal/units` checks that the templates and the Go layout agree.

The units start in `tests/linux/systemd-smoke.sh` on a CI VM (Ubuntu 24.04, amd64 and arm64) in
the `linux` workflow. If a hardening option breaks an artifact there, relax that option in the
affected template and record why in the template.

## Trust model: what the sandbox does and does not do

HANDOFF section 1 settles one service user: every unit, Postgres included, runs as `supavise`,
and so does the daemon. The mount namespace of each unit and a few other controls do the
isolating. This section says exactly what they cover and what they do not, because a node
hosts projects that run user code (SQL functions, extensions such as pg_net and http, an
agent with `execute_sql`, a compromised app).

### What is isolated

| Threat | Control | Checked by |
|---|---|---|
| Code of one project reads another project's data directory, environment files, launcher or backups by path | Allowlist mount namespace (below): every unit sees an empty tmpfs where `/var/lib/supavise` is, plus its own paths | `tests/linux/systemd-smoke.sh` and `fleet-smoke.sh`, from inside the namespaces of real units (CI, amd64 and arm64) |
| Code in a unit talks to systemd (the polkit rule lets the `supavise` user stop and tune `supavise-*` units) | `InaccessiblePaths=-/run/dbus` hides the system bus from every `supavise-*` unit; only the daemon uses it | `systemd-smoke.sh` |
| Code in a unit reads the master key or `config.toml` (S3 and DNS credentials) | `InaccessiblePaths=-/etc/supavise` on every unit. Postgres no longer needs the config: its WAL goes through the relay | same |
| Code in a project's Postgres reads or overwrites another project's WAL or base backups, or its own | The Postgres unit has no backup directory and no backend credentials. `archive_command` and `restore_command` are `supavise wal push\|fetch --socket <projects/<ref>/wal/r.sock>`: the daemon (the only holder of the credentials) does the storage I/O over a unix socket that only that project's unit can see. The socket serves exactly one project: it refuses a push for another ref and a fetch of another ref's archive unless the daemon recorded it as the source of this project's restore | `relay_test.go` (the socket contract), `systemd-smoke.sh` (the unit sees only its own socket directory, read-only; the relay answers 403 for a foreign ref) |
| Code reaches the cloud instance credentials (EC2 role: the whole backup bucket, Route 53) | `IPAddressDeny=169.254.169.254 fd00:ec2::254` on every `supavise-*` unit: Postgres, GoTrue, PostgREST, postgres-meta, Realtime, Supavisor, Storage, Studio, imgproxy, the edge runtime. Only `supavise.service` (and the `supavise-basebackup` units, which run `supavise`) keep access | `systemd-smoke.sh` and `fleet-smoke.sh`: a mock listens on both metadata addresses; a curl placed in the unit's cgroup fails, the same curl outside reaches it, and `COPY ... TO PROGRAM 'curl ...'` inside the project's Postgres fails (see "Checking the metadata rule" below) |
| A crashed or stopped daemon makes WAL pile up somewhere insecure | There is no other path: with the daemon down `archive_command` fails and Postgres retries it (the segment stays in `pg_wal`, nothing is buffered elsewhere); `restore_command` exits 126, which aborts recovery instead of ending it early | `systemd-smoke.sh` (archiving stops, then resumes), `cli_test.go` (exit statuses) |

**The exception, and why it is one.** `supavise-edge-bundle@<ref>.service` runs another person's source code through a bundler that opens whatever paths the code names, so it is the one unit whose input chooses file paths, and a mount namespace is not enough for it: under the `supavise` uid an upload could import `/proc/<pid>/root/...` of any other unit and carry that unit's files, every project's `functions-env.json` included, out in its bundle. It therefore runs under a dynamic uid, with `ProtectProc=invisible` and `ProcSubset=pid` (they hide only other users' processes, so they work for it and not for the `supavise` units), reading the sources the daemon hands over and writing two files the daemon prepares. `tests/linux/functions-smoke.sh` probes it in the real unit. The unit is a template with one instance per project (`supavise-edge-bundle@<ref>.service`, `CacheDirectory=supavise-edge-bundle/%i`), because a module cache is shared by everything bundled with it: with one cache for the node, an upload of project B could import what an upload of project A had made the bundler download (a private npm package fetched with A's `.npmrc`). Each instance's cache is private to its own uid and is deleted with the project (by the root one-shot `supavise-edge-bundle-clean@<ref>.service`, the one other unit this adds: it accepts a lowercase ref and removes `/var/cache/private/supavise-edge-bundle/<ref>`, nothing else); the smoke test plants a package through A's upload and checks that B's upload cannot import it. **The Edge Runtime does not need the same treatment against its user workers**: they are isolates inside its process, with no path to name under `/proc` (see `functions-main/README.md`, "Isolation, as measured"); the runtime stays in the one trust domain of the node, and a compromise of the runtime itself (a V8 escape) is the case the future one-uid-per-project hardening below addresses.

**What it does buy.** The allowlist below keeps a service from reading, by plain path, what it
has no business with: the master key and config (`/etc/supavise`), TLS keys, other projects' data
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

The relay keeps the `archive_command` contract exactly: success only after the compressed file is
durable in the backend, an identical re-push succeeds, a different file under the same name fails,
and a file missing from the archive is exit 1 while an unreadable archive is exit 126
(`internal/backup/README.md`).

### What is not isolated

**The mount sandbox is not a boundary against code that runs inside a unit.** All services
share one uid, and the kernel lets a process of a uid open `/proc/<pid>/environ` and
`/proc/<pid>/root` of every other process of that uid. A compromised GoTrue or PostgREST of
project A can therefore read `/proc/<postgres-pid-of-B>/root/...` (B's data directory) and the
environment of every other unit: B's JWT secret and database passwords sit in the environment
of B's GoTrue and PostgREST. A file-read bug in a service reaches the same
files (a path traversal can read `/proc/<pid>/environ`). `TemporaryFileSystem`, `BindPaths` and
`InaccessiblePaths` do not stop any of that, and no unit option available on Ubuntu 22.04,
24.04 and Debian 12 (systemd 249 to 255) does: `ProtectProc=invisible` hides only other users'
processes, and `PrivatePIDs=` needs systemd 257.

The processes that hold the master key, the config and the backend credentials are the
exception, by a kernel rule: `ptrace_may_access` (which guards `/proc/<pid>/environ` and
`/proc/<pid>/root`) refuses a caller of the same uid when the target holds a permitted
capability the caller lacks. Every `supavise-*` unit that runs tenant code has an empty capability
set, and `supavise.service`, `supavise-basebackup@.service` and `supavise-basebackup-prune.service` hold
`CAP_NET_BIND_SERVICE` in theirs (`CapabilityBoundingSet=` and `AmbientCapabilities=`), so a
tenant unit cannot read their environment, their `/proc/<pid>/root` (`/etc/supavise/master.key`,
`config.toml`) or their memory. The daemon needs the capability for its listeners on ports 80
and 443; the backup units carry it only for this check. Do not remove it from these units, and do
not give it to a unit that runs tenant code. (`tests/linux/systemd-smoke.sh` reads the capability
sets from systemd; the kernel rule is described in `ptrace(2)`.)

The CLI is the other case. A command an operator runs (`sudo -u supavise supavise ...`: `claim token`,
`backups`, `projects`) runs in the host mount namespace, so its `/proc/<pid>/root` shows
`/etc/supavise/master.key`, `config.toml` and the backups directory, and it holds no capability.
Every `supavise` process therefore calls `prctl(PR_SET_DUMPABLE, 0)` first thing in `main`
(`cmd/supavise/harden_linux.go`). A non-dumpable process refuses `PTRACE_MODE_READ` to a same-uid
caller without `CAP_SYS_PTRACE`, and that covers `/proc/<pid>/root`, `environ` and `mem` for the
whole run of the command, with or without a capability; `exec` resets the flag, so the children
a command starts (`psql`, `pg_basebackup`) are unaffected. `systemd-smoke.sh` keeps a CLI command
alive and expects `ls /proc/<pid>/root/etc/supavise` from inside a tenant unit's cgroup to fail, with
an ordinary same-uid process as the control that it works. The processes that stay readable from
another unit are therefore the tenant-facing ones: every project's Postgres, GoTrue and
PostgREST, and the fleet services.

The WAL relay does not rely on the mount namespace alone, because a process of any unit can
open another project's socket through `/proc/<pid-of-that-postmaster>/root/...`. On every
connection the relay reads the peer's pid (`SO_PEERCRED`) and its cgroup
(`/proc/<pid>/cgroup`), and serves only a peer in `supavise-postgres@<ref>.service` for the socket of
`<ref>`, in a `supavise-basebackup` unit, or outside every `supavise-*` unit (the daemon, an operator's
shell). Any other `supavise-*` unit gets a closed connection, so a tenant cannot push a bogus file under
a future segment name or read another project's archive. A tenant cannot move itself into another
unit's cgroup. Where the cgroup cannot be read, the peer is refused. The pid could be reused
between the connection and the read of its cgroup file; on Linux 6.5 and later the check takes a
pidfd of the peer (`SO_PEERPIDFD`) and refuses when that process is gone after the read. Older
kernels (Debian 12 has 6.1) lack the option, and there the check compares the process's start
time before and after the read, which closes only the window of the read itself; exploiting what
remains needs code execution in a unit plus a pid race. The check is
`internal/backup/relay_peer.go`; `systemd-smoke.sh` moves a `curl` into other units' cgroups and
expects it to be refused.

So the controls above close the paths that do not go through `/proc`: plain file reads, the
metadata service, the backup credentials and the other projects' archives. They do not make a
node safe against an attacker who has arbitrary code execution as the `supavise` user inside one
unit. Postgres runs C extensions and is reachable through Supavisor, so it is the most exposed
process; a tenant who can run `COPY ... TO PROGRAM` (a superuser: Supabase does not give that
role to the `postgres` user) or load an untrusted extension has code execution inside the unit
and reaches the others through `/proc`. Practical consequences:

- Treat every project on a node as sharing a trust domain with every other project and with the
  control plane **as far as code execution inside a unit goes**. The credentials and data of the
  other projects are out of reach of SQL, `pg_net`/`http`, an `execute_sql` agent and a plain file
  read; they are within reach of an exploit that gives code execution as `supavise`.
- `POSTGRES_PASSWORD` (the `supabase_admin` superuser password, read only on the first boot) is
  rendered into `postgres.env` only until the cluster is initialized, then removed and the
  cluster restarted, so it is not in the postmaster's environment for the life of the cluster.
  The other passwords cannot be removed from the environments of the units that need them.
- Another project's database is also reachable over loopback TCP (`127.0.0.1:<port>`), behind
  scram-sha-256; knowing a password is not the same as being able to read a data directory, and the
  passwords are not in any file a unit can open.
- The Edge Runtime (workstream J) runs user code of every project in one process tree. Keep
  `IPAddressDeny` on it, and materialize each project's functions under a directory the runtime owns
  (for example `system/edge-runtime/functions/<ref>/`), never by binding `/var/lib/supavise/projects`
  (that directory holds every project's environment files and data directories):
  `TestTemplatesContainment` fails a template that binds more than the allowlist.

### Allowlist, per template

`TemporaryFileSystem=/var/lib/supavise:ro` replaces the state directory with an empty tmpfs, and
the unit gets back only:

| Unit | Read-only | Writable |
|---|---|---|
| `supavise-postgres@<ref>` | `artifacts/`, `projects/<ref>/postgres.run`, `projects/<ref>/wal/` (the relay socket; connecting needs no write access) | `projects/<ref>/postgres/`, `artifacts/postgres/` (the launcher chmods a script there on first boot) |
| `supavise-gotrue@<ref>`, `supavise-postgrest@<ref>` | `artifacts/`, `projects/<ref>/<svc>.run` | `projects/<ref>/<svc>/` |
| fleet singletons | `artifacts/`, `projects/system/<svc>.run` | `system/<svc>/` (optional: create it first); Studio also `artifacts/studio/` |
| `supavise-basebackup@<ref>` | `projects/system/postgres/sock` (the registry), `projects/<ref>/postgres/` (the data directory it archives) | `projects/<ref>/wal/` (a CLI relay's socket while the daemon is down), `backups/` (the file backend; absent on S3) |
| `supavise-basebackup-prune` | `projects/system/postgres/sock` | `backups/` |

No template binds a project or system directory as a whole, none needs to read an environment file
(systemd, PID 1, reads `EnvironmentFile=` before it builds the namespace), and none sees `/etc/supavise`,
the `backups` directory or the certificates. `internal/units` (`TestTemplatesContainment`) pins this
shape, and `tests/linux/systemd-smoke.sh` and `fleet-smoke.sh` check it from inside the namespaces of
a real node.

The Postgres template also hides three unix sockets, which are the ways out of the IP filter of a
branch with denied egress (`IPAddressDeny` does not cover unix sockets): systemd-resolved's
`/run/systemd/resolve/io.systemd.Resolve` and nscd's socket (in addition to the D-Bus system bus,
which every template hides). systemd 255 cannot change `InaccessiblePaths` of a running unit over
D-Bus, so the template hides them for every Postgres unit, which does not need them.

The WAL relay directory `projects/<ref>/wal/` is created by the lifecycle engine before the cluster
starts (`PostgresPlane.prepare`), and the daemon's `backup.Relay` serves one socket in it per
project, picking up new projects within three seconds or at once through the engine's hook. The
bind is optional (`-` prefix) so a unit that predates the directory still starts; it archives
nothing until the engine has created the directory and the unit restarted. An upgrade from a node
without the relay therefore needs one restart of each project's Postgres (the daemon's next
start of a project does it; `supavise projects pause|resume <ref>` forces it).

**The writable artifact directory is a persistence path.** `artifacts/postgres/` is writable
in every project's Postgres namespace and shared by all clusters, the system cluster that holds
the registry included, and `artifacts` unpacks owned by the `supavise` user (`artifacts.handOver`).
One cluster can replace the Postgres binaries that every other cluster, and the control
plane's own database, run at their next restart. The only reason it is writable is the
launcher's one `chmod +x` of `share/supabase-cli/config/pgsodium_getkey.sh` under `set -e`,
which fails on a read-only or foreign-owned tree even when the bit is already set. Fix
(recorded, not done, because it needs iteration on Linux): keep the artifact tree root-owned
and read-only, give each cluster a private writable copy of only `share/supabase-cli/config`
(a per-project `BindPaths=` rendered into the unit's drop-in by the plane, since the template
cannot know the artifact tag), drop `handOver`, and send the upstream change that makes the
chmod conditional. This is part of the same trust model as above.

### Checking the metadata rule

CI virtual machines have no instance metadata service, so the check puts a mock on both
addresses: `ip addr add 169.254.169.254/32 dev lo` and `ip -6 addr add fd00:ec2::254/128 dev lo`
with an HTTP server on a high port (`imds_up` in `tests/linux/lib.sh`; `IPAddressDeny` matches the
destination address, not the port). `imds_blocked_in <unit>` then moves a `curl` into the unit's
cgroup (`echo $$ > /sys/fs/cgroup/<ControlGroup>/cgroup.procs`), where systemd's BPF filter
applies, and expects it to fail on both addresses, while the same `curl` outside the unit must
reach the mock (so the mock works). `systemd-smoke.sh` also runs `COPY (select 1) TO PROGRAM
'curl ...'` as `supabase_admin` inside a project's Postgres: the program is a child of the
postmaster in the unit's cgroup, which is the real attack path (SQL reaching the network).
`imds_denied_by_unit` reads `systemctl show -p IPAddressDeny` for the same units. To try it by
hand on a node: `sudo bash -c 'echo $$ > /sys/fs/cgroup$(systemctl show -p ControlGroup --value
supavise-gotrue@<ref>.service)/cgroup.procs && exec curl -m 3 http://169.254.169.254/'` fails with
"Operation not permitted" while the same `curl` from a plain shell reaches the metadata service.

## Future hardening: one uid per project

The real fix changes a HANDOFF convention, so it is recorded here and not done: a distinct
uid per project (and one for the fleet and one for the control plane), with the environment
files owned by the project uid and still read by systemd as root, so that the kernel denies
`/proc` access across projects and a compromised project cannot reach another's data or the
master key. Costs: uid allocation and cleanup in `lifecycle`, file ownership in
`internal/units` and in the backup paths, and the installer. (`DynamicUser=` does not fit:
Supavise backs up and restores the data directories, which must keep a stable owner.)
Alternative on systemd 257 or later: `PrivatePIDs=yes` per unit, which hides other units'
processes without new uids. Until one of them ships, the paragraphs above are the supported
trust model.

## Residual notes

The singleton `system/<svc>` directories are optional (`-` prefix): create one before a
service needs to write there (the fleet workstream owns that). `pg_hba.conf` is Supavise's: it
trusts `supabase_admin` on the cluster's private unix socket (a directory only the `supavise`
user can enter) and requires scram-sha-256 on TCP; the registry is reached through that
socket, so a process that can enter a cluster's socket directory is a superuser there. The
allowlist hides every socket directory from the other units, but not from the daemon, which
needs them.

**Cloud metadata.** See "What is isolated" and "Checking the metadata rule". On AWS
(deploy/cloudformation/supavise.yaml) the instance role holds write access to the whole backup
bucket (every project's WAL and base backups) and TXT changes for `_acme-challenge` names in one
Route 53 zone, and IMDSv2's hop limit of 1 does not stop a process on the instance itself.
Every `supavise-*` unit therefore denies the metadata addresses. Two things follow:

- WAL archiving does not use the role inside Postgres: the daemon does. The role is only used by
  `supavise.service` and by the `supavise-basebackup` units.
- `supavise-storage` cannot use the role either. Its S3 backend (`[fleet] storage_backend = "s3"`)
  needs a static key (`storage_s3_access_key_id` and `storage_s3_secret_access_key`) scoped to the
  objects bucket under systemd; `fleet.Setup` refuses to render the unit without one and says why.
  The key sits in `config.toml` (0600, owned by `supavise`, hidden from every unit) and in the unit's own
  0600 environment file.

`supavise-edge-bundle@<ref>.service` (it reads tenant code's imports) also denies loopback, so a bundler cannot reach the Management API or a database, and runs under its own uid. It cannot be started with `systemd-run` by the `supavise` user: the polkit rule grants `manage-units` only for names that start with `supavise-`, and polkit receives no unit name for a transient unit, so the sandbox is a fixed unit file, not a transient one.
