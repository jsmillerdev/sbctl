#!/usr/bin/env bash
# watchdog.sh: machine-wide safety net while agents build and test sbctl on a shared Mac.
# Every few seconds it checks free memory and disk. Below the floor it kills, in order:
# guard.sh-managed jobs, then any process started from this repo's worktrees or the
# sbctl cache (test Postgres, artifacts, Studio builds), Go compiler/test processes,
# and Docker containers named sbctl-*. It never touches other processes.
#
# Tunables (env): WATCHDOG_MIN_FREE_PCT (18), WATCHDOG_MIN_DISK_GB (5), WATCHDOG_INTERVAL (3),
# WATCHDOG_LOG (~/.cache/sbctl/watchdog.log).
set -uo pipefail
MIN_FREE_PCT=${WATCHDOG_MIN_FREE_PCT:-18}
MIN_DISK_GB=${WATCHDOG_MIN_DISK_GB:-5}
INTERVAL=${WATCHDOG_INTERVAL:-3}
LOG=${WATCHDOG_LOG:-$HOME/.cache/sbctl/watchdog.log}
PIDDIR=/tmp/sbctl-guard.pids
OURS='/Repos/supabase-selfhost/\.worktrees/|/\.cache/sbctl/|/go-build[0-9]+/.+\.test|/pkg/tool/[a-z0-9_]+/(compile|link|asm|vet|cgo)'

mkdir -p "$(dirname "$LOG")"
log() { printf '%s watchdog %s\n' "$(date '+%F %T')" "$*" >>"$LOG"; }
free_pct() { memory_pressure -Q 2>/dev/null | awk -F': ' '/free percentage/ {gsub("%", "", $2); print int($2)}'; }
swap_mb() { sysctl -n vm.swapusage | awk '{for (i = 1; i <= NF; i++) if ($i == "used") {v = $(i + 2); sub("M", "", v); print int(v)}}'; }
disk_gb() { df -g "$HOME" | awk 'NR == 2 {print $4}'; }
breached() { local f d; f=$(free_pct); d=$(disk_gb); [[ ${f:-100} -lt $MIN_FREE_PCT || ${d:-100} -lt $MIN_DISK_GB ]]; }

kill_guarded() {
  for f in "$PIDDIR"/*; do
    [[ -e "$f" ]] || continue
    local pg; pg=$(cat "$f" 2>/dev/null)
    [[ -n "$pg" ]] && kill -TERM -- "-$pg" 2>/dev/null && log "TERM guarded group $pg"
  done
}
kill_ours() {
  local pids; pids=$(pgrep -f "$OURS" | grep -vx "$$" || true)
  [[ -z "$pids" ]] && return
  log "TERM ours: $(echo $pids)"
  kill -TERM $pids 2>/dev/null; sleep 5
  pids=$(pgrep -f "$OURS" | grep -vx "$$" || true)
  [[ -n "$pids" ]] && { log "KILL ours: $(echo $pids)"; kill -KILL $pids 2>/dev/null; }
}
kill_containers() {
  local ids; ids=$(docker ps -q --filter name=sbctl- 2>/dev/null || true)
  [[ -n "$ids" ]] && { log "kill containers: $(echo $ids)"; docker kill $ids >/dev/null 2>&1; }
}

log "start: floors free ${MIN_FREE_PCT}% disk ${MIN_DISK_GB}GB"
n=0
while true; do
  if breached; then
    log "BREACH free $(free_pct)% disk $(disk_gb)GB swap $(swap_mb)MB"
    kill_guarded; kill_containers; sleep 8
    if breached; then kill_ours; sleep 5; fi
  fi
  n=$((n + 1))
  if [[ $((n % 20)) -eq 0 ]]; then
    log "ok free $(free_pct)% disk $(disk_gb)GB swap $(swap_mb)MB load $(sysctl -n vm.loadavg | awk '{print $2}')"
  fi
  sleep "$INTERVAL"
done
