# Multi-tenant self-hosted Supabase: final design

Status: v1.0, 2026-10-06. Supersedes v0.1 (topology) and v0.2 (thin edition). Evidence lives in `research/` (00 synthesis, 01 to 04 landscape and component audit, 05 to 07 verification). Marked *analysis* where a claim is ours rather than sourced.

## 1. In one paragraph

One static Go binary, working name `supavise`, turns a Linux machine into a multi-project Supabase for a team. It runs Supabase's own native service artifacts as systemd units: three per project (Postgres, GoTrue, PostgREST) and a handful shared (Supavisor, Realtime, Storage, postgres-meta, Studio). The binary itself is the edge proxy with automatic TLS, the Management API that Studio, the Supabase CLI and the Supabase MCP server talk to, the project lifecycle engine, and the WAL archiver. No Docker, no Envoy, no third-party gateway, no fork of any Supabase service. A team installs it with one command or one CloudFormation click and gets a dashboard that behaves like supabase.com with up to about a hundred projects.

## 2. Why this shape is the native one

- **Hosted topology.** Supabase runs Postgres, GoTrue and PostgREST per project and Supavisor, Realtime and Storage as shared multi-tenant fleets with tenant tables and admin APIs. We do the same with the same binaries.
- **Hosted artifacts.** Supabase publishes every service as a relocatable, checksummed, SBOM'd `tar.zst` (`supabase/slim-services`, MIT) for its dockerless CLI, built from the same Nix extension set as the Postgres image. We run those.
- **Hosted proxy pattern.** The dockerless CLI's orchestrator (`@supabase/stack`) does not ship Kong or Envoy. Its own HTTP proxy routes by path prefix and rewrites publishable and secret keys to JWTs. `supavise` does the same, per project, in Go.
- **Hosted API.** Studio, the CLI and the MCP server speak the public Management API (`v1`, `v2`, `platform` OpenAPI specs). `supavise` implements the subset they call, with server types generated from those specs.

What Supabase never open-sourced is exactly the list of things `supavise` is: management API, per-machine agent, edge.

## 3. Process inventory on one node

| Process | Count | Source | Role |
|---|---|---|---|
| `supavise` | 1 | ours | control plane, Management API, HTTPS and WebSocket proxy, ACME, WAL archiver, unit renderer |
| `supavise-postgres@system` | 1 | slim artifact | registry for `supavise`, plus `_supavisor`, `_realtime`, `_storage` metadata and the dashboard `auth` schema |
| `supavise-gotrue@system` | 1 | slim artifact | dashboard sign-in for Studio and PAT issuance |
| `supavise-supavisor` | 1 | slim artifact | one Postgres port for every project; routes on `postgres.<ref>` |
| `supavise-realtime` | 1 | slim artifact | tenants by first host label |
| `supavise-storage` | 1 | slim artifact | `MULTI_TENANT=true`, tenants by `x-forwarded-host`, file backend or S3 |
| `supavise-pgmeta` | 1 | slim artifact | stateless; per-request encrypted connection string from `supavise` |
| `supavise-studio` | 1 | our build of upstream tag | platform mode, three small patches |
| `supavise-postgres@<ref>` | per project | slim artifact | the project's cluster, full extension set, own Vault key |
| `supavise-gotrue@<ref>` | per project | slim artifact | ~12 MB |
| `supavise-postgrest@<ref>` | per project | slim artifact | ~9 MB |
| optional: `supavise-imgproxy`, `supavise-edge-runtime` | 0 or 1 | slim artifact | image transforms; Edge Functions with our tenant-aware main service |

Eight fixed processes plus three per project. The system cluster is itself a project in `supavise`'s eyes (`ref = system`), so there is one unit shape and one backup path for everything.

## 4. The binary

```
supavise
├── api/         /v1, /v2, /platform  (types generated from Supabase's OpenAPI specs)
├── proxy/       HTTPS :443 + ACME (CertMagic); host -> ref; apikey and sb_* key handling;
│                /rest /auth /graphql -> project units; /realtime /storage /functions -> fleet
├── units/       renders systemd template units + env files from versions.yaml; MemoryMax/CPUQuota
├── lifecycle/   create, pause, resume, delete, rotate-keys, upgrade; tenant calls to fleet APIs
├── backup/      archive_command and restore_command are `supavise wal push|fetch`; nightly basebackup
├── artifacts/   fetch + verify slim-services tar.zst per versions.yaml
└── cli/         supavise install | projects | backups | self-update
```

**Routing.** `<ref>.api.<domain>` resolves the project. `/rest/v1`, `/graphql/v1` and `/auth/v1` go to that project's PostgREST and GoTrue on loopback ports `supavise` allocated. `/realtime/v1` goes to Realtime with `Host: <ref>.realtime.internal`; `/storage/v1` goes to Storage with `x-forwarded-host: <ref>.api.<domain>`; `/functions/v1` goes to Edge Runtime with a tenant header our main service reads. `supavise` validates `apikey` against the project's keys and translates `sb_publishable_*` and `sb_secret_*` to internal JWTs, the same logic as upstream's Envoy Lua and stack proxy. Postgres connections do not pass through `supavise` at all: Supavisor listens on 5432 and 6543 for all projects and routes on the username suffix, which is also how hosted works.

