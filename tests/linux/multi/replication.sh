#!/usr/bin/env bash
# The two-server end-to-end test of the replica release (design 3.5): two Incus nodes with a real systemd on
# one runner, Supavise installed on both, and a project's life across them: replicas, planned moves, a hard
# failure, the old leader's return, and the upgrade of both servers.
#
#   CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.1" -o /tmp/supavise ./cmd/supavise
#   CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.2" -o /tmp/supavise-next ./cmd/supavise
#   go build -o /tmp/releasetool ./deploy/releasetool
#   sudo MULTI_MODE=auto SUPAVISE_BIN=/tmp/supavise SUPAVISE_BIN_NEXT=/tmp/supavise-next \
#     SUPAVISE_RELEASETOOL=/tmp/releasetool tests/linux/multi/replication.sh
#
# Both binaries are built from this checkout; the second only reports another version, so that `supavise
# upgrade` has a release to move to. MULTI_MODE is vm, container, container-privileged or auto (a virtual
# machine where /dev/kvm works, else a privileged container: unit hardening needs it, see lib-multi.sh).
# The checks run in this order, each recorded as PASS, FAIL or SKIP (a check whose predecessor did not pass)
# in $LOG_DIR/results.md; the id is what the report and the log file in $LOG_DIR/checks call it.
#
# Bring-up (lib-checks.sh): incus, services, launch, boot, prep, net.
#
# 1. One server
#   install            n1 from this checkout: Garage as the backup backend (one prefix for the cluster), Storage's
#                      bucket in the configuration, a fence command, no TLS
#   claim              the install's claim token, a personal access token, the dashboard account
#   projects           two small projects through the Management API, each with a table of 100 rows, the first
#                      with a Storage object of 2 MiB
#   storage            `storage migrate --to s3`: Storage serves the object from the bucket
# 2. A second server
#   token              `supavise node token` on n1 (its daemon restarts once to listen on the peer port)
#   join               `install.sh --join-token-file` on n2; the node is active the moment the installer returns
#                      (the joiner stack of the AWS template signals success then)
#   cluster            the leader healthy, a mesh session each way, n2's copy of the registry shows the projects,
#                      n2 runs the pooler and parks the other shared services
#   follower-status    `supavise status` exits 0 on n2
# 3. Read replicas
#   replica-setup      POST /v1/projects/{ref}/read-replicas/setup for both projects: databases-statuses walks to
#                      ACTIVE_HEALTHY
#   replica-shapes     GET databases and databases-statuses have the fields Studio reads
#   replica-read       a write through the primary's Data API is read from the replica's endpoint within the lag
#                      budget; the replica refuses a write
#   replica-ddl        a new table is served by the replica's endpoint (the schema reload)
#   pooler             the pooler of n2 serves the replica (a standby) and the primary (through the forwarder)
#   lb                 the project's load balancer host answers with the route it took
# 4. Planned move of one project, with a continuous writer through n2's proxy
#   project-switchover `supavise projects failover <ref>`: n2 becomes its home, zero acknowledged rows lost
#   project-failback   and back to n1
# 5. Planned move of the server
#   server-switchover  `supavise failover --to n2` run on n1: n2 leads, the shared services start there, zero rows lost
#   server-failback    `supavise failover` on n1 again: n1 leads
# 6. A hard failure
#   hard-failover      n1 is stopped at once (`incus stop --force`) while the writer runs; `supavise failover --force`
#                      on n2 runs the fence command and promotes; RPO (acknowledged rows lost) and RTO (the
#                      writer's first acknowledgement after the stop) are printed
#   fenced             n1 returns: it records that it is fenced, runs no primary and answers 503
#   rejoin             `supavise node rejoin`: n1 follows n2 and its replicas come back
# 7. Upgrade
#   upgrade-leader     `supavise upgrade` to v0.0.2 on the leader: no PostgreSQL cluster restarts
#   upgrade-follower   and on the follower
#   status-final       `supavise status` exits 0 on the leader
#   follower-status-final
#                      `supavise status` exits 0 on the follower, a check of its own
#
# What a check needs, it names with `needs`; a state that a failed check may still have reached (a move that
# ended with an error but moved the leader) is marked with `reached`, so that the checks after it still run.
# Needs root and Ubuntu 24.04 with Docker; see lib-multi.sh. Logs, results.md and the per-check output are in
# $LOG_DIR (default /tmp/supavise-multi-logs): both nodes' journals and PostgreSQL logs, their state after each
# step and when a check fails, and the writer's files. The fake AWS service is not started: no check here talks to AWS.
: "${MULTI_MEM:=5GiB}"
export MULTI_MEM
# shellcheck source=lib-multi.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-multi.sh"
# shellcheck source=lib-checks.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-checks.sh"

RESULTS_TITLE="Two-server release test"
MULTI_FAKE_AWS=0                 # lib-checks.sh does not start the fake AWS service (the spike's fakeaws checks use it)
export MULTI_FAKE_AWS
FACT_PREFIX=replication
CHECK_ORDER="incus services launch boot prep net install claim projects storage token join cluster follower-status replica-setup replica-shapes replica-read replica-ddl pooler lb project-switchover project-failback server-switchover server-failback hard-failover fenced rejoin upgrade-leader upgrade-follower status-final follower-status-final"
CHECK_ON_FAIL=on_fail

