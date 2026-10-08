# internal/branching

Branches for agents (docs/design.md section 9a). A branch is a project with a parent: its own
Postgres, GoTrue and PostgREST, its own ref, keys and host, its own fleet tenants. An agent
creates one per task, merges what worked and deletes the rest. A non-persistent branch deletes
itself when its lifetime ends.

`branching.Service` is the library. The Management API's branch endpoints
(`internal/api/branches.go`) and `supavise branches` (`cmd/supavise/cmd_branches.go`) are thin
wrappers over it, so the stock Supabase CLI (`supabase branches ...`, which needs `--experimental`) and
the Supabase MCP server's branch tools work unchanged. The MCP server must run unscoped (no
`--project-ref`): the project scope hides the account tools `create_branch` needs. `testdata/mcp-branches.mjs`
drives them against the API.

## Wiring

`supavise serve` (`internal/app/serve.go`) builds the service, hands it to the API as
`Deps.Branching`, runs the expiry sweeper (`svc.Run(gctx)`) and drains running branch operations
with the API's drain at shutdown (a cut-off operation ends `MIGRATIONS_FAILED`).

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

Without `Backup`, `with_data` needs a filesystem that can clone files, and a branch's WAL archive is
not cleaned up on delete. The base-backup path restores through `Backup.WithManager(...)`, a copy of
the service that stamps the new project as a branch. Without `Deps.Branching` the API lists only the
default branch and refuses to create one.

## Data model

Migration `0700_branching.sql` adds nullable columns to `supavise.projects`: `branch_id` (UUID,
stable across reset), `parent_ref` (foreign key, `on delete restrict`), `branch_name` (unique per
parent), `git_branch`, `persistent`, `with_data`, `expires_at`, `deletion_scheduled_at`,
`notify_url`, `branch_state`, `branch_detail`, `clone_method`, `review_requested_at`.
`registry.Project.Branch` (`*registry.BranchInfo`) is set on branches only.
`Registry.UpdateBranch` writes the branch fields and nothing else, so a concurrent lifecycle
status change is not overwritten. The egress policy (`branch_egress`) changes only through the
compare-and-set `Registry.SetBranchEgress(ref, from, to)` (`ErrConflict` when the policy is no longer
`from`), used by reset and by the end of isolation.

Branches are hidden from `GET /v1/projects` and `GET /platform/projects` and reported under
their parent (`preview_branch_refs`); they open by ref like any project (`parent_project_ref`
is set on their `/platform/projects/{ref}`). `GET /v1/projects/{ref}/branches` returns the
default branch (the project itself, `name: main`, `is_default: true`, a stable UUID derived
from the ref) first, as on hosted; called with a branch's ref it returns the whole family.

A parent cannot be deleted while it has branches: `Engine.DeleteWith` refuses before it stops
anything, and the foreign key backs that up. A create and a delete hold different lifecycle locks, so
each checks the other's write and no branch is left on a deleted parent.

## Operations

Create, merge, reset and push run detached from the request. The API answers at once (`201` with the
new branch in `CREATING_PROJECT` for a create, `{workflow_run_id, message: "ok"}` for the others) and
the branch's `status` follows: `CREATING_PROJECT`, `RUNNING_MIGRATIONS`, then `MIGRATIONS_PASSED` or
`MIGRATIONS_FAILED` with the cause in `branch_detail` (`supavise branches get`). `preview_project_status`
is the project's own status. A busy branch refuses another operation with `409`. Events
`branch.<op>.started`, `.succeeded` and `.failed` (with the run id) go to the branch's and the parent's
event log; a `notify_url` gets a POST when the operation ends. The sweeper marks an operation that no
process works on as failed: the process that started it (host, pid and service instance in the started
event) is gone, or the branch has been busy for 30 minutes. After a daemon crash the first sweep
recovers its interrupted operations at once; an operation of another host or of a live process keeps
the 30 minute rule.

### Create

`POST /v1/projects/{ref}/branches` body `{branch_name, is_default, git_branch, region,
desired_instance_size, persistent, with_data, notify_url}`. The parent must be running. `region` is
accepted and the parent's is used. A non-empty `secrets`, a `release_channel` other than `ga` and a
`postgres_engine` other than the parent's are refused with 400, as is an unknown `desired_instance_size`
(`pico` is Nano; see `internal/lifecycle/README.md`, "Compute sizes"); without one a branch gets
`[branching] default_class` (`micro`, as on hosted) and counts against the node's memory budget. A
`with_data` create is refused with `409` before anything exists when the state disk cannot hold the clone
(Free disk), and its outbound side effects are blocked unless the request opts out (Outbound isolation).

