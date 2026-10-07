#!/usr/bin/env bash
# Project upgrades under real systemd units, through the Management API and the CLI: what
# Studio's Settings > General "Service versions" section and `supavise projects upgrade` do.
#
#   sudo SUPAVISE_BIN=/path/to/supavise-linux-amd64 tests/linux/upgrade-smoke.sh [--teardown]
#
# The node starts with versions.yaml pins that are older than the ones it is later updated to
# (two real slim-services releases of GoTrue and of PostgREST that both exist, an earlier
# packaging revision of GoTrue and an earlier PostgREST). Two projects are created on the old
# pins, with a table, rows and a signed-up auth user. Then:
#
#  1. The node is "updated" to a release whose pins are newer (the versions file changes, the
#     artifacts are fetched, the daemon restarts). The projects keep running the old versions:
#     service-versions still names them, the processes still map the old artifacts, REST works,
#     and eligibility says an upgrade is available.
#  2. A release whose PostgREST does not start (an artifact whose launcher exits 1). Upgrading
#     through POST /v1/projects/{ref}/upgrade answers 201 with the project UPGRADING (a second
#     upgrade and a pause are refused with 409), then ends with the upgrade status failed, the
#     project ACTIVE_HEALTHY on the old versions again, the data and the auth user intact.
#  3. The good release. The upgrade through the API ends with status 1 (upgraded),
#     ACTIVE_HEALTHY, service-versions and the processes on the new artifacts, PostgreSQL not
#     restarted (same PID), the rows and the auth user (same id, the old password still works)
#     intact, a base backup taken for each attempt, eligibility "up to date".
#  4. The second project goes through `supavise projects upgrade --all --yes` (canary path), and
#     `supavise projects versions` agrees. `supavise artifacts gc` then removes the old artifacts
#     nothing runs or keeps, and keeps the previous release's.
#
# Without SUPAVISE_BIN the script builds supavise with the go toolchain. It needs network access
# for the artifact downloads. Not run in development (root, systemd and Linux required); CI runs
# it on an ephemeral Ubuntu 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

ADMIN=http://127.0.0.1:7000
VERSIONS_SRC=$REPO_ROOT/internal/versions/versions.yaml
# Real slim-services releases (github.com/supabase/slim-services/releases), each with its linux
# amd64 and arm64 archives: GoTrue 2.195.0 in two packaging revisions and two PostgREST releases.
OLD_AUTH=auth-v2.195.0-r0 NEW_AUTH=auth-v2.195.0-r1
OLD_REST=postgrest-v16.2-r0 NEW_REST=postgrest-v16.4-r0
BAD_REST=postgrest-v0.0.0-broken-r0
OLD_AUTH_V=v2.195.0-r0 NEW_AUTH_V=v2.195.0-r1 OLD_REST_V=v16.2-r0 NEW_REST_V=v16.4-r0
USER_EMAIL=upgrade-smoke@example.com USER_PASSWORD=upgrade-correct-horse-battery

preflight
install_binary
setup_node
BASE_CONF=$(cat "$SUPAVISE_CONF")

