#!/usr/bin/env bash
# Edge Functions under real systemd units: system init, two projects, `supavise functions dev`
# (Management API and proxy in one process, with supavise-edge-runtime started as a unit), then
# tests/functions/run.sh, which deploys fixtures with the real `supabase functions deploy` (project
# A bundled by the CLI in Docker, project B uploaded as sources with --use-api and bundled by the
# node in the sandbox of supavise-edge-bundle@<ref>.service), calls them with supabase-js and checks
# isolation between the projects. On top of that this script checks what only systemd can show: the
# unit's user, slice and memory limit, what its mount namespace hides, that the bundler's module
# cache is per project (an upload of one project cannot import a module that another project's upload made
# the bundler download) and goes with the project, recovery after `kill -9` of the runtime, and
# that the runtime-wide worker budget refuses what would not fit.
#
#   sudo SUPAVISE_BIN=/path/to/supavise-linux-amd64 tests/linux/functions-smoke.sh [--teardown]
#
# Needs network access (artifacts, the Supabase CLI release, npm, and DNS for
# *.127.0.0.1.sslip.io, which the functions use to reach their own project). Logs stay in
# $LOG_DIR (default /tmp/supavise-linux-logs).
#
# Not run in development: it needs root, systemd and Linux. CI runs it on an ephemeral Ubuntu
# 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1

DEV_PID=""
trap 'rc=$?; [[ -n $DEV_PID ]] && kill -TERM "$DEV_PID" 2>/dev/null; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; exit $rc' EXIT

DOMAIN=127.0.0.1.sslip.io
P_HTTP=18080 P_HTTPS=18443 P_ADMIN=18082 P_EDGE=19000
SUPABASE_CLI_VERSION=${SUPABASE_CLI_VERSION:-2.119.0}

SUPAVISE_DOMAIN=$DOMAIN
preflight
getent hosts "probe.$DOMAIN" >/dev/null || fail "DNS for *.$DOMAIN does not resolve here (sslip.io); the functions reach their own project through it"
for c in node npm; do command -v "$c" >/dev/null || fail "missing $c"; done
install_binary
setup_node
cat >>"$SUPAVISE_CONF" <<CONF

[listen]
http = "127.0.0.1:$P_HTTP"
https = "127.0.0.1:$P_HTTPS"
admin = "127.0.0.1:$P_ADMIN"

[ports]
edge_runtime = $P_EDGE

[functions]
enabled = true
# Longer than the whole run: the runtime retires a worker at its wall clock, and a request that
# reaches it while it drains (graceful_exit_timeout, 10 s) waits for the idle timeout. That is the
# runtime's behavior, not what these checks are about, so no worker may reach it mid-run.
wall_clock_seconds = 600
idle_timeout_seconds = 10
reconcile_seconds = 5
# Low, so that the flood check of verify.mjs can exceed it with a small flood (the default is 128).
max_per_project = 8
# The checks of run.sh keep about 15 functions of two projects warm at once (every function of
# project A and a new worker for each after a secret changes), more than the defaults (16 workers,
# 8 per project) allow. The budget stage at the end runs with small values.
max_workers = 24
max_workers_per_project = 16
CONF

# Same finding as in fleet-smoke.sh: the Postgres launcher chmods a file inside the artifact
# on its first boot, which ProtectSystem=strict forbids. VM-local drop-in, to be removed once
# supavise-postgres@.service handles it.
install -d /etc/systemd/system/supavise-postgres@.service.d
cat >/etc/systemd/system/supavise-postgres@.service.d/10-functions-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/supavise/artifacts
CONF
systemctl daemon-reload

