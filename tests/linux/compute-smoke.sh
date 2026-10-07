#!/usr/bin/env bash
# Per-project compute sizes under real systemd units: what Studio's Compute and Disk page, the
# Supabase CLI and `supavise projects resize` call.
#
#   sudo SUPAVISE_BIN=/path/to/supavise-linux-amd64 tests/linux/compute-smoke.sh [--teardown]
#
# The data volume is a loop-mounted XFS filesystem with prjquota at /var/lib/supavise, so the
# disk size of a project can be enforced too. Two projects are created through the Management API
# (Micro, the default). The first is resized through PATCH /v1/projects/{ref}/billing/addons from
# Micro to Small: the project shows RESIZING at once, a second change is refused with 409, it ends
# ACTIVE_HEALTHY, and the unit's MemoryMax and CPUQuota, Postgres's shared_buffers, max_connections
# and work_mem, and the Supavisor tenant's pool all changed, the data survived, and the other
# project's Postgres did not restart. Then Medium and Large; a size the node cannot hold (16XL on a
# 4-core runner) is refused with 400 and changes nothing; a downsize that Postgres cannot start on
# (Large to Small with more replication slots in use than Small allows) fails and puts Large back.
# `supavise projects sizes` and `projects resize` work while the daemon runs, and the project's
# disk size is set through POST /v1/projects/{ref}/config/disk: the XFS project quota is in force
# (a write past it fails with ENOSPC while the volume has room).
#
# Without SUPAVISE_BIN the script builds supavise with the go toolchain. It needs network access
# for the artifact downloads. Not run in development (root, systemd and Linux required); CI runs it
# on an ephemeral Ubuntu 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
IMG=/var/tmp/supavise-compute.img
LOOP=""
cleanup_volume() {
  systemctl stop 'supavise-*' supavise.service 2>/dev/null || true
  mountpoint -q "$SUPAVISE_STATE" 2>/dev/null && umount "$SUPAVISE_STATE" 2>/dev/null || true
  [[ -n $LOOP ]] && losetup -d "$LOOP" 2>/dev/null || true
  rm -f "$IMG"
}
trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; cleanup_volume; exit $rc' EXIT

# Ports away from anything the runner may listen on, as in settings-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000
ADMIN=http://127.0.0.1:7000

preflight

log "the data volume: XFS with prjquota at $SUPAVISE_STATE"
export DEBIAN_FRONTEND=noninteractive
command -v mkfs.xfs >/dev/null && command -v xfs_quota >/dev/null || { apt-get update -qq || true; apt-get install -y -qq xfsprogs >/dev/null || fail "xfsprogs"; }
truncate -s 8G "$IMG"
LOOP=$(losetup --find --show "$IMG")
mkfs.xfs -q "$LOOP"
mkdir -p "$SUPAVISE_STATE"
mount -o prjquota "$LOOP" "$SUPAVISE_STATE"
grep -E " $SUPAVISE_STATE xfs .*(prjquota|pquota)" /proc/mounts >/dev/null || fail "the data volume is not mounted with prjquota: $(grep " $SUPAVISE_STATE " /proc/mounts)"

install_binary
setup_node
cat >>"$SUPAVISE_CONF" <<CONF

[ports]
supavisor_session = $P_SESSION
supavisor_transaction = $P_TRANSACTION
realtime = $P_REALTIME
storage = $P_STORAGE
storage_admin = $P_STORAGE_ADMIN
pgmeta = $P_PGMETA
studio = $P_STUDIO

[fleet]
supavisor_api_port = $P_API
CONF

# Same VM-local workaround as settings-smoke.sh: the Postgres launcher writes into the artifact
# directory on its first boot, which ProtectSystem=strict forbids.
install -d /etc/systemd/system/supavise-postgres@.service.d
cat >/etc/systemd/system/supavise-postgres@.service.d/10-compute-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/supavise/artifacts
CONF
systemctl daemon-reload

