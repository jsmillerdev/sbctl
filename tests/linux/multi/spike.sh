#!/usr/bin/env bash
# Two-node harness spike (design S9): two Incus nodes with a real systemd on one runner, and what the
# replication tests need from them.
#
#   CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.1" -o /tmp/supavise ./cmd/supavise
#   go build -o /tmp/releasetool ./deploy/releasetool
#   sudo MULTI_MODE=container SUPAVISE_BIN=/tmp/supavise SUPAVISE_RELEASETOOL=/tmp/releasetool tests/linux/multi/spike.sh
#
# MULTI_MODE is vm, container, container-privileged or auto (a virtual machine when /dev/kvm works). The
# checks, in order, each recorded as PASS, FAIL or SKIP (a check whose predecessor failed) in
# $LOG_DIR/results.md:
#
#   incus      Incus installed and initialised, Docker's forwarding rules opened for the bridge
#   services   Garage, the signed release server and the fake instance metadata/EC2 service on the bridge
#   launch     both nodes created and started
#   boot       systemd is PID 1 and finished starting, cgroup v2, address as planned
#   prep       curl, sudo, python3 and the rest of what the install needs, from apt
#   net        each node reaches the internet and the three services on the bridge
#   peer       the nodes reach each other on port 7443, both ways, with the throughput
#   install    deploy/install.sh --binary of this checkout's build, the shared services active, Garage as the
#              backup backend
#   polkit     the supavise user restarts a supavise unit, and is refused daemon-reload and other units
#   hardening  the unit's mount namespace hides /etc/supavise
#   imds       lib.sh's metadata mock inside the node: IPAddressDeny holds for each unit that carries it
#   project    a project created and healthy, a base backup written to and listed from Garage
#   stub       install.sh of the signed release, fetched from the release server, verifies the release
#   fakeaws    IMDSv2, EC2 and Secrets Manager calls to the fake service answer with the node's identity
#
# SPIKE_STRICT=0 records the failures and still exits 0 (a mode that is known not to work is run this way).
# Needs root and Ubuntu 24.04 with Docker; see lib-multi.sh. Logs, results.md and the per-check output are
# in $LOG_DIR (default /tmp/supavise-multi-logs).
# shellcheck source=lib-multi.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-multi.sh"

SPIKE_STRICT=${SPIKE_STRICT:-1}
RESULTS=$LOG_DIR/results.tsv
FACTS=$LOG_DIR/facts.tsv
CHECK_ORDER="incus services launch boot prep net peer install polkit hardening imds project stub fakeaws fakeaws-log"
UNITS_IMDS=(supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service supavise-supavisor.service supavise-realtime.service supavise-storage.service)

# ---- results ----------------------------------------------------------------------------------------
note() { printf '%s\t%s\n' "$1" "$2" >>"$FACTS"; }                       # KEY VALUE
stamp() { printf '%s\t%d\n' "$1" $((SECONDS - ${2:-0})) >>"$LOG_DIR/timings.tsv"; } # LABEL SINCE
needs() { local i; for i in "$@"; do [[ -e $WORK/state/$i.ok ]] || { echo "# needs $i"; exit 125; }; done; }

# check ID DESCRIPTION CMD...: runs CMD in a subshell with errexit on; its output goes to checks/ID.log, its
# lines that start with "# " become the detail. Exit status 125 (needs) is a SKIP. Never stops the script.
check() {
  local id=$1 desc=$2 res detail="" rc=0 t0=$SECONDS f
  shift 2
  f=$LOG_DIR/checks/$id.log
  ( set -e; "$@" ) >"$f" 2>&1 & wait $! || rc=$?
  case $rc in
    0) res=PASS; touch "$WORK/state/$id.ok" ;;
    125) res=SKIP ;;
    *) res=FAIL ;;
  esac
  if [[ $res == FAIL ]]; then
    detail=$(grep -h 'FAIL:' "$f" | tail -n1 | cut -c1-300 || true)
    [[ -n $detail ]] || detail=$(tail -n1 "$f" | cut -c1-300)
  else
    detail=$(grep -h '^# ' "$f" | tail -n4 | sed 's/^# //' | paste -sd ';' - | cut -c1-300 || true)
  fi
  printf '%s\t%s\t%s\t%d\t%s\n' "$res" "$id" "$desc" $((SECONDS - t0)) "$detail" >>"$RESULTS"
  log "$res $id ($((SECONDS - t0))s): $desc${detail:+ -- $detail}"
  if [[ $res == FAIL ]]; then echo "--- end of $id.log" >&2; tail -n 30 "$f" >&2; fi
  return 0
}

