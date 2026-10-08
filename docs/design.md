# Multi-tenant self-hosted Supabase: final design

Status: v1.0.

## 1. In one paragraph

One static Go binary, `supavise`, turns a Linux machine into a multi-project Supabase for a team. It runs Supabase's own native service artifacts as systemd units: three per project (Postgres, GoTrue, PostgREST) and a handful shared (Supavisor, Realtime, Storage, postgres-meta, Studio). The binary itself is the edge proxy with automatic TLS, the Management API that Studio, the Supabase CLI and the Supabase MCP server talk to, the project lifecycle engine, and the WAL archiver. No Docker, no Envoy, no third-party gateway, no fork of any Supabase service. A team installs it with one command or one CloudFormation click and gets a dashboard that behaves like supabase.com, for up to about a hundred projects.

## 2. Why this shape is the native one

- **Hosted topology.** Supabase runs Postgres, GoTrue and PostgREST per project and Supavisor, Realtime and Storage as shared multi-tenant fleets with tenant tables and admin APIs. Supavise does the same with the same binaries.
- **Hosted artifacts.** Supabase publishes every service as a relocatable, checksummed `tar.zst` (`supabase/slim-services`, MIT) for its dockerless CLI. Supavise runs those.
- **Hosted proxy and API.** The dockerless CLI's orchestrator (`@supabase/stack`) has its own HTTP proxy, with no Kong or Envoy: it routes by path prefix and rewrites publishable and secret keys to JWTs. `supavise` does the same per project, in Go. Studio, the CLI and the MCP server speak the public Management API (`v1`, `v2`, `platform` OpenAPI specs); `supavise` implements the subset they call.

Supabase never open-sourced the management API, the per-machine agent or the edge; `supavise` is those three.

## 3. Process inventory on one node

| Process | Count | Role |
|---|---|---|
| `supavise` | 1 | control plane, Management API, HTTPS and WebSocket proxy, ACME, WAL archiver |
| `supavise-postgres@system` | 1 | the registry, `_supavisor`, `_realtime` and `_storage` metadata, and the dashboard `auth` schema |
| `supavise-gotrue@system` | 1 | dashboard sign-in for Studio and PAT issuance |
| `supavise-supavisor` | 1 | one Postgres port for every project; routes on `postgres.<ref>` |
| `supavise-realtime` | 1 | tenants by first host label |
| `supavise-storage` | 1 | `MULTI_TENANT=true`, tenants by `x-forwarded-host`, file backend or S3 |
| `supavise-pgmeta` | 1 | stateless; per-request encrypted connection string from `supavise` |
| `supavise-studio` | 1 | our build of the upstream tag; platform mode, three small patches |
| `supavise-postgres@<ref>` | per project | the project's cluster, full extension set, own Vault key |
| `supavise-gotrue@<ref>` | per project | about 12 MB |
| `supavise-postgrest@<ref>` | per project | about 9 MB |
| optional: `supavise-imgproxy`, `supavise-edge-runtime` | 0 or 1 | image transforms; Edge Functions with our tenant-aware main service |

Everything except `supavise` and Studio is a slim-services artifact. Eight fixed processes plus three per project. The system cluster is a project too (`ref = system`), so there is one unit shape and one backup path.

## 4. The binary

The code is split by job: `api`, `proxy`, `units`, `lifecycle`, `backup`, `artifacts` and the other packages under `internal/`, each documented in its own `README.md`. `units` renders the systemd template units and env files from `internal/versions/versions.yaml` with per-project `MemoryMax` and `CPUQuota`; `artifacts` fetches and verifies the slim-services `tar.zst` releases.

**Routing.** `<ref>.api.<domain>` resolves the project. `/rest/v1`, `/graphql/v1` and `/auth/v1` go to that project's PostgREST and GoTrue on loopback ports `supavise` allocated. `/realtime/v1` goes to Realtime with `Host: <ref>.realtime.internal`, `/storage/v1` to Storage with `x-forwarded-host: <ref>.api.<domain>`, and `/functions/v1` to Edge Runtime with a tenant header our main service reads. `supavise` validates `apikey` against the project's keys and translates `sb_publishable_*` and `sb_secret_*` to internal JWTs. Postgres connections do not pass through `supavise` at all: Supavisor listens on 5432 and 6543 for all projects and routes on the username suffix, as hosted does.

