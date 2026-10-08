# internal/api: the Management API

`api.New(api.Deps{...})` returns an `http.Handler` that serves the parts of the Supabase
Management API (`/v1`, `/v2`, `/platform`) that Studio, the Supabase CLI and the Supabase
MCP server call, over the registry and the lifecycle manager. Mount it at `api.<domain>`.

```go
h := api.New(api.Deps{
    Registry: reg,      // registry.Registry (Postgres or Memory)
    Secrets:  sec,      // secrets.Secrets
    Manager:  mgr,      // lifecycle.Manager (project create/pause/delete, Keys, ConnString, Health)
    Config:   cfg,      // *config.Config, including the [api] section
    Logger:   logger,   // optional
})
```

## How routes are built

The three OpenAPI documents are pinned in `gen/specs/` (v1, v2, platform) and embedded. Every operation of the three is served:

- The operations in the table below have a handler here.
- The others answer with a **stub derived from the spec**: the smallest instance of the operation's success schema (required fields only, empty arrays, enums at their first member, `204` where the spec says so), with header `X-Supavise-Stub: true`. Billing, integrations, replication, log drains and network restrictions are stubs.
- A path in no spec answers `404 {"message":"Not Found"}`, except authenticated `/platform/*` paths (Studio calls a few the platform spec omits): `GET` gives `200 {}`, other methods `204`.

Tests validate every stub and handler response against the spec's schema, because the CLI decodes strictly. Response types come from `gen/` (oapi-codegen, one package per spec). Two path pairs that Go's `ServeMux` rejects as ambiguous (`/apps/{app_id}/signing-keys`, `/apps/installations/{installation_id}`) sit in a second mux of a chain.

### Implemented

