# internal/storagemigrate

Moves Storage's objects between its file backend and an S3 bucket: `supavise storage migrate --to s3` (design 2.11). The copy runs while Storage serves from the files. The switch holds Storage's writes, stops `supavise-storage`, copies once more from files that nothing writes to, points the configuration at the bucket and starts Storage again. The run's state is a JSON file, so a run that stopped continues where it stopped.

```
supavise storage migrate --to s3 [--bucket B] [--credentials-file F | --role-arn ARN] [--rate-limit MiB/s]
supavise storage migrate --to s3 --status | --resume | --rollback | --cleanup
```

The endpoint, region and addressing style of the bucket come from the `[fleet]` settings (`storage_s3_endpoint`, `storage_s3_region`, `storage_s3_force_path_style`); `--bucket` overrides `storage_s3_bucket`. The command runs as the `supavise` user, like `supavise fleet`.

## Keys

Storage's S3 backend writes an object under the key `<ref>/<bucket>/<name>/<version>`, which is the file backend's path below `<state>/system/storage/objects/stub/`. The copy therefore needs no key mapping: each file becomes one object, with its content type and cache control taken from the file's extended attributes (`user.supabase.content-type` and `user.supabase.cache-control`; on macOS the `com.apple.metadata.supabase.` prefix). A file system without extended attributes loses both, as it does for the backups.

A name that cannot be an S3 key (not valid UTF-8, or a key over 1,024 bytes) is not copied. It is counted and listed in `--status` (the first 50) and in the output of the run. Valid non-ASCII names are copied as they are. Anything that is not a regular file is left out, as in the backup.

## Phases

| Phase | What happens |
|---|---|
| `copying` | The preflight (Storage answers; the credentials can put, get and delete an object in the bucket), then a first pass over every project's directory. A pass compares the files with the previous pass (size and modification time), sends what is new or changed with 8 workers at the rate limit (32 MiB/s by default; `--rate-limit 0` lifts it), and deletes the keys of files that were there in the pass before and are gone. The first pass of a process has no earlier pass and compares with a listing of the bucket instead, which is how a run that was cut short continues without sending everything again. That comparison only sends: a key that no file matches is never deleted, because nothing says this run sent it (the bucket may hold objects of an earlier use). `--status` and the output count such keys per project. |
| `catching_up` | Passes until one takes under 60 seconds (at most 5; then the switch begins anyway), so that the last pass inside the switch is short. |
| `verifying` | For each project whose database runs: every row of `storage.objects` must have its file at `<bucket>/<name>/<version>` with the size the row records, and every file must be in the bucket at its size (a listing of `<ref>/` is compared with the files; keys without a file are counted and left alone). A mismatch stops the run before anything is switched and names the first objects. Files without a row (partial uploads, orphans) are copied and counted. A project whose database is not running (paused) has only its copy checked, and `--status` says so. While Storage takes writes the rows are read first and the files are copied after, and only rows that had no file yet are looked up again, so an upload made during the check does not count against the project; a few rounds are made before the check fails. |
| `flipping` | Steps `hold`, `final`, `stopped`, `fence`, `switched`, `started`. The write hold is a marker file the edge proxy reads (see below). `final` is a pass while Storage still runs. `stopped` stops `supavise-storage`. `fence` is a pass over files that no longer change, plus a last read of every row against them. `switched` writes `storage_backend = "s3"` and the bucket into `config.toml`, and the credentials into `config.d/30-storage-s3.toml` (0600). `started` renders the unit, starts Storage, waits until it answers, and reads a few objects back through its port with the project's service key. A failure before `switched` starts Storage again on the files; a failure in `started` puts the configuration back first. Either way the run returns to `catching_up` and `--resume` tries again. |
| `done` | `objects/` is renamed `objects.migrated-<date>` and stays for 14 days (`--status` shows the date; nothing deletes it by itself). |