# One prefix and one domain for the cluster: a joining server takes the leader's settings, and a domain left to the
# default follows each node's own address.
S3_PREFIX=cluster
MULTI_DOMAIN=cluster.test
export S3_PREFIX MULTI_DOMAIN
REGION=us-east-1                 # both servers are in the default region
LAG_BUDGET_S=20                  # a row written on the primary is read from the replica's endpoint within this
RPO_BUDGET_S=5                   # an unplanned failover loses the rows of the last seconds at most, whatever the replay lag was
RPO_SLACK_S=2                    # and at most the largest replay lag of the ten seconds before the stop plus this
WRITER_RATE=20                   # rows a second (writer.py)
RTO_BUDGET_S=300                 # from the stop of the leader to the first acknowledged write on the new one
MULTI_HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# ---- helpers of the runner --------------------------------------------------------------------------------
# onl NODE FUNCTION ARGS...: a function of node-lib.sh, run in the node as root.
onl() { local n=$1; shift; timeout "${ONL_TIMEOUT:-3000}" incus exec "$n" --env HOME=/root -- bash -c 'source /root/node-lib.sh && "$@"' _ "$@"; }

sset() { mkdir -p "$WORK/state/kv"; printf '%s' "$2" >"$WORK/state/kv/$1"; }   # KEY VALUE: what a check leaves for the later ones
sget() { cat "$WORK/state/kv/$1"; }
lead() { sget leader; }
follower() { if [[ $(sget leader) == n1 ]]; then echo n2; else echo n1; fi; }
next_run() { local r; r=$(($(cat "$WORK/state/kv/run" 2>/dev/null || echo 0) + 1)); sset run "$r"; echo "$r"; }
refs() { echo "$(sget ref1) $(sget ref2)"; }

# copy_secrets FROM TO: the keys, the passwords and the token of the API, between the nodes through this pipe.
copy_secrets() { on "$1" tar -C / -cf - root/keys root/pat root/pat.hdr root/org | on "$2" tar -C / -xf -; }

# snapshot LABEL: both nodes' view of the cluster, for reading afterwards.
snapshot() {
  local label=$1 n
  mkdir -p "$LOG_DIR/snapshots"
  for n in "${NODES[@]}"; do
    timeout 90 incus exec "$n" --env HOME=/root -- bash -c 'source /root/node-lib.sh && node_dump' >"$LOG_DIR/snapshots/$label-$n.txt" 2>&1 || true
  done
}
# on_fail ID: after a failed check, the writer stops (it would go on writing through the next steps) and the
# nodes are looked at.
on_fail() {
  timeout 30 incus exec n2 --env HOME=/root -- bash -c 'source /root/node-lib.sh && writers_stop_all' >/dev/null 2>&1 || true
  snapshot "$1-failed"
}
check_snap() { # ID DESCRIPTION FUNC: check, then a snapshot of the state it left
  check "$@"
  [[ ! -e $WORK/state/$1.ok ]] || snapshot "$1"
}

# writer_verdict RUN REF HOME [AFTER]: stops run RUN's writer on n2, fetches what it wrote and the ids of its rows that
# HOME's cluster of REF holds, compares them (writer-compare.py) and leaves the lines in $WORK/w/RUN.cmp.
writer_verdict() {
  local run=$1 ref=$2 home=$3 after=${4:-} d=$WORK/w
  mkdir -p "$d"
  onl n2 writer_stop "$run"
  on n2 cat "/root/writer/$run/acked" >"$d/$run.acked"
  on n2 cat "/root/writer/$run/failed" >"$d/$run.failed"
  onl "$home" primary_ids "$ref" "$run" >"$d/$run.db"
  python3 "$MULTI_HERE/writer-compare.py" --acked "$d/$run.acked" --failed "$d/$run.failed" --db "$d/$run.db" ${after:+--after "$after"} | tee "$d/$run.cmp"
}
cmp_get() { grep "^$2=" "$WORK/w/$1.cmp" | cut -d= -f2; } # RUN KEY

# no_lost_rows RUN: every acknowledged insert is in the table, and nothing is in it that nobody wrote.
no_lost_rows() {
  [[ $(cmp_get "$1" acked) -gt 100 ]] || fail "the writer got only $(cmp_get "$1" acked) acknowledgements"
  [[ $(cmp_get "$1" lost) == 0 ]] || fail "$(cmp_get "$1" lost) acknowledged rows are not in the table (window $(cmp_get "$1" lost_window_s) s)"
  [[ $(cmp_get "$1" phantom) == 0 && $(cmp_get "$1" duplicates) == 0 ]] || fail "the table holds rows nobody wrote, or an id twice: $(paste -sd' ' "$WORK/w/$1.cmp")"
}

