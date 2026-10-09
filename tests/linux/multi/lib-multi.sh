#!/usr/bin/env bash
# Helpers for the two-node harness: two Incus nodes (n1, n2) with a real systemd on a GitHub Actions
# runner, and the services on the runner that the nodes use. Sourced, not executed.
#
# The runner is the host. On the Incus bridge (incusbr0, 10.213.213.1) it listens with Garage (S3), a
# release server that stands in for GitHub (the pattern of upgrade-e2e.sh) and a fake instance metadata,
# EC2 and Secrets Manager service (tests/linux/multi/fake-aws.py). They bind the bridge address only, so
# the nodes reach them and nothing else does. tests/linux/multi/README.md says what works where.
#
# Needs root on Ubuntu 24.04 (amd64 or arm64) with Docker, which the runner image has. Do not run it on a
# machine you care about: it installs Incus, changes iptables rules and starts instances.
LOG_DIR=${LOG_DIR:-/tmp/supavise-multi-logs}
# shellcheck source=../lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

MULTI_MODE=${MULTI_MODE:-auto}        # vm, container, container-privileged, or auto (vm when /dev/kvm works, else container-privileged)
MULTI_IMAGE=${MULTI_IMAGE:-images:ubuntu/24.04}
MULTI_CPU=${MULTI_CPU:-2}
MULTI_MEM=${MULTI_MEM:-4GiB}
MULTI_BRIDGE=incusbr0
MULTI_NET=${MULTI_NET:-10.213.213}    # the bridge is .1, n1 is .11, n2 is .12
BRIDGE_IP=$MULTI_NET.1
PEER_PORT=${PEER_PORT:-7443}          # the mesh port of the design
S3_PORT=3900 RELEASE_PORT=38801 AWS_PORT=38170
GARAGE_IMAGE=${GARAGE_IMAGE:-dxflrs/garage:v1.1.0}
S3_BUCKET=supavise-test              # backups (one prefix per cluster) and the key escrow
S3_OBJECTS_BUCKET=supavise-objects    # Storage's objects, after `supavise storage migrate --to s3`
NODES=(n1 n2)
SMOKE_NODE=n3                         # a third, fresh instance for systemd-smoke.sh (MULTI_SMOKE=1)
WORK=${WORK:-}

node_ip() { echo "$MULTI_NET.1${1#n}"; }
node_instance_id() { echo "i-0c1a00000000000${1#n}0"; }

# multi_init: the scratch directory, the log directory and the throwaway S3 credentials.
multi_init() {
  WORK=$(mktemp -d)
  chmod 0755 "$WORK"
  mkdir -p "$WORK/state" "$LOG_DIR/checks"
  : >"$WORK/state/pids"
  S3_KEY_ID=GK$(openssl rand -hex 12)
  S3_SECRET=$(openssl rand -hex 32)
}

# ---- nodes --------------------------------------------------------------------------------------
# on NODE CMD...: run CMD in the node as root. A script goes in on standard input: on n1 bash -s <<'EOS'.
on() { local node=$1; shift; incus exec "$node" --env HOME=/root -- "$@"; }

node_push() { incus file push --quiet --create-dirs --mode "${4:-0644}" "$2" "$1$3"; } # NODE SRC DEST [MODE]

# nsystemctl NODE ARGS...: systemctl in the node. systemd's bus is away for a moment now and then
# ("Transport endpoint is not connected", "Connection reset by peer", "disconnected from message bus
# without replying"): the calls that only read are repeated for a few seconds; any other call runs once.
# This is the wrapper of upgrade-e2e.sh, run through `on`.
nsystemctl() {
  local node=$1; shift
  case " $* " in
    *" show "* | *" is-active "* | *" is-enabled "*) ;;
    *) on "$node" systemctl "$@"; return ;;
  esac
  local try err rc=0
  err=$(mktemp -p "$WORK")
  for try in 1 2 3 4 5 6; do
    rc=0
    on "$node" systemctl "$@" 2>"$err" || rc=$?
    [[ $rc -eq 0 ]] && break
    grep -q -e "Transport endpoint is not connected" -e "Connection reset by peer" -e "disconnected from message bus" \
      -e "D-Bus connection terminated" -e "Failed to connect to bus" "$err" || break
    sleep 1
  done
  cat "$err" >&2
  rm -f "$err"
  return $rc
}