A step that was recorded is not repeated; a step that was not recorded can run twice. A run killed in the middle of the switch leaves Storage stopped until `--resume` (or `systemctl start supavise-storage`, or the next start of the daemon, which renders from the configuration as it is).

## Rollback and cleanup

`--rollback` is the switch the other way: the kept directory is moved back to `objects/`, a pass copies what the bucket gained since the switch into the files (new objects with their content type and cache control), `supavise-storage` stops, a last pass runs over the stopped service, the configuration goes back to the earlier backend (and `config.d/30-storage-s3.toml` is removed), and Storage starts on the files. Objects uploaded after the switch come back; objects deleted since are deleted from the files; the command prints both counts. When the kept files were deleted, every object is fetched from the bucket. A project for which the bucket lists nothing keeps its files: that is more likely the wrong bucket than a project whose objects were all deleted. `--cleanup` deletes `objects.migrated-<date>` and is refused unless a migration is done.

## Credentials

A secret is never an argument. `--credentials-file` names a file with `access_key_id=` and `secret_access_key=` lines (or the `AWS_` spellings), mode 0600, owned by the user who runs the command or root. `--role-arn` assumes an IAM role with the node's own credentials (the instance role) through `internal/awsapi`; the temporary credentials are renewed before they expire. With neither flag the static key in `[fleet]` is used, which is how `--rollback` after a finished migration finds the key `config.d/30-storage-s3.toml` holds. The state file keeps the path of the credentials file, never its content. `config.toml` never receives a secret; for a role the drop-in gets `storage_s3_role_arn`, which the Storage credential endpoint reads.

## The write hold

While the switch runs, `<state>/system/storage/write-hold.json` exists and is renewed every 30 seconds; it expires after two minutes, so a run that dies cannot block writes for long. `hold.Held(paths, now)` (package `internal/storagemigrate/hold`, which imports nothing but `config`) tells the edge proxy whether to answer a Storage request that changes data (any method but GET, HEAD and OPTIONS) with 503 and `Retry-After: 5` while reads go on. The stop and the last pass do not depend on it: a write that gets through before the stop is copied by the pass over the stopped service's files, so the hold keeps clients from seeing errors and is not what makes the copy complete.

## The daemon

`wireStorageMigrate` (internal/app) does nothing on a node without a record. When the daemon starts it removes an expired hold and logs a run that stopped before it finished, with the command that continues it. The daemon reads the configuration once, so after a switch or a rollback `supavise.service` should be restarted when convenient: until then a backup that the daemon itself starts looks for files (the nightly timer starts a new process and sees the new setting).

## Code

- `engine.go`, `flip.go`: the phases, the switch, rollback and cleanup.
- `sync.go`, `inventory.go`: a pass over one project (walk, compare, send, delete) and the pacing of the copy. The inventory of a project, sorted by path, is kept in memory between passes: about 100 bytes a file.
- `verify.go`: the check of the rows and of the bucket.
- `s3.go`: the bucket, over the AWS SDK already used by the backups; single request up to 64 MiB, parts above that.
- `files.go`, `xattr_*.go`: the walk of the objects directory and the extended attributes. `backup.WalkStorage` reads the attributes of every file it visits; a pass here reads them only for the files it sends.
- `node.go`: the databases (supabase_admin over the project's private socket, or the forwarder on its canonical port with the sealed password for a project on another node), `supavise-storage` through a fleet manager that touches no other service, and the read-back through Storage's loopback port.
- `state.go`: the state file, the lock (`migrate.lock`, a flock the kernel drops when the process dies) and the text of `--status`. `hold/` is the write-hold marker.

## Tests

`go test ./internal/storagemigrate/` runs the engine against an in-memory bucket and fakes for the databases and the service (copy, catch-up, verification failures, a failure and a resume in every phase, a process that dies during the switch, rollback with new and deleted objects, cleanup, names that cannot be keys), and the S3 code against an in-process S3 service. `tests/linux/storage-migrate.sh` (job `storage-migrate` of `linux.yml`) runs the whole thing under systemd against Garage.
