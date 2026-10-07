#!/usr/bin/env bash
# Shared services under real systemd units, through the daemon: system init, `sbctl fleet
# start` once (it fetches the artifacts, as the installer does), then `sbctl serve` (sbctl.service)
# brings the shared services up by itself and a project created through the Management API is
# registered with Supavisor, Realtime and Storage by the daemon's engine. Then: REST through the
# proxy, a Storage upload through the proxy, pooler logins (session and transaction mode, plain and
# sslmode=require), a Storage bucket, upload and signed-URL download, a Realtime channel join, key
# rotation, crash recovery of the services, the instance metadata service denied to every shared
# service, tenant removal with the project's delete through the API.
#
#   sudo SBCTL_BIN=/path/to/sbctl-linux-amd64 tests/linux/fleet-smoke.sh [--teardown]
#
# Without SBCTL_BIN the script builds sbctl with the go toolchain. It needs network access
# for the artifact downloads. Exit status is non-zero on the first failure; logs (journal,
# unit list, host facts, per-unit memory) stay in $LOG_DIR (default /tmp/sbctl-linux-logs).
#
# Studio is not started here: its artifact is our own build (studio.yml), not a slim-services
# release. Without [studio] artifact_url, `fleet start` skips it and `fleet status` does not
# count it.
#
# Not run in development: it needs root, systemd and Linux. CI runs it on an ephemeral
# Ubuntu 24.04 VM (amd64 and arm64).
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1

trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

# fleet_memory LABEL: append the memory each shared service holds to $LOG_DIR/fleet-memory.txt.
fleet_memory() {
  mkdir -p "$LOG_DIR"
  {
    echo "# $1"
    for u in pgmeta supavisor realtime storage; do
      printf '%s MemoryCurrent=%s bytes (%s MiB)\n' "sb-$u" \
        "$(systemctl show -p MemoryCurrent --value "sb-$u.service" 2>/dev/null)" \
        "$(( $(systemctl show -p MemoryCurrent --value "sb-$u.service" 2>/dev/null || echo 0) / 1048576 ))"
    done
  } >>"$LOG_DIR/fleet-memory.txt" 2>&1 || true
}

# Ports away from anything the runner may already listen on (Postgres on 5432, Node on
# 3000, ...). The pooler ports are public in production; here they are only loopback traffic.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000

preflight
install_binary
setup_node
cat >>"$SBCTL_CONF" <<CONF

[ports]
supavisor_session = $P_SESSION
supavisor_transaction = $P_TRANSACTION
realtime = $P_REALTIME
storage = $P_STORAGE
storage_admin = $P_STORAGE_ADMIN
pgmeta = $P_PGMETA
studio = $P_STUDIO

[fleet]
supavisor_api_port = $P_API
CONF

log "system init (downloads artifacts)"
system_init
wait_active sb-postgres@system.service 30

log "fleet start (downloads pooler, realtime, storage and pgmeta)"
sbctl fleet start || fail "fleet start"   # no Studio artifact configured: skipped with a note
for svc in pgmeta supavisor realtime storage; do
  u="sb-$svc.service"
  [[ $(unit_state "$u") == active ]] || fail "$u is $(unit_state "$u")"
  [[ $(systemctl show -p User --value "$u") == "$SBCTL_USER" ]] || fail "$u does not run as $SBCTL_USER"
  [[ $(systemctl show -p Slice --value "$u") == sbctl.slice ]] || fail "$u is not in sbctl.slice"
  [[ $(systemctl show -p MemoryMax --value "$u") == 1073741824 ]] || fail "$u: MemoryMax drop-in not applied"
done
sbctl fleet status || fail "fleet status"   # an absent Studio does not count
# A second start changes nothing: no unit restarts.
declare -A PIDS
for svc in pgmeta supavisor realtime storage; do PIDS[$svc]=$(systemctl show -p MainPID --value "sb-$svc.service"); done
sbctl fleet start --no-fetch || fail "second fleet start"
for svc in pgmeta supavisor realtime storage; do
  [[ ${PIDS[$svc]} == "$(systemctl show -p MainPID --value "sb-$svc.service")" ]] || fail "sb-$svc was restarted by an unchanged fleet start"
done

