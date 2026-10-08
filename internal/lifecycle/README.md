# internal/lifecycle

The project lifecycle: `Engine` (the `Manager`) owns the order of registry rows, sealed
secrets, units, fleet tenants and routes; `PostgresPlane` (the `DataPlane`) runs a
project's PostgreSQL, GoTrue and PostgREST as units of a `units.Supervisor`.

## Create

1. Registry row `COMING_UP`; credentials generated (or `CreateRequest.Keys` reused) and
   sealed into `project_secrets`.
2. `PostgresPlane.Create`: directories, `pg_hba.conf` and the pgsodium root key outside
   PGDATA (0600), then `bin/supabase-postgres-start` with these server arguments:
   `-p <port>`, `listen_addresses=127.0.0.1`, `unix_socket_directories=<project>/postgres/sock`
   (mode 0700), `hba_file`, `wal_level=logical`, `archive_mode=on`, `archive_command`,
   `archive_timeout`, and the project's compute size (see [Compute sizes](#compute-sizes)).
   A new project is Micro: 256MB `shared_buffers`, 60 connections, a 1 GB memory cap.
   - `internal/app` passes `backup.ArchiveCommandFor` and `[backup] archive_timeout_seconds` (default
     300; the plane falls back to 900 s without it). Under systemd the command uses the relay form
     `--socket <state>/projects/<ref>/wal/r.sock` and drops `SUPAVISE_CONFIG` from the unit's
     environment; otherwise it passes `--config` when the daemon loaded a non-default file.
   - The launcher initializes PGDATA and runs the artifact's roles and migrations once. Role passwords
     (`postgres`, `supabase_admin`, `authenticator`, `supabase_auth_admin`, `supabase_storage_admin`,
     `supabase_replication_admin`) are then set over the unix socket as SCRAM-SHA-256 verifiers computed
     in Go, with statement logging silenced for that session (the artifact logs DDL, so a plaintext
     `alter role` would land in journald). `POSTGRES_PASSWORD` is in `postgres.env` only while the data
     directory is not initialized.
   - `CreateRequest.Seed` replaces initdb and role setup and requires `CreateRequest.Keys` (the seeded
     cluster's own credentials).
3. GoTrue (`bin/auth migrate`, then `bin/auth`) and PostgREST, each health-checked with a
   real request (`/health`, `/`).
4. `fleet.Fleet.EnsureTenant` (skipped while the fleet is empty), `PutRoute` for
   `<ref>.api.<domain>`, the nightly backup timer (`Options.Timers`, systemd backend only), status `ACTIVE_HEALTHY`.

Any failure after the row exists stops and removes units and data, removes tenants and
route, and leaves the row `INIT_FAILED` with an event that carries the cause. The exception is
`ErrClusterExists` (the project directory already holds a cluster, for example an orphan after a
registry restore): Create refuses before writing anything, leaves the data alone and forgets the row.
Mutating operations on one ref are serialized by a mutex in the process and, with the Postgres
registry, by a session-level advisory lock, so the CLI and the daemon cannot, say, delete and resume
the same project at once.

`pg_hba.conf` is ours, not the artifact's: the artifact trusts all loopback connections,
which on a shared host means every local user. Ours trusts `supabase_admin` on the unix
socket only (the directory is private to the `supavise` user) and requires scram-sha-256 on
TCP. The socket is how Supavise reaches its own registry before it can decrypt any secret.

`pg_cron` runs its jobs in background workers (`cron.use_background_workers=on`,
`cron.database_name=postgres`, `cron.max_running_jobs=8`, `max_worker_processes=16`; see `cronSettings`). Hosted uses a libpq connection
to localhost that its pg_hba.conf trusts; ours does not trust loopback, so a libpq job would fail.
Workers need no connection, pg_hba rule or network, so they also work behind a branch's egress filter.
A saved `max_worker_processes` overrides the 16 (applied at the next restart); the Postgres settings
save refuses a value below 10 (`cron.max_running_jobs` plus two slots for pg_cron's launcher and pg_net's
worker). A value of 4 to 9 saved before that rule stays in force, and a job that cannot get a worker
then fails to start.

## Other operations

- `Pause`: the shared services are asked to let go of the project's database (`fleet.Quiescer`), then
  PostgREST, GoTrue, PostgreSQL stop in that order; `INACTIVE`; route and tenants stay. `Resume`
  reverses it and stops what started if it fails. Once the cluster answers, `Resume` also applies what
  was saved while paused and no unit renders: Postgres settings (`ALTER SYSTEM`) and Storage and
  Realtime tenant settings. A failure there is recorded as `project.config_apply_failed` and does not
  undo the resume.
- `Delete`/`DeleteWith`: final base backup through `BaseBackuper` (skipped for `INIT_FAILED` projects and
  with `SkipFinalBackup`; a paused project's database is started just for it), then tenants, route,
  units, data, the project's Storage objects (`<state>/system/storage/objects/stub/<ref>`) and the
  registry row. A failed backup keeps the project, except `ErrNoRestorableState` (a restore-as-new
  clone with no base backup): the delete records `project.final_backup_skipped` and goes on. A failure
  after the backup leaves `GOING_DOWN`, and a repeated delete resumes from the recorded step
  (`project.delete_started`, `project.delete_backup_done`) without a second backup.
- `RotateKeys`: new JWT secret, legacy and opaque keys; database passwords unchanged; GoTrue and
  PostgREST restart, fleet tenants update; previous keys are restored on error.
- `Health`: unit state plus SQL ping, GoTrue `/health`, PostgREST `/`; moves `ACTIVE_HEALTHY` and
  `ACTIVE_UNHEALTHY` to match.
- `StartActive`: starts every active project (after a reboot or `supavise system stop`), one at a
  time, re-reading each project after taking its lock. Before the cluster starts, `PostgresPlane.prepare`
  creates `projects/<ref>/wal/` (bind-mounted read-only into the Postgres unit) and
  `PlaneOptions.ArchiveReady(ref)` has the daemon serve the project's WAL relay socket.
- `Recover`: run once by the daemon before `StartActive`. A crash in the middle of an operation
  leaves a project in a status nothing else would move. Each move is a `project.recovered` event.

  | Found | Becomes |
  |---|---|
  | `PAUSING`, or `COMING_UP` or `RESTARTING` with a route | `INACTIVE` (units stopped) |
  | `COMING_UP` without a route (a create that never finished) | `INIT_FAILED` |
  | `GOING_DOWN`, backup step settled | removal finished, no second backup |
  | `GOING_DOWN`, cut off during the backup | back to the status it had (a paused one is stopped again) |
  | `GOING_DOWN` with no recorded events (an older version) | only logged, naming `--skip-final-backup` |
  | `RESTORING` | not touched |
  | `RESIZING` | `INACTIVE`, flagged for `ResumeRecovered`, which starts it on the size the registry holds (the new one: the record is written first) |
  | `UPGRADING` | stopped and set `ACTIVE_UNHEALTHY` when nobody runs its upgrade any more; left alone while another process holds it (see [Service versions and project upgrades](#service-versions-and-project-upgrades)) |

  A restart (the API's pause and resume) cut off after the pause is flagged `Recovered.Resume`, and
  `ResumeRecovered` brings the project back.
- Backup timers: with the systemd backend the Engine starts `supavise-basebackup@<ref>.timer` when a
  project becomes active and stops it on pause and delete (`Options.Timers`; failures are logged, never
  fatal). The timers are not enabled for boot; `supavise serve` starts the system project's timer and
  the prune timer at boot.
- Region: `CreateRequest.Region` is kept when it is an AWS region code, otherwise the configured
  `region` (default `us-east-1`) is stored: an unknown region breaks Studio's project list.

### In-place restore

`restore.go`. `Engine.BeginRestore` (the optional `DatabaseRestorer` capability; the Management API's
backup routes use it) moves an `ACTIVE_*` or `RESTORE_FAILED` project to `RESTORING` under the project
lock and returns a handle whose `Run` calls the backup service's `RestoreInPlace` (`InPlaceRestorer`,
through `Options.Backup`; `ErrNoRestorer` when the backend does not open).

- The service pauses and resumes the project through this Engine with a context marked as a restore;
  `Pause` and `Resume` then accept `RESTORING` and leave the status alone.
- `BeginRestore` first checks that the disk can hold the restored copy next to the current data
  (`ErrInsufficientDisk`, 409 through the API; skipped when the free space cannot be read).
- After a restore that worked, `Run` sets the cluster's role passwords to the registry's again (a
  failure is recorded as `restore.passwords_failed`; the restore stands) and prunes leftovers of earlier
  restores: the newest `data.pre-restore-*` stays, older ones and all `data.failed-restore-*` go. After
  a failure only older `data.failed-restore-*` go.
- `Run` settles the status: `ACTIVE_HEALTHY` after a restore, `RESTORE_FAILED` after a failure, whether
  or not the original data is running again. `Health` leaves `RESTORE_FAILED` alone; another restore,
  `Pause` (then `Resume`) and `Delete` move the project on.
- Every other operation refuses a `RESTORING` project (`ErrInvalidState`). `Delete` also refuses one
  whose restore runs in this process; a project left `RESTORING` by a stopped daemon can be deleted.
- The event is `restore.requested`.

## Service versions and project upgrades

A project runs the versions in `registry.Project.Versions` (service name to slim-services release
tag; `postgres`, `gotrue`, `postgrest`). A new project records the node's pins
(`internal/versions/versions.yaml`), and the plane renders its units from the project's own versions,
so a Supavise release that moves the pins changes no running project. A service with no recorded
version runs the node's pin, and the system project always runs the pins. A version whose artifact is
not on disk fails the render with `ErrNotFetched` and names `supavise projects upgrade <ref>`.

`Engine.CollectArtifacts` (`supavise artifacts gc`, and after a successful `projects upgrade`) removes
only artifacts that none of these keep: a project that runs it or is being upgraded to it; the node's
pins; the newest recorded release's pins; the pins of the last `[upgrade] keep_releases` releases
(default 3: the current release and the two before; written at daemon start).

`Engine.UpgradeProject(ref, target)` (`BeginUpgrade` + `Run`; the `ProjectUpgrader` capability the
Management API and the CLI use) moves a project to other versions, the node's pins by default, on the
same data directory. It follows hosted Supabase's flow (the project is `UPGRADING` with a tracking id and
a progress Studio draws, a failure brings the original back online) but is a restart on other binaries,
not a `pg_upgrade`: the services change by minor release inside one Postgres major version, which does
not change the on-disk format.

1. `BeginUpgrade`, under the project's lock, accepts only an `ACTIVE_HEALTHY` project on a node with a
   backup service, with something to change, the same Postgres major version, and no service moving to
   an older release (`ErrInvalidState`, `ErrNoBackupEngine`, `ErrUpgradeNotNeeded`,
   `ErrUpgradeUnsupported`; nothing changes on a refusal). It claims the ref (advisory lock
   `supavise:upgrade:<ref>` on a connection of its own, released when the process dies), writes the
   `project_upgrades` row (the tracking id), sets `UPGRADING` and records `project.upgrade_started`.
2. `Run` fetches and verifies the target artifacts, checks the installed extensions when the Postgres
   release changes (see [Extensions](#extensions)), and takes a base backup with the reason
   `pre-upgrade` while the project serves. A failure here ends the upgrade before any service is
   touched: the project returns to its status and the row says `failed`.
3. Under the lock: the extension check again, and the nightly backup timer stops. With a new Postgres
   release the shared services let go of the database and all three units restart from the target
   versions (database first); otherwise only GoTrue and PostgREST restart and the cluster keeps serving.
   Each unit answers a real request before the next starts, then every service is health checked and,
   after a Postgres change, every installed extension's code must load.
4. One `UpdateProject` records the target versions and `ACTIVE_HEALTHY`.

A failure in step 3 renders the previous versions again (recorded explicitly, so a project that had
none does not float onto the release that just failed), starts and health checks them, and records
`project.upgrade_failed` and the status row. If the rollback fails too the project is `ACTIVE_UNHEALTHY`
and the error names the pre-upgrade backup. The nightly backup timer starts again whenever PostgreSQL
runs after the rollback.

The unit requests of step 3 (`units.Supervisor` `Stop`, `Start`, `Status`, the limits) are sent again when the systemd bus drops them without an answer, so one such drop is not a failed upgrade (`internal/units/README.md`). `projects upgrade <ref>` acts on that project alone (`selectProjects`); a node-level revert runs one such command per project.

**Alerts.** The Engine cannot import `internal/alerts` (alerts read the node's health through this
package), so each upgrade event also goes to `Options.UpgradeNotify`, a hook that receives an
`UpgradeNotice` (event, ref, tracking id, the service moves in words, backup id, and for a failure the
error code, the cause and the outcome: `nothing was changed`, `rolled back to the previous versions`,
`the rollback failed too: ...` or `interrupted`). It is called after the registry event is recorded,
from the six places that record one: `BeginUpgrade`, a successful `swap`, `failed` (every failure
path), `settleUpgradeRows` and `recoverUpgrade` (a dead runner; the notice has `Settled`). The daemon
sets it (`internal/app`: `upgrade_started`, `upgrade_succeeded` and `upgrade_failed` alerts); the CLI
does not, because `supavise upgrade` reports a node's upgrade as one (`internal/nodeupgrade`), not as a
message per project. The hook must not block.

**A rollback does not restore the data directory.** Minor releases do not change the file format, so the
previous binaries read what the new ones left. GoTrue's (and Storage's) migrations only go forward, so an
older GoTrue can meet a schema a newer one migrated: the pre-upgrade base backup is the way back for the
data (`supavise backups restore`), and its id is in the error, the events and the status row.

**Dead runners.** `projects upgrade` ignores SIGHUP and SIGPIPE, so a dropped SSH session does not end
it; SIGKILL, the OOM killer or a daemon restart can. A project that is `UPGRADING` with the advisory
claim free lost its runner: `Recover` (at the daemon's start) and `SettleUpgrades` (every 2 minutes)
stop its units, mark the upgrade failed, and start the project on the recorded, previous versions as
`ACTIVE_UNHEALTHY`. An upgrade that died before it touched a unit (the artifact fetch or the base
backup) only has its row marked failed and the project set `ACTIVE_HEALTHY`. While the claim is held they
leave the project alone. The same pass closes a row that still says running on a project that is not
`UPGRADING`: `done` when the project runs the row's target versions, `failed` otherwise. An operator
needs to do nothing beyond waiting for the next pass or restarting the daemon; with no daemon,
`supavise serve` settles it at start.

Progress values and error codes (`1_started` through `9_completed_upgrade`, `1_upgraded_instance_launch_failed`
through `8_upgrade_completion_failed`) are hosted's names, which describe a `pg_upgrade` onto a new
instance; Studio draws them under its own labels, and the constants in `upgrade.go` say which stage each marks.

`Engine.UpgradeEligibility` answers for the node's pins: the changes, whether PostgreSQL restarts, an
estimated downtime (about 3 minutes without a PostgreSQL restart, 15 with one; the base backup does not
count) and blockers. Blockers: not `ACTIVE_HEALTHY`, another Postgres major version, a service the
project runs a newer release of than the target (`CompareTags` orders tags by upstream version, then
packaging revision; tags it cannot order are refused too), no backup service, the system project, and an
extension the target release cannot serve. `Rollout` (`rollout.go`) runs many upgrades for
`supavise projects upgrade --all`: `[upgrade] canary_projects` (default 1) one at a time, then
`[upgrade] batch_size` (default 5) at once, halting at the first failure.

### What a node upgrade adds

`supavise upgrade` (`internal/nodeupgrade`) drives `Engine.UpgradeProject` through the CLI.
`projects upgrade --all --to <service>=<tag>...` moves a project only to the releases named
(`--include-postgres` names PostgreSQL). `--allow-older` permits the one move to an older release, used
by `supavise rollback` (tags that cannot be ordered stay refused). `--reuse-backup-since` (hidden) takes
a completed base backup finished after that time as the pre-upgrade backup.

Making a release take effect on running units:

- `startAPI` (GoTrue and PostgREST, at every start) stops a running unit whose rendered files changed,
  so the `Start` after it runs the new files.
- While `supavise upgrade` moves the node forward the daemon holds restarts back (`DeferRestarts`) and
  the upgrade's rollout does them, with a canary and a stop at the first failure. The system cluster is
  not deferred.
- A held-back restart leaves a mark (`<svc>.held`, next to the unit's env file) with the digest of the
  files the process started with. The rollout restarts a unit that has a mark or whose files are newer
  than its process (`PendingRestart`, `RestartPending`); restarting a project's PostgreSQL stops its
  GoTrue and PostgREST first. A setting an Owner saved without restarting leaves no mark, so the rollout
  does not restart a cluster for it. `projects upgrade --all --restart-changed` (hidden) puts such
  projects in the same canary and batch order as the ones whose release moves.
- On rollback, a unit whose files render back to the mark's digest keeps running (nothing restarts, the
  mark is removed). A daemon start with a mark left (a rollout cut short) leaves a PostgreSQL cluster
  alone, restarts GoTrue and PostgREST, and leaves the cluster to `supavise upgrade`.
- `Engine.EnsureTenants` registers every active project with Supavisor, Realtime and Storage again once
  the shared services and projects have started. The tenants' fingerprints include the release tag of
  Storage and Realtime (`internal/fleet`), so after a release moved either of them each tenant is sent
  once more, which runs the new release's tenant migrations in every project's database; with nothing
  changed it sends nothing.

### Extensions

A Postgres upgrade swaps the binaries under the same data directory, and extensions keep their catalog
version and the shared library their functions name. A release that drops either leaves an extension
broken while the cluster starts and answers, so `extensions.go` checks, only when the Postgres release
changes:

- Before anything is touched (planning, after the artifact is fetched, and under the lock): every
  extension of every database that accepts connections must have a control file in the target
  artifact, a library (`module_pathname`, or `<extension>-<version>` for versioned shared-object builds
  such as `wrappers`), and its version's own script or a chain of update scripts to the release's default
  version. A finding is a blocker of type `unsupported_extension`, which Studio lists as an issue to
  resolve; the message tells the owner to run `ALTER EXTENSION <name> UPDATE` while the project still
  runs its current release. Planning only notes a release that is not on disk yet or a cluster that
  does not answer; the upgrade itself refuses both.
- After the new cluster starts and before the versions are recorded, `VerifyExtensions` runs Postgres's
  C-function validator (`fmgr_c_validator`) on every C function an extension owns. A failure rolls the
  upgrade back.

An upgrade never runs `ALTER EXTENSION UPDATE`: the owner updates extensions once the project is on the
new release. A GoTrue or PostgREST upgrade skips these checks.

## Branches

`CreateRequest.Branch` (`*registry.BranchInfo`) makes the new project a branch from the moment it
exists (`internal/branching` builds on that). `DeleteWith` refuses (`ErrInvalidState`) while the project
has branches. `DeleteOptions.KeepRecord` removes the fleet tenants, route, units and data but keeps the
registry row and its sealed secrets, which ends `INIT_FAILED`; `CreateRequest.Recreate` builds a new
cluster over such a row. A branch reset uses the pair so that a failure after the old cluster is gone
leaves the branch registered instead of gone.

## Saved settings

`Settings` (implemented by `projectconfig.Manager`, set as `Node.Settings`) is read whenever units and
tenants are rendered: GoTrue's and PostgREST's environment (saved values laid over the base; an empty
value removes a variable), the cluster's server arguments (only the settings that overlap the class's
sizing; see `SplitPostgresSettings`), and the Storage and Realtime `TenantSpec`. A settings source that
cannot be read fails the render rather than falling back to defaults. The system project never reads
user settings.

`Engine.ApplyConfig(ref, service, opts)` (the `Reconfigurer` capability the Management API uses) makes a
saved change take effect and touches only what the service owns: GoTrue or PostgREST re-render and
restart (only the changed one); Realtime and Storage get `EnsureTenant`; Postgres gets `ALTER SYSTEM`
for settings not on the command line, a reset for those no longer saved, `pg_reload_conf`, and a restart
when asked and needed. A Postgres restart restarts the whole project like a pause and a resume (the
GoTrue and PostgREST units are bound to the cluster's). A paused project is not started, but its units
are rendered once from the saved settings, so a value a unit cannot carry fails the save and not the
resume. A project in a transitional status is refused (`ErrInvalidState`).

`ApplyOptions.Recover` is what the API sets on the apply that undoes a failed one. For Postgres, a
cluster that does not answer (a restart failed on a value Postgres accepted in `ALTER SYSTEM`) is
stopped, every setting of the schema is removed from `postgresql.auto.conf` offline, and the project
starts on the restored settings.

`Engine.SetDatabasePassword(ref, password)` sets the `postgres` role's password as a SCRAM verifier,
seals it, and refreshes the project's Supavisor tenant (which clears its pools and cached credentials).
It restores the old password if the registry write fails and only warns when the pooler cannot be
reached.

## Compute sizes

A project has a compute size, the way a hosted project has a compute add-on. `registry.Project.Class`
holds the size's name; `classes.go` is the table. Memory, vCPU count, connection limits, replication
limits and the pooler client limit are copied from hosted's published sizes
([compute and disk](https://supabase.com/docs/guides/platform/compute-and-disk)). Hosted documents none
of its generated Postgres settings, so those columns are ours, derived by the rules below.

| Size | CPU | MemoryMax | CPUQuota | max_connections | slots and WAL senders | pooler clients | pool size | shared_buffers | work_mem | max_worker_processes |
|---|---|---|---|---|---|---|---|---|---|---|
| `nano` | 1 vCPU shared | 512M | 100% | 60 | 5 | 200 | 20 | 128MB | 4MB | 16 |
| `micro` | 1 vCPU shared | 1G | 100% | 60 | 5 | 200 | 20 | 256MB | 4MB | 16 |
| `small` | 1 vCPU shared | 2G | 100% | 90 | 5 | 400 | 35 | 512MB | 5MB | 16 |
| `medium` | 2 vCPU shared | 4G | 200% | 120 | 5 | 600 | 45 | 1GB | 8MB | 16 |
| `large` | 2 vCPU dedicated | 8G | 200% | 160 | 8 | 800 | 60 | 2GB | 12MB | 16 |
| `xlarge` | 4 vCPU dedicated | 16G | 400% | 240 | 24 | 1000 | 95 | 4GB | 17MB | 16 |
| `2xlarge` | 8 vCPU dedicated | 32G | 800% | 380 | 80 | 1500 | 150 | 8GB | 21MB | 24 |
| `4xlarge` | 16 vCPU dedicated | 64G | 1600% | 480 | 80 | 3000 | 190 | 16GB | 34MB | 40 |
| `8xlarge` | 32 vCPU dedicated | 128G | 3200% | 490 | 80 | 6000 | 195 | 32GB | 64MB | 72 |
| `12xlarge` | 48 vCPU dedicated | 192G | 4800% | 500 | 80 | 9000 | 200 | 48GB | 64MB | 104 |
| `16xlarge` | 64 vCPU dedicated | 256G | 6400% | 500 | 80 | 12000 | 200 | 64GB | 64MB | 136 |

Derived settings (`newSize`):

- `shared_buffers` is 25% of the memory and `effective_cache_size` 75%.
- `work_mem` is the memory left after `shared_buffers`, divided by three times `max_connections`, from 4 MB to 64 MB.
- `maintenance_work_mem` is a sixteenth of the memory, from 32 MB to 2 GB; `max_wal_size` equals
  `shared_buffers`, from 128 MB to 8 GB.
- `max_worker_processes` is two per vCPU plus 8, never below 16 (pg_cron workers).
- The pool size of the project's Supavisor tenant is 40% of `max_connections`, rounded down to 5.
  `default_max_clients` is hosted's pooler client limit, held under `[fleet] pooler_max_client_conn`. A
  pool size or client limit that someone saved wins.
- CPU quota is 100% per core: 1 core for Nano to Small, 2 for Medium and Large, then the vCPU count.
- Memory and CPU become the project's `Limits` (systemd `MemoryMax` and `CPUQuota`) on each of its
  units. The cap bounds each unit, so a project whose GoTrue and PostgREST grew to the cap as well would
  hold up to three times its size; they use about 12 and 9 MB idle, and the capacity check counts one
  cap per project.

**Saved Postgres settings win**, as hosted's custom config wins over its generated one. A saved
`shared_buffers` stays when a project changes size, so a resize is refused when the saved Postgres or
pooler settings would not pass validation under the new size. `work_mem` is on the command line with the
other sizing settings, so a saved `work_mem` takes effect at the next restart.

**Names.** Studio's `infra_compute_size` values are the registry names above; the add-on variant (`addon_variant` in
the Management API) is `ci_<size>`, for example `ci_2xlarge`. `ParseSize` also takes hosted's titles (`XL`, `2XL`) and `default` (what a manifest
of an earlier version calls Micro). Sizes above 16XL are not offered.

**Node capacity.** A size is a cap, not a reservation, and an idle project uses a small part of it
(docs/reference/footprint.md: about 65 MB PSS, or 150 MB with page cache, against Micro's 1 GB), so the
node promises more memory than it has.

- `[compute] overcommit` (default 6, chosen so that the budget covers the idle projects docs/guide.md
  says a node holds: 48 Micro caps on 8 GiB, 96 on 16 GiB, 192 on 32 GiB) is the ratio: the memory caps
  of the projects that count may sum to the node's memory times the ratio. A size whose vCPUs exceed the
  node's cores is never offered.
- A create that names its size (`CreateRequest.Class`; a create without one gets Micro unchecked), a
  resize and a resume are refused with `*CapacityError` (400 through the API) when the node cannot
  honor them. Branch creates and restores as a new project always name their size, so they are judged
  like `--size`. Going down never needs room.
- Projects that count: every one but the system project, a paused one (`INACTIVE`; it holds no room, so
  `Resume` judges its cap and cores like an upsize; the restart recovery of a daemon is not judged), a
  failed one and a removed one.
- `[compute] node_memory` and `node_cpus` override what is read from `/proc/meminfo` and the machine,
  for VMs whose view is the host's. Without `Engine.SetNode` (tests) nothing is checked.
- `supavise projects sizes` and the dashboard's size list use `Engine.Offers`, and `supavise status`
  shows the headroom.

**Resize** (`resize.go`; `Engine.BeginResize` and `Run`, or `Resize` for both):

1. Under the project's lock, taken without waiting (another operation running, in this process or
   another, is `ErrInvalidState`, 409 through the API): the project must be `ACTIVE_*` or paused. The
   saved Postgres and pooler settings are validated against the new size; one that does not fit refuses
   the resize with a 400 that names it (`*SettingsError`). The capacity check and the registry write are
   one step under a node-wide lock (the Engine's mutex and, with the Postgres registry, an advisory
   lock, so the daemon and the CLI cannot both take the last room).
2. The registry gets the new size and limits and the status `RESIZING` (`project.resize_started`).
   `BeginResize` returns here; the Management API runs `Run` in the background and the dashboard polls.
3. `Run` asks the shared services to let go of the database, stops the project's units, renders and
   starts them on the new size, waits for PostgreSQL, GoTrue and PostgREST to answer a real request, and
   updates the Supavisor tenant with the size's pool. Only this project restarts. The status returns to
   `ACTIVE_HEALTHY` and `project.resized` is recorded.
4. Any failure puts the previous size back: the record, the units and the tenant. The project stays
   `RESIZING` until the old units are back, then it is `ACTIVE_HEALTHY` on the old size and
   `project.resize_failed` carries the cause. Downsizing a project that holds more replication slots than
   the smaller size allows is the case that fails: Postgres refuses to start. If the old size does not
   come back either, the project is `ACTIVE_UNHEALTHY`.
5. A paused project only gets the new size in its record; `Resume` renders the units from it.

**Studio** changes the size with a `POST` of `{addon_type, addon_variant}` to `billing/addons` (routes in
`internal/api/README.md`), then sets `RESIZING` itself and polls. It adds a Nano card when the API lists
none and locks it on any plan but free unless the project is Nano already (supavise reports the plan
`enterprise`), so going down to Nano is a CLI or API step (`supavise projects resize <ref> --size nano`,
or removing the add-on). The cards have no disabled state, so sizes the node cannot give are left out of
the list; `supavise projects sizes` has them with the reason.

## System project

`InitSystem` is the same code path with ref `system` on `[ports] system_postgres` and
`system_gotrue`, class `system`, and no PostgREST. It creates the master key, starts the cluster,
creates databases `supavise`, `_supavisor`, `_realtime` and `_storage`, applies the registry migrations,
records the system project and its sealed credentials, and starts GoTrue for Studio sign-in
(`GOTRUE_SITE_URL` is `https://studio.<domain>`). Each of the last three databases belongs to its own
login role (`FleetRoles`: `supavise_supavisor`, `supavise_realtime`, `supavise_storage`), which owns the
database and the same-named schema, has no other access, and whose password is sealed in the registry
(`Engine.FleetCredentials`); fleet services connect with those, not as `supabase_admin`.
`supabase_storage_admin` may also use `_storage`.

`InitSystem` is idempotent. If the first-time init fails before the credentials are in the registry, it
deletes what it created. If the process dies mid-init, the next run notices (the launcher's init-pending
witness, or a cluster with no registry, no system credentials and no projects), removes that cluster and
starts over; a registry that holds projects but lost the system credentials is left alone and the error
says how to recover. `Open` connects to an initialized node for everything else; `RegistryDSN` is the DSN
other packages use to reach the registry.

**Dashboard SSO in the system GoTrue.** With `PlaneOptions.SystemAuth` (set by `Open` and `InitSystem`)
`supavise-gotrue@system` gets `GOTRUE_SAML_ENABLED=true` and a signing key of its own (the system secret
`saml_private_key`, created on first use), and its before-user-created hook points at the daemon's
loopback listener (`sso.HookURL`, signed with a secret derived from the master key).
`GOTRUE_DISABLE_SIGNUP` is `false` there, because GoTrue creates an SSO user by signing them up. Sign-up
stays closed because the hook refuses everything the daemon does not vouch for (`internal/api`, Single
sign-on), and GoTrue creates nobody while the daemon does not answer. Without `SystemAuth` (tests) sign-up
is closed. The daemon renders the unit again at every start and restarts it only when the files changed
(`RefreshSystemAuth`), so a node that is upgraded, or whose `[mail]` changed, picks the new environment
up without `supavise system init`.

Choices worth knowing:

- `wal_level` stays `logical` (the artifact's value): Realtime's `postgres_changes` needs it, and it is a
  superset of `replica`, so archiving and base backups are unaffected.
- `API_EXTERNAL_URL` is `https://<ref>.api.<domain>/auth/v1`, as hosted projects and the dockerless CLI
  set it. GoTrue resolves its mailer paths against that URL with an absolute path, which would drop
  `/auth/v1`, so `GOTRUE_MAILER_URLPATHS_{INVITE,CONFIRMATION,RECOVERY,EMAIL_CHANGE}` are set to
  `<API_EXTERNAL_URL>/verify`. Once the project has an active custom hostname, or else a vanity
  subdomain (`internal/domains`), the host is that one instead, which also moves the OAuth redirect URIs
  and the SAML endpoints, as on hosted. `GOTRUE_JWT_ISSUER`, `GOTRUE_SITE_URL` and PostgREST's OpenAPI
  URI keep the derived values, so adding or removing a domain invalidates no session.
- `GOTRUE_MAILER_AUTOCONFIRM=true` until SMTP is configured; nobody could confirm an address otherwise.
- `DataSeeder` fills `<project>/postgres/data`; `postmaster.opts` is created if missing because
  `pg_basebackup` omits it and the launcher refuses a data directory without it.
- Unix socket paths are limited to about 100 bytes, so on macOS and in tests the state directory must be
  short; the plane refuses a longer one with a clear error.

## Tests

The `test` job of the `ci` workflow runs `go test ./internal/lifecycle/` against fakes. The integration
tests (`go test -run Integration -v ./internal/lifecycle/`) run the create, health, SQL, pause, resume,
rotate, delete cycle against real artifacts with the exec backend. They need unpacked slim-services
artifacts (`postgres-17*`, `auth-*`, `postgrest-*`) in the directory `SUPAVISE_TEST_UNPACKED` names
(default `~/.cache/sbctl/unpacked`). The linux jobs `systemd-smoke`, `compute-smoke`, `settings-smoke`
and `upgrade-smoke` run the systemd, compute size, saved settings and upgrade flows.

## Limits

- `Recover` does not finish an interrupted restore (`RESTORING`): the project stays `RESTORING`, the data
  directory the restore moved aside is `<dir>.pre-restore-<time>`, and an operator decides
  (`internal/backup/README.md`, "Restore"). A delete interrupted by a version that recorded no events
  (`GOING_DOWN` with no `project.delete_started`) is only logged.
- `pg_hba.conf` is rewritten on start but a changed file is not reloaded in a running cluster.
- Fleet tenant calls are a hook (`Options.Fleet`).
- Upgrades across Postgres major versions (`pg_upgrade`): the eligibility answer says there is no upgrade
  path. Upgrades never touch the shared services (Supavisor, Realtime, Storage, pgmeta, Studio, the edge
  runtime); they move with the node.
- `Usage` reports disk bytes and unit memory only.