**Schema only** (the default, as on hosted): a new project, then the parent's migration history
(`supabase_migrations.schema_migrations`: versions, names and statements, each migration in its own
transaction, or outside one when a statement refuses to run in a transaction block), then the seed. The Management API has no seed field, so the seed is stored per parent (`supavise branches seed set
<parent-ref> --file seed.sql`, sealed like a project secret under `branch_seed`) or given for one branch
with `--seed-file`. A failing migration or seed leaves the branch in `MIGRATIONS_FAILED` for inspection.
Objects the parent got outside the migration history (the SQL editor, `execute_sql`) are not in a
schema-only branch: use `with_data`.

> **A `with_data` branch is a copy of production.** Whoever can run SQL in it can read all of the parent's
> data. Supavise replaces the credentials it knows about and cuts the branch off from the outside (below), but
> it does not detect node-local credentials that users put in their own tables, function bodies, Vault entries
> or settings, and a branch can reach every port on loopback. `with_data` is opt-in and meant for trusted users
> and agents; see "What a branch with data can and cannot reach".
>
> Creating a `with_data` branch through the Management API, and resetting one, needs the Owner or
> Administrator role (also when scoped to the parent project); Developers keep creating and resetting
> schema-only branches, and get 403 otherwise. `supavise branches create` runs on the node as its operator
> and is not checked.

### What a branch contains

