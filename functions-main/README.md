# functions-main

The main service of sbctl's Edge Runtime: one Deno script that every request to `/functions/v1` enters, for every project on the node. The `sb-edge-runtime` unit starts `edge-runtime start --main-service <dir>` with it (`internal/fleet/edgeruntime.go`). The TypeScript here is embedded in the `sbctl` binary (`embed.go`) and written to `<state_dir>/system/edge-runtime/main` before the unit starts, so binary and main service always match. Apache-2.0; written from the Edge Runtime's public API and the self-hosted main service of `supabase/supabase` (`docker/volumes/functions/main/index.ts`) as references.

## What a request does

```
client -> sbctl proxy -> 127.0.0.1:<edge_runtime>  (X-Sbctl-Project-Ref: <ref> and X-Sbctl-Proxy-Token, set by the proxy)
                          main service (this directory)
                            0. proxy secret = X-Sbctl-Proxy-Token (else 403)
                            1. project  = X-Sbctl-Project-Ref (20 letters, else 400)
                            2. function = first path segment (else 404 NOT_FOUND)
                            3. project env + function generation read from <root>/<ref>/
                            4. verify_jwt: HS256 token against this project's secret (else 401)
                            5. EdgeRuntime.userWorkers.create({... this project's env ...}).fetch(request)
```

The proxy strips whatever `X-Sbctl-Project-Ref`, `X-Sbctl-Proxy-Token` and `sb-api-key` a client sent and sets its own (tested in `internal/proxy`, and end to end by `tests/functions/verify.mjs`). The runtime listens on loopback, which a function worker can reach too, so the project header alone proves nothing: the main service serves only requests that carry the node's proxy secret (`SBCTL_FUNCTIONS_PROXY_TOKEN`, compared in constant time; `internal/config/functions_token.go` makes it, `internal/fleet` puts it in the unit's environment file and `internal/proxy` sends it) and answers `403` to the rest, whatever name the caller used to reach the port. Workers never see it: their environment is built from the project's file only, and the main service removes both internal headers before the request reaches the function. `/_internal/health` needs no secret (the fleet probes it). A request with the secret and no valid project header is `400`.

## The files it reads

Written by `internal/functions`, read here (`src/projects.ts`); change both together. The root is `<state_dir>/system/edge-runtime/tenants`, inside the runtime's own state directory: that is the one directory its systemd unit sees besides the artifacts, so the process that runs tenants' code does not see the projects' data directories, sockets or unit files.

```
<SBCTL_FUNCTIONS_ROOT>/<ref>/functions-env.json       {"version":1,"jwt_secret","supabase":{SUPABASE_*},"secrets":{...}}
<SBCTL_FUNCTIONS_ROOT>/<ref>/functions/<slug>          symlink to .gen/<slug>.<version>.<random>/
    .sbctl-function.json                               {slug, version, verify_jwt, kind: "eszip", entrypoint, eszip, stamp, sha256}
    bundle.eszip                                       the bundle `supabase functions deploy` built
```

A request resolves the symlink once (`realPath`) and uses that generation's real paths from then on, so one request never mixes two deployments. A link that resolves outside the project's `functions` directory is never followed, a bundle name that leaves the generation is refused, and the metadata's `slug` must equal the requested one. **Only bundles (`kind: "eszip"`) are served**; a generation without that kind (written by an older sbctl) is `404`.

## Per project, never shared

| What | How |
|---|---|
| Environment of a worker | built from that project's file only: its secrets (never named `SUPABASE_*`), then its `SUPABASE_URL`, `SUPABASE_ANON_KEY`, `SUPABASE_SERVICE_ROLE_KEY`, `SUPABASE_DB_URL`, `SUPABASE_PUBLISHABLE_KEYS`, `SUPABASE_SECRET_KEYS`, `SUPABASE_FUNCTION_SLUG`. The main service's own environment (`PATH`, `HOME`, its settings) is not copied, unlike the upstream sample. The JWT secret never reaches a worker. |
| JWT check | the secret of the project in the header; another project's valid token is `401` |
| Worker pool | `poolKey = <ref>:<slug>:<version>:<generation>:<env stamp>`, so two projects never share a worker, a new deployment or new secrets get new workers and the old ones idle out |
| Code | the function's own bundle (`maybeEszip`); `servicePath` is its generation directory, which holds nothing else |
| Limits | `memoryLimitMb`, `workerTimeoutMs`, CPU soft and hard limits, `requestAbsentTimeoutMs` from `[functions]` in `config.toml`, the same for every worker |
| Module cache | **one** `DENO_DIR` for the whole runtime (`<state_dir>/system/edge-runtime/deno`), not one per project: the runtime reads `DENO_DIR` once from its process environment, and the main worker cannot change it (`Deno.env.set` throws `NotSupported`, measured with v1.77.4). The cache is keyed by URL and holds public modules; remote imports of all projects share it. |

