# 05 - Agentic surface verification (Studio platform mode, CLI, MCP, agent skills)

Verified 2026-10-06 against live sources. Method: shallow clones of each repo's default branch, reading code, plus empirical runs of the installed CLI (2.119.0, Homebrew) against a local mock API. Items I did not execute are labeled "static read" or "unverified".

Pinned revisions (so claims are reproducible):

| Repo | Default branch | Commit read | Latest release |
|---|---|---|---|
| [supabase/cli](https://github.com/supabase/cli) | `develop` | `753520f` (2026-10-06) | v2.120.0 (2026-10-06, npm `supabase` 2.120.0) |
| [supabase/mcp](https://github.com/supabase/mcp) (old name supabase-community/supabase-mcp, redirects) | `main` | `8a26508` (2026-10-06) | `@supabase/mcp-server-supabase` 0.13.0 (2026-09-17) |
| [supabase/supabase](https://github.com/supabase/supabase) | `master` | `1a99276` (2026-10-06) | n/a (Studio `apps/studio` version 0.0.9) |
| [supabase/agent-skills](https://github.com/supabase/agent-skills) | `main` | `c9be0e9` (2026-10-02) | skill `supabase` 0.1.2 |

## 1. Verdict table

| # | Question | Verdict | Evidence |
|---|---|---|---|
| 1a | CLI can be pointed at a custom Management API host | **Yes, but only through a "profile" file, not `SUPABASE_API_URL`.** `--profile <file>` or `SUPABASE_PROFILE=<file>` (YAML/TOML/JSON) with `api_url`, `dashboard_url`, `project_host`, `pooler_host`. `SUPABASE_API_URL` is ignored by commands (tested). `SUPABASE_INTERNAL_API_HOST`, `--api-url`, `DEFAULT_API_HOST` do not exist on `develop`. | `apps/cli/src/command-internal/profile-load.ts`; `apps/cli/src/commands/db/advisors/SIDE_EFFECTS.md` line 51 ("`SUPABASE_API_URL` is **not** honored"); empirical test in section 2.1; `apps/cli-go/internal/utils/profile.go` (Go `Profile` struct, `GetSupabaseAPIHost()`) |
| 1b | Upstream CLI works against a custom control plane for link / db push / db pull / functions deploy / secrets / gen types / branches | **Yes in principle, with strict-schema and host-derivation caveats.** The TS CLI decodes responses strictly (a `{}` body fails with `SchemaError`). It also derives `https://<ref>.<project_host>` and `db.<ref>.<project_host>` and so needs wildcard DNS. An independent project (kmhari/supastack) claims the same CLI works with a profile TOML. | Empirical `link` test; `SIDE_EFFECTS.md` per command (section 2.3); `docs/supabase-cli.md` in [kmhari/supastack](https://github.com/kmhari/supastack) |
| 1c | OpenAPI spec location | **Public, no auth.** `https://api.supabase.com/api/v1-json` (115 paths / 169 ops), `/api/v2-json` (34 / 50), `/api/platform-json` (299 / 394), `/api/v1-yaml`. The CLI repo keeps a merged v1+v2 snapshot at `packages/api/src/generated/openapi.json`. | HTTP 200 on all four today; `packages/api/README.md` "Spec pipeline" |
| 2a | MCP package accepts a custom Management API URL | **Yes.** `--api-url` (CLI flag) and `apiUrl` option in `createSupabaseApiPlatform`. There is no `SUPABASE_API_URL` env var for MCP; only `SUPABASE_ACCESS_TOKEN` and `SUPABASE_CONTENT_API_URL`. The docs page does not mention `--api-url`, the code does. | `packages/mcp-server-supabase/src/cli.ts`; `src/platform/api-platform.ts` line 94-96; `server.json` |
| 2b | Hosted `mcp.supabase.com` (OAuth 2.1) is open source | **No (not in the repo).** The repo ships stdio, a local `--http` entry that takes a Bearer PAT (no OAuth), and `createSupabaseMcpHandler()` for embedding in your own HTTP server. The OAuth 2.1 + DCR front end is yours to build. | `README.md` "Self-hosting the MCP endpoint"; `src/transports/local-http-entry.ts`; no OAuth code outside generated types; [docs](https://supabase.com/docs/guides/getting-started/mcp) |
| 2c | What breaks on a custom control plane | (1) `get_project_url` hard-codes `https://<ref>.supabase.co/.green/.red` from the API hostname. (2) `get_logs` / `query_logs` send ClickHouse-dialect SQL to `/analytics/endpoints/logs`. (3) `--http` mode refuses a custom `--api-url` without `--secret-url-template`. (4) Secret-collection URL points at a dashboard route. (5) `create_project`, `get_cost`, branching, notebooks, v2 advisors need cloud-only endpoints. | `src/platform/api-platform.ts` lines 399-403, 1002-1011; `src/logs.ts`; `src/transports/local-http-entry.ts` lines 104-118 |
| 3a | Studio in platform mode can run against a custom API | **Yes, with a self-built image and a large API surface.** The published `supabase/studio` image is built with no build args, so `NEXT_PUBLIC_IS_PLATFORM` is false. The flag is inlined at build time in both the Next and the Vite/TanStack build. | `.github/workflows/publish_image.yml`; `apps/studio/lib/constants/index.ts`; `apps/studio/vite.config.ts` lines 432-470; supastack's `infra/studio-platform/Dockerfile` |
| 3b | Disable billing / telemetry / ConfigCat | **No single switch. Partial levers exist.** ConfigCat self-skips when its two env vars are unset. PostHog self-skips with no key. Telemetry POSTs are consent-gated. Billing UI is gated by `profile.disabled_features` (`billing:all`), but billing queries and entitlements still fire, so the API must answer them. | `packages/common/configcat.ts`; `packages/common/posthog-client.ts`; `packages/common/enabled-features/README.md`; `apps/studio/hooks/misc/useCheckEntitlements.ts` |
| 3c | Anyone has run it | **Yes: one live, one dead.** [kmhari/supastack](https://github.com/kmhari/supastack) (AGPL-3.0, created 2026-05-22, last push 2026-08-27, 0 stars, single author) runs platform-mode Studio with 3 source-file patches. [HarryET/supa-manager](https://github.com/HarryET/supa-manager) (last push 2024-11-20) needed 17 patches against a 2024 Studio. | `SUPASTACK-PATCHES.md` on branch `kmhari/supabase@supastack-studio`; `studio/patches/*.patch` in supa-manager |
| 3d | Studio `/mcp` endpoint | **It is the self-hosted/CLI MCP, not a platform feature.** Route `apps/studio/routes/api/mcp/index.ts` (twin `pages/api/mcp/index.ts`) builds a `SupabasePlatform` that talks straight to the local DB. In platform mode `/api/mcp` is not in `HOSTED_SUPPORTED_API_URLS`, so it 404s. | `apps/studio/lib/hosted-api-allowlist.ts`; `apps/studio/lib/api/self-hosted/mcp.ts`; [self-hosting MCP doc](https://supabase.com/docs/guides/self-hosting/enable-mcp) (no OAuth, "not intended to be exposed to the Internet") |
| 4 | Agent skills hosted-only or URL-agnostic | **Mostly URL-agnostic guidance, with hosted URLs hard-wired in a few places.** Details in section 5. | `skills/supabase/SKILL.md` |

Headline: the three tools do not need forks, but each has one hosted assumption you must neutralize (CLI profile file; MCP project URL and log dialect; Studio self-built image plus sign-in patches). The control-plane work that dominates is the response-schema fidelity of roughly 60 v1 endpoints plus roughly 120 `/platform/*` endpoints for Studio.

---

## 2. Question 1 - Supabase CLI

### 2.1 Custom API base URL

Important structural change: as of v2.120.0 the `supabase` binary is a Bun/TypeScript CLI (Effect). The Go tree is kept only as a sidecar for a few proxied commands (`apps/cli/docs/go-cli-porting-status.md`: `db diff --use-pg-schema`, `db branch *`, `db remote changes`, `gen keys`, `functions download --legacy-bundle`). Older research that cites Go internals (`internal/utils/api.go`) describes the legacy implementation; the TS implementation keeps its semantics.

What exists on `develop`:

- **Profile (works).** `apps/cli-go/internal/utils/profile.go` defines the struct (`name`, `api_url`, `dashboard_url`, `docs_url`, `project_host`, `pooler_host`, `client_id`, `studio_image`, `regions`). The TS loader `apps/cli/src/command-internal/profile-load.ts` reads built-in names (`supabase`, `supabase-staging`, `supabase-local`, `snap`) or a file path with a viper-supported extension (`toml`, `yaml`, `json`, ...). Selection order: `--profile` flag, then `SUPABASE_PROFILE`, then `~/.supabase/profile` file, then `supabase` (`apps/cli/src/config/command-settings.layer.ts`). Unknown keys fail. Tokens are stored per profile `name` in the OS keyring.
- **`SUPABASE_API_URL` (does not work for commands).** It is read only by `apps/cli/src/shared/config/cli-settings.layer.ts` (stack/telemetry settings) and `packages/api` (spec generation). `apps/cli/src/commands/db/advisors/SIDE_EFFECTS.md` states it is not honored.
- **Not present:** `SUPABASE_INTERNAL_API_HOST`, `--api-url`, `DEFAULT_API_HOST`. `GetSupabaseAPIHost()` and `GetSupabaseHost(ref)` exist only in the Go sidecar (`apps/cli-go/internal/utils/api.go`) and read `CurrentProfile`.
- **Planned change (drift risk).** `apps/cli/docs/profile.md` describes replacing profiles with a "platform" concept: env vars `SUPABASE_API_URL`, `SUPABASE_DASHBOARD_URL`, `SUPABASE_PROJECT_HOST`, `SUPABASE_POOLER_HOST`, a JSON file under `SUPABASE_HOME`, and "no backward compatibility with Go CLI profile names". Marked as design, not implemented (the code still resolves profiles). Re-check before each CLI bump.

Empirical test (CLI 2.119.0, mock server on 127.0.0.1:18080 logging requests, token `sbp_` + 40 hex):

| Invocation | Result |
|---|---|
| `SUPABASE_API_URL=http://127.0.0.1:18080 supabase projects list` | Went to the real API: `{"message":"Unauthorized"}`; mock saw nothing |
| `supabase --profile ./prof.yaml projects list` | Mock saw `GET /v1/projects`, `Authorization: Bearer sbp_...`, `User-Agent: SupabaseCLI/2.119.0` |
| `SUPABASE_PROFILE=$PWD/prof.yaml supabase projects list` | Same as above |
| `supabase --profile ./prof.yaml link --project-ref abcdefghijklmnopqrst` with the mock returning `[]`/`{}` | `GET /v1/projects/<ref>` then `SchemaError(Expected object)`: **responses are decoded strictly against the generated schema** |

Minimal profile (YAML shown; TOML also accepted, supastack distributes TOML):

```yaml
name: selfhost
api_url: https://api.example.dev
dashboard_url: https://example.dev/dashboard
project_host: example.dev      # tenant gateway = https://<ref>.example.dev ; DB = db.<ref>.example.dev
pooler_host: example.dev       # optional; empty disables the pooler-domain assertion
```

Related facts:

- Access token pattern: `^sbp_(oauth_|v0_)?[a-f0-9]{40}$` (`apps/cli/src/auth/access-token.ts`, `apps/cli-go/internal/utils/access_token.go`). Env `SUPABASE_ACCESS_TOKEN` wins over keyring and `~/.supabase/access-token`.
- Project ref pattern `^[a-z]{20}$` is enforced client-side (`apps/cli/src/shared/services/services.shared.ts` line 62; branch resolver). Your ref generator must emit 20 lowercase letters.
- `supabase login` (no `--token`) is a device flow: opens `{dashboardUrl}/cli/login?session_id&token_name&public_key`, then polls `GET {api}/platform/cli/login/{session_id}?device_code=` expecting `{access_token, public_key, nonce}` (`apps/cli/src/commands/login/SIDE_EFFECTS.md`). Studio's side is `POST /platform/cli/login` (`apps/studio/data/cli/login.ts`; page `apps/studio/pages/cli/login.tsx`). With `supabase login --token sbp_...` none of this is needed. After login it calls `GET /v1/profile` best-effort for `gotrue_id`.
- `supabase link` also probes the tenant gateway at `https://<ref>.<project_host>` (`/rest/v1/`, `/auth/v1/health`, `/storage/v1/version`, best-effort), hard-coded `https` (`services.shared.ts` line 532).
- Direct DB commands (`db push --linked`, `db pull`, `db dump`, `migration list --linked`, `inspect db --linked`, `gen types --lang != typescript`) connect to Postgres at `db.<ref>.<project_host>` and fall back to the pooler (`apps/cli/src/command-internal/db-config.layer.ts` line 351 onward). A password in `SUPABASE_DB_PASSWORD` avoids the `cli/login-role` call.
- `pg-meta`: the CLI no longer calls pg-meta (typegen runs in-process via `@supabase/postgrest-typegen`; no pg-meta container). The only Management API route that returns types is `GET /v1/projects/{ref}/types/typescript`, used for `--linked` / `--project-id` TypeScript (`gen/types/SIDE_EFFECTS.md`).
- Updates: the CLI checks GitHub releases for upgrades, unrelated to the API.

### 2.2 OpenAPI spec locations

| Spec | URL (HTTP 200 on 2026-10-06) | Size | Paths / operations |
|---|---|---|---|
| v1 | `https://api.supabase.com/api/v1-json` (also `/api/v1-yaml`) | 349 KB | 115 / 169 |
| v2 | `https://api.supabase.com/api/v2-json` | 361 KB | 34 / 50 |
| platform (Studio-internal) | `https://api.supabase.com/api/platform-json` | 803 KB | 299 / 394 |
| Swagger UI | `https://api.supabase.com/api/v1` | n/a | n/a |

Typed copies in the repos: CLI `packages/api/src/generated/openapi.json` (merged v1+v2, 137 paths / 200 ops, `operationId`s like `v1-get-project`); Studio `packages/api-types/types/{api-v1,api-v2,platform}.d.ts` (generated from the same three URLs by `packages/api-types/scripts/verify-production-types.mjs`, which defaults `PLATFORM_API_OPENAPI_URL` to `/api/platform-json`). The platform spec is the Studio contract. Caveat: Studio also calls a few paths that are not in the committed platform types (supastack lists them as "not in platform.d.ts").

### 2.3 Endpoints per CLI flow (v2.120.0, TS CLI)

Source: the `## API Routes` tables in each command's `apps/cli/src/commands/**/SIDE_EFFECTS.md`, cross-checked with the generated client. All paths are under `{api_url}`; auth is `Authorization: Bearer <sbp_ token>`.

| Flow | Method and path | Notes |
|---|---|---|
| `login --token` | `GET /v1/profile` | best-effort, reads `gotrue_id` |
| `login` (browser) | `GET /platform/cli/login/{session_id}?device_code=` | not in v1 spec; no auth |
| `projects list` | `GET /v1/projects` | |
| `orgs list` | `GET /v1/organizations` | from generated client |
| `link` | `GET /v1/projects/{ref}` (404 tolerated, treated as branch) | `INACTIVE` status aborts |
| | `GET /v1/projects/{ref}/api-keys?reveal=true` | needs anon and service_role keys |
| | `GET /v1/projects/{ref}/config/storage`, `GET /v1/projects/{ref}/config/database/pooler` | best-effort |
| | `GET /v1/projects/{parentRef}/branches` | only when a branch name is given |
| `db push --linked` / `db pull` / `db dump` / `migration *` | none required if `SUPABASE_DB_PASSWORD` is set; else `POST /v1/projects/{ref}/cli/login-role` (body `{read_only}`) | then direct Postgres. `db/pull/SIDE_EFFECTS.md` lists `/roles` and `/pooler/config`; the code comments in `db-config.layer.ts` say `cli/login-role` and `config/database/pooler`, so I treat the SIDE_EFFECTS paths as stale |
| | `GET /v1/projects/{ref}/config/database/pooler` | IPv4 pooler fallback |
| | `GET /v1/projects/{ref}/network-bans`, `DELETE /v1/projects/{ref}/network-bans` | only after a pooler self-ban |
| `db query --linked` | `POST /v1/projects/{ref}/database/query` body `{"query"}`, returns 201 + row array | |
| `db advisors --linked` | `GET /v1/projects/{ref}/advisors/security`, `.../advisors/performance` | `{lints: [...]}` |
| `functions deploy` | `GET /v1/projects/{ref}/functions`, `POST /v1/projects/{ref}/functions/deploy` (multipart, `slug` query), fallback `POST /v1/projects/{ref}/functions`, `PATCH /v1/projects/{ref}/functions/{slug}`, `PUT /v1/projects/{ref}/functions` (bulk), `DELETE .../functions/{slug}` | |
| `functions list` / `delete` / `download` | `GET /v1/projects/{ref}/functions`, `DELETE .../{slug}`, `GET .../{slug}/body` | |
| `secrets set` | `POST /v1/projects/{ref}/secrets` body `[{name,value}]` (201) | |
| `secrets list` | `GET /v1/projects/{ref}/secrets` | value is a digest |
| `secrets unset` | `DELETE /v1/projects/{ref}/secrets` body `["NAME"]` (200) | |
| `gen types` (typescript, linked) | `GET /v1/projects/{ref}/types/typescript` | other langs: DB connection |
| | `GET /v1/projects/{ref}` (404 means branch), `GET /v1/branches/{id_or_ref}` | |
| `branches create` | `POST /v1/projects/{ref}/branches` body `{branch_name, is_default:false, git_branch?, region?, desired_instance_size?, persistent?, with_data?, notify_url?}` | on 4xx also `GET /v1/projects/{ref}` and `GET /v1/organizations/{slug}/entitlements` |
| `branches list` / `get` / `delete` / `update` | `GET /v1/projects/{ref}/branches`, `GET /v1/projects/{ref}/branches/{name}`, `GET /v1/branches/{id_or_ref}`, `DELETE /v1/branches/{id_or_ref}`, `PATCH /v1/branches/{id_or_ref}` | `branches pause/unpause` use `/v1/projects/{branch_ref}/pause` |
| `postgres-config get/update` | `GET` / `PUT /v1/projects/{ref}/config/database/postgres` | raw HTTP |

Not verified at runtime: only `projects list` and `link` were exercised against a mock. The rest is from the per-command docs that live next to the handlers (they are maintained alongside the code, and one is visibly stale, see `db pull`).

---

## 3. Question 2 - Supabase MCP server

### 3.1 Repo, package, options

- Repo: `https://github.com/supabase/mcp` (the old `supabase-community/supabase-mcp` URL redirects; Apache-2.0; 2.9k stars). Monorepo packages: `mcp-server-supabase` (npm `@supabase/mcp-server-supabase` 0.13.0, registry name `com.supabase/mcp`), `mcp-server-postgrest`, `mcp-utils`.
- Docs say the hosted URL is `https://mcp.supabase.com/mcp` (OAuth 2.1 with dynamic client registration; for CI "pass a scoped PAT as a header"), query params `read_only`, `project_ref`, `features`, `skip_elicitations` ([docs](https://supabase.com/docs/guides/getting-started/mcp)).
- Options in `src/cli.ts`: `--access-token` (or env `SUPABASE_ACCESS_TOKEN`), `--project-ref`, `--read-only`, `--api-url`, `--content-api-url` (env `SUPABASE_CONTENT_API_URL`), `--secret-url-template` (needs `--http`), `--features`, `--http`, `--port` (default 3111), `--version`. There is no `SUPABASE_API_URL` env for MCP. Library API: `createSupabaseApiPlatform({accessToken, apiUrl})` and `createSupabaseMcpHandler({platform, ...})` (`README.md`, "Self-hosting the MCP endpoint").
- `--http` (added in 0.13.0, PR #401) is a local HTTP entry: Bearer PAT per request, optional `project_ref` / `read_only` / `features` / `skip_elicitations` query params, Host-header validation for localhost. It is a dev convenience (`CONTRIBUTING.md`), not an OAuth server.
- Generated types come from the same public specs: `scripts/generate-management-api-types.mjs` fetches `api/v1-json` and `api/v2-json`.

### 3.2 Endpoints the MCP server calls (`src/platform/api-platform.ts`)

| Tool group / tool | Endpoint(s) |
|---|---|
| account: `list_organizations`, `get_organization` | `GET /v1/organizations`, `GET /v1/organizations/{slug}` |
| account: `list_projects`, `get_project` | `GET /v1/projects`, `GET /v1/projects/{ref}` |
| account: `create_project`, `get_cost`/`confirm_cost` | `POST /v1/projects` (cost is computed client-side from `pricing.ts` plus lists) |
| account: `pause_project`, `restore_project` | `POST /v1/projects/{ref}/pause`, `POST /v1/projects/{ref}/restore` |
| database: `execute_sql`, `list_tables`, `list_extensions` | `POST /v1/projects/{ref}/database/query` body `{query, parameters, read_only}` (`list_*` are SQL via this route) |
| database: `list_migrations`, `apply_migration` | `GET` / `POST /v1/projects/{ref}/database/migrations` |
| debugging: `get_logs` / `query_logs` | `GET /v1/projects/{ref}/analytics/endpoints/logs?sql=&iso_timestamp_start=&iso_timestamp_end=` |
| debugging: `get_advisors` | `GET /v1/projects/{ref}/advisors/security`, `.../advisors/performance`, and on `main` (2026-10-06, PR #420) `POST /v2/projects/{ref}/advisors/run` |
| development: `get_publishable_keys` | `GET /v1/projects/{ref}/api-keys?reveal=false`, `GET /v1/projects/{ref}/api-keys/legacy` |
| development: `generate_typescript_types` | `GET /v1/projects/{ref}/types/typescript` |
| development: `get_project_url` | none: computed locally (see 3.3) |
| functions | `GET /v1/projects/{ref}/functions`, `GET .../functions/{slug}`, `GET .../functions/{slug}/body`, `POST .../functions/deploy?slug=` (multipart) |
| branching | `GET`/`POST /v1/projects/{ref}/branches`, `DELETE /v1/branches/{id}`, `POST /v1/branches/{id}/merge`, `/reset`, `/push` (rebase) |
| storage | `GET /v1/projects/{ref}/storage/buckets`, `GET`/`PATCH /v1/projects/{ref}/config/storage` |
| secrets | `GET /v1/projects/{ref}/secrets`; `create_edge_function_secret` collects the value in the browser via `--secret-url-template` (`.../dashboard/mcp/secrets?ref={ref}&name={name}`), page `apps/studio/routes/mcp/secrets.tsx` |
| notebooks | `GET /v2/projects/{ref}/notebooks`, `GET /v2/projects/{ref}/notebooks/{id}` |
| docs: `search_docs` | GraphQL to the docs content API (`content-api/`), default hosted; `--content-api-url` overrides |

### 3.3 What breaks against a self-hosted control plane

1. **`get_project_url` is wrong for any custom domain.** `getProjectDomain()` maps `api.supabase.com` to `supabase.co`, `api.supabase.green` to `supabase.green`, anything else to `supabase.red` (lines 1002-1011), then returns `https://<ref>.<domain>`. The returned URL will be a dead `*.supabase.red` host. `getPublishableKeys` still works. Workaround without a fork: `createSupabaseApiPlatform` returns a plain object, so overwrite `platform.development.getProjectUrl` (supastack does the same kind of in-place edit in `apps/mcp/src/platform-build.ts`).
2. **Logs speak ClickHouse.** `src/logs.ts` builds queries like `select ... from logs where source = 'edge_logs'` with `log_attributes['...']`, sent as `sql=` to `/analytics/endpoints/logs`. Self-hosted analytics is Logflare/BigQuery SQL. Studio's own self-hosted platform sets `logsDialect: 'bigquery'` (`apps/studio/lib/api/self-hosted/mcp.ts`), but `createSupabaseApiPlatform` has no `logsDialect` option; you can set `platform.debugging.logsDialect = 'bigquery'` after creation (`debugging-tools.ts` reads `debugging.logsDialect ?? default`). `query_logs` hides `get_logs` when `queryLogs` is present. Otherwise strip the `debugging` group.
3. **`--http` + custom `--api-url`** throws unless `--secret-url-template` is supplied (`local-http-entry.ts` lines 104-118).
4. **OAuth 2.1 / dynamic client registration is not provided.** To give agents the "paste a URL, click authorize" experience you must build the OAuth server and a hosted endpoint around `createSupabaseMcpHandler` (supastack did: `apps/mcp`, `apps/api/src/routes/oauth`). PAT-in-header works today.
5. **Cloud-only tools need stubs or removal:** `create_project`/`get_cost`/`confirm_cost` (project creation semantics, org billing), branching (needs branch infrastructure), notebooks and v2 advisors (`/v2/*`), `storage` config. supastack strips advisors, storage config, create/cost and the whole branching group before serving.
6. **Drift:** supastack's `logs.all` route targets an older MCP behavior (`service` enum). MCP 0.10.0 (2026-08-10) added `query_logs`; 0.13.0 (2026-09-17) added `--http`, elicitation-based confirmations and a v2 client. Pin the MCP version and re-run a conformance check on each bump.

### 3.4 Studio's `/mcp` (CLI 2.119 era)

- Studio route: `apps/studio/routes/api/mcp/index.ts` (TanStack) and `apps/studio/pages/api/mcp/index.ts` (Next). It imports `createSupabaseMcpServer` from `@supabase/mcp-server-supabase` (`^0.12.0` in `apps/studio/package.json`), accepts `features` (`docs, database, development, debugging`) and `read_only`, uses `DEFAULT_PROJECT.ref` as project, and implements `database`/`development`/`debugging` operations against the local stack in `apps/studio/lib/api/self-hosted/*` (direct SQL, Logflare for logs).
- The CLI's local stack publishes `/mcp` mapped to Studio `/api/mcp` (`packages/stack/src/host/Endpoints.ts` lines 92-95). Self-hosted docs map `/mcp` to `http://studio:3000/api/mcp` and say there is no OAuth ([enable-mcp](https://supabase.com/docs/guides/self-hosting/enable-mcp)).
- It does not need the Management API at all. In platform mode `/api/mcp` is blocked by the allowlist, so it is not a path to multi-project MCP.

---

## 4. Question 3 - Studio in platform mode

### 4.1 Build and runtime facts

- **Build-time flag.** `IS_PLATFORM = process.env.NEXT_PUBLIC_IS_PLATFORM === 'true'` (`apps/studio/lib/constants/index.ts`). All `NEXT_PUBLIC_*` are inlined at build in the Next build and in the Vite/TanStack build (`vite.config.ts` lines 432-470). `apps/studio/Dockerfile` has `ARG STUDIO_FRAMEWORK=next` (switch with `--build-arg STUDIO_FRAMEWORK=tanstack`); `TANSTACK_MIGRATION.md` says both runtimes ship side by side and the Vite build is "what we run today". The published image `supabase/studio` is built by `.github/workflows/publish_image.yml` with no build args, so it is not a platform-mode image. You must build your own.
- **Variables that matter:** `NEXT_PUBLIC_IS_PLATFORM=true`, `NEXT_PUBLIC_API_URL` (origin of your control plane; the client strips a trailing `/platform`, `data/fetchers.ts`), `NEXT_PUBLIC_GOTRUE_URL` (dashboard login), `NEXT_PUBLIC_BASE_PATH` (optional), `NEXT_PUBLIC_SITE_URL`. supastack's `infra/studio-platform/Dockerfile` bakes a placeholder host and `sed`-replaces it at container start to keep one image domain-agnostic.
- **CSP in platform mode** (`apps/studio/csp.ts`): `connect-src` allows `NEXT_PUBLIC_API_URL`, `SUPABASE_URL`, `NEXT_PUBLIC_GOTRUE_URL` origins, `*.supabase.co/red` and, only if set at build, `NIMBUS_PROD_PROJECTS_URL` / `_WS`. A custom project domain such as `*.example.dev` is not covered by default; supastack apparently avoids this because it proxies through its own host (unverified). This is the least obvious blocker: budget a build-time CSP extension or a Studio patch.
- **Studio's own `/api/*` in platform mode** is restricted by `apps/studio/lib/hosted-api-allowlist.ts` (applied by `proxy.ts` and `start.ts`): only `/ai/*`, `/get-ip-address`, `/get-utc-time`, `/get-deployment-commit`, `/check-cname`, `/edge-functions/test|body`, `/generate-attachment-url`, `/incident-*`, `/status-*`, `/content/graphql`, `/parse-query`, `/scoped-access-token-permissions`, `/api/integrations/stripe-sync`. Everything else, including `/api/platform/*` (the self-hosted server routes for pg-meta, auth, storage, props) and `/api/mcp`, returns 404. All data therefore must come from your API at `NEXT_PUBLIC_API_URL`.
- **Auth to your API:** the browser sends `Authorization: Bearer <GoTrue access token>` on every `/platform/*` **and** `/v1/*` call (`data/fetchers.ts` `constructHeaders`), `credentials: 'include'`, header `X-Request-Id`. So `/v1/*` must accept GoTrue JWTs (Studio) and `sbp_` PATs (CLI, MCP).
- **pg-meta contract:** Studio's client uses only `POST /platform/pg-meta/{ref}/query` (body `{query, disable_statement_timeout}`, headers `x-connection-encrypted`, `x-pg-application-name`). The ten GET routes in the platform spec (`tables`, `columns`, ...) have no client callers in current code (static read). `pgMetaGuard` throws a client-side 400 if the project's `connectionString` is empty, so `GET /platform/projects/{ref}` must return a non-empty `connectionString` (an opaque string you decode yourself).

### 4.2 Dashboard login

- `packages/common/gotrue.ts`: `new AuthClient({url: NEXT_PUBLIC_GOTRUE_URL, storageKey: 'supabase.dashboard.auth.token'})`; `lib/gotrue.ts` re-exports it. `lib/auth.tsx`: `AuthProviderInternal alwaysLoggedIn={!IS_PLATFORM}`, so platform mode requires a real GoTrue session. `useSignOut` calls `gotrueClient.signOut()`.
- Sign-in form (`components/interfaces/SignIn/SignInForm.tsx`) calls `auth.signInWithPassword` and renders `<HCaptcha sitekey={process.env.NEXT_PUBLIC_HCAPTCHA_SITE_KEY!} size="invisible">` unconditionally. With no key the widget blocks sign-in ("hCaptcha has failed to initialize", per supastack). Also `getMfaAuthenticatorAssuranceLevel` runs after sign-in.
- Pre-login feature flags (`dashboard_auth:sign_up`, `sign_in_with_github`, `sign_in_with_sso`, `sign_in_with_chatgpt`, `show_testimonial`, `show_tos`) come from `packages/common/enabled-features/enabled-features.json`, compiled into the bundle. The runtime `ENABLED_FEATURES_*` override route is a no-op when `IS_PLATFORM` is true (`enabled-features/README.md`), and `profile.disabled_features` is unavailable before login. Hence supastack's patch 1.
- First login: `GET /platform/profile`; on error "User's profile not found" Studio calls `POST /platform/profile` (`lib/profile.tsx`); a 401 signs the user out.

### 4.3 What Studio needs from profile / organizations / projects

Required response fields (live platform spec, `components.schemas`):

| Endpoint | Schema | Required fields |
|---|---|---|
| `GET /platform/profile` | `ProfileResponse_Output` | `id, auth0_id, primary_email, username, first_name, last_name, mobile, is_alpha_user, gotrue_id, free_project_limit`; optional `disabled_features` (list of feature keys, e.g. `billing:all`) |
| `GET /platform/profile/permissions` | `AccessControlPermission[]` | `actions, condition, organization_id, organization_slug, resources, restrictive, project_ids, project_refs`. Permission checks (`hooks/misc/useCheckPermissions.ts`) match `%` as wildcard, `condition: null` means unconditional; an empty list denies everything |
| `GET /platform/organizations` | `OrganizationResponse_Output[]` | `id, slug, name, billing_email, billing_partner, integration_source, is_owner, stripe_customer_id, subscription_id, opt_in_tags, restriction_data, restriction_status, plan, usage_billing_enabled, organization_requires_mfa, requires_indirect_tax_declaration, organization_missing_address, organization_missing_tax_id` |
| `GET /platform/organizations/{slug}/entitlements` | `ListEntitlementsResponse` | list of `{feature:{key}, hasAccess, type, config}`. In platform mode a missing entitlement means `hasAccess=false` (`hooks/misc/useCheckEntitlements.ts`), so return every key as granted. Keys include `instances.read_replicas`, `custom_domain`, `log_drains`, `branching_limit`, `auth.*`, `storage.*`, `backup.*`, `function.*`, `realtime.*`, ... (full enum in the spec) |
| `GET /platform/projects/{ref}` | `ProjectDetailResponse_Output` | `cloud_provider, db_host, id, inserted_at, updated_at, name, organization_id, ref, region, status, subscription_id, connectionString, restUrl, is_branch_enabled, is_physical_backups_enabled, high_availability, integration_source`. `status` uses `ACTIVE_HEALTHY`, `COMING_UP`, `INACTIVE`, ... (`lib/constants/infrastructure.ts`) |
| `GET /platform/projects/{ref}/settings` | `ProjectSettingsResponse_Output` | `name, ref, status, inserted_at, db_dns_name, db_host, db_name, db_user, db_port, ssl_enforced, cloud_provider, region`; plus `app_config` (endpoint/protocol), `jwt_secret`, `service_api_keys` used for API docs, connect dialog, project URL |
| `GET /platform/organizations/{slug}/projects` | `OrganizationProjectsResponse` | `projects`, `pagination` (infinite query) |
| `GET /platform/projects/{ref}/status` | `{status: string}` | polled while a project is not healthy |

### 4.4 Calls Studio makes on load and on opening a project (static trace, not executed)

I traced hooks from `routes/__root.tsx` / `pages/_app.tsx`, `lib/profile.tsx`, `components/layouts/*` and `components/interfaces/ProjectHome`, then resolved each hook's path in `apps/studio/data/**`. I did not run the app, so treat per-call fail-soft behavior as unverified (supastack's mock answers unknown GETs with `{}` and non-GETs with 204).

**App shell (every page, platform mode):**

| Call | Source | Needed for render? |
|---|---|---|
| GoTrue `/auth/v1/*` at `NEXT_PUBLIC_GOTRUE_URL` | `common/gotrue.ts` | yes |
| `GET /platform/profile` (+ `POST` create) | `data/profile/profile-query.ts`, `lib/profile.tsx` | yes (blocks render) |
| `GET /platform/profile/permissions` | `data/permissions/permissions-query.ts` | yes (gates every action) |
| `GET /platform/organizations` | `data/organizations/organizations-query.ts` | yes |
| `GET /platform/telemetry/feature-flags?organization_slug=&project_ref=` | `common/feature-flags.tsx` | no (`Promise.allSettled`, failure logged) |
| `POST /platform/telemetry/identify`, `/event`, `/feature-flags/track` | `common/telemetry.tsx` | only after consent; see 4.5 |
| `GET /platform/notifications` | `data/notifications/notifications-v2-query.ts` | UI badge |
| `GET /platform/projects-resource-warnings` | `data/usage/resource-warnings-query.ts` | banner |
| Studio-local: `/api/get-deployment-commit`, `/api/incident-status`, `/api/incident-banner`, `/api/status-page`, `/api/cli-release-version` | allowlisted Studio routes | no; they fetch external status pages |

**Organization view (`/org/{slug}`, `/organizations`):**
`GET /platform/organizations/{slug}`, `GET /platform/organizations/{slug}/projects`, `GET /platform/organizations/{slug}/billing/subscription`, `GET /platform/organizations/{slug}/entitlements`, `GET /platform/organizations/{slug}/usage`, `GET /platform/organizations/{slug}/members` (+ `/members/invitations`), `GET /platform/projects` (infinite list). `useOrgSubscriptionQuery` is used in 17 files; `billing:all` hides billing screens but does not stop these queries.

**Project open (`/project/{ref}`):**
`GET /platform/projects/{ref}`, `GET /platform/projects/{ref}/status`, `GET /platform/projects/{ref}/settings`, `GET /platform/projects/{ref}/api-keys/temporary` and `GET /v1/projects/{ref}/api-keys`, `GET /v1/projects/{ref}/branches`, `GET /v1/projects/{ref}/health` (service status), `GET /v2/projects/{ref}/config`, `GET /platform/projects/{ref}/config/postgrest`, `GET /platform/projects/{ref}/config/storage`, `POST /platform/pg-meta/{ref}/query` (every table/schema/role/extension list is SQL through this one route), `GET /platform/projects/{ref}/content` (SQL snippets), `GET /platform/projects/{ref}/analytics/endpoints/usage.api-counts` and `.../logs.all` (home charts, logs), `GET /platform/database/{ref}/backups`, `GET /platform/integrations/github/connections`. Section pages add `/platform/auth/{ref}/{config,users,...}` (11 paths), `/platform/storage/{ref}/{buckets,objects,...}` (23 paths), `/platform/replication/{ref}/*` (23 paths), `/v1/projects/{ref}/functions*`, `/v1/projects/{ref}/secrets`, `/v1/projects/{ref}/config/auth/{signing-keys,third-party-auth}`, `/v1/projects/{ref}/network-restrictions*`, `/v1/projects/{ref}/upgrade*`, `/v1/projects/{ref}/database/migrations`.

Scale: the string-literal path references in Studio client code (data/, lib/, components/, hooks/, state/, excluding tests and type files) resolve to **297 distinct paths** (50 `/v1`, 8 `/v2`, rest `/platform`; some are fixture-only). `/platform/props/*` has no client callers (the Studio routes exist for the self-hosted server only).

### 4.5 Disabling billing / telemetry / ConfigCat

| Subsystem | Lever | Evidence |
|---|---|---|
| ConfigCat | Leave `NEXT_PUBLIC_CONFIGCAT_SDK_KEY` and `NEXT_PUBLIC_CONFIGCAT_PROXY_URL` unset: logs "Skipping ConfigCat set up" and returns no flags | `packages/common/configcat.ts` lines 18-25 |
| PostHog browser SDK | Leave `NEXT_PUBLIC_POSTHOG_KEY` unset: "PostHog API key not found. Skipping initialization" | `packages/common/posthog-client.ts` |
| PostHog flags via your API | Cannot be disabled; Studio always calls `GET /platform/telemetry/feature-flags` when `enabled` and `API_URL` are set. Return `{}`/404; failure is tolerated | `packages/common/feature-flags.tsx` lines 143-170 |
| Event telemetry | `POST /platform/telemetry/event|identify|feature-flags/track` run only if `hasConsented()`. In platform mode with `NEXT_PUBLIC_ENVIRONMENT` not `local`/`staging`, consent comes from Usercentrics (`NEXT_PUBLIC_USERCENTRICS_RULESET_ID`); unset means the init fails and consent stays false. Do **not** set `NEXT_PUBLIC_ENVIRONMENT=local|staging`: that auto-grants consent | `packages/common/consent-state.ts` lines 280-325 |
| Sentry | no-op without `NEXT_PUBLIC_SENTRY_DSN` | `apps/studio/lib/sentry-client-options.ts` |
| Billing UI | return `disabled_features: ["billing:all", ...]` from `/platform/profile`; `useIsFeatureEnabled` merges it. Runtime `ENABLED_FEATURES_*` env does not work in platform mode | `hooks/misc/useIsFeatureEnabled.ts`, `enabled-features/README.md` |
| Billing / Stripe / plans API calls | not suppressed; implement as stubs (free/enterprise plan, empty invoices). supastack counts 37 billing stubs and marks them "intentionally cloud-only" | `scripts/studio-mock-api/API-STUBS.md` |
| Feature gating by plan | grant all entitlements (see 4.3) | `useCheckEntitlements.ts` |

### 4.6 Prior art

| Project | What it proves | What it needed | Status |
|---|---|---|---|
| [kmhari/supastack](https://github.com/kmhari/supastack) (AGPL-3.0) | Whole design from this research is built: control plane, wildcard TLS, per-project stacks, Studio `IS_PLATFORM=true` at `https://<apex>/dashboard`, `/v1` API, OAuth-fronted MCP, CLI via profile TOML, "unmodified upstream CLI >= 2.72" | Studio fork branch `kmhari/supabase@supastack-studio` = upstream plus 3 source files: `enabled-features.json` (6 `dashboard_auth:*` flags off), `SignInForm.tsx`, `ForgotPasswordWizard.tsx` (render hCaptcha only with a site key). Branch is 806 commits behind `master` as of today. Its mock server lists 217 of 392 `/platform/*` rows as stubs | 0 stars, 1 author, last push 2026-08-27. Self-reported compatibility claims; I did not run it |
| [kmhari/supastack `scripts/studio-mock-api`](https://github.com/kmhari/supastack/tree/main/scripts/studio-mock-api) | Minimal Express mock: `/platform/profile`, `/platform/organizations`, `/platform/projects[/ref]` mocked, pg-meta and auth config proxied, unknown GET `{}` and non-GET 204. Needed `alwaysLoggedIn={true}` patch in that variant | |
| [HarryET/supa-manager](https://github.com/HarryET/supa-manager) | Earlier attempt (Rust API + Studio patches) | 17 patches against a 2024 Studio: hCaptcha, ConfigCat base URL redirect, remove billing pages, remove telemetry requests, OAuth/SSO removal, `/props` prefix fix | Last push 2024-11-20; obsolete patches (ConfigCat, telemetry, billing) are now covered by the levers in 4.5 |
| powabase-ai/powabase | Uses a Studio fork with `IS_PLATFORM=false` (single project) | n/a | not platform mode |
| Code search `NEXT_PUBLIC_IS_PLATFORM=true` (GitHub, 100 results) | Everything else is `.env.example`/CONFIG.md copies of the docker config or forks of supabase/supabase | n/a | no other platform-mode deployment found |

---

## 5. Question 4 - Agent skills

[supabase/agent-skills](https://github.com/supabase/agent-skills) (two skills: `supabase`, `supabase-postgres-best-practices`) is mostly URL-agnostic in substance (RLS rules, `supabase db` workflow, advisors, migrations guidance), but `skills/supabase/SKILL.md` hard-wires the hosted product in four places: it tells the agent to fetch `https://supabase.com/changelog.md` and `https://supabase.com/docs/...` pages, links Data API settings at `https://supabase.com/dashboard/project/<ref>/integrations/data_api/settings`, and instructs the agent to check `https://mcp.supabase.com/mcp`, create `.mcp.json` pointing there, and expect an OAuth 2.1 flow. It also recommends scoped PATs for the Management API/CLI/MCP and falls back to MCP `execute_sql` / `get_advisors` when the CLI is too old. The postgres-best-practices skill contains no hosted URLs beyond postgresql.org references. For a self-hosted control plane you can use the skills unmodified for guidance, but the MCP section will point agents at the wrong server; ship a thin overlay skill (or a fork) that replaces the MCP URL, dashboard URL template and docs source, and document the `--profile`/`SUPABASE_PROFILE` requirement for CLI steps.

---

## 6. Minimum Platform API subset

Priority 0 means required for the flow to work at all; priority 1 means features degrade visibly; priority 2 means stub is fine. Response bodies must match the published schemas: the CLI decodes strictly and Studio's types assume required fields.

### 6.1 Identity and tokens (cross-cutting)

- GoTrue instance for the dashboard (`NEXT_PUBLIC_GOTRUE_URL`); API validates its JWT on `/platform/*` and `/v1/*`.
- PATs `sbp_` + 40 lowercase hex (also accept `sbp_v0_` and `sbp_oauth_` prefixes); Bearer on `/v1/*`; storage/revocation UI maps to Studio's `/platform/profile/access-tokens` family (not traced here).
- `POST /platform/cli/login` (Studio, GoTrue JWT) and `GET /platform/cli/login/{session_id}?device_code=` (CLI, unauthenticated) for browser login (P1; `--token` avoids it).
- Project refs: 20 lowercase letters. Per project DNS: `https://<ref>.<project_host>` (gateway) and `db.<ref>.<project_host>:5432` (direct) plus pooler.

### 6.2 `/v1` (CLI + MCP + parts of Studio), P0 unless noted

| Method and path | Used by |
|---|---|
| `GET /v1/profile` | CLI login stitch (P1) |
| `GET /v1/organizations`, `GET /v1/organizations/{slug}` | MCP, CLI `orgs` |
| `GET /v1/projects`, `GET /v1/projects/{ref}` | CLI, MCP, Studio |
| `POST /v1/projects`, `POST /v1/projects/{ref}/pause`, `POST /v1/projects/{ref}/restore` | MCP, CLI `projects create` (P1) |
| `GET /v1/projects/{ref}/api-keys` (`reveal`), `GET /v1/projects/{ref}/api-keys/legacy` | CLI `link`, MCP, Studio |
| `POST /v1/projects/{ref}/cli/login-role`, `DELETE /v1/projects/{ref}/cli/login-role` | CLI db commands (P0 unless `SUPABASE_DB_PASSWORD` is used) |
| `GET /v1/projects/{ref}/config/database/pooler`, `GET /v1/projects/{ref}/config/storage` | CLI `link` and DB fallback; MCP storage (P1) |
| `GET/DELETE /v1/projects/{ref}/network-bans`, `POST .../network-bans/retrieve` | CLI pooler recovery (P2) |
| `POST /v1/projects/{ref}/database/query` | MCP `execute_sql`/`list_tables`, CLI `db query --linked` |
| `GET/POST /v1/projects/{ref}/database/migrations` | MCP; Studio |
| `GET /v1/projects/{ref}/types/typescript` | CLI `gen types`, MCP |
| `GET /v1/projects/{ref}/functions`, `POST .../functions`, `PUT .../functions`, `POST .../functions/deploy`, `GET/PATCH/DELETE .../functions/{slug}`, `GET .../functions/{slug}/body` | CLI, MCP, Studio |
| `GET/POST/DELETE /v1/projects/{ref}/secrets` | CLI, MCP, Studio |
| `GET /v1/projects/{ref}/advisors/security`, `.../advisors/performance` | CLI `db advisors`, MCP (P1) |
| `GET /v1/projects/{ref}/analytics/endpoints/logs` | MCP (P1; dialect decision, see 3.3) |
| `GET /v1/projects/{ref}/health` | Studio service status |
| `GET/POST /v1/projects/{ref}/branches`, `GET /v1/projects/{ref}/branches/{name}`, `GET/PATCH/DELETE /v1/branches/{id}`, `POST /v1/branches/{id}/merge|reset|push` | CLI `branches`, MCP (P2 unless you implement branching; Studio calls `GET /v1/projects/{ref}/branches` on every project load so return `[]`) |
| `GET /v1/projects/{ref}/storage/buckets` | MCP (P2) |
| `GET /v1/organizations/{slug}/entitlements` | CLI branch gate (P2); Studio uses the `/platform/organizations/{slug}/entitlements` twin |
| `GET/PUT /v1/projects/{ref}/config/database/postgres` | CLI `postgres-config`, Studio (P2) |
| Studio-only v1 calls (P1-P2): `.../config/auth/signing-keys*`, `.../config/auth/third-party-auth*`, `.../ssl-enforcement`, `.../network-restrictions*`, `.../upgrade*`, `.../readonly/temporary-disable`, `.../read-replicas/*`, `.../custom-hostname*`, `.../database/jit*`, `.../actions*` | return empty/403-style stubs |

### 6.3 `/v2` (P2 unless you want these features)

`GET /v2/projects/{ref}/config` (Studio loads it on project open, P1), `POST /v2/projects/{ref}/advisors/run` (MCP on `main`), `/v2/projects/{ref}/notebooks*` (MCP), `/v2/projects/{ref}/compute*` (Studio).

### 6.4 `/platform` (Studio)

P0: `GET/POST /platform/profile`, `GET /platform/profile/permissions` (grant all, wildcard), `GET /platform/organizations`, `GET /platform/organizations/{slug}`, `GET /platform/organizations/{slug}/projects`, `GET /platform/projects`, `GET /platform/projects/{ref}`, `GET /platform/projects/{ref}/status`, `GET /platform/projects/{ref}/settings`, `POST /platform/pg-meta/{ref}/query`, `GET /platform/organizations/{slug}/entitlements` (grant all), `GET /platform/organizations/{slug}/billing/subscription` (plan stub).

P1 (project pages): `GET /platform/projects/{ref}/api-keys/temporary`, `GET/PATCH /platform/projects/{ref}/config/postgrest`, `GET/PATCH .../config/storage`, `GET .../config/pgbouncer`, `GET/POST/DELETE /platform/projects/{ref}/content*` (SQL snippets), `/platform/auth/{ref}/{config,users,...}` (proxy GoTrue admin), `/platform/storage/{ref}/{buckets,...}` (proxy Storage), `GET /platform/projects/{ref}/analytics/endpoints/{usage.api-counts,logs.all,...}`, `GET /platform/database/{ref}/backups`, `POST /platform/projects/{ref}/{pause,restart,restore}`, `GET /platform/organizations/{slug}/members*`, `GET /platform/organizations/{slug}/usage`, `GET /platform/notifications`, `GET /platform/projects-resource-warnings`, `GET /platform/telemetry/feature-flags`.

P2 stubs (empty lists/objects, 200/204): billing, stripe, plans, invoices, integrations (GitHub, Vercel, partners), OAuth apps, audit logs, replication, warehouse, marketplace, support, feedback, `POST /platform/telemetry/*`.

Unverified: which P1 calls Studio tolerates failing. Use supastack's catch-all (GET `{}`, others 204) as the baseline and tighten from a real browser session with network capture.

---

## 7. Upstream PRs that would be needed

None is strictly required to ship, since each gap has a workaround, but each PR removes a recurring maintenance cost.

| Repo | Change | Why | Workaround today |
|---|---|---|---|
| supabase/supabase (Studio) | Render `<HCaptcha>` only when `NEXT_PUBLIC_HCAPTCHA_SITE_KEY` is set (`SignInForm.tsx`, `ForgotPasswordWizard.tsx`) | Platform mode cannot sign in without a key | Patch (supastack patch 2) or bake hCaptcha's public test key (loads `js.hcaptcha.com`) |
| supabase/supabase (Studio) | Make `dashboard_auth:*` flags overridable by env in platform mode (extend `ENABLED_FEATURES_*` or a `NEXT_PUBLIC_` variant) | Self-signup and GitHub/SSO buttons must be off before login, when `profile.disabled_features` is not available | Patch `enabled-features.json` (supastack patch 1) |
| supabase/supabase (Studio) | Allow extra project hosts in the platform CSP via a documented env (today only `NIMBUS_PROD_PROJECTS_URL`) | Custom `*.example.dev` project domains are blocked by `connect-src` | Set `NIMBUS_PROD_PROJECTS_URL(_WS)` at build (works as an undocumented hook, verify) or patch `csp.ts` |
| supabase/supabase | Publish a platform-mode image or runtime-config for `NEXT_PUBLIC_*` | `IS_PLATFORM` and API URLs are baked at build | Self-build; supastack's placeholder-`sed` entrypoint |
| supabase/cli | Implement `SUPABASE_API_URL` / `SUPABASE_PROJECT_HOST` / `SUPABASE_POOLER_HOST` as designed in `apps/cli/docs/profile.md`, or keep profiles stable | Profiles may be dropped in favor of "platforms" (design doc says no backward compatibility) | `--profile` file today; pin CLI version and test on each bump |
| supabase/mcp | `getProjectUrl` should accept a project-host template or derive from `/v1/projects/{ref}` (currently `api.supabase.com` / `.green` / else `supabase.red`) | `get_project_url` returns a dead host | Overwrite `platform.development.getProjectUrl` after `createSupabaseApiPlatform` |
| supabase/mcp | `logsDialect` (and optionally a log-query adapter) as an option on `createSupabaseApiPlatform` | Logflare/BigQuery backends | Set `platform.debugging.logsDialect = 'bigquery'` post-construction, or drop `debugging` |
| supabase/mcp | `--http` should accept a custom `--api-url` with a default secret template derived from `--api-url`, and `SUPABASE_API_URL` env parity with the CLI | Today it throws without `--secret-url-template`; no env var | Pass `--secret-url-template` |
| supabase/agent-skills | Parameterize hosted URLs (MCP URL, dashboard URL, docs source) | Skills tell agents to use `mcp.supabase.com` | Overlay skill or fork |

---

## 8. Open items and risk notes

- **Spec drift is continuous.** The CLI repo moved from Go to TS during this research window; MCP gained v2 endpoints in 0.13.0; Studio is mid-migration to TanStack Start. Treat the three public specs as the contract, and run a nightly diff of `api-v1/v2/platform-json` against your implementation.
- **Strict decoding is the main CLI risk.** Any missing required field in a `/v1` response produces a `SchemaError` (observed). Generate server types from the spec rather than hand-writing responses.
- **DNS and TLS shape the design.** CLI and MCP assume `<ref>.<project_host>` and `db.<ref>.<project_host>`; Studio's CSP assumes known project hosts. Wildcard DNS plus a wildcard certificate (supastack's approach) is the path of least resistance.
- **Not verified:** running Studio platform mode myself; supastack's end-to-end claims; fail-soft behavior of individual Studio queries; CLI flows other than `projects list` and `link`; MCP against a mock.

## 9. Sources

Repos (read on 2026-10-06):
- https://github.com/supabase/cli (`apps/cli/src/command-internal/profile-load.ts`, `apps/cli/src/config/command-settings.layer.ts`, `apps/cli/src/shared/config/cli-settings.layer.ts`, `apps/cli/src/auth/access-token.ts`, `apps/cli/src/commands/*/SIDE_EFFECTS.md`, `apps/cli/docs/profile.md`, `apps/cli/docs/go-cli-porting-status.md`, `apps/cli-go/internal/utils/{api,profile,access_token}.go`, `packages/api/README.md`, `packages/api/src/generated/openapi.json`)
- https://github.com/supabase/mcp (`packages/mcp-server-supabase/src/{cli.ts,logs.ts,platform/api-platform.ts,transports/local-http-entry.ts,tools/debugging-tools.ts}`, `README.md`, `CONTRIBUTING.md`, `server.json`, `CHANGELOG.md`)
- https://github.com/supabase/supabase (`apps/studio/{lib/constants/index.ts,data/fetchers.ts,data/sql/execute-sql-mutation.ts,lib/auth.tsx,lib/profile.tsx,lib/hosted-api-allowlist.ts,csp.ts,vite.config.ts,Dockerfile,routes/api/mcp/index.ts,lib/api/self-hosted/mcp.ts,components/interfaces/SignIn/SignInForm.tsx,hooks/misc/useCheckEntitlements.ts,hooks/misc/useIsFeatureEnabled.ts}`, `packages/common/{gotrue.ts,configcat.ts,feature-flags.tsx,consent-state.ts,posthog-client.ts,enabled-features/}`, `packages/api-types/`, `.github/workflows/publish_image.yml`)
- https://github.com/supabase/agent-skills (`skills/supabase/SKILL.md`)
- https://github.com/kmhari/supastack (README, `docs/supabase-cli.md`, `infra/studio-platform/Dockerfile`, `scripts/studio-mock-api/{README.md,API-STUBS.md}`, `apps/mcp/src/platform-build.ts`) and https://github.com/kmhari/supabase/blob/supastack-studio/SUPASTACK-PATCHES.md
- https://github.com/HarryET/supa-manager (`studio/patches/`)

Docs and live endpoints:
- https://api.supabase.com/api/v1-json , https://api.supabase.com/api/v2-json , https://api.supabase.com/api/platform-json , https://api.supabase.com/api/v1-yaml , https://api.supabase.com/api/v1
- https://supabase.com/docs/guides/getting-started/mcp
- https://supabase.com/docs/guides/self-hosting/enable-mcp
- GitHub releases: https://github.com/supabase/cli/releases/tag/v2.120.0 , https://github.com/supabase/mcp/releases

Scratch material (not committed): clones and the mock-server test under `/private/tmp/claude-502/-Users-milo-Repos-supabase-selfhost/b015e317-cbb6-4116-9b01-add838934b9b/scratchpad/` (`cli`, `supabase-mcp`, `supabase-mono`, `agent-skills`, `ext/`, `mock/`, `specs/`, `studio_paths2.tsv`).
