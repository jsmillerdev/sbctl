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
   `max_wal_senders=5`, and `shared_buffers`, `effective_cache_size`,
   `maintenance_work_mem`, `max_wal_size`, `max_connections` from the project class
   (`micro`, `default`/`small`, `medium`, `large`; default is 32MB and 60 connections).
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
`cronSettings`). Hosted Supabase leaves pg_cron on its default, a libpq connection to localhost
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
