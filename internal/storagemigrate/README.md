# internal/storagemigrate

Moves Storage's objects between its file backend and an S3 bucket: `supavise storage migrate --to s3` (design 2.11). The copy runs while Storage serves from the files. The switch holds Storage's writes, stops `supavise-storage`, copies once more from files that nothing writes to, points the configuration at the bucket and starts Storage again. The run's state is a JSON file, so a run that stopped continues where it stopped. Reads fail while `supavise-storage` is stopped, from the stop to the start on the bucket: the fence pass walks every project's files and every online project's rows, so the window grows with the store.

```
supavise storage migrate --to s3 [--bucket B] [--credentials-file F | --role-arn ARN] [--rate-limit MiB/s]
supavise storage migrate --to s3 --status | --resume | --rollback | --cleanup
```

The endpoint, region and addressing style of the bucket come from the `[fleet]` settings (`storage_s3_endpoint`, `storage_s3_region`, `storage_s3_force_path_style`); `--bucket` overrides `storage_s3_bucket`. The command runs as the `supavise` user, like `supavise fleet`.

## Keys

Storage's S3 backend writes an object under the key `<ref>/<bucket>/<name>/<version>`, which is the file backend's path below `<state>/system/storage/objects/stub/`. The copy therefore needs no key mapping: each file becomes one object, with its content type and cache control taken from the file's extended attributes (`user.supabase.content-type` and `user.supabase.cache-control`; on macOS the `com.apple.metadata.supabase.` prefix). A file system without extended attributes loses both, as it does for the backups.

A name that cannot be an S3 key (not valid UTF-8, or a key over 1,024 bytes) is not copied. It is counted and listed in `--status` (the first 50, as `<ref>/<path>`) and in the output of the run. Valid non-ASCII names are copied as they are. Anything that is not a regular file is left out, as in the backup.

The bucket's keys are not trusted either. Going the other way, a key whose path below `<ref>/` is empty, has an empty, `.` or `..` segment or holds a NUL is left in the bucket and counted (`refused` in `--status`), because joining it would write outside the project's directory; a directory on the way that is a link or a file stops the rollback instead of being written through.

## Phases

| Phase | What happens |
|---|---|
| `copying` | The preflight (Storage answers; the credentials can put, get and delete an object in the bucket), then a first pass over every project's directory. A pass compares the files with the previous pass (size and modification time), sends what is new or changed with 8 workers, and deletes the keys of files that were there in the pass before and are gone. The first pass of a process has no earlier pass and compares with a listing of the bucket instead, which is how a run that was cut short continues without sending everything again. That comparison only sends: a key that no file matches is never deleted, because nothing says this run sent it (the bucket may hold objects of an earlier use). `--status` and the output count such keys per project. |
| `catching_up` | Passes until one takes under 60 seconds (at most 5; then the switch begins anyway), so that the last pass inside the switch is short. |
| `verifying` | For each project whose database runs: every row of `storage.objects` must have its file at `<bucket>/<name>/<version>` with the size the row records, and every file must be in the bucket at its size (a listing of `<ref>/` is compared with the files; keys without a file are counted and left alone). A mismatch stops the run before anything is switched and names the first objects. Files without a row (partial uploads, orphans) are copied and counted. A project whose database is not running (paused) has only its copy checked, and `--status` says so. While Storage takes writes the rows are read first and the files are copied after, and only rows that had no file yet are looked up again, so an upload made during the check does not count against the project; a few rounds are made before the check fails. |
| `flipping` | Before anything else the new configuration is tried without being written (see "Trying the switch"). Steps `hold`, `final`, `stopped`, `fence`, `switched`, `live`, `started`. The write hold is a marker file the edge proxy reads (see below). `final` is a pass while Storage still runs. `stopped` stops `supavise-storage`. `fence` is a pass over files that no longer change, plus a last read of every row against them. `switched` writes `storage_backend = "s3"` and the bucket into `config.toml`, and the credentials into `config.d/30-storage-s3.toml` (0600). `live` records that Storage was started on the bucket, from the rendered unit; `started` records that it served a few objects read back through its port with the project's service key (the output says how many, or that a store without rows gave it nothing to read). A failure before `live` starts Storage again on the files, puts the configuration back and returns the run to `catching_up`; `--resume` tries again. From `live` on, Storage may have taken writes that only the bucket has, so a failure leaves it where it is and the run at `flipping`: `--resume` repeats what is left, and `--rollback` copies those writes back to the files. |
| `done` | `objects/` is renamed `objects.migrated-<date>` and stays for 14 days (`--status` shows the date; nothing deletes it by itself). The name is recorded in the state before the rename, and only a directory with that name is taken for the run's files by `--rollback` and `--cleanup`. |

A step that was recorded is not repeated; a step that was not recorded can run twice. A run killed in the middle of the switch leaves Storage stopped until `--resume` (or `systemctl start supavise-storage`, or the next start of the daemon, which renders from the configuration as it is).

## Rate limit

`--rate-limit` (MiB/s, 32 by default, `0` for none) bounds the bytes the copy sends to the bucket: the HTTP transport of the S3 client asks the limiter for each 64 KiB block of a request body before it writes it, so one big object cannot burst past the limit and every worker and part shares it. The passes of the switch (`final` and `fence`) run without the limit, because writes are held or Storage is stopped while they run. Downloads (a rollback) are not limited.

The same transport ends a request that has moved no byte for 2 minutes (a stalled connection), and the SDK tries it again up to 5 times.

## Trying the switch

