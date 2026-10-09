# Development

How to build, test and change Supavise. [design.md](design.md) is the contract the code follows; each package documents itself in its own `README.md`.

## Build

- Go 1.26 or later (`go.mod`). The binary is static: `CGO_ENABLED=0`.
- `make build` writes `bin/supavise` for this machine. `make build-linux` writes `bin/supavise-linux-amd64` and `bin/supavise-linux-arm64`.
- `make fmt` runs `gofmt -w` over the tracked Go files.

## Test

- `make test` runs `go vet ./...` and `go test ./...`.
- CI runs `gofmt -l`, `go vet ./...` and `go test -race -count=1 ./...` on amd64 and arm64, with a Postgres 17 service for `SUPAVISE_TEST_DATABASE_URL`.
- The Deno main service of Edge Functions (`internal/functions/mainservice`) has its own checks: `deno fmt --check`, `deno lint`, `deno check` and `deno test`.
- Node (22) runs `tests/conformance`, the Studio build and spike (`studio/`), and the OAuth walk (`tests/linux/oauth/`).

## Integration tests on a workstation

These tests skip unless their variable is set. They start real Postgres and service processes from the unpacked slim-services artifacts, so they need a machine with a few hundred MB to 1 GB free.

| Variable | Needs | Runs |
|---|---|---|
| `SUPAVISE_TEST_DATABASE_URL` | a Postgres 17 connection URL | registry, proxy and lifecycle tests against a real database |
| `SUPAVISE_TEST_UNPACKED` | directory of unpacked artifacts | lifecycle, branching and app integration tests |
| `SUPAVISE_API_INTEGRATION=1` | `SUPAVISE_PG_BIN` (Postgres `bin` directory), `SUPAVISE_PGMETA_BIN` (pgmeta binary) | `go test ./internal/api -run Integration -v`; listens on 127.0.0.1 in 32100-32999 |
| `SUPAVISE_SSO_INTEGRATION=1` | `SUPAVISE_TEST_UNPACKED`, `SUPAVISE_SSO_PYTHON` (a Python with `signxml`) | `go test ./internal/api -run IntegrationDashboardSSO -v` |
| `SUPAVISE_SETTINGS_E2E=1` | `SUPAVISE_TEST_UNPACKED` | `TestSettingsIntegration` in `internal/app` |

The unpacked artifacts default to `~/.cache/sbctl/unpacked`, a path the test code reads (`serve-stack.sh` and the backup tests use it when no variable is set). The backup tests also accept `SUPAVISE_TEST_PG_BIN`.

`internal/api/testdata/serve-stack.sh /path/to/stack.json` starts the API integration stack (Postgres, pgmeta and the API server on 127.0.0.1) and keeps it running for real CLI and MCP clients. It writes `{api_url, pat, jwt, ref, dsn, pg_port}` to the file; delete the file to stop the stack.

`node tests/linux/oauth/selftest.mjs [--mcp]` runs the OAuth walk of the Linux job against a stand-in node (`fake-node.mjs`) with no variables, no root and no Postgres. It checks the walk against [design.md](design.md) section 13, so run it after changing the walk and before pushing, instead of waiting for CI.

## CI

| Workflow | Jobs |
|---|---|
| `ci` | `test`, `cfn-lint`, `aws-deploy`, `actionlint`, `functions-main` |
| `linux` | `systemd-smoke`, `backup-integration`, `pebble`, `fleet-smoke`, `settings-smoke`, `compute-smoke`, `backups-smoke`, `upgrade-smoke`, `upgrade-e2e`, `roles-smoke`, `sso-smoke`, `install-e2e`, `os-updates`, `functions-smoke`, `branching-xfs` (which also runs `branching-egress`), `converge-e2e`, `firstboot-e2e`, `cluster-identity`, `replica-blocks`, `fleet-follower`, `storage-migrate`, `oauth-smoke` |
| `conformance` | `suites`, `specdiff`, `bump-check` |
| `studio` | `build` (runs `studio/build.sh`, then `studio/spike.sh`), `mcp-e2e` (the OAuth walk and MCP over HTTP on the amd64 build; `tests/linux/oauth-smoke.sh --studio-artifact`) |
| `footprint` | manual; see [reference/footprint.md](reference/footprint.md) |
| `bump-proposals` | nightly; opens one pull request per upstream release newer than its pin |
| `release` | on a `v*` tag |
| `replication-spike` | the two-node Incus harness of `tests/linux/multi` on both architectures |
| `screenshots` | README screenshots |

## Conventions

Rules the code follows. A change that breaks one needs a reason in [design.md](design.md).

