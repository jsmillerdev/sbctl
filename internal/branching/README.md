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
the lifecycle at the same moment is not overwritten. That includes the egress policy
(`branch_egress`): `UpdateBranch` never writes it, so a PATCH, a restore or a status change that
read the row earlier cannot put a stale policy back. The policy is part of the row a create inserts
and is changed afterwards only through `Registry.SetBranchEgress(ref, from, to)`, a compare-and-set
(`ErrConflict` when the policy is no longer `from`) used by reset and by the end of isolation
(`denyEgress`, pending to denied).

Branches are hidden from `GET /v1/projects` and `GET /platform/projects` and reported under
their parent (`preview_branch_refs`); they open by ref like any project (`parent_project_ref`
is set on their `/platform/projects/{ref}`). `GET /v1/projects/{ref}/branches` returns the
default branch (the project itself, `name: main`, `is_default: true`, a stable UUID derived
from the ref) first, as on hosted. Called with a branch's ref it returns the whole family.

A parent cannot be deleted while it has branches: `Engine.DeleteWith` refuses before it
stops anything, and the foreign key backs that up. A branch create and a parent delete hold
different lifecycle locks, so each looks at the other's write: the delete sets the parent
`GOING_DOWN` and then looks for branch rows again (backing out if one landed), and the branch's
`Engine.Create` looks at the parent's status after its own row exists (removing the row if the parent
is going down). Whichever order they interleave in, no branch is left on a deleted parent.

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
A `with_data` create is also refused with `409` before anything is created when the state disk
cannot hold the clone (see Free disk), and its outbound side effects are blocked unless the request
opts out (see Outbound isolation). `desired_instance_size` maps to a class (`pico`/`nano`/`micro` to `micro`, `small`, `medium`,
every larger size to `large`); without one a branch is `[branching] default_class`, `micro`.

**Schema only** (the default, as on hosted): a new project, then the parent's migration history
(`supabase_migrations.schema_migrations`: versions, names and statements, each migration in its own
transaction, or outside one when a statement refuses to run in a transaction block), then the
seed. The Management API has no seed field, so the seed is stored per parent:
`sbctl branches seed set <parent-ref> --file seed.sql` (sealed like a project secret under the name
`branch_seed`), or `sbctl branches create ... --seed-file` for one branch. A failing migration or seed
leaves the branch in `MIGRATIONS_FAILED` for inspection. Objects the parent got outside the
migration history (the SQL editor, `execute_sql`) are not in a schema-only branch: use `with_data`.

**With data**

> **A `with_data` branch is a copy of production.** Whoever can run SQL in it can read all of the parent's
> data. sbctl replaces the credentials it knows about and cuts the branch off from the outside (below), but
> it does not detect node-local credentials that users put in their own tables, in function bodies, in Vault
> entries other than the keys sbctl issued, or in settings, and a branch can reach every port on loopback
> (the filter is on addresses, not ports). Branches are schema-only by default; `with_data` is opt-in and
> meant for trusted users and agents. See "What a branch with data can and cannot reach".
>
> **Followup (workstream K, part 2):** once roles exist, creating a `with_data` branch should require the
> Owner or Administrator role. Until then anyone who can create a branch can ask for the data.

The cheapest way available is chosen at runtime and recorded as `clone_method`:

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
updated. The parent's database password does not open the branch (tested). The temporary CLI login
roles the parent issued (`cli_login_*`, `sbctl_cli_ro_*`) are copied with their password verifiers;
the rotation sets them `NOLOGIN` with no password and an expired validity (the API's expired-role
sweep drops them), so a parent's `supabase db push` password does not open the branch either
(tested). The pgsodium root key
cannot change (the Vault secrets inside the data are encrypted with it): a branch with data shares
it with its parent, and whoever holds the parent's root key can decrypt the branch's copy of the
parent's Vault secrets. Re-encrypting `vault.secrets` under a key of the branch's own is **required
for full separation and is not done** (see Not done). The JWT secret, API keys and publishable and secret keys are new from the first
start (the clone path replaces them before the project is created; `backup.RestoreWith` as new does
the same), so the parent's service key never works on the branch. Only the six role passwords are
the parent's until the rotation finishes.

