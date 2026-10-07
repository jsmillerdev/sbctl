# internal/branching

Branches for agents (DESIGN.md section 9a). A branch is a project with a parent: its own
Postgres, GoTrue and PostgREST, its own ref, keys and host, its own fleet tenants. An agent
creates one per task, merges what worked and deletes the rest. A non-persistent branch deletes
itself when its lifetime ends.

`branching.Service` is the library. The Management API's branch endpoints
(`internal/api/branches.go`) and `sbctl branches` (`cmd/sbctl/cmd_branches.go`) are thin
wrappers over it, so the stock Supabase CLI (`supabase branches ...`) and the Supabase MCP
server's branch tools work unchanged (verified, below).

## Wiring

`sbctl serve` (`internal/app/serve.go`) builds the service, hands it to the API as
`Deps.Branching`, runs the expiry sweeper (`svc.Run(gctx)`) and drains running branch operations
with the API's drain at shutdown (a cut-off operation ends `MIGRATIONS_FAILED`; nothing else is
left half-done that the lifecycle's own recovery does not handle). That wiring compiles and is
covered by the API tests, but it was not exercised by a running `sbctl serve`: `TestServeIntegration`
in `internal/app` uses ports outside the range this workstream was allowed, and the real clients below
ran against `internal/branching`'s own harness (`TestServe`), which mounts the same API and service.

```go
svc, err := branching.New(branching.Deps{
    Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets,
    Engine: n.Engine,   // *lifecycle.Engine
    Backup: bk,         // optional *backup.Service (app.NewBackupService), with SetManager(n.Engine)
    Log:    log,
})
go svc.Run(ctx)                                  // the expiry sweeper
srv := api.NewServer(api.Deps{ /* ... */ Branching: svc })
svc.Drain(shutdownCtx)                           // on shutdown, after the API stops accepting requests
```

Without `Backup`, `with_data` needs a filesystem that can clone files and a branch's WAL archive is
not cleaned up on delete. The base-backup path restores through `Backup.WithManager(...)`, a copy of
the service whose restores create their project through a manager that stamps it as a branch; the
shared service's own Manager is not touched. Without `Deps.Branching` the API lists only the default
branch and refuses to create one.

## Data model

Migration `0700_branching.sql` adds nullable columns to `sbctl.projects`: `branch_id` (UUID,
stable across reset), `parent_ref` (foreign key, `on delete restrict`), `branch_name` (unique per
parent), `git_branch`, `persistent`, `with_data`, `expires_at`, `deletion_scheduled_at`,
`notify_url`, `branch_state`, `branch_detail`, `clone_method`, `review_requested_at`.
`registry.Project.Branch` (`*registry.BranchInfo`) is set on branches only.
`Registry.UpdateBranch` writes the branch fields and nothing else, so a status change made by
the lifecycle at the same moment is not overwritten.

Branches are hidden from `GET /v1/projects` and `GET /platform/projects` and reported under
their parent (`preview_branch_refs`); they open by ref like any project (`parent_project_ref`
is set on their `/platform/projects/{ref}`). `GET /v1/projects/{ref}/branches` returns the
default branch (the project itself, `name: main`, `is_default: true`, a stable UUID derived
from the ref) first, as on hosted. Called with a branch's ref it returns the whole family.

A parent cannot be deleted while it has branches: `Engine.DeleteWith` refuses before it
stops anything, and the foreign key backs that up.

## Operations

Create, merge, reset and push run detached from the request. The API answers at once
(`201`, with the new branch in `CREATING_PROJECT` for a create, or `{workflow_run_id,
message: "ok"}` for the others) and the branch's `status` follows: `CREATING_PROJECT`,
`RUNNING_MIGRATIONS`, then `MIGRATIONS_PASSED` or `MIGRATIONS_FAILED` with the cause in
`branch_detail` (`sbctl branches get`). `preview_project_status` is the project's own status.
A branch that is busy refuses another operation with `409`. Events `branch.<op>.started`,
`.succeeded` and `.failed` (with the run id) go to both the branch's and the parent's event
log; a `notify_url` gets a POST when the operation ends. The sweeper marks an operation that
no process works on as failed: the process that started it (recorded in the started event: host, pid,
service instance) is gone, or the branch has been busy for 30 minutes with no change. After a daemon
crash the first sweep recovers its interrupted operations at once, and a new merge, reset or push is
accepted without waiting; an operation of another host, or of a process that is still alive, keeps
the 30 minute rule.

