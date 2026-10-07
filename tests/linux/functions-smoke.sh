#!/usr/bin/env bash
# Edge Functions under real systemd units: system init, two projects, `sbctl functions dev`
# (Management API and proxy in one process, with sb-edge-runtime started as a unit), then
# tests/functions/run.sh, which deploys fixtures with the real `supabase functions deploy` (project
# A bundled by the CLI in Docker, project B uploaded as sources with --use-api and bundled by the
# node in the sandbox of sb-edge-bundle.service), calls them with supabase-js and checks isolation
# between the projects. On top of that this script checks what only systemd can show: the unit's
# user, slice and memory limit, what its mount namespace hides, recovery after `kill -9` of the
# runtime, and that the runtime-wide worker budget refuses what would not fit.
#
#   sudo SBCTL_BIN=/path/to/sbctl-linux-amd64 tests/linux/functions-smoke.sh [--teardown]
#
# Needs network access (artifacts, the Supabase CLI release, npm, and DNS for
# *.127.0.0.1.sslip.io, which the functions use to reach their own project). Logs stay in
# $LOG_DIR (default /tmp/sbctl-linux-logs).
#
# Not run in development: it needs root, systemd and Linux. CI runs it on an ephemeral Ubuntu
# 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1

DEV_PID=""
trap 'rc=$?; [[ -n $DEV_PID ]] && kill -TERM "$DEV_PID" 2>/dev/null; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

DOMAIN=127.0.0.1.sslip.io
P_HTTP=18080 P_HTTPS=18443 P_ADMIN=18082 P_EDGE=19000
SUPABASE_CLI_VERSION=${SUPABASE_CLI_VERSION:-2.119.0}

SBCTL_DOMAIN=$DOMAIN
preflight
getent hosts "probe.$DOMAIN" >/dev/null || fail "DNS for *.$DOMAIN does not resolve here (sslip.io); the functions reach their own project through it"
for c in node npm; do command -v "$c" >/dev/null || fail "missing $c"; done
install_binary
setup_node
cat >>"$SBCTL_CONF" <<CONF

[listen]
http = "127.0.0.1:$P_HTTP"
https = "127.0.0.1:$P_HTTPS"
admin = "127.0.0.1:$P_ADMIN"

[ports]
edge_runtime = $P_EDGE

[functions]
enabled = true
# Longer than the whole run: the runtime retires a worker at its wall clock, and a request that
# reaches it while it drains (graceful_exit_timeout, 10 s) waits for the idle timeout. That is the
# runtime's behavior, not what these checks are about, so no worker may reach it mid-run.
wall_clock_seconds = 600
idle_timeout_seconds = 10
reconcile_seconds = 5
# Low, so that the flood check of verify.mjs can exceed it with a small flood (the default is 128).
max_per_project = 8
# The checks of run.sh keep about 15 functions of two projects warm at once (every function of
# project A and a new worker for each after a secret changes), more than the defaults (16 workers,
# 8 per project) allow. The budget stage at the end runs with small values.
max_workers = 24
max_workers_per_project = 16
CONF

# Same finding as in fleet-smoke.sh: the Postgres launcher chmods a file inside the artifact
# on its first boot, which ProtectSystem=strict forbids. VM-local drop-in, to be removed once
# sb-postgres@.service handles it.
install -d /etc/systemd/system/sb-postgres@.service.d
cat >/etc/systemd/system/sb-postgres@.service.d/10-functions-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/sbctl/artifacts
CONF
systemctl daemon-reload

