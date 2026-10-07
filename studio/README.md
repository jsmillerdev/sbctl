# studio

Platform-mode Studio for `sbctl`: a build of upstream Studio with `NEXT_PUBLIC_IS_PLATFORM=true` and three small patches, packaged like the slim-services `studio` artifact so the systemd unit does not change. Also the mock Management API and the browser spike that tested it, and the endpoint list Workstream B builds against (`research/08-studio-platform-calls.md`).

| Path | What |
|---|---|
| `patches/` | The three patches, one `git format-patch` file each, made against `supabase/supabase@94b8b06eb294cf6b217c68d30357566cc8f146d9` (`versions.yaml` `studio.tag`). |
| `build.sh <platform>` | Fetch, patch, install, build, package, verify. Writes `dist/sbctl-studio-<tag>-p<N>-<platform>.tar.zst` and a line in `dist/SHA256SUMS`. |
| `Dockerfile.build` | The same build in a clean Ubuntu 24.04 image (`docker buildx build --target artifact --output type=local,dest=studio/dist`). |
| `ci-prepare.sh` | For a GitHub-hosted runner: frees disk, adds 6 GB swap, installs zstd. |
| `verify.sh` | Starts a packaged build on a loopback port and checks it (also run by `build.sh` before it writes the artifact). |
| `runtime/` | What goes into the artifact: launcher, entrypoint, runtime substitution and its tests. |
| `placeholders.json` | The per-install values baked into the build as placeholders. |
| `PATCHSET` | Revision `N` in the artifact name. Bump it when the patches or `runtime/` change without a new upstream tag. |
| `mock/` | Mock Management API (Go, `package main`). See `mock/README.md`. |
| `spike.sh`, `spike/` | The CI spike: small stack plus mock plus Studio, driven by Playwright. |

## The artifact

Same layout and launcher contract as `studio-2026.10.05-sha-94b8b06-r0`:

```
bin/studio              POSIX sh launcher; sources bin/.runtime-env.sh; cd app/; exec node/bin/node apps/studio/docker-entrypoint.mjs
bin/.runtime-env.sh     NODE_OPTIONS default (--max-old-space-size=384), NEXT_TELEMETRY_DISABLED=1; a set variable wins
node/bin/node           Node 22.23.3 from nodejs.org (checksum pinned in build.sh)
app/                    Next standalone output (apps/studio/server.js, .next, node_modules/.pnpm), plus .next/static and public/
share/licenses/         Studio (Apache-2.0), Node, sbctl
share/sbctl/            build-info.json, runtime-config.json (placeholders and the list of files that carry them), the patches
```

The upstream dotenv file is not shipped; configuration is environment only, as in the slim artifact's `bin/studio`.

### Environment at start

| Variable | Required | Meaning |
|---|---|---|
| `PORT`, `HOSTNAME` | no | Next's listener (default 3000 and `0.0.0.0`; the unit should set `HOSTNAME=127.0.0.1`). |
| `NEXT_PUBLIC_API_URL` | yes | The Management API as the browser reaches it, e.g. `https://api.example.com/platform`. A missing `/platform` suffix is added; trailing slashes are dropped. Do not use a host name that starts with `platform.`. |
| `NEXT_PUBLIC_GOTRUE_URL` | yes | The dashboard GoTrue as the browser reaches it, including its path, e.g. `https://api.example.com/auth/v1`. |
| `NEXT_PUBLIC_SITE_URL` | yes | The dashboard's own URL, e.g. `https://studio.example.com` (redirect URLs). |
| `NEXT_PUBLIC_HCAPTCHA_SITE_KEY` | no | Empty by default: no captcha widget (patch 0001). Maps from `[studio] hcaptcha_site_key`. |
| `NEXT_PUBLIC_DISABLED_FEATURES` | no | Comma-separated feature keys hidden before login. Default hides sign-up, GitHub, ChatGPT and SSO sign-in, the testimonial and the terms text (patch 0002). |
| `CSP_EXTRA_PROJECT_HOSTS` | no | Host sources serving project APIs, e.g. `*.api.example.com` (patch 0003); each is allowed over `https` and `wss`. |
| `SBCTL_STUDIO_SKIP_RUNTIME_CONFIG=1` | no | Start without substitution (debugging). |

