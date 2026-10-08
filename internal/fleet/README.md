# internal/fleet

The shared services that serve every project, and the tenant calls that register a project with them.

| Service | Unit | Role | Tenant call |
|---|---|---|---|
| Supavisor (`pooler` artifact) | `supavise-supavisor` | one Postgres endpoint for all projects: `postgres.<ref>` on the session and transaction ports | `PUT /api/tenants/<ref>` |
| Realtime | `supavise-realtime` | websockets; tenant from the first label of `Host` (`<ref>.realtime.internal`) | `POST /api/tenants` |
| Storage | `supavise-storage` | `MULTI_TENANT=true`; tenant from `x-forwarded-host` (`<ref>.api.<domain>`) | `PUT /tenants/<ref>` on the admin port |
| postgres-meta | `supavise-pgmeta` | stateless; the connection string arrives encrypted with each request | none |
| Studio | `supavise-studio` | the dashboard (our platform-mode build) | none |
| Edge Runtime (`edge-runtime` artifact), only with `[functions] enabled` | `supavise-edge-runtime` | Edge Functions for every project through Supavise's own main service (`internal/functions/mainservice/`) | none: the main service picks the project from the proxy's `X-Supavise-Project-Ref` header and reads `system/edge-runtime/tenants/<ref>/` (written by `internal/functions`; the unit sees nothing of `projects/`) |

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

`Setup` returns the tenant Fleet even when a service did not start: the error names the services that failed, and the tenants of the others work. The Fleet is nil only when Setup could not build it (missing Deps, system project not initialized). A caller must not boot with an empty Fleet because one service failed, since the Engine skips every tenant call when the Fleet is empty. Studio is optional: when it cannot start (no artifact, a unit that fails) `Start` logs a warning, returns no error for it, and `Status` reports it; its wait is capped at one minute. `Setup` generates the services' secrets on first use, so the system project must exist.

`lifecycle.Open` builds its Engine before anyone can hand it a Fleet, so the caller either builds the Engine again (above) or passes a `Lazy` to `Open`, which is what the CLI does (`openNode`), so `supavise projects create|rotate-keys|delete` register, re-key and remove tenants too:

```go
lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log})
oo.Fleet = lz.Fleet()
n, _ := lifecycle.Open(ctx, cfg, oo)
lz.Bind(n.Registry, n.Secrets)
```

A `Lazy` tenant loads its credentials on first use and skips a service whose unit this node never rendered, so a node that does not run the fleet still creates projects. `NewManager(deps)` gives `Start`, `Stop`, `Status` and `Specs` without the tenants; `Stop` and `Status` need only `Cfg` and `Supervisor`. `supavise serve` runs `Setup` with `Start` at every boot (`internal/app`), and the Engine's tenants are a `Lazy` over the same `Setup` (`cmd/supavise/serve_fleet.go`). `Deps.ProbeClient` (default 4 s) is for the health checks and `Deps.TenantClient` (default 2 minutes) for the tenant calls: creating a Realtime or Storage tenant runs migrations before it answers. `LoadTenantSpec` and `TenantSpecFor` build the `TenantSpec` of a project from the registry.

The Engine calls `Fleet.EnsureTenant` after a project's units are healthy and again after key rotation, and `RemoveTenant` on delete and on a failed create. A Fleet whose services are down makes create fail (INIT_FAILED) after the bounded retries; a node that does not run the fleet passes an empty `Fleet`.

CLI (`cmd/supavise/cmd_fleet.go`):

```
supavise fleet start [--no-fetch] [--skip studio]   # fetch artifacts, render, start in order, wait for each
supavise fleet stop
supavise fleet status
supavise fleet ensure-tenant <ref>... | --all       # repeat the tenant calls by hand
supavise fleet remove-tenant <ref>...
```

Start order: pgmeta, supavisor, realtime, storage, edge-runtime (only when `[functions] enabled`, see `ServicesFor`), studio; stop is the reverse. A service that does not start does not stop the others, and `start` then exits non-zero. Studio is optional: `fleet start` skips it with a note when no artifact is installed and `[studio] artifact_url` is unset, `fleet status` (and `fleet.AllHealthy`) does not count a Studio whose unit was never rendered, and a Studio that was rendered but does not run makes `fleet start` exit non-zero without failing the daemon's `Setup`. Readiness is a request to each service's health path (2xx): Supavisor `/api/health` (204), Realtime `/healthcheck`, Storage `/status`, pgmeta `/health`, Studio `/api/get-utc-time`.

