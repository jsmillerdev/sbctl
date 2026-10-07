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
no process has touched for 30 minutes (a crash) as failed.

### Create

`POST /v1/projects/{ref}/branches` body `{branch_name, is_default, git_branch, region,
desired_instance_size, persistent, with_data, notify_url}` (`secrets`, `release_channel` and
`postgres_engine` are accepted and ignored; `region` is the parent's). The parent must be running.
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
| `reflink` | XFS with `reflink=1`, btrfs, bcachefs, OpenZFS 2.2+ | `FICLONE` per file |
| `zfs-snapshot` | ZFS without file cloning, `zfs` usable by the `sbctl` user (`zfs allow`) | `zfs snapshot` of the dataset between `pg_backup_start` and `pg_backup_stop`, copied from `.zfs/snapshot/` |
| `base-backup` | the data directory's filesystem cannot clone (ext4, ...), or `[branching] clone = "backup"` | restore of the parent's latest base backup plus all archived WAL as a new project (`backup.RestoreWith`, `Latest`) |

Detection (`detectClone`) tries to clone the parent's `PG_VERSION` into the new branch's directory,
so it tests the exact pair of paths (same filesystem, reflink enabled). The report names the
filesystem and why a clone was not possible. When neither a clone nor a backup backend is available
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
it with its parent.

### Merge, push, reset

* **Merge** applies the branch's migrations that the parent lacks to the parent, in one
  transaction (`pg_advisory_xact_lock`, `lock_timeout` 30 s so DDL on a busy parent fails instead of
  queueing traffic behind it); a statement that cannot run in a transaction block makes it fall back
  to one transaction per migration and the result says so. It **refuses on divergence**: the parent
  has migrations the branch lacks, a version exists on both sides with different content, or a
  branch migration is older than the parent's latest. `?force=true` (our extension; the Management
  API has no flag) merges anyway and skips the versions that differ in content. `migration_version`
  merges up to and including that version. The branch is kept.
* **Push** (the rebase) applies the parent's migrations that the branch lacks to the branch. A
  version that differs in content stops it unless forced.
* **Reset** recreates the branch from the parent: delete (no final backup, archive purged), create
  again with the same ref, id, name and settings. A schema-only branch keeps its credentials and
  accepts `migration_version` (the parent's history up to it); a branch with data gets new
  credentials as at creation. Its lifetime is kept, with at least 15 minutes left. For about a
  second the branch's ref does not resolve.
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
http(s) and the call refuses loopback, private and link-local addresses at connect time unless
`allow_private_notify_urls` is set.

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
  parent's archive stays, the expiry sweep, a parent delete refused while it has branches.
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

* **Edge Functions are not part of branching.** The functions endpoints store sources and
  metadata per project but there is no runtime until phase 2 (`functions-main/`,
  `sb-edge-runtime`). Creating a branch does not copy the parent's functions and `merge` moves
  migrations only (hosted also merges functions). Once the runtime exists, copying the function
  rows (`api_functions`, `api_function_files`) from branch to parent on merge, and from parent to
  branch on create, is a row copy in this package.
* `with_data` copies the database only: Storage objects (files, with their extended attributes) and
  function sources are not copied, so `storage.objects` rows of a clone point at files that exist
  only under the parent's tenant.
* The ZFS snapshot path is written against the documented `zfs` commands and unit-tested with a fake
  runner; no ZFS was available. OpenZFS 2.2+ with block cloning normally takes the `reflink` path
  instead.
* Branches are not covered by the nightly base backup timer until something enables
  `sb-basebackup@<ref>.timer` for them (the lifecycle does not enable timers yet); a persistent
  branch is backed up on delete only.
* `reflink` (XFS) and the ext4 base-backup path ran in CI only, on a loop-file XFS; btrfs and ZFS were not run.
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