install_supabase_cli() {
  command -v supabase >/dev/null && [[ $(supabase --version 2>/dev/null | head -1) == *"$SUPABASE_CLI_VERSION"* ]] && return 0
  local arch; case $(uname -m) in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) fail "unsupported architecture $(uname -m)" ;; esac
  local base="https://github.com/supabase/cli/releases/download/v$SUPABASE_CLI_VERSION" tgz="supabase_${SUPABASE_CLI_VERSION}_linux_${arch}.tar.gz" d
  d=$(mktemp -d)
  log "downloading the Supabase CLI $SUPABASE_CLI_VERSION ($arch)"
  curl -fsSL --retry 3 -o "$d/$tgz" "$base/$tgz" || fail "download $tgz"
  curl -fsSL --retry 3 -o "$d/checksums.txt" "$base/supabase_${SUPABASE_CLI_VERSION}_checksums.txt" \
    || curl -fsSL --retry 3 -o "$d/checksums.txt" "$base/checksums.txt" || fail "download the checksums"
  (cd "$d" && grep " $tgz\$" checksums.txt | sha256sum -c -) || fail "checksum of $tgz"
  tar -xzf "$d/$tgz" -C "$d" supabase
  install -m 0755 "$d/supabase" /usr/local/bin/supabase
  supabase --version >&2 || true
}
install_supabase_cli

log "system init (downloads artifacts)"
system_init
wait_active sb-postgres@system.service 30

log "two projects"
REF_A=$(create_project fn-a micro)
REF_B=$(create_project fn-b micro)
[[ $REF_A =~ ^[a-z]{20}$ && $REF_B =~ ^[a-z]{20}$ ]] || fail "bad refs '$REF_A' '$REF_B'"

log "sbctl functions dev (API, proxy, and sb-edge-runtime as a unit)"
mkdir -p "$LOG_DIR"
PAT_FILE=$SBCTL_STATE/functions-dev.pat
sudo -u "$SBCTL_USER" -H /usr/local/bin/sbctl functions dev --token-file "$PAT_FILE" >"$LOG_DIR/functions-dev.log" 2>&1 &
DEV_PID=$!
for ((i = 0; i < 180; i++)); do
  kill -0 "$DEV_PID" 2>/dev/null || { tail -30 "$LOG_DIR/functions-dev.log" >&2; fail "sbctl functions dev exited"; }
  [[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 && -s $PAT_FILE ]] && break
  sleep 1
done
[[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 ]] || { tail -30 "$LOG_DIR/functions-dev.log" >&2; fail "the edge runtime did not come up"; }

U=sb-edge-runtime.service
log "the unit"
[[ $(unit_state "$U") == active ]] || fail "$U is $(unit_state "$U")"
[[ $(systemctl show -p User --value "$U") == "$SBCTL_USER" ]] || fail "$U does not run as $SBCTL_USER"
[[ $(systemctl show -p Slice --value "$U") == sbctl.slice ]] || fail "$U is not in sbctl.slice"
# 24 workers (max_workers) x (256 MB memory_mb + 32 MB overhead) + 256 MB for the runtime itself.
[[ $(systemctl show -p MemoryMax --value "$U") == 7516192768 ]] || fail "$U: MemoryMax drop-in not applied ($(systemctl show -p MemoryMax --value "$U"))"
[[ $(ss -Hltn "sport = :$P_EDGE" | awk '{print $4}') == "127.0.0.1:$P_EDGE" ]] || fail "the runtime does not listen on loopback only: $(ss -Hltn "sport = :$P_EDGE")"

sees() { # PATH: exit 0 if PATH is readable from the unit's namespace as the sbctl user
  local pid; pid=$(systemctl show -p MainPID --value "$U")
  [[ $pid -gt 0 ]] || fail "$U has no main pid"
  nsenter -t "$pid" -m -- runuser -u "$SBCTL_USER" -- test -r "$1" 2>/dev/null
}
for hidden in "$SBCTL_STATE/backups" "$SBCTL_STATE/certs" /etc/sbctl; do
  if sees "$hidden"; then fail "$U can read $hidden"; fi
done
# The runtime sees the tree of functions (its own state directory) and nothing of the projects.
sees "$SBCTL_STATE/system/edge-runtime/tenants" || fail "$U cannot see its tenants directory"
# (projects/system exists as the parent of its launcher script, with nothing else in it.)
for hidden in "$SBCTL_STATE/projects/$REF_A" "$SBCTL_STATE/projects/$REF_A/postgres" "$SBCTL_STATE/projects/system/postgres" "$SBCTL_STATE/projects/system/postgres.env"; do
  if sees "$hidden"; then fail "$U can read $hidden"; fi
