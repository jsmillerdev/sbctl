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

- 115 operations have a handler here (the table below).
- The other 500 answer with a **stub derived from the spec**: the smallest instance of the
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
| Projects | `/v1/projects` list, create, get, patch, delete; `pause`, `restore`; `health`; `branches` (the full branch API, served by `internal/branching`, see below); `config/database/pooler`. `/platform/projects` list, create, get, patch, delete, `status`, `settings`, `pause`, `restore`, `restart`, `restart-services`; `POST /v1/projects/{ref}/restart`; `config/postgrest`, `config/storage`, `config/pgbouncer`, `config/supavisor`; `api-keys/temporary`; `/v2/projects/{ref}/config`; `/platform/database/{ref}/backups` (empty) |
| Keys | `/v1/projects/{ref}/api-keys` (legacy `anon`, `service_role`, `sb_publishable_*`, `sb_secret_*`; secrets masked unless `reveal=true`), `api-keys/{id}`, `api-keys/legacy` |
| Database | `database/query`, `database/query/read-only` (rows as JSON; `parameters` supported), `database/migrations` list and apply (`supabase_migrations.schema_migrations`), `types/typescript` (pg-meta generator), `cli/login-role` create and delete, `advisors/*` (no lints yet) |
| Functions and secrets | `functions` list, create, deploy (multipart), get, patch, delete, `body`; `secrets` list (digests), create, delete. Sources and sealed secrets are stored; there is no runtime yet (phase 2) |
| Identity | `/v1/profile`, `/platform/profile` (get, post, patch), `profile/permissions` (owner on every organization), `profile/access-tokens` (list, create, get, delete), `/platform/cli/login` and `/platform/cli/login/{session_id}` (device login) |
| Organizations | `/v1/organizations` list, get, `entitlements`, `members`; `/platform/organizations` list, create, get, patch, `entitlements` (every feature of the spec's key enum granted), `billing/subscription` (plan stub), `members`, `projects` |
| Studio data | `/platform/projects/{ref}/content` (saved SQL snippets, reports; upsert, list, get, count, delete) and `content/folders` |
| pg-meta | every `/platform/pg-meta/{ref}/*` operation of the spec, proxied to sb-pgmeta |
| Auth admin | `/platform/auth/{ref}/users` (list, create, patch, delete), `invite`, `magiclink`, `otp`, `recover`, proxied to the project's GoTrue admin API with its `service_role` key |
| Storage admin | `/platform/storage/{ref}/buckets` (list, create, get, patch, delete, empty) and `objects` (list, move, copy, delete, sign), proxied to Storage with `x-forwarded-host: <ref>.api.<domain>` |

Everything else (billing, integrations, replication, log drains, network restrictions,
auth/storage/realtime *config* PATCHes, ...) is a stub: it answers a valid empty value and
changes nothing.

## Branches

`Deps.Branching` (a `*branching.Service`, `internal/branching/README.md`) serves `GET/POST/DELETE
/v1/projects/{ref}/branches`, `GET /v1/projects/{ref}/branches/{name}`, `GET/PATCH/DELETE
/v1/branches/{id_or_ref}` (`force=false` schedules the deletion), `POST .../merge|reset|push|restore`
and `GET .../diff`, with the exact spec shapes (`BranchResponse`, `BranchDetailResponse`, ...). Studio calls
these paths itself; the `/platform` twins are the project fields `is_branch_enabled`,
`preview_branch_refs` and `parent_project_ref`. Branches are not listed as projects. Merge, reset and
push answer `201 {workflow_run_id, message: "ok"}` at once and run in the background; the branch's
`status` follows. `?force=true` on merge and push is our extension. `GET /v1/branches/{id}` omits `db_pass` and `jwt_secret` for the default branch (the project's own
secrets stay in the secret store) and answers without them while a new branch's credentials are not
stored yet. Create refuses what the node cannot honor with 400: non-empty `secrets`, a `release_channel`
other than `ga`, a `postgres_engine` other than the parent's; `region` is accepted and the parent's
is used. PATCH refuses a `status` that differs from the branch's own and accepts the deprecated
`reset_on_push` (the spec says it is ignored). Without `Deps.Branching` the list is
the default branch only and a create answers 400. Organization entitlements already grant
`branching_limit` and `branching_persistent`, which is what the CLI checks on a failed create.

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
GoTrue's `user_metadata`; later edits win). Every authenticated user can see and change
every project and organization: members and roles are a later phase, which is why
`/platform/profile/permissions` grants `%` on `%` and every PAT carries full access.

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

## Configuration (`[api]` in config.toml, or `SBCTL_API_*`)

| Key | Meaning |
|---|---|
| `allowed_origins` | extra CORS origins besides the Studio URL (`*` is not accepted: Studio sends credentials) |
| `pgmeta_crypto_key` | passphrase shared with sb-pgmeta (`CRYPTO_KEY`). Empty: a random key kept sealed in the registry as system secret `pgmeta_crypto_key`, created by `api.EnsurePGMetaCryptoKey(ctx, reg, sec)`. The unit renderer for sb-pgmeta must call that function before it renders the unit; the API calls it on first use and fails (no ephemeral key) when the registry cannot store the key |
| `public_url`, `dashboard_url` | override the derived `https://api.<domain>` and `https://studio.<domain>` |
| `disable_device_login` | turn off the browser login flow |
| `admin_emails` | comma-separated emails allowed to use the API without the `sbctl_admin` claim |

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
implementation behind one conformance suite (`store_test.go`).

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
# unit tests, no processes
scripts/guard.sh -- go test ./internal/api ./cmd/sbctl

# integration: real Postgres + real sb-pgmeta from the darwin artifacts (~3 s, two processes)
SBCTL_API_INTEGRATION=1 \
SBCTL_PG_BIN=$HOME/.cache/sbctl/unpacked/postgres-17.11.0.004-r1-darwin-arm64/bin \
SBCTL_PGMETA_BIN=$HOME/.cache/sbctl/unpacked/pgmeta-v0.100.0-r0-darwin-arm64/bin/pgmeta \
scripts/guard.sh -- go test ./internal/api -run Integration -v
```

Real clients against the integration stack (Postgres on `127.0.0.1:32100-32999`, nothing on
a default port; delete the JSON file to stop everything):

```sh
internal/api/testdata/serve-stack.sh /path/stack.json &        # writes api_url, pat, jwt, ref
API=$(jq -r .api_url /path/stack.json); PAT=$(jq -r .pat /path/stack.json)

# MCP server over stdio: list_tables, execute_sql, migrations, types, advisors, projects ...
# (the token goes in the environment, never in argv)
SUPABASE_ACCESS_TOKEN=$PAT scripts/guard.sh -- node internal/api/testdata/mcp-smoke.mjs $API abcdefghijklmnopqrst

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
- Advisors return no lints; function bodies are stored but not run;
  `PATCH` on auth, storage, realtime and postgrest *config* is a stub; `PATCH
  /v1/projects/{ref}/database/password` is a stub.
- Not checked against a running Studio: that is workstream A. Region and cloud provider
  are reported as the configured AWS region code (`region`, default `us-east-1`) and `AWS`.
- The direct database host reported to clients is `db.<ref>.api.<domain>`, which a
  `*.api.<domain>` wildcard record does not cover (two labels).
- The regular-expression engine of the schema validator in the tests lacks look-ahead, so
  a few responses (auth config, JIT invites) are validated up to their first pattern only.