# ---- the host: KVM, Incus, Docker's forwarding rules ----------------------------------------------
kvm_usable() { # exit 0 when /dev/kvm opens and answers KVM_GET_API_VERSION
  [[ -c /dev/kvm ]] || return 1
  python3 - <<'PY'
import fcntl, os, sys
try:
    fd = os.open("/dev/kvm", os.O_RDWR)
    sys.exit(0 if fcntl.ioctl(fd, 0xAE00) == 12 else 1)
except OSError:
    sys.exit(1)
PY
}

# kvm_enable: public runners expose /dev/kvm to root and the kvm group only; this rule opens it to
# everyone, the way GitHub's changelog describes it, in case Incus starts QEMU as another user.
kvm_enable() {
  [[ -c /dev/kvm ]] || modprobe -a kvm_intel kvm_amd 2>/dev/null || true
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' >/etc/udev/rules.d/99-kvm4all.rules
  udevadm control --reload-rules
  udevadm trigger --name-match=kvm || true
}

multi_resolve_mode() { # sets MULTI_RESOLVED
  case $MULTI_MODE in
    # Unprivileged containers do not enforce IPAddressDeny (README.md).
    auto) if kvm_usable; then MULTI_RESOLVED=vm; else MULTI_RESOLVED=container-privileged; fi ;;
    vm | container | container-privileged) MULTI_RESOLVED=$MULTI_MODE ;;
    *) fail "MULTI_MODE=$MULTI_MODE: want auto, vm, container or container-privileged" ;;
  esac
}

multi_incus_install() {
  export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a
  local pkgs=(incus)
  if [[ $MULTI_RESOLVED == vm ]]; then
    case $(dpkg --print-architecture) in
      amd64) pkgs+=(qemu-system-x86 ovmf) ;;
      arm64) pkgs+=(qemu-system-arm qemu-efi-aarch64) ;;
    esac
  fi
  # The runner's own apt timers may hold the apt locks for a while after it boots. The dpkg lock timeout covers install;
  # `update` takes the lock of the package lists, which it does not cover (lib.sh's wait_apt_idle), so it is repeated.
  local try
  wait_apt_idle 360
  for try in 1 2 3 4 5 6; do
    apt-get -o DPkg::Lock::Timeout=300 update -qq && break
    [[ $try -lt 6 ]] || fail "apt-get update failed six times"
    sleep 10
  done
  apt-get -o DPkg::Lock::Timeout=300 install -y -qq "${pkgs[@]}"
  # Unprivileged containers need an id range for root.
  grep -q '^root:' /etc/subuid || echo 'root:1000000:1000000000' >>/etc/subuid
  grep -q '^root:' /etc/subgid || echo 'root:1000000:1000000000' >>/etc/subgid
  systemctl restart incus.service
  incus admin waitready --timeout 120
  incus --version >&2
}

# multi_incus_init: the bridge with the address the nodes' services are reached at, and a directory pool.
multi_incus_init() {
  incus admin init --preseed <<PRESEED
config: {}
networks:
- name: $MULTI_BRIDGE
  type: bridge
  config:
    ipv4.address: $BRIDGE_IP/24
    ipv4.nat: "true"
    ipv6.address: none
storage_pools:
- name: default
  driver: dir
profiles:
- name: default
  devices:
    eth0: {name: eth0, network: $MULTI_BRIDGE, type: nic}
    root: {path: /, pool: default, type: disk}
PRESEED
}