### Create

`POST /v1/projects/{ref}/branches` body `{branch_name, is_default, git_branch, region,
desired_instance_size, persistent, with_data, notify_url}`. `region` is accepted and the parent's is
used (a branch lives on the parent's node). `secrets` (non-empty), a `release_channel` other than `ga`
and a `postgres_engine` other than the parent's are refused with 400 rather than dropped. The parent
must be running.
`desired_instance_size` maps to a class (`pico`/`nano`/`micro` to `micro`, `small`, `medium`,
every larger size to `large`); without one a branch is `[branching] default_class`, `micro`.

**Schema only** (the default, as on hosted): a new project, then the parent's migration history
(`supabase_migrations.schema_migrations`: versions, names and statements, each migration in its own
transaction, or outside one when a statement refuses to run in a transaction block), then the
seed. The Management API has no seed field, so the seed is stored per parent:
`sbctl branches seed set <parent-ref> --file seed.sql` (sealed like a project secret under the name
`branch_seed`), or `sbctl branches create ... --seed-file` for one branch. A failing migration or seed
leaves the branch in `MIGRATIONS_FAILED` for inspection. Objects the parent got outside the
migration history (the SQL editor, `execute_sql`) are not in a schema-only branch: use `with_data`.

**With data**: the cheapest way available, decided at runtime and recorded as `clone_method`:

| `clone_method` | When | How |
|---|---|---|
| `clonefile` | APFS | `clonefile(2)` per file |
| `reflink` | XFS with `reflink=1`, btrfs, bcachefs, OpenZFS 2.2+ with block cloning enabled | `FICLONE` per file |
| `base-backup` | the data directory's filesystem cannot clone (ext4, ...), or `[branching] clone = "backup"` | restore of the parent's latest base backup plus all archived WAL as a new project (`backup.RestoreWith`, `Latest`) |

Detection (`detectClone`) tries to clone the parent's `PG_VERSION` into the new branch's directory,
so it tests the exact pair of paths (same filesystem, reflink enabled). The report names the
filesystem and why a clone was not possible. ZFS without block cloning takes the base-backup path:
a snapshot of the dataset would still be copied byte by byte, and a `zfs clone` needs one dataset
per project, which the state directory does not have, so neither gives a copy whose time and disk
do not grow with the database. When neither a clone nor a backup backend is available
the create fails before anything exists.

The clone is the low-level base backup of the PostgreSQL manual, on a superuser session that stays
open: `pg_backup_start`, a file-by-file clone of PGDATA (what a base backup omits is omitted, the
rules are shared with `internal/backup`; `pg_control` last), `pg_backup_stop`, `backup_label`
written into the clone, then the WAL segments from the one the backup started in to the one holding
the end-of-backup record, cloned from the parent's `pg_wal` into the clone's. The branch starts as a
crash recovery from the label's checkpoint over its own `pg_wal`: it needs no archive, so it works on
a node without a backup backend. If a checkpoint recycled a segment while the copy ran, the whole
procedure is retried (three attempts). A file that cannot be cloned falls back to a byte copy
(counted in the stats). Tablespaces are refused.

**Credentials**: the clone's roles still carry the parent's passwords, so right after the cluster
is up the service seals new passwords for all six service roles, sets them over the cluster's
private unix socket as SCRAM verifiers (never plaintext, with statement logging off), and calls
`Manager.RotateKeys`: new JWT secret and API keys, GoTrue and PostgREST restart, fleet tenants are
updated. The parent's database password does not open the branch (tested). The pgsodium root key
cannot change (the Vault secrets inside the data are encrypted with it): a branch with data shares
it with its parent. The JWT secret, API keys and publishable and secret keys are new from the first
start (the clone path replaces them before the project is created; `backup.RestoreWith` as new does
the same), so the parent's service key never works on the branch. Only the six role passwords are
the parent's until the rotation finishes.

**Isolation from the parent's integrations**: a copy of the data directory carries every outbound
connection of the parent, and a branch that kept them would act as a second parent toward those
systems. A logical replication subscription is copied enabled with the same slot name, so the
branch's apply worker would attach to the parent's slot on the publisher and take changes the parent
never receives. pg_cron jobs, pg_net calls included, would run on both. Requests queued in pg_net
would be sent twice. So both with_data paths start the cluster the first time with
`max_logical_replication_workers = 0`, `cron.launch_active_jobs = off` and `pg_net.database_name`
pointing at a database that does not exist (written to `postgresql.auto.conf` by the data seeder,
marked `# sbctl-branch-quarantine`). Over the private socket, in every database, every subscription
is disabled and detached from its slot (`ALTER SUBSCRIPTION ... DISABLE`, then `SET (slot_name =
NONE)`, so the publisher's slot is not dropped either), `cron.job` rows are set inactive (unless
`[branching] keep_cron_jobs = true`) and `net.http_request_queue` is emptied. Then the marked lines
are removed (the parent's own lines for those three settings, saved beside the file, are put back; a restore rewrites the file with ALTER SYSTEM, so a marker alone would not survive), the cluster restarts on the node's ordinary settings, and the credentials rotate. The
event `branch.isolated` records the counts. If any step fails the branch is stopped (`MIGRATIONS_FAILED`,
like a failed rotation). Anything else in the data that talks to the outside is the parent's:
a `postgres_fdw` or `dblink` server keeps its credentials and connects when queried, and an
external system that polls or pushes to the parent's database address does not know the branch.
Tested on real clusters (`TestIntegrationCloneIsolatesTheParentsIntegrations`).

### Merge, push, reset

* **Merge** applies the branch's migrations that the parent lacks to the parent, in one
  transaction under a session advisory lock (`lock_timeout` 30 s so DDL on a busy parent fails instead
  of queueing traffic behind it). The histories were compared before the lock was held, so the
  versions are read again under it: one that another merge applied meanwhile is refused (`409`), not
  run twice, and the version insert is a plain insert so a duplicate aborts the transaction; a statement that cannot run in a transaction block makes it fall back
  to one transaction per migration and the result says so. It **refuses on divergence**: the parent
  has migrations the branch lacks, a version exists on both sides with different content, or a
  branch migration is older than the parent's latest. `?force=true` (our extension; the Management
  API has no flag) merges anyway and skips the versions that differ in content. `migration_version`
  merges up to and including that version. The branch is kept.
* **Push** (the rebase) applies the parent's migrations that the branch lacks to the branch. A
  version that differs in content stops it unless forced.
* **Reset** recreates the branch from the parent: the old cluster is removed (no final backup,
  archive purged: **a persistent branch loses its WAL and base backups on reset**, because the
  recreated cluster reuses the ref and its WAL would collide with the old; take a backup first if you
  may want them) and a new one is created over the same registry row, with the same ref, id, name and
  settings. Everything that can fail without touching the branch is checked before anything is
  removed: the parent runs, a with_data branch still has a way to get its data (a file clone, or a
  base backup that exists), and the parent's credentials can be read. A failure after that
  (units that do not start, a restore that fails) leaves the branch registered as
  `MIGRATIONS_FAILED` with `INIT_FAILED` project status, under the same id, so the client sees the
  failure and can reset it again or delete it. A schema-only branch keeps its credentials and
  accepts `migration_version` (the parent's history up to it); a branch with data gets new
  credentials as at creation. Its lifetime is kept, with at least 15 minutes left. The branch stays
  resolvable throughout (its `status` is `CREATING_PROJECT` while it is recreated).
* **Diff** (`GET /v1/branches/{id}/diff`, `text/plain`) is the SQL a merge would apply.

### Delete, expiry

`DELETE /v1/branches/{id}` deletes at once: fleet tenants, units, data, registry row. A branch that
is **not persistent** gets no final backup and its whole WAL archive and base backups are removed
from the backend (they would cost storage, and a reset reuses the ref). A **persistent** branch
gets the lifecycle's final base backup when a backup backend is configured and its archive stays,
like any deleted project's. `?force=false` schedules the deletion after `soft_delete_grace_minutes`
(60); `POST .../restore` cancels it. `DELETE /v1/projects/{ref}/branches` deletes every branch of the
project.

`expires_at` is set when a non-persistent branch becomes ready, `now + [branching] default_ttl`
(7 days; `sbctl branches create --ttl 6h`; `off` disables). `Service.Run(ctx)` sweeps every
`sweep_interval_seconds` (60), at start too; `sbctl branches sweep [--dry-run]` does one pass by
hand. `PATCH` with `persistent: true` clears the expiry, `false` starts the default lifetime.

## Limits and guards

`[branching] max_per_project` (10) and `max_total` (50): every branch is a cluster. Branch names are
git-branch-like (`A-Za-z0-9._/-`, 100 characters); `main` is reserved. `notify_url` must be
http(s) and the call refuses loopback, private, link-local, shared (100.64.0.0/10, where some clouds
put their metadata service), benchmarking (198.18.0.0/15), IETF-reserved (192.0.0.0/24, 240.0.0.0/4)
and NAT64 (64:ff9b::/96) addresses at connect time unless `allow_private_notify_urls` is set.

## Configuration (`[branching]`, or `SBCTL_BRANCHING_*`)

| Key | Default | Meaning |
|---|---|---|
| `default_ttl` | `7d` | lifetime of a non-persistent branch (`36h`, `7d`, `off`) |
| `default_class` | `micro` | class of a branch without `desired_instance_size` |
| `max_per_project`, `max_total` | 10, 50 | caps |
| `sweep_interval_seconds` | 60 | |
| `clone` | `auto` | `backup` always restores from the base backup |
| `soft_delete_grace_minutes` | 60 | |
| `allow_private_notify_urls` | false | |
| `keep_cron_jobs` | false | keep the parent's pg_cron jobs active in a branch with data |

## CLI

```
sbctl branches list <project-ref> [--json]
sbctl branches create <project-ref> <name> [--with-data] [--persistent] [--ttl 6h|off] [--size micro]
                      [--git-branch b] [--seed-file f] [--notify-url u] [--no-wait] [--json]
sbctl branches get|delete|restore|diff <id|ref|name --project <ref>>   # delete --schedule
sbctl branches merge|reset|push <id|ref|name> [--project <ref>] [--migration-version v] [--force] [--no-wait]
sbctl branches sweep [--dry-run]
sbctl branches seed set|get|clear <project-ref> [--file seed.sql]
```

Like `sbctl projects`, these open the node in the calling process (system cluster running, run as
the `sbctl` user).

## Verified

On darwin-arm64 with the slim-services artifacts under the exec backend (small: system cluster
plus a parent and at most two branches, class micro):

* `TestIntegrationBranching` (35 s): a parent with a table, 200 rows, migrations, a stored seed.
  The schema-only branch has the schema, the seed row and no other rows; the `with_data` branch (APFS
  `clonefile`) has all rows, writes on either side do not reach the other, the parent's password and
  keys do not open it, its services are healthy on the rotated passwords, its `archive_command` is its
  own; merge reaches the parent, a diverged merge is refused and changes nothing, push, reset (same
  ref, id and credentials), the base-backup path, ephemeral delete leaves no archive object while the
  parent's archive stays, the expiry sweep, a parent delete refused while it has branches, and a reset
  of the base-backup branch into its kept registry row.
* `TestIntegrationCloneUnderWriteLoad`: four writers insert and update and a forced `CHECKPOINT` runs
  every 150 ms while the parent is cloned. The clone recovers, its newest id lies in the window the
  parent committed before and after, ids are unique, `bt_index_check(heapallindexed)` and
  `verify_heapam` find nothing, the unlogged table is empty, the timeline is 1. Runs in CI on XFS too.
* `TestIntegrationCloneIsolatesTheParentsIntegrations`: a parent with an enabled subscription (aimed at
  nothing) and a pg_cron job; the branch has the subscription disabled with no slot, the job inactive
  (active with `keep_cron_jobs`), the node's `max_logical_replication_workers` and
  `cron.launch_active_jobs` back, no marked lines left, and the parent unchanged.
* `TestIntegrationApplyRefusesAVersionAlreadyApplied`: a version already recorded is refused with
  `ErrDiverged` before its statements run, and of two concurrent applies of one new version one wins.
* Unit tests (fake engine): a reset refused before anything is removed (data path gone, no base
  backup), a reset that fails after the old cluster is gone (engine error, error before the row is
  touched, unreadable parent credentials) leaves the branch registered with its id and can be reset
  again or deleted; operations abandoned by a dead owner are recovered at once and live or foreign
  owners are not.
* `internal/branching/testdata/mcp-branches.mjs` runs the **Supabase MCP server 0.13.0**'s
  `create_branch` (with `get_cost` and `confirm_cost`), `list_branches`, `execute_sql` and
  `apply_migration` on the branch, `merge_branch`, `rebase_branch`, `reset_branch` and
  `delete_branch` against the API with a real node behind it: all pass. The MCP server's cost tools
  are client-side constants, nothing is billed. It must run unscoped (no `--project-ref`): the project
  scope hides the account tools `create_branch` needs `confirm_cost` from.
* **Supabase CLI 2.119.0** with `--profile`: `branches create --with-data`, `list`, `get`, `update
  --persistent`, `delete` all decode (the CLI is strict) and work; `get` prints the branch's pooler
  URLs and keys. The CLI needs `--experimental` for these commands.
* `sbctl branches create --with-data`, `reset`, `merge`, `push`, `diff`, `delete`, `sweep` against a
  running node, and the daemon sweeper deleting a branch whose `--ttl 8s` ran out.

Clone timings (APFS, darwin-arm64, exec backend, `TestIntegrationCloneSize`):

| parent database | clone method | file copy | whole clone (backup start to last WAL segment) | whole branch creation | new disk |
|---|---|---|---|---|---|
| 42 MB (the test parent) | clonefile | not recorded | 316 ms | 3.0 s | 312 KiB |
| 167 MB (150 MB of 100 KB rows) | clonefile | 108 ms | 282 ms | 3.0 s | 264 KiB for the clone, 17.9 MiB for the whole creation |
| 1.1 GiB (CI, XFS `reflink=1` on a loop file; two runs) | reflink | 47 to 100 ms | 147 to 226 ms | 3.0 to 4.3 s | 0.2 to 1.2 MiB for the clone, about 20 MiB for the whole creation |
| 1.1 GiB (CI, ext4; two runs) | base-backup | not applicable | not applicable | 7.0 to 12.2 s (the parent's base backup took 5.7 s beforehand in the first run) | 1.19 to 1.21 GB |

"Whole branch creation" is dominated by starting the new project (PostgreSQL, GoTrue migrations,
PostgREST) and rotating its credentials, not by the clone. New disk is the drop in free space of the
state directory's filesystem, so it includes whatever else wrote meanwhile; it is read as an order of
magnitude, not a byte count. The 1 GB rows are CI's job `branching-xfs` in `.github/workflows/linux.yml`
(`tests/linux/branching-xfs.sh`, ubuntu-24.04, also the whole branching scenario on each filesystem; its numbers are
in the job summary). On ext4 the clone is a restore, so its cost grows with the database and the new disk
is a full copy; on XFS and APFS it stays flat.

## Not done, not verified

* **Edge Functions are not copied by branching yet.** They are workstream J (v1, in progress on
  `ws/j-functions`: the `sb-edge-runtime` unit, the tenant-aware main service, and the materializer
  that writes a project's stored deployments under `projects/<ref>/functions/`). Until J is merged a
  branch has no functions, and `merge` moves migrations only (hosted also merges functions). Once J
  is on main, create copies the parent's stored deployments and secrets into the branch (rows, then
  re-materialize: a branch has its own functions directory) and merge carries function changes back;
  `secrets` in the create body is refused until then. This is an open I/J integration item in
  HANDOFF.md section 4, not a later phase.
* `with_data` copies the database only: Storage objects (files, with their extended attributes) and
  function sources are not copied, so `storage.objects` rows of a clone point at files that exist
  only under the parent's tenant.
* ZFS without block cloning uses the base-backup path (see With data); no ZFS was available to try
  OpenZFS block cloning through the `reflink` path.
* Branches are not covered by the nightly base backup timer until something enables
  `sb-basebackup@<ref>.timer` for them (the lifecycle does not enable timers yet); a persistent
  branch is backed up on delete only.
* `reflink` (XFS) and the ext4 base-backup path ran in CI only, on a loop-file XFS; btrfs and ZFS were not run.
* A `postgres_fdw`/`dblink` server in the parent's data, and anything else outside the three integrations
  listed under Isolation, is carried into a branch with data unchanged.
* The base-backup reset path is tested end to end on APFS (`TestIntegrationBranching`); the unit tests
  fake the failure after the old cluster is removed, because a failing restore needs a real archive.
* No idle sleep: an unused branch costs its idle memory (about 130 MB with GoTrue and PostgREST).
* Branches share the parent's pgsodium root key (see Credentials). `reset` of a branch made by the
  base-backup path gets new credentials.
* The Management API's action-run endpoints (`/v1/projects/{ref}/actions`, used by Studio's branch
  pages) are stubs; progress is the branch's status and `branch_detail`.
* `GET /v1/branches/{id}` reports the pooler (`pooler.<domain>`, session port, user
  `postgres.<ref>`) as the database address: sbctl has no direct database host.
* Cross-process exclusion of two operations on one branch relies on the registry state (and the
  in-process table), not on a lock: two processes starting an operation in the same instant can both
  proceed. Lifecycle operations are serialized by the lifecycle's own advisory lock.
