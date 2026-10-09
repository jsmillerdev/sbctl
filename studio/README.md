# studio

Platform-mode Studio for `supavise`: a build of upstream Studio with `NEXT_PUBLIC_IS_PLATFORM=true` and four small patches, packaged like the slim-services `studio` artifact so the systemd unit does not change. Also the mock Management API and the browser spike that tests it. The calls Studio makes are listed in [docs/reference/studio-platform-calls.md](../docs/reference/studio-platform-calls.md).

| Path | What |
|---|---|
| `patches/` | The four patches, one `git format-patch` file each, made against `supabase/supabase@94b8b06eb294cf6b217c68d30357566cc8f146d9` (`internal/versions/versions.yaml` `studio.tag`). |
| `build.sh <platform>` | Fetch, patch, install, build, package, verify. Writes `dist/supavise-studio-<tag>-p<N>-<platform>.tar.zst` and a line in `dist/SHA256SUMS`. |
| `Dockerfile.build` | The same build in an Ubuntu 24.04 image (`docker buildx build --target artifact --output type=local,dest=studio/dist`). |
| `ci-prepare.sh` | For a GitHub-hosted runner (needs `SUPAVISE_CI=1` or `GITHUB_ACTIONS`, which `sudo` drops: `sudo env SUPAVISE_CI=1 studio/ci-prepare.sh`): frees disk, adds 6 GB swap, installs the build tools. |
| `verify.sh` | Starts a packaged build on a loopback port and checks it, including the MCP route of patch 0004 (`build.sh` runs it before it writes the artifact). |
| `runtime/` | What goes into the artifact: launcher, entrypoint, runtime substitution and its tests. |
| `placeholders.json` | The per-install values baked into the build as placeholders. |
| `PATCHSET` | Revision `N` in the artifact name; bump it when the patches or `runtime/` change without a new upstream tag. |
| `mock/` | Stand-in Management API for `spike.sh` (Go, `package main`), with the endpoints behind the OAuth consent page. See `mock/README.md`. |
| `spike.sh`, `spike/` | The CI spike: small stack plus mock plus Studio, driven by Playwright. Besides the project pages it opens Connect > MCP, approves and declines a request on the consent page, and calls `/api/mcp`. |

## The artifact

Same layout and launcher contract as `studio-2026.10.05-sha-94b8b06-r0`:

```
bin/studio              POSIX sh launcher; cd app/; exec node/bin/node apps/studio/docker-entrypoint.mjs
bin/.runtime-env.sh     NODE_OPTIONS default (--max-old-space-size=384), NEXT_TELEMETRY_DISABLED=1; a set variable wins
node/bin/node           Node 22.23.3 from nodejs.org (checksum pinned in build.sh)
app/                    Next standalone output, plus .next/static and public/
share/licenses/         Studio (Apache-2.0), Node, supavise
share/supavise/         build-info.json, runtime-config.json (placeholders and the files that carry them), the patches
```

The upstream dotenv file is not shipped; configuration is environment only.

### Environment at start

| Variable | Required | Meaning |
|---|---|---|
| `PORT`, `HOSTNAME` | no | Next's listener (default 3000 and `0.0.0.0`; the unit should set `HOSTNAME=127.0.0.1`). |
| `NEXT_PUBLIC_API_URL` | yes | The Management API as the browser reaches it, e.g. `https://api.example.com/platform`. A missing `/platform` suffix is added; trailing slashes are dropped. Do not use a host name that starts with `platform.`. |
| `NEXT_PUBLIC_GOTRUE_URL` | yes | The dashboard GoTrue as the browser reaches it, including its path, e.g. `https://api.example.com/auth/v1`. |
| `NEXT_PUBLIC_SITE_URL` | yes | The dashboard's own URL, e.g. `https://studio.example.com` (redirect URLs). |
| `NEXT_PUBLIC_HCAPTCHA_SITE_KEY` | no | Empty by default: no captcha widget (patch 0001). Maps from `[studio] hcaptcha_site_key`. |
| `NEXT_PUBLIC_MCP_URL` | no | The remote MCP endpoint that Connect > MCP shows, e.g. `https://api.example.com/mcp`. Default `http://localhost:8080/mcp`, what Studio shows without it, so a unit that does not set it still starts. |
| `NEXT_PUBLIC_DISABLED_FEATURES` | no | Comma-separated feature keys hidden before login. Default hides sign-up, GitHub, ChatGPT and SSO sign-in, the testimonial and the terms text (patch 0002). |
| `CSP_EXTRA_PROJECT_HOSTS` | no | Host sources serving project APIs, e.g. `*.api.example.com` (patch 0003); each is allowed over `https` and `wss`. |
| `SUPAVISE_MANAGEMENT_API_URL` | for MCP | The Management API as this process reaches it, e.g. `http://127.0.0.1:7000`. Read for every request to `/api/mcp` (patch 0004), not substituted into the build. Without it, with `SUPAVISE_PROJECT_URL_TEMPLATE` missing, or if either is malformed, the route answers 500 "MCP is not configured". |
| `SUPAVISE_PROJECT_URL_TEMPLATE` | for MCP | The public API URL of a project with `{ref}` where its ref goes, e.g. `https://{ref}.api.example.com`. The MCP tool `get_project_url` answers from it. |
| `SUPAVISE_STUDIO_SKIP_RUNTIME_CONFIG=1` | no | Start without substitution (debugging). |

