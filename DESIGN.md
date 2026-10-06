# Multi-tenant self-hosted Supabase: final design

Status: v1.0, 2026-10-06. Supersedes v0.1 (topology) and v0.2 (thin edition). Evidence lives in `research/` (00 synthesis, 01 to 04 landscape and component audit, 05 to 07 verification). Marked *analysis* where a claim is ours rather than sourced.

## 1. In one paragraph

One static Go binary, working name `sbctl`, turns a Linux machine into a multi-project Supabase for a team. It runs Supabase's own native service artifacts as systemd units: three per project (Postgres, GoTrue, PostgREST) and a handful shared (Supavisor, Realtime, Storage, postgres-meta, Studio). The binary itself is the edge proxy with automatic TLS, the Management API that Studio, the Supabase CLI and the Supabase MCP server talk to, the project lifecycle engine, and the WAL archiver. No Docker, no Envoy, no third-party gateway, no fork of any Supabase service. A team installs it with one command or one CloudFormation click and gets a dashboard that behaves like supabase.com with up to about a hundred projects.

## 2. Why this shape is the native one

- **Hosted topology.** Supabase runs Postgres, GoTrue and PostgREST per project and Supavisor, Realtime and Storage as shared multi-tenant fleets with tenant tables and admin APIs. We do the same with the same binaries.
- **Hosted artifacts.** Supabase publishes every service as a relocatable, checksummed, SBOM'd `tar.zst` (`supabase/slim-services`, MIT) for its dockerless CLI, built from the same Nix extension set as the Postgres image. We run those.
- **Hosted proxy pattern.** The dockerless CLI's orchestrator (`@supabase/stack`) does not ship Kong or Envoy. Its own HTTP proxy routes by path prefix and rewrites publishable and secret keys to JWTs. `sbctl` does the same, per project, in Go.
- **Hosted API.** Studio, the CLI and the MCP server speak the public Management API (`v1`, `v2`, `platform` OpenAPI specs). `sbctl` implements the subset they call, with server types generated from those specs.

What Supabase never open-sourced is exactly the list of things `sbctl` is: management API, per-machine agent, edge.

## 3. Process inventory on one node

| Process | Count | Source | Role |
|---|---|---|---|
| `sbctl` | 1 | ours | control plane, Management API, HTTPS and WebSocket proxy, ACME, WAL archiver, unit renderer |
| `sb-postgres@system` | 1 | slim artifact | registry for `sbctl`, plus `_supavisor`, `_realtime`, `_storage` metadata and the dashboard `auth` schema |
| `sb-gotrue@system` | 1 | slim artifact | dashboard sign-in for Studio and PAT issuance |
| `sb-supavisor` | 1 | slim artifact | one Postgres port for every project; routes on `postgres.<ref>` |
| `sb-realtime` | 1 | slim artifact | tenants by first host label |
| `sb-storage` | 1 | slim artifact | `MULTI_TENANT=true`, tenants by `x-forwarded-host`, file backend or S3 |
| `sb-pgmeta` | 1 | slim artifact | stateless; per-request encrypted connection string from `sbctl` |
| `sb-studio` | 1 | our build of upstream tag | platform mode, three small patches |
| `sb-postgres@<ref>` | per project | slim artifact | the project's cluster, full extension set, own Vault key |
| `sb-gotrue@<ref>` | per project | slim artifact | ~12 MB |
| `sb-postgrest@<ref>` | per project | slim artifact | ~9 MB |
| optional: `sb-imgproxy`, `sb-edge-runtime` | 0 or 1 | slim artifact | image transforms; Edge Functions with our tenant-aware main service |

Eight fixed processes plus three per project. The system cluster is itself a project in `sbctl`'s eyes (`ref = system`), so there is one unit shape and one backup path for everything.

## 4. The binary

```
sbctl
├── api/         /v1, /v2, /platform  (types generated from Supabase's OpenAPI specs)
├── proxy/       HTTPS :443 + ACME (CertMagic); host -> ref; apikey and sb_* key handling;
│                /rest /auth /graphql -> project units; /realtime /storage /functions -> fleet
├── units/       renders systemd template units + env files from versions.yaml; MemoryMax/CPUQuota
├── lifecycle/   create, pause, resume, delete, rotate-keys, upgrade; tenant calls to fleet APIs
├── backup/      archive_command and restore_command are `sbctl wal push|fetch`; nightly basebackup
├── artifacts/   fetch + verify slim-services tar.zst per versions.yaml
└── cli/         sbctl install | projects | backups | self-update
```

