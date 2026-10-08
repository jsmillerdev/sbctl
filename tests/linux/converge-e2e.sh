#!/usr/bin/env bash
# Host convergence end to end: `supavise system converge` on a node that a v0.1.x-shaped release
# (PREV, the merge base with origin/main) installed, with a project running.
#
#   sudo SUPAVISE_BIN_PREV=/path/to/prev SUPAVISE_BIN=/path/to/new tests/linux/converge-e2e.sh
#
# Without the two binaries the script builds them with go and git (origin/main must be fetched).
# Case 6b needs a third binary, SUPAVISE_BIN_NEXT (this checkout as v0.2.1; built with go when it is
# there) and, without go, a built deploy/releasetool in SUPAVISE_RELEASETOOL.
# Needs root, systemd, cgroup v2 and network access (artifact downloads). Do not run it on a machine
# you care about: it creates the supavise user, writes /etc/supavise, installs units and starts real
# clusters. Exit status is non-zero on the first failure. The cases, in order:
#
#  1. Static checks of install.sh and of this script.
#  2. PREV is installed with deploy/install.sh and a project is created: it has no converged
#     marker, no /etc/supavise/cluster, no config.d.
#  3. The old driver's step: the binary is replaced and `supavise system install-units` runs on the
#     new binary, as `supavise upgrade` of v0.1.x does right after the swap. That is converge: the
#     marker records the revision `release-info` names, the directories exist (0750, supavise), a
#     second run changes nothing, `--check --json` is a list of {id, title, pending, needs_root}
#     that has nothing pending, and no unit restarted (the same MainPID and start time for every
#     supavise unit).
#  4. The daemon restarts onto the new binary, as the old driver does next: the registry migrates, the
#     system and project PostgreSQL clusters and the project's GoTrue and PostgREST keep their
#     processes (invariant I4: an upgrade restarts no project).
#  5. With the marker behind, the daemon raises host_not_converged (a webhook receiver sees it);
#     `converge` sets the marker right again.
#  6. `converge --check` needs no root (the supavise user runs it), and a fake ufw that is active
#     without the rule: `--check` reports it, `converge` opens 7443/tcp, a second run does not.
#  6b. The new driver: `supavise upgrade` to a signed release (a build of this checkout as v0.2.1,
#     served by a local server) whose converge fails after the swap (a ufw that refuses the rule).
#     That fails the upgrade: exit status 3, the node back on v0.2.0, the marker as it was, no project
#     restarted. The same upgrade without the broken ufw succeeds (exit 0) and converges the host
#     with no project restarted (invariant I4).
#  7. install.sh refuses a --join-token-file that cannot be used (a missing, empty or open file, an
#     empty value) before it downloads or changes anything, `supavise install` refuses an empty
#     --join-token-file and a --data-device without --aws-first-boot, `supavise install
#     --aws-first-boot` formats nothing off EC2, and the flags are listed. (tests/linux/
#     firstboot-e2e.sh runs first boot on loop devices.)
#  8. The mounts: with /var/lib/supavise and /etc/supavise mount points (bind mounts of themselves),
#     converge writes the RequiresMountsFor drop-ins for every supavise service unit, systemd reads
#     them, and a second run changes nothing.
BASE=127.0.0.1.sslip.io
export SUPAVISE_DOMAIN=$BASE
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

