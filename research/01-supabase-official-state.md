# 01 - Supabase official state and direction (self-hosting and multi-tenancy)

Research date: 2026-10-06. Method: live pages (WebFetch, raw `.md` of docs pages via curl), GitHub API via `gh`, and local clones of the service repos. Where a fact came from a summarizing fetch only, it is flagged. Section 7 is my own analysis, labeled as such.

---

## TL;DR

- **No official multi-tenant self-hosting exists.** Docs state self-hosted Supabase "runs as a single project" and Studio has no org/project layer. The only official offer for enterprises is a contact form ("Growth Team", "design partnership opportunities"). No BYOC / private-cloud product for Supabase Postgres is documented anywhere I could find.
- **The "dockerless CLI" is a local-dev feature, not a production runtime.** `supabase start --runtime native` (CLI 2.118+/2.119, alpha, off by default) runs each service as a host process from relocatable `tar.zst` artifacts (repo `supabase/slim-services`, MIT). Linux amd64/arm64 (glibc 2.35+) and macOS 14+ Apple silicon only. Docs say the CLI stack "must not be exposed to external traffic".
- **But the native artifacts and the `@supabase/stack` orchestrator are the closest thing to a "native, lightweight, many-stacks-per-host" building block Supabase has published.** Features: per-stack identity/state dir, lazy-start on first request, idle-sleep (60 s; Studio 5 min), `provision-tenant` for Supavisor, port allocation, snapshots. `@supabase/stack` is `private: true` (not on npm), but the source is in the public CLI repo.
- **Hosted platform = one dedicated Postgres VM per project** (docs: "its own dedicated Postgres instance", "isolated environments"). The public `supabase/postgres` repo contains the Ansible/Packer/Nix build of that VM image: Postgres + PgBouncer + PostgREST + GoTrue + Envoy (Kong legacy) + `adminapi` + `admin-mgr` + `supabase-admin-agent` + Vector + postgres_exporter + wal-g/pgbackrest + Salt. Storage, Realtime, Edge Runtime, Studio, Supavisor, Logflare are NOT in the AMI (inference: they are shared/external).
- **The control plane is closed.** `adminapi`, `admin-mgr`, `supabase-admin-agent` are fetched as prebuilt binaries from a public S3 bucket; no source repos found. The Management API is not available when self-hosting (official docs).
- **Per-service multi-tenancy status (from source):** Supavisor, Realtime, Storage (`MULTI_TENANT=true`) are natively multi-tenant. Auth's multi-tenant mode (`GOTRUE_MULTI_INSTANCE_MODE`) is an inherited legacy feature that "may be removed". PostgREST, postgres-meta, Studio, Edge Runtime (as shipped in compose) are single-project. Logflare has a `LOGFLARE_SINGLE_TENANT` flag (compose sets it true).
- **Official compose stack today (self-hosted release 0.8.2, 2026-09-23):** 11 base containers (studio, envoy, auth, rest, realtime, storage, imgproxy, meta, functions, db, supavisor) + optional logs overlay (analytics/Logflare + Vector). Postgres 17 default; Envoy replaced Kong in 0.8.0. Supabase says self-hosting is "community-supported".
- **Licensing is permissive across the board** (Apache-2.0 / MIT / PostgreSQL License). Nothing found that restricts a commercial hosting product. Not verified: trademark terms, and licenses of individual Postgres extensions bundled in the image.
- **Direction signals (Oct 2026 Select + Turso):** Supabase is investing in Multigres (HA/pooling, private alpha on platform), OrioleDB (public beta), Pipelines (ETL, managed only), Compute (private alpha), and acquired Turso (2 Oct 2026) for "millions of databases" via SQLite. None of these is a self-hosted multi-tenant story.
- **Community K8s Operator** (`supabase-community/supabase-kubernetes`, Apache-2.0, alpha API `core.supabase.io/v1alpha1`, Supabase self-hosting staff contributing) has `Project` / `SingleDatabase` CRDs and just added Supavisor support (2026-10-03). Closest existing "many projects on one cluster" tool; not officially supported.

---

## 1. Supabase Select 2026 recap and linked posts

Recap: https://supabase.com/blog/supabase-select-2026-recap (2 Oct 2026). Detail posts: build-anything, operate-with-confidence, scale-without-limits (all 2 Oct 2026). Verified by fetching all four. (The recap's WebFetch is a summarized view; details below cross-checked against the three detail posts and the docs/GitHub pages noted.)

### Build anything (https://supabase.com/blog/select-2026-build-anything)