**TLS.** Wildcard certificate by DNS-01 through CertMagic (Route 53 via the instance role on AWS; Cloudflare, Hetzner, DigitalOcean tokens elsewhere). Without a DNS API, per-project HTTP-01 certificates on first request. Without a domain, `<ref>.api.<ip>.sslip.io`. Wildcard DNS is required; there is no path-based mode.

**Backups.** Postgres's own `archive_command` calls `supavise wal push`, which writes to S3 or a local directory; on a systemd node the command talks to the daemon over a per-project unix socket and the daemon does the storage I/O, so no cluster holds backend credentials or can reach another project's archive (`internal/backup/README.md`, "The WAL relay"). A nightly `pg_basebackup` per project gives per-project point-in-time recovery with no wal-g, no pgBackRest. Restore creates a new project from a base backup plus WAL, which is also how clone-based branching will work.

**State.** Registry, secrets (encrypted with a key in `/etc/supavise`), refs, ports and versions live in the system Postgres. Project data lives in `/var/lib/supavise/projects/<ref>`.

## 5. Creating a project

1. Allocate a 20-letter ref, JWT secret, anon and service keys, publishable and secret keys, DB password, loopback ports.
2. Render and start `supavise-postgres@<ref>`; run the upstream init migrations and roles (the artifact ships them).
3. Render and start `supavise-gotrue@<ref>` and `supavise-postgrest@<ref>`.
4. `PUT /api/tenants/<ref>` on Supavisor, `POST /api/tenants` on Realtime, `POST /tenants/<ref>` on Storage.
5. Add the host route in `supavise`'s in-memory table; request a certificate if not under the wildcard.
6. Return the project to Studio, the CLI or the MCP server in Management-API shape.

Well under a minute. Delete is the reverse, with a final base backup.

## 6. Tooling compatibility

| Tool | How it connects | Patch needed |
|---|---|---|
| Studio | our platform-mode build; `NEXT_PUBLIC_API_URL` = `supavise`; sign-in via `supavise-gotrue@system` | three upstreamable patches: hCaptcha only with a site key, env-overridable `dashboard_auth:*` flags, extra project hosts in CSP |
| Supabase CLI | `--profile` file written by `supavise` with `api_url`, `dashboard_url`, `project_host`, `pooler_host`; PATs in `sbp_` format | none |
| Supabase MCP server | `--api-url`; project-URL and logs-dialect overrides | none; two small upstream PRs proposed |
| supabase-js and all client SDKs | project URL and keys | none |
| Agent skills | overlay that swaps the hosted MCP and dashboard URLs | none |

Management API subset: `/v1` projects, api-keys, `database/query`, migrations, `types/typescript`, functions, secrets, advisors, health, pooler config, `cli/login-role`, branches (empty list); `/platform` profile and permissions, organizations, projects, status, settings, `pg-meta/{ref}/query`, config, content, auth and storage admin proxies, entitlements and plan stub; `/v2` project config. Everything else returns an empty 200 or 204. The CLI decodes strictly, so types come from the specs and a nightly job diffs the specs against our implementation.

## 7. Install

**AWS.** Click the Launch Stack link, upload the release's template in the console, or run `deploy/aws/deploy.sh`. Fill in the admin email; instance size (Graviton, 8 GiB by default), domain and hosted zone are optional. CloudFormation creates one Ubuntu 24.04 instance, a data volume, an IAM role, a security group and an S3 bucket; user data runs the installer; the stack outputs the dashboard URL and the command that fetches the one-time claim token. Deleting the stack keeps the bucket and a final snapshot of the data volume. First project in under ten minutes.

**Any Linux server.** Ubuntu 24.04+ or Debian 12+ (glibc 2.35 floor for the artifacts; polkit 121+ for the unit-management rule, so Ubuntu 22.04 is out). Two DNS records, then:

```bash
curl -fsSL https://get.<name>.dev | sudo bash -s -- --domain example.com --dns cloudflare
```

The installer verifies checksums, creates the `supavise` user, writes `/etc/supavise/config.toml`, installs the units, starts `supavise`, and prints the dashboard URL and claim token. Re-running is idempotent and keeps secrets. `supavise self-update` upgrades the binary; artifacts upgrade through `versions.yaml`.

Laptops are not a target. Supabase's own `supabase start --runtime native` already covers local development with the same artifacts, and a project exported from it restores into `supavise`.

## 8. Staying in sync

1. `versions.yaml` pins every artifact release and the Studio tag. A bot opens a bump PR per upstream self-hosted release.
2. The conformance suite stands up a node, creates two projects, and runs the upstream client test suites and Studio smoke tests against both. Green merges.
3. The three Studio patches rebase in CI on each tag and are proposed upstream.
4. Nightly diff of the `v1`, `v2` and `platform` OpenAPI specs against our server.
5. Per-project rolling upgrades with per-project version pinning.

## 9. Reserved for later, designed in now

