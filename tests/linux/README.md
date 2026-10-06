# tests/linux

Scripts for an ephemeral Ubuntu 24.04 VM (22.04 and Debian 12 should work too) with systemd,
cgroup v2, sudo and network access. They create the `sbctl` user, install units under
`/etc/systemd/system`, download the real artifacts and start real clusters. Do not run them
on a machine you care about; development machines run the exec-backend tests instead
(`internal/lifecycle`, `SBCTL_TEST_UNPACKED`).

| Script | What |
|---|---|
| `systemd-smoke.sh` | system init, two projects, health (units, user, slice, `MemoryMax` drop-in, GoTrue, PostgREST, bad-JWT rejection, sign-up, no passwordless TCP), pause, resume, key rotation, `kill -9` recovery, delete (units, data, drop-ins gone). Non-zero exit on any failure. |
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
