#!/usr/bin/env bash
# The shared services of a follower node, and Storage's credentials on AWS, on one runner.
#
#   Storage: [fleet] storage_backend = "s3" with storage_s3_role_arn. The daemon assumes the role through
#            STS (a stand-in on loopback hands out the object store's key), serves the credentials on
#            a loopback endpoint, and supavise-storage, which holds no key and cannot reach the
#            instance role, uploads to Garage through it. The endpoint answers only the token of the
#            boot; a daemon restart changes neither the token nor Storage's process.
#   Follower: a hot standby of the system cluster and of a project is built next to the leader, and
#            the Go test of internal/fleet (follower_linux_test.go, tag fleetfollower) acts as the
#            daemon of a second node on it: fleet.Setup starts Supavisor on the replicated _supavisor
#            without bin/prepare and parks the other services; the replicated tenant of the project
#            logs in; a replica tenant written by the leader is served after the standby replayed it
#            and the follower's Supavisor was told to forget the old target (GET
#            /api/tenants/<id>/terminate); nothing writes to the standby.
#
#   sudo SUPAVISE_BIN=/path/to/supavise-linux-amd64 FLEET_FOLLOWER_TEST=/path/to/fleet.test tests/linux/fleet-follower.sh
#
# FLEET_FOLLOWER_TEST is `go test -c -tags fleetfollower ./internal/fleet`; without it the script builds it
# with the go toolchain. The S3 service is Garage at S3_ENDPOINT (default http://127.0.0.1:9000) with the bucket
# and key of the linux workflow's backup-integration job. Without SUPAVISE_BIN the script builds supavise.
# The follower runs under the exec supervisor in its own state directory, so nothing clashes with the
# leader's units. Needs network access for the artifact downloads. Exit status is non-zero on the first
# failure; logs stay in $LOG_DIR.
#
# Not run in development: it needs root, systemd and Linux. CI runs it on an ephemeral Ubuntu 24.04 VM.
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
FF=$(cd "$(dirname "${BASH_SOURCE[0]}")/fleet-follower" && pwd)

