#!/usr/bin/env bash
# CI only: a branch with data cannot act on the outside world (workstream I, README "Outbound
# isolation"), under real systemd units.
#
#   sudo SBCTL_BIN=/path/to/sbctl-linux-amd64 tests/linux/branching-egress.sh
#
# The state directory (/var/lib/sbctl, where the unit templates look for it) is put on a loop-file
# XFS with reflink=1, so that `branches create --with-data` clones the parent's files and needs no
# base backup. A parent project gets pg_net, pg_cron, a cron job and data. A small HTTP server on
# the runner's own non-loopback address stands in for "an external host":
#
#   1. the parent's pg_net request reaches it (the control: the request is valid and the host is up);
#   2. a with-data branch (default) has IPAddressDeny/IPAddressAllow on its Postgres unit only,
#      reports egress=denied, its pg_net request to the same address fails and never reaches the
#      server, its pg_cron job is inactive and recorded, and the parent's job is untouched;
#   3. the restriction survives a pause and resume, and a reset (which recreates the cluster);
#   4. a branch created with --allow-egress reaches the server and keeps its cron job active;
#   5. deleting a branch lifts the restriction from the unit (a later project with the ref gets none).
#
# Runs as root on an ephemeral Ubuntu 24.04 VM with systemd. No Docker. Artifacts come from the
# releases pinned in versions.yaml (sbctl system init).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

HTTP_PORT=18080
HTTP_PID=
IMG=/mnt/sbctl-egress-xfs.img
[[ -d /mnt ]] || IMG=/var/tmp/sbctl-egress-xfs.img

cleanup() {
  rc=$?
  collect_logs
  [[ -n "$HTTP_PID" ]] && kill "$HTTP_PID" 2>/dev/null || true
  teardown
  if mountpoint -q "$SBCTL_STATE"; then umount "$SBCTL_STATE" || umount -l "$SBCTL_STATE" || true; fi
  losetup -j "$IMG" 2>/dev/null | cut -d: -f1 | xargs -r losetup -d || true
  rm -f "$IMG"
  exit $rc
}
trap cleanup EXIT

preflight
export DEBIAN_FRONTEND=noninteractive
apt-get install -y --no-install-recommends xfsprogs >/dev/null || fail "xfsprogs could not be installed"
mkdir -p "$SBCTL_STATE"
truncate -s 16G "$IMG"
LOOP=$(losetup --find --show "$IMG")
mkfs.xfs -q -m reflink=1 "$LOOP"
mount "$LOOP" "$SBCTL_STATE"
xfs_info "$SBCTL_STATE" | grep -o 'reflink=[01]' | grep -q 'reflink=1' || fail "the state directory is not XFS with reflink=1"

install_binary
setup_node
log "system init (downloads artifacts)"
system_init
wait_active sb-postgres@system.service 30

# ---- the "external host": the runner's own address, which is not loopback -------------------
HOSTIP=$(hostname -I | awk '{print $1}')
[[ -n "$HOSTIP" && "$HOSTIP" != 127.* ]] || fail "no non-loopback address on this runner (hostname -I: '$(hostname -I)')"
SRV=$(mktemp -d)
echo ok >"$SRV/index.html"
(cd "$SRV" && exec python3 -m http.server "$HTTP_PORT" --bind 0.0.0.0 >"$SRV/access.log" 2>&1) &
HTTP_PID=$!
for ((i = 0; i < 20; i++)); do
  [[ $(http_code "http://$HOSTIP:$HTTP_PORT/?who=probe") == 200 ]] && break
  sleep 0.5
done
[[ $(http_code "http://$HOSTIP:$HTTP_PORT/?who=probe") == 200 ]] || fail "the test HTTP server does not answer on $HOSTIP:$HTTP_PORT"
log "test server on $HOSTIP:$HTTP_PORT"

PSQL=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
sql() { # REF SQL: run SQL as the superuser over the cluster's own socket, print tuples only
  local ref=$1 port
  port=$(project_field "$ref" 'd["ports"]["Postgres"]')
  sudo -u "$SBCTL_USER" "$PSQL" "host=$SBCTL_STATE/projects/$ref/postgres/sock port=$port user=supabase_admin dbname=postgres" \
    -qAtX -v ON_ERROR_STOP=1 -c "$2" </dev/null
}
net_request() { # REF TAG: pg_net GET to the test server; prints "<status>|<error>" once the response row exists
  local ref=$1 tag=$2 id row=
  id=$(sql "$ref" "select net.http_get('http://$HOSTIP:$HTTP_PORT/?who=$tag')") || fail "$ref: net.http_get failed"
  for ((i = 0; i < 40; i++)); do
    row=$(sql "$ref" "select coalesce(status_code::text, ''), coalesce(error_msg, '') from net._http_response where id = $id")
    [[ -n "$row" ]] && break
    sleep 1
  done
  [[ -n "$row" ]] || fail "$ref: pg_net produced no response row for request $tag"
  echo "$row"
}
served() { grep -c "who=$1 " "$SRV/access.log" || true; } # requests the test server saw with this tag

unit_prop() { systemctl show -p "$1" --value "$2"; } # PROPERTY UNIT

# ---- the parent ----------------------------------------------------------------------------
A=$(create_project egress-parent micro)
log "parent $A"
sql "$A" "create extension if not exists pg_net; create extension if not exists pg_cron;
  create table public.items (id int primary key); insert into public.items select generate_series(1, 100);
  select cron.schedule('egress-test-job', '* * * * *', 'select 1');" >/dev/null || fail "$A: could not set up pg_net, pg_cron and data"