check_each() { # ID DESCRIPTION FUNC: FUNC NODE on every node at once
  local id=$1 desc=$2 fn=$3 n p pids=()
  for n in "${NODES[@]}"; do
    check "$id@$n" "$desc ($n)" "$fn" "$n" &
    pids+=($!)
  done
  for p in "${pids[@]}"; do wait "$p" || true; done
}

write_results() {
  local f=$LOG_DIR/results.md
  {
    echo "## Two-node harness: $(dpkg --print-architecture), nodes as $MULTI_RESOLVED"
    echo
    echo "| Check | Result | Seconds | Detail |"
    echo "|---|---|---|---|"
    awk -F'\t' -v order="$CHECK_ORDER" 'BEGIN {n = split(order, o, " "); for (i = 1; i <= n; i++) idx[o[i]] = i}
      {id = $2; sub(/@.*/, "", id); print idx[id] "\t" $0}' "$RESULTS" | sort -s -t "$(printf '\t')" -k1,1n -k3,3 | cut -f2- \
      | awk -F'\t' '{gsub(/\|/, "/", $5); printf "| %s: %s | %s | %s | %s |\n", $2, $3, $1, $4, $5}'
    echo
    echo "| Fact | Value |"
    echo "|---|---|"
    awk -F'\t' '{gsub(/\|/, "/", $2); printf "| %s | %s |\n", $1, $2}' "$FACTS"
    echo
    echo "| Step | Seconds |"
    echo "|---|---|"
    awk -F'\t' '{printf "| %s | %s |\n", $1, $2}' "$LOG_DIR/timings.tsv"
    echo
    echo "| When | Who | Used MB | Detail |"
    echo "|---|---|---|---|"
    awk -F'\t' '{printf "| %s | %s | %s | %s |\n", $1, $2, $3, $4}' "$LOG_DIR/memory.tsv"
  } >"$f" 2>/dev/null
}

finish() {
  local rc=$?
  trap - EXIT
  mem_snapshot end 2>/dev/null || true
  multi_collect_logs
  write_results || true
  cat "$LOG_DIR/results.md" >&2 2>/dev/null || true
  multi_down
  if [[ $SPIKE_STRICT == 1 && $rc -eq 0 ]] && grep -q '^FAIL' "$RESULTS" 2>/dev/null; then rc=1; fi
  exit $rc
}

# ---- the checks ---------------------------------------------------------------------------------------
c_incus() {
  [[ $MULTI_RESOLVED != vm ]] || kvm_usable || fail "virtual machines need a usable /dev/kvm: $(ls -l /dev/kvm 2>&1)"
  local t0=$SECONDS
  multi_incus_install
  stamp "host: apt install incus" "$t0"
  multi_incus_init
  multi_docker_rules
  note incus.version "$(incus --version)"
  echo "# incus $(incus --version), nodes as $MULTI_RESOLVED"
}

c_services() {
  needs incus
  local t0=$SECONDS
  garage_up
  release_server_up v0.0.1 "$SUPAVISE_BIN"
  fake_aws_up
  stamp "host: Garage, release server, fake AWS" "$t0"
  note garage.image "$GARAGE_IMAGE"
  echo "# Garage :$S3_PORT, release server :$RELEASE_PORT, fake AWS :$AWS_PORT on $BRIDGE_IP"
}

c_launch() {
  needs incus
  local n t0
  for n in "${NODES[@]}"; do
    t0=$SECONDS
    multi_launch_node "$n"
    echo "$SECONDS" >"$WORK/state/started-$n"
    stamp "$n: incus init and start ($MULTI_RESOLVED)" "$t0"
  done
  note image "$(incus image list --format csv -c fdast | head -n1)"
}

