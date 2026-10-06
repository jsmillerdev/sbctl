# 03 - Landscape of multi-project / multi-tenant self-hosted Supabase, and adjacent BaaS patterns

Research date: 2026-10-06. Method: live fetches of GitHub repos/READMEs/discussions/issues (via `gh api` where possible, which returns primary text rather than a summarizer's paraphrase), vendor docs, and web search. Where a fact came only from a search-result snippet or a summarized fetch, it is marked **[snippet]** or **[summarized]**. Anything I could not confirm is collected in "Unverified / gaps" at the end.

---

## TL;DR

1. **Upstream still has no multi-project mode and says so.** Supabase docs: "Self-hosted Supabase runs as a single project, which means that Studio doesn't support multiple organizations or projects." The self-hosting lead (aantti) wrote on 2026-03-31 that on the platform each project is a separate compute instance with a "fairly complex management API and a control plane on top", and that a lightweight/multi-project Studio "will require a different architecture across the entire stack". He hinted at "more lightweight architectures across a few products this year" but promised nothing.
2. **Everything in the wild is one of four shapes:** (a) a full compose stack per project behind a reverse proxy (Coolify, Dokploy, Easypanel, DO, Elestio, Multibase, SupaConsole, supabase-multitenant); (b) a K8s Helm release/operator CR per project (supabase-community operator, STRRL operator, Stack CLI); (c) one shared Postgres cluster with a database per project plus per-project Auth/PostgREST (Sbarbase); (d) schemas/RLS inside one project (not real multi-tenancy of Supabase; Auth/Storage/roles stay shared).
3. **Every community multi-project tool is young and tiny.** supabase-multitenant (GitHub org) created 2026-09-09, 24 stars; GustavoMartins123/supabase-multitenant 21 stars; Sbarbase created 2026-09-20, 2 stars, v0.2.0 "development snapshot"; STRRL operator 10 stars, v1alpha1; supa-manager (231 stars) last pushed 2024-11-20 and undocumented. None is a safe dependency; they are design evidence.
4. **Supabase's own services are unevenly multi-tenant.** Supavisor, Realtime and Storage-api are natively multi-tenant (tenant lookup by `external_id` / subdomain / `x-forwarded-host`). PostgREST and Auth (GoTrue) are single-database. Upstream issues exist but nothing is merged: PostgREST tenant-routing PR #5087 was closed on 2026-07-09 under PostgREST's no-AI policy; Auth multi-tenant proposals #2621/#2622 (July 2026, one author) are open with no maintainer reply.
5. **"One cluster, one database per tenant" is viable but only with real compromises.** Roles are cluster-global (PostgreSQL docs), the Supabase init SQL creates `anon`/`authenticated`/`service_role`/`authenticator`/`supabase_admin` once per cluster, `pg_cron` and `pg_net` are single-database per cluster, supautils GUCs and the pgsodium/Vault root key are cluster-level, and backups/PITR are whole-cluster (CloudNativePG: "no mechanism to restore individual databases"). Sbarbase's own docs concede the engine is a shared failure boundary and that it does not provide isolation from SQL-capable operators; it lists cron/pooler as unsupported in its shared workflow.
6. **The release train moves under you.** Self-hosted 0.6.0 (2026-06-17) made Postgres 17 default and moved Studio/meta off `supabase_admin`; 0.8.0 (2026-08-11) replaced Kong with Envoy as default gateway (Kong override marked for removal). The Coolify template (last commit April 2026) still pins Postgres 15.8 and Kong; Dokploy's template (Aug 2026) is on PG17 + Kong. Any design that forks the compose file inherits this churn.
7. **PaaS installers are one-stack-per-service and port-collision-prone.** Multi-instance on one host breaks on host-published Postgres/pooler ports (Coolify #5362, Dokploy templates #558). Dokploy's "isolated deployments" (network per compose) is the cleanest native answer. Coolify maintainers recommend "new stack + migrate DB" instead of in-place updates.
8. **Kubernetes:** the community chart repo now ships both a Helm chart (v0.8.0, 2026-09-05) and an operator (`core.supabase.io/v1alpha1`: `Project`, `SingleDatabase`, per-component CRDs; "early stage... API may change"). One operator-managed `SingleDatabase` = one Postgres StatefulSet per project. CNPG maintainers advise one database per cluster for multi-tenant isolation (PITR, failure domain), at a cost in baseline overhead.
9. **Best adjacent patterns:** PocketHost (thousands of instances per VPS via an in-process launcher, claimed), Fly Machines (one app per tenant + `fly-replay` router + start-on-demand), Neon (stateless compute + shared storage; self-host operator is explicitly not production-ready), Turso/Instant (isolation unit is a DB file or a row, not a server). The transferable idea: separate the *isolation unit* from the *process*, and put a thin router in front that starts things on demand.
10. **Decision-relevant bottom line:** a pragmatic native design is "shared stateless/multi-tenant fleet (Supavisor, Realtime, Storage, gateway) + per-project Auth/PostgREST + Postgres at the right granularity (cluster per project for real isolation, DB-per-project only for trusted-operator/agency use)". Details in "Design patterns extracted".

---

## 1. Community multi-project tooling and what breaks

### 1.1 The canonical discussions

| Thread | What it establishes |
|---|---|
| [Discussion #4907](https://github.com/orgs/supabase/discussions/4907) "Creating multiple organizations and/or projects for self-hosted deployments" (opened 2022-01-10, 39 top-level comments) | Original ask. 2022 answer quotes Studio's README: Studio "is not intended for managing the deployment and administration of projects". Workarounds listed over the years: patching Studio's `/api/projects` responses, one VPS per project, Kubernetes/Helm per project, supa-manager, SupaConsole, a Studio fork. **2026-03-31, aantti (Supabase self-hosting lead):** on platform "each project is a separate compute instance and there's a fairly complex management API and a control plane on top of it - quite specific to the internal platform architecture"; Studio's self-hosted code path "doesn't have an underlying API to talk to"; multi-project Studio plus "lightweight" "will require a different architecture across the entire stack... and a significant overhead in the Studio code"; "I think we'll see more lightweight architectures across a few products this year". |
| [Discussion #38048](https://github.com/orgs/supabase/discussions/38048) "Multiple Projects on Single Self-Hosted Supabase Instance" (2025-08-19 onward) | silentworks (2025-08-20): "you need a new instance per project. This is exactly how the hosted Supabase platform works." aantti (2025-11-20): "a single self-hosted Docker Compose-based Supabase deployment is one database... Usually, people just set up multiple identical deployments. If it's on the same server, then yes - you'll have to figure out port separation." Sbarbase author (2026-09-24) lists three approaches by isolation (schemas / full stack per project / one cluster + DB per project with Auth+REST per project). |
| [Discussion #1615](https://github.com/orgs/supabase/discussions/1615) "Multi-tenant" (since 2021, 60+ participants) | steve-chavez: separate DB per tenant has connection overhead; schema-per-tenant is viable; single schema + RLS is "most straightforward". kangmingtay: "We do plan to make gotrue multi-tenant eventually but we don't have a clear timeline." (still unimplemented). |
| [Discussion #39820](https://github.com/orgs/supabase/discussions/39820) "Self-hosting: What's working (and what's not)?" (from Oct 2025) | Pain points: upgrade chaos/no changelog (since partly addressed: `CHANGELOG.md`, `versions.md`), no RBAC in self-hosted Studio, missing production Helm/CloudFormation templates, external/managed Postgres (RDS) requires heavy rewiring, no HA/replication guidance. Notes modular swap-ins (Caddy for Kong, CloudNativePG for the DB). |
| [Discussion #31147](https://github.com/orgs/supabase/discussions/31147) "Support for Kubernetes Postgres Operators (CNPG)" | Working CNPG recipes since Dec 2024: skip `demote-postgres.sql`, keep `supabase_admin` via `managed.roles`, uid 26, key files via projected volumes for pgsodium/Vault, migration Job, and (Mar 2026) CNPG 1.27+/K8s 1.33+ `ImageVolume` extension mounting. |
| [Discussion #27467](https://github.com/orgs/supabase/discussions/27467) Docker Swarm | Surfaced by search only; not read. |

Official docs page ([Self-Hosting](https://supabase.com/docs/guides/self-hosting)): states the single-project limitation; lists Docker as official; Kubernetes (operator or Helm chart) as community-maintained; not available when self-hosting: branching, advanced metrics, managed backups/PITR, Management API, etc.

### 1.2 Tools people built (verified against repos)

| Project | Isolation unit | How it works | Status (as of 2026-10-06) |
|---|---|---|---|
| [supabase-multitenant/supabase-multitenant](https://github.com/supabase-multitenant/supabase-multitenant) (MIT) | Full compose stack per project (own Postgres, Auth, Kong, Storage, Realtime, Studio) | Next.js panel + control-plane Postgres; mounts `/var/run/docker.sock` to generate/launch a compose project per tenant; Traefik with Let's Encrypt, per-project custom domains; Coolify one-click | Repo created 2026-09-09, 24 stars. README has no limits on projects/host, upgrade, or backup story. Docker-socket-in-the-web-app is the weak point. |
| [GustavoMartins123/supabase-multitenant](https://github.com/GustavoMartins123/supabase-multitenant) (Apache-2.0) | Per project: own DB, JWT secret, Realtime tenant, Storage tenant, Supavisor tenant, Nginx/Auth/PostgREST | FastAPI projects API; HMAC-signed "intents" in Postgres; a host-level systemd `host-agent` does the Docker work so the API never touches the Docker socket; OpenResty/Lua dynamic gateway lets one Studio manage every project; 20-letter random public refs | 21 stars, 583 commits, "unofficial project under active development". Best control-plane security idea in the field (intent + host agent). Uses the multi-tenant Realtime/Storage/Supavisor features. |
| [M7MMAD-OMAR/sbarbase](https://github.com/M7MMAD-OMAR/sbarbase) (Apache-2.0) | Per environment: its own database in one shared PG cluster + own Auth + own PostgREST; Storage/Realtime shared or per-env | See section 4.4. Gateway checks key and routes by path `/ENV/rest/v1/...`; Studio opened per environment on demand; nightly encrypted per-environment backups | Created 2026-09-20, 2 stars, v0.2.0 development snapshot; self-declared "production acceptance and a signed release are not established". |
| [smartpiai/multibase](https://github.com/smartpiai/multibase) (MIT) | Compose directory per project | Python generator (`setup_secure_supabase.sh`, `supabase_manager.py`) + React/Node dashboard with metrics/log streaming | 73 stars, last push 2025-11-20 (11 months stale). |
| [KHAEntertainment/SupaConsole](https://github.com/KHAEntertainment/SupaConsole) / original [sharonpraju/SupaConsole](https://github.com/sharonpraju/SupaConsole) | Compose stack per project | Next.js dashboard, Docker Compose integration, team management | Announced in #4907 on 2025-08-29; a commenter notes it "spins up new docker containers... not resource efficient". |
| [HarryET/supa-manager](https://github.com/HarryET/supa-manager) (GPL-3.0) | Emulates the Supabase platform API so a patched Studio can list/create projects | Rust API mirroring the Studio mock API; patched Studio in repo; Helm charts | 231 stars, 14 commits, last push 2024-11-20, no docs; author said in #4907 it "died off". GPL-3.0 limits reuse. |
| [flamingrubberduck/supabase-studio-multi-head](https://github.com/flamingrubberduck/supabase-studio-multi-head) | Project registry inside patched Studio | Alpha fork of Studio with multi-project/org support, overlay compose, `smh` CLI; README advertises "Business and Enterprise tiers" (licensing/commercial terms not checked) | 9 stars, created 2026-04-14, last push 2026-07-28. |
| [OpenSupabase Control Plane](https://opensupabase.sadelabs.site/) (Apache-2.0 per site; repo URL not on page) | Per server, from pinned templates | Control plane on Cloudflare Workers; an agent on each server dials out over TLS and executes "a fixed, versioned set of typed operations" via Docker's HTTP API (no remote shell); only metadata leaves your machines | Site says server add/health/container inspect are done; Supabase project deploy incomplete; backups/team/HA not started. [summarized] |
| [Powabase](https://powabase.ai/self-hosted-supabase/) | n/a | Vendor page; confirms "to run several projects, you run several stacks"; multi-project is reserved for its cloud/enterprise | Marketing source. |
| sfp server ([docs.flxbl.io](https://docs.flxbl.io/sfp-server/setting-up/self-hosted-supabase-configuration)) | Compose project per tenant | `sfp server init` writes keys/credentials into the tenant `.env` + compose | [snippet] only; not read in depth. |

### 1.3 What people say breaks

- **Ports.** The compose file publishes Postgres (5432) and pooler (6543) on the host; duplicates collide. Docs-level advice (aantti): "you'll have to figure out port separation". Typical pattern: Kong/Envoy on 8000/8100/8200, Postgres 5432/5532/5632 ([Supascale](https://www.supascale.app/blog/managing-multiple-supabase-projects-on-selfhosted-infrastruc), vendor blog).
- **Container-name collisions.** The Realtime service is deliberately named `realtime-dev.supabase-realtime` because "realtime constructs tenant id by parsing the subdomain" (comment in upstream [docker-compose.yml](https://github.com/supabase/supabase/blob/master/docker/docker-compose.yml)); fixed `container_name`s and `supabase-db` service names collide across stacks. Coolify #5362 reports log-mixing between two instances' DBs.
- **Weight.** Each project is ~8-12 containers. Supascale (vendor blog, not independently measured) estimates 700 MB-1 GB RAM minimal per stack and 4-5 production projects on 8 GB. A user in #4907 complains of 20+ Laravel apps per small VPS vs very few Supabase stacks.
- **Updates.** Upgrading N independent compose forks is N times the toil; self-hosted releases have breaking changes (PG15->17, Kong->Envoy, `supabase_admin`->`postgres`).
- **Studio** has no RBAC and single-DB scope; every multi-project tool either runs one Studio per project or patches Studio's platform API calls.

---

## 2. PaaS one-click installers

All of these deploy the **official compose file as one opaque unit**; none provides a multi-project control plane.

| Platform | How Supabase is packaged | Isolation model | Instances/host and routing | Maintenance picture |
|---|---|---|---|---|
| **Coolify** ([service doc](https://coolify.io/docs/services/supabase), [template](https://github.com/coollabsio/coolify/blob/next/templates/compose/supabase.yaml)) | One 1717-line compose template with config files inlined (`SERVICE_URL_SUPABASEKONG_8000`, generated `SERVICE_PASSWORD_*` secrets); Traefik proxy routes the generated FQDN to Kong | One Docker compose stack per "service", own network | Multiple instances possible but Postgres/pooler host ports collide: workaround is setting `POSTGRES_PORT` 5432->5532 and `POOLER_PROXY_PORT_TRANSACTION` 6543->6643 ([Dokploy/templates #558](https://github.com/Dokploy/templates/issues/558) describes it; [Coolify #5362](https://github.com/coollabsio/coolify/issues/5362) open since 2025-03-17 reports connection issues + log mixing, suggests renaming `supabase-db` per app) | Template lags upstream: pins `supabase/postgres:15.8.1.085`, Kong 3.9.1, Studio 2026.03.16 (upstream is PG17/Envoy/2026.09). Last template commits 2026-04-02..05. Maintainer advice in [Discussion #9957](https://github.com/coollabsio/coolify/discussions/9957): do not update in place; "start a new stack, ensure it works, then migrate your db over"; inlined config makes adding files like a PG17 conf hard; restarts drop pgmq/realtime jobs (user report). |
| **Dokploy** ([template](https://github.com/Dokploy/templates/tree/main/blueprints/supabase), [isolated deployments doc](https://docs.dokploy.com/docs/core/docker-compose/utilities)) | `docker-compose.yml` + `template.toml`; the toml has a **variable DSL that generates per-instance secrets and JWTs** (`${password:32}`, `${jwt:jwt_secret:anon_key_payload}`, `${uuid}` for `POOLER_TENANT_ID`, opaque `sb_publishable_...` keys) and maps one domain to `kong:8000` | "Isolated Deployments": Dokploy creates a network from `appName` and attaches every service, connecting Traefik to preserve routing; avoids name collisions between copies | Same host-port issue for PG/pooler unless unpublished; Traefik routes by domain | Fresher: last commits 2026-08-06/07 (PG17, new API keys + JWKS). Known [issue #558](https://github.com/Dokploy/templates/issues/558) "Cannot create multiple instances of Supabase". Caveat in docs: custom installs that replace standalone Traefik with a Swarm service can lose network refs after restart. |
| **Easypanel** ([template](https://easypanel.io/docs/templates/supabase)) | One "supabase: compose" service in a project; default insecure password in the template | One compose per Easypanel project | Nothing documented on multi-instance | Not investigated further. |
| **CapRover** ([one-click-apps](https://github.com/caprover/one-click-apps/tree/master/public/v4/apps)) | Only `supabase-postgres.yml` exists (the Bitnami legacy *Postgres* image `bitnamilegacy/supabase-postgres-archived`), **not** the full stack; no `supabase.yml` found | n/a | n/a | Not a viable full-stack path. (Listing checked via GitHub API on 2026-10-06.) |
| **Portainer templates** | Not researched | | | **Unverified** |
| **Elestio** ([page](https://elest.io/open-source/supabase), [guide](https://blog.elest.io/how-to-self-host-supabase-on-elestio-full-setup-guide/)) | Managed stack; "one service = one VM" | **VM per Supabase instance** (kernel-level isolation) | Multiple instances = multiple VMs, each billed separately; recommends min 2 vCPU/4 GB, 4/8 more comfortable | Elestio handles OS/software updates, SSL, backups (copies to different datacenter on same continent). Pricing from $16/mo per vendor page. |
| **DigitalOcean 1-Click** ([doc](https://docs.digitalocean.com/products/marketplace/catalog/supabase)) | Single Droplet, Ubuntu 22.04, Docker Compose, post-boot script asks for domain + email, configures HTTPS; `.env` under `/srv/supabase/supabase/docker/` | Droplet per instance | 1 per Droplet | Update procedure delegated to upstream docs. |
| **Hetzner** ([community tutorial](https://community.hetzner.com/tutorials/coolify-supabase-deploy/)) | Coolify on Hetzner CX-class VM | As Coolify | As Coolify | Page body could not be summarized (truncated); [snippet] says CX32 (4 vCPU / 8 GB) is the sizing example. |
| **Cloudron / YunoHost / Umbrel** | No official packaging found. YunoHost forum: "it is not even in the wishlist" ([thread](https://forum.yunohost.org/t/was-supabase-ever-considered-as-a-yunohost-app/29160)); Cloudron wishlist item and Umbrel community install-stuck-at-1% report [snippet] | n/a | n/a | These are single-tenant-per-app appliance platforms; not a model for this problem. |

Takeaway: the PaaS ecosystem validates **"stack per project" + reverse proxy by hostname** as the de facto standard, and exposes its two chronic failure modes (host-port collisions; template/upgrade drift). Dokploy's per-instance secret DSL and per-compose networks are the two details worth copying.

---

## 3. Kubernetes

### 3.1 Helm chart and operator (supabase-community/supabase-kubernetes)

Source: [repo](https://github.com/supabase-community/supabase-kubernetes) (822 stars, Apache-2.0, last push 2026-10-04) and its [README](https://github.com/supabase-community/supabase-kubernetes/blob/main/README.md).

- Releases (GitHub API): `supabase-0.8.0` (chart, 2026-09-05), `supabase-operator-0.1.3` / `v0.1.3` (2026-07-23). So both are alive; the chart is not abandoned as discussion #39820 feared.
- README: "supported by the community and not officially supported by Supabase". Docs page at supabase.com lists "Supabase Operator or Helm chart" as the community Kubernetes options.
- **Operator** (`core.supabase.io/v1alpha1`), "in an early stage of development and its API may change". CRDs: `Project` (shared config, credentials, migrations, DB sync), `SingleDatabase` ("Postgres database managed by the Operator"), `Function`, `Migration`, and per-component `Auth`, `Rest`, `Meta`, `Realtime`, `Supavisor`, `Storage`, `Studio`, `Envoy`, `EdgeRuntime`.
- Per-project install is a Helm release of `supabase/supabase-project` which creates a `SingleDatabase` + `Project` + enabled components; the operator "will provision the Postgres StatefulSet, the component Deployments, Services, Secrets, and run sync Jobs to configure JWT keys and the database". README shows single-project examples only; **N projects = N releases/namespaces is the implied model, not documented.**
- [PR #268](https://github.com/supabase-community/supabase-kubernetes/pull/268) (merged 2026-10-03 per fetch summary) adds a `Supavisor` CRD: single replica, `Recreate` strategy, 5432 session/6543 transaction ports, project-scoped tenant seeded from the Project's credentials. So the K8s operator mirrors the compose model (pooler per project), not a shared multi-tenant pooler.

### 3.2 Other operators

| Project | Model | Maturity |
|---|---|---|
| [STRRL/supabase-operator](https://github.com/STRRL/supabase-operator) (MIT) | `SupabaseProject` CR; README bullet: "Multi-tenant: Multiple Supabase projects per namespace"; points at an **external PostgreSQL** via a secret (`host/port/database/username/password`), initializes schemas `auth`/`storage`/`realtime`, roles `authenticator`/`anon`/`service_role` and extensions inside that DB; needs `CREATEDB` or superuser; generates `<project>-jwt` secret; deploys Kong, Auth, PostgREST, Realtime, Storage, Meta | v1alpha1, 10 stars, 34 commits; HA/backups/monitoring deferred to "v1beta1". How it avoids cluster-global role collisions between projects sharing one Postgres is **not documented in what I read.** |
| [stack-cli/stack-cli](https://github.com/stack-cli/stack-cli) (MIT; Ian Purton, per discussion #31147) | `StackApp` CRD, namespace per app, CNPG for Postgres, "Supabase-style" PostgREST/Storage/Realtime/GoTrue + Keycloak OIDC | 12 stars, last push 2026-04-03. Compatible-API approach rather than stock Supabase images. |
| [Bitnami supabase chart](https://github.com/bitnami/charts/tree/main/bitnami/supabase) | Cited in #4907 (2023) as "multitenant support is just one ingress away" | Current status **not verified** (Bitnami changed its catalog in 2025; the CapRover template references a `bitnamilegacy/...-archived` image). |

### 3.3 Postgres operators and Supabase

- **CloudNativePG**: `Database` CRD ([docs](https://cloudnative-pg.io/docs/1.28/declarative_database_management/)) declaratively creates databases, extensions, schemas, FDWs in a cluster, but names `postgres`/`template0`/`template1` are reserved, and roles are managed separately at cluster level (`managed.roles`). **Recovery is whole-cluster only** and always bootstraps a *new* cluster ([recovery docs](https://cloudnative-pg.io/docs/1.28/recovery/): "no mechanism to restore individual databases"). Maintainers' 2022 stance in [discussion #2357](https://github.com/cloudnative-pg/cloudnative-pg/discussions/2357): one database per cluster, because "if one database goes down, all of them will go down", cluster-per-workload gives independent PITR, retention and resources; participants note hundreds of tiny clusters raise baseline cost (each needs its own instance(s) + standby). That thread is old; the later `Database` CRD softened but did not reverse the guidance (I did not find a newer statement).
- **Pairing with Supabase**: only CNPG has documented attempts: [voltade/cnpg-supabase](https://github.com/voltade/cnpg-supabase) (PG 17.5 image with Supabase-picked extensions + Barman), [supafull/supabase-extensions](https://github.com/supafull/supabase-extensions) (ImageVolume extension payloads for CNPG PG18), jniclas/supabase-cloudnativedb, mdluo's image, and the recipes in #31147. Required hacks: skip `demote-postgres`, keep `supabase_admin` superuser via `managed.roles`, key files for pgsodium/Vault, a migration Job (three separate migration sets: image, supabase repo, per-service). Stack CLI uses CNPG too.
- **Zalando postgres-operator**: `spec.databases`/`spec.users` and `preparedDatabases` generate `<db>_owner/_reader/_writer` roles per database and schema ([docs](https://postgres-operator.readthedocs.io/en/latest/user/)), and it supports `pg_cron` among its extensions; no Supabase pairing found.
- **Crunchy PGO / StackGres**: no Supabase pairing found. Caveat from a comparison blog [snippet]: PGO's *images* are under the Crunchy Developer Program (not free for production without a contract) while the operator code is Apache-2.0; StackGres bundles pooling/backups/web console and 150+ extensions. I did not verify either claim against vendor pages.

---

## 4. Postgres-level multi-tenancy primitives

### 4.1 Supavisor (Supabase's multi-tenant pooler)

Sources: [connecting overview](https://supabase.github.io/supavisor/connecting/overview/), [tenants config](https://supabase.github.io/supavisor/configuration/tenants/), [repo](https://github.com/supabase/supavisor) (2268 stars, Apache-2.0, pushed 2026-10-06, release 2.9.12 pinned by self-hosted 0.8.1), [1M-connections post](https://supabase.com/blog/supavisor-1-million), router source (`lib/supavisor_web/router.ex`), and the self-hosted seed script [`docker/volumes/pooler/pooler.exs`](https://github.com/supabase/supabase/blob/master/docker/volumes/pooler/pooler.exs).

- **Tenant lookup** by `external_id`, supplied one of three ways: username suffix (`postgres.dev_tenant`), SNI subdomain (`dev_tenant.supabase.co`), or startup option `reference=...`. Self-hosted exposes this as `postgres.[POOLER_TENANT_ID]` ([Accessing Postgres](https://supabase.com/docs/guides/self-hosting/accessing-postgres)).
- **Tenant record** (metadata DB, `_supabase` database, `_supavisor` schema): `db_host`, `db_port`, `db_database` = the upstream; `upstream_ssl/verify/tls_ca`, `enforce_ssl`, `allow_list` (CIDRs), `require_user`, `auth_query`, `default_pool_size`, `default_max_clients`, `client_idle_timeout`, `sni_hostname`, plus per-tenant `users` (`db_user`, `db_password`, `mode_type`, `pool_size`, `is_manager`).
- **Self-hosted seeding**: the seed script creates one tenant with `auth_query = "SELECT * FROM pgbouncer.get_auth($1)"` and `require_user = false` using the `pgbouncer` role, i.e., each tenant database needs the `pgbouncer` auth schema/function.
- **Admin API**: `PUT/PATCH/GET/DELETE /api/tenants/:external_id`, `GET .../terminate`, `POST .../update_auth_credentials`, `POST /rebalance`, `PUT /clusters/:alias` (from router source). That is a ready-made provisioning surface for a control plane.
- **Behavior at Supabase**: "Each project receives its own connection pool maintained by the multi-tenant Supavisor cluster" (the 1M-connection post). Pools are created on first connect.
- **Gaps**: the README summary I fetched claimed "transaction mode only"; that is outdated (compose publishes both 5432 session and 6543 transaction ports and the docs describe both). PR #268 above shows the K8s operator running it single-replica per project.

**Pooler comparison (analysis built on sources):**

| | PgBouncer ([config](https://www.pgbouncer.org/config.html)) | Supavisor | PgCat ([repo](https://github.com/postgresml/pgcat)) |
|---|---|---|---|
| Multi-tenant routing key | Client-visible **database name** in `[databases]` mapped to host/port/dbname, optional `*` wildcard with `autodb_idle_timeout`; per-database `auth_query`/`auth_dbname`; pools per (user, database) | Tenant `external_id` from username/SNI/option; tenants in a DB with a REST API; dynamic | Pools per database/user in config; sharding extensions (`SET SHARD`); config reload without restart |
| Footprint/maturity | Tiny, C, latest 1.26.0 (2026-09-23); self-hosted compose has an official optional override since 0.8.1 (no tenant id in username) | Elixir/BEAM clustering; 250k-500k idle conns/node claimed by Supabase; self-hosted default | Rust; **last push 2025-02-27, last release 0.2.5 (2024-11-11)** - looks unmaintained |
| Fit | Good if tenancy is expressed as DB names (DB-per-project) and you generate `pgbouncer.ini`/use `*` wildcard; wants an `auth_query` role per DB | Native fit for "many tenants -> many upstreams" and what Supabase itself runs | Skip unless you need sharding |

### 4.2 Database-per-tenant in one cluster vs instance-per-tenant (and what Supabase assumes)

General facts (sourced):
- **Roles are cluster-global**: "Database roles are global across a database cluster installation (and not per individual database)" ([PG docs 21.1](https://www.postgresql.org/docs/current/database-roles.html)). Per-database isolation therefore depends on `CONNECT` privileges and `pg_hba.conf` rules; Sbarbase does exactly this ("connection rules naming exact login and database pairs").
- `CREATE DATABASE ... TEMPLATE` copies a template only when **no other session is connected** to it; `STRATEGY FILE_COPY` forces checkpoints; `CONNECTION LIMIT` is approximate and not enforced against superusers ([PG docs](https://www.postgresql.org/docs/current/sql-createdatabase.html)). So "template database as project seed" works for provisioning but needs a quiet template.
- **Backups/PITR are cluster-granular** (CNPG: see 3.3). Per-database logical dumps are possible but they are not PITR.
- **Catalog and worker overhead** rises with tenant count: Instant's essay claims per-app Postgres schemas "struggle beyond ~6,000 tables" ([essay](https://www.instantdb.com/essays/architecture), claim by vendor, not measured by me). Each Realtime tenant uses a logical replication slot; the Supabase image sets `max_replication_slots = 5`, `max_wal_senders = 10`, `wal_level = logical` ([postgresql.conf.j2](https://github.com/supabase/postgres/blob/develop/ansible/files/postgresql_config/postgresql.conf.j2)).

**Supabase features that assume cluster-level control (primary sources: [supabase/postgres init SQL](https://github.com/supabase/postgres/blob/develop/migrations/db/init-scripts/00000000000000-initial-schema.sql), [postgresql.conf.j2](https://github.com/supabase/postgres/blob/develop/ansible/files/postgresql_config/postgresql.conf.j2), [supautils.conf.j2](https://github.com/supabase/postgres/blob/develop/ansible/files/postgresql_config/supautils.conf.j2), [supautils README](https://github.com/supabase/supautils), [docker compose](https://github.com/supabase/supabase/blob/master/docker/docker-compose.yml)):**

| Feature | What the sources show | Consequence for DB-per-tenant in one cluster |
|---|---|---|
| Roles `anon`, `authenticated`, `service_role`, `authenticator` | Created once with `create role ...` in the init script (`authenticator` is granted `anon`, `authenticated`, `service_role` and **`supabase_admin`**). `service_role` has `BYPASSRLS`. Statement timeouts set with `alter role anon set statement_timeout='3s'` (role-wide). | One set shared by all tenants. A tenant's `anon`/`authenticated` privileges in its own DB are per-DB grants, so data isolation is feasible, but any cluster-level `ALTER ROLE ... SET` and passwords are shared. `authenticator` being a member of `supabase_admin` is an unusual privilege path worth threat-modeling per tenant. |
| `supabase_admin` | `alter user supabase_admin with superuser createdb createrole replication bypassrls`; Realtime and Supavisor connect as `supabase_admin`; the init scripts hard-code `grant create on database postgres`. Since 0.6.0 Studio and meta use `postgres` instead ([changelog](https://github.com/supabase/supabase/blob/master/docker/CHANGELOG.md), [how-to](https://supabase.com/docs/guides/self-hosting/remove-superuser-access) - which says objects created in Studio may remain owned by `supabase_admin` outside `public`). | A superuser is a cross-tenant superuser. Realtime/Supavisor-as-superuser is the shared trust boundary. |
| `supabase_auth_admin`, `supabase_storage_admin`, `supabase_functions_admin`, `pgbouncer`, `supabase_realtime_admin`, etc. | Each is one cluster role with one shared password (`roles.sql` sets all to `POSTGRES_PASSWORD`); services use them against `${POSTGRES_DB}`. | Need per-tenant logins or per-tenant passwords + `CONNECT` limited by HBA; Supabase images expect these exact names, so you cannot simply rename them per tenant. Sbarbase creates "three scoped service logins" per environment (so it must remap names; mechanism not examined). |
| Database name | Compose parameterizes `POSTGRES_DB` for Auth, REST, Realtime, Storage, meta, Studio, functions. But init scripts also create a second database `_supabase` (Supavisor `_supavisor` and Logflare `_analytics` schemas) and a `_realtime` schema inside the main DB, and use `\c postgres`. | A non-default DB name works for runtime services, but init/migration scripts that assume `postgres` need re-running per DB. Sbarbase's native placement contract insists on "application database `postgres`" plus a maintenance database, suggesting upstream tooling is sensitive to this. |
| Database-level settings | `ALTER DATABASE postgres SET "app.settings.jwt_exp"` (jwt.sql) | Good: per-DB GUCs exist natively. (0.8.2 also removed `app.settings.jwt_secret`.) |
| `pg_cron` | "may only be installed to one database in a cluster", `cron.database_name` (default `postgres`); other databases only via `cron.schedule_in_database()` ([pg_cron README](https://github.com/citusdata/pg_cron)). Supabase overrides schema to `pg_catalog`. | A tenant's own DB cannot have the Supabase cron UI/extension; the platform would have to schedule on their behalf from the one cron DB (also a cross-tenant superuser-ish path). Sbarbase's shared workflow "does not expose pooler or cron support". |
| `pg_net` | `pg_net.database_name` defaults to `postgres`; "Using pg_net on multiple databases in a cluster is not yet supported" ([README](https://github.com/supabase/pg_net)); also requires `shared_preload_libraries`. | Database webhooks / `net.http_*` work for exactly one DB per cluster. Hard blocker for DB-per-tenant parity. |
| `pgsodium` / Vault | `shared_preload_libraries` includes `pgsodium` and `supabase_vault`; the compose file persists the decryption key in a named volume `db-config:/etc/postgresql-custom`. Docs: pgsodium is "pending deprecation"; Vault does not depend on it but "share[s] the same per-project root encryption key" ([docs](https://supabase.com/docs/guides/database/extensions/pgsodium)). | Root key is per cluster (file on the server), so all DBs in a cluster share it; per-tenant keys need cluster-per-tenant. (Statement that key is per *cluster* is my inference from the file-based mount, not an explicit doc claim.) |
| `supautils` | `shared_preload_libraries = supautils`, every GUC cluster-wide in `postgresql.conf` (`reserved_roles`, `reserved_memberships`, `privileged_extensions`, `policy_grants`, `drop_trigger_grants` ...). `policy_grants`/`drop_trigger_grants` hard-code `auth.*` / `realtime.*` tables for the `postgres` role. `reserved_roles` includes wildcards like `anon*`. | One policy for all tenants. Per-tenant `postgres`-equivalent roles would need those JSON lists extended. |
| Logical replication / Realtime | `wal_level=logical`, publication `supabase_realtime` created in the init script (database-level object); Realtime is multi-tenant and reads each tenant's DB. | Slot count and WAL volume are cluster resources: noisy-neighbor and `max_replication_slots` limits come into play. |
| Extensions in general | Image preloads `pg_stat_statements, pgaudit, plpgsql_check, pg_cron, pg_net, pgsodium, timescaledb, auto_explain, pg_tle, plan_filter, supabase_vault`. | Preload list is cluster-wide: one tenant's extension needs mean one cluster-wide restart. |
| `auth` and `storage` schemas | Created per database by init scripts (`00000000000001-auth-schema.sql`, `...02-storage-schema.sql`) and migrated by GoTrue/Storage themselves. | Per-DB, so fine. Auth cannot multiplex DBs (no multi-tenant GoTrue yet). |

### 4.3 How "multi-tenant aware" are Supabase's own services?

| Service | Multi-tenant support | Evidence |
|---|---|---|
| Supavisor | Native | Sections above. |
| Realtime | Native. "A multi-tenant Phoenix app... A tenant is identified by its `external_id`"; tenant derived from request host subdomain; JWT verified against the tenant's `jwt_secret`/`jwt_jwks`; tenants/extensions rows (DB credentials) encrypted with `DB_ENC_KEY` in `_realtime.tenants`; "talks to its own database as well as to individual tenant databases"; `/api/tenants` admin API (blocked at the gateway in self-hosted, 0.6.0 security fix) | [ARCHITECTURE.md](https://github.com/supabase/realtime/blob/main/ARCHITECTURE.md), [ENVS.md](https://github.com/supabase/realtime/blob/main/ENVS.md), self-hosted [CHANGELOG](https://github.com/supabase/supabase/blob/master/docker/CHANGELOG.md) 0.6.0 |
| Storage-api | Native multi-tenant mode: `MULTI_TENANT`, `DATABASE_MULTITENANT_URL`, `REQUEST_X_FORWARDED_HOST_REGEXP` (example `^([a-z]{20}).local.(?:com|dev)$`), admin API on port 5001, `DB_INSTALL_ROLES`, per-tenant DB; the self-hosted compose uses single-tenant `TENANT_ID` | [docker-compose-multi-tenant.yml](https://github.com/supabase/storage/blob/master/docker-compose-multi-tenant.yml). (GustavoMartins' 20-letter public refs match this regexp, an obvious borrowing.) |
| PostgREST | Single DB per process. Open upstream issue [#2798](https://github.com/PostgREST/postgrest/issues/2798) (2023, "expose multiple different databases on a single postgrest instance"). A tenant-aware PR [#5087](https://github.com/PostgREST/postgrest/pull/5087) (tenant id from JWT claim / host regex / header; `tenant-db-uri-template`; bounded resident tenants, idle eviction, cold-start limits) was **closed unmerged 2026-07-09** by maintainer with the comment "Closing in violation of our strict no-AI policy" (so upstream will not take AI-authored code; the *design* is still instructive). |
| Auth (GoTrue) | Single DB per process. [#2621](https://github.com/supabase/auth/issues/2621) and [#2622](https://github.com/supabase/auth/issues/2622) (2026-07-09/10, same author as the PostgREST PR) propose `isolated_db` multi-tenant mode: control-plane `auth_tenants` table, tenant from header or forwarded-host regex, bounded per-tenant DB pool, LISTEN/NOTIFY cache invalidation; explicitly keeps the old shared `instance_id` mode unsupported. **Open, no maintainer response visible; claims of load-test results are the author's own.** Historic: kangmingtay (#1615) "plan to make gotrue multi-tenant eventually"; the legacy `instances` table is being deprecated [snippet]. |
| Studio / postgres-meta | Single DB each. | Self-hosting docs; compose. |
| Edge runtime | One per project; function code is mounted from a volume. | compose. |
| API gateway | Envoy (default since 0.8.0, 2026-08-11) with `lds.template.yaml`; Kong override slated for removal. | CHANGELOG. Envoy supports dynamic config in general (analysis), but upstream's file is a static template. |

Whether Supabase Cloud actually shares Realtime/Storage fleets across projects is **not stated** in the architecture doc ([architecture](https://supabase.com/docs/guides/getting-started/architecture)); the doc lists Postgres, Studio, GoTrue, PostgREST, pg_meta, Deno, pg_graphql as per-project and is silent on Realtime/Storage/Supavisor (the Supavisor post says it is a shared multi-tenant cluster).

### 4.4 Worked example: Sbarbase (the only "one cluster, DB per project" implementation found)

From its own docs ([architecture](https://github.com/M7MMAD-OMAR/sbarbase/blob/main/docs/explain/architecture.md), [isolation and trust](https://github.com/M7MMAD-OMAR/sbarbase/blob/main/docs/explain/isolation-and-trust.md), [status](https://github.com/M7MMAD-OMAR/sbarbase/blob/main/docs/reference/status.md), [PROJECT_GOAL](https://github.com/M7MMAD-OMAR/sbarbase/blob/main/PROJECT_GOAL.md)):
- Shared: one PostgreSQL cluster, one Storage process (tenant header), gateway, control catalog (SQLite). Per environment: database, three scoped service logins, `pg_hba` rules pairing exact login and database, Auth, PostgREST, publishable keys (hashed), Storage tenant and signing keys, optional Realtime/Functions container, Studio on demand.
- Rejected: one Auth+PostgREST for all environments ("Neither upstream service supports that as an established mode, and making it work means rewriting security-critical code") and hostile multi-tenant hosting ("a shared PostgreSQL engine cannot give" isolation from the operator or from arbitrary SQL).
- Admits: canonical roles exist once per engine "because PostgreSQL roles are cluster-wide"; engine crash/full disk/long lock hits every environment; resource tiers "configured, not calibrated"; reloading HBA does not end open sessions; admission is per gateway process; cron/pooler not supported in the shared workflow; an experimental "native-dedicated" placement (own engine per environment, patched PostgreSQL 17.11) is **not accepted** - startup, cron/Vault continuity and physical recovery are UNRUN. 1504 tests pass, but the author states production acceptance is not established.
- Provenance caution: heavily agent-generated documentation, project 3 weeks old, 2 stars. Treat as a worked design sketch.

---

## 5. Adjacent BaaS / platforms (isolation unit + reusable pattern)

| Platform | Isolation unit | Reusable pattern | Sources |
|---|---|---|---|
| **Nhost** (9.3k stars, pushed 2026-10-06) | Cloud: each project gets its own Postgres (Hasura + Hasura Auth + Storage + functions around it). Self-host/CLI: a compose project per Nhost project; running several locally means overriding `--project-name`, `--http-port`, `--postgres-port`, giving "separate containers, networks, and data volumes". | Same stack-per-project as Supabase; the CLI's `--project-name` namespacing is the lightest version of the pattern. Cloud isolation internals not documented in what I read. | [docs](https://docs.nhost.io/platform/cli/multiple-projects), [repo](https://github.com/nhost/nhost) |
| **Appwrite** (57.6k stars) | Not "single-tenant by design" as the brief assumed: one self-hosted instance supports multiple projects, but only **one organization** (created at first sign-in); all projects share the instance's one database (Postgres default in 2.0), logically separated at the application layer; DB engine choice is permanent per instance. Recommended SaaS pattern: one Appwrite *project* per tenant with scoped API keys, plus a mapping table (tenant id, org id, project id, region). Appwrite 2.0 combined-worker topology cuts default containers from 33 to 16. | A real "console with multiple projects" on shared infrastructure is possible when the platform is *built* for it (project id in every row/route), which is the thing Supabase's services lack. Also: ship a "combined" topology to cut container count. | [multi-tenancy guide](https://appwrite.io/docs/partners/guides/multi-tenancy), [2.0 post](https://appwrite.io/blog/post/appwrite-2-self-hosted); shared-DB statement from search snippet |
| **PocketBase / PocketHost** (1.4k stars, MIT, pushed 2026-09-28) | One PocketBase instance (single Go binary + SQLite files) per customer; subdomain/custom domain per instance. Pocker announcement claims a single Go process hosts "1000+ PocketBase instances... on a single 256mb VPS" vs about 50 per 32 GB with Docker, 5 ms vs 600-1000 ms boot. Production page and README say instances run in Docker with auto-SSL; I could not reconcile exactly what runs where. | **Make the tenant the process/goroutine, not a container**; boot on demand; subdomain routing. Not directly transferable to Postgres-based Supabase, but the lesson is "idle tenants must cost almost nothing". Treat the 1000-instance claim as vendor-stated. | [Pocker post](https://pockethost.io/blog/announcing-pocker), [repo](https://github.com/pockethost/pockethost), [about](https://pockethost.io/about) |
| **Convex self-host** (12.7k stars) | One backend binary/container per deployment with SQLite default, optional Postgres/MySQL; announcement says self-hosting is "not... an acceptable escape hatch for any major product gaps" and does not offer projects/teams (a 2024 comment in #4907 says the Convex team told a user project/team support was their most requested feature). | Same gap as Supabase: the open runtime is single-deployment; the project/team layer is the proprietary control plane. | [announcement](https://news.convex.dev/self-hosting/), #4907 |
| **Directus** | Project-scoping = separate Directus instances (one DB each) behind nginx, "may not be viable" self-hosted; alternative is row/schema scoping; Directus Cloud professional projects are multi-tenant but "each... scoped to one container with dedicated minimum resources" | Reinforces "instance per tenant + proxy" as default across CMS/BaaS. | search [snippet](https://directus.io/docs/cloud/getting-started/introduction); [GitHub #3987](https://github.com/directus/directus/discussions/3987) |
| **Neon** | Cloud: stateless compute (Postgres) per branch + shared multi-tenant storage (pageservers, safekeepers, S3 as source of truth); scale to zero after 5 min idle; branching is copy-on-write. Self-host: [molnett/neon-operator](https://github.com/molnett/neon-operator) (CRDs `NeonCluster`, `NeonProject`, `NeonBranch`; Apache-2.0, "part of acquisition of Molnett") runs computes persistently - "do not scale to zero", manual tenant sharding, "functional for development and testing", not production ready. | **Separate durable storage from ephemeral compute** so idle tenants cost only storage; start compute on connect (proxy). The self-host form is not ready, so this is a target architecture, not an option. Whether Neon's production control plane is open is **not confirmed** (the main repo's `control_plane` dir is for `neon_local`). | [Neon repo](https://github.com/neondatabase/neon), [neon-operator](https://github.com/molnett/neon-operator), [Neon blog](https://neon.com/blog/architecture-decisions-in-neon) |
| **Fly.io Machines** | One Fly *app* per tenant (recommended) with its own Machines; single shared app is discouraged because the proxy's stop loop "stops or suspends at most one Machine per region each pass" and a compromised tenant can reach all Machines; router app does wildcard subdomain -> `fly-replay` header (~10 ms) naming target app+machine; proxy starts a stopped Machine on demand; shared secret via `state`; volumes survive restart but hardware loss can destroy them (persist elsewhere). | The cleanest "VM-per-tenant with scale-to-zero" recipe: **thin stateless router + on-demand start + per-tenant network namespace + per-tenant shared secret between router and tenant**. Directly mappable to systemd/Firecracker/containerd on AWS. | [per-user dev environments](https://docs.fly.io/blueprints/per-user-dev-environments), [connecting to user machines](https://docs.fly.io/blueprints/connecting-to-user-machines) |
| **Turso / libSQL** | A database (SQLite file) per tenant; free plan 500 DBs, $29 plan 10,000 DBs, created via API; embedded replicas, PITR and branching per DB [snippet figures] | Isolation unit = a cheap file; per-tenant lifecycle (create, branch, restore) is an API call. For Postgres the nearest analog is DB-per-tenant with a `TEMPLATE` database, but without per-DB PITR. | [Turso multi-tenancy](https://turso.tech/multi-tenancy) |
| **InstantDB** | A row-level `app_id` in one giant triple-store table in one Postgres: "When you create a new project, we don't spin up a VM. We just insert a few database rows in a multi-tenant instance." | The extreme end: tenant = a key. Only possible because the whole platform (sync engine, permissions) was built tenant-aware from day one. | [architecture essay](https://www.instantdb.com/essays/architecture) |
| **Supabase Cloud** (reference) | Dedicated Postgres instance on its own server per project (compute add-on tiers; Nano-Medium shared CPU, Large+ dedicated); control plane + Management API are not open source (aantti, #4907) | The upstream answer is "instance per project, platform-side orchestration". | [compute docs](https://supabase.com/docs/guides/platform/compute-and-disk) [snippet], #4907 |

---

## Design patterns extracted (analysis, mine - not claims from the sources)

**P1. Compose stack per project behind a hostname router.** The ecosystem baseline (Coolify/Dokploy/Multibase/supabase-multitenant). *Pros:* stock upstream images, strongest isolation short of VMs, trivially reproducible, matches Supabase Cloud's layout so projects migrate in and out. *Cons:* 8-12 containers per project; duplicated forks to upgrade; host-port and container-name collisions unless every publish is removed and every `container_name` is templated; no cross-project pooling or scale-to-zero. Mitigations seen: Dokploy isolated networks; unpublished DB ports; per-instance secret generation.

**P2. Kubernetes namespace (or CR) per project via the community operator/Helm.** *Pros:* declarative lifecycle, native secrets/networking, upgrades by bumping a CR, maps from compose (aantti notes the compose model "is easier to map then into" the Kubernetes repo). *Cons:* operator is v1alpha1; per-project Supavisor/Envoy/Postgres StatefulSet means high baseline per tenant; only helps if the target platform is Kubernetes (EKS), which the brief calls "heavier" than native.

**P3. Hybrid: shared multi-tenant fleet + per-project thin services + Postgres per project.** Run once: Supavisor (tenant API), Realtime (tenant by subdomain), Storage-api (tenant by `x-forwarded-host`), the gateway/router. Run per project: Auth and PostgREST (the two services with no multi-tenant mode), and Postgres. *Pros:* the three services that Supabase itself built to be multi-tenant are shared by design (so no fork), and the two "small" services per project are cheap (Sbarbase makes the same argument); isolation of data and keys is real. *Cons:* the shared Realtime/Storage are cross-tenant failure and compromise boundaries (Sbarbase: "a compromise of that process is a compromise of all tenants' files"); Realtime/Supavisor connect as superuser to every tenant DB; need an upstream-compatible way to seed per-tenant Storage and Realtime rows (their admin APIs).

**P4. Postgres granularity choice.** (a) *Cluster per project* (CNPG `Cluster` or a Postgres container/VM): independent PITR, extensions, `pg_cron`, `pg_net`, Vault root key, role sets, `supautils` config, upgrades and noisy neighbors; costs 1 Postgres process (+standby) per project, mitigated by scale-to-zero. (b) *Database per project in a shared cluster*: cheap and fast to provision (`CREATE DATABASE ... TEMPLATE`), pooler routing by DB name; but `pg_net` and `pg_cron` are effectively single-DB, roles/Vault key/`supautils` settings/replication slots/WAL are shared, PITR is cluster-wide, a superuser (`supabase_admin`, also used by Realtime and Supavisor) spans tenants. Appropriate for "agency with trusted operators" (Sbarbase's stated scope), not for mutually hostile tenants. (c) *Schema per project*: not Supabase multi-tenancy (Auth/Storage/roles shared). Evidence favors (a) for a service you want to describe as isolated, (b) as an optional density tier for low-risk tenants.

**P5. Tenant-scoped routing from the hostname, not from request bodies.** Realtime, Storage and Supavisor already derive the tenant from the subdomain / `x-forwarded-host` / SNI; supabase-multitenant uses 20-letter random refs in the same regexp shape; Fly uses wildcard subdomain + `fly-replay`. A single wildcard DNS name and TLS cert plus a small router (Envoy/Caddy/OpenResty) that maps `<ref>.domain` to the project's upstreams gives zero per-project proxy config and makes tenant identity unforgeable from the client. *Con:* Studio and database connections need their own scheme (Supavisor SNI works for the DB side).

**P6. Declarative desired state + small reconciler, not a web app holding the Docker socket.** Contrast: supabase-multitenant panel mounts `/var/run/docker.sock` (root-equivalent exposure) vs GustavoMartins' HMAC-signed intents executed by a root host agent vs OpenSupabase's outbound-only agent with "a fixed, versioned set of typed operations" vs the K8s operator CRs. The last three are the same idea: a narrow, typed, auditable command channel. *Pro:* limits blast radius of a compromised control UI. *Con:* you write and maintain the agent/reconciler.

**P7. Per-project secret/key generation as a template DSL, applied at creation.** Dokploy's `template.toml` generates passwords, a JWT secret, `anon`/`service_role` JWTs, opaque `sb_publishable_`/`sb_secret_` keys, Realtime enc key and a `POOLER_TENANT_ID` once at deploy. Self-hosted 0.7+ also ships key-management/update scripts. Generate once, store in a secret store, never in repo or compose files. *Con:* rotation needs the upstream scripts per release.

**P8. Treat upstream releases as a pinned bundle (image set + compose + SQL migrations), not as files you fork.** The upstream CHANGELOG/`versions.md` give image sets per release; the template-lag problem (Coolify pinned to PG15/Kong after upstream moved to PG17/Envoy) is the cost of copying. A generator that renders per-project config from the pinned upstream version, and migrates projects in canary waves, avoids hand-merging.

**P9. Scale-to-zero via on-demand start behind a router (Fly/Neon/PocketHost lesson).** Keep a tiny always-on edge (router + Supavisor) and let each project's Postgres/Auth/PostgREST sleep; the router (or pooler on connect) starts them. *Pros:* idle tenants cost memory only for the control state; *Cons:* cold-start latency (seconds for Postgres), Realtime/WebSocket tenants cannot sleep, `pg_cron` jobs and Realtime slots break sleep, and no shipped upstream support. The Neon operator shows that even Neon's self-host path lacks scale-to-zero.

**P10. Make the backup/restore unit equal the isolation unit.** CNPG: restore is whole-cluster only. Cluster-per-project gives per-project PITR; DB-per-project needs logical dumps or per-DB base backups (Sbarbase does nightly encrypted per-environment backups and single-environment restore, but its native physical recovery is unaccepted). Decide this before choosing P4.

**P11. Ride the one upstream seam that does exist: Supavisor tenants API + Realtime/Storage admin APIs + `external_id`/subdomain conventions.** These are upstream-supported multi-tenant control surfaces. Building anything that depends on a PostgREST/Auth multi-tenant fork means maintaining patches (the PostgREST PR was rejected for process reasons; the Auth proposals are unreviewed).

---

## Unverified / gaps

- **Portainer templates**, **Easypanel multi-instance behavior**, **Cloudron/Umbrel packaging details**: only search snippets or nothing.
- **Hetzner tutorial body** was not readable (truncated by fetch).
- **Whether Supabase Cloud shares Realtime/Storage fleets across projects**: not stated in docs I could reach.
- **STRRL operator**: how multiple projects sharing one external Postgres avoid cluster-global role collisions is not documented in the README sections read.
- **supabase-multitenant (org)**: README says "Postgres per project"; no data on max projects per host, upgrades or backups. I did not read its docs/SELF-HOSTING.md.
- **OpenSupabase** repo URL, and its license claim, come from its site only.
- **Sbarbase** claims (isolation, tests passed, recovery) are self-reported by a 3-week-old, agent-assisted project; I did not run it.
- **Supascale** per-instance RAM figures (700 MB-1 GB) and "4-5 projects on 8 GB" are from a vendor blog, not measured.
- **PocketHost** "1000+ instances on 256 MB" and 5 ms boot are vendor-stated; production process model (Docker vs in-process) is described inconsistently across its own pages.
- **Turso limits/pricing**, **Convex multi-deployment guidance**, **Nhost cloud isolation internals**, **Directus cloud details**: partly search snippets.
- **PGO image licensing and StackGres extension counts**: from a comparison blog snippet.
- **Bitnami Supabase chart current status**, **Supabase `instances` table deprecation**, **Coolify "Supabase plans to remove built-in logging/analytics"** (discussion summary; the CHANGELOG does confirm analytics/logging became optional compose overrides in 0.5.0 per fetch summary): not independently verified.
- **Role-per-tenant remapping**: I did not verify whether Supabase's Auth/Storage migrations tolerate renamed `supabase_auth_admin`/`supabase_storage_admin` roles (relevant to P4b); Storage's `DB_INSTALL_ROLES` hints that roles are expected by name.
- **Docker Swarm discussion #27467**, **sfp server docs**: not read.
- Fetch tool outputs for several pages are model-summarized; where I could, I re-read primary text with `gh api` (upstream CHANGELOG, compose file, init SQL, postgresql.conf.j2, supautils.conf.j2, Supavisor router/seed script, discussion bodies, Sbarbase docs, PostgREST/Auth issue bodies).

---

## Sources

Upstream Supabase
- https://supabase.com/docs/guides/self-hosting
- https://supabase.com/docs/guides/self-hosting/accessing-postgres
- https://supabase.com/docs/guides/self-hosting/remove-superuser-access
- https://supabase.com/docs/guides/getting-started/architecture
- https://supabase.com/docs/guides/database/postgres/roles
- https://supabase.com/docs/guides/database/extensions/pgsodium
- https://supabase.com/docs/guides/platform/compute-and-disk
- https://supabase.com/blog/supavisor-1-million
- https://github.com/supabase/supabase/blob/master/docker/docker-compose.yml
- https://github.com/supabase/supabase/blob/master/docker/CHANGELOG.md
- https://github.com/supabase/supabase/tree/master/docker/volumes/db (`_supabase.sql`, `realtime.sql`, `pooler.sql`, `roles.sql`, `jwt.sql`, `logs.sql`)
- https://github.com/supabase/supabase/blob/master/docker/volumes/pooler/pooler.exs
- https://github.com/supabase/postgres (init-scripts `00000000000000-initial-schema.sql`; `ansible/files/postgresql_config/postgresql.conf.j2`, `supautils.conf.j2`, `pg_hba.conf.j2`)
- https://github.com/supabase/supautils
- https://github.com/supabase/pg_net
- https://github.com/supabase/vault
- https://github.com/supabase/supavisor
- https://supabase.github.io/supavisor/ , /connecting/overview/ , /configuration/tenants/
- https://github.com/supabase/realtime (ARCHITECTURE.md, ENVS.md)
- https://github.com/supabase/storage/blob/master/docker-compose-multi-tenant.yml
- https://github.com/supabase/auth/issues/2621
- https://github.com/supabase/auth/issues/2622
- https://github.com/PostgREST/postgrest/pull/5087
- https://github.com/PostgREST/postgrest/issues/5086
- https://github.com/PostgREST/postgrest/issues/2798
- https://github.com/PostgREST/postgrest/issues/5266
- https://github.com/orgs/supabase/discussions/4907
- https://github.com/orgs/supabase/discussions/38048
- https://github.com/orgs/supabase/discussions/1615
- https://github.com/orgs/supabase/discussions/39820
- https://github.com/orgs/supabase/discussions/31147
- https://github.com/orgs/supabase/discussions/46081
- https://github.com/orgs/supabase/discussions/27467 (not read)

Community multi-project tooling
- https://github.com/supabase-multitenant/supabase-multitenant
- https://github.com/GustavoMartins123/supabase-multitenant
- https://github.com/M7MMAD-OMAR/sbarbase (docs/explain/architecture.md, isolation-and-trust.md, docs/reference/status.md, PROJECT_GOAL.md)
- https://github.com/smartpiai/multibase
- https://github.com/KHAEntertainment/SupaConsole
- https://github.com/sharonpraju/SupaConsole
- https://github.com/HarryET/supa-manager
- https://github.com/flamingrubberduck/supabase-studio-multi-head
- https://opensupabase.sadelabs.site/
- https://powabase.ai/self-hosted-supabase/
- https://www.supascale.app/blog/managing-multiple-supabase-projects-on-selfhosted-infrastruc
- https://docs.flxbl.io/sfp-server/setting-up/self-hosted-supabase-configuration
- https://github.com/Pukujan/octo-database/issues/71 (research charter listing alternatives; no conclusions)

PaaS installers
- https://coolify.io/docs/services/supabase
- https://github.com/coollabsio/coolify/blob/next/templates/compose/supabase.yaml
- https://github.com/coollabsio/coolify/issues/5362
- https://github.com/coollabsio/coolify/discussions/9957
- https://github.com/Dokploy/templates/tree/main/blueprints/supabase
- https://github.com/Dokploy/templates/issues/558
- https://docs.dokploy.com/docs/core/docker-compose/utilities
- https://easypanel.io/docs/templates/supabase
- https://github.com/caprover/one-click-apps/tree/master/public/v4/apps
- https://elest.io/open-source/supabase
- https://blog.elest.io/how-to-self-host-supabase-on-elestio-full-setup-guide/
- https://docs.digitalocean.com/products/marketplace/catalog/supabase
- https://community.hetzner.com/tutorials/coolify-supabase-deploy/
- https://forum.yunohost.org/t/was-supabase-ever-considered-as-a-yunohost-app/29160

Kubernetes / Postgres operators
- https://github.com/supabase-community/supabase-kubernetes
- https://github.com/supabase-community/supabase-kubernetes/pull/268
- https://github.com/STRRL/supabase-operator
- https://github.com/stack-cli/stack-cli
- https://github.com/cloudnative-pg/cloudnative-pg/discussions/2357
- https://cloudnative-pg.io/docs/1.28/declarative_database_management/
- https://cloudnative-pg.io/docs/1.28/recovery/
- https://github.com/voltade/cnpg-supabase
- https://github.com/supafull/supabase-extensions
- https://postgres-operator.readthedocs.io/en/latest/user/
- https://github.com/zalando/postgres-operator (search result only)

Postgres / poolers
- https://www.postgresql.org/docs/current/database-roles.html
- https://www.postgresql.org/docs/current/sql-createdatabase.html
- https://github.com/citusdata/pg_cron
- https://www.pgbouncer.org/config.html
- https://github.com/postgresml/pgcat
- https://chat2db.ai/resources/blog/pgcat-vs-pgbouncer-vs-supavisor (comparison blog, snippet)

Adjacent platforms
- https://docs.nhost.io/platform/cli/multiple-projects
- https://github.com/nhost/nhost
- https://appwrite.io/docs/partners/guides/multi-tenancy
- https://appwrite.io/blog/post/appwrite-2-self-hosted
- https://pockethost.io/blog/announcing-pocker
- https://pockethost.io/about
- https://github.com/pockethost/pockethost
- https://news.convex.dev/self-hosting/
- https://directus.io/docs/cloud/getting-started/introduction
- https://github.com/directus/directus/discussions/3987
- https://github.com/neondatabase/neon
- https://github.com/molnett/neon-operator
- https://neon.com/blog/architecture-decisions-in-neon
- https://docs.fly.io/blueprints/per-user-dev-environments
- https://docs.fly.io/blueprints/connecting-to-user-machines
- https://turso.tech/multi-tenancy
- https://www.instantdb.com/essays/architecture
