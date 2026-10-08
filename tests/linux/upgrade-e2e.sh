#!/usr/bin/env bash
# Node upgrade and rollback end to end: `supavise upgrade` and `supavise rollback` on a node that
# deploy/install.sh installed from the previous release.
#
#   tests/linux/upgrade-e2e-build.sh /tmp/upgrade-bins
#   sudo UPGRADE_E2E_BIN_DIR=/tmp/upgrade-bins tests/linux/upgrade-e2e.sh
#
# The previous release (prev) is origin/main with older GoTrue, PostgREST, postgres-meta and Storage
# pins, v0.0.1. Two projects get a table, rows and an auth user. The releases are signed with a
# throwaway key and served by a local server that stands in for GitHub (the same one install-e2e.sh
# uses). The cases, in order:
#
#  1. `--check` and `--plan` change nothing (nothing is fetched, the marker is not written).
#  2. Refusals leave the node as it was and exit with status 2: a release that does not exist, one
#     whose manifest says it upgrades from v0.0.5, a binary that does not match its checksum,
#     `--unattended` on a node that is not backed up and escrowed, a release that pins an
#     artifact that cannot be fetched, and a config whose bin_path names another file (root runs
#     only the installed binary, never a path the supavise user can write: nothing is executed).
#  3. The upgrade to v0.0.2 (the driver is not the installed binary: the upgrade reads the new
#     binary's pins, backs everything up, swaps, restarts the daemon, rolls pgmeta, Storage and the
#     system GoTrue, then the two projects with a canary): versions in the registry and in the
#     running processes, PostgreSQL untouched (same PID), data and the auth user intact, the kept
#     releases, the marker, `supavise status` healthy, and a second run that has nothing to do.
#  4. A release whose daemon dies when it starts (v0.0.3): exit status 3, the node back on v0.0.2.
#  5. A release whose PostgREST does not start (v0.0.4): the rollout halts at the first project, the
#     other project is not touched, exit status 3, back on v0.0.2.
#  6. The upgrade to v0.0.5 (a newer GoTrue) and `supavise rollback`: refused while the registry
#     holds a migration v0.0.2 does not know, then the projects go back to the GoTrue they ran and
#     the binary to v0.0.2, data intact; a second rollback steps back to v0.0.1 and is refused for
#     the same reason when the registry holds a migration v0.0.1 does not know (origin/main has
#     not got this branch's migrations until it is merged).
#  5b. A release that moves GoTrue and whose daemon dies when it starts (a stub of v0.0.5 as v0.0.8):
#     the rollback must not wait for the services the dead daemon never moved; exit status 3.
#  7. A release that moves no service and renders PostgREST's environment differently (v0.0.6): the
#     daemon starts without restarting the projects, the upgrade's rollout restarts each project's
#     PostgREST (canary first), PostgreSQL untouched, data intact.
#  8. SIGTERM to `supavise upgrade` while the projects are being upgraded (a stub of v0.0.5 as
#     v0.0.9): the worker is told to stop and is not killed, the upgrade rolls back (exit status 3),
#     and no project is left UPGRADING.
#
#  9. A release that renders the projects' PostgreSQL differently (v0.0.10): the daemon leaves the
#     clusters running, the rollout restarts them.
# 10. `supavise rollback` from v0.0.10: its output names the restarts, and the old daemon restarts the
#     clusters onto the old settings.
# 11. Automatic upgrades: [update] mode = "auto" and supavise-upgrade.service, the unit the maintenance
#     window's timer starts (its ExecStart gets the release server of this test and nothing else). With
#     the window somewhere else it does nothing. With a window that covers now it runs a real
#     `supavise upgrade --unattended` from v0.0.6 to v0.0.11 (both builds of this checkout; the installed
#     build drives the upgrade, so it is the one that raises the alerts), exits 0, records the result in
#     its StateDirectory (/var/lib/supavise-upgrade, the unit's $STATE_DIRECTORY, never a path from
#     config.toml) and sends upgrade_started and upgrade_succeeded to a local webhook receiver. A second
#     start in the same window does nothing.

# Needs root, systemd, cgroup v2 and network access (artifact downloads). Do not run it on a
# machine you care about: it creates the supavise user, writes /etc/supavise and starts real
# clusters. Exit status is non-zero on the first failure.
BASE=127.0.0.1.sslip.io
export SUPAVISE_DOMAIN=$BASE
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

