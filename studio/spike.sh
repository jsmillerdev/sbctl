#!/usr/bin/env bash
# The Studio spike: stands up a small stack, runs the platform-mode Studio against the mock
# Management API, and drives it with a headless browser. Written for a fresh Ubuntu 24.04 CI VM
# (amd64 or arm64). Do not run it on the shared dev Mac without studio/../scripts/guard.sh; the
# whole stack needs about 1.5 GB of RAM and no Docker.
#
#   studio/spike.sh [--keep] [--stack-only]
#
# Stack (all on 127.0.0.1, ports SPIKE_PORT_BASE+1..+6, default 31001..31006):
#   +1 Postgres (slim artifact, one cluster, databases proj_a and proj_b = the "two projects")
#   +2 GoTrue   (slim artifact, the dashboard sign-in)
#   +3 postgres-meta (slim artifact; its admin port is +6, because pg-meta defaults it to its port + 1)
#   +4 the mock Management API (studio/mock, built here with go)
#   +5 Studio (our platform-mode build)
# Then Playwright signs in, lists the two projects, opens the table editor on each, runs SQL,
# visits the project home, auth users, storage, database and settings pages, and writes the
# request log of the mock plus screenshots. Exit status is non-zero if any step failed.
#
# Needs on the VM: curl, tar, zstd, git is not needed; go (to build the mock) unless
# SPIKE_MOCK_BIN is set; node 20+ and npm (for playwright-core); sudo or root if Chromium's system
# libraries are missing (playwright install --with-deps).
#
# Environment (all optional):
#   SPIKE_STUDIO_ARCHIVE   sbctl-studio-*.tar.zst to test (default: the newest in studio/dist)
#   SPIKE_DIR              working directory (default studio/.build-cache/spike)
#   SPIKE_OUT              results directory (default $SPIKE_DIR/out)
#   SPIKE_PORT_BASE        ports are base+1..base+6 (default 31000)
#   SPIKE_ARTIFACT_POSTGRES / _AUTH / _PGMETA   use this already unpacked artifact directory
#   SPIKE_MOCK_BIN         prebuilt studio mock binary
#   SPIKE_CHROME           path of a Chrome or Chromium executable to use instead of downloading one
#   SPIKE_PLAYWRIGHT       playwright-core version (default 1.63.0)
#   --keep                 leave the stack running when the browser steps are done (stop with Ctrl-C)
#   --stack-only           start the stack, print the URLs and wait; no browser (for debugging)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
KEEP=0; STACK_ONLY=0
for a in "$@"; do
  case "$a" in --keep) KEEP=1 ;; --stack-only) STACK_ONLY=1; KEEP=1 ;; *) echo "unknown argument: $a" >&2; exit 2 ;; esac
done

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'spike.sh: %s\n' "$*" >&2; exit 1; }

# ---- platform, paths, ports -------------------------------------------------------------------
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) PLATFORM=linux-amd64 ;; Linux-aarch64|Linux-arm64) PLATFORM=linux-arm64 ;;
  Darwin-arm64) PLATFORM=darwin-arm64 ;; *) die "unsupported host $(uname -sm)" ;;
esac
SPIKE_DIR="${SPIKE_DIR:-$HERE/.build-cache/spike}"
OUT="${SPIKE_OUT:-$SPIKE_DIR/out}"
BASE="${SPIKE_PORT_BASE:-31000}"
PG_PORT=$((BASE + 1)); AUTH_PORT=$((BASE + 2)); META_PORT=$((BASE + 3)); MOCK_PORT=$((BASE + 4)); STUDIO_PORT=$((BASE + 5)); META_ADMIN_PORT=$((BASE + 6))
HOST=127.0.0.1
mkdir -p "$SPIKE_DIR" "$OUT/logs" "$OUT/screenshots"

[[ "$(id -u)" -ne 0 ]] || die "run as a normal user: postgres refuses to start as root"
for t in curl tar zstd; do command -v "$t" >/dev/null 2>&1 || die "missing tool: $t"; done
sha256_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
rand() { local s; s="$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c "$1" || true)"; printf '%s' "$s"; }

