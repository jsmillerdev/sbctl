#!/usr/bin/env bash
# Starts a packaged sbctl Studio build on a loopback port and checks that it is a platform-mode
# build whose placeholders are replaced, twice with different values, and that missing required
# values stop it with exit code 78.
#
#   studio/verify.sh <sbctl-studio-*.tar.zst | extracted directory>
#
# A directory is copied first (the check rewrites files). Needs curl, tar and zstd (for archives).
# Used by build.sh before it writes the artifact, and by studio/spike.sh.
set -euo pipefail

SRC="${1:-}"
[[ -n "$SRC" ]] || { echo "usage: verify.sh <archive|dir>" >&2; exit 2; }

TMP="$(mktemp -d "${STUDIO_VERIFY_TMP:-${TMPDIR:-/tmp}}/sbctl-studio-verify.XXXXXX")"
PID=""
cleanup() {
  if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "verify: FAIL: $*" >&2; [[ -f "$TMP/studio.log" ]] && tail -n 30 "$TMP/studio.log" >&2; exit 1; }
ok() { echo "verify: ok: $*" >&2; }

ROOT="$TMP/root"
mkdir -p "$ROOT"
if [[ -d "$SRC" ]]; then
  if [[ "$(uname -s)" == Darwin ]]; then cp -cR "$SRC/." "$ROOT/" 2>/dev/null || cp -R "$SRC/." "$ROOT/"
  else cp -R --reflink=auto "$SRC/." "$ROOT/"; fi
else
  zstd -dc "$SRC" | tar -C "$ROOT" -xf -
fi
for f in bin/studio node/bin/node app/apps/studio/server.js app/apps/studio/docker-entrypoint.mjs share/sbctl/runtime-config.json; do
  [[ -e "$ROOT/$f" ]] || fail "artifact is missing $f"
done
NODE="$ROOT/node/bin/node"