# ---- 1. one server ---------------------------------------------------------------------------------------
c_install() {
  needs net@n1 net@n2
  local t0=$SECONDS d=$LOG_DIR/n1
  multi_push_tests n1
  multi_install n1 --claim-token-file /root/claim-token \
    --set failover.fencing=command --set failover.fence_command=/usr/local/lib/supavise-ci/fence.sh \
    --set "fleet.storage_s3_bucket=$S3_OBJECTS_BUCKET" --set "fleet.storage_s3_endpoint=http://$BRIDGE_IP:$S3_PORT" \
    --set fleet.storage_s3_region=us-east-1 --set fleet.storage_s3_force_path_style=true \
    || { tail -n 40 "$d/install.log"; fail "n1: install.sh --binary failed"; }
  stamp "n1: install.sh --binary" "$t0"
  grep -q "Supavise is running" "$d/install.log" || fail "n1: install.sh did not report success"
  on n1 bash -s -- "$S3_BUCKET" "$S3_PREFIX" <<'EOS' || fail "n1: the node is not what the install should have made"
source /root/lib.sh
for u in supavise.service supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service \
    supavise-supavisor.service supavise-realtime.service supavise-storage.service; do wait_active "$u" 120; done
grep -Eq "^backend = ['\"]s3://$1/$2['\"]" /etc/supavise/config.toml || fail "the backup backend is not s3://$1/$2: $(grep backend /etc/supavise/config.toml)"
grep -Eq "^fence_command = ['\"]/usr/local/lib/supavise-ci/fence.sh['\"]" /etc/supavise/config.toml || fail "no fence_command in config.toml"
[[ -x /usr/local/lib/supavise-ci/fence.sh ]] || fail "the fence command is not there"
EOS
  note "n1.version" "$(on n1 /usr/local/bin/supavise --version | head -n1)"
  echo "# $(on n1 /usr/local/bin/supavise --version | head -n1) installed; backups in s3://$S3_BUCKET/$S3_PREFIX on Garage; domain $MULTI_DOMAIN"
}

c_claim() {
  needs install
  onl n1 wait_api 120
  onl n1 claim_and_save
  echo "# claimed; the personal access token and the organization are in files on n1"
}

