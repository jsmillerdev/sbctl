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

`config.toml` holds `backup.s3_secret_access_key` when static S3 keys are used, so it must be mode 0600 and owned by the `sbctl` user (`sbctl backups` warns otherwise); with the AWS default credential chain (instance role) no secret is stored.

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

## The WAL relay

On a systemd node the clusters do not open the backend themselves. A project's Postgres runs user code, and the backend credentials (an S3 key in `config.toml`, or the EC2 instance role) and every other project's archive must stay out of its reach. So `archive_command` and `restore_command` of every cluster are `sbctl wal push --ref <ref> --socket <state>/projects/<ref>/wal/r.sock %p` and `sbctl wal fetch --ref <source> --socket <that same socket> %f %p` (`backup.ArchiveCommandRelay`, `RestoreCommandRelay`; no `--config`, the unit cannot read it), and the daemon does the storage I/O (`relay.go`, `backup.Relay`):

- One unix socket per project in `projects/<ref>/wal/`. The cluster's systemd unit bind-mounts only that directory, read-only (`deploy/systemd/README.md`), so the mount namespace decides which cluster reaches which relay. The socket belongs to one project: a push through it for another ref is a 403, and so is a fetch of another ref's archive, unless that ref is named in `projects/<ref>/restore-sources` (the file the seeder writes for a restore to a new project; the clone's `restore_command` reads the source's WAL, then recovery removes the file). The file is in the project directory, outside every directory the project's units see.
- **Who may connect.** Units share one uid, so the mount namespace does not stop a process that opens another project's socket through `/proc/<pid>/root`. The relay checks every connection's peer (`SO_PEERCRED` pid, then the systemd unit in `/proc/<pid>/cgroup`; `relay_peer.go`, `RelayOptions.PeerCheck`): `sb-postgres@<ref>.service` for the socket of `<ref>`, the `sb-basebackup` units and processes outside every `sb-*` unit are served; any other `sb-*` unit gets a closed connection, and so does a peer whose cgroup cannot be read. Only Linux checks; other platforms have no such units.
- **One relay per socket.** A relay never replaces a socket that answers (it pings first), and never unlinks a socket that is no longer its own (`SetUnlinkOnClose(false)`; shutdown removes the path only when it still names the relay's inode). The daemon and a CLI relay can therefore run at once; whichever listened first keeps the project until it stops, and the other takes over at its next reconcile (three seconds for the daemon, one for a CLI).
- The protocol is HTTP over the socket: `POST /v1/wal/push?name=&ref=` with the file as the body (the daemon compresses and stores it, and answers 204 only when the object is durable in the backend: `Service.PushWALReader`, the code behind `sbctl wal push`), `GET /v1/wal/fetch?ref=&name=` (the decompressed file; 404 when the archive does not hold it, which the client turns into exit 1; any other failure makes the client exit 126, as does a daemon that does not answer). A body shorter than its Content-Length fails the push and leaves nothing in the archive. `PushWALReader` returns only after nothing reads the request body any more (the relay interrupts a read that is stuck on a stalled client), so the handler can drain the rest of the body without racing the compressor.
- **The daemon is a dependency of archiving.** With it down the socket does not answer, `archive_command` fails and Postgres retries it (the segments stay in `pg_wal`; nothing is buffered elsewhere, on purpose), and a recovery that needs `restore_command` stops with a fatal error instead of promoting early. `sbctl.service` restarts always; a node whose daemon stays down for long fills `pg_wal`, as it would with an unreachable bucket. A command-line process that waits for archived WAL while the daemon is down (`sbctl backups create|restore|finish-restore`, the nightly timer's service, `sbctl projects delete` with its final base backup) serves the sockets nobody answers for as long as it runs (`app.StartWALRelay`; it serves only the projects the command works on, `RelayOptions.Only`, and read-only commands such as `backups list` and `prune` start none), so those still complete.
- The daemon starts the relay before it opens the registry (the system cluster archives too) and stops it after the lifecycle drain, because a delete's final base backup needs WAL archived until the end. It picks up projects within three seconds and at once through the lifecycle engine's hook (`PlaneOptions.ArchiveReady`).
- `[backup] wal_relay` is `auto` (the default: on under systemd, off under the exec backend of development and tests), `on` or `off`. With `off` the commands are the direct form above and read the config, as before. Config validation refuses `off` under the systemd supervisor: the Postgres unit hides `/etc/sbctl` and the backups directory, so a direct push could never succeed and WAL would pile up in `pg_wal`.

The direct form is also what the nightly timer, `sbctl backups` and the daemon use to read the archive: they run outside any unit's sandbox.

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

PostgreSQL 13 and later stop at a time target only on a commit or abort record after it. If the archive has none, recovery ends with "recovery ended before configured recovery target was reached", PostgreSQL shuts down, and the restore reports an error (see step 4): it never silently restores a different time than asked. For a running source, restore first writes a commit record on the source (it forces an xid and commits it), switches WAL and waits until the newest segment is archived (`Options.ArchiveFlushTimeout`, 60 seconds), for time targets as well as for `latest`. A commit that is still in the current segment, often the very `DROP` being undone, then counts, and so does any target before "now" on a project that has been idle since: the restore's own commit record follows it. A target after the end of the archive (the future, or any time after a deleted project's final backup, whose source is gone) cannot be reached and needs `--to latest`.

1. Choose the newest base backup that finished at or before `--to` (or `--backup-id`) and lies on the archive's timeline history: the newest `*.history` file lists where each timeline forked, and a backup of timeline 1 taken after an in-place restore forked timeline 2 is skipped, because PostgreSQL would refuse to follow timeline 2 from it. Check that the WAL it starts from is archived.
2. A `lifecycle.DataSeeder` (`Service.Seeder`) unpacks the base backup into the new data directory, recreates `postmaster.opts` (the Supabase launcher refuses a data directory without it), appends to `postgresql.auto.conf`: `restore_command = '<bin> wal fetch --ref <source> %f %p'`, the recovery target for the mode above, `recovery_target_action = 'promote'` (with a target), `recovery_target_timeline = 'latest'`, and `archive_mode`/`archive_command` for the target ref, then writes `recovery.signal` last.
3. `lifecycle.Manager.Create` (or `Resume` for in place) starts the cluster. It may return while the cluster is still replaying WAL (hot_standby accepts connections during recovery); restore does not assume otherwise.
4. Restore polls `pg_is_in_recovery()` (up to `Options.RecoveryTimeout`, default 30 minutes) and only after promotion runs `ALTER SYSTEM RESET` for the recovery settings, so the cluster never replays another archive. Resetting earlier would be fatal: `restore_command` is reloadable, recovery would run out of WAL before its target, and the cluster would refuse to start again. If recovery is still replaying when the time is up, the settings stay, a `restore.cleanup_pending` event is recorded, and `sbctl backups finish-restore <ref>` finishes the job later. A restore passes on that `Manager.Create` or `Resume` already saw the cluster accept connections, so a cluster that is unreachable from the first probe on, for `Options.RecoveryFailGrace` (15 seconds), has failed: restore does not wait out the 30 minutes (an in-place restore keeps the production project down until it decides). Under systemd the dead cluster is restarted (`Restart=on-failure`), answers again in recovery for a moment and dies again, which would restart the grace period every time, so the number of times it went from reachable to unreachable is counted too (`Options.RecoveryMaxOutages`, 3): that is a failed recovery as well, never `restore.cleanup_pending`. A cluster that is down when the wait ends is a failure too. Recovery has failed in all these cases: the restore returns an error, records `restore.failed` and no `restore.completed`. A new project stays registered in that case so that its postgres log can be read; it holds nothing to back up, so `sbctl projects delete` removes it without a final backup (see "What lifecycle calls": `FinalBackup` answers `lifecycle.ErrNoRestorableState` for a clone whose recovery failed or never finished and that has no completed base backup, and the delete goes on; a later `restore.recovery_finished` event, written when recovery completes or `finish-restore` finishes it, supersedes that). The events do not decide alone: a clone tagged `restore.cleanup_pending` promotes by itself later (`recovery_target_action = 'promote'`), so `FinalBackup` and every base backup ask the live cluster first; one that has left recovery gets its recovery settings cleared, `restore.recovery_finished` recorded and its backup taken. The daemon also runs `FinishPendingRestores` after boot, repeating while any clone is pending, so nobody has to run `finish-restore` by hand. An in-place restore puts the original data back and keeps the failed one as `<dir>.failed-restore-<time>`. The `archive_*` lines stay in `postgresql.auto.conf` on purpose: the data directory's `postgresql.conf` is the source's, and without them the clone would archive into the source's archive.

New project (`--as`): database role passwords and the pgsodium root key are the source's, because the restored cluster contains them. Everything a client can hold is new: JWT secret, legacy `anon` and `service_role` keys re-signed for the new ref, new `sb_publishable_` and `sb_secret_` keys. The clone archives to its own ref from its first new-timeline file on, never into the source's archive. `<newref>` must have no data in the archive: a deleted project's backups are kept, and reusing its ref would collide with its WAL (`archive_command` would fail on every segment and `pg_wal` would fill the disk), so the restore refuses. The clone has no base backup of its own until the first scheduled or manual one.

In place (`--force`): check that the data directory holds `PG_VERSION` and, when the cluster is reachable, is the directory the server reports as `data_directory` (otherwise refuse before stopping anything), `Manager.Pause`, move the data directory to `<dir>.pre-restore-<time>`, seed, `Manager.Resume`, wait for promotion, then a base backup with reason `post-restore`, so the new timeline has a base at once (if that backup fails, a `restore.backup_pending` event is recorded and the restore still counts). A failed seed, a failed start (`Manager.Resume` returns an error: the restored cluster died before the readiness check saw it, which is how the fatal "recovery ended before configured recovery target was reached" usually surfaces) or a failed recovery puts the original back the same way: the restored directory moves to `<dir>.failed-restore-<time>`, the original returns and the project resumes on it. If the rollback itself fails, the error names both directories. The old directory is never deleted automatically.

Limitation: an in-place restore brings back the database role passwords the backup holds, while the registry, GoTrue and PostgREST keep the current ones. They differ only if a database password was reset after the backup (`RotateKeys` does not touch them); restore then logs a warning and records a `restore.password_drift` event, and the password has to be reset by hand. A restore to a new project is not affected: it reuses the backup's passwords everywhere.

The `system` cluster cannot be restored with this command: it holds the registry that `restore` reads and writes.

Run `sbctl backups` and `sbctl wal` as the `sbctl` user (`sudo -u sbctl sbctl backups ...`). As root they refuse to run: the file backend and the restored data directory would be created root-owned, which the timer (`User=sbctl`) could no longer read or prune and which Postgres refuses to open.

### Disaster recovery of the system cluster

Not automated and not tested end to end. With the control plane stopped and the system cluster archived under ref `system`: unpack `<prefix>/system/base/<id>/data.tar.zst` (`zstd -dc | tar -x`) into an empty directory (mode 0700, owned by the user that runs Postgres), add `restore_command = 'sbctl wal fetch --ref system %f %p'`, optionally a recovery target, `recovery_target_action = 'promote'`, create `recovery.signal`, and start Postgres. List the base backups by looking at the `backup.json` manifests in that directory. `sbctl backups` needs a reachable registry, so it cannot be used for this.

## Retention

`backup.retention_days` (config, default 7; 0 disables pruning). `Prune` keeps every base backup that finished inside the window plus the newest one before it (so the window's oldest instant is restorable) and always at least one. The anchor must be a backup that restore could use: after an in-place restore forked a new timeline, a backup of the old timeline taken after the fork is off the history and never becomes the anchor, so the older usable one (and its WAL) is kept. Prune deletes WAL older than the oldest kept backup's start segment (timeline-aware; `.history` files are kept), abandoned uploads with no manifest older than 24 hours, leftover `*.tmp-*` files of crashed file-store writes older than that, and the matching registry rows. With no base backup it deletes nothing. `backups prune` without a ref also covers refs that only exist in the backend (deleted projects).

The per-project timer runs `backups prune <ref>` after each backup. A deleted project's timer is gone, so a node-wide pair, `sb-basebackup-prune.service` and `.timer` (daily at 05:00), runs `sbctl backups prune` without a ref; enable it once per node (`systemctl enable --now sb-basebackup-prune.timer`). On S3, add a bucket lifecycle rule that aborts incomplete multipart uploads after a day or two: a crashed upload leaves parts that no `List` shows and that cost storage.

`DeleteObjects` is sent with the SDK's CRC32 checksum, which AWS accepts. Some S3-compatible services insist on `Content-MD5` for it and answer `MissingContentMD5` or `InvalidRequest`; the store then repeats the request with `Content-MD5` and keeps doing so. Verified against a stub server that demands the header and against gofakes3 (neither is a real provider); MinIO runs in CI. Not verified against Ceph RGW, Backblaze, R2 or Hetzner.

## What lifecycle (workstream D) calls

| Need | API |
|---|---|
| `archive_mode`, `archive_command`, `archive_timeout` for every cluster | `backup.ArchiveCommandFor(cfg, ref, configPath)` (the relay form on a systemd node, `backup.ArchiveCommand` otherwise) and `cfg.Backup.ArchiveTimeoutSeconds`; `internal/app` hands them to lifecycle (`PlaneOptions.ArchiveCommandFor`), which renders them as `-c` arguments (`backup.ArchiveSettings` is the same thing as `postgresql.conf` lines). `wal_level` must be `replica` or `logical`; the artifact's `logical` is fine |
| Delete-time final backup (call while the cluster still runs; abort the delete on error) | `(*backup.Service).FinalBackup(ctx, ref)`, found by the Engine through `lifecycle.FinalBackuper`. It returns an error wrapping `lifecycle.ErrNoRestorableState` for a clone that never recovered; `Engine.DeleteWith` then deletes without a backup and records `project.final_backup_skipped` |
| Nightly timer | `backup.RenderBackupService(binPath)`, `backup.RenderBackupTimer(cfg.Backup.BaseBackupOnCalendar)` (an empty or control-character-bearing calendar falls back to the default; `RenderBackupTimerChecked` and `ValidateOnCalendar` report it; `RenderPruneService` and `RenderPruneTimer` for the node-wide prune pair), then start `sb-basebackup@<ref>.timer` (`backup.BackupTimerInstance`). The Engine starts it through the supervisor (D-Bus `StartUnit`, which the polkit rule allows for `sb-*` units) when a project becomes active, stops it on pause and delete, and `sbctl serve` starts the system project's timer and the node-wide prune timer at boot; the timers are not enabled for boot, the daemon is what brings them back |
| Restore (`backups restore`) | `(*backup.Service).SetManager(engine)`: the Engine needs the service as its base backuper and the service needs the Engine, so one is built first and handed to the other (`internal/app`; `lifecycle.OpenOptions.BackupFactory` does it for the daemon, `openBackupService` in `cmd/sbctl/cmd_backups.go` for the CLI) |
| Database access without a Manager | `backup.AccessFromRegistry`; a `lifecycle.Manager` also satisfies `backup.Access` |

`deploy/systemd/sb-basebackup@.service` and `.timer` (plus the node-wide `sb-basebackup-prune.*` pair) are the default renderings; `TestDeployUnits` keeps them in sync (`UPDATE_DEPLOY=1 go test ./internal/backup -run TestDeployUnits` regenerates). `sbctl backups` runs outside the daemon and finds the registry through `lifecycle.RegistryDSN`: the system cluster's private unix socket as `supabase_admin`, which needs no password and no environment file, and works for the `sbctl` user the timers run as. `SBCTL_REGISTRY_DSN` still overrides it (the service reads an optional `/etc/sbctl/sbctl.env` that may hold it; if the file holds a password it must be mode 0600 and owned by the `sbctl` user, and `sbctl backups` warns on stderr when it is group- or world-readable). The file backend creates objects 0600 and directories 0700: base backups carry `pg_authid` (SCRAM verifiers) and Vault ciphertext, and only the `sbctl` user reads them.

## Testing

```
go test ./internal/backup/ ./cmd/sbctl/        # everything below that can run locally
```

| Test | What it covers | Needs |
|---|---|---|
| `store_test.go`, `wal_test.go`, `retention_test.go`, `restore_test.go`, `archivecmd_test.go` | store contract, WAL push and fetch contract, retention and prune, tar safety, restore planning, seeder, new-project keys, in-place rollback | nothing |
| `cli_test.go` | exit statuses of the real `sbctl wal push` and `wal fetch` binary | `go build` |
| `integration_test.go` `TestPointInTimeRestore` | real Postgres from the artifact, archive and restore commands are the built `sbctl`: table, row 1, base backup, row 2, time T, drop table, restore to T as a new project and in place (the manager returns before promotion), the post-restore base backup on timeline 2, then `--to latest` of the running source | Postgres artifact (see below) |
| `restore_modes_test.go` | failed recovery is an error (new project and in place, with rollback), data directory check, timeline-aware base backup choice, `latest`/`backup` targets, recovery settings kept until promotion, leftover-archive and `system` refusals | nothing |
| `TestRestoreInPlaceRollsBackWhenRecoveryCannotReachItsTarget` | an in-place restore to a time no commit reaches: PostgreSQL ends recovery with a fatal error, the original data directory is put back, the project runs on it, the failed attempt is kept in `.failed-restore-*` and the original still backs up | Postgres artifact |
| `TestRestoreRunningSourceToRecentTime` | a running source restored to a time whose next commit is still unarchived (the restore must archive it first); a target after the last commit must fail with an error | Postgres artifact |
| `store_s3_test.go`, `TestPointInTimeRestoreS3Fake` | the S3 store and the whole scenario over S3 against an in-process fake S3 (gofakes3) | Postgres artifact for the second |
| `TestS3StoreAgainstRealService`, `TestPointInTimeRestoreS3` | the same against a real S3-compatible service | `SBCTL_TEST_S3_ENDPOINT`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY` (optional `_REGION`); CI only |

The Postgres tests use `SBCTL_TEST_PG_BIN` (the artifact's `bin/` directory) or the unpacked darwin or linux artifact under `~/.cache/sbctl/unpacked/`, and skip without one, with `-short`, or with `SBCTL_TEST_PG=0`. They run two clusters at a time at most (`shared_buffers=16MB`, `max_connections=30`, TCP ports from 35000-35999, no unix sockets), stopped in `t.Cleanup`. `ci-integration.sh` (refuses to run as root, because `initdb` does) downloads the Linux artifact pinned in `versions.yaml`, verifies it against the release's `SHA256SUMS` and runs them with the S3 variables for an S3 service container (Garage in CI: MinIO no longer publishes images or binaries, and SeaweedFS lists empty "directories" as prefixes). `S3Store.Delete` treats a per-key `NoSuchKey` as deleted, as AWS does by answering success.

## Not done

- Tablespaces are refused, not backed up. Supabase projects do not use them.
- No incremental or differential base backups and no parallel file copy: each backup is a full compressed copy of the data directory, read sequentially.
- No restore from a point before the oldest retained backup, by design (see retention).
- Passwords in `secrets.json` are sealed with the node's master key, so restoring on another node needs the same `master.key`.
- The clone from a restore starts with no base backup of its own (see above). Only an in-place restore takes one immediately.
- Nothing stops project creation (workstream D) from reusing the ref of a deleted project whose archive is still in the backend; only `backups restore --as` checks. D should apply the same check, or give each project incarnation its own archive prefix.
- Abandoned S3 multipart uploads are not listed or aborted by prune; use a bucket lifecycle rule.
- The unit files are exercised by the systemd smoke test in CI (the `linux` workflow), not by `systemd-analyze verify`.
