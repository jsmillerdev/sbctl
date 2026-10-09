#!/usr/bin/env bash
# Helpers that the two-server test (replication.sh) runs inside a node, as root. Sourced, not executed:
# replication.sh pushes this file, lib.sh, writer.py and multi-env.sh (the cluster's domain) to /root of
# each node (multi_push_tests) and calls a function with `onl NODE FUNCTION ARGS...`.
#
# A function that fails calls `fail`, which prints "FAIL: ..." and ends the shell with status 1; the runner
# takes the last such line as the reason of the check. What a function prints on standard output is its
# result; progress goes to standard error through `log`.
#
# Secrets stay in 0600 files under /root: the keys of a project and its database password (/root/keys), the
# personal access token (/root/pat), a dashboard session (/root/jwt.hdr). A request that needs one reads it
# from a header file (curl -H @file) or from standard input, so none is ever an argument of a command.
# shellcheck source=/dev/null
[[ ! -f /root/multi-env.sh ]] || source /root/multi-env.sh
# shellcheck source=../lib.sh
source /root/lib.sh

SV=/usr/local/bin/supavise
SYSTEM_UNITS=(supavise.service supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service
  supavise-supavisor.service supavise-realtime.service supavise-storage.service)
RUN_SPAN=10000000                  # a writer run owns the ids from run * RUN_SPAN up
STORAGE_BUCKET=docs

# ---- SQL on the clusters of this node ------------------------------------------------------------------
psql_bin() { ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql 2>/dev/null | head -n1; }

