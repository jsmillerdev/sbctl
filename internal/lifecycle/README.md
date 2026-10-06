# internal/lifecycle

The project lifecycle: `Engine` (the `Manager`) owns the order of registry rows, sealed
secrets, units, fleet tenants and routes; `PostgresPlane` (the `DataPlane`) runs a
project's PostgreSQL, GoTrue and PostgREST as units of a `units.Supervisor`.

## Create

1. Registry row `COMING_UP`; credentials generated (or `CreateRequest.Keys` reused) and
   sealed into `project_secrets`.
2. `PostgresPlane.Create`: directories, `pg_hba.conf` and the pgsodium root key outside
   PGDATA (0600), then `bin/supabase-postgres-start` with these server arguments:
   `-p <port>`, `listen_addresses=127.0.0.1`, `unix_socket_directories=<project>/postgres/sock`
   (mode 0700), `hba_file`, `wal_level=logical`, `archive_mode=on`,
   `archive_command='<bin_path> wal push --ref <ref> %p'`, `archive_timeout=900`,
   `max_wal_senders=5`, and `shared_buffers`, `effective_cache_size`,
   `maintenance_work_mem`, `max_wal_size`, `max_connections` from the project class
   (`micro`, `default`/`small`, `medium`, `large`; default is 32MB and 60 connections).
   The launcher initializes PGDATA and runs the artifact's roles and migrations once.
   Role passwords of `postgres`, `supabase_admin`, `authenticator`,
   `supabase_auth_admin`, `supabase_storage_admin` and `supabase_replication_admin` are set
   afterwards over the unix socket. `CreateRequest.Seed` replaces initdb and role setup and
requires `CreateRequest.Keys` (the seeded cluster's own credentials; fresh ones would not
match its passwords).
3. GoTrue (`bin/auth migrate`, then `bin/auth`) and PostgREST, each health-checked with a
   real request (`/health`, `/`).
4. `fleet.Fleet.EnsureTenant` (skipped while the fleet is empty), `PutRoute` for
   `<ref>.api.<domain>`, status `ACTIVE_HEALTHY`.

Any failure after the row exists stops and removes units and data, removes tenants and
route, and leaves the row `INIT_FAILED` with an event that carries the cause. Cleanup uses
a context that outlives the cancelled request. The one exception is `ErrClusterExists`
(the project directory already holds a cluster, for example an orphan after a registry
restore): Create refuses before writing anything, leaves the data alone and forgets the row.

Mutating operations on one ref are serialized by a mutex in the process and, with the
Postgres registry, by a session-level advisory lock on a dedicated connection, so the CLI
and the daemon cannot run, say, delete and resume on the same project at once.

`pg_hba.conf` is ours, not the artifact's: the artifact trusts all loopback connections,
which on a shared host means every local user. Ours trusts `supabase_admin` on the unix
socket only (the directory is private to the `sbctl` user) and requires scram-sha-256 on
TCP. The socket is how sbctl reaches its own registry before it can decrypt any secret.

## Other operations

- `Pause`: PostgREST, GoTrue, PostgreSQL stop in that order; `INACTIVE`; route and tenants
  stay. `Resume` reverses it; on failure what started is stopped and the project stays
  `INACTIVE`.
- `Delete`/`DeleteWith`: final base backup through `BaseBackuper` (skipped when nil, for
  `INIT_FAILED` projects and with `SkipFinalBackup`; a paused project's database is started
  just for it), tenants, route, units, data, registry row. A failed backup keeps the project.
  A failure after the backup leaves `GOING_DOWN` so the delete can be repeated.
- `RotateKeys`: new JWT secret, legacy and opaque keys; database passwords unchanged;
  GoTrue and PostgREST restart, fleet tenants update; previous keys are restored on error.
- `Health`: unit state plus SQL ping, GoTrue `/health`, PostgREST `/`; moves
  `ACTIVE_HEALTHY` and `ACTIVE_UNHEALTHY` to match.
- `StartActive`: starts every active project (after a reboot or `sbctl system stop`).

## System project

`InitSystem` is the same code path with ref `system` on `[ports] system_postgres` and
`system_gotrue`, class `system`, and no PostgREST. It creates the master key, starts the
cluster, creates databases `sbctl`, `_supavisor`, `_realtime` and `_storage`. Each of the
last three belongs to its own login role (`FleetRoles`: `sbctl_supavisor`,
`sbctl_realtime`, `sbctl_storage`), which owns the database and the same-named schema,
has no other access, and whose password is sealed in the registry
(`Engine.FleetCredentials`); fleet services connect with those, not as `supabase_admin`.
`supabase_storage_admin` may also use `_storage`. It applies the registry migrations, records the system project and its
sealed credentials, and starts GoTrue for Studio sign-in (`GOTRUE_SITE_URL` is
`https://studio.<domain>`, sign-up closed). It is idempotent. If the first-time init fails
before the credentials are in the registry, it deletes what it created. If the process
dies mid-init, the next run notices (the launcher's init-pending witness, or a cluster with
no registry, no system credentials and no projects), removes that cluster and starts over;
a registry that holds projects but lost the system credentials is left alone and the error
says how to recover.
`Open` connects to an initialized node for everything else; `RegistryDSN` is the DSN other
packages use to reach the registry.

Choices worth knowing:

- `wal_level` stays `logical` (the artifact's value): Realtime's `postgres_changes` needs
  it, and it is a superset of `replica`, so archiving and base backups are unaffected.
- `API_EXTERNAL_URL` is `https://<ref>.api.<domain>/auth/v1`, as hosted projects and the
  dockerless CLI set it. GoTrue resolves its mailer paths against that URL with an absolute
  path, which would drop `/auth/v1`, so `GOTRUE_MAILER_URLPATHS_{INVITE,CONFIRMATION,
  RECOVERY,EMAIL_CHANGE}` are set to `<API_EXTERNAL_URL>/verify`, as the dockerless CLI does.
- `GOTRUE_MAILER_AUTOCONFIRM=true` until SMTP is configured; nobody could confirm an
  address otherwise.
- The seeder contract: `DataSeeder` fills `<project>/postgres/data`; `postmaster.opts` is
  created if missing because `pg_basebackup` omits it and the launcher refuses a data
  directory without it.
- Unix socket paths are limited to about 100 bytes, so on macOS and in tests the state
  directory must be short; the plane refuses a longer one with a clear error.

## Test

```
go test ./internal/lifecycle/                       # fakes: state machine, specs, failure paths
SBCTL_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked \
  go test -run Integration -v ./internal/lifecycle/ # real artifacts, exec backend
```

The integration tests need unpacked slim-services artifacts (`postgres-17*`, `auth-*`,
`postgrest-*`) for the running platform. They start the system cluster and one project
(about 130 MB), run the full create, health, SQL, pause, resume, rotate, delete cycle, and
a create that fails on a busy port. State lives in a short `/tmp/sbt*` directory and is
removed on exit.

## Not done

- No recovery pass for projects stuck in `COMING_UP`, `PAUSING` or `GOING_DOWN` after a
  daemon crash (delete works from any status).
- `Snapshot` and the final backup depend on the backup workstream's `BaseBackup`; until it
  is wired in, `Delete` does not back up.
- Fleet tenant calls are a hook (`Options.Fleet`); the services themselves are the fleet
  workstream's.
- Project upgrade (artifact version changes) is not implemented.
- `Usage` reports disk bytes and unit memory only.
