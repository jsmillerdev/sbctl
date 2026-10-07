# tests/linux

Scripts for an ephemeral Ubuntu 24.04 VM (Debian 12 should work too; Ubuntu 22.04 cannot, its polkit ignores .rules files) with systemd,
cgroup v2, sudo and network access. They create the `sbctl` user, install units under
`/etc/systemd/system`, download the real artifacts and start real clusters. Do not run them
on a machine you care about; development machines run the exec-backend tests instead
(`internal/lifecycle`, `SBCTL_TEST_UNPACKED`).

| Script | What |
|---|---|
| `systemd-smoke.sh` | system init, two projects, health (units, user, slice, `MemoryMax` drop-in, GoTrue, PostgREST, bad-JWT rejection, sign-up, no passwordless TCP), containment from inside the namespaces (no `/etc/sbctl`, no backups, only its own WAL relay directory, read-only), the daemon, WAL archiving through the relay socket (another project's socket is not there and the relay refuses a foreign ref with 403), archiving that fails closed with the daemon down and resumes, a base backup with the daemon down, a restore of a clone whose recovery reads the source archive through the clone's own relay socket, the instance metadata service denied to every unit from inside it (mock on both metadata addresses, `COPY TO PROGRAM` inside Postgres), a project created through the Management API with REST through the proxy, pause, resume, key rotation, `kill -9` recovery, delete (units, data, drop-ins gone). Non-zero exit on any failure. |
| `fleet-smoke.sh` | the shared services under systemd and through the daemon: `sbctl fleet start` fetches the artifacts, then `sbctl serve` starts the services by itself; a project created through the Management API is registered with Supavisor, Realtime and Storage; REST and a Storage upload through the proxy, pooler logins (session and transaction port, plain and `sslmode=require`), signed-URL download, Realtime channel join, key rotation, `kill -9` of each service, the metadata service denied to each of them, tenant removal, project delete through the API, `fleet stop`. Skips Studio (no slim artifact). |
| `functions-smoke.sh` | Edge Functions under systemd: system init, two projects, `sbctl functions dev` (API, proxy and `sb-edge-runtime` as a unit), then `tests/functions/run.sh` (real `supabase functions deploy` of fixtures, supabase-js, JWT on and off, per-project secrets and code, a function querying its own database, crash, runaway and memory-hog isolation, redeploy, delete), the unit's user, slice, memory limit, loopback-only listener, hidden paths, `kill -9` recovery. Installs the Supabase CLI release (checksum-verified). |
| `branching-egress.sh` | a branch with data under systemd cannot act on the outside world: state directory on a loop-file XFS (reflink clone), a parent with pg_net, pg_cron, a job and data, a test HTTP server on the runner's non-loopback address as the external host. The parent's pg_net request reaches it (control); the default branch has `IPAddressDeny`/`IPAddressAllow` on its Postgres unit only, `egress` denied, its request fails and never reaches the server, its cron job is inactive and recorded in `sbctl_branch.paused_cron_jobs`; the restriction survives pause/resume and reset; a `--allow-egress` branch reaches the server and keeps its job; delete lifts the restriction. Run by CI job `branching-xfs`. |
| `footprint.sh` | grows to 10, 25 and 50 projects; per size records create time, per-project PSS and RSS, system project PSS, `sbctl.slice` memory, disk per project, resume time and whole-node cold start; prints a markdown table (`$LOG_DIR/footprint.md`). |
| `install-e2e.sh` | `deploy/install.sh` on a fresh VM and everything after it: signature and checksum refusals, the install, the claim flow, a project through the API with a PAT, REST and Storage through the proxy, the pooler, an idempotent re-run, `sbctl self-update`. See `deploy/README.md`, Tests. |
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