**Routing.** `<ref>.api.<domain>` resolves the project. `/rest/v1`, `/graphql/v1` and `/auth/v1` go to that project's PostgREST and GoTrue on loopback ports `sbctl` allocated. `/realtime/v1` goes to Realtime with `Host: <ref>.realtime.internal`; `/storage/v1` goes to Storage with `x-forwarded-host: <ref>.api.<domain>`; `/functions/v1` goes to Edge Runtime with a tenant header our main service reads. `sbctl` validates `apikey` against the project's keys and translates `sb_publishable_*` and `sb_secret_*` to internal JWTs, the same logic as upstream's Envoy Lua and stack proxy. Postgres connections do not pass through `sbctl` at all: Supavisor listens on 5432 and 6543 for all projects and routes on the username suffix, which is also how hosted works.

**TLS.** Wildcard certificate by DNS-01 through CertMagic (Route 53 via the instance role on AWS; Cloudflare, Hetzner, DigitalOcean tokens elsewhere). Without a DNS API, per-project HTTP-01 certificates on first request. Without a domain, `<ref>.api.<ip>.sslip.io`. Wildcard DNS is required; there is no path-based mode.

**Backups.** Postgres's own `archive_command` calls `sbctl wal push`, which writes to S3 or a local directory. A nightly `pg_basebackup` per project gives per-project point-in-time recovery with no wal-g, no pgBackRest. Restore creates a new project from a base backup plus WAL, which is also how clone-based branching will work.

**State.** Registry, secrets (encrypted with a key in `/etc/sbctl`), refs, ports and versions live in the system Postgres. Project data lives in `/var/lib/sbctl/projects/<ref>`.

## 5. Creating a project

1. Allocate a 20-letter ref, JWT secret, anon and service keys, publishable and secret keys, DB password, loopback ports.
2. Render and start `sb-postgres@<ref>`; run the upstream init migrations and roles (the artifact ships them).
3. Render and start `sb-gotrue@<ref>` and `sb-postgrest@<ref>`.
4. `PUT /api/tenants/<ref>` on Supavisor, `POST /api/tenants` on Realtime, `POST /tenants/<ref>` on Storage.
5. Add the host route in `sbctl`'s in-memory table; request a certificate if not under the wildcard.
6. Return the project to Studio, the CLI or the MCP server in Management-API shape.

Well under a minute. Delete is the reverse, with a final base backup.

## 6. Tooling compatibility

| Tool | How it connects | Patch needed |
|---|---|---|
| Studio | our platform-mode build; `NEXT_PUBLIC_API_URL` = `sbctl`; sign-in via `sb-gotrue@system` | three upstreamable patches: hCaptcha only with a site key, env-overridable `dashboard_auth:*` flags, extra project hosts in CSP |
| Supabase CLI | `--profile` file written by `sbctl` with `api_url`, `dashboard_url`, `project_host`, `pooler_host`; PATs in `sbp_` format | none |
| Supabase MCP server | `--api-url`; project-URL and logs-dialect overrides | none; two small upstream PRs proposed |
| supabase-js and all client SDKs | project URL and keys | none |
| Agent skills | overlay that swaps the hosted MCP and dashboard URLs | none |

Management API subset: `/v1` projects, api-keys, `database/query`, migrations, `types/typescript`, functions, secrets, advisors, health, pooler config, `cli/login-role`, branches (empty list); `/platform` profile and permissions, organizations, projects, status, settings, `pg-meta/{ref}/query`, config, content, auth and storage admin proxies, entitlements and plan stub; `/v2` project config. Everything else returns an empty 200 or 204. The CLI decodes strictly, so types come from the specs and a nightly job diffs the specs against our implementation.

## 7. Install

**AWS.** Click the Launch Stack link. Fill in instance size, admin email, optional domain and hosted zone. CloudFormation creates one Ubuntu 24.04 instance, an IAM role, a security group and an S3 bucket; user data runs the installer; the stack outputs the dashboard URL and a one-time claim token. First project in under ten minutes.

**Any Linux server.** Ubuntu 22.04+ or Debian 12+ (glibc 2.35 floor). Two DNS records, then:

```bash
curl -fsSL https://get.<name>.dev | sudo bash -s -- --domain example.com --dns cloudflare
```