# multi_docker_rules: the runner image runs Docker, which sets the FORWARD policy to DROP, so what the
# nodes send to the internet reaches the DOCKER-USER chain first. Accept what leaves the bridge, and what
# comes back. (Traffic between the nodes is bridged, not forwarded; br_netfilter is not loaded on the
# runner, so it never meets the policy. The `rules` check in spike.sh shows both.)
multi_docker_rules() {
  if ! iptables -w -nL DOCKER-USER >/dev/null 2>&1; then
    log "no DOCKER-USER chain: Docker does not filter forwarding on this host"
    return 0
  fi
  iptables -w -C DOCKER-USER -i "$MULTI_BRIDGE" -j ACCEPT 2>/dev/null \
    || iptables -w -I DOCKER-USER -i "$MULTI_BRIDGE" -j ACCEPT
  iptables -w -C DOCKER-USER -o "$MULTI_BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
    || iptables -w -I DOCKER-USER -o "$MULTI_BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
}

multi_docker_rules_remove() { # what multi_docker_rules added, for the probe that shows what it is for
  iptables -w -D DOCKER-USER -i "$MULTI_BRIDGE" -j ACCEPT 2>/dev/null || true
  iptables -w -D DOCKER-USER -o "$MULTI_BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
}

# multi_launch_node NODE: create the instance with its address and start it (does not wait). A virtual
# machine gets a bigger root disk than the image's; multi_grow_root grows the file system into it.
multi_launch_node() {
  local node=$1 flags=(-c "limits.cpu=$MULTI_CPU" -c "limits.memory=$MULTI_MEM")
  case $MULTI_RESOLVED in
    vm) flags+=(--vm -c security.secureboot=false) ;;
    container) flags+=(-c security.nesting=true) ;;
    container-privileged) flags+=(-c security.nesting=true -c security.privileged=true) ;;
  esac
  incus delete --force "$node" >/dev/null 2>&1 || true
  incus init "$MULTI_IMAGE" "$node" "${flags[@]}"
  incus config device override "$node" eth0 "ipv4.address=$(node_ip "$node")"
  [[ $MULTI_RESOLVED != vm ]] || incus config device override "$node" root size=16GiB
  incus start "$node"
}

# multi_wait NODE: the node answers `incus exec` and systemd has finished starting. Prints the state
# (running or degraded). `systemctl is-system-running --wait` is not used: it fails at once while D-Bus is
# not up yet.
multi_wait() {
  local node=$1 i st=""
  for ((i = 0; i < 90; i++)); do
    incus exec "$node" -- true 2>/dev/null && break
    sleep 2
  done
  incus exec "$node" -- true || fail "$node does not answer incus exec"
  for ((i = 0; i < 90; i++)); do
    st=$(incus exec "$node" -- systemctl is-system-running 2>/dev/null) || true
    case $st in running | degraded | stopping) break ;; esac
    sleep 2
  done
  echo "$st"
}

# multi_grow_root NODE: grow the root partition and its file system to the size of the disk.
multi_grow_root() {
  on "$1" bash -s <<'EOS'
set -euo pipefail
src=$(findmnt -no SOURCE /)
disk=/dev/$(lsblk -no PKNAME "$src")
num=$(cat "/sys/class/block/$(basename "$src")/partition")
DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y -qq cloud-guest-utils
growpart "$disk" "$num" || true
resize2fs "$src"
EOS
}

# multi_prep_node NODE: what the install needs and the image may lack. polkit is left to the installer.
# The apt timers are masked: a timer that fires during the install holds the apt lock.
multi_prep_node() {
  on "$1" bash -s <<'EOS'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
systemctl stop apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
systemctl mask apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
for try in 1 2 3 4 5; do apt-get -o DPkg::Lock::Timeout=300 update -qq && break; sleep 5; done
apt-get -o DPkg::Lock::Timeout=300 install -y -qq curl ca-certificates sudo python3 iproute2 iputils-ping openssl procps
EOS
}

