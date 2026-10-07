#!/usr/bin/env bash
# janitor.sh: keeps disk free on a shared dev Mac during long agent builds by trimming
# Go build-cache entries that have not been used recently. Go tolerates concurrent trims
# (it trims its own cache the same way). Runs until killed.
#
# Tunables (env): JANITOR_DISK_GB (10): trim when free disk falls below this;
# JANITOR_AGE_MIN (30): delete cache entries not touched for this many minutes.
set -uo pipefail
DISK_GB=${JANITOR_DISK_GB:-10}
AGE_MIN=${JANITOR_AGE_MIN:-30}
# The lock, pid and cache paths keep their old names (/tmp/sbctl-guard.lock, ~/.cache/sbctl) because other checkouts on this machine still share them.
LOG=${JANITOR_LOG:-$HOME/.cache/sbctl/janitor.log}
CACHE=$(go env GOCACHE 2>/dev/null || echo "$HOME/Library/Caches/go-build")
while true; do
  free=$(df -g "$HOME" | awk 'NR == 2 {print $4}')
  if [[ ${free:-99} -lt $DISK_GB ]]; then
    before=$(du -sm "$CACHE" 2>/dev/null | cut -f1)
    find "$CACHE" -type f -mmin +"$AGE_MIN" -delete 2>/dev/null
    after=$(du -sm "$CACHE" 2>/dev/null | cut -f1)
    printf '%s janitor disk %sGB: go cache %sMB -> %sMB\n' "$(date '+%F %T')" "$free" "$before" "$after" >>"$LOG"
  fi
  sleep 300
done
