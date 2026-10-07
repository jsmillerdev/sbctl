# functions-main

The main service of sbctl's Edge Runtime: one Deno script that every request to `/functions/v1` enters, for every project on the node. The `sb-edge-runtime` unit starts `edge-runtime start --main-service <dir>` with it (`internal/fleet/edgeruntime.go`). The TypeScript here is embedded in the `sbctl` binary (`embed.go`) and written to `<state_dir>/system/edge-runtime/main` before the unit starts, so binary and main service always match. Apache-2.0; written from the Edge Runtime's public API and the self-hosted main service of `supabase/supabase` (`docker/volumes/functions/main/index.ts`) as references.

## What a request does

```
client -> sbctl proxy -> 127.0.0.1:<edge_runtime>  (X-Sbctl-Project-Ref: <ref>, set by the proxy)
                          main service (this directory)
                            1. project  = X-Sbctl-Project-Ref (20 letters, else 400)
                            2. function = first path segment (else 404 NOT_FOUND)
                            3. project env + function generation read from <root>/<ref>/
                            4. verify_jwt: HS256 token against this project's secret (else 401)
                            5. EdgeRuntime.userWorkers.create({... this project's env ...}).fetch(request)
```

The proxy strips whatever `X-Sbctl-Project-Ref` and `sb-api-key` a client sent and sets its own (tested in `internal/proxy`, and end to end by `tests/functions/verify.mjs`). The main service trusts the header and nothing else: a request without a valid one is `400`.

## The files it reads

Written by `internal/functions`, read here (`src/projects.ts`); change both together. The root is `<state_dir>/system/edge-runtime/tenants`, inside the runtime's own state directory: that is the one directory its systemd unit sees besides the artifacts, so the process that runs tenants' code does not see the projects' data directories, sockets or unit files.

```
<SBCTL_FUNCTIONS_ROOT>/<ref>/functions-env.json       {"version":1,"jwt_secret","supabase":{SUPABASE_*},"secrets":{...}}
<SBCTL_FUNCTIONS_ROOT>/<ref>/functions/<slug>          symlink to .gen/<slug>.<version>.<random>/
    .sbctl-function.json                               {slug, version, verify_jwt, kind, entrypoint, import_map, eszip, sha256}
    supabase/functions/<slug>/index.ts ...             kind "source": the uploaded files
    bundle.eszip                                       kind "eszip": the bundle `supabase functions deploy` built
```

A request resolves the symlink once (`realPath`) and uses that generation's real paths from then on, so one request never mixes two deployments. A link that resolves outside the project's `functions` directory is never followed, an entrypoint or bundle name that leaves the generation is refused, and the metadata's `slug` must equal the requested one.

## Per project, never shared

| What | How |
|---|---|
| Environment of a worker | built from that project's file only: its secrets (never named `SUPABASE_*`), then its `SUPABASE_URL`, `SUPABASE_ANON_KEY`, `SUPABASE_SERVICE_ROLE_KEY`, `SUPABASE_DB_URL`, `SUPABASE_PUBLISHABLE_KEYS`, `SUPABASE_SECRET_KEYS`, `SUPABASE_FUNCTION_SLUG`. The main service's own environment (`PATH`, `HOME`, its settings) is not copied, unlike the upstream sample. The JWT secret never reaches a worker. |
| JWT check | the secret of the project in the header; another project's valid token is `401` |
| Worker pool | `poolKey = <ref>:<slug>:<version>:<generation>:<env stamp>`, so two projects never share a worker, a new deployment or new secrets get new workers and the old ones idle out |
| Code | `servicePath` is the project's own generation directory (source) or its bundle |
| Limits | `memoryLimitMb`, `workerTimeoutMs`, CPU soft and hard limits, `requestAbsentTimeoutMs` from `[functions]` in `config.toml`, the same for every worker |
| Module cache | **one** `DENO_DIR` for the whole runtime (`<state_dir>/system/edge-runtime/deno`), not one per project: the runtime reads `DENO_DIR` once from its process environment, and the main worker cannot change it (`Deno.env.set` throws `NotSupported`, measured with v1.77.4). The cache is keyed by URL and holds public modules; remote imports of all projects share it. |

## Isolation, as measured

