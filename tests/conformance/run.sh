#!/usr/bin/env bash
# Conformance suite: official Supabase clients against a node, unchanged.
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 SUPABASE_CLI=/path/to/supabase \
#     tests/conformance/run.sh
#
# 1. installs a node the way a customer does (deploy/install.sh), claims it, makes a personal
#    access token and creates two projects through the Management API;
# 2. turns auto-confirm on for project A through the Management API (what a customer does in
#    the dashboard before testing sign-ups);
# 3. runs the supabase-js suites (js/*.test.mjs) against project A: auth, PostgREST, Realtime,
#    Storage and Functions, each with the opaque keys and the legacy JWTs, then pg_cron and branches;
# 4. runs the Supabase CLI (cli.sh) against project B.
#
# Every suite runs even when an earlier one failed; the exit status is non-zero when any did.
# Needs root, systemd, cgroup v2, node 22+ with `npm ci` done in tests/conformance/js, the
# Supabase CLI binary in SUPABASE_CLI, and network access (artifact and npm downloads).
# Do not run it on a machine you care about: it creates the supavise user, writes /etc/supavise and
# /etc/hosts, installs units and starts real clusters.
#
# Names: the node's domain is conformance.test. /etc/hosts maps api., pooler., studio. and each
# project's host to 127.0.0.1, so nothing needs DNS and, on purpose, db.<ref>.api.conformance.test
# does not resolve: the CLI then reaches the database through the pooler, as it does on any
# network without a direct route. The proxy listens on port 80 (tls.mode off).
export SUPAVISE_DOMAIN=conformance.test
# shellcheck source=../linux/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../linux/lib.sh"

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
JS_DIR=$HERE/js
WORK=$(mktemp -d)
DOMAIN=$SUPAVISE_DOMAIN
ADMIN_EMAIL=conformance@example.com
ADMIN_PASSWORD=conformance-correct-horse-battery
RESULTS=()
FAILED=0

# keep_work copies what helps to read a failure (the CLI's step logs and the profile) into the
# uploaded log directory. conformance.json and claim.json hold the PAT and the database
# passwords and stay out; a redaction pass over the copy drops any key or password that a
# CLI log echoed.
keep_work() {
  mkdir -p "$LOG_DIR/work"
  cp -R "$WORK"/cli-out "$WORK"/profile.yaml "$LOG_DIR/work/" 2>/dev/null || return 0
  local f
  while IFS= read -r -d '' f; do
    sed -E -i 's/(sbp_|sb_secret_|sb_publishable_|eyJ)[A-Za-z0-9._-]+/[redacted]/g' "$f" || true
    for secret in "${DBPASS_A:-}" "${DBPASS_B:-}" "${PAT:-}" "$ADMIN_PASSWORD"; do
      [[ -n $secret ]] || continue
      SECRET=$secret python3 - "$f" <<'PY' || true
import os, sys
p = sys.argv[1]
t = open(p, errors="replace").read()
open(p, "w").write(t.replace(os.environ["SECRET"], "[redacted]"))
PY
    done
  done < <(find "$LOG_DIR/work" -type f -print0)
}
trap 'rc=$?; collect_logs; keep_work; rm -rf "$WORK"; exit $rc' EXIT

need_root
preflight
# shellcheck source=pins.env
source "$HERE/pins.env"
command -v node >/dev/null || fail "missing node"
[[ $(node -p 'process.versions.node.split(".")[0]') -ge 22 ]] || fail "node 22 or newer is required, found $(node --version)"
[[ -x ${SUPABASE_CLI:-} ]] || fail "SUPABASE_CLI must be the supabase CLI binary"
[[ $("$SUPABASE_CLI" --version) == *"$SUPABASE_CLI_VERSION"* ]] || fail "SUPABASE_CLI is $("$SUPABASE_CLI" --version), the pin is $SUPABASE_CLI_VERSION"
[[ -d $JS_DIR/node_modules/@supabase/supabase-js ]] || fail "run npm ci in $JS_DIR first"
cd "$REPO_ROOT" || exit 1

if [[ -z $SUPAVISE_BIN ]]; then
  command -v go >/dev/null || fail "no SUPAVISE_BIN and no go toolchain"
  SUPAVISE_BIN=$WORK/supavise
  CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.1" -o "$SUPAVISE_BIN" ./cmd/supavise
fi

# ---- 1. the node ------------------------------------------------------------------------
# A runner image may ship services on our ports.
systemctl stop apache2 nginx postgresql mysql 2>/dev/null || true
for p in 80 443 5432 6543 5433 9999 7000 3000 8080 4000 5000 9000; do
  if ss -ltnH "sport = :$p" | grep -q .; then ss -ltnp "sport = :$p" >&2; fail "port $p is already in use on this VM"; fi
done
printf '127.0.0.1 api.%s studio.%s pooler.%s\n' "$DOMAIN" "$DOMAIN" "$DOMAIN" >>/etc/hosts

log "install.sh (a fresh node, Edge Functions on)"
deploy/install.sh --binary "$SUPAVISE_BIN" --domain "$DOMAIN" --public-ip 127.0.0.1 --tls off --email ci@example.com \
  --firewall none --no-studio --set functions.enabled=true --claim-token-file "$WORK/claim-token" 2>&1 | tee "$WORK/install.log"