**TLS.** A wildcard certificate by DNS-01 through CertMagic (Route 53 via the instance role on AWS; Cloudflare, Hetzner or DigitalOcean tokens elsewhere). Without a DNS API, per-project HTTP-01 certificates on first request. Without a domain, `<ref>.api.<ip>.sslip.io`. Wildcard DNS is required; there is no path-based mode.

**Backups.** Postgres's own `archive_command` calls `supavise wal push`, which writes to S3 or a local directory. On a systemd node the command talks to the daemon over a per-project unix socket and the daemon does the storage I/O, so no cluster holds backend credentials or can reach another project's archive (`internal/backup/README.md`, "The WAL relay"). A nightly `pg_basebackup` per project gives point-in-time recovery with no wal-g or pgBackRest. Restore builds a new project from a base backup plus WAL; branches without a copy-on-write clone use the same path (section 9a).

**State.** Registry, secrets (encrypted with a key in `/etc/supavise`), refs, ports and versions live in the system Postgres. Project data lives in `/var/lib/supavise/projects/<ref>`.

## 5. Creating a project

1. Allocate a 20-letter ref, JWT secret, API keys, DB password and loopback ports.
2. Render and start `supavise-postgres@<ref>`, then run the upstream init migrations and roles (the artifact ships them), then start `supavise-gotrue@<ref>` and `supavise-postgrest@<ref>`.
3. `PUT /api/tenants/<ref>` on Supavisor, `POST /api/tenants` on Realtime, `POST /tenants/<ref>` on Storage.
4. Add the host route to the proxy's table; request a certificate if not under the wildcard.
5. Return the project to Studio, the CLI or the MCP server in Management-API shape.

Creation takes well under a minute. Delete is the reverse, with a final base backup.

## 6. Tooling compatibility

| Tool | How it connects | Patch needed |
|---|---|---|
| Studio | our platform-mode build; `NEXT_PUBLIC_API_URL` = `supavise`; sign-in via `supavise-gotrue@system` | three upstreamable patches: hCaptcha only with a site key, env-overridable `dashboard_auth:*` flags, extra project hosts in CSP |
| Supabase CLI | `--profile` file written by `supavise` with `api_url`, `dashboard_url`, `project_host`, `pooler_host`; PATs in `sbp_` format | none |
| Supabase MCP server | `--api-url`; project-URL and logs-dialect overrides | none |
| supabase-js and all client SDKs | project URL and keys | none |
| Agent skills | overlay that swaps the hosted MCP and dashboard URLs | none |

Management API subset: `/v1` projects, api-keys, `database/query`, migrations, `types/typescript`, functions, secrets, advisors, health, pooler config, `cli/login-role`, branches; `/platform` profile and permissions, organizations, projects, status, settings, `pg-meta/{ref}/query`, config, content, auth and storage admin proxies, entitlements and plan stub; `/v2` project config. Everything else returns an empty 200 or 204. The CLI decodes strictly, so types come from the specs.

## 7. Install

**AWS.** Use the Launch Stack link, upload the release's template in the console, or run `deploy/aws/deploy.sh`. Fill in the admin email; instance size (Graviton, 8 GiB by default), domain and hosted zone are optional. CloudFormation creates one Ubuntu 24.04 instance, a data volume, an IAM role, a security group and an S3 bucket; user data runs the installer; the stack outputs the dashboard URL and the command that fetches the one-time claim token. Deleting the stack keeps the bucket and a final snapshot of the data volume.

**Any Linux server.** Ubuntu 24.04+ or Debian 12+ (glibc 2.35 for the artifacts; polkit 121+ for the unit-management rule, so Ubuntu 22.04 is out). Four DNS records (`api.`, `studio.`, `pooler.`, `*.api.`), then:

```bash
curl -fsSL https://github.com/supavise/supavise/releases/latest/download/install.sh | sudo bash -s -- --domain example.com --dns cloudflare
```

The installer verifies checksums, creates the `supavise` user, writes `/etc/supavise/config.toml`, installs the units, starts `supavise`, and prints the dashboard URL and claim token. Re-running keeps secrets. `supavise self-update` upgrades the binary. Artifacts upgrade through `internal/versions/versions.yaml`; a project follows the node's pins when its Owner or Administrator upgrades it (Studio, the Management API or `supavise projects upgrade`).

Laptops are not a target: `supabase start --runtime native` covers local development with the same artifacts, and a project exported from it restores into `supavise`.

