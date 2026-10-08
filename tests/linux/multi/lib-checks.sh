#!/usr/bin/env bash
# The check runner of the harness's scripts (spike.sh, replication.sh) and the checks that bring the two
# nodes up. Sourced after lib-multi.sh. A script sets CHECK_ORDER (the ids in the order of its report) and
# RESULTS_TITLE, optionally EXPECT_FAIL and CHECK_ON_FAIL, calls `check` for each step and traps `finish`.
#
# Every check is recorded as PASS, FAIL or SKIP (a check whose predecessor did not pass) in
# $LOG_DIR/results.tsv, and results.md is the same as a table for the job summary.
#
# EXPECT_FAIL names the checks (without the @node) that are known to fail in a mode, each with the reason
# its failure must give: "imds=can reach the instance metadata service;smoke=can reach the instance
# metadata service". The script exits 0 when exactly those fail, each for its reason, and non-zero when
# another check fails, when one of them fails for another reason (the cause that was known is gone and
# something else broke) or when one of them passes (the README is then out of date).
# shellcheck source=lib-multi.sh
[[ -n ${WORK+x} ]] || source "$(dirname "${BASH_SOURCE[0]}")/lib-multi.sh"

EXPECT_FAIL=${EXPECT_FAIL:-}
CHECK_ON_FAIL=${CHECK_ON_FAIL:-}          # a function called with the check's id after a check failed
CHECK_ORDER=${CHECK_ORDER:-}
RESULTS_TITLE=${RESULTS_TITLE:-Two-node harness}
RESULTS=$LOG_DIR/results.tsv
FACTS=$LOG_DIR/facts.tsv

# ---- results ----------------------------------------------------------------------------------------
note() { printf '%s\t%s\n' "$1" "$2" >>"$FACTS"; }                       # KEY VALUE
stamp() { printf '%s\t%d\n' "$1" $((SECONDS - ${2:-0})) >>"$LOG_DIR/timings.tsv"; } # LABEL SINCE
needs() { local i; for i in "$@"; do [[ -e $WORK/state/$i.ok ]] || { echo "# needs $i"; exit 125; }; done; }
reached() { touch "$WORK/state/$1.ok"; }  # a state the later checks need, whether or not the check that got there passed

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
  if [[ $res == FAIL ]]; then
    echo "--- end of $id.log" >&2; tail -n 30 "$f" >&2
    [[ -z $CHECK_ON_FAIL ]] || "$CHECK_ON_FAIL" "$id" || true
  fi
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
    echo "## $RESULTS_TITLE: $(dpkg --print-architecture), nodes as $MULTI_RESOLVED"
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

# expected_reason ID: the reason EXPECT_FAIL gives for the check ID (no @node), and status 0, when it expects
# the check to fail.
expected_reason() {
  local base=$1 e parts=()
  IFS=';' read -r -a parts <<<"$EXPECT_FAIL"
  for e in ${parts[@]+"${parts[@]}"}; do
    [[ -n $e && ${e%%=*} == "$base" ]] || continue
    [[ $e == *=* ]] && echo "${e#*=}" || echo ""
    return 0
  done
  return 1
}

# verdict: 0 when the failures are exactly the ones EXPECT_FAIL names, each for the reason it gives.
verdict() {
  local res id base detail reason rc=0
  while IFS=$'\t' read -r res id _ _ detail; do
    base=${id%@*}
    case $res in
      FAIL)
        if ! reason=$(expected_reason "$base"); then
          log "unexpected failure: $id"; rc=1
        elif [[ -z $reason ]]; then
          log "$id fails and EXPECT_FAIL gives no reason for it (write $base=REASON)"; rc=1
        elif [[ $detail != *"$reason"* ]]; then
          log "$id fails for another reason than the expected '$reason': $detail"; rc=1
        fi ;;
      PASS) ! expected_reason "$base" >/dev/null || { log "$id was expected to fail and passed"; rc=1; } ;;
    esac
  done <"$RESULTS"
  return $rc
}

finish() {
  local rc=$?
  trap - EXIT
  note "${FACT_PREFIX:-run}.total_seconds" "$SECONDS"
  mem_snapshot end 2>/dev/null || true
  multi_collect_logs
  write_results || true
  cat "$LOG_DIR/results.md" >&2 2>/dev/null || true
  multi_down
  [[ $rc -ne 0 ]] || verdict || rc=1
  exit $rc
}

# checks_init: the files the runner writes to; called once, after multi_init.
checks_init() {
  : >"$RESULTS"; : >"$FACTS"; : >"$LOG_DIR/timings.tsv"; : >"$LOG_DIR/memory.tsv"
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
  note "$n.systemd_version" "$(on "$n" systemctl --version | head -n1)"
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

