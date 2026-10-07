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

log "pause and resume $A"
sbctl projects pause "$A"
for svc in postgres gotrue postgrest; do
  [[ $(unit_state "sb-$svc@$A.service") == inactive ]] || fail "sb-$svc@$A is $(unit_state "sb-$svc@$A.service") after pause"
done
[[ $(project_field "$A" 'd["status"]') == INACTIVE ]] || fail "$A not INACTIVE"
[[ $(unit_state "sb-postgres@$B.service") == active ]] || fail "pausing $A disturbed $B"
sbctl projects resume "$A"
check_project "$A"

log "rotate keys of $A"
OLD=$(project_field "$A" 'd["keys"]["anon_key"]' --show-keys)
sbctl projects rotate-keys "$A" >/dev/null
NEW=$(project_field "$A" 'd["keys"]["anon_key"]' --show-keys)
RT=$(project_field "$A" 'd["ports"]["PostgREST"]')
[[ $(http_code -H "Authorization: Bearer $OLD" "http://127.0.0.1:$RT/") == 401 ]] || fail "old key still accepted"
[[ $(http_code -H "Authorization: Bearer $NEW" "http://127.0.0.1:$RT/") == 200 ]] || fail "new key rejected"

log "crash recovery: kill -9 PostgREST of $B"
PID=$(systemctl show -p MainPID --value "sb-postgrest@$B.service")
kill -9 "$PID"
for ((i = 0; i < 30; i++)); do
  [[ $(unit_state "sb-postgrest@$B.service") == active && $(systemctl show -p MainPID --value "sb-postgrest@$B.service") != "$PID" ]] && break
  sleep 1
done
sbctl projects health "$B" || fail "$B did not recover from a PostgREST crash"

log "delete both projects"
for ref in "$A" "$B"; do
  sbctl projects delete "$ref"
  for svc in postgres gotrue postgrest; do
    [[ $(unit_state "sb-$svc@$ref.service") == inactive ]] || fail "sb-$svc@$ref still $(unit_state "sb-$svc@$ref.service")"
  done
  [[ ! -e "$SBCTL_STATE/projects/$ref" ]] || fail "$ref: data directory remains"
  # The drop-in files stay (removing them needs a polkit action sbctl must not hold); the limits are lifted.
  [[ $(systemctl show -p MemoryMax --value "sb-postgres@$ref.service") == infinity ]] || fail "$ref: MemoryMax limit remains"
done
[[ $(unit_state sb-postgres@system.service) == active ]] || fail "system postgres stopped"
sbctl system status || fail "system status after deletes"

log "OK"
