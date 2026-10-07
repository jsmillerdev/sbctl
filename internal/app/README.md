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
  admin address and at `api.<domain>` through the proxy, and starts every active project one at a
  time next to the listeners (the system project's backup timer and the prune timer too, on
  systemd). `ctx` ending (SIGTERM) first drains the API (`api.Server.Drain`: new lifecycle operations
  answer 503, running ones, including creates and a delete's final backup, finish, bounded by
  `StopBudget`, 10 minutes; `sbctl.service` allows 660 s to stop). The edge proxy keeps serving
  project, `api.<domain>` and `studio.<domain>` traffic during the drain and stops only after it
  returns (`superviseStop`); then the admin listener and the registry close; project units belong to systemd and keep running. At boot it
  waits up to 2 minutes for the registry (`lifecycle.ErrRegistryUnreachable`), resumes projects
  whose restart was cut off after the pause (`Engine.ResumeRecovered`), and keeps finishing restored
  clones tagged `restore.cleanup_pending` (`backup.Service.FinishPendingRestores`). The system project must exist (`sbctl system init`).

## Not done

- The fleet (`Options.Fleet`) is an empty `fleet.Fleet` until `cmd/sbctl/serve_fleet.go` calls
  the fleet workstream's `fleet.Setup`; nothing here starts Supavisor, Realtime, Storage,
  postgres-meta or Studio.
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

```
SBCTL_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked SBCTL_SETTINGS_E2E=1 \
  go test -run TestSettingsIntegration -v ./internal/app/       # about 25 s, under 1 GB
```

`TestSettingsIntegration` (ports 42000-42999, exec backend) runs the daemon with the system
cluster, one micro project, Supavisor and Storage, saves settings through the Management API
with a PAT and watches the services change: GoTrue's redirect allow list, sign-up toggle, SMTP and
a custom mail template reaching a mail sink; PostgREST serving a newly exposed schema; Storage's
upload limit; immediate refusal of a revoked key and of disabled legacy keys; the database
password (direct and through the pooler); Postgres settings with `ALTER SYSTEM`, and their reset.
With `SBCTL_SETTINGS_E2E_HOLD=<file>` it leaves the stack up and writes the connection details
there, for the real Supabase CLI (`supabase --profile=... config push`); delete the file to stop.