log "system init, fleet, daemon"
system_init
wait_active supavise-postgres@system.service 30
supavise fleet start || fail "fleet start"
systemctl start supavise.service
for ((i = 0; i < 60; i++)); do
  [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && break
  sleep 1
done
[[ $(http_code "$ADMIN/v1/projects") == 401 ]] || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "the Management API does not answer on the admin listener"; }
claim_and_token
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' | json_get 'd["access_token"]') || fail "dashboard sign-in"
japi() { local m=$1 p=$2; shift 2; api "$m" "$p" -H "Authorization: Bearer $JWT" "$@"; }

log "two projects through the Management API (Micro, the default)"
gen_dbpass
REF=$(api_create_project compute-smoke)
OTHER=$(api_create_project compute-other)
PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
pg_port() { project_field "$1" 'd["ports"]["Postgres"]'; }
sql() { # REF SQL: as supabase_admin over the cluster's private socket
  sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/$1/postgres/sock port=$(pg_port "$1") user=supabase_admin dbname=postgres" -Atc "$2" </dev/null
}
sysql() { sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/system/postgres/sock port=5433 user=supabase_admin dbname=$1" -Atc "$2" </dev/null; }
# Supavisor keeps its tenants in the _supavisor database of the system cluster.
pool_row() { sysql _supavisor "select t.default_pool_size || ',' || t.default_max_clients || ',' || u.pool_size from _supavisor.tenants t join _supavisor.users u on u.tenant_external_id = t.external_id and u.is_manager where t.external_id = '$1'" 2>&1 || true; }
unit_prop() { systemctl show -p "$2" --value "supavise-postgres@$1.service"; }
status() { papi GET "/v1/projects/$REF" | json_get 'd["status"]'; }
size_of() { japi GET "/platform/projects/$REF" | json_get 'd["infra_compute_size"]'; }
code() { local m=$1 p=$2 b=${3:-}; papi "$m" "$p" -o /dev/null -w '%{http_code}' ${b:+-H 'Content-Type: application/json' -d "$b"} || true; }
must() { # STATUS METHOD PATH [BODY]
  local want=$1 got
  got=$(code "$2" "$3" "${4:-}")
  [[ $got == "$want" ]] || { log "response: $(papi "$2" "$3" ${4:+-H 'Content-Type: application/json' -d "$4"} | head -c 600)"; fail "$2 $3 answered $got, want $want"; }
}
wait_healthy() { # SECONDS
  local n=${1:-300} s=""
  for ((i = 0; i < n; i++)); do
    s=$(status 2>/dev/null || true)
    [[ $s == ACTIVE_HEALTHY ]] && return 0
    sleep 1
  done
  journalctl --no-pager -u supavise.service | tail -40 >&2
  fail "$REF is $s after ${n}s, want ACTIVE_HEALTHY"
}
change() { # VARIANT: ask for a size and expect the answer 200
  must 200 PATCH "/v1/projects/$REF/billing/addons" "{\"addon_type\":\"compute_instance\",\"addon_variant\":\"$1\"}"
}
expect_size() { # NAME MEMORY_BYTES CPUQUOTA SHARED_BUFFERS MAX_CONNECTIONS WORK_MEM POOL_ROW
  [[ $(size_of) == "$1" ]] || fail "infra_compute_size is $(size_of), want $1"
  [[ $(unit_prop "$REF" MemoryMax) == "$2" ]] || fail "$1: MemoryMax is $(unit_prop "$REF" MemoryMax), want $2"
  [[ $(unit_prop "$REF" CPUQuotaPerSecUSec) == "$3" ]] || fail "$1: CPUQuota is $(unit_prop "$REF" CPUQuotaPerSecUSec), want $3"
  [[ $(sql "$REF" "show shared_buffers") == "$4" ]] || fail "$1: shared_buffers is $(sql "$REF" "show shared_buffers"), want $4"
  [[ $(sql "$REF" "show max_connections") == "$5" ]] || fail "$1: max_connections is $(sql "$REF" "show max_connections"), want $5"
  [[ $(sql "$REF" "show work_mem") == "$6" ]] || fail "$1: work_mem is $(sql "$REF" "show work_mem"), want $6"
  [[ $(sql "$REF" "show max_worker_processes") -ge 16 ]] || fail "$1: max_worker_processes is below the pg_cron floor"
  [[ $(pool_row "$REF") == "$7" ]] || fail "$1: Supavisor's tenant is '$(pool_row "$REF")', want $7"
}

log "Micro: the size a project starts with"
sql "$REF" "create table public.compute_smoke (id int primary key, label text); insert into public.compute_smoke values (1, 'before')" >/dev/null || fail "create table"
expect_size micro 1073741824 1s 256MB 60 4MB 20,200,20
ADDONS=$(japi GET "/platform/projects/$REF/billing/addons")
[[ $(json_get '[a["variant"]["identifier"] for a in d["selected_addons"] if a["type"] == "compute_instance"][0]' <<<"$ADDONS") == ci_micro ]] || fail "selected add-on: $ADDONS"
OFFERED=$(json_get '",".join(v["identifier"] for a in d["available_addons"] if a["type"] == "compute_instance" for v in a["variants"])' <<<"$ADDONS")
[[ $OFFERED == *ci_small* && $OFFERED == *ci_large* && $OFFERED != *ci_16xlarge* ]] || fail "the sizes offered: $OFFERED"
OTHER_PID=$(unit_prop "$OTHER" MainPID)

log "Micro to Small: RESIZING at once, a second change refused, healthy again on the new size"
change ci_small
[[ $(status) == RESIZING ]] || fail "the project is $(status) right after the change, want RESIZING"
[[ $(japi GET "/platform/projects/$REF/status" | json_get 'd["status"]') == RESIZING ]] || fail "/platform status is not RESIZING"
must 409 PATCH "/v1/projects/$REF/billing/addons" '{"addon_type":"compute_instance","addon_variant":"ci_large"}'
wait_healthy
expect_size small 2147483648 1s 512MB 90 5MB 35,400,35
[[ $(sql "$REF" "select label from public.compute_smoke where id = 1") == before ]] || fail "the data did not survive the resize"
[[ $(unit_prop "$OTHER" MainPID) == "$OTHER_PID" ]] || fail "the other project's Postgres restarted"
log "the project answers through the proxy after the resize"
project_keys "$REF"
for ((i = 0; i < 30; i++)); do
  [[ $(curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Host: $REF.api.$SUPAVISE_DOMAIN" -H "apikey: $PUB" "http://127.0.0.1/auth/v1/settings") == 200 ]] && break
  sleep 1
done
[[ $(curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Host: $REF.api.$SUPAVISE_DOMAIN" -H "apikey: $PUB" "http://127.0.0.1/auth/v1/settings") == 200 ]] || fail "GoTrue does not answer after the resize"
[[ $(curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Host: $REF.api.$SUPAVISE_DOMAIN" -H "apikey: $SEC" "http://127.0.0.1/rest/v1/") == 200 ]] || fail "PostgREST does not answer after the resize"

log "Medium (the platform route Studio calls), then Large"
[[ $(japi POST "/platform/projects/$REF/billing/addons" -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
  -d '{"addon_type":"compute_instance","addon_variant":"ci_medium"}') == 201 ]] || fail "POST platform billing/addons"
wait_healthy
expect_size medium 4294967296 2s 1GB 120 8MB 45,600,45
change ci_large
wait_healthy
expect_size large 8589934592 2s 2GB 160 12MB 60,800,60

log "a size the node cannot hold is refused and changes nothing"
BAD=$(papi PATCH "/v1/projects/$REF/billing/addons" -H 'Content-Type: application/json' -w '\n%{http_code}' -d '{"addon_type":"compute_instance","addon_variant":"ci_16xlarge"}')
[[ $(tail -1 <<<"$BAD") == 400 && $BAD == *"cannot run a 16XL project"* ]] || fail "16XL on this node: $BAD"
[[ $(status) == ACTIVE_HEALTHY && $(size_of) == large ]] || fail "a refused change touched the project"
supavise projects sizes "$REF" | tee "$LOG_DIR/sizes.txt" >/dev/null
grep -q '^16xlarge .* no$' "$LOG_DIR/sizes.txt" && grep -q '^large .* current$' "$LOG_DIR/sizes.txt" || fail "projects sizes: $(cat "$LOG_DIR/sizes.txt")"
{ supavise status 2>&1 || true; } | grep -q 'capacity' || fail "supavise status has no capacity line"

log "a downsize Postgres cannot start on puts the previous size back"
# Small allows 5 replication slots; 6 are in use. Postgres refuses to start with fewer slots than exist.
sql "$REF" "select pg_create_physical_replication_slot('compute_smoke_' || i) from generate_series(1, 6) i" >/dev/null || fail "create slots"
change ci_small
[[ $(status) == RESIZING ]] || fail "not RESIZING after the change that must fail"
wait_healthy 400
[[ $(size_of) == large ]] || fail "after a failed resize the project reports $(size_of), want large"
expect_size large 8589934592 2s 2GB 160 12MB 60,800,60
[[ $(sql "$REF" "select count(*) from pg_replication_slots where slot_name like 'compute_smoke_%'") == 6 ]] || fail "the slots are gone"
journalctl --no-pager -u supavise.service | grep -q 'compute: resize failed' || fail "the daemon did not log the failed resize"
sql "$REF" "select pg_drop_replication_slot(slot_name) from pg_replication_slots where slot_name like 'compute_smoke_%'" >/dev/null
[[ $(sql "$REF" "select label from public.compute_smoke where id = 1") == before ]] || fail "the data did not survive the failed resize"

log "the CLI resizes while the daemon runs: Large to Micro"
supavise projects resize "$REF" --size micro || fail "supavise projects resize"
wait_healthy 60
expect_size micro 1073741824 1s 256MB 60 4MB 20,200,20
[[ $(supavise projects resize "$REF" --size micro) == *"already Micro"* ]] || fail "resizing to the size it has"

log "the project's disk size is a quota in force on the XFS volume"
DISK=$(papi GET "/v1/projects/$REF/config/disk")
[[ $(json_get 'd["attributes"]["type"]' <<<"$DISK") == gp3 ]] || fail "GET config/disk: $DISK"
must 400 POST "/v1/projects/$REF/config/disk" '{"attributes":{"type":"gp3","size_gb":100000,"iops":3000}}'
must 201 POST "/v1/projects/$REF/config/disk" '{"attributes":{"type":"gp3","size_gb":1,"iops":3000}}'
SEQ=$(sysql supavise "select seq from supavise.projects where ref = '$REF'")
PID_Q=$((100000 + SEQ))
xfs_quota -x -c "report -p -b -N" "$SUPAVISE_STATE" | awk -v id="#$PID_Q" '$1 == id {print $4}' | grep -qx 1048576 || { xfs_quota -x -c "report -p -b" "$SUPAVISE_STATE" >&2; fail "no 1 GB hard limit for project $PID_Q"; }
UTIL=$(papi GET "/v1/projects/$REF/config/disk/util")
[[ $(json_get 'd["metrics"]["fs_size_bytes"]' <<<"$UTIL") == 1073741824 && $(json_get 'd["metrics"]["fs_used_bytes"]' <<<"$UTIL") -gt 0 ]] || fail "disk util under a quota: $UTIL"
FILL=$(sudo -u "$SUPAVISE_USER" dd if=/dev/zero of="$SUPAVISE_STATE/projects/$REF/fill" bs=1M count=1200 2>&1 || true)
# XFS answers a project quota that is full with ENOSPC ("No space left on device"), not EDQUOT. That the
# volume itself has room is what makes the failure the quota's.
FREE_KB=$(df --output=avail -k "$SUPAVISE_STATE" | tail -1 | tr -dc 0-9)
rm -f "$SUPAVISE_STATE/projects/$REF/fill"
[[ $FILL == *"No space left on device"* || $FILL == *"quota exceeded"* ]] || fail "a write past the quota did not fail: $FILL"
(( FREE_KB > 2 * 1024 * 1024 )) || fail "the volume itself is full ($FREE_KB KB free), so the quota proves nothing"
# A cluster that hit the quota while the file was written may be recovering for a moment.
for ((i = 0; i < 60; i++)); do
  [[ $(sql "$REF" "select label from public.compute_smoke where id = 1" 2>/dev/null || true) == before ]] && break
  sleep 1
done
[[ $(sql "$REF" "select label from public.compute_smoke where id = 1") == before ]] || fail "the data did not survive"
log "compute smoke: done"
