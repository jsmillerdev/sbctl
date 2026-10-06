# 02 - Named approaches teardown: Supascale, supabase-on-aws (community), sample-supabase-on-aws (AWS), plus forks and siblings

Research date: 2026-10-06. Method: live fetches of product pages and docs, plus shallow clones and `gh api` queries of the repos (README, CDK/compose code, issues, commit history). Where a claim comes from reading code, the file is named. Where I could not verify something it is marked **UNVERIFIED**. Sections labeled "Analysis" are my own inference, not a sourced fact.

---

## TL;DR

1. **Supascale is a one-stack-per-project manager, not true multi-tenancy.** Each project is its own Docker Compose stack (own Postgres container, Kong, Auth, Studio, etc.) on one Linux host, with dynamic port assignment. Docs quote ~1 GB RAM per project and ~4-5 production projects on an 8 GB box. Free CLI is GPLv3 bash; the web GUI is proprietary, one-time $99/$299/$799 with 1 year of updates. One-person-style vendor (Frog Byte LLC / "Kevin"); the CLI repo had a 7-month gap (2026-01-18 to 2026-08-12).
2. **supabase-community/supabase-on-aws (B) is effectively abandoned and single-project.** CDK, ECS Fargate (Graviton) + Aurora Serverless v2 PG 15.4 + CloudFront + Amplify-hosted Studio. Last real commit 2024-05-10 (last push of any branch 2024-07-26). Pinned images date to the Nov 2023 release (GoTrue v2.110.0, PostgREST v11.2.0, Realtime v2.25.27, Kong `:latest`). 32 open issues; Realtime is the most-reported breakage. One deployment = one Supabase project (one VPC, one NAT, one Aurora).
3. **aws-samples/sample-supabase-on-aws (C) is the only AWS option with a real multi-tenant design**, and it is young (created 2026-03-19, last commit 2026-09-10, 6 stars, 0 issues, 3 contributors). Database-per-tenant on shared Aurora "worker" clusters, a Fastify tenant-manager as control plane, Kong that mints 5-minute JWTs from opaque `sb_publishable_*`/`sb_secret_*` keys, and one PostgREST Lambda per project. Self-reported cost: ~$300/mo test, ~$1,460/mo production *platform floor*.
4. **C has two blocking gaps for us.** (a) Studio is on a public internet-facing ALB with `STUDIO_NO_AUTH_MODE=true` (every visitor is treated as project "owner"); no CDK-level auth or IP restriction (verified in `infra/lib/supabase-stack.ts` and the Studio middleware). (b) There is **no Realtime service** in the CDK stack (README test table shows Realtime 1 passed, 3 skipped), and no Analytics or pooler.
5. **C vendors forks of Studio, GoTrue and storage-api in-tree** (`app/supabase`, `app/supabase-auth`, `app/function-deploy`), so the upgrade burden of tracking upstream sits on whoever adopts it. README says it is built on Supabase commit `0c35ed5`.
6. **The best-fit open-source "shared plane, per-tenant DB" designs are small and new:** GustavoMartins123/supabase-multitenant (shared Postgres + Supavisor + Realtime + Storage + Functions + Meta, per-project Nginx/GoTrue/PostgREST, one Studio behind Authelia, FastAPI control plane, host-agent; 21 stars, created 2025-09-21) and the supabase-multitenant/supabase-multitenant org repo (per-project full stack today; ADR-0003 plans a shared plane; measured 1,616.8 MiB idle per project, 11 containers).
7. **Kubernetes is the only place a vendor-adjacent project is actively maintained:** supabase-community/supabase-kubernetes pushed 2026-10-04, now ships a new Operator (CRDs `Project`, `SingleDatabase`, `Auth`, `Rest`, `Realtime`, `Supavisor`, `Studio`, `Envoy`...) plus the legacy Helm chart. Operator is "early stage"; one `Project` references one `SingleDatabase` (a Postgres StatefulSet), so it is per-tenant Postgres instance. Supabase's own docs still say self-hosted Studio is single-project and the K8s project is community-maintained.
8. **Nobody has solved the Studio problem cleanly.** The Studio "platform" API is closed, so every multi-project solution either forks Studio (C, GustavoMartins123, superbase2, studio-multi-head) or gives each project its own Studio (Supascale, supabase-multitenant today, SupaPanel, STRRL operator). Forking means permanent upstream-merge cost.
9. **Backup/PITR is weak everywhere except where Aurora/RDS gives it for free.** Supascale = `.tar.gz` with pg dump (no PITR documented). Aurora-based B/C get native automated backups and PITR, but at whole-cluster granularity (C puts many tenant DBs in one worker cluster; per-tenant restore is therefore a dump/restore exercise - Analysis, not verified in their docs).
10. **Per-tenant cost floors differ by ~10-100x:** Docker-per-stack ~0.7-1.6 GB RAM idle per tenant; C marginal tenant is a database + a Lambda + a Secrets Manager entry (near zero, Analysis) on top of a ~$300/mo platform floor; B/K8s-per-stack multiplies a full stack (and for B a full VPC/NAT/ALB/Aurora) per tenant.

---

## A. Supascale (https://www.supascale.app/)

