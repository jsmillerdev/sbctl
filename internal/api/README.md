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

The three OpenAPI documents are pinned in `gen/specs/` (v1, v2, platform) and embedded.
Every operation of the three is served:

- The operations in the table below have a handler here.
- The others answer with a **stub derived from the spec**: the smallest instance of the
  operation's success schema (required fields only, empty arrays, enums at their first
  member, `204` where the spec says so), with header `X-Supavise-Stub: true`.
  `TestStubsMatchSpec` calls every stub and validates it against its schema.
- A request to a path that is in no spec answers `404 {"message":"Not Found"}`, except
  authenticated `/platform/*` paths (Studio calls a few the platform spec omits): `GET`
  gives `200 {}`, other methods `204`. Both are logged at debug.

`TestImplementedRoutesMatchSpec` drives every handler and validates each response against
the schema in the spec, because the CLI decodes strictly. `TestEveryImplementedRouteIsExercised`
fails when a handler has no test. Response *types* come from `gen/` (oapi-codegen, one
package per spec: `gen/v1`, `gen/v2`, `gen/platform`); where a schema is deeply nested
the handler starts from the spec's minimal instance and overwrites the known fields, so
no response struct is written by hand.

Go's `ServeMux` rejects a few path pairs of the specs as ambiguous
(`/apps/{app_id}/signing-keys` and `/apps/installations/{installation_id}`); those are
registered in a second mux of a chain, tried in order.

### Implemented

