#!/usr/bin/env bash
# OAuth sign-in for MCP clients under real systemd units: the authorization server of the Management
# API (registration, authorize, consent, token, revoke, the organization's OAuth Apps), the OAuth
# principal on /v1, and the gate in front of api.<domain>/mcp.
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 [REQUIRE_CLIENTS=1] \
#     tests/linux/oauth-smoke.sh [--teardown]
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 \
#     tests/linux/oauth-smoke.sh --studio-artifact /path/to/supavise-studio-<tag>-p<N>-linux-amd64.tar.zst
#
# The first form is the `oauth-smoke` job (no Studio): the node, two organizations with a project each,
# an Owner, an Administrator and a Developer, and tests/linux/oauth/oauth-walk.mjs, which runs steps
# L-01 to L-14 (the contract is docs/design.md, section 13). The real stdio MCP server and its smoke scripts then run with an OAuth
# access token (REQUIRE_CLIENTS=1 makes a missing node or npx a failure instead of a skip). Afterwards
# the script reads the registry and the journal: no plaintext secret in any oauth table or log line, no
# oauth token in access_tokens, the audit events of each state change, and none of them carrying a
# secret.
#
# With --studio-artifact (the `mcp-e2e` job, amd64) Studio runs from the given build and the walk adds
# L-09: MCP over Streamable HTTP at /mcp with two protocol revisions, project-scoped and account-scoped
# URLs, read_only, features, a personal access token as bearer, get_project_url and create_project.
# The artifact is served to the node from a local HTTP server and checked against a SHA256SUMS file
# next to it when there is one.
#
# Not run in development (root, systemd and Linux required); CI runs it on an ephemeral Ubuntu 24.04
# VM. tests/linux/oauth/selftest.mjs runs the walk against a stand-in for a development machine.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
STUDIO_ARTIFACT=${STUDIO_ARTIFACT:-}
while (($#)); do
  case $1 in
    --teardown) TEARDOWN=1 ;;
    --studio-artifact) STUDIO_ARTIFACT=${2:?--studio-artifact needs a path}; shift ;;
    *) fail "unknown argument $1" ;;
  esac
  shift
done
MCP=0
[[ -n $STUDIO_ARTIFACT ]] && MCP=1

WORK=$(mktemp -d)
HTTP_PID=""
trap 'rc=$?; [[ -n $HTTP_PID ]] && kill "$HTTP_PID" 2>/dev/null; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; rm -rf "$WORK"; exit $rc' EXIT

# Ports away from anything the runner may listen on, as in roles-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000
ADMIN=http://127.0.0.1:7000
ORG=default       # the claim keeps the organization the first project created
ORG2=second       # inserted into the registry below, before `projects create --org`; the Owner joins it after the claim
OWNER_EMAIL=owner@example.com
# A password for this run only; it reaches the walk through a 0600 file, never a command line.
PASSWORD=oauth-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')

command -v node >/dev/null || fail "node is required: the walk (tests/linux/oauth/oauth-walk.mjs) runs on it"
if [[ $MCP -eq 1 ]]; then
  [[ -s $STUDIO_ARTIFACT ]] || fail "the Studio artifact '$STUDIO_ARTIFACT' is missing or empty"
  STUDIO_SHA=$(sha256sum "$STUDIO_ARTIFACT" | cut -d' ' -f1)
  SUMS=$(dirname "$STUDIO_ARTIFACT")/SHA256SUMS
  if [[ -f $SUMS ]]; then
    grep -q "^$STUDIO_SHA " "$SUMS" || fail "SHA256SUMS next to the artifact does not list $STUDIO_SHA"
  fi
  mkdir -p "$WORK/studio"
  cp "$STUDIO_ARTIFACT" "$WORK/studio/"
  STUDIO_HTTP_PORT=$(free_port)
  python3 -m http.server "$STUDIO_HTTP_PORT" --bind 127.0.0.1 --directory "$WORK/studio" >"$WORK/studio-http.log" 2>&1 &
  HTTP_PID=$!
fi

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

[fleet]
supavisor_api_port = $P_API
CONF
if [[ $MCP -eq 1 ]]; then
  cat >>"$SUPAVISE_CONF" <<CONF

[studio]
artifact_url = "http://127.0.0.1:$STUDIO_HTTP_PORT/$(basename "$STUDIO_ARTIFACT")"
artifact_sha256 = "$STUDIO_SHA"
CONF
fi

# Same VM-local workaround as roles-smoke.sh.
install -d /etc/systemd/system/supavise-postgres@.service.d
cat >/etc/systemd/system/supavise-postgres@.service.d/10-oauth-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/supavise/artifacts
CONF
systemctl daemon-reload