c_boot() {
  local n=$1 st t0 free
  needs launch
  t0=$(cat "$WORK/state/started-$n")
  st=$(multi_wait "$n")
  stamp "$n: start to systemd finished starting" "$t0"
  note "$n.systemd" "$st"
  note "$n.failed-units" "$(on "$n" systemctl --failed --no-legend --plain | awk '{print $1}' | paste -sd ' ' - || true)"
  [[ $st == running || $st == degraded ]] || fail "$n: systemd is '$st'"
  [[ $(on "$n" cat /proc/1/comm) == systemd ]] || fail "$n: PID 1 is not systemd"
  on "$n" test -f /sys/fs/cgroup/cgroup.controllers || fail "$n: no cgroup v2"
  note "$n.os" "$(on "$n" bash -c '. /etc/os-release; echo "$PRETTY_NAME"')"
  note "$n.kernel" "$(on "$n" uname -r)"
  note "$n.virt" "$(on "$n" systemd-detect-virt || true)"
  note "$n.cpus" "$(on "$n" nproc)"
  note "$n.mem_total_mb" "$(on "$n" free -m | awk '/^Mem:/ {print $2}')"
  free=$(on "$n" df -BG --output=avail / | tail -n1 | tr -dc 0-9)
  if [[ $MULTI_RESOLVED == vm && $free -lt 10 ]]; then
    multi_grow_root "$n" || echo "# the root file system could not be grown"
    free=$(on "$n" df -BG --output=avail / | tail -n1 | tr -dc 0-9)
  fi
  note "$n.root_free_gb" "$free"
  [[ $free -ge 8 ]] || fail "$n: $free GB free on the root file system"
  [[ $(on "$n" ip -4 route get "$BRIDGE_IP" | grep -o 'src [0-9.]*' | cut -d' ' -f2) == "$(node_ip "$n")" ]] \
    || fail "$n does not have the address $(node_ip "$n")"
  echo "# systemd $st, $(on "$n" bash -c '. /etc/os-release; echo "$PRETTY_NAME"'), $(node_ip "$n")"
}

c_prep() {
  local n=$1 t0=$SECONDS
  needs "boot@$n"
  multi_prep_node "$n"
  stamp "$n: apt packages" "$t0"
}