- **Idle sleep.** Because `supavise` is the proxy, it can stop a project's GoTrue and PostgREST after an idle period and start them on the next request. Not in v1: measured Postgres idle is 15 to 20 MB, so a hundred warm projects already fit on one machine.
- **File-database engine.** The project record carries `engine: postgres | file` from day one and the data-plane sits behind a five-call interface. Turso has no open multi-tenant server yet, so nothing is built on it; when one exists it becomes one more shared process per shard, with auth and quotas in `supavise`'s proxy.
- **Image transforms, Logflare.** Optional units behind flags. (Edge Functions moved into v1 as workstream J: a tenant-aware main service inside the edge-runtime artifact.)
- **Restore UI, multi-node scheduling.** After the single-node product is solid. (Settings writes, API keys, password reset, storage actions, members and roles, and SSO for the dashboard and for projects are v1: HANDOFF workstreams K and L.)

## 9a. Branching for agents (right after v1)

Self-hosted Supabase has no branching; hosted gives every branch its own Postgres instance. AI agents need exactly that: a disposable environment each, many in parallel. A branch in `supavise` is a project with a `parent_ref`, so it gets its own cluster, keys and host for about 100 MB idle. `supavise` implements the Management API branch endpoints (`/v1/projects/{ref}/branches`, `/v1/branches/{id}`, `merge`, `reset`, `push`), so the stock Supabase MCP server and CLI branch tools work unchanged.

- **Schema-only** (hosted default): new project, then the parent's migrations and `seed.sql`.
- **With data**: a copy-on-write clone of the parent's data directory (`pg_backup_start`, reflink copy, `pg_backup_stop`) when the data disk is XFS, btrfs or OpenZFS 2.2+ with block cloning (macOS APFS uses clonefile), so creation time and disk use do not grow with the database; otherwise a restore from the parent's latest base backup plus WAL (workstream F).
- **Merge** applies the branch's new migrations to the parent; **reset** recreates the branch from the parent; **push** (rebase) applies the parent's new migrations to the branch.
- **Expiry**: branches carry an optional TTL and are deleted when it lapses; with idle sleep, an unused branch costs only disk.

## 10. What was cut from earlier drafts and why

| Cut | Replaced by | Reason |
|---|---|---|
| Envoy, xDS server, SDS, Lua filters | Go proxy inside `supavise` | Supabase's own stack orchestrator is its own proxy; removes three config surfaces and a second process |
| Separate TCP edge for Postgres | Supavisor on 5432/6543 | Supavisor already routes every project on the username; hosted does the same |
| wal-g or pgBackRest | `archive_command` = `supavise wal push`, nightly `pg_basebackup` | Postgres built-ins plus a Go S3 client; nothing to bundle |
| Docker runner | none | Native artifacts verified as a real runtime |
| Logflare and Vector in v1 | journald, `supavise logs`, optional later | Heaviest service; Studio's logs pages tolerate empty responses |
| Path-based routing mode | wildcard DNS required; sslip.io for trials | Realtime and Storage resolve tenants from the host |
| Local-CA laptop mode | Supabase's own dockerless CLI | It already exists and uses the same artifacts |
| Separate control-plane database | registry in the system Postgres | One cluster, one backup path, same unit shape as a project |
| Separate dashboard GoTrue setup | `supavise-gotrue@system` | The system project is a project |
| Idle sleep in v1 | always-on, sleep later | Density target already met warm |

## 11. Phases

Ordering only, no dates. The workstream split and shared conventions for parallel agents are in `HANDOFF.md`.

0. **Spike.** Build platform-mode Studio with the three patches. Generate a mock `/platform` and `/v1` from the specs. Sign in, list two projects, open the table editor on each, run SQL, `supabase link`, MCP `list_tables`. Exit: the captured list of endpoints Studio calls and which tolerate stubs.
1. **Single-node v1.** `supavise` proxy, ACME, units, artifact fetch, lifecycle, fleet tenant calls, P0 API subset, WAL archiving and restore, installer, conformance suite. Exit: Auth, REST, Realtime, Storage and Studio at parity on Ubuntu 24.04; measured Linux footprint at 10, 25 and 50 projects.
2. **Branching.** Section 9a: branch API, schema-only and copy-on-write data branches, merge, reset, push, expiry. Exit: the Supabase MCP server's branch tools work against `supavise` unchanged.
3. **AWS polish.** CloudFormation quick-create hardening, imgproxy, idle sleep. (Edge Functions ship in v1, workstream J.)
4. **Product.** Members, restore UI, metrics, docs, name and trademark check, upstream PRs.

## 12. Open items

- Linux per-project RSS and cold start with the Supabase preload set at 10, 25 and 50 projects.
- Which Studio platform calls tolerate stubs (spike exit criterion).
- Storage's S3-protocol endpoint on the file backend in multi-tenant mode.
- Whether the CLI keeps `--profile` or moves to "platforms"; pin and test per bump.
- Product name: must not contain "Supabase" per the partner catalog rule; no public trademark policy exists.

Supavise is not affiliated with or endorsed by Supabase Inc.
