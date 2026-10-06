# Handoff: build plan for parallel agents

Read `DESIGN.md` first; it is the contract. `research/` is evidence, not instructions. This file splits the build into workstreams that can run in parallel in fresh sessions, and fixes the conventions every workstream must share so they fit together without coordination.

## 0. Repo state and first actions

- This directory is **not its own git repository** (the enclosing repo root is the home directory). First action in a fresh session: `git init` here, commit `DESIGN.md`, `HANDOFF.md`, `research/`, then create the skeleton in section 2.
- Language: **Go** (single static binary, `CGO_ENABLED=0`). Module path placeholder `github.com/OWNER/sbctl` until the product is named. The name must not contain "Supabase".
- License: **Apache-2.0** for everything we write. Studio patches carry upstream's Apache-2.0.
- Target OS for v1: Ubuntu 24.04 and 22.04, Debian 12, amd64 and arm64. glibc 2.35 floor comes from the artifacts.
- Do not vendor or copy code from `kmhari/supastack` (AGPL). Reading it for evidence is fine.

## 1. Shared conventions (every workstream obeys these)

| Thing | Convention |
|---|---|
| Binary and service user | `sbctl`, runs as system user `sbctl`; Postgres units run as `sbctl` too (artifacts are relocatable, no root needed after install) |
| Config | `/etc/sbctl/config.toml`; env `SBCTL_*` overrides; secrets key at `/etc/sbctl/master.key` (0600) |
| State root | `/var/lib/sbctl/` with `artifacts/<service>/<version>/`, `projects/<ref>/{postgres,gotrue,postgrest}/`, `system/`, `certs/`, `backups/` (local backend) |
| Logs | journald only; unit names are the log selector |
| systemd units | templates `sb-postgres@.service`, `sb-gotrue@.service`, `sb-postgrest@.service`; singletons `sb-supavisor`, `sb-realtime`, `sb-storage`, `sb-pgmeta`, `sb-studio`, optional `sb-imgproxy`, `sb-edge-runtime`; all in slice `sbctl.slice`; per-project `MemoryMax` and `CPUQuota` from the project record; env files at `/var/lib/sbctl/projects/<ref>/<svc>.env` (0600) |
| Ref | 20 lowercase ASCII letters, `system` reserved |
| Ports | loopback only for project services. Postgres `20000 + 3n`, GoTrue `20001 + 3n`, PostgREST `20002 + 3n` where `n` is the project's registry sequence. Fleet: Supavisor 5432 (session) and 6543 (transaction) public; Realtime 4000, Storage 5000 and admin 5001, pgmeta 8080, Studio 3000, system Postgres 5433, system GoTrue 9999, imgproxy 5002, edge-runtime 9000, all loopback. `sbctl` 80 and 443 public, 7000 loopback admin |
| Hostnames | project API `<ref>.api.<domain>`; dashboard `studio.<domain>`; API server `api.<domain>`; pooler `pooler.<domain>`; Realtime internal host `<ref>.realtime.internal`; Storage tenant via `x-forwarded-host: <ref>.api.<domain>` |
| Keys | per project: HS256 JWT secret (40 bytes), legacy `anon` and `service_role` JWTs, `sb_publishable_<base58>` and `sb_secret_<base58>` opaque keys mapped to the legacy JWTs by the proxy; PATs `sbp_` + 40 lowercase hex |
| Registry schema | Postgres database `sbctl` in the system cluster, schema `sbctl`: `organizations`, `projects` (ref, org_id, seq, engine, class, status, versions jsonb, limits jsonb, created_at), `project_secrets` (encrypted blobs), `access_tokens`, `routes`, `backups`, `events`. Migrations embedded in the binary |
| Versions | `versions.yaml` at repo root: `artifacts.<service>: <slim-services release tag>`, `studio.tag`, `cli.version_tested`. Agents fetch current tags from `github.com/supabase/slim-services/releases`; never guess |
| API types | generated from `https://api.supabase.com/api/v1-json`, `v2-json`, `platform-json` into `internal/api/gen/`; never hand-write response structs |
| Internal interfaces | `DataPlane` (`Create`, `Delete`, `Snapshot`, `Route`, `Usage`), `Fleet` (`EnsureTenant`, `RemoveTenant` per service), `Supervisor` (`Render`, `Start`, `Stop`, `Status`), `Secrets` (`Seal`, `Open`), `Backup` (`PushWAL`, `FetchWAL`, `BaseBackup`, `Restore`) |
| Errors to users | Management-API error envelope `{ "message": string }` with the upstream status codes |
| Tests | `go test ./...` unit; `tests/conformance/` spins a node and runs upstream client suites; every workstream ships tests with its code |