A missing required value or an unsafe one exits with status 78 and a message naming it. Values may only contain `A-Za-z0-9._~:/@%+,*=-`, so they cannot break out of a JS string, a JSON string or an HTML attribute.

### How the substitution works

Next inlines `NEXT_PUBLIC_*` and evaluates the CSP at build time, so `build.sh` builds with unique placeholder strings (`https://sbctl-placeholder-api.invalid/platform`, ...). At packaging time `runtime/sbctl-runtime-config.mjs prepare` finds every file that contains one (about 280 files, 7 MB: client and server chunks, prerendered HTML, `routes-manifest.json` with the CSP), keeps a pristine copy as `<file>.sbctl-tpl` and fails the build if a placeholder did not survive the bundler. At every start the entrypoint rewrites those files from the `.sbctl-tpl` copies, in one regex pass, with a temp file and a rename per file. It never reads its own output, so starting again with other values works. The API and GoTrue placeholders carry a marker path so the bare origin used in the CSP is replaced by the bare origin of the real URL (a CSP source with a path only matches that exact path).

**Requirement for unit D:** the artifact directory (`app/`) must be writable by the service user at start, for example `ReadWritePaths=` on the artifact directory under `ProtectSystem=strict`. Next also writes `app/apps/studio/.next/cache` at runtime (image cache).

**Also for unit D, exit codes:** the launcher exits 78 (`EX_CONFIG`) on a missing or unsafe configuration value and mirrors the child's exit status, so a SIGTERM stop ends as 143. The unit needs `RestartPreventExitStatus=78` (a bad value would otherwise restart-loop) and `SuccessExitStatus=143` (a normal stop is not a failure).

`/_next/static` chunks keep their file names when a value changes, and Next serves them as immutable. After changing `NEXT_PUBLIC_API_URL` or `NEXT_PUBLIC_GOTRUE_URL` on a running install, users need a hard reload once.

### Studio's own routes the proxy answers

Studio asks its own `/api/incident-banner` route for incident.io banners on every page. Without an incident.io key it answers 500, react-query retries it after 1, 4 and 16 s, and the sign-in form awaits that query, so the redirect after sign-in took 22 s in the spike (research/08 section 9). The artifact is not changed for this: sbctl's proxy answers `GET studio.<domain>/api/incident-banner` itself with `{"incidents":[]}` (`internal/proxy`), so Studio keeps exactly the three patches. Running the artifact without the proxy (`verify.sh`) shows the 500; the spike's browser script answers the route itself for the same reason.

The proxy also rewrites the Content-Security-Policy that Studio sends so that the browser cannot reach `usercentrics.eu`, the consent-banner vendor Studio calls on every page load (`internal/proxy`, `studio.go`).

## Patches, as reviewed

