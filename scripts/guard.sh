#!/usr/bin/env bash
# guard.sh: run a build or test command on a shared dev Mac without taking the machine down.
#
#   scripts/guard.sh [--no-lock] -- <command> [args...]
#
# - Waits until free memory and disk are above their floors before starting.
# - Holds one of GUARD_SLOTS machine-wide slots (default 2), so at most that many heavy jobs
#   run at once; a second slot is taken only while free memory is well above the floor
#   (--no-lock skips the slots, for light long-running helpers such as a test Postgres).
# - Runs the command at low priority in its own process group with capped Go and Node
#   parallelism and heap.
# - Kills the whole process group if free memory, swap growth or free disk cross a floor.
#
# Tunables (env): GUARD_MIN_FREE_PCT (25), GUARD_MIN_DISK_GB (6), GUARD_MAX_SWAP_GROWTH_MB (1500),
# GUARD_WAIT_SECS (900), GUARD_SLOTS (2), GUARD_LOCK (/tmp/sbctl-guard.lock), GUARD_LOG (~/.cache/sbctl/guard.log).
set -uo pipefail

# The lock, pid and cache paths keep their old names (/tmp/sbctl-guard.lock, ~/.cache/sbctl) because other checkouts on this machine still share them.
MIN_FREE_PCT=${GUARD_MIN_FREE_PCT:-25}
MIN_DISK_GB=${GUARD_MIN_DISK_GB:-6}
MAX_SWAP_GROWTH_MB=${GUARD_MAX_SWAP_GROWTH_MB:-1500}
WAIT_SECS=${GUARD_WAIT_SECS:-900}
LOCK=${GUARD_LOCK:-/tmp/sbctl-guard.lock}
SLOTS=${GUARD_SLOTS:-2}
LOG=${GUARD_LOG:-$HOME/.cache/sbctl/guard.log}
PIDDIR=/tmp/sbctl-guard.pids

use_lock=1
if [[ "${1:-}" == "--no-lock" ]]; then use_lock=0; shift; fi
[[ "${1:-}" == "--" ]] && shift
[[ $# -gt 0 ]] || { echo "usage: guard.sh [--no-lock] -- command [args...]" >&2; exit 2; }

export GOMAXPROCS=${GOMAXPROCS:-4}
export GOFLAGS=${GOFLAGS:--p=2}
export GOMEMLIMIT=${GOMEMLIMIT:-1536MiB}
export NODE_OPTIONS=${NODE_OPTIONS:---max-old-space-size=2048}

mkdir -p "$(dirname "$LOG")" "$PIDDIR"
log() { printf '%s guard[%d] %s\n' "$(date '+%F %T')" $$ "$*" | tee -a "$LOG" >&2; }

free_pct() { memory_pressure -Q 2>/dev/null | awk -F': ' '/free percentage/ {gsub("%", "", $2); print int($2)}'; }
swap_mb() { sysctl -n vm.swapusage | awk '{for (i = 1; i <= NF; i++) if ($i == "used") {v = $(i + 2); sub("M", "", v); print int(v)}}'; }
disk_gb() { df -g "$HOME" | awk 'NR == 2 {print $4}'; }

healthy() { # $1 = extra headroom required to start
  local f d
  f=$(free_pct); d=$(disk_gb)
  [[ ${f:-0} -ge $((MIN_FREE_PCT + $1)) && ${d:-0} -ge $((MIN_DISK_GB + $1 / 5)) ]]
}

held=""
release() {
  if [[ -n "$held" ]]; then rm -rf "$held"; held=""; fi
}
slot_path() { if [[ $1 -eq 1 ]]; then echo "$LOCK"; else echo "$LOCK.$1"; fi; }
try_slot() { # $1 = slot number; succeeds when the slot was free or stale and is now ours
  local p owner; p=$(slot_path "$1")
  if mkdir "$p" 2>/dev/null; then echo $$ >"$p/pid"; held="$p"; return 0; fi
  owner=$(cat "$p/pid" 2>/dev/null || true)
  if [[ -n "$owner" ]] && ! kill -0 "$owner" 2>/dev/null; then rm -rf "$p"; try_slot "$1"; return; fi
  return 1
}
acquire() {
  local waited=0 i
  while true; do
    try_slot 1 && return
    # Extra slots only while memory is well above the floor.
    for ((i = 2; i <= SLOTS; i++)); do
      if [[ $(free_pct) -ge $((MIN_FREE_PCT + 20)) ]] && try_slot "$i"; then return; fi
    done
    [[ $waited -eq 0 ]] && log "waiting for a heavy-job slot"
    sleep 3; waited=$((waited + 3))
  done
}

pgid=0
cleanup() {
  if [[ $pgid -gt 0 ]] && kill -0 -- "-$pgid" 2>/dev/null; then
    kill -TERM -- "-$pgid" 2>/dev/null; sleep 5; kill -KILL -- "-$pgid" 2>/dev/null
  fi
  rm -f "$PIDDIR/$$"
  release
}
trap 'cleanup; exit 130' INT TERM HUP
trap 'cleanup' EXIT

[[ $use_lock -eq 1 ]] && acquire

waited=0
until healthy 10; do
  if [[ $waited -ge $WAIT_SECS ]]; then
    log "refusing to start: free memory $(free_pct)% / disk $(disk_gb)GB below floor after ${WAIT_SECS}s: $*"
    exit 75
  fi
  [[ $waited -eq 0 ]] && log "waiting for headroom (free $(free_pct)%, disk $(disk_gb)GB): $*"
  sleep 5; waited=$((waited + 5))
done

swap0=$(swap_mb)
set -m
nice -n 10 "$@" &
pgid=$!
set +m
echo "$pgid" >"$PIDDIR/$$"

reason=""
while kill -0 "$pgid" 2>/dev/null; do
  f=$(free_pct); d=$(disk_gb); s=$(swap_mb)
  if [[ ${f:-100} -lt $MIN_FREE_PCT ]]; then reason="free memory ${f}% < ${MIN_FREE_PCT}%"
  elif [[ ${d:-100} -lt $MIN_DISK_GB ]]; then reason="free disk ${d}GB < ${MIN_DISK_GB}GB"
  elif [[ $((s - swap0)) -gt $MAX_SWAP_GROWTH_MB ]]; then reason="swap grew $((s - swap0))MB > ${MAX_SWAP_GROWTH_MB}MB"
  fi
  if [[ -n "$reason" ]]; then
    log "KILLED ($reason): $*"
    kill -TERM -- "-$pgid" 2>/dev/null; sleep 5; kill -KILL -- "-$pgid" 2>/dev/null
    wait "$pgid" 2>/dev/null
    echo "guard.sh: killed to protect the machine: $reason" >&2
    exit 137
  fi
  sleep 2
done
wait "$pgid"