c_projects() {
  needs claim
  local out ref1 ref2 sha1
  out=$(onl n1 make_projects)
  ref1=$(grep '^ref1=' <<<"$out" | cut -d= -f2) ref2=$(grep '^ref2=' <<<"$out" | cut -d= -f2) sha1=$(grep '^sha1=' <<<"$out" | cut -d= -f2)
  [[ $ref1 =~ ^[a-z]{20}$ && $ref2 =~ ^[a-z]{20}$ && ${#sha1} == 64 ]] || fail "make_projects printed: $out"
  sset ref1 "$ref1"; sset ref2 "$ref2"; sset sha1 "$sha1"; sset leader n1
  onl n1 data_intact "$ref1" "$sha1"
  [[ $(onl n1 pg_local "$ref2" "select count(*) from public.items") == 100 ]] || fail "$ref2: the table has no 100 rows"
  [[ $(onl n1 home_of "$ref1") == n1 && $(onl n1 home_of "$ref2") == n1 ]] || fail "the projects are not homed on n1"
  echo "# projects $ref1 (small, a Storage object) and $ref2 (small), 100 rows each"
}

c_storage() {
  needs projects
  local ref1 sha1 objects
  ref1=$(sget ref1) sha1=$(sget sha1)
  onl n1 storage_to_s3
  onl n1 data_intact "$ref1" "$sha1"
  objects=$(docker exec garage /garage bucket info "$S3_OBJECTS_BUCKET" 2>&1 | grep -i -E '^ *Objects' | head -n1 | tr -s ' ')
  log "bucket $S3_OBJECTS_BUCKET: ${objects:-no object count}"
  [[ $objects =~ [1-9] ]] || fail "the Storage bucket holds no object: $(docker exec garage /garage bucket info "$S3_OBJECTS_BUCKET" 2>&1 | head -n 12)"
  echo "# Storage serves $ref1's object from s3://$S3_OBJECTS_BUCKET ($objects)"
}

# ---- 2. a second server ----------------------------------------------------------------------------------
c_token() {
  needs storage
  onl n1 make_join_token
  echo "# a join token on n1; its daemon listens on the peer port"
}

c_join() {
  needs token
  local t0=$SECONDS d=$LOG_DIR/n2 state
  multi_push_tests n2
  # The token goes from one node to the other through this pipe, into 0600 files: never on a command line.
  on n1 cat /root/join-token | on n2 bash -c 'umask 077; cat >/root/join-token'
  multi_install n2 --join-token-file /root/join-token \
    || { tail -n 60 "$d/install.log"; fail "n2: install.sh --join-token-file failed"; }
  stamp "n2: install.sh --join-token-file (the join, the first copy of the registry)" "$t0"
  on n2 rm -f /root/join-token
  on n1 rm -f /root/join-token
  # The installer joins before it returns, so the leader lists the node as active at once. The joiner stack of the
  # AWS template reports success when the installer returns.
  state=$(onl n1 node_state n2)
  [[ $state == active ]] || fail "n2: install.sh returned while the leader lists the node as '$state', not active"
  copy_secrets n1 n2
  echo "# n2 joined; the leader lists it as $state when install.sh returns ($((SECONDS - t0)) s)"
}

c_cluster() {
  needs join
  local ref1 ref2 n rows
  ref1=$(sget ref1) ref2=$(sget ref2)
  for n in n1 n2; do
    onl "$n" wait_nodes n1 180
    onl "$n" wait_peers 180
  done
  onl n1 wait_healthy 300
  onl n1 leader_services
  onl n2 follower_services
  # The registry is readable on n2, and its copy follows the leader's.
  rows=$(onl n2 reg "select ref || ' ' || node_id || ' ' || status from supavise.projects where ref in ('$ref1', '$ref2') order by ref")
  [[ $(grep -c ' n1 ACTIVE_HEALTHY$' <<<"$rows") == 2 ]] || fail "n2's registry shows: $rows"
  [[ $(onl n1 node_epoch) == "$(onl n2 node_epoch)" ]] || fail "the epoch differs between the nodes"
  note cluster.epoch "$(onl n1 node_epoch)"
  mem_snapshot cluster
  echo "# n1 leads at epoch $(onl n1 node_epoch) and is healthy; a session each way; n2 serves the registry and the pooler, and parks the other shared services"
}

# What `supavise status` says on a follower is its own question: a follower parks the shared services, and the
# checks that move the cluster do not wait for it.
c_follower_status() {
  needs cluster
  onl n2 wait_healthy 300
  echo "# supavise status exits 0 on the follower n2"
}

# ---- 3. read replicas ----------------------------------------------------------------------------------------
c_replica_setup() {
  needs cluster
  local ref r t0=$SECONDS
  for ref in $(refs); do onl n1 replica_request "$ref" "$REGION" >/dev/null; done
  for ref in $(refs); do onl n1 wait_replicas_api "$ref" 1 1500; done
  for ref in $(refs); do
    r=$(onl n1 replica_ids "$ref")
    [[ $r == "$ref"-rr-* ]] || fail "$ref: replica identifier '$r'"
    [[ $(onl n1 reg "select node_id from supavise.replicas where identifier = '$r'") == n2 ]] || fail "$ref: the replica is not on n2"
  done
  sset ident1 "$(onl n1 replica_ids "$(sget ref1)")"
  sset ident2 "$(onl n1 replica_ids "$(sget ref2)")"
  stamp "replicas of two projects: setup to ACTIVE_HEALTHY" "$t0"
  mem_snapshot replicas
  echo "# a replica of each project is ACTIVE_HEALTHY on n2 after $((SECONDS - t0)) s"
}

c_replica_shapes() {
  needs replica-setup
  local ref key
  for ref in $(refs); do onl n1 check_database_shapes "$ref" 1; done
  # `replicas ls` is a command of the leader (its help says so): it opens the registry for writing, which a follower's
  # standby refuses. It lists each replica on the other node.
  onl "$(lead)" supavise replicas ls
  for key in ident1 ident2; do onl "$(lead)" replica_listed "$(sget "$key")" "$(follower)"; done
}

c_replica_read() {
  needs replica-setup
  local ref ident id secs code
  ref=$(sget ref1) ident=$(sget ident1)
  id=$((9000000000 + RANDOM))
  onl n1 primary_write "$ref" "$id"
  secs=$(onl n2 replica_sees "$ident" "$ref" "$id" "$LAG_BUDGET_S")
  note replica.row_visible_seconds "$secs"
  code=$(onl n2 replica_refuses_write "$ident" "$ref")
  [[ $(onl n2 pg_local "$ref" "select pg_is_in_recovery()") == t ]] || fail "$ref: n2's cluster of the project is not a standby"
  echo "# a write on the primary was read from the replica's endpoint after $secs s (budget $LAG_BUDGET_S s); a write to the replica answered $code"
}

c_replica_ddl() {
  needs replica-read
  local ref ident secs
  ref=$(sget ref1) ident=$(sget ident1)
  onl n1 ddl_probe "$ref"
  secs=$(onl n2 replica_sees_table "$ident" "$ref" ddl_probe 60)
  note replica.ddl_visible_seconds "$secs"
  echo "# a new table was served by the replica's endpoint after $secs s"
}

c_pooler() {
  needs replica-setup
  local ref ident
  ref=$(sget ref1) ident=$(sget ident1)
  [[ $(onl n2 pooler_is_recovery "postgres.$ident" "$ref") == t ]] || fail "the pooler of n2 does not serve the replica $ident as a standby"
  [[ $(onl n2 pooler_is_recovery "postgres.$ref" "$ref") == f ]] || fail "the pooler of n2 does not serve the primary $ref as a primary"
  echo "# n2's pooler: postgres.<replica> is a standby, postgres.<ref> reaches the primary on n1"
}

c_lb() {
  needs replica-setup
  local ref route
  ref=$(sget ref1)
  route=$(onl n2 lb_route "$ref")
  [[ -n $route ]] || fail "the load balancer host of $ref answered without X-Supavise-Route"
  [[ $route == "$ref" || $route == "$ref"-rr-* ]] || fail "the load balancer sent the read to '$route'"
  echo "# the load balancer host of $ref routed a read to ${route/$ref/<ref>}"
}

# ---- 4. planned move of one project ----------------------------------------------------------------------------
# project_move FROM TO: `supavise projects failover` of the first project, with a writer running through n2's proxy, and
# a look afterwards: the new home, the replica on the old one, the data, no acknowledged row lost.
project_move() {
  local from=$1 to=$2 ref sha1 run lead t0
  ref=$(sget ref1) sha1=$(sget sha1) lead=$(lead)
  onl "$lead" wait_replicas "$ref" 1 600
  run=$(next_run)
  onl n2 writer_start "$ref" "$run"
  sleep 10
  onl "$lead" supavise projects failover "$ref" --to "$to" --dry-run || log "the dry run was refused (the move would be refused)"
  t0=$SECONDS
  onl "$lead" supavise projects failover "$ref" --to "$to" --yes
  note "project.$from-$to.seconds" "$((SECONDS - t0))"
  onl "$lead" wait_project "$ref" ACTIVE_HEALTHY 300
  [[ $(onl "$lead" home_of "$ref") == "$to" ]] || fail "$ref is homed on $(onl "$lead" home_of "$ref"), want $to"
  reached "project-moved-$to"
  [[ $(onl "$lead" last_move | cut -d'|' -f2,3,4,8) == "project|switchover|$ref|done" ]] || fail "the last move is $(onl "$lead" last_move)"
  [[ $(onl "$to" pg_local "$ref" "select pg_is_in_recovery()") == f ]] || fail "$ref: the cluster on $to is not a primary"
  onl "$lead" wait_replicas "$ref" 1 600
  [[ $(onl "$lead" reg "select node_id from supavise.replicas where ref = '$ref'") == "$from" ]] || fail "$ref: the replica is not on $from"
  [[ $(onl "$from" pg_local "$ref" "select pg_is_in_recovery()") == t ]] || fail "$ref: the old home $from does not follow"
  sleep 10
  writer_verdict "$run" "$ref" "$to"
  no_lost_rows "$run"
  onl n1 data_intact "$ref" "$sha1"
  onl n2 data_intact "$ref" "$sha1"
  onl n1 primary_write "$ref" $((9100000000 + RANDOM))
  note "project.$from-$to.writer_silence_seconds" "$(cmp_get "$run" max_gap_s)"
  echo "# $ref moved $from -> $to in $((SECONDS - t0)) s; $(cmp_get "$run" acked) rows acknowledged, none lost, the longest silence $(cmp_get "$run" max_gap_s) s"
}
c_project_switchover() { needs replica-read; project_move n1 n2; }
c_project_failback() { needs project-moved-n2; project_move n2 n1; }

# ---- 5. planned move of the server ---------------------------------------------------------------------------------
# server_move NEW RUNNER: moves the leadership to NEW with `supavise failover --to NEW` run on RUNNER, with a writer
# through n2's proxy, and looks at the cluster afterwards.
server_move() {
  local new=$1 runner=$2 old ref1 ref2 sha1 run t0 rc=0 epoch0 ref
  old=$(lead) ref1=$(sget ref1) ref2=$(sget ref2) sha1=$(sget sha1)
  epoch0=$(onl "$old" node_epoch)
  for ref in $ref1 $ref2; do onl "$old" wait_replicas "$ref" 1 600; done
  run=$(next_run)
  onl n2 writer_start "$ref1" "$run"
  sleep 10
  onl "$runner" supavise failover --to "$new" --dry-run || log "the dry run was refused (the move would be refused)"
  t0=$SECONDS
  onl "$runner" supavise failover --to "$new" --yes || rc=$?
  note "server.$old-$new.seconds" "$((SECONDS - t0))"
  log "supavise failover exited $rc after $((SECONDS - t0)) s"
  # The old leader's daemon restarts into the follower's role in the middle of a move that was started on it, so what
  # the move did is read from the new leader, whatever the command said.
  onl "$new" wait_nodes "$new" 600
  sset leader "$new"
  reached "server-moved-$new"
  [[ $(onl "$new" node_epoch) == $((epoch0 + 1)) ]] || fail "the epoch is $(onl "$new" node_epoch), want $((epoch0 + 1))"
  [[ $(onl "$new" last_move | cut -d'|' -f2,3,5,6,8) == "server|switchover|$old|$new|done" ]] || fail "the last move is $(onl "$new" last_move)"
  onl "$new" wait_api 300
  onl "$new" leader_services
  onl "$old" follower_services
  for ref in system $ref1 $ref2; do
    [[ $(onl "$new" home_of "$ref") == "$new" ]] || fail "$ref is homed on $(onl "$new" home_of "$ref"), want $new"
  done
  for ref in $ref1 $ref2; do onl "$new" wait_project "$ref" ACTIVE_HEALTHY 300; done
  for ref in $ref1 $ref2; do onl "$new" wait_replicas "$ref" 1 900; done
  sleep 10
  writer_verdict "$run" "$ref1" "$new"
  no_lost_rows "$run"
  onl "$new" data_intact "$ref1" "$sha1"
  onl "$new" wait_items_rest "$ref2" 100 120
  onl "$old" data_intact "$ref1" "$sha1"
  onl "$new" wait_healthy 300
  note "server.$old-$new.writer_silence_seconds" "$(cmp_get "$run" max_gap_s)"
  [[ $rc -eq 0 ]] || fail "supavise failover exited $rc, although the cluster did move to $new"
  echo "# the leader moved $old -> $new in $((SECONDS - t0)) s at epoch $(onl "$new" node_epoch); $(cmp_get "$run" acked) rows acknowledged, none lost, the longest silence $(cmp_get "$run" max_gap_s) s"
}
c_server_switchover() { needs project-moved-n1; server_move n2 n1; }
c_server_failback() { needs server-moved-n2; server_move n1 n1; }

# ---- 6. a hard failure ------------------------------------------------------------------------------------------------
c_hard_failover() {
  needs server-moved-n1
  local ref1 ref2 sha1 run tstop t0 rc=0 lag lost win rto epoch0 ref budget
  ref1=$(sget ref1) ref2=$(sget ref2) sha1=$(sget sha1)
  epoch0=$(onl n1 node_epoch)
  for ref in $ref1 $ref2; do onl n1 wait_replicas "$ref" 1 600; done
  run=$(next_run)
  onl n2 writer_start "$ref1" "$run"
  sleep 5
  lag=$(onl n1 replay_lag_max "$ref1" 10)
  note hard.replay_lag_before_seconds "$lag"
  log "n1 stops now (the largest replay lag of the project's replica in the last 10 s: $lag s)"
  multi_kill_node n1
  tstop=$(on n2 date +%s.%N)
  onl n2 supavise failover --force --dry-run || log "the dry run was refused (the move would be refused)"
  t0=$SECONDS
  onl n2 supavise failover --force --yes || rc=$?
  note hard.failover_command_seconds "$((SECONDS - t0))"
  log "supavise failover --force exited $rc after $((SECONDS - t0)) s"
  [[ $(onl n2 node_leader) == n2 ]] || fail "n2's registry names $(onl n2 node_leader) the leader"
  sset leader n2
  reached hard-moved
  onl n2 wait_api 300
  onl n2 leader_services
  [[ $(onl n2 node_epoch) == $((epoch0 + 1)) ]] || fail "the epoch is $(onl n2 node_epoch), want $((epoch0 + 1))"
  [[ $(onl n2 node_state n1) == fenced ]] || fail "n1 is '$(onl n2 node_state n1)' in n2's registry, want fenced"
  [[ $(onl n2 last_move | cut -d'|' -f2,3,5,6,8) == "server|failover|n1|n2|done" ]] || fail "the last move is $(onl n2 last_move)"
  [[ $(on n2 grep -c 'fence done' /var/lib/supavise/fence-test.log) -ge 1 ]] || fail "the fence command did not run to the end: $(on n2 cat /var/lib/supavise/fence-test.log)"
  on n2 grep 'fence called' /var/lib/supavise/fence-test.log | tail -n1 | grep -q "OLD_NODE_ID=n1.*NEW_NODE=.*PLANNED=0" \
    || fail "the fence command ran with another environment: $(on n2 cat /var/lib/supavise/fence-test.log)"
  for ref in system $ref1 $ref2; do
    [[ $(onl n2 home_of "$ref") == n2 ]] || fail "$ref is homed on $(onl n2 home_of "$ref"), want n2"
  done
  for ref in $ref1 $ref2; do onl n2 wait_project "$ref" ACTIVE_HEALTHY 300; done
  sleep 20
  writer_verdict "$run" "$ref1" n2 "$tstop"
  lost=$(cmp_get "$run" lost) win=$(cmp_get "$run" lost_window_s) rto=$(cmp_get "$run" rto_s)
  note hard.rpo_rows_lost "$lost"
  note hard.rpo_window_seconds "$win"
  note hard.rto_seconds "$rto"
  log "RPO: $lost acknowledged rows are not on the new primary (the last $win s of writes; the replay lag was $lag s); RTO: the first acknowledged write came $rto s after the stop"
  [[ $(cmp_get "$run" phantom) == 0 && $(cmp_get "$run" duplicates) == 0 ]] || fail "the table holds rows nobody wrote, or an id twice: $(paste -sd' ' "$WORK/w/$run.cmp")"
  [[ $(cmp_get "$run" acked) -gt 100 ]] || fail "the writer got only $(cmp_get "$run" acked) acknowledgements"
  [[ $rto != none ]] || fail "no write was acknowledged after the stop"
  # Asynchronous replication loses what the standby had not received: the writes of the replay lag, and a little more for
  # the stop itself. The budget is the lag measured before the stop and RPO_SLACK_S, and never more than RPO_BUDGET_S.
  budget=$(awk -v lag="$lag" -v slack="$RPO_SLACK_S" -v cap="$RPO_BUDGET_S" 'BEGIN {b = lag + slack; print (b > cap ? cap : b)}')
  note hard.rpo_budget_seconds "$budget"
  awk -v l="$lost" -v w="$win" -v r="$rto" -v b="$budget" -v rate="$WRITER_RATE" -v rb="$RTO_BUDGET_S" \
    'BEGIN {exit !(l <= rate * b && w <= b && r <= rb)}' \
    || fail "RPO $lost rows over $win s (budget $budget s: at most $(awk -v b="$budget" -v rate="$WRITER_RATE" 'BEGIN {print int(rate * b)}') rows; the replay lag was $lag s), RTO $rto s (budget $RTO_BUDGET_S s)"
  onl n2 data_intact "$ref1" "$sha1"
  onl n2 wait_items_rest "$ref2" 100 120
  onl n2 primary_write "$ref1" $((9200000000 + RANDOM))
  [[ $rc -eq 0 ]] || fail "supavise failover --force exited $rc, although n2 leads"
  echo "# RPO $lost rows ($win s of writes), RTO $rto s; the failover command took $((SECONDS - t0)) s; the fence command ran to the end; epoch $((epoch0 + 1))"
}

c_fenced() {
  needs hard-moved
  multi_start_node n1 >/dev/null
  onl n1 wait_fenced 300
  [[ $(on n1 python3 -c 'import json; print(json.load(open("/var/lib/supavise/fenced.json"))["leader"])') == n2 ]] \
    || fail "fenced.json does not name n2 the leader: $(on n1 cat /var/lib/supavise/fenced.json)"
  # The new leader lists the old one as fenced and does not take it back by itself.
  [[ $(onl n2 node_state n1) == fenced ]] || fail "n2 lists n1 as '$(onl n2 node_state n1)'"
  reached fenced-state
  on n1 bash -s <<'EOS' || fail "the fenced node does not say so in its status"
source /root/lib.sh
out=$(supavise status 2>&1 || true)
grep -q 'FENCED' <<<"$out" || { echo "$out" >&2; fail "supavise status does not say FENCED"; }
EOS
  echo "# n1 came back fenced: fenced.json names n2 as the leader, no PostgreSQL primary runs, the launchers are gone, port 80 answers 503"
}

c_rejoin() {
  needs fenced-state
  local ref t0=$SECONDS
  onl n1 supavise node rejoin
  onl n1 wait_unfenced 300
  onl n2 wait_nodes n2 300
  # The upgrade needs two active nodes, not the replicas that the rest of this check waits for up to 30 minutes.
  reached cluster-two-nodes
  [[ $(on n1 sh -c 'ls -d /var/lib/supavise/projects/*/postgres/data.diverged-* 2>/dev/null | wc -l') -ge 1 ]] || fail "the old data of n1 was not kept as data.diverged-<epoch>"
  for ref in $(refs); do onl n2 wait_replicas "$ref" 1 1800; done
  onl n1 follower_services
  onl n2 wait_healthy 300
  echo "# n1 follows n2 again after $((SECONDS - t0)) s; its replicas are ACTIVE_HEALTHY; the old data is kept as data.diverged-<epoch>"
}

# ---- 7. upgrade ------------------------------------------------------------------------------------------------------
# upgrade_one NODE: supavise upgrade to v0.0.2 on NODE, which restarts no PostgreSQL cluster, and the cluster after it.
upgrade_one() {
  local n=$1 lead other ref ident id secs
  lead=$(lead)
  node_push "$n" "$WORK/keys/pub.pem" /root/release-pub.pem
  onl "$n" unit_stamp >"$WORK/stamp-$n.before"
  # A stamp that is empty, or that holds no start times, would be equal to the one after the upgrade whatever happened:
  # it names the system cluster and the two projects, each with the time its postmaster started.
  [[ $(wc -l <"$WORK/stamp-$n.before") -ge 3 ]] && ! grep -q ' none$' "$WORK/stamp-$n.before" \
    || fail "$n: the stamp of the PostgreSQL clusters is not complete: $(cat "$WORK/stamp-$n.before")"
  log "$n: $(on "$n" /usr/local/bin/supavise --version | head -n1) -> v0.0.2; PostgreSQL clusters here: $(wc -l <"$WORK/stamp-$n.before")"
  on "$n" timeout 3000 /usr/local/bin/supavise upgrade --repo o/r --api-base "http://$BRIDGE_IP:$RELEASE_PORT" \
    --public-key-file /root/release-pub.pem --yes --version v0.0.2 \
    || fail "$n: supavise upgrade exited $?"
  [[ $(on "$n" /usr/local/bin/supavise --version) == *v0.0.2* ]] || fail "$n: the installed binary is $(on "$n" /usr/local/bin/supavise --version)"
  [[ $(onl "$n" daemon_version) == *v0.0.2* ]] || fail "$n: the daemon runs $(onl "$n" daemon_version)"
  [[ $n != "$lead" ]] || onl "$n" wait_healthy 300
  onl "$n" unit_stamp >"$WORK/stamp-$n.after"
  diff -u "$WORK/stamp-$n.before" "$WORK/stamp-$n.after" || fail "$n: a PostgreSQL cluster was restarted by the upgrade"
  echo "# $n: $(wc -l <"$WORK/stamp-$n.after") PostgreSQL clusters kept their invocation and their postmaster"
  # The cluster as it was: two active nodes and a session between them; and, when `rejoin` brought the replicas back,
  # the replicas healthy and a row written on the primary read from a replica.
  onl "$lead" wait_nodes "$lead" 300
  onl "$n" wait_peers 180
  [[ -e $WORK/state/rejoin.ok ]] || { echo "# the replicas were not looked at after the upgrade: rejoin did not pass"; return 0; }
  for ref in $(refs); do onl "$lead" wait_replicas "$ref" 1 600; done
  ref=$(sget ref1)
  other=$(follower)
  ident=$(onl "$lead" replica_ids "$ref")
  id=$((9300000000 + RANDOM))
  onl "$lead" primary_write "$ref" "$id"
  secs=$(onl "$other" replica_sees "$ident" "$ref" "$id" "$LAG_BUDGET_S")
  echo "# a row written on the primary reached the replica in $secs s"
}

c_upgrade_leader() {
  needs cluster-two-nodes
  local lead
  lead=$(lead)
  [[ -x ${SUPAVISE_BIN_NEXT:-} ]] || fail "SUPAVISE_BIN_NEXT: a Linux build of this checkout, built with -X main.version=v0.0.2"
  [[ $("$SUPAVISE_BIN_NEXT" --version) == *v0.0.2* ]] || fail "SUPAVISE_BIN_NEXT must report v0.0.2: $("$SUPAVISE_BIN_NEXT" --version)"
  (cd "$REPO_ROOT" && make_release v0.0.2 "$SUPAVISE_BIN_NEXT")
  upgrade_one "$lead"
  echo "# $lead (the leader) runs v0.0.2; no PostgreSQL cluster restarted"
}

c_upgrade_follower() {
  needs upgrade-leader
  local f lead
  f=$(follower) lead=$(lead)
  upgrade_one "$f"
  # The leader writes a peer's version into the registry on its next ping, a few seconds after the peer's daemon is back.
  onl "$lead" wait_node_versions v0.0.2 60 n1 n2
  echo "# $f (the follower) runs v0.0.2 too; both nodes report v0.0.2; no PostgreSQL cluster restarted"
}

# The cluster after the upgrade: the leader's status gates; the follower's is its own check, like follower-status, so that
# a follower that reports degraded does not hide the leader's state.
c_status_final() {
  needs upgrade-follower
  onl "$(lead)" wait_healthy 300
  echo "# supavise status exits 0 on the leader $(lead) after the upgrade"
}

c_follower_status_final() {
  needs upgrade-follower
  onl "$(follower)" wait_healthy 300
  echo "# supavise status exits 0 on the follower $(follower) after the upgrade"
}

# ---- run -------------------------------------------------------------------------------------------------------------
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
note node.memory "$MULTI_MEM"
[[ $MULTI_MODE == container* ]] || kvm_enable
multi_resolve_mode
note mode "$MULTI_RESOLVED"
mem_snapshot start 2>/dev/null || true

check incus "Incus is installed and the bridge is up" c_incus
check services "Garage and the release server listen on the bridge" c_services
check launch "both nodes are created and started" c_launch
check_each boot "systemd boots" c_boot
check_each prep "apt packages install" c_prep
check_each net "internet and bridge services are reachable" c_net
mem_snapshot booted

check install "n1 is installed from this checkout" c_install
check claim "n1 is claimed and has a personal access token" c_claim
check projects "two small projects with data, one with a Storage object" c_projects
check storage "Storage serves the object from the S3 bucket" c_storage
check token "n1 makes a join token" c_token
check join "n2 joins with install.sh --join-token-file" c_join
check_snap cluster "the leader is healthy, a session each way, n2 reads the registry" c_cluster
check follower-status "supavise status is healthy on the follower" c_follower_status
check_snap replica-setup "read replicas of both projects reach ACTIVE_HEALTHY through the Management API" c_replica_setup
check replica-shapes "GET databases and databases-statuses have the shapes Studio reads" c_replica_shapes
check replica-read "a write on the primary is read from the replica's endpoint within the lag budget" c_replica_read
check replica-ddl "a new table is served by the replica's endpoint" c_replica_ddl
check pooler "the pooler of n2 serves the replica and the primary" c_pooler
check lb "the load balancer host routes a read" c_lb
check_snap project-switchover "projects failover moves the project to n2 with no acknowledged row lost" c_project_switchover
check_snap project-failback "projects failover moves it back to n1" c_project_failback
check_snap server-switchover "failover --to n2 makes n2 the leader with no acknowledged row lost" c_server_switchover
check_snap server-failback "failover makes n1 the leader again" c_server_failback
check_snap hard-failover "failover --force promotes n2 after n1 is stopped at once; RPO and RTO" c_hard_failover
check fenced "n1 returns fenced and runs no primary" c_fenced
check_snap rejoin "node rejoin brings n1 back as a follower" c_rejoin
check_snap upgrade-leader "supavise upgrade of the leader restarts no PostgreSQL cluster" c_upgrade_leader
check_snap upgrade-follower "supavise upgrade of the follower restarts no PostgreSQL cluster" c_upgrade_follower
check status-final "supavise status is healthy on the leader" c_status_final
check follower-status-final "supavise status is healthy on the follower" c_follower_status_final

for n in "${NODES[@]}"; do note "$n.root_used_mb_at_end" "$(on "$n" df -BM --output=used / 2>/dev/null | tail -n1 | tr -dc 0-9 || true)"; done
note garage.bucket "$(docker exec garage /garage bucket info "$S3_BUCKET" 2>&1 | grep -i -E 'objects|size' | paste -sd ' ' - || true)"
log "done in $SECONDS s"