A missing required value or an unsafe one exits with status 78 and a message naming it. The two `SUPAVISE_*` variables are not placeholders and are not checked at start. Values may only contain `A-Za-z0-9._~:/@%+,*=-`, so they cannot break out of a JS string, JSON string or HTML attribute.

### How the substitution works

Next inlines `NEXT_PUBLIC_*` and evaluates the CSP at build time, so `build.sh` builds with unique placeholder strings (`https://supavise-placeholder-api.invalid/platform`, ...). `runtime/supavise-runtime-config.mjs prepare` keeps a pristine `<file>.supavise-tpl` copy of every file that contains one (about 280 files) and fails the build if a placeholder did not survive the bundler. At every start the entrypoint rewrites those files from the copies, so starting again with other values works. The API and GoTrue placeholders carry a marker path so the bare origin used in the CSP is replaced by the bare origin of the real URL (a CSP source with a path only matches that exact path).

**Unit requirements.** The artifact directory (`app/`) must be writable by the service user at start (for example `ReadWritePaths=` under `ProtectSystem=strict`), because Next also writes `app/apps/studio/.next/cache` (image cache). The launcher exits 78 (`EX_CONFIG`) on a missing or unsafe value and mirrors the child's exit status, so a SIGTERM stop ends as 143: the unit needs `RestartPreventExitStatus=78` (a bad value would otherwise restart-loop) and `SuccessExitStatus=143`.

`/_next/static` chunks keep their file names when a value changes and Next serves them as immutable, so after changing `NEXT_PUBLIC_API_URL` or `NEXT_PUBLIC_GOTRUE_URL` users need a hard reload once.

### Studio's own routes the proxy answers

Studio's `/api/incident-banner` route answers 500 without an incident.io key, which delays sign-in by about 22 s (docs/reference/studio-platform-calls.md, "Findings from running Studio"). The artifact is not changed for this: Supavise's proxy answers `GET studio.<domain>/api/incident-banner` with `{"incidents":[]}` (`internal/proxy`). Running the artifact without the proxy (`verify.sh`) shows the 500; the spike's browser script answers the route itself.

`/api/mcp` is the remote MCP endpoint in platform mode (patch 0004; Supavise's proxy forwards `api.<domain>/mcp` to it after it has checked the caller's token). Upstream answers 404 for it in platform mode, where Supabase runs MCP as its own service.

The proxy also rewrites Studio's Content-Security-Policy so that the browser cannot reach `usercentrics.eu`, the consent-banner vendor Studio calls on every page load (`internal/proxy/studio.go`).

## Patches

Applied with `git am --3way` onto the pinned commit. Each carries its own tests (`csp.test.ts`, `enabled-features/index.test.ts`, `lib/api/platform-mcp.test.ts`).