row=$(net_request "$A" parent)
[[ $row == "200|" ]] || fail "control: the parent's own pg_net request to the test server answered '$row', want '200|' (the test cannot tell anything otherwise)"
[[ $(served parent) -ge 1 ]] || fail "control: the test server never saw the parent's request"
log "control ok: the parent reaches $HOSTIP:$HTTP_PORT"

branch_json() { sbctl branches get "$1" --json | json_get "$2"; }
create_branch() { # NAME [extra flags]: prints the branch's project ref
  local name=$1; shift
  sbctl branches create "$A" "$name" --with-data --json "$@" | json_get 'd["project_ref"]'
}

# ---- default: egress denied -----------------------------------------------------------------
log "branch with data, default isolation"
B=$(create_branch egress-denied)
[[ $(branch_json "$B" 'd.get("egress", "")') == denied ]] || fail "$B: egress is '$(branch_json "$B" 'd.get("egress", "")')', want denied"
[[ $(branch_json "$B" 'd["clone_method"]') == reflink ]] || fail "$B: clone_method is $(branch_json "$B" 'd["clone_method"]'), want reflink"
PGB="sb-postgres@$B.service"
deny=$(unit_prop IPAddressDeny "$PGB") allow=$(unit_prop IPAddressAllow "$PGB")
[[ $deny == *0.0.0.0/0* && $deny == *::/0* ]] || fail "$PGB: IPAddressDeny is '$deny', want the whole address space"
[[ $allow == *127.0.0.0/8* && $allow == *::1* ]] || fail "$PGB: IPAddressAllow is '$allow', want loopback"
for svc in gotrue postgrest; do
  [[ -z $(unit_prop IPAddressDeny "sb-$svc@$B.service") ]] || fail "sb-$svc@$B has an egress restriction (only the cluster's Postgres should)"
done
[[ -z $(unit_prop IPAddressDeny "sb-postgres@$A.service") ]] || fail "the parent's Postgres has an egress restriction"
sbctl projects health "$B" || fail "$B: unhealthy behind the egress filter"
[[ $(sql "$B" "select count(*) from public.items") == 100 ]] || fail "$B: the clone lost data"

[[ $(sql "$B" "select active from cron.job where jobname = 'egress-test-job'") == f ]] || fail "$B: the parent's cron job is still active in the branch"
[[ $(sql "$B" "select count(*) from sbctl_branch.paused_cron_jobs p join cron.job j using (jobid) where j.jobname = 'egress-test-job'") == 1 ]] \
  || fail "$B: the paused cron job is not recorded in sbctl_branch.paused_cron_jobs"
[[ $(sql "$A" "select active from cron.job where jobname = 'egress-test-job'") == t ]] || fail "the parent's cron job was touched"

row=$(net_request "$B" branch-denied)
[[ $row == '|'?* ]] || fail "$B: a pg_net request to $HOSTIP:$HTTP_PORT answered '$row'; it must fail with no status and an error"
[[ $(served branch-denied) -eq 0 ]] || fail "$B: the test server saw the branch's request, so egress is not blocked"
log "$B: pg_net request failed as it must ($row)"

log "the restriction survives a pause and resume"
sbctl projects pause "$B"
sbctl projects resume "$B"
sbctl projects health "$B" || fail "$B: unhealthy after resume"
[[ $(unit_prop IPAddressDeny "$PGB") == *0.0.0.0/0* ]] || fail "$PGB: the restriction is gone after resume"
row=$(net_request "$B" branch-denied-after-resume)
[[ $row == '|'?* && $(served branch-denied-after-resume) -eq 0 ]] || fail "$B: after resume a pg_net request answered '$row' or reached the server"

log "a reset recreates the cluster and denies again"
sbctl branches reset "$B"
[[ $(branch_json "$B" 'd.get("egress", "")') == denied ]] || fail "$B: egress after reset is '$(branch_json "$B" 'd.get("egress", "")')', want denied"
[[ $(unit_prop IPAddressDeny "$PGB") == *0.0.0.0/0* ]] || fail "$PGB: no restriction after reset"
[[ $(sql "$B" "select active from cron.job where jobname = 'egress-test-job'") == f ]] || fail "$B: the cron job is active after reset"
row=$(net_request "$B" branch-denied-after-reset)
[[ $row == '|'?* && $(served branch-denied-after-reset) -eq 0 ]] || fail "$B: after reset a pg_net request answered '$row' or reached the server"

# ---- the opt-out ----------------------------------------------------------------------------
log "branch with --allow-egress"
C=$(create_branch egress-open --allow-egress)
[[ $(branch_json "$C" 'd.get("egress", "")') == allowed ]] || fail "$C: egress is '$(branch_json "$C" 'd.get("egress", "")')', want allowed"
[[ -z $(unit_prop IPAddressDeny "sb-postgres@$C.service") ]] || fail "$C: an opted-out branch has a restriction"
[[ $(sql "$C" "select active from cron.job where jobname = 'egress-test-job'") == t ]] || fail "$C: the opt-out did not keep the cron job active"
row=$(net_request "$C" branch-open)
[[ $row == "200|" ]] || fail "$C: with --allow-egress a pg_net request answered '$row', want '200|'"
log "$C: reaches the server as the parent does"

# ---- delete lifts the restriction -----------------------------------------------------------
log "delete the branches"
sbctl branches delete "$B"
sbctl branches delete "$C"
[[ -z $(unit_prop IPAddressDeny "$PGB") ]] || fail "$PGB: the restriction remains after the branch was deleted (a later project with the ref would inherit it)"
[[ $(unit_state "$PGB") == inactive ]] || fail "$PGB is $(unit_state "$PGB") after delete"

log "OK"