BINS=${UPGRADE_E2E_BIN_DIR:?run tests/linux/upgrade-e2e-build.sh OUT_DIR first and pass UPGRADE_E2E_BIN_DIR}
SRV_PORT=38801
WORK=$(mktemp -d)
# The supavise user reads the release key (--plan as that user); the private files keep their own modes.
chmod 0755 "$WORK"
SRV_PID=""
HOOK_PID=""
cleanup() {
  local rc=$?
  [[ -n $SRV_PID ]] && kill "$SRV_PID" 2>/dev/null || true
  [[ -n $HOOK_PID ]] && kill "$HOOK_PID" 2>/dev/null || true
  journalctl --no-pager -u supavise-upgrade.service >"$WORK/outputs/supavise-upgrade.journal" 2>/dev/null || true
  collect_logs
  cp -r "$WORK/outputs" "$LOG_DIR/" 2>/dev/null || true
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT
mkdir -p "$WORK/outputs"

need_root
preflight
ARCH=$(dpkg --print-architecture)
cd "$REPO_ROOT" || exit 1
for f in prev v2 v4 v5 v6 v7 v8 releasetool; do [[ -x $BINS/$f ]] || fail "$BINS/$f is missing: run tests/linux/upgrade-e2e-build.sh"; done
# The driver and the stubs are run by the supavise user too (as workers): out of a private directory.
install -d -m 0755 /opt/supavise-e2e
for f in prev v2 v4 v5 v6 v7 v8; do install -m 0755 "$BINS/$f" "/opt/supavise-e2e/$f"; done
B=/opt/supavise-e2e
SV=/usr/local/bin/supavise
STATE=$SUPAVISE_STATE
RELEASES=/usr/local/lib/supavise/releases
OLD_AUTH=auth-v2.195.0-r0 OLD_REST=postgrest-v16.2-r0 OLD_META=pgmeta-v0.99.0-r0 OLD_STORAGE=storage-v1.79.35-r0
NEW_AUTH=$(awk '/^  auth:/{print $2}' internal/versions/versions.yaml)
NEW_REST=$(awk '/^  postgrest:/{print $2}' internal/versions/versions.yaml)
NEW_META=$(awk '/^  pgmeta:/{print $2}' internal/versions/versions.yaml)
NEW_STORAGE=$(awk '/^  storage:/{print $2}' internal/versions/versions.yaml)
NEWER_AUTH=auth-v2.196.0-r0
BAD_REST=postgrest-v99.0.0-r0
USER_EMAIL=upgrade-e2e@example.com USER_PASSWORD=upgrade-correct-horse-battery

# ---- the release server --------------------------------------------------------------------
KEYS=$WORK/keys; mkdir -p "$KEYS"
openssl genpkey -algorithm ed25519 -out "$KEYS/sign.pem"
openssl pkey -in "$KEYS/sign.pem" -pubout -out "$KEYS/pub.pem"
make_release() { # TAG BINARY VERSIONS-YAML [latest|nolatest]: $WORK/srv/download/TAG and the API files for repo o/r
  local tag=$1 bin=$2 yaml=$3 latest=${4:-nolatest} d=$WORK/srv/download/$1
  rm -rf "$d"; mkdir -p "$d"
  cp "$bin" "$d/supavise-linux-$ARCH"
  if [[ $ARCH == amd64 ]]; then echo other-arch >"$d/supavise-linux-arm64"; else echo other-arch >"$d/supavise-linux-amd64"; fi
  echo "not a real studio build" >"$d/supavise-studio-test-p1-linux-$ARCH.tar.zst"
  SUPAVISE_RELEASE_TAG=$tag SUPAVISE_RELEASETOOL=$BINS/releasetool SUPAVISE_VERSIONS_FILE=$yaml \
    deploy/release-assets.sh "$d" "$KEYS/sign.pem" "$KEYS/pub.pem" >/dev/null
  mkdir -p "$WORK/srv/repos/o/r/releases/tags"
  python3 - "$tag" "$d" "$SRV_PORT" "$WORK/srv/repos/o/r/releases" "$latest" <<'PY'
import json, os, sys
tag, d, port, out, latest = sys.argv[1:]
rel = {"tag_name": tag, "assets": [{"name": n, "browser_download_url": f"http://127.0.0.1:{port}/download/{tag}/{n}"} for n in sorted(os.listdir(d))]}
for name in (("latest",) if latest == "latest" else ()) + (f"tags/{tag}",):
    with open(os.path.join(out, name), "w") as f:
        json.dump(rel, f)
PY
}
mkdir -p "$WORK/srv"
make_release v0.0.1 "$B/prev" "$BINS/prev.versions.yaml"
(cd "$WORK/srv" && exec python3 -m http.server "$SRV_PORT" --bind 127.0.0.1 >"$WORK/http.log" 2>&1) &
SRV_PID=$!
for ((i = 0; i < 20; i++)); do [[ $(http_code "http://127.0.0.1:$SRV_PORT/download/v0.0.1/SHA256SUMS") == 200 ]] && break; sleep 0.5; done

# ---- install the previous release ---------------------------------------------------------
systemctl stop apache2 nginx postgresql mysql 2>/dev/null || true
for p in 80 443 5432 6543 5433 9999 7000 3000 8080 4000 5000; do
  if ss -ltnH "sport = :$p" | grep -q .; then ss -ltnp "sport = :$p" >&2; fail "port $p is already in use on this VM"; fi
done
log "install.sh --binary: the previous release (v0.0.1, older GoTrue, PostgREST, postgres-meta and Storage)"
deploy/install.sh --binary "$B/prev" --public-ip 127.0.0.1 --tls off --email ci@example.com --firewall none --no-studio \
  --claim-token-file "$WORK/claim-token" 2>&1 | tee "$WORK/install.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "install.sh failed"
[[ $($SV --version) == *v0.0.1* ]] || fail "the installed binary is $($SV --version)"
for u in supavise.service supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service supavise-supavisor.service supavise-realtime.service supavise-storage.service; do wait_active "$u" 90; done
ADMIN=http://127.0.0.1:7000

# ---- helpers --------------------------------------------------------------------------------
PSQL=$(ls -d "$STATE"/artifacts/postgres/*/bin/psql | head -1)
pg_sock() { # REF SQL DB: as supabase_admin over the cluster's private socket
  local ref=$1 sql=$2 db=${3:-postgres} port
  if [[ $ref == system ]]; then port=5433; else port=$(sudo -u "$SUPAVISE_USER" "$PSQL" "host=$STATE/projects/system/postgres/sock port=5433 user=supabase_admin dbname=supavise" -Atc "select 20000 + 3 * seq from supavise.projects where ref = '$ref'" </dev/null); fi
  sudo -u "$SUPAVISE_USER" "$PSQL" "host=$STATE/projects/$ref/postgres/sock port=$port user=supabase_admin dbname=$db" -Atc "$sql" </dev/null
}
reg() { pg_sock system "$1" supavise; }
versions_of() { reg "select coalesce(versions->>'gotrue','') || ' ' || coalesce(versions->>'postgrest','') || ' ' || coalesce(versions->>'postgres','') from supavise.projects where ref = '$1'"; }
pg_pid() { systemctl show -p MainPID --value "supavise-postgres@$1.service"; }
# unit_runs UNIT TAG: a process of the unit maps files of that artifact release.
unit_runs() {
  local cg pid
  cg=$(systemctl show -p ControlGroup --value "$1")
  for pid in $(cat "/sys/fs/cgroup$cg/cgroup.procs" 2>/dev/null); do
    grep -q "/$2/" "/proc/$pid/maps" 2>/dev/null && return 0
  done
  return 1
}
runs() { unit_runs "$1" "$2" || fail "$1 does not run $2"; }
runs_not() { ! unit_runs "$1" "$2" || fail "$1 runs $2, which it should not"; }
daemon_version() { "/proc/$(systemctl show -p MainPID --value supavise.service)/exe" --version; }
wait_daemon() {
  local i
  for ((i = 0; i < 120; i++)); do [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && return 0; sleep 1; done
  journalctl --no-pager -u supavise.service | tail -40 >&2
  fail "the Management API does not answer"
}
wait_status() { # REF STATUS SECONDS
  local ref=$1 want=$2 n=${3:-300} i s=""
  for ((i = 0; i < n; i++)); do
    s=$(papi GET "/v1/projects/$ref" | json_get 'd["status"]' 2>/dev/null || true)
    [[ $s == "$want" ]] && return 0
    sleep 1
  done
  fail "$ref is $s after ${n}s, want $want"
}
# run CMD...: OUT and RC from a command that may fail; the output is kept in $WORK/outputs too.
run() {
  local n; n=$(ls "$WORK/outputs" | wc -l)
  if OUT=$("$@" 2>&1); then RC=0; else RC=$?; fi
  printf '%s\n' "$OUT" >"$WORK/outputs/$(printf '%02d' "$n").log"
}
upgrade() { # BINARY args...: the driver is a copy of the target release, not the installed binary
  local bin=$1; shift
  timeout 3000 "$bin" upgrade --repo o/r --api-base "http://127.0.0.1:$SRV_PORT" --public-key-file "$KEYS/pub.pem" "$@"
}
marker() { python3 -c 'import json; d=json.load(open("/var/lib/supavise/system/upgrade.json")); print(d["phase"], d["from"], d["to"])'; }
unchanged() { # WHAT: the node is as the last assertion left it
  [[ $($SV --version) == *"$EXPECT_VERSION"* ]] || fail "$1: the installed binary is $($SV --version), want $EXPECT_VERSION"
  [[ $(pg_pid "$REF") == "$PG_PID" && $(pg_pid "$REF2") == "$PG_PID2" ]] || fail "$1: a project's PostgreSQL was restarted"
}

# ---- two projects with data ------------------------------------------------------------------
log "claim, token, two projects with a table, rows and an auth user"
claim_and_token
gen_dbpass; REF=$(api_create_project upgrade-e2e)
gen_dbpass; REF2=$(api_create_project upgrade-e2e-2)
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' | json_get 'd["access_token"]') || fail "dashboard sign-in"
declare -A UID_OF
for r in "$REF" "$REF2"; do
  project_keys "$r"
  pg_sock "$r" "$SMOKE_TABLE_SQL" >/dev/null || fail "$r: create the table"
  rest_through_proxy "$r" "$PUB"
  resp=$(curl -sS -m 30 -H "Host: $r.api.$BASE" -H "apikey: $PUB" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$USER_EMAIL\",\"password\":\"$USER_PASSWORD\"}" http://127.0.0.1/auth/v1/signup)
  UID_OF[$r]=$(json_get '(d.get("user") or d)["id"]' <<<"$resp") || fail "$r: sign-up: $resp"
done
intact() { # REF: rows, the user (same id), REST, a healthy project
  local r=$1 pub got
  pub=$(papi GET "/v1/projects/$r/api-keys?reveal=true" | json_get '[x["api_key"] for x in d if str(x.get("api_key","")).startswith("sb_publishable_")][0]')
  rest_through_proxy "$r" "$pub"
  [[ $(pg_sock "$r" "select count(*) from public.smoke_items") == 2 ]] || fail "$r: the rows are gone"
  got=$(curl -sS -m 30 -H "Host: $r.api.$BASE" -H "apikey: $pub" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$USER_EMAIL\",\"password\":\"$USER_PASSWORD\"}" 'http://127.0.0.1/auth/v1/token?grant_type=password' | json_get 'd["user"]["id"]') || fail "$r: the user cannot sign in"
  [[ $got == "${UID_OF[$r]}" ]] || fail "$r: the user id is $got, want ${UID_OF[$r]}"
  [[ $(papi GET "/v1/projects/$r" | json_get 'd["status"]') == ACTIVE_HEALTHY ]] || fail "$r is not ACTIVE_HEALTHY"
  project_keys "$r"
  [[ $(curl -s -o /dev/null -w '%{http_code}' -m 30 -H "Host: $r.api.$BASE" -H "apikey: $SEC" "http://127.0.0.1/storage/v1/bucket") == 200 ]] || fail "$r: Storage does not serve the project"
}
EXPECT_VERSION=v0.0.1
PG_PID=$(pg_pid "$REF") PG_PID2=$(pg_pid "$REF2")
[[ $(versions_of "$REF") == "$OLD_AUTH $OLD_REST "* ]] || fail "the project does not record the previous release's pins: $(versions_of "$REF")"
runs supavise-gotrue@$REF.service "$OLD_AUTH"
runs supavise-pgmeta.service "$OLD_META"
runs supavise-storage.service "$OLD_STORAGE"
runs supavise-gotrue@system.service "$OLD_AUTH"
intact "$REF"; intact "$REF2"

# ---- 1. --check and --plan change nothing ----------------------------------------------------
make_release v0.0.2 "$B/v2" "$BINS/v2.versions.yaml" latest
log "--check and --plan: read-only"
run upgrade "$B/v2" --check
[[ $RC -eq 0 && $OUT == *"installed v0.0.1, newest v0.0.2: update available"* ]] || fail "--check: $RC $OUT"
run upgrade "$B/v2" --plan
[[ $RC -eq 0 ]] || fail "--plan exited $RC: $OUT"
for want in "Supavise v0.0.1 -> v0.0.2" "gotrue" "pgmeta" "storage" "Projects (gotrue" "What restarts:" "Expected impact:" "supavise.service" "canary first"; do
  [[ $OUT == *"$want"* ]] || fail "--plan lacks '$want':
$OUT"
done
[[ $OUT == *"2 to upgrade"* ]] || fail "--plan does not count the two projects: $OUT"
unchanged "--plan"
[[ ! -e $STATE/artifacts/auth/$NEW_AUTH && ! -e $STATE/artifacts/storage/$NEW_STORAGE ]] || fail "--plan fetched artifacts"
[[ ! -e $RELEASES ]] || fail "--plan kept a release"
[[ ! -e $STATE/system/upgrade.json ]] || fail "--plan wrote the upgrade marker"
ls /usr/local/bin/.supavise.new-* >/dev/null 2>&1 && fail "--plan left a staged binary in /usr/local/bin"
# --plan as the supavise user needs no root either.
run sudo -u "$SUPAVISE_USER" -H "$B/v2" upgrade --repo o/r --api-base "http://127.0.0.1:$SRV_PORT" --public-key-file "$KEYS/pub.pem" --plan
[[ $RC -eq 0 && $OUT == *"Supavise v0.0.1 -> v0.0.2"* ]] || fail "--plan as the supavise user: $RC $OUT"
run sudo -u "$SUPAVISE_USER" -H "$B/v2" upgrade --repo o/r --api-base "http://127.0.0.1:$SRV_PORT" --public-key-file "$KEYS/pub.pem" --yes
[[ $RC -eq 2 && $OUT == *"run as root"* ]] || fail "an upgrade without root: $RC $OUT"

# ---- 2. refusals ----------------------------------------------------------------------------
log "refusals exit with status 2 and leave the node alone"
run upgrade "$B/v2" --yes --version v9.9.9
[[ $RC -eq 2 && $OUT == *"not found"* ]] || fail "a release that does not exist: $RC $OUT"
unchanged "missing release"
SUPAVISE_MIN_UPGRADE_FROM=v0.0.5 make_release v0.0.6 "$B/v2" "$BINS/v2.versions.yaml"
run upgrade "$B/v2" --yes --version v0.0.6
[[ $RC -eq 2 && $OUT == *"upgrades from v0.0.5 or later"* ]] || fail "min_upgrade_from: $RC $OUT"
unchanged "min_upgrade_from"
# Root runs only the installed binary. A bin_path in the config (the supavise user owns that file)
# that names another file is refused before anything is executed, `--check` included.
printf '#!/bin/sh\ntouch %s/evil-ran\n' "$WORK" >"$WORK/evil-supavise"; chmod 755 "$WORK/evil-supavise"
{ echo "bin_path = \"$WORK/evil-supavise\""; grep -v '^bin_path' /etc/supavise/config.toml; } >"$WORK/evil.toml"
run upgrade "$B/v2" --check --config "$WORK/evil.toml"
[[ $RC -eq 2 && $OUT == *"bin_path"* && $OUT == *"$SV"* ]] || fail "a foreign bin_path: $RC $OUT"
run upgrade "$B/v2" --yes --version v0.0.2 --config "$WORK/evil.toml"
[[ $RC -eq 2 && $OUT == *"bin_path"* ]] || fail "a foreign bin_path with --yes: $RC $OUT"
[[ ! -e $WORK/evil-ran ]] || fail "root ran the binary that bin_path named"
unchanged "foreign bin_path"
make_release v0.0.7 "$B/v2" "$BINS/v2.versions.yaml"
echo tampered >>"$WORK/srv/download/v0.0.7/supavise-linux-$ARCH"
run upgrade "$B/v2" --yes --version v0.0.7
[[ $RC -eq 2 && $OUT == *"does not match its checksum"* ]] || fail "a tampered binary: $RC $OUT"
unchanged "tampered binary"
run upgrade "$B/v2" --unattended
[[ $RC -eq 2 && ( $OUT == *"unattended upgrade"* || $OUT == *"master key"* || $OUT == *"no backup newer"* ) ]] || fail "--unattended on a node without escrow and backups: $RC $OUT"
unchanged "--unattended"
ls /usr/local/bin/.supavise.new-* >/dev/null 2>&1 && fail "a refused upgrade left a staged binary in /usr/local/bin"
[[ ! -e $RELEASES ]] || fail "a refusal kept a release"

# A release that pins an artifact that cannot be fetched: nothing is stopped or swapped. (v0.0.4
# pins a PostgREST release that does not exist until the test creates its directory below.)
make_release v0.0.4 "$B/v4" "$BINS/v4.versions.yaml"
log "a release whose artifacts cannot be fetched is refused before anything changes"
run upgrade "$B/v4" --yes --version v0.0.4
[[ $RC -eq 2 && $OUT == *"nothing was changed"* ]] || fail "an unfetchable artifact: $RC $OUT"
unchanged "unfetchable artifact"
[[ $(marker) == "refused v0.0.1 v0.0.4" ]] || fail "marker after the refusal: $(marker)"
[[ ! -e $RELEASES/v0.0.1 ]] || fail "a refusal before the swap kept a release"

# ---- 3. the upgrade ------------------------------------------------------------------------
log "supavise upgrade --yes --version v0.0.2"
SUPAVISE_UPGRADE_TIMEOUT=3000
run upgrade "$B/v2" --yes --version v0.0.2
[[ $RC -eq 0 ]] || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "the upgrade exited $RC: $OUT"; }
for want in "taking a base backup of 3 project(s)" "installing v0.0.2" "rolling" "upgrading 2 project(s)" "Supavise v0.0.2 is running"; do
  [[ $OUT == *"$want"* ]] || fail "the upgrade's output lacks '$want':
$OUT"
done
[[ $OUT == *"upgrade_started"* && $OUT == *"upgrade_succeeded"* ]] || fail "the upgrade logged no upgrade_started and upgrade_succeeded"
EXPECT_VERSION=v0.0.2
[[ $($SV --version) == *v0.0.2* && $(daemon_version) == *v0.0.2* ]] || fail "binary $($SV --version), daemon $(daemon_version)"
wait_daemon
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
[[ $(marker) == "done v0.0.1 v0.0.2" ]] || fail "marker: $(marker)"
[[ $(versions_of "$REF") == "$NEW_AUTH $NEW_REST "* && $(versions_of "$REF2") == "$NEW_AUTH $NEW_REST "* ]] || fail "versions: $(versions_of "$REF") / $(versions_of "$REF2")"
for r in "$REF" "$REF2"; do
  runs "supavise-gotrue@$r.service" "$NEW_AUTH"; runs_not "supavise-gotrue@$r.service" "$OLD_AUTH"
  runs "supavise-postgrest@$r.service" "$NEW_REST"; runs_not "supavise-postgrest@$r.service" "$OLD_REST"
done
runs supavise-pgmeta.service "$NEW_META"; runs_not supavise-pgmeta.service "$OLD_META"
runs supavise-storage.service "$NEW_STORAGE"; runs_not supavise-storage.service "$OLD_STORAGE"
runs supavise-gotrue@system.service "$NEW_AUTH"
[[ $(pg_pid "$REF") == "$PG_PID" && $(pg_pid "$REF2") == "$PG_PID2" ]] || fail "a project's PostgreSQL was restarted by an upgrade that only moves GoTrue and PostgREST"
intact "$REF"; intact "$REF2"
[[ $(reg "select count(*) from supavise.backups where status = 'completed' and ref in ('$REF','$REF2','system')") -ge 3 ]] || fail "the upgrade took no base backups"
[[ $(reg "select count(*) from supavise.project_upgrades where status = 1") == 2 ]] || fail "the projects' upgrade rows"
# Two kept releases, root's, with their pins and the schema they run on.
for v in v0.0.1 v0.0.2; do
  [[ -x $RELEASES/$v/supavise && $(stat -c %U "$RELEASES/$v/supavise") == root ]] || fail "kept release $v: $(ls -l "$RELEASES/$v" 2>&1)"
done
[[ $(python3 -c 'import json; d=json.load(open("'"$RELEASES"'/v0.0.1/release.json")); print(d["pins"]["gotrue"], len(d["registry_migrations"]) > 0, d["migrations_from"] in ("applied", "binary"))') == "$OLD_AUTH True True" ]] || fail "the record of v0.0.1: $(cat "$RELEASES/v0.0.1/release.json")"
sup_status=0; supavise status >"$WORK/status.txt" || sup_status=$?
[[ $sup_status -eq 0 ]] || { cat "$WORK/status.txt" >&2; fail "supavise status exited $sup_status after the upgrade"; }
ARTS=$($SV artifacts list); grep -q "$NEW_AUTH" <<<"$ARTS" || fail "artifacts list does not show the new GoTrue"
# The previous release's artifacts stay for a rollback, although the previous daemon recorded no history.
for a in "auth/$OLD_AUTH" "pgmeta/$OLD_META" "storage/$OLD_STORAGE"; do [[ -d $STATE/artifacts/$a ]] || fail "the upgrade's cleanup removed $a, which the previous release needs"; done
JOURNAL=$(journalctl --no-pager -u supavise.service)
grep -q 'projects registered with the shared services' <<<"$JOURNAL" || fail "the daemon did not register the projects with the shared services after the start"
run upgrade "$B/v2" --yes --version v0.0.2
[[ $RC -eq 0 && $OUT == *"nothing to change"* ]] || fail "a second upgrade should have nothing to do: $RC $OUT"
unchanged "second upgrade"

# ---- 4. a daemon that dies --------------------------------------------------------------------
log "a release whose daemon exits: exit status 3 and the node back on v0.0.2"
cp "$B/v2" /opt/supavise-e2e/v2-real
cat >"$WORK/stub" <<'EOF'
#!/bin/sh
REAL=/opt/supavise-e2e/v2-real
for a in "$@"; do [ "$a" = serve ] && { echo "simulated crash" >&2; exit 1; }; done
[ "$1" = --version ] && { echo "supavise version v0.0.3"; exit 0; }
if [ "$1" = release-info ]; then "$REAL" release-info --json | sed 's/"version": "v0.0.2"/"version": "v0.0.3"/'; exit $?; fi
exec "$REAL" "$@"
EOF
chmod 755 "$WORK/stub"
make_release v0.0.3 "$WORK/stub" "$BINS/v2.versions.yaml"
run upgrade "$B/v2" --yes --version v0.0.3 --wait 20s
[[ $RC -eq 3 ]] || { journalctl --no-pager -u supavise.service | tail -40 >&2; fail "a daemon that exits: exit $RC, want 3: $OUT"; }
[[ $OUT == *"rolled back"* && $OUT == *"upgrade_rolled_back"* ]] || fail "rollback message: $OUT"
EXPECT_VERSION=v0.0.2
unchanged "stub release"
wait_daemon; [[ $(daemon_version) == *v0.0.2* ]] || fail "the daemon does not run the restored binary: $(daemon_version)"
[[ $(marker) == rolled_back* ]] || fail "marker: $(marker)"
[[ $(versions_of "$REF") == "$NEW_AUTH $NEW_REST "* ]] || fail "versions changed: $(versions_of "$REF")"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
intact "$REF"; intact "$REF2"
[[ $(python3 -c 'import json; print(json.load(open("'"$RELEASES"'/v0.0.3/release.json")).get("withdrawn"))') == True ]] || fail "the failed release is still a rollback target"

# ---- 5. a project that does not start ---------------------------------------------------------
log "a release whose PostgREST does not start: the rollout halts at the first project, exit status 3"
BAD_DIR=$STATE/artifacts/postgrest/$BAD_REST
sudo -u "$SUPAVISE_USER" mkdir -p "$BAD_DIR/bin"
printf '#!/bin/sh\necho "this release does not start" >&2\nexit 1\n' | sudo -u "$SUPAVISE_USER" tee "$BAD_DIR/bin/postgrest" >/dev/null
sudo -u "$SUPAVISE_USER" chmod 0755 "$BAD_DIR" "$BAD_DIR/bin" "$BAD_DIR/bin/postgrest"
UPGRADES_BEFORE=$(reg "select count(*) from supavise.project_upgrades")
run upgrade "$B/v4" --yes --version v0.0.4
[[ $RC -eq 3 ]] || { journalctl --no-pager -u supavise.service | tail -40 >&2; fail "a project that does not start: exit $RC, want 3: $OUT"; }
[[ $OUT == *"rollout"* && $OUT == *"upgrade_failed"* && $OUT == *"rolled back"* ]] || fail "the output does not show the halted rollout and the rollback: $OUT"
unchanged "bad PostgREST release"
wait_daemon; [[ $(daemon_version) == *v0.0.2* ]] || fail "the daemon runs $(daemon_version)"
[[ $(reg "select count(*) from supavise.project_upgrades") -le $((UPGRADES_BEFORE + 1)) ]] || fail "the rollout went on after the first failure: $(reg "select ref, status from supavise.project_upgrades order by initiated_at")"
[[ $(versions_of "$REF") == "$NEW_AUTH $NEW_REST "* && $(versions_of "$REF2") == "$NEW_AUTH $NEW_REST "* ]] || fail "versions: $(versions_of "$REF") / $(versions_of "$REF2")"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
for r in "$REF" "$REF2"; do runs "supavise-postgrest@$r.service" "$NEW_REST"; done
intact "$REF"; intact "$REF2"

# ---- 5b. a daemon that dies after the release moved a shared service ---------------------------
log "a release that moves GoTrue and whose daemon exits: the rollback does not wait for services that never moved, exit status 3"
cp "$B/v5" /opt/supavise-e2e/v5-real
cat >"$WORK/stub5" <<'EOF'
#!/bin/sh
REAL=/opt/supavise-e2e/v5-real
for a in "$@"; do [ "$a" = serve ] && { echo "simulated crash" >&2; exit 1; }; done
[ "$1" = --version ] && { echo "supavise version v0.0.8"; exit 0; }
if [ "$1" = release-info ]; then "$REAL" release-info --json | sed 's/"version": "v0.0.5"/"version": "v0.0.8"/'; exit $?; fi
exec "$REAL" "$@"
EOF
chmod 755 "$WORK/stub5"
make_release v0.0.8 "$WORK/stub5" "$BINS/v5.versions.yaml"
run upgrade "$B/v5" --yes --version v0.0.8 --wait 20s
[[ $RC -eq 3 ]] || { journalctl --no-pager -u supavise.service | tail -40 >&2; fail "a daemon that exits on a release that moves GoTrue: exit $RC, want 3: $OUT"; }
[[ $OUT == *"rolled back"* ]] || fail "rollback message: $OUT"
unchanged "stub release that moves GoTrue"
wait_daemon; [[ $(daemon_version) == *v0.0.2* ]] || fail "the daemon does not run the restored binary: $(daemon_version)"
[[ $(versions_of "$REF") == "$NEW_AUTH $NEW_REST "* ]] || fail "versions changed: $(versions_of "$REF")"
runs supavise-gotrue@system.service "$NEW_AUTH"; runs_not supavise-gotrue@system.service "$NEWER_AUTH"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
intact "$REF"; intact "$REF2"

# ---- 6. upgrade to v0.0.5 and roll back -----------------------------------------------------
log "upgrade to v0.0.5 (a newer GoTrue)"
make_release v0.0.5 "$B/v5" "$BINS/v5.versions.yaml"
run upgrade "$B/v5" --yes --version v0.0.5
[[ $RC -eq 0 ]] || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "the upgrade to v0.0.5 exited $RC: $OUT"; }
EXPECT_VERSION=v0.0.5
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
[[ $(versions_of "$REF") == "$NEWER_AUTH $NEW_REST "* && $(versions_of "$REF2") == "$NEWER_AUTH $NEW_REST "* ]] || fail "versions after v0.0.5: $(versions_of "$REF")"
runs "supavise-gotrue@$REF.service" "$NEWER_AUTH"; runs supavise-gotrue@system.service "$NEWER_AUTH"
intact "$REF"; intact "$REF2"

log "rollback is refused while the registry is newer than the previous release expects"
reg "insert into supavise.schema_migrations (version) values ('migrations/9999_from_the_future.sql')" >/dev/null
run "$SV" rollback --yes
[[ $RC -eq 2 && $OUT == *"restore the system cluster from its pre-upgrade base backup"* ]] || fail "rollback with a newer registry: $RC $OUT"
unchanged "refused rollback"
[[ $(versions_of "$REF") == "$NEWER_AUTH $NEW_REST "* ]] || fail "a refused rollback moved a project"
reg "delete from supavise.schema_migrations where version = 'migrations/9999_from_the_future.sql'" >/dev/null

log "projects upgrade <ref> plans that project alone, whichever of the two it is (a revert must touch nothing else)"
for pair in "$REF $REF2" "$REF2 $REF"; do
  read -r mine other <<<"$pair"
  run supavise projects upgrade "$mine" --dry-run --allow-older --to "gotrue=$NEW_AUTH"
  [[ $RC -eq 0 && $OUT == *"$mine"* && $OUT != *"$other"* ]] || fail "projects upgrade $mine --dry-run lists or acts on the other project: $RC $OUT"
done

log "supavise rollback --yes: back to v0.0.2, the projects back on their GoTrue"
run "$SV" rollback --yes
[[ $RC -eq 0 ]] || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "rollback exited $RC: $OUT"; }
EXPECT_VERSION=v0.0.2
[[ $($SV --version) == *v0.0.2* ]] || fail "rollback left $($SV --version)"
wait_daemon; [[ $(daemon_version) == *v0.0.2* ]] || fail "the daemon runs $(daemon_version)"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
[[ $(versions_of "$REF") == "$NEW_AUTH $NEW_REST "* && $(versions_of "$REF2") == "$NEW_AUTH $NEW_REST "* ]] || fail "versions after the rollback: $(versions_of "$REF") / $(versions_of "$REF2")"
for r in "$REF" "$REF2"; do runs "supavise-gotrue@$r.service" "$NEW_AUTH"; runs_not "supavise-gotrue@$r.service" "$NEWER_AUTH"; done
runs supavise-gotrue@system.service "$NEW_AUTH"
[[ $(marker) == rolled_back* ]] || fail "marker after the rollback: $(marker)"
intact "$REF"; intact "$REF2"
sup_status=0; supavise status >"$WORK/status.txt" || sup_status=$?
[[ $sup_status -eq 0 ]] || { cat "$WORK/status.txt" >&2; fail "supavise status exited $sup_status after the rollback"; }

log "a second rollback steps back to v0.0.1, whose schema the registry has outgrown: refused"
run "$SV" rollback --yes
if [[ $RC -eq 2 ]]; then
  [[ $OUT == *"restore the system cluster from its pre-upgrade base backup"* ]] || fail "second rollback refusal: $OUT"
else
  # The previous release already carries every migration (a main that has this code): the
  # rollback goes through to v0.0.1, and the projects the upgrade to v0.0.2 moved go back too
  # (the revert of the first rollback is by now their latest upgrade; the window of v0.0.2's
  # upgrade finds the moves).
  [[ $RC -eq 0 && $($SV --version) == *v0.0.1* ]] || fail "second rollback: $RC $OUT"
  [[ $(versions_of "$REF") == "$OLD_AUTH $OLD_REST "* && $(versions_of "$REF2") == "$OLD_AUTH $OLD_REST "* ]] || fail "the second rollback left the projects on $(versions_of "$REF") / $(versions_of "$REF2")"
fi

# ---- 7. a release that renders the units differently ------------------------------------------
log "a release that renders PostgREST differently: the daemon holds the restarts back and the rollout does them"
if [[ $($SV --version) == *v0.0.1* ]]; then # the second rollback went all the way back
  run upgrade "$B/v2" --yes --version v0.0.2
  [[ $RC -eq 0 ]] || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "the upgrade back to v0.0.2 exited $RC: $OUT"; }
fi
EXPECT_VERSION=v0.0.2
wait_daemon
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
PG_PID=$(pg_pid "$REF") PG_PID2=$(pg_pid "$REF2")
declare -A REST_PID
for r in "$REF" "$REF2"; do
  REST_PID[$r]=$(systemctl show -p MainPID --value "supavise-postgrest@$r.service")
  grep -q 'PGRST_DB_POOL="5"' "$STATE/projects/$r/postgrest.env" || fail "$r: PostgREST is not on pool size 5 before the upgrade"
done
make_release v0.0.6 "$B/v6" "$BINS/v6.versions.yaml"
run upgrade "$B/v6" --yes --version v0.0.6
[[ $RC -eq 0 ]] || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "the upgrade to v0.0.6 exited $RC: $OUT"; }
EXPECT_VERSION=v0.0.6
for want in "restarting the projects whose service files" "restarted on their changed files" "Supavise v0.0.6 is running"; do
  [[ $OUT == *"$want"* ]] || fail "the upgrade's output lacks '$want':
$OUT"
done
wait_daemon; [[ $(daemon_version) == *v0.0.6* ]] || fail "the daemon runs $(daemon_version)"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
for r in "$REF" "$REF2"; do
  grep -q 'PGRST_DB_POOL="6"' "$STATE/projects/$r/postgrest.env" || fail "$r: the files were not rendered with pool size 6"
  now_pid=$(systemctl show -p MainPID --value "supavise-postgrest@$r.service")
  [[ $now_pid != "${REST_PID[$r]}" ]] || fail "$r: PostgREST was not restarted on its new files"
done
[[ $(pg_pid "$REF") == "$PG_PID" && $(pg_pid "$REF2") == "$PG_PID2" ]] || fail "a project's PostgreSQL was restarted by a release that moves no PostgreSQL"
journalctl --no-pager -u supavise.service | grep -q "restart waits for the upgrade's rollout" || fail "the daemon did not hold the restarts back for the rollout"
[[ $(marker) == "done v0.0.2 v0.0.6" ]] || fail "marker: $(marker)"
intact "$REF"; intact "$REF2"
sup_status=0; supavise status >"$WORK/status.txt" || sup_status=$?
[[ $sup_status -eq 0 ]] || { cat "$WORK/status.txt" >&2; fail "supavise status exited $sup_status after the upgrade to v0.0.6"; }

# ---- 8. a stop while the projects are upgraded ---------------------------------------------------
log "SIGTERM to the upgrade while it upgrades the projects: the worker settles, the upgrade rolls back, no project stays UPGRADING"
cp "$B/v5" /opt/supavise-e2e/v5-real
cat >"$WORK/stub9" <<'EOF'
#!/bin/sh
REAL=/opt/supavise-e2e/v5-real
[ "$1" = --version ] && { echo "supavise version v0.0.9"; exit 0; }
if [ "$1" = release-info ]; then "$REAL" release-info --json | sed 's/"version": "v0.0.5"/"version": "v0.0.9"/'; exit $?; fi
exec "$REAL" "$@"
EOF
chmod 755 "$WORK/stub9"
make_release v0.0.9 "$WORK/stub9" "$BINS/v5.versions.yaml"
"$B/v5" upgrade --repo o/r --api-base "http://127.0.0.1:$SRV_PORT" --public-key-file "$KEYS/pub.pem" --yes --version v0.0.9 >"$WORK/cancel.out" 2>&1 &
UP_PID=$!
for ((i = 0; i < 1200; i++)); do
  [[ $(marker 2>/dev/null || true) == "projects "* ]] && break
  kill -0 "$UP_PID" 2>/dev/null || break
  sleep 0.25
done
kill -TERM "$UP_PID" 2>/dev/null || true
RC=0; wait "$UP_PID" || RC=$?
OUT=$(cat "$WORK/cancel.out"); cp "$WORK/cancel.out" "$WORK/outputs/cancel.log"
if [[ $RC -eq 0 ]]; then
  # The rollout was already over when the signal arrived (it is quick on a small node): there is
  # nothing to cancel, and the upgrade to v0.0.9 stands. The checks below need the cancelled run.
  log "the upgrade finished before the signal reached it; the cancel checks are skipped"
else
  [[ $RC -eq 3 ]] || { journalctl --no-pager -u supavise.service | tail -40 >&2; fail "a stopped upgrade: exit $RC, want 3: $OUT"; }
  [[ $OUT == *"rolled back"* ]] || fail "a stopped upgrade did not roll back: $OUT"
  EXPECT_VERSION=v0.0.6
  unchanged "stopped upgrade"
  wait_daemon; [[ $(daemon_version) == *v0.0.6* ]] || fail "the daemon runs $(daemon_version)"
  [[ $(reg "select count(*) from supavise.projects where status = 'UPGRADING'") == 0 ]] || fail "a project is left UPGRADING: $(reg "select ref, status from supavise.projects")"
  [[ $(reg "select count(*) from supavise.project_upgrades where status = 0") == 0 ]] || fail "an upgrade row still says running: $(reg "select ref, status, progress from supavise.project_upgrades order by initiated_at")"
  wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
  [[ $(versions_of "$REF") == "$NEW_AUTH $NEW_REST "* && $(versions_of "$REF2") == "$NEW_AUTH $NEW_REST "* ]] || fail "versions after the stopped upgrade: $(versions_of "$REF") / $(versions_of "$REF2")"
  intact "$REF"; intact "$REF2"
fi

# ---- 9. a release that renders the projects' PostgreSQL units differently -------------------------
log "a release that changes the PostgreSQL settings of every project: the daemon leaves the clusters running, the rollout restarts them"
make_release v0.0.10 "$B/v7" "$BINS/v7.versions.yaml"
FROM_VERSION=$($SV --version | grep -o 'v[0-9][0-9.]*' | head -1)
PG_PID=$(pg_pid "$REF") PG_PID2=$(pg_pid "$REF2")
for r in "$REF" "$REF2"; do
  [[ $(pg_sock "$r" "show max_connections") == 60 ]] || fail "$r: max_connections is not 60 before the upgrade"
done
run upgrade "$B/v7" --yes --version v0.0.10
[[ $RC -eq 0 ]] || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "the upgrade to v0.0.10 exited $RC: $OUT"; }
EXPECT_VERSION=v0.0.10
for want in "restarted on their changed files" "Supavise v0.0.10 is running"; do
  [[ $OUT == *"$want"* ]] || fail "the upgrade's output lacks '$want':
$OUT"
done
wait_daemon; [[ $(daemon_version) == *v0.0.10* ]] || fail "the daemon runs $(daemon_version)"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
journalctl --no-pager -u supavise.service | grep -q "postgres settings changed; the restart waits for the upgrade's rollout" || fail "the daemon did not hold the restart of the clusters back for the rollout"
for r in "$REF" "$REF2"; do
  [[ $(pg_sock "$r" "show max_connections") == 61 ]] || fail "$r: PostgreSQL still runs its old settings"
done
[[ $(pg_pid "$REF") != "$PG_PID" && $(pg_pid "$REF2") != "$PG_PID2" ]] || fail "a project's PostgreSQL was not restarted by the rollout"
[[ $(marker) == "done $FROM_VERSION v0.0.10" ]] || fail "marker: $(marker), want done $FROM_VERSION v0.0.10"
intact "$REF"; intact "$REF2"
sup_status=0; supavise status >"$WORK/status.txt" || sup_status=$?
[[ $sup_status -eq 0 ]] || { cat "$WORK/status.txt" >&2; fail "supavise status exited $sup_status after the upgrade to v0.0.10"; }

# ---- 10. going back across a release that renders PostgreSQL differently -----------------------
log "supavise rollback after v0.0.10: the output names the restarts, the old daemon restarts the clusters the rollout had restarted"
PG_PID=$(pg_pid "$REF") PG_PID2=$(pg_pid "$REF2")
run "$SV" rollback --yes
[[ $RC -eq 0 ]] || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "the rollback from v0.0.10 exited $RC: $OUT"; }
EXPECT_VERSION=v0.0.6
for want in "one after another and outside the canary and batches" "drops the project's database connections" "Supavise v0.0.6 is running"; do
  [[ $OUT == *"$want"* ]] || fail "the rollback's output lacks '$want':
$OUT"
done
wait_daemon; [[ $(daemon_version) == *v0.0.6* ]] || fail "the daemon runs $(daemon_version)"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
for r in "$REF" "$REF2"; do
  for ((i = 0; i < 120; i++)); do [[ $(pg_sock "$r" "show max_connections" 2>/dev/null || true) == 60 ]] && break; sleep 1; done
  [[ $(pg_sock "$r" "show max_connections") == 60 ]] || fail "$r: PostgreSQL still runs the settings of v0.0.10 after the rollback"
done
[[ $(pg_pid "$REF") != "$PG_PID" && $(pg_pid "$REF2") != "$PG_PID2" ]] || fail "the old daemon did not restart a cluster that runs the newer settings"
journalctl --no-pager -u supavise.service | grep -q "postgres settings changed; restarting the running cluster" || fail "the old daemon did not restart the clusters"
intact "$REF"; intact "$REF2"
sup_status=0; supavise status >"$WORK/status.txt" || sup_status=$?
[[ $sup_status -eq 0 ]] || { cat "$WORK/status.txt" >&2; fail "supavise status exited $sup_status after the rollback from v0.0.10"; }

# ---- 11. automatic upgrades: the command of the maintenance window's timer ---------------------------------
log "auto mode: supavise-upgrade.service upgrades the node inside the window, and does nothing outside it"
[[ $($SV --version) == *v0.0.6* ]] || fail "the node should run v0.0.6 here: $($SV --version)"
install -m 0644 "$KEYS/pub.pem" /opt/supavise-e2e/pub.pem # the unit has a private /tmp
make_release v0.0.11 "$B/v8" "$BINS/v8.versions.yaml" latest

# The record sits in the unit's StateDirectory and nowhere else: a directory that the environment names
# and systemd did not make is refused, and nothing is created.
run env STATE_DIRECTORY=/var/lib/supavise-e2e-elsewhere "$SV" update run
[[ $RC -ne 0 && $OUT == *"does not exist"* && $OUT == *"StateDirectory="* && ! -e /var/lib/supavise-e2e-elsewhere ]] || fail "update run with a state directory that systemd did not make: $RC $OUT"
run env STATE_DIRECTORY=relative/dir "$SV" update run
[[ $RC -ne 0 && $OUT == *"clean absolute path"* ]] || fail "update run with a relative state directory: $RC $OUT"

# What the unattended upgrade needs: a healthy node, a copy of the master key in the backup backend and a
# backup of every running project newer than 24 hours.
printf 'upgrade-e2e passphrase: correct horse\n' >"$WORK/passphrase"; chown "$SUPAVISE_USER" "$WORK/passphrase"; chmod 0600 "$WORK/passphrase"
supavise system escrow-key --passphrase-file "$WORK/passphrase" >/dev/null || fail "escrow-key"
for r in "$REF" "$REF2"; do supavise backups create "$r" >/dev/null || fail "backups create $r"; done
supavise status --json >"$WORK/status.json" || { cat "$WORK/status.json" >&2; fail "supavise status is not healthy before the unattended upgrade"; }
[[ $(json_get '[c["state"] for c in d["components"] if c["name"] == "key escrow"][0]' <"$WORK/status.json") == ok ]] || fail "the key escrow is not ok: $(cat "$WORK/status.json")"

# A local alert receiver: the upgrade alerts must arrive.
cat >"$WORK/hook.py" <<'PY'
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with open(sys.argv[2], "ab") as f:
            f.write(body + b"\n")
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
HOOK_PORT=38802
python3 "$WORK/hook.py" "$HOOK_PORT" "$WORK/hook.jsonl" &
HOOK_PID=$!
for ((i = 0; i < 40; i++)); do ss -ltnH "sport = :$HOOK_PORT" | grep -q . && break; sleep 0.25; done
hook_events() { # "kind severity text" of the upgrade_* alerts received, in order
  python3 - "$WORK/hook.jsonl" <<'PY'
import json, os, sys
if os.path.exists(sys.argv[1]):
    for line in open(sys.argv[1]):
        if line.strip():
            d = json.loads(line)
            if d["kind"].startswith("upgrade_"):
                print(d["kind"] + " " + d["severity"] + " " + d["text"].replace("\n", " "))
PY
}
hook_kinds() { hook_events | awk '{print $1}' | paste -sd, -; }

# The window is somewhere else: auto mode, a window two hours from now. `update config` also writes the
# timer, which would wake the unit in the middle of this test; the unit is started by hand below.
OUTSIDE="daily $(date -d '+2 hours' +%H:%M)-$(date -d '+4 hours' +%H:%M)"
$SV update config --mode auto --window "$OUTSIDE" --os-security-updates=false --os-reboot never >/dev/null || fail "update config --mode auto"
[[ $(systemctl is-enabled supavise-upgrade.timer) == enabled ]] || fail "update config --mode auto did not enable the timer"
grep -q '^OnCalendar=' /etc/systemd/system/supavise-upgrade.timer || fail "the timer has no ticks: $(cat /etc/systemd/system/supavise-upgrade.timer)"
systemctl disable --now supavise-upgrade.timer 2>/dev/null || true
printf '\n[[alerts.webhooks]]\nurl = "http://127.0.0.1:%s/hook"\n' "$HOOK_PORT" >>/etc/supavise/config.toml
[[ $($SV update config | grep -E '^(mode|window)' | tr '\n' ' ') == "mode = \"auto\" window = \"$OUTSIDE\" " ]] || fail "update config: $($SV update config)"
# The real unit, with the release server of this test as its only change.
install -d /etc/systemd/system/supavise-upgrade.service.d
cat >/etc/systemd/system/supavise-upgrade.service.d/e2e.conf <<UNIT
[Service]
ExecStart=
ExecStart=$SV update run --repo o/r --api-base http://127.0.0.1:$SRV_PORT --public-key-file /opt/supavise-e2e/pub.pem
UNIT
systemctl daemon-reload
start_unit() { # runs supavise-upgrade.service to its end; UNIT_RC is the exit status of systemctl start
  UNIT_RC=0
  timeout 3000 systemctl start supavise-upgrade.service || UNIT_RC=$?
}
unit_journal() { journalctl --no-pager -u supavise-upgrade.service --since "$1"; }
journal_has() { # SINCE TEXT: the unit's journal since SINCE mentions TEXT (a file, not a pipe: grep -q would end a pipe early)
  unit_journal "$1" >"$WORK/journal.tmp" || true
  grep -q -- "$2" "$WORK/journal.tmp"
}

log "auto mode outside the window: the unit exits 0 and changes nothing"
HOOK_BEFORE=$(hook_kinds); T0=$(date '+%Y-%m-%d %H:%M:%S')
MARKER_BEFORE=$(marker)
start_unit
[[ $UNIT_RC -eq 0 ]] || { unit_journal "$T0" >&2; fail "supavise-upgrade.service outside the window exited $UNIT_RC"; }
[[ $($SV --version) == *v0.0.6* ]] || fail "the unit upgraded the node outside the window: $($SV --version)"
[[ $(hook_kinds) == "$HOOK_BEFORE" ]] || fail "the unit raised upgrade alerts outside the window: $(hook_events)"
if journal_has "$T0" unattended_upgrade_started; then fail "the unit started an upgrade outside the window"; fi
if [[ -e /var/lib/supavise-upgrade/state.json ]]; then
  [[ $(python3 -c 'import json; print(json.load(open("/var/lib/supavise-upgrade/state.json")).get("result"))') == None ]] || fail "the unit recorded a result outside the window"
fi
[[ $(marker) == "$MARKER_BEFORE" ]] || fail "the upgrade marker changed outside the window: $(marker), was $MARKER_BEFORE"

log "auto mode inside the window: a real supavise upgrade --unattended from v0.0.6 to v0.0.11"
INSIDE="daily $(date -d '-1 hour' +%H:%M)-$(date -d '+3 hours' +%H:%M)"
sed -i "s|^window = .*|window = \"$INSIDE\"|" /etc/supavise/config.toml
[[ $($SV update config | grep -E '^(mode|window)' | tr '\n' ' ') == "mode = \"auto\" window = \"$INSIDE\" " ]] || fail "the window was not set: $($SV update config)"
for r in "$REF" "$REF2"; do
  REST_PID[$r]=$(systemctl show -p MainPID --value "supavise-postgrest@$r.service")
done
PG_PID=$(pg_pid "$REF") PG_PID2=$(pg_pid "$REF2")
HOOK_BEFORE=$(hook_kinds); T1=$(date '+%Y-%m-%d %H:%M:%S')
start_unit
unit_journal "$T1" >"$WORK/outputs/unit-inside-window.log"
[[ $UNIT_RC -eq 0 ]] || { tail -80 "$WORK/outputs/unit-inside-window.log" >&2; journalctl --no-pager -u supavise.service | tail -40 >&2; fail "supavise-upgrade.service inside the window exited $UNIT_RC"; }
EXPECT_VERSION=v0.0.11
for want in "unattended_upgrade_started" "outcome=ok" "Supavise v0.0.11 is running" "upgrade_succeeded"; do
  grep -q -- "$want" "$WORK/outputs/unit-inside-window.log" || { cat "$WORK/outputs/unit-inside-window.log" >&2; fail "the unit's journal lacks '$want'"; }
done
[[ $($SV --version) == *v0.0.11* ]] || fail "the installed binary is $($SV --version)"
wait_daemon; [[ $(daemon_version) == *v0.0.11* ]] || fail "the daemon runs $(daemon_version)"
wait_status "$REF" ACTIVE_HEALTHY 180; wait_status "$REF2" ACTIVE_HEALTHY 180
[[ $(marker) == "done v0.0.6 v0.0.11" ]] || fail "marker after the unattended upgrade: $(marker) (before: $MARKER_BEFORE)"
# The record: in the unit's StateDirectory, root's, with the result and nothing left in progress.
UNIT_STATE=/var/lib/supavise-upgrade/state.json
[[ -f $UNIT_STATE && $(stat -c %U /var/lib/supavise-upgrade) == root && $(stat -c %U "$UNIT_STATE") == root ]] || fail "the record is not root's in /var/lib/supavise-upgrade: $(ls -ld /var/lib/supavise-upgrade; ls -l "$UNIT_STATE" 2>&1)"
[[ $(python3 -c 'import json; d=json.load(open("'"$UNIT_STATE"'")); print(d["result"]["exit"], d["result"]["version"], "in_progress" in d, "blocked" in d, "rolled_back" in d, "window" in d)') == "0 v0.0.6 False False False True" ]] || fail "the record: $(cat "$UNIT_STATE")"
[[ $(systemctl show -p Result --value supavise-upgrade.service) == success ]] || fail "the unit's result is $(systemctl show -p Result --value supavise-upgrade.service)"
[[ $(supavise update status --json | json_get 'd["last_unattended_upgrade"]["exit"]') == 0 ]] || fail "update status does not show the result"
# The upgrade really ran: the rollout restarted each project's PostgREST onto the files of v0.0.11, PostgreSQL untouched.
for r in "$REF" "$REF2"; do
  grep -q 'PGRST_DB_POOL="5"' "$STATE/projects/$r/postgrest.env" || fail "$r: the files were not rendered by v0.0.11"
  [[ $(systemctl show -p MainPID --value "supavise-postgrest@$r.service") != "${REST_PID[$r]}" ]] || fail "$r: PostgREST was not restarted by the rollout"
done
[[ $(pg_pid "$REF") == "$PG_PID" && $(pg_pid "$REF2") == "$PG_PID2" ]] || fail "a project's PostgreSQL was restarted"
intact "$REF"; intact "$REF2"
# The alerts: one started and one succeeded for this upgrade, in order, with the versions and who started it.
NEW_EVENTS=$(hook_events | tail -n 2)
[[ $(hook_kinds) == "${HOOK_BEFORE:+$HOOK_BEFORE,}upgrade_started,upgrade_succeeded" ]] || fail "upgrade alerts: $(hook_kinds) (before: $HOOK_BEFORE)"
grep -q '^upgrade_started info .*v0.0.6 -> v0.0.11 started by the maintenance window' <<<"$NEW_EVENTS" || fail "the started alert: $NEW_EVENTS"
grep -q '^upgrade_succeeded info .*v0.0.6 -> v0.0.11 finished by the maintenance window' <<<"$NEW_EVENTS" || fail "the succeeded alert: $NEW_EVENTS"
sup_status=0; supavise status >"$WORK/status.txt" || sup_status=$?
[[ $sup_status -eq 0 ]] || { cat "$WORK/status.txt" >&2; fail "supavise status exited $sup_status after the unattended upgrade"; }

log "a second start in the same window does nothing: the window had its attempt"
HOOK_BEFORE=$(hook_kinds); T2=$(date '+%Y-%m-%d %H:%M:%S')
start_unit
[[ $UNIT_RC -eq 0 ]] || { unit_journal "$T2" >&2; fail "the second start exited $UNIT_RC"; }
if journal_has "$T2" unattended_upgrade_started; then fail "the unit upgraded twice in one window"; fi
[[ $(hook_kinds) == "$HOOK_BEFORE" && $($SV --version) == *v0.0.11* ]] || fail "the second start changed something: $(hook_kinds)"

log "upgrade end to end: all checks passed"
