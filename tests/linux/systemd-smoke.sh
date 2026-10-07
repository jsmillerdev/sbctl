#!/usr/bin/env bash
# systemd backend smoke test: system init, two projects, health, pause/resume, key
# rotation, crash recovery and delete, all under real systemd units as the sbctl user.
#
#   sudo SBCTL_BIN=/path/to/sbctl-linux-amd64 tests/linux/systemd-smoke.sh [--teardown]
#
# Without SBCTL_BIN the script builds sbctl with the go toolchain. It needs network
# access for the artifact downloads. Exit status is non-zero on the first failure; logs
# (journal, unit list, host facts) stay in $LOG_DIR (default /tmp/sbctl-linux-logs).
#
# Not run in development: it needs root, systemd and Linux. CI runs it on an ephemeral
# Ubuntu 24.04 VM.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1

trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

preflight
install_binary
setup_node

log "system init (downloads artifacts)"
system_init
for u in sb-postgres@system sb-gotrue@system; do
  wait_active "$u.service" 30
  [[ $(systemctl is-enabled "$u.service") == enabled ]] || fail "$u is not enabled for boot (install-units enables it)"
done
sbctl system status || fail "system status"

log "polkit: the sbctl user must not reload systemd or manage unit files"
if sudo -u "$SBCTL_USER" systemctl --no-ask-password daemon-reload 2>/dev/null; then
  fail "the sbctl user can run daemon-reload (polkit rule too broad)"
fi

log "create two projects"
REFS=()
for name in smoke-a smoke-b; do
  ref=$(create_project "$name" micro)
  [[ $ref =~ ^[a-z]{20}$ ]] || fail "bad ref '$ref'"
  REFS+=("$ref")
  log "created $name = $ref"
done
A=${REFS[0]} B=${REFS[1]}

check_project() { # REF
  local ref=$1 pg gt rt anon
  pg=$(project_field "$ref" 'd["ports"]["Postgres"]')
  gt=$(project_field "$ref" 'd["ports"]["GoTrue"]')
  rt=$(project_field "$ref" 'd["ports"]["PostgREST"]')
  anon=$(project_field "$ref" 'd["keys"]["anon_key"]' --show-keys)

  sbctl projects health "$ref" || fail "$ref: health"
  for svc in postgres gotrue postgrest; do
    local u="sb-$svc@$ref.service"
    [[ $(unit_state "$u") == active ]] || fail "$u not active"
    [[ $(systemctl show -p User --value "$u") == "$SBCTL_USER" ]] || fail "$u does not run as $SBCTL_USER"
    [[ $(systemctl show -p Slice --value "$u") == sbctl.slice ]] || fail "$u is not in sbctl.slice"
  done
  [[ $(systemctl show -p MemoryMax --value "sb-postgres@$ref.service") == 1073741824 ]] || fail "$ref: MemoryMax drop-in not applied"
  [[ $(http_code "http://127.0.0.1:$gt/health") == 200 ]] || fail "$ref: gotrue /health"
  [[ $(http_code -H "apikey: $anon" -H "Authorization: Bearer $anon" "http://127.0.0.1:$rt/") == 200 ]] || fail "$ref: postgrest /"
  [[ $(http_code -H "Authorization: Bearer not.a.jwt" "http://127.0.0.1:$rt/") == 401 ]] || fail "$ref: postgrest accepted a bad JWT"
  # Sign-up proves GoTrue wrote to the project's auth schema.
  [[ $(http_code -X POST -H "apikey: $anon" -H 'Content-Type: application/json' \
      -d "{\"email\":\"smoke-$RANDOM$RANDOM@example.com\",\"password\":\"correct-horse-battery-1\"}" "http://127.0.0.1:$gt/signup") == 200 ]] || fail "$ref: signup"
  # Passwordless TCP login is refused.
  local psql
  psql=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
  if "$psql" "host=127.0.0.1 port=$pg user=postgres dbname=postgres connect_timeout=3" -Atc 'select 1' </dev/null >/dev/null 2>&1; then
    fail "$ref: passwordless TCP login accepted"
  fi
  log "$ref: healthy"
}
check_project "$A"
check_project "$B"

