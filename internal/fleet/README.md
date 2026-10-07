# internal/fleet

The shared services that serve every project, and the tenant calls that register a project with them.

| Service | Unit | Role | Tenant call |
|---|---|---|---|
| Supavisor (`pooler` artifact) | `sb-supavisor` | one Postgres endpoint for all projects: `postgres.<ref>` on the session and transaction ports | `PUT /api/tenants/<ref>` |
| Realtime | `sb-realtime` | websockets; tenant from the first label of `Host` (`<ref>.realtime.internal`) | `POST /api/tenants` |
| Storage | `sb-storage` | `MULTI_TENANT=true`; tenant from `x-forwarded-host` (`<ref>.api.<domain>`) | `PUT /tenants/<ref>` on the admin port |
| postgres-meta | `sb-pgmeta` | stateless; the connection string arrives encrypted with each request | none |
| Studio | `sb-studio` | the dashboard (our platform-mode build) | none |
| Edge Runtime (`edge-runtime` artifact), only with `[functions] enabled` | `sb-edge-runtime` | Edge Functions for every project through sbctl's own main service (`functions-main/`) | none: the main service picks the project from the proxy's `X-Sbctl-Project-Ref` header and reads `system/edge-runtime/tenants/<ref>/` (written by `internal/functions`; the unit sees nothing of `projects/`) |

Supavisor, Realtime and Storage keep their metadata in the system cluster (`_supavisor`, `_realtime`, `_storage`), each as its own role (`lifecycle.FleetRoles`), with passwords that `lifecycle.InitSystem` seals in the registry.

## Using it

```go
n, _ := lifecycle.Open(ctx, cfg, oo)                       // or InitSystem
fl, err := fleet.Setup(ctx, fleet.Deps{
    Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets,
    Supervisor: n.Supervisor, Artifacts: n.Artifacts,
    Start: true, // render and start the services first; Skip: []string{"studio"} to leave one out
})
if err != nil { log.Error("fleet", err) } // fl is still usable unless it is nil
eng := lifecycle.NewEngine(cfg, n.Registry, n.Secrets, n.Artifacts, n.Plane, lifecycle.Options{Fleet: fl})
```

`Setup` returns the tenant Fleet even when a service did not start: the error names the services that failed, and the tenants of the others work. The Fleet is nil only when Setup could not build it (missing Deps, system project not initialized). A caller must not boot with an empty Fleet because one service failed, since the Engine skips every tenant call when the Fleet is empty. Studio is optional: when it cannot start (no artifact, a unit that fails) `Start` logs a warning, returns no error for it, and `Status` reports it; its wait is capped at one minute.

`lifecycle.Open` builds its Engine before anyone can hand it a Fleet, so the caller either builds the Engine again (above) or passes a `Lazy` to `Open`, which is what the CLI does (`openNode`), so `sbctl projects create|rotate-keys|delete` register, re-key and remove tenants too:

```go
lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log})
oo.Fleet = lz.Fleet()
n, _ := lifecycle.Open(ctx, cfg, oo)
lz.Bind(n.Registry, n.Secrets)
```

A `Lazy` tenant loads its credentials on first use and skips a service whose unit this node never rendered, so a node that does not run the fleet still creates projects. `Setup` generates the services' secrets on first use, so the system project must exist. `NewManager(deps)` gives `Start`, `Stop`, `Status` and `Specs` without the tenants; `Stop` and `Status` need only `Cfg` and `Supervisor`. `sbctl serve` is the caller that matters in production: it runs `Setup` with `Start` at every boot (`internal/app`), and the Engine's tenants are a `Lazy` over the same `Setup` (`cmd/sbctl/serve_fleet.go`); `tests/linux/fleet-smoke.sh` runs that path on a real systemd node (daemon starts the services, a project created through the Management API reaches Supavisor, Realtime and Storage). `Deps.ProbeClient` (default 4 s) is for the health checks and `Deps.TenantClient` (default 2 minutes) for the tenant calls: creating a Realtime or Storage tenant runs migrations before it answers. `LoadTenantSpec` and `TenantSpecFor` build the `TenantSpec` of a project from the registry.