# pg_local REF SQL [DB]: runs SQL as supabase_admin on the cluster of REF that runs on this node, over its
# private socket: the primary on the canonical port, or the standby on the replica port. Prints the rows
# unaligned. Status 1 when no cluster of REF answers here.
pg_local() {
  local ref=$1 sql=$2 db=${3:-postgres} d f port psql out err
  d=$SUPAVISE_STATE/projects/$ref/postgres/sock
  psql=$(psql_bin)
  [[ -n $psql ]] || { echo "no psql in the artifacts" >&2; return 1; }
  err=$(mktemp)
  for f in $(find "$d" -maxdepth 1 -name '.s.PGSQL.[0-9]*' ! -name '*.lock' 2>/dev/null | sort); do
    port=${f##*.}
    if out=$(sudo -u "$SUPAVISE_USER" "$psql" "host=$d port=$port user=supabase_admin dbname=$db connect_timeout=5" \
        -v ON_ERROR_STOP=1 -Atq -c "$sql" </dev/null 2>"$err"); then
      rm -f "$err"
      printf '%s\n' "$out"
      return 0
    fi
    grep -q -e 'could not connect' -e 'Connection refused' -e 'No such file' "$err" || { cat "$err" >&2; rm -f "$err"; return 1; }
  done
  rm -f "$err"
  return 1
}

# reg SQL: SQL on the registry (the supavise database of the system cluster). On a follower that is the
# hot standby, which is a few moments behind the leader.
reg() { pg_local system "$1" supavise; }

home_of() { reg "select node_id from supavise.projects where ref = '$1'"; }
project_status() { reg "select status from supavise.projects where ref = '$1'"; }
last_move() { reg "select id, scope, kind, coalesce(ref, ''), from_node, to_node, epoch, state, left(error, 300) from supavise.moves order by id desc limit 1"; }
pg_start_time() { pg_local "$1" "select pg_postmaster_start_time()"; }

# node ls --json, whatever the exit status of the command: this node's copy of the registry.
nodes_json() { supavise node ls --json 2>/dev/null; }
node_leader() { nodes_json | json_get 'd["leader"]'; }
node_epoch() { nodes_json | json_get 'd["epoch"]'; }
node_state() { # NODE: the state of a node in this node's copy of the registry
  nodes_json | json_get '[n["state"] for n in d["nodes"] if n["id"] == "'"$1"'"][0]'
}
node_version() { nodes_json | json_get '[n["version"] for n in d["nodes"] if n["id"] == "'"$1"'"][0]'; }
node_has_version() { [[ $(node_version "$1") == "$2" ]]; } # NODE VERSION

# ---- waiting -----------------------------------------------------------------------------------------
# wait_for SECONDS WHAT CMD...: polls CMD every two seconds until it succeeds. CMD runs in a subshell, so a
# function of this file that calls `fail` ends only that attempt.
wait_for() {
  local n=$1 what=$2 i
  shift 2
  for ((i = 0; i < n; i += 2)); do
    ( "$@" ) >/dev/null 2>&1 && return 0
    sleep 2
  done
  fail "timed out after ${n}s waiting for $what"
}

# wait_node_versions VERSION SECONDS NODE...: this node's copy of the registry shows every NODE at VERSION.
wait_node_versions() {
  local want=$1 n=$2 id
  shift 2
  for id in "$@"; do wait_for "$n" "the registry to show $id at $want" node_has_version "$id" "$want"; done
}

wait_api() { # SECONDS: the Management API answers (401 without a token) on this node
  local n=${1:-120} i
  for ((i = 0; i < n; i++)); do
    [[ $(http_code -H "Host: $API_HOST" http://127.0.0.1/v1/projects) == 401 ]] && return 0
    sleep 1
  done
  journalctl --no-pager -u supavise.service -n 40 >&2
  fail "the Management API does not answer on this node after ${n}s"
}

# wait_move_ended SECONDS: the newest move in the registry is no longer running. A server move goes on in the daemon of
# the node that takes over after the old leader's daemon restarted, and the command that started it may have ended.
wait_move_ended() {
  local n=${1:-600} i st=""
  for ((i = 0; i < n; i += 3)); do
    st=$(last_move 2>/dev/null | cut -d'|' -f8) || st=""
    [[ -n $st && $st != running ]] && return 0
    sleep 3
  done
  last_move >&2 || true
  fail "the newest move is '${st:-unknown}' after ${n}s"
}

# wait_project REF STATUS SECONDS: the registry shows the project in STATUS.
wait_project() {
  local ref=$1 want=$2 n=${3:-300} i s=""
  for ((i = 0; i < n; i += 2)); do
    s=$(project_status "$ref" 2>/dev/null) || s=""
    [[ $s == "$want" ]] && return 0
    sleep 2
  done
  fail "$ref is '$s' after ${n}s, want $want"
}

# wait_replicas REF COUNT SECONDS: the project has exactly COUNT replicas and each is ACTIVE_HEALTHY. A
# replica whose setup failed ends the wait at once.
wait_replicas() {
  local ref=$1 want=$2 n=${3:-900} i row="" healthy total failed
  for ((i = 0; i < n; i += 3)); do
    row=$(reg "select count(*) filter (where status = 'ACTIVE_HEALTHY'), count(*), count(*) filter (where status = 'INIT_READ_REPLICA_FAILED') from supavise.replicas where ref = '$ref'" 2>/dev/null) || row=""
    IFS='|' read -r healthy total failed <<<"$row"
    if [[ ${failed:-0} -gt 0 ]]; then
      reg "select identifier, node_id, status, init_step, init_error from supavise.replicas where ref = '$ref'" >&2 || true
      fail "$ref: a replica failed its setup"
    fi
    [[ ${total:-} == "$want" && ${healthy:-} == "$want" ]] && return 0
    sleep 3
  done
  reg "select identifier, node_id, status, init_step, init_error from supavise.replicas where ref = '$ref'" >&2 || true
  fail "$ref: $want replica(s) ACTIVE_HEALTHY wanted after ${n}s, the registry has '${row:-nothing}' (healthy|total|failed)"
}

# ---- the Management API --------------------------------------------------------------------------------
# papi METHOD PATH [curl args]: a /v1 call with the personal access token, which is read from a header file.
papi() { local m=$1 p=$2; shift 2; api "$m" "$p" -H @/root/pat.hdr "$@"; }

dash_login() { # a dashboard session for the /platform routes (a personal access token does not open them)
  local jwt
  (umask 077; printf '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' >/root/signin-body.json)
  jwt=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' -d @/root/signin-body.json \
    | json_get 'd["access_token"]') || { rm -f /root/signin-body.json; fail "dashboard sign-in"; }
  rm -f /root/signin-body.json
  (umask 077; printf 'Authorization: Bearer %s\n' "$jwt" >/root/jwt.hdr)
}
japi() { local m=$1 p=$2; shift 2; api "$m" "$p" -H @/root/jwt.hdr "$@"; }

# claim_and_save: claims the node with the install-style token (an admin and an organization), signs in and
# creates a personal access token; the token and the organization stay in 0600 files under /root (the test
# copies them to the other node). The calls are those of lib.sh's claim_and_token, with the token, the password
# and the session in files instead of arguments.
claim_and_save() {
  local tok pat org
  tok=$(supavise claim token 2>/dev/null) || fail "supavise claim token"
  (umask 077; printf '{"token":"%s","email":"smoke@example.com","password":"smoke-correct-horse-battery","organization_name":"Smoke"}' "$tok" >/root/claim-body.json)
  [[ $(api POST /claim -H 'Content-Type: application/json' -d @/root/claim-body.json -o /dev/null -w '%{http_code}') == 201 ]] || fail "claim with the token failed"
  rm -f /root/claim-body.json
  dash_login
  pat=$(japi POST /platform/profile/access-tokens -H 'Content-Type: application/json' -d '{"name":"smoke"}' | json_get 'd["token"]') \
    || fail "creating a personal access token"
  [[ $pat == sbp_* ]] || fail "the personal access token has an unexpected shape"
  (umask 077; printf '%s' "$pat" >/root/pat; printf 'Authorization: Bearer %s\n' "$pat" >/root/pat.hdr)
  org=$(papi GET /v1/organizations | json_get 'd[0]["slug"]') || fail "listing organizations with the token"
  printf '%s' "$org" >/root/org
}

# save_keys REF: the keys of a project and its database password, in 0600 files under /root/keys. The .hdr
# files are header files for curl (-H @file).
save_keys() {
  local ref=$1
  (umask 077; mkdir -p /root/keys
   printf '%s' "$PUB" >"/root/keys/$ref.pub"; printf '%s' "$SEC" >"/root/keys/$ref.sec"; printf '%s' "$DBPASS" >"/root/keys/$ref.dbpass"
   printf 'apikey: %s\n' "$PUB" >"/root/keys/$ref.pub.hdr"
   printf 'apikey: %s\n' "$SEC" >"/root/keys/$ref.sec.hdr"
   printf 'Authorization: Bearer %s\n' "$SEC" >"/root/keys/$ref.auth.hdr")
}

# create_api_project NAME SIZE: through POST /v1/projects with the password in DBPASS (which gen_dbpass sets);
# waits until the project is ACTIVE_HEALTHY, keeps its keys and password (save_keys) and prints the ref.
create_api_project() {
  local name=$1 size=$2 ref status="" i org
  org=$(cat /root/org)
  (umask 077; printf '{"name":"%s","organization_slug":"%s","db_pass":"%s","region":"us-east-1","desired_instance_size":"%s"}' \
    "$name" "$org" "$DBPASS" "$size" >/root/create-body.json)
  ref=$(papi POST /v1/projects -H 'Content-Type: application/json' -d @/root/create-body.json | json_get 'd["ref"]') \
    || fail "creating project $name through the API"
  rm -f /root/create-body.json
  [[ $ref =~ ^[a-z]{20}$ ]] || fail "project ref: $ref"
  for ((i = 0; i < 180; i++)); do
    status=$(papi GET "/v1/projects/$ref" | json_get 'd["status"]' 2>/dev/null || true)
    [[ $status == ACTIVE_HEALTHY || $status == INIT_FAILED ]] && break
    sleep 3
  done
  [[ $status == ACTIVE_HEALTHY ]] || { journalctl --no-pager -u supavise.service -n 60 >&2; fail "project $ref is $status"; }
  project_keys "$ref"
  save_keys "$ref"
  echo "$ref"
}

# ---- data of the projects ------------------------------------------------------------------------------
# seed_project REF LABEL: items (100 rows) and writes (empty, for the writer), readable by anon, writable by
# service_role.
seed_project() {
  pg_local "$1" "
    create table public.items (id int primary key, label text);
    insert into public.items select g, '$2-' || g from generate_series(1, 100) g;
    grant select on public.items to anon, authenticated, service_role;
    create table public.writes (id bigint primary key, note text, at timestamptz not null default now());
    grant select on public.writes to anon, authenticated;
    grant all on public.writes to service_role;
    notify pgrst, 'reload schema'" >/dev/null || fail "$1: seeding the tables"
}

items_rest() { # REF: rows of public.items as the Data API of REF shows them through this node's proxy
  local ref=$1
  curl -sS -m 20 -H "Host: $ref.api.$SUPAVISE_DOMAIN" -H "@/root/keys/$ref.pub.hdr" "http://127.0.0.1/rest/v1/items?select=id" 2>/dev/null \
    | json_get 'len(d) if isinstance(d, list) else -1' 2>/dev/null || echo -1
}
wait_items_rest() { # REF COUNT SECONDS
  local ref=$1 want=$2 n=${3:-120} i got=""
  for ((i = 0; i < n; i += 2)); do
    got=$(items_rest "$ref")
    [[ $got == "$want" ]] && return 0
    sleep 2
  done
  fail "$ref: the Data API through this node shows $got items, want $want"
}

storage_curl() { # REF curl-args...: a Storage API call for REF through this node's proxy, with the secret key
  local ref=$1
  shift
  curl -sS -m 120 -H "Host: $ref.api.$SUPAVISE_DOMAIN" -H "@/root/keys/$ref.sec.hdr" -H "@/root/keys/$ref.auth.hdr" "$@"
}
storage_seed() { # REF: a bucket with one 2 MiB object; prints its SHA-256
  local ref=$1 f=/root/object-$1.bin
  head -c 2097152 /dev/urandom >"$f"
  [[ $(storage_curl "$ref" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "{\"name\":\"$STORAGE_BUCKET\"}" \
    http://127.0.0.1/storage/v1/bucket) == 200 ]] || fail "$ref: creating the Storage bucket"
  [[ $(storage_curl "$ref" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/octet-stream' -H 'Cache-Control: max-age=3600' \
    --data-binary @"$f" "http://127.0.0.1/storage/v1/object/$STORAGE_BUCKET/blob.bin") == 200 ]] || fail "$ref: uploading the Storage object"
  sha256sum "$f" | cut -d' ' -f1
}
storage_sha() { storage_curl "$1" "http://127.0.0.1/storage/v1/object/$STORAGE_BUCKET/blob.bin" | sha256sum | cut -d' ' -f1; }
wait_storage() { # REF SHA SECONDS: the object downloads through this node with the checksum it was stored with
  local i got=""
  for ((i = 0; i < ${3:-120}; i += 2)); do
    got=$(storage_sha "$1" 2>/dev/null) || got=""
    [[ $got == "$2" ]] && return 0
    sleep 2
  done
  fail "$1: the Storage object is '$got' through this node after ${3:-120}s, want $2"
}

# data_intact REF SHA: the project's data as seeded, through this node's proxy: the Data API and Storage.
data_intact() {
  wait_items_rest "$1" 100 120
  wait_storage "$1" "$2" 120
}

# make_projects: two small projects (replicas start at Small) with data, the first with a Storage object.
# Prints ref1=, ref2= and sha1= (the object's checksum).
make_projects() {
  local r1 r2 sha1
  gen_dbpass; r1=$(create_api_project replica-one small)
  seed_project "$r1" one
  sha1=$(storage_seed "$r1")
  gen_dbpass; r2=$(create_api_project replica-two small)
  seed_project "$r2" two
  echo "ref1=$r1"; echo "ref2=$r2"; echo "sha1=$sha1"
}

# storage_to_s3: moves Storage's objects to the bucket (supavise storage migrate --to s3). The bucket, the
# endpoint and the addressing are in config.toml from the install; the key is read from a 0600 file.
storage_to_s3() {
  install -m 0600 -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" /root/s3.cred /etc/supavise/storage-credentials
  supavise storage migrate --to s3 --bucket "$S3_OBJECTS_BUCKET" --credentials-file /etc/supavise/storage-credentials --rate-limit 0 \
    || fail "supavise storage migrate --to s3"
  supavise storage migrate --to s3 --status
  grep -Eq "^storage_backend = ['\"]s3['\"]" /etc/supavise/config.toml || fail "config.toml does not name the s3 Storage backend: $(grep storage_ /etc/supavise/config.toml | redact)"
}

# ---- the cluster ---------------------------------------------------------------------------------------
# make_join_token: a one-time token for a server to join (supavise node token) in /root/join-token, 0600. The
# first token gives this server its cluster identity and the daemon restarts once to listen on the peer port.
make_join_token() {
  (umask 077; supavise node token --ttl 30m >/root/join-token) || fail "supavise node token"
  [[ -s /root/join-token ]] || fail "supavise node token printed no token"
  # wait_for ends its shell when it gives up, hence the subshell: the journal is printed first.
  ( wait_for 180 "the daemon to listen on the peer port 7443" bash -c 'ss -ltnH "sport = :7443" | grep -q .' ) \
    || { journalctl --no-pager -u supavise.service -n 60 | cut -c1-300 >&2; fail "the daemon does not listen on the peer port 7443 after a join token"; }
  wait_active supavise.service 60
  wait_api 120
}

# assert_nodes LEADER: this node's registry names LEADER the leader and both nodes active.
assert_nodes() {
  local j
  j=$(nodes_json) || fail "supavise node ls on this node"
  [[ $(json_get 'd["leader"]' <<<"$j") == "$1" ]] || fail "the leader is $(json_get 'd["leader"]' <<<"$j") on this node, want $1"
  [[ $(json_get 'sorted(n["state"] for n in d["nodes"])' <<<"$j") == "['active', 'active']" ]] \
    || fail "the nodes are not both active: $(json_get '[(n["id"], n["state"]) for n in d["nodes"]]' <<<"$j")"
}
wait_nodes() { # LEADER SECONDS: assert_nodes holds
  wait_for "${2:-180}" "$1 to be the leader of two active nodes in this node's registry" assert_nodes "$1"
}

# wait_healthy SECONDS: `supavise status` exits 0 (healthy); the report is printed when it does not.
wait_healthy() {
  local n=${1:-300} i rc=0
  for ((i = 0; i < n; i += 5)); do
    rc=0
    supavise status >/root/status.txt 2>&1 || rc=$?
    [[ $rc -eq 0 ]] && return 0
    sleep 5
  done
  cat /root/status.txt >&2
  fail "supavise status exits $rc after ${n}s (0 is healthy)"
}

# wait_peers SECONDS: the daemon's view shows a session with every other active node.
wait_peers() {
  local n=${1:-180} i out="?"
  for ((i = 0; i < n; i += 3)); do
    out=$({ supavise status --json 2>/dev/null || true; } \
      | json_get '[x["id"] for x in d["cluster"]["nodes"] if not x.get("self") and x["state"] == "active" and x.get("connected") is not True]' 2>/dev/null) || out="?"
    [[ $out == "[]" ]] && return 0
    sleep 3
  done
  fail "no mesh session with $out after ${n}s"
}

# leader_services: the shared services run here (a follower parks all but the pooler).
leader_services() {
  local u
  for u in "${SYSTEM_UNITS[@]}"; do wait_active "$u" 120; done
}

# follower_services: the pooler runs on the replicated tenants; the other shared services are parked (a node that
# has just been demoted takes a moment to stop them).
follower_services() {
  local u
  for u in supavise.service supavise-postgres@system.service supavise-supavisor.service; do wait_active "$u" 120; done
  wait_for 120 "the shared services other than the pooler to be parked" parked
}
parked() {
  local u
  for u in supavise-gotrue@system.service supavise-realtime.service supavise-storage.service supavise-pgmeta.service; do
    [[ $(unit_state "$u") != active ]] || { echo "$u runs on a follower" >&2; return 1; }
  done
}

# unit_stamp: the invocation of every PostgreSQL unit on this node and the start time of its postmaster; it
# changes when a cluster restarts.
unit_stamp() {
  local u ref
  for u in $(systemctl list-units 'supavise-postgres@*.service' --all --no-legend --plain | awk '{print $1}' | sort); do
    ref=${u#supavise-postgres@}; ref=${ref%.service}
    printf '%s %s %s\n' "$u" "$(systemctl show -p InvocationID --value "$u")" "$(pg_start_time "$ref" 2>/dev/null || echo none)"
  done
}

daemon_version() { "/proc/$(systemctl show -p MainPID --value supavise.service)/exe" --version; }

# fenced_refs: the PostgreSQL clusters of this node. They are the ones in the data directory: after a restart of the
# machine `systemctl list-units` shows only the units that are loaded, and a unit that never started is not, so the
# list alone could be empty and check nothing.
fenced_refs() {
  {
    find /var/lib/supavise/projects -mindepth 2 -maxdepth 2 -name postgres -type d | awk -F/ '{print $(NF-1)}'
    systemctl list-units 'supavise-postgres@*.service' --all --no-legend --plain | awk '{print $1}' | sed -e 's/^supavise-postgres@//' -e 's/\.service$//'
  } | sort -u
}

# fenced_settled: the system cluster and at least two projects are here, and none runs or has a launcher. It prints the
# first thing that is not so on standard error and returns 1.
fenced_settled() {
  local ref refs
  refs=$(fenced_refs)
  [[ $(grep -cx system <<<"$refs") == 1 && $(grep -vcx system <<<"$refs") -ge 2 ]] \
    || { echo "the fenced node has the PostgreSQL clusters '$(paste -sd' ' - <<<"$refs")', want system and the two projects" >&2; return 1; }
  for ref in $refs; do
    [[ $(unit_state "supavise-postgres@$ref.service") != active ]] || { echo "supavise-postgres@$ref.service runs on a fenced node" >&2; return 1; }
    [[ ! -e /var/lib/supavise/projects/$ref/postgres.run ]] || { echo "$ref: the fenced node still has its launcher /var/lib/supavise/projects/$ref/postgres.run" >&2; return 1; }
  done
}

# wait_fenced SECONDS: a node that came back after its replacement records that it is fenced, runs no
# PostgreSQL primary (no unit, and no launcher for the unit's ConditionPathExists) and answers 503.
# BootCheck writes the record first and then takes each launcher away and stops each cluster in turn, and systemd may
# have started the three clusters a moment before: on a loaded runner they are in crash recovery for a while, so the
# state is waited for, not read once.
wait_fenced() {
  local n=${1:-240} i why="" u ref
  wait_for "$n" "the node to record that it is fenced" test -s /var/lib/supavise/fenced.json
  for ((i = 0; i < 120; i += 3)); do
    why=$(fenced_settled 2>&1) && break
    sleep 3
  done
  [[ -z $why ]] || fail "after 120s: $why"
  for ref in $(fenced_refs); do
    u=supavise-postgres@$ref.service
    systemctl start "$u" 2>/dev/null || true
    [[ $(unit_state "$u") != active ]] || fail "$u started on a fenced node although its launcher is gone"
    [[ $(systemctl show -p ConditionResult --value "$u") == no ]] || fail "$u: the unit's condition is '$(systemctl show -p ConditionResult --value "$u")', want no"
  done
  [[ $(http_code http://127.0.0.1/) == 503 ]] || fail "a fenced node answers $(http_code http://127.0.0.1/) on port 80, want 503"
  log "fenced.json: leader and epoch $(python3 -c 'import json; d=json.load(open("/var/lib/supavise/fenced.json")); print(d.get("leader", ""), d.get("epoch", ""))')"
}

# wait_unfenced SECONDS: the record is gone and the daemon runs as a follower.
wait_unfenced() {
  wait_for "${1:-300}" "fenced.json to go" test ! -e /var/lib/supavise/fenced.json
  wait_active supavise.service 120
  wait_active supavise-postgres@system.service 120
}

# ---- replicas through the Management API ---------------------------------------------------------------------------
# replica_request REF REGION: POST /v1/projects/REF/read-replicas/setup.
replica_request() {
  local code
  code=$(papi POST "/v1/projects/$1/read-replicas/setup" -H 'Content-Type: application/json' -d "{\"read_replica_region\":\"$2\"}" -o /root/last.json -w '%{http_code}')
  [[ $code == 2* ]] || { cat /root/last.json >&2; fail "$1: read-replicas/setup answered $code"; }
  echo "$code"
}

# wait_replicas_api REF COUNT SECONDS: databases-statuses lists the primary and COUNT replicas, every one
# ACTIVE_HEALTHY; each change of the steps is logged. A failed setup ends the wait.
wait_replicas_api() {
  local ref=$1 want=$2 n=${3:-1500} i json cur last=""
  dash_login
  for ((i = 0; i < n; i += 5)); do
    json=$(japi GET "/platform/projects/$ref/databases-statuses") || json="[]"
    cur=$(python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("?")
    sys.exit(0)
out = []
for x in d:
    s = x.get("replicaInitializationStatus") or {}
    step = "(%s%s)" % (s.get("status", ""), " " + s["progress"] if s.get("progress") else "") if s else ""
    out.append("%s=%s%s" % (x["identifier"][-14:], x["status"], step))
print(" ".join(out))
' <<<"$json")
    [[ $cur == "$last" ]] || { log "$ref: $cur"; last=$cur; }
    [[ $cur != *FAILED* ]] || fail "$ref: a replica failed its setup: $cur"
    if [[ $(json_get 'len(d) == '"$((want + 1))"' and all(x["status"] == "ACTIVE_HEALTHY" for x in d)' <<<"$json" 2>/dev/null) == True ]]; then
      return 0
    fi
    sleep 5
  done
  fail "$ref: databases-statuses shows '$last' after ${n}s, want the primary and $want replica(s) ACTIVE_HEALTHY"
}

# check_database_shapes REF COUNT: GET databases and databases-statuses have the shapes Studio reads.
check_database_shapes() {
  local ref=$1 want=$2
  dash_login
  japi GET "/platform/projects/$ref/databases" >/root/databases.json || fail "GET databases"
  japi GET "/platform/projects/$ref/databases-statuses" >/root/databases-statuses.json || fail "GET databases-statuses"
  python3 - "$ref" "$want" /root/databases.json /root/databases-statuses.json <<'PY' || exit 1
import json, re, sys
ref, want = sys.argv[1], int(sys.argv[2])
dbs = json.load(open(sys.argv[3]))
sts = json.load(open(sys.argv[4]))
def need(ok, what):
    if not ok:
        print("FAIL: " + what, file=sys.stderr)
        sys.exit(1)
need(isinstance(dbs, list) and len(dbs) == want + 1, "databases lists %s rows, want %d" % (len(dbs) if isinstance(dbs, list) else dbs, want + 1))
need(isinstance(sts, list) and len(sts) == len(dbs), "databases-statuses lists %s rows, databases %d (Studio asks again until they match)" % (len(sts) if isinstance(sts, list) else sts, len(dbs)))
keys = ["identifier", "inserted_at", "status", "size", "region", "cloud_provider", "db_port", "db_name", "db_user", "restUrl", "db_host",
        "connectionString", "connection_string_read_only"]
for i, row in enumerate(dbs):
    for k in keys:
        need(k in row and row[k] not in (None, ""), "databases[%d] lacks %s: %s" % (i, k, row))
    need(row["status"] == "ACTIVE_HEALTHY", "databases[%d] is %s" % (i, row["status"]))
    need("[YOUR-PASSWORD]" in row["connectionString"] and "[YOUR-PASSWORD]" in row["connection_string_read_only"], "databases[%d]: a connection string with no placeholder" % i)
    need(row["identifier"] in row["restUrl"] and row["identifier"] in row["db_host"], "databases[%d]: restUrl and db_host name another database: %s" % (i, row))
need(dbs[0]["identifier"] == ref, "the primary is not the first row and does not carry the ref: %s" % dbs[0]["identifier"])
for row in dbs[1:]:
    need(re.fullmatch(re.escape(ref) + r"-rr-[a-z0-9-]+-[a-z0-9]{6}", row["identifier"]), "replica identifier %s" % row["identifier"])
need([r["identifier"] for r in sts] == [r["identifier"] for r in dbs], "databases-statuses and databases name different databases in different orders")
for r in sts:
    need(r["status"] == "ACTIVE_HEALTHY", "databases-statuses: %s is %s" % (r["identifier"], r["status"]))
for r in sts[1:]:
    need(r.get("replicaInitializationStatus", {}).get("status") == "completed", "databases-statuses: %s has replicaInitializationStatus %s, want completed" % (r["identifier"], r.get("replicaInitializationStatus")))
print("# databases: %s" % ", ".join("%s %s" % (r["identifier"][-14:], r["status"]) for r in dbs))
PY
}

replica_ids() { reg "select identifier from supavise.replicas where ref = '$1' order by created_at"; }

# replica_listed IDENTIFIER NODE: `supavise replicas ls --json` lists the replica on NODE as ACTIVE_HEALTHY. The
# command opens the registry for writing, so it belongs to the leader: on a follower the registry is a standby.
replica_listed() {
  local got
  got=$(supavise replicas ls --json | json_get '[(r["node"], r["status"]) for r in d if r["identifier"] == "'"$1"'"]') \
    || fail "supavise replicas ls --json"
  [[ $got == "[('$2', 'ACTIVE_HEALTHY')]" ]] || fail "supavise replicas ls lists $1 as $got, want [('$2', 'ACTIVE_HEALTHY')]"
}

replica_get() { # IDENTIFIER REF PATH: a Data API read of the replica through this node's proxy, with REF's publishable key
  curl -sS -m 20 -H "Host: $1.api.$SUPAVISE_DOMAIN" -H "@/root/keys/$2.pub.hdr" "http://127.0.0.1$3"
}

# primary_write REF ID: one row through the primary's Data API (this node's proxy), with the secret key.
primary_write() {
  local code
  code=$(curl -sS -m 20 -o /root/last.json -w '%{http_code}' -X POST -H "Host: $1.api.$SUPAVISE_DOMAIN" -H "@/root/keys/$1.sec.hdr" \
    -H 'Content-Type: application/json' -H 'Prefer: return=minimal' -d "{\"id\": $2, \"note\": \"probe\"}" http://127.0.0.1/rest/v1/writes)
  [[ $code == 201 ]] || { cat /root/last.json >&2; fail "$1: a write through the primary's Data API answered $code"; }
}

# replica_sees IDENTIFIER REF ID SECONDS: the row appears on the replica's endpoint; prints how long it took.
replica_sees() {
  local ident=$1 ref=$2 id=$3 n=${4:-30} t0 got
  t0=$(date +%s.%N)
  while (($(date +%s) - ${t0%.*} <= n)); do
    got=$(replica_get "$ident" "$ref" "/rest/v1/writes?id=eq.$id&select=id" | json_get 'len(d) if isinstance(d, list) else -1' 2>/dev/null || echo -1)
    if [[ $got == 1 ]]; then
      awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN {printf "%.1f\n", b - a}'
      return 0
    fi
    sleep 0.5
  done
  fail "$ref: the row $id is not on the replica $ident after ${n}s"
}

# replica_refuses_write IDENTIFIER REF: a write through the replica's endpoint is not accepted; prints the status.
replica_refuses_write() {
  local code
  code=$(curl -sS -m 20 -o /root/last.json -w '%{http_code}' -X POST -H "Host: $1.api.$SUPAVISE_DOMAIN" -H "@/root/keys/$2.sec.hdr" \
    -H 'Content-Type: application/json' -d '{"id": 777, "note": "refused"}' http://127.0.0.1/rest/v1/writes)
  [[ ${code:0:1} != 2 ]] || fail "$2: the replica $1 accepted a write ($code)"
  echo "$code"
}

# ddl_probe REF: a new table on the primary, readable by anon, and the NOTIFY PostgREST listens for.
ddl_probe() {
  pg_local "$1" "create table public.ddl_probe (id int primary key); insert into public.ddl_probe values (1);
    grant select on public.ddl_probe to anon; notify pgrst, 'reload schema'" >/dev/null || fail "$1: creating the probe table"
}
# replica_sees_table IDENTIFIER REF TABLE SECONDS: the table is served by the replica's endpoint; prints how long it took.
replica_sees_table() {
  local ident=$1 ref=$2 table=$3 n=${4:-60} t0 got
  t0=$(date +%s.%N)
  while (($(date +%s) - ${t0%.*} <= n)); do
    got=$(replica_get "$ident" "$ref" "/rest/v1/$table?select=id" | json_get 'len(d) if isinstance(d, list) else -1' 2>/dev/null || echo -1)
    if [[ $got == 1 ]]; then
      awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN {printf "%.1f\n", b - a}'
      return 0
    fi
    sleep 1
  done
  fail "$ref: the new table $table is not served by the replica $ident after ${n}s"
}

# pooler_is_recovery USER REF: through this node's Supavisor (session port) as USER with the project's
# password; prints t on a standby and f on a primary.
pooler_is_recovery() {
  PGPASSWORD=$(cat "/root/keys/$2.dbpass") "$(psql_bin)" "host=127.0.0.1 port=5432 user=$1 dbname=postgres sslmode=prefer connect_timeout=10" \
    -Atq -c 'select pg_is_in_recovery()' </dev/null
}

# lb_route REF: the X-Supavise-Route header of the project's load balancer host on this node.
lb_route() {
  curl -sS -m 20 -D - -o /dev/null -H "Host: $1-lb.api.$SUPAVISE_DOMAIN" -H "@/root/keys/$1.pub.hdr" "http://127.0.0.1/rest/v1/items?select=id&limit=1" \
    | tr -d '\r' | awk -F': ' 'tolower($1) == "x-supavise-route" {print $2}'
}

# ---- the writer ----------------------------------------------------------------------------------------
# writer_start REF RUN RATE: a continuous writer of REF through this node's proxy (writer.py, RATE rows a second; the
# runner's WRITER_RATE, which bounds the rows an unplanned failover may lose), as a transient unit. Its attempts are in
# /root/writer/RUN/acked and failed.
writer_start() {
  local ref=$1 run=$2 rate=${3:?writer_start REF RUN RATE}
  mkdir -p "/root/writer/$run"
  systemd-run --quiet --unit="sv-writer-$run" --collect --property=Type=exec \
    /usr/bin/python3 /root/writer.py --host "$ref.api.$SUPAVISE_DOMAIN" --key-file "/root/keys/$ref.sec" \
    --out "/root/writer/$run" --start $((run * RUN_SPAN)) --rate "$rate"
  sleep 3
  [[ $(unit_state "sv-writer-$run.service") == active ]] || fail "the writer $run does not run: $(journalctl --no-pager -u "sv-writer-$run.service" -n 10)"
}
writer_stop() { systemctl stop "sv-writer-$1.service" 2>/dev/null || true; }
writers_stop_all() { systemctl stop 'sv-writer-*.service' 2>/dev/null || true; }
writer_counts() { echo "$(wc -l <"/root/writer/$1/acked") acknowledged, $(wc -l <"/root/writer/$1/failed") failed"; }
# primary_ids REF RUN: the ids of the writer run that the cluster of REF on this node holds.
primary_ids() {
  pg_local "$1" "select id from public.writes where id >= $(($2 * RUN_SPAN)) and id < $((($2 + 1) * RUN_SPAN)) order by id"
}
# replay_lag REF: the largest replay lag a standby of REF shows on this node (a primary), in seconds.
replay_lag() {
  pg_local "$1" "select coalesce(max(extract(epoch from replay_lag)), 0)::numeric(8,3) from pg_stat_replication"
}
# replay_lag_max REF SECONDS: the largest replay_lag seen when it is read once a second for SECONDS; one reading can fall
# between two bursts of the writer.
replay_lag_max() {
  local i v max=0
  for ((i = 0; i < $2; i++)); do
    v=$(replay_lag "$1") || v=0
    max=$(awk -v a="$max" -v b="${v:-0}" 'BEGIN {print (b > a ? b : a)}')
    sleep 1
  done
  echo "$max"
}

# ---- what the test leaves for the artifact ---------------------------------------------------------------------
# A name that suggests a secret, or a URL or DSN (which can carry a password), loses its value: the artifact of a public
# repository is world-readable.
redact() { sed -E 's/^([A-Za-z0-9_]*(secret|pass|token|key|credential|dsn|url)[A-Za-z0-9_]*) *=.*/\1 = "<removed>"/I'; }

# node_dump: this node's view of the cluster, for a failed check and for the end of the run. The test sets no secret in
# the configuration, and the configuration is printed with every value whose name suggests one (see redact) removed.
node_dump() {
  set +e +u +o pipefail
  local d ref f
  echo "== $(date -u +%FT%TZ) $(hostname) $($SV --version 2>&1 | head -n1)"
  echo "-- units"; systemctl list-units 'supavise*' 'sv-writer*' --all --no-legend --plain 2>&1 | awk '{print $1, $3, $4}'
  echo "-- config.toml"; redact </etc/supavise/config.toml 2>&1
  for f in /etc/supavise/config.d/*.toml; do [[ -f $f ]] && { echo "-- $f"; redact <"$f"; }; done
  echo "-- cluster directory"; ls -l /etc/supavise/cluster 2>&1
  echo "-- fenced.json"; cat /var/lib/supavise/fenced.json 2>&1
  echo "-- fence command log"; cat /var/lib/supavise/fence-test.log 2>&1
  echo "-- data directories"; ls -d /var/lib/supavise/projects/*/postgres/data* 2>&1
  echo "-- status"; supavise status 2>&1 | head -n 80
  echo "-- node ls"; supavise node ls 2>&1
  echo "-- host layer"; supavise system converge --check --json 2>&1 | head -c 4000; echo
  echo "-- registry"
  reg "select id, name, state, region, version, peer_addr from supavise.nodes order by id" 2>&1
  reg "select leader, epoch, maintenance from supavise.cluster" 2>&1
  reg "select ref, node_id, status, class from supavise.projects order by ref" 2>&1
  reg "select identifier, node_id, status, init_step, init_error from supavise.replicas order by identifier" 2>&1
  reg "select id, scope, kind, coalesce(ref, ''), from_node, to_node, epoch, state, left(error, 300) from supavise.moves order by id" 2>&1
  echo "-- PostgreSQL clusters on this node"
  for d in /var/lib/supavise/projects/*/postgres/sock; do
    ref=$(basename "$(dirname "$(dirname "$d")")")
    echo "$ref: $(pg_local "$ref" "select case when pg_is_in_recovery() then 'standby, replayed ' || coalesce(pg_last_wal_replay_lsn()::text, '?') else 'primary at ' || pg_current_wal_lsn()::text end, current_setting('hot_standby'), (select count(*) from pg_stat_replication), (select coalesce(string_agg(status || ' ' || coalesce(sender_host, ''), ','), '') from pg_stat_wal_receiver)" 2>&1 | tr '\n' ' ')"
  done
  echo "-- writer"
  for d in /root/writer/*; do [[ -d $d ]] && echo "$d: $(wc -l <"$d/acked") acknowledged, $(wc -l <"$d/failed") failed"; done
  echo "-- listening"; ss -ltnH 2>&1 | awk '{print $4}' | sort -t: -k2 -n | paste -sd' ' -
  true
}
