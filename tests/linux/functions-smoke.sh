#!/usr/bin/env bash
# Edge Functions under real systemd units: system init, two projects, `sbctl functions dev`
# (Management API and proxy in one process, with sb-edge-runtime started as a unit), then
# tests/functions/run.sh, which deploys fixtures with the real `supabase functions deploy`,
# calls them with supabase-js and checks isolation between the projects. On top of that this
# script checks what only systemd can show: the unit's user, slice and memory limit, what its
# mount namespace hides, and recovery after `kill -9` of the runtime.
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
wall_clock_seconds = 30
idle_timeout_seconds = 10
reconcile_seconds = 5
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
# 16 workers (max_parallelism) x 256 MB (memory_mb) + 256 MB for the runtime itself.
[[ $(systemctl show -p MemoryMax --value "$U") == 4563402752 ]] || fail "$U: MemoryMax drop-in not applied ($(systemctl show -p MemoryMax --value "$U"))"
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
export REF_A REF_B STATE_DIR=$SBCTL_STATE RUNTIME_URL="http://127.0.0.1:$P_EDGE" WORK="$LOG_DIR/functions-work"
"$REPO_ROOT/tests/functions/run.sh" || fail "tests/functions/run.sh"

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

log "functions smoke test passed"