## Environment per service

- **Supavisor**: `DATABASE_URL` (`ecto://supavise_supavisor@…/_supavisor`), `PORT` = `[fleet] supavisor_api_port` (4001), `PROXY_PORT_SESSION`/`PROXY_PORT_TRANSACTION` = `[ports]`, `API_JWT_SECRET`, `METRICS_JWT_SECRET` (random, never shown, so the metrics endpoint is unreachable), `SECRET_KEY_BASE`, `VAULT_ENC_KEY` (32 bytes), `PROXY_PORT=0`, `SESSION_PROXY_PORTS=0`, `TRANSACTION_PROXY_PORTS=0` (the internal shard listeners take ephemeral ports), `GLOBAL_DOWNSTREAM_CERT_PATH` and `GLOBAL_DOWNSTREAM_KEY_PATH` (pooler TLS, below), `bin/prepare` before start (migrations in `_supavisor`; not on a follower, see Follower mode). No `CLUSTER_POSTGRES`: each node runs its own Supavisor.
- **Realtime**: `DB_*` for `_realtime` as `supavise_realtime` with `DB_AFTER_CONNECT_QUERY=SET search_path TO _realtime`, `API_JWT_SECRET`, `METRICS_JWT_SECRET`, `SECRET_KEY_BASE`, `DB_ENC_KEY` (16) and `DB_ENC_KEY_GCM` (32) with `DB_ENC_WRITE_GCM=true`, `APP_NAME=supavise`, `PORT`, `PHX_HTTP_IP=127.0.0.1` (a slim-services patch; upstream binds every interface), gen_rpc on loopback at `Ports.Realtime + 1369` (5369 by default), `RUN_JANITOR=true`. No `SEED_SELF_HOST`: tenants come through the API. `bin/prepare` before start (migrations).
- **Storage**: `MULTI_TENANT=true`, `DATABASE_MULTITENANT_URL` (`_storage` as `supavise_storage`), `SERVER_HOST=127.0.0.1`, `SERVER_PORT`, `SERVER_ADMIN_PORT` = `Ports.StorageAdmin`, `SERVER_ADMIN_API_KEYS` (random), `AUTH_ENCRYPTION_KEY`, `REQUEST_X_FORWARDED_HOST_REGEXP=^([a-z]{20})\.api\.<domain>$`, `REQUEST_ALLOW_X_FORWARDED_PATH=true` (the proxy sends `X-Forwarded-Prefix`), `S3_PROTOCOL_NON_CANONICAL_HOST_HEADER=X-Supavise-Client-Host` (Storage checks an S3 request's signature against the host the client signed; with a custom hostname that is not the derived host in `x-forwarded-host`, so the proxy puts the client's own host in this header), `STORAGE_BACKEND=file` with `STORAGE_FILE_BACKEND_PATH=<state>/system/storage/objects` (objects end up under `stub/<ref>/<bucket>/`), or `s3` from `[fleet] storage_s3_*`. Under systemd the s3 backend needs `storage_s3_role_arn` (the daemon serves the role's credentials, see Storage credentials on AWS) or `storage_s3_access_key_id` and `storage_s3_secret_access_key`: `supavise-storage` denies the instance metadata service, so the EC2 instance role is not available to it, and `Setup` refuses the unit with neither (a static key must be scoped to the objects bucket). The two kinds are alternatives; with the role, Storage's environment holds `AWS_CONTAINER_CREDENTIALS_FULL_URI` and `AWS_CONTAINER_AUTHORIZATION_TOKEN` and no key. The server runs its multi-tenant migrations itself at start, so there is no `bin/prepare`.
- **pgmeta**: `PG_META_HOST=127.0.0.1`, `PG_META_PORT`, `PG_META_ADMIN_PORT=Ports.PGMeta+1`, `CRYPTO_KEY` = `[api] pgmeta_crypto_key`, or the sealed system secret `pgmeta_crypto_key` (the one `api.EnsurePGMetaCryptoKey` creates).
- **Edge Runtime** (`edgeruntime.go`): `bin/edge-runtime start --ip 127.0.0.1 --port <ports.edge_runtime> --main-service <state>/system/edge-runtime/main --policy per_worker --user-worker-request-idle-timeout <ms> --graceful-exit-timeout 10 --max-parallelism n`, with `EDGE_RUNTIME_PORT`, `SUPAVISE_FUNCTIONS_ROOT` (`<state>/system/edge-runtime/tenants`), the `SUPAVISE_FUNCTIONS_*` limits from `[functions]`, `SUPAVISE_FUNCTIONS_PROXY_TOKEN` (the node's proxy secret from `<state>/system/edge-runtime.token`, made on first use by `config.LoadFunctionsProxyToken`), `DENO_DIR=<state>/system/edge-runtime/deno` and `HOME`; no other secret. The main service is embedded in the binary and written by `EnsureMainService` before the unit starts (its hash is in the unit's environment, so a new binary restarts the runtime). Both paths are real paths: the runtime fails with "Module not found" for a main service reached through a symlink (macOS `/tmp`). Readiness is `GET /_internal/health`. The worker budget (`max_workers`, `max_parallelism`, and the `MemoryMax` derived from them: 4864M by default) is described in `internal/functions/mainservice/README.md`. **Turning `[functions] enabled` off** takes the runtime off the node: `fleet start` removes the rendered unit (stopping it) and deletes the tenants tree, which holds every project's JWT secret, service key, database password and function secrets; `fleet stop` and `fleet status` still show the unit while it is rendered (status: optional, `UNHEALTHY` if it still runs).
- **Studio**: `HOSTNAME`, `PORT`, `NEXT_PUBLIC_API_URL` (`<api url>/platform`), `NEXT_PUBLIC_GOTRUE_URL` (`<api url>/auth/v1`, the system GoTrue through the proxy), `NEXT_PUBLIC_SITE_URL`, `CSP_EXTRA_PROJECT_HOSTS=*.api.<domain>`, `NEXT_PUBLIC_HCAPTCHA_SITE_KEY` when `[studio] hcaptcha_site_key` is set, and `NEXT_PUBLIC_DISABLED_FEATURES` while the dashboard has an SSO provider. Studio's build disables `dashboard_auth:sign_in_with_sso` by default (`studio/placeholders.json`); with a provider registered (`supavise.sso_providers`, through `registry.Postgres.HasDashboardSSO` or `Deps.DashboardSSO`) the unit carries the default list without the SSO entry (`StudioDisabledFeatures`). `Manager.RefreshStudio` renders Studio's unit again and restarts it only when its files changed; the API server and `supavise sso add|remove` call it when the first provider appears and the last one goes, and it does nothing on a node that never rendered Studio. Contract: `studio/README.md`.

The elixir services also get `RELEASE_TMP=<state>/system/<svc>/tmp` (a release may write there; the artifact directory is read-only under systemd). Every unit runs with `[defaults]` limits (1G, 100%).

Secrets (all sealed in the registry under the system project, generated once and never replaced because some encrypt data at rest): `fleet_supavisor_{api_jwt_secret,metrics_jwt_secret,secret_key_base,vault_enc_key}`, `fleet_realtime_{api_jwt_secret,metrics_jwt_secret,secret_key_base,db_enc_key,db_enc_key_gcm}`, `fleet_storage_{admin_api_key,encryption_key}`, `pgmeta_crypto_key`. The insert is insert-if-absent followed by a read, so the CLI and the daemon agree when they race.

## Pooler TLS

Supavisor answers `SSLRequest` on 5432 and 6543 from a certificate at `<state>/system/supavisor/tls/server.{crt,key}` (key 0600), which `Manager` creates when it renders the unit: RSA 2048, self-signed, valid 10 years, with `pooler.<domain>`, `localhost` and the loopback addresses as names. It is replaced 30 days before it ends, when its key does not match, or when the domain changed, and the unit restarts then (the certificate's hash is in the unit's environment as `SUPAVISE_DOWNSTREAM_CERT_SHA256`, which Supavisor ignores). The Supabase CLI refuses a remote database that answers without TLS, so the pooler has to offer it. `sslmode=require` and `prefer`, the defaults of psql, pgx and the CLI, encrypt without verifying the chain; `verify-ca` and `verify-full` need this certificate handed to the client. Plain connections still work (`enforce_ssl` is false).

## Tenants

All three are idempotent and keep a fingerprint (a sealed hash of the request) per project and service in `project_secrets` (`fleet_tenant_<service>`). `EnsureTenant` asks the service for the tenant first; if it exists and the fingerprint matches, nothing is sent (an update makes Supavisor terminate the tenant's pools, and one with a re-encrypted secret makes Realtime disconnect the tenant's sockets). A tenant the service lost (404) is created again, and a changed spec (key rotation) is sent. Calls retry connection errors, timeouts, 408, 425, 429 and 5xx with exponential backoff (5 tries, 0.5 s to 8 s, 25% jitter, bounded by the context); 4xx answers are final. `RemoveTenant` treats 404 as success and clears the fingerprint.

- **Supavisor**: `PUT /api/tenants/<ref>` with `{"tenant": {db_host, db_port, db_database, upstream_ssl:false, enforce_ssl:false, require_user:false, auth_query:"SELECT * FROM pgbouncer.get_auth($1)", default_pool_size, default_max_clients, users:[{db_user:"pgbouncer", db_password, mode_type, pool_size, is_manager:true}]}}`, bearer JWT over `API_JWT_SECRET`. This is the scheme hosted Supabase and the CLI use: no per-user rows; the manager user runs the auth query to fetch the SCRAM verifier of whoever logs in. The `pgbouncer` role ships without a password, so `EnsureTenant` first sets one as `supabase_admin` over loopback (a SCRAM verifier, statement logging off for the session). The password is `hex(HMAC-SHA256(supabase_admin password, "supavise supavisor manager"))`: stable, nothing to store, and the pooler holds no superuser credential, only the right to call `get_auth`. `default_pool_size` (and the manager user's `pool_size`) and `default_max_clients` come from the project's saved pooler settings (`TenantSpec.PoolSize`, `MaxClients`; 15 and 1000 when unset). Both are always sent, because a tenant update that omits `default_max_clients` keeps the old value; a changed value changes the fingerprint, so the next `EnsureTenant` updates the tenant, which ends its pooled connections. Removal calls `/terminate` once (stops the pools), then `DELETE`.
- **Realtime**: `POST /api/tenants` with `{"tenant": {external_id, name, jwt_secret, postgres_cdc_default, extensions:[{type:"postgres_cdc_rls", tenant_external_id, settings:{region, db_host, db_port (a string), db_name, db_user, db_password, slot_name, publication, poll_*, ssl_enforced}}]}}`, bearer JWT over `API_JWT_SECRET`. Realtime requires a superuser in the settings, so it connects as `supabase_admin`. Creating a tenant runs Realtime's tenant migrations in the project database.
- **Storage**: `PUT /tenants/<ref>` on the admin port with the `apikey` header and `{anonKey, serviceKey, jwtSecret, databaseUrl, maxConnections, fileSizeLimit, features}`. `PUT` is upstream's upsert (`POST` inserts and fails on a second call). The database URL uses `supabase_storage_admin`; its password comes from `TenantSpec.StorageAdminPassword` or, when empty (what `lifecycle.Engine.tenantSpec` passes today), from the project's sealed secrets. Creating a tenant runs Storage's migrations in the project database.

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

`Fleet.QuiesceTenant(ref)` calls `Quiescer.QuiesceTenant` before a project's PostgreSQL stops (pause, a settings-driven restart): Realtime's `POST /api/tenants/<ref>/reload` stops the tenant's CDC processes and database pool and disconnects its sockets. Without it, Realtime's logical replication connection (a walsender) keeps a fast shutdown from finishing, and systemd kills the cluster after its 90 second stop timeout. The service reconnects when a client next asks for the tenant.

## Follower mode

A node whose system cluster is a hot standby follows the leader of its cluster. The shared services run in a follower profile there (`follower.go`):

| Service | On a follower |
|---|---|
| Supavisor | runs, against the replicated `_supavisor`, without `bin/prepare` (it migrates a database and exits 1 on a standby). The tenants, users and secrets are the leader's rows; the same `VAULT_ENC_KEY` (a sealed registry secret every node reads) opens them. A pooler login for a replicated tenant works at once. |
| pg-meta, Realtime, Storage, Edge Runtime, Studio | parked: the unit is rendered, so the artifact and files are in place, and stopped. Their ports belong to the mesh's forwarders, which carry a connection to the leader's service. A missing artifact of a parked service is logged, not an error. |

Nothing is written. `Start` reads the services' secrets instead of generating them (the leader made them and they replicated), every spec drops its `PreStart`, and the tenants skip `EnsureTenant`, `RemoveTenant`, `QuiesceTenant` and the replica calls: those are the leader's, and the fingerprint they keep in the registry could not be saved on a standby. A parked service answers `HasTenant` with "present", so the health check does not blame a follower for a tenant that only the leader's Realtime or Storage can hold. `RefreshTenant` is the one tenant call a follower makes (below).

Which profile applies is read from the system cluster, `pg_is_in_recovery()` on the registry's pool, so a daemon boot, a CLI call and a promotion agree without being told. `Deps.Follower` replaces the question (tests, a caller that knows better); a registry that cannot be asked, the in-memory one, is a leader; an error is returned, not guessed. Every tenant asks again on each call, so a `Lazy` that outlives a promotion starts working at once.

`Manager.Apply(ctx, mode)` fixes the profile by hand and is what a role change calls: `ModeLeader` starts everything (a promotion starts the parked services, and Supavisor restarts once, because its launcher gains `bin/prepare`), `ModeFollower` parks them and restarts Supavisor without `bin/prepare` (a demotion), `ModeStopped` stops all of it (a fence). The daemon's hook (`internal/app/wire_fleet.go`) calls it from `cluster.Membership.Watch`: the first snapshot is the role the daemon booted in, which `startFleet` already put in effect; each later change is applied, and a failed apply (a forwarder that still holds a service's port) is tried again every 5 seconds until it works, whatever role is wanted by then. A node that is part of no cluster (no `mesh.Mesh`) is a leader for good and the hook does nothing.

`Status` marks a parked service optional and "STOPPED: parked", and reports one that runs on a follower as unhealthy, because its port is the forwarder's. `Start` records the profile it applied in `<state>/system/fleet-mode`, which `supavise fleet status` (no registry) reads. `RefreshStudio` does nothing on a follower.

## Replica tenants

A replica's pool is a Supavisor tenant of its own: external id `<ref>-rr-<region>-<id6>` (the replica identifier), `db_port` the replica port (`config.ReplicaPorts`). The port is the replica itself on its node and a forwarder on every other, so one row in `_supavisor` serves the Supavisor of every node, and `postgres.<identifier>` logs in to the standby. `TenantSpecForReplica` and `LoadReplicaTenantSpec` (which refuses a project whose sequence is above `config.MaxReplicaSeq`) build the spec; `Fleet.EnsureReplicaTenant` and `Fleet.RemoveReplicaTenant` reach the tenants that implement `ReplicaTenanter` (Supavisor only, and its `Lazy` entry).

The tenant body is the project's own with the replica's port. The manager login is the project's `pgbouncer` role, whose password the project's own `EnsureTenant` set on the primary and the standby replicated; a standby cannot be written, so `EnsureReplicaTenant` sets nothing, and the project's tenant is ensured first. The fingerprint is kept next to the project's under `fleet_tenant_supavisor-<identifier>`, so an unchanged replica tenant sends nothing and the project's rows delete it with the project. `RefreshTenant` and `HasTenant` take an identifier as well as a ref.

## Tenant refresh on followers

A follower's Supavisor keeps the target of a tenant until `GET /api/tenants/<id>/terminate` (`Refresher.RefreshTenant`); a new tenant needs no refresh. The refresh has to come after the follower's standby has replayed the change, or Supavisor reads the old row again. The leader's writer therefore sends the WAL position of its write: `POST /peer/v1/fleet/refresh/{tenant}` with `peerapi.RefreshRequest{LSN}`, the leader's `pg_current_wal_lsn()` taken after the write.

- `PeerRefresh` (`refresh.go`) is the follower's half: validate the tenant and the position, wait up to 10 seconds for `pg_last_wal_replay_lsn()` to reach it (`PGReplay`), then refresh. Replay that does not arrive in time is `ErrReplayBehind` and nothing is refreshed; the endpoint answers 503 `replay_behind` and the caller tries again. A node that does not run Supavisor answers `refreshed: false`.
- `PeerRefresher` is what the leader's callers use: `RefreshPeers(ctx, tenant)` reads the leader's position once and asks every other active node, in parallel. The hook provides it (`app.Get[fleet.PeerRefresher](w)`) when the node has a mesh; the caller changes the row, refreshes its own Supavisor with `Fleet.RefreshTenant`, then calls it. The daemon also provides its `fleet.Fleet` the same way, for the replica controller.
- The endpoint answers only the node the registry names as leader; any other peer gets 403 `not_leader`.

## Storage credentials on AWS

`[fleet] storage_s3_role_arn` names the IAM role that carries the objects bucket's policy (the stack's `StorageRole`). The daemon assumes it with `sts:AssumeRole` through `internal/awsapi`, with the instance's credentials, and serves the result on `127.0.0.1:[fleet] storage_credentials_port` (4010) in the ECS container-credential shape (`credentials.go`):