## 2. Repo skeleton to create first

```
cmd/sbctl/main.go
internal/{api,proxy,units,lifecycle,backup,artifacts,fleet,registry,secrets,config}/
internal/api/gen/            # generated from the three OpenAPI specs
studio/{patches/,build.sh,Dockerfile.build}   # build only; the output is a tar.zst artifact
functions-main/              # Deno main service for edge-runtime (phase 2)
deploy/{install.sh,systemd/,cloudformation/sbctl.yaml}
tests/conformance/
versions.yaml
```

## 3. Workstreams

Each is independent given section 1. "Done" means merged with tests and a short `README.md` in its directory.

### A. Studio platform build (the spike, gates nothing else but answers the API list)
- Build upstream Studio at the pinned tag with `NEXT_PUBLIC_IS_PLATFORM=true`, `NEXT_PUBLIC_API_URL`, `NEXT_PUBLIC_GOTRUE_URL` as runtime-substituted placeholders.
- Patches: render hCaptcha only when a site key is set; env-overridable `dashboard_auth:*` flags; extra project hosts in the platform CSP. Keep each as one `git format-patch` file in `studio/patches/`.
- Run it against a mock `/platform` and `/v1` generated from the specs (workstream B can share the mock). Sign in, list two projects, open table editor on each, run SQL.
- Deliver: `studio/`, the artifact, and `research/08-studio-platform-calls.md` listing every endpoint Studio called, status needed, and whether an empty stub suffices.

### B. Management API server
- Generate types from the three specs. Implement the P0 subset in `DESIGN.md` section 6 over the registry. Stub everything else with empty 200/204 and log unknown routes at debug.
- Auth: GoTrue JWT from `sb-gotrue@system` on `/platform/*`; `sbp_` PATs on `/v1/*`; device-code login endpoints for `supabase login`.
- `pg-meta/{ref}/query` proxies to `sb-pgmeta` with `x-connection-encrypted` built from the project's connection string.
- Verify with the real `supabase` CLI (`--profile`) for `projects list`, `link`, `db push`, `gen types`, `functions deploy`, and the MCP server (`--api-url`) for `list_tables` and `execute_sql`. CLI decodes strictly; a missing required field is a test failure.

### C. Proxy and TLS
- `net/http` reverse proxy with WebSocket passthrough. Host to ref lookup from the registry with an in-memory cache invalidated on change.
- `apikey` validation, `sb_publishable_*` and `sb_secret_*` to legacy JWT translation, query-string `apikey` stripping, route table from `DESIGN.md` section 4.
- CertMagic: DNS-01 wildcard with libdns providers (route53, cloudflare, hetzner, digitalocean), HTTP-01 per host fallback, sslip.io default when no domain.
- Hooks for later idle-wake (a `Start(ref)` call before forwarding) but no sleep logic in v1.

### D. Artifacts, units and lifecycle
- Fetch `tar.zst` releases per `versions.yaml`, verify SHA-256, unpack to the artifacts dir. Read each artifact's launcher contract (`bin/supabase-postgres-start`, `bin/auth`, `bin/postgrest`, `bin/server`, `bin/storage`, `bin/pgmeta`, `bin/studio`) from `research/06` and the slim-services `HOST_NATIVE_ARTIFACTS.md`.
- Render systemd units and env files; `systemctl` via D-Bus; status and health checks.
- Project create, pause, resume, delete, rotate-keys per `DESIGN.md` section 5, including running the artifact's init migrations and roles against a fresh cluster and setting the per-cluster `pgsodium` root key.
- The system project: same code path with `ref = system`, hosting the registry, `_supavisor`, `_realtime`, `_storage` databases and the dashboard auth schema.
- Deliver a measured table: RSS and cold start on Ubuntu 24.04 at 10, 25 and 50 projects.