free_port() { "$NODE" -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'; }

# placeholders still present in any listed file (the .sbctl-tpl copies are expected to keep them)
leftover_placeholders() {
  "$NODE" -e '
    const fs = require("fs"), path = require("path");
    const cfg = JSON.parse(fs.readFileSync(process.argv[1] + "/share/sbctl/runtime-config.json", "utf8"));
    const needles = cfg.values.flatMap((v) => [v.placeholder, v.origin].filter(Boolean));
    let bad = 0;
    for (const f of cfg.files) {
      const text = fs.readFileSync(path.join(process.argv[1], "app", f), "utf8");
      for (const n of needles) if (text.includes(n)) { console.log(f + " still has " + n); bad++; }
    }
    process.exit(bad ? 1 : 0);
  ' "$ROOT"
}
files_containing() { # needle -> count of listed files that contain it
  "$NODE" -e '
    const fs = require("fs"), path = require("path");
    const cfg = JSON.parse(fs.readFileSync(process.argv[1] + "/share/sbctl/runtime-config.json", "utf8"));
    let n = 0;
    for (const f of cfg.files) if (fs.readFileSync(path.join(process.argv[1], "app", f), "utf8").includes(process.argv[2])) n++;
    console.log(n);
  ' "$ROOT" "$1"
}

# build.sh exports the build-time placeholders as NEXT_PUBLIC_* for next build. Studio must
# start from a clean slate here, or it would read a placeholder as a configured value.
while IFS= read -r v; do unset "$v"; done < <(compgen -e | grep -E '^(NEXT_PUBLIC_|CSP_EXTRA_PROJECT_HOSTS$)' || true)

start() { # api_url gotrue_url site_url hosts
  local port; port="$(free_port)"; PORT_NOW="$port"
  : > "$TMP/studio.log"
  env PORT="$port" HOSTNAME=127.0.0.1 \
    NEXT_PUBLIC_API_URL="$1" NEXT_PUBLIC_GOTRUE_URL="$2" NEXT_PUBLIC_SITE_URL="$3" \
    CSP_EXTRA_PROJECT_HOSTS="$4" \
    "$ROOT/bin/studio" > "$TMP/studio.log" 2>&1 &
  PID=$!
  local i
  for i in $(seq 1 180); do
    if curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:$port/sign-in" 2>/dev/null; then return 0; fi
    kill -0 "$PID" 2>/dev/null || fail "Studio exited during start"
    sleep 1
  done
  fail "Studio did not answer /sign-in within 180 s"
}
stop() {
  kill "$PID" 2>/dev/null || true
  local rc=0
  wait "$PID" 2>/dev/null || rc=$?
  PID=""
  [[ $rc -eq 143 || $rc -eq 0 ]] || fail "Studio exited with $rc on SIGTERM (want 143 or 0)"
}

check_round() { # api origin, hosts-regex-literal
  local api="$1" host_a="$2" port="$PORT_NOW"
  local headers; headers="$(curl -sS -D - -o /dev/null --max-time 10 "http://127.0.0.1:$port/sign-in")"
  echo "$headers" | grep -qi '^content-security-policy:' || fail "no CSP header (not a platform build?)"
  echo "$headers" | grep -i '^content-security-policy:' | grep -qF "$api" || fail "CSP does not allow $api"
  echo "$headers" | grep -i '^content-security-policy:' | grep -qF "wss://$host_a" || fail "CSP does not allow wss://$host_a"
  if echo "$headers" | grep -qi 'sbctl-placeholder'; then fail "CSP still has a placeholder"; fi
  # CSP source expressions with a path match that exact path only, so the API origin must appear
  # as a bare origin token (an entry like https://host/platform would block /platform/profile).
  echo "$headers" | grep -i '^content-security-policy:' | tr ' ;' '\n\n' | grep -qxF "$api" || fail "CSP has no bare $api token"
  ok "CSP carries $api and wss://$host_a"

  # IS_PLATFORM is baked in: platform mode answers 404 for Studio's own /api/platform/* routes.
  local code
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "http://127.0.0.1:$port/api/platform/profile")"
  [[ "$code" == 404 ]] || fail "/api/platform/profile returned $code, want 404 in platform mode"
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "http://127.0.0.1:$port/api/get-utc-time")"
  [[ "$code" == 200 ]] || fail "/api/get-utc-time returned $code, want 200"
  ok "platform mode: /api/platform/* is 404, allowlisted routes work"

  # /api/incident-banner answers 500 here (no incident.io key); sbctl's proxy answers it
  # itself on studio.<domain> (internal/proxy), so the artifact carries no fixup for it.

  leftover_placeholders || fail "placeholders left in rewritten files"
  [[ "$(files_containing "$api/platform")" -gt 0 ]] || fail "$api/platform is not in any rewritten file"
  ok "rewritten files carry $api/platform, no placeholder left"
  if command -v pgrep >/dev/null 2>&1; then
    local child; child="$(pgrep -P "$PID" | head -n 1)"
    [[ -z "$child" ]] || printf 'verify: studio server RSS: %s MB\n' "$(ps -o rss= -p "$child" | awk '{printf "%d", $1/1024}')" >&2
  fi
}

# round 1
start "https://api.one.test/platform" "https://auth.one.test/auth/v1" "https://studio.one.test" "*.api.one.test, db.one.test:8443"
check_round "https://api.one.test" "*.api.one.test"
stop
# round 2: other values over the same tree
start "https://api.two.test" "https://auth.two.test/auth/v1" "https://studio.two.test" "*.api.two.test"
check_round "https://api.two.test" "*.api.two.test"
if [[ "$(files_containing "api.one.test")" -ne 0 ]]; then fail "first round's values are still in the tree"; fi
stop

# required values missing: exit 78 (EX_CONFIG)
rc=0
env -u NEXT_PUBLIC_API_URL -u NEXT_PUBLIC_GOTRUE_URL -u NEXT_PUBLIC_SITE_URL PORT=1 HOSTNAME=127.0.0.1 "$ROOT/bin/studio" > "$TMP/missing.log" 2>&1 || rc=$?
[[ $rc -eq 78 ]] || { cat "$TMP/missing.log" >&2; fail "missing config exited with $rc, want 78"; }
grep -q 'NEXT_PUBLIC_API_URL is required' "$TMP/missing.log" || fail "missing-config message not shown"
ok "missing required values exit 78 with a message"

echo "verify: all checks passed" >&2
