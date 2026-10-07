#!/usr/bin/env bash
# Starts the integration stack (Postgres + supavise-pgmeta + the API server, all on
# 127.0.0.1 in 32100-32999) and keeps it up for real clients.
#
#   internal/api/testdata/serve-stack.sh /path/to/stack.json
#
# The file gets {api_url, pat, jwt, ref, dsn, pg_port}; delete it to stop the stack.
# Needs the unpacked slim-services artifacts (default ~/.cache/sbctl/unpacked).
set -euo pipefail
out=${1:?usage: serve-stack.sh /path/to/stack.json}
root=$(cd "$(dirname "$0")/../../.." && pwd)
guard=${GUARD:-$root/scripts/guard.sh}
# In a git worktree that predates scripts/guard.sh, use the main checkout's copy.
[[ -x $guard ]] || guard=$(cd "$(git -C "$root" rev-parse --git-common-dir)/.." && pwd)/scripts/guard.sh
art=${SUPAVISE_ARTIFACTS:-$HOME/.cache/sbctl/unpacked}
export SUPAVISE_API_INTEGRATION=1
export SUPAVISE_API_IT_PORT_BASE=${SUPAVISE_API_IT_PORT_BASE:-}
export SUPAVISE_PG_BIN=${SUPAVISE_PG_BIN:-$(ls -d "$art"/postgres-*-darwin-arm64/bin | head -1)}
export SUPAVISE_PGMETA_BIN=${SUPAVISE_PGMETA_BIN:-$(ls -d "$art"/pgmeta-*-darwin-arm64/bin/pgmeta | head -1)}
export SUPAVISE_API_SERVE_FILE=$out
bin=$(mktemp -t supavise-api-test.XXXXXX)
trap 'rm -f "$bin"' EXIT
cd "$root"
# Compile under the machine-wide heavy-job lock, then run the binary without it:
# the stack is light and must not hold the lock for the length of a session.
"$guard" -- go test -c -o "$bin" ./internal/api
"$guard" --no-lock -- "$bin" -test.run IntegrationServe -test.v -test.timeout 40m