# What an API or fleet unit can see, from inside its own mount namespace as the sbctl user.
sees() { # UNIT PATH: exit 0 if PATH is readable from UNIT's namespace
  local pid
  pid=$(systemctl show -p MainPID --value "$1")
  [[ $pid -gt 0 ]] || fail "$1 has no main pid"
  nsenter -t "$pid" -m -- runuser -u "$SBCTL_USER" -- test -r "$2" 2>/dev/null
}
log "containment: sb-gotrue@$A sees only the artifacts, its launcher and its own work directory"
G="sb-gotrue@$A.service"
sees "$G" "$SBCTL_STATE/projects/$A/gotrue.run" || fail "$G cannot read its own launcher"
sees "$G" "$SBCTL_STATE/projects/$A/gotrue" || fail "$G cannot read its work directory"
sees "$G" "$SBCTL_STATE/artifacts" || fail "$G cannot read the artifacts"
for hidden in "$SBCTL_STATE/backups" "$SBCTL_STATE/certs" "$SBCTL_STATE/projects/$B" "$SBCTL_STATE/projects/$A/postgres" \
    "$SBCTL_STATE/projects/$A/gotrue.env" "$SBCTL_STATE/projects/$A/postgres.env" "$SBCTL_STATE/projects/$A/postgrest.env" \
    "$SBCTL_STATE/projects/$A/postgrest.run" "$SBCTL_STATE/projects/system" /etc/sbctl; do
  if sees "$G" "$hidden"; then fail "$G can read $hidden"; fi
done
log "containment: sb-postgres@$A sees its cluster, its own backups and nothing of the node"
PGU="sb-postgres@$A.service"
sees "$PGU" "$SBCTL_STATE/projects/$A/postgres" || fail "$PGU cannot read its cluster directory"
sees "$PGU" "$SBCTL_STATE/projects/$A/postgres.run" || fail "$PGU cannot read its launcher"
for hidden in /etc/sbctl/master.key "$SBCTL_STATE/projects/$B" "$SBCTL_STATE/projects/system" "$SBCTL_STATE/projects/$A/postgres.env" \
    "$SBCTL_STATE/projects/$A/gotrue.env" "$SBCTL_STATE/certs" "$SBCTL_STATE/backups/$B"; do
  if sees "$PGU" "$hidden"; then fail "$PGU can read $hidden"; fi
done
# archive_command must still work from inside that namespace: it writes into backups/<ref>.
sees "$PGU" /etc/sbctl/config.toml || fail "$PGU cannot read config.toml, so archive_command cannot find its backend"
if [[ $(unit_state sb-studio.service) == active ]]; then
  for hidden in "$SBCTL_STATE/projects/system/supavisor.env" "$SBCTL_STATE/projects/system/storage.env" "$SBCTL_STATE/backups"; do
    if sees sb-studio.service "$hidden"; then fail "sb-studio can read $hidden"; fi
  done
fi

log "WAL archiving: a switched segment of $A reaches the backend through sbctl wal push"
PGPORT_A=$(project_field "$A" 'd["ports"]["Postgres"]')
PSQL=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
SEG=$(sudo -u "$SBCTL_USER" "$PSQL" "host=$SBCTL_STATE/projects/$A/postgres/sock port=$PGPORT_A user=supabase_admin dbname=postgres" \
  -Atc "select pg_walfile_name(pg_switch_wal() - 1)" </dev/null) || fail "$A: could not switch WAL over the cluster socket"
for ((i = 0; i < 40; i++)); do
  [[ -s "$SBCTL_STATE/backups/$A/wal/$SEG.zst" ]] && break
  sleep 1
done
[[ -s "$SBCTL_STATE/backups/$A/wal/$SEG.zst" ]] || { journalctl --no-pager -u "sb-postgres@$A" | tail -20 >&2; fail "$A: WAL segment $SEG was not archived (archive_command inside the namespace)"; }
[[ $(sudo -u "$SBCTL_USER" "$PSQL" "host=$SBCTL_STATE/projects/$A/postgres/sock port=$PGPORT_A user=supabase_admin dbname=postgres" \
  -Atc "select failed_count from pg_stat_archiver" </dev/null) == 0 ]] || fail "$A: archive_command has failed"

