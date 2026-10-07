# internal/functions

Puts what the Management API stores for Edge Functions where the runtime reads it. The API (`internal/api`, workstream B) stores deployments (bundles; sources only where no runtime is configured), and sealed secrets in the registry's database; the Edge Runtime's main service (`functions-main/`) reads files. This package is the step between them.

```go
syncer, err := functions.New(functions.Deps{
    Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets,
    Store: functions.NewStore(n.Registry), // or the api.Store the API server uses
    Keys:  n.Engine.Keys,                  // lifecycle.Manager.Keys
    Log:   log,
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
- **Only bundles are served.** A function that runs from source files runs from real paths, and the runtime's module loader follows relative imports (static and dynamic) out of the function's directory: a source function of one project could import `../../../../<other ref>/functions-env.json` (the other project's JWT secret, service_role key, database password and secrets) or another project's code. The module specifiers inside an eszip are virtual, so a bundled function has nothing of the node to import (`tests/functions/verify.mjs`, function `escape`). Therefore the API refuses source uploads (`supabase functions deploy --use-api`, `POST .../functions/deploy`) with a 400 where a runtime is configured, and this package never writes source files: a function stored as sources (before this rule, or while no runtime was configured) is skipped with one warning per stored version and no files.
- **A deployment** writes a new generation in `.gen/` and renames a new symlink over `functions/<slug>`, which is atomic: a reader gets the old function or the new one, never a gap or a mixture (`TestSwapIsAtomicForReaders`). The previous generation stays, because workers of the old deployment may still read from it; older ones are removed, as are generations nothing points at after 15 minutes and leftovers of an interrupted swap. Generation names use a dot as separator (`hello-2.1.x` belongs to `hello-2`, never to `hello`).
- **Unchanged functions cost nothing.** `Reconcile` visits every function of every project every few seconds, and the stored files can be large, so a function is looked at in this order: its stamp (store version and update time; every store write bumps both) against the live generation's; only when they differ are its files loaded, decoded and written. A deployment that has nothing to serve (no files, sources, a bundle that cannot be decoded or has no entrypoint) is remembered in memory by its stamp, so a large or hostile upload is loaded once per version, not once per cycle; the error of a bad bundle is returned the first time (the deploy that stored it gets the 500) and later syncs skip it silently. `TestReconcileLoadsNothingForUnchangedFunctions` counts the loads.
- **Refused, with the generation removed and nothing live**: a bundle that is not `EZBR`+Brotli+eszip or is over the cap; a bundle without an entrypoint. One broken function does not keep the others from going live (the errors are joined).
- **A function without files** (created with the legacy JSON create, no body) is not served.

## When it runs

| Trigger | What |
|---|---|
| API change (hook) | `SyncProject(ref)`: env file, then every function of the project; links of functions the store no longer has are removed |
| every `reconcile_seconds`, and at start | `Reconcile`: the same for every project but `system`, then `collectGone`; this is what makes key rotation (`sbctl projects rotate-keys`), restored data and a failed first attempt show up |
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

`dev` exists because `sbctl serve` (workstream X) wires the API and the proxy and this feature is not in it yet. It forces `[functions] enabled`, starts only the runtime of the fleet, mounts `api.Server` with the hook, runs the reconcile loop and the proxy with `FunctionsEnabled`, and can mint a personal access token for the Supabase CLI. `tests/functions/run.sh` and `tests/linux/functions-smoke.sh` use it.

## Wiring into `sbctl serve` (not done here, `cmd/sbctl/cmd_serve.go` belongs to X)

```go
store := functions.NewStore(n.Registry)                       // or the store already given to api.Deps
fs, err := functions.New(functions.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Store: store, Keys: n.Engine.Keys, Log: log})
// api.Deps: add  Store: store, Functions: fs
// proxy.Options: add  FunctionsEnabled: cfg.Functions.Enabled
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