| | Schema-only | With data |
|---|---|---|
| Database schema and migration history | the parent's `supabase_migrations` history, replayed, then the seed | everything in the parent's database |
| Rows | none, except what the migrations and the seed insert | all of them (auth users and identities included; their sessions and one-time tokens are removed) |
| Storage buckets | those the migrations create | the parent's buckets, with their settings and policies |
| Storage objects | none | none: the rows of `storage.objects` (and `storage.prefixes`, `storage.s3_multipart_uploads*`) are removed from the clone; the files stay with the parent. Exception: when a table of yours has a foreign key into one of them, the rows stay (below) |
| Edge Functions | none | none |
| Function secrets | none | none |
| API keys, JWT secret, database password | the branch's own | the branch's own (the parent's are replaced everywhere Supavise finds them) |

Hosted Supabase copies neither Storage objects nor Edge Functions into a branch, and neither does Supavise: a branch has its own Storage tenant and functions, so nothing is shared and nothing is copied half way.

- **Storage objects are not copied.** A clone would carry the rows without the files, so every listed object would answer 404. Isolation empties the object metadata tables and leaves the buckets (`branch.isolated` event, `storage_rows_deleted`; user triggers do not fire), in a database that has Storage's own tables. If a table outside Storage has a foreign key into them, the wipe is skipped in that database (changing your tables is not isolation's job) and the branch lists the parent's objects, which answer 404 until you remove or re-upload them; the event names the constraints (`storage_wipe_skipped`).
- **Edge Functions and function secrets are not copied.** Deploy to the branch's ref (`supabase functions deploy --project-ref <branch ref>`, `supabase secrets set --project-ref <branch ref>`). Create, `reset` and `push` never read the parent's deployments, and `merge` does not carry functions to the parent (the result names the ones that differ). A reset leaves the branch's functions and secrets alone; files the branch uploaded stay in its Storage backend, unreferenced.
- **Clone method** (`clone_method`, the cheapest available): `clonefile` on APFS, `reflink` on XFS with `reflink=1`, btrfs, bcachefs and OpenZFS 2.2+ with block cloning, else `base-backup` (restore of the parent's latest base backup plus all archived WAL; `[branching] clone = "backup"` forces it). The copy-on-write methods keep time and disk flat as the database grows; the restore grows with it. `detectClone` tries to clone the parent's `PG_VERSION` into the new directory, so it tests the exact pair of paths and names the filesystem and reason when a clone is impossible. ZFS without block cloning takes the base-backup path. With neither a clone nor a backup backend the create fails before anything exists.
- **How the clone is taken:** `pg_backup_start` on a superuser session that stays open, a file-by-file clone of PGDATA (`pg_control` last), `pg_backup_stop`, `backup_label`, then the WAL segments up to the end-of-backup record; the branch starts as a crash recovery and needs no archive. A recycled segment retries the whole procedure (three attempts), a file that cannot be cloned is byte-copied, tablespaces are refused.
- **Service credentials are rotated.** The clone's roles carry the parent's passwords, so once the cluster is up the service seals new passwords for the six service roles, sets them over the private socket as SCRAM verifiers and calls `Manager.RotateKeys` (new JWT secret and API keys, GoTrue and PostgREST restart, fleet tenants updated). The parent's database password does not open the branch, and the parent's temporary CLI login roles (`cli_login_*`, `supavise_cli_ro_*`) are set `NOLOGIN` with no password. The pgsodium root key cannot change (Vault secrets are encrypted with it), so the branch shares it with its parent: whoever holds the parent's root key can decrypt the branch's copy of the parent's Vault secrets.
- **The parent's credentials inside the data are replaced.** A cloned service key is a key to the parent that anyone with SQL in the branch can read, and a branch reaches the node's loopback. After the rotation `rewriteCredentials` (`credswap.go`) replaces, with the branch's value, every parent credential in Vault secrets, pg_cron job commands (paused or not) and database and role settings (`pg_db_role_setting`). A parent credential is an anon or service_role JWT that verifies against the parent's JWT secret, the `sb_publishable_` and `sb_secret_` keys, the JWT secret or one of the six database passwords. A value is rewritten when it contains one (`Bearer <key>`, a connection string); a credential under 20 characters must match whole. Replacements are recorded by name, never value (event `branch.credentials_rewritten`, table `supavise_branch.rewritten_credentials`). A failure stops the branch.
- **Sessions of the parent's users are removed**, because a refresh token read in the branch is a session of a production user. In every `with_data` branch, with or without `--allow-egress`, `wipeAuthSessions` (`isolate_auth.go`) empties the session, refresh-token, MFA-challenge, one-time-token, flow-state, SAML/OAuth-state and WebAuthn-challenge tables of `auth`, and blanks the token columns of `auth.users` and the passkey challenge of `auth.mfa_factors`. Users and identities stay, so branch users sign in again with their passwords and get tokens of the branch. What stays is production data, not a session: password hashes, TOTP secrets, passkey public keys, OAuth and SSO configuration and third-party client secrets (`auth.custom_oauth_providers`). The event records counts per table (`auth_rows_deleted`, `auth_rows_cleared`).
- **Auth tables are classified.** The two lists in `isolate_auth.go` (wipe, blank) cover the GoTrue release pinned in `internal/versions/versions.yaml`. A table in neither list is named in the event (`auth_tables_not_reviewed`) and the integration test fails on it, so a GoTrue upgrade that adds a table gets classified.
- **Subscriptions, cron jobs and pg_net are neutralized**, because a copy would attach to the parent's replication slot and run cron jobs and queued pg_net calls twice. Both with_data paths start the cluster first with `max_logical_replication_workers = 0`, `cron.launch_active_jobs = off` and `pg_net.database_name` pointing at a missing database (lines marked `# supavise-branch-quarantine` in `postgresql.auto.conf`). Then, in every database, subscriptions are disabled and detached from their slot (so the publisher's slot survives), `cron.job` rows are set inactive (unless `[branching] keep_cron_jobs = true`) and `net.http_request_queue` is emptied; the marked lines go, the cluster restarts and the credentials rotate. A failing step stops the branch (`MIGRATIONS_FAILED`).
- **Paused cron jobs** are recorded in `supavise_branch.paused_cron_jobs`; the owner opts in with `update cron.job set active = true where jobid in (select jobid from supavise_branch.paused_cron_jobs);`. Job `nodename` and `nodeport` point at the branch's own cluster, and a connection string written as a literal in a command is replaced by a disabled one (`supavise_branch.neutralized_cron_commands`; the original is not kept, it may hold a password).
- **Every database but `template0`** is isolated, templates and `datallowconn = false` databases included. Such a database is opened with `ALTER DATABASE ... ALLOW_CONNECTIONS true` for the duration and closed again (`databases_opened`), so its owner cannot find its foreign servers and Vault secrets as the parent had them.
- **Foreign servers are disabled** in every database of a branch without the egress opt-out (`neutralizeForeign`, `foreign.go`; safe to run twice), loopback or not, because a loopback server reaches another project on this node with its stored password. This covers `postgres_fdw`, `dblink_fdw` and any server with a libpq option (`host`, `hostaddr`, `port`, `dbname`, `service`): `host` becomes the nonexistent socket `/nonexistent/supavise-branch-disabled`, `hostaddr` and `service` are dropped, and user mappings lose a `password`, `sslpassword` or `passwd` option. A wrapper whose validator refuses the change is named in the event (`foreign_servers_not_neutralized`). `supavise_branch.paused_foreign_servers` records what was done, **but not the passwords** (the table is readable by whoever owns the branch). To turn a server back on, set its options and re-enter the password: `alter server s options (set host '...'); alter user mapping for ... server s options (add password '...');`. Foreign tables and functions that use the server fail until then.

