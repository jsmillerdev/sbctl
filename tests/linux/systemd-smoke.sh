#!/usr/bin/env bash
# systemd backend smoke test: system init, two projects, health, containment from inside the
# unit namespaces, WAL archiving through the daemon's relay (fail-closed with the daemon down), the
# instance metadata service denied to every unit, a project created through the Management API,
# a restore through the relay, pause/resume, key rotation, crash recovery and delete, all under
# real systemd units as the sbctl user.
#
#   sudo SBCTL_BIN=/path/to/sbctl-linux-amd64 tests/linux/systemd-smoke.sh [--teardown]
#
# Without SBCTL_BIN the script builds sbctl with the go toolchain. It needs network
# access for the artifact downloads. Exit status is non-zero on the first failure; logs
# (journal, unit list, host facts) stay in $LOG_DIR (default /tmp/sbctl-linux-logs).
#
# Not run in development: it needs root, systemd and Linux. CI runs it on an ephemeral
# Ubuntu 24.04 VM.
# shellcheck source=lib.sh
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
log "containment: sb-postgres@$A sees its cluster and its own WAL relay directory, and nothing of the node"
PGU="sb-postgres@$A.service"
sees "$PGU" "$SBCTL_STATE/projects/$A/postgres" || fail "$PGU cannot read its cluster directory"
sees "$PGU" "$SBCTL_STATE/projects/$A/postgres.run" || fail "$PGU cannot read its launcher"
sees "$PGU" "$SBCTL_STATE/projects/$A/wal" || fail "$PGU cannot see its WAL relay directory"
# No backend credentials and no backups: config.toml may hold the S3 key, and the shared backups
# directory holds every project's WAL and base backups. WAL leaves through the relay socket.
for hidden in /etc/sbctl /etc/sbctl/config.toml /etc/sbctl/master.key /run/dbus/system_bus_socket "$SBCTL_STATE/projects/$B" "$SBCTL_STATE/projects/$B/wal" \
    "$SBCTL_STATE/projects/system" "$SBCTL_STATE/projects/system/wal" "$SBCTL_STATE/projects/$A/postgres.env" \
    "$SBCTL_STATE/projects/$A/gotrue.env" "$SBCTL_STATE/certs" "$SBCTL_STATE/backups" "$SBCTL_STATE/backups/$A" "$SBCTL_STATE/backups/$B"; do
  if sees "$PGU" "$hidden"; then fail "$PGU can read $hidden"; fi
done
if nsenter -t "$(systemctl show -p MainPID --value "$PGU")" -m -- runuser -u "$SBCTL_USER" -- sh -c "echo x > '$SBCTL_STATE/projects/$A/wal/probe'" 2>/dev/null; then
  fail "$PGU can write into its WAL relay directory (it must be read-only)"
fi
if [[ $(unit_state sb-studio.service) == active ]]; then
  for hidden in "$SBCTL_STATE/projects/system/supavisor.env" "$SBCTL_STATE/projects/system/storage.env" "$SBCTL_STATE/backups"; do
    if sees sb-studio.service "$hidden"; then fail "sb-studio can read $hidden"; fi
  done
fi

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

PSQL=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
pg_admin() { # REF SQL: run SQL as supabase_admin over the cluster's private socket
  local ref=$1 port
  port=$(project_field "$ref" 'd["ports"]["Postgres"]')
  sudo -u "$SBCTL_USER" "$PSQL" "host=$SBCTL_STATE/projects/$ref/postgres/sock port=$port user=supabase_admin dbname=postgres" -Atc "$2" </dev/null
}
# switch_wal REF: writes a WAL record and switches to a new segment, so the switch is never a
# no-op on an idle cluster; prints the name of the segment that was just completed.
switch_wal() { pg_admin "$1" "select pg_walfile_name(pg_switch_wal() - 1) from (select pg_logical_emit_message(true, 'smoke', 'x')) s"; }
wait_archived() { # REF SEGMENT SECONDS: the segment is in the file backend
  local ref=$1 seg=$2 n=${3:-40} i
  for ((i = 0; i < n; i++)); do
    [[ -s "$SBCTL_STATE/backups/$ref/wal/$seg.zst" ]] && return 0
    sleep 1
  done
  return 1
}

