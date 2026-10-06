# internal/backup

WAL archiving, base backups, retention and point-in-time restore for project clusters. `sbctl wal push` is every cluster's `archive_command`, `sbctl wal fetch` is its `restore_command`, and a systemd timer runs `sbctl backups create` and `sbctl backups prune` per project. No wal-g, pgBackRest or `pg_basebackup`.

## Layout in the backend

```
<prefix>/<ref>/wal/<segment>.zst               one object per archived WAL file (also .history, .partial, .backup)
<prefix>/<ref>/base/<id>/data.tar.zst          the data directory as one tar stream
<prefix>/<ref>/base/<id>/secrets.json          the project's sealed secrets at backup time
<prefix>/<ref>/base/<id>/backup.json           manifest, written last: its presence means "complete"
```

`<id>` is the backup start time in UTC plus a random suffix (`20261006T030000Z-3f9a1c`), so two backups that start in the same second never share a directory. The manifest is the source of truth for restore and prune. The registry `backups` table is an index over it (no new tables, so there is no `04xx` migration). Backups outlive their project: a deleted project's final backup stays restorable.

Backends (`Store` interface in `store.go`): `file://` (temporary file, fsync, rename, fsync of the directory) and `s3://bucket/prefix` (aws-sdk-go-v2; `s3_endpoint` and `s3_force_path_style` for MinIO, R2, Backblaze, Hetzner; static keys or the default AWS credential chain; multipart upload that aborts on failure).

S3 permissions the credentials need (for the IAM policy that workstream G writes): `s3:ListBucket` on the bucket (limited to the prefix with an `s3:prefix` condition if wanted) and `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:AbortMultipartUpload` on `<bucket>/<prefix>/*`. `s3:ListBucket` is not optional: without it S3 answers 403 instead of 404 for a key that does not exist, so every first `wal push` and the end of every `wal fetch` fail. The store adds that explanation to the error when it sees a 403.

## The archive_command contract

| Case | Behavior |
|---|---|
| Push succeeds | exit 0 only after the compressed object is durable in the backend |
| Identical re-push | exit 0, nothing rewritten |
| Same name, different content | exit 1, archive untouched (`ErrWALConflict`) |
| Fetch of a missing file | exit 1 within milliseconds (Postgres reads it as "end of archive") |
| Fetch when the archive cannot be read (outage, corrupt object, bad config) | exit 126, which makes Postgres abort recovery instead of promoting early (`WALExitFatal`) |
| Names | segments, `.partial`, `.backup` and `.history` files; anything else is refused |

`wal push` and `wal fetch` load the config and open only the backend. They never open the registry or a database connection, so each call starts in a few milliseconds plus the first storage round trip.

## Base backup method

Chosen: `pg_backup_start()` / `pg_backup_stop()` on a `supabase_admin` connection plus a direct copy of the data directory into a streaming tar.zst upload. Reasons:

- The Supabase Postgres artifact has no `pg_basebackup`, and ships `max_wal_senders = 0` with no replication HBA entry. The replication-protocol route (`BASE_BACKUP` through pgconn) would mean reconfiguring and restarting every cluster, and implementing the tar framing of the protocol ourselves.
- `sbctl` always runs on the same host as the cluster it backs up (design section 3), so reading files directly needs no network path and no extra role.
- It is the same procedure PostgreSQL documents for file-system-level backups (PostgreSQL 15 or later), and the copy excludes exactly what `pg_basebackup` excludes (`basebackup.c`: `pg_wal`, `pg_replslot`, temporary files, `postmaster.pid`, and so on).
- `pg_backup_stop(true)` waits until the WAL covering the backup is archived, so a backup that completes is restorable.

Refused with a clear error: clusters without `archive_mode`/`archive_command`, with `full_page_writes = off`, in recovery, with tablespaces or stray symlinks, or in the middle of the artifact's first-boot initialization. The registry row records timeline, start and stop LSN and stored size; the manifest adds the start and stop WAL file names and times.

## Restore

`sbctl backups restore <ref> --to <RFC3339 time | latest | backup> [--as <newref>] [--force] [--backup-id <id>]`

| `--to` | Recovery target | Needs |
|---|---|---|
| an RFC3339 time | `recovery_target_time`, then promote | a transaction that committed after that time in the archive (see below) |
| `latest` | no target: replay every archived file, then promote | nothing. Use it to restore a deleted project's last state from its final backup, or "now" on a running project: the source first switches WAL and waits for archiving, so its newest transactions are included |
| `backup` | `recovery_target = 'immediate'`: exactly the state at the end of the chosen base backup (`--backup-id`, default the newest) | nothing beyond the backup's own WAL |

