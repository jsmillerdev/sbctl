#!/usr/bin/env bash
# Shared helpers for tests/linux/*.sh. Sourced, not executed.
#
# These scripts are meant for an ephemeral Ubuntu 24.04 (or 22.04, Debian 12) CI VM with
# systemd and sudo. They create a system user, install units under /etc/systemd/system,
# and start real PostgreSQL clusters. Do not run them on a machine you care about.

set -euo pipefail

REPO_ROOT=${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
SBCTL_BIN=${SBCTL_BIN:-}            # a linux sbctl binary; built from REPO_ROOT when empty
SBCTL_USER=sbctl
SBCTL_STATE=/var/lib/sbctl
SBCTL_CONF=/etc/sbctl/config.toml
SBCTL_DOMAIN=${SBCTL_DOMAIN:-sbctl.test}
LOG_DIR=${LOG_DIR:-/tmp/sbctl-linux-logs}

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

# install_binary: put sbctl at /usr/local/bin/sbctl (build it when SBCTL_BIN is not set).
install_binary() {
  if [[ -z "$SBCTL_BIN" ]]; then
    command -v go >/dev/null || fail "no SBCTL_BIN and no go toolchain to build one"
    log "building sbctl"
    (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -o /tmp/sbctl-linux-test ./cmd/sbctl)
    SBCTL_BIN=/tmp/sbctl-linux-test
  fi
  install -m 0755 "$SBCTL_BIN" /usr/local/bin/sbctl
  /usr/local/bin/sbctl --version >&2 || true
}

# setup_node: user, directories, config, units. Safe to run twice.
setup_node() {
  # The sbctl user drives systemd over D-Bus; that needs polkit and the rule that
  # `sbctl system install-units` installs.
  if ! command -v pkaction >/dev/null; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq || log "apt-get update failed; trying the install anyway"
    apt-get install -y polkitd pkexec \
      || apt-get install -y policykit-1 \
      || fail "polkit is not installed and could not be installed"
  fi
  id "$SBCTL_USER" >/dev/null 2>&1 || useradd --system --home-dir "$SBCTL_STATE" --shell /usr/sbin/nologin "$SBCTL_USER"
  install -d -o "$SBCTL_USER" -g "$SBCTL_USER" -m 0750 "$SBCTL_STATE" /etc/sbctl
  cat >"$SBCTL_CONF" <<CONF
domain = "$SBCTL_DOMAIN"
supervisor = "systemd"
log_level = "info"
bin_path = "/usr/local/bin/sbctl"

[tls]
mode = "off"
CONF
  chown "$SBCTL_USER:$SBCTL_USER" "$SBCTL_CONF"
  chmod 0640 "$SBCTL_CONF"
  /usr/local/bin/sbctl system install-units
  systemctl daemon-reload
}

# sbctl: run the CLI as the sbctl user, which owns the state directory and is the only
# user the polkit rule lets manage the sb-* units.
sbctl() { sudo -u "$SBCTL_USER" -H /usr/local/bin/sbctl "$@"; }

# system_init: fetch artifacts and create the system project.
system_init() { sbctl system init; }

# json_get FILE_OR_STDIN PYEXPR: evaluate a Python expression over the parsed JSON "d".
json_get() { python3 -c 'import json,sys; d=json.load(sys.stdin); print('"$1"')'; }

# create_project NAME CLASS: prints the ref.
create_project() {
  sbctl projects create --name "$1" --class "${2:-default}" --json | json_get 'd["ref"]'
}

# project_field REF PYEXPR [--show-keys]
project_field() {
  local ref=$1 expr=$2; shift 2
  sbctl projects get "$ref" --json "$@" | json_get "$expr"
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

collect_logs() {
  mkdir -p "$LOG_DIR"
  journalctl --no-pager -o short-iso -u 'sb-*' -u sbctl.service >"$LOG_DIR/journal.log" 2>&1 || true
  systemctl list-units --all --no-pager 'sb-*' 'sbctl*' >"$LOG_DIR/units.txt" 2>&1 || true
  systemctl status --no-pager sbctl.slice >"$LOG_DIR/slice.txt" 2>&1 || true
  df -h /var/lib >"$LOG_DIR/df.txt" 2>&1 || true
  log "logs in $LOG_DIR"
}

teardown() {
  systemctl stop 'sb-*' 2>/dev/null || true
}
