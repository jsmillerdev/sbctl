# internal/functions

Puts what the Management API stores for Edge Functions where the runtime reads it. The API (`internal/api`, workstream B) stores deployments (bundles, and for uploads of sources the sources together with the bundle the node made from them) and sealed secrets in the registry's database; the Edge Runtime's main service (`functions-main/`) reads files. This package is the step between them.

```go
syncer, err := functions.New(functions.Deps{
    Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets,
    Store: functions.NewStore(n.Registry), // or the api.Store the API server uses
    Keys:  n.Engine.Keys,                  // lifecycle.Manager.Keys
    Log:   log,
    Supervisor: n.Supervisor, Artifacts: n.Artifacts, // to bundle uploaded sources (see below); without them such uploads answer 501
})
go syncer.Run(ctx)                         // reconcile now and every [functions] reconcile_seconds
api.New(api.Deps{ /* ... */ Store: store, Functions: syncer }) // api.FunctionsHook
```

`*Syncer` implements `api.FunctionsHook`: the API calls `FunctionsChanged(ctx, ref)` after every create, deploy, patch and delete of a function, after secrets are set or removed, and after a project is deleted, paused or resumed. The call returns once the files match the store; if it fails, the API answers 500 "The change was stored but could not be applied", the deployment is not lost, and the periodic reconcile retries it. A node without the runtime leaves `Deps.Functions` empty and nothing here runs.

## What it writes

```
<state>/system/edge-runtime/tenants/<ref>/functions-env.json           0600   jwt secret, SUPABASE_* values, secrets (JSON)
<state>/system/edge-runtime/tenants/<ref>/functions/<slug>             symlink  ->  .gen/<slug>.<version>.<random>
<state>/system/edge-runtime/tenants/<ref>/functions/.gen/<...>/        0700   one generation: bundle.eszip and .sbctl-function.json
```

The tree is in the Edge Runtime's own state directory (`config.Paths.FunctionsRoot()`), not under `projects/<ref>/` as `HANDOFF.md` 3.J says: since the unit templates became allowlists, `sb-edge-runtime` sees only the artifacts, its launcher and `system/edge-runtime`, and binding `projects/` to it would show it every cluster's data directory and unix socket (a process that sees a socket connects as `supabase_admin`) while it runs tenants' code. Cost: a project's functions are not removed by `Engine.Delete`'s `RemoveAll` of `projects/<ref>`; `sbctl projects delete` calls `RemoveFiles`, and a running API server's reconcile removes the tree of any project the registry no longer has.