# versions.yaml: artifacts.<name>
tag_of() { awk -v k="$1" '/^artifacts:/{s=1;next} s&&/^[^ ]/{s=0} s&&$1==k":"{print $2; exit}' "$REPO/versions.yaml"; }

# ---- process management -----------------------------------------------------------------------
PIDS=(); NAMES=()
start() { # name command...
  local name="$1"; shift
  "$@" > "$OUT/logs/$name.log" 2>&1 &
  PIDS+=("$!"); NAMES+=("$name")
  echo "spike: started $name (pid $!)" >&2
}
cleanup() {
  set +e
  local i
  for ((i=${#PIDS[@]}-1; i>=0; i--)); do  # (no iterations when empty)
    local pid="${PIDS[$i]}" name="${NAMES[$i]}"
    kill -0 "$pid" 2>/dev/null || continue
    if [[ "$name" == postgres ]]; then kill -INT "$pid"; else kill -TERM "$pid"; fi
  done
  local t=0
  while [[ $t -lt 20 ]]; do
    local alive=0
    for pid in ${PIDS[@]+"${PIDS[@]}"}; do kill -0 "$pid" 2>/dev/null && alive=1; done
    [[ $alive -eq 0 ]] && break
    sleep 1; t=$((t + 1))
  done
  for pid in ${PIDS[@]+"${PIDS[@]}"}; do kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null; done
  rm -rf "$SPIKE_DIR/studio-run" 2>/dev/null
}
trap cleanup EXIT
trap 'exit 130' INT TERM

wait_http() { # url seconds label
  local i
  for i in $(seq 1 "$2"); do
    if curl -fsS -o /dev/null --max-time 3 "$1" 2>/dev/null; then return 0; fi
    sleep 1
  done
  die "$3 did not answer $1 within $2 s (see $OUT/logs)"
}

# ---- artifacts --------------------------------------------------------------------------------
fetch_artifact() { # NAME(variable suffix) tag -> prints the directory
  local override_var="SPIKE_ARTIFACT_$1" tag="$2"
  if [[ -n "${!override_var:-}" ]]; then echo "${!override_var}"; return; fi
  local dir="$SPIKE_DIR/artifacts/$tag-$PLATFORM"
  if [[ ! -f "$dir/.complete" ]]; then
    local base="https://github.com/supabase/slim-services/releases/download/$tag" file="$tag-$PLATFORM.tar.zst"
    mkdir -p "$SPIKE_DIR/dl" "$dir"
    [[ -f "$SPIKE_DIR/dl/$file" ]] || curl -fsSL --retry 3 -o "$SPIKE_DIR/dl/$file" "$base/$file"
    curl -fsSL --retry 3 -o "$SPIKE_DIR/dl/$tag.SHA256SUMS" "$base/SHA256SUMS"
    local want; want="$(awk -v f="$file" '$2==f{print $1}' "$SPIKE_DIR/dl/$tag.SHA256SUMS")"
    [[ -n "$want" ]] || die "no checksum for $file in the release"
    [[ "$(sha256_of "$SPIKE_DIR/dl/$file")" == "$want" ]] || die "checksum mismatch for $file"
    zstd -dc "$SPIKE_DIR/dl/$file" | tar -C "$dir" -xf -
    touch "$dir/.complete"
  fi
  echo "$dir"
}

log "artifacts ($PLATFORM)"
PGDIR="$(fetch_artifact POSTGRES "$(tag_of postgres)")"
AUTHDIR="$(fetch_artifact AUTH "$(tag_of auth)")"
METADIR="$(fetch_artifact PGMETA "$(tag_of pgmeta)")"
STUDIO_ARCHIVE="${SPIKE_STUDIO_ARCHIVE:-$(ls -t "$HERE"/dist/sbctl-studio-*-"$PLATFORM".tar.zst 2>/dev/null | head -n 1 || true)}"
[[ -f "$STUDIO_ARCHIVE" ]] || die "no Studio archive (set SPIKE_STUDIO_ARCHIVE or run studio/build.sh $PLATFORM first)"
STUDIO_DIR="$SPIKE_DIR/studio-run"
rm -rf "$STUDIO_DIR"; mkdir -p "$STUDIO_DIR"
zstd -dc "$STUDIO_ARCHIVE" | tar -C "$STUDIO_DIR" -xf -
echo "spike: Studio $(basename "$STUDIO_ARCHIVE") sha256 $(sha256_of "$STUDIO_ARCHIVE")" | tee "$OUT/studio-artifact.txt" >&2

# ---- secrets (fresh per run, never written outside $OUT/run.env which is chmod 600) --------------
PGPW="$(rand 24)"; JWT_SECRET="$(rand 48)"; META_KEY="$(rand 32)"; USER_EMAIL="admin@example.test"; USER_PW="$(rand 20)"
REF_A=mockprojectalphaaaaa; REF_B=mockprojectbetabbbbb   # 20 lowercase letters, like real refs

# ---- Postgres ---------------------------------------------------------------------------------
log "postgres on :$PG_PORT"
rm -rf "$SPIKE_DIR/pgdata"
export PGDATA="$SPIKE_DIR/pgdata" POSTGRES_PASSWORD="$PGPW"
# Two small settings from the brief; no unix socket (paths can exceed the socket path limit).
start postgres "$PGDIR/bin/supabase-postgres-start" -p "$PG_PORT" -c "listen_addresses=$HOST" -c "unix_socket_directories=" \
  -c shared_buffers=16MB -c max_connections=30
unset PGDATA POSTGRES_PASSWORD
for i in $(seq 1 120); do "$PGDIR/bin/pg_isready" -q -h "$HOST" -p "$PG_PORT" && break; sleep 1; done
"$PGDIR/bin/pg_isready" -q -h "$HOST" -p "$PG_PORT" || die "postgres did not start (see $OUT/logs/postgres.log)"
psql_admin() { "$PGDIR/bin/psql" -h "$HOST" -p "$PG_PORT" -U supabase_admin -v ON_ERROR_STOP=1 -Atq "$@"; }
psql_proj() { PGPASSWORD="$PGPW" "$PGDIR/bin/psql" -h "$HOST" -p "$PG_PORT" -U postgres -v ON_ERROR_STOP=1 -Atq "$@"; }
psql_admin -d postgres -c "alter role postgres with login password '$PGPW'" -c "alter role supabase_auth_admin with password '$PGPW'"
# Two projects = two databases cloned from the template the image ships (all Supabase schemas),
# made before GoTrue connects to `postgres` so the clone is possible.
for pair in "proj_a:alpha" "proj_b:beta"; do
  db="${pair%%:*}"; tag="${pair##*:}"
  # The template must have no other sessions: pg_cron and pg_net workers hold some, so end them
  # and retry (they only reconnect when there is work).
  created=0
  for attempt in 1 2 3 4 5; do
    psql_admin -d postgres -c "select pg_terminate_backend(pid) from pg_stat_activity where datname = 'postgres' and pid <> pg_backend_pid()" >/dev/null
    if psql_admin -d postgres -c "create database $db template postgres owner postgres" 2>/dev/null; then created=1; break; fi
    sleep 1
  done
  [[ $created -eq 1 ]] || die "could not create database $db from the postgres template"
  psql_proj -d "$db" -c "create table public.todos(id bigint generated always as identity primary key, task text not null, done boolean not null default false); insert into public.todos(task) values ('$tag-task-1'), ('$tag-task-2'), ('$tag-task-3')"
done

# ---- GoTrue (dashboard sign-in, and the auth schema of the two project databases) ---------------
log "gotrue on :$AUTH_PORT"
export GOTRUE_DB_DRIVER=postgres GOTRUE_DB_DATABASE_URL="postgres://supabase_auth_admin:$PGPW@$HOST:$PG_PORT/postgres" \
  GOTRUE_JWT_SECRET="$JWT_SECRET" GOTRUE_JWT_EXP=3600 GOTRUE_JWT_AUD=authenticated GOTRUE_JWT_DEFAULT_GROUP_NAME=authenticated \
  API_EXTERNAL_URL="http://$HOST:$AUTH_PORT" GOTRUE_SITE_URL="http://$HOST:$STUDIO_PORT" GOTRUE_API_HOST="$HOST" GOTRUE_API_PORT="$AUTH_PORT" \
  GOTRUE_DISABLE_SIGNUP=true GOTRUE_MAILER_AUTOCONFIRM=true GOTRUE_EXTERNAL_EMAIL_ENABLED=true GOTRUE_DB_NAMESPACE=auth
"$AUTHDIR/bin/auth" migrate > "$OUT/logs/gotrue-migrate.log" 2>&1 || die "gotrue migrate failed (see $OUT/logs/gotrue-migrate.log)"
# Each project database gets GoTrue's schema as a real project's own GoTrue would create it, so
# the Users page queries find the columns they expect.
for db in proj_a proj_b; do
  GOTRUE_DB_DATABASE_URL="postgres://supabase_auth_admin:$PGPW@$HOST:$PG_PORT/$db" "$AUTHDIR/bin/auth" migrate \
    > "$OUT/logs/gotrue-migrate-$db.log" 2>&1 || die "gotrue migrate on $db failed"
done
"$AUTHDIR/bin/auth" admin createuser "$USER_EMAIL" "$USER_PW" > "$OUT/logs/gotrue-createuser.log" 2>&1 || die "createuser failed"
# `admin createuser` leaves auth.users.role empty, so the access token would carry "role": "".
# Hosted dashboard users have role authenticated.
psql_admin -d postgres -c "update auth.users set role = 'authenticated' where email = '$USER_EMAIL'"
start gotrue "$AUTHDIR/bin/auth" serve
wait_http "http://$HOST:$AUTH_PORT/health" 60 gotrue
unset GOTRUE_DB_DRIVER GOTRUE_DB_DATABASE_URL GOTRUE_JWT_SECRET

# ---- postgres-meta ----------------------------------------------------------------------------
log "postgres-meta on :$META_PORT"
PG_META_HOST="$HOST" PG_META_PORT="$META_PORT" PG_META_ADMIN_PORT="$META_ADMIN_PORT" CRYPTO_KEY="$META_KEY" PG_META_DB_HOST="$HOST" PG_META_DB_PORT="$PG_PORT" \
  PG_META_DB_NAME=proj_a PG_META_DB_USER=postgres PG_META_DB_PASSWORD="$PGPW" \
  start pgmeta "$METADIR/bin/pgmeta"
wait_http "http://$HOST:$META_PORT/health" 60 postgres-meta

# ---- mock Management API ----------------------------------------------------------------------
log "mock on :$MOCK_PORT"
MOCK_BIN="${SPIKE_MOCK_BIN:-}"
if [[ -z "$MOCK_BIN" ]]; then
  command -v go >/dev/null 2>&1 || die "go is not installed; set SPIKE_MOCK_BIN to a built studio/mock binary"
  MOCK_BIN="$SPIKE_DIR/bin/studio-mock"
  mkdir -p "$SPIKE_DIR/bin"
  (cd "$REPO" && CGO_ENABLED=0 go build -o "$MOCK_BIN" ./studio/mock)
fi
cat > "$OUT/mock.json" <<JSON
{
  "listen": "$HOST:$MOCK_PORT",
  "jwt_secret": "$JWT_SECRET",
  "studio_origins": ["http://$HOST:$STUDIO_PORT"],
  "pgmeta_url": "http://$HOST:$META_PORT",
  "pgmeta_crypto_key": "$META_KEY",
  "scheme": "http",
  "project_host": "localhost:$MOCK_PORT",
  "request_log": "$OUT/request-log.jsonl",
  "projects": [
    {"ref": "$REF_A", "name": "Alpha", "db_url": "postgresql://postgres:$PGPW@$HOST:$PG_PORT/proj_a?sslmode=disable"},
    {"ref": "$REF_B", "name": "Beta",  "db_url": "postgresql://postgres:$PGPW@$HOST:$PG_PORT/proj_b?sslmode=disable"}
  ]
}
JSON
chmod 600 "$OUT/mock.json"
: > "$OUT/request-log.jsonl"
start mock "$MOCK_BIN" serve -config "$OUT/mock.json"
wait_http "http://$HOST:$MOCK_PORT/healthz" 20 mock

# ---- Studio -----------------------------------------------------------------------------------
log "studio on :$STUDIO_PORT"
PORT="$STUDIO_PORT" HOSTNAME="$HOST" \
  NEXT_PUBLIC_API_URL="http://$HOST:$MOCK_PORT/platform" NEXT_PUBLIC_GOTRUE_URL="http://$HOST:$AUTH_PORT" \
  NEXT_PUBLIC_SITE_URL="http://$HOST:$STUDIO_PORT" CSP_EXTRA_PROJECT_HOSTS="*.localhost:$MOCK_PORT" \
  start studio "$STUDIO_DIR/bin/studio"
wait_http "http://$HOST:$STUDIO_PORT/sign-in" 180 studio

cat > "$OUT/urls.txt" <<EOF
studio   http://$HOST:$STUDIO_PORT/sign-in
gotrue   http://$HOST:$AUTH_PORT
mock     http://$HOST:$MOCK_PORT
pgmeta   http://$HOST:$META_PORT
postgres $HOST:$PG_PORT
user     $USER_EMAIL
projects $REF_A $REF_B
EOF
{ echo "SPIKE_USER_PASSWORD=$USER_PW"; } > "$OUT/run.env"; chmod 600 "$OUT/run.env"

snapshot_resources() {
  { printf '%-10s %8s  %s\n' name rss_mb pid
    local i pid
    for i in "${!PIDS[@]}"; do
      pid="${PIDS[$i]}"
      for p in $pid $(pgrep -P "$pid" 2>/dev/null || true); do
        printf '%-10s %8s  %s\n' "${NAMES[$i]}" "$(ps -o rss= -p "$p" 2>/dev/null | awk '{printf "%d", $1/1024}')" "$p"
      done
    done; } > "$OUT/resources-$1.txt"
}

if [[ $STACK_ONLY -eq 1 ]]; then
  cat "$OUT/urls.txt" >&2
  echo "spike: password for $USER_EMAIL is in $OUT/run.env; Ctrl-C to stop" >&2
  wait
  exit 0
fi

# ---- browser steps ----------------------------------------------------------------------------
log "playwright"
PW_VERSION="${SPIKE_PLAYWRIGHT:-1.63.0}"
command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1 || die "node and npm are needed for playwright"
PWDIR="$SPIKE_DIR/pw"
mkdir -p "$PWDIR"
cp "$HERE/spike/spike.mjs" "$HERE/spike/package.json" "$PWDIR/"
(cd "$PWDIR" && npm install --silent --no-audit --no-fund "playwright-core@$PW_VERSION")
export PLAYWRIGHT_BROWSERS_PATH="$SPIKE_DIR/browsers"
if [[ -z "${SPIKE_CHROME:-}" ]]; then
  deps=()
  if [[ "$(uname -s)" == Linux ]] && { [[ "$(id -u)" -eq 0 ]] || sudo -n true 2>/dev/null; }; then deps=(--with-deps); fi
  (cd "$PWDIR" && if [[ ${#deps[@]} -gt 0 && "$(id -u)" -ne 0 ]]; then sudo -n env "PLAYWRIGHT_BROWSERS_PATH=$PLAYWRIGHT_BROWSERS_PATH" "$(command -v node)" node_modules/playwright-core/cli.js install "${deps[@]}" chromium
       else node node_modules/playwright-core/cli.js install "${deps[@]}" chromium; fi)
fi
snapshot_resources before-browser
set +e
(cd "$PWDIR" && \
  SPIKE_STUDIO_URL="http://$HOST:$STUDIO_PORT" SPIKE_EMAIL="$USER_EMAIL" SPIKE_PASSWORD="$USER_PW" \
  SPIKE_REFS="$REF_A,$REF_B" SPIKE_MARKERS="alpha,beta" SPIKE_DATABASES="proj_a,proj_b" SPIKE_OUT="$OUT" \
  SPIKE_REQUEST_LOG="$OUT/request-log.jsonl" SPIKE_CHROME="${SPIKE_CHROME:-}" node spike.mjs)
RC=$?
set -e
snapshot_resources after-browser

"$MOCK_BIN" summarize "$OUT/request-log.jsonl" > "$OUT/calls.md" || true
echo "spike: results in $OUT (steps.json, screenshots/, request-log.jsonl, calls.md, resources-*.txt, logs/)" >&2
if [[ $KEEP -eq 1 ]]; then
  echo "spike: --keep: stack is still running; Ctrl-C to stop" >&2
  wait
fi
exit $RC
