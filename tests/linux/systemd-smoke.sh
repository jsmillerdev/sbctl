#!/usr/bin/env bash
# systemd backend smoke test: system init, two projects, health, containment from inside the
# unit namespaces, WAL archiving through the daemon's relay (fail-closed with the daemon down), the
# instance metadata service denied to every unit, a project created through the Management API,
# a restore through the relay, a pg_cron job that runs (and runs again after a pause and resume), pause/resume, key rotation, crash recovery and delete, all under
# real systemd units as the supavise user.
#
#   sudo SUPAVISE_BIN=/path/to/supavise-linux-amd64 tests/linux/systemd-smoke.sh [--teardown]
#
# Without SUPAVISE_BIN the script builds supavise with the go toolchain. It needs network
# access for the artifact downloads. Exit status is non-zero on the first failure; logs
# (journal, unit list, host facts) stay in $LOG_DIR (default /tmp/supavise-linux-logs).
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
for u in supavise-postgres@system supavise-gotrue@system; do
  wait_active "$u.service" 30
  [[ $(systemctl is-enabled "$u.service") == enabled ]] || fail "$u is not enabled for boot (install-units enables it)"
done
supavise system status || fail "system status"

log "polkit: the supavise user must not reload systemd or manage unit files"
if sudo -u "$SUPAVISE_USER" systemctl --no-ask-password daemon-reload 2>/dev/null; then
  fail "the supavise user can run daemon-reload (polkit rule too broad)"
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

  supavise projects health "$ref" || fail "$ref: health"
  for svc in postgres gotrue postgrest; do
    local u="supavise-$svc@$ref.service"
    [[ $(unit_state "$u") == active ]] || fail "$u not active"
    [[ $(systemctl show -p User --value "$u") == "$SUPAVISE_USER" ]] || fail "$u does not run as $SUPAVISE_USER"
    [[ $(systemctl show -p Slice --value "$u") == supavise.slice ]] || fail "$u is not in supavise.slice"
  done
  [[ $(systemctl show -p MemoryMax --value "supavise-postgres@$ref.service") == 1073741824 ]] || fail "$ref: MemoryMax drop-in not applied"
  [[ $(http_code "http://127.0.0.1:$gt/health") == 200 ]] || fail "$ref: gotrue /health"
  [[ $(http_code -H "apikey: $anon" -H "Authorization: Bearer $anon" "http://127.0.0.1:$rt/") == 200 ]] || fail "$ref: postgrest /"
  [[ $(http_code -H "Authorization: Bearer not.a.jwt" "http://127.0.0.1:$rt/") == 401 ]] || fail "$ref: postgrest accepted a bad JWT"
  # Sign-up proves GoTrue wrote to the project's auth schema.
  [[ $(http_code -X POST -H "apikey: $anon" -H 'Content-Type: application/json' \
      -d "{\"email\":\"smoke-$RANDOM$RANDOM@example.com\",\"password\":\"correct-horse-battery-1\"}" "http://127.0.0.1:$gt/signup") == 200 ]] || fail "$ref: signup"
  # Passwordless TCP login is refused.
  local psql
  psql=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
  if "$psql" "host=127.0.0.1 port=$pg user=postgres dbname=postgres connect_timeout=3" -Atc 'select 1' </dev/null >/dev/null 2>&1; then
    fail "$ref: passwordless TCP login accepted"
  fi
  log "$ref: healthy"
}
check_project "$A"
check_project "$B"

# What an API or fleet unit can see, from inside its own mount namespace as the supavise user.
sees() { # UNIT PATH: exit 0 if PATH is readable from UNIT's namespace
  local pid
  pid=$(systemctl show -p MainPID --value "$1")
  [[ $pid -gt 0 ]] || fail "$1 has no main pid"
  nsenter -t "$pid" -m -- runuser -u "$SUPAVISE_USER" -- test -r "$2" 2>/dev/null
}
log "containment: supavise-gotrue@$A sees only the artifacts, its launcher and its own work directory"
G="supavise-gotrue@$A.service"
sees "$G" "$SUPAVISE_STATE/projects/$A/gotrue.run" || fail "$G cannot read its own launcher"
sees "$G" "$SUPAVISE_STATE/projects/$A/gotrue" || fail "$G cannot read its work directory"
sees "$G" "$SUPAVISE_STATE/artifacts" || fail "$G cannot read the artifacts"
for hidden in "$SUPAVISE_STATE/backups" "$SUPAVISE_STATE/certs" "$SUPAVISE_STATE/projects/$B" "$SUPAVISE_STATE/projects/$A/postgres" \
    "$SUPAVISE_STATE/projects/$A/gotrue.env" "$SUPAVISE_STATE/projects/$A/postgres.env" "$SUPAVISE_STATE/projects/$A/postgrest.env" \
    "$SUPAVISE_STATE/projects/$A/postgrest.run" "$SUPAVISE_STATE/projects/system" /etc/supavise; do
  if sees "$G" "$hidden"; then fail "$G can read $hidden"; fi