# multi_install NODE [FLAGS...]: copies the binary (SUPAVISE_BIN), install.sh, lib.sh and the S3 credentials (a
# 0600 file) into the node and runs `install.sh --binary` there with the flags every node of the harness takes:
# its own address, no TLS, no firewall changes, no dashboard, no OS updates, and Garage as the backup backend
# under the prefix S3_PREFIX (default: the node's name; a cluster shares one prefix, so a joining server gets the
# same one as the server it joins). When MULTI_DOMAIN is set, that is the domain: left to the default, a domain
# follows each node's own address, and the nodes of a cluster need one. FLAGS follow, for example
# --claim-token-file or --join-token-file. The output goes to $LOG_DIR/NODE/install.log; the exit status is
# install.sh's.
multi_install() {
  local n=$1 ip d=$LOG_DIR/$1 rc=0 try
  shift
  ip=$(node_ip "$n")
  mkdir -p "$d"
  node_push "$n" "$SUPAVISE_BIN" /root/supavise 0755
  node_push "$n" "$REPO_ROOT/deploy/install.sh" /root/install.sh 0755
  node_push "$n" "$REPO_ROOT/tests/linux/lib.sh" /root/lib.sh
  printf 'access_key_id=%s\nsecret_access_key=%s\n' "$S3_KEY_ID" "$S3_SECRET" | on "$n" bash -c 'umask 077; cat >/root/s3.cred'
  # install.sh is idempotent. The artifact host answers 500 now and then, and the installer does not try again,
  # so a run that fails on a server error of that host is run once more.
  for try in 1 2; do
    rc=0
    on "$n" /root/install.sh --binary /root/supavise --public-ip "$ip" --tls off --email ci@example.com --firewall none \
      --no-studio --no-os-updates ${MULTI_DOMAIN:+--domain "$MULTI_DOMAIN"} \
      --s3-endpoint "http://$BRIDGE_IP:$S3_PORT" --s3-bucket "$S3_BUCKET" --s3-prefix "${S3_PREFIX:-$n}" --s3-region us-east-1 \
      --s3-path-style --s3-credentials-file /root/s3.cred "$@" >"$d/install.log" 2>&1 || rc=$?
    [[ $rc -eq 0 ]] && return 0
    grep -q -E 'GET https://[^ ]+: status 5[0-9][0-9]' "$d/install.log" || return $rc
    log "$n: install.sh failed on a server error of the artifact host; running it once more"
    cp "$d/install.log" "$d/install-try$try.log"
  done
  return $rc
}

# multi_push_tests NODE: the helpers the replication test runs inside the node, and the environment they read.
multi_push_tests() {
  local n=$1 here=$REPO_ROOT/tests/linux
  node_push "$n" "$here/lib.sh" /root/lib.sh
  node_push "$n" "$here/multi/node-lib.sh" /root/node-lib.sh
  node_push "$n" "$here/multi/writer.py" /root/writer.py 0755
  node_push "$n" "$here/multi/fence.sh" /usr/local/lib/supavise-ci/fence.sh 0755
  # incus creates the directory for root alone, and the daemon, which runs the fence command, is the supavise user.
  on "$n" chmod 0755 /usr/local/lib/supavise-ci
  printf 'export SUPAVISE_DOMAIN=%s\nexport BRIDGE_IP=%s\nexport S3_BUCKET=%s\nexport S3_OBJECTS_BUCKET=%s\n' \
    "${MULTI_DOMAIN:-$(node_ip n1).sslip.io}" "$BRIDGE_IP" "$S3_BUCKET" "$S3_OBJECTS_BUCKET" | on "$n" tee /root/multi-env.sh >/dev/null
}

# multi_kill_node NODE: the node stops at once, the way a failed machine does (no shutdown, no flush).
multi_kill_node() { incus stop --force "$1"; }

# multi_start_node NODE: starts a stopped node and waits for systemd. Prints the state.
multi_start_node() { incus start "$1" && multi_wait "$1"; }