```
GET /credentials   Authorization: <token>
{"AccessKeyId": ..., "SecretAccessKey": ..., "Token": ..., "Expiration": "2006-01-02T15:04:05Z", "RoleArn": ...}
```

- Storage's AWS SDK finds the endpoint through `AWS_CONTAINER_CREDENTIALS_FULL_URI`, sends `AWS_CONTAINER_AUTHORIZATION_TOKEN`, caches the credentials and asks again within five minutes of their end. `storageEnv` sets those two variables and no key. The unit's `IPAddressDeny` covers only the metadata addresses, so loopback needs no change.
- The endpoint answers a GET of `/credentials` from a loopback address that presents the token (constant-time compare), and 403 with no body to everyone else. A failed STS call is 502 with a generic message, logged at most every 30 seconds.
- The role is assumed for an hour on the first request and again when five minutes are left (`awsapi.CachedCredentials`); a renewal that fails keeps the old credentials for as long as they work.
- The token is random per boot: 32 bytes in `<state>/system/storage/credentials.json` (0600), with the kernel's boot id. A daemon restart, or a CLI that renders Storage's unit, reads the same token, so neither restarts Storage; a reboot makes a new one, and the next render restarts Storage. A flock makes the first creation happen once.
- The daemon binds the port before it renders Storage's unit. A node that cannot build the endpoint (no AWS client, port taken) still serves its projects, and logs that Storage on S3 will fail its requests.
- The config check refuses `storage_s3_role_arn` together with a static key. Off AWS, the static keys work as before.