### What it is
- A "management platform" for running many self-hosted Supabase instances on your own VPS. Pitch: stop paying Supabase Cloud's $25/mo Pro + $10 per additional project; buy once. Creator is "Kevin", motivated by his own agency's margins (supascale.app home).
- Two parts: **Supascale Free** = open-source Bash CLI (`supascale.sh`) on GitHub, GPLv3, copyright "Frog Byte, LLC"; **Supascale paid** = proprietary Next.js web GUI + REST API + MCP server (pricing page: "partially open-source ... paid tiers add proprietary web GUI").
- GitHub: `LambdaSoftworks/Supascale` (CLI only). 37 stars, 7 forks, 2 open issues (#7 test suite PR, #8 macOS password generation bug), created 2025-05-15. Commits: 2025-10 (license, VAULT_ENC_KEY 32-char fix), 2026-01-18 ("close issues #2,#3,#4": auto-update, container updates, SSL/domain, backup), then **2026-08-12 v1.6.0** (fixed compose corruption, dead safety checks, broken stop/restore paths). Gap of ~7 months between the previous commit and v1.6.0. Last push 2026-08-12 (API). The web-app version visible in the demo UI is `v1.1.33-DEMO` (demo.supascale.app).
- Pricing (live pricing page 2026-10-06): Free $0; Personal $99; Commercial $299; Agency $799 - all one-time, "unlimited projects on servers you own", 1 year of updates, then optional renewal $49/$99/$199; Free/Personal are solo-developer use only, Commercial/Agency required for company or client work; licences non-transferable. A search-engine snippet showed "$39.99" - likely a stale earlier price; **UNVERIFIED which is current beyond the pricing page**.
- Discussion/critique: I found **no HN/Reddit threads** on Supascale (searches returned nothing specific). Supascale's own blog (marketing, SEO-style) is the main content. Treat claims as vendor claims.

### How multi-project works
- **One Docker Compose stack per project** on one host. Docs architecture page: "Each Supabase project operates as isolated Docker containers orchestrated via Docker Compose ... custom networks and named volumes ... dynamic port assignment", directory `~/projects/<name>/supabase/docker/`.
- CLI README: projects live in `$HOME/<project_id>/supabase/`, ports start at 54321 and increment by 1000 per project; each gets own `.env`, volumes.
- Created project exposes API on :8000 (per its own network, mapped to a dynamic host port), Postgres 5432, **Studio on :3001 - a Studio container per project**. 2-5 minutes to provision: validate, generate compose, pull images, create, start, health check.
- Control plane = Next.js 15 / React 19 app with **SQLite (WAL)** at `/opt/supascale-web/data/`, run under **PM2**; NextAuth v5 + bcrypt (12 rounds), AES-encrypted stored credentials, API keys for the REST API, optional 2FA.
- Supabase images: "official Supabase Docker images", configurable per project, default latest stable, pinnable (FAQ).
- Resource guidance (docs): ~1 GB RAM per project; small 1-3 projects, medium 4-10, large 10+ "distributed setup recommended"; Supascale's blog gives 700 MB-1 GB per minimal project, 8 GB server hosts 4-5 production projects, and admits "operational complexity scales with project count... non-linearly".
- Ports 80/443 (Let's Encrypt), 3000 (Supascale), 5432+ (DBs).

### Routing / domains
- One custom domain per project, bound via UI/API; Supascale **generates Nginx/Apache/Caddy server blocks** with SSL termination (Let's Encrypt HTTP-01, DNS-01 wildcard support, or BYO cert). Docs do not explain Studio domain handling or Studio access protection. The panel itself sits behind a hand-configured Nginx/Apache/Caddy proxying to 127.0.0.1:3000.

### Backup / update
- Backups: full (database + storage + functions + config), DB-only, storage-only; `.tar.gz` containing a PostgreSQL dump, storage files, functions, compose/env, `metadata.json`; optional encryption; targets local, S3, GCS, Azure, S3-compatible; scheduled via cron expressions. **No PITR/WAL docs found.**
- Updates: CLI "update individual or all services with automatic backup and rollback", post-update health checks with auto-rollback. Release cadence claim: security patches ASAP, bug fixes weekly-biweekly, features monthly (FAQ) - the CLI repo's commit history does not obviously support that cadence.

### Scorecard - Supascale

| Dimension | Finding |
|---|---|
| Isolation model | Per-tenant **full stack incl. its own Postgres container** on a shared host (separate compose project, network, volumes). Strongest data isolation; weakest density. |
| Shared components | Only host OS/Docker daemon, the reverse proxy (Nginx/Apache/Caddy), and the Supascale control plane (Next.js + SQLite + PM2). |
| Per-tenant components | Everything: Postgres, Kong, GoTrue, PostgREST, Realtime, Storage, imgproxy, Meta, Functions, Studio, pooler, analytics (up to 13 services; optional ones selectable). |
| Routing / ingress | Dynamic host ports per project; optional one domain per project via generated host-level Nginx/Apache/Caddy vhosts. No central gateway, no subdomain-per-project wildcard scheme documented (one domain per project officially). |
| Provisioning | Web UI (click), REST API (all UI features), CLI (free tier), MCP for Claude/Cursor. 2-5 min per project. |
| Upgrade story | Per-project image pinning; CLI/GUI update with backup + health-check rollback. Each project upgraded individually (N stacks to roll). |
| Backup / PITR | tar.gz dumps, scheduled, S3/GCS/Azure; **no PITR documented**. Control-plane state = copy `/opt/supascale-web/data/`. |
| Studio access model | Stock Studio container per project (port 3001); no unified multi-project Studio; no documented auth in front of Studio. |
| Auth for control plane | NextAuth v5 (credentials, bcrypt), JWT sessions, API keys, 2FA, audit logs. |
| Cost floor per tenant | ~0.7-1 GB RAM (vendor docs); one tenant per ~1 GB of VPS. $99-$799 one-time licence. Other sources measure 1.6 GiB idle for a similar 11-container stack (see D3), so **treat 1 GB as optimistic**. |
| Cloud portability | Any Linux with Docker 20.10+ / Compose v2 (Ubuntu 20.04+, Debian 11+, RHEL-family). Single host; no multi-server orchestration documented. Not AWS-native (no IAM, RDS, ALB integration). |
| Maintenance status | CLI last push 2026-08-12 (v1.6.0); web app v1.1.33 in demo. Single small vendor. |
| License | CLI GPLv3; GUI proprietary, per-tier commercial licence (Free/Personal solo only). |
| Biggest weakness | Linear cost and ops with project count; control plane holds Docker/host-level power; closed-source GUI; one-person vendor; no PITR; Studio is per-project and unprotected-by-design. |

---

## B. supabase-community/supabase-on-aws (https://github.com/supabase-community/supabase-on-aws)

### Facts
- Author: **Kazuki Matsuda (mats16)** - 103 of ~106 commits. Apache-2.0. 554 stars, 88 forks, **32 open issues** (API). Created 2022-07-23. Last real commit **2024-05-10** (README IAM policy fix, PR #91); last push any branch 2024-07-26. Releases: v0.7.0 (2023-11-24, "support edge-functions", upgrade to 2023.11) is the last of 5 listed. No blog posts by the author found (search returned none) - **UNVERIFIED that none exist**.
- IaC: AWS CDK (TypeScript, projen), ships CloudFormation templates with one-click "Launch Stack" for 8 regions (Virginia, Oregon, Ireland, Tokyo, Osaka, Singapore, Sydney, Mumbai), "stable" and "latest" variants. Main file `src/supabase-stack.ts` (32 KB) plus `supabase-db`, `supabase-studio`, `supabase-cdn`, `json-web-token`, `amazon-ses-smtp`, `aws-workmail`, `supabase-waf-stack.ts`.

### Architecture (from README and `src/*.ts`)
- **VPC** with **1 NAT gateway**. **Aurora Serverless v2 PostgreSQL 15.4** (writer + one reader; `highAvailability` flag toggles the reader, ACU min default 0.5, max 32). Parameter group preloads pg_tle, pg_stat_statements, pgaudit, pg_cron; logical replication on; `rds.force_ssl=0` (SSL not enforced).
- **ECS Fargate (Graviton2)**: Kong (`public.ecr.aws/u3p7q2r8/kong:latest`), GoTrue v2.110.0, PostgREST v11.2.0, Realtime v2.25.27, Storage API v0.43.11, imgproxy v1.2.0, postgres-meta v0.74.2. Auto-scaling. Service discovery via Cloud Map (`*.supabase.internal`).
- **Internet-facing ALB restricted to CloudFront via prefix list; CloudFront in front** (optional WAFv2). SES for SMTP (optional WorkMail), S3 for Storage, Secrets Manager + SSM for keys.
- **Studio is hosted on AWS Amplify Hosting (SSR)**, built from `supabase/supabase` `studio` branch (default tag v0.23.09) mirrored by a Lambda into **CodeCommit**. README: "Supabase Studio is open to web and can be accessed by malicious actors" - recommends Amplify Access Control. **No Cognito** - JWT-only auth.
- Limits: **no pg_graphql** (Aurora), no supabase_vault (issue #46), no pg_net (#50). Single-project by construction: Realtime env `IS_MULTITENANT=false`, tenant id `stub`.
- Task sizes micro (256 CPU/512 MB) up to 4xlarge. Deploy IAM requires ~12 services of permissions.
- Cost: **README gives no cost figure** ("review AWS pricing"). Analysis (UNVERIFIED, my arithmetic): Aurora 0.5 ACU at the $0.12/ACU-hour list price I found = ~$44/mo minimum, and engine 15.4 predates Aurora's scale-to-zero (needs 15.7+), so no auto-pause; plus a NAT gateway, ALB, CloudFront, 7+ Fargate services, Amplify. Expect a low-hundreds-of-dollars/month floor per deployment. Issue #58 reports "$2.50/day" CloudWatch cost alone.

### Most-reported problems (verified via `gh api`, sorted by comments)
- #94 Realtime Not Working (open, 13 comments, 2024-05); #90 Realtime Postgres updates unreliable (closed, 5); #1 and #66 earlier Realtime failures.
- #89 Amplify deploy fails (open, 6); #93 Studio build failed on Amplify (closed); #87 Studio dir moved `/studio` -> `/apps/studio` (open, 5, 2024-01) - upstream restructuring broke the Studio build.
- #106 CodeCommit CreateRepository not allowed (open, 2024-08) and #109 "CodeCommit discontinued" (open, 2025-05). Note: AWS re-opened CodeCommit to new customers in Nov 2025, so this is now less severe but the issues were never closed.
- #82 Kong 401 (open), #79 Kong "Invalid authentication credentials" with latest image (closed) - the unpinned `kong:latest` already bit users; #84 how to rotate JWT secrets (open); #47 upgrade-method standardization (open); #70 how to lock down Studio (closed).
- (A summarizer's mention of "#9 Launch Stack doesn't create working stack, 13 comments" did **not** appear in my `gh api` listing of issues, so I dropped it.)

### Scorecard - B

| Dimension | Finding |
|---|---|
| Isolation model | **Single project per deployment**: whole stack (VPC, NAT, ALB, Aurora, Fargate) per project. Tenant isolation = separate CloudFormation stack. |
| Shared components | None across projects (each stack self-contained). |
| Per-tenant components | Everything: VPC+NAT, Aurora Serverless v2 cluster, ECS cluster + 7 services, ALB, CloudFront, Amplify Studio, S3, SES identity, Secrets. |
| Routing / ingress | CloudFront -> ALB (prefix-list locked) -> Kong -> services; Studio separately via Amplify default domain. One project = one URL. |
| Provisioning | CloudFormation "Launch Stack" click or `cdk deploy Supabase`. No API/CLI for creating projects; a "project" is a stack. |
| Upgrade story | Bump pinned image tags in CDK and redeploy; Studio via Amplify branch param. Frozen at 2023.11 (GoTrue v2.110.0, etc.); `kong:latest` unpinned. Migration scripts exist but upgrade procedure was an open issue (#47). |
| Backup / PITR | Aurora automated backups/PITR (retention not verified in code); S3 for storage objects; no per-project tooling. |
| Studio access model | Stock Studio on Amplify, "open to web"; optional Amplify access control; no per-user auth. Single project only. |
| Auth for control plane | None (no control plane). Deploy-time IAM only; Studio basic protection manual. |
| Cost floor per tenant | A full stack + NAT + ALB + CloudFront + Aurora (0.5 ACU min, no pause on PG 15.4) each time. UNVERIFIED estimate: low hundreds $/mo. |
| Cloud portability | AWS-only (Fargate, Aurora, CloudFront, Amplify, SES, Cloud Map). 8 regions. |
| Maintenance status | **Stale**: last real commit 2024-05-10; last push 2024-07-26; 32 open issues; no release since 2023-11-24. |
| License | Apache-2.0 |
| Biggest weakness | Abandoned, ancient pins, broken Studio/CodeCommit build path, Realtime issues, no multi-project, Studio exposed. |

Forks of B (via `gh api .../forks`): none with traction (all 0-1 stars). Of note: `rcmschiavi/supabase-on-aws-no-codecommit-repo` (2025-02-08, removes CodeCommit dependency), `Simulnetic/supabase-cloudformation` (2025-07-30), `s99906209-blip/supabase-on-aws` (2026-03-18, **UNVERIFIED** content). None appear to bump the Supabase versions meaningfully; I did not diff them.

---

## C. aws-samples/sample-supabase-on-aws (https://github.com/aws-samples/sample-supabase-on-aws)

### Facts
- Created **2026-03-19**; last commit **2026-09-10** ("remove 'unsafe-eval' from Studio CSP"); last push 2026-09-17 (dependabot PRs). 39 commits in history; contributors: linnjia-aws 22, crazyoyo 8, syhxz 8, bot 1. 6 stars, 0 forks, **0 issues ever** (only dependabot PRs, 5 open, none by humans). Apache-2.0. ~27 MB. No releases. A single Chinese-language upgrade guide in `docs/` (`客户升级指南.html`, "customer upgrade guide") suggests AWS China/Greater China solution-architect origin (inference).
- **No accompanying AWS blog post found** (searched; only the repo and the `multi-tenancy-blog-post-collection` index appeared) - **UNVERIFIED that none exists**.
- Built from Supabase commit `0c35ed5` (README). Docs: `README.md`, `architecture-diagrams.md` (169 KB), `customer-deployment-guide.md`, `aurora-pg-serverless-setup-guide.md`. 104 tests across six suites (Studio CRUD, Auth, RLS, tenant isolation, Edge Functions...).
- Differs from B: B is "one Supabase = one stack"; C is a **platform** (tenant-manager + multi-tenant GoTrue/Storage + Kong gateway) and **deliberately not stock Supabase**: PostgREST runs as per-project Lambdas, API keys are the new opaque `sb_publishable_*`/`sb_secret_*` format.

### Architecture (README + `infra/lib/supabase-stack.ts` + `architecture-diagrams.md` + tenant-manager code)
- Request path: `Kong ALB -> Kong (ECS) -> dynamic-lambda-router -> PostgREST Lambda (SigV4) -> Worker Aurora`. Kong `pre-function` maps **subdomain -> project id** (`<ref>.<baseDomain>`, wildcard ACM cert, you CNAME `*.baseDomain` to the ALB). `key-auth` identifies consumers `{ref}--anon`/`{ref}--service_role`; the Lua plugin mints 5-minute HS256 JWTs from a Redis-cached tenant secret and invokes the project's Lambda.
- Control plane = **tenant-manager** (Fastify, ECS, port 3001): `POST /project/create-pgrest-lambda`, `GET /project/:id/config`, `/postgrest-config`, `POST /project/:id/api-keys`. Provisioner (`provisioner.service.ts`) runs `CREATE DATABASE "<db>" WITH TEMPLATE "<template>" OWNER postgres` (with a legacy non-template fallback), initializes roles + RLS, creates `postgrest-{tenant}` Lambda, registers Kong consumers, stores metadata in `supabase_platform`.
- **Data tier**: *Platform Aurora* (`supabase_platform` DB: Kong config, projects, keys) + **Worker Aurora** cluster(s) holding many tenant DBs (`supabase_project_alpha`, own owner role, `anon`/`service_role`). README prose says "each project receives its own dedicated Aurora cluster", but the deployment guide, the architecture diagrams and the code say **database-per-tenant on a shared worker cluster**; `create-rds-and-project.sh` can *optionally* add a new worker cluster (`supabase-worker-<suffix>`) and place a project there. I treat the code/guide as authoritative: **per-tenant database on shared cluster, with an escape hatch to per-tenant cluster**. Aurora PostgreSQL 16.8, serverless v2, ACU 0.5-4 test / 1-16 prod, 7/30-day backup retention, prod deletion protection.
- **Shared services (ECS Fargate, 2-20 task autoscaling)**: Kong, tenant-manager, Studio (public ALB), function-deploy (Next.js), Functions service (Deno, EFS-backed), GoTrue (multi-tenant mode via `GOTRUE_MULTI_TENANT_TENANT_MANAGER_URL`), Storage API (`MULTI_TENANT=true`, S3), postgres-meta. Plus ElastiCache Redis 7.1 (t3.micro test / r6g.large x2 prod), EFS, WAFv2 (rate limit 2000/IP, SQLi and common rules in count mode), CloudWatch alarms + SNS. No CloudFront, no Cognito, no Route53 resources.
- **Not present**: Realtime (CDK has no Realtime service; README test table 1 pass / 3 skipped), Analytics/Logflare, Supavisor/pooler (PostgREST Lambdas connect to Aurora directly - connection-count pressure is a risk, Analysis).
- **Studio**: `STUDIO_NO_AUTH_MODE: 'true'` set in the CDK for function-deploy; middleware (`project-isolation-middleware.ts`) in that mode grants `userId: 'system'`, `accessType: 'owner'`, all permissions. Studio ALB is internet-facing with only TLS and WAF. **The multi-project Studio is therefore an open admin console unless you add auth yourself.**
- Images all default to `:latest` from your own ECR (you build them with `build-and-push.sh`; Docker linux/amd64; Go build of auth can hang -> goproxy workaround).
- Upgrade: two documented paths - image rebuild + force ECS redeploy, or CDK infra change; zero-downtime runbook for existing deployments (commit 2026-06-30). Recent commits show **self-heal of auth schema on startup**, storage-api fork bumped v1.48.26 -> v1.61.7, per-tenant Google OAuth API, Studio CSP hardening, grype/semgrep/gitleaks configs - i.e. active, security-conscious development, but by ~2-3 people.
- Cost (README): test ~$300/mo (2 AZ, 1 NAT, Aurora 0.5-4 ACU x2, t3.micro Redis, 7 small Fargate services); production ~$1,460/mo (3 AZ, 2 NAT, 1-16 ACU, r6g.large multi-AZ Redis, 2 replicas/service). **Marginal tenant cost is not stated**; Analysis: one empty-ish database on an existing worker + one Lambda + Secrets Manager entries (~$0.40/secret/mo list price, UNVERIFIED) = near zero idle, but connection pressure and noisy neighbors scale with tenants.

### Scorecard - C

| Dimension | Finding |
|---|---|
| Isolation model | **Per-tenant database on a shared Aurora worker cluster** (own DB, owner role, anon/service_role); optional per-tenant-cluster via script. Per-tenant PostgREST Lambda. Per-tenant API keys/JWT secret. |
| Shared components | Kong, tenant-manager, Studio, function-deploy, Functions runtime, GoTrue (multi-tenant mode), Storage API (multi-tenant mode), postgres-meta, Redis, platform Aurora, worker Aurora, ALBs, EFS, WAF. |
| Per-tenant components | Database, PostgREST Lambda function, Kong consumers + key-auth credentials, Secrets Manager entries, tenant rows in platform DB. |
| Routing / ingress | Wildcard `*.baseDomain` -> internet-facing ALB -> Kong; subdomain = project ref. Separate `studio.<baseDomain>` ALB. Custom per-tenant domains not documented. |
| Provisioning | Studio "create project" -> tenant-manager API; or CLI scripts (`provision-worker-and-create-project.sh`, `create-rds-and-project.sh`). API-driven. |
| Upgrade story | Rebuild 8 images in your ECR and force ECS redeploy, or CDK deploy; full runbook; **vendored forks** of Studio/auth/storage mean you merge upstream yourself. Tenant DBs from template, with startup auth-schema self-heal for drift. |
| Backup / PITR | Aurora automated backups (7 d test / 30 d prod) with PITR at cluster level; deletion protection + RETAIN in prod. Per-tenant restore not documented (Analysis: dump/restore). S3 versioning disabled. |
| Studio access model | Single multi-project Studio (fork), **no auth** (`STUDIO_NO_AUTH_MODE=true`), public ALB. |
| Auth for control plane | Admin API key (Secrets Manager) for tenant-manager; no user-level auth/RBAC on Studio. Gateway keys are opaque, JWTs minted per request. |
| Cost floor per tenant | Platform floor ~$300/mo (test) to ~$1,460/mo (prod); marginal per tenant very low (Analysis, UNVERIFIED). |
| Cloud portability | **AWS-only** (Lambda, Aurora, ElastiCache, ECS Fargate, EFS, Secrets Manager, SigV4). No portability path. |
| Maintenance status | Active: last commit 2026-09-10, 39 commits since 2026-03-19; 3 contributors; sample-code status (no SLA, no releases, no issues). |
| License | Apache-2.0 |
| Biggest weakness | No Studio auth + no Realtime/pooler/analytics + vendored forks of Studio/GoTrue/Storage with unproven upstream-tracking; PostgREST-as-Lambda diverges from stock Supabase semantics (long-lived connections, extensions, `pg_net`, etc. - Analysis). |

Forks of C: none upstream-listed; `jerdog/sample-supabase-on-aws` ("fixes for personal use", 1 star, 2026-09-17) exists but I did not diff it.

---

## D. Forks and siblings

### D1. supabase-community/supabase-kubernetes - Helm chart + new Operator
https://github.com/supabase-community/supabase-kubernetes - 822 stars, Apache-2.0, 36 open issues, created 2022-01-05, **pushed 2026-10-04**; releases `supabase-0.8.0` (chart, 2026-09-05), `supabase-operator-0.1.3` / `v0.1.3` (2026-07-23), `supabase-project-0.1.0` (2026-07-18). Maintainer hand-off: issue #120 (2025-09) "Passing the torch" - the original maintainer said he no longer had time and called for new maintainers; recent commits come from Luiz Felipe Machado. Now two approaches: the legacy single-stack Helm chart (`charts/supabase`) and a **new Operator** (`core.supabase.io/v1alpha1`: `Project`, `SingleDatabase`, `Auth`, `Rest`, `Meta`, `Realtime`, `Supavisor`, `Storage`, `Studio`, `Envoy`, `EdgeRuntime`, `Function`, `Migration`). README: "Operator is in an early stage of development and its API may change"; "supported by the community and not officially supported by Supabase". `supabase-project` chart creates a `SingleDatabase` (operator-managed Postgres StatefulSet), a `Project`, and component CRs; Studio behind Envoy with basic auth; optional ingress. In code `DatabaseRef.Kind` is enum-restricted to `SingleDatabase`, so today it is **one Postgres instance per Project**. **Multi-project = N `Project` CRs (namespace per tenant is natural)**; the README does not claim multi-tenant sharing. Open issue #198 (2026-06): helm upgrade silently reverts JWT/DB secrets to public demo values. Open issue #53 roadmap (14 comments), #85 multi-node roadmap. Supabase docs (self-hosting page) list K8s as community-maintained and state self-hosted Studio is single-project.

### D2. STRRL/supabase-operator
https://github.com/STRRL/supabase-operator - MIT, 10 stars, created 2025-09-30, pushed 2026-10-03, 7 open issues. A separate Go operator: one `SupabaseProject` CRD manages Kong 2.8.1, GoTrue 2.177.0, PostgREST 12.2.12, Realtime 2.34.47, Storage 1.25.7, Meta 0.91.0, Studio (2025.10.01 build). "Multi-tenant: multiple projects per namespace." **External Postgres and S3 are user-provided** (CloudNativePG + MinIO quickstart), so database-per-tenant (or any) topology is up to you. Studio protected by dashboard basic-auth secret. Per-project service stack, no shared plane. Image pins in README are about a year old (**UNVERIFIED** whether the code defaults differ).

### D3. supabase-multitenant/supabase-multitenant (the "webboxes" panel)
https://github.com/supabase-multitenant/supabase-multitenant - MIT, 24 stars, created 2026-09-09, v1.0.0 (2026-09-09), v1.1.0 (2026-09-23), 108 open issues (mostly planning/ADR-style), pushed 2026-09-28. Next.js 15 + Prisma + Postgres control plane, Traefik v3 with Let's Encrypt, per-project **full Docker Compose stack**, custom domains per project, install via `curl | sh` or Coolify one-click, Docker Hub image `webboxes/supabase-multitenant`. The panel **mounts `/var/run/docker.sock`**. Issue #118 (ADR-0003, a "reason the product exists") records a measured baseline: **one idle project = 11 containers, 1,616.8 MiB** (studio 279, realtime 258, pooler 254, storage 180, db 152, meta 112, functions 104, imgproxy 94, api-gw 94, rest 51, auth 40); 10 projects = 15.8 GiB / 110 containers; 50 = 78.9 GiB. Plan: hoist gateway (envoy), Supavisor, imgproxy, one Studio into a shared plane; keep db/auth/rest/realtime/storage/functions per project; "shared Postgres with per-tenant schemas explicitly rejected". Phases 0-6 unchecked as of fetch (issue #120 "Phase 1" open). This is the best public measurement I found of the "stack per tenant" tax. Very young, fast-moving, AI-assisted-looking issue stream - **treat as pre-1.0 despite the v1.1.0 tag**.

### D4. GustavoMartins123/supabase-multitenant
https://github.com/GustavoMartins123/supabase-multitenant - Apache-2.0, 21 stars, created 2025-09-21, pushed 2026-10-06; 14 open issues, mostly dependabot. README: "unofficial project under active development." Architecture (README + mermaid): **one Postgres cluster, database `_supabase_<name>` per project**; **shared** Supavisor, a *modified* Realtime, Storage API in official multi-tenant mode (objects namespaced by tenant UUID), ImgProxy, Edge Functions, Postgres Meta, Analytics/Logflare + Vector, key-authorizer (+ Redis, GeoIP), Traefik, Projects API (FastAPI); **per project**: Nginx, GoTrue, PostgREST, DB, config dir. **Single Studio** managed via an OpenResty/Lua gateway, protected by **Authelia**, with a Flutter project selector. Opaque publishable/secret API-key slots with rotation, per-key geo/CIDR/rate/quota policies. The Projects API does **not** touch the Docker socket; lifecycle intents are HMAC-signed rows leased by a systemd `host-agent` - the most careful control-plane/host-privilege design I found. Single machine or two-machine (admin box + server) layouts; Linux + Docker + Python 3.10. No cloud IaC, no HA story. This is closest to the "per-tenant DB on shared Postgres + shared services" model, but it is a Docker-compose-on-one-host design with a Studio fork/gateway to maintain.

### D5. SupaPanel (alanfrigo, flavioduque fork) and SupaConsole
https://github.com/alanfrigo/SupaPanel - MIT, 17 stars, created 2025-12-16, pushed 2026-09-16, 18 open issues; fork flavioduque/SupaPanel (pushed 2026-10-02). Based on sharonpraju/SupaConsole; Next.js panel + Docker Compose + Traefik (installs on Dokploy), per-project stack (own Postgres, Auth, Storage, Realtime, Functions, Studio, private network), Companies/RBAC (owner/admin/developer/viewer), per-project branches and cloning (pause source, copy DB + files), integrated table/SQL editor, direct/session/transaction pooler connection strings. Panel needs the Docker socket. KHAEntertainment/SupaConsole (0 stars, no licence, pushed 2026-10-06) is another fork. Same per-stack tax as Supascale, open source (MIT) rather than paid.

### D6. Sbarbase (M7MMAD-OMAR/sbarbase)
https://github.com/M7MMAD-OMAR/sbarbase - Apache-2.0, 2 stars, created 2026-09-20, v0.1.0 (2026-09-21), package 0.2.0 "development snapshot". "Many Supabase projects. One server." Runs original Supabase services (PostgreSQL, Auth, PostgREST, Storage) for clients > projects > environments; shared PostgreSQL and Storage with separate DB, logins and keys per environment; gateway fair-share scheduling with borrowing; alerts by email/webhook/Telegram. Self-states "no fixed project count is validated" and "independent public-server and production acceptance remain open". First named in Supabase discussion #38048 as the "separate databases + auth services per project on one Postgres cluster" option. Too new to rely on.

### D7. SuperBase2 (zbcoding/superbase2) and Studio multi-head
https://github.com/zbcoding/superbase2 - Apache-2.0, 0 stars, created 2026-03-27, pushed 2026-10-06. "Experimental / Hobby-use." One Postgres, one Kong, one Studio (forked, with project switcher), imgproxy, analytics, pooler shared; GoTrue, PostgREST, Realtime, Storage, Functions, Meta per project, each pointed at its own database; routing by project ref in the URL path through Kong :8000; stock images except Studio. https://github.com/flamingrubberduck/supabase-studio-multi-head (9 stars, created 2026-04-14, pushed 2026-07-28, licence NOASSERTION) is a Studio fork adding multi-project/organization support on Docker self-hosted. Both are Studio-fork approaches with the upstream-merge cost noted above.

### D8. Pigsty Supabase template (pgsty/pigsty)
https://pigsty.io/docs/app/supabase/ and https://github.com/pgsty/pigsty - 5,767 stars, Apache-2.0, pushed 2026-09-28. Not a multi-project system: it deploys **one** Supabase per run, with Pigsty managing PostgreSQL (HA via Patroni, PITR, monitoring; PG 15-18) and MinIO/"Silo" for object storage, while the stateless Supabase services run via Docker Compose. Minimum 2 vCPU/4 GB (4/8 recommended). Relevant as the strongest *data-tier* story (HA, PITR, extensions) to put under any of the shared-Postgres designs; multi-tenant behavior is **not documented there** (my search found no mention).

### D9. Coolify / Dokploy / Railway / Fly / Hetzner recipes
- **Coolify**: official one-click "Supabase" service template (compose); Hetzner community tutorial (https://community.hetzner.com/tutorials/coolify-supabase-deploy/). "Multiple projects" = deploy the template again per project. Dokploy has a Supabase template as well (per search-result summary). 
- **Railway**: several community "Deploy & Host Supabase" templates (12-service stack; "PG On Rails" fork of the compose). One template instance per project; Railway project per tenant.
- **Fly.io**: `supabase/self-hosted-edge-functions-demo` (93 stars, last push 2023-05-09 - only edge functions) and `SupaAutoFly/SupaAutoFly` (2 stars, MIT, script for Supabase on Fly, pushed 2026-04-02, 5 open issues). Render: no Supabase blueprint found in my search - **UNVERIFIED**.
- **Terraform/EKS**: my searches found no maintained Terraform module for Supabase on ECS/EKS; only `DiegoCruzcr/self-hosting-supabase-eks-terraform` (0 stars, created 2026-03-05, pushed 2026-04-09), a personal repo. StackGres published (2023-04-06) a single-project Supabase-on-StackGres recipe (PG 14, PgBouncer, Helm chart) - dated.
- Supabase's own stance (supabase.com/docs/guides/self-hosting, discussions #38048 and #39820): "you need a new instance per project"; Studio single-project; Kubernetes/CloudFormation templates acknowledged as outdated by staff in #39820; no announced multi-project roadmap in those threads.

### D scorecard (condensed; columns are the strongest siblings)

| Dimension | K8s Operator (community) | supabase-multitenant (webboxes) | GustavoMartins123 | SupaPanel | Sbarbase | superbase2 |
|---|---|---|---|---|---|---|
| Isolation | Per-tenant Postgres instance (SingleDatabase StatefulSet) | Per-tenant full stack (shared plane planned) | Per-tenant DB on shared Postgres | Per-tenant full stack | Per-environment DB on shared Postgres | Per-project DB on shared Postgres |
| Shared | Operator controller only | Panel, Traefik, panel DB | Postgres, Supavisor, Realtime, Storage, Functions, Meta, Studio, Logflare | Panel, Traefik | PG, Storage, gateway | PG, Kong, Studio, imgproxy, analytics, pooler |
| Per-tenant | All components + DB | All 11 containers (~1.6 GiB idle) | Nginx, GoTrue, PostgREST, DB | Everything | Auth/REST per env (UNVERIFIED) | GoTrue, PostgREST, Realtime, Storage, Functions, Meta |
| Routing | Envoy per Project + Ingress | Traefik, domain per project | Traefik `/<public_ref>` path + per-project Nginx | Traefik, API/Studio domains | Gateway w/ fair-share | Kong path by project ref |
| Provisioning | `kubectl apply` / Helm | Web panel, one command | Studio + FastAPI + host-agent | Web panel | Bootstrap script + API | `/sb2` dashboard |
| Upgrade | Helm/CRD upgrades; migrations CR | Manual per stack | Rebuild + scripts | Panel image + per-stack | Guide exists | `git pull` + compose pull |
| Backup/PITR | BYO (CNPG etc.) | Not documented | Not found in README (**UNVERIFIED**) | Branch clone | Guide exists | Not documented |
| Studio | Studio per project, basic auth | Studio per project | Single Studio + Authelia | Studio per project behind gateway | n/a | Forked Studio w/ switcher |
| Control-plane auth | K8s RBAC | Auth.js, first-user admin | Authelia + RBAC | NextAuth + Company RBAC | Operator file | Panel login |
| Cloud portability | Any K8s | Any Docker host | Any Linux Docker host | Dokploy/any Docker | Fedora/Docker host | Docker/Coolify |
| Maintenance | Active (2026-10-04), early | Active (2026-09-28), pre-1.0 | Active (2026-10-06), solo | Active (2026-09-16) | Dev snapshot | Experimental |
| License | Apache-2.0 | MIT | Apache-2.0 | MIT | Apache-2.0 | Apache-2.0 |

---

## Cross-comparison (A, B, C and the closest alternatives)

| Dimension | Supascale | B: community supabase-on-aws | C: aws-samples sample | K8s Operator | GustavoMartins123 |
|---|---|---|---|---|---|
| Isolation | Stack per tenant (own PG) | Stack per tenant (own VPC+Aurora) | **DB per tenant on shared Aurora** | PG instance per tenant | DB per tenant on shared PG |
| Shared infra | Host + control plane only | None | Kong, tenant-mgr, Studio, Auth, Storage, Functions, Meta, Redis, 2 Aurora | Operator only | Most services |
| Tenant creation | UI/API/CLI, 2-5 min | New CloudFormation stack | API/UI/script; DB from template + Lambda | CR apply | UI/API, host-agent |
| Studio | Per project | Single project | Single, multi-project, **no auth** | Per project | Single + Authelia |
| Realtime | Yes (per stack) | Yes (buggy, #94) | **No** | Yes | Yes (modified, shared) |
| PITR | No (dumps) | Aurora native | Aurora native (cluster level) | BYO | Not found |
| Cost floor / tenant | ~0.7-1.6 GiB RAM | UNVERIFIED low hundreds $/mo | ~$300/mo platform, marginal ~0 | Stack RAM + PVC | Lower than per-stack (shared) |
| Portability | Any Linux | AWS only | AWS only | Any K8s | Any Linux |
| Maintenance | CLI 2026-08-12; GUI v1.1.x | Stale 2024-05 | Active 2026-09 (sample) | Active 2026-10 (early) | Active 2026-10 (solo) |
| Licence | GPLv3 CLI + proprietary GUI | Apache-2.0 | Apache-2.0 | Apache-2.0 | Apache-2.0 |

---

## What they all get wrong / leave open (Analysis - my own judgment, not sourced claims)

1. **Studio is the unsolved coupling.** Studio's platform API (orgs, projects, billing, management API) is closed (Supabase docs list the Management API among features unavailable when self-hosting). Every approach picks the least-bad option: per-project Studio (simple, N copies of ~280 MiB, no cross-project view) or a forked Studio (one pane, permanent rebase tax). C's vendored Studio fork already sits on a ~2026 snapshot with `STUDIO_NO_AUTH_MODE`; nobody has published an upstream-tracking strategy. A thin, stable control plane that exposes the *platform API contract* to unmodified Studio would beat all of these, but nobody ships one.
2. **Control-plane privilege is under-designed.** Supascale, supabase-multitenant and SupaPanel need the Docker socket (root-equivalent); only GustavoMartins123 separates intent from execution (signed intents + host-agent). C's tenant-manager has IAM to create Lambdas and secrets but its console has no user auth.
3. **Isolation vs. density is a false binary.** The measured tax is ~1.6 GiB idle per stack (D3), dominated by services that *could* be shared (Studio, pooler, gateway, imgproxy, Realtime, Storage, Meta). Realistic sharing candidates verified in these repos: Supavisor (multi-tenant by design), Storage API (official `MULTI_TENANT`), imgproxy, gateway, Studio (with a fork); Realtime needed modification in GustavoMartins123 and was dropped in C; GoTrue needs a fork or per-tenant instance (C uses a multi-tenant mode with a tenant-manager). What nobody does: a tiered model (shared plane for small tenants, dedicated stack or dedicated cluster for large ones) with live migration between tiers.
4. **No per-tenant PITR or tenant-level restore.** Supascale uses dumps; C/B rely on Aurora cluster-level backups, which restore whole clusters, not one tenant DB among many. None integrates pgBackRest/WAL-G per tenant, nor tests restore drills (supabase-multitenant mentions a "restore drill" project name but I could not verify tooling).
5. **Upgrades are per-stack and unmanaged.** No fleet-level story: canary tenants, per-service version skew limits, Postgres major upgrades, migration ordering (Supabase staff themselves list "Postgres init scripts run once; image updates don't migrate" as the top pain, discussion #39820). C added an auth-schema self-heal on boot, which is a symptom of the same gap.
6. **No tenant lifecycle economics.** None offers hibernation/scale-to-zero for idle tenants, quotas, metering, or noisy-neighbor controls beyond rate limiting (GustavoMartins123 and Sbarbase partly). C could use Aurora Serverless v2 scale-to-zero (PG 16.8 supports it; their config sets min 0.5 ACU), but with a shared worker cluster it only helps if the *whole* cluster idles.
7. **Maintenance concentration.** B: one author, abandoned. C: 2-3 AWS-side authors, "sample" status, no issues channel in use. Supascale: single small vendor. GustavoMartins123 / supabase-multitenant / Sbarbase / superbase2: months old, 0-24 stars. The only repo with community breadth is supabase-kubernetes, which itself just changed maintainers and rewrote its core.
8. **Portability is mostly accidental.** Docker-host designs are portable but not HA; AWS designs (B, C) are HA-capable but fully AWS-coupled (Lambda/SigV4/Aurora). Nobody delivers one control plane that targets both a plain Linux host and managed cloud primitives (RDS/EC2/ECS/Fargate) behind a common provisioner interface.
9. **Security posture gaps recurring:** Studio exposed without auth (B, C, Supascale undocumented); `rds.force_ssl=0` in B; unpinned `:latest` images (B Kong, C all services); public demo-credential fallbacks in Helm upgrades (supabase-kubernetes #198).

### Open questions I could not close
- Supascale web-app source/changelog/version history (no public changelog found in docs nav).
- Whether any Supascale-proprietary feature (MCP, API) has had security review.
- B forks' actual diffs (not inspected). C fork `jerdog/...` diff not inspected.
- C: whether Aurora min capacity can be set to 0 ACU through `config.json` (code shows 0.5 floor in templates); whether CDK sets Aurora backup retention for the platform cluster the same as worker.
- Any AWS blog/workshop for C (none found).
- Marginal per-tenant cost for C (not stated; my estimate is unmeasured).
- Aurora/NAT/ALB pricing figures other than ACU $0.12/hr were not re-verified live (used only qualitatively).

---

## Sources

Supascale
- https://www.supascale.app/
- https://www.supascale.app/pricing
- https://www.supascale.app/docs
- https://www.supascale.app/docs/introduction/architecture
- https://www.supascale.app/docs/introduction/features-overview
- https://www.supascale.app/docs/getting-started/system-requirements
- https://www.supascale.app/docs/getting-started/creating-first-project
- https://www.supascale.app/docs/configuration/web-server-setup
- https://www.supascale.app/docs/domains/binding-domains
- https://www.supascale.app/docs/backups/creating-backups
- https://www.supascale.app/docs/faq/general-questions
- https://www.supascale.app/blog/managing-multiple-supabase-projects-on-selfhosted-infrastruc
- https://www.supascale.app/blog/multitenant-architecture-for-selfhosted-supabase-a-complete-
- https://demo.supascale.app/
- https://github.com/LambdaSoftworks/Supascale (README, `api.github.com/repos/LambdaSoftworks/Supascale`, `/commits`, `/issues`)

supabase-on-aws (B)
- https://github.com/supabase-community/supabase-on-aws (README, `src/supabase-stack.ts`, `src/supabase-db/index.ts`, `src/supabase-studio/index.ts`, `containers/`)
- `gh api repos/supabase-community/supabase-on-aws` (+ `/issues`, `/commits`, `/releases`, `/forks`)
- https://github.com/rcmschiavi/supabase-on-aws-no-codecommit-repo
- https://aws.amazon.com/blogs/devops/aws-codecommit-returns-to-general-availability (CodeCommit reversal; via search result)
- https://aws.amazon.com/about-aws/whats-new/2024/11/amazon-aurora-serverless-v2-scaling-zero-capacity

sample-supabase-on-aws (C)
- https://github.com/aws-samples/sample-supabase-on-aws (README, `customer-deployment-guide.md`, `architecture-diagrams.md`, `infra/lib/supabase-stack.ts`, `app/tenant-manager/src/modules/provisioning/provisioner.service.ts`, `app/function-deploy/apps/studio/lib/api/project-isolation-middleware.ts`)
- `gh api repos/aws-samples/sample-supabase-on-aws` (+ `/issues`, `/pulls`, `/contributors`, `/commits`)
- https://github.com/jerdog/sample-supabase-on-aws

Siblings / alternatives
- https://github.com/supabase-community/supabase-kubernetes (+ issues #53, #85, #120, #198)
- https://github.com/STRRL/supabase-operator
- https://github.com/supabase-multitenant/supabase-multitenant (+ issue #118)
- https://github.com/GustavoMartins123/supabase-multitenant
- https://github.com/alanfrigo/SupaPanel , https://github.com/flavioduque/SupaPanel , https://github.com/KHAEntertainment/SupaConsole
- https://github.com/M7MMAD-OMAR/sbarbase
- https://github.com/zbcoding/superbase2
- https://github.com/flamingrubberduck/supabase-studio-multi-head
- https://github.com/pgsty/pigsty , https://pigsty.io/docs/app/supabase/
- https://github.com/SupaAutoFly/SupaAutoFly , https://github.com/supabase/self-hosted-edge-functions-demo
- https://github.com/DiegoCruzcr/self-hosting-supabase-eks-terraform
- https://stackgres.io/blog/running-supabase-on-top-of-stackgres/
- https://community.hetzner.com/tutorials/coolify-supabase-deploy/
- https://railway.com/deploy/supabase , https://railway.com/deploy/supabase-self-hosted-full-stack

Supabase official stance and discussions
- https://supabase.com/docs/guides/self-hosting
- https://github.com/orgs/supabase/discussions/38048
- https://github.com/orgs/supabase/discussions/39820
- https://github.com/orgs/supabase/discussions/4907
- https://github.com/orgs/supabase/discussions/31147 (CNPG, listed only; not read)
