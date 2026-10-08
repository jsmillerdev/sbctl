#!/usr/bin/env bash
# Two-node harness spike (design S9): two Incus nodes with a real systemd on one runner, and what the
# replication tests need from them.
#
#   CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.1" -o /tmp/supavise ./cmd/supavise
#   go build -o /tmp/releasetool ./deploy/releasetool
#   sudo MULTI_MODE=container SUPAVISE_BIN=/tmp/supavise SUPAVISE_RELEASETOOL=/tmp/releasetool tests/linux/multi/spike.sh
#
# MULTI_MODE is vm, container, container-privileged or auto (a virtual machine when /dev/kvm works, else a
# privileged container). The
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
#   rules      a probe: with the two DOCKER-USER rules taken away, what the nodes can still reach
#   install    deploy/install.sh --binary of this checkout's build, the shared services active, Garage as the
#              backup backend
#   polkit     the supavise user restarts a supavise unit, and is refused daemon-reload and other units
#   hardening  the unit's mount namespace hides /etc/supavise
#   imds       lib.sh's metadata mock inside the node: IPAddressDeny holds for each unit that carries it
#   project    a project created and healthy, a base backup written to and listed from Garage
#   stub       install.sh of the signed release, fetched from the release server, verifies the release
#   fakeaws    IMDSv2, EC2 and Secrets Manager calls to the fake service answer with the node's identity
#   smoke      only with MULTI_SMOKE=1: tests/linux/systemd-smoke.sh on a third, fresh node of the same kind
#
# EXPECT_FAIL names the checks (without the @node) that are known to fail in the mode, each with the reason
# its failure gives: "imds=can reach the instance metadata service" for system containers that are not
# privileged. The script exits 0 when exactly those fail, each for its reason, and non-zero when another
# check fails, when one of them fails for another reason or when one of them passes (the README is then
# out of date). lib-checks.sh has the runner.
# Needs root and Ubuntu 24.04 with Docker; see lib-multi.sh. Logs, results.md and the per-check output are
# in $LOG_DIR (default /tmp/supavise-multi-logs).
# shellcheck source=lib-multi.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-multi.sh"
# shellcheck source=lib-checks.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-checks.sh"

RESULTS_TITLE="Two-node harness"
FACT_PREFIX=spike
CHECK_ORDER="incus services launch boot prep net peer rules install polkit hardening imds project stub fakeaws fakeaws-log smoke"
UNITS_IMDS=(supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service supavise-supavisor.service supavise-realtime.service supavise-storage.service)

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

# The nodes' traffic is forwarded by the host, and the runner image's Docker sets the FORWARD policy to DROP.
# Take the rules away for a moment and record what still works; they are put back whatever happens.
c_rules() {
  needs peer
  local out peer
  iptables -w -nL DOCKER-USER >/dev/null 2>&1 || { echo "# no DOCKER-USER chain on this host"; return 0; }
  note docker.forward_policy "$(iptables -w -S FORWARD | head -n1)"
  trap multi_docker_rules EXIT
  multi_docker_rules_remove
  out=$(on n1 curl -s -o /dev/null -w '%{http_code}' -m 8 https://github.com 2>/dev/null) || true
  peer_listen n2
  peer=$(peer_send n1 "$(node_ip n2)" 0 2>/dev/null) || peer=none
  on n2 systemctl stop peer-sink.service || true
  multi_docker_rules
  note rules.internet_without "${out:-000}"
  note rules.n1_to_n2_without "${peer:-none}"
  echo "# without the rules: internet from n1 answers '${out:-000}', n1 to n2 on $PEER_PORT answers '${peer:-none}'"
}

c_install() {
  local n=$1 ip t0=$SECONDS d=$LOG_DIR/$1
  ip=$(node_ip "$n")
  needs "net@$n"
  multi_install "$n" --claim-token-file /root/claim-token \
    || { tail -n 40 "$d/install.log"; fail "$n: install.sh --binary failed"; }
  stamp "$n: copy the binary into the node, install.sh --binary" "$t0"
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

# systemd-smoke.sh, unchanged, on a fresh node (it sets the node up itself, so it cannot run on n1 or n2).
c_smoke() {
  local n=$SMOKE_NODE
  needs install@n1 install@n2
  multi_launch_node "$n"
  multi_wait "$n" >/dev/null
  multi_prep_node "$n"
  node_push "$n" "$SUPAVISE_BIN" /root/supavise 0755
  tar -C "$REPO_ROOT" -cf - tests/linux/lib.sh tests/linux/systemd-smoke.sh | on "$n" tar -C /root -xf -
  on "$n" env SUPAVISE_BIN=/root/supavise LOG_DIR=/root/smoke-logs timeout 2400 bash /root/tests/linux/systemd-smoke.sh \
    >"$LOG_DIR/systemd-smoke.log" 2>&1 || { tail -n 40 "$LOG_DIR/systemd-smoke.log"; fail "systemd-smoke.sh failed on $n: $(grep 'FAIL:' "$LOG_DIR/systemd-smoke.log" | tail -n1 | cut -c1-200)"; }
  incus file pull --quiet -r "$n/root/smoke-logs" "$LOG_DIR/" || true
  echo "# $(tail -n 1 "$LOG_DIR/systemd-smoke.log" | cut -c1-200)"
}

# ---- run -------------------------------------------------------------------------------------------------
need_root
preflight
multi_init
trap finish EXIT
checks_init
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
check rules "what the nodes lose without Docker's forwarding rules (a probe)" c_rules
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
[[ ${MULTI_SMOKE:-0} != 1 ]] || check smoke "systemd-smoke.sh passes on a fresh node" c_smoke
note garage.bucket "$(docker exec garage /garage bucket info "$S3_BUCKET" 2>&1 | grep -i -E 'objects|size' | paste -sd ' ' - || true)"
for n in "${NODES[@]}"; do note "$n.root_used_mb_at_end" "$(on "$n" df -BM --output=used / 2>/dev/null | tail -n1 | tr -dc 0-9 || true)"; done
note host.incus_storage_mb "$(du -sm /var/lib/incus/storage-pools 2>/dev/null | cut -f1 || true)"
log "done in $SECONDS s"
