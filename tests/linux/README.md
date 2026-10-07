# tests/linux

Scripts for an ephemeral Ubuntu 24.04 VM (22.04 and Debian 12 should work too) with systemd,
cgroup v2, sudo and network access. They create the `sbctl` user, install units under
`/etc/systemd/system`, download the real artifacts and start real clusters. Do not run them
on a machine you care about; development machines run the exec-backend tests instead
(`internal/lifecycle`, `SBCTL_TEST_UNPACKED`).

| Script | What |
|---|---|
| `systemd-smoke.sh` | system init, two projects, health (units, user, slice, `MemoryMax` drop-in, GoTrue, PostgREST, bad-JWT rejection, sign-up, no passwordless TCP), pause, resume, key rotation, `kill -9` recovery, delete (units, data, drop-ins gone). Non-zero exit on any failure. |
| `fleet-smoke.sh` | the shared services under systemd: `sbctl fleet start`, one project registered as a tenant of Supavisor, Realtime and Storage, pooler logins (session and transaction port), Storage bucket, upload and signed-URL download, Realtime channel join, key rotation, `kill -9` of each service, tenant removal, project delete, `fleet stop`. Skips Studio (no slim artifact). |
| `branching-egress.sh` | a branch with data under systemd cannot act on the outside world: state directory on a loop-file XFS (reflink clone), a parent with pg_net, pg_cron, a job and data, a test HTTP server on the runner's non-loopback address as the external host. The parent's pg_net request reaches it (control); the default branch has `IPAddressDeny`/`IPAddressAllow` on its Postgres unit only, `egress` denied, its request fails and never reaches the server, its cron job is inactive and recorded in `sbctl_branch.paused_cron_jobs`; the restriction survives pause/resume and reset; a `--allow-egress` branch reaches the server and keeps its job; delete lifts the restriction. Run by CI job `branching-xfs`. |
| `footprint.sh` | grows to 10, 25 and 50 projects; per size records create time, per-project PSS and RSS, system project PSS, `sbctl.slice` memory, disk per project, resume time and whole-node cold start; prints a markdown table (`$LOG_DIR/footprint.md`). |
| `lib.sh` | shared helpers |

```
sudo SBCTL_BIN=./bin/sbctl-linux-amd64 tests/linux/systemd-smoke.sh --teardown
sudo SBCTL_BIN=./bin/sbctl-linux-amd64 tests/linux/footprint.sh --sizes "10 25 50" --class default
```

`SBCTL_BIN` is a Linux build (`make build-linux`); without it the scripts build one with go.
Logs (journal, unit list, host facts, df) stay in `$LOG_DIR` (default
`/tmp/sbctl-linux-logs`). `SBCTL_DOMAIN` sets the config domain (default `sbctl.test`;
TLS is off, nothing needs DNS).

Status: written and shell-syntax checked, **not run** (no Linux in development).
