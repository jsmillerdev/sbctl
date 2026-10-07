# internal/functions

Puts what the Management API stores for Edge Functions where the runtime reads it. The API (`internal/api`, workstream B) stores deployments, sources or bundles, and sealed secrets in the registry's database; the Edge Runtime's main service (`functions-main/`) reads files. This package is the step between them.

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

`*Syncer` implements `api.FunctionsHook`: the API calls `FunctionsChanged(ctx, ref)` after every create, deploy (source or bundle), patch and delete of a function and after secrets are set or removed. The call returns once the files match the store; if it fails, the API answers 500 "The change was stored but could not be applied", the deployment is not lost, and the periodic reconcile retries it. A node without the runtime leaves `Deps.Functions` empty and nothing here runs.

## What it writes

```
<state>/system/edge-runtime/tenants/<ref>/functions-env.json           0600   jwt secret, SUPABASE_* values, secrets (JSON)
<state>/system/edge-runtime/tenants/<ref>/functions/<slug>             symlink  ->  .gen/<slug>.<version>.<random>
<state>/system/edge-runtime/tenants/<ref>/functions/.gen/<...>/        0700   one generation: the files and .sbctl-function.json
```

The tree is in the Edge Runtime's own state directory (`config.Paths.FunctionsRoot()`), not under `projects/<ref>/` as `HANDOFF.md` 3.J says: since the unit templates became allowlists, `sb-edge-runtime` sees only the artifacts, its launcher and `system/edge-runtime`, and binding `projects/` to it would show it every cluster's data directory and unix socket (a process that sees a socket connects as `supabase_admin`) while it runs tenants' code. Cost: a project's functions are not removed by `Engine.Delete`'s `RemoveAll` of `projects/<ref>`; `sbctl projects delete` calls `RemoveFiles`, and a running API server's reconcile removes the tree of any project the registry no longer has.

- **`functions-env.json`** (`buildEnv`): `SUPABASE_URL` (the project's public origin; see `ProjectURL`, `[functions] project_url_template`), `SUPABASE_ANON_KEY`, `SUPABASE_SERVICE_ROLE_KEY`, `SUPABASE_DB_URL` (`postgres://postgres:<password>@127.0.0.1:<project postgres port>/postgres?sslmode=disable`, straight to the project's cluster, no pooler), `SUPABASE_PUBLISHABLE_KEYS` and `SUPABASE_SECRET_KEYS` (`{"default": "..."}` as the upstream compose file passes them), the project's JWT secret for the main service only, and the function secrets, opened from the sealed store. Rewritten only when its content or mode changes. A project with no function and no secret gets no file (its keys are not copied for a project that does not use Edge Functions), and loses the file and the `functions` directory when it ends up with neither.
- **A generation** is a function's uploaded files at their uploaded paths (the CLI anchors them at the workdir: `supabase/functions/<slug>/index.ts`, `supabase/functions/_shared/...`), or, for a function the CLI bundled, `bundle.eszip`: the upload is `EZBR` + Brotli of an eszip, and the runtime wants the plain eszip, so it is decompressed here (capped at 512 MiB, must start with `ESZIP`). `.sbctl-function.json` holds slug, version, `verify_jwt`, kind (`source` or `eszip`), entrypoint (a path for sources, the bundle's entry module URL for bundles), import map, and a SHA-256 over the stored files.
- **A deployment** writes a new generation in `.gen/` and renames a new symlink over `functions/<slug>`, which is atomic: a reader gets the old function or the new one, never a gap or a mixture (`TestSwapIsAtomicForReaders`). The previous generation stays, because workers of the old deployment may still read from it; older ones are removed, as are generations nothing points at after 15 minutes and leftovers of an interrupted swap. A function that is unchanged (same version and hash) gets no new generation. Generation names use a dot as separator (`hello-2.1.x` belongs to `hello-2`, never to `hello`).
- **Refused, with the generation removed and nothing live**: a file path that is absolute, contains `..` or a backslash; a file named `.sbctl-function.json`; an entrypoint or import map that is not among the files; a bundle that is not `EZBR`+Brotli+eszip; a bundle without an entrypoint. The API validates uploads already; this package validates again because it writes files with the names. One broken function does not keep the others from going live (the errors are joined).
- **A function without sources** (created with the legacy JSON create, no body) is not served.

## When it runs

| Trigger | What |
|---|---|
| API change (hook) | `SyncProject(ref)`: env file, then every function of the project; links of functions the store no longer has are removed |
| every `reconcile_seconds`, and at start | `Reconcile`: the same for every project but `system`, then `collectGone`; this is what makes key rotation (`sbctl projects rotate-keys`), restored data and a failed first attempt show up |
| project delete | `sbctl projects delete` calls `RemoveFiles(cfg, ref)`; otherwise (a delete through the API) the next `SyncProject(ref)` or `Reconcile` finds the project gone and removes its tree (`collectGone` looks the project up again under its lock, so a project created since the listing keeps what its first deployment wrote). A project that is `GOING_DOWN`, `REMOVED` or `INIT_FAILED` loses its tree the same way |

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

Unit tests cover the env file (mode, content, multi-line secrets, nothing of another project), generation swap and garbage collection, atomic swaps under concurrent readers, two projects with the same slug, delete, orphans, refused uploads, bundles (a real CLI 2.119.0 upload is `testdata/hello.ezbr`), reconcile and key rotation, project URLs.

## Verified

On darwin-arm64 with the slim-services artifacts under the exec backend (`sbctl functions dev`), the real `supabase` CLI 2.119.0 and supabase-js 2.117.2: two projects, the same slugs with different code deployed with `supabase functions deploy` (bundled, the default, and `--use-api`), `secrets set`, invoked through `internal/proxy`; per-project code, secrets and `SUPABASE_URL`; `verify_jwt` on (anon, service_role, publishable and secret keys accepted; no, bad, expired, foreign tokens `401`) and off (`--no-verify-jwt`); a function that queries its own database through `SUPABASE_DB_URL` and through supabase-js with the service key; a function that does not boot, one that never returns and one that allocates without end, none of which affected the other project; changed secrets and a redeploy visible on the next request; delete of a function; delete of a project removing its files. All of it is `tests/functions/run.sh` + `verify.mjs`.

## Not done, not verified

- Linux and systemd: `tests/linux/functions-smoke.sh` (CI job `functions-smoke`, amd64 and arm64) runs the same steps under the real unit; see `.github/workflows/linux.yml` for its result.
- A runtime process per project is not an option here; isolation is the runtime's (see `functions-main/README.md`).
- Per-project function logs and `supabase functions download` of bundled functions (`functions-main/README.md`, "Not done").
- Metrics, per-project concurrency limits and rate limits.