fleet_memory "after fleet start, no project registered"
log "containment: what the fleet units cannot read"
sees() { # UNIT PATH: exit 0 if PATH is readable from UNIT's namespace as the sbctl user
  local pid
  pid=$(systemctl show -p MainPID --value "$1")
  [[ $pid -gt 0 ]] || fail "$1 has no main pid"
  nsenter -t "$pid" -m -- runuser -u "$SBCTL_USER" -- test -r "$2" 2>/dev/null
}
for svc in supavisor realtime storage pgmeta; do
  for hidden in "$SBCTL_STATE/backups" "$SBCTL_STATE/certs" /etc/sbctl; do
    if sees "sb-$svc.service" "$hidden"; then fail "sb-$svc can read $hidden"; fi
  done
done
# The units are allowlists (TemporaryFileSystem=/var/lib/sbctl plus BindPaths of the unit's own
# run directory), so no shared unit sees any environment file under projects/, its own
# included (systemd reads EnvironmentFile= before it builds the namespace), nor a sibling's.
no_env_visible() { # every shared service against every environment file that exists now
  local svc f n=0
  for svc in supavisor realtime storage pgmeta; do
    for f in "$SBCTL_STATE"/projects/*/*.env; do
      [[ -e $f ]] || continue
      n=$((n + 1))
      if sees "sb-$svc.service" "$f"; then fail "sb-$svc can read $f"; fi
    done
  done
  [[ $n -gt 0 ]] || fail "containment check found no environment file to test"
}
no_env_visible

log "instance metadata: denied to every shared service, from inside its unit"
imds_up
trap 'rc=$?; imds_down; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT
for svc in pgmeta supavisor realtime storage; do
  imds_denied_by_unit "sb-$svc.service"
  imds_blocked_in "sb-$svc.service"
done
imds_down
trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

log "the daemon starts the shared services at boot (sbctl serve, fleet.Setup)"
sbctl fleet stop || fail "fleet stop"
for svc in pgmeta supavisor realtime storage; do
  [[ $(unit_state "sb-$svc.service") == inactive ]] || fail "sb-$svc is $(unit_state "sb-$svc.service") after fleet stop"
done
systemctl start sbctl.service
for ((i = 0; i < 180; i++)); do
  sbctl fleet status >/dev/null 2>&1 && break
  sleep 2
done
sbctl fleet status || { journalctl --no-pager -u sbctl.service | tail -40 >&2; fail "the daemon did not bring the shared services up"; }
for svc in pgmeta supavisor realtime storage; do
  [[ $(unit_state "sb-$svc.service") == active ]] || fail "sb-$svc is $(unit_state "sb-$svc.service") after the daemon started"
done
for ((i = 0; i < 30; i++)); do
  [[ $(http_code -H "Host: $API_HOST" http://127.0.0.1/v1/projects) == 401 ]] && break
  sleep 1
done
[[ $(http_code -H "Host: $API_HOST" http://127.0.0.1/v1/projects) == 401 ]] || fail "the Management API does not answer through the proxy"

log "create a project through the Management API: the daemon registers it with the shared services"
claim_and_token
gen_dbpass
DBPW=$DBPASS
REF=$(api_create_project fleet-api)
project_keys "$REF"
SVC=$(project_field "$REF" 'd["keys"]["service_role_key"]' --show-keys)
ANON=$(project_field "$REF" 'd["keys"]["anon_key"]' --show-keys)
HOST="$REF.api.$SBCTL_DOMAIN"
PSQL=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
no_env_visible   # now also covers the project's own environment files

# No `sbctl fleet ensure-tenant` anywhere below until the CLI step near the end: every check
# runs on the tenants the daemon's engine registered when it created the project, and the
# rotation and the delete are the engine's too.
log "REST through the proxy with the publishable key (tenants registered by the daemon only)"
rest_through_proxy "$REF" "$PUB" api

log "Storage through the proxy: bucket, upload and read-back with the secret key"
st_proxy() { curl -fsS -m 30 -H "Host: $HOST" -H "apikey: $SEC" -H "Authorization: Bearer $SEC" "$@"; }
st_proxy -X POST -H 'Content-Type: application/json' -d '{"name":"proxied"}' "http://127.0.0.1/storage/v1/bucket" >/dev/null || fail "create a bucket through the proxy"
st_proxy -X POST -H 'Content-Type: text/plain' --data-binary 'uploaded through the proxy' "http://127.0.0.1/storage/v1/object/proxied/hello.txt" >/dev/null \
  || fail "upload through the proxy"
got=$(st_proxy "http://127.0.0.1/storage/v1/object/proxied/hello.txt") || fail "download through the proxy"
[[ $got == 'uploaded through the proxy' ]] || fail "downloaded '$got'"

log "pooler: postgres.$REF through Supavisor with sslmode=require on both ports"
for port in $P_SESSION $P_TRANSACTION; do
  info=$(PGPASSWORD=$DBPW "$PSQL" "host=127.0.0.1 port=$port user=postgres.$REF dbname=postgres sslmode=require connect_timeout=10" \
    -At -c 'select count(*) from public.smoke_items' -c '\conninfo' </dev/null) || fail "pooler port $port: sslmode=require login failed"
  grep -q '^2$' <<<"$info" || fail "pooler port $port: the project's table did not come back: $info"
  grep -Eiq 'ssl connection.*(protocol|true)' <<<"$info" || fail "pooler port $port: sslmode=require connected without TLS: $info"
done

log "pooler: postgres.$REF logs in on the session and transaction ports"
for port in $P_SESSION $P_TRANSACTION; do
  got=$(PGPASSWORD=$DBPW "$PSQL" "host=127.0.0.1 port=$port user=postgres.$REF dbname=postgres sslmode=disable connect_timeout=10" -Atc 'select current_user' </dev/null) \
    || fail "pooler port $port: login failed"
  [[ $got == postgres ]] || fail "pooler port $port: current_user is '$got'"
done
# The Supabase CLI refuses a remote database that does not answer TLS, so the pooler must
# (node-generated certificate: require encrypts without verifying it).
log "pooler: TLS on both ports (sslmode=require)"
for port in $P_SESSION $P_TRANSACTION; do
  info=$(PGPASSWORD=$DBPW "$PSQL" "host=127.0.0.1 port=$port user=postgres.$REF dbname=postgres sslmode=require connect_timeout=10" -Atc '\conninfo' </dev/null) \
    || fail "pooler port $port: sslmode=require login failed"
  grep -Eiq 'ssl connection.*(protocol|true)' <<<"$info" || fail "pooler port $port: sslmode=require connected without TLS: $info"
done
if PGPASSWORD=wrong "$PSQL" "host=127.0.0.1 port=$P_SESSION user=postgres.$REF dbname=postgres sslmode=disable connect_timeout=10" -Atc 'select 1' </dev/null >/dev/null 2>&1; then
  fail "pooler accepted a wrong password"
fi

log "storage: bucket, upload, signed URL download"
storage() { # METHOD PATH [curl args...]: prints the body, fails on a non-2xx status
  local m=$1 p=$2; shift 2
  curl -fsS --max-time 20 -X "$m" -H "x-forwarded-host: $HOST" -H "Authorization: Bearer $SVC" -H "apikey: $SVC" "$@" "http://127.0.0.1:$P_STORAGE$p"
}
storage POST /bucket -H 'Content-Type: application/json' -d '{"name":"files"}' >/dev/null || fail "create bucket"
storage POST /object/files/hello.txt -H 'Content-Type: text/plain' --data-binary 'hello from the fleet' >/dev/null || fail "upload"
SIGNED=$(storage POST /object/sign/files/hello.txt -H 'Content-Type: application/json' -d '{"expiresIn":60}' | json_get 'd["signedURL"]') || fail "sign"
[[ -n $SIGNED ]] || fail "empty signed URL"
got=$(curl -fsS --max-time 20 -H "x-forwarded-host: $HOST" "http://127.0.0.1:$P_STORAGE$SIGNED") || fail "signed download"
[[ $got == 'hello from the fleet' ]] || fail "downloaded '$got'"
[[ $(http_code -H "x-forwarded-host: $HOST" "http://127.0.0.1:$P_STORAGE/object/files/hello.txt") != 200 ]] || fail "anonymous read of a private object"
# Objects land under <ref>/ in the file backend.
[[ -n $(find "$SBCTL_STATE/system/storage/objects" -type d -name "$REF" 2>/dev/null) ]] || fail "no object directory for $REF"

log "realtime: join a channel (tenant from the Host header, JWT from the project)"
ws_join() { # PORT HOST ANON: exits 0 when the server answers phx_join with status ok
  python3 - "$@" <<'PY'
import base64, json, os, socket, struct, sys
port, host, anon = int(sys.argv[1]), sys.argv[2], sys.argv[3]
s = socket.create_connection(("127.0.0.1", port), timeout=20)
key = base64.b64encode(os.urandom(16)).decode()
s.sendall((f"GET /socket/websocket?apikey={anon}&vsn=1.0.0 HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\n"
           f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
buf = b""
while b"\r\n\r\n" not in buf:
    chunk = s.recv(4096)
    if not chunk:
        sys.exit("closed during handshake: %r" % buf)
    buf += chunk
head, rest = buf.split(b"\r\n\r\n", 1)
if b" 101 " not in head.split(b"\r\n")[0]:
    sys.exit("handshake refused: %r" % head.split(b"\r\n")[0])
def send(text):
    p = text.encode(); mask = os.urandom(4)
    n = len(p)
    hdr = bytes([0x81]) + (bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + struct.pack(">H", n))
    s.sendall(hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(p)))
def need(n):
    global rest
    while len(rest) < n:
        chunk = s.recv(4096)
        if not chunk:
            sys.exit("connection closed")
        rest += chunk
    out, rest = rest[:n], rest[n:]
    return out
def frame():
    b0, b1 = need(2)
    n = b1 & 0x7F
    if n == 126: n = struct.unpack(">H", need(2))[0]
    elif n == 127: n = struct.unpack(">Q", need(8))[0]
    payload = need(n)
    return b0 & 0x0F, payload
send(json.dumps({"topic": "realtime:smoke", "event": "phx_join", "ref": "1", "join_ref": "1", "payload": {
    "config": {"broadcast": {"self": False, "ack": False}, "presence": {"key": ""}, "postgres_changes": [], "private": False},
    "access_token": anon}}))
for _ in range(20):
    op, payload = frame()
    if op != 1:
        continue
    msg = json.loads(payload)
    if msg.get("event") == "phx_reply" and msg.get("ref") == "1":
        if msg["payload"].get("status") == "ok":
            print("joined"); sys.exit(0)
        sys.exit("join refused: %s" % payload.decode())
sys.exit("no reply to phx_join")
PY
}
ws_join "$P_REALTIME" "$REF.realtime.internal" "$ANON" || fail "realtime join"
if ws_join "$P_REALTIME" "abcdefghijklmnopqrst.realtime.internal" "$ANON" >/dev/null 2>&1; then
  fail "realtime accepted a token for an unknown tenant"
fi

# Helpers shared by the rotation, CLI and delete steps. The pooler login uses sslmode=require.
pooler_login() { # PORT [STDERR_VAR]: logs in as postgres.$REF with the project's password
  PGPASSWORD=$DBPW "$PSQL" "host=127.0.0.1 port=$1 user=postgres.$REF dbname=postgres sslmode=require connect_timeout=10" -Atc 'select 1' </dev/null 2>&1
}
storage_code() { # KEY: status of GET /bucket on the Storage port for the project's tenant
  http_code -H "x-forwarded-host: $HOST" -H "Authorization: Bearer $1" "http://127.0.0.1:$P_STORAGE/bucket"
}
# tenant_served KEY ANON: the pooler (both ports), Storage and Realtime all serve the tenant.
tenant_served() {
  local port
  for port in $P_SESSION $P_TRANSACTION; do pooler_login "$port" >/dev/null || return 1; done
  [[ $(storage_code "$1") == 200 ]] || return 1
  ws_join "$P_REALTIME" "$REF.realtime.internal" "$2" >/dev/null 2>&1
}
# tenant_gone KEY ANON: the pooler says the tenant is unknown, Storage and Realtime refuse it.
tenant_gone() {
  local port out
  for port in $P_SESSION $P_TRANSACTION; do
    out=$(pooler_login "$port") && { log "pooler port $port still serves $REF"; return 1; }
    grep -qi 'not found' <<<"$out" || { log "pooler port $port refused $REF for another reason: $out"; return 1; }
  done
  [[ $(storage_code "$1") != 200 ]] || { log "storage still serves $REF"; return 1; }
  if ws_join "$P_REALTIME" "$REF.realtime.internal" "$2" >/dev/null 2>&1; then log "realtime still serves $REF"; return 1; fi
}
wait_for() { # SECONDS CMD...: retries CMD every second
  local n=$1 i; shift
  for ((i = 0; i < n; i++)); do "$@" && return 0; sleep 1; done
  "$@"
}

log "key rotation through the engine: the tenants follow without ensure-tenant"
# The Management API has no rotation endpoint; `sbctl projects rotate-keys` runs the same Engine
# with the same lazily built fleet as the daemon does for a project create and delete.
OLDSEC=$SEC
sbctl projects rotate-keys "$REF" >/dev/null || fail "rotate-keys"
NEWSVC=$(project_field "$REF" 'd["keys"]["service_role_key"]' --show-keys)
NEWANON=$(project_field "$REF" 'd["keys"]["anon_key"]' --show-keys)
[[ $NEWSVC != "$SVC" ]] || fail "rotate-keys changed nothing"
[[ $(storage_code "$SVC") != 200 ]] || fail "storage accepts the old service key"
[[ $(storage_code "$NEWSVC") == 200 ]] || fail "storage rejects the new service key"
if ws_join "$P_REALTIME" "$REF.realtime.internal" "$ANON" >/dev/null 2>&1; then fail "realtime accepts the old anon key"; fi
ws_join "$P_REALTIME" "$REF.realtime.internal" "$NEWANON" || fail "realtime join with the new key"
for port in $P_SESSION $P_TRANSACTION; do
  pooler_login "$port" >/dev/null || fail "pooler port $port: login failed after the rotation"
done
# The daemon serves the rotated keys through the Management API and the proxy.
project_keys "$REF"
[[ $SEC != "$OLDSEC" ]] || fail "the API still lists the old secret key"
rotated_proxy() { curl -fsS -m 30 -H "Host: $HOST" -H "apikey: $SEC" -H "Authorization: Bearer $SEC" "http://127.0.0.1/storage/v1/bucket" >/dev/null 2>&1; }
wait_for 20 rotated_proxy || fail "the proxy refuses the new secret key"
[[ $(http_code -H "Host: $HOST" -H "apikey: $OLDSEC" -H "Authorization: Bearer $OLDSEC" "http://127.0.0.1/storage/v1/bucket") != 200 ]] || fail "the proxy accepts the old secret key"

log "crash recovery: kill -9 each shared service"
for svc in supavisor realtime storage pgmeta; do
  u="sb-$svc.service"
  pid=$(systemctl show -p MainPID --value "$u")
  [[ $pid -gt 0 ]] || fail "$u has no main pid"
  kill -9 "$pid"
  ok=0
  for ((i = 0; i < 90; i++)); do
    sleep 1
    new=$(systemctl show -p MainPID --value "$u")
    if [[ $new -gt 0 && $new != "$pid" && $(unit_state "$u") == active ]] && sbctl fleet status >/dev/null 2>&1; then ok=1; break; fi
  done
  [[ $ok -eq 1 ]] || fail "$u did not come back healthy after kill -9"
  log "$u recovered"
done
# The tenants live in the services' databases, so they survive the restarts.
wait_for 30 tenant_served "$NEWSVC" "$NEWANON" || fail "the tenant was lost in the restarts"

fleet_memory "one project registered, after the crash recovery"

log "CLI tenant commands, a separate step: remove-tenant, then ensure-tenant again"
sbctl fleet remove-tenant "$REF" || fail "remove-tenant"
wait_for 20 tenant_gone "$NEWSVC" "$NEWANON" || fail "remove-tenant left the tenant in a service"
sbctl fleet ensure-tenant "$REF" || fail "ensure-tenant"
wait_for 30 tenant_served "$NEWSVC" "$NEWANON" || fail "ensure-tenant did not register the tenant again"

log "delete through the Management API: the daemon's engine removes the tenants (no remove-tenant before)"
papi DELETE "/v1/projects/$REF" -o /dev/null -m 900 || fail "project delete through the API"
for svc in postgres gotrue postgrest; do
  [[ $(unit_state "sb-$svc@$REF.service") == inactive ]] || fail "sb-$svc@$REF is $(unit_state "sb-$svc@$REF.service") after the delete"
done
wait_for 20 tenant_gone "$NEWSVC" "$NEWANON" || fail "the project delete left the tenant in a service"
sbctl fleet remove-tenant "$REF" || fail "remove-tenant of a deleted project must be a no-op"

log "fleet stop"
systemctl stop sbctl.service
sbctl fleet stop || fail "fleet stop"
for svc in pgmeta supavisor realtime storage; do
  [[ $(unit_state "sb-$svc.service") == inactive ]] || fail "sb-$svc is $(unit_state "sb-$svc.service") after fleet stop"
done
[[ $(unit_state sb-postgres@system.service) == active ]] || fail "fleet stop touched the system cluster"
log "fleet smoke test passed"