| Announcement | Status | Facts |
|---|---|---|
| **Local development without Docker** | Alpha, off by default | "Local Supabase can now run as native processes, so it starts on machines without a Docker daemon" (Claude Code sandbox, Codex, Perplexity Computer, CI). Enable with `[experimental] stack = true` in `supabase/config.toml` or `SUPABASE_EXPERIMENTAL_STACK=1`. "One per directory" multiple instances. "On a machine with Docker, nothing changes." |
| **Declarative Schemas 2.0** | New projects default | SQL files are source of truth; `pg-delta` (CLI's diff engine) generates migrations. `supabase db schema declarative sync` then `supabase db push`. `[experimental.pgdelta] enabled = true` for existing projects. `supabase config pull` syncs project settings. pg-delta is Postgres-licensed (`supabase/pg-delta`). |
| **Supabase Compute** | Private alpha (waitlist) | Run web services/agents in any language, no duration limits, full Linux env; deploy via CLI or Management API; service defs in `config.toml`. Managed only. https://supabase.com/compute |
| **Your app's MCP server** | Available | `npx shadcn@latest add @supabase/mcp-server`; needs asymmetric signing keys + Auth OAuth server; built on `@supabase/server`. |

### Operate with confidence (https://supabase.com/blog/select-2026-operate-with-confidence)

| Announcement | Status | Facts |
|---|---|---|
| **Pipelines (ETL)** | Public alpha, Pro/Team/Enterprise | Postgres logical replication to BigQuery, ClickHouse, DuckLake, Snowflake. Powered by open-source "Supabase ETL" (`supabase/etl`, Apache-2.0). Managed infrastructure runs in AWS eu-central-1 (docs). Self-hosted: official self-hosting page lists ETL as unavailable. Docs: https://supabase.com/docs/guides/database/replication/pipelines |
| **Database Connections** monitor | GA, "on by default for every project, including self-hosted" | Sessions, long-running queries, locks; cancel/terminate from dashboard. Only self-hosted-relevant item in the whole recap. |
| Health Check Advisors | GA | Flags elevated error rates in Data API/Auth/Storage/Edge Functions. |
| Agent prompts, `query_logs` MCP tool, MCP elicitations, scoped PATs, enterprise-managed MCP auth (Okta) | GA / default | Platform features. |
| Explorer + Notebooks | Rollout 12 Oct | `supabase notebooks pull|push`. |
| Unified Logs (separate post 16 Jul 2026) | Open beta | Aggregates logs across gateway/Postgres/Auth/Storage/PostgREST/Realtime/poolers in dashboard; says nothing about Logflare or self-hosting (https://supabase.com/blog/unified-logs-open-beta). |

### Scale without limits (https://supabase.com/blog/select-2026-scale-without-limits)

- **Multigres**: "open source operating system for Postgres" by the Vitess team. Multi-node HA, "three Postgres nodes across availability zones within one region", sub-second/seconds replica promotion, committed writes preserved, built-in connection pooling (no separate pooler), works with Auth/Storage/Edge Functions, no app code changes. **Private alpha on Supabase** (invite). OSS repo `multigres/multigres` (Apache-2.0), v0.1.0 released 2026-05-30; operator repo `multigres/multigres-operator`; runs on Kubernetes ("requires a Kubernetes cluster along with a location for backups"). v0.1 post (4 Jun 2026): "not yet ready for production workloads", "open-source-only release. Multigres for Supabase is coming soon". Components named: Multigateway, Multipooler (post); repo also has `Dockerfile.pgctld`. https://supabase.com/blog/multigres-v0-1-alpha
- **OrioleDB**: public beta; storage engine replacing heap with undo log, no VACUUM/bloat, 64-bit xids, "up to 1.8x higher throughput than Postgres heap" (TPC-C-derived, Supabase's claim); chosen at project creation; per-table `USING orioledb`/`USING heap`. Docs: must be selected at project creation, cannot be added/removed later. Self-hosting not addressed in docs. In the `supabase/postgres` repo there is a `Dockerfile-orioledb-17` and release `17.11.0.004-orioledb`, so an OrioleDB image exists. CLI 2.119: OrioleDB config "graduates from experimental" (`oriole`).
- **dbarena**: public benchmark site (Supabase, RDS, Cloud SQL). No Postgres version numbers or Supavisor mention in the post.
- No licensing, pricing, or Postgres-version-support announcement appears in any of the four posts.

### Other recent official posts relevant to direction (list from https://supabase.com/blog)

- **Supabase is acquiring Turso** (2 Oct 2026): "Supabase is already launching over one million databases per week"; Turso "a single server can manage millions of databases, loading them when needed and suspending them when they're not"; Turso databases run "on Turso Cloud or in customers' own clouds"; Supabase keeps Postgres, Turso keeps SQLite. https://supabase.com/blog/supabase-is-acquiring-turso
- Branching without Git is default for all projects (4 May 2026): "Your branch gets its own Postgres instance with your current production schema" (a branch is a separate instance). https://supabase.com/blog/branching-without-git-is-now-the-default
- Supabase Series F (4 Jun 2026) and `@supabase/server` (6 May 2026) exist; not fetched.

### The dockerless CLI in depth

Primary sources: https://supabase.com/docs/guides/local-development/docker-and-native-runtimes (fetched; summarized), CLI repo `apps/cli/docs/stack-commands.md`, `packages/stack/README.md` + `ARCHITECTURE.md`, and https://github.com/supabase/slim-services (README, HOST_NATIVE_ARTIFACTS.md).

- **What it is:** a new experimental local-stack runtime, `supabase stack ...` (alias for `start/stop/status` when `experimental.stack = true`). Runtimes: Docker, Podman, native. `--runtime auto` picks Docker if its daemon answers, else Podman, else native (Linux x64/arm64, macOS arm64). The selected runtime is saved per stack; the CLI does not move data between runtimes.
- **Commands (v2.118+):** `stack start | stop | restart | status [--env] | list | logs [-f] | prepare [--capability X] | destroy`. `supabase start --runtime native`, `stack prepare --runtime native`, `--eager`, `--preparation on-demand`.
- **How it runs services:** each service is a host process. Artifacts are `tar.zst` archives downloaded from `supabase/slim-services` GitHub releases (fallback `supabase-cli-artifacts.s3.us-east-1.amazonaws.com`), checksum-verified, cached in `~/.supabase/cache/stack/` (override `$SUPABASE_HOME`), project data in `~/.supabase/stacks/<id>/`. Archives are relocatable rootfs trees with a matched loader+glibc (Postgres, PostgREST, BEAM services, Node services) or static binaries (Auth) - no Nix store paths at runtime. Process wiring is done by the CLI's own TypeScript orchestrator (`packages/stack`: Orchestrator, Owner, HostProcess, Proxy/HttpProxy, Ports, State); slim-services docs call it "process-compose wiring".
- **Services packaged (slim-services/services/):** analytics (Logflare), auth, edge-runtime, imgproxy, mailpit, pgmeta, pooler (Supavisor), postgres, postgrest, realtime, storage, studio, vector. No Kong/Envoy: a built-in Node HTTP proxy (`HttpProxy.ts`) routes by prefix and rewrites `sb_publishable_*`/`sb_secret_*` keys to JWTs on a shared API listener (this also serves Studio's MCP at `<API_URL>/mcp`).
- **Versions in artifacts (slim-services README, snapshot at read time):** Postgres 17 `17.6.1.175` (newer releases `17.11.0.004`, `15.19.0.004`, `17.11.0.004-orioledb` published 2026-10-06), PostgREST v16.4 (as in README table; compose pins v14.17, so treat README table as lagging), Auth v2.197.0, Realtime v2.138.1, Storage v1.79.x, Edge Runtime v1.77.0 (no-AI build), Studio 2026.09.21, Logflare v1.50.15, pg-meta v0.99.0/v0.100.0, Supavisor v2.9.13. Release scheme `<upstream>-r<N>`.
- **Lazy lifecycle:** services start on first request and idle-stop after 60 s (Studio 5 min; Functions never; database always eager). Database snapshots (`saveSnapshot`/`restoreSnapshot`) with a shared cache.
- **Postgres in native mode:** listens on a Unix socket only; stack writes its own HBA: `supabase_admin` trusted, every other role `scram-sha-256`. Refuses uid 0 (set `SUPABASE_NATIVE_POSTGRES_USER`). Launcher `bin/supabase-postgres-start` does initdb + bundled migrations. Ships `pg_dump`, `psql`, `pg_prove`.
- **Supavisor tenancy hook:** artifact `bin/provision-tenant` reads `POSTGRES_HOST/PORT/PASSWORD`, `TENANT_ID`, `POOL_MODE`, `DEFAULT_POOL_SIZE`, `MAX_CLIENT_CONN` and idempotently creates/updates a tenant. Pooler needs `DATABASE_URL` and a 32-char `VAULT_ENC_KEY`.
- **OS support:** Linux amd64/arm64 (Ubuntu 22.04 / glibc 2.35+; Debian 13+, Fedora 40+); macOS 14+ arm64. NOT Windows, NOT macOS Intel (docs table).
- **Install:** the CLI itself (`npm i supabase`, brew, etc.; v2.120.0 released 2026-10-06). Source: https://github.com/supabase/cli (the CLI is mid-migration from Go to a TypeScript shell: `apps/cli`, `packages/stack`, `packages/config`; Go binary still shipped as `supabase-go`).
- **Intended use:** local development, CI, sandboxed agents. Self-hosting docs: "That stack is not a self-hosted deployment: it is not hardened for production and must not be exposed to external traffic." Native public listeners bind loopback. "Multiple native projects on one machine share host resources without container isolation."
- **Density goal (slim-services README):** working hypothesis is "~25 [local] stacks on a 32 GB laptop"; the repo explicitly says its numbers "do not establish complete Dockerless CLI-stack behavior or a 25-parallel-stack capacity result".
- **Idle RSS (slim-services README, host-native table, darwin-arm64 values; treat as order-of-magnitude for Linux):** Postgres 97.6 MiB, PostgREST 56.0, Auth 29.9, Realtime 211.2, Storage 284.6, Edge Runtime 58.0, Studio 323.6, Analytics 503.2, pg-meta 165.4, Pooler 203.7. Archive sizes: Postgres 102 MiB, Studio 75, Storage 36, Edge 40, etc. Slim set 529.6 MiB vs 2036.6 MiB upstream for the Linux-arm64 image set (74% smaller).

Not verified: exact Linux idle memory; whether native mode supports non-loopback binding for server use (README says loopback); whether `@supabase/stack` will ever be published (it is `"private": true`, version 0.1.0; `npm view @supabase/stack` returns 404).

---

## 2. Official self-hosting docs

Sources: https://supabase.com/docs/guides/self-hosting (read as raw markdown), https://supabase.com/docs/guides/self-hosting/docker, https://supabase.com/docs/guides/self-hosting/self-hosted-envoy, https://supabase.com/docs/guides/self-hosting/self-hosted-proxy-https.

### What Supabase itself says

- "Self-hosted Supabase runs as a single project, which means that Studio doesn't support multiple organizations or projects. Most settings are configured through environment variables."
- Unavailable when self-hosting: "branching, advanced metrics beyond logs, managed backups and PITR, analytics and vector buckets, ETL, and the Management API."
- "You are responsible for": provisioning, security hardening, config, DB maintenance, **HA and scalability**, backups/DR, monitoring.
- "Self-hosted Supabase **does not phone home or collect any telemetry**." (CLI does collect telemetry.)
- "Self-hosted Supabase is community-supported." Official paths: Docker Compose. Community: Kubernetes (Operator or Helm), observability project.
- **Enterprise statement (full text of the only one):** "If you're an enterprise using self-hosted Supabase, we'd love to hear from you. Reach out to our Growth Team [https://forms.supabase.com/enterprise] to discuss your use case, share feedback, or explore design partnership opportunities."
- Searches for "supabase byoc", "bring your own cloud", "enterprise self hosted", "private cloud" (web search, extended) found **no Supabase-run BYOC / self-hosted-platform product**. Third parties (Vela, Northflank, Pigsty) market BYOC/enterprise self-host. https://supabase.com/solutions/enterprise has only a customer quote ("open-source scalable back-end ... that we can self-host") and no BYOC/VPC page content. `/docs/guides/platform` I did not find a BYOC page (not exhaustively crawled).
- Supabase staff (self-hosting team member "aantti") in https://github.com/orgs/supabase/discussions/39820: "can't promise immediate fixes", "I've just started some experimental lightweight process for CHANGELOG and image version updates"; maintainer: treat releases as "building blocks toward self-hosted successful outcome". No commitments to K8s templates/backups/replication. (Fetched; summarized.)
- Long-standing multi-project discussion https://github.com/orgs/supabase/discussions/4907: Studio README says managing projects is "out of scope"; community workarounds: `supa-manager` (HarryET), patched Studio, separate VPS per project, K8s, schemas in one DB. No official solution as of the thread (summarized fetch).

### Official docker-compose stack (supabase/supabase `docker/`, self-hosted release v0.8.2, 2026-09-23)

Verified by reading `docker/docker-compose.yml` (587 lines), `.env.example`, `README.md`, `versions.md`, `CHANGELOG.md`, `upgrades.json`.

Repo layout `docker/`: `docker-compose.yml`, overrides `docker-compose.{caddy,envoy(no-op shim),kong,logs,nginx,pg15,pg17,pgbouncer,rustfs,s3}.yml`, `.env.example`, `CONFIG.md`, `README.md`, `CHANGELOG.md`, `versions.md`, `upgrades.json`, `run.sh`, `setup.sh`, `update.sh`, `reset.sh`, `utils/` (`generate-keys.sh`, `add-new-auth-keys.sh`, `rotate-new-api-keys.sh`, `db-passwd.sh`, `reassign-owner.sh`, `upgrade-pg17.sh`), `volumes/{api,db,functions,logs,pooler,proxy,snippets,storage}`, `dev/`, `tests/`. Base `COMPOSE_FILE=docker-compose.yml`. Upcoming removals: kong, MinIO s3, pg15 overrides.

Containers and pinned images (as of 2026-09-09 snapshot, v0.8.1/0.8.2):

| Service | container_name | Image |
|---|---|---|
| studio | supabase-studio | `supabase/studio:2026.09.07-sha-7996410` |
| api-gw (Envoy, default since 0.8.0; aliases `envoy`, `kong`) | supabase-envoy | `envoyproxy/envoy:v1.39.1` (Kong override: `kong/kong:3.9.3`) |
| auth | supabase-auth | `supabase/gotrue:v2.196.0` |
| rest | supabase-rest | `postgrest/postgrest:v14.17` |
| realtime | `realtime-dev.supabase-realtime` (name encodes tenant id; "realtime constructs tenant id by parsing the subdomain") | `supabase/realtime:v2.134.10` |
| storage | supabase-storage | `supabase/storage-api:v1.74.0` |
| imgproxy | supabase-imgproxy | `darthsim/imgproxy:v3.31.4` |
| meta | supabase-meta | `supabase/postgres-meta:v0.99.0` |
| functions | supabase-edge-functions | `supabase/edge-runtime:v1.76.2` |
| db | supabase-db | `supabase/postgres:17.6.1.136` (PG17 default since 0.6.0, 2026-06-17; `docker-compose.pg15.yml` pins PG15) |
| supavisor | supabase-pooler | `supabase/supavisor:2.9.12` |
| *(logs overlay)* analytics | supabase-analytics | `supabase/logflare:1.50.10` |
| *(logs overlay)* vector | supabase-vector | `timberio/vector:0.53.0-alpine` |
| *(optional)* rustfs / minio / pgbouncer | | `rustfs/rustfs:v1.0.0-rc.5`; `cgr.dev/chainguard/minio`; `edoburu/pgbouncer:v1.25.2-p0` |

Discrepancy to note: the docs/README list Logflare and Vector as part of the stack, but in the current repo they are only in `docker-compose.logs.yml` (not in the base compose or default `COMPOSE_FILE`). I read the base compose and found no analytics/vector service.

Config details useful for design:
- Gateway: Envoy static files `volumes/api/envoy/{envoy.yaml,cds.yaml,lds.template.yaml,docker-entrypoint.sh}`; Lua filters enforce API keys and translate opaque `sb_publishable_*`/`sb_secret_*` keys into ES256 JWTs; dashboard basic auth on `/`; **no hot reload, plain HTTP only** (TLS via Caddy/nginx in front).
- Postgres init via mounted SQL: `volumes/db/{realtime,webhooks,roles,jwt,_supabase,logs,pooler}.sql` into `/docker-entrypoint-initdb.d`; database `_supabase` holds Supavisor/Logflare config (schemas `_analytics`, `_realtime`).
- Env keys (`.env.example`): `POSTGRES_PASSWORD`, `JWT_SECRET`, `ANON_KEY`, `SERVICE_ROLE_KEY`, `SUPABASE_PUBLISHABLE_KEY`, `SUPABASE_SECRET_KEY`, `JWT_KEYS`, `JWT_JWKS`, `SECRET_KEY_BASE`, `REALTIME_DB_ENC_KEY`, `VAULT_ENC_KEY`, `PG_META_CRYPTO_KEY`, `LOGFLARE_*_ACCESS_TOKEN`, `SUPABASE_PUBLIC_URL`, `API_EXTERNAL_URL`, `SITE_URL`, `POSTGRES_HOST/DB/PORT`, `POOLER_TENANT_ID`, `POOLER_DEFAULT_POOL_SIZE`, `POOLER_MAX_CLIENT_CONN`, `STORAGE_TENANT_ID`, `REGION`, `STUDIO_DEFAULT_ORGANIZATION/PROJECT`, `PGRST_DB_SCHEMAS`, `FUNCTIONS_VERIFY_JWT`, `API_GW_HTTP_PORT`, SMTP/auth toggles. `POOLER_TENANT_ID`, `STORAGE_TENANT_ID` default to stubs - the services are already tenant-keyed internally.
- Requirements (docs): 4 GB RAM min (8 GB+ recommended), 2 cores min (4+), 40 GB SSD min. Secrets via `utils/generate-keys.sh` + `add-new-auth-keys.sh` (asymmetric JWT keypair). Updates via `update.sh` (3-way merge, `upgrades.json` gates breaking releases), `run.sh` helper.
- Storage backend: local file by default; S3-compatible (AWS S3, RustFS, MinIO, R2) supported; S3 protocol endpoint.
- Edge Functions in compose: one `edge-runtime` with a user "main" worker (`volumes/functions/main/index.ts`) routing to `volumes/functions/<name>/`; functions are files on disk (self-hosted-functions doc). Not fetched in detail.
- Known limitations (official + observed): single project; Studio has no project mgmt; no Management API; no PITR/backups tooling in-box; no HA; Envoy needs restart to reload; `app.settings.jwt_secret` removed from DB init in 0.8.2 (breaking-ish); external Postgres support is a recurring complaint (discussion 39820); compose image tags lag the release pipeline (compose PG `17.6.1.136` vs published `17.11.0.004`).

Community K8s: https://github.com/supabase-community/supabase-kubernetes (Apache-2.0, 822 stars, last push 2026-10-04): Helm chart `supabase` plus **Operator** (`core.supabase.io/v1alpha1`: `Project`, `SingleDatabase`, `Function`, `Migration`, `Auth`, `Rest`, `Meta`, `Realtime`, `Supavisor`, `Storage`, `Studio`, `Envoy`, `EdgeRuntime`), kubebuilder, charts `supabase-operator`, `supabase-project`, `supabase`. "Early stage ... API may change"; "not officially supported by Supabase". `Project` is namespace-scoped. Contributors include `aantti`.

---

## 3. supabase/supabase `docker/` and CLI repo state

- `docker/` details are in section 2. Tags: GitHub releases named `self-hosted/vX.Y.Z` (latest 0.8.2). `upgrades.json` encodes breaking steps: 0.6.0 (PG17 default, `utils/upgrade-pg17.sh`), 0.7.0 (`API_EXTERNAL_URL` now includes `/auth/v1`; anon access to OpenAPI removed), 0.8.0 (Envoy default, `kong` service renamed `api-gw`).
- **supabase/cli** (https://github.com/supabase/cli): latest **v2.120.0 (2026-10-06)**; v2.119.0 (2026-09-30). ~47 stable releases since April 2026 (very high cadence, with many beta tags). GitHub reports no license file; README says "Supabase CLI packages are released under the MIT license" (flag: no LICENSE detected by GitHub API).
- Timeline of relevant CLI changes (from release notes): pg-delta introduced (alpha, v2.90 era) and made the only engine by v2.117; v2.99 internal stack work (process-compose, edge-runtime in stack); v2.107 HA project support, pg-delta default; v2.114 "managed stack persistence"; v2.117 `SUPABASE_USE_SLIM_IMAGES=true` (smaller GHCR images, `ghcr.io/supabase/cli/...`), `config diff/pull`; **v2.118 experimental `stack` commands**, `compute new/build`, `SUPABASE_EXPERIMENTAL_STACK`; v2.119 Studio MCP at `/mcp`, OrioleDB config stable, Docker-preferred-over-Podman; v2.120 native-backend port reservation, stack id prefixes. Postgres versions referenced: 17.6.1.132 -> 17.6.1.156 in CLI Docker defaults; slim-services publishes 15.19.0.004 and 17.11.0.004.
- `supabase services` in stack mode lists canonical `ghcr.io/supabase/cli/...` image names from the CLI artifact catalog; PostgreSQL major 15 or 17 selectable.

---

## 4. How the hosted platform is architected

Sources: https://supabase.com/docs/guides/getting-started/architecture (raw md), https://supabase.com/docs/guides/platform/compute-and-disk (raw md), https://supabase.com/docs/guides/database/connecting-to-postgres/pooling-and-limits (summarized), https://supabase.com/blog/supavisor-1-million (search result only), and the `supabase/postgres` repo (cloned; commit date 2026-10-06).

**Explicitly documented**
- "Every project on the Supabase Platform comes with its own dedicated Postgres instance." "All Postgres databases on Supabase run in isolated environments." Compute sizes Nano/Micro-Medium shared CPU; Large (2 vCPU) and up dedicated vCPUs. Disk gp3 or io2; compute size dictates disk IOPS/throughput ceilings (e.g., Micro 1 GB RAM, 500 baseline IOPS).
- Architecture page: "Each Supabase project consists of several tools": Envoy API gateway in front of GoTrue, PostgREST, Realtime, Storage, pg_meta, Functions, pg_graphql, all talking to "a single Postgres instance"; Supavisor is "a cloud-native, multi-tenant Postgres connection pooler". (Envoy is shown as the gateway there; the AMI still ships both Envoy 1.28.0 and Kong 2.8.1.)
- **Two poolers:** Supavisor = shared, multi-tenant, "runs on separate servers from the database", IPv4-only; PgBouncer = dedicated, "runs alongside your Postgres instance", paid plans, IPv6.
- Branching: each branch gets its own Postgres instance.
- Scale claim (Turso post): >1,000,000 databases launched per week.

**What the project VM image contains** (supabase/postgres repo, `ansible/playbook.yml`, `ansible/vars.yml`, `ansible/tasks/**`, built by Packer `amazon-{amd64,arm64}-nix.pkr.hcl` + `stage2-nix-psql.pkr.hcl` + `ebssurrogate/`, and a QEMU variant `qemu.pkr.hcl`):
- Ubuntu base; **Postgres built via Nix flake** (`nix/`, extensions in `nix/ext/`), flavors `15`, `17`, `orioledb-17` (`postgres_release: 15.19.0.004 / 17.11.0.004 / 17.11.0.004-orioledb`); stage1 installs services via Ansible, stage2 installs the Nix-built Postgres and runs first-boot optimizations (`database-optimizations.service`, generated tuning into `/etc/postgresql-custom/generated-optimizations.conf`).
- Services in the VM: **PgBouncer**, **GoTrue 2.190.0**, **PostgREST 14.18**, **Envoy 1.28.0** and legacy **Kong 2.8.1** (`setup-envoy.yml`, `setup-kong.yml`), **WAL-G** and **pgBackRest** (backups/PITR), **Vector 0.48.0-1** (logs), **postgres_exporter 0.15.0**, **fail2ban**, **nftables** (`internal/setup-nftables.yml`), `pg_egress_collect`, coredump processing, `tuned`, KVM timesync, AWS CLI, Salt minion 3007.14.
- Closed management binaries downloaded as tarballs from `https://supabase-public-artifacts-bucket.s3.amazonaws.com/...` with pinned sha256: **`supabase-admin-api` (adminapi) 0.122.1**, **`admin-mgr` 0.44.5**, **`supabase-admin-agent` 1.14.4**. `adminapi` runs as a user in groups `postgres, pgbouncer, postgrest, gotrue, envoy, kong, vector, wal-g, root, systemd-journal` with a sudoers file; ships helper scripts: `grow_fs.sh`, `manage_readonly_mode.sh`, `mount-volume.sh`, `unmount-volume.sh`, `pg_egress_collect.pl`, and **`pg_upgrade_scripts/{check,common,complete,initiate,prepare,pgsodium_getkey}.sh`** (this is how major-version upgrades are driven). I found no public source repos for the three binaries (`gh repo view` for plausible names returned not found) - flag: could exist under other names.
- Postgres config surface: `ansible/files/postgresql_config/` (`postgresql.conf.j2`, `pg_hba.conf.j2`, `supautils.conf.j2`, jsonlog/csvlog, `custom_walg.conf`, `custom_read_replica.conf`, AppArmor profile `sbpostgres_apparmor`) and `postgresql_extension_custom_scripts/` per-extension hooks.
- **supautils** config (`supautils.conf.j2`): `privileged_role = supabase_privileged_role` (so tenants get a non-superuser "postgres" that can still create privileged extensions), `privileged_extensions` (long allowlist, ~80 extensions), `reserved_roles`/`reserved_memberships` (supabase_admin, supabase_auth_admin, supabase_storage_admin, supabase_realtime_admin, supabase_replication_admin, supabase_etl_admin, dashboard_user, pgbouncer, authenticator, anon/authenticated/service_role), `policy_grants`/`drop_trigger_grants` for auth/storage/realtime tables, `privileged_role_allowed_configs`. This is the mechanism that gives customers a "postgres" role without superuser.
- Extension set: ~80 (postgis, pgvector, pg_cron, pgsodium, supabase_vault, pg_graphql, pg_net, pgmq, pgaudit, timescaledb (PG15 only), plv8, wrappers, index_advisor, ...).
- Containerization/orchestration hints: `Dockerfile-15/17/orioledb-17`, `Dockerfile-kubernetes` (QEMU + qcow2 image in a container with `virtiofsd`, sleeps forever), `qemu_artifact.md` (VM image shipped inside a container image "to the nodes where they're needed", "use ... with e.g. KubeVirt", iterate on "ubuntu bare-metal node that's part of the EKS cluster"), `Dockerfile-multigres` (Postgres image for Multigres with externally managed startup; pgsodium not enabled), `docs/plans/2026-02-27-pgctld-pgbackrest.md`. **Inference (not stated):** Supabase appears to run project VMs on Kubernetes/EKS via KubeVirt-style QEMU VMs in addition to plain EC2 AMIs. I could not find a Supabase statement confirming this.

**Not in the VM image (so, shared or external - inference from absence in Ansible):** Storage API, Realtime, Edge Runtime, Studio, Supavisor, Logflare, postgres-meta, imgproxy.

**How the shared services are tenant-aware (read from source in clones):**
- **Supavisor** (Elixir, Apache-2.0): tenant config in its own Postgres; dynamic per-tenant pools started on first client connect; pool process IDs distributed across a cluster; per-tenant pool mode; OpenAPI at `/api/openapi`; compose uses `POOLER_TENANT_ID`, tenant selected by the client username suffix (`user.<tenant>` - standard Supavisor behavior; not re-verified in this session). Claims: 1M connections on a cluster; 250k idle on one 16-core/64 GB node.
- **Realtime** (Elixir, Apache-2.0): `ARCHITECTURE.md`: "Realtime is a multi-tenant Phoenix app"; tenant = `external_id` taken from request host (`<external_id>.realtime.example.com`), JWT verified against the tenant's `jwt_secret`/`jwt_jwks`; per-tenant singletons `Realtime.Tenants.Connect` (DB pool, logical-replication broadcast, auto-stop when idle) and `PostgresCdcRls`; tenants and extensions live in a `_realtime` schema with `DB_ENC_KEY`-encrypted fields; per-tenant limits (`TENANT_MAX_EVENTS_PER_SECOND`, `TENANT_MAX_CONCURRENT_USERS`, `TENANT_MAX_CHANNELS_PER_CLIENT`, `TENANT_MAX_BYTES_PER_SECOND`); tenant management HTTP API signed with `API_JWT_SECRET`. Compatibility table lists PG >= 14; Multigres supported.
- **Storage** (TS/Fastify, Apache-2.0): `MULTI_TENANT=true` + `DATABASE_MULTITENANT_URL` (+ pool URL) + `SERVER_ADMIN_API_KEYS`/`SERVER_ADMIN_PORT=5001` admin API (`/tenants/...`), tenant resolved from `REQUEST_X_FORWARDED_HOST_REGEXP` (example regex `^([a-z]{20}).local.(?:com|dev)$`, i.e. a 20-char project ref), per-tenant DB credentials, pg-boss queue (`PG_QUEUE_ENABLE`), `docker-compose-multi-tenant.yml` in the repo, S3/RustFS backend. Also has HTTP/TUS/S3/Iceberg/vector routes.
- **Logflare** (Elixir, Apache-2.0): `LOGFLARE_SINGLE_TENANT` + `LOGFLARE_SUPABASE_MODE` (compose sets both true); multi-backend (`multibackend=true`; Postgres or BigQuery backend). Platform-side multi-tenant use not verified.
- **Auth/GoTrue** (Go, MIT): single database per process. README lists "Multi-tenancy via the `instances` table i.e. `GOTRUE_MULTI_INSTANCE_MODE`" under "Inherited features ... not supported by Supabase and ... may be removed without prior notice." Platform runs GoTrue inside each project VM (Ansible).
- **PostgREST** (Haskell, MIT): one instance per database (`PGRST_DB_URI`); no tenant concept found. Runs in the VM on the platform.
- **Edge Runtime** (Rust/Deno, MIT): in compose, a single instance with a "main" worker; platform-side tenancy not documented in sources I read (README grep found no tenant terms).

---

## 5. Licensing

Verified via GitHub API (`gh repo view --json licenseInfo`) on 2026-10-06 and repo LICENSE files.

| Component | Repo | License |
|---|---|---|
| Studio (and docs, `docker/`) | supabase/supabase (apps/studio) | Apache-2.0 (root LICENSE; architecture doc also says Studio "Apache 2"). Not checked: per-directory exceptions. |
| Auth (GoTrue) | supabase/auth | MIT |
| PostgREST | PostgREST/postgrest | MIT |
| Realtime | supabase/realtime | Apache-2.0 |
| Storage API | supabase/storage | Apache-2.0 |
| Edge Runtime | supabase/edge-runtime | MIT |
| postgres-meta | supabase/postgres-meta | Apache-2.0 |
| Supavisor | supabase/supavisor | Apache-2.0 |
| Logflare | Logflare/logflare | Apache-2.0 |
| supabase/postgres (build system, migrations) | supabase/postgres | PostgreSQL License |
| supautils | supabase/supautils | Apache-2.0 |
| pg_graphql, pg_net, pg_jsonschema, wrappers | supabase/* | Apache-2.0 |
| index_advisor, pg-delta, pg-toolbelt | supabase/* | PostgreSQL License |
| supabase_vault | supabase/vault | GitHub reports "other" (not inspected) |
| Supabase ETL | supabase/etl | Apache-2.0 |
| CLI | supabase/cli | README says MIT; GitHub detects no license file |
| slim-services (native artifacts) | supabase/slim-services | MIT (bundled third-party notices in `THIRD_PARTY_NOTICES.md`) |
| Multigres, multigres-operator | multigres/multigres | Apache-2.0 |
| OrioleDB | orioledb/orioledb | Apache-2.0 (Postgres patches follow PostgreSQL License - not verified) |
| Envoy | envoyproxy/envoy | Apache-2.0 |
| Community K8s operator/charts | supabase-community/supabase-kubernetes | Apache-2.0 |

Nothing in these licenses restricts building a commercial multi-tenant hosting product (no AGPL/SSPL/BSL/Elastic-style clauses found in the core components). Caveats I could not verify: (a) **trademark/brand** terms for the "Supabase" name; (b) licenses of bundled Postgres extensions (e.g. PostGIS GPL, TimescaleDB edition in the PG15 image, plv8, pgroonga) - audit per extension before offering them as a hosted service; (c) the closed binaries (`adminapi`, `admin-mgr`, `supabase-admin-agent`) have no published license and can't be redistributed by assumption (they are served from a public bucket, but no license grant seen); (d) Management API is a proprietary service (its OpenAPI client lives in `supabase/cli/packages/api`).

---

## 6. Gaps / unverified items

- Whether Supabase's hosted control plane places project VMs on EC2 only, or on K8s/KubeVirt too (only circumstantial evidence from `qemu_artifact.md`, `Dockerfile-kubernetes`).
- Where Storage/Realtime/Edge Runtime/Logflare physically run on the hosted platform and how many tenants per node; no public statement found.
- Whether Supabase will ship an official multi-project/BYOC self-host offering: no statement; "design partnership" form only.
- Supavisor tenant-addressing format in 2.9.x (username suffix) - inherited from prior knowledge/compose `POOLER_TENANT_ID`, not re-read from docs this session.
- Linux idle-memory figures for native services (only darwin-arm64 host numbers captured).
- Docs pages I read via WebFetch (docker-and-native-runtimes, pooling-and-limits, Pipelines, OrioleDB, Multigres post, Unified Logs, Turso, Branching, discussions) are summarized by the fetch tool; where the claim matters I cross-checked with raw markdown or the GitHub repos (noted above).
- `https://supabase.com/docs/guides/platform` was not crawled for BYOC mentions beyond search results.

---

## 7. Implications for a multi-tenant self-hosted design (MY ANALYSIS, not Supabase statements)

1. **Plan to build the control plane yourself.** Supabase's own platform control plane (project provisioning, `adminapi`, `admin-mgr`, `supabase-admin-agent`, Management API, billing) is closed or unpublished. What is open and reusable: the *data plane* parts (Postgres VM build, supautils, service repos) and the public Management API OpenAPI schema (in the CLI) if you want Dashboard/CLI compatibility. The community `supa-manager` and the K8s operator (`Project` CRD) are prior art, not endorsed.
2. **Choose the isolation unit per service, following the platform's own split.** Evidence suggests: per-project = Postgres (+ PgBouncer), PostgREST, GoTrue (and gateway) ; shared multi-tenant = Supavisor, Realtime, Storage (host-based tenant routing + admin API), likely Logflare and Edge Runtime. A design that mirrors this keeps per-tenant idle cost near "Postgres + PostgREST + Auth" (roughly 100 + 56 + 30 = ~185 MiB RSS using slim-services' darwin-arm64 idle numbers; Linux container numbers were lower for PostgREST/Auth), versus ~1.9 GB if every tenant ran the full 10-service stack natively (sum of the table above). Treat those as rough planning numbers only.
3. **Native artifacts are a credible "as native as possible" substrate.** `supabase/slim-services` release archives (MIT, relocatable, `linux-amd64/arm64`, glibc floor 2.35, checksummed, SBOM per archive, bundled loader for most services) can be run under systemd (per-tenant instances) with no container runtime on AWS EC2 Ubuntu 22.04+/Debian 13. Caveats: published for local/CI use, entrypoints are launchers (`bin/prepare`, `bin/supabase-postgres-start`, `bin/storage`, `bin/provision-tenant`) designed to be driven by the CLI's orchestrator, defaults are local-dev (loopback, trust for `supabase_admin`), and Supabase makes no production support promise. You can reuse the artifact catalog and launcher contract while supplying your own orchestrator (systemd/Nomad/K8s) instead of `@supabase/stack`.
4. **`@supabase/stack` ideas worth copying:** per-stack identity + state dir, sticky port allocation, lazy start on first HTTP/TCP request and idle sleep (60 s) behind a proxy, snapshot-based fast clone of a database, and the "provision-tenant" idempotent hook. These map directly to scale-to-zero multi-tenancy (cheap idle projects), consistent with Supabase's own Turso-driven thesis of "millions of databases ... loading them when needed and suspending them".
5. **Postgres choice:** use `supabase/postgres` images or the Nix flake outputs (PG15/17/orioledb-17) so supautils, extensions, role model (`supabase_admin`, `postgres` as non-superuser privileged role) and migrations (`migrations/db/init-scripts`) match what Auth/Storage/Realtime/Studio expect. The VM build (Ansible + Packer) is documented in-repo, and `ansible/vars.yml` shows exactly which versions the platform pins (GoTrue 2.190.0, PostgREST 14.18, Envoy 1.28.0, wal-g/pgBackRest for backups/PITR).
6. **Gateway:** the self-hosted Envoy config (static CDS/LDS + Lua key translation, no hot reload) is single-project and needs rework for N tenants (dynamic xDS or per-tenant hostnames). Storage and Realtime already expect host-based tenant resolution (`<ref>.domain`), so wildcard DNS + host routing is the natural fit.
7. **Auth is the hardest shared-service question:** upstream says multi-instance mode may be removed; assume one GoTrue process per tenant (cheap: static Go binary, ~10-30 MiB), possibly lazily started.
8. **Operations gaps to fill (Supabase says they are yours):** HA, backups/PITR (use wal-g or pgBackRest as the platform does), upgrades (study `pg_upgrade_scripts/`), observability (community `supabase-observability`; Logflare is optional), monitoring.
9. **Watch list:** Multigres (Apache-2.0, K8s operator; "Multigres for Supabase is coming soon") for HA + pooling; OrioleDB (beta); the CLI `stack` packaging maturing; and the enterprise "design partnership" channel if a commercial relationship (or clarity on a supported multi-tenant path) matters. The risk is Supabase's direction is managed-platform-first; self-hosting parity features (branching, Management API, ETL, Analytics) are explicitly excluded.
10. **Legal checks before launch:** trademark use of "Supabase", per-extension licensing, and the closed admin binaries (do not depend on them).

---

## 8. Sources

Blog and recap
- https://supabase.com/blog/supabase-select-2026-recap
- https://supabase.com/blog/select-2026-build-anything
- https://supabase.com/blog/select-2026-operate-with-confidence
- https://supabase.com/blog/select-2026-scale-without-limits
- https://supabase.com/blog/supabase-is-acquiring-turso (read raw md)
- https://supabase.com/blog/multigres-v0-1-alpha
- https://supabase.com/blog/branching-without-git-is-now-the-default
- https://supabase.com/blog/unified-logs-open-beta
- https://supabase.com/blog (listing)
- https://supabase.com/blog/supavisor-1-million (search result only)

Docs
- https://supabase.com/docs/guides/self-hosting (raw md)
- https://supabase.com/docs/guides/self-hosting/docker
- https://supabase.com/docs/guides/self-hosting/self-hosted-envoy
- https://supabase.com/docs/guides/self-hosting/self-hosted-proxy-https
- https://supabase.com/docs/guides/local-development/cli/getting-started
- https://supabase.com/docs/guides/local-development/docker-and-native-runtimes
- https://supabase.com/docs/guides/getting-started/architecture (raw md)
- https://supabase.com/docs/guides/platform/compute-and-disk (raw md)
- https://supabase.com/docs/guides/database/connecting-to-postgres/pooling-and-limits
- https://supabase.com/docs/guides/database/replication/pipelines
- https://supabase.com/docs/guides/database/orioledb
- https://supabase.com/docs/guides/platform/migrating-within-supabase/dashboard-restore
- https://supabase.com/solutions/enterprise
- https://forms.supabase.com/enterprise

GitHub discussions
- https://github.com/orgs/supabase/discussions/39820
- https://github.com/orgs/supabase/discussions/4907

Repos and files (read via `gh api` or local clones)
- https://github.com/supabase/supabase (docker/: docker-compose.yml, docker-compose.logs.yml, docker-compose.envoy.yml, docker-compose.pg17.yml, docker-compose.pgbouncer.yml, docker-compose.rustfs.yml, docker-compose.s3.yml, .env.example, README.md, CHANGELOG.md, versions.md, upgrades.json; apps/docs/content/guides/self-hosting/)
- https://github.com/supabase/cli (apps/cli/docs/stack-commands.md, packages/stack/README.md, packages/stack/ARCHITECTURE.md, packages/stack/src/**, release notes v2.90-v2.120)
- https://github.com/supabase/slim-services (README.md, HOST_NATIVE_ARTIFACTS.md, releases)
- https://github.com/supabase/postgres (README.md, ansible/**, qemu_artifact.md, Dockerfile-kubernetes, Dockerfile-multigres, docs/multigres-image.md, flake.nix, nix/)
- https://github.com/supabase/auth (README.md)
- https://github.com/supabase/realtime (README.md, ARCHITECTURE.md, ENVS.md)
- https://github.com/supabase/storage (docker-compose-multi-tenant.yml, .env.sample, src/config.ts)
- https://github.com/supabase/supavisor (README.md)
- https://github.com/Logflare/logflare (config/runtime.exs)
- https://github.com/supabase/edge-runtime, https://github.com/supabase/postgres-meta, https://github.com/PostgREST/postgrest, https://github.com/supabase/supautils, https://github.com/supabase/etl
- https://github.com/multigres/multigres, https://github.com/multigres/multigres-operator
- https://github.com/supabase-community/supabase-kubernetes (README.md, DEVELOPERS.md, api/v1alpha1/)
- https://github.com/orioledb/orioledb (license via API)

Search-only (not primary)
- https://pigsty.io/docs/app/supabase/, https://vela.run/supabase-alternative/, https://northflank.com/blog/supabase-alternative (third-party BYOC alternatives surfaced by search)