done
log "containment: supavise-postgres@$A sees its cluster and its own WAL relay directory, and nothing of the node"
PGU="supavise-postgres@$A.service"
sees "$PGU" "$SUPAVISE_STATE/projects/$A/postgres" || fail "$PGU cannot read its cluster directory"
sees "$PGU" "$SUPAVISE_STATE/projects/$A/postgres.run" || fail "$PGU cannot read its launcher"
sees "$PGU" "$SUPAVISE_STATE/projects/$A/wal" || fail "$PGU cannot see its WAL relay directory"
# No backend credentials and no backups: config.toml may hold the S3 key, and the shared backups
# directory holds every project's WAL and base backups. WAL leaves through the relay socket.
for hidden in /etc/supavise /etc/supavise/config.toml /etc/supavise/master.key /run/dbus/system_bus_socket "$SUPAVISE_STATE/projects/$B" "$SUPAVISE_STATE/projects/$B/wal" \
    "$SUPAVISE_STATE/projects/system" "$SUPAVISE_STATE/projects/system/wal" "$SUPAVISE_STATE/projects/$A/postgres.env" \
    "$SUPAVISE_STATE/projects/$A/gotrue.env" "$SUPAVISE_STATE/certs" "$SUPAVISE_STATE/backups" "$SUPAVISE_STATE/backups/$A" "$SUPAVISE_STATE/backups/$B"; do
  if sees "$PGU" "$hidden"; then fail "$PGU can read $hidden"; fi
done
if nsenter -t "$(systemctl show -p MainPID --value "$PGU")" -m -- runuser -u "$SUPAVISE_USER" -- sh -c "echo x > '$SUPAVISE_STATE/projects/$A/wal/probe'" 2>/dev/null; then
  fail "$PGU can write into its WAL relay directory (it must be read-only)"
fi
if [[ $(unit_state supavise-studio.service) == active ]]; then
  for hidden in "$SUPAVISE_STATE/projects/system/supavisor.env" "$SUPAVISE_STATE/projects/system/storage.env" "$SUPAVISE_STATE/backups"; do
    if sees supavise-studio.service "$hidden"; then fail "supavise-studio can read $hidden"; fi
  done
fi