done

log "tests/functions/run.sh"
export SBCTL_RUN="sudo -u $SBCTL_USER -H /usr/local/bin/sbctl" AS_SBCTL="sudo -u $SBCTL_USER"
export API_URL="http://api.$DOMAIN:$P_HTTP" PAT_FILE PROJECT_URL="http://{ref}.api.$DOMAIN:$P_HTTP"
export REF_A REF_B STATE_DIR=$SBCTL_STATE RUNTIME_URL="http://127.0.0.1:$P_EDGE" WORK="$LOG_DIR/functions-work" SANDBOXED_BUNDLER=1 MAX_PER_PROJECT=8
"$REPO_ROOT/tests/functions/run.sh" || fail "tests/functions/run.sh"

log "the bundler unit ran for project B's uploads and is idle now"
B=sb-edge-bundle.service
[[ $(unit_state "$B") == inactive ]] || fail "$B is $(unit_state "$B")"
[[ $(systemctl show -p Result --value "$B") == success ]] || fail "$B: last result $(systemctl show -p Result --value "$B")"
[[ $(systemctl show -p User --value "$B") == "$SBCTL_USER" ]] || fail "$B does not run as $SBCTL_USER"
[[ $(systemctl show -p MemoryMax --value "$B") == 1073741824 ]] || fail "$B: MemoryMax $(systemctl show -p MemoryMax --value "$B")"
systemctl show -p IPAddressDeny --value "$B" | grep -q . || fail "$B has no IPAddressDeny"
[[ ! -e $SBCTL_STATE/system/edge-bundle/work ]] || fail "the scratch directory of an upload stayed in $SBCTL_STATE/system/edge-bundle"

log "crash recovery: kill -9 of the runtime"
open_code() { http_code "http://$REF_A.api.$DOMAIN:$P_HTTP/functions/v1/open"; }
[[ $(open_code) == 200 ]] || fail "open before the crash: $(open_code)"
pid=$(systemctl show -p MainPID --value "$U")
kill -9 "$pid"
ok=0
for ((i = 0; i < 60; i++)); do
  sleep 1
  new=$(systemctl show -p MainPID --value "$U")
  if [[ $new -gt 0 && $new != "$pid" && $(unit_state "$U") == active && $(open_code) == 200 ]]; then ok=1; break; fi
done
[[ $ok -eq 1 ]] || fail "$U did not come back and serve after kill -9"
log "$U recovered (MemoryCurrent $(( $(systemctl show -p MemoryCurrent --value "$U") / 1048576 )) MiB)"

log "worker budget: restart with max_workers = 4 and 3 per project"
# One runtime serves every project and sbctl-main enforces what fits into its memory limit:
# at most 4 live workers (4 x 288 MB + 256 MB = the unit's MemoryMax), at most 3 of one project.
# Over-budget requests are refused with 503, nothing is killed, warm functions keep answering.
kill -TERM "$DEV_PID"; wait "$DEV_PID" 2>/dev/null || true; DEV_PID=""
for ((i = 0; i < 60; i++)); do [[ $(unit_state "$U") == active ]] || break; sleep 1; done
sed -i -e 's/^max_workers = 24$/max_workers = 4/' -e 's/^max_workers_per_project = 16$/max_workers_per_project = 3/' "$SBCTL_CONF"
grep -q '^max_workers = 4$' "$SBCTL_CONF" && grep -q '^max_workers_per_project = 3$' "$SBCTL_CONF" || fail "could not set max_workers"
rm -f "$PAT_FILE"
sudo -u "$SBCTL_USER" -H /usr/local/bin/sbctl functions dev --token-file "$PAT_FILE" >"$LOG_DIR/functions-dev-budget.log" 2>&1 &
DEV_PID=$!
for ((i = 0; i < 180; i++)); do
  kill -0 "$DEV_PID" 2>/dev/null || { tail -30 "$LOG_DIR/functions-dev-budget.log" >&2; fail "sbctl functions dev exited"; }
  [[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 && -s $PAT_FILE ]] && break
  sleep 1
