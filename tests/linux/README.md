# tests/linux

Scripts for an ephemeral Ubuntu 24.04 VM (Debian 12 should work too; Ubuntu 22.04 cannot, its polkit ignores .rules files) with systemd,
cgroup v2, sudo and network access. They create the `sbctl` user, install units under
`/etc/systemd/system`, download the real artifacts and start real clusters. Do not run them
on a machine you care about; development machines run the exec-backend tests instead
(`internal/lifecycle`, `SBCTL_TEST_UNPACKED`).

| Script | What |
|---|---|
| `systemd-smoke.sh` | system init, two projects, health (units, user, slice, `MemoryMax` drop-in, GoTrue, PostgREST, bad-JWT rejection, sign-up, no passwordless TCP), pause, resume, key rotation, `kill -9` recovery, delete (units, data, drop-ins gone). Non-zero exit on any failure. |
| `fleet-smoke.sh` | the shared services under systemd: `sbctl fleet start`, one project registered as a tenant of Supavisor, Realtime and Storage, pooler logins (session and transaction port), Storage bucket, upload and signed-URL download, Realtime channel join, key rotation, `kill -9` of each service, tenant removal, project delete, `fleet stop`. Skips Studio (no slim artifact). |
| `settings-smoke.sh` | dashboard writes under systemd: with a PAT from the claim flow, saves auth settings (a new redirect URL accepted by GoTrue, a refused sign-up, a provider's client id, SMTP and a custom recovery template reaching a mail sink), a newly exposed PostgREST schema, a bigger Storage upload, Realtime `private_only` and limits (tenant row), Postgres settings through `ALTER SYSTEM` and one that needs a restart, a revoked secret key and the legacy switch (refused at once), a database password reset (direct and through the pooler), and that a pause and resume keep every setting. |
| `roles-smoke.sh` | members and roles under systemd: the claimed first user is Owner; an Administrator, a Developer and a Read-only member join through `sbctl users invite --role`; `/platform/profile/permissions` evaluated with Studio's matching rule; with PATs a Read-only member reads and is refused secrets, migrations, settings, keys, lifecycle, and its SQL fails in the database (data checked), a Developer writes SQL and migrations but not settings, an Administrator manages settings and members but not Owners, the last Owner stays; a project-scoped role; the real MCP server (`internal/api/testdata/mcp-roles.mjs`) and Supabase CLI (`SUPABASE_CLI`) as Read-only (listing works, `apply_migration` and `secrets set` refused) and as Owner; `[mail]` to a mail sink: an invitation to a new address (GoTrue invite) and to an existing account (sign-in link), each link followed to the invitation, accepted and checked. |
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