log "system init, fleet, a project in each of two organizations"
system_init
wait_active supavise-postgres@system.service 30
supavise fleet start || fail "fleet start"
REF=$(create_project oauth micro)
[[ $REF =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF'"
supavise fleet ensure-tenant "$REF" || fail "ensure-tenant $REF"
PSQL=$(ls -d "$SUPAVISE_STATE"/artifacts/postgres/*/bin/psql | head -1)
reg() { sudo -u "$SUPAVISE_USER" "$PSQL" "host=$SUPAVISE_STATE/projects/system/postgres/sock port=5433 user=supabase_admin dbname=supavise" -Atc "$1" </dev/null; }
# Only the "default" organization is made on first use; any other slug must exist before `projects create --org`.
reg "insert into supavise.organizations (slug, name) values ('$ORG2', 'Second')" >/dev/null || fail "making organization $ORG2"
REF2=$(supavise projects create --name oauth-second --class micro --org "$ORG2" --json | json_get 'd["ref"]')
[[ $REF2 =~ ^[a-z]{20}$ && $REF2 != "$REF" ]] || fail "bad ref '$REF2'"
supavise fleet ensure-tenant "$REF2" || fail "ensure-tenant $REF2"

log "daemon: supavise.service"
systemctl start supavise.service
for ((i = 0; i < 60; i++)); do
  [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && break
  sleep 1
done
[[ $(http_code "$ADMIN/v1/projects") == 401 ]] || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "the Management API does not answer on the admin listener"; }

if [[ $MCP -eq 1 ]]; then
  log "Studio from the build under test"
  wait_active supavise-studio.service 120
  for ((i = 0; i < 120; i++)); do
    [[ $(http_code -H "Host: studio.$SUPAVISE_DOMAIN" http://127.0.0.1/api/get-utc-time) == 200 ]] && break
    sleep 1
  done
  [[ $(http_code -H "Host: studio.$SUPAVISE_DOMAIN" http://127.0.0.1/api/get-utc-time) == 200 ]] || { journalctl --no-pager -u supavise-studio.service | tail -40 >&2; fail "Studio does not answer through studio.$SUPAVISE_DOMAIN"; }
fi

log "the first user is the Owner of both organizations, with a personal access token"
# Passwords, the claim token and the session go through stdin and header files, never an argument.
bearer_api() { local m=$1 p=$2 t=$3; shift 3; api "$m" "$p" -H @<(printf 'Authorization: Bearer %s\n' "$t") "$@"; }
CLAIM=$(supavise claim token | tr -d '[:space:]')
[[ $CLAIM =~ ^sbc_[0-9a-f]{48}$ ]] || fail "claim token shape"
[[ $(api POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' --data-binary @- \
  <<<"{\"token\":\"$CLAIM\",\"email\":\"$OWNER_EMAIL\",\"password\":\"$PASSWORD\",\"organization_name\":\"Default\"}") == 201 ]] || fail "claim"
unset CLAIM
supavise users role "$OWNER_EMAIL" owner --org "$ORG2" >/dev/null || fail "making the Owner of $ORG2"
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' --data-binary @- \
  <<<"{\"email\":\"$OWNER_EMAIL\",\"password\":\"$PASSWORD\"}" | json_get 'd["access_token"]') || fail "owner sign-in"
OWNER_PAT=$(bearer_api POST /platform/profile/access-tokens "$JWT" -H 'Content-Type: application/json' -d '{"name":"oauth-smoke"}' | json_get 'd["token"]') || fail "owner PAT"
[[ $OWNER_PAT =~ ^sbp_[0-9a-f]{40}$ ]] || fail "owner PAT shape"
unset JWT

log "the walk (L-01 to L-14)"
STDIO_JSON=null
if [[ $MCP -eq 0 ]]; then
  if command -v npx >/dev/null; then
    STDIO_JSON="{\"smoke\":\"$REPO_ROOT/internal/api/testdata/mcp-smoke.mjs\",\"roles\":\"$REPO_ROOT/internal/api/testdata/mcp-roles.mjs\"}"
  else
    [[ ${REQUIRE_CLIENTS:-} == 1 ]] && fail "npx is required (REQUIRE_CLIENTS=1)"
    log "npx is not installed: the stdio MCP server checks are skipped"
  fi
fi
(
  umask 077
  OW_PASSWORD=$PASSWORD OW_PAT=$OWNER_PAT OW_PATH="$WORK/walk.json" OW_STDIO=$STDIO_JSON \
  OW_DOMAIN=$SUPAVISE_DOMAIN OW_ORG=$ORG OW_ORG2=$ORG2 OW_REF=$REF OW_REF2=$REF2 OW_EMAIL=$OWNER_EMAIL OW_MCP=$MCP OW_ADMIN=$ADMIN \
  python3 - <<'PY'
import json, os
e = os.environ
cfg = {
    "domain": e["OW_DOMAIN"],
    "issuer": "http://api." + e["OW_DOMAIN"],
    "dashboard": "http://studio." + e["OW_DOMAIN"],
    "proxy": {"host": "127.0.0.1", "port": 80},
    "admin_url": e["OW_ADMIN"],
    "org": e["OW_ORG"], "org2": e["OW_ORG2"], "ref": e["OW_REF"], "ref2": e["OW_REF2"],
    "owner": {"email": e["OW_EMAIL"], "password": e["OW_PASSWORD"]},
    "owner_pat": e["OW_PAT"],
    "password": e["OW_PASSWORD"],
    "cli": ["sudo", "-u", "supavise", "-H", "/usr/local/bin/supavise"],
    "mcp": e["OW_MCP"] == "1",
    "stdio": json.loads(e["OW_STDIO"]),
}
fd = os.open(e["OW_PATH"], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(cfg, f)
PY
)
node "$REPO_ROOT/tests/linux/oauth/oauth-walk.mjs" "$WORK/walk.json" || { journalctl --no-pager -u supavise.service | tail -60 >&2; fail "the OAuth walk failed"; }

log "the daemon is up, and the registry holds no plaintext secret"
systemctl is-active --quiet supavise.service || fail "supavise.service is not active after the walk"
for t in oauth_apps oauth_app_secrets oauth_grants oauth_authorizations oauth_tokens; do
  [[ $(reg "select count(*) from supavise.$t") -gt 0 ]] || fail "supavise.$t is empty after the walk"
done
LEAK=$(for t in oauth_apps oauth_app_secrets oauth_grants oauth_authorizations oauth_tokens; do
  reg "select row_to_json(t)::text from supavise.$t t"
done | grep -E 'sbp_oauth_[0-9a-f]{20}|sbr_[0-9a-f]{20}|sbc_[0-9a-f]{20}|sba_[0-9a-f]{20}' || true)
[[ -z $LEAK ]] || fail "an oauth table holds a plaintext secret: $(printf '%s' "$LEAK" | head -c 200 | sed -E 's/(sb[a-z]_)[0-9a-zA-Z_]+/\1<cut>/g')"
[[ $(reg "select count(*) from supavise.access_tokens where token_prefix like 'sbp_oaut%'") == 0 ]] || fail "an OAuth token is stored in access_tokens"

log "audit events for each state change, none carrying a secret"
for k in authorization_approved authorization_declined grant_created grant_revoked code_reuse app_created app_updated app_deleted client_secret_created client_secret_deleted; do
  [[ $(reg "select count(*) from supavise.events where kind = 'oauth.$k'") -gt 0 ]] || fail "no oauth.$k event after the walk"
done
EVLEAK=$(reg "select kind || ' ' || payload::text from supavise.events where kind like 'oauth.%'" | grep -E 'sbp_oauth_|sbr_|sbc_|sba_[0-9a-f]{8}|"(state|code|code_challenge|code_verifier|client_secret|refresh_token|access_token)"' || true)
[[ -z $EVLEAK ]] || fail "an oauth event carries a secret or a request value: $(printf '%s' "$EVLEAK" | head -c 200 | sed -E 's/(sb[a-z]_)[0-9a-zA-Z_]+/\1<cut>/g')"

log "no token, code or request value in the journal"
journalctl --no-pager -o cat -u supavise.service >"$WORK/daemon.log"
if grep -E 'sbp_oauth_[0-9a-f]{20}|sbr_[0-9a-f]{20}|sbc_[0-9a-f]{64}|sba_[0-9a-f]{20}|[?&]code=|code_challenge|refresh_token=|code_verifier' "$WORK/daemon.log" >"$WORK/leaks.txt"; then
  sed -E 's/(sb[a-z]_)[0-9a-zA-Z_]+/\1<cut>/g' "$WORK/leaks.txt" | head -5 >&2
  fail "the daemon's journal holds a secret or an authorization request value"
fi
if [[ $MCP -eq 1 ]]; then
  systemctl is-active --quiet supavise-studio.service || fail "Studio is not active after the MCP checks"
  journalctl --no-pager -o cat -u supavise-studio.service >"$WORK/studio.log"
  if grep -E 'sbp_oauth_[0-9a-f]{20}|sbr_[0-9a-f]{20}|authorization: *bearer' -i "$WORK/studio.log" >/dev/null; then fail "Studio's journal holds a bearer token"; fi
fi

log "oauth smoke passed"