## Tenant presence

`TenantChecker.HasTenant(ctx, ref)` asks a service whether it holds a project's tenant: one GET of the tenant record, 2xx is present, 404 absent, anything else (and a service that cannot be reached) an error, with no retries so that a health check never waits out `EnsureTenant`'s backoff. Supavisor, Realtime and Storage implement it; `Fleet.TenantPresence(ctx, ref)` asks all of them at once and keeps the fleet's order. The lazy tenants skip a service the node never rendered and answer "present". `internal/health` uses it for the per-project check.

## Releases and upgrades

- `Manager.Start` restarts a running service whose rendered files changed, one at a time and each waited for. `Deps.HaltOnFailure` makes it stop at the first service that does not start; the daemon sets it while a `supavise upgrade` is running (`notice.UpgradeRunning`), so that a release whose Realtime does not come up does not also restart Storage, and the upgrade rolls back with the later services still on the old release.
- `RenderedTag(cfg, svc)` reads the release a service's launcher script is set to run (the artifact directory in `<state_dir>/projects/system/<svc>.run`). `supavise upgrade` uses it to learn what release a node runs without asking the binary that rendered it, and to see that a service moved.
- The Storage and Realtime tenants fold the release tag of their service into the fingerprint they keep per project (`withRelease`), so a Supavise release that moves either one sends every tenant once more through `Engine.EnsureTenants`: Storage runs its tenant migrations when the tenant is updated, Realtime when it is created. A node that cannot say which release it runs keeps the old fingerprint. Supavisor's tenants are not sent again: that would drop its pools, and a new Supavisor needs nothing in a tenant.