# pins_file NAME AUTH POSTGREST: a copy of versions.yaml with these two pins.
pins_file() {
  local out=/etc/supavise/versions-$1.yaml
  python3 - "$VERSIONS_SRC" "$out" "$2" "$3" <<'PY'
import re, sys
src, out, auth, rest = sys.argv[1:5]
s = open(src).read()
s, n1 = re.subn(r"(?m)^(  auth: ).*$", r"\g<1>" + auth, s)
s, n2 = re.subn(r"(?m)^(  postgrest: ).*$", r"\g<1>" + rest, s)
if n1 != 1 or n2 != 1:
    sys.exit("versions.yaml has no auth and postgrest pins")
open(out, "w").write(s)
PY
  chmod 0644 "$out"
}
# use_pins NAME: the node's config names that versions file (the "release" the node runs).
use_pins() {
  printf '%s\n\n[artifacts]\nversions_file = "/etc/supavise/versions-%s.yaml"\n' "$BASE_CONF" "$1" >"$SUPAVISE_CONF"
  chown "$SUPAVISE_USER:$SUPAVISE_USER" "$SUPAVISE_CONF"
  chmod 0640 "$SUPAVISE_CONF"
}
restart_daemon() {
  systemctl restart supavise.service
  local i
  for ((i = 0; i < 120; i++)); do
    [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && return 0
    sleep 1
  done
  journalctl --no-pager -u supavise.service | tail -40 >&2
  fail "the Management API does not answer after the daemon restart"
}

pins_file old "$OLD_AUTH" "$OLD_REST"
pins_file new "$NEW_AUTH" "$NEW_REST"
pins_file bad "$NEW_AUTH" "$BAD_REST"

log "release 1: GoTrue $OLD_AUTH, PostgREST $OLD_REST; system init, daemon"
use_pins old
system_init
wait_active supavise-postgres@system.service 30
systemctl start supavise.service
for ((i = 0; i < 60; i++)); do
  [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && break
  sleep 1
done
[[ $(http_code "$ADMIN/v1/projects") == 401 ]] || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "the Management API does not answer"; }

log "two projects on the old versions"
claim_and_token
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' | json_get 'd["access_token"]') || fail "dashboard sign-in"
japi() { local m=$1 p=$2; shift 2; api "$m" "$p" -H "Authorization: Bearer $JWT" "$@"; }
gen_dbpass
REF=$(api_create_project upgrade-smoke)
gen_dbpass
REF2=$(api_create_project upgrade-smoke-2)
PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
pg_admin() { # REF SQL: as supabase_admin over the cluster's private socket
  local ref=$1 port
  port=$(project_field "$ref" 'd["ports"]["Postgres"]')
  sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/$ref/postgres/sock port=$port user=supabase_admin dbname=postgres" -Atc "$2" </dev/null
}
status() { papi GET "/v1/projects/$1" | json_get 'd["status"]'; }
code() { # METHOD PATH [BODY]
  local m=$1 p=$2 b=${3:-}
  papi "$m" "$p" -o /dev/null -w '%{http_code}' ${b:+-H 'Content-Type: application/json' -d "$b"} || true
}
must() { # STATUS METHOD PATH [BODY]
  local want=$1 got
  got=$(code "$2" "$3" "${4:-}")
  [[ $got == "$want" ]] || { log "response: $(papi "$2" "$3" ${4:+-H 'Content-Type: application/json' -d "$4"} | head -c 400)"; fail "$2 $3 answered $got, want $want"; }
}
wait_status() { # REF STATUS SECONDS
  local ref=$1 want=$2 n=${3:-600} i s=""
  for ((i = 0; i < n; i++)); do
    s=$(status "$ref" 2>/dev/null || true)
    [[ $s == "$want" ]] && return 0
    sleep 1
  done
  journalctl --no-pager -u supavise.service | tail -60 >&2
  fail "$ref is $s after ${n}s, want $want"
}
# upgrade_status REF PYEXPR: a field of GET /v1/projects/{ref}/upgrade/status's databaseUpgradeStatus.
upgrade_status() { papi GET "/v1/projects/$1/upgrade/status" | json_get "$2"; }
wait_upgrade() { # REF STATUS_NUMBER SECONDS: until the newest upgrade of REF has that status
  local ref=$1 want=$2 n=${3:-600} i s=""
  for ((i = 0; i < n; i++)); do
    s=$(upgrade_status "$ref" 'd["databaseUpgradeStatus"]["status"]' 2>/dev/null || true)
    [[ $s == "$want" ]] && return 0
    sleep 1
  done
  journalctl --no-pager -u supavise.service | tail -60 >&2
  fail "the upgrade of $ref has status '$s' after ${n}s, want $want"
}
# unit_runs UNIT TAG: a process of the unit maps files of that artifact release.
unit_runs() {
  local cg pid
  cg=$(systemctl show -p ControlGroup --value "$1")
  for pid in $(cat "/sys/fs/cgroup$cg/cgroup.procs" 2>/dev/null); do
    grep -q "/$2/" "/proc/$pid/maps" 2>/dev/null && return 0
  done
  return 1
}
runs() { unit_runs "supavise-$2@$1.service" "$3" || fail "supavise-$2@$1 does not run $3"; }
runs_not() { ! unit_runs "supavise-$2@$1.service" "$3" || fail "supavise-$2@$1 runs $3, which it should not"; }
service_versions() { # REF: "gotrue postgrest" as /platform/projects/{ref}/service-versions reports them
  japi GET "/platform/projects/$1/service-versions" | json_get 'd["gotrue"] + " " + d["postgrest"]'
}
pg_pid() { systemctl show -p MainPID --value "supavise-postgres@$1.service"; }

# Data in both projects: a table with rows, an auth user.
declare -A UID_OF
for r in "$REF" "$REF2"; do
  project_keys "$r"
  pg_admin "$r" "$SMOKE_TABLE_SQL" >/dev/null || fail "$r: create the table"
  rest_through_proxy "$r" "$PUB"
  resp=$(curl -sS -m 30 -H "Host: $r.api.$SUPAVISE_DOMAIN" -H "apikey: $PUB" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$USER_EMAIL\",\"password\":\"$USER_PASSWORD\"}" http://127.0.0.1/auth/v1/signup)
  UID_OF[$r]=$(json_get '(d.get("user") or d)["id"]' <<<"$resp") || fail "$r: sign-up: $resp"
done
project_keys "$REF"
# signs_in REF: the signed-up user logs in with the original password; prints the user id.
signs_in() {
  local r=$1 pub
  pub=$(papi GET "/v1/projects/$r/api-keys?reveal=true" | json_get '[x["api_key"] for x in d if str(x.get("api_key","")).startswith("sb_publishable_")][0]')
  curl -sS -m 30 -H "Host: $r.api.$SUPAVISE_DOMAIN" -H "apikey: $pub" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$USER_EMAIL\",\"password\":\"$USER_PASSWORD\"}" 'http://127.0.0.1/auth/v1/token?grant_type=password' | json_get 'd["user"]["id"]'
}
# intact REF: the rows, the user (same id), REST and a healthy project.
intact() {
  local r=$1 pub got
  pub=$(papi GET "/v1/projects/$r/api-keys?reveal=true" | json_get '[x["api_key"] for x in d if str(x.get("api_key","")).startswith("sb_publishable_")][0]')
  rest_through_proxy "$r" "$pub"
  [[ $(pg_admin "$r" "select count(*) from public.smoke_items") == 2 ]] || fail "$r: the rows are gone"
  got=$(signs_in "$r") || fail "$r: the signed-up user cannot sign in"
  [[ $got == "${UID_OF[$r]}" ]] || fail "$r: the user id is $got, want ${UID_OF[$r]}"
  [[ $(pg_admin "$r" "select count(*) from auth.users") == 1 ]] || fail "$r: auth.users does not hold exactly the signed-up user"
  supavise projects health "$r" || fail "$r is not healthy"
}

log "the projects run the old versions"
[[ $(service_versions "$REF") == "$OLD_AUTH_V $OLD_REST_V" ]] || fail "service versions: $(service_versions "$REF"), want $OLD_AUTH_V $OLD_REST_V"
runs "$REF" gotrue "$OLD_AUTH"
runs "$REF" postgrest "$OLD_REST"
intact "$REF"
intact "$REF2"

log "release 2: the node is updated to GoTrue $NEW_AUTH, PostgREST $NEW_REST; the projects stay on release 1"
use_pins new
supavise artifacts fetch gotrue postgrest
restart_daemon
wait_status "$REF" ACTIVE_HEALTHY 180
wait_status "$REF2" ACTIVE_HEALTHY 180
[[ $(service_versions "$REF") == "$OLD_AUTH_V $OLD_REST_V" ]] || fail "a node update moved a project: $(service_versions "$REF")"
runs "$REF" gotrue "$OLD_AUTH"
runs "$REF" postgrest "$OLD_REST"
ELIG=$(papi GET "/v1/projects/$REF/upgrade/eligibility")
[[ $(json_get 'd["eligible"]' <<<"$ELIG") == True ]] || fail "not eligible after a node update: $ELIG"
[[ $(json_get 'len(d["target_upgrade_versions"])' <<<"$ELIG") == 1 && $(json_get 'd["target_upgrade_versions"][0]["postgres_version"]' <<<"$ELIG") == 17 ]] || fail "target versions: $ELIG"
[[ $(json_get 'd["current_app_version"] == d["latest_app_version"]' <<<"$ELIG") == True ]] || fail "only GoTrue and PostgREST differ, so the Postgres app versions are equal: $ELIG"
[[ $(json_get 'len(d["validation_errors"]) + len(d["warnings"])' <<<"$ELIG") == 0 ]] || fail "unexpected blockers: $ELIG"
supavise projects versions | grep -q "available" || { supavise projects versions >&2; fail "projects versions does not show the available upgrade"; }
intact "$REF"
PG_PID=$(pg_pid "$REF")

log "release 3 does not start: an upgrade that fails is rolled back"
BAD_DIR=$SUPAVISE_STATE/artifacts/postgrest/$BAD_REST
sudo -u "$SUPAVISE_USER" mkdir -p "$BAD_DIR/bin"
printf '#!/bin/sh\necho "this release does not start" >&2\nexit 1\n' | sudo -u "$SUPAVISE_USER" tee "$BAD_DIR/bin/postgrest" >/dev/null
sudo -u "$SUPAVISE_USER" chmod 0755 "$BAD_DIR" "$BAD_DIR/bin" "$BAD_DIR/bin/postgrest"
use_pins bad
supavise artifacts fetch gotrue
restart_daemon
wait_status "$REF" ACTIVE_HEALTHY 180
must 400 POST "/v1/projects/$REF/upgrade" '{"target_version":"18"}'
must 400 POST "/v1/projects/$REF/upgrade" '{"target_version":"17","release_channel":"beta"}'
[[ $(status "$REF") == ACTIVE_HEALTHY ]] || fail "a refused upgrade changed the project"
must 201 POST "/v1/projects/$REF/upgrade" '{"target_version":"17","release_channel":"ga"}'
[[ $(status "$REF") == UPGRADING ]] || fail "the project is $(status "$REF") right after the upgrade began, want UPGRADING"
[[ $(japi GET "/platform/projects/$REF" | json_get 'd["status"]') == UPGRADING ]] || fail "the platform project does not say UPGRADING"
must 409 POST "/v1/projects/$REF/upgrade" '{"target_version":"17"}'
must 409 POST "/v1/projects/$REF/pause"
wait_upgrade "$REF" 2 600
wait_status "$REF" ACTIVE_HEALTHY 180
ERR=$(upgrade_status "$REF" 'd["databaseUpgradeStatus"]["error"]')
[[ $ERR == 5_data_upgrade_completion_failed ]] || fail "the failed upgrade's error is '$ERR'"
[[ $(service_versions "$REF") == "$OLD_AUTH_V $OLD_REST_V" ]] || fail "a failed upgrade changed the versions: $(service_versions "$REF")"
runs "$REF" gotrue "$OLD_AUTH"
runs "$REF" postgrest "$OLD_REST"
[[ $(pg_pid "$REF") == "$PG_PID" ]] || fail "PostgreSQL was restarted by an upgrade that only changes GoTrue and PostgREST"
intact "$REF"
[[ $(papi GET "/v1/projects/$REF/database/backups" | json_get 'len(d["backups"])') -ge 1 ]] || fail "no base backup was taken before the failed upgrade"
journalctl --no-pager -u supavise.service | grep -q 'msg=upgrade_failed' || fail "no upgrade_failed log line"

log "release 4, the good one: the upgrade through the API"
use_pins new
restart_daemon
wait_status "$REF" ACTIVE_HEALTHY 180
must 201 POST "/v1/projects/$REF/upgrade" '{"target_version":"17"}'
TRACK=$(papi GET "/v1/projects/$REF/upgrade/status" | json_get 'd["databaseUpgradeStatus"]["status"]')
[[ $TRACK == 0 || $TRACK == 1 ]] || fail "status right after the upgrade began: $TRACK"
wait_upgrade "$REF" 1 600
wait_status "$REF" ACTIVE_HEALTHY 180
[[ $(upgrade_status "$REF" 'd["databaseUpgradeStatus"]["progress"]') == 9_completed_upgrade ]] || fail "the finished upgrade's progress: $(upgrade_status "$REF" 'd["databaseUpgradeStatus"]')"
[[ $(service_versions "$REF") == "$NEW_AUTH_V $NEW_REST_V" ]] || fail "service versions after the upgrade: $(service_versions "$REF")"
runs "$REF" gotrue "$NEW_AUTH"
runs "$REF" postgrest "$NEW_REST"
runs_not "$REF" gotrue "$OLD_AUTH"
runs_not "$REF" postgrest "$OLD_REST"
[[ $(pg_pid "$REF") == "$PG_PID" ]] || fail "PostgreSQL was restarted"
intact "$REF"
ELIG=$(papi GET "/v1/projects/$REF/upgrade/eligibility")
[[ $(json_get 'd["eligible"]' <<<"$ELIG") == False && $(json_get 'len(d["target_upgrade_versions"])' <<<"$ELIG") == 0 ]] || fail "still eligible after the upgrade: $ELIG"
[[ $(papi GET "/v1/projects/$REF/database/backups" | json_get 'len(d["backups"])') -ge 2 ]] || fail "each upgrade attempt takes a base backup first"
must 400 POST "/v1/projects/$REF/upgrade" '{"target_version":"17"}'
# The other project did not move.
[[ $(service_versions "$REF2") == "$OLD_AUTH_V $OLD_REST_V" ]] || fail "the other project moved: $(service_versions "$REF2")"
runs "$REF2" gotrue "$OLD_AUTH"

log "the second project through the CLI: supavise projects upgrade --all --yes"
supavise projects upgrade --all --dry-run | grep -q "$REF2" || fail "--dry-run does not list $REF2"
supavise projects upgrade --all --yes || fail "supavise projects upgrade --all --yes"
wait_status "$REF2" ACTIVE_HEALTHY 180
[[ $(service_versions "$REF2") == "$NEW_AUTH_V $NEW_REST_V" ]] || fail "service versions of $REF2: $(service_versions "$REF2")"
runs "$REF2" gotrue "$NEW_AUTH"
runs "$REF2" postgrest "$NEW_REST"
intact "$REF2"
[[ $(upgrade_status "$REF2" 'd["databaseUpgradeStatus"]["status"]') == 1 ]] || fail "the CLI's upgrade left status $(upgrade_status "$REF2" 'd["databaseUpgradeStatus"]')"
supavise projects versions "$REF2" | grep -q "up to date" || { supavise projects versions "$REF2" >&2; fail "projects versions $REF2 does not say up to date"; }
[[ $(supavise projects versions --json | json_get 'sum(1 for p in d if p["upgrade"]["up_to_date"])') == 2 ]] || fail "projects versions --json"
supavise projects upgrade --all --yes | grep -q "nothing to upgrade" || fail "a second --all upgrades something"

log "artifact garbage collection: the old release goes, the previous one stays"
GC=$(supavise artifacts gc --dry-run)
grep -q "auth $OLD_AUTH" <<<"$GC" && grep -q "postgrest $OLD_REST" <<<"$GC" || { echo "$GC" >&2; fail "gc --dry-run does not list the old release"; }
grep -q "$BAD_REST" <<<"$GC" && fail "gc would remove a release the node ran last: $GC"
[[ -d $SUPAVISE_STATE/artifacts/auth/$OLD_AUTH ]] || fail "--dry-run removed an artifact"
supavise artifacts gc
[[ ! -e $SUPAVISE_STATE/artifacts/auth/$OLD_AUTH && ! -e $SUPAVISE_STATE/artifacts/postgrest/$OLD_REST ]] || fail "gc left the old artifacts"
[[ -d $SUPAVISE_STATE/artifacts/auth/$NEW_AUTH && -d $SUPAVISE_STATE/artifacts/postgrest/$NEW_REST ]] || fail "gc removed artifacts in use"
intact "$REF"
intact "$REF2"

log "upgrade smoke passed"
