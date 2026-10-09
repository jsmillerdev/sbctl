#!/usr/bin/env bash
# Backups and point-in-time restore through the Management API, under real systemd units: what
# the dashboard's Backups pages and `supabase` clients call.
#
#   sudo SUPAVISE_BIN=/path/to/supavise-linux-amd64 tests/linux/backups-smoke.sh [--teardown]
#
# A project is created through the API and rows are written in it (row 1, a base backup, row 2, a
# time T, row 3). The backup list then shows the base backup and a restorable span that contains T.
# A restore to T through POST /v1/projects/{ref}/database/backups/restore-pitr answers 201 with
# the project RESTORING, refuses a second restore with 409 while it runs, and ends with the project
# ACTIVE_HEALTHY holding rows 1 and 2 and not row 3. Then the same project is restored from the
# listed base backup (POST .../backups/restore with its id) and holds row 1 only. Times outside the
# span are refused with 400 and leave the project untouched.
#
# Without SUPAVISE_BIN the script builds supavise with the go toolchain. It needs network access
# for the artifact downloads. Not run in development (root, systemd and Linux required); CI runs it
# on an ephemeral Ubuntu 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

preflight
install_binary
setup_node

log "system init, daemon"
system_up
start_daemon

log "a project through the Management API"
claim_and_token
gen_dbpass
REF=$(api_create_project backups-smoke)
CFG="/v1/projects/$REF"
PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
PGPORT=$(project_field "$REF" 'd["ports"]["Postgres"]')
pg_admin() { # SQL: as supabase_admin over the cluster's private socket
  sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/$REF/postgres/sock port=$PGPORT user=supabase_admin dbname=postgres" -Atc "$1" </dev/null
}
rows() { pg_admin "select coalesce(string_agg(label, ',' order by id), '') from public.restore_smoke"; }
status() { papi GET "$CFG" | json_get 'd["status"]'; }
# restore_failed prints the daemon's log line of a restore that failed (the project is then
# RESTORE_FAILED, which the waits below treat as the end of the wait).
restore_failed() { journalctl --no-pager -u supavise.service 2>/dev/null | grep 'msg="restore failed"' | tail -1 || true; }

log "the backup list and the PITR add-on of a new project"
LIST=$(papi GET "$CFG/database/backups")
[[ $(json_get 'd["pitr_enabled"]' <<<"$LIST") == True ]] || fail "pitr_enabled is not true: $LIST"
# /platform routes take the dashboard session, not a personal access token.
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' | json_get 'd["access_token"]') || fail "dashboard sign-in"
ADDONS=$(japi GET "/platform/projects/$REF/billing/addons")
# The compute size is listed first (compute.go); the PITR add-on is the one of type pitr.
[[ $(json_get '[a["type"] for a in d["selected_addons"]]' <<<"$ADDONS") == *pitr* ]] || fail "no PITR add-on: $ADDONS"
[[ $(json_get '[a["variant"]["meta"]["backup_duration_days"] for a in d["selected_addons"] if a["type"] == "pitr"][0]' <<<"$ADDONS") == 7 ]] || fail "retention in the add-on: $ADDONS"
# The nightly timer has not run on a new project, so there is normally nothing to restore from yet.
if [[ $(json_get 'len(d["backups"])' <<<"$LIST") == 0 ]]; then
  [[ $(json_get 'len(d["physical_backup_data"])' <<<"$LIST") == 0 ]] || fail "a restorable span before any base backup: $LIST"
  must 400 POST "$CFG/database/backups/restore-pitr" "{\"recovery_time_target_unix\":$(date +%s)}"
fi

log "rows 1 and 2 around a base backup, a time T, then row 3"
pg_admin "create table public.restore_smoke (id int primary key, label text); insert into public.restore_smoke values (1, 'one')" >/dev/null || fail "create table"
supavise backups create "$REF" || fail "backups create"
pg_admin "insert into public.restore_smoke values (2, 'two')" >/dev/null
sleep 3
T=$(date +%s)
sleep 3
pg_admin "insert into public.restore_smoke values (3, 'three')" >/dev/null
[[ $(rows) == one,two,three ]] || fail "rows before the restore: $(rows)"