- **0001 hCaptcha only with a site key.** Covers every page that mounts the widget with the build-time key (`SignInForm`, `ForgotPasswordWizard`, `SignInSSOForm`, `SignUpForm`, `ChangeEmailAddress`, `/new`) through a module-level `HCAPTCHA_SITE_KEY` that each checks. Not touched: the four billing dialogs, which need Stripe and are not reachable without billing.
- **0002 feature flags through `NEXT_PUBLIC_DISABLED_FEATURES`.** Upstream resolves `dashboard_auth:*` from `profile.disabled_features` (unavailable before login) and the `ENABLED_FEATURES_*` override (a no-op when `IS_PLATFORM` is true). The patch merges a build-time list into the static set, so it covers the six `dashboard_auth:*` flags before login. The SSO button is the one flag a node turns on at run time: while the dashboard has a SAML identity provider (`supavise sso add`), `internal/fleet` starts Studio without `dashboard_auth:sign_in_with_sso` in the list, and restarts it when the first provider is added or the last removed.
- **0003 extra project hosts in the platform CSP.** `CSP_EXTRA_PROJECT_HOSTS`, hosts validated against a strict pattern. It is read when `next.config.ts` computes the headers, which is build time, hence the placeholder. `verify.sh` checks the substituted CSP.
- **0004 the MCP route in platform mode.** Studio already bundles the MCP server package with every tool, and Supavise has no process that could run it as hosted does, so `pages/api/mcp` serves it (`lib/api/platform-mcp.ts`). It exists because this deployment has no separate MCP service, so it is not an upstreamable change. Tests: `lib/api/platform-mcp.test.ts` (vitest) and `verify.sh`.
  - Each request needs `POST` and `Authorization: Bearer <token>` (`405` and `401` otherwise). The route builds a server for that request from the package's Management API platform (`createSupabaseApiPlatform`) with the token and `SUPAVISE_MANAGEMENT_API_URL`. The API, not Studio, decides what the token may do. The token is never logged or stored.
  - The query is checked strictly, and anything else is a `400`. `project_ref` is 20 lowercase letters and scopes the server to that project, which hides the account tools. `read_only` is `true` or `false` (default `false`); `true` hides the write tools and sends `read_only` with every query. `features` names groups the package has (`storage` is opt-in). `skip_elicitations` is accepted and has no effect, because the stateless JSON transport cannot ask the client anything.
  - `get_project_url` is filled from `SUPAVISE_PROJECT_URL_TEMPLATE`. The package derives a hosted domain from the API host (`supabase.red` for any host it does not know), which is wrong here. The project ref comes from a tool argument, so it is checked before it goes into the template.
  - The patch adds `/mcp` to the allowlist of API routes (`lib/hosted-api-allowlist.ts`) and sets the body limit of the route to 8 MB, because deploying an Edge Function sends the files. Only the pages router changes; the TanStack twin of the route (`routes/api/mcp`) is not part of this build.
  - `search_docs` calls `supabase.com/docs/api/graphql` from the node, as hosted does.

## Build on CI

```bash
sudo studio/ci-prepare.sh          # GitHub-hosted runner only
studio/build.sh linux-amd64        # or linux-arm64, on a machine of that architecture
```

`build.sh` downloads its own checksum-pinned Node and pnpm, so it needs only `git`, `curl`, `tar`, `zstd`, `python3` and a C toolchain (`build-essential`: the pnpm install compiles native modules such as `libpg-query`). `spike.sh` needs `go` (or `SPIKE_MOCK_BIN`), `node` with `npm`, and `sudo` without a password or root for `playwright install-deps`.

`build.sh` runs `next build` with one static-generation worker (`STUDIO_BUILD_WORKERS`) and a 4 GB V8 heap per node process (`STUDIO_BUILD_HEAP_MB`). Expect about 9 GB of disk and a peak near 8 GB of RAM for `next build` (Turbopack, measured on a Mac); `build.sh` warns if RAM plus swap is under 11 GB. The artifact is about 75 MB compressed. `STUDIO_UNTIL=prune` runs only the fetch, patch, prune and lockfile check.

## Tests

```bash
node --test studio/runtime/test/                     # substitution (placeholders, runtime config)
go test ./studio/mock/                               # the mock (needs no Studio)
```

The tests that patches 0001 to 0004 carry run with Studio's own vitest, inside the build's work tree (`STUDIO_KEEP_WORK=1`, then `pnpm --filter studio exec vitest --run lib/api/platform-mcp.test.ts` in its `prune` directory). `build.sh` does not run them.

The `studio` workflow's `build` job runs `ci-prepare.sh`, `build.sh` and then `spike.sh` on amd64 and arm64 runners. To test packaging without the Next build, set `STUDIO_PREBUILT=<apps/studio with .next/standalone>` and run `studio/build.sh <platform>`.

## Limits

- Studio calls `api.usercentrics.eu` on every page load even without a ruleset id (it fails soft). The proxy's CSP rewrite blocks it; removing the cause needs a fourth patch that skips Usercentrics initialization when `NEXT_PUBLIC_USERCENTRICS_RULESET_ID` is unset.
- The spike does not exercise Realtime, Edge Functions, Logs, Advisors, Integrations or Reports (see the reference for the pages it visits).