| Area | Routes |
|---|---|
| Projects | `/v1/projects` list, create, get, patch, delete; `pause`, `restore`; `health`; `branches` (the full branch API, served by `internal/branching`, see below); `config/database/pooler`. `/platform/projects` list, create, get, patch, delete, `status`, `settings`, `pause`, `restore`, `restart`, `restart-services`; `POST /v1/projects/{ref}/restart`; `config/postgrest`, `config/storage`, `config/pgbouncer`, `config/supavisor`; `api-keys/temporary`; `/v2/projects/{ref}/config`; `/platform/projects/{ref}/billing/addons` and `/v1/projects/{ref}/billing/addons` (the PITR add-on, and a `custom_domain` entry Studio looks for) |
| Domains | `GET`, `DELETE /v1/projects/{ref}/custom-hostname`; `POST .../custom-hostname/initialize`, `/reverify`, `/activate`; `GET`, `DELETE .../vanity-subdomain`; `POST .../vanity-subdomain/check-availability`, `/activate` (`domains.go`, rules in `internal/domains/README.md`). Studio's Custom Domains page reads them; it first looks for a `custom_domain` entry in the billing add-ons, which both add-on routes report as included. `GET custom-hostname` without a hostname answers 400 "Project does not have a custom hostname configuration" (Studio matches the words "custom hostname configuration"), a refused verification 429 with `Retry-After`, a hostname another project holds 409. Activating or deleting a domain restarts the project's GoTrue through `ApplyConfig` (Auth); if that fails the domain stays saved, the answer is 500 and repeating `activate` retries it. Owners and Administrators change domains, every role reads them (`authz.go`) |
| Upgrades | `GET /v1/projects/{ref}/upgrade/eligibility`, `POST /v1/projects/{ref}/upgrade`, `GET /v1/projects/{ref}/upgrade/status`, `GET /platform/projects/{ref}/service-versions`. See Project upgrades below |
| Backups | `GET /platform/database/{ref}/backups` and `GET /v1/projects/{ref}/database/backups` (the node's base backups and the span a point-in-time restore reaches); `POST /platform/database/{ref}/backups/restore`, `restore-physical` and `/pitr`, `POST /v1/projects/{ref}/database/backups/restore` and `restore-pitr` (restores in place). See Backups and point-in-time restore below |
| Keys | `/v1/projects/{ref}/api-keys`: list, create (publishable and secret keys with names; the secret is shown in full by the create and by `reveal=true`, masked otherwise), get, patch, delete (a revocation), `api-keys/legacy` get and put (`?enabled=`). See Settings and keys below |
| Settings | `GET` and `PATCH /v1/projects/{ref}/config/auth` and `/platform/auth/{ref}/config` (+ `/hooks`), `/v1/projects/{ref}/postgrest` and `/platform/projects/{ref}/config/postgrest`, `config/realtime`, `config/storage` (v1 and platform), `GET` and `PUT /v1/projects/{ref}/config/database/postgres`, `GET` and `PATCH /v1/projects/{ref}/config/database/pooler` (and `/platform/projects/{ref}/config/pgbouncer`; `config/supavisor` is the read), `GET /v2/projects/{ref}/config` (the document the CLI diffs), `PATCH /v1/projects/{ref}/database/password` and `/platform/projects/{ref}/db-password` |
| Database | `database/query`, `database/query/read-only` (rows as JSON; `parameters` supported), `database/migrations` list and apply (`supabase_migrations.schema_migrations`), `types/typescript` (pg-meta generator), `cli/login-role` create and delete, `advisors/*` (no lints yet) |
| Functions and secrets | `functions` list, create, deploy (multipart), get, patch, delete, `body`; `secrets` list (digests), create, delete. Sources, bundles and sealed secrets are stored. Uploads: multipart sources (`POST .../functions/deploy`, `supabase functions deploy --use-api`, the CLI when Docker is not running, Studio's editor; where `Deps.Functions` is set the runtime serves bundles only, because a function run from source files could import other projects' files, so the hook's `api.SourceBundler` bundles the sources in a sandbox and they are stored together with the bundle (`.supavise-bundle.ezbr`, `.supavise-bundle.json`; answers: 400 with the bundler's output for broken code, 501 where the node cannot bundle, 429 when the queue is full); the sources stay readable through `.../body`) and bundles (`POST` create and `PATCH` update with `Content-Type: application/vnd.denoland.eszip` and a body of `EZBR` + Brotli, which is what plain `supabase functions deploy` sends; metadata in the query, `ezbr_sha256` checked, stored as the file `.supavise-bundle.ezbr`; `functions_bundle.go`). `Deps.Functions` (`api.FunctionsHook`, `functions_hook.go`) is told after each change so `internal/functions` can put the files where the Edge Runtime reads them |
| Identity | `/v1/profile`, `/platform/profile` (get, post, patch), `profile/permissions` and `permissions/v2` (computed from the caller's roles), `profile/access-tokens` (list, create, get, delete), `/platform/cli/login` and `/platform/cli/login/{session_id}` (device login) |
| Organizations | `/v1/organizations` list, get, `entitlements`, `members`; `/platform/organizations` list, create, get, patch, delete (see Deleting an organization below), `entitlements` (every feature of the spec's key enum granted), `billing/subscription` (plan stub), `projects` |
| Members and roles | `/platform/organizations/{slug}/members` (list; `PATCH` and `DELETE .../{gotrue_id}`; `PUT` and `DELETE .../{gotrue_id}/roles/{role_id}`), `members/invitations` (list, create, delete by id, get and accept by token), `members/mfa/enforcement`, `roles`, `members/reached-free-project-limit`; `/platform/projects/{ref}/members`; `/v2/organizations/{slug}/members`, `roles`, `PATCH .../members/{user_id}/roles`, `POST` and `DELETE .../members/invitations`. See Members, roles and permissions below |
| Single sign-on | `/platform/organizations/{slug}/sso` (get, post, put, delete: Studio's organization page, one provider), Supavise's `.../sso/providers` (list, create, get, update, delete), `.../sso/pending` (list; `POST` approves and `DELETE` denies `.../pending/{user_id}`); `/v1/projects/{ref}/config/auth/sso/providers` (create, list, get, update, delete: a project's own identity providers, proxied to its GoTrue). See Single sign-on below |
| Studio data | `/platform/projects/{ref}/content` (saved SQL snippets, reports; upsert, list, get, count, delete) and `content/folders` |
| pg-meta | every `/platform/pg-meta/{ref}/*` operation of the spec, proxied to supavise-pgmeta |
| Auth admin | `/platform/auth/{ref}/users` (list, create, patch, delete), `invite`, `magiclink`, `otp`, `recover`, proxied to the project's GoTrue admin API with its `service_role` key |
| Storage admin | `/platform/storage/{ref}/buckets` (list, create, get, patch, delete, empty) and `objects` (list, list-v2, move, copy, delete, sign, sign-multi), proxied to Storage with `x-forwarded-host: <ref>.api.<domain>`; `objects/public-url` (built here, after checking that the bucket is public); `credentials` (the project's S3 access keys, through Storage's admin API) |

Everything else (billing, integrations, replication, log drains, network restrictions, ...) is a stub: it answers a valid empty value and changes nothing.

## Pooler config

`GET /v1/projects/{ref}/config/database/pooler` (what the CLI diffs as `db.pooler.*`), `GET
/platform/projects/{ref}/config/supavisor` and `config/pgbouncer` (what Studio's database settings page reads),
and `/v2/projects/{ref}/config` report the project's `default_pool_size` (default 15) and `max_client_conn`
(default 1000, Supavisor's `default_max_clients`) from the saved settings (`projectconfig.Pooler`).
`PATCH /v1/projects/{ref}/config/database/pooler` (`default_pool_size`, `pool_mode`; `max_client_conn` is accepted
too) and `PATCH /platform/projects/{ref}/config/pgbouncer` (Studio sends `default_pool_size` and the
`ignore_startup_parameters` it was shown) save them and update the project's Supavisor tenant (`EnsureTenant`),
which ends the tenant's pooled connections. A `null` returns a field to its default. The shared Supavisor cannot
honor every field, so these are refused with 400 `{"message": ...}` and nothing is saved: `pool_mode` other than
`transaction` (session mode is the other port, for every project), the PgBouncer settings `server_idle_timeout`,
`server_lifetime`, `query_wait_timeout` and `reserve_pool_size`, `pgbouncer_enabled: false`, an
`ignore_startup_parameters` that is neither empty nor the value the GET reports, a `default_pool_size` of 0
(it would leave the tenant without a database connection) or over the route's limit (3000 on v1, 4950 on the platform route), and a
`max_client_conn` outside 1 to 54000. Two limits depend on the project and the node, and a request above either
is refused with 400 as well: `default_pool_size` may not exceed the project's `max_connections` (the saved
Postgres setting, else the class's) minus 10, which stay free for superusers and the project's own services, and
`max_client_conn` may not exceed `[fleet] pooler_max_client_conn` (5000 unless set), so that one project cannot
claim the shared Supavisor's client capacity. The shipped defaults (15 and 1000) always pass. Lowering
`max_connections` later does not re-check a pool size that was saved before; the next pooler save does. A field that already has the value it would get is accepted, because the
dashboard saves every field it was shown. Both PATCH routes need the permission to update project settings (Owner
or Administrator).

## Settings and keys

Saving a setting (`config_*.go`) validates it (`internal/projectconfig`), stores it, applies it to
what runs (`lifecycle.Reconfigurer.ApplyConfig`: only GoTrue's or PostgREST's unit restarts; the
Realtime and Storage tenants are updated; Postgres settings go through `ALTER SYSTEM`) and answers
with what was saved. If applying fails (GoTrue does not come back on the new environment, a
tenant refuses it) the previous settings are saved and applied again and the caller gets a 502, so
a rejected save leaves the project as it was. Saves of one service of one project are serialized,
run on a context that outlives the request, and are counted by the shutdown drain like every
lifecycle operation. A paused project accepts a save, which applies when it resumes. Secrets in
responses are SHA-256 hashes (`projectconfig.Redact`), never the value. The details of every
setting are in `internal/projectconfig/README.md`.

`GET /internal/templates/{ref}/{name}` serves a project's saved email template to its GoTrue
(no credentials; loopback clients that did not come through the edge proxy only).

API keys: opaque keys are stored as sealed `project_secrets` records (`internal/secrets/apikeys.go`),
so the proxy's key cache, which already drops a project's keys on every `project_secrets` change,
sees a created or revoked key on its next request. The two `default` keys live where rotation
puts them; revoking or renaming one writes a record next to it. Names are lowercase letters,
digits and underscores, unique per project; at most 50 active keys. Only
`secret_jwt_template: {"role": "service_role"}` is supported. `PUT .../api-keys/legacy?enabled=false`
makes the proxy refuse the anon and service_role JWTs (and JWTs signed with the project secret) as
`apikey`, on Functions too, refuse the exact legacy keys as a bearer, as Storage's S3 session token
(`403` XML `AccessDenied`) and inside a Realtime socket (the proxy closes it with `1008`; sockets
opened before the switch reconnect first); it is refused while no
publishable and secret key exist to fall back on. JWTs signed with the legacy secret stay valid in
`Authorization` until the JWT secret is rotated, as on hosted. Rotating the keys makes a revoked
default key usable again with its new value.

The database password reset (`SetDatabasePassword`) changes the `postgres` role in the cluster
(as a SCRAM verifier), then the sealed secret the API connects with, then makes Supavisor drop the
tenant's pools and cached logins; a failure to store the secret puts the old password back.
Passwords need 8 to 128 characters.

## Branches

`Deps.Branching` (a `*branching.Service`, `internal/branching/README.md`) serves `GET/POST/DELETE
/v1/projects/{ref}/branches`, `GET /v1/projects/{ref}/branches/{name}`, `GET/PATCH/DELETE
/v1/branches/{id_or_ref}` (`force=false` schedules the deletion), `POST .../merge|reset|push|restore`
and `GET .../diff`, with the exact spec shapes (`BranchResponse`, `BranchDetailResponse`, ...). Studio calls
these paths itself; the `/platform` twins are the project fields `is_branch_enabled`,
`preview_branch_refs` and `parent_project_ref`. Branches are not listed as projects. Merge, reset and
push answer `201 {workflow_run_id, message: "ok"}` at once and run in the background; the branch's
`status` follows. `?force=true` on merge and push is our extension. A branch has none of the parent's Storage objects and none of its Edge Functions or function secrets, with or without `with_data`, as on hosted (`internal/branching/README.md`, "What a branch contains"): a `with_data` clone keeps the parent's buckets and drops the rows that describe objects, functions come from deploys to the branch's own ref, and the `secrets` field of the create body is refused with 400. `GET /v1/branches/{id}` omits `db_pass` and `jwt_secret` for the default branch (the project's own
secrets stay in the secret store) and answers without them while a new branch's credentials are not
stored yet. Create refuses what the node cannot honor with 400: non-empty `secrets`, a `release_channel`
other than `ga`, a `postgres_engine` other than the parent's; `region` is accepted and the parent's
is used. PATCH refuses a `status` that differs from the branch's own and accepts the deprecated
`reset_on_push` (the spec says it is ignored). Without `Deps.Branching` the list is
the default branch only and a create answers 400. Organization entitlements already grant
`branching_limit` and `branching_persistent`, which is what the CLI checks on a failed create.

## Backups and point-in-time restore

`Deps.Backups` (a `*backup.Service`, `internal/backup/README.md`) feeds the Database > Backups pages of Studio and the Management API's backup routes; `internal/app` passes it when the backup backend is configured. Restores run through the lifecycle Engine (`lifecycle.DatabaseRestorer`), which the API finds on its `Manager`. Without a backup service the list is empty, `pitr_enabled` is false and a restore answers 503.

**What is listed.** The node's base backups (`backup.RestoreWindow`): complete ones on the archive's timeline history whose first WAL file is still archived, the ones a restore can use. Each is `isPhysicalBackup: true` (`is_physical_backup` on v1), status `COMPLETED`, with `inserted_at` the end of the backup and `id` the registry row id of its `backups` entry (failed and running attempts are not listed). `pitr_enabled` and `walg_enabled` are true whenever a backup service exists, because every cluster archives its WAL. `physicalBackupData` (`physical_backup_data` on v1) holds the earliest and latest restorable times in Unix seconds, rounded inward: the earliest is the end of the oldest listed backup, the latest is now for a running project (a restore first archives the source's newest WAL, so any time up to now is reachable) and the newest archived object otherwise. They are absent until the first base backup exists, and Studio then shows "No backups yet".

**What Studio shows.** With `pitr_enabled` true Studio's Scheduled backups tab states that PITR is on and hides the list, as it does for a hosted project with the PITR add-on; the Point in time tab holds the picker, bounded by the span above. A listed backup is restorable through the API (`restore`, `restore-physical`) or `supavise backups restore --to backup`. The tab's text "Database changes are logged every 2 minutes" is Studio's and hosted's; here a restore does not depend on it. "Restore to new project" is not offered: `database:restore_to_new_project` is in `disabledFeatures` (`identity.go`) because Studio's clone request carries a new project name and database password that `backups restore --as` cannot take (the restored cluster keeps the source's passwords), so run that from the command line.

**The PITR add-on.** Studio reads the retention it states from `selected_addons` of `GET /platform/projects/{ref}/billing/addons`. A node with a backup service reports one `pitr` add-on with `meta.backup_duration_days` = `backup.retention_days` (with pruning off, 0, the days since the project was created), priced 0 ("Included"), and no available add-ons: nothing is for sale. The spec limits the variant id to `pitr_7`, `pitr_14` and `pitr_28`; the id is the nearest of them and the real number of days is in `meta` and the name.

**Restoring.** `POST .../backups/pitr` and `.../restore-pitr` take `recovery_time_target_unix`; `.../restore` and `.../restore-physical` take a listed backup's `id` (`restore-physical` also accepts `recovery_time_target`, RFC 3339 or Unix seconds, to replay from that backup to a time). A time outside the span answers 400 with the span in the message, as does a backup that is no longer usable; an unknown id is 404; a node without backups 503; a project that is neither `ACTIVE_*` nor `RESTORE_FAILED` (paused, starting, or already RESTORING) 409, as is one whose disk cannot hold the restored copy next to the current data (`lifecycle.ErrInsufficientDisk`: twice the data plus a fifth of it, at least 1 GiB, free). Otherwise the answer is 201 with no body and the restore runs in the background on a context detached from the request and without a deadline (an expired context would also expire the rollback that stops and starts the project; the log warns when a restore has run 4 hours; `Drain` waits for it, but only until its own timeout, `TimeoutStopSec`, about 11 minutes). `Engine.BeginRestore` moves the project to `RESTORING` under the project lock before the answer, so every other operation (pause, restart, a second restore, delete) is refused with 409 for as long as it runs; `backup.Service.RestoreWith` pauses and resumes the project through the same Engine, which keeps the status at `RESTORING` instead of walking it through `PAUSING`, `INACTIVE` and `COMING_UP`. When the restore ends the project is `ACTIVE_HEALTHY`. If it fails the backup service has put the original data back where it could, and the project becomes `RESTORE_FAILED` whether or not the original is running again, so that Studio's restore screen and a client polling `GET /v1/projects/{ref}` do not read a failed restore as a finished one (Studio shows its "Something went wrong while restoring your project" screen; the project's data is the original where the rollback worked). Health checks leave `RESTORE_FAILED` alone. A second restore (listing and restore routes accept the project), a pause followed by a resume, or a delete moves it on; a restart does both pause and resume, so it clears the status when the original data starts. The error is in the log and in the project's `restore.failed` event. A daemon that stops during a restore leaves the project `RESTORING` (`Engine.Recover` does not touch it, and nothing starts it): `internal/backup/README.md`, "From the dashboard and the Management API", says how an operator settles it. The old data directory is kept as `projects/<ref>/postgres/data.pre-restore-<time>` next to the new one; after a restore that worked the Engine removes the older `data.pre-restore-*` directories and every `data.failed-restore-*`, so one copy stays per project (after a failed one only the newest `data.failed-restore-*` stays). The Engine also sets the cluster's role passwords back to the registry's after a restore that worked, so a restore to a time before a database password reset does not leave the cluster and the registry disagreeing. A restart of the daemon is not safe while a restore runs, since `Drain` gives up after about 11 minutes.

Not implemented, left as stubs: `restore-point`, `schedule`, `undo`, `download`, `downloadable-backups`, `enable-physical-backups`.

## Authentication

| Route family | Credentials |
|---|---|
| `/platform/*` | GoTrue session JWT from `supavise-gotrue@system`: HS256, signed with the system project's JWT secret (`Manager.Keys("system")`; re-read when a signature fails, at most once every 5 seconds, so a rotation takes effect and garbage tokens cost nothing), audience `authenticated`, an `exp` claim, not anonymous, **and an admin** (below). The session is identified by audience and signature, not by the `role` claim: users created through GoTrue's admin API have an empty `auth.users.role`, so their tokens carry `role: ""` (docs/research/08 section 9); tokens with role `anon` or `service_role` are refused |
| `/v1/*`, `/v2/*` | `sbp_` personal access token (`sbp_` + 40 hex, also `sbp_v0_`, `sbp_oauth_`; looked up by `secrets.HashToken`, expiry honored, `last_used_at` touched at most once a minute) **or** a dashboard JWT |
| `GET /platform/cli/login/{session_id}` | none (the CLI has no token yet); guarded by the verification code |

**Admin gate.** A valid session is not enough: the user needs `app_metadata.supavise_admin = true`
(`api.AdminClaim`; GoTrue lets users edit `user_metadata` but not `app_metadata`) or an email in
`[api] admin_emails`, otherwise the answer is `403`. Supavise sets the claim on every dashboard user
it creates, and `supavise-gotrue@system` creates no other user: its sign-up is open to GoTrue (an SSO
user is created by signing up) and closed by the **before-user-created hook**, which the daemon
answers and which allows registered SSO providers and invited addresses only (Single sign-on,
below); the gate is defense in depth if that is ever lost. A user whose account came from SSO has
no claim and is admitted by the SSO rules instead.
The email allowlist trusts the JWT's `email` claim: GoTrue's access token carries no claim
that proves the address was confirmed, so a registered SSO provider that vouches for an
allowlisted address would be believed. Prefer the `supavise_admin` claim and roles.
A PAT is not re-checked against the user's admin status (the claim above) on each use: delete a
user's tokens when you remove their account. It is checked against its owner's roles at every request
(Members, roles and permissions, below), so removing the owner from an organization takes the access away at once.
Authentication only says who the caller is; what the caller may do is the authorization of the next section.

Dashboard users are recorded in `supavise.api_users` on first sight (profile fields come from
GoTrue's `user_metadata`; later edits win). What a user may do is decided by their roles, below.

### Claim and invite (`GET` and `POST /claim`, `claim.go`)

`supavise-gotrue@system` has sign-up disabled, so dashboard accounts come only from here. A claim token
(`sbc_` + 48 hex) creates the first administrator, an invite token (`sbi_`) creates one user for a
fixed address. Only the SHA-256 is stored (`supavise.claim_tokens`, migration `0600`); a token works
once and expires (claim 72 hours, invite 7 days). `POST /claim {token, email, password,
organization_name}` consumes the token with one conditional `UPDATE`, creates the user through
GoTrue's admin API on `supavise-gotrue@system` with the system `service_role` key (`email_confirm: true`,
`app_metadata.supavise_admin = true`), and for a claim token creates the first organization (or keeps
the existing one); the answer is `201 {email, user_id, organization, dashboard_url}`. When the user
cannot be created (the address exists, the password is refused) the token is released and answers
again. Unknown, used and expired tokens all answer `403`; ten failures in a minute answer `429` for
the rest of the minute, node-wide. `GET /claim` serves a self-contained page (one inline script
pinned by hash in the CSP, no third-party requests). Neither route takes credentials: the token is
the credential. The claimed first user becomes Owner of the organization. An invite token is bound to the one
organization invitation it was issued for (`claim_tokens.invitation_id`, migration `0901`): redeeming it accepts that
invitation and no other, so the link an Administrator of one organization holds cannot take a seat in another
organization that invited the same address, and it dies with its invitation (replaced, revoked, accepted or expired).
Invitations of other organizations to the address wait for the invitee's own sign-in (`/join`). A token that is
bound to no invitation creates an account with no membership. Redeeming records the account in
`supavise.api_users`; the claim page fills the token and address from the link's `#token=...&email=...` fragment, which a
browser never sends to a server. `Accounts` (the same type the CLI uses for `supavise claim token`, `supavise users invite|list|role|remove`)
also lists dashboard users and removes one, which ends the user's access at once and takes the user's seats away:

1. the user's memberships and project roles are removed. This is the step that can refuse: it fails, and
   changes nothing, when the user is the only Owner of an organization (unless `--force`);
2. the user is recorded in `supavise.removed_users` (migration `0610`), and from then on `authJWT` refuses
   a session whose `sub` is in it and `authPAT` refuses a token whose owner is in it, on every request
   and with no cache, whichever process made the removal (a GoTrue access token would otherwise stay
   valid until it expires, an hour, and could mint a personal access token that never expires);
3. the personal access tokens the user created are deleted;
4. the GoTrue account is deleted, which also deletes its sessions and refresh tokens.

A step that fails leaves the account findable, and step 2 has already cut the access off, so
running `users remove` again finishes the job. `claim token --if-none` issues nothing while an unused,
unexpired claim token exists (the installer uses it, so a re-run does not replace a token handed over
earlier).

### `supabase login` (device flow)

Studio's `/cli/login` page calls `POST /platform/cli/login {session_id, public_key,
token_name}` with the user's session. The server mints a PAT, seals it to the CLI's P-256
public key (ECDH, shared secret as the AES-256-GCM key) and returns `{nonce}`; the first 8
hex digits of the nonce are the verification code the page shows. The CLI then polls
`GET /platform/cli/login/{session_id}?device_code=<code>` and gets
`{access_token, public_key, nonce}`. A session works once and lives 10 minutes; five wrong
codes (counted in place with one atomic update) destroy it and its token. A session that
expires unclaimed, or is replaced by a new authorization with the same id, takes its token
with it (reaped on the next login request or claim attempt). Protocol read from `supabase/cli` (`login-crypto.layer.ts`,
`ensure-login.ts`) and Studio (`pages/cli/login.tsx`). Set `[api] disable_device_login` to
turn it off.

## Members, roles and permissions

Roles are hosted's four, with hosted's ids kept in code (`internal/members/roles.go`): **Owner** (1),
**Administrator** (2), **Developer** (3) and **Read-only** (4), organization-wide or limited to projects.
Capabilities follow hosted's access-control documentation and the role descriptions in Studio's Team page:

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
| Create a branch with data (`with_data: true`; it copies the parent's data), or reset one that has data (it clones the parent's current data again) | yes | yes | | |
| Read everything else: config, logs, advisors, users, buckets, functions, secrets (digests), `SELECT` SQL, types | yes | yes | yes | yes |
| Saved SQL snippets: create; change or delete one's own (Owner and Administrator: anyone's shared ones) | yes | yes | yes | yes |
| Saved reports: same, but Read-only may not create or change them | yes | yes | yes | |

A **project-scoped role** is a base role held on a set of projects. The member sees only those projects (lists
filter, the other projects answer 403) and has the base role's permissions on them; on the organization itself the
member can read it and its members, nothing else. Each project holds one role per member: giving a member another
scoped role on a project takes the project out of the first. A member with no organization-wide role and no project
has no access (the "no-access" state). **An organization always keeps one Owner**: demoting, removing or leaving as
the last Owner answers 400 (`ErrLastOwner`), concurrent changes included (the check runs in a transaction that locks
the organization). Members may always leave. `POST /platform/organizations` needs an Owner role somewhere (or an
empty node); the creator owns the new organization.

**Deleting an organization** (`DELETE /platform/organizations/{slug}`, Owners only; `supavise orgs delete <slug> --yes`
for the operator, `supavise orgs list` shows what exists, and without `--yes` the command lists what it would delete
and stops) does what hosted does, whose dialog says the organization is deleted "and remove all of its projects":
the projects go through the lifecycle manager, so each takes its final base backup as a normal project delete does
(a branch goes first, through the branching service). The steps are `OrgDeleter` in `org_delete.go`, in an order that
keeps a failed run harmless and lets a second run finish it:

1. The organization's SSO providers are removed like `.../sso/providers/{id}` does (GoTrue drops the provider, its
   users lose their seats and tokens, its record, users and refusals go), except that the last-owner rule does not
   apply to the organization being deleted. This comes first because it can refuse (403, 409) before any project
   is touched.
2. The projects are deleted. One that cannot be deleted (its final backup failed) answers 500, or 409 for a project
   in a state that refuses a delete, naming the project; the organization, its members and the projects not yet
   reached stay, and the Owner runs the request again.
3. The organization row is deleted. Foreign keys cascade from it to the members, project-scoped roles, invitations
   (and the invite tokens bound to them), the MFA setting and the default-role rules, so nothing is left behind; the
   row's `on delete restrict` from `projects` refuses (409) when a project was created in the meantime. The audit
   event `org.deleted` is recorded under the system project.

The node's **last organization is never deleted** (409). Studio copes with none (it shows its "no organizations"
page), but this API gives a node with no organization a "Default" one owned by whoever lists organizations first
(`allOrgs`), which would make an arbitrary member of the deleted organization the Owner of a new one. The
one-time sign-up grants that invitations created (`signup_grants`) are keyed by address, not by organization, and
expire on their own.

### Enforcement

`authz.go` holds the route table: every operation of the three specs, and every extra route Supavise serves, resolves
to an action and a resource of hosted's `PermissionAction` model (`tenant:Sql:Admin:Write` on `migrations`,
`write:Update` on `custom_config_gotrue`, `infra:Execute` on `reboot`, ...), checked against the caller's effective
permissions before the handler runs. A row of the table is `method, path template, need`; `TestRouteTableCoversTheSpecs`
fails for a rule that no route reaches. The defaults are deny: a write no rule names needs `write:Update` on the
project (Owner, Administrator) or on the organization (Owner), and a write outside any organization or project
needs the Owner role somewhere. Routes about the caller's own account (profile, tokens, notifications, telemetry)
and the invitation the caller holds the token of are open to every signed-in user. `TestRoleMatrix` drives about a
hundred representative routes as an Owner, an Administrator, a Developer, a Read-only member, a project-scoped
Developer and a user without a membership through the real handlers.

- **Branches follow their parent.** A branch is a project of its own, but the roles that govern it are those of
  its parent project: a role scoped to a project covers the project's branches (`scopeRef`), and a route that names a
  branch by id or ref (`/v1/branches/{branch_id_or_ref}/**`) is resolved to the parent before the check
  (`branchParent`): read needs `read:Read` on `preview_branches`, delete `write:Delete`, the other writes
  `write:Update`. `POST /v1/projects/{ref}/branches` with `with_data: true` also needs the permission to update
  the project (Owner or Administrator, `requireBranchData`), checked by the handler because the route table cannot
  see the body; the denial is the usual 403 `{"message": "Your role does not allow this action (...)"}` and nothing
  is created (`TestBranchWithDataNeedsOwnerOrAdministrator`). `POST /v1/branches/{id}/reset` on a branch that
  has data needs the same permission, because a reset clones the parent's current data again; the handler looks
  the branch up first, and a schema-only branch keeps the Developer permission
  (`TestResetOfBranchWithDataNeedsOwnerOrAdministrator`). `GET /v1/branches/{id}` leaves out `db_pass` and `jwt_secret` for a caller who cannot read the
  project's keys (Read-only). `TestBranchRoutesFollowTheParentsRoles` covers it, and
  `TestImplementedRoutesOpenToEveryUserAreAllowlisted` fails for a hand-written route that is open to every
  signed-in user and is not on the short list.

- **The same list serves Studio.** `GET /platform/profile/permissions` returns the caller's entries
  (`actions`, `resources`, `condition`, `organization_slug`, `project_refs`, `restrictive`), and the server evaluates
  them with the semantics of Studio's `doPermissionsCheck` (`internal/members/permission.go`): `%` wildcards, a
  matching restrictive entry beats any grant, entries scoped to a project take over for that project, an empty list
  denies. `TestPermissionsEndpointMatchesEnforcement` decodes the JSON and checks that Studio's rule and the
  server's agree for every role. Owners get `%` on `%`; Administrators the same minus restrictive entries for
  organization settings, project transfer and role changes of Owners (a json-logic condition on
  `resource.role_id`, listing the Owner role and every project-scoped Owner role); Developer and Read-only are
  allow lists.
- **Read-only SQL is read-only in the database.** Whoever lacks `tenant:Sql:Write:Insert` has their SQL run as
  the `supavise_read_only` role on every path: `POST /v1/projects/{ref}/database/query` (the MCP server's `execute_sql`),
  Studio's `POST /platform/pg-meta/{ref}/query` and the other pg-meta reads, and `cli/login-role` hands such a caller
  the read-only login role (`supavise_cli_ro_*`) whatever `read_only` says. `TestIntegrationRoles` runs inserts,
  updates, deletes, DDL, `begin read write`, `set session characteristics` and `reset role` against a real
  Postgres through both routes.
- **Secrets stay with the roles that may read them.** For a caller without `read:Read` on `service_api_keys`,
  `field.jwt_secret` and `storage.s3_credentials` (Read-only), `GET .../settings` omits the JWT secret and the
  service_role key, `config/postgrest` blanks `jwt_secret`, `api-keys?reveal=true` and the temporary key answer 403 and
  the S3 credentials are not listed.
- **Personal access tokens carry their owner's permissions**, read at each request: demote or remove the member
  and their tokens lose the access at once (`users remove` also deletes the tokens). **Dashboard sessions** are
  checked the same way. A token is not an interactive session, so the MFA requirement below does not apply to it; that is why minting one
  (`POST /platform/profile/access-tokens`, `POST /platform/cli/login`) needs an aal2 session when the caller belongs to
  an organization that requires MFA. Tokens minted before the requirement was turned on keep working.
- **MFA requirement.** `GET` and `PATCH /platform/organizations/{slug}/members/mfa/enforcement` store and report
  it (the answer is 201 on both, as the spec says), and `organization_requires_mfa` in the organization list follows
  it. While it is on, a dashboard session whose JWT does not carry `aal: aal2` is refused in the organization's
  routes (403 `MFA required`), still sees the organization list, profile and permissions, and cannot accept an
  invitation; turning it on needs an aal2 session, so an Owner cannot lock themselves out. `mfa_enabled` of a member
  is always false (GoTrue's factor list is not queried). **Exemption for SSO:** a dashboard session that came from a
  registered SSO identity provider is treated as aal2, whatever the provider did (see Single sign-on below). Every
  registered provider satisfies the requirement, including one an Administrator registered, and the aal2 check on minting
  personal access tokens as well. An Owner who turns the requirement on and does not trust a provider's authentication
  should not let that provider's users in (remove it, or keep its default role at none and approve users by hand).

- **Saved content belongs to its owner.** A Developer or Read-only member may create saved items, but change, rename or
  delete only their own (folders included): the permission entries carry the condition `resource.owner_id ==
  subject.id`, Studio passes both in its checks, and the content handlers repeat the check against the stored item
  (the route table only checks the member's own item, `chkOwn`). Otherwise a member could rewrite a shared snippet
  that an Owner later opens and runs as `postgres`. `last_updated_by` records the real editor (migration `0902`).
- **Judgment calls against hosted's table.** Hosted's access-control page lists Developers under Auth Hooks
  (create, delete); Supavise stores hooks in the Auth settings (`custom_config_gotrue`), which only Owners and
  Administrators may change, so Developers cannot manage hooks. Developers hold the Restart row as listed; hosted
  also lets them restore backups, but a restore overwrites the project's data, so here it needs Owner or Administrator
  (`queue_job.restore.prepare` and `queue_job.walg.prepare_restore`, the resources Studio checks, so its Restore
  buttons are disabled for a Developer too). The Read-only role's secret list (service key, JWT secret, S3 credentials) follows the same page.
- **Residual exposure of Read-only SQL.** The write barrier is table privileges (`pg_read_all_data` only) plus
  `default_transaction_read_only`, which a client can override with `set` or `begin read write`. A Read-only
  member can therefore still call a `SECURITY DEFINER` function that `PUBLIC` may execute (Postgres's default for
  new functions) and write through it. Hosted's read-only role has the same exposure. Revoke `EXECUTE` from `PUBLIC`
  on such functions.

### Invitations

`POST .../members/invitations` takes both body shapes of the spec (`emails` + `role_id` + `role_scoped_projects`,
Studio's; or `data[].attributes` with `role` and `projects`) and answers `{succeeded, failed}`; an address that
is already a member lands in `failed`, an invalid address is a 400, and a caller who may not invite the role gets a 403.
Supavise adds `invite_links: [{email, url, emailed}]` to that answer. The token is `sbo_` + 48 hex, stored as its
SHA-256 (`supavise.org_invitations`), works once and expires after 7 days (hosted: 24 hours; here the link often travels
by hand); inviting an address again replaces the pending invitation, which needs the permission to revoke it (an
Administrator cannot cancel an Owner's invitation by re-inviting the address with a lower role; the answer is 403). Studio's `/join?token=...&slug=...` page uses
`GET` and `POST .../invitations/{token}`: the signed-in user's email must match (case-insensitively) and joins with the
invited role, or with the invited project-scoped role on the projects that still exist.

How the invitee is told:

- With **`[mail]` configured** (an SMTP relay for `supavise-gotrue@system`, rendered into its `GOTRUE_SMTP_*`), supavise-gotrue sends
  the message: GoTrue's admin invite for an address without an account (the account is created confirmed and gets
  `supavise_admin`; the link signs the person in and lands on the invitation), a sign-in link (`/magiclink`) that lands on
  the invitation for an existing account. If the relay fails the invitation stays and the caller gets the link.
- **Without mail**, the answer carries the link and the server does not log it (the claim URL is a credential; the log
  records the address, organization and invitation id only): the invitation page for an
  existing account, the claim page (token and address prefilled) for a new address, where the invitee picks a password
  and joins with the invited role in one step. `supavise users invite <email> --role <role> [--org <slug>] [--project <ref>]...`
  prints that link on stdout.

### Bootstrap, SSO and the CLI

- The claimed first user is Owner. Dashboard accounts that existed before roles keep full access: migration `0900`
  makes every user the API had seen Owner of every organization, and any other account created before the migration
  became Owner of every organization on its first request (the account's creation time comes from `supavise-gotrue@system`;
  a failed lookup denies and retries after a minute; each account is looked at once, so removing it from an
  organization sticks). Accounts created afterwards have no access until they are invited, claimed or granted a role.
- `members.Service.GrantSSODefault(ctx, userID, email)` is what the SSO rules call for a first-time SSO sign-in
  (`DashboardSSO.Admit`): the email domain's rule (`supavise sso add --domain ... --default-role ...`, or by hand
  `supavise users default-role set <domain> <role> [--org]`, table `supavise.sso_default_roles`) makes the user a member of its
  organization with its role, provided a registered identity provider that vouches for the domain is the one the user
  signed in through. A domain without a rule grants nothing; an existing membership is never changed.
- `supavise users invite|list|role|remove|default-role` (see `supavise users --help`): `role` sets an organization-wide role
  as the operator and also adds a user to an organization, which is how an organization without an Owner gets one
  back; `remove` refuses to delete the only Owner of an organization unless `--force`. `supavise functions dev --token-file` seats a stand-in user (`members.StandInOwnerID`) as Owner of every organization while it runs; the stand-in never counts as an Owner for these rules (`CountOwners` leaves it out), and the daemon removes its seats and tokens at start (`SweepStandIn`) in case a crash left them.

## Single sign-on

Dashboard sign-in with SAML 2.0 (Okta, Entra ID, Google Workspace, anything that speaks it), and the same for the end
users of a project. Implemented by `sso_dashboard.go` (the service, shared with `supavise sso`), `sso_routes.go` (the
routes), `sso_hook.go` (the sign-up hook) and `internal/sso` (the GoTrue admin client, metadata checks, signing keys,
the hook's signature). `internal/sso/README.md` has the building blocks.

**Dashboard.** `supavise-gotrue@system` runs with SAML on and a signing key of its own (RSA 2048, sealed as the system secret
`saml_private_key`). An Owner or Administrator registers a provider (`supavise sso add`, `POST .../sso/providers`, or
Studio's organization SSO page): GoTrue gets the provider and its email domains through its admin SSO API, Supavise
records the organization, the domains and the role a first-time user gets (`--default-role`; none means the user waits
for approval), and the default-role rule of each domain is set. The identity provider is configured with
`https://api.<domain>/auth/v1/sso/saml/acs` (assertion consumer service) and
`.../sso/saml/metadata` (entity id; `?download=true` gives a long-lived metadata file). Studio's sign-in page asks for an
email address, GoTrue's `POST /auth/v1/sso` finds the provider by the domain, and the browser goes through the identity
provider and back to `/auth/v1/sso/saml/acs`, which the proxy's dashboard-auth route forwards to `supavise-gotrue@system`
like every other `/auth/v1` path.

- **Who gets in** (`DashboardSSO.Admit`, on every request of an SSO session): the session's
  `app_metadata.provider` is `sso:<provider id>`; the provider must be registered with Supavise (a provider created in
  GoTrue behind Supavise's back opens nothing, and a removed one ends its sessions within ten seconds); the user must belong
  to an organization. On the user's first request the email's domain decides: a registered provider that vouches for
  the domain, and a default-role rule of the provider's organization for it, make the user a member with that role.
  Any other user is recorded as pending and refused on every `/platform` and `/v1` route with `403` until an
  administrator approves them (`supavise sso approve`, `POST .../sso/pending/{user_id}` with a role) or invites or promotes
  them the usual way; denying (`supavise sso deny`, `DELETE`) deletes the account. The default role is for the first
  sign-in only: a user who later loses every membership waits again and is not given it again, and neither is a person
  whose address an administrator denied or removed (next bullet), whose new GoTrue account is a first sight of a
  new user id but not a first sign-in of the person. A second provider
  cannot give its users another provider's default role by asserting that provider's domains. A personal access token
  that an SSO user made carries the user's roles like any other, and is held to the same gate: while the user waits for
  approval (they lost every membership, or were denied) the token is refused (403) as the session is. Removing a
  provider revokes the tokens of its users. An SSO
  session meets an organization's "require MFA" and the aal2 check on minting personal access tokens (GoTrue marks it aal1
  whatever the provider did, and the provider is where strong authentication is enforced; the switch would otherwise lock
  out every SSO user). The exemption is unconditional: Supavise cannot see how strongly a provider authenticated its user,
  so any registered provider, including one an Administrator registered, satisfies the requirement. There is no
  per-provider setting yet.
- **Who may change a provider.** The provider's default role is the role a first-time user gets, taken from the provider's
  own record (`sso_providers.default_role`) at the moment of the first sign-in; the domain's default-role rule is kept in
  step by `add` and `update` (a rule of the organization that was there before is replaced, or removed when the role is
  none) and a rule that has drifted from the provider does not raise the role. A domain that has another organization's
  rule is refused (409) unless the caller owns that organization (or is the operator). An Administrator cannot register
  or change a provider so that it hands out Owner. Changing what a provider vouches for (metadata, attribute mapping,
  name id format, switching it off) and removing it also need the caller to be able to grant every role that the
  provider's users hold, in any organization and on any project: whoever controls the provider controls the accounts it
  signs in (GoTrue links an identity provider's user to the existing account by name id or verified email), and removal
  ends their sessions. A provider with an Owner among its users is the business of an Owner or the operator (403 for
  an Administrator).
- **One address, several accounts.** GoTrue keeps email addresses unique among password accounts only (its index skips
  SSO users), and an identity provider may vouch for any address, including one that has a password account. The two
  are separate accounts, and the SSO one is the newer, so it comes first in GoTrue's list. Nothing that acts on an
  address takes the first match. `Accounts.ResolveUser` resolves an address to the password account when there is
  exactly one, and to the only account when a single one exists; with several SSO accounts and no password account, or
  with a selector that matches none, it refuses (`AmbiguousUserError`, listing ids and providers). `supavise users role`
  and `supavise users remove` take `--user-id` and `--provider` (`email`, or an identity provider id) to name another
  account and print when the account they changed is an SSO one; `supavise users list` shows how each account signs in
  and its id. Invitations (`users invite`, the Team page) look at password accounts only: an address that only an
  identity provider has an account for gets the link that creates a password account. A first sign-in whose address
  has a password account is logged and recorded in the `sso.user.first_sign_in` event (`shares_email_with`); the
  SSO account gets the provider's default role like any other, and nothing of the password account.
- **Denying and removing.** `deny` re-checks the memberships first: a user who became a member since the last request
  (invited, `supavise users role`) is not denied (409) and the account stays. Deleting the GoTrue account does not keep the
  person out: signing in through the identity provider again creates a new account with a new user id. So `deny`, and
  `supavise users remove` of an SSO account, also record the refusal by provider and lower-cased email address
  (`sso_denied`), before anything is deleted. A refused address that signs in again gets a new account that is pending,
  without the default role (the `sso.user.first_sign_in` event says `default_role_withheld`), and is refused on every
  route until an administrator approves it (`supavise sso approve`, `POST .../sso/pending/{user_id}`; the approval clears
  the refusal) or runs `supavise sso allow <email>` (a waiting account of the address is forgotten, so its next request is a first sight again, default role included).
  Inviting or promoting the person the usual way lets them in too, and leaves the refusal in place for a later removal
  to renew. The refusal belongs to the provider: removing the provider drops it. Removing a provider removes the memberships
  and project roles of the users that signed in through it, in every organization (the accounts cannot sign in again),
  before anything else changes; when one of them is the only Owner of an organization the removal is refused (409)
  until another Owner exists, as `supavise users remove` refuses it. The provider is deleted from GoTrue (a 404 there counts
  as done) before Supavise's own record goes, and the record goes last, so a removal that stopped at GoTrue is finished by
  running it again. The users' personal access tokens are revoked between the two: a token that cannot be revoked
  stops the removal with 502 and keeps the record (a token whose owner has no SSO record passes `AdmitUser`, which
  takes such an owner for a password account), and the retry finishes. Deleting the organization removes its
  providers the same way (see Deleting an organization under Members, roles and permissions).
- **Domains are claimed node-wide.** GoTrue finds the provider by the email domain, across the whole node, and Supavise
  does not verify that the registrant controls the domain: the first provider to register a domain gets it (a second
  one is refused, 409). A provider is also refused for a domain that another organization's default-role rule holds,
  with or without a default role, unless the caller owns that organization (the operator may). An organization that
  registers a provider for a domain that another organization's members use, before that organization did, takes over
  the "Continue with SSO" sign-in for those addresses, so on a node with several organizations the Owners should
  register their domains early and `supavise sso list` shows who holds which. There is no DNS verification yet.
- **Sign-up stays closed.** GoTrue's own switch (`GOTRUE_DISABLE_SIGNUP`) also stops an SSO user's first sign-in, so
  it is off on `supavise-gotrue@system` and its before-user-created hook is on: GoTrue asks `POST
  /internal/hooks/before-user-created` on the loopback admin listener (signed with a secret derived from the master key
  as Standard Webhooks specify; a request that came through the edge proxy or whose signature is wrong is refused) before
  it creates any user, and the daemon allows exactly a user of a registered SSO provider and an address that an
  administrator invited by mail (the invite carries a one-time `supavise_grant` in its metadata that the daemon issued for
  that address and spends in its answer; a person who merely knows the address cannot sign up with it first).
  Everything else is refused (`403`, "Sign-up is closed on this dashboard"), and while the daemon does not answer GoTrue
  creates nobody. Users `supavise` creates itself (the claim page, `users invite` without mail) go through GoTrue's admin
  API, which has no hook.
- **Studio's button.** Studio shows "Continue with SSO" only while the feature `dashboard_auth:sign_in_with_sso` is
  enabled, which its build disables by default (`NEXT_PUBLIC_DISABLED_FEATURES`, patch 0002, no new patch). While the
  dashboard has a provider, `fleet` starts Studio with that list minus the SSO entry; the API re-renders and restarts
  Studio's unit (`Deps.StudioRefresh`, in the background) when the first provider appears and when the last one goes,
  and `supavise sso add|remove` does the same before it returns.
- **Studio's organization page** (`/platform/organizations/{slug}/sso`) is served for real: one provider per
  organization, `join_org_on_signup_enabled` and `join_org_on_signup_role` are the default role, `enabled` is GoTrue's
  disabled flag; before one exists the page gets the 404 it reads as "not set up". Several providers per organization
  and the pending list are Supavise's own routes.

Provider changes, a user's first sign-in, approvals and denials are events of the system project in the registry
(`sso.provider.added|updated|removed`, `sso.user.first_sign_in|approved|denied|allowed`; ids, domains and role names only).

**Projects.** `/v1/projects/{ref}/config/auth/sso/providers` (create, list, get, update, delete) is a proxy to the
project's GoTrue admin SSO API with the exact shapes of the spec (the list carries each provider's metadata document,
which GoTrue's own list leaves out). As on hosted, it answers `404` ("SAML 2.0 support is not enabled for this
project") until `saml_enabled` is on in the project's Auth settings; turning it on renders `GOTRUE_SAML_ENABLED` and
the project's own signing key (sealed project secret `saml_private_key`, created on first use, never shared, not a
setting and not rotated by `rotate-keys`) into its GoTrue, which restarts. Writes need the right to change Auth
settings (Administrator, Owner), reads the right to see the project. `saml_external_url` and
`saml_allow_encrypted_assertions` are settings too. `supabase sso add|list|show|update|remove --project-ref` work with
`--profile` unchanged (Supabase CLI 2.119.0, `tests/linux/sso-smoke.sh`).

**Limits.** Dashboard providers take SAML metadata as an https address (GoTrue fetches and refreshes it) or as a document; `supavise sso add` also
fetches a plain-http address on this machine (a development provider) and passes the document, which GoTrue does not
refresh. There is no OIDC provider and no SCIM. A cloned or restored project starts with SAML off like every other
setting. A user with a pending invitation who signs in through a provider without a membership is refused until
the invitation is dealt with another way: `/join` is behind the same gate. Dashboard OAuth sign-in (Google, GitHub,
Azure) behind the same allowlist is not built.

## Configuration (`[api]` in config.toml, or `SUPAVISE_API_*`)

| Key | Meaning |
|---|---|
| `allowed_origins` | extra CORS origins besides the Studio URL (`*` is not accepted: Studio sends credentials) |
| `pgmeta_crypto_key` | passphrase shared with supavise-pgmeta (`CRYPTO_KEY`). Empty: a random key kept sealed in the registry as system secret `pgmeta_crypto_key`, created by `api.EnsurePGMetaCryptoKey(ctx, reg, sec)`. The unit renderer for supavise-pgmeta must call that function before it renders the unit; the API calls it on first use and fails (no ephemeral key) when the registry cannot store the key |
| `public_url`, `dashboard_url` | override the derived `https://api.<domain>` and `https://studio.<domain>` |
| `disable_device_login` | turn off the browser login flow |
| `admin_emails` | comma-separated emails allowed to use the API without the `supavise_admin` claim |

`[mail]` (or `SUPAVISE_MAIL_*`) is the SMTP relay `supavise-gotrue@system` sends the dashboard's mail through, used for
organization invitations: `smtp_host`, `smtp_port` (587), `smtp_user`, `smtp_pass`, `smtp_from`, `smtp_name`. It
needs a host and a sender; without it no mail is sent. The password sits in `config.toml` in plain text, so keep the
file mode 0600. Changing it renders the system GoTrue's environment again; restart `supavise-gotrue@system` for it to apply.

## What this package needs from the rest of the system

- **`Manager.ConnString(ref, role)`** for roles `postgres` (SQL, migrations, the Studio
  pg-meta proxy, types) and `supabase_admin` (creating login roles and the read-only role).
  Parameterized queries connect with pgx to that DSN directly; everything else goes
  through pg-meta. The DSN must be a URL (`postgres://...`), and the project's `pg_hba`
  must accept password (SCRAM) logins on loopback for the roles below.
- **Studio's pg-meta runs as `postgres`**, not `supabase_admin` as upstream's self-hosted
  Studio does: objects made in the SQL editor then belong to the role the CLI and
  migrations use and can be altered or dropped by them. A feature that needs a superuser
  would fail with a permission error; that is the trade-off taken.
- **Read-only SQL runs as the role `supavise_read_only`**: `login`, `bypassrls` (as upstream's
  `supabase_read_only_user`: `pg_read_all_data` alone does not bypass row level security, so
  RLS tables would read as empty) and member of `pg_read_all_data`, `default_transaction_read_only = on`
  as a second layer. The API creates it on demand (as `supabase_admin`; a cheap catalog check
  every 2 minutes per project, a write only when the role differs) with a password derived from the
  project's admin password (HMAC) and a deterministic SCRAM verifier, never cleartext. A client cannot
  write by sending `begin read write` or `reset role`: the role lacks the privileges
  (`TestIntegrationDatabase` runs those escapes on both read-only routes). Used by
  `database/query/read-only` and `database/query` with `read_only: true`, which the MCP
  server's `--read-only` mode relies on.
- **CLI login roles**: `cli_login_<rand>` (member of `postgres`, `set role = postgres`) for
  read-write; read-only requests get `supavise_cli_ro_<rand>` (`bypassrls`, member of `pg_read_all_data`;
  `pg_dump` runs with `row_security = off` and needs it on RLS tables),
  because the CLI runs `SET SESSION ROLE postgres` after connecting as any `cli_login_*`
  user (cli-go `internal/utils/connect.go`) and a read-only role cannot do that. Both expire
  after an hour, are dropped by the next create or by `DELETE .../cli/login-role` (which needs the right to change database roles: it drops every member's), and are
  created with a SCRAM verifier. The CLI's `db dump --role-only` skips only `cli_login_*`, so
  it lists `supavise_read_only` and any live `supavise_cli_ro_*` role; a read-only role cannot use
  the `cli_login_` name (see above), so this stays a known limit.
- **supavise-pgmeta** at `127.0.0.1:<ports.pgmeta>` with `CRYPTO_KEY` equal to the key above. The
  `x-connection-encrypted` header is built in `cryptojs/` (crypto-js passphrase AES, checked
  against vectors from the real library in `testdata/cryptojs/`). pg-meta also listens on
  `port+1` for its admin app.
- **GoTrue** per project at `127.0.0.1:<PortsFor(ref, seq).GoTrue>` and **Storage** at
  `127.0.0.1:<ports.storage>` (multi-tenant, tenant from `x-forwarded-host`).
- **`Manager.Create`** may block until the project is healthy: `POST /v1/projects` returns
  `201 COMING_UP` as soon as the registry row exists, or after ten seconds with the
  allocated ref (never a 504, which would invite a retry that creates a second project;
  clients poll `GET .../projects/{ref}`, which answers 404 until the row exists). Better
  for clients: have `Manager.Create` insert the registry row before it does anything slow.
- **`supavise-gotrue@system`** with SAML on, its before-user-created hook pointing at this daemon's loopback listener
  (`lifecycle.SystemAuth`, rendered by `supavise system init` and, at every daemon start, by `RefreshSystemAuth`), and
  `app_metadata.supavise_admin = true` on each password account Supavise creates (see Authentication).

## State

Migrations `internal/registry/migrations/0100_api.sql` and `0101_api_login_failures.sql`
(range 0100-0199): `api_users`, `api_cli_login_sessions`, `api_functions`,
`api_function_files`, `api_function_secrets`, `api_content`, `api_content_folders`. `Store` (`store.go`) has a Postgres and a memory
implementation behind one conformance suite (`store_test.go`). Members, roles and invitations are in
`0900_members.sql` (range 0900-0999: `org_members`, `org_project_roles` and `org_project_role_refs`, `org_invitations`,
`org_mfa`, `sso_default_roles`, and the legacy-account bookkeeping; `0901_claim_token_invitation.sql` binds invite claim
tokens to their invitation; `0902_content_updated_by.sql` records who edited a saved item last). The legacy rule (an account
from before roles becomes Owner of every organization that existed when roles began, on its first request) runs once per account: writing any membership for an account settles it, so removing the
membership later never hands the account back to the rule. Behind `internal/members` (Postgres and memory
implementations, one service test suite that runs on both).

Custom hostnames and vanity subdomains are in `1190_custom_domains.sql` (range 1190-1219, `internal/domains/README.md`).

Single sign-on is in `1000_sso.sql` (range 1000-1099): `sso_providers` (the providers Supavise registered in
`supavise-gotrue@system`: organization, domains, default role), `sso_users` (who signed in through one, `pending` or `active`),
`1001_sso_denied.sql`'s `sso_denied` (addresses an administrator denied or removed, per provider)
and `signup_grants` (one-time grants that let GoTrue create an invited address). Behind `SSOStore` (`sso_store.go`, a
Postgres and a memory implementation, one conformance suite) and `ClaimStore`.

## Generated types

`gen/` is generated from the pinned specs by `go generate ./internal/api/gen`, which works
offline on the committed `gen/specs/*.json`. Re-pinning the specs is a separate, deliberate
step: `sh internal/api/gen/fetch-specs.sh` (a `make specs` target should call it), review
the diff, then generate.

## `supavise api profile`

Prints the file the Supabase CLI reads with `--profile` (api_url, dashboard_url,
project_host, pooler_host):

```sh
supavise api profile --format yaml > supavise-profile.yaml     # the CLI picks its parser by extension
supabase --profile=./supavise-profile.yaml login --token sbp_...
supabase --profile=./supavise-profile.yaml projects list
```

`project_host` is `api.<domain>`: the CLI derives `https://<ref>.<project_host>` (the
project gateway, matching `<ref>.api.<domain>`) and `db.<ref>.<project_host>`. `pooler_host` is
the registrable domain of `pooler.<domain>` (`example.com` for `pooler.example.com`): the CLI
connects through the pooler URL a linked project records only when the URL's host has exactly
that effective TLD plus one, so a profile that names the host itself leaves `db push` without a
route when `db.<ref>.<project_host>` does not answer.

## Testing

```sh
# unit tests, no processes (the role matrix, permissions, members, invitations, mail, MFA)
scripts/guard.sh -- go test ./internal/api ./internal/members ./cmd/supavise
# with a Postgres (SUPAVISE_TEST_DATABASE_URL) the members tests also run against it, in a throwaway database

# integration: real Postgres + real supavise-pgmeta from the darwin artifacts (~3 s, two processes);
# TestIntegrationRoles proves a Read-only member cannot write through any route
# (SUPAVISE_API_IT_PORT_BASE moves the 900 ports it uses off 32100-32999)
SUPAVISE_API_INTEGRATION=1 \
SUPAVISE_PG_BIN=$HOME/.cache/sbctl/unpacked/postgres-17.11.0.004-r1-darwin-arm64/bin \
SUPAVISE_PGMETA_BIN=$HOME/.cache/sbctl/unpacked/pgmeta-v0.100.0-r0-darwin-arm64/bin/pgmeta \
scripts/guard.sh -- go test ./internal/api -run Integration -v
```

The dashboard's single sign-on end to end on this machine (a real `supavise-gotrue@system` with SAML and the sign-up hook
pointing at an in-process API, a real project, and `testdata/saml-idp.py`, a SAML identity provider for tests that
needs `pip install signxml`; about 25 seconds):

```sh
python3 -m venv /tmp/idp-venv && /tmp/idp-venv/bin/pip install signxml
SUPAVISE_SSO_INTEGRATION=1 SUPAVISE_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked SUPAVISE_SSO_PYTHON=/tmp/idp-venv/bin/python \
  SUPAVISE_API_IT_PORT_BASE=44100 scripts/guard.sh -- go test ./internal/api -run IntegrationDashboardSSO -v
```

Real clients against the integration stack (Postgres on `127.0.0.1:32100-32999`, or from `SUPAVISE_API_IT_PORT_BASE`, nothing on
a default port; delete the JSON file to stop everything):

```sh
internal/api/testdata/serve-stack.sh /path/stack.json &        # writes api_url, pat, jwt, ref
API=$(jq -r .api_url /path/stack.json); PAT=$(jq -r .pat /path/stack.json)

# MCP server over stdio: list_tables, execute_sql, migrations, types, advisors, projects ...
# (the token goes in the environment, never in argv)
SUPABASE_ACCESS_TOKEN=$PAT scripts/guard.sh -- node internal/api/testdata/mcp-smoke.mjs $API abcdefghijklmnopqrst

# The MCP server as a Read-only member and as an Owner (the stack file has a PAT per role):
SUPABASE_ACCESS_TOKEN=$(jq -r .pats.ro /path/stack.json) \
  scripts/guard.sh --no-lock -- node internal/api/testdata/mcp-roles.mjs $API abcdefghijklmnopqrst read-only
SUPABASE_ACCESS_TOKEN=$(jq -r .pats.owner /path/stack.json) \
  scripts/guard.sh --no-lock -- node internal/api/testdata/mcp-roles.mjs $API abcdefghijklmnopqrst owner

# Supabase CLI
cat > profile.yaml <<EOF
name: supavise-test
api_url: $API
dashboard_url: http://127.0.0.1:1
project_host: api.supavise.test
pooler_host: supavise.test
EOF
export SUPABASE_ACCESS_TOKEN=$PAT SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1
supabase --profile=./profile.yaml projects list
supabase --profile=./profile.yaml link --project-ref abcdefghijklmnopqrst
supabase --profile=./profile.yaml gen types typescript --linked
supabase --profile=./profile.yaml functions deploy hello --use-api
supabase --profile=./profile.yaml secrets set FOO=bar

# `supabase login` browser flow, driven with a pty (the CLI side) and a POST (Studio's side)
python3 internal/api/testdata/cli-login.py ./profile.yaml /path/stack.json
```

When a run is over, check that nothing of ours is left: `lsof -iTCP -sTCP:LISTEN -nP | grep -E ':32[1-9][0-9][0-9] '`
must list nothing, and `pgrep -fl 'supabase.*--profile'` must find no CLI started with your profile.
The Supabase CLI may also have started its own stack host (`supabase __supabase_stack_host__`)
for the project directory it ran in; that process belongs to the CLI and to whatever project
you ran it from, so check its working directory (`lsof -p <pid> | grep cwd`) before stopping it.

## Project upgrades

A project runs the Postgres, GoTrue and PostgREST versions it was created or last upgraded with; the
node's pins move with a Supavise release. Studio shows the project's versions in Settings > General,
"Service versions" (`GET /platform/projects/{ref}/service-versions`: `gotrue`, `postgrest`,
`supabase-postgres`, as release tags without the service prefix, for example `v2.195.0-r1`), and
offers "Upgrade project" when the eligibility answer says `eligible`. Studio places this section
on the General page, not on Infrastructure. The section (the alert, the validation errors and the
warnings) renders only while the profile does not list `project_settings:database_upgrades` in
`disabled_features`, so the list leaves it out (`TestStudioCanOfferTheUpgrade`). The upgrade dialog
also reads `GET /platform/projects/{ref}/disk` and prints "Your current disk size of NGB will also be
right-sized" unless `size_gb` equals the plan's included size (8 for an enterprise organization on
gp3), so that call answers 8 GB gp3; a project has no provisioned volume and an upgrade resizes
nothing. Routes and rules (`upgrade.go`, `internal/lifecycle/upgrade.go`):

- `GET .../upgrade/eligibility`: `current_app_version` and `latest_app_version` are
  `supabase-<postgres release tag>` (Studio shows what follows `supabase-postgres-`),
  `target_upgrade_versions` has one entry (`postgres_version` the node's Postgres major version,
  `release_channel` `ga`) when the project can be upgraded and none otherwise, and
  `duration_estimate_hours` is how long the project is offline (the base backup does not count).
  Every array of the response is present, and mostly empty: the hosted ones that describe objects that
  block `pg_upgrade` (`legacy_auth_custom_roles`, `warnings`, and `potential_breaking_changes`, which Studio reads
  and the spec omits) cannot apply to a restart on the same major version. `validation_errors` holds a
  paused project's `project_hibernating` and, when the node's Postgres release differs from the
  project's, one `unsupported_extension` (`extension_name`, which Studio lists with a link to the
  extensions page) per installed extension the new release cannot serve; `unsupported_extensions`
  repeats their names. `eligible` is false
  when the project is on the node's versions, is not `ACTIVE_HEALTHY`, runs another Postgres major version (no
  upgrade path), has such an extension, or the node has no backup service. `duration_estimate_hours` is the real estimate
  in hours, so it is fractional (0.05 when only GoTrue and PostgREST restart, 0.25 with PostgreSQL), and Studio prints it as
  "offline for up to 0.05 hours" where hosted's whole hours read "1 hour".
- `POST .../upgrade` with `target_version` (Postgres's major version as Studio posts it, `"17"`; the app version or the
  release tag also work) and an optional `release_channel` (only `ga`): 201 with `tracking_id`, the
  project `UPGRADING`, the upgrade running in the background on a context detached from the request and
  bounded to 2 hours (a shutdown waits for it like for a delete). 400 for a target that is not the
  node's, another channel, a project that already runs the node's versions or has no upgrade path; 409 for a
  project that is not `ACTIVE_HEALTHY` or is being upgraded; 503 on a node without a backup service.
- `GET .../upgrade/status`: `databaseUpgradeStatus` is null for a project never upgraded, otherwise the newest upgrade:
  `status` 0 upgrading, 1 upgraded, 2 failed (Studio's `DatabaseUpgradeStatus`), `progress` and `error` as hosted names
  them (the mapping is in `internal/lifecycle/README.md`), `initiated_at`, `latest_status_at`, `target_version`. The
  `tracking_id` query parameter is accepted and the newest upgrade is returned whatever it says. A failed upgrade shows
  Studio's failure banner and "back online" screen, which is true once the rollback worked.

Permissions (`authz.go`): eligibility, status and service-versions are reads (Read-only and up, a role scoped to the
project included); the upgrade is `infra:Execute` on `queue_jobs.projects.upgrade`, so Owners and Administrators only,
like hosted (a Developer restarts, `reboot`, but does not upgrade). `POST /platform/projects/{ref}/restart-services`
restarts the whole project whatever services it names.

Differences from hosted that Studio shows as it does there:

- Studio's note on a paused project in "Service versions" says that restoring it updates Postgres to
  the newest version. Resume here keeps the versions recorded; the project is upgraded only when its
  owner asks.
- Studio's "Your project can be upgraded to the latest version of Postgres" alert appears whenever the
  eligibility answer is `eligible`, including when only GoTrue or PostgREST differ from the node's
  pins; the Postgres badge next to it can still read "Latest".

## Lifecycle calls outlive the request

Delete, pause, restore (resume), restart, upgrade and create run on a context detached from the HTTP
request (`context.WithoutCancel`) with a bound: 30 minutes for delete (it includes a final base
backup), 20 for create, 10 for pause and resume, 20 for restart (pause and resume as one unit). A database restore has no deadline (the detach bound
covers only `BeginRestore`); the log warns when one has run 4 hours. A
client that leaves partway (Ctrl-C on `supabase projects delete`, a closed Studio tab, a proxy
idle timeout) therefore cannot strand a project half deleted or stopped; the operation finishes
and its outcome shows in the project's status. Status codes follow the specs: v1 pause, restore
and restart answer 200; platform pause, restart and restart-services answer 201 and platform
restore 200. A request cancelled by the client answers 499 (not 5xx, and not logged as an
error), which is what a browser navigation does to Studio's in-flight pg-meta queries.
The HTTP client toward pg-meta, GoTrue and Storage drops idle keep-alive connections after 2
seconds, below the 5 seconds after which postgres-meta (fastify) closes them, because a SQL
`POST` that hits a closed connection cannot be replayed. `api.Deps.Store` is passed explicitly by
`internal/app` (the Postgres store); the in-memory fallback logs a warning. Concurrent device-login
creates for one `session_id` are serialized, so a duplicate create replaces the first session and
deletes its token instead of orphaning it. The project region shown to clients is always an AWS
region code (`config.Region`, default `us-east-1`).

## Not done / known limits

- **Roles.** Hosted's `no-access` invitation role is refused; the plan limits on Read-only and project-scoped roles
  do not exist here. `mfa_enabled` of a member is always false and the MFA requirement is enforced for dashboard
  sessions only, from the `aal` claim of GoTrue's JWT. A member's access follows the roles at every request, but a
  dashboard session keeps working for its remaining lifetime (up to an hour) for the routes that only need the user
  to exist (profile); everything else re-reads the roles. Studio's pages gate buttons on the permission list it reads
  once every five minutes, so a changed role shows in the UI after a reload while the API refuses at once. Function
  secrets are an Administrator right here (Developers deploy functions but do not set their secrets); hosted's
  documentation does not say. The Developer role cannot read or change anything the table above reserves for settings.

- **No `db push --linked` end to end here.** The CLI dials `db.<ref>.<project_host>:5432` and
  the pooler on 5432; both ports are hard-coded and this development machine may not use
  them. The API side (`cli/login-role`, pooler config) is verified by direct requests
  against a real Postgres and by schema validation; the database leg needs a Linux node.
- The CLI (2.119.0) builds the browser-login link from its built-in profile table, so with a
  custom profile it prints a `supabase.com` link; `login --token` works, and the device
  endpoints work when the link is opened on the right host (`cli-login.py` does that).
- The MCP server derives `get_project_url` from the API host (`*.supabase.red`); the URL it
  returns is wrong for any custom domain. Upstream fix proposed in `docs/research/05`.
- Parameterized queries are wrapped in a CTE so Postgres serializes the rows (trailing
  semicolons and comments are stripped first). Statements that cannot sit in a CTE (DDL,
  INSERT without RETURNING, SHOW, EXPLAIN) run unwrapped and their rows are marshaled from
  the driver's values, which can differ from Postgres's JSON for exotic types.
- `database/query` for `parameters` and for pg-meta SQL return rows as Postgres/pg-meta
  serialize them; `bigint` columns can differ between the two paths (number vs string).
- Advisors return no lints; function bodies are stored and, with `[functions] enabled`, run by the Edge Runtime (`internal/functions`).
- Not checked against a running Studio: that is workstream A. Region and cloud provider
  are reported as the configured AWS region code (`region`, default `us-east-1`) and `AWS`.
- The direct database host reported to clients is `db.<ref>.api.<domain>`, which a
  `*.api.<domain>` wildcard record does not cover (two labels).
- The regular-expression engine of the schema validator in the tests lacks look-ahead, so
  a few responses (auth config, JIT invites) are validated up to their first pattern only.
