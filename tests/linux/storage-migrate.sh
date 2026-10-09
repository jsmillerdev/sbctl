#!/usr/bin/env bash
# `supavise storage migrate` under real systemd units against a real S3 service (Garage): objects
# uploaded through the Storage API on the file backend (and two that Storage's API cannot make, an
# 80 MiB file and a file with a non-ASCII name, put on disk by hand) are copied to the bucket while
# Storage keeps serving and a writer keeps uploading; a configuration that would not take effect is
# refused before anything is copied; the run is killed once the bucket holds some of the objects and
# not the big one, and continued with --resume, which sends only the rest; Storage then serves every
# object from the bucket with its content type and cache control; the credentials are in a 0600 file
# under config.d and nowhere else, and the record of the run is outside Storage's writable
# directory; --rollback copies the objects written and deleted on the bucket back to the files; a
# second migration is followed by --cleanup.
#
#   sudo S3_ENDPOINT=http://127.0.0.1:9000 S3_BUCKET=supavise-objects S3_KEY=... S3_SECRET=... \
#     SUPAVISE_BIN=/path/to/supavise-linux-amd64 tests/linux/storage-migrate.sh
#
# The bucket must exist and the key must be able to read, write, delete and list in it. The defaults
# are the Garage the linux.yml job starts. Without SUPAVISE_BIN the script builds supavise with the go
# toolchain. It needs network access for the artifact downloads.
#
# Not run in development: it needs root, systemd and Linux.
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