Edge Runtime v1.77.4, user workers, with `permissions` set as in `src/handler.ts` (the runtime's defaults for user workers plus a deny rule):

- **Disk.** `Deno.readTextFile` of `/etc/hosts`, of another project's `functions-env.json` and of the function's own files all fail with `NotFound`: a worker sees its module graph, not the disk. `tests/functions/verify.mjs` asserts this on macOS and on Linux (CI). Its `Deno.env.get` also sees only the variables above.
- **The runtime's own port.** A worker's `fetch` to `127.0.0.1`, `localhost`, `[::1]`, `0.0.0.0` or `[::]` on the runtime's port fails with `NotCapable` (`deny_net`), so a function cannot call this service with a project reference of its choosing. The rule matches host names as written; a DNS name that points to loopback is not caught. The remaining exposure is small: a forged reference reaches only what the proxy serves to anyone, behind the function's own JWT check.
- **Other workers' memory and CPU.** Each worker is a V8 isolate with its own heap limit; a crash, a boot failure, a busy loop or an allocation loop in one project's function ended with an error response for that request while the other project answered within milliseconds (`verify.mjs`, "runaway function", "memory limit"). This is V8 isolation, not a VM or a container: a V8 escape would reach the runtime process and, in the systemd unit, everything it can read (see `deploy/systemd/README.md`).
- **CPU limit.** `cpuTimeHardLimitMs` is enforced by the runtime's CPU timer, which exists on Linux only (`CPU timer: not enabled (need Linux)` on macOS). On macOS a busy loop is ended by the request idle timeout (504) or the wall clock; on Linux by the CPU limit.

## Errors

Shapes follow the self-hosted main service, so clients that parse them keep working.

| Situation | Status | `sb-error-code` |
|---|---|---|
| no or malformed project header | 400 | `BAD_REQUEST` |
| unknown project, function or slug | 404 | `NOT_FOUND` |
| no / malformed / foreign / expired token | 401 | `UNAUTHORIZED_NO_AUTH_HEADER`, `UNAUTHORIZED_INVALID_JWT_FORMAT`, `UNAUTHORIZED_LEGACY_JWT`, `UNAUTHORIZED_UNSUPPORTED_TOKEN_ALGORITHM` |
| unreadable deployment files, worker does not boot | 503 | `BOOT_ERROR` |
| over the memory or CPU limit, request cancelled | 546 | `WORKER_RESOURCE_LIMIT` |
| no response within `idle_timeout_seconds` | 504 | `IDLE_TIMEOUT` |
| worker died or threw while answering | 500 | `WORKER_ERROR`, `EDGE_FUNCTION_ERROR`, `INVALID_RESPONSE_STATUS_CODE` |
| the function answered 5xx itself | as sent | `EDGE_FUNCTION_ERROR` added |

`OPTIONS` requests skip the token check (browsers send them without credentials). A retired worker is retried up to three times before the request fails. JWT verification is HS256 only (WebCrypto, no remote module, so the main service starts without network access): projects sign with their HS256 secret, and an `ES256` or `RS256` token is `401`. `Authorization: Bearer sb_publishable_...` and `apikey: sb_secret_...` work because the proxy replaces them with the project's JWT in `sb-api-key`, which this service reads.

## Settings

The unit passes them in the environment (`internal/fleet/edgeruntime.go`); defaults are in `src/config.ts` and `internal/config/functions.go` and equal hosted limits.

| Variable | `[functions]` key | Default |
|---|---|---|
| `SBCTL_FUNCTIONS_ROOT` | none (`<state_dir>/system/edge-runtime/tenants`) | `/var/lib/sbctl/system/edge-runtime/tenants` |
| `SBCTL_FUNCTIONS_MEMORY_MB` | `memory_mb` | 256 |
| `SBCTL_FUNCTIONS_WALL_CLOCK_SEC` | `wall_clock_seconds` | 400 |
| `SBCTL_FUNCTIONS_IDLE_TIMEOUT_SEC` | `idle_timeout_seconds` (also passed as `--user-worker-request-idle-timeout`) | 150 |
| `SBCTL_FUNCTIONS_CPU_SOFT_MS`, `_CPU_HARD_MS` | `cpu_soft_ms`, `cpu_hard_ms` (negative: off) | 1000, 2000 |
| `SBCTL_FUNCTIONS_WORKER_IDLE_SEC` | none | 60 |
| `EDGE_RUNTIME_PORT` | `[ports] edge_runtime` | 9000 |

## Develop

```
cd functions-main
deno task fmt && deno task lint && deno task check && deno task test    # deno 2.x; CI: ci.yml, job functions-main
go test ./functions-main ./internal/fleet                                 # the embed list, the unit spec
```

`deno test` covers the JWT checks, the file layout and its containment rules, and the handler against a fake runtime (`src/testutil.ts`); the real runtime is exercised by `tests/functions/run.sh` (macOS, exec backend) and `tests/linux/functions-smoke.sh` (Linux, systemd). Imports are relative and there is no remote module, so adding a file means adding it to the `go:embed` line in `embed.go` too; `embed_test.go` fails otherwise.

## Not done

- Per-project logs: a function's `console.log` goes to the runtime's log without a project tag. An event worker (`--event-worker`) could tag lines with the project of the worker's `servicePath` and write `<ref>/functions.log`; `sbctl functions logs <ref>` would then select it.
- Asymmetric JWTs (`ES256`, `RS256`, JWKS): not needed while projects sign with HS256.
- Per-project `DENO_DIR` (see above).
- `static_patterns` of the CLI's `--use-api` metadata are not stored. Every uploaded file is on disk next to the function, but the runtime's virtual file system, not the real one, serves reads from a worker, and reading a static file from a function was not tested.
- `supabase functions download` of a function that was uploaded bundled: the stored upload is a compressed eszip, not sources.
- Request bodies to functions are not limited by sbctl (the 64 MiB limit is on deployments).