### Outbound isolation

Everything else the parent's data can do toward the outside world is outbound traffic from the branch's Postgres: database webhooks and triggers that call `pg_net`, cron jobs that make HTTP calls, other Wrappers servers, any extension that connects out. A branch with data has these defaults:

1. **Its Postgres unit may reach 127.0.0.1 and ::1 only.** Under systemd the unit gets `IPAddressDeny=any` and `IPAddressAllow=127.0.0.1/32 ::1/128` through the per-unit drop-in (`units.Spec.DenyEgress`, as for `MemoryMax`). The allow list is not systemd's `localhost` (127.0.0.0/8), whose other addresses include systemd-resolved's stub at 127.0.0.53, a DNS side channel. The filter is a cgroup BPF program, so it covers every process of the cluster (backends, pg_net and pg_cron workers, `COPY ... PROGRAM`) and survives restarts and reboots.
2. **Unix sockets are hidden instead**, because an IP filter does not touch them: `deploy/systemd/supavise-postgres@.service` sets `InaccessiblePaths` for `/etc/supavise`, `/run/dbus`, systemd-resolved's varlink socket and nscd's socket, for every project's Postgres (systemd 255 cannot change it over D-Bus). Names then resolve only through `/etc/hosts` or a resolver on 127.0.0.1:53. A node installed before this needs the new unit template (the installer and self-update install it).
3. **Cron jobs, subscriptions, the pg_net queue, foreign servers and the parent's credentials** are handled as above, whatever the supervisor.

The filter is applied after the first start (a base-backup restore may need the backup backend to finish recovery). The branch's `egress` field (`supavise branches get --json`; `supavise_egress` in the API's branch JSON, an extra field that spec-decoding clients ignore) is `pending` (until isolation finishes), `denied`, `allowed` or `unenforced`, and its `detail` says in words what the branch can reach. A reset keeps the policy: the new cluster is created open, then denied again after its first start.

**Opt-out:** `supavise branches create ... --with-data --allow-egress`, or `POST /v1/projects/{ref}/branches?allow_egress=true` (our query parameter, so the stock CLI and MCP server cannot set it). The branch keeps the parent's outbound side effects: no egress block, cron jobs active, foreign servers, user mappings and cron connection strings as the parent had them (a loopback foreign server still reaches another project on the node); `egress` is `allowed`. Subscriptions are still detached and credentials still replaced. `[branching] keep_cron_jobs` keeps only the cron jobs. Use the opt-out for a branch that must call a real service, not for an agent you do not trust.

**The exec backend cannot block egress.** `supervisor = "exec"` (development and tests) runs units as plain child processes with no cgroup, so a branch with data reports `egress: unenforced` ("egress NOT blocked"). The other neutralizing steps still run, but a webhook, pg_net or any extension can reach the outside: do not clone a production database through the exec backend.

Limits of the block:

- A **remote backup backend** (`s3://`) is outbound traffic too, so WAL archiving of a branch with denied egress cannot reach it (the default `file://` backend works). On an S3 node the `archive_command` of a with-data branch keeps failing and its WAL piles up in `pg_wal`: give a persistent or write-heavy branch there `--allow-egress`, or delete it when its work is done.
- A schema-only branch replays the parent's migrations, which may schedule cron jobs or install triggers that call out. It inherits no data, so it is not isolated.
- Only the branch's Postgres unit is confined, not its Realtime, Storage and pooler tenants.

### What a branch with data can and cannot reach

For a `with_data` branch created without `--allow-egress`, on the systemd supervisor, and nothing more.

**It cannot:**

* Send anything from its Postgres unit to an address other than 127.0.0.1 and ::1 (a lookup can still carry data out through a resolver on 127.0.0.1:53 where one runs; IPv6 is not tested).
* Use a foreign server of the cloned data or a cron `dblink` string to reach the parent or another project.
* Use the parent's API keys, JWT secret or database passwords where its setup kept them (Vault, cron commands, settings); only the keys Supavise issued are covered.
* Open another project's Postgres through its unix socket or read its files: the unit sees an empty `/var/lib/supavise` with only its own cluster directory.
* Keep the parent's subscriptions, queued pg_net requests, active cron jobs or user sessions.