done
[[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 ]] || fail "the edge runtime did not come back with the budget"
[[ $(systemctl show -p MemoryMax --value "$U") == 1476395008 ]] || fail "$U: MemoryMax for 4 workers is $(systemctl show -p MemoryMax --value "$U"), want 1476395008 (4 x 288 + 256 MiB)"

REF_C=$(create_project fn-c micro)
PAT=$(<"$PAT_FILE")
API_URL="http://api.$DOMAIN:$P_HTTP"
# One real bundle (project A's own `open`, as the node materialized it), uploaded as many slugs as needed.
node -e '
  const z = require("node:zlib"), fs = require("node:fs")
  const raw = fs.readFileSync(process.argv[1])
  fs.writeFileSync(process.argv[2], Buffer.concat([Buffer.from("EZBR"), z.brotliCompressSync(raw)]))' \
  "$SBCTL_STATE/system/edge-runtime/tenants/$REF_A/functions/open/bundle.eszip" "$LOG_DIR/budget.ezbr" || fail "could not make the bundle"
entry=$(curl -s -H "Authorization: Bearer $PAT" "$API_URL/v1/projects/$REF_A/functions/open" | python3 -c 'import json,sys; print(json.load(sys.stdin)["entrypoint_path"])')
[[ -n $entry ]] || fail "no entrypoint_path for A/open"
entry_q=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$entry")
upload() { # REF SLUG
  [[ $(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $PAT" -H 'Content-Type: application/vnd.denoland.eszip' \
    --data-binary "@$LOG_DIR/budget.ezbr" "$API_URL/v1/projects/$1/functions?slug=$2&name=$2&verify_jwt=false&entrypoint_path=$entry_q") == 201 ]] || fail "upload $1/$2"
}
for s in b1 b2 b3 b4; do upload "$REF_A" "$s"; done
for s in c1 c2; do upload "$REF_C" "$s"; done
fn_code() { http_code --max-time 30 "http://$1.api.$DOMAIN:$P_HTTP/functions/v1/$2"; }
fn_err() { curl -s -D - -o /dev/null --max-time 30 "http://$1.api.$DOMAIN:$P_HTTP/functions/v1/$2" | tr -d '\r' | awk -F': ' 'tolower($1)=="sb-error-code" {print $2}'; }
for s in b1 b2 b3; do [[ $(fn_code "$REF_A" "$s") == 200 ]] || fail "A/$s did not answer 200 within the budget: $(fn_code "$REF_A" "$s")"; done
[[ $(fn_code "$REF_A" b4) == 503 && $(fn_err "$REF_A" b4) == PROJECT_AT_CAPACITY ]] || fail "A/b4 (a 4th function of one project) was not refused: $(fn_code "$REF_A" b4)"
[[ $(fn_code "$REF_C" c1) == 200 ]] || fail "C/c1 (the 4th worker of the runtime) answered $(fn_code "$REF_C" c1)"
[[ $(fn_code "$REF_C" c2) == 503 && $(fn_err "$REF_C" c2) == PROJECT_AT_CAPACITY ]] || fail "C/c2 (a 5th worker for the runtime) was not refused: $(fn_code "$REF_C" c2)"
for s in b1 b2 b3; do [[ $(fn_code "$REF_A" "$s") == 200 ]] || fail "warm A/$s stopped answering"; done
[[ $(fn_code "$REF_C" c1) == 200 ]] || fail "warm C/c1 stopped answering"
[[ $(unit_state "$U") == active ]] || fail "$U is $(unit_state "$U") after the budget was spent"
mem=$(systemctl show -p MemoryCurrent --value "$U")
log "$U holds 4 workers: MemoryCurrent $((mem / 1048576)) MiB of 1408"
(( mem < 1476395008 )) || fail "$U uses $mem bytes, over its limit"
[[ $(systemctl show -p NRestarts --value "$U") == 0 ]] || fail "$U restarted"

log "functions smoke test passed"