PostgreSQL 13 and later stop at a time target only on a commit or abort record after it. If the archive has none, recovery ends with "recovery ended before configured recovery target was reached" and the restore fails, deliberately: it never silently restores a different time than asked. A target later than the newest commit in the archive (for example "now", before `archive_timeout` has archived the newest WAL, or any time after a deleted project's final backup) therefore needs `--to latest`.

1. Choose the newest base backup that finished at or before `--to` (or `--backup-id`) and lies on the archive's timeline history: the newest `*.history` file lists where each timeline forked, and a backup of timeline 1 taken after an in-place restore forked timeline 2 is skipped, because PostgreSQL would refuse to follow timeline 2 from it. Check that the WAL it starts from is archived.
2. A `lifecycle.DataSeeder` (`Service.Seeder`) unpacks the base backup into the new data directory, recreates `postmaster.opts` (the Supabase launcher refuses a data directory without it), appends to `postgresql.auto.conf`: `restore_command = '<bin> wal fetch --ref <source> %f %p'`, the recovery target for the mode above, `recovery_target_action = 'promote'` (with a target), `recovery_target_timeline = 'latest'`, and `archive_mode`/`archive_command` for the target ref, then writes `recovery.signal` last.
3. `lifecycle.Manager.Create` (or `Resume` for in place) starts the cluster. It may return while the cluster is still replaying WAL (hot_standby accepts connections during recovery); restore does not assume otherwise.
4. Restore polls `pg_is_in_recovery()` (up to `Options.RecoveryTimeout`, default 30 minutes) and only after promotion runs `ALTER SYSTEM RESET` for the recovery settings, so the cluster never replays another archive. Resetting earlier would be fatal: `restore_command` is reloadable, recovery would run out of WAL before its target, and the cluster would refuse to start again. If recovery has not finished in time the settings stay, a `restore.cleanup_pending` event is recorded, and `sbctl backups finish-restore <ref>` finishes the job later. The `archive_*` lines stay in `postgresql.auto.conf` on purpose: the data directory's `postgresql.conf` is the source's, and without them the clone would archive into the source's archive.

New project (`--as`): database role passwords and the pgsodium root key are the source's, because the restored cluster contains them. Everything a client can hold is new: JWT secret, legacy `anon` and `service_role` keys re-signed for the new ref, new `sb_publishable_` and `sb_secret_` keys. The clone archives to its own ref from its first new-timeline file on, never into the source's archive. `<newref>` must have no data in the archive: a deleted project's backups are kept, and reusing its ref would collide with its WAL (`archive_command` would fail on every segment and `pg_wal` would fill the disk), so the restore refuses. The clone has no base backup of its own until the first scheduled or manual one.

In place (`--force`): `Manager.Pause`, move the data directory to `<dir>.pre-restore-<time>`, seed, `Manager.Resume`, wait for promotion, then a base backup with reason `post-restore`, so the new timeline has a base at once (if that backup fails, a `restore.backup_pending` event is recorded and the restore still counts). A failed seed puts the original back. The old directory is never deleted automatically.

The `system` cluster cannot be restored with this command: it holds the registry that `restore` reads and writes.

Run restores as the `sbctl` user. If run as root, the seeder gives the data directory the owner of its parent.

### Disaster recovery of the system cluster

Not automated and not tested end to end. With the control plane stopped and the system cluster archived under ref `system`: unpack `<prefix>/system/base/<id>/data.tar.zst` (`zstd -dc | tar -x`) into an empty directory (mode 0700, owned by the user that runs Postgres), add `restore_command = 'sbctl wal fetch --ref system %f %p'`, optionally a recovery target, `recovery_target_action = 'promote'`, create `recovery.signal`, and start Postgres. List the base backups by looking at the `backup.json` manifests in that directory. `sbctl backups` needs a reachable registry, so it cannot be used for this.

## Retention

`backup.retention_days` (config, default 7; 0 disables pruning). `Prune` keeps every base backup that finished inside the window plus the newest one before it (so the window's oldest instant is restorable) and always at least one. It deletes WAL older than the oldest kept backup's start segment (timeline-aware; `.history` files are kept), abandoned uploads with no manifest older than 24 hours, and the matching registry rows. With no base backup it deletes nothing. `backups prune` without a ref also covers refs that only exist in the backend (deleted projects).

## What lifecycle (workstream D) calls

| Need | API |
|---|---|
| `archive_mode`, `archive_command`, `archive_timeout` for every cluster's `postgresql.conf` | `backup.ArchiveSettings(cfg, ref, configPath)` (`wal_level` must be `replica` or `logical`; the artifact's `logical` is fine) |
| Delete-time final backup (call while the cluster still runs; abort the delete on error) | `(*backup.Service).FinalBackup(ctx, ref)`, same shape as `lifecycle.DataPlane.Snapshot` |
| Nightly timer | `backup.RenderBackupService(binPath)`, `backup.RenderBackupTimer(cfg.Backup.BaseBackupOnCalendar)`, then `systemctl enable --now sb-basebackup@<ref>.timer` (`backup.BackupTimerInstance`) |
| Restore (`backups restore`) | wire a `lifecycle.Manager` into `newLifecycleManager` in `cmd/sbctl/cmd_backups.go` |
| Database access without a Manager | `backup.AccessFromRegistry`; a `lifecycle.Manager` also satisfies `backup.Access` |

`deploy/systemd/sb-basebackup@.service` and `.timer` are the default renderings; `TestDeployUnits` keeps them in sync (`UPDATE_DEPLOY=1 go test ./internal/backup -run TestDeployUnits` regenerates). The service reads the optional `/etc/sbctl/sbctl.env`, which must be mode 0600 and owned by the `sbctl` user because it holds the registry password (`sbctl backups` warns on stderr when it is group- or world-readable; whoever writes the file, the installer or lifecycle, sets the mode). `SBCTL_REGISTRY_DSN` goes there: `sbctl backups` runs outside the daemon and finds the registry through that variable (`openRegistry` in `cmd/sbctl/cmd_backups.go` is the replaceable seam).

## Testing

```
go test ./internal/backup/ ./cmd/sbctl/        # everything below that can run locally
```

| Test | What it covers | Needs |
|---|---|---|
| `store_test.go`, `wal_test.go`, `retention_test.go`, `restore_test.go`, `archivecmd_test.go` | store contract, WAL push and fetch contract, retention and prune, tar safety, restore planning, seeder, new-project keys, in-place rollback | nothing |
| `cli_test.go` | exit statuses of the real `sbctl wal push` and `wal fetch` binary | `go build` |
| `integration_test.go` `TestPointInTimeRestore` | real Postgres from the artifact, archive and restore commands are the built `sbctl`: table, row 1, base backup, row 2, time T, drop table, restore to T as a new project and in place (the manager returns before promotion), the post-restore base backup on timeline 2, then `--to latest` of the running source | Postgres artifact (see below) |
| `restore_modes_test.go` | timeline-aware base backup choice, `latest`/`backup` targets, recovery settings kept until promotion, leftover-archive and `system` refusals | nothing |
| `store_s3_test.go`, `TestPointInTimeRestoreS3Fake` | the S3 store and the whole scenario over S3 against an in-process fake S3 (gofakes3) | Postgres artifact for the second |
| `TestS3StoreAgainstRealService`, `TestPointInTimeRestoreS3` | the same against a real S3-compatible service | `SBCTL_TEST_S3_ENDPOINT`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY` (optional `_REGION`); CI only |

The Postgres tests use `SBCTL_TEST_PG_BIN` (the artifact's `bin/` directory) or the unpacked darwin or linux artifact under `~/.cache/sbctl/unpacked/`, and skip without one, with `-short`, or with `SBCTL_TEST_PG=0`. They run two clusters at a time at most (`shared_buffers=16MB`, `max_connections=30`, TCP ports from 35000-35999, no unix sockets), stopped in `t.Cleanup`. `ci-integration.sh` (refuses to run as root, because `initdb` does) downloads the Linux artifact pinned in `versions.yaml`, verifies it against the release's `SHA256SUMS` and runs them with the S3 variables for a MinIO service container.

## Not done

- Tablespaces are refused, not backed up. Supabase projects do not use them.
- No incremental or differential base backups and no parallel file copy: each backup is a full compressed copy of the data directory, read sequentially.
- No restore from a point before the oldest retained backup, by design (see retention).
- Passwords in `secrets.json` are sealed with the node's master key, so restoring on another node needs the same `master.key`.
- The clone from a restore starts with no base backup of its own (see above). Only an in-place restore takes one immediately.
- Nothing stops project creation (workstream D) from reusing the ref of a deleted project whose archive is still in the backend; only `backups restore --as` checks. D should apply the same check, or give each project incarnation its own archive prefix.
- `systemd-analyze verify` has not been run on the unit files (no systemd on the dev Mac).
- Registry lookup for `sbctl backups` is the `SBCTL_REGISTRY_DSN` placeholder until the integration step decides how the CLI finds the system cluster.
