# Multi-tenant self-hosted Supabase: research synthesis

Date: 2026-10-06. Sources: the four reports in this directory (01 official state, 02 named-approaches teardown, 03 landscape and patterns, 04 component audit). Items marked *analysis* are our conclusions, not sourced facts.

## 1. The problem, precisely

Supabase self-hosting is one project per stack by design. The docs say so, and the self-hosting lead said in March 2026 that multi-project would need "a different architecture across the entire stack". The hosted platform's control plane (`adminapi`, `admin-mgr`, `supabase-admin-agent`, the Management API) is closed source. Nothing announced at Select 2026 (Multigres, OrioleDB, Compute, Pipelines, the Turso acquisition) changes that. The "dockerless CLI" is `supabase start --runtime native`: alpha, local-dev only, but backed by public MIT artifacts (`supabase/slim-services`) and an orchestrator (`@supabase/stack`) that already does lazy start, idle sleep and per-stack state.

## 2. What is already multi-tenant upstream (the part we can reuse as-is)

| Service | Tenant key | Tenant store | Runtime create API |
|---|---|---|---|
| Supavisor | `user.<external_id>`, SNI, or `options=reference=` | `_supavisor` schema | `PUT /api/tenants/:id` (JWT) |
| Realtime | first label of Host header | `_realtime` schema | `POST /api/tenants` (JWT) |
| Storage | `x-forwarded-host` regex capture | tenants DB | `/tenants/:id` on admin port 5001 (apikey) |
| postgres-meta | per-request encrypted connection string header | stateless | n/a |
| Logflare / Vector | `project` field on events | `_analytics` | mgmt API |
| imgproxy | stateless | n/a | n/a |

Not multi-tenant: **Postgres** (roles, `pg_cron`, `pg_net`, Vault key, supautils GUCs and PITR are all cluster-level), **GoTrue** (multi-instance mode disabled since 2021, about 9 MiB idle), **PostgREST** (single `db-uri`; tenant-routing PR closed unmerged July 2026), **Studio** (self-host mode stubs `/api/platform/*/[ref]` but ignores `[ref]`), **Edge Runtime** (isolates are multi-tenant-capable but the routing main service is user-written), **Envoy gateway** (one project's keys baked in).

This matches how Supabase hosts: Postgres + PgBouncer + GoTrue + PostgREST + Envoy live on a per-project VM (visible in the `supabase/postgres` Ansible build); Storage, Realtime, Supavisor and Edge Runtime are absent from that image and are shared fleets (inference).

## 3. The four shapes everyone has built

| Shape | Examples | Isolation | Per-project cost | Verdict (*analysis*) |
|---|---|---|---|---|
| A. Full stack per project behind a proxy | Supascale, Coolify, Dokploy, Elestio, supabase-multitenant org | Strong (own Postgres) | 0.7 to 1.6 GiB idle, 11 containers, port collisions | Simple but not lightweight. No shared services. Every project pays for Kong/Envoy, Studio, Realtime, Storage, Logflare |
| B. Kubernetes operator / Helm release per project | supabase-community operator (alpha, Oct 2026), STRRL operator | Strong | Same as A plus K8s overhead | Most actively maintained prior art. Still one StatefulSet of everything per project; Studio per project |
| C. Shared cluster, database per project | AWS sample, Sbarbase, GustavoMartins123 | Weak (shared roles, cron, Vault key, PITR, superuser Realtime) | Near zero marginal, but ~$300 to $1,460/mo floor on Aurora | Only safe for trusted operators. AWS sample has no Realtime, no pooler, public unauthenticated Studio, and vendors forks of Studio/GoTrue/Storage |
| D. Schemas or RLS inside one project | common advice | None at the Supabase level | zero | Not multi-tenant Supabase; Auth, Storage and roles are shared |

Nobody has solved Studio cleanly: every solution forks it or runs one per project. Backups are dump-and-tar everywhere except where Aurora provides PITR, and there it is whole-cluster.

## 4. The design space we are left with (*analysis*)

The hosted platform's own split is the native answer: a **shared fleet** of the services that already have tenant tables (Supavisor, Realtime, Storage, postgres-meta, imgproxy, optional Logflare), a **per-project unit** of exactly what Supabase itself puts on a project VM (Postgres + GoTrue + PostgREST, with Envoy's role moved to a tenant-aware edge), and a **small control plane** that creates a project by (1) starting the per-project unit and (2) calling the three tenant APIs. The things that have to be built or patched are short:

1. **Tenant-aware edge router.** Resolve project from the host header (`<ref>.api.example.com`), validate the project's anon/service key or JWT, and forward `/rest` and `/auth` to that project's PostgREST/GoTrue and `/realtime`, `/storage`, `/functions` to the shared fleet with the right host header. Envoy with xDS or a few hundred lines of Go. Fly's `fly-replay` and PocketHost's launcher are the pattern: the router can also start a sleeping project on first request.
2. **Studio.** Smallest fork: make `getConnectionString()` and `DEFAULT_PROJECT` honor `[ref]` and list projects from the control plane. Alternative with zero fork: implement the subset of the platform API Studio calls (`/platform/projects`, `/platform/organizations`, `/platform/pg-meta/[ref]/...`) in the control plane and run upstream Studio with `NEXT_PUBLIC_IS_PLATFORM=true`. Nobody has published a working recipe for the second route; it is the elegant one and worth a two-day spike. Either way one Studio serves every project.
3. **Per-project unit packaging.** Three options, same shape:
   - 3a. Containers: three small containers (or one pod) per project from upstream images. Works anywhere Docker or Kubernetes runs.
   - 3b. Native processes: run `postgres`, `gotrue`, `postgrest` from the `slim-services` tar.zst artifacts under systemd or a tiny supervisor, one unit per project, with lazy start and idle sleep borrowed from `@supabase/stack`. This is the "dockerless" idea applied to production. Highest density, lowest overhead; alpha artifacts, Linux glibc 2.35+ only.
   - 3c. Micro-VM per project (Firecracker / Fly Machines / EC2) using the `supabase/postgres` AMI or Nix build. Strongest isolation, closest to hosted, heaviest floor.
4. **Postgres granularity.** Cluster per project is the isolation-correct default (roles, `pg_cron`, `pg_net`, Vault, PITR all become per-project). Database per project on a shared cluster is an "economy mode" for trusted internal tenants only; keep it as a configuration, not the design.
5. **Edge Functions.** One shared `edge-runtime` with a main service that maps `(project, function)` to a code path and a per-project env array. Hosted does this with closed code; ours is a few hundred lines of TypeScript.
6. **Control plane.** Postgres-backed, Apache-2.0, small: projects table, secrets (JWT secret, anon/service keys, DB password, Realtime/Storage tenant records), lifecycle (create, pause, resume, delete, upgrade), backups (`pg_dump` plus WAL archiving per cluster via wal-g/pgBackRest to S3, which cluster-per-project makes per-project PITR). Expose it as a REST API and a CLI; Studio talks to it.

Rough per-project footprint for shape 3a/3b (*analysis, unbenchmarked*): Postgres 17 ~100 to 150 MiB idle, GoTrue ~10 MiB, PostgREST ~30 to 115 MiB, so roughly 150 to 300 MiB versus 1 to 1.6 GiB for a full stack. With idle sleep, dev and preview projects cost disk only.

## 5. Fork policy (*analysis*)

Forking is on the table. Keep patches as a short rebasing series on upstream tags rather than a hard fork: the Studio `[ref]` patch and any Envoy/Storage header tweaks are small, and upstream moves fast (Postgres 17 default June 2026, Envoy replaced Kong August 2026, Studio migrating to TanStack). Contribute the Studio change upstream if possible; the self-hosting team has hinted at "more lightweight architectures" and the community operator has Supabase staff on it.

Licensing is permissive across the stack (Apache-2.0, MIT, PostgreSQL). Unverified: trademark use of "Supabase" in a product name, and per-extension licenses in the Postgres image.

## 6. Open questions that change the design

See the questions posed in chat on 2026-10-06. Short form: tenant trust level, target project count, substrate (VM / Docker / Kubernetes / managed Postgres), Studio strategy, day-one feature parity, scale-to-zero, per-project PITR, product vs internal.