**It can:**

* Reach **any port on 127.0.0.1 and ::1**, because the filter is on addresses, not ports: the parent's Postgres, every other project's, the proxy, the admin API (`127.0.0.1:7000` by default) and the fleet services, through pg_net, `COPY ... PROGRAM` (superuser), untrusted procedural languages or a `dblink` string in a query. Only their own authentication stands in the way.
* Use any credential of the parent that the data holds in a place that is not rewritten.

**Residual risk:**

* **Node-local credentials that users stored themselves are not detected**, because Supavise cannot tell which strings in a copy of production are secrets. Credentials in user tables, function bodies, trigger arguments (database webhook headers), foreign table or other wrapper options, Storage objects, Vault entries other than the keys Supavise issued, settings other than those rewritten, encoded forms or `ALTER SYSTEM` stay as they were. That includes a connection string or key for another project on this node or a custom login role of the parent, an `sbp_` token for the admin API, and a JWT with a role other than `anon` or `service_role`. Egress denial does not help, because the target is on the node; a third-party credential (a Stripe key) cannot leave the node while egress is denied, but it is in the branch.
* Whoever can run SQL in the branch has a copy of the parent's data, whatever is rewritten, and the shared pgsodium root key decrypts unrewritten Vault secrets.
* The unit's isolation is a mount namespace and an IP filter, not a sandbox: all units run as the same user and share `/proc`, so code execution inside a branch's Postgres is not contained.
* A schema-only branch replays migrations verbatim, so a credential written into a migration is in the branch.
* Without systemd (the exec backend) the network is not filtered.

For untrusted agents prefer schema-only branches (the default, as on hosted): they copy no data.

### Free disk

A `with_data` create or reset refuses to start when the state directory's disk (`statfs` of the parent's
cluster directory) has less free space than **1.2 times the parent's data (the apparent size of its PGDATA,
pg_wal included) plus a reserve**, `[branching] disk_reserve_mb` (default 2048). The answer is `409` in the
API's message envelope and nothing has been created or removed. The check is the same for every clone method, because a copy-on-write clone diverges as either side writes. A reset of a
base-backup branch counts the space of its old private copy, which the reset removes first; a copy-on-write
branch's does not count. A free-space figure that cannot be read skips the check with a log warning.
Schema-only branches copy nothing and are not checked.

### Merge, push, reset

* **Merge** applies the branch's migrations that the parent lacks to the parent, in one transaction under a
  session advisory lock (`lock_timeout` 30 s, so DDL on a busy parent fails instead of queueing traffic behind
  it). Versions and divergence are checked again under the lock (a version another merge applied meanwhile is refused with `409`), and a statement that cannot run in a transaction block makes it fall back to one transaction per migration (the result says so). It **refuses on divergence**: the parent has migrations the
  branch lacks, a version exists on both sides with different content, or a branch migration is older than the
  parent's latest. `?force=true` (our extension) merges anyway and skips the versions that differ in content.
  `migration_version` merges up to and including that version. The branch is kept.
* **Push** (the rebase) applies the parent's migrations that the branch lacks to the branch. A version that
  differs in content stops it unless forced.
* **Reset** recreates the branch from the parent: the old cluster is removed (no final backup, archive
  purged: **a persistent branch loses its WAL and base backups on reset**, because the recreated cluster
  reuses the ref; take a backup first if you may want them) and a new one is created over the same registry
  row, with the same ref, id, name and settings. What can fail without touching the branch is checked first (the parent runs, a with_data branch can still get its data, the parent's credentials can be read). A later failure leaves the branch registered as `MIGRATIONS_FAILED` with `INIT_FAILED` project status under the same id, so it can be reset again or deleted. A schema-only branch keeps its credentials and accepts `migration_version` (the parent's history up
  to it); a branch with data gets new credentials as at creation. Its lifetime is kept, with at least 15
  minutes left, and the branch stays resolvable (its `status` is `CREATING_PROJECT` meanwhile).
* **Diff** (`GET /v1/branches/{id}/diff`, `text/plain`) is the SQL a merge would apply.

### Delete, expiry

`DELETE /v1/branches/{id}` deletes at once: fleet tenants, units, data, registry row. A **non-persistent**
branch gets no final backup and its whole WAL archive and base backups are removed (a reset reuses the
ref). A **persistent** branch gets the lifecycle's final base backup when a backup backend is configured and
its archive stays, like any deleted project's. `?force=false` schedules the deletion after
`soft_delete_grace_minutes` (60); `POST .../restore` cancels it. `DELETE /v1/projects/{ref}/branches` deletes
every branch of the project.