| Thing | Convention |
|---|---|
| Binary and user | `supavise`; units run as the system user `supavise` (the one-shot `supavise-edge-bundle@` unit uses a dynamic uid), so artifacts are relocatable and nothing needs root after install |
| Config | `/etc/supavise/config.toml`, `SUPAVISE_*` environment overrides, secrets key `/etc/supavise/master.key` (mode 0600) |
| State root | `/var/lib/supavise/`: `artifacts/<service>/<version>/`, `projects/<ref>/<service>/` with env files at `projects/<ref>/<service>.env` (0600), `system/<service>/` for fleet services, `certs/`, `backups/` (local backend). The Edge Functions tree is `system/edge-runtime/tenants/<ref>` |
| Logs | journald; the unit name selects the log |
| Units | templates `supavise-postgres@`, `supavise-gotrue@`, `supavise-postgrest@`; singletons `supavise-supavisor`, `-realtime`, `-storage`, `-pgmeta`, `-studio`, optional `-imgproxy` and `-edge-runtime`; all in `supavise.slice`, with `MemoryMax` and `CPUQuota` per project |
| Ref | 20 lowercase ASCII letters; `system` is the reserved ref of the system project |
| Ports | project services listen on loopback: Postgres `20000 + 3n`, GoTrue `20001 + 3n`, PostgREST `20002 + 3n` (`n` is the project's registry sequence). Public: Supavisor 5432 (session) and 6543 (transaction), the proxy on 80 and 443. Loopback: Realtime 4000, Storage 5000 (admin 5001), imgproxy 5002, Studio 3000, pgmeta 8080, edge-runtime 9000, system Postgres 5433, system GoTrue 9999, admin API 7000, the Storage credential endpoint 4010 (`[fleet] storage_credentials_port`). A replica of project `n` on a node that is not its home: Postgres `10000 + 3n`, PostgREST `10002 + 3n` (`[ports] replica_base`), and the standby of the system cluster on `10000`. Between the nodes of a cluster: TCP 7443 (`[node] peer_listen`). All are set in `internal/config/layout.go` and changeable in `[ports]` |
| Hostnames | project `<ref>.api.<domain>`, replica `<ref>-rr-<region>-<id6>.api.<domain>`, load balancer `<ref>-lb.api.<domain>`, dashboard `studio.<domain>`, API `api.<domain>`, pooler `pooler.<domain>`; Realtime gets `Host: <ref>.realtime.internal`, Storage gets `x-forwarded-host: <ref>.api.<domain>`. Without a domain, `<ip>.sslip.io` is the base domain |
| Keys | per project: a 40-character JWT secret, legacy `anon` and `service_role` JWTs, and `sb_publishable_<base58>` and `sb_secret_<base58>` keys that the proxy maps to the legacy JWTs. Personal access tokens are `sbp_` plus 40 lowercase hex characters. OAuth access tokens are `sbp_oauth_` plus 40 hex, refresh tokens `sbr_` plus 64 hex, authorization codes `sbc_` plus 64 hex and client secrets `sba_` plus 64 hex; they live in the `oauth_*` tables and not in `access_tokens` |
| Registry | database `supavise` in the system cluster, schema `supavise`. Migrations in `internal/registry/migrations` are embedded in the binary and applied in file-name order; number ranges per area are listed in `0001_init.sql` and continue in the later files. Migrations from 1300 on only add (create a table or index, add a column with a default), so a binary one minor behind runs against the newer schema; `migrations_lint_test.go` enforces it |
| Versions | `internal/versions/versions.yaml` is the only pin source: `artifacts.<service>`, `studio.tag`, `cli.version_tested` |
| API types | generated from the three Management API specs (`v1`, `v2`, `platform`) in `internal/api/gen/` with `go generate`; never hand-written. Refresh the specs with `internal/api/gen/fetch-specs.sh` |
| Interfaces | `lifecycle.DataPlane` (`Create`, `Delete`, `Snapshot`, `Route`, `Usage`), `fleet.Tenant` (`EnsureTenant`, `RemoveTenant`), `units.Supervisor` (`Render`, `Start`, `Stop`, `Status`, `Remove`), `secrets.Secrets` (`Seal`, `Open`), `backup.Backup` (`PushWAL`, `FetchWAL`, `BaseBackup`, `Restore`) |
| Errors | the Management API envelope `{"message": "..."}` with the upstream status codes |
| License | Apache-2.0 for everything written here; the Studio patches carry upstream's Apache-2.0 |
| Provenance | no code copied from `kmhari/supastack` (AGPL); reading it for reference is fine |
| Name | the product name must not contain "Supabase"; Supavise is not affiliated with Supabase Inc. |

**Decided.** Do not reintroduce these without a written reason in [design.md](design.md) section 10: one Postgres cluster per project; native artifacts and no Docker; the proxy inside the binary and no Envoy; Supavisor as the only Postgres entry point; WAL archiving through the binary and no wal-g; wildcard DNS required; the platform-mode Studio build with exactly four patches (three upstreamable, and patch 0004 that serves the MCP route; the reason is in section 10); API types generated from the specs.

## Releases

A `v*` tag starts `.github/workflows/release.yml`: it checks that the commit has passed its tests, builds the binaries and the Studio artifact, signs the checksum list and publishes the release. Signing key setup and the release steps are in [deploy/README.md](../deploy/README.md), "Release signing".
