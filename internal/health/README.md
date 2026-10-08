# internal/health

One answer to "is this node all right?": `health.CheckNode(ctx, deps)` returns a `Report` with a verdict, a result for every component of the node and one for every project. `supavise status`, the public `/healthz`, the operator's `/healthz/detail` and the alert checker (`internal/alerts`) all read it.

```go
deps, _ := health.ForNode(node, health.NodeOptions{Version: version, Tenants: lazyFleet})
rep, _ := health.CheckNode(ctx, deps)
rep.Verdict.ExitCode() // 0 healthy, 1 degraded, 2 down
```

## What it checks

| Component | How | If it fails |
|---|---|---|
| `daemon`, `edge` | the CLI connects to the admin listener and the public listener on loopback; inside the daemon they are "this process" | down |
| `system postgres` | the system project's unit and a SQL ping | down |
| `system gotrue` | unit and `/health` (the dashboard's sign-in) | degraded |
| `supavisor`, `realtime`, `storage`, `pgmeta`, `studio`, `edge-runtime`, `imgproxy` | `fleet.Manager.Status`: unit state and the service's health endpoint. A service the node never rendered is a note, not a failure | degraded |
| each project | `PostgresPlane.Health`: Postgres answers a query, GoTrue `/health`, PostgREST `/`, all on loopback; then whether Supavisor, Realtime and Storage hold the project's tenant (`fleet.Fleet.TenantPresence`, one GET each, no retries); then the age of the newest completed base backup | degraded |
| `system backup` | the same freshness rule for the system project, which holds the registry | degraded |
| `disk` | free space of the state volume | degraded |
| `certificates` | the earliest expiry among the certificates the node must keep valid: `api.<domain>`, `studio.<domain>` and the `*.api.<domain>` wildcard under `<state_dir>/certs`; for each name the newest copy counts, because CertMagic keeps one copy per issuer and an old issuer's copy (after `[tls] ca` changed) is never renewed | degraded |
| `key escrow` | whether the backup backend holds an encrypted copy of the master key | note |
| `update` | the record of the daily update check (below) | note |
| `upgrade` | a node upgrade that is running (`<state_dir>/system/upgrade.json`, `internal/notice`): the phase, the releases it moves between, how long it has run, what it is doing now, and, when the process that wrote the marker is gone, that and what to run | note |
| `maintenance` | the announced maintenance window (`internal/notice`) | note |
| `held restarts` | the projects whose restart an upgrade held back (`lifecycle.HeldRestart`: the daemon rendered new PostgreSQL, GoTrue or PostgREST files and left the running unit on the old ones for the rollout to restart); the detail names them and the command that finishes them, `sudo supavise upgrade` (during a running upgrade it says the rollout restarts them). Only `ACTIVE_HEALTHY` projects count, as in the upgrade plan | note |

A project is probed only when it should answer. A paused project (`INACTIVE`) is listed as paused, one being created, restored or deleted as busy, `INIT_FAILED` as a note and `RESTORE_FAILED` as a warning. A removed project is not listed. The checks never write: unlike `Engine.Health` they leave the registry's project status alone.

Certificates for project hosts and custom hostnames (HTTP-01 on demand) do not judge the node. CertMagic renews them only when someone opens the host, so one for a live host can sit near its end, and one for a deleted project or a removed hostname is never renewed. The report counts the near ones and the retired ones in the `certificates` detail as notes; a node with fifty changing projects therefore stays healthy for `supavise upgrade --unattended`.

Projects are probed 16 at a time with 10 seconds for each, so fifty projects take a few seconds. The registry's backup rows (`ListBackups`) give the backup age; a backup is stale after the period of `[backup] base_backup_on_calendar` plus 12 hours (36 hours for the default nightly timer; a weekly calendar gives 7 days and 12 hours), or after `[health] backup_stale_hours` when that is set. The period is read from the common calendar forms (daily time of day, `hourly`, `weekly`, a weekday list, `monthly`); anything else counts as daily, so set `backup_stale_hours` for an unusual calendar. A project younger than that with no backup yet is "no backup yet", not stale; a project whose newest backup failed after the last good one reports that error.

## The verdict

- **healthy**: nothing failed or warned. Notes (a release is available, the master key has no copy in the backups, a maintenance window is announced, a node upgrade is running, a project's restart is held back for the next upgrade) do not change it, so they do not stop `supavise upgrade --unattended` and do not turn an uptime monitor red.
- **degraded**: the node serves but something needs the operator: a project or shared service is down, a backup is stale or failed, the disk or a certificate is running out.
- **down**: the daemon, the edge, the registry or the system cluster's Postgres is not running.

`Report.Summary` is the one line `supavise status` prints first: `healthy: 50 projects answering, 2 paused`, or `degraded: 2 projects not answering: abc..., def...; ...` with the worst problems first and at most three of them. A node whose registry cannot be opened (the system cluster is down, or the master key is unreadable by the user who ran the command) still reports what needs no registry, and says why the projects are missing.

## `supavise status`

```
sudo -u supavise supavise status [--json] [-v]
```

Exit status 0 healthy, 1 degraded, 2 down. The human form is the verdict line, a table of components and a table of the projects that need attention (`-v` lists all of them). `--json` prints the `Report`: `status`, `checked_at`, `version`, `summary`, `issues`, `components[]` (`name`, `state` ok|info|warn|fail, `critical`, `detail`) and `projects[]` (`ref`, `name`, `status`, `state`, `probed`, `services[]`, `tenants[]`, `backup`, `detail`). The organization id of a project is not part of it.

## The daemon: `Monitor`

The daemon builds one `Monitor` over `CheckNode`. It reuses a report for `[health] cache_seconds` (20), runs one check for any number of callers that arrive while it runs (`singleflight`), and lets a check finish when the caller that started it gives up. If a check fails or times out, a report under two minutes old stands in; an older one does not, because a check that has been failing for minutes must not leave `/healthz` saying what it said before. The result is that anyone who can reach `/healthz` costs the node one probe of its projects per 20 seconds, however often they ask.

The escrow lookup lists the backup backend, which can be slow, so inside the daemon it refreshes in the background (once an hour) and the report says "not checked yet" until the first answer.

## The update check

`CheckUpdate` asks GitHub (`selfupdate.Latest`) for the newest release and records it in `<state_dir>/system/update.json`: when it checked, the installed version, the latest, whether it is newer (never for a development build) and which version the operator was already told about. It installs nothing. The daemon's alert checker runs it once a day (see `internal/alerts`); `supavise status` shows the record. It is the node's only release check: `supavise update status` and the unattended upgrade read its record, and `supavise update run` never checks. It reads `[update] check_interval` (a duration such as `12h`, whole days such as `2d`, or a bare number of seconds) and `SUPAVISE_UPDATE_CHECK_INTERVAL` on its own, with a 24-hour default and a one-hour floor (`off`, `never` or `0` turns the check off, for a node with no route to GitHub). The grammar is `config.ParseCheckInterval`, the same one the config validator applies, so a value this reads is a value `config.toml` accepts; it ignores every other key of the section: that section belongs to the release tooling, and a missing or unreadable one gives the default.

## Config

`[health]` (or `SUPAVISE_HEALTH_*`), all optional:

| Key | Default | Meaning |
|---|---|---|
| `disk_low_percent` | 10 | free space below this percentage of the state volume degrades the node; below half of it is a failure |
| `disk_low_gb` | 5 | the same as an absolute floor in GiB, for volumes where a percentage says little |
| `backup_stale_hours` | backup period + 12 | age of the newest completed base backup past which a project is stale (36 for the default nightly timer) |
| `certificate_warn_days` | 14 | days before expiry at which a certificate is reported (CertMagic renews at 30) |
| `cache_seconds` | 20 | how long the daemon reuses a report |

## Tests

```
go test ./internal/health/
```

`supavise status` exits 0, 1 or 2 only for a verdict. An error before there is one (no readable config, a user who may not read the node's files, a cancelled context) exits 1 through the shared error path, the same code as degraded, so a gate such as `supavise upgrade --unattended` reads the report (`CheckNode`, or `--json`'s `status` field), not the exit status alone.

Unit tests with fakes for the registry, the plane, the shared services, the tenants, the disk and the clock: the verdict and exit-code table, the summary, project statuses, backup freshness, tenants missing or unreachable, disk thresholds, real certificates generated in the test (an expired copy from an old issuer next to a good one; stale on-demand certificates for removed hosts that must not judge the node), advisories that must not change the verdict, the report's JSON shape, the monitor's cache, single flight and stale-answer rules, the update check against a fake GitHub API, the `[update]` reader's defaults, the escrow lookup against a file backend. `cmd/supavise/cmd_ops_test.go` runs `supavise status` against a node that is not running (exit status 2); the `install-e2e` job runs it on a healthy node, stops one project's PostgREST and expects degraded and exit status 1.

## Not done

- No history: a report is a moment, not a trend.
- Health does not probe the data path of a project end to end (a REST request through the proxy with its key); it probes each unit on loopback and the tenant records.
- The check of a shared service's tenant asks the service for the tenant record; it does not open a Realtime socket or a Storage upload.
