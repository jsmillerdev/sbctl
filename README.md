# Supavise

One static Go binary that turns a Linux machine into a multi-project Supabase for a team. It runs Supabase's own native service artifacts as systemd units and is itself the edge proxy (automatic TLS), the Management API that Studio, the Supabase CLI and the Supabase MCP server talk to, the project lifecycle engine and the WAL archiver.

- Design: [DESIGN.md](DESIGN.md)
- Build plan and shared conventions: [HANDOFF.md](HANDOFF.md)
- Evidence: [research/](research/)

## Layout

| Path | What |
|---|---|
| `cmd/supavise` | the binary; each area registers its subcommands in `cmd_<area>.go` |
| `internal/config` | `/etc/supavise/config.toml`, `SUPAVISE_*` overrides, paths, ports, hostnames |
| `internal/secrets` | master-key sealing and every generated credential |
| `internal/registry` | control-plane store (system Postgres, schema `supavise`) |
| `internal/app` | composition: `supavise serve` (the daemon), backup wired into lifecycle |
| `internal/api` | Management API (`/v1`, `/v2`, `/platform`) |
| `internal/proxy` | HTTPS/WebSocket edge, CertMagic, apikey handling |
| `internal/artifacts`, `internal/units`, `internal/lifecycle` | artifact fetch, systemd units, project lifecycle |
| `internal/fleet` | Supavisor, Realtime and Storage tenant registration |
| `internal/backup` | `supavise wal push|fetch`, base backups, point-in-time restore |
| `studio/` | platform-mode Studio build and its three patches |
| `deploy/` | installer, systemd templates, CloudFormation |
| `tests/conformance` | end-to-end suite against a running node |
| `tests/e2e` | browser check of Studio through the edge (`studio-smoke.mjs`) |
| `tests/linux` | systemd smoke test and footprint measurement (CI) |

## Configuration notes

`region` (top level, default `us-east-1`) is the AWS region code that every project reports to
Studio, the CLI and the MCP server. It is only a label here, but it must be one of the 17 codes
in Studio's region table (`AWS_REGIONS` in `packages/shared-data/regions.ts` at the pinned Studio
commit; `config.Regions` copies it): Studio resolves it against that table and the project list breaks
on anything else, including real AWS regions such as `eu-south-1` (a project created with another
label gets this value).

No project's units read `/etc/supavise` or the backups: a cluster's `archive_command` and
`restore_command` talk to the daemon over a unix socket in the project's own directory
(`[backup] wal_relay`, on under systemd), and the daemon holds the backup credentials; every `supavise-*`
unit is denied the cloud instance metadata service, so the EC2 instance role is out of reach of SQL,
`pg_net` and the like. What the per-unit sandbox does and does not isolate (same uid, `/proc`) is
spelled out in `deploy/systemd/README.md`; do not take it for a boundary against code execution
inside a unit.

`supavise serve` is the daemon that `supavise.service` runs. On SIGTERM it refuses new lifecycle
operations (503) and waits up to 10 minutes for running ones (a delete with its final backup, a
restart) before it exits; `supavise.service` allows 11 minutes to stop. An operation cut off anyway
is finished or reverted at the next start. At boot it waits up to 2 minutes for the registry.

## Develop

```bash
make test
```

Registry tests against Postgres run when `SUPAVISE_TEST_DATABASE_URL` points at an empty database.

## License

Apache-2.0. See [LICENSE](LICENSE).

Supavise is not affiliated with or endorsed by Supabase Inc.