| Area | Routes |
|---|---|
| Projects | `/v1/projects` list, create, get, patch, delete, `pause`, `restore`, `restart`, `health`, `branches`; `/platform/projects` the same plus `status`, `settings`, `restart-services`, `config/*`, `api-keys/temporary`; `/v2/projects/{ref}/config`; `billing/addons` |
| Domains | `custom-hostname` and `vanity-subdomain` under `/v1/projects/{ref}` (`domains.go`; rules in `internal/domains/README.md`). Owners and Administrators change them, every role reads them. A missing hostname answers 400 "Project does not have a custom hostname configuration" (Studio matches "custom hostname configuration"), a refused verification 429, a hostname another project holds 409. Activate and delete restart the project's GoTrue; if that fails the domain stays saved and the answer is 500 |
| Settings, keys | Auth, PostgREST, Realtime, Storage and pooler config (v1 and platform twins), `config/database/postgres`, `/v2/projects/{ref}/config` (the document the CLI diffs), `api-keys`, `database/password`. See Settings and keys |
| Read replicas | `POST /v1/projects/{ref}/read-replicas/setup` and `remove`; `GET /platform/projects/{ref}/databases`, `databases-statuses` and `load-balancers`; one pooler entry per database in `config/supavisor` and `config/database/pooler`; a replica's rows in the organization's project list; `POST /platform/projects/{ref}/restart` with `database_identifier`; the replication lag in `infra-monitoring`; the pg-meta database selector. See Read replicas |
| Database | `database/query` and `query/read-only` (`parameters` supported), `database/migrations` list and apply (`supabase_migrations.schema_migrations`), `types/typescript` (pg-meta generator), `cli/login-role`, `advisors/*` (no lints) |
| Functions, secrets | `functions` list, create, deploy (multipart or an eszip bundle), get, patch, delete, `body`; `secrets` list (digests), create, delete |
| Identity, organizations | `/v1/profile`, `/platform/profile`, `profile/permissions`, `profile/access-tokens`, `/platform/cli/login`; `/v1/organizations`, `/platform/organizations` (`entitlements` grants every feature of the spec's key enum; `billing/subscription` is a plan stub), members, invitations, roles, MFA enforcement, SSO |
| Other | `GET /healthz` (no credentials; `{"status":"healthy"\|"degraded"\|"down"}`, 200 unless down) and `GET /healthz/detail` (Owner or Administrator), not in the specs (`internal/health/README.md`); `/platform/projects/{ref}/content` (saved SQL snippets, reports); proxied: pg-meta, Auth admin (to the project's GoTrue with its `service_role` key) and Storage admin (with `x-forwarded-host: <ref>.api.<domain>`; `objects/public-url` is built here) |

Upgrades and backups are described below.

**Route groups by file.** Each group of routes is registered by one `routesX(add)` method that `implemented()` in `server.go` calls, so that a group is added without editing the others. The read-replica groups are `replicas.go`, `databases.go`, `load_balancers.go` and `infra_monitoring.go`; the failover group has its file and method in place (`failover.go`), and a group with no routes leaves its operations to the spec-derived stubs.

**Function uploads.** Where `Deps.Functions` is set the runtime serves bundles only, so multipart sources (`supabase functions deploy --use-api`, Studio's editor) are bundled in a sandbox by `api.SourceBundler` and stored with the bundle (`.supavise-bundle.ezbr`, `.supavise-bundle.json`); they stay readable through `.../body`. Answers: 400 with the bundler's output for broken code, 501 where the node cannot bundle, 429 when the queue is full. A bundle upload (`Content-Type: application/vnd.denoland.eszip`, `EZBR` + Brotli; what plain `supabase functions deploy` sends) has its `ezbr_sha256` checked. `Deps.Functions` (`api.FunctionsHook`) is told after each change so `internal/functions` can put the files where the Edge Runtime reads them.

## Pooler config

`GET /v1/projects/{ref}/config/database/pooler` (what the CLI diffs as `db.pooler.*`), `GET /platform/projects/{ref}/config/supavisor` and `config/pgbouncer` (Studio) and `/v2/projects/{ref}/config` report `default_pool_size` (default 15) and `max_client_conn` (default 1000, Supavisor's `default_max_clients`) from the saved settings (`projectconfig.Pooler`). `PATCH /v1/projects/{ref}/config/database/pooler` (`default_pool_size`, `pool_mode`, `max_client_conn`) and `PATCH /platform/projects/{ref}/config/pgbouncer` (Studio sends `default_pool_size` and the `ignore_startup_parameters` it was shown) save them and update the project's Supavisor tenant (`EnsureTenant`), which ends its pooled connections. A `null` returns a field to its default. Both need the permission to update project settings (Owner or Administrator). The two GET routes list one entry per database: the primary's, then each read replica's (`database_type` `READ_REPLICA`, `db_user` `postgres.<identifier>`, `db_host` the public host of the node it runs on, else the pooler host), all with the project's pool settings.

The shared Supavisor cannot honor every field, so these are refused with 400 `{"message": ...}` and nothing is saved:

- `pool_mode` other than `transaction` (session mode is the other port, for every project); the PgBouncer settings `server_idle_timeout`, `server_lifetime`, `query_wait_timeout` and `reserve_pool_size`; `pgbouncer_enabled: false`; an `ignore_startup_parameters` other than empty or the GET's value.
- A `default_pool_size` of 0, over the route's limit (3000 on v1, 4950 on platform), or above the project's `max_connections` (the saved Postgres setting, else the size's) minus 10, which stay free for superusers and the project's own services.
- A `max_client_conn` outside 1 to 54000 or above `[fleet] pooler_max_client_conn` (5000 unless set), so one project cannot claim the shared Supavisor's capacity.

The defaults of the project's size (a pool of 40% of `max_connections`, hosted's client limit for the size: 20 and 200 for Micro) always pass, and so does a field that already has the value it would get (the dashboard saves every field it was shown). Lowering `max_connections` later does not re-check a saved pool size; the next pooler save does.

## Read replicas

Read replicas are standby databases of a project on other servers of the cluster. `internal/replicas` runs them (setup steps, status, lag, removal); this package is the Management API in front of it, in the shapes Studio's Infrastructure page, SQL editor and reports read. It asks `replicas.Service` for everything a replica does and the `placement.Resolver` for which replicas a project has (`api.Deps.Replicas`, `Deps.Placement`; an empty Placement is `placement.RegistryResolver` over the registry). The Resolver answers for one project (the cap on adding a replica, the pg-meta selector, the infra-monitoring identifier); the listings ask `replicas.Service.List` per project, and the organization's project list reads the replicas of its whole page from the registry in one query. On the leader all three read the same rows. When the controller cannot answer (on a follower it forwards to the leader, which may be unreachable during a failover), `replicasOf` and `databases-statuses` log the error and answer from the registry rows the node has: the listing shows each replica with the status and setup step its row holds, and the Infrastructure page does not go blank while the leader changes. A node with no `Deps.Replicas` lists the primary alone, answers 503 "Read replicas are not set up on this Supavise server" to setup, remove and restart of a replica, and keeps `infrastructure:read_replicas` in `disabled_features`. The wiring must pass an untyped nil when there is no controller: the handlers test the interface, and a nil `*replicas.Controller` in it counts as wired. `TestNewServerTakesTheReplicaDeps` pins what a `Deps` with and without a controller builds.

| Route | Answer |
|---|---|
| `POST /v1/projects/{ref}/read-replicas/setup` `{read_replica_region}` | 204 once the controller has recorded the replica; it comes up in the background. Owners and Administrators |
| `POST /v1/projects/{ref}/read-replicas/remove` `{database_identifier}` | 204; 404 `Read replica not found` for an identifier that is not one of the project's replicas, the primary's included. The handler checks that itself (the Resolver's rows) before it asks the controller, which checks it too, so a controller that took any identifier for granted would not let one project's admin remove another's replica |
| `GET /platform/projects/{ref}/databases` | The primary first, then each replica oldest first. A replica's row has the primary's shape: its identifier, the region of its node, `restUrl` `https://<identifier>.api.<domain>/rest/v1/`, `db_host` `db.<identifier>.api.<domain>`, and `connectionString` and `connection_string_read_only` with a `[YOUR-PASSWORD]` placeholder. `size` is the project's size |
| `GET /platform/projects/{ref}/databases-statuses` | The same databases, the primary included (Studio refetches until the two lists have the same length). A replica carries `replicaInitializationStatus`: `in_progress` with the step reached and the estimates, `completed`, or `failed` with the step's error |
| `GET /platform/projects/{ref}/load-balancers` | `[]` without a replica, and while `Deps.LoadBalancers` is false (the proxy does not serve `<ref>-lb.api.<domain>`). Otherwise one balancer: its endpoint and every database with its type and status |
| `POST /platform/projects/{ref}/restart` | With a `database_identifier` that is a replica of the project, restarts that replica alone (201); 404 for another identifier. `replicas.Service.Restart` does not wait for the replica to come back: it returns once the replica is `RESTARTING` and the units restart in the background, so the answer is quick and databases-statuses shows `RESTARTING`, then `ACTIVE_HEALTHY`, or `ACTIVE_UNHEALTHY` if the node could not do it. Without one, or with the project's own, it restarts the project, which is the primary only. A body that is not JSON is a 400 where a replica controller is wired, so a client that meant a replica cannot restart the project by sending it badly; a node without one ignores a body it cannot parse, as it always did, and answers 503 to a body that names a replica (restarting the primary for a request that meant a replica would be worse) |
| `GET /platform/projects/{ref}/infra-monitoring` | The replication lag of `databaseIdentifier` for `physical_replication_lag_physical_replication_lag_seconds`: the mean of the controller's one-minute samples per `interval`, as strings. One attribute answers in the single-attribute shape (the value on each point, which Studio's replica page reads), several in the multi-attribute shape with an `errors` entry for each metric Supavise does not collect. The primary has an empty series. A request that names no other attribute than the lag keeps the stub's empty 200 |

A setup is refused with 400 and a message Studio shows as it is: the API's own checks are a project that is not running (409), a branch, a size below Small ("Read replicas need a compute size of small or larger."), the cap of the size (none up to Micro, four for Small to Large, five above: "The project already has the maximum of <n> read replicas.") and a project whose port sequence is above `config.MaxReplicaSeq` or a `ports.replica_base` that leaves no room (`config.CheckReplicaPorts`). The controller refuses the rest with a `replicas.UserError` (no server joined in the region, a replica on that server already, capacity, backup storage).

**Delete and restore remove the replicas first.** A standby cannot follow a restored primary, and the project row owns its replica rows, so a delete would take the rows and leave the instances on their nodes. `DELETE /v1/projects/{ref}` and `/platform/projects/{ref}`, the deletion of an organization's projects, and the restore routes (`database/backups/restore`, `restore-physical`, `restore-pitr`, `pitr`) call `RemoveAll` of the `replicas.Remover` first (`Deps.Replicas` must also implement it, as the controller does; a service that does not is refused with 503 for a project that has replicas). A removal that cannot finish (a node that does not answer, a worker still setting the replica up) leaves it `GOING_DOWN` for the controller to retry, and the request is a 409 that names the replicas: the project is neither deleted nor restored, and the same request succeeds once they are gone. A restore does not make them again; the server-wide default does when the project runs. The CLI paths (`supavise projects delete`, `supavise backups restore`) are not behind this API.

**Where this differs from design 2.9.** (1) pg-meta's "Permission: `infrastructure:read_replicas`" is not an authorization check. `infrastructure:read_replicas` is a Studio feature key in the profile's `disabled_features` (identity.go): it is removed only when a controller is wired and at least two nodes are active (one `ListNodes` per profile request), and what it shows or hides is the Infrastructure page, the source selector and the replica rows of the reports. The pg-meta routes are authorized by `{ref}` and the route's own permission, as before. (2) The read-only role does not follow the read-only connection string: the header is a selector only, and the role is the caller's on a replica as on the primary (`postgres`, or `supavise_read_only` for a member who may only query). Studio's reports read `pg_stat_statements` as `postgres` does, and no role has to reach the replica with the WAL first.

**What Studio needs of the project.** `GET /platform/projects/{ref}` carries `dbVersion` as `supabase-postgres-<version>` (a bare version counts as below Postgres 15 for the Add button) and `is_physical_backups_enabled`, which is whether the node has a backup service, as the PITR flag of the backups list is. The profile lists `infrastructure:read_replicas` in `disabled_features` until a controller is wired and a second node is active; it hides the Infrastructure page, the source selector and the replica rows of the reports. `studio_eligibility_test.go` ports the rules of Studio's `useCheckEligibilityDeployReplica` at the pinned version and evaluates them against these handlers, and checks that where Studio disables the button for the size or the cap the API refuses too.

**pg-meta selects a database by the host of `x-connection-encrypted`.** Studio sends back the `connectionString` (or `connection_string_read_only`) of the database it is working on. The header is a selector and nothing else: the host `db.<identifier>.api.<domain>` names the project or one of its replicas, and the connection is built from the project's own credentials (at the replica's port on this node, `config.ReplicaPorts`, which is the replica itself or a forwarder to its node). The request is authorized by `{ref}`, so an identifier that is neither the project nor one of its replicas (by the Resolver) is refused with 403, a replica that is not `ACTIVE_*` with 503, and a header that is not a database host of this node, or absent, selects the primary as before. The user in the string (`postgres`, or `supabase_read_only_user`, hosted's name, for the read-only string) is not read: the role is the caller's on a replica as on the primary, so Studio's reports read `pg_stat_statements` as `postgres` does and no role has to reach the replica with the WAL first. A caller who may only query runs as `supavise_read_only` on both; that role is created on the primary and reaches a replica with the WAL. The host is read from the authority of the URL alone.

`supabase_read_only_user` is created by the pinned Postgres artifact's init SQL (`login bypassrls`, member of `pg_read_all_data`, `default_transaction_read_only = on`); the API only names it. `TestPinnedPostgresCreatesTheReadOnlyUser` reads that SQL from the unpacked artifact when `SUPAVISE_TEST_UNPACKED` is set; no workflow sets it for this package, so in CI it does not run. `tests/linux/roles-smoke.sh` asks a real cluster of each shipped archive for the role, its login, bypassrls, `pg_read_all_data` and its read-only session default, and is what checks the archives.

## Settings and keys

Saving a setting (`config_*.go`) validates it (`internal/projectconfig` describes every setting), stores it, applies it to what runs (`lifecycle.Reconfigurer.ApplyConfig`: only GoTrue's or PostgREST's unit restarts; Realtime and Storage tenants are updated; Postgres settings go through `ALTER SYSTEM`) and answers with what was saved. If applying fails, the previous settings are saved and applied again and the caller gets a 502. Saves of one service of one project are serialized. A paused project accepts a save, which applies when it resumes. Secrets in responses are SHA-256 hashes (`projectconfig.Redact`). `GET /internal/templates/{ref}/{name}` serves a project's saved email template to its GoTrue (loopback clients that did not come through the edge proxy only).

**API keys** (`/v1/projects/{ref}/api-keys`: list, create, get, patch, delete; `api-keys/legacy`). Opaque keys are sealed `project_secrets` records (`internal/secrets/apikeys.go`), so the proxy's key cache sees a created or revoked key on its next request. The secret is shown in full by the create and by `reveal=true`; delete is a revocation. The two `default` keys live where rotation puts them. Names are lowercase letters, digits and underscores, unique per project; at most 50 active keys. Only `secret_jwt_template: {"role": "service_role"}` is supported.

`PUT .../api-keys/legacy?enabled=false` makes the proxy refuse the anon and service_role JWTs (and JWTs signed with the project secret) as `apikey`, on Functions too, and refuse the exact legacy keys as a bearer, as Storage's S3 session token (`403` XML `AccessDenied`) and inside a Realtime socket (closed with `1008`). It is refused while no publishable and secret key exist. JWTs signed with the legacy secret stay valid in `Authorization` until the JWT secret is rotated, as on hosted.

**Database password** (`PATCH .../database/password`, 8 to 128 characters). `SetDatabasePassword` changes the `postgres` role in the cluster (as a SCRAM verifier), then the sealed secret the API connects with, then makes Supavisor drop the tenant's pools and cached logins; a failure to store the secret puts the old password back.

## Branches

`Deps.Branching` (a `*branching.Service`, `internal/branching/README.md`) serves the branch API with the exact spec shapes: `GET/POST/DELETE /v1/projects/{ref}/branches`, `GET /v1/projects/{ref}/branches/{name}`, `GET/PATCH/DELETE /v1/branches/{id_or_ref}` (`force=false` schedules the deletion), `POST .../merge|reset|push|restore` and `GET .../diff`. Studio calls these paths itself; the `/platform` twins are the project fields `is_branch_enabled`, `preview_branch_refs` and `parent_project_ref`. Branches are not listed as projects.

- Merge, reset and push answer `201 {workflow_run_id, message: "ok"}` at once and run in the background; the branch's `status` follows. `?force=true` on merge and push is an extension.
- A branch has none of the parent's Storage objects, Edge Functions or function secrets, as on hosted (`internal/branching/README.md`, "What a branch contains"). Create refuses `secrets`, a `release_channel` other than `ga` and a `postgres_engine` other than the parent's with 400; `region` is accepted and the parent's is used.
- PATCH refuses a `status` that differs from the branch's own and accepts the deprecated `reset_on_push`. `GET /v1/branches/{id}` omits `db_pass` and `jwt_secret` for the default branch and until a new branch's credentials are stored.
- Without `Deps.Branching` the list is the default branch only and a create answers 400. Entitlements grant `branching_limit` and `branching_persistent`, which the CLI checks on a failed create.

## Backups and point-in-time restore

`Deps.Backups` (a `*backup.Service`, `internal/backup/README.md`; `internal/app` passes it when the backup backend is configured) feeds Studio's Database > Backups pages and the routes `GET /platform/database/{ref}/backups`, `GET /v1/projects/{ref}/database/backups` and the restores below. Restores run through the lifecycle Engine (`lifecycle.DatabaseRestorer`, found on the `Manager`). Without a backup service the list is empty, `pitr_enabled` is false and a restore answers 503.

- **Listed:** the node's base backups (`backup.RestoreWindow`): complete ones on the archive's timeline history whose first WAL file is still archived, each `isPhysicalBackup: true` (`is_physical_backup` on v1), status `COMPLETED`, with the registry row id as `id`. `pitr_enabled` and `walg_enabled` are true whenever a backup service exists. `physicalBackupData` (`physical_backup_data` on v1) holds the earliest restorable time (the end of the oldest listed backup) and the latest (now for a running project, else the newest archived object), in Unix seconds; both are absent until the first base backup exists, and Studio then shows "No backups yet".
- **Studio** states that PITR is on and hides the list. "Restore to new project" is not offered (`database:restore_to_new_project` is in `disabledFeatures`, `identity.go`) because Studio's clone request carries a name and password that `backups restore --as` cannot take.
- **PITR add-on:** `GET .../billing/addons` reports one `pitr` add-on with `meta.backup_duration_days` = `backup.retention_days` (with pruning off, the days since the project was created), priced 0; the variant id is the nearest of `pitr_7`, `pitr_14` and `pitr_28`.

**Restoring.** `POST /platform/database/{ref}/backups/pitr` and `POST /v1/projects/{ref}/database/backups/restore-pitr` take `recovery_time_target_unix`; `.../backups/restore` and `.../restore-physical` (platform and v1) take a listed backup's `id` (`restore-physical` also takes `recovery_time_target`, RFC 3339 or Unix seconds, to replay from that backup).

| Condition | Answer |
|---|---|
| Time outside the span, or a backup no longer usable | 400, with the span in the message |
| Unknown backup id | 404 |
| Node without backups | 503 |
| Project neither `ACTIVE_*` nor `RESTORE_FAILED`, or a disk that cannot hold the restored copy beside the current data (`lifecycle.ErrInsufficientDisk`: twice the data plus a fifth of it, at least 1 GiB, free) | 409 |
| Otherwise | 201, no body; the restore runs in the background |

- The restore has no deadline (the log warns at 4 hours). `Drain` waits for it only about 11 minutes (`TimeoutStopSec`), so a daemon restart is not safe while one runs.
- `Engine.BeginRestore` moves the project to `RESTORING` under the project lock before the answer; pause, restart, a second restore and delete are refused with 409 until it ends. It ends `ACTIVE_HEALTHY`, with the cluster's role passwords set back to the registry's.
- On failure the backup service puts the original data back where it can and the project becomes `RESTORE_FAILED` (Studio: "Something went wrong while restoring your project"). Health checks leave it alone; a second restore, a pause then resume, a restart or a delete moves it on. The error is in the log and the `restore.failed` event.
- A daemon that stops during a restore leaves the project `RESTORING`; `internal/backup/README.md`, "From the dashboard and the Management API", says how an operator settles it.
- The old data directory is kept as `projects/<ref>/postgres/data.pre-restore-<time>`; a restore that worked removes older `data.pre-restore-*` and every `data.failed-restore-*`, a failed one keeps only the newest `data.failed-restore-*`.

`restore-point`, `schedule`, `undo`, `download`, `downloadable-backups` and `enable-physical-backups` are stubs.

## Compute sizes and disk (`compute.go`)

The project's size is `infra_compute_size` (Studio's names: `nano`, `micro`, `small`, ... `2xlarge`); what each size sets is in `internal/lifecycle/README.md`, "Compute sizes". Owners and Administrators change sizes and disks; every role that sees the project reads them.

- `GET .../billing/addons` (platform and v1): `selected_addons` lists the compute add-on of the size (none for Nano), `available_addons` the sizes the node can give (`lifecycle.Resizer.Offers`), at price 0. Nano is never listed (the specs have no `ci_nano`; Studio adds its own card).
- `POST` (platform) or `PATCH` (v1) `.../billing/addons` with `{addon_type: "compute_instance", addon_variant: "ci_small"}` (`ci_nano` too): 201 (200 on v1) once the project is `RESIZING`; the restart runs on. 400 when the node cannot honor the size or the variant is unknown, 409 while another operation runs or the project is not running or paused. `DELETE .../billing/addons/{variant}` returns the project to Nano. Project creation takes `desired_instance_size`, refused with 400 when the node cannot hold it.
- `GET .../disk` and `/v1/.../config/disk`: gp3, 3000 IOPS, 125 MB/s; `size_gb` is the project's quota where one is in force, else the whole volume; `.../disk/util` reports the real use of the data directory. `POST .../disk`, `/v1/.../config/disk` and `/platform/projects/{ref}/resize` set the size where the volume is XFS with `prjquota` (`internal/diskquota`; `Deps.Disk` is the limiter), between used space plus 20% and the volume; elsewhere they answer 400 that the size is informational. Type, IOPS and throughput cannot change.

## Authentication

| Route family | Credentials |
|---|---|
| `/platform/*` | GoTrue session JWT from `supavise-gotrue@system`: HS256 with the system project's JWT secret (`Manager.Keys("system")`, re-read when a signature fails, at most every 5 seconds), audience `authenticated`, an `exp` claim, not anonymous, **and an admin**. The `role` claim is not used (users made through GoTrue's admin API have `role: ""`; `docs/reference/studio-platform-calls.md`, "Findings from running Studio"); `anon` and `service_role` tokens are refused |
| `/v1/*`, `/v2/*` | `sbp_` personal access token (`sbp_` + 40 hex, also `sbp_v0_`, `sbp_oauth_`; looked up by `secrets.HashToken`, expiry honored) **or** a dashboard JWT |
| `GET /platform/cli/login/{session_id}` | none; guarded by the verification code |

**Admin gate.** The user needs `app_metadata.supavise_admin = true` (`api.AdminClaim`; users cannot edit `app_metadata`) or an email in `[api] admin_emails`, otherwise `403`. Supavise sets the claim on every dashboard user it creates; `supavise-gotrue@system` creates no other (its sign-up is closed by a hook, see Single sign-on), and an SSO user is admitted by the SSO rules instead. The email allowlist trusts the JWT's unconfirmed `email` claim, so a registered SSO provider that vouches for an allowlisted address would be believed: prefer the claim and roles. A PAT is not re-checked against its owner's admin status, so delete a user's tokens when you remove the account; it is checked against the owner's roles at every request.

**Claim and invite** (`GET` and `POST /claim`, `claim.go`). Dashboard accounts come only from here, because `supavise-gotrue@system` has sign-up disabled. A claim token (`sbc_` + 48 hex) creates the first administrator, an invite token (`sbi_`) one user for a fixed address; the token is the credential.

- Only the SHA-256 is stored (`supavise.claim_tokens`). A token works once and expires (claim 72 hours, invite 7 days). Unknown, used and expired tokens answer `403`; ten failures in a minute answer `429` for the rest of the minute, node-wide.
- `POST /claim {token, email, password, organization_name}` creates the user through GoTrue's admin API (confirmed, with `supavise_admin = true`); a claim token also creates the first organization (or keeps the existing one) with the user as Owner. Answer: `201 {email, user_id, organization, dashboard_url}`. If the user cannot be created the token is released.
- An invite token accepts only the invitation it was issued for (`claim_tokens.invitation_id`) and dies with it; one bound to none creates an account with no membership.
- `GET /claim` serves a self-contained page that reads the token and address from the link's `#token=...&email=...` fragment, which a browser never sends to a server.
- `claim token --if-none` issues nothing while an unused, unexpired claim token exists (the installer uses it).

`supavise users remove` ends a user's access at once: memberships and project roles go (this refuses, changing nothing, for an organization's only Owner unless `--force`), the user is recorded in `supavise.removed_users` (every request of their session or token is refused from then on, with no cache), their personal access tokens are deleted, and the GoTrue account is deleted. Running it again finishes an interrupted removal.

**`supabase login`** (device flow). Studio's `/cli/login` page calls `POST /platform/cli/login {session_id, public_key, token_name}` with the user's session. The server mints a PAT, seals it to the CLI's P-256 public key and returns `{nonce}`, whose first 8 hex digits are the verification code the page shows. The CLI polls `GET /platform/cli/login/{session_id}?device_code=<code>` for `{access_token, public_key, nonce}`. A session works once and lives 10 minutes; five wrong codes, expiry or replacement by a new authorization with the same id destroy it and its token. `[api] disable_device_login` turns the flow off.

## Members, roles and permissions

Roles are hosted's four with hosted's ids (`internal/members/roles.go`): **Owner** (1), **Administrator** (2), **Developer** (3), **Read-only** (4), organization-wide or limited to projects. Capabilities follow hosted's access-control documentation and Studio's Team page:

| | Owner | Administrator | Developer | Read-only |
|---|---|---|---|---|
| Organization settings, delete the organization, project transfer, MFA requirement | yes | | | |
| Single sign-on: identity providers, their domains and default role, people waiting for approval (a default role is granted only if the caller may add members with it, so an Administrator cannot make Owners through it) | yes | yes | | |
| Add or remove Owners (also project-scoped Owner roles) | yes | | | |
| Add, change, remove and invite Administrators, Developers and Read-only members | yes | yes | | |
| Billing: read | yes | yes | yes | yes |
| Billing: update; OAuth apps | yes | yes | | |
| Create, rename, pause, restore and delete projects; database password | yes | yes | | |
| Restart a project | yes | yes | yes | |
| Restore a backup or a point in time (it overwrites the project's data) | yes | yes | | |
| Project settings (Auth, PostgREST, Realtime, Storage, Postgres); API keys create, update, revoke; function secrets write; any unnamed write | yes | yes | | |
| Read the service_role key, the JWT secret, the S3 credentials; temporary keys | yes | yes | yes | |
| Write SQL, apply migrations, change schema (Studio and pg-meta), Auth users, Storage buckets and objects, deploy and delete functions, preview branches (schema-only) | yes | yes | yes | |
| Create a branch with data (`with_data: true`), or reset one that has data (both copy the parent's data) | yes | yes | | |
| Read everything else: config, logs, advisors, users, buckets, functions, secrets (digests), `SELECT` SQL, types | yes | yes | yes | yes |
| Saved SQL snippets: create; change or delete one's own (Owner and Administrator: anyone's shared ones) | yes | yes | yes | yes |
| Saved reports: same, but Read-only may not create or change them | yes | yes | yes | |

Differences from hosted: Developers cannot manage Auth hooks (they are Auth settings here), cannot restore backups (a restore overwrites data; Studio disables the buttons too) and cannot set function secrets.

- **Project-scoped roles** hold a base role on a set of projects, one role per project per member. The member sees only those projects (lists filter, others answer 403) and can read the organization and its members. A member with no organization-wide role and no project has no access.
- **An organization keeps one Owner.** Demoting, removing or leaving as the last Owner answers 400 (`ErrLastOwner`). `POST /platform/organizations` needs an Owner role somewhere (or an empty node).
- **Deleting an organization** (`DELETE /platform/organizations/{slug}`, Owners only; `supavise orgs list`, `supavise orgs delete <slug> --yes`, which without `--yes` lists what it would delete) removes its SSO providers first (this can refuse with 403 or 409 before anything else changes), then its projects, each with a final base backup (a failure answers 500, or 409 for a state that refuses a delete, naming the project; the Owner repeats the request), then the organization row with its members, roles, invitations, MFA setting and default-role rules (`org_delete.go`). The node's last organization is never deleted (409).

### Enforcement

`authz.go` maps every operation of the specs and every extra route to an action and resource of hosted's `PermissionAction` model (`tenant:Sql:Admin:Write` on `migrations`, `write:Update` on `custom_config_gotrue`, `infra:Execute` on `reboot`, ...), checked before the handler runs. Tests fail for a rule no route reaches.

- **Defaults deny.** A write no rule names needs `write:Update` on the project (Owner, Administrator) or the organization (Owner); a write outside both needs Owner somewhere. Routes about the caller's own account (profile, tokens, notifications, telemetry) and the invitation the caller holds a token for are open to every signed-in user, from a short allowlist.
- **Branches follow their parent.** A project-scoped role covers the project's branches, and a route naming a branch by id or ref is resolved to the parent (read needs `read:Read` on `preview_branches`, delete `write:Delete`, other writes `write:Update`). A create with `with_data: true` and a reset of a branch with data also need the permission to update the project; the handlers check that, since the route table cannot see the body or the branch.
- **Studio uses the same list.** `GET /platform/profile/permissions` returns the caller's entries, evaluated like Studio's `doPermissionsCheck` (`internal/members/permission.go`): `%` wildcards, a matching restrictive entry beats any grant, project-scoped entries take over, an empty list denies. Studio reads it every five minutes, so a changed role shows in the UI after a reload while the API refuses at once.
- **Read-only SQL is read-only in the database.** Whoever lacks `tenant:Sql:Write:Insert` has SQL run as `supavise_read_only` on every path (`database/query`, which the MCP server's `execute_sql` uses; Studio's pg-meta reads), and `cli/login-role` hands them the read-only login role. The barrier is table privileges plus `default_transaction_read_only`, which a client can override with `set` or `begin read write`, so a Read-only member can still call a `SECURITY DEFINER` function that `PUBLIC` may execute (Postgres's default) and write through it, as on hosted. Revoke `EXECUTE` from `PUBLIC` on such functions.
- **Secrets stay with the roles that may read them.** Without `read:Read` on `service_api_keys`, `field.jwt_secret` and `storage.s3_credentials` (Read-only), settings omit the JWT secret and service_role key, `api-keys?reveal=true` and the temporary key answer 403, S3 credentials are not listed, and branches omit `db_pass` and `jwt_secret`.
- **Tokens and sessions follow the roles** at each request, so demoting or removing a member takes their tokens' access away at once. A dashboard session of a removed member keeps answering the routes that only need the user to exist (profile) for the rest of its lifetime, up to an hour; every other route re-reads the roles.
- **MFA requirement.** `GET` and `PATCH .../members/mfa/enforcement` store and report it (201 on both, as the spec says). While it is on, a dashboard session whose JWT lacks `aal: aal2` gets 403 `MFA required` in the organization's routes and cannot accept an invitation; turning it on needs an aal2 session, and so does minting a token (`POST /platform/profile/access-tokens`, `POST /platform/cli/login`) for a member of such an organization. Earlier tokens keep working. `mfa_enabled` of a member is always false. An SSO session counts as aal2 whatever the provider did, so an Owner who does not trust a provider's authentication should remove it, or keep its default role at none and approve users by hand.
- **Saved content belongs to its owner.** Developers and Read-only members change or delete only their own items (folders included), so a member cannot rewrite a shared snippet that an Owner later runs as `postgres`.

### Invitations and bootstrap

- `POST .../members/invitations` takes both body shapes of the spec (Studio's `emails` + `role_id` + `role_scoped_projects`, or `data[].attributes`) and answers `{succeeded, failed}` plus `invite_links: [{email, url, emailed}]`: an existing member lands in `failed`, an invalid address is a 400, a caller who may not invite the role gets 403.
- The token is `sbo_` + 48 hex, stored as its SHA-256 (`supavise.org_invitations`), works once and expires after 7 days (hosted: 24 hours; here the link often travels by hand). Inviting an address again replaces the pending invitation, which needs the permission to revoke it. Studio's `/join?token=...&slug=...` accepts it; the signed-in user's email must match.
- With `[mail]` configured, supavise-gotrue sends the message (if the relay fails the invitation stays and the caller gets the link). Without mail the answer carries the link and the server does not log it, since the claim URL is a credential; `supavise users invite <email> --role <role> [--org <slug>] [--project <ref>]...` prints it.
- Accounts that existed before roles keep full access (migration `0900` made them Owner of every organization); later accounts have none until invited, claimed or granted a role.
- A first-time SSO sign-in gets its role from the email domain's rule (`supavise sso add --domain ... --default-role ...`, or `supavise users default-role set <domain> <role> [--org]`; `members.Service.GrantSSODefault`), provided a registered provider that vouches for the domain is the one the user signed in through. A domain without a rule grants nothing; an existing membership is never changed.
- `supavise users invite|list|role|remove|default-role` (see `--help`): `role` sets an organization-wide role as the operator, which is how an organization without an Owner gets one back. `supavise functions dev --token-file` seats a stand-in Owner that never counts for the last-Owner rule; the daemon removes it at start.

## Single sign-on

Dashboard sign-in with SAML 2.0 (Okta, Entra ID, Google Workspace, anything that speaks it), and the same for a project's end users. Code: `sso_dashboard.go` (the service, shared with `supavise sso`), `sso_routes.go`, `sso_hook.go` and `internal/sso` (`internal/sso/README.md`).

**Dashboard.** `supavise-gotrue@system` runs with SAML on and its own signing key (RSA 2048, system secret `saml_private_key`). An Owner or Administrator registers a provider (`supavise sso add`, `POST .../sso/providers`, or Studio's organization SSO page): GoTrue gets the provider and its email domains, and Supavise records the organization, the domains and the role a first-time user gets (`--default-role`; none means the user waits for approval). Configure the identity provider with `https://api.<domain>/auth/v1/sso/saml/acs` (assertion consumer service) and `.../sso/saml/metadata` (entity id; `?download=true` gives a long-lived metadata file).

- **Who gets in** (`DashboardSSO.Admit`, on every request of an SSO session): the session's provider is `sso:<provider id>` and registered with Supavise (a provider created in GoTrue behind its back opens nothing; a removed one ends its sessions within ten seconds), and the user belongs to an organization. On the first request the email's domain decides: a registered provider that vouches for it plus the organization's default-role rule make the user a member. Anyone else is pending and gets `403` on every `/platform` and `/v1` route until an administrator approves them (`supavise sso approve`, `POST .../sso/pending/{user_id}` with a role) or invites or promotes them; `supavise sso deny` (`DELETE`) deletes the account. The default role applies to the first sign-in only, and the user's tokens face the same gate.
- **Who may change a provider.** A domain with another organization's rule is refused (409) unless the caller owns that organization (or is the operator). An Administrator cannot register or change a provider so that it hands out Owner; changing what a provider vouches for or removing it needs the caller to be able to grant every role its users hold, because whoever controls the provider controls the accounts it signs in.
- **One address, several accounts.** A provider may vouch for an address that has a password account. `Accounts.ResolveUser` picks the password account when exactly one exists, else the only account, else refuses; `supavise users role` and `remove` take `--user-id` and `--provider` to name one. Invitations (`users invite`, the Team page) look at password accounts only: an address that only an identity provider has gets the link that creates a password account. A first SSO sign-in whose address has a password account is logged and recorded in the `sso.user.first_sign_in` event (`shares_email_with`); the SSO account gets the provider's default role like any other, and nothing of the password account.
- **Denying and removing.** `deny` re-checks memberships first: a user who became a member since the last request (invited, `supavise users role`) is not denied (409) and the account stays. `deny` and `supavise users remove` of an SSO account record the refusal by provider and lower-cased address (`sso_denied`), because deleting the GoTrue account would not keep the person out. That address, signing in again, is pending without the default role until an administrator approves it (which clears the refusal) or runs `supavise sso allow <email>`. Inviting or promoting the person the usual way lets them in too and leaves the refusal in place for a later removal to renew. Removing a provider drops its refusals and its users' memberships and project roles (409 while one is an organization's only Owner), deletes it from GoTrue, revokes their tokens (a failure stops the removal with 502 and keeps the record) and deletes Supavise's record last, so an interrupted removal is finished by running it again.
- **Domains are claimed node-wide and not verified.** The first provider to register a domain gets it (a second is refused, 409). An organization that registers a domain another organization's members use, before that organization did, takes over their "Continue with SSO" sign-in, so on a node with several organizations the Owners should register their domains early (`supavise sso list` shows who holds which). There is no DNS verification.
- **Sign-up stays closed.** GoTrue's own switch (`GOTRUE_DISABLE_SIGNUP`) would also stop an SSO user's first sign-in, so it is off on `supavise-gotrue@system` and its before-user-created hook is on: GoTrue asks `POST /internal/hooks/before-user-created` on the loopback admin listener (Standard Webhooks signature; requests through the edge proxy or with a wrong signature are refused). The daemon allows only a user of a registered provider and an address an administrator invited by mail (a one-time `supavise_grant`). Everything else gets `403` "Sign-up is closed on this dashboard", and while the daemon does not answer GoTrue creates nobody.
- **Studio** shows "Continue with SSO" only while `dashboard_auth:sign_in_with_sso` is enabled, which its build disables by default (`NEXT_PUBLIC_DISABLED_FEATURES`). While the dashboard has a provider, `fleet` starts Studio with that list minus the SSO entry, and the API restarts Studio's unit (`Deps.StudioRefresh`) when the first provider appears and the last goes. Studio's organization page shows one provider per organization; the multi-provider and pending routes are Supavise's own.

Provider changes and sign-in decisions are events of the system project (`sso.provider.*`, `sso.user.first_sign_in|approved|denied|allowed`).

**Projects.** `/v1/projects/{ref}/config/auth/sso/providers` proxies to the project's GoTrue admin SSO API with the spec's shapes. As on hosted it answers `404` ("SAML 2.0 support is not enabled for this project") until `saml_enabled` is on in the project's Auth settings; turning it on restarts its GoTrue with the project's own signing key (sealed project secret `saml_private_key`, never shared, not rotated by `rotate-keys`). `saml_external_url` and `saml_allow_encrypted_assertions` are settings too. Writes need the right to change Auth settings (Administrator, Owner), reads the right to see the project. `supabase sso add|list|show|update|remove --project-ref` work with `--profile` unchanged (Supabase CLI 2.119.0).

**Limits.** Dashboard providers take SAML metadata as an https address (GoTrue refreshes it) or a document; `supavise sso add` also fetches a plain-http address on the local machine (a development provider) and passes the document, which GoTrue does not refresh. There is no OIDC provider, no SCIM and no dashboard OAuth sign-in (Google, GitHub, Azure). A cloned or restored project starts with SAML off. A user with a pending invitation who signs in through a provider without a membership is refused until the invitation is dealt with another way (`/join` is behind the same gate).

## Configuration (`[api]` in config.toml, or `SUPAVISE_API_*`)

| Key | Meaning |
|---|---|
| `allowed_origins` | extra CORS origins besides the Studio URL (`*` is not accepted: Studio sends credentials) |
| `pgmeta_crypto_key` | passphrase shared with supavise-pgmeta (`CRYPTO_KEY`). Empty: a random key kept sealed in the registry as system secret `pgmeta_crypto_key`, created by `api.EnsurePGMetaCryptoKey(ctx, reg, sec)`. The unit renderer for supavise-pgmeta must call that function before it renders the unit; the API calls it on first use and fails (no ephemeral key) when the registry cannot store the key |
| `public_url`, `dashboard_url` | override the derived `https://api.<domain>` and `https://studio.<domain>` |
| `disable_device_login` | turn off the browser login flow |
| `admin_emails` | comma-separated emails allowed to use the API without the `supavise_admin` claim |

`[mail]` (or `SUPAVISE_MAIL_*`) is the SMTP relay `supavise-gotrue@system` sends the dashboard's mail through, used for organization invitations: `smtp_host`, `smtp_port` (587), `smtp_user`, `smtp_pass`, `smtp_from`, `smtp_name`. It needs a host and a sender; without it no mail is sent. The password sits in `config.toml` in plain text, so keep the file mode 0600. Restart `supavise-gotrue@system` after a change.

## What this package needs from the rest of the system

- **`Manager.ConnString(ref, role)`** for `postgres` (SQL, migrations, the Studio pg-meta proxy, types) and `supabase_admin` (login roles and the read-only role). Parameterized queries connect with pgx to that DSN, which must be a URL; everything else goes through pg-meta. The project's `pg_hba` must accept password (SCRAM) logins on loopback for these roles.
- **Studio's pg-meta runs as `postgres`**, not `supabase_admin` as upstream's self-hosted Studio does, so SQL-editor objects belong to the role the CLI and migrations use. A feature that needs a superuser fails with a permission error.
- **`replicas.Service` and `placement.Resolver`** for read replicas (`Deps.Replicas`, `Deps.Placement`; `Deps.LoadBalancers` says whether the proxy serves the balancer). The replica service answers on the leader only. A replica's Postgres listens on `config.ReplicaPorts(ref, seq).Postgres` on every node, as itself or as a forwarder, which is where pg-meta and the parameterized-query path connect for a selected replica. That listener accepts password (SCRAM) logins on loopback for `postgres` and `supavise_read_only`, as the primary's does.
- **`supavise_read_only`** (`database/query/read-only`, `database/query` with `read_only: true`, the MCP server's `--read-only` mode): `login`, `bypassrls` (`pg_read_all_data` alone leaves RLS tables reading as empty), member of `pg_read_all_data`, `default_transaction_read_only = on`. The API creates it on demand as `supabase_admin`, with an HMAC-derived password and a SCRAM verifier. It lacks the privileges to escape with `begin read write` or `reset role`.
- **CLI login roles.** `cli_login_<rand>` (member of `postgres`, `set role = postgres`) for read-write; read-only requests get `supavise_cli_ro_<rand>` (`bypassrls`, member of `pg_read_all_data`), because the CLI runs `SET SESSION ROLE postgres` after connecting as any `cli_login_*` user, which a read-only role cannot do. Both expire after an hour and are dropped by the next create or by `DELETE .../cli/login-role` (needs the right to change database roles; drops every member's). `db dump --role-only` skips only `cli_login_*`, so it lists `supavise_read_only` and any live `supavise_cli_ro_*` role.
- **supavise-pgmeta** at `127.0.0.1:<ports.pgmeta>` with `CRYPTO_KEY` equal to `pgmeta_crypto_key`; it also listens on `port+1` for its admin app. **GoTrue** per project at `127.0.0.1:<PortsFor(ref, seq).GoTrue>` and **Storage** at `127.0.0.1:<ports.storage>` (multi-tenant, tenant from `x-forwarded-host`).
- **`supavise-gotrue@system`** with SAML on and its before-user-created hook pointing at this daemon's loopback listener (`lifecycle.SystemAuth`, rendered by `supavise system init` and at every daemon start by `RefreshSystemAuth`).

## State

Migrations in `internal/registry/migrations/`: `0100`-`0101` (`api_users`, `api_cli_login_sessions`, `api_functions*`, `api_content*`; `Store` in `store.go` has a Postgres and a memory implementation behind one conformance suite); `0900`-`0902` (`org_members`, `org_project_roles`, `org_invitations`, `org_mfa`, `sso_default_roles`; behind `internal/members`); `1000`-`1001` (`sso_providers`, `sso_users`, `signup_grants`, `sso_denied`; behind `SSOStore` and `ClaimStore`); `1190` (custom domains, `internal/domains/README.md`). `api.Deps.Store` is passed by `internal/app`; the in-memory fallback logs a warning.

## Generated types

`gen/` is generated from the pinned specs by `go generate ./internal/api/gen`, which works offline on the committed `gen/specs/*.json`. Re-pinning the specs is a separate step: `sh internal/api/gen/fetch-specs.sh`, review the diff, then generate.

## `supavise api profile`

Prints the file the Supabase CLI reads with `--profile` (api_url, dashboard_url, project_host, pooler_host):

```sh
supavise api profile --format yaml > supavise-profile.yaml     # the CLI picks its parser by extension
supabase --profile=./supavise-profile.yaml login --token sbp_...
supabase --profile=./supavise-profile.yaml projects list
```

`project_host` is `api.<domain>`; the CLI derives `https://<ref>.<project_host>` and `db.<ref>.<project_host>` from it. `pooler_host` is the registrable domain of `pooler.<domain>` (`example.com` for `pooler.example.com`): the CLI uses a linked project's pooler URL only when its host has exactly that effective TLD plus one, so a profile that names the host itself leaves `db push` without a route when `db.<ref>.<project_host>` does not answer.

## Project upgrades

A project runs the versions it was created or last upgraded with; the node's pins move with a Supavise release. `internal/lifecycle/README.md`, "Service versions and project upgrades", covers what an upgrade does, its rollback and how a dead runner is settled. Routes (`upgrade.go`):

- `GET /platform/projects/{ref}/service-versions`: `gotrue`, `postgrest`, `supabase-postgres` as release tags (for example `v2.195.0-r1`).
- `GET /v1/projects/{ref}/upgrade/eligibility`: `target_upgrade_versions` has one entry (`postgres_version` the node's major version, `release_channel` `ga`) when upgradable. `eligible` is false when the project is on the node's versions, is not `ACTIVE_HEALTHY`, runs another Postgres major version, has an extension the node's Postgres release cannot serve (`unsupported_extension` in `validation_errors`), or the node has no backup service; a paused project gets `project_hibernating`. `duration_estimate_hours` is fractional (0.05 for GoTrue and PostgREST only, 0.25 with PostgreSQL) and excludes the base backup.
- `POST /v1/projects/{ref}/upgrade` with `target_version` (the major version as Studio posts it, `"17"`; the app version or release tag also work) and `release_channel` `ga` only: 201 with `tracking_id`; the project is `UPGRADING` and the upgrade runs in the background (bounded to 2 hours). 400 for another target or channel, or a project already on the node's versions or with no upgrade path; 409 for a project that is not `ACTIVE_HEALTHY` or is being upgraded, and for any project while `supavise upgrade` moves the node; 503 without a backup service.
- `GET /v1/projects/{ref}/upgrade/status`: `databaseUpgradeStatus` is null for a project never upgraded, else the newest upgrade (`status` 0 upgrading, 1 upgraded, 2 failed); `tracking_id` is accepted and ignored. `progress` takes hosted's names (`0_requested` through `9_completed_upgrade`) and `error` the failed stage; `internal/lifecycle/upgrade.go` says what each marks here.

Eligibility, status and service-versions are reads (Read-only and up); the upgrade is `infra:Execute` on `queue_jobs.projects.upgrade`, so Owners and Administrators only. `POST /platform/projects/{ref}/restart-services` restarts the whole project whatever services it names. Studio shows the upgrade section only while the profile does not list `project_settings:database_upgrades` in `disabled_features`. Two of its texts differ from this node: a paused project's note says restoring updates Postgres (resume here keeps the recorded versions), and the upgrade dialog says the disk "will also be right-sized" whenever `size_gb` differs from the plan's included size (8 GB on gp3), although Supavise resizes nothing.

## Lifecycle calls outlive the request

Delete, pause, restore (resume), restart, upgrade and create run on a context detached from the HTTP request (`context.WithoutCancel`) with a bound: 30 minutes for delete (it includes a final base backup), 20 for create, 10 for pause and resume, 20 for restart; a database restore has none. A client that leaves partway (Ctrl-C on `supabase projects delete`, a closed tab, a proxy idle timeout) cannot strand a project half deleted or stopped; the outcome shows in the project's status.

- `POST /v1/projects` returns `201 COMING_UP` once the registry row exists, or after ten seconds with the allocated ref (never a 504, which would invite a second create); clients poll `GET .../projects/{ref}`, which answers 404 until the row exists.
- v1 pause, restore and restart answer 200; platform pause, restart and restart-services 201; platform restore 200. A request the client cancelled answers 499 (not logged as an error).

## Tests

`ci.yml` job `test` runs the unit tests (route table, role matrix, stubs against the specs, members) with a Postgres service (`SUPAVISE_TEST_DATABASE_URL`). `linux.yml` jobs `roles-smoke`, `sso-smoke` and `settings-smoke` run the role, single sign-on and settings paths against real services. `conformance.yml` job `suites` runs official clients against a node and job `specdiff` diffs the pinned specs against upstream (`tests/conformance/README.md`).

`replicas_test.go`, `databases_test.go`, `infra_monitoring_test.go`, `pgmeta_selector_test.go` and `studio_eligibility_test.go` run the read-replica routes over a fake `replicas.Service` and the Memory registry; every shape is checked against the spec. Two local gates run real processes. `SUPAVISE_API_INTEGRATION=1` with `SUPAVISE_PG_BIN` (Postgres `bin` directory) and `SUPAVISE_PGMETA_BIN` runs `go test ./internal/api -run Integration`, including the proof that a Read-only member cannot write through any route; `SUPAVISE_API_IT_PORT_BASE` moves its 900 ports off 32100-32999. `SUPAVISE_SSO_INTEGRATION=1` with `SUPAVISE_TEST_UNPACKED` (the unpacked artifacts directory) and `SUPAVISE_SSO_PYTHON` (a Python with `signxml`, for `testdata/saml-idp.py`) runs `-run IntegrationDashboardSSO` against a real `supavise-gotrue@system`. `SUPAVISE_TEST_UNPACKED` alone runs `TestPinnedPostgresCreatesTheReadOnlyUser`.

Real clients against the integration stack (delete the JSON file to stop it):

```sh
internal/api/testdata/serve-stack.sh /path/stack.json &        # writes api_url, pat, jwt, ref
API=$(jq -r .api_url /path/stack.json); PAT=$(jq -r .pat /path/stack.json)
SUPABASE_ACCESS_TOKEN=$PAT node internal/api/testdata/mcp-smoke.mjs $API abcdefghijklmnopqrst
SUPABASE_ACCESS_TOKEN=$(jq -r .pats.ro /path/stack.json) \
  node internal/api/testdata/mcp-roles.mjs $API abcdefghijklmnopqrst read-only   # a PAT per role: ro, owner
cat > profile.yaml <<EOF
name: supavise-test
api_url: $API
dashboard_url: http://127.0.0.1:1
project_host: api.supavise.test
pooler_host: supavise.test
EOF
SUPABASE_ACCESS_TOKEN=$PAT SUPABASE_NO_KEYRING=1 supabase --profile=./profile.yaml projects list
python3 internal/api/testdata/cli-login.py ./profile.yaml /path/stack.json       # `supabase login` browser flow
```

Afterwards check that nothing still listens on `32100-32999` and no CLI started with your profile is running (the CLI may leave a `supabase __supabase_stack_host__` process for its project directory).

## Limits

- **Roles.** Hosted's `no-access` invitation role is refused, and the plan limits on Read-only and project-scoped roles do not exist here. `mfa_enabled` of a member is always false, and the MFA requirement covers dashboard sessions only (the `aal` claim of GoTrue's JWT).
- **`supabase login`.** The CLI (2.119.0) builds the browser-login link from its built-in profile table, so with a custom profile it prints a `supabase.com` link. `login --token` works, and the device endpoints work when the link is opened on the right host (`cli-login.py` does that).
- **`supabase db push --linked`.** The CLI dials `db.<ref>.<project_host>:5432` and the pooler on 5432; both ports are hard-coded in the CLI.
- **MCP `get_project_url`.** The MCP server derives it from the API host (`*.supabase.red`), so it is wrong for any custom domain. This is an upstream limitation.
- **Query serialization.** Parameterized queries are wrapped in a CTE so Postgres serializes the rows (trailing semicolons and comments are stripped first). Statements that cannot sit in a CTE (DDL, INSERT without RETURNING, SHOW, EXPLAIN) run unwrapped and their rows are marshaled from the driver's values, which can differ from Postgres's JSON for exotic types. `bigint` columns can differ between `parameters` queries and pg-meta SQL (number vs string).
- **Advisors** return no lints.
- **Region and provider** are reported as the configured AWS region code (`region`, default `us-east-1`) and `AWS`.
- **Direct database host.** The host reported to clients is `db.<ref>.api.<domain>`, which a `*.api.<domain>` wildcard record does not cover (two labels). A read replica's `db.<identifier>.api.<domain>` is the same; the pooler strings are the working path to a replica.
- **Logs of a replica.** Logs by source have no per-replica routing (Studio's `LOG_ROUTES_WITH_REPLICA_SUPPORT` finds no route that has it).
