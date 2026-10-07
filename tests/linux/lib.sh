#!/usr/bin/env bash
# Shared helpers for tests/linux/*.sh. Sourced, not executed.
#
# These scripts are meant for an ephemeral Ubuntu 24.04 (or Debian 12) CI VM with
# systemd and sudo. They create a system user, install units under /etc/systemd/system,
# and start real PostgreSQL clusters. Do not run them on a machine you care about.

set -euo pipefail

REPO_ROOT=${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
SUPAVISE_BIN=${SUPAVISE_BIN:-}            # a linux supavise binary; built from REPO_ROOT when empty
SUPAVISE_USER=supavise
SUPAVISE_STATE=/var/lib/supavise
SUPAVISE_CONF=/etc/supavise/config.toml
SUPAVISE_DOMAIN=${SUPAVISE_DOMAIN:-supavise.test}
LOG_DIR=${LOG_DIR:-/tmp/supavise-linux-logs}

log()  { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

need_root() { [[ $(id -u) -eq 0 ]] || fail "run as root (sudo)"; }

preflight() {
  need_root
  [[ -d /run/systemd/system ]] || fail "systemd is not the init system here"
  [[ -f /sys/fs/cgroup/cgroup.controllers ]] || fail "cgroup v2 is required"
  for c in curl python3 systemctl; do command -v "$c" >/dev/null || fail "missing $c"; done
  mkdir -p "$LOG_DIR"
  {
    echo "date: $(date -u +%FT%TZ)"
    echo "kernel: $(uname -srm)"
    echo "os: $(. /etc/os-release && echo "$PRETTY_NAME")"
    echo "glibc: $(ldd --version | head -1)"
    echo "cpus: $(nproc)"
    echo "mem_total_mb: $(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)"
    echo "disk_free_gb: $(df -BG --output=avail /var/lib | tail -1 | tr -dc 0-9)"
  } | tee "$LOG_DIR/host.txt" >&2
}

# install_binary: put supavise at /usr/local/bin/supavise (build it when SUPAVISE_BIN is not set).
install_binary() {
  if [[ -z "$SUPAVISE_BIN" ]]; then
    command -v go >/dev/null || fail "no SUPAVISE_BIN and no go toolchain to build one"
    log "building supavise"
    (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -o /tmp/supavise-linux-test ./cmd/supavise)
    SUPAVISE_BIN=/tmp/supavise-linux-test
  fi
  install -m 0755 "$SUPAVISE_BIN" /usr/local/bin/supavise
  /usr/local/bin/supavise --version >&2 || true
}

# setup_node: user, directories, config, units. Safe to run twice.
setup_node() {
  # The supavise user drives systemd over D-Bus; that needs polkit and the rule that
  # `supavise system install-units` installs.
  if ! command -v pkaction >/dev/null; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq || log "apt-get update failed; trying the install anyway"
    apt-get install -y polkitd pkexec \
      || apt-get install -y policykit-1 \
      || fail "polkit is not installed and could not be installed"
  fi
  id "$SUPAVISE_USER" >/dev/null 2>&1 || useradd --system --home-dir "$SUPAVISE_STATE" --shell /usr/sbin/nologin "$SUPAVISE_USER"
  install -d -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" -m 0750 "$SUPAVISE_STATE" /etc/supavise
  cat >"$SUPAVISE_CONF" <<CONF
domain = "$SUPAVISE_DOMAIN"
supervisor = "systemd"
log_level = "info"
bin_path = "/usr/local/bin/supavise"

[tls]
mode = "off"
CONF
  chown "$SUPAVISE_USER:$SUPAVISE_USER" "$SUPAVISE_CONF"
  chmod 0640 "$SUPAVISE_CONF"
  /usr/local/bin/supavise system install-units
  systemctl daemon-reload
}

# supavise: run the CLI as the supavise user, which owns the state directory and is the only
# user the polkit rule lets manage the supavise-* units.
supavise() { sudo -u "$SUPAVISE_USER" -H /usr/local/bin/supavise "$@"; }

# system_init: fetch artifacts and create the system project.
system_init() { supavise system init; }

# json_get FILE_OR_STDIN PYEXPR: evaluate a Python expression over the parsed JSON "d".
json_get() { python3 -c 'import json,sys; d=json.load(sys.stdin); print('"$1"')'; }

# create_project NAME CLASS: prints the ref.
create_project() {
  supavise projects create --name "$1" --class "${2:-default}" --json | json_get 'd["ref"]'
}

# project_field REF PYEXPR [--show-keys]
project_field() {
  local ref=$1 expr=$2; shift 2
  supavise projects get "$ref" --json "$@" | json_get "$expr"
}

unit_state() { systemctl show -p ActiveState --value "$1"; }

wait_active() { # UNIT SECONDS
  local u=$1 n=${2:-30}
  for ((i = 0; i < n; i++)); do
    [[ $(unit_state "$u") == active ]] && return 0
    sleep 1
  done
  fail "$u is $(unit_state "$u") after ${n}s"
}

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$@" || true; }

# ---- the daemon's Management API ------------------------------------------------------
# The proxy listens on 127.0.0.1:80 (config tls.mode off); every name is reached with a Host header.
API_HOST="api.$SUPAVISE_DOMAIN"
api() { # METHOD PATH [curl args...]: the Management API through the proxy
  local m=$1 p=$2; shift 2
  curl -sS -m 120 -X "$m" -H "Host: $API_HOST" "$@" "http://127.0.0.1$p"
}

# claim_and_token: claims the node with the install-style token (an admin and an organization),
# signs in and creates a personal access token. Sets PAT and ORG for papi.
claim_and_token() {
  local tok jwt
  tok=$(supavise claim token 2>/dev/null) || fail "supavise claim token"
  [[ $(api POST /claim -H 'Content-Type: application/json' \
      -d "{\"token\":\"$tok\",\"email\":\"smoke@example.com\",\"password\":\"smoke-correct-horse-battery\",\"organization_name\":\"Smoke\"}" \
      -o /dev/null -w '%{http_code}') == 201 ]] || fail "claim with the token failed"
  jwt=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
    -d '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' | json_get 'd["access_token"]') || fail "dashboard sign-in"
  PAT=$(api POST /platform/profile/access-tokens -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' -d '{"name":"smoke"}' | json_get 'd["token"]') \
    || fail "creating a personal access token"
  [[ $PAT == sbp_* ]] || fail "personal access token: $PAT"
  ORG=$(papi GET /v1/organizations | json_get 'd[0]["slug"]') || fail "listing organizations with the token"
}
papi() { local m=$1 p=$2; shift 2; api "$m" "$p" -H "Authorization: Bearer $PAT" "$@"; }

# gen_dbpass: sets DBPASS (the database password a project is created with). It cannot be set
# inside api_create_project, which runs in a command substitution.
gen_dbpass() { DBPASS=Smoke-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n'); }

# api_create_project NAME: creates a project with the password in DBPASS through POST
# /v1/projects, waits until it is ACTIVE_HEALTHY and prints its ref.
api_create_project() {
  local name=$1 ref status="" i
  ref=$(papi POST /v1/projects -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"organization_slug\":\"$ORG\",\"db_pass\":\"$DBPASS\",\"region\":\"us-east-1\"}" | json_get 'd["ref"]') || fail "creating project $name through the API"
  [[ $ref =~ ^[a-z]{20}$ ]] || fail "project ref: $ref"
  for ((i = 0; i < 180; i++)); do
    status=$(papi GET "/v1/projects/$ref" | json_get 'd["status"]' 2>/dev/null || true)
    [[ $status == ACTIVE_HEALTHY || $status == INIT_FAILED ]] && break
    sleep 3
  done
  [[ $status == ACTIVE_HEALTHY ]] || { journalctl --no-pager -u supavise.service -n 60 >&2; fail "project $ref is $status"; }
  echo "$ref"
}

# project_keys REF: sets PUB (publishable key) and SEC (secret key) of an API project.
project_keys() {
  local k
  k=$(papi GET "/v1/projects/$1/api-keys?reveal=true")
  PUB=$(printf '%s' "$k" | json_get '[x["api_key"] for x in d if str(x.get("api_key","")).startswith("sb_publishable_")][0]') || fail "no publishable key: $k"
  SEC=$(printf '%s' "$k" | json_get '[x["api_key"] for x in d if str(x.get("api_key","")).startswith("sb_secret_")][0]') || fail "no secret key: $k"
}

# SMOKE_TABLE_SQL creates the table the REST and pooler checks read.
SMOKE_TABLE_SQL="create table public.smoke_items (id int primary key, label text); insert into public.smoke_items values (1, 'one'), (2, 'two'); grant select on public.smoke_items to anon; notify pgrst, 'reload schema';"

# rest_through_proxy REF PUB [api]: reads public.smoke_items with the publishable key through
# the proxy (the table is created first through the Management API's database/query, which needs
# postgres-meta, when the third argument is "api"; otherwise the caller created it).
rest_through_proxy() {
  local ref=$1 pub=$2 how=${3:-} n="" body="" i
  if [[ $how == api ]]; then
    papi POST "/v1/projects/$ref/database/query" -H 'Content-Type: application/json' \
      -d "$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$SMOKE_TABLE_SQL")" >/dev/null || fail "$ref: create table through database/query"
  fi
  for ((i = 0; i < 30; i++)); do
    body=$(curl -sS -m 30 -H "Host: $ref.api.$SUPAVISE_DOMAIN" -H "apikey: $pub" "http://127.0.0.1/rest/v1/smoke_items?select=id" || true)
    n=$(printf '%s' "$body" | json_get 'len(d) if isinstance(d, list) else -1' 2>/dev/null || true)
    [[ $n == 2 ]] && return 0
    sleep 2
  done
  fail "$ref: REST through the proxy returned $body, want 2 rows"
}

# ---- cloud metadata (IMDS) --------------------------------------------------------------
# The EC2 instance role must not be reachable from the units that run tenant code. CI VMs have
# no instance metadata service, so a mock listens on the metadata addresses (added to lo, on a
# high port: the daemon owns :80); IPAddressDeny matches the destination address, not the port.
# imds_blocked_in tests the real unit: it moves a curl into the unit's cgroup, where the BPF
# program of IPAddressDeny applies, and the same curl outside the unit must reach the mock.
IMDS_PORT=38169
IMDS_DIR=/tmp/supavise-imds-mock
imds_up() {
  mkdir -p "$IMDS_DIR"; echo imds-role-credentials >"$IMDS_DIR/index.html"
  ip addr add 169.254.169.254/32 dev lo 2>/dev/null || true
  ip -6 addr add fd00:ec2::254/128 dev lo nodad 2>/dev/null || true
  (cd "$IMDS_DIR" && nohup python3 -m http.server "$IMDS_PORT" --bind 169.254.169.254 >"$IMDS_DIR/v4.log" 2>&1 & echo $! >"$IMDS_DIR/v4.pid")
  (cd "$IMDS_DIR" && nohup python3 -m http.server "$IMDS_PORT" --bind fd00:ec2::254 >"$IMDS_DIR/v6.log" 2>&1 & echo $! >"$IMDS_DIR/v6.pid")
  local i
  for ((i = 0; i < 20; i++)); do
    curl -fsS -m 2 -o /dev/null "http://169.254.169.254:$IMDS_PORT/" && curl -fsS -m 2 -g -o /dev/null "http://[fd00:ec2::254]:$IMDS_PORT/" && return 0
    sleep 0.5
  done
  cat "$IMDS_DIR"/*.log >&2 || true
  fail "the metadata mock does not answer from outside the units"
}
imds_down() {
  for f in v4 v6; do [[ -f $IMDS_DIR/$f.pid ]] && kill "$(cat "$IMDS_DIR/$f.pid")" 2>/dev/null || true; done
  ip addr del 169.254.169.254/32 dev lo 2>/dev/null || true
  ip -6 addr del fd00:ec2::254/128 dev lo 2>/dev/null || true
  rm -rf "$IMDS_DIR"
}
imds_blocked_in() { # UNIT: fails unless the unit's cgroup cannot reach the mock on either address
  local unit=$1 cg url
  cg=$(systemctl show -p ControlGroup --value "$unit")
  [[ -n $cg && -w /sys/fs/cgroup$cg/cgroup.procs ]] || fail "$unit: no writable cgroup ($cg)"
  for url in "http://169.254.169.254:$IMDS_PORT/" "http://[fd00:ec2::254]:$IMDS_PORT/"; do
    curl -fsS -m 3 -g -o /dev/null "$url" || fail "the mock does not answer outside $unit ($url)"
    if bash -c 'echo $$ >"$1" && exec curl -fsS -m 3 -g -o /dev/null "$2"' _ "/sys/fs/cgroup$cg/cgroup.procs" "$url" 2>/dev/null; then
      fail "$unit can reach the instance metadata service at $url"
    fi
  done
  log "$unit: the instance metadata service is not reachable (IPv4 and IPv6)"
}
imds_denied_by_unit() { # UNIT: the unit file denies both metadata addresses
  local d
  d=$(systemctl show -p IPAddressDeny --value "$1")
  [[ $d == *169.254.169.254* && $d == *fd00:ec2::254* ]] || fail "$1: IPAddressDeny is '$d'"
}

# mint_dashboard_jwt SECRET: a dashboard session as GoTrue issues it (HS256 with the system
# project's secret, aud "authenticated", app_metadata.supavise_admin), for the Management API.
mint_dashboard_jwt() {
  python3 - "$1" <<'PY'
import base64, hashlib, hmac, json, sys, time
def b64(x): return base64.urlsafe_b64encode(x).rstrip(b"=")
head = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
body = b64(json.dumps({"aud": "authenticated", "sub": "00000000-0000-4000-8000-000000000001",
                       "email": "smoke@example.test", "role": "", "app_metadata": {"supavise_admin": True},
                       "exp": int(time.time()) + 1800}).encode())
sig = b64(hmac.new(sys.argv[1].encode(), head + b"." + body, hashlib.sha256).digest())
print((head + b"." + body + b"." + sig).decode())
PY
}

collect_logs() {
  mkdir -p "$LOG_DIR"
  journalctl --no-pager -o short-iso -u 'supavise-*' -u supavise.service >"$LOG_DIR/journal.log" 2>&1 || true
  systemctl list-units --all --no-pager 'supavise-*' 'supavise*' >"$LOG_DIR/units.txt" 2>&1 || true
  systemctl status --no-pager supavise.slice >"$LOG_DIR/slice.txt" 2>&1 || true
  df -h /var/lib >"$LOG_DIR/df.txt" 2>&1 || true
  log "logs in $LOG_DIR"
}

teardown() {
  imds_down 2>/dev/null || true
  systemctl stop 'supavise-*' 2>/dev/null || true
}

# ws_join PORT HOST ANON: join a Realtime channel over a WebSocket (tenant from the Host header,
# the project's anon key as token); exit 0 when the server answers phx_join with status ok.
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

# cron_runs SQLFN REF JOB [STATUS]: how many runs of the pg_cron job JOB cron.job_run_details
# lists for project REF (with STATUS, when given). SQLFN is the script's "run SQL on REF" function
# (REF SQL; prints tuples only).
cron_runs() {
  local run=$1 ref=$2 job=$3 status=${4:-}
  "$run" "$ref" "select count(*) from cron.job_run_details d join cron.job j using (jobid) where j.jobname = '$job' ${status:+and d.status = '$status'}"
}

# wait_cron_success SQLFN REF JOB SECONDS [AFTER]: waits until cron.job_run_details shows more than
# AFTER (default 0) succeeded runs of JOB; a clone of a project carries its parent's run history,
# so a branch passes the count it started with. pg_cron on a cluster whose jobs cannot connect (libpq, a pg_hba.conf that trusts no
# loopback connection) records every run as failed with "connection failed"; the last rows are
# printed when the wait times out.
wait_cron_success() {
  local run=$1 ref=$2 job=$3 n=${4:-120} after=${5:-0} i c
  for ((i = 0; i < n; i += 2)); do
    c=$(cron_runs "$run" "$ref" "$job" succeeded 2>/dev/null) || c=0
    [[ $c =~ ^[0-9]+$ && $c -gt $after ]] && return 0
    sleep 2
  done
  "$run" "$ref" "select d.status, d.return_message from cron.job_run_details d join cron.job j using (jobid) where j.jobname = '$job' order by d.runid desc limit 3" >&2 || true
  return 1
}

# assert_os_updates on|off: what `supavise system os-updates` leaves on a Ubuntu or Debian host.
# On: unattended-upgrades is installed and reads only the security origins (checked through
# apt-config and through the program's own "Allowed origins" line), never reboots by itself, and
# needrestart is told to leave supavise-* units alone. Off: Supavise's files are gone.
assert_os_updates() { # on|off
  if [[ $1 == on ]]; then
    [[ -f /etc/apt/apt.conf.d/52supavise-unattended-upgrades ]] || fail "the apt configuration for unattended security updates is missing"
    dpkg -s unattended-upgrades >/dev/null 2>&1 || fail "unattended-upgrades is not installed"
    local cfg pats origins l
    cfg=$(apt-config dump)
    # Only security origins, whichever list the distribution's own file used (we clear both).
    [[ -z $(grep '^Unattended-Upgrade::Allowed-Origins::' <<<"$cfg") ]] || fail "Allowed-Origins still lists origins: $(grep '^Unattended-Upgrade::Allowed-Origins::' <<<"$cfg")"
    pats=$(grep '^Unattended-Upgrade::Origins-Pattern::' <<<"$cfg") || fail "no Origins-Pattern entries: $cfg"
    while IFS= read -r l; do [[ $l == *security* ]] || fail "an origin that is not a security origin: $l"; done <<<"$pats"
    grep -q '^Unattended-Upgrade::Automatic-Reboot "false";' <<<"$cfg" || fail "unattended-upgrades may reboot by itself"
    grep -q '^APT::Periodic::Unattended-Upgrade "1";' <<<"$cfg" || fail "unattended upgrades are not switched on"
    # What the program itself reads from that configuration.
    origins=$(unattended-upgrade --dry-run --debug 2>&1 || true)
    origins=$(grep -m1 'Allowed origins are' <<<"$origins") || fail "unattended-upgrade printed no allowed origins"
    [[ $origins == *security* && $origins != *-updates* && $origins != *backports* ]] || fail "unattended-upgrade would take more than security updates: $origins"
    [[ ! -d /etc/needrestart/conf.d || -f /etc/needrestart/conf.d/50-supavise.conf ]] || fail "needrestart may restart supavise units"
  else
    [[ ! -e /etc/apt/apt.conf.d/52supavise-unattended-upgrades ]] || fail "the apt configuration is still there"
    [[ ! -e /etc/needrestart/conf.d/50-supavise.conf ]] || fail "the needrestart drop-in is still there"
  fi
}

