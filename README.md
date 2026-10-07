# sbctl

One static Go binary that turns a Linux machine into a multi-project Supabase for a team. It runs Supabase's own native service artifacts as systemd units and is itself the edge proxy (automatic TLS), the Management API that Studio, the Supabase CLI and the Supabase MCP server talk to, the project lifecycle engine and the WAL archiver.

- Design: [DESIGN.md](DESIGN.md)
- Build plan and shared conventions: [HANDOFF.md](HANDOFF.md)
- Evidence: [research/](research/)

## Layout

| Path | What |
|---|---|
| `cmd/sbctl` | the binary; each area registers its subcommands in `cmd_<area>.go` |
| `internal/config` | `/etc/sbctl/config.toml`, `SBCTL_*` overrides, paths, ports, hostnames |
| `internal/secrets` | master-key sealing and every generated credential |
| `internal/registry` | control-plane store (system Postgres, schema `sbctl`) |
| `internal/app` | composition: `sbctl serve` (the daemon), backup wired into lifecycle |
| `internal/api` | Management API (`/v1`, `/v2`, `/platform`) |
| `internal/proxy` | HTTPS/WebSocket edge, CertMagic, apikey handling |
| `internal/artifacts`, `internal/units`, `internal/lifecycle` | artifact fetch, systemd units, project lifecycle |
| `internal/fleet` | Supavisor, Realtime and Storage tenant registration |
| `internal/backup` | `sbctl wal push|fetch`, base backups, point-in-time restore |
| `studio/` | platform-mode Studio build and its three patches |
| `deploy/` | installer, systemd templates, CloudFormation |
| `tests/conformance` | end-to-end suite against a running node |
| `tests/e2e` | browser check of Studio through the edge (`studio-smoke.mjs`) |
| `tests/linux` | systemd smoke test and footprint measurement (CI) |

## Configuration notes

`region` (top level, default `us-east-1`) is the AWS region code that every project reports to
Studio, the CLI and the MCP server. It is only a label here, but it must be a real region code:
Studio resolves it against a list of regions and the project list breaks on anything else (a
project created with another label gets this value). `sbctl serve` is the daemon that
`sbctl.service` runs.

## Develop

```bash
make test
```

Registry tests against Postgres run when `SBCTL_TEST_DATABASE_URL` points at an empty database.

## License

Apache-2.0. See [LICENSE](LICENSE).
