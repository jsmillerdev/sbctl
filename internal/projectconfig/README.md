# internal/projectconfig

Per-project service settings: what Studio, the Supabase CLI and the Management API save with
`PATCH /v1/projects/{ref}/config/auth`, `.../postgrest`, `.../config/storage`, `.../config/realtime`,
`PUT .../config/database/postgres`, `PATCH .../config/database/pooler` and their `/platform` twins. This package stores them,
validates them against the Management API specs and the services' own rules, and renders them as
the environment of GoTrue and PostgREST, the server arguments of PostgreSQL and the tenant
settings of Storage and Realtime. The routes are in `internal/api` (`config_*.go`), applying a
change to what runs is `internal/lifecycle` (`ApplyConfig`).

```go
m := projectconfig.NewManager(projectconfig.NewPGStore(pool), sealer, projectconfig.Options{
    TemplateBaseURL: lifecycle.TemplateBaseURL(cfg), // where GoTrue fetches mail templates
    Event: func(ctx context.Context, ref, kind string, payload any) { ... },
})
ch, err := m.Patch(ctx, ref, projectconfig.Auth, map[string]any{"site_url": "https://app.example.com"},
    projectconfig.CrossContext{MemoryLimit: limit})   // *ValidationError on bad input
st, _ := m.Get(ctx, ref, projectconfig.Auth)           // st.Effective: defaults under the saved values
env, _ := m.AuthEnv(ctx, ref, externalURL)             // GoTrue environment for the saved settings
```

## What is stored

Migration `0800_project_settings.sql`: one row per project and service (`auth`, `postgrest`,
`realtime`, `storage`, `postgres`; `pooler` from migration `0801`) in `sbctl.project_settings`, deleted with the project. It holds
only what was changed: `values` (plain), `sealed` (secrets, base64 of `secrets.Seal`: OAuth client
secrets, the SMTP password, SMS provider tokens, hook secrets, the captcha secret). A setting with
no entry has its default, which is what the units render when no row exists, so a project that
never saved anything runs exactly as before this package. `version` counts saves; a write applies
only if the version is the one it read (`Store.Put`), and `Manager.Patch` retries a lost race on
top of the newer row. Each save appends the registry event `settings.<service>.updated` with the
version and the names of the changed settings, never values.

## Schemas

`Schema` lists each service's settings as `Field`s (type, range, enum, pattern, secret flag,
default, environment variable). Names are the ones of the specs: `UpdateAuthConfigBody` of the v1
spec (the platform twin uses them in upper case), `V1UpdatePostgrestConfigBody`,
`UpdateRealtimeConfigBody`, `UpdateStorageConfigBody`, `UpdatePostgresConfigBody`. The api package
test `TestAuthSchemaCoversTheSpecs` fails when a re-pinned spec lists an auth setting the schema
does not know.

Patch semantics: a key the schema does not know is ignored (the specs grow); `null` returns a
setting to its default; a secret sent back as the redaction a GET handed out (below) changes
nothing, because clients that edit a form save every field they were shown; a partial `features`
object of Storage is merged into the saved one. The result is validated as a whole before
anything is written (`Schema.Cross`).

### Auth

Every setting has the GoTrue variable it renders to (`GOTRUE_<NAME>` except where GoTrue's struct
differs: `refresh_token_rotation_enabled` is `GOTRUE_SECURITY_REFRESH_TOKEN_ROTATION_ENABLED`,
`mailer_templates_*_content` is `GOTRUE_MAILER_TEMPLATES_*`, the seconds-valued ones are Go
durations, `sessions_timebox` is hours, `sms_test_otp` uses `:` where the API uses `=`).
`testdata/gotrue-env-names.txt` lists every variable GoTrue v2.195.0 reads, generated from the
structs of `internal/conf` the way `envconfig` names them (`testdata/genenv`), and
`TestAuthEnvNamesExist` pins every name the schema renders to that list. Regenerate it when
`versions.yaml` bumps auth:

```sh
go build -o /tmp/genenv internal/projectconfig/testdata/genenv/main.go
/tmp/genenv configuration.go saml.go rate.go > internal/projectconfig/testdata/gotrue-env-names.txt  # from supabase/auth internal/conf at the pinned tag
```