- **`functions-env.json`** (`buildEnv`): `SUPABASE_URL` (the project's public origin; see `ProjectURL`, `[functions] project_url_template`), `SUPABASE_ANON_KEY`, `SUPABASE_SERVICE_ROLE_KEY`, `SUPABASE_DB_URL` (`postgres://postgres:<password>@127.0.0.1:<project postgres port>/postgres?sslmode=disable`, straight to the project's cluster, no pooler), `SUPABASE_PUBLISHABLE_KEYS` and `SUPABASE_SECRET_KEYS` (`{"default": "..."}` as the upstream compose file passes them), the project's JWT secret for the main service only, and the function secrets, opened from the sealed store. Rewritten only when its content or mode changes. A project with no function and no secret gets no file (its keys are not copied for a project that does not use Edge Functions), and loses the file and the `functions` directory when it ends up with neither.
- **A generation** is `bundle.eszip` and `.sbctl-function.json`. The upload is `EZBR` + Brotli of an eszip and the runtime wants the plain eszip, so it is decompressed here, streaming into the file, capped at 64 MiB (a bomb stops at the cap; hosted functions are limited to about 20 MB) and required to start with `ESZIP`. `.sbctl-function.json` holds slug, version, `verify_jwt`, kind (`eszip`), the bundle's entry module URL, a `stamp` (the stored deployment's version and update time) and a SHA-256 over the stored upload.
- **Only bundles are served.** A function that runs from source files runs from real paths, and the runtime's module loader follows relative imports (static and dynamic) out of the function's directory: a source function of one project could import `../../../../<other ref>/functions-env.json` (the other project's JWT secret, service_role key, database password and secrets) or another project's code. The module specifiers inside an eszip are virtual, so a bundled function has nothing of the node to import (`tests/functions/verify.mjs`, function `escape`). Uploads of sources (`supabase functions deploy --use-api`, the CLI when Docker is not running, Studio's function editor, `POST .../functions/deploy`) are therefore bundled by the node first, in a sandbox, see "Bundling uploaded sources". A function stored as sources without a bundle (before this existed, or while no runtime was configured) is skipped with one warning per stored version and no files.
- **A deployment** writes a new generation in `.gen/` and renames a new symlink over `functions/<slug>`, which is atomic: a reader gets the old function or the new one, never a gap or a mixture (`TestSwapIsAtomicForReaders`). The previous generation stays, because workers of the old deployment may still read from it; older ones are removed, as are generations nothing points at after 15 minutes and leftovers of an interrupted swap. Generation names use a dot as separator (`hello-2.1.x` belongs to `hello-2`, never to `hello`).
- **Unchanged functions cost nothing.** `Reconcile` visits every function of every project every few seconds, and the stored files can be large, so a function is looked at in this order: its stamp (store version and update time; every store write bumps both) against the live generation's; only when they differ are its files loaded, decoded and written. A deployment that has nothing to serve (no files, sources, a bundle that cannot be decoded or has no entrypoint) is remembered in memory by its stamp, so a large or hostile upload is loaded once per version, not once per cycle; the error of a bad bundle is returned the first time (the deploy that stored it gets the 500) and later syncs skip it silently. `TestReconcileLoadsNothingForUnchangedFunctions` counts the loads.
- **Refused, with the generation removed and nothing live**: a bundle that is not `EZBR`+Brotli+eszip or is over the cap; a bundle without an entrypoint. One broken function does not keep the others from going live (the errors are joined).
- **A function without files** (created with the legacy JSON create, no body) is not served.

## Bundling uploaded sources

`Bundler` (`bundler.go`, wired through `Syncer.BundleSources`, which implements `api.SourceBundler`; the API calls it from `POST /v1/projects/{ref}/functions/deploy`) turns an upload of source files into the eszip that is served, with the artifact's own `edge-runtime bundle`, the command the Supabase CLI runs in its Docker image:

1. The files are written under `<state>/system/edge-bundle/work/src/` (paths as uploaded, relative to the project's working directory; `..`, duplicates and file/directory conflicts are refused) and `edge-runtime bundle --entrypoint <src>/<entrypoint_path> --output <work>/out.eszip [--import-map <src>/<import_map_path>] [--static <pattern>]... --timeout 100` runs as the one-shot unit **`sb-edge-bundle.service`**. Arguments and environment follow the CLI (`apps/cli/src/shared/functions/deploy.ts`, `bundleFunctionWithDocker`): the import map is passed unless it is the `deno.json` next to the entrypoint, `DENO_NO_PACKAGE_JSON=1` unless the entrypoint's directory has a `package.json` and there is no import map.
2. Bundling reads the imports of someone else's code from the disk, which is the same attack as running it (a relative import that climbs out of the upload resolves to any file the `sbctl` user can read, and the bundle carries the content out to the uploader). So the unit (`deploy/systemd/sb-edge-bundle.service`) sees nothing of the node but the scratch directory `system/edge-bundle` and the artifacts (`TemporaryFileSystem=/var/lib/sbctl:ro`, the same containment as `sb-edge-runtime`), has no access to loopback services (`IPAddressDeny=localhost`, with the stub resolver of systemd-resolved allowed) or the instance metadata service, `MemoryMax=1G`, `CPUQuota=100%`, `TasksMax=512` and `TimeoutStartSec=150s`. It needs the network for remote imports (`https:`, `npm:`). It does not run the function's code. `tests/functions/run.sh` asserts on Linux that an upload importing project A's `functions-env.json` is refused (400, nothing stored) and that the JWT secret does not appear in the error.
3. The bundler's output is read (at most 64 MiB, must start with `ESZIP`), compressed to the form the CLI uploads (`EZBR` + Brotli, quality 6) and stored by the API **next to the sources**: `.sbctl-bundle.ezbr` and `.sbctl-bundle.json` (`{"entrypoint": ...}`, the file URL of the entrypoint where it was bundled; edge-runtime v1.77.4 starts from the entrypoint key recorded inside the eszip and uses this URL only as a fallback). The sources stay readable for `supabase functions download` and Studio's editor (the node's two files are left out of the download); the materializer serves the bundle and ignores the sources. The names `.sbctl-bundle.*` cannot be uploaded.
4. One bundling at a time (the unit is a singleton with one scratch directory), up to eight uploads waiting, then `429`. The scratch directory is deleted afterwards, whatever happened; `system/edge-bundle/deno` is the module cache of the bundler, emptied when it passes 512 MiB.

Errors reach the uploader: a failure of the code (`Module not found ...`, a syntax error) is `400 Could not bundle the function: <the bundler's output>` (with the scratch path removed), a node that cannot bundle (no supervisor, artifact not fetched, no sandbox) is `501` with what to do instead (`supabase functions deploy` with Docker running, which bundles on the client), a full queue `429`.

**A node whose supervisor cannot sandbox does not bundle uploads.** Only systemd units are confined (`units.Sandboxer`); on the exec backend (macOS and development machines) uploads of sources answer `501`, and the bundler is created only with `[functions] bundle_unsandboxed = true` (or `SBCTL_FUNCTIONS_BUNDLE_UNSANDBOXED=true`), which is for a development machine that serves nobody else's projects. Without it, on such a node deploy a bundle: `supabase functions deploy` with Docker running, or `tests/functions/run.sh` with `DEPLOY_VIA=artifact` (the artifact's own `edge-runtime bundle` and curl).

**Without Docker, the CLI falls back to the API.** `supabase functions deploy` uses Docker to bundle when Docker runs and otherwise uploads the sources (`useLocalBundler = !flags.useApi && ...` then "Docker is not running" falls back to `deployWithApi`, CLI v2.119.0), so on a machine without Docker the plain command, `--use-api` and Studio all depend on this.

## When it runs

| Trigger | What |
|---|---|
| API change (hook) | `SyncProject(ref)`: env file, then every function of the project; links of functions the store no longer has are removed |
| `sbctl projects rotate-keys` | the command calls `SyncProject(ref)` itself after the keys changed (`cmd/sbctl/cmd_projects.go`), so functions check the new JWT secret at once instead of at the next reconcile of a running API server |
| every `reconcile_seconds`, and at start | `Reconcile`: the same for every project but `system`, then `collectGone`; this is what makes restored data and a failed first attempt show up (and key rotation, for a rotation made by another route) |
| project delete, pause, resume | the API tells the hook after each (`functionsGone`), so a deleted or paused project's keys and secrets leave the disk at once; `sbctl projects delete` calls `RemoveFiles(cfg, ref)`; any other deletion is found by the next `SyncProject(ref)` or `Reconcile` (`collectGone` looks the project up again under its lock, so a project created since the listing keeps what its first deployment wrote). A project in a status the proxy does not serve (`INACTIVE`, `PAUSING`, `GOING_DOWN`, `REMOVED`, `INIT_FAILED`) has no files: a request that reaches the runtime without passing the proxy's status check finds nothing to run, and resuming rebuilds the files from the store |

`SyncProject` of a project the registry does not know removes its tree. Work on one project is serialized by a lock per ref, shared with `collectGone`.

## CLI

`cmd/sbctl/cmd_functions.go`:

```
sbctl functions list <ref> [--json]            what is deployed, and whether the disk has that version ("live")
sbctl functions invoke <ref> <slug> [subpath]  one request through the node's own proxy, with the project's anon key
                                               (-X, -d, -H, --service-role, --jwt, --no-auth)
sbctl functions logs [ref] [-n N] [-f] [--all] the shared runtime log (journalctl, or the exec backend's file)
sbctl functions dev [--token-file F]           Management API + proxy + sb-edge-runtime in one process
```

`dev` exists because `sbctl serve` (workstream X) wires the API and the proxy and this feature is not in it yet. It forces `[functions] enabled`, starts only the runtime of the fleet, mounts `api.Server` with the hook, runs the reconcile loop and the proxy with `FunctionsEnabled`, and with `--token-file` mints a personal access token for the Supabase CLI: it carries the rights of a node administrator, expires after 12 hours and is deleted, with the file, when the command ends. `tests/functions/run.sh` and `tests/linux/functions-smoke.sh` use it.

## Wiring into `sbctl serve` (not done here, `cmd/sbctl/cmd_serve.go` belongs to X)

```go
store := functions.NewStore(n.Registry)                       // or the store already given to api.Deps
fs, err := functions.New(functions.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Store: store, Keys: n.Engine.Keys, Log: log,
    Supervisor: n.Supervisor, Artifacts: n.Artifacts}) // the last two let uploads of sources be bundled
// api.Deps: add  Store: store, Functions: fs
// proxy.Options: add  FunctionsEnabled: cfg.Functions.Enabled   (the proxy reads the node's proxy secret itself)
if cfg.Functions.Enabled { go fs.Run(ctx) }
// fleet.Setup / fleet.NewManager: nothing to add; with [functions] enabled they start sb-edge-runtime,
// and `sbctl fleet start` fetches the edge-runtime artifact (fleet.ServicesFor).
```

## Tests

```
go test ./internal/functions        # unit tests over the memory registry and memory store, no processes
tests/functions/run.sh              # the real thing against a running node, see its header
```

Unit tests cover the env file (mode, content, multi-line secrets, nothing of another project), generation swap and garbage collection, atomic swaps under concurrent readers, two projects with the same slug, delete, orphans, source functions that are not materialized, bundles (a real CLI 2.119.0 upload is `testdata/hello.ezbr`) including a compression bomb, loads avoided for unchanged functions, reconcile, key rotation and pausing, project URLs.

## Verified

On darwin-arm64 with the slim-services artifacts under the exec backend (`sbctl functions dev`), the real `supabase` CLI 2.119.0 and supabase-js 2.117.2: two projects, the same slugs with different code deployed with `supabase functions deploy` (bundled, the default; `--use-api` is refused), `secrets set`, invoked through `internal/proxy`; per-project code, secrets and `SUPABASE_URL`; `verify_jwt` on (anon, service_role, publishable and secret keys accepted; no, bad, expired, foreign tokens `401`) and off (`--no-verify-jwt`); a function that queries its own database through `SUPABASE_DB_URL` and through supabase-js with the service key; a function that does not boot, one that never returns and one that allocates without end, none of which affected the other project; changed secrets and a redeploy visible on the next request; delete of a function; delete of a project removing its files. All of it is `tests/functions/run.sh` + `verify.mjs`.

## Not done, not verified

- Linux and systemd: `tests/linux/functions-smoke.sh` (CI job `functions-smoke`, amd64 and arm64) runs the same steps under the real unit; see `.github/workflows/linux.yml` for its result.
- A runtime process per project is not an option here; isolation is the runtime's (see `functions-main/README.md`).
- Per-project function logs and `supabase functions download` of bundled functions (`functions-main/README.md`, "Not done").
- Metrics and rate limits. Per-project concurrency is limited in the main service (`functions-main/README.md`, "Fairness").