c_net() {
  local n=$1 code
  needs "prep@$n" services
  code=$(on "$n" curl -sL -o /dev/null -w '%{http_code}' -m 30 https://github.com/supabase/slim-services) || true
  [[ $code == 200 ]] || fail "$n: github.com answered '$code'"
  code=$(on "$n" curl -s -o /dev/null -w '%{http_code}' -m 10 "http://$BRIDGE_IP:$S3_PORT/") || true
  [[ $code =~ ^(200|400|403|404)$ ]] || fail "$n: Garage on the bridge answered '$code'"
  code=$(on "$n" curl -s -o /dev/null -w '%{http_code}' -m 10 "http://$BRIDGE_IP:$RELEASE_PORT/download/v0.0.1/SHA256SUMS") || true
  [[ $code == 200 ]] || fail "$n: the release server on the bridge answered '$code'"
  code=$(on "$n" curl -s -o /dev/null -w '%{http_code}' -m 10 "http://$BRIDGE_IP:$AWS_PORT/_calls") || true
  [[ $code == 200 ]] || fail "$n: the fake AWS service on the bridge answered '$code'"
  note "$n.rtt_ms_to_bridge" "$(on "$n" ping -c 5 -q "$BRIDGE_IP" | awk -F/ '/^rtt/ {print $5}')"
  echo "# internet, S3, release server and fake AWS reachable"
}

c_peer() {
  needs prep@n1 prep@n2
  local n pair a b out ack secs bytes=$((256 * 1048576))
  for n in "${NODES[@]}"; do peer_listen "$n"; done
  sleep 1
  for pair in "n1 n2" "n2 n1"; do
    read -r a b <<<"$pair"
    out=$(peer_send "$a" "$(node_ip "$b")" 0)
    [[ ${out%% *} == 0 ]] || fail "$a -> $b:$PEER_PORT: '$out'"
    out=$(peer_send "$a" "$(node_ip "$b")" "$bytes")
    ack=${out%% *} secs=${out##* }
    [[ $ack == "$bytes" ]] || fail "$a -> $b:$PEER_PORT carried $ack of $bytes bytes"
    note "peer.$a-$b.mb_per_s" "$(awk -v b="$ack" -v s="$secs" 'BEGIN {printf "%.0f", b / 1048576 / s}')"
  done
  note "peer.rtt_ms_n1-n2" "$(on n1 ping -c 5 -q "$(node_ip n2)" | awk -F/ '/^rtt/ {print $5}')"
  note peer.dns "$(on n1 getent hosts n2.incus || echo none)"
  for n in "${NODES[@]}"; do on "$n" systemctl stop peer-sink.service; done
  echo "# n1 <-> n2 on $PEER_PORT"
}

c_install() {
  local n=$1 ip t0=$SECONDS d=$LOG_DIR/$1
  ip=$(node_ip "$n")
  needs "net@$n"
  mkdir -p "$d"
  node_push "$n" "$SUPAVISE_BIN" /root/supavise 0755
  node_push "$n" "$REPO_ROOT/deploy/install.sh" /root/install.sh 0755
  node_push "$n" "$REPO_ROOT/tests/linux/lib.sh" /root/lib.sh
  printf 'access_key_id=%s\nsecret_access_key=%s\n' "$S3_KEY_ID" "$S3_SECRET" | on "$n" tee /root/s3.cred >/dev/null
  on "$n" chmod 0600 /root/s3.cred
  stamp "$n: copy the binary into the node" "$t0"
  t0=$SECONDS
  on "$n" /root/install.sh --binary /root/supavise --public-ip "$ip" --tls off --email ci@example.com --firewall none \
    --no-studio --no-os-updates --claim-token-file /root/claim-token \
    --s3-endpoint "http://$BRIDGE_IP:$S3_PORT" --s3-bucket "$S3_BUCKET" --s3-prefix "$n" --s3-region us-east-1 \
    --s3-path-style --s3-credentials-file /root/s3.cred >"$d/install.log" 2>&1 \
    || { tail -n 40 "$d/install.log"; fail "$n: install.sh --binary failed"; }
  stamp "$n: install.sh --binary" "$t0"
  grep -q "Supavise is running" "$d/install.log" || fail "$n: install.sh did not report success"
  on "$n" test -s /root/claim-token || fail "$n: no claim token file"
  on "$n" bash -s -- "$ip" <<'EOS' || fail "$n: the node is not healthy after the install"
source /root/lib.sh
for u in supavise.service supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service \
    supavise-supavisor.service supavise-realtime.service supavise-storage.service; do wait_active "$u" 90; done
[[ $(systemctl show -p User --value supavise.service) == supavise ]] || fail "supavise.service does not run as supavise"
[[ $(http_code -H "Host: api.$1.sslip.io" http://127.0.0.1/claim) == 200 ]] || fail "the claim page does not answer through the proxy"
grep -q "^backend = 's3://" /etc/supavise/config.toml || fail "config.toml does not name an S3 backup backend: $(grep backend /etc/supavise/config.toml)"
EOS
  note "$n.version" "$(on "$n" /usr/local/bin/supavise --version | head -n1)"
  note "$n.polkit" "$(on "$n" pkaction --version)"
  echo "# $(on "$n" /usr/local/bin/supavise --version | head -n1) installed, S3 backend $BRIDGE_IP:$S3_PORT"
}

c_polkit() {
  local n=$1
  needs "install@$n"
  on "$n" bash -s <<'EOS' || fail "$n: polkit does not behave as the rule says"
source /root/lib.sh
as() { runuser -u supavise -- "$@"; }
as systemctl --no-ask-password restart supavise-pgmeta.service || fail "the supavise user cannot restart a supavise unit"
wait_active supavise-pgmeta.service 60
if as systemctl --no-ask-password daemon-reload 2>/dev/null; then fail "the supavise user can run daemon-reload"; fi
if as systemctl --no-ask-password restart systemd-journald.service 2>/dev/null; then fail "the supavise user can restart a unit that is not Supavise's"; fi
if as systemctl --no-ask-password stop supavise-upgrade.service 2>/dev/null; then fail "the supavise user can stop supavise-upgrade.service"; fi
EOS
  echo "# restart of a supavise unit allowed; daemon-reload, a foreign unit and supavise-upgrade refused"
}

c_hardening() {
  local n=$1
  needs "install@$n"
  on "$n" bash -s <<'EOS' || fail "$n: the unit's sandbox is not what the unit file says"
source /root/lib.sh
u=supavise-postgres@system.service
pid=$(systemctl show -p MainPID --value "$u")
[[ $pid -gt 0 ]] || fail "$u has no main pid"
sees() { nsenter -t "$pid" -m -- runuser -u supavise -- test -r "$1" 2>/dev/null; }
sees /var/lib/supavise/projects/system/postgres || fail "$u cannot read its own cluster directory"
if sees /etc/supavise/master.key; then fail "$u can read /etc/supavise/master.key"; fi
if sees /var/lib/supavise/backups; then fail "$u can read the backups directory"; fi
systemd-analyze security --no-pager "$u" | tail -n 1 | sed 's/^/# /' >&2 || true
EOS
  note "$n.exposure" "$(on "$n" systemd-analyze security --no-pager supavise-postgres@system.service | tail -n1 | sed 's/^[^:]*: *//' | LC_ALL=C tr -cd '\11\12\15\40-\176' || true)"
  echo "# mount namespace hides /etc/supavise and the backups from supavise-postgres@system"
}

c_imds() {
  local n=$1
  needs "install@$n"
  # What the kernel and systemd can do for IPAddressDeny, recorded whether or not the check passes.
  note "$n.ip_accounting" "$(on "$n" systemctl show -p IPAccounting -p IPIngressBytes supavise-postgres@system.service | paste -sd ' ' - || true)"
  note "$n.bpf_messages" "$(on "$n" journalctl -b --no-pager -q | grep -i -c -E 'bpf|ip firewall' || true)"
  on "$n" bash -s -- "${UNITS_IMDS[@]}" <<'EOS' || fail "$n: the instance metadata block does not hold"
source /root/lib.sh
imds_up
trap imds_down EXIT
for u in "$@"; do
  imds_denied_by_unit "$u"
  imds_blocked_in "$u"
done
EOS
  echo "# IPAddressDeny blocks 169.254.169.254 and fd00:ec2::254 in ${#UNITS_IMDS[@]} units; the mock answers outside them"
}

c_project() {
  local n=$1
  needs "install@$n"
  on "$n" bash -s <<'EOS' || fail "$n: project or backup failed"
source /root/lib.sh
ref=$(create_project spike micro) || fail "supavise projects create"
[[ $ref =~ ^[a-z]{20}$ ]] || fail "project ref '$ref'"
supavise projects health "$ref" || fail "$ref: health"
for svc in postgres gotrue postgrest; do wait_active "supavise-$svc@$ref.service" 60; done
supavise backups create "$ref" --skip-files || fail "$ref: backups create"
listed=$(supavise backups list "$ref" --store) || fail "$ref: backups list --store"
[[ $(wc -l <<<"$listed") -ge 2 ]] || fail "$ref: the backend lists no base backup: $listed"
echo "# project $ref healthy; base backup stored in and listed from Garage"
EOS
}

c_stub() {
  local n=$1
  needs "net@$n"
  on "$n" bash -s -- "$BRIDGE_IP:$RELEASE_PORT" <<'EOS' || fail "$n: the signed release does not verify"
set -euo pipefail
curl -fsSL -o /tmp/install-stamped.sh "http://$1/download/v0.0.1/install.sh"
out=$(SUPAVISE_INSTALL_BASE_URL="http://$1" bash /tmp/install-stamped.sh --version v0.0.1 --verify-only)
echo "# $out"
[[ $out == *"verified v0.0.1"* ]]
EOS
}

c_fakeaws() {
  local n=$1 id
  needs "net@$n"
  id=$(node_instance_id "$n")
  on "$n" bash -s -- "$BRIDGE_IP:$AWS_PORT" "$id" <<'EOS' || fail "$n: the fake AWS service does not answer as it should"
set -euo pipefail
b=http://$1
tok=$(curl -fsS -m 5 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' "$b/latest/api/token")
[[ $(curl -fsS -m 5 -H "X-aws-ec2-metadata-token: $tok" "$b/latest/meta-data/instance-id") == "$2" ]]
[[ $(curl -fsS -m 5 -H "X-aws-ec2-metadata-token: $tok" "$b/latest/meta-data/tags/instance/supavise:cluster") == ci ]]
[[ $(curl -s -o /dev/null -w '%{http_code}' -m 5 "$b/latest/meta-data/instance-id") == 401 ]]
out=$(curl -fsS -m 5 -X POST -d 'Action=DescribeInstances&Version=2016-11-15' "$b/")
[[ $out == *"<instanceId>$2</instanceId>"* ]]
[[ $(curl -s -o /dev/null -w '%{http_code}' -m 5 -X POST -d "Action=StopInstances&InstanceId.1=$2&DryRun=true" "$b/") == 412 ]]
[[ $(curl -fsS -m 5 -X POST -H 'X-Amz-Target: secretsmanager.GetSecretValue' -d '{"SecretId":"supavise/join"}' "$b/") == *fake-join-token* ]]
EOS
  echo "# $id: IMDSv2 token, instance id, tags, DescribeInstances, a DryRun refusal, GetSecretValue"
}

c_fakeaws_log() {
  local n calls
  needs fakeaws@n1 fakeaws@n2
  for n in "${NODES[@]}"; do
    calls=$(grep -c "\"node\": \"$n\"" "$LOG_DIR/fake-aws.jsonl")
    [[ $calls -ge 6 ]] || fail "the fake AWS service logged $calls calls from $n, want 6 or more"
  done
  echo "# the call log names each node by its address"
}

# ---- run -------------------------------------------------------------------------------------------------
need_root
preflight
multi_init
trap finish EXIT
: >"$RESULTS"; : >"$FACTS"; : >"$LOG_DIR/timings.tsv"; : >"$LOG_DIR/memory.tsv"
[[ -x ${SUPAVISE_BIN:-} ]] || fail "SUPAVISE_BIN: a Linux build of this checkout, built with -X main.version=v0.0.1"
[[ $("$SUPAVISE_BIN" --version) == *v0.0.1* ]] || fail "SUPAVISE_BIN must report v0.0.1 (the release server signs it as v0.0.1): $("$SUPAVISE_BIN" --version)"
cd "$REPO_ROOT"

note runner.arch "$(dpkg --print-architecture)"
note runner.kernel "$(uname -r)"
note runner.cpus "$(nproc)"
note runner.mem_mb "$(free -m | awk '/^Mem:/ {print $2}')"
note runner.disk_free_gb "$(df -BG --output=avail / | tail -n1 | tr -dc 0-9)"
note runner.virt "$(lscpu | grep -i -E 'hypervisor vendor|virtualization' | tr -s ' ' | paste -sd ';' - || true)"
note kvm.device "$(ls -l /dev/kvm 2>&1 | tr -s ' ')"
if kvm_usable; then note kvm.usable_before_rule yes; else note kvm.usable_before_rule no; fi
[[ $MULTI_MODE == container* ]] || kvm_enable
if kvm_usable; then note kvm.usable yes; else note kvm.usable no; fi
multi_resolve_mode
note mode "$MULTI_RESOLVED"
mem_snapshot start 2>/dev/null || true

check incus "Incus is installed and the bridge is up" c_incus
check services "Garage, the release server and the fake AWS service listen on the bridge" c_services
check launch "both nodes are created and started" c_launch
check_each boot "systemd boots" c_boot
mem_snapshot booted
check_each prep "apt packages install" c_prep
check_each net "internet and bridge services are reachable" c_net
check peer "the nodes reach each other on port $PEER_PORT" c_peer
check_each install "install.sh --binary succeeds" c_install
mem_snapshot installed
check_each polkit "polkit lets the supavise user manage only its units" c_polkit
check_each hardening "the unit sandbox hides the node's secrets" c_hardening
check_each imds "the instance metadata block is enforced" c_imds
check_each project "a project runs and its base backup reaches S3" c_project
mem_snapshot project
check_each stub "the signed release verifies from the release server" c_stub
check_each fakeaws "the fake AWS service answers each node" c_fakeaws
check fakeaws-log "the fake AWS service saw each node" c_fakeaws_log
note garage.bucket "$(docker exec garage /garage bucket info "$S3_BUCKET" 2>&1 | grep -i -E 'objects|size' | paste -sd ' ' - || true)"
log "done in $SECONDS s"