log "the list shows the base backup and a span that contains T"
LIST=$(papi GET "$CFG/database/backups")
[[ $(json_get 'len(d["backups"])' <<<"$LIST") -ge 1 ]] || fail "no backup listed after one was taken: $LIST"
[[ $(json_get 'd["backups"][0]["is_physical_backup"]' <<<"$LIST") == True && $(json_get 'd["backups"][0]["status"]' <<<"$LIST") == COMPLETED ]] || fail "backup entry: $LIST"
FIRST_ID=$(json_get 'min(b["id"] for b in d["backups"])' <<<"$LIST")
EARLIEST=$(json_get 'd["physical_backup_data"]["earliest_physical_backup_date_unix"]' <<<"$LIST")
LATEST=$(json_get 'd["physical_backup_data"]["latest_physical_backup_date_unix"]' <<<"$LIST")
(( EARLIEST <= T && T <= LATEST )) || fail "T=$T is not inside the span $EARLIEST..$LATEST"
PLAT=$(japi GET "/platform/database/$REF/backups")
[[ $(json_get 'd["backups"][0]["isPhysicalBackup"]' <<<"$PLAT") == True && $(json_get 'd["pitr_enabled"]' <<<"$PLAT") == True ]] || fail "platform list: $PLAT"

log "refused: a time outside the span, a backup that does not exist"
must 400 POST "$CFG/database/backups/restore-pitr" "{\"recovery_time_target_unix\":$((EARLIEST - 5))}"
must 400 POST "$CFG/database/backups/restore-pitr" "{\"recovery_time_target_unix\":$((LATEST + 3600))}"
must 404 POST "$CFG/database/backups/restore" '{"id":999999}'
[[ $(status) == ACTIVE_HEALTHY && $(rows) == one,two,three ]] || fail "a refused restore changed the project"

log "restore to T through the API: RESTORING, a second restore refused, ACTIVE_HEALTHY, rows 1 and 2"
must 201 POST "$CFG/database/backups/restore-pitr" "{\"recovery_time_target_unix\":$T}"
[[ $(status) == RESTORING ]] || fail "the project is $(status) right after the restore began, want RESTORING"
must 409 POST "$CFG/database/backups/restore-pitr" "{\"recovery_time_target_unix\":$T}"
must 409 POST "$CFG/pause"
wait_status "$REF" ACTIVE_HEALTHY 900 RESTORE_FAILED
[[ -z $(restore_failed) ]] || fail "the restore failed: $(restore_failed)"
GOT=$(rows)
[[ $GOT == one,two ]] || fail "after the restore to T the rows are '$GOT', want one,two"
supavise projects health "$REF" || fail "$REF is not healthy after the restore"
[[ $(unit_state "supavise-postgres@$REF.service") == active ]] || fail "supavise-postgres@$REF is not active after the restore"
# The new timeline has a base backup at once: the restore takes one (the list's fourth column is the timeline).
supavise backups list "$REF" | awk '$2 == "completed" && $4 == 2 { found = 1 } END { exit !found }' || { supavise backups list "$REF" >&2; fail "no base backup of timeline 2 after the restore"; }
LIST=$(papi GET "$CFG/database/backups")
[[ $(json_get 'len(d["backups"])' <<<"$LIST") -ge 2 ]] || fail "no base backup of the new timeline is listed: $LIST"

log "restore the state of the first listed base backup (id $FIRST_ID)"
must 201 POST "$CFG/database/backups/restore" "{\"id\":$FIRST_ID}"
[[ $(status) == RESTORING ]] || fail "the project is $(status) right after the restore began, want RESTORING"
wait_status "$REF" ACTIVE_HEALTHY 900 RESTORE_FAILED
[[ -z $(restore_failed) ]] || fail "the restore failed: $(restore_failed)"
GOT=$(rows)
[[ $GOT == one ]] || fail "after the restore of the first base backup the rows are '$GOT', want one"

supavise projects health "$REF" || fail "$REF is not healthy at the end"
log "backups smoke passed"
