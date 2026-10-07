# internal/app

Composition: the pieces that cannot import each other (`backup` needs the lifecycle `Manager`,
`lifecycle` needs a base backuper, `api` and `proxy` need both) are joined here, and `sbctl serve`
is the daemon that `sbctl.service` runs.

- `LifecycleOptions(cfg, Options)` is `lifecycle.OpenOptions` with the backup package wired in:
  every cluster archives WAL through `backup.ArchiveCommand` with the configured
  `archive_timeout`, deleting a project takes the final base backup through the backup service
  (`FinalBackup`), and a restore reaches the Engine (`SetManager`). The backup service is built on
  first use, so commands that never back up do not open the backend. Every command that creates or
  deletes projects (`sbctl projects`, `system`, `backups`, `serve`) goes through it.
- `NewBackupService` builds the service over the configured backend and the registry.
- `PGMetaCryptoKey` is the passphrase shared by sb-pgmeta (its `CRYPTO_KEY`) and the Management
  API: `[api] pgmeta_crypto_key`, else the sealed system secret `pgmeta_crypto_key`, created on
  first use. Whoever renders the `sb-pgmeta` unit (the fleet workstream) must pass exactly it.
- `Serve(ctx, cfg, Options)` is the daemon: it opens the node (registry in the system cluster,
  master key, Engine), runs `Engine.Recover` (statuses a crash left behind), creates the pg-meta
  key, builds the Management API (`api.NewServer`, the Postgres store) and the edge proxy
  (`proxy.New`, `KeySource` = the Engine, no-op `Waker`), listens for the API on the loopback
  admin address and at `api.<domain>` through the proxy, starts the shared services (below) and
  every active project one at a time next to the listeners (the system project's backup timer and
  the prune timer too, on systemd). `ctx` ending (SIGTERM) first drains the API (`api.Server.Drain`: new lifecycle operations
  answer 503, running ones, including creates and a delete's final backup, finish, bounded by
  `StopBudget`, 10 minutes; `sbctl.service` allows 660 s to stop). The edge proxy keeps serving
  project, `api.<domain>` and `studio.<domain>` traffic during the drain and stops only after it
  returns (`superviseStop`); then the admin listener and the registry close; project units belong to systemd and keep running. At boot it
  waits up to 2 minutes for the registry (`lifecycle.ErrRegistryUnreachable`), resumes projects
  whose restart was cut off after the pause (`Engine.ResumeRecovered`), and keeps finishing restored
  clones tagged `restore.cleanup_pending` (`backup.Service.FinishPendingRestores`). `StartActive`
  re-reads each project after taking its lock, so an API pause or delete that lands between the
  listing and the start is not undone. The system project must exist (`sbctl system init`).

- **The shared services.** On a systemd node `Serve` calls `fleet.Setup` with `Start` set next to the
  projects (`startFleet`): it generates the services' sealed secrets on first use, renders the
  units, starts postgres-meta, Supavisor, Realtime, Storage and Studio in order and waits for each,
  so a reboot brings the whole node back from `sbctl.service` alone (the units are not enabled for
  boot). The Engine registers, re-keys and removes tenants (Supavisor, Realtime, Storage) on project
  create, rotate-keys and delete through the same `fleet.Setup`, built lazily
  (`cmd/sbctl/serve_fleet.go`: a `fleet.Lazy`, bound to the registry and master key right after
  `lifecycle.Open`, `Options.BindFleet`), so the daemon and the CLI register tenants the same way.
  A service whose artifact was never fetched fails at boot and is logged; the rest of the node
  comes up. The installer fetches the artifacts with `sbctl fleet start` first.
- **The WAL relay.** With `[backup] wal_relay` on (the default under systemd) `Serve` starts
  `app.StartWALRelay` before it opens the registry and stops it after the lifecycle drain; it serves
  one unix socket per project through which the clusters archive and restore WAL without holding
  any backend credential (`internal/backup/README.md`, "The WAL relay"). The lifecycle engine
  tells it about a project before the cluster starts (`Options.ArchiveReady`). The same function
  serves the sockets nobody answers for a command-line process that waits for WAL
  (`sbctl backups ...`, `projects delete`) while the daemon is down.

## Not done

- `Serve` does not supervise: a crashed project unit is systemd's to restart (`Restart=on-failure`)
  and shows as unhealthy through `Health`.

## Test

```
go test ./internal/app/                                          # wiring, pg-meta key
SBCTL_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked go test -run Serve -v ./internal/app/
```

The gated test runs the daemon on real artifacts under the exec backend: system init, a project
created and stopped before the daemon starts, the daemon bringing it back at boot, requests
through the proxy (project API, bad key, Management API, dashboard GoTrue) and the admin
listener, and a graceful stop. It starts two PostgreSQL clusters, GoTrue and PostgREST.
