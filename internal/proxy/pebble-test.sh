#!/usr/bin/env bash
# Runs the Pebble ACME integration test of internal/proxy (CI only; not run on dev machines).
#
# Starts pebble and pebble-challtestsrv (built from source with Go), points Pebble's
# DNS at the challenge test server so every name resolves to 127.0.0.1, and runs
# TestPebbleIssuance (the node's own hosts) and TestPebbleCustomHostname (a customer's hostname:
# initialize, DNS verification, activation, REST and Auth over its certificate, delete) against it.
# Needs: go, git, curl, free ports 5001, 5002, 8053, 8055, 14000, 15000.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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

# Wait until Pebble's directory and the challenge test server's management API answer.
wait_for() { # url...
  for _ in $(seq 1 60); do
    if curl -sk --max-time 2 -o /dev/null "$@"; then return 0; fi
    sleep 1
  done
  echo "timed out waiting for $*" >&2
  return 1
}
wait_for https://localhost:14000/dir
wait_for -X POST -d '{}' http://127.0.0.1:8055/clear-request-history

cd "$root"
# The challenge test server is also the DNS stub of the custom-hostname test: it answers every A
# query with 127.0.0.1 and serves the TXT record the test publishes through its management API.
SUPAVISE_TEST_PEBBLE_URL=https://localhost:14000/dir \
SUPAVISE_TEST_PEBBLE_CA="$work/pebble/test/certs/pebble.minica.pem" \
SUPAVISE_TEST_CHALLTESTSRV_URL=http://127.0.0.1:8055 \
SUPAVISE_TEST_CHALLTESTSRV_DNS=127.0.0.1:8053 \
  go test -count=1 -v -run 'TestPebbleIssuance|TestPebbleCustomHostname' ./internal/proxy/