### E. Fleet integration
- Start Supavisor, Realtime, Storage, pgmeta from artifacts against the system cluster. Env from upstream's compose and the slim-services recipes, with `MULTI_TENANT=true` for Storage and `REQUEST_X_FORWARDED_HOST_REGEXP` matching `<ref>.api.<domain>`.
- `Fleet.EnsureTenant` per service: Supavisor `PUT /api/tenants/<ref>` (JWT with `API_JWT_SECRET`), Realtime `POST /api/tenants`, Storage `POST /tenants/<ref>` on 5001 with admin apikey. Idempotent, retried.
- Verify with supabase-js: realtime broadcast and postgres_changes, storage upload and signed URL, pooler connect as `postgres.<ref>` in both modes.

### F. Backups
- `sbctl wal push <path>` and `sbctl wal fetch <name> <dest>` for `archive_command` and `restore_command`, backends `file://` and `s3://` (AWS SDK v2, works with any S3-compatible endpoint).
- Nightly `pg_basebackup` per project via a systemd timer; retention policy in config.
- `sbctl backups restore <ref> --to <timestamp> [--as <newref>]` builds a new project from base plus WAL.
- Test: write, back up, destroy, restore to a point before the destroy, assert row.

### G. Installer and AWS
- `deploy/install.sh`: OS and glibc check, user, checksum-verified download, config, units, firewall (80, 443, 5432, 6543), start, print URL and claim token. Idempotent.
- Claim flow in `sbctl`: first admin created only with the token printed at install.
- `deploy/cloudformation/sbctl.yaml`: one Ubuntu 24.04 instance, instance role with Route 53 change rights scoped to the hosted zone and S3 rights scoped to the bucket, security group, S3 bucket, Secrets Manager secret for the claim token, user data calling `install.sh`, `cfn-signal`, outputs. Quick-create link in the README.
- `sbctl self-update` with signature check.

### H. Conformance suite
- `tests/conformance/`: given a running node, create two projects and run the upstream `supabase-js` test suites for auth, postgrest, realtime, storage against each, plus Playwright smoke on Studio (sign in, switch project, table editor, SQL editor). Nightly and on every `versions.yaml` bump.
- `tests/specdiff/`: nightly fetch of the three OpenAPI specs and a diff against the routes the server registers; new P0-tier routes fail the build.

### Phase 2 workstreams (start once A to H are green)
- Edge Functions: tenant-aware main service in `functions-main/`, `sb-edge-runtime` unit, `/functions/v1` route, `/v1/projects/{ref}/functions*` and secrets endpoints wired to the per-project functions directory.
- Idle sleep in C and D.
- imgproxy unit and Storage transform flag.
- Members and RBAC, clone-based branching on top of F, restore UI.

## 4. Dependencies between workstreams

```
A (Studio + call list) ----> B (API stubs tightened)
D (units/lifecycle) -------> E (fleet tenants)  -------> H (conformance)
B + C + D + E + F ---------> G (installer)      -------> H
```

A, B, C, D, F can start simultaneously. E needs D's system cluster. G needs a working binary. H needs G.

## 5. What a fresh session must not re-decide

Cluster per project. Native artifacts, no Docker. Proxy inside the binary, no Envoy. Supavisor as the only Postgres entry point. WAL archiving via the binary, no wal-g. Wildcard DNS required. Studio platform build with exactly the three patches. Management API types generated from the specs. Apache-2.0. Name without "Supabase". Everything else in `DESIGN.md` section 10 was cut deliberately; do not reintroduce it without a written reason in that table.