Validation mirrors what GoTrue refuses at start, because a bad value would turn into a crash
loop: the redirect allow list is compiled with the same glob library and separators GoTrue uses;
hook URIs must be `pg-functions://postgres/<schema>/<function>`, HTTPS, or HTTP on loopback; hook
secrets must look like `v1,whsec_...`; captcha needs a provider and a secret; WebAuthn and
passkeys need an RP id, name and origins; `mailer_autoconfirm` and
`mailer_allow_unverified_email_sign_ins` cannot both be on. Switching SMTP off (`smtp_host` set to
`""`) clears every `smtp_*` setting, as hosted does. Until SMTP is set, `mailer_autoconfirm` is on
(nobody could confirm an address otherwise, the default `lifecycle` renders); once `smtp_host` is
set it turns off unless saved explicitly.

**Email templates.** GoTrue loads a template from a URL. The body is saved as
`mailer_templates_<name>_content`; the unit gets
`http://<admin listener>/internal/templates/<ref>/<name>?v=<version>&t=<token>`, served by the
daemon (`internal/api/templates.go`) to loopback clients that did not come through the edge
proxy. The version in the URL keeps GoTrue's template cache from serving an old body. The token
is an HMAC of the project and template under a key derived from the node's master key
(`Manager.TemplateToken`, `secrets.AESGCM.Derive`), so a process of the node that merely reaches
the loopback port (user code in an Edge Function, another project's service) cannot read a
project's templates; a request without the token is answered `404`. Secrets that cannot derive a
key (test doubles) leave the route open.

**Line breaks.** A setting that renders to an environment variable (everything with an `Env`
except the template bodies) refuses `\r` and `\n`, because a unit's environment file cannot carry
them. The save is a 400 instead of a resume that fails. `Manager.Patch` also renders the service's
environment before it saves and refuses any value with a line break or NUL, which covers a secret
that was stored before the check existed. A multi-line SMS template therefore cannot be saved;
GoTrue reads `GOTRUE_SMS_TEMPLATE` from the environment only.

**External providers.** Every provider of the Management API that GoTrue supports (`apple` to
`zoom`, including Azure, GitLab, Keycloak and WorkOS with their `url`, Google and Apple with
`additional_client_ids` joined into GoTrue's client id list). An enabled provider also gets its
`GOTRUE_EXTERNAL_<P>_REDIRECT_URI` set to `<API_EXTERNAL_URL>/callback`, which is GoTrue's default
made explicit; the callback cannot be changed (hosted shows it read-only, too).

**Stored, not rendered.** `saml_*` (workstream L owns the signing key GoTrue needs before SAML can
be on), `nimbus_*` (hosted-only), and the response-only settings of the specs
(`custom_oauth_max_providers`, `saml_allow_encrypted_assertions`, Studio's `MFA_ALLOW_LOW_AAL`,
`AUDIT_LOG_DISABLE_POSTGRES`, ...). The GET answers them with defaults.

### Secrets in responses

`Redact(secret)` is the hex SHA-256 of the value. Both the public API and Studio's twin answer a
secret setting with it (null when unset): the same thing hosted's v1 GET gives the CLI, which
compares it with a hash of its local value (`hash:` in `config.toml`). Plaintext never leaves the
registry; Studio shows the hash in a secret field and a save that leaves it untouched keeps the
stored secret.

### PostgREST

`db_schema` (default `public,graphql_public`), `db_extra_search_path` (`public,extensions`),
`max_rows`, `db_pool` and `db_pool_acquisition_timeout` render to `PGRST_DB_SCHEMAS`,
`PGRST_DB_EXTRA_SEARCH_PATH`, `PGRST_DB_MAX_ROWS`, `PGRST_DB_POOL`,
`PGRST_DB_POOL_ACQUISITION_TIMEOUT`; lists are trimmed and joined without spaces, a pool of 0 means
the default. The auth setting `jwt_exp` also sets `PGRST_APP_SETTINGS_JWT_EXP`. An empty
`db_schema` is rejected: PostgREST falls back to `public` for it, so "Data API off" (the CLI's
`[api] enabled = false`) would not be what runs.

### Realtime and Storage

Applied to the shared service's tenant of the project (the fingerprint of the tenant call includes
them, so only a change sends an update). Realtime: `max_concurrent_users`, `max_events_per_second`,
`max_bytes_per_second`, `max_channels_per_client`, `max_joins_per_second`,
`max_presence_events_per_second`, `max_payload_size_in_kb`, `private_only`, `suspend` and
`presence_enabled` are fields of the tenant; `connection_pool` is the extension setting `db_pool`
(the pool Realtime's `realtime_connect` connection uses), `postgres_changes_pool` is translated by
its controller to `subcriber_pool_size`. Only changed settings are sent; the defaults reported
are the server's own `TENANT_MAX_*`. The tenant API applies only the fields it receives, so a
`null` (return to default) stores the default as an explicit value, and the tenant is sent it
(`Schema.ResetStoresDefault`); without that Realtime would keep the old value while GET said
default. Storage: `fileSizeLimit` and `features`
(`imageTransformation`, `s3Protocol`, `purgeCache`, `icebergCatalog`, `vectorBuckets`) of the
tenant API; Iceberg and vector buckets are saved and reported (the CLI's default `config.toml`
pushes them) but not sent, because the fleet does not run the services behind them, and
`capabilities.iceberg_catalog` says so.

### Pooler

`default_pool_size` (1 to 4950, default 15) and `max_client_conn` (1 to 54000, default 1000) are
the pool size and the client limit of the project's Supavisor tenant: `internal/lifecycle` puts
them in `fleet.TenantSpec` (`PoolSize`, `MaxClients`) and `EnsureTenant` sends them as
`default_pool_size` (also as the pool size of the manager user, which is the one Supavisor takes
the login pools' size from) and `default_max_clients`. The defaults are what the tenant runs with
nothing saved: Supavisor's own, so a project that never saved anything runs as before. A save is
applied live (the tenant call ends the tenant's pooled connections; clients reconnect), a save
while the project is paused when it resumes. A pool size of 0, which the specs allow, would leave
the tenant without a database connection, so it is refused. `pool_mode` and the PgBouncer-only
fields of the platform spec are not settings: the API refuses a value the shared pooler cannot
honor (`internal/api/config_pooler.go`).

### Postgres

The settings hosted lets a project change. Sizes and durations need a unit (a bare number means
a different unit per setting) and are checked against the range Postgres documents for the
setting, since an out-of-range value keeps the postmaster from starting at the next restart. The
whole is checked against the project's memory limit (`shared_buffers` at most 40 percent,
`work_mem` and `maintenance_work_mem` 25) and against what the project's own services need
(`max_connections` at least 20, `max_wal_senders` at least 3, `max_replication_slots` at least 2).
Rendering: settings that overlap the class's command-line sizing (`shared_buffers`,
`effective_cache_size`, `maintenance_work_mem`, `max_wal_size`, `max_connections`,
`max_wal_senders`, `max_replication_slots`) become server arguments after the class's and take
effect at the next restart; every other one is applied with `ALTER SYSTEM` and a reload, so it
survives restarts in `postgresql.auto.conf`. The counts that size shared memory at start are
capped far below what Postgres accepts in `ALTER SYSTEM` (`max_locks_per_transaction` 1024,
`max_worker_processes` and `max_wal_senders` 256, `max_logical_replication_workers` 64, ...),
because Postgres cannot start with an oversized one ("out of memory" while creating shared
memory) and would leave the project down. What is left is estimated: the lock table
(`max_locks_per_transaction` times the backends, about 300 bytes an entry) must stay under 10
percent of the project's memory limit and the shared memory as a whole (with `shared_buffers`
and a slot per backend) under 60 percent; with no limit known only a modest table (64 MB, 600
backends) is accepted. If an apply still fails, the API restores the previous settings and the
lifecycle brings the cluster back (`ApplyOptions.Recover`, see internal/lifecycle). `PUT` with `restart_database: true` restarts the
cluster when something needs it; without it the values are saved, rendered, and flagged
"pending restart" in the log until the next restart (a pause and resume, for example).

## Limits

- Settings written through the Management API are the only ones rendered: a unit's other
  environment (ports, database URLs, keys, mailer URL paths) is lifecycle's and cannot be
  overridden here.
- A restored or cloned project starts with the default settings; they are not copied from the
  source.
- Reads of Postgres settings query the running cluster with the `postgres` role; a paused
  project reports what was saved.