## 8. Staying in sync

`internal/versions/versions.yaml` pins every artifact release and the Studio tag; a nightly job opens a bump pull request per newer upstream release, and the conformance suite (two projects, the upstream client test suites, Studio smoke tests) gates it. The three Studio patches are rebased on each tag by the Studio build and proposed upstream. A nightly job diffs the `v1`, `v2` and `platform` OpenAPI specs against the server. Projects upgrade one at a time with per-project version pinning (`internal/lifecycle/README.md`, "Service versions and project upgrades"). The jobs are listed in [development.md](development.md).

## 9. In v1 and reserved

**In v1.** Everything above, plus Edge Functions (a tenant-aware main service in the edge-runtime artifact); settings writes, API keys and database password reset; storage dashboard actions; organization members and roles; single sign-on for the dashboard and for projects; and the restore UI (Studio's Backups pages restore a project in place, to a point in time or to a base backup, through the Management API; restore to a new project stays on the command line). Branching is section 9a.

**Reserved, designed in but not built.**

- **Idle sleep.** Because `supavise` is the proxy, it can stop a project's GoTrue and PostgREST after an idle period and start them on the next request. Measured Postgres idle is 15 to 20 MB, so a hundred warm projects already fit on one machine.
- **File-database engine.** The project record carries `engine: postgres | file` and the data plane sits behind a five-call interface. Turso has no open multi-tenant server yet; when one exists it becomes one more shared process per shard, with auth and quotas in `supavise`'s proxy.
- **Image transforms, Logflare.** Optional units behind flags.
- **Multi-node scheduling.** After the single-node product is solid.

## 9a. Branching

Hosted Supabase gives every branch its own Postgres instance, which is what AI agents need: a disposable environment each, many in parallel. Self-hosted Supabase has no branching. A branch in `supavise` is a project with a `parent_ref`, so it gets its own cluster, keys and host for about 100 MB idle. `supavise` implements the Management API branch endpoints (`/v1/projects/{ref}/branches`, `/v1/branches/{id}`, `merge`, `reset`, `push`), so the stock Supabase MCP server and CLI branch tools work unchanged.

- **Schema-only** (hosted default): new project, then the parent's migrations and `seed.sql`.
- **With data**: a copy-on-write clone of the parent's data directory (`pg_backup_start`, reflink copy, `pg_backup_stop`) when the data disk is XFS, btrfs or OpenZFS 2.2+ with block cloning (macOS APFS uses clonefile), so creation time and disk use do not grow with the database; otherwise a restore from the parent's latest base backup plus WAL.
- **Merge** applies the branch's new migrations to the parent; **reset** recreates the branch from the parent; **push** (rebase) applies the parent's new migrations to the branch.
- **Expiry**: branches carry an optional TTL and are deleted when it lapses; with idle sleep (section 9), an unused branch would cost only disk.

## 10. What was cut from earlier drafts and why

| Cut | Replaced by | Reason |
|---|---|---|
| Envoy, xDS server, SDS, Lua filters | Go proxy inside `supavise` | Supabase's stack orchestrator has its own proxy; removes three config surfaces and a second process |
| Separate TCP edge for Postgres | Supavisor on 5432/6543 | Supavisor already routes every project on the username; hosted does the same |
| wal-g or pgBackRest | `archive_command` = `supavise wal push`, nightly `pg_basebackup` | Postgres built-ins plus a Go S3 client |
| Docker runner | none | Native artifacts work as a real runtime |
| Logflare and Vector in v1 | journald, `supavise logs`, optional later | Heaviest service; Studio's logs pages tolerate empty answers |
| Path-based routing mode | wildcard DNS required; sslip.io for trials | Realtime and Storage resolve tenants from the host |
| Local-CA laptop mode | Supabase's own dockerless CLI | It already exists and uses the same artifacts |
| Separate control-plane database | registry in the system Postgres | One cluster, one backup path, same unit shape as a project |
| Separate dashboard GoTrue setup | `supavise-gotrue@system` | The system project is a project |
| Idle sleep in v1 | always-on | Density target already met warm |

## 11. Open questions

- Storage's S3-protocol endpoint on the file backend in multi-tenant mode.
- Whether the CLI keeps `--profile` or moves to "platforms"; pin and test per bump.

Supavise is not affiliated with or endorsed by Supabase Inc.