**The parent's credentials inside the data**: the data also holds credentials where the parent's
own setup put them, and a branch can reach the node's loopback, so a cloned service key is a key to
the parent that whoever runs SQL in the branch can read. After the rotation, `rewriteCredentials`
(`credswap.go`) replaces, with the branch's corresponding value, every parent credential it finds
in three places: Vault secrets (`vault.decrypted_secrets`, rewritten with `vault.update_secret`),
the commands of pg_cron jobs (paused or not), and database and role settings
(`pg_db_role_setting`, for example `app.settings.service_role_key`). A parent credential is the
legacy anon and service_role JWTs, any other anon or service_role JWT that verifies against the
parent's JWT secret (an older key, expired or not), the `sb_publishable_` and `sb_secret_` keys,
the JWT secret, and the parent's six database passwords. A value is rewritten when it contains one
(`Bearer <key>`, a connection string), not only when it equals one; a credential shorter than 20
characters (sbctl generates none) is matched whole. What was replaced is recorded by name, never
by value: the event `branch.credentials_rewritten` and the table `sbctl_branch.rewritten_credentials`
(kind, name, which credentials). A failure stops the branch like a failed rotation. Not detected:
see "What a branch with data can and cannot reach".

**Sessions of the parent's users**: the `auth` schema of the clone holds bearer secrets of the parent's
GoTrue, and the parent's GoTrue listens on loopback: plaintext refresh tokens (`auth.refresh_tokens`), the
key that checks them per session (`auth.sessions.refresh_token_hmac_key`), the hashes of outstanding
recovery, magic link and confirmation tokens (`auth.one_time_tokens` and the `*_token` columns of
`auth.users`), and PKCE and OAuth flow state. Left in the branch, a refresh token read there is a session of
a production user (the branch's GoTrue accepted one in a test before this was removed). So in **every**
`with_data` branch, with or without `--allow-egress`, isolation (`wipeAuthSessions`, `isolate_auth.go`)
empties `auth.sessions`, `auth.refresh_tokens`, `auth.mfa_amr_claims`, `auth.mfa_challenges`,
`auth.one_time_tokens`, `auth.flow_state`, `auth.saml_relay_states`, `auth.oauth_authorizations`,
`auth.oauth_client_states` and `auth.webauthn_challenges`, and blanks the token columns of `auth.users`
(and the stored passkey challenge of `auth.mfa_factors`). Users and identities stay, so **the branch's users
sign in again** with their passwords and get tokens of the branch (its own JWT secret). The event
`branch.isolated` records the counts per table (`auth_rows_deleted`, `auth_rows_cleared`), never values. The
two table lists in `isolate_auth.go` cover the GoTrue release pinned in `versions.yaml` (auth-v2.195.0); a
table in neither list is named in the event (`auth_tables_not_reviewed`) and the integration test fails
on it, so a GoTrue upgrade that adds a table gets classified. What stays is production data, not a
session: password hashes, TOTP secrets, passkey public keys, OAuth and SSO configuration, and the client
secrets of third-party identity providers (`auth.custom_oauth_providers`). Tested on real clusters
(`TestIntegrationCloneNeutralizesForeignServersAndParentCredentials`: the parent's refresh token is gone
from the branch and its GoTrue refuses it, the branch signs the user in again, the parent's token still works).

**Isolation from the parent's integrations**: a copy of the data directory carries every outbound
connection of the parent, and a branch that kept them would act as a second parent toward those
systems. A logical replication subscription is copied enabled with the same slot name, so the
branch's apply worker would attach to the parent's slot on the publisher and take changes the parent
never receives. pg_cron jobs, pg_net calls included, would run on both. Requests queued in pg_net
would be sent twice. So both with_data paths start the cluster the first time with
`max_logical_replication_workers = 0`, `cron.launch_active_jobs = off` and `pg_net.database_name`
pointing at a database that does not exist (written to `postgresql.auto.conf` by the data seeder,
marked `# sbctl-branch-quarantine`: by the copy-on-write seeder right after the clone, and by the
restore's seeder on the base-backup path, in both cases before the first postmaster starts; unit tests
cover both seeders, and the integration test asks the first postmaster for its settings and its
replication processes before isolation runs). Over the private socket, in every database, every subscription
is disabled and detached from its slot (`ALTER SUBSCRIPTION ... DISABLE`, then `SET (slot_name =
NONE)`, so the publisher's slot is not dropped either), `cron.job` rows are set inactive (unless
`[branching] keep_cron_jobs = true`) and `net.http_request_queue` is emptied. Then the marked lines
are removed (the parent's own lines for those three settings, saved beside the file, are put back; a restore rewrites the file with ALTER SYSTEM, so a marker alone would not survive), the cluster restarts on the node's ordinary settings, and the credentials rotate. The
event `branch.isolated` records the counts. If any step fails the branch is stopped (`MIGRATIONS_FAILED`,
like a failed rotation). A pg_cron job that was paused is recorded in `sbctl_branch.paused_cron_jobs` in the
database that holds `cron.job` (`jobid`, `jobname`, `schedule`, `database`, `username`, `paused_at`), so that
whoever owns the branch can opt in later:
`update cron.job set active = true where jobid in (select jobid from sbctl_branch.paused_cron_jobs);`.
pg_cron stamps the parent's port into each job (`cron.job.nodename`, `nodeport`) when it is scheduled, so
isolation sets every job's `nodename` and `nodeport` to the branch's own cluster (`127.0.0.1` and its port,
whether or not the job is paused); a job re-activated later cannot connect to the parent as its user
(`cron_nodes_reset` in the event).
Isolation and the credential rewrite work through **every database but `template0`**, template databases
and databases with `datallowconn = false` included: such a database is opened for the duration with
`ALTER DATABASE ... ALLOW_CONNECTIONS true` as the superuser and closed again afterwards (named in the event as
`databases_opened`; the flags are as the parent had them). The owner of such a database could otherwise allow
connections again in the branch and find its foreign servers and Vault secrets as the parent had them. If
sbctl stops between the two statements the flag stays open, and the branch did not finish creating, so it
ends up failed.
Re-activating a job is not enough to make it behave as in the parent, because two things inside
its command were changed: a connection string written as a literal (`dblink('host=... password=...', ...)`)
was replaced by a disabled one (the jobs are in `sbctl_branch.neutralized_cron_commands`; the original
is not kept, it may hold a password), and a parent credential (an API key, a JWT secret, a database
password) was replaced by the branch's own (`sbctl_branch.rewritten_credentials`). A job that reaches a
foreign server by name needs that server turned on again (next paragraph).
Tested on real clusters (`TestIntegrationCloneIsolatesTheParentsIntegrations`).

**Foreign servers**: a copy of the data carries the parent's foreign servers and user mappings. A
remote target is already blocked by the egress filter, but a server whose host is loopback reaches
another project on this node (or the parent itself) as whoever its stored password says. So in every
database of a branch that does not have the egress opt-out, **every** foreign server that speaks the
Postgres protocol is disabled, loopback or not: `postgres_fdw` and `dblink_fdw` servers, and any server
that carries a libpq option (`host`, `hostaddr`, `port`, `dbname`, `service`), which `dblink_connect`
accepts by name. Its `host` becomes a unix socket path that does not exist
(`/nonexistent/sbctl-branch-disabled`: no network, no DNS) and `hostaddr` and `service` are dropped.
Every user mapping of any wrapper with a `password`, `sslpassword` or `passwd` option loses it.
Another wrapper whose validator refuses the change is left as it was and
named in the `branch.isolated` event (`foreign_servers_not_neutralized`) and in the table. This is
`neutralizeForeign` (`foreign.go`), in one transaction per database, safe to run twice.
What was done is recorded in `sbctl_branch.paused_foreign_servers` (server, wrapper, the host,
hostaddr, port and plain database name it had, the roles whose mapping lost a password, a note).
**The passwords are not recorded anywhere**: they are credentials, the table is readable by whoever
owns the branch (maybe an untrusted agent), and a copy encrypted with a key the node holds would be
a second place to protect for no use the branch needs. **Turning a server back on therefore means
setting its options again and re-entering the password in its user mapping**:
`alter server s options (set host '...'); alter user mapping for ... server s options (add password '...');`
(a dbname that is a connection string or URI is not recorded either, it may contain one).
Foreign tables, views and functions that use the server stay as they are and fail until then. The
connection strings in cron commands are handled the same way (above).

**Outbound isolation (egress)**: subscriptions, cron, the pg_net queue and the Postgres foreign servers are what the
isolation step neutralizes in the data. Everything else the parent's data can do toward the outside world is
outbound traffic from the branch's Postgres: database webhooks and triggers that call `pg_net`, cron jobs
that make HTTP calls, other foreign servers (Wrappers), any other extension that
connects out. A branch must not act on production's systems as if it were production, so a branch
with data has these defaults:

1. **Its Postgres unit may reach the two loopback addresses only.** Under systemd the unit is given
   `IPAddressDeny=any` and `IPAddressAllow=127.0.0.1/32 ::1/128` through the per-unit drop-in, the same persistent
   drop-in mechanism as `MemoryMax` (`SetUnitProperties` over D-Bus with `runtime=false`, `units.Spec.DenyEgress`).
   The allow list is deliberately not systemd's `localhost` (all of 127.0.0.0/8): on a host the rest of that block holds
   addresses something answers on, systemd-resolved's stub at 127.0.0.53 first, which forwards queries upstream and so
   would be a DNS side channel out of a confined unit. Every client of a project's Postgres uses 127.0.0.1 (the cluster
   listens on `127.0.0.1` only; GoTrue, PostgREST, the pooler and the fleet connect to it) or the unix socket, which a
   filter on IP addresses does not touch. An earlier release allowed 127.0.0.0/8: the next render of a unit narrows it.
   The IP filter does not touch unix sockets, so the unit's `InaccessiblePaths` also hides `/run/systemd/resolve`
   (systemd-resolved's varlink socket, which glibc uses for lookups through `nss-resolve`), `/run/dbus` (the system bus,
   where a polkit rule could let the unit lift its own filter) and `/run/nscd`, in addition to the master key
   (`units.EgressHiddenPaths`, applied with the filter, in force when the unit starts; the delete drops them again). Names then
   resolve only through `/etc/hosts` or a resolver on 127.0.0.1:53 (dnsmasq, unbound), which the loopback allow still reaches.
   The filter is a cgroup BPF program on the unit, so it covers every process of the cluster (backends, the pg_net
   and pg_cron workers, `COPY ... PROGRAM`) and it survives restarts and reboots. The unit
   still talks to its own GoTrue and PostgREST and to the pooler over loopback and unix sockets.
2. **The parent's pg_cron jobs are paused** (above), whatever the supervisor.
3. **Subscriptions are detached and the pg_net queue is emptied** (above), whatever the supervisor.
4. **Foreign servers are disabled and cron connection strings replaced** (above), whatever the supervisor.
5. **The parent's credentials inside the data are replaced by the branch's own** (above), whatever the supervisor
   and also with the opt-out.

The filter is applied after the first start, not before: the first postmaster already runs with the
first-start settings, and a base-backup restore may need the backup backend to finish recovery.
The branch's `egress` field says where it stands: `pending` (until isolation finishes), `denied`, `allowed`
or `unenforced`. It is in `sbctl branches get --json` (`egress`), in the branch JSON of the API (`sbctl_egress`, an extra field next to the
spec's; clients that decode the spec ignore it), and the branch's `detail` says in words what the branch can reach.
A reset keeps the policy: the new cluster is created open, then denied again after its first start.

**Opt-out**: `sbctl branches create ... --with-data --allow-egress`, or `POST /v1/projects/{ref}/branches?allow_egress=true`
(the query parameter is ours, like `force` on merge: the spec's create body is unchanged, so the stock CLI and the
MCP server cannot set it and always get the default). With it the branch keeps the parent's outbound side effects: no egress block, the
cron jobs stay active (as they do with `[branching] keep_cron_jobs`, the node-wide setting, which keeps
only the cron jobs and leaves egress denied), and foreign servers, user mappings (passwords included)
and connection strings in cron commands stay as the parent had them, so a loopback foreign server still
reaches another project on the node. `egress` is `allowed`. Subscriptions are detached and the parent's
credentials inside the data are replaced by the branch's own either way. Use it for a branch that must
call a real service, for example a sandbox or staging API, and not for an agent you do not trust.

**On the exec backend egress cannot be blocked.** `supervisor = "exec"` (development and tests) runs the
units as plain child processes, with no cgroup to attach a filter to. A branch with data created there reports
`egress: unenforced` and its detail starts the egress sentence with "egress NOT blocked", so the state does not
claim an isolation that is not there. The cron jobs are still paused, subscriptions are still detached, the queue is still emptied, Postgres foreign servers are still
disabled and the parent's credentials are still replaced, but a webhook, a pg_net call or any other extension in the data can
still reach the outside world from such a branch: do not clone a production database through the exec backend. Verified under systemd by `tests/linux/branching-egress.sh` (CI job `branching-xfs`): a with-data branch's pg_net request to an
external host fails while the parent's succeeds, the unit carries the deny and allow lists, the cron jobs are inactive and
recorded, and a branch created with `--allow-egress` reaches the host.

Limits of the egress block: a **remote backup backend** (`s3://`) is outbound traffic too, so WAL archiving (`archive_command`)
of a branch with denied egress cannot reach it; with the default `file://` backend it works (the unit still sees its
own `backups/<ref>`). Until archiving for such branches goes through the daemon instead of the unit, `archive_command` of a with-data
branch on an S3 node keeps failing and its WAL piles up in `pg_wal` (expected from how archiving works; not
tested against S3): give a persistent or write-heavy branch there `--allow-egress`, or delete it when its work is done. A schema-only branch replays the parent's migrations and may therefore schedule its own cron
jobs or install triggers that call out: it is not isolated, because it inherits no data (the check is on
`with_data` branches). The Realtime, Storage and pooler tenants of a branch are not confined; only its Postgres unit is.

### What a branch with data can and cannot reach

For a `with_data` branch created without `--allow-egress`, on the systemd supervisor, and nothing more
than that (on the exec backend there is no network filter at all). Where something is verified, the tests
are named under Verified.

**It cannot** (enforced; tested except where noted):

* Send anything from its Postgres unit to an address other than 127.0.0.1 and ::1: another host, the internet, the node's own
  non-loopback addresses, the resolver stub at 127.0.0.53. The resolver's varlink socket, the D-Bus system bus and nscd's socket
  are hidden from the unit, so a name resolves only through `/etc/hosts` or a resolver listening on 127.0.0.1:53 (where one
  runs, a lookup can still carry data out through it). Every process of the unit's cgroup is covered. Not tested for IPv6.
* Use a foreign server of the cloned data (postgres_fdw, dblink, any server with libpq options) to write to or read from the parent or
  another project: the server is disabled and its stored passwords are dropped (above). A dblink connection string
  in a cron command is replaced, and the cron jobs are paused.
* Use the parent's API keys, JWT secret or database passwords where the parent's setup kept them
  (Vault secrets, cron commands, database and role settings): they are the branch's own keys now (above). This covers the keys
  sbctl issued and only those; see the first item under "Residual risk".
* Open another project's Postgres through its unix socket or read its files: the unit sees an empty
  `/var/lib/sbctl` with only its own cluster directory (the mount namespace in `sb-postgres@.service`; read from the unit file, not tested here).
* Keep the parent's subscriptions, queued pg_net requests or active cron jobs (above), or the sessions of the parent's
  users (refresh tokens, one-time tokens, flow state; above).

**It can**:

* Reach **any port on 127.0.0.1 and ::1**. The filter is on addresses, not ports, so from the branch's Postgres
  the parent's Postgres and every other project's (their ports follow from the project's sequence number), the
  proxy, sbctl's admin API (`127.0.0.1:7000` by default) and the fleet services are connectable. What stands between the branch and
  those is their own authentication, which is why the parent's credentials are replaced. pg_net, `COPY ... PROGRAM` (superuser only),
  untrusted procedural languages and a `dblink` connection string typed into a query all connect from there.
* Use any credential of the parent that the branch's data holds in a place that is **not** rewritten, or that someone brings in.

**Residual risk, not covered**:

* **Accepted by design: node-local credentials that users stored themselves are not detected.** A `with_data` branch is
  a copy of production, and sbctl cannot tell which strings in it are secrets. Credentials in arbitrary user tables, in function
  bodies, in trigger arguments (the headers of a database webhook), in the options of foreign tables or of other wrappers' servers,
  in Storage objects, in a Vault secret's name or description, in Vault entries other than the keys sbctl issued, in settings other
  than the ones rewritten, in encoded or split form, or set with `ALTER SYSTEM`, stay as they were. That includes a connection string
  or key for **another project on this node or for a custom login role of the parent**, an `sbp_` personal access token for the
  admin API, and a JWT with a role other than `anon` or `service_role`. Whoever can run SQL in the branch can read them and use them
  over loopback (a database connection, or an HTTP call through the proxy with the parent's host name), and **loopback is open
  on every port**: systemd filters by address, not port. Egress denial does not help here, because the target is on the node. Only
  the credentials listed above are replaced, and only where listed. A credential of a third party (a Stripe key in the Vault) cannot
  leave the node while egress is denied, but it is in the branch. This is why branches are schema-only by default and `with_data`
  is for trusted users and agents.
* A branch with data gives whoever can run SQL in it a copy of the parent's data, whatever is rewritten.
* The shared pgsodium root key (see Credentials): whoever holds the parent's root key can decrypt the branch's Vault secrets that were not rewritten.
* The unit's isolation from other projects is a mount namespace and an IP filter, not a sandbox: all units run as the same user and
  share `/proc`, so arbitrary code execution inside a branch's Postgres (a superuser, or a vulnerability) is not contained.
* A schema-only branch holds no table data but replays the parent's migrations and seed verbatim; a credential written into a
  migration (a key in a `cron.schedule` command, an `alter database ... set` with a key) is in the branch, and nothing rewrites it.
  A schema-only branch is not isolated.
* On a node without systemd (the exec backend) the network is not filtered.

**For untrusted agents, prefer schema-only branches** (the default, as on hosted): they copy no data, so there is nothing of the
parent's in them to find. A branch with data is for work that needs the data, by an agent you trust with a copy of it.

### Free disk

A `with_data` create or reset refuses to start when the state directory's disk (`statfs` of the parent's
cluster directory) has less free space than **1.2 times the parent's data (the apparent size of its PGDATA, pg_wal included)
plus a reserve**, `[branching] disk_reserve_mb`, default 2048. The answer is `409` in the API's message envelope ("not enough free disk: the
disk of the state directory has 1.3 GiB free and cloning project ... needs about 3.0 GiB ...") and nothing has been created or removed. The
check is the same for `clonefile`, `reflink` and `base-backup`: a copy-on-write clone costs little
at first but diverges as either side writes, so the check is conservative on purpose. A reset of a base-backup branch counts the
space of its old private copy, which the reset removes first; a copy-on-write branch's does not count. A free-space figure that cannot be read
skips the check with a log warning. Schema-only branches copy nothing and are not checked.

### Merge, push, reset

* **Merge** applies the branch's migrations that the parent lacks to the parent, in one
  transaction under a session advisory lock (`lock_timeout` 30 s so DDL on a busy parent fails instead
  of queueing traffic behind it). The histories were compared before the lock was held, so the
  versions are read again under it: one that another merge applied meanwhile is refused (`409`), not
  run twice, the whole divergence check is made again against the history read under the lock (a
  merge from another branch that landed meanwhile makes this one diverge exactly as a serial merge
  would), and the version insert is a plain insert so a duplicate aborts the transaction; a statement that cannot run in a transaction block makes it fall back
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
| `keep_cron_jobs` | false | keep the parent's pg_cron jobs active in a branch with data (egress stays denied); `--allow-egress` keeps them per branch and opens egress |
| `disk_reserve_mb` | 2048 | free disk that must remain after a with_data create or reset, on top of 1.2 times the parent's data |

## CLI

```
sbctl branches list <project-ref> [--json]
sbctl branches create <project-ref> <name> [--with-data [--allow-egress]] [--persistent] [--ttl 6h|off] [--size micro]
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
* `TestIntegrationCloneNeutralizesForeignServersAndParentCredentials` also checks, on the exec backend, with and without
  `--allow-egress` and on the clone and base-backup paths in CI: a user signed in on the parent (refresh token, session,
  one-time token, flow state, user token columns) has none of it in the branch, the branch's GoTrue refuses the parent's refresh token
  and signs the user in again, the parent's token still works afterwards; cron jobs name the branch's port and the parent's are
  unchanged; a database with `datallowconn = false` (and one that is a template) has its foreign servers disabled and its Vault
  secret rewritten and keeps its flags. Under systemd `tests/linux/branching-egress.sh` checks that the denied unit hides the resolver
  socket, D-Bus and nscd inside its mount namespace and still hides the master key, and that the paths are gone from the drop-in after delete.
* Outbound isolation: `TestIntegrationCloneIsolatesTheParentsIntegrations` also checks, on the exec backend, that the
  branch reports `egress: unenforced` with a detail that says "egress NOT blocked", that the paused cron job is
  recorded in `sbctl_branch.paused_cron_jobs` (and the table is absent with `keep_cron_jobs`), and that
  `allow_egress` reports `allowed` and keeps the job. Unit tests cover the policy per supervisor, reset keeping it, the
  free-disk refusals (create, reset, the base-backup path, schema-only exempt) and the API's 409. Under systemd,
  `tests/linux/branching-egress.sh` (CI job `branching-xfs`, ubuntu-24.04, a loop-file XFS state directory) passed: the
  parent's pg_net request to the runner's non-loopback address is answered, the default branch's Postgres unit has
  `IPAddressDeny` for both families and `IPAddressAllow` for loopback (GoTrue and PostgREST do not), its pg_net request
  fails after pg_net's 5 s timeout (packets are dropped, not refused) and the test server never sees it, the cron job is
  inactive and recorded, the restriction holds after pause/resume and reset, `--allow-egress` reaches the server, and a
  delete lifts the restriction from the unit. The same script now also checks that the allow list is exactly 127.0.0.1 and
  ::1 (a pg_net request from the branch to 127.0.0.1 is answered and one to 127.0.0.2 is dropped and never reaches the server;
  GoTrue and PostgREST stay healthy behind the filter), and, with a parent that has a loopback `postgres_fdw` server to
  another project and a Vault secret holding its own service key, that in the branch (also after a reset) the server has the
  disabled host and no password, an insert through the foreign table fails, the other project is not written to, the Vault secret
  holds the branch's service key and is recorded by name, and that the parent's own foreign table and secret still work and are unchanged.
  `TestIntegrationCloneNeutralizesForeignServersAndParentCredentials` runs in the same CI job on XFS (reflink) and on ext4 (base-backup restore).
  pg_cron jobs were not used as a check that clients keep working behind the filter: in that job a pg_cron job fails with
  "connection failed" on the unconfined parent too, so it says nothing about the filter.
* `TestIntegrationCloneNeutralizesForeignServersAndParentCredentials` (darwin-arm64, exec backend, ~27 s): a parent with a
  loopback `postgres_fdw` server to another project (with a user mapping password and a foreign table), a `dblink_fdw` server, two cron
  jobs (one with a dblink connection string, one with the service key in its command), Vault secrets (the service key, `Bearer <anon key>`,
  an unrelated one) and database settings holding the service key. In the branch both servers have the disabled host and no password,
  an insert through the foreign table and `dblink_connect` fail and the other project is not written to, the table names the servers and
  holds no password, the cron command keeps its SQL but not its connection string, the Vault secrets, the cron command and the setting
  hold the branch's keys and the unrelated ones are unchanged, the names (not values) are in `sbctl_branch.rewritten_credentials` and in
  the events; the parent's foreign table, vault and cron commands are unchanged. A branch with `allow_egress` keeps the foreign server
  and its password and still gets its credentials replaced. Unit tests cover the credential matching (current and older keys, expired
  keys, other roles, keys of another project, short secrets), the connection-string scanner, the order (rotate, then rewrite), the failure
  path (the branch is stopped), the compare-and-set setter, and the stale-update race for the egress policy.
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

* **`with_data` is not restricted by role yet.** Until roles exist (workstream K, part 2) anyone who can create a branch can
  ask for the data; the followup is to require Owner or Administrator for it. The credentials that are not detected are an
  accepted residual risk (see "What a branch with data can and cannot reach").

* **Edge Functions are not copied by branching yet.** They are workstream J (v1, in progress on
  `ws/j-functions`: the `sb-edge-runtime` unit, the tenant-aware main service, and the materializer
  that writes a project's stored deployments under `projects/<ref>/functions/`). Until J is merged a
  branch has no functions, and `merge` moves migrations only (hosted also merges functions); when the
  branch has functions the parent lacks or has in another version (compared by settings and source
  files through `Deps.Functions`), the merge result says "Edge Functions are NOT merged" and names
  them, so an agent does not take them as merged. Once J
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
* Egress is blocked only where the supervisor can do it (systemd); on the exec backend a branch with data reports
  `egress: unenforced` (see Outbound isolation). The block covers the branch's Postgres unit, not its other services, and
  it blocks WAL archiving to an `s3://` backup backend; a schema-only branch is not isolated at all. Loopback is open on every port
  (see "What a branch with data can and cannot reach", which also lists what the credential replacement does not find).
* The paused pg_cron jobs, the disabled foreign servers and their passwords are restored by hand (the SQL under Isolation and Foreign
  servers); there is no `sbctl` command for it. The passwords are not kept, so the owner enters them again.
* The base-backup reset path is tested end to end on APFS (`TestIntegrationBranching`); the unit tests
  fake the failure after the old cluster is removed, because a failing restore needs a real archive.
* No idle sleep: an unused branch costs its idle memory (about 130 MB with GoTrue and PostgREST).
* Branches with data share the parent's pgsodium root key (see Credentials): the root key is
  required to be separate for full isolation of Vault secrets, and is not. Re-encrypting
  `vault.secrets` under a new root key after the first start is the missing step. `reset` of a branch made by the
  base-backup path gets new credentials.
* The Management API's action-run endpoints (`/v1/projects/{ref}/actions`, used by Studio's branch
  pages) are stubs; progress is the branch's status and `branch_detail`.
* `GET /v1/branches/{id}` reports the pooler (`pooler.<domain>`, session port, user
  `postgres.<ref>`) as the database address: sbctl has no direct database host.
* Cross-process exclusion of two operations on one branch relies on the registry state (and the
  in-process table), not on a lock: two processes starting an operation in the same instant can both
  proceed. Lifecycle operations are serialized by the lifecycle's own advisory lock.