install_supabase_cli() {
  command -v supabase >/dev/null && [[ $(supabase --version 2>/dev/null | head -1) == *"$SUPABASE_CLI_VERSION"* ]] && return 0
  local arch; case $(uname -m) in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) fail "unsupported architecture $(uname -m)" ;; esac
  local base="https://github.com/supabase/cli/releases/download/v$SUPABASE_CLI_VERSION" tgz="supabase_${SUPABASE_CLI_VERSION}_linux_${arch}.tar.gz" d
  d=$(mktemp -d)
  log "downloading the Supabase CLI $SUPABASE_CLI_VERSION ($arch)"
  curl -fsSL --retry 3 -o "$d/$tgz" "$base/$tgz" || fail "download $tgz"
  curl -fsSL --retry 3 -o "$d/checksums.txt" "$base/supabase_${SUPABASE_CLI_VERSION}_checksums.txt" \
    || curl -fsSL --retry 3 -o "$d/checksums.txt" "$base/checksums.txt" || fail "download the checksums"
  (cd "$d" && grep " $tgz\$" checksums.txt | sha256sum -c -) || fail "checksum of $tgz"
  tar -xzf "$d/$tgz" -C "$d" supabase
  install -m 0755 "$d/supabase" /usr/local/bin/supabase
  supabase --version >&2 || true
}
install_supabase_cli

log "system init (downloads artifacts)"
system_init
wait_active supavise-postgres@system.service 30

log "two projects"
REF_A=$(create_project fn-a micro)
REF_B=$(create_project fn-b micro)
[[ $REF_A =~ ^[a-z]{20}$ && $REF_B =~ ^[a-z]{20}$ ]] || fail "bad refs '$REF_A' '$REF_B'"

log "supavise functions dev (API, proxy, and supavise-edge-runtime as a unit)"
mkdir -p "$LOG_DIR"
PAT_FILE=$SUPAVISE_STATE/functions-dev.pat
sudo -u "$SUPAVISE_USER" -H /usr/local/bin/supavise functions dev --token-file "$PAT_FILE" >"$LOG_DIR/functions-dev.log" 2>&1 &
DEV_PID=$!
for ((i = 0; i < 180; i++)); do
  kill -0 "$DEV_PID" 2>/dev/null || { tail -30 "$LOG_DIR/functions-dev.log" >&2; fail "supavise functions dev exited"; }
  [[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 && -s $PAT_FILE ]] && break
  sleep 1
done
[[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 ]] || { tail -30 "$LOG_DIR/functions-dev.log" >&2; fail "the edge runtime did not come up"; }

U=supavise-edge-runtime.service
log "the unit"
[[ $(unit_state "$U") == active ]] || fail "$U is $(unit_state "$U")"
[[ $(systemctl show -p User --value "$U") == "$SUPAVISE_USER" ]] || fail "$U does not run as $SUPAVISE_USER"
[[ $(systemctl show -p Slice --value "$U") == supavise.slice ]] || fail "$U is not in supavise.slice"
# 24 workers (max_workers) x (256 MB memory_mb + 32 MB overhead) + 256 MB for the runtime itself.
[[ $(systemctl show -p MemoryMax --value "$U") == 7516192768 ]] || fail "$U: MemoryMax drop-in not applied ($(systemctl show -p MemoryMax --value "$U"))"
[[ $(ss -Hltn "sport = :$P_EDGE" | awk '{print $4}') == "127.0.0.1:$P_EDGE" ]] || fail "the runtime does not listen on loopback only: $(ss -Hltn "sport = :$P_EDGE")"

sees() { # PATH: exit 0 if PATH is readable from the unit's namespace as the supavise user
  local pid; pid=$(systemctl show -p MainPID --value "$U")
  [[ $pid -gt 0 ]] || fail "$U has no main pid"
  nsenter -t "$pid" -m -- runuser -u "$SUPAVISE_USER" -- test -r "$1" 2>/dev/null
}
for hidden in "$SUPAVISE_STATE/backups" "$SUPAVISE_STATE/certs" /etc/supavise; do
  if sees "$hidden"; then fail "$U can read $hidden"; fi
done
# The runtime sees the tree of functions (its own state directory) and nothing of the projects.
sees "$SUPAVISE_STATE/system/edge-runtime/tenants" || fail "$U cannot see its tenants directory"
# (projects/system exists as the parent of its launcher script, with nothing else in it.)
for hidden in "$SUPAVISE_STATE/projects/$REF_A" "$SUPAVISE_STATE/projects/$REF_A/postgres" "$SUPAVISE_STATE/projects/system/postgres" "$SUPAVISE_STATE/projects/system/postgres.env"; do
  if sees "$hidden"; then fail "$U can read $hidden"; fi