The Engine calls `Fleet.EnsureTenant` after a project's units are healthy, again after key rotation, and `RemoveTenant` on delete and on a failed create. A Fleet whose services are down makes create fail (INIT_FAILED) after the bounded retries; a node that does not run the fleet passes an empty `Fleet`.

CLI (`cmd/sbctl/cmd_fleet.go`):

```
sbctl fleet start [--no-fetch] [--skip studio]   # fetch artifacts, render, start in order, wait for each
sbctl fleet stop
sbctl fleet status
sbctl fleet ensure-tenant <ref>... | --all       # repeat the tenant calls by hand
sbctl fleet remove-tenant <ref>...
```

Start order: pgmeta, supavisor, realtime, storage, edge-runtime (only when `[functions] enabled`, see `ServicesFor`), studio; stop is the reverse. A service that does not start does not stop the others, and `start` then exits non-zero. Studio is optional: `fleet start` skips it with a note when no artifact is installed and `[studio] artifact_url` is unset, `fleet status` (and `fleet.AllHealthy`) does not count a Studio whose unit was never rendered, and a Studio that was rendered but does not run makes `fleet start` exit non-zero after the table without failing the daemon's `Setup`. Readiness is a request to each service's health path (2xx): Supavisor `/api/health` (answers 204), Realtime `/healthcheck`, Storage `/status`, pgmeta `/health`, Studio `/api/get-utc-time`.

## Environment per service