`expires_at` is set when a non-persistent branch becomes ready, `now + [branching] default_ttl` (7 days;
`supavise branches create --ttl 6h`; `off` disables). `Service.Run(ctx)` sweeps every
`sweep_interval_seconds` (60), at start too; `supavise branches sweep [--dry-run]` does one pass by hand.
`PATCH` with `persistent: true` clears the expiry, `false` starts the default lifetime.

## Limits and guards

`[branching] max_per_project` (10) and `max_total` (50): every branch is a cluster. Branch names are
git-branch-like (`A-Za-z0-9._/-`, 100 characters); `main` is reserved. `notify_url` must be http(s), and the
call refuses loopback, private, link-local, shared (100.64.0.0/10), benchmarking (198.18.0.0/15),
IETF-reserved (192.0.0.0/24, 240.0.0.0/4) and NAT64 (64:ff9b::/96) addresses at connect time unless
`allow_private_notify_urls` is set.

Limits:

* No command copies a parent's functions or objects into a branch, and `merge` moves migrations only.
* Re-encrypting `vault.secrets` under a key of the branch's own is not done (the pgsodium root key is shared).
* Paused cron jobs and disabled foreign servers are turned back on by hand (the SQL above); no `supavise` command does it.
* Egress is blocked only under systemd, covers the branch's Postgres unit only, and blocks WAL archiving to an
  `s3://` backend.
* No idle sleep: an unused branch costs its idle memory (about 130 MB with GoTrue and PostgREST).
* The Management API's action-run endpoints (`/v1/projects/{ref}/actions`, used by Studio's branch pages) are
  stubs; progress is the branch's status and `branch_detail`.
* `GET /v1/branches/{id}` reports the pooler (`pooler.<domain>`, session port, user `postgres.<ref>`) as the
  database address: Supavise has no direct database host.
* Two operations on one branch are excluded through the registry state and an in-process table, not a cross-process lock, so two processes starting one in the same instant can both proceed.
* The clone paths ran on APFS (`clonefile`), XFS (`reflink`) and ext4 (base backup); btrfs and OpenZFS
  block cloning were not run.

## Configuration (`[branching]`, or `SUPAVISE_BRANCHING_*`)

| Key | Default | Meaning |
|---|---|---|
| `default_ttl` | `7d` | lifetime of a non-persistent branch (`36h`, `7d`, `off`) |
| `default_class` | `micro` | class of a branch without `desired_instance_size` |
| `max_per_project`, `max_total` | 10, 50 | caps |
| `sweep_interval_seconds` | 60 | |
| `clone` | `auto` | `backup` always restores from the base backup |
| `soft_delete_grace_minutes` | 60 | |
| `allow_private_notify_urls` | false | |
| `keep_cron_jobs` | false | keep the parent's pg_cron jobs active in a branch with data (egress stays denied); `--allow-egress` keeps them per branch and opens egress |
| `disk_reserve_mb` | 2048 | free disk that must remain after a with_data create or reset, on top of 1.2 times the parent's data |

## CLI

```
supavise branches list <project-ref> [--json]
supavise branches create <project-ref> <name> [--with-data [--allow-egress]] [--persistent] [--ttl 6h|off] [--size micro]
                      [--git-branch b] [--seed-file f] [--notify-url u] [--no-wait] [--json]
supavise branches get|delete|restore|diff <id|ref|name --project <ref>>   # delete --schedule
supavise branches merge|reset|push <id|ref|name> [--project <ref>] [--migration-version v] [--force] [--no-wait]
supavise branches sweep [--dry-run]
supavise branches seed set|get|clear <project-ref> [--file seed.sql]
```

Like `supavise projects`, these open the node in the calling process (system cluster running, run as the
`supavise` user).

## Tests

The `ci.yml` job `test` runs the unit tests. The
`linux.yml` job `branching-xfs` runs `tests/linux/branching-xfs.sh` (the whole branching scenario on a
loop-file XFS with `reflink=1` and on ext4) and `tests/linux/branching-egress.sh` (egress isolation under
systemd: the denied unit cannot reach a non-loopback host while the parent can, the allow list is exactly
127.0.0.1 and ::1, `--allow-egress` reaches the host, the restriction survives pause, resume and reset). The
integration tests in `internal/branching` run real clusters and are skipped unless `SUPAVISE_TEST_UNPACKED`
points at the unpacked artifacts directory.