log "WAL archiving: a switched segment of $A reaches the backend through the daemon's relay, not through the cluster's unit"
for ref in system "$A" "$B"; do
  [[ -S "$SBCTL_STATE/projects/$ref/wal/r.sock" ]] || fail "$ref: the daemon serves no WAL relay socket"
done
archive_cmd=$(pg_admin "$A" "show archive_command")
[[ $archive_cmd == *"--socket $SBCTL_STATE/projects/$A/wal/r.sock"* && $archive_cmd != *--config* ]] || fail "$A: archive_command is not the relay form: $archive_cmd"
SEG=$(switch_wal "$A") || fail "$A: could not switch WAL over the cluster socket"
wait_archived "$A" "$SEG" 90 || { journalctl --no-pager -u "sb-postgres@$A" -u sbctl.service | tail -30 >&2; fail "$A: WAL segment $SEG was not archived through the relay"; }
[[ $(pg_admin "$A" "select last_archived_wal is not null from pg_stat_archiver") == t ]] || fail "$A: pg_stat_archiver shows no archived WAL"
# From inside the cluster's own namespace: its socket works, another project's is not there, and
# the socket of $A refuses to read $B's archive.
PGPID=$(systemctl show -p MainPID --value "$PGU")
inns() { nsenter -t "$PGPID" -m -- runuser -u "$SBCTL_USER" -- "$@"; }
[[ $(inns curl -sS -m 10 -o /dev/null -w '%{http_code}' --unix-socket "$SBCTL_STATE/projects/$A/wal/r.sock" http://relay/v1/ping) == 204 ]] || fail "$A: the relay does not answer from inside the unit's namespace"
if inns test -e "$SBCTL_STATE/projects/$B/wal/r.sock"; then fail "$PGU sees the WAL relay socket of $B"; fi
[[ $(inns curl -sS -m 10 -o /dev/null -w '%{http_code}' --unix-socket "$SBCTL_STATE/projects/$A/wal/r.sock" \
  "http://relay/v1/wal/fetch?ref=$B&name=000000010000000000000001") == 403 ]] || fail "$A's relay served the archive of $B"
[[ $(inns curl -sS -m 10 -o /dev/null -w '%{http_code}' --unix-socket "$SBCTL_STATE/projects/$A/wal/r.sock" -X POST --data-binary x \
  "http://relay/v1/wal/push?ref=$B&name=000000010000000000000001") == 403 ]] || fail "$A's relay accepted a push for $B"

# The relay also checks who is on the other end (SO_PEERCRED and the peer's cgroup), because
# another unit can reach this socket through /proc/<pid>/root. A curl moved into $B's postgres cgroup
# sees $A's socket path (this shell's mount namespace) but is refused; one in $A's cgroup is served.
relay_ping_from_unit() { # UNIT: prints the HTTP status of a ping to $A's relay from a process in UNIT's cgroup, 000 if refused
  local cg; cg=$(systemctl show -p ControlGroup --value "$1")
  [[ -n $cg && -w /sys/fs/cgroup$cg/cgroup.procs ]] || fail "$1: no writable cgroup ($cg)"
  bash -c 'echo $$ >"$1" && exec curl -sS -m 5 -o /dev/null -w "%{http_code}" --unix-socket "$2" http://relay/v1/ping' _ \
    "/sys/fs/cgroup$cg/cgroup.procs" "$SBCTL_STATE/projects/$A/wal/r.sock" 2>/dev/null || true
}
[[ $(relay_ping_from_unit "$PGU") == 204 ]] || fail "$A's own postgres unit is refused by its relay"
[[ $(relay_ping_from_unit "sb-postgres@$B.service") != 204 ]] || fail "the relay of $A served a process of $B's postgres unit"
[[ $(relay_ping_from_unit "sb-postgrest@$A.service") != 204 ]] || fail "the relay of $A served a process of its PostgREST unit"

log "WAL archiving fails closed when the daemon is down, and resumes when it is back"
systemctl stop sbctl.service
SEG2=$(switch_wal "$A") || fail "$A: could not switch WAL"
FAILS0=$(pg_admin "$A" "select failed_count from pg_stat_archiver")
for ((i = 0; i < 30; i++)); do
  [[ $(pg_admin "$A" "select failed_count from pg_stat_archiver") -gt $FAILS0 ]] && break
  sleep 1
done
[[ $(pg_admin "$A" "select failed_count from pg_stat_archiver") -gt $FAILS0 ]] || fail "$A: archive_command did not fail while the daemon was down (WAL must not be buffered anywhere else)"
[[ ! -e "$SBCTL_STATE/backups/$A/wal/$SEG2.zst" ]] || fail "$A: $SEG2 reached the backend with the daemon down"
systemctl start sbctl.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done
# Postgres retries by itself (the archiver waits up to a minute between rounds).
wait_archived "$A" "$SEG2" 150 || { journalctl --no-pager -u "sb-postgres@$A" -u sbctl.service | tail -30 >&2; fail "$A: $SEG2 was not archived after the daemon came back"; }

log "instance metadata: denied to every sb-* unit that runs tenant code, from inside the unit"
imds_up
trap 'rc=$?; imds_down; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT
for u in "sb-postgres@$A.service" "sb-postgres@system.service" "sb-gotrue@$A.service" "sb-postgrest@$A.service"; do
  imds_denied_by_unit "$u"
  imds_blocked_in "$u"
done
[[ -z $(systemctl show -p IPAddressDeny --value sbctl.service) ]] || fail "sbctl.service must keep access to the instance role (it is the only holder of the backup credentials)"
# The threat itself: SQL that runs a program (superuser here; pg_net, http and untrusted
# extensions reach the same network) cannot fetch role credentials. The program runs as a
# child of the postmaster, in the unit's cgroup.
pg_admin "$A" "copy (select 1) to program '/usr/bin/curl -sS -m 5 -o /dev/null -w %{http_code} http://169.254.169.254:$IMDS_PORT/ > $SBCTL_STATE/projects/$A/postgres/imds-probe.txt 2>&1; echo \" rc=\$?\" >> $SBCTL_STATE/projects/$A/postgres/imds-probe.txt'" >/dev/null 2>&1 || true
PROBE=$(cat "$SBCTL_STATE/projects/$A/postgres/imds-probe.txt" 2>/dev/null || echo "no probe output")
[[ $PROBE != 200* && $PROBE != *"rc=0"* ]] || fail "SQL running a program inside sb-postgres@$A reached the metadata service: $PROBE"
[[ $PROBE == *"curl: ("* ]] || fail "the metadata probe from SQL did not run curl, so it proves nothing: $PROBE"
log "COPY TO PROGRAM inside sb-postgres@$A: $PROBE"
rm -f "$SBCTL_STATE/projects/$A/postgres/imds-probe.txt"
imds_down
trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

log "a project created through the Management API: REST through the proxy"
claim_and_token
gen_dbpass
C=$(api_create_project smoke-api)
project_keys "$C"
# No postgres-meta runs here (the fleet is fleet-smoke.sh), so the table is created over the cluster socket.
pg_admin "$C" "$SMOKE_TABLE_SQL" >/dev/null || fail "$C: create table"
rest_through_proxy "$C" "$PUB"
[[ $(http_code -H "Host: $C.api.$SBCTL_DOMAIN" -H "apikey: sb_publishable_wrong" http://127.0.0.1/rest/v1/smoke_items) == 401 ]] || fail "$C: a wrong key was not a 401"
[[ -S "$SBCTL_STATE/projects/$C/wal/r.sock" ]] || fail "$C: no WAL relay socket for a project created through the API"
SEGC=$(switch_wal "$C")
wait_archived "$C" "$SEGC" 90 || fail "$C: WAL of a project created through the API was not archived"
papi DELETE "/v1/projects/$C" -o /dev/null -m 900 || fail "$C: delete through the API"

# The units that hold the master key keep a capability in their permitted set, so a tenant unit (no
# capabilities) fails the kernel's ptrace check on their /proc/<pid>/environ and /proc/<pid>/root.
for u in sbctl.service "sb-basebackup@$A.service" sb-basebackup-prune.service; do
  [[ $(systemctl show -p AmbientCapabilities --value "$u") == *cap_net_bind_service* ]] || fail "$u lost the capability that hides its /proc from tenant units"
done
for u in "sb-postgres@$A.service" "sb-gotrue@$A.service" "sb-postgrest@$A.service"; do
  [[ -z $(systemctl show -p AmbientCapabilities --value "$u") ]] || fail "$u holds a capability"
done
log "nightly backup: the sb-basebackup@$A service runs as the timer would"
systemctl start "sb-basebackup@$A.service" || { journalctl --no-pager -u "sb-basebackup@$A" | tail -30 >&2; fail "$A: sb-basebackup service failed"; }
[[ $(sbctl backups list "$A" | grep -c completed) -ge 1 ]] || { sbctl backups list "$A" >&2 || true; fail "$A: no completed base backup after the backup service ran"; }

log "a base backup with the daemon down: the command serves the relay sockets nobody answers while it runs"
systemctl stop sbctl.service
sbctl backups create "$B" --reason manual || { journalctl --no-pager -u "sb-postgres@$B" | tail -20 >&2; fail "$B: base backup with the daemon down"; }
[[ $(sbctl backups list "$B" | grep -c completed) -ge 1 ]] || fail "$B: no completed base backup after a backup with the daemon down"
systemctl start sbctl.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done

log "restore through the relay: a clone of $A reads $A's archive through its own socket"
pg_admin "$A" "create table public.restore_marker (id int); insert into public.restore_marker values (1)" >/dev/null || fail "$A: create the restore marker"
SEGR=$(switch_wal "$A") || fail "$A: could not switch WAL"
wait_archived "$A" "$SEGR" 90 || fail "$A: the segment with the restore marker was not archived"
CLONE=restoredcloneprojxyz
sbctl backups restore "$A" --to latest --as "$CLONE" || { journalctl --no-pager -u "sb-postgres@$CLONE" -u sbctl.service | tail -40 >&2; fail "restore of $A as $CLONE"; }
sbctl projects health "$CLONE" || fail "$CLONE: unhealthy after the restore"
[[ $(pg_admin "$CLONE" "select count(*) from public.restore_marker") == 1 ]] || fail "$CLONE: the restored cluster lacks the row written after the base backup (WAL replay through the relay)"
[[ ! -e "$SBCTL_STATE/projects/$CLONE/restore-sources" ]] || fail "$CLONE: the file that lets its relay read $A's archive survived the recovery"
[[ -z $(pg_admin "$CLONE" "show restore_command") ]] || fail "$CLONE: restore_command was not reset after recovery"
CLONE_ARCHIVE=$(pg_admin "$CLONE" "show archive_command")
[[ $CLONE_ARCHIVE == *"--ref $CLONE "* && $CLONE_ARCHIVE == *"projects/$CLONE/wal/r.sock"* ]] || fail "$CLONE archives somewhere else: $CLONE_ARCHIVE"
SEGK=$(switch_wal "$CLONE") || fail "$CLONE: could not switch WAL"
wait_archived "$CLONE" "$SEGK" 90 || fail "$CLONE: the clone's own WAL was not archived through its relay"
sbctl projects delete "$CLONE" --skip-final-backup >/dev/null || fail "delete of $CLONE"

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