done

log "tests/functions/run.sh"
export SUPAVISE_RUN="sudo -u $SUPAVISE_USER -H /usr/local/bin/supavise" AS_SUPAVISE="sudo -u $SUPAVISE_USER"
export API_URL="http://api.$DOMAIN:$P_HTTP" PAT_FILE PROJECT_URL="http://{ref}.api.$DOMAIN:$P_HTTP"
export REF_A REF_B STATE_DIR=$SUPAVISE_STATE RUNTIME_URL="http://127.0.0.1:$P_EDGE" WORK="$LOG_DIR/functions-work" SANDBOXED_BUNDLER=1 MAX_PER_PROJECT=8
# Processes of the node whose /proc/<pid>/root and environ an uploaded source tries to import: the
# runtime (it holds every project's environment) and the daemon, both of the supavise user.
RT_PID=$(systemctl show -p MainPID --value "$U")
DAEMON_PID=$(pgrep -u "$SUPAVISE_USER" -f 'supavise functions dev' | head -1)
[[ $RT_PID -gt 0 && -n $DAEMON_PID ]] || fail "no pid for the runtime ($RT_PID) or the daemon ($DAEMON_PID)"
export PROC_ESCAPE_PIDS="$RT_PID $DAEMON_PID"
"$REPO_ROOT/tests/functions/run.sh" || fail "tests/functions/run.sh"

