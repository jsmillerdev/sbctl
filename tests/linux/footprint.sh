#!/usr/bin/env bash
# Footprint and cold-start measurement: create projects up to each size, then record
# memory per project and in total, cold start times and disk use as a markdown table.
#
#   sudo SUPAVISE_BIN=... tests/linux/footprint.sh [--sizes "10 25 50"] [--class default] [--teardown]
#
# Memory is read from cgroup v2 (supavise.slice and each unit). "PSS" sums
# /proc/<pid>/smaps_rollup Pss over every process of a unit, which splits shared pages
# (shared_buffers, the loaded libraries) fairly between the processes that map them; it
# is the number to use for sizing. "RSS" sums Rss and counts shared pages once per
# process, so it overstates a Postgres cluster. memory.current includes page cache and is
# shown for the slice only. See docs/reference/footprint.md for the method.
#
# Not run in development. CI runs it on an ephemeral Ubuntu 24.04 VM sized like the target
# (state the instance type in the results).
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SIZES="10 25 50"
CLASS=${CLASS:-default}
TEARDOWN=0
while [[ $# -gt 0 ]]; do
  case $1 in
    --sizes) SIZES=$2; shift 2 ;;
    --class) CLASS=$2; shift 2 ;;
    --teardown) TEARDOWN=1; shift ;;
    *) fail "unknown argument $1" ;;
  esac
done

trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

now() { date +%s.%N; }
elapsed() { python3 -c "print(round($(now) - $1, 2))"; }

unit_pids() { cat "/sys/fs/cgroup/supavise.slice/$1/cgroup.procs" 2>/dev/null || true; }

# unit_mem UNIT FIELD: sum of Pss or Rss (kB) over the unit's processes.
unit_mem() {
  local u=$1 field=$2 sum=0 v
  for pid in $(unit_pids "$u"); do
    v=$(awk -v f="$field:" '$1 == f {print $2}' "/proc/$pid/smaps_rollup" 2>/dev/null || echo 0)
    sum=$((sum + ${v:-0}))
  done
  echo "$sum"
}

project_mem_kb() { # REF FIELD
  local t=0 v
  for s in postgres gotrue postgrest; do
    v=$(unit_mem "supavise-$s@$1.service" "$2")
    t=$((t + v))
  done
  echo "$t"
}

median() { sort -n | awk '{a[NR]=$1} END {if (NR==0) print 0; else print (NR%2 ? a[(NR+1)/2] : (a[NR/2]+a[NR/2+1])/2)}'; }
mb() { python3 -c "print(round($1 / 1024, 1))"; }

preflight
install_binary
setup_node
log "system init"
system_init

REFS=()
RESULTS="$LOG_DIR/footprint.md"
{
  echo "| projects | create avg (s) | PSS per project, median (MB) | RSS per project, median (MB) | system project PSS (MB) | supavise.slice memory.current (MB) | disk per project (MB) | resume avg (s) | node cold start (s) |"
  echo "|---|---|---|---|---|---|---|---|---|"
} >"$RESULTS"

for target in $SIZES; do
  log "growing to $target projects"
  t_create=0
  new=0
  while [[ ${#REFS[@]} -lt $target ]]; do
    t0=$(now)
    ref=$(create_project "fp-$((${#REFS[@]} + 1))" "$CLASS")
    t_create=$(python3 -c "print($t_create + $(now) - $t0)")
    REFS+=("$ref")
    new=$((new + 1))
  done
  create_avg=$(python3 -c "print(round($t_create / max($new, 1), 2))")

  log "settling 30 s"
  sleep 30
  pss=() rss=()
  for ref in "${REFS[@]}"; do
    pss+=("$(project_mem_kb "$ref" Pss)")
    rss+=("$(project_mem_kb "$ref" Rss)")
  done
  pss_med=$(printf '%s\n' "${pss[@]}" | median)
  rss_med=$(printf '%s\n' "${rss[@]}" | median)
  sys_pss=$(( $(unit_mem supavise-postgres@system.service Pss) + $(unit_mem supavise-gotrue@system.service Pss) ))
  slice_cur=$(( $(cat /sys/fs/cgroup/supavise.slice/memory.current) / 1024 ))
  disk_kb=$(du -sk "$SUPAVISE_STATE/projects/${REFS[0]}" | cut -f1)

  # Resume time: pause five projects, then resume them one at a time.
  sample=("${REFS[@]:0:5}")
  for ref in "${sample[@]}"; do supavise projects pause "$ref"; done
  t_resume=0
  for ref in "${sample[@]}"; do
    t0=$(now)
    supavise projects resume "$ref"
    t_resume=$(python3 -c "print($t_resume + $(now) - $t0)")
  done
  resume_avg=$(python3 -c "print(round($t_resume / ${#sample[@]}, 2))")

  # Whole-node cold start: stop everything, then start the system project and every
  # active project the way the daemon does after a boot.
  systemctl stop 'supavise-*'
  sleep 2
  t0=$(now)
  supavise system start
  cold=$(elapsed "$t0")

  printf '| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n' \
    "$target" "$create_avg" "$(mb "$pss_med")" "$(mb "$rss_med")" "$(mb "$sys_pss")" "$(mb "$slice_cur")" "$(mb "$disk_kb")" "$resume_avg" "$cold" >>"$RESULTS"
  log "row for $target projects recorded"
done

{
  echo
  echo "Class: $CLASS. Host:"
  echo
  sed 's/^/    /' "$LOG_DIR/host.txt"
} >>"$RESULTS"
cat "$RESULTS"