## Isolation, as measured

Edge Runtime v1.77.4, user workers, with `permissions` set as in `src/handler.ts` (the runtime's defaults for user workers plus a deny rule):

- **Modules.** A function runs from its eszip only. A function that ran from source files (an upload of sources, which the node now bundles first) could import files outside its directory: the module loader follows relative specifiers, static and dynamic, and `allow_read: []` only blocks `Deno.readTextFile`, not module loading, so with the tenants tree on disk `import("../../../../<other ref>/functions-env.json", {with: {type: "json"}})` returned the other project's JWT secret, service_role key, database password and secrets, and `import "../../../../<other ref>/functions/hello/index.ts"` ran its code (measured with v1.77.4; `customModuleRoot` did not stop it). The module URLs inside an eszip are virtual, so absolute paths, file URLs and 14 levels of `../` toward the real tenants files all fail with a module error: `tests/functions/verify.mjs` (function `escape`) asserts it on macOS and on Linux. Remote imports, bundled at deploy time by the CLI, are inside the eszip too.
- **Disk.** `Deno.readTextFile` of `/etc/hosts`, of another project's `functions-env.json` and of the function's own files all fail with `NotFound`: a worker sees its module graph, not the disk. `tests/functions/verify.mjs` asserts this on macOS and on Linux (CI). Its `Deno.env.get` also sees only the variables above.
- **`/proc`, processes, and whether the runtime needs a uid of its own against its workers.** It does not. The bundler needed one because it is a separate process that opens paths an uploader names (a process may open `/proc/<pid>/root` and `/proc/<pid>/environ` of every process with its own uid, and that walks around a mount namespace). A user worker is an isolate inside the runtime process: its file system is its module graph plus its own `/tmp`, its `Deno.readDir('/proc')` is `PermissionDenied`, `/proc/self/environ`, `/proc/self/root/...` and `/proc/1/environ` are `NotFound`, and `Deno.Command` does not run a child (measured with v1.77.4 on macOS, asserted on Linux by `verify.mjs`, function `readfs`), so tenant code has no way to name a path under `/proc`, and no process of its own to name it from. What a different uid for the runtime would add is protection only after a V8 or runtime escape, and then the attacker is inside the one process that holds every project's `functions-env.json` in its heap (the main service reads them), so the boundary is lost with the escape whatever uid the runtime has. The runtime shares the `sbctl` uid with the other units like every other service (`deploy/systemd/README.md`, "Trust model"); one uid per project, and the runtime as its own, stays under "Future hardening" there.
- **Disk quota of `/tmp`.** A worker's `/tmp` is its one writable place, and the runtime backs it with real files on the node's disk, which holds every project's database and WAL (measured: a worker wrote 40 MiB with no limit). Every worker therefore gets `tmpFsConfig: { quota }` from `[functions] tmp_quota_mb` (default 64; the quota is in bytes in the runtime, `crates/fs/impl/tmp_fs.rs`). A write past it throws `filesystem quota exceeded` inside that function; the other functions, the other projects and the runtime are not affected (`verify.mjs`, function `tmpwrite`, and a local run against the real runtime). At most `max_workers x tmp_quota_mb` is in use at once (1 GiB by default): the config does not check it against the free space of the disk, so a node with a small disk lowers one of the two.
- **The runtime's own port.** A worker's `fetch` to `127.0.0.1`, `localhost`, `[::1]`, `0.0.0.0` or `[::]` on the runtime's port fails with `NotCapable` (`deny_net`), but the rule matches host names as written: a name that resolves to loopback (`<x>.127.0.0.1.sslip.io`) and IPv4-mapped IPv6 literals (`[::ffff:127.0.0.1]`) get through to the port. They get `403` there, because the worker does not have the proxy secret; the project header it writes is worthless. `tests/functions/verify.mjs` (function `callout`) tries four ways and asserts that none is served. The same loopback is open to a worker for every other service of the node (the Management API on the admin listener, the proxy's own listeners, Postgres and the project services behind their own credentials); the claim endpoint therefore believes `X-Forwarded-For` from a loopback peer only with the proxy secret when Edge Functions are on (`internal/api/claim.go`).
- **Fairness.** See the next section.
- **Other workers' memory and CPU.** Each worker is a V8 isolate with its own heap limit; a crash, a boot failure, a busy loop or an allocation loop in one project's function ended with an error response for that request while the other project answered within milliseconds (`verify.mjs`, "runaway function", "memory limit"). This is V8 isolation, not a VM or a container: a V8 escape would reach the runtime process and, in the systemd unit, everything it can read (see `deploy/systemd/README.md`).
- **Threads.** User workers do not get a thread each. Their isolates are pinned to a pool of OS threads whose size is `EDGE_RUNTIME_WORKER_POOL_SIZE` or, unset, the number of CPUs (`crates/base_rt/src/lib.rs`, v1.77.4), and an isolate that computes without yielding blocks every other isolate on its thread, whichever project it belongs to. Measured on macOS with a function that spins forever: with a dozen warm functions on ten CPUs, requests to an unrelated function (another project's too) waited for the idle timeout until the spinner was gone, and which function was unlucky depended on how many were warm. The unit therefore sets `EDGE_RUNTIME_WORKER_POOL_SIZE` to `max_workers x max_parallelism`, one thread per isolate the budget allows (`internal/fleet/edgeruntime.go`; the threads are idle until used). Tenants still share the CPUs, and a spinning function costs one core until it ends: on Linux by the CPU limit, on macOS by the wall clock.
- **CPU limit.** `cpuTimeHardLimitMs` is enforced by the runtime's CPU timer, which exists on Linux only (`CPU timer: not enabled (need Linux)` on macOS). On macOS a busy loop is ended by the request idle timeout (504) or the wall clock; on Linux by the CPU limit.

## Fairness between projects, and the memory budget

One runtime serves every project, and the unit's `MemoryMax` is shared by all of its workers: a runtime that reaches its cgroup limit is killed whole, for every project. edge-runtime's `--max-parallelism` does **not** bound that. In v1.77.4 it is the size of a semaphore that each worker pool key gets for itself (`crates/base/src/worker/pool.rs`: `ActiveWorkerRegistry::new(max_parallelism)` per `worker_pool_key`), and sbctl's pool key is `<ref>:<slug>:<version>:<generation>:<env stamp>`, so the number of projects, functions and generations is the only thing that limits the workers. `--max-parallelism` is therefore set per function (`[functions] max_parallelism`, default 1: the `per_worker` policy sends every request of a function to its one worker, so more than one exists only for a moment during a cold burst), and the budget is enforced here, in `src/limiter.ts`, before a worker is touched:

| Budget | Setting | Default | Over it |
|---|---|---|---|
| live workers of the whole runtime, all projects | `max_workers` (or derived from `memory_max`) | 16 | `503 PROJECT_AT_CAPACITY`, "the runtime is running as many functions as it can" |
| live workers of one project | `max_workers_per_project` | half of `max_workers` | `503 PROJECT_AT_CAPACITY` |
| requests of one project in flight | `max_per_project` | 128 | `503 PROJECT_AT_CAPACITY` |
| bundle bytes of one project's functions in flight | `SBCTL_FUNCTIONS_MAX_BUNDLE_MB` | 192 | `503 PROJECT_AT_CAPACITY` |

A pool key counts as live while it has a request in flight and for the worker idle time after the last one (the runtime retires the worker then), so a redeployment, or new secrets, count as an extra worker until the old one idles out. **A request is in flight until its response body has ended**, not until the headers are back (`src/body.ts`): a function that streams (server-sent events, token streams) keeps its worker busy for as long as the stream runs, up to the wall clock, so the body is wrapped and the place in the budgets is given back when it completes, fails (the worker was retired) or is cancelled (the client left). A response without a body (HEAD, 204, a WebSocket upgrade, whose socket the main service does not see) releases at once. A consumer that neither reads nor cancels is let go at the worker's wall clock plus 5 s, when the runtime has retired the worker anyway. The unit's `MemoryMax` is derived from the same numbers (`internal/config/functions.go`): `max_workers x (memory_mb + 32 MB isolate overhead) x max_parallelism + 256 MB` for the runtime itself, 4864M by default, and config validation refuses a `memory_max` that cannot hold `max_workers` workers (with `max_workers` unset, `memory_max` decides how many fit). A request over a request cap is a cap, not a scheduler: many projects together can use the whole worker budget, and the ones that come later get `503` until a worker idles out; raise `max_workers` (and the memory) on a node with many functions. A worker serves many requests at once, so `max_per_project` is generous and is not what protects memory. `src/limiter_test.ts` and `src/handler_test.ts` ("many projects each warming many functions never exceed the budget", "the runtime-wide worker budget holds across projects") check the budget with several projects and memory-hungry functions, and `tests/linux/functions-smoke.sh` restarts the real unit with `max_workers = 4` and asserts the 503s, the unit's `MemoryMax` of 1408 MiB, and that nothing restarted. `tests/functions/verify.mjs` also floods project A with parallel runaway and memory-hungry calls and asserts that B answers within 3 s and that A's excess is refused fast.

The bundle of the function is read into this service's heap for every request (the runtime takes the bytes with the worker options). Bundles are cached in memory (128 MiB, least recently used first out, a bundle bigger than the cache is read each time), at most 32 MiB decompressed (`internal/functions`; hosted accepts about 20 MB), and the bundle bytes of the functions a project has in flight are charged against its budget above.

## Errors

Shapes follow the self-hosted main service, so clients that parse them keep working.

| Situation | Status | `sb-error-code` |
|---|---|---|
| no or wrong proxy secret | 403 | `BAD_REQUEST` |
| no or malformed project header | 400 | `BAD_REQUEST` |
| unknown project, function or slug | 404 | `NOT_FOUND` |
| no / malformed / foreign / expired token | 401 | `UNAUTHORIZED_NO_AUTH_HEADER`, `UNAUTHORIZED_INVALID_JWT_FORMAT`, `UNAUTHORIZED_LEGACY_JWT`, `UNAUTHORIZED_UNSUPPORTED_TOKEN_ALGORITHM` |
| unreadable deployment files, worker does not boot | 503 | `BOOT_ERROR` |
| over the memory or CPU limit, request cancelled | 546 | `WORKER_RESOURCE_LIMIT` |
| over a budget (see above) | 503 (`Retry-After: 1`) | `PROJECT_AT_CAPACITY` |
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
| `SBCTL_FUNCTIONS_TMP_QUOTA_MB` | `tmp_quota_mb` (negative: off; the variable takes 0 for off) | 64 |
| `SBCTL_FUNCTIONS_WORKER_IDLE_SEC` | none | 60 |
| `SBCTL_FUNCTIONS_PROXY_TOKEN` | none (`<state_dir>/system/edge-runtime.token`) | empty accepts any caller (tests only; sbctl always sets it) |
| `SBCTL_FUNCTIONS_MAX_PER_PROJECT` | `max_per_project` (negative: off) | 128 |
| `SBCTL_FUNCTIONS_MAX_WORKERS` | `max_workers` (negative: off) | 16 |
| `SBCTL_FUNCTIONS_MAX_WORKERS_PER_PROJECT` | `max_workers_per_project` | 8 |
| `SBCTL_FUNCTIONS_MAX_BUNDLE_MB` | none | 192 |
| `SBCTL_FUNCTIONS_WORKER_COST_MB` | none (informational: what one function counts against the budget) | `max_parallelism x (memory_mb + 32)` |
| `--max-parallelism` | `max_parallelism` (workers one function may have) | 1 |
| `EDGE_RUNTIME_PORT` | `[ports] edge_runtime` | 9000 |

## Develop

```
cd functions-main
deno task fmt && deno task lint && deno task check && deno task test    # deno 2.x; CI: ci.yml, job functions-main
go test ./functions-main ./internal/fleet                                 # the embed list, the unit spec
```

`deno test` covers the JWT checks, the file layout and its containment rules, and the handler against a fake runtime (`src/testutil.ts`; its responses are read to the end by `draining`, as the HTTP server reads them, because a response holds its place in the budgets until then); the real runtime is exercised by `tests/functions/run.sh` (macOS, exec backend) and `tests/linux/functions-smoke.sh` (Linux, systemd). Imports are relative and there is no remote module, so adding a file means adding it to the `go:embed` line in `embed.go` too; `embed_test.go` fails otherwise.

## Not done

- Per-project logs: a function's `console.log` goes to the runtime's log without a project tag. An event worker (`--event-worker`) could tag lines with the project of the worker's `servicePath` and write `<ref>/functions.log`; `sbctl functions logs <ref>` would then select it.
- Asymmetric JWTs (`ES256`, `RS256`, JWKS): not needed while projects sign with HS256.
- Per-project `DENO_DIR`: decided against for v1 (see "Per project, never shared"). Only eszip bundles run, and their modules are inside the bundle, so workers use the cache for nothing but public remote modules the runtime itself fetches. The one place a cache matters is the bundler of uploaded sources, which keeps its own (`sb-edge-bundle.service`, see `internal/functions/README.md`).
- `supabase functions download` of a function that was uploaded already bundled (the CLI's Docker flow): the stored upload is a compressed eszip, not sources. Functions uploaded as sources (`--use-api`, Studio) keep their sources and download as such.
- Request bodies to functions are not limited by sbctl (the 64 MiB limit is on deployments).