log "the bundler's module cache is per project"
# run.sh deleted project B at its end (and with it B's bundler cache: the check follows), so a
# third project, V, plays the part of the victim whose uploads try to import A's cache.
[[ ! -e /var/cache/private/supavise-edge-bundle/$REF_B && ! -e /var/cache/supavise-edge-bundle/$REF_B ]] || fail "the module cache of the deleted project $REF_B is still there"
REF_V=$(create_project fn-v micro)
[[ $REF_V =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF_V'"
# Project A's upload makes the bundler fetch a package; project V's uploads must not be able to
# import what is now in A's cache, however the path is spelled. The marker is the package.json of
# the package, a JSON module that an import can name (as the steal checks of run.sh do).
PAT=$(<"$PAT_FILE")
API="http://api.$DOMAIN:$P_HTTP"
CACHE_ROOT=/var/cache/supavise-edge-bundle
deploy_src() { # REF SLUG SOURCE-FILE: upload sources, print "<body>\n<status>"
  curl -s -w '\n%{http_code}' -X POST -H "Authorization: Bearer $PAT" \
    -F "metadata={\"entrypoint_path\":\"index.ts\",\"name\":\"$2\"};type=application/json" \
    -F "file=@$3;filename=index.ts" "$API/v1/projects/$1/functions/deploy?slug=$2"
}
delete_fn() { curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $PAT" "$API/v1/projects/$1/functions/$2"; }
printf 'import postgres from "npm:postgres@3.4.5"\nDeno.serve(() => new Response(typeof postgres))\n' >"$LOG_DIR/cache-mark.ts"
cache_marker() { # REF: the package.json of the package in REF's cache, as the unit's namespace names it
  find "$CACHE_ROOT/$1/" -path '*/postgres/3.4.5/package.json' 2>/dev/null | head -1
}
for ref in "$REF_A" "$REF_V"; do
  out=$(deploy_src "$ref" cachemark "$LOG_DIR/cache-mark.ts") || fail "curl: cachemark upload to $ref"
  [[ $(tail -n1 <<<"$out") =~ ^20[01]$ ]] || fail "the upload that imports npm:postgres to $ref answered: $out"
  [[ -d $CACHE_ROOT/$ref ]] || fail "the bundler of $ref has no cache directory $CACHE_ROOT/$ref ($(ls -la "$CACHE_ROOT/" /var/cache/private/ 2>&1 | tr '\n' ' '))"
  [[ $(stat -c %a "$CACHE_ROOT/$ref/") == 700 ]] || fail "$CACHE_ROOT/$ref has mode $(stat -c %a "$CACHE_ROOT/$ref/"), want 700"
done
A_MARKER=$(cache_marker "$REF_A"); V_MARKER=$(cache_marker "$REF_V")
[[ -n $A_MARKER && -n $V_MARKER ]] || fail "no cached postgres package in the caches of A ('$A_MARKER') or V ('$V_MARKER'): $(find "$CACHE_ROOT/$REF_A/" -maxdepth 4 2>&1 | head -20 | tr '\n' ' ')"
[[ $A_MARKER != "$V_MARKER" && $A_MARKER == "$CACHE_ROOT/$REF_A/"* && $V_MARKER == "$CACHE_ROOT/$REF_V/"* ]] || fail "the caches are not separate directories: $A_MARKER $V_MARKER"
# Control: a bundling of V can import a module of V's own cache, so the refusals below are about
# whose cache it is and not about importing a file by path.
steal_cache() { # NAME IMPORT-PATH EXPECTED-STATUS-REGEX
  printf 'import marker from "file://%s" with { type: "json" }\nDeno.serve(() => Response.json(marker))\n' "$2" >"$LOG_DIR/cache-steal.ts"
  local out; out=$(deploy_src "$REF_V" cachesteal "$LOG_DIR/cache-steal.ts") || fail "curl: cache steal upload ($1)"
  [[ $(tail -n1 <<<"$out") =~ $3 ]] || fail "an upload of V that imports $1 answered: $out"
  if [[ $3 == '^400$' ]]; then
    grep -q "Could not bundle" <<<"$out" || fail "the refusal ($1) does not say why: $out"
    grep -q '"name": *"postgres"' <<<"$out" && fail "the bundler's error ($1) shows the package of A's cache: $out"
    [[ $(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $PAT" "$API/v1/projects/$REF_V/functions/cachesteal") == 404 ]] || fail "the refused upload ($1) was stored"
  else
    delete_fn "$REF_V" cachesteal
  fi
}
steal_cache "its own cache (control)" "$V_MARKER" '^20[01]$'
steal_cache "A's cache by its path" "$A_MARKER" '^400$'
steal_cache "A's cache through the private directory" "/var/cache/private/supavise-edge-bundle/${A_MARKER#"$CACHE_ROOT"/}" '^400$'
steal_cache "A's cache through V's own directory" "$CACHE_ROOT/$REF_V/../${A_MARKER#"$CACHE_ROOT"/}" '^400$'
steal_cache "the shared cache of earlier versions" "$CACHE_ROOT/${A_MARKER#"$CACHE_ROOT/$REF_A/"}" '^400$'
delete_fn "$REF_A" cachemark; delete_fn "$REF_V" cachemark

log "the bundler unit ran for project V's uploads and is idle now"
B=supavise-edge-bundle@$REF_V.service
[[ $(unit_state "$B") == inactive ]] || fail "$B is $(unit_state "$B")"
[[ $(systemctl show -p Result --value "$B") == success ]] || fail "$B: last result $(systemctl show -p Result --value "$B")"
# Another uid than the supavise user: the kernel then refuses /proc/<pid>/root and environ of every
# process of the node (below), and /proc shows none of them.
[[ $(systemctl show -p DynamicUser --value "$B") == yes && $(systemctl show -p User --value "$B") != "$SUPAVISE_USER" ]] || fail "$B does not run under a uid of its own (DynamicUser=$(systemctl show -p DynamicUser --value "$B"), User=$(systemctl show -p User --value "$B"))"
[[ $(systemctl show -p ProtectProc --value "$B") == invisible && $(systemctl show -p ProcSubset --value "$B") == pid ]] || fail "$B: ProtectProc/ProcSubset not applied"
[[ $(systemctl show -p MemoryMax --value "$B") == 1073741824 ]] || fail "$B: MemoryMax $(systemctl show -p MemoryMax --value "$B")"
systemctl show -p IPAddressDeny --value "$B" | grep -q . || fail "$B has no IPAddressDeny"
[[ ! -e $SUPAVISE_STATE/system/edge-bundle/work ]] || fail "the scratch directory of an upload stayed in $SUPAVISE_STATE/system/edge-bundle"

log "the bundler's uid cannot read the node's files, the processes of its units or another project's cache"
# The real unit (an instance for A and one for B), with its ExecStart replaced by a probe (a
# drop-in, removed afterwards), so the probe runs as the bundler does: same uid, namespace, /proc
# and cache directory. It exits 3 when it could read something it must not, which fails this
# start (1 is a bad upload, see the unit). It also prints its uid: the two instances must differ.
cat >/usr/local/sbin/supavise-bundle-probe <<'PROBE'
#!/bin/sh
bad=0
violate() { echo "probe: VIOLATION: $*"; bad=1; }
ok() { echo "probe: ok $*"; }
[ "$(id -u)" != "$PROBE_SUPAVISE_UID" ] && ok "runs under uid $(id -u), not the supavise user's $PROBE_SUPAVISE_UID" || violate "runs as the supavise uid"
# Controls: what the unit may do works, so the refusals below say something.
cat /proc/self/environ >/dev/null 2>&1 && ok "reads its own /proc/self/environ" || violate "cannot read /proc/self/environ (control)"
ls /var/lib/supavise/artifacts >/dev/null 2>&1 && ok "reads the artifacts" || violate "cannot read the artifacts (control)"
echo "probe: uid=$(id -u)"
touch "/var/cache/supavise-edge-bundle/$PROBE_SELF/probe" 2>/dev/null && ok "writes its own cache directory" || violate "cannot write /var/cache/supavise-edge-bundle/$PROBE_SELF (control)"
cat "$PROBE_SELF_MARKER" >/dev/null 2>&1 && ok "reads a module of its own cache" || violate "cannot read $PROBE_SELF_MARKER (control)"
# Another project's cache: whatever the namespace shows of it, this uid cannot open it.
echo "probe: info, visible under /var/cache/supavise-edge-bundle: $(ls -A /var/cache/supavise-edge-bundle 2>&1 | tr '\n' ' ')"
ls "/var/cache/supavise-edge-bundle/$PROBE_OTHER/" >/dev/null 2>&1 && violate "lists the cache directory of another project"
ls "/var/cache/private/supavise-edge-bundle/$PROBE_OTHER/" >/dev/null 2>&1 && violate "lists the cache directory of another project through /var/cache/private"
cat "$PROBE_OTHER_MARKER" >/dev/null 2>&1 && violate "reads $PROBE_OTHER_MARKER"
cat "/var/cache/private/supavise-edge-bundle/${PROBE_OTHER_MARKER#/var/cache/supavise-edge-bundle/}" >/dev/null 2>&1 && violate "reads another project's cache through /var/cache/private"
# The two files the daemon creates are writable, and nothing else can be made there.
echo probe >/var/lib/supavise/system/edge-bundle/work/out/out.eszip 2>/dev/null && ok "writes the output file" || violate "cannot write the output file (control)"
touch /var/lib/supavise/system/edge-bundle/work/out/other 2>/dev/null && violate "creates a file next to the output files"
touch /var/lib/supavise/system/edge-bundle/work/src/probe 2>/dev/null && violate "writes the sources"
rm -f /var/lib/supavise/system/edge-bundle/work/out/out.eszip 2>/dev/null && violate "deletes the output file"
# What it must not do.
env_file="$PROBE_TENANTS/$PROBE_REF/functions-env.json"
cat "$env_file" >/dev/null 2>&1 && violate "reads $env_file"
for pid in $PROBE_RT_PID $PROBE_DAEMON_PID; do
  [ -e "/proc/$pid" ] && violate "/proc/$pid is visible"
  cat "/proc/$pid/environ" >/dev/null 2>&1 && violate "reads /proc/$pid/environ"
  ls "/proc/$pid/root/" >/dev/null 2>&1 && violate "lists /proc/$pid/root/"
  cat "/proc/$pid/root$env_file" >/dev/null 2>&1 && violate "reads $env_file through /proc/$pid/root"
done
[ -e /proc/1 ] && violate "/proc/1 is visible"
[ -e /proc/meminfo ] && violate "/proc/meminfo is visible (ProcSubset=pid)"
n=$(ls /proc | grep -c '^[0-9][0-9]*$')
[ "$n" -le 8 ] && ok "/proc lists $n processes, all of them this unit's" || violate "/proc lists $n processes"
for d in /var/lib/supavise/backups /var/lib/supavise/certs /var/lib/supavise/projects/system/edge-runtime.env /var/lib/supavise/system/edge-runtime /etc/supavise/master.key; do
  [ -e "$d" ] && violate "sees $d"
done
[ "$bad" = 0 ] && exit 0 || exit 3
PROBE
chmod 0755 /usr/local/sbin/supavise-bundle-probe
install -d -o "$SUPAVISE_USER" -g "$SUPAVISE_USER" -m 0755 "$SUPAVISE_STATE/system/edge-bundle/work/src" "$SUPAVISE_STATE/system/edge-bundle/work/out"
install -m 0666 /dev/null "$SUPAVISE_STATE/system/edge-bundle/work/out/out.eszip"
chown "$SUPAVISE_USER:$SUPAVISE_USER" "$SUPAVISE_STATE/system/edge-bundle/work/out/out.eszip"
declare -A PROBE_UID
for pair in "$REF_V:$REF_A:$A_MARKER:$V_MARKER" "$REF_A:$REF_V:$V_MARKER:$A_MARKER"; do
  IFS=: read -r self other other_marker self_marker <<<"$pair"
  inst=supavise-edge-bundle@$self.service
  install -d /run/systemd/system/$inst.d
  cat >/run/systemd/system/$inst.d/90-probe.conf <<CONF
[Service]
Environment=PROBE_RT_PID=$RT_PID PROBE_DAEMON_PID=$DAEMON_PID PROBE_SUPAVISE_UID=$(id -u "$SUPAVISE_USER") PROBE_TENANTS=$SUPAVISE_STATE/system/edge-runtime/tenants PROBE_REF=$REF_A
Environment=PROBE_SELF=$self PROBE_OTHER=$other PROBE_SELF_MARKER=$self_marker PROBE_OTHER_MARKER=$other_marker
ExecStart=
ExecStart=/usr/local/sbin/supavise-bundle-probe
CONF
  systemctl daemon-reload
  # The unit does not run as the daemon would start it (no upload): a supavise-owned work/src and work/out
  # stand in for what the daemon lays out. A start by root is fine for the probe.
  if ! systemctl start "$inst"; then
    journalctl -u "$inst" -n 60 --no-pager -o cat >&2 || true
    fail "$inst: the bundler's uid could read what it must not (see the probe lines above), or the probe did not run"
  fi
  journalctl -u "$inst" -n 60 --no-pager -o cat | grep '^probe: ' >&2 || fail "the probe of $inst printed nothing"
  PROBE_UID[$self]=$(journalctl -u "$inst" -n 60 --no-pager -o cat | sed -n 's/^probe: uid=\([0-9][0-9]*\)$/\1/p' | tail -1)
  rm -rf /run/systemd/system/$inst.d
  systemctl daemon-reload
  [[ -z $(systemctl show -p DropInPaths --value "$inst") ]] || fail "the probe drop-in of $inst is still in place"
done
rm -f /usr/local/sbin/supavise-bundle-probe
rm -rf "$SUPAVISE_STATE/system/edge-bundle/work"
# Each project's bundler runs under a uid of its own, so that the mode of its cache directory (0700) keeps
# the others out even where a directory of another project is in reach.
[[ -n ${PROBE_UID[$REF_A]} && -n ${PROBE_UID[$REF_V]} && ${PROBE_UID[$REF_A]} != "${PROBE_UID[$REF_V]}" ]] || fail "the bundlers of A and V do not run under different uids (A: ${PROBE_UID[$REF_A]:-?}, V: ${PROBE_UID[$REF_V]:-?})"
[[ ${PROBE_UID[$REF_A]} != "$(id -u "$SUPAVISE_USER")" && ${PROBE_UID[$REF_V]} != "$(id -u "$SUPAVISE_USER")" ]] || fail "a bundler runs under the supavise uid"

log "crash recovery: kill -9 of the runtime"
open_code() { http_code "http://$REF_A.api.$DOMAIN:$P_HTTP/functions/v1/open"; }
[[ $(open_code) == 200 ]] || fail "open before the crash: $(open_code)"
pid=$(systemctl show -p MainPID --value "$U")
kill -9 "$pid"
ok=0
for ((i = 0; i < 60; i++)); do
  sleep 1
  new=$(systemctl show -p MainPID --value "$U")
  if [[ $new -gt 0 && $new != "$pid" && $(unit_state "$U") == active && $(open_code) == 200 ]]; then ok=1; break; fi
done
[[ $ok -eq 1 ]] || fail "$U did not come back and serve after kill -9"
log "$U recovered (MemoryCurrent $(( $(systemctl show -p MemoryCurrent --value "$U") / 1048576 )) MiB)"

log "worker budget: restart with max_workers = 4 and 3 per project"
# One runtime serves every project and supavise-main enforces what fits into its memory limit:
# at most 4 live workers (4 x 288 MB + 256 MB = the unit's MemoryMax), at most 3 of one project.
# Over-budget requests are refused with 503, nothing is killed, warm functions keep answering.
kill -TERM "$DEV_PID"; wait "$DEV_PID" 2>/dev/null || true; DEV_PID=""
for ((i = 0; i < 60; i++)); do [[ $(unit_state "$U") == active ]] || break; sleep 1; done
sed -i -e 's/^max_workers = 24$/max_workers = 4/' -e 's/^max_workers_per_project = 16$/max_workers_per_project = 3/' "$SUPAVISE_CONF"
grep -q '^max_workers = 4$' "$SUPAVISE_CONF" && grep -q '^max_workers_per_project = 3$' "$SUPAVISE_CONF" || fail "could not set max_workers"
rm -f "$PAT_FILE"
sudo -u "$SUPAVISE_USER" -H /usr/local/bin/supavise functions dev --token-file "$PAT_FILE" >"$LOG_DIR/functions-dev-budget.log" 2>&1 &
DEV_PID=$!
for ((i = 0; i < 180; i++)); do
  kill -0 "$DEV_PID" 2>/dev/null || { tail -30 "$LOG_DIR/functions-dev-budget.log" >&2; fail "supavise functions dev exited"; }
  [[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 && -s $PAT_FILE ]] && break
  sleep 1
done
[[ $(http_code "http://127.0.0.1:$P_EDGE/_internal/health") == 200 ]] || fail "the edge runtime did not come back with the budget"
[[ $(systemctl show -p MemoryMax --value "$U") == 1476395008 ]] || fail "$U: MemoryMax for 4 workers is $(systemctl show -p MemoryMax --value "$U"), want 1476395008 (4 x 288 + 256 MiB)"

REF_C=$(create_project fn-c micro)
PAT=$(<"$PAT_FILE")
API_URL="http://api.$DOMAIN:$P_HTTP"
# One real bundle (project A's own `open`, as the node materialized it), uploaded as many slugs as needed.
node -e '
  const z = require("node:zlib"), fs = require("node:fs")
  const raw = fs.readFileSync(process.argv[1])
  fs.writeFileSync(process.argv[2], Buffer.concat([Buffer.from("EZBR"), z.brotliCompressSync(raw)]))' \
  "$SUPAVISE_STATE/system/edge-runtime/tenants/$REF_A/functions/open/bundle.eszip" "$LOG_DIR/budget.ezbr" || fail "could not make the bundle"
entry=$(curl -s -H "Authorization: Bearer $PAT" "$API_URL/v1/projects/$REF_A/functions/open" | python3 -c 'import json,sys; print(json.load(sys.stdin)["entrypoint_path"])')
[[ -n $entry ]] || fail "no entrypoint_path for A/open"
entry_q=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$entry")
upload() { # REF SLUG
  [[ $(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $PAT" -H 'Content-Type: application/vnd.denoland.eszip' \
    --data-binary "@$LOG_DIR/budget.ezbr" "$API_URL/v1/projects/$1/functions?slug=$2&name=$2&verify_jwt=false&entrypoint_path=$entry_q") == 201 ]] || fail "upload $1/$2"
}
for s in b1 b2 b3 b4; do upload "$REF_A" "$s"; done
for s in c1 c2; do upload "$REF_C" "$s"; done
fn_code() { http_code --max-time 30 "http://$1.api.$DOMAIN:$P_HTTP/functions/v1/$2"; }
fn_err() { curl -s -D - -o /dev/null --max-time 30 "http://$1.api.$DOMAIN:$P_HTTP/functions/v1/$2" | tr -d '\r' | awk -F': ' 'tolower($1)=="sb-error-code" {print $2}'; }
for s in b1 b2 b3; do [[ $(fn_code "$REF_A" "$s") == 200 ]] || fail "A/$s did not answer 200 within the budget: $(fn_code "$REF_A" "$s")"; done
[[ $(fn_code "$REF_A" b4) == 503 && $(fn_err "$REF_A" b4) == PROJECT_AT_CAPACITY ]] || fail "A/b4 (a 4th function of one project) was not refused: $(fn_code "$REF_A" b4)"
[[ $(fn_code "$REF_C" c1) == 200 ]] || fail "C/c1 (the 4th worker of the runtime) answered $(fn_code "$REF_C" c1)"
[[ $(fn_code "$REF_C" c2) == 503 && $(fn_err "$REF_C" c2) == PROJECT_AT_CAPACITY ]] || fail "C/c2 (a 5th worker for the runtime) was not refused: $(fn_code "$REF_C" c2)"
for s in b1 b2 b3; do [[ $(fn_code "$REF_A" "$s") == 200 ]] || fail "warm A/$s stopped answering"; done
[[ $(fn_code "$REF_C" c1) == 200 ]] || fail "warm C/c1 stopped answering"
[[ $(unit_state "$U") == active ]] || fail "$U is $(unit_state "$U") after the budget was spent"
mem=$(systemctl show -p MemoryCurrent --value "$U")
log "$U holds 4 workers: MemoryCurrent $((mem / 1048576)) MiB of 1408"
(( mem < 1476395008 )) || fail "$U uses $mem bytes, over its limit"
[[ $(systemctl show -p NRestarts --value "$U") == 0 ]] || fail "$U restarted"

log "deleting a project removes the module cache of its bundler"
# V's bundler has a cache directory (above), private to that instance's uid, which the daemon cannot
# delete itself; V goes, A's cache stays.
out=$(deploy_src "$REF_V" cachemark "$LOG_DIR/cache-mark.ts") || fail "curl: cachemark upload to $REF_V"
[[ $(tail -n1 <<<"$out") =~ ^20[01]$ ]] || fail "the upload that imports npm:postgres to $REF_V answered: $out"
[[ -d $CACHE_ROOT/$REF_V && -n $(cache_marker "$REF_V") ]] || fail "the bundler of $REF_V has no cached package in $CACHE_ROOT/$REF_V"
[[ -f $SUPAVISE_STATE/projects/$REF_V/edge-bundle.env ]] || fail "no rendered files for the bundler of $REF_V"
supavise projects delete "$REF_V" --skip-final-backup >/dev/null || fail "projects delete $REF_V"
for gone in "$CACHE_ROOT/$REF_V" "/var/cache/private/supavise-edge-bundle/$REF_V" "$SUPAVISE_STATE/projects/$REF_V"; do
  [[ ! -e $gone ]] || fail "$gone is still there after the project was deleted"
done
[[ $(unit_state "supavise-edge-bundle@$REF_V.service") == inactive ]] || fail "the bundler of the deleted project is $(unit_state "supavise-edge-bundle@$REF_V.service")"
[[ -d $CACHE_ROOT/$REF_A && -n $(cache_marker "$REF_A") ]] || fail "deleting $REF_V removed the cache of project A"

log "functions smoke test passed"