S3_ENDPOINT=${S3_ENDPOINT:-http://127.0.0.1:9000}
S3_BUCKET=${S3_BUCKET:-supavise-test}
S3_KEY=${S3_KEY:-GK0123456789abcdef01234567}
S3_SECRET=${S3_SECRET:-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}
ROLE_ARN=arn:aws:iam::123456789012:role/supavise-storage
CI_AWS_KEY=AKIACIFLEETFOLLOWER   # what the daemon signs its STS calls with (a stand-in checks nothing)

# The leader's ports, away from anything the runner may listen on; then the follower's.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000 P_EDGE=19000
P_CREDS=14010 P_STS=19890
F_SYS=26000 F_SESSION=25433 F_TRANSACTION=26544 F_API=24002 F_REALTIME=24100 F_STORAGE=25100 F_STORAGE_ADMIN=25101 F_PGMETA=28180 F_STUDIO=23100 F_EDGE=29100

F_STATE=/var/lib/supavise-follower   # the follower node's state directory
FF_ROOT=/var/lib/supavise-ff         # standbys, the follower's configuration, scratch files
STS_LOG=$LOG_DIR/sts.log

cleanup() {
  local rc=$?
  set +e
  collect_logs
  mkdir -p "$LOG_DIR"
  cp "$STS_LOG" "$FF_ROOT"/follower.toml "$LOG_DIR/" 2>/dev/null
  cp "$F_STATE"/logs/*.log "$LOG_DIR/" 2>/dev/null
  for u in $(systemctl list-units --all --plain --no-legend 'ff-*' 2>/dev/null | awk '{print $1}'); do
    journalctl --no-pager -o short-iso -u "$u" >"$LOG_DIR/$u.log" 2>&1
  done
  { ps -eo pid,ppid,etimes,rss,args --sort=pid | grep -E 'postgres|beam|supavise|python3' | grep -v grep; ss -tnlp; } >"$LOG_DIR/processes.txt" 2>&1
  [[ -s $FF_ROOT/sts.pid ]] && kill "$(cat "$FF_ROOT/sts.pid")" 2>/dev/null
  systemctl stop 'ff-*' >/dev/null 2>&1
  teardown
  exit "$rc"
}
trap cleanup EXIT

as_sv() { sudo -u "$SUPAVISE_USER" "$@"; }
# has OUTPUT PATTERN: grep -q over a string. A pipe into grep -q would fail under pipefail, because the
# producer dies of SIGPIPE once grep has its match.
has() { grep -qE -- "$2" <<<"$1"; }

# ---- the leader node: Storage on S3 with a role --------------------------------------------------

preflight
[[ $(http_code "$S3_ENDPOINT") != 000 ]] || fail "no S3 service answers on $S3_ENDPOINT"
install_binary
FOLLOWER_TEST=${FLEET_FOLLOWER_TEST:-}
if [[ -z $FOLLOWER_TEST ]]; then
  command -v go >/dev/null || fail "no FLEET_FOLLOWER_TEST and no go toolchain to build one"
  (cd "$REPO_ROOT" && go test -c -tags fleetfollower -o /tmp/fleet-follower.test ./internal/fleet) || fail "building the follower test"
  FOLLOWER_TEST=/tmp/fleet-follower.test
fi
chmod 0755 "$FOLLOWER_TEST"
setup_node
cat >>"$SUPAVISE_CONF" <<CONF

[ports]
supavisor_session = $P_SESSION
supavisor_transaction = $P_TRANSACTION
realtime = $P_REALTIME
storage = $P_STORAGE
storage_admin = $P_STORAGE_ADMIN
pgmeta = $P_PGMETA
studio = $P_STUDIO
edge_runtime = $P_EDGE

[fleet]
supavisor_api_port = $P_API
storage_backend = "s3"
storage_s3_bucket = "$S3_BUCKET"
storage_s3_endpoint = "$S3_ENDPOINT"
storage_s3_region = "us-east-1"
storage_s3_force_path_style = true
storage_s3_role_arn = "$ROLE_ARN"
storage_credentials_port = $P_CREDS
CONF

log "a stand-in for STS, and the environment the daemon calls it with"
mkdir -p "$LOG_DIR" "$FF_ROOT"
: >"$STS_LOG"
python3 "$FF/fake-sts.py" --port "$P_STS" --key-id "$S3_KEY" --secret "$S3_SECRET" --log "$STS_LOG" >"$LOG_DIR/fake-sts.out" 2>&1 &
echo $! >"$FF_ROOT/sts.pid"
mkdir -p /etc/systemd/system/supavise.service.d
cat >/etc/systemd/system/supavise.service.d/fleet-follower.conf <<CONF
[Service]
Environment=AWS_ACCESS_KEY_ID=$CI_AWS_KEY
Environment=AWS_SECRET_ACCESS_KEY=ci-secret-not-checked
Environment=AWS_REGION=us-east-1
Environment=AWS_ENDPOINT_URL_STS=http://127.0.0.1:$P_STS
CONF
systemctl daemon-reload

log "system init (downloads artifacts), fleet start"
system_init
wait_active supavise-postgres@system.service 30
supavise fleet start || fail "fleet start"

ENV_FILE=$SUPAVISE_STATE/projects/system/storage.env
TOKEN_FILE=$SUPAVISE_STATE/system/storage/credentials.json
log "Storage's environment: the endpoint and its token, no key"
grep -q "^AWS_CONTAINER_CREDENTIALS_FULL_URI=\"http://127.0.0.1:$P_CREDS/credentials\"" "$ENV_FILE" || { sed -E 's/(TOKEN|SECRET|KEY|PASSWORD|URL)([A-Z_]*)="[^"]*"/\1\2=<redacted>/' "$ENV_FILE" >&2; fail "storage.env does not point at the credential endpoint"; }
grep -q '^AWS_CONTAINER_AUTHORIZATION_TOKEN="[0-9a-f]\{64\}"' "$ENV_FILE" || fail "storage.env has no authorization token"
if grep -qE '^AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)=' "$ENV_FILE"; then fail "storage.env holds a static AWS key"; fi
[[ $(stat -c %a "$TOKEN_FILE") == 600 && $(stat -c %U "$TOKEN_FILE") == "$SUPAVISE_USER" ]] || fail "the token file is not 0600 and the supavise user's: $(stat -c '%a %U' "$TOKEN_FILE")"
TOKEN=$(json_get 'd["token"]' <"$TOKEN_FILE")
grep -q "^AWS_CONTAINER_AUTHORIZATION_TOKEN=\"$TOKEN\"" "$ENV_FILE" || fail "storage.env carries another token than the token file"

log "the daemon brings the services up and serves the credentials"
systemctl start supavise.service
for ((i = 0; i < 180; i++)); do
  supavise fleet status >/dev/null 2>&1 && break
  sleep 2
done
supavise fleet status || { journalctl --no-pager -u supavise.service | tail -40 >&2; fail "the daemon did not bring the shared services up"; }
for ((i = 0; i < 30; i++)); do
  [[ $(http_code -H "Host: $API_HOST" http://127.0.0.1/v1/projects) == 401 ]] && break
  sleep 1
done
[[ $(http_code -H "Host: $API_HOST" http://127.0.0.1/v1/projects) == 401 ]] || fail "the Management API does not answer through the proxy"
has "$(journalctl --no-pager -u supavise.service)" 'storage credentials: serving the role' || fail "the daemon did not say it serves the role"

CRED_URL=http://127.0.0.1:$P_CREDS/credentials
[[ $(http_code "$CRED_URL") == 403 ]] || fail "the credential endpoint answers a caller without the token"
[[ $(http_code -H "Authorization: $TOKEN-wrong" "$CRED_URL") == 403 ]] || fail "the credential endpoint answers a wrong token"
[[ $(http_code -H "Authorization: Bearer $TOKEN" "$CRED_URL") == 403 ]] || fail "the credential endpoint takes the token with a scheme"
[[ -z $(curl -s -m 5 "$CRED_URL") ]] || fail "a refusal has a body"
CREDS=$(curl -fsS -m 10 -H "Authorization: $TOKEN" "$CRED_URL") || fail "the credential endpoint refuses the token of this boot"
[[ $(json_get 'd["AccessKeyId"]' <<<"$CREDS") == "$S3_KEY" ]] || fail "the endpoint does not hand out the assumed role's key"
[[ $(json_get 'd["RoleArn"]' <<<"$CREDS") == "$ROLE_ARN" && $(json_get 'bool(d["Token"])' <<<"$CREDS") == True ]] || fail "unexpected credentials shape"
[[ $(json_get 'd["Expiration"].endswith("Z") and len(d["Expiration"]) == 20' <<<"$CREDS") == True ]] || fail "the expiration is not an RFC 3339 UTC time"
grep -q "AssumeRole role=$ROLE_ARN session=supavise-storage seconds=3600 signed_by=$CI_AWS_KEY" "$STS_LOG" || { cat "$STS_LOG" >&2; fail "the daemon did not assume the role with the instance's credentials"; }

log "create a project and upload through Storage, which has only the endpoint"
claim_and_token
gen_dbpass
DBPW=$DBPASS
REF=$(api_create_project ff-leader)
project_keys "$REF"
HOST="$REF.api.$SUPAVISE_DOMAIN"
st_proxy() { curl -fsS -m 30 -H "Host: $HOST" -H "apikey: $SEC" -H "Authorization: Bearer $SEC" "$@"; }
st_proxy -X POST -H 'Content-Type: application/json' -d '{"name":"files"}' "http://127.0.0.1/storage/v1/bucket" >/dev/null || fail "create a bucket"
st_proxy -X POST -H 'Content-Type: text/plain' --data-binary 'uploaded through the credential endpoint' "http://127.0.0.1/storage/v1/object/files/hello.txt" >/dev/null \
  || { journalctl --no-pager -u supavise-storage.service | tail -30 >&2; fail "upload through Storage failed"; }
got=$(st_proxy "http://127.0.0.1/storage/v1/object/files/hello.txt") || fail "download through Storage"
[[ $got == 'uploaded through the credential endpoint' ]] || fail "downloaded '$got'"
s3_list() { curl -fsS -m 20 --aws-sigv4 "aws:amz:us-east-1:s3" --user "$S3_KEY:$S3_SECRET" "$S3_ENDPOINT/$S3_BUCKET?list-type=2&prefix=$1"; }
has "$(s3_list "$REF/files/hello.txt/")" "<Key>$REF/files/hello.txt/" || { s3_list "" >&2 || true; fail "the object is not in the bucket under $REF/files/hello.txt/"; }
[[ -z $(find "$SUPAVISE_STATE/system/storage/objects" -type f -path "*$REF*" 2>/dev/null) ]] || fail "the object was also written to the file backend"
SPID=$(systemctl show -p MainPID --value supavise-storage.service)
SENV=$(tr '\0' '\n' <"/proc/$SPID/environ")
if has "$SENV" '^AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY)='; then fail "supavise-storage runs with a static AWS key"; fi
has "$SENV" '^AWS_CONTAINER_CREDENTIALS_FULL_URI=' || fail "supavise-storage does not run with the endpoint"
for i in 1 2 3; do
  st_proxy -X POST -H 'Content-Type: text/plain' --data-binary "again $i" "http://127.0.0.1/storage/v1/object/files/again-$i.txt" >/dev/null || fail "upload $i"
done
[[ $(grep -c AssumeRole "$STS_LOG") -eq 1 ]] || { cat "$STS_LOG" >&2; fail "the role was assumed $(grep -c AssumeRole "$STS_LOG") times for six requests of Storage and mine; the daemon caches it"; }

log "a daemon restart keeps the token, Storage's process, and the credentials"
systemctl restart supavise.service
for ((i = 0; i < 120; i++)); do
  [[ $(http_code -H "Host: $API_HOST" http://127.0.0.1/v1/projects) == 401 ]] && supavise fleet status >/dev/null 2>&1 && break
  sleep 2
done
[[ $(json_get 'd["token"]' <"$TOKEN_FILE") == "$TOKEN" ]] || fail "the token changed with a daemon restart"
[[ $(systemctl show -p MainPID --value supavise-storage.service) == "$SPID" ]] || fail "supavise-storage was restarted by a daemon restart"
curl -fsS -m 10 -H "Authorization: $TOKEN" "$CRED_URL" >/dev/null || fail "the endpoint refuses the token after a daemon restart"
st_proxy -X POST -H 'Content-Type: text/plain' --data-binary 'after the restart' "http://127.0.0.1/storage/v1/object/files/restarted.txt" >/dev/null || fail "upload after the daemon restart"

# ---- the follower: standbys of the system cluster and of the project -------------------------------------

PGBIN=$(dirname "$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)")
PSQL=$PGBIN/psql
run_port() { sed -n "s/.*'-p' '\([0-9]*\)'.*/\1/p" "$SUPAVISE_STATE/projects/$1/postgres.run" | head -1; } # REF
proj_sock() { echo "$SUPAVISE_STATE/projects/$1/postgres/sock"; }                                          # REF
psql_at() { as_sv "$PSQL" "host=$1 port=$2 user=supabase_admin dbname=$3" -qAtX -v ON_ERROR_STOP=1 -c "$4" </dev/null; } # SOCK PORT DB SQL

# make_standby SRC_REF NAME DATA SOCK PORT: a hot standby of SRC_REF's running cluster, started through the
# primary's own launcher on its own port, data directory and socket directory. The postgres artifact has
# no pg_basebackup, so the base backup is taken the way internal/backup takes one: pg_backup_start, a copy of
# the data directory without the volatile directories, pg_backup_stop with its backup_label, then the
# WAL segments the primary holds. The standby follows the primary over its private socket as supabase_admin
# (trusted there) and streams; wal_keep_size stands in for the archive.
make_standby() {
  local ref=$1 name=$2 data=$3 sock=$4 port=$5 wd=$FF_ROOT/$2 src=$SUPAVISE_STATE/projects/$1 psrc sport ssock i
  rm -rf "$data" "$wd"
  install -d -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" -m 0700 "$data" "$sock"
  install -d -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" -m 0750 "$wd"
  psrc=$SUPAVISE_STATE/projects/$ref/postgres/data
  python3 "$FF/standby.py" render --src-run "$src/postgres.run" --src-env "$src/postgres.env" --dir "$wd" --data "$data" --sock "$sock" --port "$port" --name "$name" \
    >"$wd/render.json" || fail "$name: could not render the launcher of $ref"
  chown -R "$SUPAVISE_USER:$SUPAVISE_USER" "$wd"
  sport=$(json_get 'd["src_port"]' <"$wd/render.json")
  ssock=$(json_get 'd["src_sock"]' <"$wd/render.json")
  psql_at "$ssock" "$sport" postgres "alter system set wal_keep_size = '256MB'" >/dev/null || fail "$name: wal_keep_size"
  psql_at "$ssock" "$sport" postgres "select pg_reload_conf()" >/dev/null || fail "$name: reload"
  cat >"$wd/backup.sql" <<SQL
\\set ON_ERROR_STOP on
select pg_backup_start('fleet-follower-$name', true);
\\! tar -C '$psrc' --exclude=./postmaster.pid --exclude=./postmaster.opts --exclude='./pg_wal/*' --exclude='./pg_replslot/*' --exclude='./pg_dynshmem/*' --exclude='./pg_notify/*' --exclude='./pg_serial/*' --exclude='./pg_snapshots/*' --exclude='./pg_stat_tmp/*' --exclude='./pg_subtrans/*' -cf - . | tar -C '$data' -xf -
\\pset format unaligned
\\pset tuples_only on
\\o '$data/backup_label'
select labelfile from pg_backup_stop(false);
\\o
SQL
  chmod 0644 "$wd/backup.sql"
  as_sv "$PSQL" "host=$ssock port=$sport user=supabase_admin dbname=postgres" -qAtX -f "$wd/backup.sql" >"$LOG_DIR/basebackup-$name.log" 2>&1 \
    || { cat "$LOG_DIR/basebackup-$name.log" >&2; fail "$name: the base backup of $ref failed"; }
  as_sv sh -c "mkdir -p '$data/pg_wal/archive_status' && cp -a '$psrc'/pg_wal/[0-9A-F]* '$data/pg_wal/' && sed -i -e '\$ {/^\$/d}' '$data/backup_label' && echo postgres >'$data/postmaster.opts' && touch '$data/standby.signal'" \
    || fail "$name: the WAL of the base backup"
  as_sv sh -c "printf '%s\\n' \"primary_conninfo = 'host=$ssock port=$sport user=supabase_admin application_name=$name'\" >>'$data/postgresql.auto.conf'"
  systemd-run --quiet --collect --unit="ff-pg-$name" --uid="$SUPAVISE_USER" --gid="$SUPAVISE_USER" \
    --property=EnvironmentFile="$wd/env" --property=KillMode=mixed --property=KillSignal=SIGINT \
    --property=TimeoutStopSec=60 --property=LimitNOFILE=16384 /bin/sh "$wd/run.sh" || fail "$name: systemd-run"
  for ((i = 0; i < 120; i++)); do
    [[ $(psql_at "$sock" "$port" postgres 'select pg_is_in_recovery()' 2>/dev/null || true) == t ]] && break
    sleep 1
  done
  if [[ $(psql_at "$sock" "$port" postgres 'select pg_is_in_recovery()' 2>/dev/null || true) != t ]]; then
    journalctl --no-pager -u "ff-pg-$name" | tail -40 >&2
    fail "$name: the standby does not accept connections"
  fi
  for ((i = 0; i < 60; i++)); do
    [[ $(psql_at "$sock" "$port" postgres 'select status from pg_stat_wal_receiver' 2>/dev/null || true) == streaming ]] && break
    sleep 1
  done
  [[ $(psql_at "$sock" "$port" postgres 'select status from pg_stat_wal_receiver' 2>/dev/null || true) == streaming ]] \
    || { journalctl --no-pager -u "ff-pg-$name" | tail -40 >&2; fail "$name: the standby does not stream"; }
  log "$name: a hot standby of $ref on port $port, streaming"
}

PRIMARY_PORT=$(project_field "$REF" 'd["ports"]["Postgres"]')
REPLICA_PORT=$((10000 + PRIMARY_PORT - 20000)) # config.ReplicaPorts with the default bases; the Go test checks it
install -d -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" -m 0750 "$F_STATE" "$F_STATE/projects" "$F_STATE/projects/system" "$F_STATE/projects/system/postgres" "$FF_ROOT"

log "the follower's system cluster: a hot standby of the leader's, where a node keeps its own"
make_standby system ff-system "$F_STATE/projects/system/postgres/data" "$F_STATE/projects/system/postgres/sock" "$F_SYS"
log "the project's replica"
make_standby "$REF" ff-replica "$FF_ROOT/replica/data" "$FF_ROOT/replica/sock" "$REPLICA_PORT"
# The row of the project's tenant is on the standby.
SUP_DB=_supavisor
for ((i = 0; i < 30; i++)); do
  [[ $(psql_at "$F_STATE/projects/system/postgres/sock" "$F_SYS" "$SUP_DB" "select count(*) from _supavisor.tenants where external_id = '$REF'" 2>/dev/null || true) == 1 ]] && break
  sleep 1
done
[[ $(psql_at "$F_STATE/projects/system/postgres/sock" "$F_SYS" "$SUP_DB" "select count(*) from _supavisor.tenants where external_id = '$REF'") == 1 ]] || fail "the tenant row did not reach the follower's standby"

# The follower's configuration: its own state directory and ports, the cluster's master key, the exec
# supervisor. The artifacts are the leader's.
ln -sfn "$SUPAVISE_STATE/artifacts" "$F_STATE/artifacts"
cat >"$FF_ROOT/follower.toml" <<CONF
domain = "$SUPAVISE_DOMAIN"
supervisor = "exec"
state_dir = "$F_STATE"
log_level = "info"
bin_path = "/usr/local/bin/supavise"

[tls]
mode = "off"

[ports]
system_postgres = $F_SYS
supavisor_session = $F_SESSION
supavisor_transaction = $F_TRANSACTION
realtime = $F_REALTIME
storage = $F_STORAGE
storage_admin = $F_STORAGE_ADMIN
pgmeta = $F_PGMETA
studio = $F_STUDIO
edge_runtime = $F_EDGE

[fleet]
supavisor_api_port = $F_API
CONF
chown "$SUPAVISE_USER:$SUPAVISE_USER" "$FF_ROOT/follower.toml"
chmod 0640 "$FF_ROOT/follower.toml"

log "the follower node's shared services on its standby (Go test, as the supavise user)"
T0=$(date '+%Y-%m-%d %H:%M:%S')
sleep 1
export FLEET_FOLLOWER_CONFIG=$FF_ROOT/follower.toml FLEET_LEADER_CONFIG=$SUPAVISE_CONF FLEET_REF=$REF FLEET_DB_PASSWORD=$DBPW FLEET_REPLICA_PORT=$REPLICA_PORT
(cd / && sudo -u "$SUPAVISE_USER" -H --preserve-env=FLEET_FOLLOWER_CONFIG,FLEET_LEADER_CONFIG,FLEET_REF,FLEET_DB_PASSWORD,FLEET_REPLICA_PORT \
  "$FOLLOWER_TEST" -test.v -test.run '^TestLinuxFollower$' -test.timeout 20m) 2>&1 | tee "$LOG_DIR/follower-test.log" || fail "the follower test failed"

log "nothing wrote to the standby"
errs=$(journalctl --no-pager -o cat -u ff-pg-ff-system --since "$T0" | grep -E 'ERROR|FATAL|PANIC|read-only|cannot execute' || true)
[[ -z $errs ]] || { echo "$errs" | head -20 >&2; fail "the follower's standby logged errors while the follower ran"; }
rerrs=$(journalctl --no-pager -o cat -u ff-pg-ff-replica --since "$T0" | grep -E 'ERROR|FATAL|PANIC|read-only|cannot execute' || true)
[[ -z $rerrs ]] || { echo "$rerrs" | head -20 >&2; fail "the project's replica logged errors"; }
# The detector sees what it looks for: a write to the standby is refused and logged.
T1=$(date '+%Y-%m-%d %H:%M:%S')
sleep 1
psql_at "$F_STATE/projects/system/postgres/sock" "$F_SYS" "$SUP_DB" 'create table _supavisor.ff_control (x int)' >/dev/null 2>&1 || true
has "$(journalctl --no-pager -o cat -u ff-pg-ff-system --since "$T1")" 'read-only|cannot execute' || fail "a deliberate write on the standby left no line the detector matches"

log "the follower's Supavisor log shows no database errors"
[[ -s $F_STATE/logs/supavise-supavisor.service.log ]] || fail "the follower's Supavisor left no log under $F_STATE/logs"
serrs=$(grep -iE 'read-only|cannot execute|postgrex.*error' "$F_STATE/logs/supavise-supavisor.service.log" | head -5 || true)
[[ -z $serrs ]] || { echo "$serrs" >&2; fail "the follower's Supavisor logged database errors"; }
log "fleet-follower: ok"