grep -q "Supavise is running" "$WORK/install.log" || fail "install.sh did not report success"
for u in supavise.service supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service supavise-supavisor.service supavise-realtime.service supavise-storage.service supavise-edge-runtime.service; do
  wait_active "$u" 120
done

log "claim, sign in, personal access token"
TOKEN=$(tr -d '[:space:]' <"$WORK/claim-token")
[[ $(api POST /claim -H 'Content-Type: application/json' -o "$WORK/claim.json" -w '%{http_code}' \
  -d "{\"token\":\"$TOKEN\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\",\"organization_name\":\"Conformance\"}") == 201 ]] || { cat "$WORK/claim.json" >&2; fail "claim"; }
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | json_get 'd["access_token"]') || fail "dashboard sign-in"
PAT=$(api POST /platform/profile/access-tokens -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' -d '{"name":"conformance"}' | json_get 'd["token"]') \
  || fail "creating a personal access token"
[[ $PAT == sbp_* ]] || fail "personal access token: $PAT"
ORG=$(papi GET /v1/organizations | json_get 'd[0]["slug"]') || fail "listing organizations with the token"

log "two projects through the Management API"
gen_dbpass; DBPASS_A=$DBPASS
REF_A=$(api_create_project conformance-a)
gen_dbpass; DBPASS_B=$DBPASS
REF_B=$(api_create_project conformance-b)
printf '127.0.0.1 %s.api.%s %s.api.%s\n' "$REF_A" "$DOMAIN" "$REF_B" "$DOMAIN" >>/etc/hosts
log "projects $REF_A and $REF_B"

# ---- 2. auto-confirm on project A ---------------------------------------------------------
project_keys "$REF_A"
[[ $(papi PATCH "/v1/projects/$REF_A/config/auth" -H 'Content-Type: application/json' -d '{"mailer_autoconfirm":true}' -o /dev/null -w '%{http_code}') == 200 ]] \
  || fail "PATCH config/auth with mailer_autoconfirm"
confirmed=""
for ((i = 0; i < 90; i++)); do
  confirmed=$(curl -sS -m 10 -H "apikey: $PUB" "http://$REF_A.api.$DOMAIN/auth/v1/settings" 2>/dev/null | json_get 'd.get("mailer_autoconfirm")' 2>/dev/null || true)
  [[ $confirmed == True ]] && break
  sleep 1
done
[[ $confirmed == True ]] || fail "GoTrue of project A does not report mailer_autoconfirm after the Management API saved it"

# ---- 3. supabase-js ------------------------------------------------------------------------
python3 - "$WORK/conformance.json" <<PY
import json, sys
json.dump({
  "apiUrl": "http://api.$DOMAIN",
  "pat": "$PAT",
  "projects": {
    "a": {"ref": "$REF_A", "url": "http://$REF_A.api.$DOMAIN", "dbPassword": "$DBPASS_A"},
    "b": {"ref": "$REF_B", "url": "http://$REF_B.api.$DOMAIN", "dbPassword": "$DBPASS_B"},
  },
}, open(sys.argv[1], "w"))
PY
chmod 600 "$WORK/conformance.json"

record() { # NAME STATUS
  RESULTS+=("$1:$2")
  [[ $2 == pass ]] || FAILED=1
}
mkdir -p "$LOG_DIR"
for f in auth postgrest realtime storage functions cron branches; do
  log "supabase-js suite: $f"
  if (cd "$JS_DIR" && CONFORMANCE_CONFIG=$WORK/conformance.json node --test --test-force-exit --test-timeout=300000 \
      --test-reporter=spec --test-reporter-destination=stdout \
      --test-reporter=junit --test-reporter-destination="$LOG_DIR/junit-$f.xml" "$f.test.mjs") 2>&1 | tee "$LOG_DIR/suite-$f.log"; then
    record "supabase-js $f" pass
  else
    record "supabase-js $f" FAIL
  fi
done

# ---- 4. the Supabase CLI --------------------------------------------------------------------
log "Supabase CLI"
supavise api profile --format yaml >"$WORK/profile.yaml" || fail "supavise api profile"
cat "$WORK/profile.yaml" >&2
if SUPABASE_CLI=$SUPABASE_CLI SUPABASE_CLI_VERSION=$SUPABASE_CLI_VERSION PROFILE=$WORK/profile.yaml API_URL=http://api.$DOMAIN PAT=$PAT \
    REF=$REF_B DBPASS=$DBPASS_B PROJECT_URL=http://$REF_B.api.$DOMAIN WORK=$WORK "$HERE/cli.sh" 2>&1 | tee "$LOG_DIR/suite-cli.log"; then
  record "supabase CLI" pass
else
  record "supabase CLI" FAIL
fi

# ---- summary --------------------------------------------------------------------------------
{
  echo "## Conformance"
  echo
  echo "| Suite | Result |"
  echo "|---|---|"
  for r in "${RESULTS[@]}"; do echo "| ${r%%:*} | ${r##*:} |"; done
} | tee "$LOG_DIR/summary.md" >&2
[[ $FAILED -eq 0 ]] || fail "at least one suite failed"
log "all suites passed"