The installer verifies checksums, creates the `sbctl` user, writes `/etc/sbctl/config.toml`, installs the units, starts `sbctl`, and prints the dashboard URL and claim token. Re-running is idempotent and keeps secrets. `sbctl self-update` upgrades the binary; artifacts upgrade through `versions.yaml`.

Laptops are not a target. Supabase's own `supabase start --runtime native` already covers local development with the same artifacts, and a project exported from it restores into `sbctl`.

## 8. Staying in sync

1. `versions.yaml` pins every artifact release and the Studio tag. A bot opens a bump PR per upstream self-hosted release.
2. The conformance suite stands up a node, creates two projects, and runs the upstream client test suites and Studio smoke tests against both. Green merges.
3. The three Studio patches rebase in CI on each tag and are proposed upstream.
4. Nightly diff of the `v1`, `v2` and `platform` OpenAPI specs against our server.
5. Per-project rolling upgrades with per-project version pinning.

## 9. Reserved for later, designed in now

- **Idle sleep.** Because `sbctl` is the proxy, it can stop a project's GoTrue and PostgREST after an idle period and start them on the next request. Not in v1: measured Postgres idle is 15 to 20 MB, so a hundred warm projects already fit on one machine.
- **File-database engine.** The project record carries `engine: postgres | file` from day one and the data-plane sits behind a five-call interface. Turso has no open multi-tenant server yet, so nothing is built on it; when one exists it becomes one more shared process per shard, with auth and quotas in `sbctl`'s proxy.
- **Edge Functions, image transforms, Logflare.** Optional units behind flags; Functions needs our tenant-aware main service.
- **Members and RBAC, clone-based branching, restore UI, multi-node scheduling.** After the single-node product is solid.

## 10. What was cut from earlier drafts and why

| Cut | Replaced by | Reason |
|---|---|---|
| Envoy, xDS server, SDS, Lua filters | Go proxy inside `sbctl` | Supabase's own stack orchestrator is its own proxy; removes three config surfaces and a second process |
| Separate TCP edge for Postgres | Supavisor on 5432/6543 | Supavisor already routes every project on the username; hosted does the same |
| wal-g or pgBackRest | `archive_command` = `sbctl wal push`, nightly `pg_basebackup` | Postgres built-ins plus a Go S3 client; nothing to bundle |
| Docker runner | none | Native artifacts verified as a real runtime |
| Logflare and Vector in v1 | journald, `sbctl logs`, optional later | Heaviest service; Studio's logs pages tolerate empty responses |
| Path-based routing mode | wildcard DNS required; sslip.io for trials | Realtime and Storage resolve tenants from the host |
| Local-CA laptop mode | Supabase's own dockerless CLI | It already exists and uses the same artifacts |
| Separate control-plane database | registry in the system Postgres | One cluster, one backup path, same unit shape as a project |
| Separate dashboard GoTrue setup | `sb-gotrue@system` | The system project is a project |
| Idle sleep in v1 | always-on, sleep later | Density target already met warm |

## 11. Phases

Ordering only, no dates. The workstream split and shared conventions for parallel agents are in `HANDOFF.md`.

0. **Spike.** Build platform-mode Studio with the three patches. Generate a mock `/platform` and `/v1` from the specs. Sign in, list two projects, open the table editor on each, run SQL, `supabase link`, MCP `list_tables`. Exit: the captured list of endpoints Studio calls and which tolerate stubs.
1. **Single-node v1.** `sbctl` proxy, ACME, units, artifact fetch, lifecycle, fleet tenant calls, P0 API subset, WAL archiving and restore, installer, conformance suite. Exit: Auth, REST, Realtime, Storage and Studio at parity on Ubuntu 24.04; measured Linux footprint at 10, 25 and 50 projects.
2. **AWS and Functions.** CloudFormation quick-create, Edge Functions main service, imgproxy, idle sleep.
3. **Product.** Members, branching, restore UI, metrics, docs, name and trademark check, upstream PRs.

## 12. Open items

- Linux per-project RSS and cold start with the Supabase preload set at 10, 25 and 50 projects.
- Which Studio platform calls tolerate stubs (spike exit criterion).
- Storage's S3-protocol endpoint on the file backend in multi-tenant mode.
- Whether the CLI keeps `--profile` or moves to "platforms"; pin and test per bump.
- Product name: must not contain "Supabase" per the partner catalog rule; no public trademark policy exists.