## Tests

The follower profile, the replica tenants, the refresh and the credential endpoint have unit tests over a fake supervisor, a fake tenant API, `awsfake` and a scripted standby (`follower_test.go`, `replica_test.go`, `refresh_test.go`, `credentials_test.go`, and `internal/app/wire_fleet_test.go` for the hook). `follower_linux_test.go` (build tag `fleetfollower`) is the Go half of the linux `fleet-follower` job (`tests/linux/fleet-follower.sh`, Ubuntu 24.04 on amd64 and arm64): the script builds hot standbys of the leader's system cluster and of a project on the runner, the test acts as the daemon of a second node on them (exec supervisor, its own state directory and ports) and checks that Supavisor serves the replicated tenant without `bin/prepare`, that the other services are parked with their ports free, that a replica tenant written by the leader is served after replay and refresh, and that nothing wrote to the standby. The same script starts the leader's daemon with `storage_s3_role_arn` against Garage and a stand-in for STS, uploads through Storage, and checks the endpoint's token, the cache, and that a daemon restart changes neither the token nor Storage's process.

`TestIntegrationFleetTenants` runs system init, `Setup` with `Start`, `Engine.Create`, pooler logins (checking the connection is a `*tls.Conn`), a Storage bucket, Realtime tenant health, `RotateKeys` and `Delete`; it runs only when `SUPAVISE_TEST_UNPACKED` points at unpacked slim-services artifacts (default `~/.cache/sbctl/unpacked`). The `ci` workflow's `test` job runs the unit tests. The linux `fleet-smoke` job (`tests/linux/fleet-smoke.sh`, Ubuntu 24.04 on amd64 and arm64) runs the services under the real systemd templates as the `supavise` user: start, a second start that restarts nothing, one project registered, pooler logins on both ports, a Storage bucket with upload and signed-URL download, a Realtime join, key rotation, `kill -9` of each service, tenant removal, project delete and stop.

