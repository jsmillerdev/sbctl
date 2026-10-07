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
  member, `204` where the spec says so), with header `X-Sbctl-Stub: true`.
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
| Projects | `/v1/projects` list, create, get, patch, delete; `pause`, `restore`; `health`; `branches` (empty list, branch lookups 404); `config/database/pooler`. `/platform/projects` list, create, get, patch, delete, `status`, `settings`, `pause`, `restore`, `restart`, `restart-services`; `POST /v1/projects/{ref}/restart`; `config/postgrest`, `config/storage`, `config/pgbouncer`, `config/supavisor`; `api-keys/temporary`; `/v2/projects/{ref}/config`; `/platform/database/{ref}/backups` (empty) |
| Keys | `/v1/projects/{ref}/api-keys`: list, create (publishable and secret keys with names; the secret is shown in full by the create and by `reveal=true`, masked otherwise), get, patch, delete (a revocation), `api-keys/legacy` get and put (`?enabled=`). See Settings and keys below |
| Settings | `GET` and `PATCH /v1/projects/{ref}/config/auth` and `/platform/auth/{ref}/config` (+ `/hooks`), `/v1/projects/{ref}/postgrest` and `/platform/projects/{ref}/config/postgrest`, `config/realtime`, `config/storage` (v1 and platform), `GET` and `PUT /v1/projects/{ref}/config/database/postgres`, `GET /v2/projects/{ref}/config` (the document the CLI diffs), `PATCH /v1/projects/{ref}/database/password` and `/platform/projects/{ref}/db-password` |
| Database | `database/query`, `database/query/read-only` (rows as JSON; `parameters` supported), `database/migrations` list and apply (`supabase_migrations.schema_migrations`), `types/typescript` (pg-meta generator), `cli/login-role` create and delete, `advisors/*` (no lints yet) |
| Functions and secrets | `functions` list, create, deploy (multipart), get, patch, delete, `body`; `secrets` list (digests), create, delete. Sources and sealed secrets are stored; there is no runtime yet (phase 2) |
| Identity | `/v1/profile`, `/platform/profile` (get, post, patch), `profile/permissions` and `permissions/v2` (computed from the caller's roles), `profile/access-tokens` (list, create, get, delete), `/platform/cli/login` and `/platform/cli/login/{session_id}` (device login) |
| Organizations | `/v1/organizations` list, get, `entitlements`, `members`; `/platform/organizations` list, create, get, patch, `entitlements` (every feature of the spec's key enum granted), `billing/subscription` (plan stub), `projects` |
| Members and roles | `/platform/organizations/{slug}/members` (list; `PATCH` and `DELETE .../{gotrue_id}`; `PUT` and `DELETE .../{gotrue_id}/roles/{role_id}`), `members/invitations` (list, create, delete by id, get and accept by token), `members/mfa/enforcement`, `roles`, `members/reached-free-project-limit`; `/platform/projects/{ref}/members`; `/v2/organizations/{slug}/members`, `roles`, `PATCH .../members/{user_id}/roles`, `POST` and `DELETE .../members/invitations`. See Members, roles and permissions below |
| Studio data | `/platform/projects/{ref}/content` (saved SQL snippets, reports; upsert, list, get, count, delete) and `content/folders` |
| pg-meta | every `/platform/pg-meta/{ref}/*` operation of the spec, proxied to sb-pgmeta |
| Auth admin | `/platform/auth/{ref}/users` (list, create, patch, delete), `invite`, `magiclink`, `otp`, `recover`, proxied to the project's GoTrue admin API with its `service_role` key |
| Storage admin | `/platform/storage/{ref}/buckets` (list, create, get, patch, delete, empty) and `objects` (list, list-v2, move, copy, delete, sign, sign-multi), proxied to Storage with `x-forwarded-host: <ref>.api.<domain>`; `objects/public-url` (built here, after checking that the bucket is public); `credentials` (the project's S3 access keys, through Storage's admin API) |

Everything else (billing, integrations, replication, log drains, network restrictions,
Supavisor pool settings, ...) is a stub: it answers a valid empty value and changes nothing.

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
`apikey`, on Functions too, and refuse the exact legacy keys as a bearer; it is refused while no
publishable and secret key exist to fall back on. JWTs signed with the legacy secret stay valid in
`Authorization` until the JWT secret is rotated, as on hosted. Rotating the keys makes a revoked
default key usable again with its new value.

The database password reset (`SetDatabasePassword`) changes the `postgres` role in the cluster
(as a SCRAM verifier), then the sealed secret the API connects with, then makes Supavisor drop the
tenant's pools and cached logins; a failure to store the secret puts the old password back.
Passwords need 8 to 128 characters.

## Authentication

| Route family | Credentials |
|---|---|
| `/platform/*` | GoTrue session JWT from `sb-gotrue@system`: HS256, signed with the system project's JWT secret (`Manager.Keys("system")`; re-read when a signature fails, at most once every 5 seconds, so a rotation takes effect and garbage tokens cost nothing), audience `authenticated`, an `exp` claim, not anonymous, **and an admin** (below). The session is identified by audience and signature, not by the `role` claim: users created through GoTrue's admin API have an empty `auth.users.role`, so their tokens carry `role: ""` (research/08 section 9); tokens with role `anon` or `service_role` are refused |
| `/v1/*`, `/v2/*` | `sbp_` personal access token (`sbp_` + 40 hex, also `sbp_v0_`, `sbp_oauth_`; looked up by `secrets.HashToken`, expiry honored, `last_used_at` touched at most once a minute) **or** a dashboard JWT |
| `GET /platform/cli/login/{session_id}` | none (the CLI has no token yet); guarded by the verification code |

**Admin gate.** A valid session is not enough: the user needs `app_metadata.sbctl_admin = true`
(`api.AdminClaim`; GoTrue lets users edit `user_metadata` but not `app_metadata`) or an email in
`[api] admin_emails`, otherwise the answer is `403`. sbctl sets the claim on every dashboard user
it creates, and `sb-gotrue@system` **must run with `GOTRUE_DISABLE_SIGNUP=true`** so nobody else
can obtain a session at all; the gate is defense in depth if that setting is ever lost.
The email allowlist trusts the JWT's `email` claim: GoTrue's access token carries no claim
that proves the address was confirmed, so with open signup and autoconfirm an unregistered
allowlisted address could be claimed. Keep signup disabled; prefer the `sbctl_admin` claim.
A PAT is not re-checked against the user's admin status on each use: delete a user's tokens
when you remove their access.

Dashboard users are recorded in `sbctl.api_users` on first sight (profile fields come from
GoTrue's `user_metadata`; later edits win). What a user may do is decided by their roles, below.

### Claim and invite (`GET` and `POST /claim`, `claim.go`)

`sb-gotrue@system` has sign-up disabled, so dashboard accounts come only from here. A claim token
(`sbc_` + 48 hex) creates the first administrator, an invite token (`sbi_`) creates one user for a
fixed address. Only the SHA-256 is stored (`sbctl.claim_tokens`, migration `0600`); a token works
once and expires (claim 72 hours, invite 7 days). `POST /claim {token, email, password,
organization_name}` consumes the token with one conditional `UPDATE`, creates the user through
GoTrue's admin API on `sb-gotrue@system` with the system `service_role` key (`email_confirm: true`,
`app_metadata.sbctl_admin = true`), and for a claim token creates the first organization (or keeps
the existing one); the answer is `201 {email, user_id, organization, dashboard_url}`. When the user
cannot be created (the address exists, the password is refused) the token is released and answers
again. Unknown, used and expired tokens all answer `403`; ten failures in a minute answer `429` for
the rest of the minute, node-wide. `GET /claim` serves a self-contained page (one inline script
pinned by hash in the CSP, no third-party requests). Neither route takes credentials: the token is
the credential. The claimed first user becomes Owner of the organization. Redeeming an invite token also accepts the
organization invitations that wait for its address (invitations carry the role, below), and records the account in
`sbctl.api_users`; the claim page fills the token and address from the link's `#token=...&email=...` fragment, which a
browser never sends to a server. `Accounts` (the same type the CLI uses for `sbctl claim token`, `sbctl users invite|list|role|remove`)
also lists dashboard users and removes one together with its memberships and the personal access tokens it created.

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
| Organization settings, delete the organization, project transfer, SSO, MFA requirement | yes | | | |
| Add or remove Owners (also project-scoped Owner roles) | yes | | | |
| Add, change, remove and invite Administrators, Developers and Read-only members | yes | yes | | |
| Billing: read | yes | yes | yes | yes |
| Billing: update; OAuth apps | yes | yes | | |
| Create, rename, pause, restart, restore and delete projects; database password; backups restore | yes | yes | | |
| Project settings (Auth, PostgREST, Realtime, Storage, Postgres); API keys create, update, revoke; function secrets write; any unnamed write | yes | yes | | |
| Read the service_role key, the JWT secret, the S3 credentials; temporary keys | yes | yes | yes | |
| Write SQL, apply migrations, change schema (Studio and pg-meta), Auth users, Storage buckets and objects, deploy and delete functions, preview branches | yes | yes | yes | |
| Read everything else: config, logs, advisors, users, buckets, functions, secrets (digests), `SELECT` SQL, types | yes | yes | yes | yes |
| Saved SQL snippets, reports | yes | yes | yes | yes |

A **project-scoped role** is a base role held on a set of projects. The member sees only those projects (lists
filter, the other projects answer 403) and has the base role's permissions on them; on the organization itself the
member can read it and its members, nothing else. Each project holds one role per member: giving a member another
scoped role on a project takes the project out of the first. A member with no organization-wide role and no project
has no access (the "no-access" state). **An organization always keeps one Owner**: demoting, removing or leaving as
the last Owner answers 400 (`ErrLastOwner`), concurrent changes included (the check runs in a transaction that locks
the organization). Members may always leave. `POST /platform/organizations` needs an Owner role somewhere (or an
empty node); the creator owns the new organization.

### Enforcement

`authz.go` holds the route table: every operation of the three specs, and every extra route sbctl serves, resolves
to an action and a resource of hosted's `PermissionAction` model (`tenant:Sql:Admin:Write` on `migrations`,
`write:Update` on `custom_config_gotrue`, `infra:Execute` on `reboot`, ...), checked against the caller's effective
permissions before the handler runs. A row of the table is `method, path template, need`; `TestRouteTableCoversTheSpecs`
fails for a rule that no route reaches. The defaults are deny: a write no rule names needs `write:Update` on the
project (Owner, Administrator) or on the organization (Owner), and a write outside any organization or project
needs the Owner role somewhere. Routes about the caller's own account (profile, tokens, notifications, telemetry)
and the invitation the caller holds the token of are open to every signed-in user. `TestRoleMatrix` drives about a
hundred representative routes as an Owner, an Administrator, a Developer, a Read-only member, a project-scoped
Developer and a user without a membership through the real handlers.

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
  the `sbctl_read_only` role on every path: `POST /v1/projects/{ref}/database/query` (the MCP server's `execute_sql`),
  Studio's `POST /platform/pg-meta/{ref}/query` and the other pg-meta reads, and `cli/login-role` hands such a caller
  the read-only login role (`sbctl_cli_ro_*`) whatever `read_only` says. `TestIntegrationRoles` runs inserts,
  updates, deletes, DDL, `begin read write`, `set session characteristics` and `reset role` against a real
  Postgres through both routes.
- **Secrets stay with the roles that may read them.** For a caller without `read:Read` on `service_api_keys`,
  `field.jwt_secret` and `storage.s3_credentials` (Read-only), `GET .../settings` omits the JWT secret and the
  service_role key, `config/postgrest` blanks `jwt_secret`, `api-keys?reveal=true` and the temporary key answer 403 and
  the S3 credentials are not listed.
- **Personal access tokens carry their owner's permissions**, read at each request: demote or remove the member
  and their tokens lose the access at once (`users remove` also deletes the tokens). **Dashboard sessions** are
  checked the same way. A token is not an interactive session, so the MFA requirement below does not apply to it.
- **MFA requirement.** `GET` and `PATCH /platform/organizations/{slug}/members/mfa/enforcement` store and report
  it (the answer is 201 on both, as the spec says), and `organization_requires_mfa` in the organization list follows
  it. While it is on, a dashboard session whose JWT does not carry `aal: aal2` is refused in the organization's
  routes (403 `MFA required`), still sees the organization list, profile and permissions, and cannot accept an
  invitation; turning it on needs an aal2 session, so an Owner cannot lock themselves out. `mfa_enabled` of a member
  is always false (GoTrue's factor list is not queried).

### Invitations

`POST .../members/invitations` takes both body shapes of the spec (`emails` + `role_id` + `role_scoped_projects`,
Studio's; or `data[].attributes` with `role` and `projects`) and answers `{succeeded, failed}`; an address that
is already a member lands in `failed`, an invalid address is a 400, and a caller who may not invite the role gets a 403.
sbctl adds `invite_links: [{email, url, emailed}]` to that answer. The token is `sbo_` + 48 hex, stored as its
SHA-256 (`sbctl.org_invitations`), works once and expires after 7 days (hosted: 24 hours; here the link often travels
by hand); inviting an address again replaces the pending invitation. Studio's `/join?token=...&slug=...` page uses
`GET` and `POST .../invitations/{token}`: the signed-in user's email must match (case-insensitively) and joins with the
invited role, or with the invited project-scoped role on the projects that still exist.

How the invitee is told:

- With **`[mail]` configured** (an SMTP relay for `sb-gotrue@system`, rendered into its `GOTRUE_SMTP_*`), sb-gotrue sends
  the message: GoTrue's admin invite for an address without an account (the account is created confirmed and gets
  `sbctl_admin`; the link signs the person in and lands on the invitation), a sign-in link (`/magiclink`) that lands on
  the invitation for an existing account. If the relay fails the invitation stays and the caller gets the link.
- **Without mail**, the answer carries the link and the server logs it at info level: the invitation page for an
  existing account, the claim page (token and address prefilled) for a new address, where the invitee picks a password
  and joins with the invited role in one step. `sbctl users invite <email> --role <role> [--org <slug>] [--project <ref>]...`
  prints that link on stdout.

### Bootstrap, SSO and the CLI

- The claimed first user is Owner. Dashboard accounts that existed before roles keep full access: migration `0900`
  makes every user the API had seen Owner of every organization, and any other account created before the migration
  became Owner of every organization on its first request (the account's creation time comes from `sb-gotrue@system`;
  a failed lookup denies and retries after a minute; each account is looked at once, so removing it from an
  organization sticks). Accounts created afterwards have no access until they are invited, claimed or granted a role.
- `members.Service.GrantSSODefault(ctx, userID, email)` is what the SSO workstream calls for a first-time SSO sign-in:
  the email domain's rule (`sbctl users default-role set <domain> <role> [--org]`, table `sbctl.sso_default_roles`)
  makes the user a member of its organization with its role. A domain without a rule grants nothing; an existing
  membership is never changed.
- `sbctl users invite|list|role|remove|default-role` (see `sbctl users --help`): `role` sets an organization-wide role
  as the operator and also adds a user to an organization, which is how an organization without an Owner gets one
  back; `remove` refuses to delete the only Owner of an organization unless `--force`.

## Configuration (`[api]` in config.toml, or `SBCTL_API_*`)

| Key | Meaning |
|---|---|
| `allowed_origins` | extra CORS origins besides the Studio URL (`*` is not accepted: Studio sends credentials) |
| `pgmeta_crypto_key` | passphrase shared with sb-pgmeta (`CRYPTO_KEY`). Empty: a random key kept sealed in the registry as system secret `pgmeta_crypto_key`, created by `api.EnsurePGMetaCryptoKey(ctx, reg, sec)`. The unit renderer for sb-pgmeta must call that function before it renders the unit; the API calls it on first use and fails (no ephemeral key) when the registry cannot store the key |
| `public_url`, `dashboard_url` | override the derived `https://api.<domain>` and `https://studio.<domain>` |
| `disable_device_login` | turn off the browser login flow |
| `admin_emails` | comma-separated emails allowed to use the API without the `sbctl_admin` claim |

`[mail]` (or `SBCTL_MAIL_*`) is the SMTP relay `sb-gotrue@system` sends the dashboard's mail through, used for
organization invitations: `smtp_host`, `smtp_port` (587), `smtp_user`, `smtp_pass`, `smtp_from`, `smtp_name`. It
needs a host and a sender; without it no mail is sent. The password sits in `config.toml` in plain text, so keep the
file mode 0600. Changing it renders the system GoTrue's environment again; restart `sb-gotrue@system` for it to apply.

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
- **Read-only SQL runs as the role `sbctl_read_only`**: `login`, `bypassrls` (as upstream's
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
  read-write; read-only requests get `sbctl_cli_ro_<rand>` (`bypassrls`, member of `pg_read_all_data`;
  `pg_dump` runs with `row_security = off` and needs it on RLS tables),
  because the CLI runs `SET SESSION ROLE postgres` after connecting as any `cli_login_*`
  user (cli-go `internal/utils/connect.go`) and a read-only role cannot do that. Both expire
  after an hour, are dropped by the next create or by `DELETE .../cli/login-role`, and are
  created with a SCRAM verifier. The CLI's `db dump --role-only` skips only `cli_login_*`, so
  it lists `sbctl_read_only` and any live `sbctl_cli_ro_*` role; a read-only role cannot use
  the `cli_login_` name (see above), so this stays a known limit.
- **sb-pgmeta** at `127.0.0.1:<ports.pgmeta>` with `CRYPTO_KEY` equal to the key above. The
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
- **`sb-gotrue@system`** with `GOTRUE_DISABLE_SIGNUP=true`, and `app_metadata.sbctl_admin = true`
  on each dashboard user sbctl creates (see Authentication).

## State

Migrations `internal/registry/migrations/0100_api.sql` and `0101_api_login_failures.sql`
(range 0100-0199): `api_users`, `api_cli_login_sessions`, `api_functions`,
`api_function_files`, `api_function_secrets`, `api_content`, `api_content_folders`. `Store` (`store.go`) has a Postgres and a memory
implementation behind one conformance suite (`store_test.go`). Members, roles and invitations are in
`0900_members.sql` (range 0900-0999: `org_members`, `org_project_roles` and `org_project_role_refs`, `org_invitations`,
`org_mfa`, `sso_default_roles`, and the legacy-account bookkeeping), behind `internal/members` (Postgres and memory
implementations, one service test suite that runs on both).

## Generated types

`gen/` is generated from the pinned specs by `go generate ./internal/api/gen`, which works
offline on the committed `gen/specs/*.json`. Re-pinning the specs is a separate, deliberate
step: `sh internal/api/gen/fetch-specs.sh` (a `make specs` target should call it), review
the diff, then generate.

## `sbctl api profile`

Prints the file the Supabase CLI reads with `--profile` (api_url, dashboard_url,
project_host, pooler_host):

```sh
sbctl api profile --format yaml > sbctl-profile.yaml     # the CLI picks its parser by extension
supabase --profile=./sbctl-profile.yaml login --token sbp_...
supabase --profile=./sbctl-profile.yaml projects list
```

`project_host` is `api.<domain>`: the CLI derives `https://<ref>.<project_host>` (the
project gateway, matching `<ref>.api.<domain>`) and `db.<ref>.<project_host>`.

## Testing

```sh
# unit tests, no processes (the role matrix, permissions, members, invitations, mail, MFA)
scripts/guard.sh -- go test ./internal/api ./internal/members ./cmd/sbctl
# with a Postgres (SBCTL_TEST_DATABASE_URL) the members tests also run against it, in a throwaway database

# integration: real Postgres + real sb-pgmeta from the darwin artifacts (~3 s, two processes);
# TestIntegrationRoles proves a Read-only member cannot write through any route
# (SBCTL_API_IT_PORT_BASE moves the 900 ports it uses off 32100-32999)
SBCTL_API_INTEGRATION=1 \
SBCTL_PG_BIN=$HOME/.cache/sbctl/unpacked/postgres-17.11.0.004-r1-darwin-arm64/bin \
SBCTL_PGMETA_BIN=$HOME/.cache/sbctl/unpacked/pgmeta-v0.100.0-r0-darwin-arm64/bin/pgmeta \
scripts/guard.sh -- go test ./internal/api -run Integration -v
```

Real clients against the integration stack (Postgres on `127.0.0.1:32100-32999`, or from `SBCTL_API_IT_PORT_BASE`, nothing on
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
name: sbctl-test
api_url: $API
dashboard_url: http://127.0.0.1:1
project_host: api.sbctl.test
pooler_host: pooler.sbctl.test
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

## Lifecycle calls outlive the request

Delete, pause, restore (resume), restart and create run on a context detached from the HTTP
request (`context.WithoutCancel`) with a bound: 30 minutes for delete (it includes a final base
backup), 20 for create, 10 for pause and resume, 20 for restart (pause and resume as one unit). A
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
  returns is wrong for any custom domain. Upstream fix proposed in `research/05`.
- Parameterized queries are wrapped in a CTE so Postgres serializes the rows (trailing
  semicolons and comments are stripped first). Statements that cannot sit in a CTE (DDL,
  INSERT without RETURNING, SHOW, EXPLAIN) run unwrapped and their rows are marshaled from
  the driver's values, which can differ from Postgres's JSON for exotic types.
- `database/query` for `parameters` and for pg-meta SQL return rows as Postgres/pg-meta
  serialize them; `bigint` columns can differ between the two paths (number vs string).
- Advisors return no lints; function bodies are stored but not run; branches are always empty.
- Not checked against a running Studio: that is workstream A. Region and cloud provider
  are reported as the configured AWS region code (`region`, default `us-east-1`) and `AWS`.
- The direct database host reported to clients is `db.<ref>.api.<domain>`, which a
  `*.api.<domain>` wildcard record does not cover (two labels).
- The regular-expression engine of the schema validator in the tests lacks look-ahead, so
  a few responses (auth config, JIT invites) are validated up to their first pattern only.