The preflight and the start of the `flipping` phase load the configuration the switch would leave, in a scratch copy: `config.toml` with `storage_backend = "s3"` and the bucket, the drop-ins of `config.d` and the credentials drop-in, read the way the daemon reads them, with the environment. The run is refused when something overrides the backend (a drop-in or a `SUPAVISE_FLEET_STORAGE_BACKEND` variable) or when the unit of `supavise-storage` cannot be rendered for that configuration (no bucket; under systemd, no static key, because the unit cannot reach the instance role). Nothing is written to the node and Storage keeps running. The same check runs again before the stop, because the copy can take days.

## Rollback and cleanup

`--rollback` is the switch the other way: the kept directory is moved back to `objects/`, a pass copies what the bucket gained since the switch into the files (new objects with their content type and cache control), `supavise-storage` stops, a last pass runs over the stopped service, the configuration goes back to the earlier backend (and `config.d/30-storage-s3.toml` is removed), and Storage starts on the files. Objects uploaded after the switch come back; objects deleted since are deleted from the files; the command prints both counts. When the kept files were deleted, every object is fetched from the bucket. A project for which the bucket lists nothing keeps its files: that is more likely the wrong bucket than a project whose objects were all deleted. `--cleanup` deletes `objects.migrated-<date>` and is refused unless a migration is done.

## Credentials

A secret is never an argument. `--credentials-file` names a file with `access_key_id=` and `secret_access_key=` lines (or the `AWS_` spellings), mode 0600, owned by the user who runs the command or root. `--role-arn` assumes an IAM role with the node's own credentials (the instance role) through `internal/awsapi`; the temporary credentials are renewed before they expire. With neither flag a run that used a role goes on with it, else the static key in `[fleet]` is used, which is how `--rollback` after a finished migration finds the key `config.d/30-storage-s3.toml` holds, else the credentials file the run used. The state file keeps the path of the credentials file, never its content. An endpoint that is plain `http` on a host other than this machine gets a warning. `config.toml` never receives a secret; for a role the drop-in gets `storage_s3_role_arn`, which the Storage credential endpoint reads.

## The write hold

While the switch runs, `<state>/system/storage-migrate/write-hold.json` exists and is renewed every 30 seconds; it expires after two minutes, so a run that dies cannot block writes for long. `hold.Held(paths, now)` (package `internal/storagemigrate/hold`, which imports nothing but `config`) tells the edge proxy whether to answer a Storage request that changes data (any method but GET, HEAD and OPTIONS) with 503 and `Retry-After: 5` while reads go on. The stop and the last pass do not depend on it: a write that gets through before the stop is copied by the pass over the stopped service's files, so the hold keeps clients from seeing errors and is not what makes the copy complete.

The record of the run (`migrate.json`), its lock and the hold live in `<state>/system/storage-migrate/`, not in Storage's directory: the unit of `supavise-storage`, which runs on user input, may write below `system/storage` only. Before it acts on a record, `--resume` and `--rollback` check that it names a backend and directories this command writes and that `[fleet]` still names the endpoint, region and addressing the objects were copied to.

## The daemon

`wireStorageMigrate` (internal/app) does nothing on a node without a record. When the daemon starts it removes an expired hold and logs a run that stopped before it finished, with the command that continues it. The daemon reads the configuration once, so after a switch or a rollback `supavise.service` should be restarted at once: until then a backup that the daemon itself starts looks for the files, finds no directory and records an empty snapshot of them (the nightly timer starts a new process and sees the new setting). The run says so when it ends.

## Assumptions

- A bucket copy found by a run that continues counts as current when it has the file's size and is newer than the file by more than 2 seconds, so the clocks of the node and the bucket should agree within that. Verification compares sizes, not content.
- A key in the bucket that no file matches is counted per project (`Extra`) and never deleted: a process that continues a run has no earlier pass to say that the run sent it, so an object deleted while a run was stopped stays in the bucket as an unreferenced key until someone removes it.

## Code

- `engine.go`, `flip.go`: the phases, the switch, rollback and cleanup.
- `sync.go`, `inventory.go`: a pass over one project (walk, compare, send, delete) and the pacing of the copy. The inventory of a project, sorted by path, is kept in memory between passes: about 100 bytes a file.
- `verify.go`: the check of the rows and of the bucket.
- `s3.go`, `transport.go`: the bucket, over the AWS SDK already used by the backups; single request up to 64 MiB, parts above that. The transport paces the request bodies and ends a stalled request.
- `files.go`, `xattr_*.go`: the walk of the objects directory and the extended attributes. `backup.WalkStorage` reads the attributes of every file it visits; a pass here reads them only for the files it sends.
- `node.go`: the databases (supabase_admin over the project's private socket, or the forwarder on its canonical port with the sealed password for a project on another node), `supavise-storage` through a fleet manager that touches no other service, and the read-back through Storage's loopback port.
- `state.go`: the state file, the lock (`migrate.lock`, a flock the kernel drops when the process dies) and the text of `--status`. `hold/` is the write-hold marker.

## Tests

`go test ./internal/storagemigrate/` runs the engine against an in-memory bucket and fakes for the databases and the service (copy, catch-up, verification failures, a failure and a resume in every phase, a process that dies during the switch, a failure after Storage started on the bucket, the try of the switch, rollback with new and deleted objects and with hostile keys, cleanup, names that cannot be keys), and the S3 code against an in-process S3 service (the pacing, a stalled connection). `tests/linux/storage-migrate.sh` (job `storage-migrate` of `linux.yml`) runs the whole thing under systemd against Garage: a refused configuration, a copy at 1 MiB/s killed while the bucket holds some of the objects and not the 80 MiB one, the resume that sends only the rest, the switch, the rollback, a second migration and `--cleanup`.