log "daemon: supavise.service (supavise serve) next to the CLI"
systemctl start supavise.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done
[[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "the management API does not answer on the admin listener"; }
[[ $(http_code -H "Host: api.$SUPAVISE_DOMAIN" http://127.0.0.1/v1/projects) == 401 ]] || fail "the management API is not served at api.<domain> through the proxy"
[[ $(http_code -H "Host: api.$SUPAVISE_DOMAIN" http://127.0.0.1/auth/v1/settings) == 200 ]] || fail "the dashboard GoTrue is not reachable at api.<domain>/auth/v1"
PUB_A=$(project_field "$A" 'd["keys"]["publishable_key"]' --show-keys)
[[ $(http_code -H "Host: $A.api.$SUPAVISE_DOMAIN" -H "apikey: $PUB_A" http://127.0.0.1/auth/v1/settings) == 200 ]] || fail "$A: project API through the proxy"
[[ $(http_code -H "Host: $A.api.$SUPAVISE_DOMAIN" -H "apikey: sb_publishable_wrong" http://127.0.0.1/rest/v1/) == 401 ]] || fail "$A: a wrong key was not a 401"
# Studio's sign-in goes to api.<domain>/auth/v1; the Studio host answers its banner route itself.
[[ $(http_code -H "Host: studio.$SUPAVISE_DOMAIN" http://127.0.0.1/api/incident-banner) == 200 ]] || fail "the proxy does not answer /api/incident-banner"
# The daemon starts the timers next to its listeners, so give it a moment after the API answers.
for t in "supavise-basebackup@$A.timer" "supavise-basebackup@system.timer" supavise-basebackup-prune.timer; do
  for ((i = 0; i < 30; i++)); do
    [[ $(unit_state "$t") == active ]] && break
    sleep 1
  done
  [[ $(unit_state "$t") == active ]] || { journalctl --no-pager -u supavise.service | tail -20 >&2; fail "$t was not started by the daemon"; }
done

PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
pg_admin() { # REF SQL: run SQL as supabase_admin over the cluster's private socket
  local ref=$1 port
  port=$(project_field "$ref" 'd["ports"]["Postgres"]')
  sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/$ref/postgres/sock port=$port user=supabase_admin dbname=postgres" -Atc "$2" </dev/null
}
# switch_wal REF: writes a WAL record and switches to a new segment, so the switch is never a
# no-op on an idle cluster; prints the name of the segment that was just completed.
switch_wal() { pg_admin "$1" "select pg_walfile_name(pg_switch_wal() - 1) from (select pg_logical_emit_message(true, 'smoke', 'x')) s"; }
wait_archived() { # REF SEGMENT SECONDS: the segment is in the file backend
  local ref=$1 seg=$2 n=${3:-40} i
  for ((i = 0; i < n; i++)); do
    [[ -s "$SUPAVISE_STATE/backups/$ref/wal/$seg.zst" ]] && return 0
    sleep 1
  done
  return 1
}

log "pg_cron: a job scheduled on $A runs (background workers, no libpq connection)"
pg_admin "$A" "create extension if not exists pg_cron; create table public.cron_smoke (at timestamptz default now());
  select cron.schedule('smoke-job', '5 seconds', 'insert into public.cron_smoke default values');" >/dev/null || fail "$A: could not schedule a pg_cron job"
wait_cron_success pg_admin "$A" smoke-job 90 || fail "$A: cron.job_run_details shows no succeeded run of the job (pg_cron cannot connect?)"
[[ $(pg_admin "$A" "select count(*) from public.cron_smoke") -ge 1 ]] || fail "$A: the cron job reported success but its insert is not in the table"
[[ $(pg_admin "$A" "select current_setting('cron.use_background_workers')") == on ]] || fail "$A: cron.use_background_workers is not on"
[[ $(cron_runs pg_admin "$A" smoke-job failed) -eq 0 ]] || fail "$A: a cron run failed: $(pg_admin "$A" "select return_message from cron.job_run_details where status = 'failed' limit 1")"

log "WAL archiving: a switched segment of $A reaches the backend through the daemon's relay, not through the cluster's unit"
for ref in system "$A" "$B"; do
  [[ -S "$SUPAVISE_STATE/projects/$ref/wal/r.sock" ]] || fail "$ref: the daemon serves no WAL relay socket"
done
archive_cmd=$(pg_admin "$A" "show archive_command")
[[ $archive_cmd == *"--socket $SUPAVISE_STATE/projects/$A/wal/r.sock"* && $archive_cmd != *--config* ]] || fail "$A: archive_command is not the relay form: $archive_cmd"
SEG=$(switch_wal "$A") || fail "$A: could not switch WAL over the cluster socket"
wait_archived "$A" "$SEG" 90 || { journalctl --no-pager -u "supavise-postgres@$A" -u supavise.service | tail -30 >&2; fail "$A: WAL segment $SEG was not archived through the relay"; }
[[ $(pg_admin "$A" "select last_archived_wal is not null from pg_stat_archiver") == t ]] || fail "$A: pg_stat_archiver shows no archived WAL"
# From inside the cluster's own namespace: its socket works, another project's is not there, and
# the socket of $A refuses to read $B's archive.
PGPID=$(systemctl show -p MainPID --value "$PGU")
inns() { nsenter -t "$PGPID" -m -- runuser -u "$SUPAVISE_USER" -- "$@"; }
[[ $(inns curl -sS -m 10 -o /dev/null -w '%{http_code}' --unix-socket "$SUPAVISE_STATE/projects/$A/wal/r.sock" http://relay/v1/ping) == 204 ]] || fail "$A: the relay does not answer from inside the unit's namespace"
if inns test -e "$SUPAVISE_STATE/projects/$B/wal/r.sock"; then fail "$PGU sees the WAL relay socket of $B"; fi
[[ $(inns curl -sS -m 10 -o /dev/null -w '%{http_code}' --unix-socket "$SUPAVISE_STATE/projects/$A/wal/r.sock" \
  "http://relay/v1/wal/fetch?ref=$B&name=000000010000000000000001") == 403 ]] || fail "$A's relay served the archive of $B"
[[ $(inns curl -sS -m 10 -o /dev/null -w '%{http_code}' --unix-socket "$SUPAVISE_STATE/projects/$A/wal/r.sock" -X POST --data-binary x \
  "http://relay/v1/wal/push?ref=$B&name=000000010000000000000001") == 403 ]] || fail "$A's relay accepted a push for $B"

# The relay also checks who is on the other end (SO_PEERCRED and the peer's cgroup), because
# another unit can reach this socket through /proc/<pid>/root. A curl moved into $B's postgres cgroup
# sees $A's socket path (this shell's mount namespace) but is refused; one in $A's cgroup is served.
relay_ping_from_unit() { # UNIT: prints the HTTP status of a ping to $A's relay from a process in UNIT's cgroup, 000 if refused
  local cg; cg=$(systemctl show -p ControlGroup --value "$1")
  [[ -n $cg && -w /sys/fs/cgroup$cg/cgroup.procs ]] || fail "$1: no writable cgroup ($cg)"
  bash -c 'echo $$ >"$1" && exec curl -sS -m 5 -o /dev/null -w "%{http_code}" --unix-socket "$2" http://relay/v1/ping' _ \
    "/sys/fs/cgroup$cg/cgroup.procs" "$SUPAVISE_STATE/projects/$A/wal/r.sock" 2>/dev/null || true
}
[[ $(relay_ping_from_unit "$PGU") == 204 ]] || fail "$A's own postgres unit is refused by its relay"
[[ $(relay_ping_from_unit "supavise-postgres@$B.service") != 204 ]] || fail "the relay of $A served a process of $B's postgres unit"
[[ $(relay_ping_from_unit "supavise-postgrest@$A.service") != 204 ]] || fail "the relay of $A served a process of its PostgREST unit"

log "WAL archiving fails closed when the daemon is down, and resumes when it is back"
systemctl stop supavise.service
SEG2=$(switch_wal "$A") || fail "$A: could not switch WAL"
FAILS0=$(pg_admin "$A" "select failed_count from pg_stat_archiver")
for ((i = 0; i < 30; i++)); do
  [[ $(pg_admin "$A" "select failed_count from pg_stat_archiver") -gt $FAILS0 ]] && break
  sleep 1
done
[[ $(pg_admin "$A" "select failed_count from pg_stat_archiver") -gt $FAILS0 ]] || fail "$A: archive_command did not fail while the daemon was down (WAL must not be buffered anywhere else)"
[[ ! -e "$SUPAVISE_STATE/backups/$A/wal/$SEG2.zst" ]] || fail "$A: $SEG2 reached the backend with the daemon down"
systemctl start supavise.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done
# Postgres retries by itself (the archiver waits up to a minute between rounds).
wait_archived "$A" "$SEG2" 150 || { journalctl --no-pager -u "supavise-postgres@$A" -u supavise.service | tail -30 >&2; fail "$A: $SEG2 was not archived after the daemon came back"; }

log "instance metadata: denied to every supavise-* unit that runs tenant code, from inside the unit"
imds_up
trap 'rc=$?; imds_down; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT
for u in "supavise-postgres@$A.service" "supavise-postgres@system.service" "supavise-gotrue@$A.service" "supavise-postgrest@$A.service"; do
  imds_denied_by_unit "$u"
  imds_blocked_in "$u"
done
[[ -z $(systemctl show -p IPAddressDeny --value supavise.service) ]] || fail "supavise.service must keep access to the instance role (it is the only holder of the backup credentials)"
# The threat itself: SQL that runs a program (superuser here; pg_net, http and untrusted
# extensions reach the same network) cannot fetch role credentials. The program runs as a
# child of the postmaster, in the unit's cgroup.
pg_admin "$A" "copy (select 1) to program '/usr/bin/curl -sS -m 5 -o /dev/null -w %{http_code} http://169.254.169.254:$IMDS_PORT/ > $SUPAVISE_STATE/projects/$A/postgres/imds-probe.txt 2>&1; echo \" rc=\$?\" >> $SUPAVISE_STATE/projects/$A/postgres/imds-probe.txt'" >/dev/null 2>&1 || true
PROBE=$(cat "$SUPAVISE_STATE/projects/$A/postgres/imds-probe.txt" 2>/dev/null || echo "no probe output")
[[ $PROBE != 200* && $PROBE != *"rc=0"* ]] || fail "SQL running a program inside supavise-postgres@$A reached the metadata service: $PROBE"
[[ $PROBE == *"curl: ("* ]] || fail "the metadata probe from SQL did not run curl, so it proves nothing: $PROBE"
log "COPY TO PROGRAM inside supavise-postgres@$A: $PROBE"
rm -f "$SUPAVISE_STATE/projects/$A/postgres/imds-probe.txt"
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
[[ $(http_code -H "Host: $C.api.$SUPAVISE_DOMAIN" -H "apikey: sb_publishable_wrong" http://127.0.0.1/rest/v1/smoke_items) == 401 ]] || fail "$C: a wrong key was not a 401"
[[ -S "$SUPAVISE_STATE/projects/$C/wal/r.sock" ]] || fail "$C: no WAL relay socket for a project created through the API"
SEGC=$(switch_wal "$C")
wait_archived "$C" "$SEGC" 90 || fail "$C: WAL of a project created through the API was not archived"
papi DELETE "/v1/projects/$C" -o /dev/null -m 900 || fail "$C: delete through the API"

# The units that hold the master key keep a capability in their permitted set, so a tenant unit (no
# capabilities) fails the kernel's ptrace check on their /proc/<pid>/environ and /proc/<pid>/root.
for u in supavise.service "supavise-basebackup@$A.service" supavise-basebackup-prune.service; do
  [[ $(systemctl show -p AmbientCapabilities --value "$u") == *cap_net_bind_service* ]] || fail "$u lost the capability that hides its /proc from tenant units"
done
for u in "supavise-postgres@$A.service" "supavise-gotrue@$A.service" "supavise-postgrest@$A.service"; do
  [[ -z $(systemctl show -p AmbientCapabilities --value "$u") ]] || fail "$u holds a capability"
done

# A CLI command an operator runs (`sudo -u supavise supavise ...`) holds no capability. It marks itself
# non-dumpable at startup, so the kernel refuses its /proc/<pid>/root (the host view, with
# /etc/supavise) to a same-uid process of a tenant unit. `supavise wal push` blocks opening a FIFO
# that stands in for the WAL file, which keeps a CLI process alive without side effects.
log "a running CLI command is not dumpable: a tenant unit cannot read its /proc/<pid>/root"
DUMPDIR=$(mktemp -d); chown "$SUPAVISE_USER" "$DUMPDIR"
mkfifo "$DUMPDIR/000000010000000000000099"; chown "$SUPAVISE_USER" "$DUMPDIR/000000010000000000000099"
sudo -u "$SUPAVISE_USER" sleep 120 & SLEEPER=$!   # control: an ordinary same-uid process stays dumpable
sudo -u "$SUPAVISE_USER" -H /usr/local/bin/supavise wal push --ref "$A" --socket "$SUPAVISE_STATE/projects/$A/wal/r.sock" \
  "$DUMPDIR/000000010000000000000099" >"$DUMPDIR/cli.log" 2>&1 & CLIWAIT=$!
CLIPID=""
for ((i = 0; i < 30; i++)); do
  CLIPID=$(pgrep -u "$SUPAVISE_USER" -f '^/usr/local/bin/supavise wal push' | head -1 || true)
  # A non-dumpable process has its /proc/<pid>/environ owned by root (the /proc/<pid> directory
  # itself is always world-readable and keeps the process's owner).
  [[ -n $CLIPID && $(stat -c %U "/proc/$CLIPID/environ" 2>/dev/null) == root ]] && break
  CLIPID=""; sleep 1
done
SLEEPPID=$(pgrep -u "$SUPAVISE_USER" -x sleep | head -1 || true)
tenant_ls() { # PID: `ls /proc/PID/root/etc/supavise` as the supavise user from inside the cgroup of A's PostgREST unit
  local cg; cg=$(systemctl show -p ControlGroup --value "supavise-postgrest@$A.service")
  [[ -n $cg && -w /sys/fs/cgroup$cg/cgroup.procs ]] || fail "supavise-postgrest@$A: no writable cgroup ($cg)"
  bash -c 'echo $$ >"$1" && exec runuser -u "$2" -- ls "/proc/$3/root/etc/supavise"' _ "/sys/fs/cgroup$cg/cgroup.procs" "$SUPAVISE_USER" "$1" >/dev/null 2>&1
}
if [[ -n $CLIPID && -n $SLEEPPID ]]; then
  tenant_ls "$SLEEPPID" || { pkill -KILL -u "$SUPAVISE_USER" -x sleep || true; pkill -KILL -u "$SUPAVISE_USER" -f '^/usr/local/bin/supavise wal push' || true; fail "control: a tenant unit cannot read an ordinary process's /proc/<pid>/root, so the check below proves nothing"; }
  tenant_ls "$CLIPID" && { pkill -KILL -u "$SUPAVISE_USER" -f '^/usr/local/bin/supavise wal push' || true; fail "a tenant unit read the /proc/<pid>/root of a running CLI command"; }
else
  ps -u "$SUPAVISE_USER" -o pid,user,stat,args >&2 || true
  cat "$DUMPDIR/cli.log" >&2 || true
  for pid in $(pgrep -u "$SUPAVISE_USER" -f '^/usr/local/bin/supavise wal push') $(pgrep -u "$SUPAVISE_USER" -x sleep); do ls -l "/proc/$pid/environ" >&2 || true; done
  fail "the CLI command did not become non-dumpable (cli pid '${CLIPID}', control pid '${SLEEPPID}')"
fi
pkill -KILL -u "$SUPAVISE_USER" -f '^/usr/local/bin/supavise wal push' || true
pkill -KILL -u "$SUPAVISE_USER" -x sleep || true   # SIGTERM would not stop the CLI: Go catches it while open(2) blocks on the FIFO
wait "$CLIWAIT" "$SLEEPER" 2>/dev/null || true
rm -rf "$DUMPDIR"

log "an object in $A's Storage directory (and one in $B's), for the nightly backup to copy"
OBJ_ROOT="$SUPAVISE_STATE/system/storage/objects/stub"
for r in "$A" "$B"; do
  sudo -u "$SUPAVISE_USER" mkdir -p "$OBJ_ROOT/$r/bucket/hello.txt"
  echo "stored object of $r" | sudo -u "$SUPAVISE_USER" tee "$OBJ_ROOT/$r/bucket/hello.txt/v1" >/dev/null
done

log "nightly backup: the supavise-basebackup@$A service runs as the timer would"
systemctl start "supavise-basebackup@$A.service" || { journalctl --no-pager -u "supavise-basebackup@$A" | tail -30 >&2; fail "$A: supavise-basebackup service failed"; }
[[ $(supavise backups list "$A" | grep -c completed) -ge 1 ]] || { supavise backups list "$A" >&2 || true; fail "$A: no completed base backup after the backup service ran"; }
# The unit sees $A's objects through its mount allowlist and copied them.
[[ $(supavise backups list "$A" --files | awk '$2 == "storage" && $5 >= 1' | wc -l) -ge 1 ]] || { supavise backups list "$A" --files >&2 || true; journalctl --no-pager -u "supavise-basebackup@$A" | tail -30 >&2; fail "$A: the nightly backup did not copy the Storage object"; }

log "a base backup with the daemon down: the command serves the relay sockets nobody answers while it runs"
systemctl stop supavise.service
supavise backups create "$B" --reason manual || { journalctl --no-pager -u "supavise-postgres@$B" | tail -20 >&2; fail "$B: base backup with the daemon down"; }
[[ $(supavise backups list "$B" | grep -c completed) -ge 1 ]] || fail "$B: no completed base backup after a backup with the daemon down"
[[ $(supavise backups list "$B" --files | awk '$2 == "storage" && $5 == 1' | wc -l) -ge 1 ]] || { supavise backups list "$B" --files >&2 || true; fail "$B: the manual backup did not copy the Storage object"; }
systemctl start supavise.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done

log "restore through the relay: a clone of $A reads $A's archive through its own socket"
pg_admin "$A" "create table public.restore_marker (id int); insert into public.restore_marker values (1)" >/dev/null || fail "$A: create the restore marker"
SEGR=$(switch_wal "$A") || fail "$A: could not switch WAL"
wait_archived "$A" "$SEGR" 90 || fail "$A: the segment with the restore marker was not archived"
CLONE=restoredcloneprojxyz
supavise backups restore "$A" --to latest --as "$CLONE" || { journalctl --no-pager -u "supavise-postgres@$CLONE" -u supavise.service | tail -40 >&2; fail "restore of $A as $CLONE"; }
supavise projects health "$CLONE" || fail "$CLONE: unhealthy after the restore"
[[ $(cat "$OBJ_ROOT/$CLONE/bucket/hello.txt/v1" 2>/dev/null) == "stored object of $A" ]] || fail "$CLONE: the Storage object of $A did not come back with the restore"
[[ $(pg_admin "$CLONE" "select count(*) from public.restore_marker") == 1 ]] || fail "$CLONE: the restored cluster lacks the row written after the base backup (WAL replay through the relay)"
[[ ! -e "$SUPAVISE_STATE/projects/$CLONE/restore-sources" ]] || fail "$CLONE: the file that lets its relay read $A's archive survived the recovery"
[[ -z $(pg_admin "$CLONE" "show restore_command") ]] || fail "$CLONE: restore_command was not reset after recovery"
CLONE_ARCHIVE=$(pg_admin "$CLONE" "show archive_command")
[[ $CLONE_ARCHIVE == *"--ref $CLONE "* && $CLONE_ARCHIVE == *"projects/$CLONE/wal/r.sock"* ]] || fail "$CLONE archives somewhere else: $CLONE_ARCHIVE"
SEGK=$(switch_wal "$CLONE") || fail "$CLONE: could not switch WAL"
wait_archived "$CLONE" "$SEGK" 90 || fail "$CLONE: the clone's own WAL was not archived through its relay"
supavise projects delete "$CLONE" --skip-final-backup >/dev/null || fail "delete of $CLONE"

log "daemon restart leaves the projects running"
PG_PID=$(systemctl show -p MainPID --value "supavise-postgres@$A.service")
systemctl restart supavise.service
for ((i = 0; i < 30; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done
[[ $(systemctl show -p MainPID --value "supavise-postgres@$A.service") == "$PG_PID" ]] || fail "restarting supavise.service restarted a project's Postgres"
supavise projects health "$A" || fail "$A unhealthy after the daemon restarted"

log "pause and resume $A"
CRON_BEFORE=$(cron_runs pg_admin "$A" smoke-job succeeded)
supavise projects pause "$A"
for svc in postgres gotrue postgrest; do
  [[ $(unit_state "supavise-$svc@$A.service") == inactive ]] || fail "supavise-$svc@$A is $(unit_state "supavise-$svc@$A.service") after pause"
done
[[ $(project_field "$A" 'd["status"]') == INACTIVE ]] || fail "$A not INACTIVE"
[[ $(unit_state "supavise-postgres@$B.service") == active ]] || fail "pausing $A disturbed $B"
[[ $(unit_state "supavise-basebackup@$A.timer") != active ]] || fail "$A is paused but its nightly backup timer still runs"
supavise projects resume "$A"
check_project "$A"
[[ $(unit_state "supavise-basebackup@$A.timer") == active ]] || fail "$A resumed but its nightly backup timer was not started"
for ((i = 0; i < 45; i++)); do
  [[ $(cron_runs pg_admin "$A" smoke-job succeeded) -gt $CRON_BEFORE ]] && break
  sleep 2
done
[[ $(cron_runs pg_admin "$A" smoke-job succeeded) -gt $CRON_BEFORE ]] || fail "$A: the cron job did not run again after the pause and resume"
pg_admin "$A" "select cron.unschedule('smoke-job')" >/dev/null || fail "$A: could not unschedule the cron job"

log "rotate keys of $A"
OLD=$(project_field "$A" 'd["keys"]["anon_key"]' --show-keys)
supavise projects rotate-keys "$A" >/dev/null
NEW=$(project_field "$A" 'd["keys"]["anon_key"]' --show-keys)
RT=$(project_field "$A" 'd["ports"]["PostgREST"]')
[[ $(http_code -H "Authorization: Bearer $OLD" "http://127.0.0.1:$RT/") == 401 ]] || fail "old key still accepted"
[[ $(http_code -H "Authorization: Bearer $NEW" "http://127.0.0.1:$RT/") == 200 ]] || fail "new key rejected"
# The daemon's proxy learns of the rotation from the registry (LISTEN/NOTIFY), not from a restart.
NEWPUB=$(project_field "$A" 'd["keys"]["publishable_key"]' --show-keys)
for ((i = 0; i < 20; i++)); do
  [[ $(http_code -H "Host: $A.api.$SUPAVISE_DOMAIN" -H "apikey: $NEWPUB" http://127.0.0.1/auth/v1/settings) == 200 ]] && break
  sleep 1
done
[[ $(http_code -H "Host: $A.api.$SUPAVISE_DOMAIN" -H "apikey: $NEWPUB" http://127.0.0.1/auth/v1/settings) == 200 ]] || fail "the proxy does not accept the rotated publishable key"
[[ $(http_code -H "Host: $A.api.$SUPAVISE_DOMAIN" -H "apikey: $PUB_A" http://127.0.0.1/rest/v1/) == 401 ]] || fail "the proxy still accepts the old publishable key"

log "crash recovery: kill -9 PostgREST of $B"
PID=$(systemctl show -p MainPID --value "supavise-postgrest@$B.service")
kill -9 "$PID"
for ((i = 0; i < 30; i++)); do
  [[ $(unit_state "supavise-postgrest@$B.service") == active && $(systemctl show -p MainPID --value "supavise-postgrest@$B.service") != "$PID" ]] && break
  sleep 1
done
# The unit is active with a new process before PostgREST answers (it loads its schema cache first).
for ((i = 0; i < 30; i++)); do
  supavise projects health "$B" && break
  sleep 1
done
supavise projects health "$B" || fail "$B did not recover from a PostgREST crash"

JWT=$(mint_dashboard_jwt "$(project_field system 'd["keys"]["jwt_secret"]' --show-keys)")
# The minted user is not an account of supavise-gotrue@system, so it is neither a claimed Owner nor an
# account from before roles: give it the Owner role in the registry, as `supavise users role` would.
sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/system/postgres/sock port=5433 user=supabase_admin dbname=supavise" -qAt \
  -c "insert into supavise.org_members (org_id, user_id, role_id) select id, '00000000-0000-4000-8000-000000000001', 1 from supavise.organizations on conflict do nothing" </dev/null \
  || fail "could not give the smoke user the Owner role"
[[ $(http_code -H "Authorization: Bearer $JWT" http://127.0.0.1:7000/v1/projects) == 200 ]] || fail "a dashboard session is not accepted by the Management API"

log "delete both projects: $A through the Management API (the daemon's own sandbox), $B with the CLI"
for ref in "$A" "$B"; do
  if [[ $ref == "$A" ]]; then
    code=$(curl -s -o "${LOG_DIR:-/tmp}/api-delete.json" -w '%{http_code}' --max-time 900 -X DELETE -H "Authorization: Bearer $JWT" "http://127.0.0.1:7000/v1/projects/$ref" || true)
    [[ $code == 200 ]] || { cat "${LOG_DIR:-/tmp}/api-delete.json" >&2 || true; journalctl --no-pager -u supavise.service | tail -30 >&2; fail "$ref: API delete answered $code"; }
  else
    supavise projects delete "$ref"
  fi
  for svc in postgres gotrue postgrest; do
    [[ $(unit_state "supavise-$svc@$ref.service") == inactive ]] || fail "supavise-$svc@$ref still $(unit_state "supavise-$svc@$ref.service")"
  done
  [[ ! -e "$SUPAVISE_STATE/projects/$ref" ]] || fail "$ref: data directory remains"
  # The delete took a final base backup and stopped the nightly timer.
  ls "$SUPAVISE_STATE/backups/$ref/base/" 2>/dev/null | grep -q . || fail "$ref: no final base backup in the backend"
  [[ $(unit_state "supavise-basebackup@$ref.timer") != active ]] || fail "$ref: backup timer still runs after delete"
  # The drop-in files stay (removing them needs a polkit action supavise must not hold); the limits are lifted.
  [[ $(systemctl show -p MemoryMax --value "supavise-postgres@$ref.service") == infinity ]] || fail "$ref: MemoryMax limit remains"
done
[[ $(unit_state supavise-postgres@system.service) == active ]] || fail "system postgres stopped"
supavise system status || fail "system status after deletes"

log "daemon stops gracefully"
systemctl stop supavise.service
[[ $(systemctl show -p Result --value supavise.service) == success ]] || fail "supavise.service did not stop cleanly: $(systemctl show -p Result --value supavise.service)"
[[ $(unit_state supavise-postgres@system.service) == active ]] || fail "stopping the daemon stopped the system cluster"

log "OK"