Applied with `git am --3way` onto a fresh sparse checkout of the pinned commit: all three apply cleanly. The new tests (`csp.test.ts`, `enabled-features/index.test.ts`, 7 cases) pass with vitest 5 in a scratch directory (Studio's full vitest setup was not installed).

- **0001 hCaptcha only with a site key.** Covers the sign-in path (`SignInForm`, `ForgotPasswordWizard`) and the other pages that mount the widget with the build-time key (`SignInSSOForm`, `SignUpForm`, `ChangeEmailAddress`, `/new`), through a module-level `HCAPTCHA_SITE_KEY` that each page checks before rendering the widget. Seen working: with an empty key the sign-in page renders no widget and sign-in succeeds (spike, with the first two forms patched; the other four use the same guard and were checked by applying the patch, not by running them). Not touched: the four billing dialogs (`CreditCodeRedemption`, `CreditTopUp`, `PaymentMethodSelection`, `AddNewPaymentMethodModal`), which need Stripe and are not reachable without billing. The patch-set number (`PATCHSET`) went from 1 to 2 with this change.
- **0002 feature flags through `NEXT_PUBLIC_DISABLED_FEATURES`.** Upstream resolves `dashboard_auth:*` through `useIsFeatureEnabled`, which layers `profile.disabled_features` (unavailable before login) and the `ENABLED_FEATURES_*` override (a no-op when `IS_PLATFORM` is true) on the static JSON. The patch merges a build-time list into the static set, so it covers the six `dashboard_auth:*` flags before login. The bundle contains the placeholder (checked in the built chunks) and the launcher substitutes it. Seen working in the spike: the sign-in page shows e-mail sign-in only, no sign-up link, no GitHub or SSO button, no testimonial, no terms text.
- **0003 extra project hosts in the platform CSP.** `CSP_EXTRA_PROJECT_HOSTS`, hosts validated against a strict pattern. It is read when `next.config.ts` computes the headers, which is build time, hence the placeholder. `verify.sh` checks that the substituted CSP contains the hosts over `https` and `wss`, the bare API origin, and no placeholder.

## Build on CI

```bash
sudo studio/ci-prepare.sh          # GitHub-hosted runner only
studio/build.sh linux-amd64        # or linux-arm64, on a machine of that architecture
```

`build.sh` downloads its own checksum-pinned Node and pnpm, so it needs nothing preinstalled beyond `git`, `curl`, `tar` and `zstd`. `spike.sh` does not: it needs `go` (to build the mock; or set `SPIKE_MOCK_BIN` to a prebuilt binary), `node` with `npm` (for `playwright-core`), and, for the browser, `sudo` without a password or root for `playwright install-deps` (the browser binaries themselves are installed as the invoking user). There is no CI workflow in this directory; whoever owns `.github/` needs to wire `actions/setup-go` and `actions/setup-node` before the `spike.sh` step, plus the cache and artifact upload of `studio/.build-cache/spike/out`.

`build.sh` runs `next build` with one static-generation worker (`STUDIO_BUILD_WORKERS`, set through `CIRCLE_NODE_TOTAL`, which Next uses to size its worker pool) and a 4 GB V8 heap per node process (`STUDIO_BUILD_HEAP_MB`), on any host size. Expect about 9 GB of disk and a peak near 8 GB of RAM for `next build` (Turbopack; measured on the maintainer's Mac, not on a runner); `build.sh` prints the peak from `/usr/bin/time -v` and warns if RAM plus swap is under 11 GB. The artifact is about 75 MB compressed (the repackaged Mac output measured 73 MB). `STUDIO_UNTIL=prune` runs only the fetch, patch, prune and lockfile check (a few minutes, 300 MB).

## Test it

```bash
node --test studio/runtime/test/                     # substitution (placeholders, runtime config)
go test ./studio/mock/                               # the mock (needs no Studio)
STUDIO_PREBUILT=<apps/studio with .next/standalone> studio/build.sh <platform>   # packaging, launcher and verify.sh without the Next build
studio/spike.sh                                      # whole spike; see the header of the script
```

## Not done, not verified

- The Next build has not run on Linux or on a CI runner. Verified up to the lockfile check (fetch, patches, prune) on the real upstream commit. The packaging, launcher, substitution and `verify.sh` ran on darwin-arm64 against a platform-mode Next output of the same commit built earlier on this machine; the Linux node binary, `tar --sort`, `cp --reflink`, `/usr/bin/time` and `ci-prepare.sh` paths were not executed.
- `spike.sh` ran to a green result on darwin-arm64 only (19 of 19 steps), with `SPIKE_CHROME` pointing at Chrome. The Linux parts (artifact download and checksum, `playwright install-deps` and `install`, apt) are untested.
- Studio makes a request to `api.usercentrics.eu` on every page load even without a ruleset id (it fails soft). Removing it needs a fourth patch; proposed upstream change: skip Usercentrics initialization when `NEXT_PUBLIC_USERCENTRICS_RULESET_ID` is unset.
- Pages beyond the spike's list (Realtime, Edge Functions, Logs, Advisors, Integrations, Reports) are not exercised.
