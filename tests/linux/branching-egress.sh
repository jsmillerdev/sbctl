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
#   5. deleting a branch lifts the restriction from the unit (a later project with the ref gets none);
#   6. the loopback allow is 127.0.0.1 and ::1 only: from the branch's Postgres a pg_net request to
#      127.0.0.1 is answered (and the cluster's own clients, GoTrue and PostgREST, stay healthy)
#      and one to 127.0.0.2 (the rest of 127.0.0.0/8, where systemd-resolved's stub lives) is dropped;
#      The unit also hides the resolver socket, the D-Bus system bus and nscd (InaccessiblePaths in the unit
#      template, which the IP filter does not cover) and still hides /etc/sbctl (the master key, the config).
#      Cron jobs cloned from the parent name the branch's own port.
#   7. a parent with a loopback postgres_fdw server to another project and a Vault secret that holds
#      its own service key: in the branch the foreign table cannot write (the server is disabled and
#      its password dropped, recorded in sbctl_branch.paused_foreign_servers), the Vault secret holds
#      the branch's service key, the other project is not written to, and the parent's own foreign
#      table and secret are untouched. The same holds after a reset.
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
net_request() { # REF TAG [HOST]: pg_net GET to the test server; prints "<status>|<error>" once the response row exists
  local ref=$1 tag=$2 host=${3:-$HOSTIP} id row=
  id=$(sql "$ref" "select net.http_get('http://$host:$HTTP_PORT/?who=$tag')") || fail "$ref: net.http_get failed"
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
# No branch restriction: no IPAddressAllow and no deny of the whole address space. Every sb-* unit
# keeps the template's metadata-service deny (169.254.169.254, fd00:ec2::254), and a lifted branch
# restriction must restore it, not leave the list empty.
unconfined() { # UNIT
  local d a
  d=$(unit_prop IPAddressDeny "$1") a=$(unit_prop IPAddressAllow "$1")
  [[ $d != *0.0.0.0/0* && $d != *::/0* && -z $a && $d == *169.254.169.254* && $d == *fd00:ec2::254* ]]
}

# ---- the parent ----------------------------------------------------------------------------
A=$(create_project egress-parent micro)
log "parent $A"
sql "$A" "create extension if not exists pg_net; create extension if not exists pg_cron;
  create table public.items (id int primary key); insert into public.items select generate_series(1, 100);
  select cron.schedule('egress-test-job', '* * * * *', 'select 1');" >/dev/null || fail "$A: could not set up pg_net, pg_cron and data"

# A second project the parent reaches through a loopback postgres_fdw server, and a Vault secret that
# holds the parent's own service key (the cron-to-functions pattern keeps it there).
D=$(create_project egress-other micro)
DPORT=$(project_field "$D" 'd["ports"]["Postgres"]')
DPW=$(project_field "$D" 'd["keys"]["db_password"]' --show-keys)
A_SERVICE=$(project_field "$A" 'd["keys"]["service_role_key"]' --show-keys)
sql "$D" "create table public.victim (id int primary key, v text); insert into public.victim values (1, 'orig');" >/dev/null || fail "$D: could not set up the other project"
sql "$A" "create extension if not exists postgres_fdw;
  create server other_pg foreign data wrapper postgres_fdw options (host '127.0.0.1', port '$DPORT', dbname 'postgres');
  create user mapping for public server other_pg options (user 'postgres', password '$DPW');
  create foreign table public.victim_ft (id int, v text) server other_pg options (schema_name 'public', table_name 'victim');
  create extension if not exists supabase_vault cascade;
  select vault.create_secret('$A_SERVICE', 'parent_service_key');
  insert into public.victim_ft values (2, 'written by the parent');" >/dev/null || fail "$A: could not set up postgres_fdw and the Vault secret"
[[ $(sql "$D" "select count(*) from public.victim") == 2 ]] || fail "control: the parent's foreign table did not write to the other project"
log "control ok: the parent writes to $D through postgres_fdw"

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
# systemd prints a full-length prefix without it: 127.0.0.1/32 as 127.0.0.1, ::1/128 as ::1.
allow_norm=$(printf '%s\n' $allow | sed -e 's#/32$##' -e 's#/128$##' | LC_ALL=C sort | tr '\n' ' ')
[[ $allow_norm == "127.0.0.1 ::1 " ]] || fail "$PGB: IPAddressAllow is '$allow', want exactly 127.0.0.1 and ::1 (not 127.0.0.0/8: 127.0.0.53 is the DNS stub)"
# The IP filter does not cover unix sockets: the resolver's varlink socket, the D-Bus system bus and
# nscd's socket are hidden from the Postgres unit (InaccessiblePaths in the unit template: systemd 255
# cannot change that property of a unit over D-Bus), and the unit still hides /etc/sbctl.
check_hidden_paths() { # UNIT: the unit's property and, in its mount namespace, the sockets
  local unit=$1 hidden pid p
  hidden=$(unit_prop InaccessiblePaths "$unit")
  for p in /run/systemd/resolve/io.systemd.Resolve /run/dbus/system_bus_socket /run/nscd/socket; do
    [[ $hidden == *"$p"* ]] || fail "$unit: InaccessiblePaths is '$hidden', want it to hide $p"
  done
  [[ $hidden == */etc/sbctl* ]] || fail "$unit: InaccessiblePaths lost /etc/sbctl (the master key and config): '$hidden'"
  pid=$(unit_prop MainPID "$unit")
  [[ $pid -gt 0 ]] || fail "$unit: no main pid"
  # InaccessiblePaths puts an inaccessible node of the same type (a socket, mode 000) over the path.
  for p in /run/dbus/system_bus_socket /run/systemd/resolve/io.systemd.Resolve; do
    if [[ -S $p ]]; then
      [[ $(stat -c %a "/proc/$pid/root$p" 2>/dev/null) == 0 ]] || fail "$unit: $p is reachable inside the unit (mode $(stat -c %a "/proc/$pid/root$p" 2>&1), want 0)"
    fi
  done
}
check_hidden_paths "$PGB"
check_hidden_paths "sb-postgres@$A.service" # in the template, so the parent has it too
BPORT=$(project_field "$B" 'd["ports"]["Postgres"]')
for svc in gotrue postgrest; do
  unconfined "sb-$svc@$B.service" || fail "sb-$svc@$B has an egress restriction (only the cluster's Postgres should)"
done
unconfined "sb-postgres@$A.service" || fail "the parent's Postgres has an egress restriction"
sbctl projects health "$B" || fail "$B: unhealthy behind the egress filter"
[[ $(sql "$B" "select count(*) from public.items") == 100 ]] || fail "$B: the clone lost data"

[[ $(sql "$B" "select active from cron.job where jobname = 'egress-test-job'") == f ]] || fail "$B: the parent's cron job is still active in the branch"
[[ $(sql "$B" "select count(*) from sbctl_branch.paused_cron_jobs p join cron.job j using (jobid) where j.jobname = 'egress-test-job'") == 1 ]] \
  || fail "$B: the paused cron job is not recorded in sbctl_branch.paused_cron_jobs"
[[ $(sql "$A" "select active from cron.job where jobname = 'egress-test-job'") == t ]] || fail "the parent's cron job was touched"
# pg_cron stamps the parent's port into a job: in the branch the jobs name the branch's own cluster.
[[ $(sql "$B" "select count(*) from cron.job where nodeport <> $BPORT or nodename <> '127.0.0.1'") == 0 ]] || fail "$B: a cron job still names a node other than the branch ($BPORT)"

row=$(net_request "$B" branch-denied)
[[ $row == '|'?* ]] || fail "$B: a pg_net request to $HOSTIP:$HTTP_PORT answered '$row'; it must fail with no status and an error"
[[ $(served branch-denied) -eq 0 ]] || fail "$B: the test server saw the branch's request, so egress is not blocked"
log "$B: pg_net request failed as it must ($row)"

# The allow list is the two loopback addresses and nothing else: the test server listens on every
# address, so 127.0.0.1 (allowed) and 127.0.0.2 (the rest of 127.0.0.0/8, dropped) tell them apart.
row=$(net_request "$B" branch-lo1 127.0.0.1)
[[ $row == "200|" && $(served branch-lo1) -ge 1 ]] || fail "$B: a pg_net request to 127.0.0.1 answered '$row'; loopback must stay reachable"
row=$(net_request "$B" branch-lo2 127.0.0.2)
[[ $row == '|'?* && $(served branch-lo2) -eq 0 ]] || fail "$B: a pg_net request to 127.0.0.2 answered '$row' or reached the server; only 127.0.0.1 and ::1 may be allowed"
log "$B: 127.0.0.1 is reachable, 127.0.0.2 is not"
# Foreign servers and parent credentials in the data.
check_isolated_data() { # BRANCH: the foreign server is disabled, the Vault secret is the branch's own, the other project is untouched
  local b=$1 host passwords secret bsvc
  host=$(sql "$b" "select (select split_part(o, '=', 2) from unnest(srvoptions) o where o like 'host=%') from pg_foreign_server where srvname = 'other_pg'")
  [[ $host == /nonexistent/sbctl-branch-disabled ]] || fail "$b: the foreign server other_pg has host '$host', want it disabled"
  passwords=$(sql "$b" "select count(*) from pg_user_mappings m, unnest(m.umoptions) o where m.srvname = 'other_pg' and o like 'password=%'")
  [[ $passwords == 0 ]] || fail "$b: $passwords user mapping password(s) of other_pg remain"
  if sql "$b" "insert into public.victim_ft values (3, 'written by the branch')" >/dev/null 2>&1; then
    fail "$b: the foreign table accepted a write"
  fi
  [[ $(sql "$D" "select count(*) from public.victim") == 2 ]] || fail "$b: the other project $D was written to"
  [[ $(sql "$b" "select original_host || ':' || original_port || ' ' || passwords_dropped_for::text from sbctl_branch.paused_foreign_servers where server_name = 'other_pg'") == "127.0.0.1:$DPORT {public}" ]] \
    || fail "$b: other_pg is not recorded in sbctl_branch.paused_foreign_servers"
  [[ $(sql "$b" "select count(*) from sbctl_branch.paused_foreign_servers t where t::text like '%$DPW%'") == 0 ]] || fail "$b: the stored password is in sbctl_branch.paused_foreign_servers"
  bsvc=$(project_field "$b" 'd["keys"]["service_role_key"]' --show-keys)
  secret=$(sql "$b" "select decrypted_secret from vault.decrypted_secrets where name = 'parent_service_key'")
  [[ $bsvc != "$A_SERVICE" && -n $bsvc ]] || fail "$b: the branch has the parent's service key"
  [[ $secret == "$bsvc" ]] || fail "$b: the Vault secret is not the branch's service key (it is the parent's: $([[ $secret == "$A_SERVICE" ]] && echo yes || echo no))"
  [[ $(sql "$b" "select count(*) from sbctl_branch.rewritten_credentials where kind = 'vault_secret' and name = 'parent_service_key'") == 1 ]] || fail "$b: the rewritten secret is not recorded by name"
  [[ $(sql "$b" "select count(*) from sbctl_branch.rewritten_credentials t where t::text like '%$A_SERVICE%' or t::text like '%$bsvc%'") == 0 ]] || fail "$b: sbctl_branch.rewritten_credentials holds a key"
}
check_isolated_data "$B"
# The parent is untouched: its foreign table still writes, its secret is still its own key.
sql "$A" "insert into public.victim_ft values (4, 'parent again')" >/dev/null || fail "the parent's foreign table stopped working"
[[ $(sql "$D" "select count(*) from public.victim") == 3 ]] || fail "the parent's write after the branch did not reach the other project"
[[ $(sql "$A" "select decrypted_secret from vault.decrypted_secrets where name = 'parent_service_key'") == "$A_SERVICE" ]] || fail "the parent's Vault secret changed"
sql "$D" "delete from public.victim where id = 4" >/dev/null
log "$B: foreign server disabled, Vault secret is the branch's own, the parent's fdw and secret are untouched"

log "the restriction survives a pause and resume"
sbctl projects pause "$B"
sbctl projects resume "$B"
sbctl projects health "$B" || fail "$B: unhealthy after resume"
[[ $(unit_prop IPAddressDeny "$PGB") == *0.0.0.0/0* ]] || fail "$PGB: the restriction is gone after resume"
check_hidden_paths "$PGB"
row=$(net_request "$B" branch-denied-after-resume)
[[ $row == '|'?* && $(served branch-denied-after-resume) -eq 0 ]] || fail "$B: after resume a pg_net request answered '$row' or reached the server"

log "a reset recreates the cluster and denies again"
sbctl branches reset "$B"
[[ $(branch_json "$B" 'd.get("egress", "")') == denied ]] || fail "$B: egress after reset is '$(branch_json "$B" 'd.get("egress", "")')', want denied"
[[ $(unit_prop IPAddressDeny "$PGB") == *0.0.0.0/0* ]] || fail "$PGB: no restriction after reset"
check_hidden_paths "$PGB"
[[ $(sql "$B" "select active from cron.job where jobname = 'egress-test-job'") == f ]] || fail "$B: the cron job is active after reset"
check_isolated_data "$B"
row=$(net_request "$B" branch-denied-after-reset)
[[ $row == '|'?* && $(served branch-denied-after-reset) -eq 0 ]] || fail "$B: after reset a pg_net request answered '$row' or reached the server"

# ---- the opt-out ----------------------------------------------------------------------------
log "branch with --allow-egress"
C=$(create_branch egress-open --allow-egress)
[[ $(branch_json "$C" 'd.get("egress", "")') == allowed ]] || fail "$C: egress is '$(branch_json "$C" 'd.get("egress", "")')', want allowed"
unconfined "sb-postgres@$C.service" || fail "$C: an opted-out branch has a restriction"
[[ $(sql "$C" "select count(*) from cron.job where nodeport <> $(project_field "$C" 'd["ports"]["Postgres"]')") == 0 ]] || fail "$C: the opted-out branch's cron jobs name the parent's port"
[[ $(sql "$C" "select active from cron.job where jobname = 'egress-test-job'") == t ]] || fail "$C: the opt-out did not keep the cron job active"
row=$(net_request "$C" branch-open)
[[ $row == "200|" ]] || fail "$C: with --allow-egress a pg_net request answered '$row', want '200|'"
log "$C: reaches the server as the parent does"

# ---- delete lifts the restriction -----------------------------------------------------------
log "delete the branches"
sbctl branches delete "$B"
sbctl branches delete "$C"
unconfined "$PGB" || fail "$PGB: the restriction remains after the branch was deleted (a later project with the ref would inherit it)"
[[ $(unit_state "$PGB") == inactive ]] || fail "$PGB is $(unit_state "$PGB") after delete"

log "OK"