export S3_ENDPOINT=${S3_ENDPOINT:-http://127.0.0.1:9000}
export S3_BUCKET=${S3_BUCKET:-supavise-objects}
export S3_KEY=${S3_KEY:-GK0123456789abcdef01234567}
export S3_SECRET=${S3_SECRET:-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}
export S3_REGION=${S3_REGION:-us-east-1}
export PYTHONUTF8=1
S3PY=$(dirname "${BASH_SOURCE[0]}")/storage-migrate-s3.py
W=/tmp/storage-migrate
mkdir -p "$W"
rm -f "$W"/*

trap 'rc=$?; rm -f "$W/live.run"; collect_logs; cp "$W"/*.log "$W"/*.txt "$LOG_DIR/" 2>/dev/null || true; exit $rc' EXIT

s3() { python3 "$S3PY" "$@"; }
sha() { sha256sum | cut -d' ' -f1; }

# Ports away from anything the runner may already listen on, as in fleet-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000 P_EDGE=19000

s3 list "" >/dev/null || fail "the S3 service at $S3_ENDPOINT does not answer for bucket $S3_BUCKET with this key"

preflight
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
edge_runtime = $P_EDGE

[fleet]
supavisor_api_port = $P_API
storage_s3_bucket = "$S3_BUCKET"
storage_s3_endpoint = "$S3_ENDPOINT"
storage_s3_region = "$S3_REGION"
storage_s3_force_path_style = true
CONF
# The key is in a file only the supavise user can read, as an operator would keep it.
CREDS=/etc/supavise/storage-credentials
install -m 0600 -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" /dev/null "$CREDS"
printf 'access_key_id=%s\nsecret_access_key=%s\n' "$S3_KEY" "$S3_SECRET" >"$CREDS"

log "system init, shared services, daemon"
system_up
supavise fleet start || fail "fleet start"
systemctl start supavise.service
wait_fleet "the shared services are not up"
wait_proxy_api 60
claim_and_token
gen_dbpass
REF=$(api_create_project storage-migrate)
project_keys "$REF"
HOST="$REF.api.$SUPAVISE_DOMAIN"
STORAGE_DIR=$SUPAVISE_STATE/system/storage
RUN_DIR=$SUPAVISE_STATE/system/storage-migrate
OBJ_ROOT=$STORAGE_DIR/objects/stub/$REF
BASE=http://127.0.0.1/storage/v1

st() { curl -sS -m 120 -H "Host: $HOST" -H "apikey: $SEC" -H "Authorization: Bearer $SEC" "$@"; }
st_code() { st -o /dev/null -w '%{http_code}' "$@"; }
storage_up() { [[ $(st_code "$BASE/bucket" 2>/dev/null) == 200 ]]; }
asuser() { sudo -u "$SUPAVISE_USER" -H "$@"; }
# conf_has KEY VALUE: config.toml sets KEY to the string VALUE (the encoder quotes with ' or ").
conf_has() { grep -Eq "^$1 = ['\"]$2['\"]" "$SUPAVISE_CONF"; }
migrate() { asuser /usr/local/bin/supavise storage migrate "$@"; }
status() { migrate --to s3 --status; }
# status_has REGEX: the status output has a line that matches. The output is read into a variable
# first: a grep -q that quits early would send SIGPIPE to the command and, with pipefail, fail the pipe.
status_has() { local out; out=$(status 2>/dev/null) && grep -q "$1" <<<"$out"; }
enc() { python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe="/"))' "$1"; }

# ---- objects on the file backend ---------------------------------------------------------------
log "uploading objects through the Storage API (file backend)"
st -X POST -H 'Content-Type: application/json' -d '{"name":"docs"}' "$BASE/bucket" >/dev/null || fail "create the bucket docs"
printf 'top level object\n' >"$W/top.txt"
head -c 2097152 /dev/urandom >"$W/nested.bin"
printf 'plus and equals\n' >"$W/plus.txt"
printf 'a name with a space\n' >"$W/space.txt"
printf 'to be deleted on the bucket\n' >"$W/doomed.txt"
# NAME|CONTENT_TYPE|FILE
OBJECTS=(
  "top.txt|text/plain|$W/top.txt"
  "dir/sub/nested.bin|application/octet-stream|$W/nested.bin"
  "a+b=c.txt|text/plain|$W/plus.txt"
  "sp ace.txt|text/plain|$W/space.txt"
  "doomed.txt|text/plain|$W/doomed.txt"
)
for o in "${OBJECTS[@]}"; do
  IFS='|' read -r name type file <<<"$o"
  [[ $(st_code -X POST -H "Content-Type: $type" -H 'Cache-Control: max-age=3600' --data-binary @"$file" "$BASE/object/docs/$(enc "$name")") == 200 ]] || fail "upload $name"
done
headers_of() { # NAME: the headers a client sees for docs/NAME
  st -D - -o /dev/null "$BASE/object/docs/$(enc "$1")" | tr -d '\r' | grep -i -E '^(content-type|cache-control|content-length):' | tr 'A-Z' 'a-z' | sort
}
declare -A BEFORE_HEADERS
for o in "${OBJECTS[@]}"; do
  IFS='|' read -r name _ file <<<"$o"
  BEFORE_HEADERS[$name]=$(headers_of "$name")
  [[ $(st "$BASE/object/docs/$(enc "$name")" | sha) == "$(sha <"$file")" ]] || fail "$name did not come back as uploaded"
done
[[ ${BEFORE_HEADERS[top.txt]} == *"cache-control: max-age=3600"* && ${BEFORE_HEADERS[top.txt]} == *"content-type: text/plain"* ]] \
  || fail "the file backend did not keep the headers: ${BEFORE_HEADERS[top.txt]}"

# Two files the Storage API cannot make: 80 MiB (over its 50 MiB upload limit, so the copy needs a
# multipart upload) and a name with non-ASCII characters (Storage refuses those now; older releases
# did not). Neither has a row, which the migration reports and copies anyway.
BIG_VERSION=$(cat /proc/sys/kernel/random/uuid)
UNI_VERSION=$(cat /proc/sys/kernel/random/uuid)
asuser env OBJ_ROOT="$OBJ_ROOT" BIG_VERSION="$BIG_VERSION" UNI_VERSION="$UNI_VERSION" python3 - <<'PY'
import os
root = os.fsencode(os.environ["OBJ_ROOT"])
for rel, size, ctype in ((b"docs/big.bin/" + os.environb[b"BIG_VERSION"], 80 << 20, b"application/x-big"),
                         (b"docs/\xc3\xbc-\xc3\xa9.txt/" + os.environb[b"UNI_VERSION"], 20, b"text/x-unicode")):
    p = os.path.join(root, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "wb") as f:
        f.write(os.urandom(size))
    os.setxattr(p, b"user.supabase.content-type", ctype)
    os.setxattr(p, b"user.supabase.cache-control", b"max-age=60")
PY

# ---- a run that is killed, and continues ----------------------------------------------------------
# A writer that keeps uploading while the migration runs; it records what Storage accepted.
touch "$W/live.run" "$W/live.ok"
(
  i=0
  while [[ -f $W/live.run ]]; do
    i=$((i + 1))
    printf 'live object %s\n' "$i" >"$W/live-$i.txt"
    if [[ $(st_code -X POST -H 'Content-Type: text/plain' -H 'Cache-Control: max-age=3600' --data-binary @"$W/live-$i.txt" "$BASE/object/docs/live-$i.txt" 2>/dev/null) == 200 ]]; then
      echo "$i" >>"$W/live.ok"
    fi
    sleep 0.3
  done
) &
WRITER=$!

log "a credentials file that others can read is refused"
chmod 0644 "$CREDS"
if migrate --to s3 --credentials-file "$CREDS" >"$W/wide.log" 2>&1; then fail "a world-readable credentials file was accepted"; fi
grep -q 'chmod 600' "$W/wide.log" || { cat "$W/wide.log" >&2; fail "the refusal does not say how to fix it"; }
grep -q "$S3_SECRET" "$W/wide.log" && fail "the secret is in the error"
chmod 0600 "$CREDS"

log "a configuration that would not take effect is refused before anything is copied"
install -d -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" -m 0755 /etc/supavise/config.d
printf '[fleet]\nstorage_backend = "file"\n' >/etc/supavise/config.d/50-test-override.toml
chmod 0644 /etc/supavise/config.d/50-test-override.toml
if migrate --to s3 --credentials-file "$CREDS" >"$W/override.log" 2>&1; then fail "a migration went ahead although config.d overrides storage_backend"; fi
rm -f /etc/supavise/config.d/50-test-override.toml
grep -q 'overrides config.toml' "$W/override.log" || { cat "$W/override.log" >&2; fail "the refusal does not name the override"; }
[[ ! -e $RUN_DIR/migrate.json ]] || fail "a refused run left a record"
[[ -z $(s3 list "$REF/") ]] || fail "a refused run copied objects"
[[ $(unit_state supavise-storage.service) == active ]] || fail "Storage is $(unit_state supavise-storage.service) after the refused run"

# The bucket copies a file as current when it is newer than the file by a few seconds of clock slack:
# the objects above are older than that when the migration starts.
sleep 3
log "migrate at 1 MiB/s: the 80 MiB file cannot be in the bucket for the first 80 seconds"
migrate --to s3 --credentials-file "$CREDS" --rate-limit 1 >"$W/migrate1.log" 2>&1 &
MIGRATE=$!
for ((i = 0; i < 120; i++)); do
  status_has '^phase:  *copying' && break
  sleep 1
done
status_has '^phase:  *copying' || { cat "$W/migrate1.log" >&2; fail "the run never reached the copy"; }
bucket_has_some() { [[ $(s3 list "$REF/" | wc -l) -ge 3 ]]; }
wait_for 90 0.5 "some of the objects in the bucket" bucket_has_some
pkill -KILL -f 'supavise storage migrate' || true
wait "$MIGRATE" 2>/dev/null || true
s3 list "$REF/" | LC_ALL=C sort >"$W/keys-at-kill.txt"
log "killed with $(wc -l <"$W/keys-at-kill.txt") objects in the bucket"
grep -q "^$REF/docs/big.bin/" "$W/keys-at-kill.txt" && fail "the 80 MiB file reached the bucket in the first seconds of a 1 MiB/s copy: the rate limit does not bound the bytes sent"
status | tee "$W/status-killed.txt" >&2
grep -q '^phase:  *copying' "$W/status-killed.txt" || fail "a killed run did not leave its phase"
grep -q 'migrate --resume' "$W/status-killed.txt" || fail "the status does not say how to continue"
if migrate --to s3 --credentials-file "$CREDS" >"$W/second.log" 2>&1; then fail "a second run started over a run in progress"; fi
grep -q -- '--resume' "$W/second.log" || { cat "$W/second.log" >&2; fail "the refusal does not mention --resume"; }
[[ $(unit_state supavise-storage.service) == active ]] || fail "Storage is $(unit_state supavise-storage.service) after the killed run"
[[ ! -e $RUN_DIR/write-hold.json ]] || log "a hold from the killed run is left (it expires on its own)"
FIRST_KEY=$(grep -v "^$REF/docs/live-" "$W/keys-at-kill.txt" | head -1)
[[ -n $FIRST_KEY ]] || fail "only the writer's objects were in the bucket at the kill"
FIRST_HEAD=$(s3 head "$FIRST_KEY")

log "resume (no limit)"
migrate --to s3 --resume --credentials-file "$CREDS" --rate-limit 0 >"$W/migrate2.log" 2>&1 || { cat "$W/migrate2.log" >&2; fail "--resume failed"; }
cat "$W/migrate2.log" >&2
rm -f "$W/live.run"
wait "$WRITER" || true
status | tee "$W/status-done.txt" >&2
grep -q '^phase:  *done' "$W/status-done.txt" || fail "the run did not finish"
grep -q 'have no row' "$W/migrate2.log" || fail "the run did not report the files that have no row"
# The resumed run sent only what the bucket lacked: the objects there at the kill were written long
# before the copy started (all but the writer's), and are not sent again.
COPY_LINE=$(sed -nE 's/^copy: ([0-9]+) files in [0-9]+ projects, ([0-9]+) sent.*/\1 \2/p' "$W/migrate2.log" | head -1)
read -r FILES SENT <<<"$COPY_LINE"
OLD_AT_KILL=$(grep -vc "^$REF/docs/live-" "$W/keys-at-kill.txt" || true)
[[ -n ${FILES:-} && $OLD_AT_KILL -ge 1 && $SENT -le $((FILES - OLD_AT_KILL)) ]] \
  || fail "the resumed run sent $SENT of $FILES files although $OLD_AT_KILL were in the bucket"
[[ $(s3 head "$FIRST_KEY") == "$FIRST_HEAD" ]] || fail "$FIRST_KEY was sent again by the resumed run"
log "resumed: $SENT of $FILES files sent, $OLD_AT_KILL were already in the bucket"

# ---- what the switch left ------------------------------------------------------------------------
log "the configuration: the backend, the bucket, and the key in its own file"
conf_has storage_backend s3 || fail "config.toml does not name the s3 backend"
conf_has storage_s3_bucket "$S3_BUCKET" || fail "config.toml does not name the bucket"
if grep -q "$S3_SECRET" "$SUPAVISE_CONF"; then fail "the secret is in config.toml"; fi
DROPIN=/etc/supavise/config.d/30-storage-s3.toml
[[ -f $DROPIN ]] || fail "no $DROPIN"
[[ $(stat -c '%a %U' "$DROPIN") == "600 $SUPAVISE_USER" ]] || fail "$DROPIN is $(stat -c '%a %U' "$DROPIN")"
grep -q "$S3_SECRET" "$DROPIN" || fail "the credentials are not in $DROPIN"
STORAGE_ENV=$SUPAVISE_STATE/projects/system/storage.env
grep -q '^STORAGE_BACKEND="s3"' "$STORAGE_ENV" || fail "storage.env does not name the s3 backend"
[[ $(unit_state supavise-storage.service) == active ]] || fail "supavise-storage is $(unit_state supavise-storage.service)"
[[ ! -e $STORAGE_DIR/objects ]] || fail "the objects directory is still where Storage's file backend reads"
KEPT=$(ls -d "$STORAGE_DIR"/objects.migrated-* 2>/dev/null | head -1 || true)
[[ -n $KEPT && -d $KEPT/stub/$REF ]] || fail "the files were not kept ($STORAGE_DIR)"
[[ ! -e $RUN_DIR/write-hold.json ]] || fail "the write hold was not released"
[[ -f $RUN_DIR/migrate.json && ! -e $STORAGE_DIR/migrate.json && ! -e $STORAGE_DIR/migrate.lock ]] \
  || fail "the record of the run is not in $RUN_DIR, outside the directory supavise-storage may write"
[[ $(stat -c %U "$RUN_DIR") == "$SUPAVISE_USER" ]] || fail "$RUN_DIR belongs to $(stat -c %U "$RUN_DIR")"
journalctl --no-pager -u 'supavise*' >"$W/journal.txt" 2>&1 || true
if grep -q "$S3_SECRET" "$W/journal.txt"; then fail "the secret is in the journal"; fi
if grep -q "$S3_SECRET" "$W"/*.log "$W"/status-*.txt; then fail "the secret is in the output of the commands"; fi

log "the bucket holds every file under <ref>/<bucket>/<name>/<version>, with its headers"
s3 list "$REF/" | LC_ALL=C sort >"$W/keys-s3.txt"
(cd "$KEPT/stub/$REF" && find . -type f -printf '%P\n' | sed "s#^#$REF/#" | LC_ALL=C sort) >"$W/keys-files.txt"
[[ -s $W/keys-files.txt ]] || fail "no files were kept"
missing=$(LC_ALL=C comm -13 "$W/keys-s3.txt" "$W/keys-files.txt")
[[ -z $missing ]] || fail "files that are not in the bucket: $missing"
grep -q "^$REF/docs/ü-é.txt/$UNI_VERSION\$" "$W/keys-s3.txt" || fail "the file with a non-ASCII name was not copied under its own name"
grep -q "^$REF/docs/big.bin/$BIG_VERSION\$" "$W/keys-s3.txt" || fail "the 80 MiB file is not in the bucket"
head_of="$(s3 head "$REF/docs/big.bin/$BIG_VERSION")"
grep -q 'content-length: 83886080' <<<"$head_of" || fail "the 80 MiB file has the wrong size: $head_of"
grep -q 'content-type: application/x-big' <<<"$head_of" || fail "the extended attribute (content type) was not copied: $head_of"
grep -q 'cache-control: max-age=60' <<<"$head_of" || fail "the extended attribute (cache control) was not copied: $head_of"
topkey=$(grep "^$REF/docs/top.txt/" "$W/keys-s3.txt")
head_of="$(s3 head "$topkey")"
grep -q 'content-type: text/plain' <<<"$head_of" && grep -q 'cache-control: max-age=3600' <<<"$head_of" || fail "top.txt in the bucket: $head_of"
# The bytes are the file's.
[[ $(s3 get "$REF/docs/big.bin/$BIG_VERSION" | sha) == "$(sha <"$KEPT/stub/$REF/docs/big.bin/$BIG_VERSION")" ]] || fail "the 80 MiB file differs in the bucket"

log "Storage serves every object from the bucket, with the headers it had"
wait_for 60 0.5 "Storage" storage_up
for o in "${OBJECTS[@]}"; do
  IFS='|' read -r name _ file <<<"$o"
  [[ $(st "$BASE/object/docs/$(enc "$name")" | sha) == "$(sha <"$file")" ]] || fail "$name from the bucket differs from what was uploaded"
  after=$(headers_of "$name")
  [[ $after == "${BEFORE_HEADERS[$name]}" ]] || fail "$name: headers changed with the backend
before: ${BEFORE_HEADERS[$name]}
after:  $after"
done
LIVE_OK=$(wc -l <"$W/live.ok")
[[ $LIVE_OK -ge 3 ]] || fail "the writer got only $LIVE_OK uploads accepted; the run was too short to mean anything"
while read -r i; do
  [[ $(st "$BASE/object/docs/live-$i.txt") == "live object $i" ]] || fail "live-$i.txt, accepted while the copy ran, is not served from the bucket"
done <"$W/live.ok"
log "$LIVE_OK objects written during the migration are all served"

# ---- the bucket takes writes and deletes, and the rollback brings them back ------------------------
log "writes and a delete on the bucket"
printf 'written on the bucket\n' >"$W/after.txt"
[[ $(st_code -X POST -H 'Content-Type: text/x-after' -H 'Cache-Control: no-cache' --data-binary @"$W/after.txt" "$BASE/object/docs/after-switch.txt") == 200 ]] || fail "upload on the bucket"
[[ $(st_code -X DELETE "$BASE/object/docs/doomed.txt") == 200 ]] || fail "delete on the bucket"
[[ $(st_code "$BASE/object/docs/doomed.txt") != 200 ]] || fail "doomed.txt is still served"

log "rollback"
migrate --to s3 --rollback >"$W/rollback.log" 2>&1 || { cat "$W/rollback.log" >&2; fail "--rollback failed"; }
cat "$W/rollback.log" >&2
grep -q 'copied back' "$W/rollback.log" || fail "the rollback did not say what it copied back"
status | tee "$W/status-rolled-back.txt" >&2
grep -q '^phase:  *rolled_back' "$W/status-rolled-back.txt" || fail "not rolled back"
if conf_has storage_backend s3; then fail "config.toml still names the s3 backend"; fi
[[ ! -e $DROPIN ]] || fail "the credentials file was not removed"
grep -q '^STORAGE_BACKEND="file"' "$STORAGE_ENV" || fail "storage.env does not name the file backend"
[[ -d $STORAGE_DIR/objects/stub/$REF ]] || fail "the files are not back in place"
[[ -z $(ls -d "$STORAGE_DIR"/objects.migrated-* 2>/dev/null || true) ]] || fail "the kept files are still aside after the rollback"
wait_for 60 0.5 "Storage" storage_up
for o in "${OBJECTS[@]}"; do
  IFS='|' read -r name _ file <<<"$o"
  [[ $name == doomed.txt ]] && continue
  [[ $(st "$BASE/object/docs/$(enc "$name")" | sha) == "$(sha <"$file")" ]] || fail "$name differs after the rollback"
  [[ $(headers_of "$name") == "${BEFORE_HEADERS[$name]}" ]] || fail "$name: headers changed with the rollback"
done
[[ $(st "$BASE/object/docs/after-switch.txt") == "written on the bucket" ]] || fail "the object written on the bucket did not come back"
after_headers=$(headers_of after-switch.txt)
[[ $after_headers == *"content-type: text/x-after"* ]] || fail "the content type of the object written on the bucket was lost: $after_headers"
[[ $(st_code "$BASE/object/docs/doomed.txt") != 200 ]] || fail "the object deleted on the bucket is served again"
log "rolled back: the objects written and deleted on the bucket followed"

# ---- again, and the files are deleted ------------------------------------------------------------
log "migrate again, then --cleanup"
migrate --to s3 --credentials-file "$CREDS" --rate-limit 0 >"$W/migrate3.log" 2>&1 || { cat "$W/migrate3.log" >&2; fail "the second migration failed"; }
status_has '^phase:  *done' || fail "the second migration did not finish"
[[ $(st "$BASE/object/docs/after-switch.txt") == "written on the bucket" ]] || fail "after-switch.txt is not served from the bucket"
KEPT=$(ls -d "$STORAGE_DIR"/objects.migrated-* | head -1 || true)
[[ -n $KEPT ]] || fail "the second migration kept no files"
migrate --to s3 --cleanup >"$W/cleanup.log" 2>&1 || { cat "$W/cleanup.log" >&2; fail "--cleanup failed"; }
[[ ! -e $KEPT ]] || fail "--cleanup left $KEPT"
status_has '^files:  *deleted' || fail "the status does not say the files are deleted"
[[ $(st "$BASE/object/docs/after-switch.txt") == "written on the bucket" ]] || fail "Storage lost an object when the files were deleted"
if migrate --to s3 --cleanup >/dev/null 2>&1; then fail "a second --cleanup worked"; fi

log "storage migrate: passed"