# ---- services on the host, bound to the bridge ------------------------------------------------------
# garage_up: Garage as the S3 service (MinIO no longer publishes images; the backup-integration job uses
# Garage the same way), one bucket and one key. The image is a single static binary.
garage_up() {
  command -v docker >/dev/null || { apt-get -o DPkg::Lock::Timeout=300 install -y -qq docker.io; }
  local d=$WORK/garage i id b
  mkdir -p "$d/meta" "$d/data"
  cat >"$d/garage.toml" <<TOML
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "127.0.0.1:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(openssl rand -hex 32)"
[s3_api]
s3_region = "us-east-1"
api_bind_addr = "$BRIDGE_IP:$S3_PORT"
[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "supavise-ci-admin-token"
TOML
  docker rm -f garage >/dev/null 2>&1 || true
  docker run -d --name garage --network host \
    -v "$d/garage.toml:/etc/garage.toml" -v "$d/meta:/var/lib/garage/meta" -v "$d/data:/var/lib/garage/data" \
    "$GARAGE_IMAGE" >/dev/null
  for ((i = 0; i < 60; i++)); do docker exec garage /garage status >/dev/null 2>&1 && break; sleep 1; done
  id=$(docker exec garage /garage node id -q | cut -d@ -f1)
  docker exec garage /garage layout assign -z dc1 -c 1G "$id" >/dev/null
  docker exec garage /garage layout apply --version 1 >/dev/null
  docker exec garage /garage key import --yes -n ci "$S3_KEY_ID" "$S3_SECRET" >/dev/null
  for b in "$S3_BUCKET" "$S3_OBJECTS_BUCKET"; do
    docker exec garage /garage bucket create "$b" >/dev/null
    docker exec garage /garage bucket allow --read --write --owner "$b" --key ci >/dev/null
  done
  for ((i = 0; i < 30; i++)); do
    [[ $(http_code "http://$BRIDGE_IP:$S3_PORT/") != 000 ]] && return 0
    sleep 1
  done
  docker logs garage 2>&1 | tail -20 >&2
  fail "Garage does not answer on $BRIDGE_IP:$S3_PORT"
}
garage_down() { docker rm -f garage >/dev/null 2>&1 || true; }

# make_release TAG BINARY: $WORK/srv/download/TAG and the API files of the repository o/r, signed with
# the throwaway key. The other architecture's binary is a placeholder, as in upgrade-e2e.sh. Needs
# SUPAVISE_RELEASETOOL (a built deploy/releasetool; root has no Go toolchain).
make_release() {
  local tag=$1 bin=$2 arch d=$WORK/srv/download/$1
  arch=$(dpkg --print-architecture)
  release_stage "$d" "$bin" "$arch"
  SUPAVISE_RELEASE_TAG=$tag deploy/release-assets.sh "$d" "$WORK/keys/sign.pem" "$WORK/keys/pub.pem" >/dev/null
  release_api "$tag" "$d" "$BRIDGE_IP:$RELEASE_PORT" latest
}

# release_server_up TAG BINARY: a signed release of BINARY, served on the bridge address. A node installs
# from it with SUPAVISE_INSTALL_BASE_URL=http://$BRIDGE_IP:$RELEASE_PORT (the install.sh in the release
# carries the public key) or upgrades with --repo o/r --api-base and $WORK/keys/pub.pem.
release_server_up() {
  local i
  [[ -x ${SUPAVISE_RELEASETOOL:-} ]] || fail "SUPAVISE_RELEASETOOL: a built deploy/releasetool is needed to sign a release"
  export SUPAVISE_RELEASETOOL
  mkdir -p "$WORK/keys" "$WORK/srv"
  openssl genpkey -algorithm ed25519 -out "$WORK/keys/sign.pem"
  openssl pkey -in "$WORK/keys/sign.pem" -pubout -out "$WORK/keys/pub.pem"
  (cd "$REPO_ROOT" && make_release "$1" "$2")
  (cd "$WORK/srv" && exec python3 -m http.server "$RELEASE_PORT" --bind "$BRIDGE_IP" >"$WORK/http.log" 2>&1) &
  echo $! >>"$WORK/state/pids"
  for ((i = 0; i < 20; i++)); do
    [[ $(http_code "http://$BRIDGE_IP:$RELEASE_PORT/download/$1/SHA256SUMS") == 200 ]] && return 0
    sleep 0.5
  done
  fail "the release server does not answer on $BRIDGE_IP:$RELEASE_PORT"
}

# fake_aws_up: instance metadata (IMDSv2), the EC2 Query API and Secrets Manager on one address, the way
# AWS_ENDPOINT_URL_IMDS, _EC2 and _SECRETSMANAGER point at it. The calls go to $LOG_DIR/fake-aws.jsonl.
# The real metadata address, 169.254.169.254, is lib.sh's imds_up inside a node, where the units'
# IPAddressDeny is tested.
fake_aws_up() {
  local i n args=()
  for n in "${NODES[@]}"; do args+=(--node "$n=$(node_ip "$n")=$(node_instance_id "$n")"); done
  python3 "$REPO_ROOT/tests/linux/multi/fake-aws.py" --bind "$BRIDGE_IP" --port "$AWS_PORT" \
    --log "$LOG_DIR/fake-aws.jsonl" --cluster ci --secret "supavise/join=fake-join-token" "${args[@]}" \
    >"$WORK/fake-aws.out" 2>&1 &
  echo $! >>"$WORK/state/pids"
  for ((i = 0; i < 20; i++)); do
    [[ $(http_code "http://$BRIDGE_IP:$AWS_PORT/_calls") == 200 ]] && return 0
    sleep 0.5
  done
  cat "$WORK/fake-aws.out" >&2
  fail "the fake AWS service does not answer on $BRIDGE_IP:$AWS_PORT"
}

# ---- node to node -------------------------------------------------------------------------------------
# peer_listen NODE: a TCP sink on PEER_PORT as a transient unit. It counts the bytes of each connection
# and answers with the number.
peer_listen() {
  on "$1" tee /root/peer-sink.py >/dev/null <<'PY'
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", int(sys.argv[1])))
s.listen(8)
while True:
    c, _ = s.accept()
    n = 0
    while True:
        b = c.recv(1 << 20)
        if not b:
            break
        n += len(b)
    c.sendall(b"%d\n" % n)
    c.close()
PY
  on "$1" systemctl stop peer-sink.service 2>/dev/null || true
  on "$1" systemd-run --quiet --unit=peer-sink python3 /root/peer-sink.py "$PEER_PORT"
}

# peer_send FROM-NODE ADDRESS BYTES: sends BYTES to the sink on ADDRESS. Prints the bytes it acknowledged
# and the seconds it took.
peer_send() {
  on "$1" python3 - "$2" "$PEER_PORT" "$3" <<'PY'
import socket, sys, time
host, port, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
t0 = time.time()
s = socket.create_connection((host, port), timeout=10)
chunk = b"\0" * (1 << 20)
sent = 0
while sent < n:
    k = min(len(chunk), n - sent)
    s.sendall(chunk[:k])
    sent += k
s.shutdown(socket.SHUT_WR)
print(int(s.recv(64).decode().strip()), round(time.time() - t0, 3))
PY
}

# ---- measurements and logs ----------------------------------------------------------------------------
# mem_snapshot LABEL: memory of the host and of each node, one line each in $LOG_DIR/memory.tsv:
# label, who, used MB (free -m; the host's counts the nodes), detail.
mem_snapshot() {
  local label=$1 n used slice cur
  printf '%s\thost\t%s\tavailable %s MB of %s MB\n' "$label" \
    "$(free -m | awk '/^Mem:/ {print $3}')" "$(free -m | awk '/^Mem:/ {print $7}')" "$(free -m | awk '/^Mem:/ {print $2}')" >>"$LOG_DIR/memory.tsv"
  for n in "${NODES[@]}"; do
    used=$(on "$n" free -m | awk '/^Mem:/ {print $3}') || continue
    slice=$(nsystemctl "$n" show -p MemoryCurrent --value supavise.slice 2>/dev/null || true)
    [[ $slice =~ ^[0-9]+$ ]] && slice="supavise.slice $((slice / 1048576)) MB" || slice="supavise.slice n/a"
    cur=$(incus info "$n" 2>/dev/null | awk -F': ' '/Memory \(current\)/ {print $2; exit}') || true
    printf '%s\t%s\t%s\t%s; incus %s\n' "$label" "$n" "$used" "$slice" "${cur:-n/a}" >>"$LOG_DIR/memory.tsv"
  done
}

multi_collect_logs() {
  local n
  mkdir -p "$LOG_DIR/host"
  {
    echo "== incus list"; incus list
    echo "== incus version"; incus --version
    echo "== ip addr"; ip -br addr
    echo "== iptables FORWARD and DOCKER-USER"; iptables -S FORWARD; iptables -S DOCKER-USER
    echo "== sysctl"; sysctl net.bridge.bridge-nf-call-iptables net.ipv4.ip_forward
    echo "== kvm"; ls -l /dev/kvm; lscpu | grep -i -E 'model name|virtualization|hypervisor'
    echo "== docker"; docker ps -a
    echo "== df"; df -h / /var/lib
  } >"$LOG_DIR/host/state.txt" 2>&1 || true
  nft list ruleset >"$LOG_DIR/host/nft.txt" 2>&1 || true
  journalctl --no-pager -o short-iso -u incus.service >"$LOG_DIR/host/incus.journal" 2>&1 || true
  dmesg 2>/dev/null | tail -200 >"$LOG_DIR/host/dmesg.txt" || true
  cp -r /var/log/incus "$LOG_DIR/host/incus-logs" 2>/dev/null || true
  docker logs garage >"$LOG_DIR/host/garage.log" 2>&1 || true
  cp "$WORK/http.log" "$LOG_DIR/host/release-server.log" 2>/dev/null || true
  cp "$WORK/fake-aws.out" "$LOG_DIR/host/fake-aws.out" 2>/dev/null || true
  for n in "${NODES[@]}" "$SMOKE_NODE"; do
    incus info "$n" >/dev/null 2>&1 || continue
    mkdir -p "$LOG_DIR/$n"
    incus info "$n" >"$LOG_DIR/$n/incus-info.txt" 2>&1 || true
    incus config show "$n" --expanded >"$LOG_DIR/$n/incus-config.yaml" 2>&1 || true
    incus console "$n" --show-log >"$LOG_DIR/$n/console.log" 2>&1 || true
    # A node that a check stopped (multi_kill_node) and did not start again has no journal to read, in the run where the
    # old leader's journal is the one that explains the failure: it is started, after its console log is taken.
    if [[ $(incus info "$n" 2>/dev/null | awk '/^Status:/ {print toupper($2); exit}') == STOPPED ]]; then
      echo "$n was stopped when the run ended; it was started to collect its logs. incus-info.txt and console.log are from before" \
        >"$LOG_DIR/$n/started-for-collection.txt"
      incus start "$n" >/dev/null 2>&1 && multi_wait "$n" >/dev/null 2>&1 || true
    fi
    timeout 60 incus exec "$n" -- journalctl --no-pager -o short-iso >"$LOG_DIR/$n/journal.log" 2>&1 || true
    # The daemon and every project's PostgreSQL on their own, for reading: the journal holds everything.
    timeout 60 incus exec "$n" -- journalctl --no-pager -o short-iso -u supavise.service >"$LOG_DIR/$n/daemon.journal" 2>&1 || true
    timeout 60 incus exec "$n" -- journalctl --no-pager -o short-iso -u 'supavise-postgres@*' >"$LOG_DIR/$n/postgres.journal" 2>&1 || true
    if incus exec "$n" -- test -f /root/node-lib.sh 2>/dev/null; then
      timeout 120 incus exec "$n" --env HOME=/root -- bash -c 'source /root/node-lib.sh && node_dump' >"$LOG_DIR/$n/state.txt" 2>&1 || true
    fi
    incus file pull --quiet -r "$n/root/writer" "$LOG_DIR/$n/" >/dev/null 2>&1 || true
    timeout 30 incus exec "$n" -- systemctl list-units --all --no-pager >"$LOG_DIR/$n/units.txt" 2>&1 || true
    timeout 30 incus exec "$n" -- systemctl --failed --no-pager >"$LOG_DIR/$n/failed-units.txt" 2>&1 || true
  done
}

# multi_down: the servers started here (their pids are in $WORK/state/pids, because a check starts them in
# a subshell), Garage and the nodes.
multi_down() {
  local n p
  while read -r p; do kill "$p" 2>/dev/null || true; done <"$WORK/state/pids" 2>/dev/null || true
  garage_down
  for n in "${NODES[@]}" "$SMOKE_NODE"; do incus delete --force "$n" >/dev/null 2>&1 || true; done
}
