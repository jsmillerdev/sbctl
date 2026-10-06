#!/usr/bin/env bash
# Runs the Pebble ACME integration test of internal/proxy (CI only; not run on dev machines).
#
# Starts pebble and pebble-challtestsrv (built from source with Go), points Pebble's
# DNS at the challenge test server so every name resolves to 127.0.0.1, and runs
# TestPebbleIssuance against it. Needs: go, git, free ports 5001, 5002, 8053, 8055, 14000, 15000.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
pids=()
cleanup() { for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$work"; }
trap cleanup EXIT

PEBBLE_REF="${PEBBLE_REF:-v2.10.1}"
git clone --quiet --depth 1 --branch "$PEBBLE_REF" https://github.com/letsencrypt/pebble "$work/pebble"
(cd "$work/pebble" && go build -o "$work/pebble-bin" ./cmd/pebble && go build -o "$work/challtestsrv" ./cmd/pebble-challtestsrv)

cat >"$work/pebble.json" <<JSON
{"pebble": {
  "listenAddress": "127.0.0.1:14000", "managementListenAddress": "127.0.0.1:15000",
  "certificate": "$work/pebble/test/certs/localhost/cert.pem", "privateKey": "$work/pebble/test/certs/localhost/key.pem",
  "httpPort": 5002, "tlsPort": 5001, "ocspResponderURL": "", "externalAccountBindingRequired": false}}
JSON

"$work/challtestsrv" -http01 "" -https01 "" -tlsalpn01 "" -doh "" -management 127.0.0.1:8055 -dnsserver 127.0.0.1:8053 -defaultIPv4 127.0.0.1 -defaultIPv6 "" &
pids+=($!)
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 "$work/pebble-bin" -config "$work/pebble.json" -dnsserver 127.0.0.1:8053 &
pids+=($!)
sleep 3

cd "$root"
SBCTL_TEST_PEBBLE_URL=https://localhost:14000/dir \
SBCTL_TEST_PEBBLE_CA="$work/pebble/test/certs/pebble.minica.pem" \
  go test -count=1 -v -run TestPebbleIssuance ./internal/proxy/
