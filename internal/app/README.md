# internal/app

Composition: the pieces that cannot import each other (`backup` needs the lifecycle `Manager`,
`lifecycle` needs a base backuper, `api` and `proxy` need both) are joined here, and `supavise serve`
is the daemon that `supavise.service` runs.

- `LifecycleOptions(cfg, Options)` is `lifecycle.OpenOptions` with the backup package wired in:
  every cluster archives WAL through `backup.ArchiveCommand` with the configured
  `archive_timeout`, deleting a project takes the final base backup through the backup service
  (`FinalBackup`), and a restore reaches the Engine (`SetManager`). The backup service is built on
  first use, so commands that never back up do not open the backend. Every command that creates or
  deletes projects (`supavise projects`, `system`, `backups`, `serve`) goes through it.
- `NewBackupService` builds the service over the configured backend and the registry.
- `PGMetaCryptoKey` is the passphrase shared by supavise-pgmeta (its `CRYPTO_KEY`) and the Management
  API: `[api] pgmeta_crypto_key`, else the sealed system secret `pgmeta_crypto_key`, created on
  first use. Whoever renders the `supavise-pgmeta` unit (the fleet workstream) must pass exactly it.
- `Serve(ctx, cfg, Options)` is the daemon: it opens the node (registry in the system cluster,
  master key, Engine), runs `Engine.Recover` (statuses a crash left behind), creates the pg-meta
  key, builds the Management API (`api.NewServer`, the Postgres store) and the edge proxy
  (`proxy.New`, `KeySource` = the Engine, no-op `Waker`), listens for the API on the loopback
  admin address and at `api.<domain>` through the proxy, starts the shared services (below) and
  every active project one at a time next to the listeners (the system project's backup timer and
  the prune timer too, on systemd). `ctx` ending (SIGTERM) first drains the API (`api.Server.Drain`: new lifecycle operations
  answer 503, running ones, including creates and a delete's final backup, finish, bounded by
  `StopBudget`, 10 minutes; `supavise.service` allows 660 s to stop). The edge proxy keeps serving
  project, `api.<domain>` and `studio.<domain>` traffic during the drain and stops only after it
  returns (`superviseStop`); then the admin listener and the registry close; project units belong to systemd and keep running. At boot it
  waits up to 2 minutes for the registry (`lifecycle.ErrRegistryUnreachable`), resumes projects
  whose restart was cut off after the pause (`Engine.ResumeRecovered`), and keeps finishing restored
  clones tagged `restore.cleanup_pending` (`backup.Service.FinishPendingRestores`). `StartActive`
  re-reads each project after taking its lock, so an API pause or delete that lands between the
  listing and the start is not undone. The system project must exist (`supavise system init`).

- **Health and alerts.** `Serve` builds one `health.Monitor` over `health.CheckNode` (`health.ForNode` with `InDaemon`, and a `fleet.Lazy` of its own for the tenant checks) and hands it to the Management API (`api.Deps.Health`: `GET /healthz`, `GET /healthz/detail`). It makes an `alerts.Notifier` the package default (`alerts.Notify` for the rest of the node) and runs an `alerts.Checker` next to the listeners: conditions from the health report every minute, the daily update check, and nothing while an upgrade or a maintenance window is on (`internal/health/README.md`, `internal/alerts/README.md`).
- **The shared services.** On a systemd node `Serve` calls `fleet.Setup` with `Start` set next to the
  projects (`startFleet`): it generates the services' sealed secrets on first use, renders the
  units, starts postgres-meta, Supavisor, Realtime, Storage and Studio in order and waits for each,
  so a reboot brings the whole node back from `supavise.service` alone (the units are not enabled for
  boot). The Engine registers, re-keys and removes tenants (Supavisor, Realtime, Storage) on project
  create, rotate-keys and delete through the same `fleet.Setup`, built lazily
  (`cmd/supavise/serve_fleet.go`: a `fleet.Lazy`, bound to the registry and master key right after
  `lifecycle.Open`, `Options.BindFleet`), so the daemon and the CLI register tenants the same way.
  A service whose artifact was never fetched fails at boot and is logged; the rest of the node
  comes up. The installer fetches the artifacts with `supavise fleet start` first.
- **The WAL relay.** With `[backup] wal_relay` on (the default under systemd) `Serve` starts
  `app.StartWALRelay` before it opens the registry and stops it after the lifecycle drain; it serves
  one unix socket per project through which the clusters archive and restore WAL without holding
  any backend credential (`internal/backup/README.md`, "The WAL relay"). The lifecycle engine
  tells it about a project before the cluster starts (`Options.ArchiveReady`). The same function
  serves the sockets nobody answers for a command-line process that waits for WAL
  (`supavise backups ...`, `projects delete`) while the daemon is down.

## Not done

- `Serve` does not supervise: a crashed project unit is systemd's to restart (`Restart=on-failure`)
  and shows as unhealthy through `Health`.

## Test

```
go test ./internal/app/                                          # wiring, pg-meta key
SUPAVISE_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked go test -run Serve -v ./internal/app/
```

The gated test runs the daemon on real artifacts under the exec backend: system init, a project
created and stopped before the daemon starts, the daemon bringing it back at boot, requests
through the proxy (project API, bad key, Management API, dashboard GoTrue) and the admin
listener, and a graceful stop. It starts two PostgreSQL clusters, GoTrue and PostgREST.

```
SUPAVISE_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked SUPAVISE_SETTINGS_E2E=1 \
  go test -run TestSettingsIntegration -v ./internal/app/       # about 25 s, under 1 GB
```

`TestSettingsIntegration` (ports 42000-42999, exec backend) runs the daemon with the system
cluster, one micro project, Supavisor and Storage, saves settings through the Management API
with a PAT and watches the services change: GoTrue's redirect allow list, sign-up toggle, SMTP and
a custom mail template reaching a mail sink; PostgREST serving a newly exposed schema; Storage's
upload limit; immediate refusal of a revoked key and of disabled legacy keys; the database
password (direct and through the pooler); Postgres settings with `ALTER SYSTEM`, and their reset.
With `SUPAVISE_SETTINGS_E2E_HOLD=<file>` it leaves the stack up and writes the connection details
there, for the real Supabase CLI (`supabase --profile=... config push`); delete the file to stop.