Verified against the upstream source at the pinned versions (`config/runtime.exs` of Supavisor v2.9.13 and Realtime v2.140.10, `src/config.ts` and `src/http/routes/admin/tenants.ts` of Storage v1.79.36, the dockerless CLI's `packages/stack/src/services/*.ts`, the upstream compose file), and by running the real artifacts.

- **Supavisor**: `DATABASE_URL` (`ecto://sbctl_supavisor@…/_supavisor`), `PORT` = `[fleet] supavisor_api_port` (4001), `PROXY_PORT_SESSION`/`PROXY_PORT_TRANSACTION` = `[ports]`, `API_JWT_SECRET`, `METRICS_JWT_SECRET` (random, never shown, so the metrics endpoint is unreachable), `SECRET_KEY_BASE`, `VAULT_ENC_KEY` (32 bytes), `PROXY_PORT=0`, `SESSION_PROXY_PORTS=0`, `TRANSACTION_PROXY_PORTS=0` (the internal shard listeners take ephemeral ports, as the CLI does), `GLOBAL_DOWNSTREAM_CERT_PATH` and `GLOBAL_DOWNSTREAM_KEY_PATH` (pooler TLS, below), `bin/prepare` before start (migrations in `_supavisor`). No `CLUSTER_POSTGRES`: one node.
- **Realtime**: `DB_*` for `_realtime` as `sbctl_realtime` with `DB_AFTER_CONNECT_QUERY=SET search_path TO _realtime`, `API_JWT_SECRET`, `METRICS_JWT_SECRET`, `SECRET_KEY_BASE`, `DB_ENC_KEY` (16) and `DB_ENC_KEY_GCM` (32) with `DB_ENC_WRITE_GCM=true`, `APP_NAME=sbctl`, `PORT`, `PHX_HTTP_IP=127.0.0.1` (a slim-services patch; upstream binds every interface), gen_rpc on loopback at `Ports.Realtime + 1369` (5369 by default), `RUN_JANITOR=true`. No `SEED_SELF_HOST`: tenants come through the API. `bin/prepare` before start (migrations).
- **Storage**: `MULTI_TENANT=true`, `DATABASE_MULTITENANT_URL` (`_storage` as `sbctl_storage`), `SERVER_HOST=127.0.0.1`, `SERVER_PORT`, `SERVER_ADMIN_PORT` = `Ports.StorageAdmin`, `SERVER_ADMIN_API_KEYS` (random), `AUTH_ENCRYPTION_KEY`, `REQUEST_X_FORWARDED_HOST_REGEXP=^([a-z]{20})\.api\.<domain>$`, `REQUEST_ALLOW_X_FORWARDED_PATH=true` (the proxy sends `X-Forwarded-Prefix`), `STORAGE_BACKEND=file` with `STORAGE_FILE_BACKEND_PATH=<state>/system/storage/objects` (objects end up under `stub/<ref>/<bucket>/`), or `s3` from `[fleet] storage_s3_*`. Under systemd the s3 backend needs `storage_s3_access_key_id` and `storage_s3_secret_access_key`: `sb-storage` denies the instance metadata service, so the EC2 instance role is not available to it, and `Setup` refuses the unit without keys (use a key scoped to the objects bucket). The server runs its multi-tenant migrations itself at start, so there is no `bin/prepare` (that one migrates a single tenant database).
- **pgmeta**: `PG_META_HOST=127.0.0.1`, `PG_META_PORT`, `PG_META_ADMIN_PORT=Ports.PGMeta+1`, `CRYPTO_KEY` = `[api] pgmeta_crypto_key`, or the sealed system secret `pgmeta_crypto_key` (the one `api.EnsurePGMetaCryptoKey` creates; a test pins the name and the value).
- **Edge Runtime** (`edgeruntime.go`): `bin/edge-runtime start --ip 127.0.0.1 --port <ports.edge_runtime> --main-service <state>/system/edge-runtime/main --policy per_worker --user-worker-request-idle-timeout <ms> --graceful-exit-timeout 10 --max-parallelism n`, with `EDGE_RUNTIME_PORT`, `SBCTL_FUNCTIONS_ROOT` (`<state>/system/edge-runtime/tenants`), the `SBCTL_FUNCTIONS_*` limits from `[functions]`, `SBCTL_FUNCTIONS_PROXY_TOKEN` (the node's proxy secret from `<state>/system/edge-runtime.token`, made on first use by `config.LoadFunctionsProxyToken`; the main service refuses requests without it), `DENO_DIR=<state>/system/edge-runtime/deno` (one module cache for the process: the runtime reads it once) and `HOME` in the environment, no secret of any kind. The main service is the TypeScript of `functions-main/`, embedded in the binary and written by `EnsureMainService` before the unit starts (its hash is in the unit's environment, so a new binary restarts the runtime). Both paths are passed as real paths: the runtime fails with "Module not found" for a main service reached through a symlink (macOS `/tmp`). Readiness is `GET /_internal/health`, answered by the main service. `--max-parallelism` is `[functions] max_parallelism` (default 1): in edge-runtime v1.77.4 it is a semaphore per worker pool key, i.e. workers per function, not a limit on the runtime. The cap on all live workers is the main service's (`SBCTL_FUNCTIONS_MAX_WORKERS` = `[functions] max_workers`, default 16; `SBCTL_FUNCTIONS_MAX_WORKERS_PER_PROJECT`, `SBCTL_FUNCTIONS_MAX_PER_PROJECT`, see `functions-main/README.md`), and the unit's `MemoryMax` is derived from it: `max_workers x max_parallelism x (memory_mb + 32 MB) + 256 MB` (4864M by default) unless `[functions] memory_max` sets it (config refuses a value that cannot hold `max_workers` workers; with `max_workers` unset it decides how many fit). **Turning `[functions] enabled` off** takes the runtime off the node: `fleet start` removes the rendered unit (stopping it) and deletes the tenants tree, which holds every project's JWT secret, service key, database password and function secrets, and `fleet stop` and `fleet status` still show the unit while it is rendered (status: optional, `UNHEALTHY` if it still runs).
- **Studio**: `HOSTNAME`, `PORT`, `NEXT_PUBLIC_API_URL` (`<api url>/platform`), `NEXT_PUBLIC_GOTRUE_URL` (`<api url>/auth/v1`, the system GoTrue through the proxy), `NEXT_PUBLIC_SITE_URL`, `CSP_EXTRA_PROJECT_HOSTS=*.api.<domain>`, `NEXT_PUBLIC_HCAPTCHA_SITE_KEY` when `[studio] hcaptcha_site_key` is set, and `NEXT_PUBLIC_DISABLED_FEATURES` while the dashboard has an SSO provider. Studio's build disables `dashboard_auth:sign_in_with_sso` by default (that is what hides "Continue with SSO", `studio/placeholders.json`), and the runtime substitution reads the list when Studio starts; with a provider registered (`sbctl.sso_providers`, asked of the registry through `registry.Postgres.HasDashboardSSO` or `Deps.DashboardSSO`) the unit carries the default list without the SSO entry (`StudioDisabledFeatures`, kept equal to the placeholder file by a test). `Manager.RefreshStudio` renders Studio's unit again and restarts it only when its files changed; the API server and `sbctl sso add|remove` call it when the first provider appears and the last one goes. It does nothing on a node that never rendered Studio. Contract: `studio/README.md`.

The elixir services also get `RELEASE_TMP=<state>/system/<svc>/tmp` (a release may write there; the artifact directory is read-only under systemd). Every unit runs with `[defaults]` limits (1G, 100%).

Secrets (all sealed in the registry under the system project, generated once and never replaced because some encrypt data at rest): `fleet_supavisor_{api_jwt_secret,metrics_jwt_secret,secret_key_base,vault_enc_key}`, `fleet_realtime_{api_jwt_secret,metrics_jwt_secret,secret_key_base,db_enc_key,db_enc_key_gcm}`, `fleet_storage_{admin_api_key,encryption_key}`, `pgmeta_crypto_key`. The insert is insert-if-absent followed by a read, so the CLI and the daemon agree when they race.

## Pooler TLS

Supavisor answers `SSLRequest` on 5432 and 6543 from a certificate at `<state>/system/supavisor/tls/server.{crt,key}` (key 0600), which `Manager` creates when it renders the unit: RSA 2048, self-signed, valid 10 years, with `pooler.<domain>`, `localhost` and the loopback addresses as names. It is replaced 30 days before it ends, when its key does not match, or when the domain changed, and the unit restarts then (the certificate's hash is in the unit's environment as `SBCTL_DOWNSTREAM_CERT_SHA256`, which Supavisor ignores). The Supabase CLI refuses a remote database that answers without TLS (`utils/connect.go` drops the plaintext fallback whenever a TLS config exists), so the pooler has to offer it. `sslmode=require` and `prefer`, the defaults of psql, pgx and the CLI, encrypt without verifying the chain; `verify-ca` and `verify-full` need this certificate handed to the client. Plain connections still work (`enforce_ssl` is false). Exporting the CertMagic certificate for `pooler.<domain>` instead is a follow-up.

## Tenants

All three are idempotent and keep a fingerprint (a sealed hash of the request) per project and service in `project_secrets` (`fleet_tenant_<service>`). `EnsureTenant` asks the service for the tenant first; if it exists and the fingerprint matches, nothing is sent. That matters: an update makes Supavisor terminate the tenant's pools, and one with a re-encrypted secret makes Realtime disconnect the tenant's sockets. A tenant the service lost (404) is created again, and a changed spec (key rotation) is sent. Calls retry connection errors, timeouts, 408, 425, 429 and 5xx with exponential backoff (5 tries, 0.5 s to 8 s, 25% jitter, bounded by the context); 4xx answers are final. `RemoveTenant` treats 404 as success and clears the fingerprint.

- **Supavisor**: `PUT /api/tenants/<ref>` with `{"tenant": {db_host, db_port, db_database, upstream_ssl:false, enforce_ssl:false, require_user:false, auth_query:"SELECT * FROM pgbouncer.get_auth($1)", default_pool_size, default_max_clients, users:[{db_user:"pgbouncer", db_password, mode_type, pool_size, is_manager:true}]}}`, bearer JWT over `API_JWT_SECRET`. This is the scheme hosted Supabase and the CLI use: no per-user rows; the manager user runs the auth query to fetch the SCRAM verifier of whoever logs in. The `pgbouncer` role ships without a password in the supabase/postgres init scripts, so `EnsureTenant` first sets one as `supabase_admin` over loopback (a SCRAM verifier, with statement logging off for the session). The password is `hex(HMAC-SHA256(supabase_admin password, "sbctl supavisor manager"))`: stable, nothing to store, and the pooler holds no superuser credential, only the right to call `get_auth`. `default_pool_size` (and the manager user's `pool_size`) and `default_max_clients` come from the project's saved pooler settings (`TenantSpec.PoolSize`, `MaxClients`; 15 and 1000 when unset). Both are always sent, because a tenant update that omits `default_max_clients` keeps the old value; a changed value changes the fingerprint, so the next `EnsureTenant` updates the tenant, which ends its pooled connections. Removal calls `/terminate` once (stops the pools), then `DELETE`.
- **Realtime**: `POST /api/tenants` with `{"tenant": {external_id, name, jwt_secret, postgres_cdc_default, extensions:[{type:"postgres_cdc_rls", tenant_external_id, settings:{region, db_host, db_port (a string), db_name, db_user, db_password, slot_name, publication, poll_*, ssl_enforced}}]}}`, bearer JWT over `API_JWT_SECRET`. Realtime requires a superuser in the settings (upstream ENVS.md), so it connects as `supabase_admin`. Creating a tenant runs Realtime's tenant migrations in the project database.
- **Storage**: `PUT /tenants/<ref>` on the admin port with the `apikey` header and `{anonKey, serviceKey, jwtSecret, databaseUrl, maxConnections, fileSizeLimit, features}`. The HANDOFF names `POST`, which inserts and fails on a second call; `PUT` is upstream's upsert. The database URL uses `supabase_storage_admin` as upstream's compose does; its password comes from `TenantSpec.StorageAdminPassword` or, when empty (what `lifecycle.Engine.tenantSpec` passes today), from the project's sealed secrets. Creating a tenant runs Storage's migrations in the project database.

## Per-project settings and password changes

`TenantSpec.Storage` and `TenantSpec.Realtime` carry the project's saved settings
(`projectconfig.StorageSettings`, `RealtimeSettings`): the Storage tenant body gets `fileSizeLimit`
and the feature flags (`icebergCatalog` and `vectorBuckets` stay off at the tenant: their services
are not run), the Realtime tenant gets its limits and `private_only`/`suspend`/`presence_enabled`
at the top level and `db_pool` and `postgres_changes_pool` in the `postgres_cdc_rls` extension
settings. They are in the fingerprint, so an unchanged tenant is not touched and a saved change
sends one update. `LoadTenantSpec` (the CLI's `fleet ensure-tenant`) reads them from the
registry, so the CLI never sends defaults over saved settings.

`Fleet.RefreshTenant(ref)` calls `Refresher.RefreshTenant` on the tenants that cache logins:
Supavisor's `GET /api/tenants/<ref>/terminate` ("Stop tenant's pools and clear cache") makes the
new password of a role take effect at once through the pooler. Lazy tenants forward it.

`Fleet.QuiesceTenant(ref)` calls `Quiescer.QuiesceTenant` before a project's PostgreSQL stops (pause, a settings-driven restart): Realtime's `POST /api/tenants/<ref>/reload` stops the tenant's CDC processes and database pool and disconnects its sockets. Without it, Realtime's logical replication connection (a walsender) keeps a fast shutdown from finishing, and systemd kills the cluster after its 90 second stop timeout (found by `tests/linux/settings-smoke.sh`). The service reconnects when a client next asks for the tenant.

## Verified

On darwin-arm64 with the slim-services artifacts under the exec backend, through `internal/proxy` and supabase-js 2.117.2 against `<ref>.api.127.0.0.1.sslip.io` (see `TestIntegrationFleetTenants` for what is automated):

- `sbctl system init`, a project, `sbctl fleet start`, `ensure-tenant`: all four services healthy, a second `start` restarts nothing, `fleet stop` then `start` keeps every tenant.
- Pooler: `psql` as `postgres.<ref>` logs in on the session and the transaction port, with `sslmode=disable` and over TLS with `sslmode=require` (`TestIntegrationFleetTenants` checks that the connection is a `*tls.Conn`); a wrong password and an unknown tenant are refused.
- Storage: bucket create, upload, signed URL, download through it without a key, anonymous read of a private object refused, public bucket, list, remove. Two projects do not see each other's buckets, and one's key is refused on the other's host.
- Realtime: broadcast between two clients and `postgres_changes` INSERT (table over psql, row through PostgREST). Two projects with the same topic do not see each other.
- Key rotation: the services refuse the new keys until `ensure-tenant`, and accept them after. Tenant removal and re-creation.
- Studio: our build packaged from the earlier prebuilt output starts under the unit with this environment, the CSP carries the project hosts, no placeholder is left.

`TestIntegrationFleetTenants` (gated on `SBCTL_TEST_UNPACKED`, about 20 s, under 1 GB) runs system init, `Setup` with `Start`, `Engine.Create`, pooler logins, a Storage bucket, Realtime tenant health, `RotateKeys`, `Delete`.

## Not done, not verified

- **Linux and systemd**: not run on a developer machine. `tests/linux/fleet-smoke.sh` (job `fleet-smoke` in `.github/workflows/linux.yml`, Ubuntu 24.04 amd64 and arm64) runs the four services under the real templates and as the `sbctl` user: start, a second start that restarts nothing, one project registered, pooler logins on both ports, a Storage bucket with upload and signed-URL download, a Realtime join, key rotation, `kill -9` of each service, tenant removal, project delete, stop. Studio is not part of it (no slim artifact). The job needs a VM-local drop-in that makes the artifact directory writable for `sb-postgres@`: the Postgres launcher runs `chmod +x` on `share/supabase-cli/config/pgsodium_getkey.sh` on the first boot, and `ProtectSystem=strict` makes the artifacts read-only. The fix belongs in `sb-postgres@.service` (or in the unpack step: chmod the script once) and the drop-in goes away with it.
- **Studio under systemd needs a template change** (`deploy/systemd/sb-studio.service`, not ours): the launcher rewrites files under the artifact directory at every start (`studio/README.md`), which `ProtectSystem=strict` plus the read-only artifact bind forbids, so the unit needs the artifact's `app/` writable (`ReadWritePaths=` and `BindPaths=` for it), `RestartPreventExitStatus=78` and `SuccessExitStatus=143`.
- **Supavisor's API and shard listeners are on every interface.** The pinned artifact reads `SUPAVISOR_BIND_IP` (a slim-services addition) for the HTTP API and the proxy listeners, but that also moves 5432 and 6543, which must be reachable from outside, so it is not set. The API (`[fleet] supavisor_api_port`, 4001) needs a JWT only sbctl can sign, and the shard listeners (`PROXY_PORT`, `SESSION_PROXY_PORTS`, `TRANSACTION_PROXY_PORTS`) take ephemeral ports, but the host firewall or security group must still close all of them and open 5432 and 6543 only. The installer and the CloudFormation template (workstream G) must do that.
- The pooler certificate is self-signed (see Pooler TLS); verifying clients need it, and a CertMagic export is not done.
- The S3 backend of Storage and its S3 protocol endpoint in multi-tenant mode (DESIGN open item) were not run; only the environment is tested.
- Image transformation (imgproxy), the Storage queue (`PG_QUEUE_ENABLE`), Realtime clustering and Supavisor clustering are off.
- `sbctl system stop` (`lifecycle.StopAll`) does not stop the fleet units; use `sbctl fleet stop` first. `sbctl serve` starts the shared services at boot (`app.startFleet`, next to the projects, systemd supervisor only) and registers projects through a `Lazy` fleet; the units are not enabled for boot, the daemon is.
- Storage objects of a deleted project stay where the backend keeps them (`<ref>/`).
