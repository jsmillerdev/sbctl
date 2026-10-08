# tests/conformance

The compatibility goal of `docs/design.md` is that official Supabase clients and tools work against a node unchanged. This directory checks it with the clients themselves: supabase-js and the Supabase CLI run against projects on a node installed with `deploy/install.sh`, and two small programs watch what upstream moves under us.

| Part | What it does | Where it runs |
|---|---|---|
| `run.sh` | Installs a node, claims it, creates two projects through the Management API with a personal access token, then runs the supabase-js suites and the CLI script. | `conformance.yml`, job `suites` (amd64 and arm64): on pushes, on pull requests that touch `internal/versions/versions.yaml` or this directory, on `workflow_dispatch`, nightly. |
| `js/*.test.mjs` | supabase-js suites and the pg_cron and branch checks (Node 22, `node:test`) against project A. | called by `run.sh` |
| `cli.sh` | The Supabase CLI against project B. | called by `run.sh` |
| `specdiff/` | Downloads the three Management API OpenAPI documents and diffs them with the pinned copies in `internal/api/gen/specs`. Exit 1 on drift. | job `specdiff`: nightly, on dispatch, and on every push except to `main` |
| `bumpcheck/` | Checks that every pin of `internal/versions/versions.yaml` (and the client pins below) still exists upstream, with its linux amd64 and arm64 archives, and lists newer releases. | job `bump-check`, same triggers |

Run the Go parts anywhere: `go run ./tests/conformance/specdiff`, `go run ./tests/conformance/bumpcheck` (`GITHUB_TOKEN` raises the GitHub rate limit). Their logic is unit tested offline (`go test ./tests/conformance/...`). The suites need Linux with systemd and root; see the header of `run.sh`. Nothing here calls AWS.

## Pins

| Client | Pin | Bump |
|---|---|---|
| supabase-js | `js/package.json` and `js/package-lock.json` | edit both with `npm install --save-exact @supabase/supabase-js@X --prefix tests/conformance/js` |
| Supabase CLI | `pins.env` (`SUPABASE_CLI_VERSION`), mirrored by `cli.version_tested` in `internal/versions/versions.yaml` | edit both |
| Node | 22, in the workflow | |

A bump is a pull request; the `suites` job gates it. `bumpcheck` lists newer releases of every pin so a bump is noticed. It never fails for a newer release, only for a pin that no longer exists or a slim-services pin it could not check (after a rate limit, for example); the client pins only raise a warning annotation.

## The node under test

`run.sh` runs `deploy/install.sh --binary ... --domain conformance.test --public-ip 127.0.0.1 --tls off --no-studio --set functions.enabled=true`, the same path as `install-e2e`. `/etc/hosts` maps `api.`, `pooler.`, `studio.` and each project's host to 127.0.0.1, so nothing needs DNS; `db.<ref>.api.conformance.test` deliberately does not resolve, so the CLI reaches the database through the pooler, as on any network without a direct route. Studio is not installed (its checks are `tests/e2e`); Realtime, Storage, Supavisor, pg-meta and the Edge Runtime are the real artifacts.

Project A is where the supabase-js suites run; auto-confirm is turned on through `PATCH /v1/projects/{ref}/config/auth` first, as a customer would before testing sign-ups. Project B belongs to the CLI, and a test in `postgrest.test.mjs` checks that A's keys do not open B. Every suite runs once with the opaque keys (`sb_publishable_`, `sb_secret_`) and once with the legacy `anon` and `service_role` JWTs, except Functions.

## What is covered

supabase-js 2.117.3:

- **Auth** (`auth.test.mjs`): sign up with auto-confirm, a wrong password, sign in and the claims of the access token (role, audience, subject, issuer = the project's `/auth/v1`), `getUser`, `getSession`, `getClaims`, `refreshSession` (the refresh token rotates), `updateUser`, `signOut` (the ended session's tokens are refused); the admin API with the secret key, refused for the low-privilege key.
- **PostgREST** (`postgrest.test.mjs`): a table, policies and functions created through `POST /v1/projects/{ref}/database/query`; CRUD with returning and upsert, the filter and modifier set, exact counts, `single`/`maybeSingle`, RPC (a missing function gives `PGRST202`); RLS for anon, an authenticated user and the secret key; no key and a wrong key are 401; GraphQL at `/graphql/v1` after `create extension pg_graphql` (off in a new project, as on hosted); project A's keys are refused by project B.
- **Realtime** (`realtime.test.mjs`): `postgres_changes` INSERT, broadcast between two clients with acknowledgement, presence `join`, `leave` and state.
- **Storage** (`storage.test.mjs`): buckets, objects (upload, download, list, upsert and its refusal, move, copy, remove), signed URLs, signed upload URLs and public URLs (fetched with no key; a private bucket refuses a public URL), and RLS on `storage.objects`.
- **Functions** (`functions.test.mjs`): a function deployed through `POST /v1/projects/{ref}/functions/deploy` (the endpoint `supabase functions deploy --use-api` calls; the node bundles it) and invoked with the anon JWT and a user's token; JWT verification refusing no token and a forged token; a function deployed without verification answering with no token and with the opaque key; the `SUPABASE_*` variables and a REST query from inside the function; a secret set with `POST /v1/projects/{ref}/secrets` reaching the function and listed by name only; a redeploy raising the version; a deleted function answering 404. It is also the regression gate for `serve`'s Functions wiring (the syncer loop, the Management API hook and the proxy route), which `internal/app/serve_integration_test.go` checks only as far as the setting reaching the proxy.
- **pg_cron** (`cron.test.mjs`): `cron.schedule` with a seconds schedule, a `succeeded` row in `cron.job_run_details` and the job's insert, `cron.unschedule`. A node whose pg_cron cannot connect to its own cluster records every run as failed.
- **Branches** (`branches.test.mjs`): a schema-only branch created with `POST /v1/projects/{ref}/branches` (the endpoint behind `supabase branches create` and Studio) reaches `MIGRATIONS_PASSED`, is listed next to the default branch and found by name, carries the parent's migration and none of its data, and `DELETE /v1/branches/{id}` removes it.

Supabase CLI (`cli.sh`), through a profile file made by `supavise api profile`: `init`, `projects list`, `link` (and the pooler URL it records), `projects api-keys`, `migration new|list`, `db push` through the pooler (the migration's table and rows exist, the version is in `supabase_migrations.schema_migrations`; a second push finds nothing to do), `gen types typescript --project-id` and `--linked`, `secrets set|list|unset`, `functions deploy --use-api|list|delete` (with a call that sees the secret), `unlink`.

## How the CLI finds a node

The CLI has no flag or environment variable for the Management API URL (`SUPABASE_API_URL` is ignored by commands). The supported way is a profile file: `--profile <file>` or `SUPABASE_PROFILE=<file>` with `name`, `api_url`, `dashboard_url`, `project_host` and `pooler_host` (`supavise api profile --format yaml` prints it). `pooler_host` must be the registrable domain of the pooler host, not the host: the CLI compares the effective TLD plus one of the pooler URL's host with it before it uses the pooler, so `supavise api profile` prints the registrable domain (`internal/api/profile.go`).

## Where hosted Supabase and this suite differ, on purpose

- The **opaque keys are not JWTs**, so a function with JWT verification on cannot be called with `sb_publishable_`; hosted documents that such functions must be deployed with `--no-verify-jwt` to take opaque keys. The Functions suite calls a verifying function with the legacy anon JWT and a user token, and the function without verification with the opaque key. Whether this node should translate opaque keys for verifying functions is not asserted.
- **Functions are deployed with the node bundling the source** (`--use-api`, and the equivalent deploy endpoint) because the CLI's default bundling pulls an image with Docker. `tests/linux/functions-smoke.sh` covers Docker-based bundling.
- **Sign-ups need no email**: auto-confirm is on for project A. Mail delivery (confirmation, recovery, invitation) is covered by `tests/linux/settings-smoke.sh` and `roles-smoke.sh` with a mail sink.

## Not covered yet

- Studio (Playwright): `tests/e2e/studio-smoke.mjs` is a separate script that needs the platform build.
- OAuth providers, SAML, MFA, phone auth, anonymous sign-ins, the S3-compatible Storage protocol, image transforms, Realtime authorization (private channels), `supabase db dump|pull|diff` (they run `pg_dump` in a container) and `supabase start`.
- `supabase db push` through the direct host: on a node that uses the `<ip>.sslip.io` default name, `db.<ref>.api.<ip>.sslip.io` resolves to the node and answers on 5432 (Supavisor), where the user `postgres` without a tenant is refused, so the CLI never falls back to the pooler. The suite uses a name that does not resolve; a node needs a decision on how it answers a direct database host.