## Limits

- A fenced node is stopped when the role changes to fenced while the daemon runs. A node that boots fenced starts its services as a leader's: `startFleet` does not ask the membership, and the fence logic owns that case.
- The refresh of followers is sent by whoever changes a tenant row and calls `PeerRefresher`. The Engine's own `Fleet.RefreshTenant` (a role password changed, a restore) refreshes the local Supavisor only.

- **Supavisor's API and shard listeners are on every interface.** The pinned artifact reads `SUPAVISOR_BIND_IP` (a slim-services addition) for the HTTP API and the proxy listeners, but that also moves 5432 and 6543, which must be reachable from outside, so it is not set. The API (`[fleet] supavisor_api_port`, 4001) needs a JWT only Supavise can sign, and the shard listeners (`PROXY_PORT`, `SESSION_PROXY_PORTS`, `TRANSACTION_PROXY_PORTS`) take ephemeral ports, but the host firewall or security group must still close all of them and open 5432 and 6543 only. The CloudFormation template's security group does that; the installer does when ufw is active or `--firewall ufw` is given, and does not with `--firewall none` or when ufw is not installed (see `deploy/README.md`, "Firewall and ports").
- Studio is not part of `fleet-smoke` (no slim artifact). Its unit requirements under systemd are in `studio/README.md`. The shipped `deploy/systemd/supavise-studio.service` makes the artifact directory writable but does not set `RestartPreventExitStatus=78` or `SuccessExitStatus=143`: with `Restart=on-failure` and `StartLimitIntervalSec=0`, a bad config value makes Studio restart in a loop, and a normal stop is recorded as a failure.
- The pooler certificate is self-signed (see Pooler TLS); verifying clients need it, and exporting the CertMagic certificate for `pooler.<domain>` instead is not done.
- Storage on S3 is exercised against Garage by `tests/linux/fleet-follower.sh`, with a stand-in for STS. The AWS half (a real role, session tokens validated by S3, no traffic to the metadata addresses) is the owner's rehearsal checklist.
- Image transformation (imgproxy), the Storage queue (`PG_QUEUE_ENABLE`), Realtime clustering and Supavisor clustering are off.
- `supavise system stop` (`lifecycle.StopAll`) does not stop the fleet units; use `supavise fleet stop` first. `supavise serve` starts the shared services at boot (`app.startFleet`, systemd supervisor only) and registers projects through a `Lazy` fleet; the units are not enabled for boot, the daemon is.
- Storage objects of a deleted project stay where the backend keeps them (`<ref>/`).
