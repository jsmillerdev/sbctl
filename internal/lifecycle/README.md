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
   (mode 0700), `hba_file`, `wal_level=logical`, `archive_mode=on`,
   `archive_command` and `archive_timeout`, `PlaneOptions.ArchiveCommandFor` and `ArchiveTimeout`: `internal/app` passes `backup.ArchiveCommandFor(cfg, ref, config file)` (shell-quoted, `%` doubled; the relay form `--socket <state>/projects/<ref>/wal/r.sock` under systemd, otherwise `--config` when the daemon loaded a non-default file; the relay form also drops `SUPAVISE_CONFIG` from the unit's environment) and `[backup] archive_timeout_seconds` (default 300); without them the plane falls back to a built-in quoting of `bin_path` and 900 s,
   and the project's compute size (see Compute sizes): `shared_buffers`, `effective_cache_size`,
   `work_mem`, `maintenance_work_mem`, `max_wal_size`, `max_connections`, `max_worker_processes`,
   `max_wal_senders` and `max_replication_slots`. A new project is Micro: 256MB `shared_buffers`, 60 connections, a 1 GB memory cap.
   The launcher initializes PGDATA and runs the artifact's roles and migrations once.
   Role passwords of `postgres`, `supabase_admin`, `authenticator`,
   `supabase_auth_admin`, `supabase_storage_admin` and `supabase_replication_admin` are set
   afterwards over the unix socket as SCRAM-SHA-256 verifiers computed in Go (after SASLprep, as libpq and Postgres do) (the artifact
logs DDL, so a plaintext `alter role` would land in journald), with statement logging also
silenced for that session. A running cluster whose rendered settings changed is restarted by
`StartDatabase`. `POSTGRES_PASSWORD` (the superuser password the launcher reads on the first boot only) is in `postgres.env` only while the data directory is not initialized (no `PG_VERSION`, or the launcher's init-pending witness exists); once the role passwords are set, `createDatabase` renders the unit again without it and restarts the cluster, so it is not in the postmaster's environment for the life of the cluster. `CreateRequest.Seed` replaces initdb and role setup and
requires `CreateRequest.Keys` (the seeded cluster's own credentials; fresh ones would not
match its passwords).
3. GoTrue (`bin/auth migrate`, then `bin/auth`) and PostgREST, each health-checked with a
   real request (`/health`, `/`).
4. `fleet.Fleet.EnsureTenant` (skipped while the fleet is empty), `PutRoute` for
   `<ref>.api.<domain>`, the nightly backup timer (`Options.Timers`, systemd backend only), status `ACTIVE_HEALTHY`.

Any failure after the row exists stops and removes units and data, removes tenants and
route, and leaves the row `INIT_FAILED` with an event that carries the cause. Cleanup uses
a context that outlives the cancelled request. The one exception is `ErrClusterExists`
(the project directory already holds a cluster, for example an orphan after a registry
restore): Create refuses before writing anything, leaves the data alone and forgets the row.

Mutating operations on one ref are serialized by a mutex in the process and, with the
Postgres registry, by a session-level advisory lock on a dedicated connection, so the CLI
and the daemon cannot run, say, delete and resume on the same project at once.

`pg_hba.conf` is ours, not the artifact's: the artifact trusts all loopback connections,
which on a shared host means every local user. Ours trusts `supabase_admin` on the unix
socket only (the directory is private to the `supavise` user) and requires scram-sha-256 on
TCP. The socket is how Supavise reaches its own registry before it can decrypt any secret.

`pg_cron` runs its jobs in background workers of the cluster (`cron.use_background_workers=on`,
`cron.database_name=postgres`, `cron.max_running_jobs=8`, `max_worker_processes=16`; see
`cronSettings`; every size keeps that worker count as a floor, `MinWorkerProcesses`). Hosted Supabase leaves pg_cron on its default, a libpq connection to localhost
that its pg_hba.conf trusts; ours does not trust a loopback connection, so a libpq job would fail
with "connection failed". Workers need no connection, no pg_hba rule and no network, which also
keeps them working behind a branch's egress filter and independent of the `nodename` and `nodeport`
stored in `cron.job`. A saved `max_worker_processes` overrides the 16 (it is a command-line setting
like the class's sizing, applied at the next restart); the Postgres settings save refuses a value below
10, which is `cron.max_running_jobs` plus two (pg_cron's launcher and pg_net's worker each hold a slot).
A value of 4 to 9 that was saved before that rule stays in force, and the project then runs fewer
concurrent jobs: a job that cannot get a worker fails to start. A project that already runs picks the
settings up the next time the daemon starts (`StartActive` renders the unit again and restarts a
running cluster whose rendered settings changed), or on resume (a Postgres settings save also renders it and reports a
pending restart); no migration is needed.

## Other operations

- `Pause`: the shared services are asked to let go of the project's database (`fleet.Quiescer`), then PostgREST, GoTrue, PostgreSQL stop in that order; `INACTIVE`; route and tenants
  stay. `Resume` reverses it; on failure what started is stopped and the project stays
  `INACTIVE`. Once the cluster answers, `Resume` also applies what no unit renders and what
  was saved while paused: the Postgres settings applied with `ALTER SYSTEM` and the Storage and
  Realtime tenant settings (`EnsureTenant`). A failure there is logged and recorded as
  `project.config_apply_failed`; it does not undo the resume.
- `Delete`/`DeleteWith`: final base backup through `BaseBackuper` (the `FinalBackuper` method
  when it has one, so the manifest says "final"; skipped when nil, for `INIT_FAILED` projects
  and with `SkipFinalBackup`; a paused project's database is started just for it), tenants,
  route, units, data, the project's Storage objects directory (`<state>/system/storage/objects/stub/<ref>`, removed after the data and only when the record goes too), registry row. A failed backup keeps the project, except one that wraps
  `ErrNoRestorableState` (a restore-as-new clone whose recovery failed or never finished and
  that has no base backup): there is nothing to back up, the delete records
  `project.final_backup_skipped` and goes on. A delete records `project.delete_started` (with
  the status to go back to) and `project.delete_backup_done` (the backup step is settled); the
  nightly timer stops right after it. A failure after the backup leaves `GOING_DOWN`, and a
  repeated delete resumes from the recorded step without a second backup.
- `RotateKeys`: new JWT secret, legacy and opaque keys; database passwords unchanged;
  GoTrue and PostgREST restart, fleet tenants update; previous keys are restored on error. A revoked
  default key comes back as the new default key (the rotation replaced its value), so rotating
  never leaves a project without a usable default pair.
- `Health`: unit state plus SQL ping, GoTrue `/health`, PostgREST `/`; moves
  `ACTIVE_HEALTHY` and `ACTIVE_UNHEALTHY` to match.
- `StartActive`: starts every active project (after a reboot or `supavise system stop`). It lists the projects first and starts them one at a time; each start re-reads the project after taking its lock, so a pause or delete that landed in between is not undone.
- `PostgresPlane.prepare` also creates `projects/<ref>/wal/` (the relay directory the Postgres unit bind-mounts read-only) and calls `PlaneOptions.ArchiveReady(ref)` so the daemon serves that project's WAL relay socket before the cluster starts.
- `Recover`: run once by the daemon before `StartActive`. A crash in the middle of an
  operation leaves a project in a status nothing else would move: `PAUSING` becomes
  `INACTIVE` (units stopped), `COMING_UP` or `RESTARTING` with a route (a resume or restart
  was cut short) becomes `INACTIVE`, and `COMING_UP` without a route (a create that never
  finished) becomes `INIT_FAILED`. `GOING_DOWN` is read from the delete's events: with the backup
  step settled the removal is finished at once (no second backup); cut off during the backup,
  the project returns to the status it had (a paused one is stopped again); with no record (an
  older version) it is only logged, naming `--skip-final-backup`. A restart (the API's pause and
  resume, bracketed by `project.restart_requested` and `project.restart_finished`) cut off after
  the pause is flagged `Recovered.Resume`, and `ResumeRecovered` brings the project back; a stale
  request on a project that is not paused is cleared. `RESTORING` is not touched. Each move is a
  `project.recovered` event. `UPGRADING` is stopped and set `ACTIVE_UNHEALTHY` when nobody runs its
  upgrade any more, and left alone while another process (the CLI) holds it (see "Service versions
  and project upgrades").
- In-place restore (`restore.go`): `Engine.BeginRestore` (the optional `DatabaseRestorer` capability of the Manager; the Management API's backup routes use it) moves an `ACTIVE_*` or `RESTORE_FAILED` project to `RESTORING` under the project lock and returns a handle whose `Run` calls the backup service's `RestoreInPlace` (`InPlaceRestorer`, found through `Options.Backup`; the late backuper checks beforehand that the backend opens, `ErrNoRestorer` otherwise). The service pauses and resumes the project through this Engine with a context marked as a restore, and `Pause` and `Resume` then accept `RESTORING` and leave the status alone, so the project stays `RESTORING` from the first call to the end. `BeginRestore` first checks that the disk can hold the restored copy next to the current data (`ErrInsufficientDisk`, 409 through the API; skipped when the free space cannot be read). After a restore that worked, `Run` sets the cluster's role passwords to the registry's again (`rolePasswordPlane`, which `PostgresPlane` implements; a failure is logged and recorded as `restore.passwords_failed`, the restore stands) and prunes the leftovers of earlier restores (`restore_space.go`): the newest `data.pre-restore-*` stays, older ones and all `data.failed-restore-*` go; after a failure only older `data.failed-restore-*` go. `Run` settles it: `ACTIVE_HEALTHY` after a restore, `RESTORE_FAILED` after a failure, whether or not the original data is running again (a status that went back to `ACTIVE_HEALTHY` would look like a restore that worked to Studio's restore screen and to API clients). `Health` leaves `RESTORE_FAILED` alone; another restore, `Pause` (then `Resume`, which is what a restart does) and `Delete` move the project on, and a `Pause` that fails keeps the status. Every other operation refuses a `RESTORING` project (`ErrInvalidState`), and `Delete` also refuses one whose restore runs in this process; a project left `RESTORING` by a stopped daemon can be deleted. `restore.requested` is the event.
- Backup timers: with the systemd backend the Engine starts `supavise-basebackup@<ref>.timer` when
  a project becomes active (create, resume, start) and stops it on pause and delete
  (`Options.Timers`; failures are logged, never fatal). The timers are not enabled for boot;
  `supavise serve` starts the system project's timer and the prune timer at boot.
- Region: `CreateRequest.Region` is kept when it is an AWS region code, otherwise the
  configured `region` (default `us-east-1`) is stored: Studio and the CLI resolve the project
  region against a list of real regions, and an unknown one breaks the project list.

## Service versions and project upgrades

A project runs the versions in `registry.Project.Versions` (service name to slim-services release
tag; `postgres`, `gotrue`, `postgrest`). A new project records the node's pins
(`internal/versions/versions.yaml`). The plane renders the project's Postgres, GoTrue and PostgREST
units from the project's own versions (`PostgresPlane.artifactDir`, through `artifacts.Store.DirFor`),
so a Supavise release that moves the pins changes no running project. A service the project
recorded no version for runs the node's pin, and the system project always runs the pins: it
belongs to the node. A version whose artifact is not on disk fails the render with
`ErrNotFetched` and names `supavise projects upgrade <ref>`. Artifacts stay on disk while a project
runs them: `Engine.CollectArtifacts` (`supavise artifacts gc`, and after a successful
`projects upgrade`) removes only what no project runs or is being upgraded to, what the node
pins, what the newest recorded release pinned (what the daemon last started with, whatever
`keep_releases` says: the CLI that runs the collection may be a newer binary than the daemon), and
what the last `[upgrade] keep_releases` releases of the node's release history pinned
(`artifacts.Store.RecordPins`, written at daemon start; default 3: the current release and the
two before).

`Engine.UpgradeProject(ref, target)` (`BeginUpgrade` + `Run`; the `ProjectUpgrader` capability of
the Manager, which the Management API and the CLI use) moves a project to other versions, the
node's pins by default, on the same data directory. It follows hosted Supabase's flow (Owner or
Administrator asks, the project is `UPGRADING` with a tracking id and a progress Studio draws, a
failure brings the original back online) but is a restart on other binaries, not a `pg_upgrade`
onto a new instance: the three services change by minor release inside one Postgres major version,
and that does not change the on-disk format.

1. `BeginUpgrade`, under the project's lock: only `ACTIVE_HEALTHY` projects, a node with a backup
   service, something to change, no other Postgres major version and no service that would move to
   an older release than the project runs (`ErrInvalidState`,
   `ErrNoBackupEngine`, `ErrUpgradeNotNeeded`, `ErrUpgradeUnsupported`; nothing changes on a
   refusal). It claims the upgrade of the ref (a session-level advisory lock on a connection of its
   own, `supavise:upgrade:<ref>`, held until `Run` ends; the lock goes when the process dies),
   writes the `registry.Upgrade` row (the tracking id; `project_upgrades`), sets `UPGRADING` and
   records `project.upgrade_started`. Other operations on the ref fail at once with
   `ErrInvalidState` while the upgrade runs in this process (`upgradeBusy`), and a second runner
   in another process is refused.
2. `Run`: the target artifacts are fetched and verified (`FetchTag`; progress `1_started`); when
   the Postgres release changes, the installed extensions are checked against it (see
   "Extensions"); a base backup with the reason `pre-upgrade` is taken while the project serves
   (`2_...`). Any of them failing ends the upgrade before any service is touched: fail closed, the
   project returns to its status, the upgrade row says `failed` with the stage's error code.
3. Under the lock: the extension check again (a database may have changed during the backup). The
   nightly backup timer stops. With a new Postgres release the shared services let
   go of the database, all three units stop and start again from the target versions (database
   first); otherwise only GoTrue and PostgREST restart (`Runner.Reconfigure`) and the cluster keeps
   serving. Each unit answers a real request before the next starts (GoTrue runs its migrations
   first), then `Runner.Health` checks every service and, after a Postgres change, every
   installed extension's code must load (`VerifyExtensions`).
4. One `UpdateProject` records the target versions and `ACTIVE_HEALTHY`; the status row says
   `9_completed_upgrade`, status 1.

A failure in step 3 renders the previous versions again (recorded explicitly, so a project that had
none does not float onto the release that just failed), starts and health checks them, and
records `project.upgrade_failed`, the status row (status 2, the error code of the stage, the
cause) and the log message `upgrade_failed`. If the rollback fails too the project is
`ACTIVE_UNHEALTHY` and the error names the pre-upgrade backup; its nightly backup timer starts
again whenever PostgreSQL itself runs after the rollback, whatever the API units do (otherwise the
status row says the scheduled backups stay paused until the project restarts). **The data directory is not
restored by a rollback.** A Postgres, GoTrue or PostgREST minor release does not change the format
of the files, so the previous binaries read what the new ones left. GoTrue's (and Storage's)
migrations run when the service starts and only go forward, so an older GoTrue can meet a schema a
newer one migrated: the pre-upgrade base backup is the way back for the data
(`supavise backups restore`), and its id is in the error, the events and the status row.

Who runs an upgrade can die: the daemon restarts, or the CLI's process is killed (`projects
upgrade` ignores SIGHUP and SIGPIPE, so a dropped SSH session does not end it, but SIGKILL and the
OOM killer can). The claim tells a live runner from a dead one. A project that is `UPGRADING` with
the claim free lost its runner: `Recover` (at the daemon's start, before `StartActive`) and
`SettleUpgrades` (the daemon calls it every 2 minutes) stop its units, mark the upgrade failed and
set `ACTIVE_UNHEALTHY`, and start it on the recorded, previous versions. An upgrade that died
before it touched a unit (progress before `4_attached_volume_to_original_instance`: the artifact
fetch, the base backup) only has its row marked failed and the project set `ACTIVE_HEALTHY`:
nothing is stopped. While the claim is held
they leave the project alone, so a daemon restart during a CLI upgrade's base backup does not stop
a serving project. The same pass closes an upgrade row that still says running on a project that is
not `UPGRADING` (the process died after recording the versions, or after a rollback): `done` when
the project runs the row's target versions, `failed` otherwise. An operator needs to do nothing
beyond waiting for the next pass or restarting the daemon; with no daemon, `supavise serve` settles
the project when it starts. The pre-upgrade backup id stays in the events and the status row.

### What a node upgrade adds

`supavise upgrade` (`internal/nodeupgrade`) drives `Engine.UpgradeProject` through the CLI: `projects upgrade --all --to <service>=<tag>...` moves a project only to the releases named (a service it does not name stays; `--include-postgres` is how the node upgrade names PostgreSQL), and three more request fields serve it. `UpgradeRequest.Target` and `UpgradeEligibilityFor(ref, request)` plan for a target other than the node's pins. `AllowOlder` (`--allow-older`) lets the target be older than what the project runs: the one move to an older release that is allowed, used by `supavise rollback` to put the projects an upgrade moved back on the releases they ran (tags that cannot be ordered stay refused). `ReuseBackupSince` (`--reuse-backup-since`, hidden) lets the upgrade take as its pre-upgrade backup a completed base backup of the project that finished after that time instead of taking another: the node upgrade backs everything up first, and a base backup plus the archived WAL after it restores a project to any moment up to the upgrade.

Two changes make a release take effect on units that are already running. `startAPI` (a project's GoTrue and PostgREST, at every start) renders the units and stops a running one whose files changed, so that the `Start` after it runs the new files (it used to leave the old process running: `Start` on a running unit is a no-op); an unchanged unit is left alone. And `Engine.EnsureTenants` registers every active project with Supavisor, Realtime and Storage again, one at a time; the tenants' fingerprints include the release tag of Storage and Realtime (`internal/fleet`), so after a release moved either of them the call sends each project's tenant once more, which is what runs the new release's tenant migrations in every project's database (Storage on the tenant update, Realtime on the tenant create). The daemon calls it once the shared services and the projects have started; with nothing changed it asks each service and sends nothing.

### Extensions

A Postgres upgrade swaps the binaries under the same data directory, and the extensions created in
the project's databases keep their catalog version and the shared library their functions name. A
release that drops either leaves an extension broken while the cluster starts and answers, so
`extensions.go` checks, only when the Postgres release changes:

- Before anything is touched (in `UpgradeEligibility`, again after the artifact is fetched, and again
  under the lock): every extension of every database (`Plane`'s optional `ExtensionInspector`;
  `PostgresPlane` reads `pg_extension` of each database that accepts connections) must have a control
  file in the target artifact, a library (`module_pathname`, or `<extension>-<version>` for extensions
  built in versioned shared-object mode such as `wrappers`), and its version's own script or a chain
  of update scripts to the release's default version (`CheckExtensionFiles`). A finding is a blocker
  of type `unsupported_extension`, which Studio shows in its list of issues to resolve; the message
  tells the owner to run `ALTER EXTENSION <name> UPDATE` while the project still runs its current
  release. Planning only notes a release that is not on disk yet or a cluster that does not answer;
  the upgrade itself refuses in both cases.
- After the new cluster starts and before the versions are recorded: `VerifyExtensions` runs
  Postgres's own C-function validator (`fmgr_c_validator`) on every C function an extension owns,
  which loads the library and looks up the symbol. A failure rolls the upgrade back.

An upgrade never runs `ALTER EXTENSION UPDATE`: extensions keep their versions, and the owner
updates them once the project is on the new release. A GoTrue or PostgREST upgrade does not touch
the databases' extensions and skips these checks.

Progress values and what they mean here (hosted's names describe a `pg_upgrade` onto a new
instance; Studio draws them under its own labels): `0_requested` accepted, `1_started` artifacts,
`2_launched_upgraded_instance` backup running, `3_detached_volume_from_upgraded_instance` backup
done, `4_attached_volume_to_original_instance` stopping services, `5_initiated_data_upgrade`
starting PostgreSQL, `6_completed_data_upgrade` starting GoTrue and PostgREST,
`7_detached_volume_from_original_instance` health check, `8_attached_volume_to_upgraded_instance`
recording, `9_completed_upgrade`. Error codes: `1_upgraded_instance_launch_failed` (artifacts),
`4_data_upgrade_initiation_failed` (the backup), `5_data_upgrade_completion_failed` (stop or start),
`8_upgrade_completion_failed` (health gate, recording, an interrupted upgrade).

`Engine.UpgradeEligibility` answers for the node's pins: current and target versions, the changes
and whether PostgreSQL restarts, blockers (not `ACTIVE_HEALTHY`, another Postgres major version,
a service the project runs a newer release of than the target (`Ahead`; `CompareTags` orders the
tags by upstream version, then packaging revision, and tags it cannot order are refused too),
no backup service, the system project, an extension the target release cannot serve), an
estimated downtime (about 3 minutes without PostgreSQL restarting, 15 when it restarts; the base
backup does not count, the project serves during it) and notes. `Rollout` (`rollout.go`) runs many upgrades for `supavise projects upgrade --all`:
`[upgrade] canary_projects` (default 1) one at a time, then `[upgrade] batch_size` (default 5)
at once, halting at the first failure.

## Branches

`CreateRequest.Branch` (`*registry.BranchInfo`) makes the new project a branch: the registry row is
written with it, so a branch is a branch from the moment it exists. `internal/branching` builds on
that. `DeleteWith` refuses (`ErrInvalidState`, before it stops anything) while the project has branches.
`DeleteOptions.KeepRecord` removes the fleet tenants, route, units and data but keeps the registry
row and its sealed secrets, which ends `INIT_FAILED`; `CreateRequest.Recreate` builds a new cluster
over such a row (same ref, sequence number, name and branch info; the row must be `INIT_FAILED`). A
branch reset uses the pair so that a failure after the old cluster is gone leaves the branch
registered instead of gone.

## Saved settings

`Settings` (implemented by `projectconfig.Manager`, built by `Open` and `InitSystem` as
`Node.Settings`) is read whenever units and tenants are rendered: GoTrue's and PostgREST's
environment (saved values laid over the base environment; an empty value removes a variable),
the cluster's server arguments (only the settings that overlap the class's sizing; see
`SplitPostgresSettings`), and the Storage and Realtime `TenantSpec`. A project that never saved a
setting renders exactly as before. A settings source that cannot be read fails the render rather
than falling back to defaults. The system project never reads user settings.

`Engine.ApplyConfig(ref, service, opts)` (the `Reconfigurer` capability the Management API uses)
makes a saved change take effect and touches only what the service owns: `ReconfigureService`
re-renders both API unit files but restarts only GoTrue or PostgREST and waits for it to answer;
Realtime and Storage get `EnsureTenant`; Postgres goes through `ApplyPostgresSettings`
(`ALTER SYSTEM` for the settings not on the command line, a reset for those no longer saved,
`pg_reload_conf`, and a restart when asked and needed: the whole project restarts like a pause and
a resume, because the GoTrue and PostgREST units are bound to the cluster's, after the shared
services were asked to let go of its database; `pg_settings.pending_restart` and a diff of
the command-line settings against the running ones say whether one is pending). A paused project
is not started, but its units are rendered once from the saved settings (`CheckRender`), so a value
a unit cannot carry fails the save and not the resume; a project in a transitional status is
refused (`ErrInvalidState`).

`ApplyOptions.Recover` is what the API sets on the apply that undoes a failed one. For Postgres,
`RecoverPostgres` first checks that the cluster answers. A cluster that does not (a restart that
failed on a value Postgres accepted in `ALTER SYSTEM` and cannot start with) is stopped, every
setting of the schema is removed from `postgresql.auto.conf` offline, and the project starts on
the restored settings; the apply that follows writes them again with `ALTER SYSTEM`.

`Engine.SetDatabasePassword(ref, password)` sets the `postgres` role's password over the unix
socket as a SCRAM verifier, seals it, and calls `fleet.RefreshTenant` (Supavisor's terminate,
which clears its pools and cached credentials); it restores the old password if the registry write
fails and only warns when the pooler cannot be reached.

## Compute sizes

A project has a compute size, the way a hosted project has a compute add-on. `registry.Project.Class` holds
the size's name; `classes.go` is the table. Hosted publishes the memory, the vCPU count, the connection
limits, the replication limits and the pooler client limit of each size
([compute and disk](https://supabase.com/docs/guides/platform/compute-and-disk)); those columns are
copied. It generates the Postgres settings per size with a closed tool (`supabase-admin-api optimize db`)
and documents none of them, so those columns are ours, derived from the published ones by the rules below.

| Size | Add-on variant | CPU | MemoryMax | CPUQuota | max_connections | slots and WAL senders | pooler clients | pool size |
|---|---|---|---|---|---|---|---|---|
| `nano` (Nano) | `ci_nano` | 1 vCPU shared | 512M | 100% | 60 | 5 | 200 | 20 |
| `micro` (Micro) | `ci_micro` | 1 vCPU shared | 1G | 100% | 60 | 5 | 200 | 20 |
| `small` (Small) | `ci_small` | 1 vCPU shared | 2G | 100% | 90 | 5 | 400 | 35 |
| `medium` (Medium) | `ci_medium` | 2 vCPU shared | 4G | 200% | 120 | 5 | 600 | 45 |
| `large` (Large) | `ci_large` | 2 vCPU dedicated | 8G | 200% | 160 | 8 | 800 | 60 |
| `xlarge` (XL) | `ci_xlarge` | 4 vCPU dedicated | 16G | 400% | 240 | 24 | 1000 | 95 |
| `2xlarge` (2XL) | `ci_2xlarge` | 8 vCPU dedicated | 32G | 800% | 380 | 80 | 1500 | 150 |
| `4xlarge` (4XL) | `ci_4xlarge` | 16 vCPU dedicated | 64G | 1600% | 480 | 80 | 3000 | 190 |
| `8xlarge` (8XL) | `ci_8xlarge` | 32 vCPU dedicated | 128G | 3200% | 490 | 80 | 6000 | 195 |
| `12xlarge` (12XL) | `ci_12xlarge` | 48 vCPU dedicated | 192G | 4800% | 500 | 80 | 9000 | 200 |
| `16xlarge` (16XL) | `ci_16xlarge` | 64 vCPU dedicated | 256G | 6400% | 500 | 80 | 12000 | 200 |

| Size | shared_buffers | effective_cache_size | work_mem | maintenance_work_mem | max_wal_size | max_worker_processes |
|---|---|---|---|---|---|---|
| `nano` | 128MB | 384MB | 4MB | 32MB | 128MB | 16 |
| `micro` | 256MB | 768MB | 4MB | 64MB | 256MB | 16 |
| `small` | 512MB | 1536MB | 5MB | 128MB | 512MB | 16 |
| `medium` | 1GB | 3GB | 8MB | 256MB | 1GB | 16 |
| `large` | 2GB | 6GB | 12MB | 512MB | 2GB | 16 |
| `xlarge` | 4GB | 12GB | 17MB | 1GB | 4GB | 16 |
| `2xlarge` | 8GB | 24GB | 21MB | 2GB | 8GB | 24 |
| `4xlarge` | 16GB | 48GB | 34MB | 2GB | 8GB | 40 |
| `8xlarge` | 32GB | 96GB | 64MB | 2GB | 8GB | 72 |
| `12xlarge` | 48GB | 144GB | 64MB | 2GB | 8GB | 104 |
| `16xlarge` | 64GB | 192GB | 64MB | 2GB | 8GB | 136 |

How the derived columns follow from the published ones (`newSize`):

- `shared_buffers` is 25% of the memory and `effective_cache_size` 75%, PostgreSQL's usual guidance.
- `work_mem` is the memory left after `shared_buffers`, divided by three times `max_connections`, from 4 MB to 64 MB.
- `maintenance_work_mem` is a sixteenth of the memory, from 32 MB to 2 GB; `max_wal_size` equals
  `shared_buffers`, from 128 MB to 8 GB.
- `max_worker_processes` is two per vCPU plus 8, never below 16: pg_cron runs its jobs in background workers
  (`cronSettings`), and its launcher and pg_net's worker take two slots.
- The pool size of the project's Supavisor tenant is 40% of `max_connections`, rounded down to 5: the share
  hosted advises when PostgREST is in heavy use. `default_max_clients` is hosted's pooler client limit,
  held under `[fleet] pooler_max_client_conn`. A pool size or client limit that someone saved wins
  (`ApplyPoolDefaults`; the Management API reports the same defaults).
- CPU: hosted publishes only "shared" up to Medium and dedicated vCPUs from Large. The quota is 100% per
  core: 1 core for Nano to Small, 2 for Medium and Large, then the vCPU count of the size.
- The size's memory and CPU become the project's `Limits` (systemd `MemoryMax` and `CPUQuota`), applied to
  each of its units. The memory cap bounds each unit, so a project whose GoTrue and PostgREST grew to the
  cap as well would hold up to three times its size; they use about 12 and 9 MB idle, and the capacity
  check counts one cap per project.

**Saved Postgres settings win**, as hosted's custom Postgres config wins over its generated one. A saved
`shared_buffers` stays when a project changes size, so a resize is refused (see Resize, step 1) when the saved Postgres or pooler settings would not pass validation under the new size. `work_mem` is on
the command line with the other sizing settings (`cmdlineSettings`), so a saved `work_mem` takes effect at the
next restart like they do.

**Names.** Studio's `infra_compute_size` values are the registry names (`nano`, `micro`, `small`, `medium`,
`large`, `xlarge`, `2xlarge` ... `16xlarge`); the add-on variants are `ci_` plus the name. `ParseSize` also takes
hosted's titles (`XL`, `2XL`) and `default` (what a manifest of an earlier version calls Micro). Registry
migration 1250 renamed the classes of earlier versions: `micro` (16 MB `shared_buffers`, 30 connections) became
Nano, `default` became Micro, `small`, `medium` and `large` kept their names, and a row whose limits were still the
node default took the limits of its size. Sizes above 16XL are not offered.

**Node capacity.** A size is a cap, not a reservation, and an idle project uses a small part of it
(docs/research/09-footprint.md: about 65 MB PSS, or 150 MB with page cache, against Micro's 1 GB). The node
therefore promises more memory than it has. `[compute] overcommit` (default 6, chosen so that the budget covers the
idle projects docs/guide.md says a node holds: 48 Micro caps on 8 GiB, 96 on 16 GiB, 192 on 32 GiB) is the ratio: the sum of the memory
caps of the projects that count may reach the node's memory times the ratio, and a size whose vCPUs exceed the
node's cores is never offered. A create that names its size (`CreateRequest.Class`; a create without one gets Micro
unchecked), a resize and a resume are refused with `*CapacityError` (400 through the API, with the reason in the
message) when the node cannot honor them. Branch creates and restores as a new project always name their size (the
branch's, the original's), so they are judged like `--size`; the plain create is the one exception. Going down never needs room, so a node that is over its budget can still shrink a
project. Projects that count: every one but the system project, a paused one (`INACTIVE`, its units are down;
it holds no room, so `Resume` judges its cap and cores like an upsize, which keeps a resize into a paused project's room from being undone by resuming it; the restart recovery of a daemon is not judged), a failed one and a removed one. `[compute] node_memory` and `node_cpus` override what is
read from `/proc/meminfo` and the machine, for VMs whose view is the host's. Without `Engine.SetNode` (tests) nothing
is checked; `Open` and `InitSystem` set it. `Engine.Offers` lists every size with whether it fits;
`supavise projects sizes` and the dashboard's size list use it, and `supavise status` shows the headroom
(`Capacity.Summary`).

**Resize** (`resize.go`; `Engine.BeginResize` and `Run`, or `Resize` for both):

1. Under the project's lock, taken without waiting (another operation running on the project, in this process or
   another, is `ErrInvalidState`, 409 through the API): the project must be `ACTIVE_*` or paused. The saved Postgres and
   pooler settings are validated against the new size (memory cap, `max_connections`); one that does not fit refuses
   the resize with a 400 that names it (`*SettingsError`). The capacity check and the registry write are one step under
   a node-wide lock (the Engine's mutex and, with the Postgres registry, an advisory lock, so the daemon and a CLI
   `supavise projects resize` cannot both take the last room).
2. The registry gets the new size and limits and the status `RESIZING`, which Studio shows as "Resizing"
   (`project.resize_started`). `BeginResize` returns here; the Management API answers and runs `Run` in the
   background, and the dashboard polls the status.
3. `Run` asks the shared services to let go of the database (`quiesce`), stops the project's units, renders and starts
   them again on the new size (the `MemoryMax` and `CPUQuota` drop-ins, the server arguments, the saved settings on
   top), waits for PostgreSQL, GoTrue and PostgREST to answer a real request, and updates the Supavisor tenant with the
   size's pool. Only this project restarts. The status returns to `ACTIVE_HEALTHY` and `project.resized` is recorded.
4. Any failure puts the previous size back: the record, the units (stopped and started again on the old size) and the
   tenant. The project stays `RESIZING` until the old units are back (a daemon that stops meanwhile recovers a `RESIZING`
   project), then it is `ACTIVE_HEALTHY` on the old size, `project.resize_failed` carries the cause, and the error says
   so. Downsizing a project that holds more replication slots than the smaller size allows is the case that fails: Postgres
   refuses to start. If the old size does not come back either, the project is `ACTIVE_UNHEALTHY` and the error says that.
5. A paused project only gets the new size in its record; `Resume` renders the units from it. A resize to the size the
   project has changes nothing.

**What Studio calls** (the pinned tag, `components/interfaces/DiskManagement`, which the Compute and Disk page renders):
the project's size from `infra_compute_size` of `GET /platform/projects/{ref}`; the cards from `available_addons`
(type `compute_instance`) of `GET /platform/projects/{ref}/billing/addons`; the change as `POST` of
`{addon_type, addon_variant}` to the same path (it then sets the project's status to `RESIZING` itself and polls); disk
from `GET /platform/projects/{ref}/disk`, `/disk/util` and `/disk/custom-config`, and `POST` of `/disk` and
`/disk/custom-config`; `POST /platform/projects/{ref}/resize` is the older volume-size call. The page reads the
permission to update projects and the entitlement `instances.compute_update_available_sizes` (granted: supavise
has no plans). Two things Studio decides on its own: it adds a Nano card when the API lists none, and it locks that
card on any plan but free unless the project is Nano already (supavise reports the plan `enterprise`), so going down
to Nano is a CLI or API step (`supavise projects resize <ref> --size nano`, or removing the add-on). Its cards have no
disabled state of their own, so the sizes the node cannot give are left out of the list instead of shown disabled;
`supavise projects sizes` has them with the reason.

`Recover` (run at daemon start) finds a project left `RESIZING` by a stopped daemon, stops its units, sets
`INACTIVE` and flags it for `ResumeRecovered`, which starts it on the size the registry holds (the size the resize was
going to, because the record is written first).

## System project

`InitSystem` is the same code path with ref `system` on `[ports] system_postgres` and
`system_gotrue`, class `system`, and no PostgREST. It creates the master key, starts the
cluster, creates databases `supavise`, `_supavisor`, `_realtime` and `_storage`. Each of the
last three belongs to its own login role (`FleetRoles`: `supavise_supavisor`,
`supavise_realtime`, `supavise_storage`), which owns the database and the same-named schema,
has no other access, and whose password is sealed in the registry
(`Engine.FleetCredentials`); fleet services connect with those, not as `supabase_admin`.
`supabase_storage_admin` may also use `_storage`. It applies the registry migrations, records the system project and its
sealed credentials, and starts GoTrue for Studio sign-in (`GOTRUE_SITE_URL` is
`https://studio.<domain>`). It is idempotent. If the first-time init fails
before the credentials are in the registry, it deletes what it created. If the process
dies mid-init, the next run notices (the launcher's init-pending witness, or a cluster with
no registry, no system credentials and no projects), removes that cluster and starts over;
a registry that holds projects but lost the system credentials is left alone and the error
says how to recover.
`Open` connects to an initialized node for everything else; `RegistryDSN` is the DSN other
packages use to reach the registry.

**Dashboard SSO in the system GoTrue.** With `PlaneOptions.SystemAuth` (set by `Open` and `InitSystem`;
`systemAuth` in `system.go`) `supavise-gotrue@system` gets `GOTRUE_SAML_ENABLED=true` and a signing key of its own (the
system secret `saml_private_key`, created on first use), and its before-user-created hook points at the daemon's
loopback listener (`sso.HookURL`, signed with a secret derived from the master key). `GOTRUE_DISABLE_SIGNUP` is `false`
there: GoTrue creates an SSO user by signing them up, so it cannot stay on. Sign-up stays closed because the hook
refuses everything the daemon does not vouch for (internal/api, Single sign-on), and GoTrue creates nobody while the
daemon does not answer. Without `SystemAuth` (tests, a Secrets that cannot derive keys) the unit is rendered as before,
with sign-up closed. `PostgresPlane.RefreshSystemAuth` renders the unit again and restarts it only when the files
changed; the daemon calls it at every start, so a node that is upgraded, or whose `[mail]` changed, picks the new
environment up without `supavise system init`.

Choices worth knowing:

- `wal_level` stays `logical` (the artifact's value): Realtime's `postgres_changes` needs
  it, and it is a superset of `replica`, so archiving and base backups are unaffected.
- `API_EXTERNAL_URL` is `https://<ref>.api.<domain>/auth/v1`, as hosted projects and the
  dockerless CLI set it. GoTrue resolves its mailer paths against that URL with an absolute
  path, which would drop `/auth/v1`, so `GOTRUE_MAILER_URLPATHS_{INVITE,CONFIRMATION,
  RECOVERY,EMAIL_CHANGE}` are set to `<API_EXTERNAL_URL>/verify`, as the dockerless CLI does.
  Once the project has an active custom hostname, or else a vanity subdomain (`internal/domains`), the
  host is that one instead (`presentedAuthURL`), which also moves the OAuth redirect URIs and the SAML
  endpoints, as on hosted when a custom domain is activated. `GOTRUE_JWT_ISSUER`, `GOTRUE_SITE_URL` and
  PostgREST's OpenAPI URI keep the derived values, so adding or removing a domain invalidates no session.
- `GOTRUE_MAILER_AUTOCONFIRM=true` until SMTP is configured; nobody could confirm an
  address otherwise.
- The seeder contract: `DataSeeder` fills `<project>/postgres/data`; `postmaster.opts` is
  created if missing because `pg_basebackup` omits it and the launcher refuses a data
  directory without it.
- Unix socket paths are limited to about 100 bytes, so on macOS and in tests the state
  directory must be short; the plane refuses a longer one with a clear error.

## Test

```
go test ./internal/lifecycle/                       # fakes: state machine, specs, failure paths
SUPAVISE_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked \
  go test -run Integration -v ./internal/lifecycle/ # real artifacts, exec backend
```

The integration tests need unpacked slim-services artifacts (`postgres-17*`, `auth-*`,
`postgrest-*`) for the running platform. They start the system cluster and one project
(about 130 MB), run the full create, health, SQL, pause, resume, rotate, delete cycle, and
a create that fails on a busy port. State lives in a short `/tmp/sbt*` directory and is
removed on exit.

## Not done

- `Recover` does not finish an interrupted restore (`RESTORING`): the project stays `RESTORING`, the data
  directory the restore moved aside is `<dir>.pre-restore-<time>`, and an operator decides
  (`internal/backup/README.md`, "Restore"). A delete interrupted by a version that recorded no events
  (`GOING_DOWN` with no `project.delete_started`) is only logged.
- `pg_hba.conf` is rewritten on start but a changed file is not reloaded in a running cluster.
- Fleet tenant calls are a hook (`Options.Fleet`); the services themselves are the fleet
  workstream's.
- Upgrades across Postgres major versions (`pg_upgrade`): the eligibility answer says there is no upgrade path. Upgrades never touch the shared services (Supavisor, Realtime, Storage, pgmeta, Studio, the edge runtime); they move with the node.
- `Usage` reports disk bytes and unit memory only.