WORK=$(mktemp -d)
chmod 0755 "$WORK"
HOOK_PID=""
SRV_PID=""
BOUND=0 # set once the mount checks have bound the two directories to themselves
cleanup() {
  local rc=$?
  [[ -n $HOOK_PID ]] && kill "$HOOK_PID" 2>/dev/null || true
  [[ -n $SRV_PID ]] && kill "$SRV_PID" 2>/dev/null || true
  if [[ $BOUND -eq 1 ]]; then
    umount -l /etc/supavise 2>/dev/null || true
    umount -l /var/lib/supavise 2>/dev/null || true
  fi
  collect_logs
  cp -r "$WORK"/*.log "$LOG_DIR/" 2>/dev/null || true
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

need_root
preflight
cd "$REPO_ROOT" || exit 1
SV=/usr/local/bin/supavise
STATE=$SUPAVISE_STATE
ADMIN=http://127.0.0.1:7000

# ---- 1. static checks -----------------------------------------------------------------------
log "static checks"
bash -n deploy/install.sh tests/linux/converge-e2e.sh
if command -v shellcheck >/dev/null; then
  shellcheck -x -S warning deploy/install.sh tests/linux/converge-e2e.sh
else
  log "shellcheck is not installed; skipped"
fi

# ---- the two binaries ------------------------------------------------------------------------
# PREV is the checkout as origin/main last had it: a node that main installed is what a v0.1.x node
# is. NEW is this checkout.
PREV=${SUPAVISE_BIN_PREV:-}
if [[ -z $PREV ]]; then
  command -v go >/dev/null || fail "no SUPAVISE_BIN_PREV and no go toolchain"
  git fetch --no-tags origin main 2>/dev/null || true
  base=$(git merge-base HEAD FETCH_HEAD 2>/dev/null || git rev-parse HEAD)
  git worktree add --detach "$WORK/prev-tree" "$base" >/dev/null
  PREV=$WORK/supavise-prev
  (cd "$WORK/prev-tree" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.1.1" -o "$PREV" ./cmd/supavise)
  git worktree remove --force "$WORK/prev-tree"
fi
NEW=${SUPAVISE_BIN:-}
if [[ -z $NEW ]]; then
  command -v go >/dev/null || fail "no SUPAVISE_BIN and no go toolchain"
  NEW=$WORK/supavise-new
  CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.2.0" -o "$NEW" ./cmd/supavise
fi
NEXT=${SUPAVISE_BIN_NEXT:-}
if [[ -z $NEXT ]]; then
  command -v go >/dev/null || fail "no SUPAVISE_BIN_NEXT and no go toolchain"
  NEXT=$WORK/supavise-next
  CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.2.1" -o "$NEXT" ./cmd/supavise
fi
[[ $("$NEXT" --version) == *v0.2.1* ]] || fail "SUPAVISE_BIN_NEXT reports $("$NEXT" --version), want v0.2.1"
install -d -m 0755 /opt/supavise-e2e
install -m 0755 "$PREV" /opt/supavise-e2e/prev
install -m 0755 "$NEW" /opt/supavise-e2e/new
install -m 0755 "$NEXT" /opt/supavise-e2e/next
PREV_HAS_CONVERGE=0
/opt/supavise-e2e/prev system --help >"$WORK/prev-help.log" 2>&1 || true
grep -q '^  converge' "$WORK/prev-help.log" && PREV_HAS_CONVERGE=1
if [[ $PREV_HAS_CONVERGE -eq 1 ]]; then
  log "the previous release already has 'system converge': the checks about a node without a marker are skipped"
fi
REV=$(/opt/supavise-e2e/new release-info --json | json_get 'd["converge_revision"]')
[[ $REV =~ ^[1-9][0-9]*$ ]] || fail "release-info names no converge revision: $REV"
[[ $(/opt/supavise-e2e/new release-info --json | json_get 'len(d["host_changes"])') -ge 1 ]] || fail "release-info lists no host changes"

# ---- 2. the previous release, with a project -----------------------------------------------
systemctl stop apache2 nginx postgresql mysql 2>/dev/null || true
for p in 80 443 5432 6543 5433 9999 7000 3000 8080 4000 5000; do
  if ss -ltnH "sport = :$p" | grep -q .; then ss -ltnp "sport = :$p" >&2; fail "port $p is already in use on this VM"; fi
done
log "install.sh --binary: the previous release"
deploy/install.sh --binary /opt/supavise-e2e/prev --public-ip 127.0.0.1 --tls off --email ci@example.com --firewall none --no-studio \
  --claim-token-file "$WORK/claim-token" 2>&1 | tee "$WORK/install.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "install.sh failed"
for u in supavise.service supavise-postgres@system.service supavise-gotrue@system.service; do wait_active "$u" 90; done

# pending_ids: the ids of the steps that `converge --check` reports pending, in a comma list with a
# comma at each end, so that a step is looked for as ",id,". The answers are read into a variable
# first: a grep -q that ends a pipe early makes the writer die of SIGPIPE, and pipefail then fails.
pending_ids() { local out; out=$("$SV" system converge --check --json) || return 1; printf ',%s,' "$(json_get '",".join(s["id"] for s in d if s["pending"])' <<<"$out")"; }
is_pending() { [[ $(pending_ids) == *",$1,"* ]]; }

wait_daemon() {
  local i
  for ((i = 0; i < 120; i++)); do [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && return 0; sleep 1; done
  journalctl --no-pager -u supavise.service | tail -40 >&2
  fail "the Management API does not answer"
}
wait_daemon
claim_and_token
gen_dbpass
REF=$(api_create_project converge-e2e)
log "project $REF"

if [[ $PREV_HAS_CONVERGE -eq 0 ]]; then
  [[ ! -e $STATE/converged ]] || fail "the previous release left a converged marker"
  [[ ! -e /etc/supavise/cluster ]] || fail "the previous release made /etc/supavise/cluster"
fi

# stamp UNIT: the process and the time its current run began; a restarted unit changes both.
stamp() { systemctl show -p MainPID -p ActiveEnterTimestampMonotonic "$1" | tr '\n' ' '; }
UNITS=(supavise.service supavise-postgres@system.service supavise-gotrue@system.service "supavise-postgres@$REF.service" "supavise-gotrue@$REF.service" "supavise-postgrest@$REF.service")
snapshot() { local u; for u in "${UNITS[@]}"; do echo "$u $(stamp "$u")"; done; }
for u in "${UNITS[@]}"; do wait_active "$u" 60; done
SNAP0=$(snapshot)
PIDS0=$(for u in "${UNITS[@]}"; do systemctl show -p MainPID --value "$u"; done | paste -sd, -)

# ---- 3. the old driver's step ---------------------------------------------------------------
log "the old driver's step: the binary is replaced, then 'system install-units' runs on the new one"
install -m 0755 /opt/supavise-e2e/new "$SV.new"
mv -f "$SV.new" "$SV"
[[ $($SV --version) == *v0.2.0* ]] || fail "the installed binary is $($SV --version)"
$SV system converge --check --json >"$WORK/check0.json" || fail "converge --check"
python3 - "$WORK/check0.json" <<'PY' || fail "converge --check --json is not a list of steps: $(cat "$WORK/check0.json")"
import json, sys
steps = json.load(open(sys.argv[1]))
assert isinstance(steps, list) and steps, steps
for s in steps:
    assert set(["id", "title", "pending", "needs_root"]) <= set(s), s
assert steps[-1]["id"] == "marker", steps
PY
if [[ $PREV_HAS_CONVERGE -eq 0 ]]; then
  is_pending directories || fail "a v0.1.x node has no pending directories step: $(cat "$WORK/check0.json")"
  is_pending marker || fail "a v0.1.x node has no pending marker: $(cat "$WORK/check0.json")"
fi

$SV system install-units 2>&1 | tee "$WORK/converge1.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "system install-units (converge) failed"
grep -q "recorded converge revision $REV" "$WORK/converge1.log" || [[ $PREV_HAS_CONVERGE -eq 1 ]] || fail "converge did not record revision $REV"
grep -qx "revision=$REV" "$STATE/converged" || fail "the marker is $(cat "$STATE/converged" 2>&1)"
[[ $(stat -c '%a' "$STATE/converged") == 644 ]] || fail "the marker is mode $(stat -c '%a' "$STATE/converged")"
for d in cluster config.d; do
  [[ $(stat -c '%U:%G:%a' "/etc/supavise/$d") == supavise:supavise:750 ]] || fail "/etc/supavise/$d is $(stat -c '%U:%G:%a' "/etc/supavise/$d" 2>&1), want supavise:supavise:750"
done
[[ $(pending_ids) == ",," ]] || fail "steps are pending after converge: $($SV system converge --check --json)"
$SV system converge 2>&1 | tee "$WORK/converge2.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "the second converge failed"
grep -q "units are up to date" "$WORK/converge2.log" || fail "the second converge does not say the units are up to date"
grep -q -e installed -e created -e wrote -e allowed -e recorded "$WORK/converge2.log" && fail "the second converge changed something: $(cat "$WORK/converge2.log")"
[[ $(snapshot) == "$SNAP0" ]] || fail "converge restarted a unit:
$SNAP0
--
$(snapshot)"
log "converge applied; no unit restarted"

# ---- 4. the daemon restarts onto the new binary ---------------------------------------------
log "the old driver restarts the daemon"
systemctl restart supavise.service
wait_daemon
for u in "${UNITS[@]:1}"; do wait_active "$u" 60; done
PIDS1=$(for u in "${UNITS[@]}"; do systemctl show -p MainPID --value "$u"; done | paste -sd, -)
# The daemon is the first unit and it did restart; every other process must be the same.
[[ ${PIDS0#*,} == "${PIDS1#*,}" ]] || fail "a project's process was restarted by the new daemon (I4):
before $PIDS0
after  $PIDS1"
[[ $(papi GET "/v1/projects/$REF" | json_get 'd["status"]') == ACTIVE_HEALTHY ]] || fail "the project is not ACTIVE_HEALTHY after the daemon restart"
log "the project's processes are the same (I4)"

# ---- 5. host_not_converged ------------------------------------------------------------------
log "a marker that is behind: the daemon raises host_not_converged"
cat >"$WORK/hook.py" <<'PY'
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with open(sys.argv[2], "ab") as f:
            f.write(body + b"\n")
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
HOOK_PORT=38803
python3 "$WORK/hook.py" "$HOOK_PORT" "$WORK/hook.jsonl" &
HOOK_PID=$!
for ((i = 0; i < 40; i++)); do ss -ltnH "sport = :$HOOK_PORT" | grep -q . && break; sleep 0.25; done
printf '\n[[alerts.webhooks]]\nurl = "http://127.0.0.1:%s/hook"\n' "$HOOK_PORT" >>/etc/supavise/config.toml
printf 'revision=0\n' >"$STATE/converged"
is_pending marker || fail "a marker at revision 0 is not pending"
systemctl restart supavise.service
wait_daemon
hook_kinds() { python3 -c '
import json, os, sys
if os.path.exists(sys.argv[1]):
    print(",".join(json.loads(l)["kind"] for l in open(sys.argv[1]) if l.strip()))' "$WORK/hook.jsonl"; }
for ((i = 0; i < 150; i++)); do
  [[ $(hook_kinds) == *host_not_converged* ]] && break
  sleep 1
done
[[ $(hook_kinds) == *host_not_converged* ]] || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "the daemon did not raise host_not_converged within 150 s: $(hook_kinds)"; }
grep -q 'sudo supavise system converge' "$WORK/hook.jsonl" || fail "the alert does not name the command: $(tail -1 "$WORK/hook.jsonl")"
$SV system converge >/dev/null || fail "converge after the alert"
grep -qx "revision=$REV" "$STATE/converged" || fail "converge did not set the marker right: $(cat "$STATE/converged")"
log "host_not_converged raised and cleared by converge"

# ---- 6. no root, and the firewall -----------------------------------------------------------
log "converge --check as the supavise user"
as_user=$(sudo -u "$SUPAVISE_USER" -H "$SV" system converge --check --json) || fail "converge --check as the supavise user"
[[ $(json_get 'len(d)' <<<"$as_user") -ge 1 ]] || fail "converge --check as the supavise user printed no steps: $as_user"
run_as_user_apply=$(sudo -u "$SUPAVISE_USER" -H "$SV" system converge 2>&1 || true)
[[ $run_as_user_apply == *"run as root"* ]] || fail "converge without root: $run_as_user_apply"

log "a ufw that is active and does not admit the mesh port"
FAKE=$WORK/fakebin; mkdir -p "$FAKE"
UFW_STATE=$WORK/ufw-rules; : >"$UFW_STATE"
cat >"$FAKE/ufw" <<UFW
#!/usr/bin/env bash
case "\$1" in
  status) printf 'Status: active\n\nTo                         Action      From\n--                         ------      ----\n22/tcp                     ALLOW       Anywhere\n'; cat "$UFW_STATE" ;;
  allow) printf '%-26s ALLOW       Anywhere\n' "\$2" >>"$UFW_STATE" ;;
  *) exit 1 ;;
esac
UFW
chmod 0755 "$FAKE/ufw"
PATH="$FAKE:$PATH" is_pending ufw || fail "an active ufw without 7443/tcp is not pending"
PATH="$FAKE:$PATH" $SV system converge 2>&1 | tee "$WORK/converge3.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "converge with ufw failed"
grep -q "allowed 7443/tcp in ufw" "$WORK/converge3.log" || fail "converge did not open 7443/tcp: $(cat "$WORK/converge3.log")"
grep -q '^7443/tcp .*ALLOW' "$UFW_STATE" || fail "ufw was not told to allow 7443/tcp: $(cat "$UFW_STATE")"
PATH="$FAKE:$PATH" $SV system converge >"$WORK/converge3b.log" 2>&1 || fail "the second converge with ufw failed"
grep -q "allowed 7443" "$WORK/converge3b.log" && fail "the second converge opened 7443/tcp again"
[[ $(grep -c '^7443/tcp' "$UFW_STATE") == 1 ]] || fail "the rule was added twice: $(cat "$UFW_STATE")"

# ---- 6b. the new driver: a converge that fails after the swap ------------------------------
log "the new driver: a converge that fails after the swap fails the upgrade and rolls it back"
KEYS=$WORK/keys; mkdir -p "$KEYS"
openssl genpkey -algorithm ed25519 -out "$KEYS/sign.pem" 2>/dev/null
openssl pkey -in "$KEYS/sign.pem" -pubout -out "$KEYS/pub.pem"
ARCH=$(dpkg --print-architecture)
SRV_PORT=38804
REL=$WORK/srv/download/v0.2.1
mkdir -p "$REL" "$WORK/srv/repos/o/r/releases/tags"
cp "$NEXT" "$REL/supavise-linux-$ARCH"
if [[ $ARCH == amd64 ]]; then echo other-arch >"$REL/supavise-linux-arm64"; else echo other-arch >"$REL/supavise-linux-amd64"; fi
echo "not a real studio build" >"$REL/supavise-studio-test-p1-linux-$ARCH.tar.zst"
env SUPAVISE_RELEASE_TAG=v0.2.1 SUPAVISE_RELEASETOOL="${SUPAVISE_RELEASETOOL:-}" deploy/release-assets.sh "$REL" "$KEYS/sign.pem" "$KEYS/pub.pem" >/dev/null
python3 - v0.2.1 "$REL" "$SRV_PORT" "$WORK/srv/repos/o/r/releases" <<'PY'
import json, os, sys
tag, d, port, out = sys.argv[1:]
rel = {"tag_name": tag, "assets": [{"name": n, "browser_download_url": f"http://127.0.0.1:{port}/download/{tag}/{n}"} for n in sorted(os.listdir(d))]}
for name in ("latest", f"tags/{tag}"):
    with open(os.path.join(out, name), "w") as f:
        json.dump(rel, f)
PY
(cd "$WORK/srv" && exec python3 -m http.server "$SRV_PORT" --bind 127.0.0.1 >"$WORK/http.log" 2>&1) &
SRV_PID=$!
for ((i = 0; i < 20; i++)); do [[ $(http_code "http://127.0.0.1:$SRV_PORT/download/v0.2.1/SHA256SUMS") == 200 ]] && break; sleep 0.5; done
[[ $(http_code "http://127.0.0.1:$SRV_PORT/download/v0.2.1/SHA256SUMS") == 200 ]] || fail "the local release server does not answer"

# upgrade_next [DIR]: the driver is a copy of the target release, as in upgrade-e2e.sh; a DIR is put
# first on the PATH of the driver, which its `system converge` inherits.
upgrade_next() {
  PATH="${1:+$1:}$PATH" timeout 1800 /opt/supavise-e2e/next upgrade --repo o/r --api-base "http://127.0.0.1:$SRV_PORT" \
    --public-key-file "$KEYS/pub.pem" --version v0.2.1 --yes
}
projects_pids() { for u in "${UNITS[@]:1}"; do systemctl show -p MainPID --value "$u"; done | paste -sd, -; }
upgrade_phase() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["phase"])' "$STATE/system/upgrade.json"; }

BROKEN=$WORK/brokenbin; mkdir -p "$BROKEN"
cat >"$BROKEN/ufw" <<'UFW'
#!/usr/bin/env bash
case "$1" in
  status) printf 'Status: active\n\nTo                         Action      From\n--                         ------      ----\n22/tcp                     ALLOW       Anywhere\n' ;;
  *) echo "ERROR: ufw is broken" >&2; exit 1 ;;
esac
UFW
chmod 0755 "$BROKEN/ufw"

rc=0
out=$(upgrade_next "$BROKEN" 2>&1) || rc=$?
printf '%s\n' "$out" >"$WORK/upgrade-broken-converge.log"
[[ $rc -eq 3 ]] || { printf '%s\n' "$out" >&2; fail "an upgrade whose converge failed exited with status $rc, want 3"; }
[[ $out == *"system converge"* && $out == *"rolled back"* ]] || { printf '%s\n' "$out" >&2; fail "the failed upgrade does not name the converge or the rollback"; }
[[ $($SV --version) == *v0.2.0* ]] || fail "after the rollback the installed binary is $($SV --version), want v0.2.0"
wait_daemon
for u in "${UNITS[@]:1}"; do wait_active "$u" 60; done
[[ $(upgrade_phase) == rolled_back ]] || fail "the upgrade marker says $(upgrade_phase), want rolled_back"
grep -qx "revision=$REV" "$STATE/converged" || fail "the failed converge changed the marker: $(cat "$STATE/converged")"
[[ $(projects_pids) == "${PIDS0#*,}" ]] || fail "the failed upgrade restarted a project's process"
log "exit status 3, back on v0.2.0, the marker and the projects as they were"

log "the same upgrade with a working ufw: exit 0, converged, no project restarted (I4)"
rc=0
out=$(upgrade_next 2>&1) || rc=$?
printf '%s\n' "$out" >"$WORK/upgrade-converge.log"
[[ $rc -eq 0 ]] || { printf '%s\n' "$out" >&2; fail "the upgrade exited with status $rc"; }
[[ $($SV --version) == *v0.2.1* ]] || fail "after the upgrade the installed binary is $($SV --version), want v0.2.1"
[[ $(upgrade_phase) == "done" ]] || fail "the upgrade marker says $(upgrade_phase), want done"
wait_daemon
for u in "${UNITS[@]:1}"; do wait_active "$u" 60; done
grep -qx "revision=$REV" "$STATE/converged" || fail "the marker after the upgrade is $(cat "$STATE/converged")"
[[ $(pending_ids) == ",," ]] || fail "steps are pending after the upgrade: $($SV system converge --check --json)"
[[ $(projects_pids) == "${PIDS0#*,}" ]] || fail "the upgrade restarted a project's process (I4)"
[[ $(papi GET "/v1/projects/$REF" | json_get 'd["status"]') == ACTIVE_HEALTHY ]] || fail "the project is not ACTIVE_HEALTHY after the upgrade"

# ---- 7. install.sh and --aws-first-boot -----------------------------------------------------
log "install.sh --join-token-file refuses a token file that cannot be used, before it changes anything"
BIN_SUM=$(sha256sum "$SV" | cut -d' ' -f1)
out=$(deploy/install.sh --binary /opt/supavise-e2e/prev --join-token-file "$WORK/no-such-token" 2>&1) && fail "install.sh accepted a missing token file"
[[ $out == *"no such file"* ]] || fail "missing token file: $out"
: >"$WORK/empty-token"; chmod 600 "$WORK/empty-token"
out=$(deploy/install.sh --binary /opt/supavise-e2e/prev --join-token-file "$WORK/empty-token" 2>&1) && fail "install.sh accepted an empty token file"
[[ $out == *"is empty"* ]] || fail "empty token file: $out"
echo svj1.example >"$WORK/open-token"; chmod 644 "$WORK/open-token"
out=$(deploy/install.sh --binary /opt/supavise-e2e/prev --join-token-file "$WORK/open-token" 2>&1) && fail "install.sh accepted a token file that others can read"
[[ $out == *"chmod 600"* ]] || fail "open token file: $out"
out=$(deploy/install.sh --binary /opt/supavise-e2e/prev --join-token-file= 2>&1) && fail "install.sh accepted an empty --join-token-file="
[[ $out == *"needs a path"* ]] || fail "empty --join-token-file=: $out"
[[ $(sha256sum "$SV" | cut -d' ' -f1) == "$BIN_SUM" ]] || fail "install.sh replaced the binary although it refused the token file"
out=$($SV install --join-token-file "" 2>&1) && fail "install accepted an empty --join-token-file"
[[ $out == *"needs a path"* ]] || fail "install --join-token-file \"\": $out"
out=$($SV install --data-device /dev/nvme1n1 2>&1) && fail "install accepted --data-device without --aws-first-boot"
[[ $out == *"belongs to --aws-first-boot"* ]] || fail "install --data-device without --aws-first-boot: $out"
install_help=$($SV install --help)
for flag in aws-first-boot data-device join-token-file; do
  [[ $install_help == *"--$flag"* ]] || fail "install --help does not list --$flag"
done

log "supavise install --aws-first-boot formats nothing off EC2"
FSTAB_SUM=$(sha256sum /etc/fstab | cut -d' ' -f1)
out=$($SV install --aws-first-boot --skip-os-check 2>&1) && fail "install --aws-first-boot succeeded off EC2"
[[ $out == *"EC2 instance"* ]] || fail "install --aws-first-boot off EC2: $out"
[[ $(sha256sum /etc/fstab | cut -d' ' -f1) == "$FSTAB_SUM" ]] || fail "fstab changed"
[[ $(systemctl is-active supavise.service) == active ]] || fail "the refused first boot touched the daemon"

# ---- 8. the mounts --------------------------------------------------------------------------
log "mount protection: /var/lib/supavise and /etc/supavise are mount points"
if mount --bind "$STATE" "$STATE" && BOUND=1 && mount --bind /etc/supavise /etc/supavise; then
  is_pending mounts || fail "mounts are not pending with the directories mounted"
  $SV system converge 2>&1 | tee "$WORK/converge4.log"
  [[ ${PIPESTATUS[0]} -eq 0 ]] || fail "converge with mounts failed"
  for u in supavise.service supavise-postgres@.service supavise-gotrue@.service supavise-postgrest@.service supavise-basebackup@.service supavise-realtime.service supavise-storage.service; do
    [[ $(cat "/etc/systemd/system/$u.d/10-supavise-data-mount.conf") == $'[Unit]\nRequiresMountsFor=/var/lib/supavise' ]] || fail "$u has no data-mount drop-in"
    [[ $(cat "/etc/systemd/system/$u.d/20-supavise-etc-mount.conf") == $'[Unit]\nRequiresMountsFor=/etc/supavise' ]] || fail "$u has no config-mount drop-in"
  done
  [[ ! -e /etc/systemd/system/supavise-upgrade.service.d/10-supavise-data-mount.conf ]] || fail "the upgrade service waits for the data volume"
  requires=$(systemctl show -p RequiresMountsFor --value supavise-gotrue@system.service)
  [[ $requires == *"/var/lib/supavise"* && $requires == *"/etc/supavise"* ]] || fail "systemd does not read the drop-ins: RequiresMountsFor=$requires"
  $SV system converge >"$WORK/converge4b.log" 2>&1 || fail "the second converge with mounts failed"
  grep -q -e wrote -e installed "$WORK/converge4b.log" && fail "the second converge rewrote the drop-ins"
  [[ $(for u in "${UNITS[@]:1}"; do systemctl show -p MainPID --value "$u"; done | paste -sd, -) == "${PIDS0#*,}" ]] || fail "writing the drop-ins restarted a unit"
else
  log "bind mounts are not possible here; the mount checks are skipped"
fi

log "converge-e2e passed"
