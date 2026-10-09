# Multi-tenant self-hosted Supabase: final design

Status: v1.0, with the cluster work of [section 12](#12-clusters-replicas-and-failover) and the MCP sign-in of [section 13](#13-oauth-sign-in-and-the-remote-mcp-endpoint).

## 1. In one paragraph

One static Go binary, `supavise`, turns a Linux machine into a multi-project Supabase for a team. It runs Supabase's own native service artifacts as systemd units: three per project (Postgres, GoTrue, PostgREST) and a handful shared (Supavisor, Realtime, Storage, postgres-meta, Studio). The binary itself is the edge proxy with automatic TLS, the Management API that Studio, the Supabase CLI and the Supabase MCP server talk to (and the OAuth server that signs remote MCP clients in, section 13), the project lifecycle engine, and the WAL archiver. No Docker, no Envoy, no third-party gateway, no fork of any Supabase service. A team installs it with one command or one CloudFormation click and gets a dashboard that behaves like supabase.com, for up to about a hundred projects. A second server can join the first for per-project read replicas, a planned switchover and failover (section 12).

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
| `supavise-studio` | 1 | our build of the upstream tag; platform mode, four small patches; it also serves the remote MCP route behind `api.<domain>/mcp` (section 13) |
| `supavise-postgres@<ref>` | per project | the project's cluster, full extension set, own Vault key |
| `supavise-gotrue@<ref>` | per project | about 12 MB |
| `supavise-postgrest@<ref>` | per project | about 9 MB |
| optional: `supavise-imgproxy`, `supavise-edge-runtime` | 0 or 1 | image transforms; Edge Functions with our tenant-aware main service |

Everything except `supavise` and Studio is a slim-services artifact. Eight fixed processes plus three per project. The system cluster is a project too (`ref = system`), so there is one unit shape and one backup path.

## 4. The binary

The code is split by job: `api`, `proxy`, `units`, `lifecycle`, `backup`, `artifacts` and the other packages under `internal/`, each documented in its own `README.md`. `units` renders the systemd template units and env files from `internal/versions/versions.yaml` with per-project `MemoryMax` and `CPUQuota`; `artifacts` fetches and verifies the slim-services `tar.zst` releases.

**Routing.** `<ref>.api.<domain>` resolves the project. `/rest/v1`, `/graphql/v1` and `/auth/v1` go to that project's PostgREST and GoTrue on loopback ports `supavise` allocated. `/realtime/v1` goes to Realtime with `Host: <ref>.realtime.internal`, `/storage/v1` to Storage with `x-forwarded-host: <ref>.api.<domain>`, and `/functions/v1` to Edge Runtime with a tenant header our main service reads. `supavise` validates `apikey` against the project's keys and translates `sb_publishable_*` and `sb_secret_*` to internal JWTs. Postgres connections do not pass through `supavise` at all: Supavisor listens on 5432 and 6543 for all projects and routes on the username suffix, as hosted does.

**TLS.** A wildcard certificate by DNS-01 through CertMagic (Route 53 via the instance role on AWS; Cloudflare, Hetzner or DigitalOcean tokens elsewhere). Without a DNS API, per-project HTTP-01 certificates on first request. Without a domain, `<ref>.api.<ip>.sslip.io`. Wildcard DNS is required; there is no path-based mode.

**Backups.** Postgres's own `archive_command` calls `supavise wal push`, which writes to S3 or a local directory. On a systemd node the command talks to the daemon over a per-project unix socket and the daemon does the storage I/O, so no cluster holds backend credentials or can reach another project's archive (`internal/backup/README.md`, "The WAL relay"). A nightly `pg_basebackup` per project gives point-in-time recovery with no wal-g or pgBackRest. Restore builds a new project from a base backup plus WAL; branches without a copy-on-write clone use the same path (section 9a). The same base backups seed a read replica's standby, the relay refuses a push from a standby until the node is promoted, and the backup store holds the leader marker of a cluster (section 12).

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
| Studio | our platform-mode build; `NEXT_PUBLIC_API_URL` = `supavise`; sign-in via `supavise-gotrue@system` | three upstreamable patches: hCaptcha only with a site key, env-overridable `dashboard_auth:*` flags, extra project hosts in CSP; and a fourth that only Supavise needs, which serves Studio's MCP route in platform mode (section 13.5) |
| Supabase CLI | `--profile` file written by `supavise` with `api_url`, `dashboard_url`, `project_host`, `pooler_host`; PATs in `sbp_` format | none |
| Supabase MCP server (stdio) | `--api-url`; project-URL and logs-dialect overrides | none |
| Remote MCP clients: Claude Code, Cursor, VS Code, Codex, Claude desktop | `https://api.<domain>/mcp?project_ref=<ref>`; OAuth 2.1 sign-in (section 13) | none on the client |
| supabase-js and all client SDKs | project URL and keys | none |
| Agent skills | overlay that swaps the hosted MCP and dashboard URLs | none |

Management API subset: `/v1` projects, api-keys, `database/query`, migrations, `types/typescript`, functions, secrets, advisors, health, pooler config, `cli/login-role`, branches; `/platform` profile and permissions, organizations, projects, status, settings, `pg-meta/{ref}/query`, config, content, auth and storage admin proxies, entitlements and plan stub; `/v2` project config. Everything else returns an empty 200 or 204. The CLI decodes strictly, so types come from the specs.

## 7. Install

**AWS.** Use the Launch Stack link, upload the release's template in the console, or run `deploy/aws/deploy.sh`. Fill in the admin email; instance size (Graviton, 8 GiB by default), domain and hosted zone are optional. CloudFormation creates one Ubuntu 24.04 instance, a data volume, an IAM role, a security group and an S3 bucket; user data runs the installer; the stack outputs the dashboard URL and the command that fetches the one-time claim token. Deleting the stack keeps the bucket and a final snapshot of the data volume.

**Any Linux server.** Ubuntu 24.04+ or Debian 12+ (glibc 2.35 for the artifacts; polkit 121+ for the unit-management rule, so Ubuntu 22.04 is out). Four DNS records (`api.`, `studio.`, `pooler.`, `*.api.`), then:

```bash
curl -fsSL https://github.com/supavise/supavise/releases/latest/download/install.sh | sudo bash -s -- --domain example.com --dns cloudflare
```

The installer verifies checksums, creates the `supavise` user, writes `/etc/supavise/config.toml`, installs the units, starts `supavise`, and prints the dashboard URL and claim token. Re-running keeps secrets. `supavise upgrade` moves the node onto a newer release and `supavise self-update` replaces only the binary. Artifacts upgrade through `internal/versions/versions.yaml`; a project follows the node's pins when its Owner or Administrator upgrades it (Studio, the Management API or `supavise projects upgrade`).

Laptops are not a target: `supabase start --runtime native` covers local development with the same artifacts, and a project exported from it restores into `supavise`.

## 8. Staying in sync

`internal/versions/versions.yaml` pins every artifact release and the Studio tag; a nightly job opens a bump pull request per newer upstream release, and the conformance suite (two projects, the upstream client test suites, Studio smoke tests) gates it. The four Studio patches are rebased on each tag by the Studio build; the first three are proposed upstream, and the fourth is specific to Supavise (section 13.5). A nightly job diffs the `v1`, `v2` and `platform` OpenAPI specs against the server. Projects upgrade one at a time with per-project version pinning (`internal/lifecycle/README.md`, "Service versions and project upgrades"). The jobs are listed in [development.md](development.md).

## 9. In v1 and reserved

**In v1.** Everything above, plus read replicas, switchover and failover on a second server, Storage on S3 and the upgrade path for servers and AWS stacks (section 12), Edge Functions (a tenant-aware main service in the edge-runtime artifact); settings writes, API keys and database password reset; storage dashboard actions; organization members and roles; single sign-on for the dashboard and for projects; and the restore UI (Studio's Backups pages restore a project in place, to a point in time or to a base backup, through the Management API; restore to a new project stays on the command line). Branching is section 9a.

**Reserved, designed in but not built.**

- **Idle sleep.** Because `supavise` is the proxy, it can stop a project's GoTrue and PostgREST after an idle period and start them on the next request. Measured Postgres idle is 15 to 20 MB, so a hundred warm projects already fit on one machine.
- **File-database engine.** The project record carries `engine: postgres | file` and the data plane sits behind a five-call interface. Turso has no open multi-tenant server yet; when one exists it becomes one more shared process per shard, with auth and quotas in `supavise`'s proxy.
- **Image transforms, Logflare.** Optional units behind flags.
- **Automatic failback, `pg_rewind` and a witness.** A node that returns is rebuilt from the archive and moved back by hand; no lease or third-party witness exists (section 12.5).
- **Cross-region failover of the service address.** An Elastic IP belongs to one region, so the address moves by hand across regions.

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
| Replication slots for replicas | a standby that streams and falls back to the WAL archive | No disk-fill risk, no slot to recreate at each promotion, no competition with Realtime's logical slots; the primary's units stay as they were |
| A lease or witness for failover | a hard fence, and an epoch marker in the backup store | A lease is a second mechanism, and stopping the instance already guarantees one writer |
| An IAM authorizer for join | a token, a pinned CA and a keyed proof | One mechanism; on AWS the token travels through Secrets Manager |
| Resources for a second server inside the first stack | a second stack from the same template | The template's network is one /24 with one subnet, and a changed network interface replaces the instance |
| A stored IAM key for Storage on AWS | a role the daemon assumes, with short-lived credentials | No secret to store or rotate (spike S5) |
| Hosted's separate MCP service, or a Go reimplementation of its tools | Studio's bundled MCP route, served in platform mode by patch 0004 behind an authenticating gate in the Go edge | Supavise has no process for a separate MCP service, and about 30 tools written again in Go would drift from the package. Studio already bundles the package with every tool. The patch is Supavise-specific and cannot go upstream (section 13.5) |
| OAuth tokens in `access_tokens` | five `oauth_*` tables | A release without OAuth would read an `sbp_oauth_` token in `access_tokens` as an all-permissions personal access token. With separate tables it finds no row and answers 401 |

## 11. Open questions

- Storage's S3-protocol endpoint on the file backend in multi-tenant mode.
- Whether the CLI keeps `--profile` or moves to "platforms"; pin and test per bump.
- Real AWS behavior of the stack update and of fencing onto a replica server: `deploy/aws/rehearse.sh` is the check, and no recorded run exists (section 12.6).
- Recorded measurements of a full switchover and failover (RPO and RTO). The bounds in [reference/replicas.md](reference/replicas.md#rpo-and-rto) are designed, not measured.

## 12. Clusters, replicas and failover

A second Supavise server can join the first. The servers form a cluster: one registry, one writable copy of each project, and a standby copy of the registry on every other server. [reference/replicas.md](reference/replicas.md) is the operator's view of this section. The packages `internal/mesh`, `cluster`, `placement`, `replicas`, `failover`, `infra`, `hostsetup`, `nodeupgrade` and `storagemigrate` hold the code's rules in their own READMEs.

### 12.1 Model and invariants

| Term | Meaning |
|---|---|
| cluster | The joined servers. A server that never joined is a cluster of one and runs as before. |
| node | One server: one row in `supavise.nodes`, id `n1`, `n2`, ... |
| leader | The node whose system Postgres cluster is not in recovery. It is not a flag: promoting the system cluster makes a node the leader. It runs the writable registry, the Management API, Studio and the shared services. |
| home | `projects.node_id`: where a project's primary, Auth and REST run. New projects are homed on the leader. |
| replica | A standby Postgres and a PostgREST of one project on one node that is not its home: one `supavise.replicas` row. |
| canonical ports | `PortsFor(ref, seq)`: Postgres `project_base + 3s`, Auth `+1`, PostgREST `+2`. |
| replica ports | `replica_base + 3s` for Postgres, `+2` for PostgREST. |
| forwarder | A loopback listener on a canonical, replica or shared-service port that sends each connection through the mesh to the node that runs the service. |
| epoch | A counter in the `cluster` row, raised at each promotion of the system cluster. |

Because every loopback port a consumer expects is either the real service or a forwarder to it, nothing re-points when a project or the whole server moves. The proxy, Supavisor, Realtime, Storage, Functions, pg-meta and the backups keep dialing `127.0.0.1:<canonical port>`. A failover is: promote, fence, and change `projects.node_id`.

Invariants. Each has a test.

- **I1** A node is the leader if and only if `pg_is_in_recovery()` is false on its system cluster. A leader that learns of a higher epoch fences itself.
- **I2** A project has exactly one home. A replica is never on the home node: `supavise.replicas` has `unique (ref, node_id)`, and the setup refuses the home node.
- **I3** On every node, each canonical port of every project is Postgres, Auth or PostgREST itself (home) or a forwarder to the home. Each replica port is the replica itself or a forwarder. Each shared-service port is the service (leader) or a forwarder to the leader.
- **I4** Upgrading to the release with this work changes no rendered unit file or environment of an existing primary, so the rollout restarts none. `TestPrimaryUnitsMatchV011` in `internal/lifecycle` pins it with units rendered as v0.1.1 rendered them.
- **I5** Registry rows are desired state. Agents report observed state to the leader, which writes status transitions. One snapshot deviates in spirit: the leader's replica controller writes `<state_dir>/replicas-status.json` (receiver status and lag, mode 0600, no LSNs) every pass, only so that `supavise replicas ls` in another process can show lag. Nothing reads it back as state.
- **I6** Registry migrations from 1300 on are additive: create a table, create an index, add a column with a default. A binary one minor behind runs against the newer schema. `internal/registry/migrations_lint_test.go` enforces it.

Decisions between the two designs that preceded this one:

| Question | Decision | Reason |
|---|---|---|
| Placement | `projects.node_id` plus a `replicas` table | Studio identifies the primary as `identifier === ref`. After a failover the new primary must carry `<ref>`, so identifiers cannot be keys of a table whose roles flip. A home column leaves every reader of `registry.Project` working. |
| Reaching a database on another node | forwarders on canonical ports, extended to the shared-service ports | the proxy, Supavisor, Storage, Realtime and Functions need no change |
| Transport | one mTLS port, one multiplexed session per node pair, two stream types, either side opens streams | a returning node can have an unstable address; whichever side can dial makes the session, and both use it |
| Cluster CA | derived from the master key (Ed25519) | no secret to store, replicate or lose, and every node already holds the key |
| Join | a token, a pinned CA and a keyed proof, in two phases (`joining`, then `active`) | one mechanism; a joiner that stops resumes with `--resume` |
| Shared-service state on a follower | the follower's Supavisor reads the replicated `_supavisor` | no second home for tenant data (spike S1) |
| Replication slots | none | a standby streams and falls back to the archive; no slot to recreate at promotion |
| A standby must not push WAL | `archive_mode=on` (not `always`), and the relay refuses a push for a replica without `promote.ok` | an accidental `pg_promote()` becomes an alert, not a corrupted archive |
| Replica names | `<identifier>.api.<domain>` and `<ref>-lb.api.<domain>` | one wildcard certificate and the existing CSP list cover them |
| Connection strings | placeholder passwords; the `x-connection-encrypted` header only selects a database | the header is never a credential; the handler authorizes by `{ref}` |
| Failover witness | none; the hard fence guards, and an epoch marker in the backup store is written at promotion and read at boot | stopping the instance guarantees one writer; the marker covers a returning node that cannot reach its peer |
| Return of the old primary | a planned switchover demotes in place; an unplanned failover rebuilds and keeps `data.diverged-<epoch>` for 3 days | |
| Storage credentials on AWS | a role the daemon assumes, short-lived, no stored key | spike S5 |
| Second AWS server | a second stack from the same template | the template's network is one /24 with one subnet |
| Stack upgrade | `supavise upgrade --aws` wraps a signed script that also runs standalone | the instance never gains CloudFormation or IAM rights |
| Fencing IAM | scoped by the `supavise:cluster` tag | works for peers made later, in other stacks and regions |
| Automatic modes | `project` and `server`, with a hard fence (AWS) | per project is the common case; per server is the escalation when a node is dead |

### 12.2 The mesh

Nodes talk over one TCP port, `[node] peer_listen` (7443), with TLS 1.3 and the ALPN `supavise-mesh/1`.

**Identity.** The cluster CA is an Ed25519 key derived from `Derive("supavise/cluster-ca/v1")`, with a fixed subject, serial 1 and 20 years of validity, so every node computes the same certificate. Its SHA-256 is the pin in a join token. A node certificate is Ed25519, valid one year, with the name `supavise://node/<id>`; the key never leaves the node. A handshake succeeds when the chain verifies to the CA, and the registry has a row for that node whose serial matches and whose state is `joining`, `active` or `fenced`. A `left` node, an unknown node or another serial is refused. A node renews at 30 days. A caller with no certificate can reach the join endpoints only, with tight limits on body size, streams, sessions and handshakes per address.

**Sessions and streams.** One multiplexed session (`xtaci/smux`) joins each pair of nodes, and either side opens streams. A stream starts with one JSON line: `{"t":"fwd","kind":"postgres","ref":"<ref>"}` for raw bytes to a loopback port, or `{"t":"rpc"}` for one HTTP request and its answer. The reader ignores fields it does not know, so a node one release ahead can add one. The node with the lower id dials at once; the other waits 3 seconds and dials if there is still no session. The address is the node's `peer_address`, then, on AWS, the instance's current addresses from EC2.

**Authorization.** `kind` is an enumeration, never a port, so there is no open relay: a stream names `postgres`, `gotrue` or `postgrest` of a ref the target homes, `replica-postgres` or `replica-postgrest` of a ref it holds a replica of, or `svc:<name>` of a shared service, which only the leader serves. A `fenced` node may open nothing, and a `joining` node only the system cluster's Postgres.

**Forwarders.** On each node the forwarders bind the canonical ports of projects homed elsewhere, the replica ports of replicas it does not hold, and, on a non-leader, the shared-service ports. They follow the registry. A port that something holds is tried again every 2 seconds, so a promotion that needs the canonical port waits for the old forwarder to let go.

**Peer API.** JSON over `rpc` streams:

| Endpoint | Purpose |
|---|---|
| `GET /peer/v1/ping` | node, epoch, leader, release, applied migrations and health; every 5 seconds |
| `GET`, `POST /peer/v1/join`, `POST .../join/confirm`, `POST .../rejoin` | joining and rejoining |
| `POST /peer/v1/certs/renew`, `GET /peer/v1/certs` | node certificate renewal; the leader's certificate store, with an ETag |
| `GET /peer/v1/config` | the cluster-scoped settings |
| `POST /peer/v1/report` | observed state from a node to the leader |
| `PUT`, `GET`, `DELETE /peer/v1/instances/{identifier}` and `POST .../{action}` | ensure, observe, remove and operate a replica instance |
| `POST /peer/v1/projects/{ref}/plane/{method}`, `.../backup/{op}` | a lifecycle plane call or a backup operation for a project homed on the node |
| `POST /peer/v1/fleet/refresh/{tenant}` | refresh a Supavisor tenant on a follower, after its standby replayed the change |
| `POST /peer/v1/fence`, `.../failover/...` | the cooperative fence, the planned stop and the server switchover |

### 12.3 Registry, configuration and roles

Migrations 1300 and 1301 add `nodes`, `cluster` (name, epoch, leader, service address, maintenance), `join_tokens`, `replicas`, `replica_optouts` and `moves`, and a `node_id` column on `projects`. `registry.OpenExisting` and `OpenReadOnly` read a registry that is not yet migrated (v0.1.x) in legacy mode, where every project is on `n1`.

`config.d/*.toml` is merged over `config.toml` in file-name order. `supavise node join` writes `10-cluster.toml` with the cluster-scoped keys (`domain`, `tls.*`, `backup.*`, `fleet.storage_*`, `replicas.*`, `failover.*`), and `supavise system converge` refreshes it from the leader. `20-aws.toml` holds `[aws] stack_name` and `30-storage-s3.toml` holds Storage's bucket credentials. A key with no value keeps its code default, so a v0.1.x `config.toml` loads unchanged.

| Component | Leader | Follower |
|---|---|---|
| `supavise.service`: proxy, peer server, forwarders, agents, WAL relay | runs | runs |
| `supavise-postgres@system` | primary | hot standby |
| Management API, Studio, `supavise-gotrue@system` | run | not run; `api.` forwards to the leader, `studio.` through the forwarder |
| Supavisor | primary pools | runs against the replicated `_supavisor`, with no `bin/prepare` |
| Realtime, Storage, postgres-meta, Edge Runtime | run | installed, not started; their ports are forwarders to the leader |
| Projects | homed here | those homed here, plus replicas |

A follower opens its registry read-only: no migration, no locks, no tenant writes and no certificate issuance. It mirrors the leader's certificate store every minute and issues nothing, so two nodes never ask the CA for one name. A registry whose schema is newer than the binary leaves running instances alone and raises `standby_behind`.

**Boot and role changes.** Before the node opens, `DecideBoot` weighs the local registry, every peer that answers a ping within 4 seconds, and `_node/leader.json` in the backup store. A node whose row is `fenced` or `left`, or that another source names as leader at an epoch no lower than its own, starts fenced: it records `fenced.json`, stops its units, raises `fenced` and answers HTTP `503`. A node that nothing can reach, with no demotion in its record, starts as leader. When the system cluster of a running node is promoted or demoted, the daemon exits with `app.ErrRoleChanged` and systemd starts it in the new role. The first token created on a node gives the node its cluster identity and restarts the daemon once to listen on the peer port.

### 12.4 Replicas

A replica is the project's Postgres in standby mode plus a PostgREST, rendered by the same code as the primary with a different port and a few settings. No unit template is new.

- **Seeding.** The node extracts the project's newest base backup, writes `postgresql.auto.conf` with `primary_conninfo` (the canonical port on the replica's node, which is a forwarder to the home; the replication role and its sealed password), `restore_command` (the node's own WAL relay) and `recovery_target_timeline = 'latest'`, and writes `standby.signal` last. The Postgres artifact has no `pg_basebackup`, so seeding uses the tarball.
- **Streaming and the archive.** No slot exists. A standby streams, and when the stream breaks it replays from the archive through `restore_command`, then streams again. The primary does not recycle a segment before `archive_command` succeeds.
- **No push from a standby.** `archive_mode=on`, so a standby never archives. The relay also refuses a push for a ref that is a replica on the node unless `promote.ok` holds the cluster's epoch, which only a promotion writes. A push for a replica gets `412`.
- **Settings.** The same class settings as the primary (Postgres needs the standby's limits at least the primary's), `hot_standby=on` and `hot_standby_feedback=on`. No Auth runs, because Auth migrates and a replica is read-only.
- **PostgREST.** `PGRST_DB_URI` lists the replica first and the canonical port second with `target_session_attrs=read-only`: the pool uses the replica, and only the `LISTEN` session reaches the primary. A `SIGUSR1` every `schema_reload_seconds` covers a notification that arrives before replay.
- **Pooler.** A Supavisor tenant with the replica's identifier and replica port. It replicates, so each node's Supavisor serves it. The leader asks a follower to refresh a tenant only after the follower's standby replayed the change.
- **Status and lag.** The leader's controller is level triggered. It reads rows, moves each through seven setup steps, maps observations to a status and keeps a 24-hour lag series in memory.
- **Server default.** `[replicas] default = "all"` makes a missing row for every project that is not a branch, not the system project and running, on every other active node, unless an opt-out row names the pair. A replica removed by hand writes an opt-out; a later setup clears it.

### 12.5 Failover

A switchover is planned: the old primary stops cleanly and nothing is lost. A failover is unplanned: the old primary is fenced first. Both exist for one project and for the whole server. Every step is written to a log after it finishes, and a step that is in the log is not done again, so `--resume` continues a move that stopped. The orchestrator never touches another node's database or files itself. It calls ports that the daemon wires to `placement`, the mesh, `fleet` and `backup`.

**Project switchover**: `begin`, `quiesce` (status `RESTARTING`), `stop-old` (Realtime and the pooler let go, the old primary stops, its final checkpoint is read), `promote` (the replica replays past that position, then `promote.ok(epoch)`, `pg_promote`, restart as primary), `homed` (`SetProjectNode`, and a replica row for the old home), `start-new`, `tenant`, `demote-old` and `base-backup`. A failover replaces `quiesce` and `stop-old` with the cooperative fence of that project, and drains the archive instead of waiting for a position.

**Server move**: `begin`, then for a switchover `quiesce` (the leader enters maintenance, stops the project clusters eight at a time, then the system GoTrue, the shared services and last the system cluster) and `caught-up`; for a failover `fence`. Then `marker` (`_node/leader.json`), `address` (the service address moves to the survivor), `promote-system`, `leader` (the daemon takes the role, the log moves into a `moves` row, the old leader is marked `fenced` after a failover), each project four at a time, and for a switchover `demote:<ref>` for the old leader's clusters. A project with no replica is refused unless `--restore-missing`, which seeds a standby from the archive.

**Ordering.** Nothing is promoted before the old leader cannot write, and nothing is written to the marker before the old leader is beyond return, so a switchover that fails earlier can be undone. The marker comes before the address, so only a survivor whose marker stands reaches it. A store that already holds a higher epoch, or this epoch under another leader, ends the move `aborted`.

**Fencing.** The AWS provider stops the peer's instance (a forced stop after the first wait), then associates the service address's Elastic IP with the survivor, on its secondary private address when the primary one already carries an Elastic IP. The command provider runs the operator's `fence_command` and `takeover_command`. With neither, the operator asserts with `--old-primary-is-down` that the old leader cannot write, and the node tries the cooperative fence first. A cooperative fence writes `fenced.json`, removes the launcher of each primary (the unit's `ConditionPathExists` then keeps it down, at boot too) and stops it.

**Automatic modes** need the AWS fencer, whose `DryRun` probe must pass. Server mode needs the leader silent for `grace_seconds`, the public health probe of the service address failing and EC2 saying the leader is down; with several followers they take turns by node id. Project mode needs a project's primary unhealthy for `project_grace_seconds`. A held gate is maintenance, a cooldown, a different release or region, an unknown or large lag, or a move already running.

**The old primary's return.** A node that led and returns learns of the higher epoch at boot (12.3) and starts fenced. `supavise node rejoin` moves each `data` directory to `data.diverged-<epoch>`, seeds a fresh system standby from the current leader's archive, keeps the node's identity and rebuilds its replicas through the controller. A running leader that sees a higher epoch in a ping or in the marker fences itself the same way (I1).

**Role change inside a move.** A server move cuts its own run once, when the node's system cluster is promoted, because the daemon restarts. The cut is not a failure: the move stays `running`, the daemon that starts continues it, and the CLI waits for it and goes on printing steps.

### 12.6 Upgrade layers

`supavise upgrade` brings an existing install forward in three layers. Each is idempotent, and the first two do not need the third.

1. **The host: `supavise system converge`.** A list of root steps that bring unit files, directories (`/etc/supavise/cluster`, `config.d`), mount protection (`RequiresMountsFor`), the firewall rule for 7443 and declared packages to what the binary expects. A second run changes nothing. It writes `<state_dir>/converged` with the revision it completed, and the daemon raises `host_not_converged` while the file is behind. `install-units` is an alias, which is how a v0.1.x driver reaches it: the old `supavise upgrade` runs `system install-units` on the new binary right after the swap.
2. **The release.** `supavise release-info` on the new binary reports the converge revision, the host steps, the registry migrations it adds and the infrastructure revision it needs. The plan prints them and the projects that restart. The signed manifest carries `host.converge_revision`, `aws.stack_revision`, the template's asset name and SHA-256, `min_peer_from` (the oldest release a joined server may run beside this one, from `deploy/MIN_PEER_FROM`) and `wal_compat` (false when the release's PostgreSQL cannot read the WAL of earlier releases). The first hop from v0.1.x is driven by the old binary, so it prints the old plan; everything new runs on the new binary after the swap. Registry migrations only go forward, so a rollback after the new daemon started is refused, and the pre-upgrade base backup of the system project is the way back.
3. **The AWS stack.** The node reads its stack's revision from its own instance tags through the metadata service, with no IAM permission. `supavise upgrade --aws` runs the signed `supavise-aws-deploy.sh update` with the operator's credentials, never the instance role. The script creates a CloudFormation change set from the release's signed template with `UsePreviousValue` for every parameter the stack has, shows it, refuses a change that would replace or remove a resource other than rules and policies, blocks one that would move the service address back after a failover, and applies it after the operator types `apply`. A stack that lacks the credentials is not an error: the node side proceeds and the command to run is printed.

A test renders the v0.1.1 template and the current one with the v0.1.1 parameters and fails if any property of an existing resource that replaces or interrupts the instance, volume, address or network differs. A real change set cannot run in CI; `deploy/aws/rehearse.sh` makes a throwaway stack from the v0.1.1 template for that.

### 12.7 Storage on S3

A server move needs Storage on S3: objects on one node's disk cannot follow the leader. `supavise storage migrate --to s3` copies every project's objects to a bucket while Storage serves from files, catches up, verifies each project's `storage.objects` rows against the bucket, and switches. At the switch Storage's writes get `503` with `Retry-After: 5`, `supavise-storage` stops, a last pass runs over the files that no longer change, and Storage starts on the bucket. Reads fail from the stop to the start. The files are kept as `objects.migrated-<date>`; `--rollback` copies back what changed. Storage's S3 key, `<ref>/<bucket>/<name>/<version>`, equals the file backend's relative path, so the copy needs no mapping.

On AWS the stack makes a `StorageRole` scoped to the objects bucket, and the instance role may assume exactly that role. The daemon assumes it, caches the credentials until five minutes before they end and serves them on `127.0.0.1:[fleet] storage_credentials_port` in the container-credential shape, to `supavise-storage` only, with a token that is random per boot. Storage's environment names the endpoint and holds no key. Off AWS, static `storage_s3_*` keys work as before.

### 12.8 Spike outcomes

These were proven before the dependent work was built. The spike code was not merged.

| Spike | Outcome |
|---|---|
| S1 Supavisor on a standby | It serves a replicated tenant against the replicated `_supavisor` without `bin/prepare`, which exits 1 on a standby. It needs the leader's `VAULT_ENC_KEY`. After a replicated row changes it keeps the old target until `GET /api/tenants/<id>/terminate`, so the leader asks only after the follower replayed the change. |
| S2 Background workers | `pg_cron` and `pg_net` workers do not start on a standby and start at promotion with no restart: no preload change. A replica connection cannot call `net.http_*`. The Edge Functions syncer only reads the registry. The replica must set `hot_standby=on`, because the project's cluster ships `hot_standby=off` and `postgresql.auto.conf` overrides it. |
| S3 PostgREST on a standby | The two-host connection string works. The notification alone missed up to 5 of 15 tables under 20 MB/s of WAL. A `SIGUSR1` timer made every change visible: with a 5-second timer the slowest was 4.96 s, and the signal to visibility took 63 to 103 ms. The default is `schema_reload_seconds = 10`. |
| S4 Realtime | After a promotion Realtime recreates its slots and `EnsureTenant` does nothing (the fingerprint matches). Before a planned stop, quiesce the tenant (`POST /api/tenants/<ref>/reload`) and keep clients out until the stop ends: without that the stop timed out at 90 s on Realtime's logical walsender. Keep the daemon, and with it the WAL relay, up until the cluster has stopped. |
| S5 Storage | Its S3 key equals the file backend's path, so migration is a plain copy with content type and cache control. Names that are not valid UTF-8 are reported, not copied. It honors `AWS_CONTAINER_CREDENTIALS_FULL_URI` on loopback with `AWS_CONTAINER_AUTHORIZATION_TOKEN`, caches the credentials and fails with a wrong token. |
| S6 Mesh throughput | Physical replication through a TLS and smux forwarder kept lag p99 at 0.10 to 0.41 s at 20 MB/s of WAL over 2 to 70 ms of round trip, only with sized windows: at least 4 MiB per stream and 16 MiB per session, and the receiver's setting governs. The walreceiver streamed again within 5.2 s of the forwarders returning. The smux keepalive timeout must stay below `wal_receiver_timeout` (60 s). Forwarder CPU was 10 to 12 percent of a core per side at 45 to 50 MB/s. |
| S7 Catch-up | A standby with a broken stream catches up through `restore_command` and the relay and resumes streaming, also when the primary has recycled segments. The rebuild is for WAL that left the archive. `pg_basebackup` is not in the artifact, so seeding uses the tarball; a seeded directory needs `postmaster.opts`. |
| S8 AWS | The owner's rehearsal checklist: `UsePreviousValue` for parameters new to a stack, a tags-only update with no instance interruption, tags in the metadata service without a restart, `DryRun` of the fencing calls, and the association to a secondary private address. It has not run. |
| S9 Two-node CI | Incus virtual machines on amd64 and privileged system containers on arm64. Unprivileged containers do not enforce `IPAddressDeny`. |

### 12.9 As built, against the design

- Replica identifiers, hosts and the Studio contract follow the design. Two corrections: `infrastructure:read_replicas` is a key of `disabled_features` in the Studio profile, removed when the replica controller is wired and two nodes are active, not a permission check; and the read-only connection string keeps the caller's role on a replica, because Studio's reports read `pg_stat_statements` as `postgres` and no role has to reach the replica with the WAL first.
- The replica controller's lag state is memory, plus the snapshot file of I5.
- `supavise upgrade` acts on the node it runs on: it plans, backs up and rolls out the projects homed there. It does not set the cluster's maintenance announcement and does not order servers, except that a release whose manifest says `wal_compat: false` is refused on a server that holds a primary while another server holds an older standby of it. The release-match gate holds the automatic modes while two nodes run different releases. Upgrade the leader first when a release adds registry migrations.
- A major upgrade of a project with replicas is refused, and `--drop-replicas` does not exist.
- The alert kinds `replica_needs_rebuild` and `storage_not_s3` are defined and nothing raises them. A replica that falls behind WAL that left the archive stays `ACTIVE_UNHEALTHY`.
- The AWS stack, the rehearsal script and the fencing onto a replica server have not run against AWS (section 12.6). The two-server end-to-end run has no recorded RPO or RTO.

## 13. OAuth sign-in and the remote MCP endpoint

An MCP client connects to `https://api.<domain>/mcp?project_ref=<ref>`, finds the authorization server, registers itself, and opens Studio's "Authorize API access" page. A signed-in Owner or Administrator picks an organization and approves. The client receives tokens and calls `/mcp` with them. Studio's Connect > MCP panel shows the URL, and the organization's OAuth Apps page lists the client with a Revoke button. No personal access token is needed. `[api] disable_oauth = true` turns all of it off: every route below answers 404 and Studio gets no MCP URL. Personal access tokens and dashboard sessions are not affected.

### 13.1 The authorization server

The Management API is the authorization server. The issuer is `cfg.APIURL()` (which honors `[api] public_url`) and the MCP resource is `<issuer>/mcp`. The feature needs no extra host name, process or required configuration key. The code is in `internal/oauth` (rules and stores) and `internal/api/oauth_*.go` (HTTP).

| Route | What it does |
|---|---|
| `GET /.well-known/oauth-protected-resource/mcp`, `GET /.well-known/oauth-authorization-server` | Discovery. Built from configuration, never from the `Host` header. S256 is the only PKCE method, and `scopes_supported` lists 13 scopes. Open CORS. |
| `POST /platform/oauth/apps/register` | Open dynamic client registration (RFC 7591), as on hosted. A redirect URI is `https`, or `http` on `localhost`, `127.0.0.1` or `[::1]`; no custom schemes. Limited per client address and by a cap on stored apps; unused apps are pruned. |
| `GET /v1/oauth/authorize` | Validates the request, stores it as pending for 10 minutes and redirects to Studio's `/authorize?auth_id=…`. Refuses an unknown client or an unregistered redirect URI with an HTML page and no redirect. Every other error returns to the client with `error`, `state` and `iss`. |
| `GET /platform/oauth/authorizations/{id}`, `POST` and `DELETE /platform/organizations/{slug}/oauth/authorizations/{id}` | The calls Studio's consent page already makes: describe, approve, decline. Only an Owner or Administrator of the chosen organization approves. When the approver belongs to an organization that enforces MFA, the approval needs an MFA-verified session (aal2), as creating a personal access token does. |
| `POST /v1/oauth/token`, `POST /v1/oauth/revoke` | Code exchange, refresh rotation and revocation. |
| Organization apps and client secrets (8 operations under `/platform/organizations/{slug}/oauth/apps`) | The OAuth Apps page: the authorized list, revoke, manual apps and their client secrets. Studio's page needs no change. |

PKCE is required for dynamic apps. A code lives for 60 seconds, works once, and is bound to the client, the exact redirect URI, the resource, the challenge, the approving user and the organization; a redemption that fails any of these burns the code. Redeeming a code a second time revokes the grant the first redemption created and raises an `oauth_token_reuse` alert. Redirect matching is exact, except that a registered loopback `http` URI may differ in port (RFC 8252). Hosted Supabase differs in a few places on purpose (section 13.7).

### 13.2 Tokens and grants

| Item | Format | Lifetime |
|---|---|---|
| Access token | `sbp_oauth_` + 40 hex (matches the CLI's token pattern) | 1 hour |
| Refresh token | `sbr_` + 64 hex, rotated on every use | 90 days, renewed by each rotation |
| Authorization code | `sbc_` + 64 hex | 60 seconds, single use |
| Client secret | `sba_` + 64 hex, shown once, listed as `sba_xxxx********` | none |

A grant is (app, user, organization, scopes, resource) and is the unit of revocation. Secrets, codes and tokens are stored as SHA-256 hashes and appear in no log line, audit event, alert or error message. A second use of a refresh token within 10 seconds of the first issues a fresh pair, so two processes that refresh at once do not force a sign-in; a later reuse revokes the grant. Tokens live in the `oauth_*` tables (migration `1350_oauth.sql`) and not in `access_tokens`.

### 13.3 What an OAuth token may do

`authenticate()` sends `sbp_oauth_` tokens to their own lookup. The result is a principal bound to one organization: `members.Access.Restrict(orgID)` leaves that organization's membership and nothing else, so every later check sees one organization. Rights are recomputed on each request from the approver's live role. A role downgrade applies at once, and a token never exceeds its approver's current rights.

Scopes come from the `x-oauth-scope` annotations of the pinned specs (134 of 169 operations in `v1`, 21 of 50 in `v2`), plus an override for the two storage-config operations. An operation without an annotation answers `403 This operation is not available to OAuth tokens`; a scope the token lacks answers `403` with an `insufficient_scope` challenge. Matching is exact, so a write scope does not imply its read scope. OAuth tokens never reach `/platform/**`, which means they cannot approve a grant, and they cannot call `/v1/profile` or create organizations. A token ends at once when its user is removed or no longer admitted by single sign-on, leaves the organization, or when the app or the grant is revoked (`supavise oauth grants revoke`, Studio's Revoke button, `POST /v1/oauth/revoke`).

### 13.4 The `/mcp` endpoint

`api.<domain>/mcp` is a protected resource. The gate sits at the edge, in the proxy's `serveAPI`, and not in the API mux, so `/mcp` is not served on the loopback admin listener.

1. With no bearer, the gate answers `401` with `WWW-Authenticate: Bearer resource_metadata="<issuer>/.well-known/oauth-protected-resource/mcp"`.
2. It accepts an OAuth access token or a personal access token (hosted accepts both). A dashboard session, an unknown, expired or revoked token, or a grant bound to another resource answers `401 invalid_token`, which clients read as the signal to refresh. The check has to happen here: `initialize` and `tools/list` make no Management API call, so without it a revoked token would never see a 401.
3. It limits each grant or token to 600 requests per minute and 8 in flight, rebuilds the query from `project_ref`, `read_only`, `features` and `skip_elicitations`, caps the body at 8 MiB, strips cookies, and forwards through the proxy's `forward()` to Studio's loopback `/api/mcp`. `studio.<domain>/api/mcp` answers 404.

Studio's MCP route runs the MCP server package that Studio bundles. It calls the Management API on loopback with the same bearer, so every tool call is authenticated and scoped again by the API. The route is stateless: it uses the Streamable HTTP transport without sessions, so `skip_elicitations` is accepted and ignored, and a tool that would ask the client to confirm a cost (`create_project`, `create_branch`) cannot ask. The `mcp-e2e` job records what `create_project` does on this route. `search_docs` calls `supabase.com` from the node, as on hosted. The route uses the stateless transport of the 1.x SDK, so a client that speaks only a newer protocol revision may not connect. The `mcp-e2e` job tries two revisions, and the manual client check records which revision each client speaks.

### 13.5 Studio patch 0004

Hosted runs MCP as a separate service. Supavise has no such process, and Studio already bundles the server package with every tool, so a fourth patch makes Studio's existing `/api/mcp` route work in platform mode: `lib/hosted-api-allowlist.ts` gains `/mcp`, a new `lib/api/platform-mcp.ts` builds the platform with `createSupabaseApiPlatform({accessToken, apiUrl})` and fills `get_project_url` from `SUPAVISE_PROJECT_URL_TEMPLATE`, and `pages/api/mcp/index.ts` calls it first when `IS_PLATFORM` is set and raises the body limit to 8 MB. The query is validated and fails closed (`read_only` other than `true` or `false` is a 400, never read-write). The patch is the only way to ship the hosted tool set without a fifth shared process or a Go copy of about 30 tools. It is specific to Supavise, so unlike patches 0001 to 0003 it is not proposed upstream, and each Studio bump may need it rebased (`pages/api/mcp/index.ts` is the likely conflict). `studio/PATCHSET` counts it in the artifact name.

One placeholder, `NEXT_PUBLIC_MCP_URL`, carries the URL into Connect > MCP (`required: false`, so an old unit with a new artifact shows the old localhost URL and does not fail to start). The fleet sets it with `SUPAVISE_MANAGEMENT_API_URL` and `SUPAVISE_PROJECT_URL_TEMPLATE`, and omits all three under `disable_oauth`.

### 13.6 Operations

- **Audit and alerts.** Each state change writes an `oauth.*` event; payloads carry ids, the organization slug, the user id, a sanitized client name, the redirect host and scopes, and no token, code, secret, `state` or challenge. A code or refresh-token replay raises one `oauth_token_reuse` alert per grant.
- **Limits** (in memory, per node): 100 registrations per 10 minutes and 5,000 stored dynamic apps; 60 authorize requests per minute, 25 pending per app and 10,000 pending in all; 30 token or revoke failures per minute; 600 `/mcp` requests per minute and 8 in flight per principal. Every 429 carries `Retry-After`.
- **Pruning.** The register and token handlers prune old authorizations, tokens, revoked grants and unused dynamic apps, at most once per 10 minutes per node.
- **Rollback.** Migration 1350 adds five tables and changes none. Because the registry holds a migration an older release does not know, `supavise rollback` refuses until the system cluster is restored from its pre-upgrade backup (`deploy/README.md`). An older binary does not read OAuth tokens as personal access tokens, since they are not in `access_tokens`.

### 13.7 Differences from hosted

| Area | Hosted | Supavise | Reason |
|---|---|---|---|
| PKCE | S256 and `plain` | S256 only | `plain` does not meet the MCP authorization rules |
| Grant types | adds `jwt-bearer` | not supported | beta, and tied to higher plans |
| Discovery hosts | an MCP host and an API host | one host | no extra DNS name |
| `revocation_endpoint` | not advertised | advertised | clients can revoke on sign-out |
| Dynamic app logo | probably shown | never returned | a self-asserted logo spoofs the consent page |
| Public clients | a secret is issued even to a `none` client | a dynamic app may omit its secret when its PKCE verifier is right | tolerates clients that registered `none` |
| CORS | echoes the origin, with credentials | `*`, no credentials | cannot leak cookies |
| `scope` at authorize | deprecated | honored, narrowing only | least privilege |
| `skip_elicitations` | works | accepted and ignored | stateless transport |
| `get_project_url` | correct | correct on the remote route; the stdio server still derives `supabase.red` | the package derives the host for unknown API hosts |
| Redirect schemes | not verified | `https` and loopback `http` only | no custom schemes |

Whether hosted lets a Developer open the consent page, the `expires_in` of a hosted token, and what an OAuth token gets on `GET /v1/projects/{ref}/config/storage` are unverified; the defaults here come from the specs and from the shape of hosted's discovery documents.

### 13.8 Proving tests

`internal/oauth` (rules, PKCE vectors from RFC 7636, redirect matcher and its fuzz target, fake-clock tests for expiry and rotation, a dump of every `oauth_*` table that finds no plaintext), `internal/api` (the endpoints, the scope golden file, the revocation matrix, the role matrix) and `internal/proxy` (the `/mcp` forward, the frame headers) cover the rules. The `oauth-smoke` job of `linux.yml` runs the whole flow against a real node, and the `mcp-e2e` job of `studio.yml` adds the MCP route on the Studio build. `upgrade-e2e` checks migration 1350 and that a token made before the upgrade still works. The checks that only a person can make (the Claude Code, Cursor, VS Code, Codex and Claude desktop flows, and the consent page as a Developer) are listed in the pull request that ships the feature.

Supavise is not affiliated with or endorsed by Supabase Inc.
