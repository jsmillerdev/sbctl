#!/usr/bin/env bash
# Starts a packaged supavise Studio build on a loopback port and checks that it is a platform-mode
# build whose placeholders are replaced, twice with different values, that the MCP route of patch
# 0004 answers (with and without the node's MCP settings), and that missing required values stop it
# with exit code 78.
#
#   studio/verify.sh <supavise-studio-*.tar.zst | extracted directory>
#
# A directory is copied first (the check rewrites files). Needs curl, tar and zstd (for archives).
# Used by build.sh before it writes the artifact, and by studio/spike.sh.
set -euo pipefail

SRC="${1:-}"
[[ -n "$SRC" ]] || { echo "usage: verify.sh <archive|dir>" >&2; exit 2; }

TMP="$(mktemp -d "${STUDIO_VERIFY_TMP:-${TMPDIR:-/tmp}}/supavise-studio-verify.XXXXXX")"
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
for f in bin/studio node/bin/node app/apps/studio/server.js app/apps/studio/docker-entrypoint.mjs share/supavise/runtime-config.json; do
  [[ -e "$ROOT/$f" ]] || fail "artifact is missing $f"
done
NODE="$ROOT/node/bin/node"

free_port() { "$NODE" -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'; }

# placeholders still present in any listed file (the .supavise-tpl copies are expected to keep them)
leftover_placeholders() {
  "$NODE" -e '
    const fs = require("fs"), path = require("path");
    const cfg = JSON.parse(fs.readFileSync(process.argv[1] + "/share/supavise/runtime-config.json", "utf8"));
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
    const cfg = JSON.parse(fs.readFileSync(process.argv[1] + "/share/supavise/runtime-config.json", "utf8"));
    let n = 0;
    for (const f of cfg.files) if (fs.readFileSync(path.join(process.argv[1], "app", f), "utf8").includes(process.argv[2])) n++;
    console.log(n);
  ' "$ROOT" "$1"
}

# build.sh exports the build-time placeholders as NEXT_PUBLIC_* for next build. Studio must
# start from a clean slate here, or it would read a placeholder as a configured value.
while IFS= read -r v; do unset "$v"; done < <(compgen -e | grep -E '^(NEXT_PUBLIC_|CSP_EXTRA_PROJECT_HOSTS$|SUPAVISE_MANAGEMENT_API_URL$|SUPAVISE_PROJECT_URL_TEMPLATE$)' || true)

# start api_url gotrue_url site_url hosts [mcp_url management_api_url project_url_template]
# An empty or missing MCP value is left unset, as an older unit leaves it.
start() {
  local port; port="$(free_port)"; PORT_NOW="$port"
  : > "$TMP/studio.log"
  local extra=()
  [[ -z "${5:-}" ]] || extra+=("NEXT_PUBLIC_MCP_URL=$5")
  [[ -z "${6:-}" ]] || extra+=("SUPAVISE_MANAGEMENT_API_URL=$6")
  [[ -z "${7:-}" ]] || extra+=("SUPAVISE_PROJECT_URL_TEMPLATE=$7")
  env PORT="$port" HOSTNAME=127.0.0.1 \
    NEXT_PUBLIC_API_URL="$1" NEXT_PUBLIC_GOTRUE_URL="$2" NEXT_PUBLIC_SITE_URL="$3" \
    CSP_EXTRA_PROJECT_HOSTS="$4" ${extra[@]+"${extra[@]}"} \
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

# mcp_call query authorization json-body: POST /api/mcp, sets MCP_CODE and MCP_BODY. An empty
# authorization sends none. The token is made up; the route only passes it on.
mcp_call() {
  local args=(-sS -o "$TMP/mcp.body" -w '%{http_code}' --max-time 30 -X "${MCP_METHOD:-POST}"
    -H 'content-type: application/json' -H 'accept: application/json, text/event-stream')
  [[ -z "$2" ]] || args+=(-H "authorization: $2")
  MCP_CODE="$(curl "${args[@]}" -d "$3" "http://127.0.0.1:$PORT_NOW/api/mcp$1")"
  MCP_BODY="$(cat "$TMP/mcp.body")"
}

# The MCP route of patch 0004 (platform mode). It is on the allowlist of Studio's own API routes,
# answers without a token, and builds the server per request. tools/list and get_project_url make
# no call to the Management API, so the URL it is given here is never contacted.
check_mcp_route() { # project-url-host-suffix, or "unconfigured"
  local ref=aaaaaaaaaaaaaaaaaaaa auth='Bearer sbp_verify_not_a_real_token'
  local list='{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
  mcp_call "" "" "$list"
  [[ "$MCP_CODE" == 401 && "$MCP_BODY" == *"No access token provided"* ]] || fail "POST /api/mcp without a token returned $MCP_CODE $MCP_BODY, want 401 (404 means patch 0004 is not in this build)"
  MCP_METHOD=GET mcp_call "" "$auth" ""
  [[ "$MCP_CODE" == 405 ]] || fail "GET /api/mcp returned $MCP_CODE, want 405"
  if [[ "$1" == unconfigured ]]; then
    mcp_call "" "$auth" "$list"
    [[ "$MCP_CODE" == 500 && "$MCP_BODY" == *"MCP is not configured"* ]] || fail "/api/mcp without the node's MCP settings returned $MCP_CODE $MCP_BODY, want 500"
    ok "MCP route: 401 without a token, 405 for GET, 500 when the node did not configure it"
    return
  fi
  mcp_call "?read_only=maybe" "$auth" "$list"
  [[ "$MCP_CODE" == 400 ]] || fail "/api/mcp?read_only=maybe returned $MCP_CODE, want 400"
  mcp_call "?project_ref=$ref&read_only=true" "$auth" "$list"
  [[ "$MCP_CODE" == 200 && "$MCP_BODY" == *'"execute_sql"'* ]] || fail "tools/list returned $MCP_CODE $MCP_BODY"
  [[ "$MCP_BODY" != *'"list_organizations"'* ]] || fail "a project-scoped server lists the account tools"
  [[ "$MCP_BODY" != *'"apply_migration"'* ]] || fail "a read-only server lists a write tool"
  mcp_call "?project_ref=$ref" "$auth" '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_project_url","arguments":{}}}'
  [[ "$MCP_CODE" == 200 && "$MCP_BODY" == *"https://$ref.$1"* ]] || fail "get_project_url returned $MCP_CODE $MCP_BODY, want https://$ref.$1"
  ok "MCP route: 401, 405, 400 for a bad read_only, project-scoped read-only tool list, project URL from the template"
}

check_round() { # api origin, hosts-regex-literal, mcp url, project-url-host-suffix or "unconfigured"
  local api="$1" host_a="$2" mcp="$3" port="$PORT_NOW"
  local headers; headers="$(curl -sS -D - -o /dev/null --max-time 10 "http://127.0.0.1:$port/sign-in")"
  echo "$headers" | grep -qi '^content-security-policy:' || fail "no CSP header (not a platform build?)"
  echo "$headers" | grep -i '^content-security-policy:' | grep -qF "$api" || fail "CSP does not allow $api"
  echo "$headers" | grep -i '^content-security-policy:' | grep -qF "wss://$host_a" || fail "CSP does not allow wss://$host_a"
  if echo "$headers" | grep -qi 'supavise-placeholder'; then fail "CSP still has a placeholder"; fi
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

  # /api/incident-banner answers 500 here (no incident.io key); supavise's proxy answers it
  # itself on studio.<domain> (internal/proxy), so the artifact carries no fixup for it.

  leftover_placeholders || fail "placeholders left in rewritten files"
  [[ "$(files_containing "$api/platform")" -gt 0 ]] || fail "$api/platform is not in any rewritten file"
  ok "rewritten files carry $api/platform, no placeholder left"
  [[ "$(files_containing "$mcp")" -gt 0 ]] || fail "$mcp (NEXT_PUBLIC_MCP_URL) is not in any rewritten file"
  ok "rewritten files carry the MCP URL $mcp"
  check_mcp_route "$4"
  if command -v pgrep >/dev/null 2>&1; then
    local child; child="$(pgrep -P "$PID" | head -n 1)"
    [[ -z "$child" ]] || printf 'verify: studio server RSS: %s MB\n' "$(ps -o rss= -p "$child" | awk '{printf "%d", $1/1024}')" >&2
  fi
}

# round 1: every setting, including the node's MCP settings (127.0.0.1:9 is never contacted)
start "https://api.one.test/platform" "https://auth.one.test/auth/v1" "https://studio.one.test" "*.api.one.test, db.one.test:8443" \
  "https://api.one.test/mcp" "http://127.0.0.1:9" "https://{ref}.api.one.test"
check_round "https://api.one.test" "*.api.one.test" "https://api.one.test/mcp" "api.one.test"
stop
# round 2: other values over the same tree, and no MCP settings, as an older unit starts it: the
# build's default MCP URL, and a route that says it is not configured
start "https://api.two.test" "https://auth.two.test/auth/v1" "https://studio.two.test" "*.api.two.test"
check_round "https://api.two.test" "*.api.two.test" "http://localhost:8080/mcp" "unconfigured"
if [[ "$(files_containing "api.one.test")" -ne 0 ]]; then fail "first round's values are still in the tree"; fi
stop

# required values missing: exit 78 (EX_CONFIG)
rc=0
env -u NEXT_PUBLIC_API_URL -u NEXT_PUBLIC_GOTRUE_URL -u NEXT_PUBLIC_SITE_URL PORT=1 HOSTNAME=127.0.0.1 "$ROOT/bin/studio" > "$TMP/missing.log" 2>&1 || rc=$?
[[ $rc -eq 78 ]] || { cat "$TMP/missing.log" >&2; fail "missing config exited with $rc, want 78"; }
grep -q 'NEXT_PUBLIC_API_URL is required' "$TMP/missing.log" || fail "missing-config message not shown"
ok "missing required values exit 78 with a message"

echo "verify: all checks passed" >&2