log "daemon: sbctl.service (sbctl serve) next to the CLI"
systemctl start sbctl.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done
[[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] || { journalctl --no-pager -u sbctl.service | tail -30 >&2; fail "the management API does not answer on the admin listener"; }
[[ $(http_code -H "Host: api.$SBCTL_DOMAIN" http://127.0.0.1/v1/projects) == 401 ]] || fail "the management API is not served at api.<domain> through the proxy"
[[ $(http_code -H "Host: api.$SBCTL_DOMAIN" http://127.0.0.1/auth/v1/settings) == 200 ]] || fail "the dashboard GoTrue is not reachable at api.<domain>/auth/v1"
PUB_A=$(project_field "$A" 'd["keys"]["publishable_key"]' --show-keys)
[[ $(http_code -H "Host: $A.api.$SBCTL_DOMAIN" -H "apikey: $PUB_A" http://127.0.0.1/auth/v1/settings) == 200 ]] || fail "$A: project API through the proxy"
[[ $(http_code -H "Host: $A.api.$SBCTL_DOMAIN" -H "apikey: sb_publishable_wrong" http://127.0.0.1/rest/v1/) == 401 ]] || fail "$A: a wrong key was not a 401"
# Studio's sign-in goes to api.<domain>/auth/v1; the Studio host answers its banner route itself.
[[ $(http_code -H "Host: studio.$SBCTL_DOMAIN" http://127.0.0.1/api/incident-banner) == 200 ]] || fail "the proxy does not answer /api/incident-banner"
# The daemon starts the timers next to its listeners, so give it a moment after the API answers.
for t in "sb-basebackup@$A.timer" "sb-basebackup@system.timer" sb-basebackup-prune.timer; do
  for ((i = 0; i < 30; i++)); do
    [[ $(unit_state "$t") == active ]] && break
    sleep 1
  done
  [[ $(unit_state "$t") == active ]] || { journalctl --no-pager -u sbctl.service | tail -20 >&2; fail "$t was not started by the daemon"; }
done

log "nightly backup: the sb-basebackup@$A service runs as the timer would"
systemctl start "sb-basebackup@$A.service" || { journalctl --no-pager -u "sb-basebackup@$A" | tail -30 >&2; fail "$A: sb-basebackup service failed"; }
[[ $(sbctl backups list "$A" | grep -c completed) -ge 1 ]] || { sbctl backups list "$A" >&2 || true; fail "$A: no completed base backup after the backup service ran"; }

log "daemon restart leaves the projects running"
PG_PID=$(systemctl show -p MainPID --value "sb-postgres@$A.service")
systemctl restart sbctl.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done
[[ $(systemctl show -p MainPID --value "sb-postgres@$A.service") == "$PG_PID" ]] || fail "restarting sbctl.service restarted a project's Postgres"
sbctl projects health "$A" || fail "$A unhealthy after the daemon restarted"

log "pause and resume $A"
sbctl projects pause "$A"
for svc in postgres gotrue postgrest; do
  [[ $(unit_state "sb-$svc@$A.service") == inactive ]] || fail "sb-$svc@$A is $(unit_state "sb-$svc@$A.service") after pause"
done
[[ $(project_field "$A" 'd["status"]') == INACTIVE ]] || fail "$A not INACTIVE"
[[ $(unit_state "sb-postgres@$B.service") == active ]] || fail "pausing $A disturbed $B"
[[ $(unit_state "sb-basebackup@$A.timer") != active ]] || fail "$A is paused but its nightly backup timer still runs"
sbctl projects resume "$A"
check_project "$A"
[[ $(unit_state "sb-basebackup@$A.timer") == active ]] || fail "$A resumed but its nightly backup timer was not started"

log "rotate keys of $A"
OLD=$(project_field "$A" 'd["keys"]["anon_key"]' --show-keys)
sbctl projects rotate-keys "$A" >/dev/null
NEW=$(project_field "$A" 'd["keys"]["anon_key"]' --show-keys)
RT=$(project_field "$A" 'd["ports"]["PostgREST"]')
[[ $(http_code -H "Authorization: Bearer $OLD" "http://127.0.0.1:$RT/") == 401 ]] || fail "old key still accepted"
[[ $(http_code -H "Authorization: Bearer $NEW" "http://127.0.0.1:$RT/") == 200 ]] || fail "new key rejected"
# The daemon's proxy learns of the rotation from the registry (LISTEN/NOTIFY), not from a restart.
NEWPUB=$(project_field "$A" 'd["keys"]["publishable_key"]' --show-keys)
for ((i = 0; i < 20; i++)); do
  [[ $(http_code -H "Host: $A.api.$SBCTL_DOMAIN" -H "apikey: $NEWPUB" http://127.0.0.1/auth/v1/settings) == 200 ]] && break
  sleep 1
done
[[ $(http_code -H "Host: $A.api.$SBCTL_DOMAIN" -H "apikey: $NEWPUB" http://127.0.0.1/auth/v1/settings) == 200 ]] || fail "the proxy does not accept the rotated publishable key"
[[ $(http_code -H "Host: $A.api.$SBCTL_DOMAIN" -H "apikey: $PUB_A" http://127.0.0.1/rest/v1/) == 401 ]] || fail "the proxy still accepts the old publishable key"

log "crash recovery: kill -9 PostgREST of $B"
PID=$(systemctl show -p MainPID --value "sb-postgrest@$B.service")
kill -9 "$PID"
for ((i = 0; i < 30; i++)); do
  [[ $(unit_state "sb-postgrest@$B.service") == active && $(systemctl show -p MainPID --value "sb-postgrest@$B.service") != "$PID" ]] && break
  sleep 1
done
sbctl projects health "$B" || fail "$B did not recover from a PostgREST crash"

# mint_dashboard_jwt SECRET: a dashboard session as GoTrue issues it (HS256 with the system
# project's secret, aud "authenticated", app_metadata.sbctl_admin), for the Management API.
mint_dashboard_jwt() {
  python3 - "$1" <<'PY'
import base64, hashlib, hmac, json, sys, time
def b64(x): return base64.urlsafe_b64encode(x).rstrip(b"=")
head = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
body = b64(json.dumps({"aud": "authenticated", "sub": "00000000-0000-4000-8000-000000000001",
                       "email": "smoke@example.test", "role": "", "app_metadata": {"sbctl_admin": True},
                       "exp": int(time.time()) + 1800}).encode())
sig = b64(hmac.new(sys.argv[1].encode(), head + b"." + body, hashlib.sha256).digest())
print((head + b"." + body + b"." + sig).decode())
PY
}
JWT=$(mint_dashboard_jwt "$(project_field system 'd["keys"]["jwt_secret"]' --show-keys)")
[[ $(http_code -H "Authorization: Bearer $JWT" http://127.0.0.1:7000/v1/projects) == 200 ]] || fail "a dashboard session is not accepted by the Management API"

log "delete both projects: $A through the Management API (the daemon's own sandbox), $B with the CLI"
for ref in "$A" "$B"; do
  if [[ $ref == "$A" ]]; then
    code=$(curl -s -o "${LOG_DIR:-/tmp}/api-delete.json" -w '%{http_code}' --max-time 900 -X DELETE -H "Authorization: Bearer $JWT" "http://127.0.0.1:7000/v1/projects/$ref" || true)
    [[ $code == 200 ]] || { cat "${LOG_DIR:-/tmp}/api-delete.json" >&2 || true; journalctl --no-pager -u sbctl.service | tail -30 >&2; fail "$ref: API delete answered $code"; }
  else
    sbctl projects delete "$ref"
  fi
  for svc in postgres gotrue postgrest; do
    [[ $(unit_state "sb-$svc@$ref.service") == inactive ]] || fail "sb-$svc@$ref still $(unit_state "sb-$svc@$ref.service")"
  done
  [[ ! -e "$SBCTL_STATE/projects/$ref" ]] || fail "$ref: data directory remains"
  # The delete took a final base backup and stopped the nightly timer.
  ls "$SBCTL_STATE/backups/$ref/base/" 2>/dev/null | grep -q . || fail "$ref: no final base backup in the backend"
  [[ $(unit_state "sb-basebackup@$ref.timer") != active ]] || fail "$ref: backup timer still runs after delete"
  # The drop-in files stay (removing them needs a polkit action sbctl must not hold); the limits are lifted.
  [[ $(systemctl show -p MemoryMax --value "sb-postgres@$ref.service") == infinity ]] || fail "$ref: MemoryMax limit remains"
done
[[ $(unit_state sb-postgres@system.service) == active ]] || fail "system postgres stopped"
sbctl system status || fail "system status after deletes"

log "daemon stops gracefully"
systemctl stop sbctl.service
[[ $(systemctl show -p Result --value sbctl.service) == success ]] || fail "sbctl.service did not stop cleanly: $(systemctl show -p Result --value sbctl.service)"
[[ $(unit_state sb-postgres@system.service) == active ]] || fail "stopping the daemon stopped the system cluster"

log "OK"
