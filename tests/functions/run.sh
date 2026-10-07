#!/usr/bin/env bash
# Edge Functions end to end, against a node that is already up: the Management API and the
# proxy reachable at $API_URL, sb-edge-runtime running, and two projects. It deploys the
# fixtures in tests/functions/fixtures, sets secrets with `supabase secrets set`, calls the
# functions with supabase-js and fetch (verify.mjs), redeploys, deletes, and checks the files on
# disk. Project A's functions are bundled on the machine that runs this script and uploaded as
# bundles; project B's are uploaded as sources with `supabase functions deploy --use-api`, which
# the node bundles itself (in the sandbox of sb-edge-bundle.service on Linux).
#
# How project A is bundled (DEPLOY_VIA):
#   cli       `supabase functions deploy`, the default flow of the CLI. It bundles in a Docker
#             container when Docker runs, so this is the Linux CI default, where the VM is
#             ephemeral. On macOS the script refuses it while Docker is up: it would start
#             containers on a machine that has other people's.
#   artifact  the edge-runtime artifact's own `edge-runtime bundle`, then curl with the exact
#             query and Content-Type the CLI uses. No Docker. The macOS default.
#
# Environment (all required unless a default is shown):
#   SBCTL_RUN       how to run sbctl, for example "/usr/local/bin/sbctl" or
#                   "sudo -u sbctl -H /usr/local/bin/sbctl" (config comes with it)
#   AS_SBCTL        prefix for commands that read the node's files, "" when the caller
#                   owns them, "sudo -u sbctl" when sbctl does
#   API_URL         Management API origin, e.g. http://api.127.0.0.1.sslip.io:40080
#   PAT_FILE        file with a personal access token for the API
#   PROJECT_URL     project origin with {ref}, e.g. http://{ref}.api.127.0.0.1.sslip.io:40080
#   REF_A, REF_B    the two projects
#   STATE_DIR       the node's state directory (default /var/lib/sbctl)
#   RUNTIME_URL     optional: http://127.0.0.1:<edge runtime port>, checked directly
#   WORK            scratch directory (default: a new temporary one)
#   NODE_DIR        directory with @supabase/supabase-js installed (default: $WORK/node,
#                   installed with npm)
#   DEPLOY_VIA      cli or artifact, see above (default: artifact on macOS, cli elsewhere)
#   DEPLOY_B_VIA    api (default: project B uploads sources with --use-api and the node bundles
#                   them) or artifact (project B is bundled here too, for a node that cannot
#                   bundle sources)
#   EDGE_RUNTIME_BIN  bin/edge-runtime of the artifact, for DEPLOY_VIA=artifact (default: the one
#                   under $STATE_DIR/artifacts)
#   MAX_PER_PROJECT  the node's [functions] max_per_project when it was set low (8) so that the flood
#                   check can exceed it; unset skips the "refused beyond the cap" assertion
#   TMP_QUOTA_MB    the node's [functions] tmp_quota_mb (default 64), which the tmpwrite check writes past
#   SANDBOXED_BUNDLER  1 when the node's bundler runs in its systemd sandbox: the script then
#                   also checks that an upload cannot import another project's files
#   PROC_ESCAPE_PIDS  with SANDBOXED_BUNDLER: pids of processes of the node (the runtime, the
#                   daemon) whose /proc/<pid>/root and environ an upload tries to import
#   PROXY_TOKEN_FILE  file with the node's proxy secret, for checks that call the runtime
#                   directly (default: $STATE_DIR/system/edge-runtime.token)
#
# Needs: supabase CLI, node and npm, python3, curl, network access (npm packages that the
# fixtures import, supabase-js).
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
: "${SBCTL_RUN:?}" "${API_URL:?}" "${PAT_FILE:?}" "${PROJECT_URL:?}" "${REF_A:?}" "${REF_B:?}"
AS_SBCTL=${AS_SBCTL:-}
STATE_DIR=${STATE_DIR:-/var/lib/sbctl}
if [[ -z ${DEPLOY_VIA:-} ]]; then
  if [[ $(uname -s) == Darwin ]]; then DEPLOY_VIA=artifact; else DEPLOY_VIA=cli; fi
fi
SANDBOXED_BUNDLER=${SANDBOXED_BUNDLER:-0}
PROXY_TOKEN_FILE=${PROXY_TOKEN_FILE:-$STATE_DIR/system/edge-runtime.token}
WORK=${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/sbctl-functions-XXXXXX")}
NODE_DIR=${NODE_DIR:-$WORK/node}
SUPABASE_JS=2.117.2

log()  { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }
sbctl() { $SBCTL_RUN "$@"; }
asnode() { if [[ -n $AS_SBCTL ]]; then $AS_SBCTL "$@"; else "$@"; fi; }
jget() { python3 -c 'import json,sys; d=json.load(sys.stdin); print('"$1"')'; }

mkdir -p "$WORK"
PAT=$(<"$PAT_FILE")
TENANTS=$STATE_DIR/system/edge-runtime/tenants
export SUPABASE_ACCESS_TOKEN=$PAT SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1 SUPABASE_DISABLE_UPDATE_CHECK=1

# The CLI reads the API from a profile file; project_host takes no port.
API_HOST=${API_URL#*://}; API_HOST=${API_HOST%%:*}
cat >"$WORK/profile.yaml" <<EOF
name: sbctl-functions-test
api_url: $API_URL
dashboard_url: http://127.0.0.1:1
project_host: $API_HOST
pooler_host: pooler.${API_HOST#api.}
EOF
sb() { local dir=$1; shift; (cd "$dir" && supabase --profile="$WORK/profile.yaml" "$@" 2> >(grep -v 'new version\|recommend updating' >&2)); }

case $DEPLOY_VIA in
  cli)
    if [[ $(uname -s) == Darwin ]] && command -v docker >/dev/null && docker info >/dev/null 2>&1; then
      fail "DEPLOY_VIA=cli would bundle in Docker containers, and Docker is running on this machine; use DEPLOY_VIA=artifact"
    fi ;;
  artifact)
    EDGE_RUNTIME_BIN=${EDGE_RUNTIME_BIN:-$(ls -d "$STATE_DIR"/artifacts/edge-runtime/*/bin/edge-runtime 2>/dev/null | head -1)}
    [[ -x ${EDGE_RUNTIME_BIN:-} ]] || fail "DEPLOY_VIA=artifact needs the edge-runtime artifact: set EDGE_RUNTIME_BIN" ;;
  *) fail "DEPLOY_VIA must be cli or artifact, not $DEPLOY_VIA" ;;
esac

urlenc() { python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }

# deploy_artifact DIR REF SLUG [--no-verify-jwt]: what `supabase functions deploy` does with Docker,
# without Docker: bundle with the artifact's `edge-runtime bundle` (DENO_NO_PACKAGE_JSON=1 and the
# entrypoint as an absolute path, as the CLI passes them), compress as the CLI does ("EZBR" and
# Brotli, quality 6), and upload with the CLI's request: POST /v1/projects/{ref}/functions (create)
# or PATCH .../functions/{slug} (update), Content-Type application/vnd.denoland.eszip, query
# verify_jwt, entrypoint_path, ezbr_sha256 (and slug, name on create).
deploy_artifact() {
  local dir=$1 ref=$2 slug=$3 verify=true; shift 3
  [[ ${1:-} == --no-verify-jwt ]] && verify=false
  local entry="$dir/supabase/functions/$slug/index.ts" raw="$WORK/$slug.eszip" ezbr="$WORK/$slug.ezbr" sha code
  (cd "$dir" && DENO_NO_PACKAGE_JSON=1 DENO_DIR="$WORK/deno" "$EDGE_RUNTIME_BIN" bundle --entrypoint "$entry" --output "$raw" --quiet) || return 1
  sha=$(node -e '
    const z = require("node:zlib"), fs = require("node:fs"), c = require("node:crypto")
    const out = Buffer.concat([Buffer.from("EZBR"), z.brotliCompressSync(fs.readFileSync(process.argv[1]), { params: { [z.constants.BROTLI_PARAM_QUALITY]: 6 } })])
    fs.writeFileSync(process.argv[2], out)
    console.log(c.createHash("sha256").update(out).digest("hex"))' "$raw" "$ezbr") || return 1
  local q="verify_jwt=$verify&entrypoint_path=$(urlenc "file://$entry")&ezbr_sha256=$sha" method=POST url="$API_URL/v1/projects/$ref/functions"
  if [[ $(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $PAT" "$url/$slug") == 200 ]]; then
    method=PATCH url="$url/$slug"
  else
    q="slug=$slug&name=$slug&$q"
  fi
  code=$(curl -s -o "$WORK/deploy-response.json" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $PAT" \
    -H 'Content-Type: application/vnd.denoland.eszip' --data-binary "@$ezbr" "$url?$q") || return 1
  [[ $code == 200 || $code == 201 ]] || { log "$method $url answered $code: $(cat "$WORK/deploy-response.json")"; return 1; }
}

# deploy_a SLUG [--no-verify-jwt]: project A is bundled here (see DEPLOY_VIA).
deploy_a() {
  local slug=$1; shift
  if [[ $DEPLOY_VIA == cli ]]; then
    sb "$WORK/work-a" functions deploy "$slug" --project-ref "$REF_A" "$@" >/dev/null
  else
    deploy_artifact "$WORK/work-a" "$REF_A" "$slug" "$@"
  fi
}
# deploy_b SLUG [--no-verify-jwt]: project B uploads sources; the node bundles them.
deploy_b() {
  local slug=$1; shift
  if [[ ${DEPLOY_B_VIA:-api} == artifact ]]; then # a node that cannot bundle sources
    deploy_artifact "$WORK/work-b" "$REF_B" "$slug" "$@"
  else
    sb "$WORK/work-b" functions deploy "$slug" --use-api --project-ref "$REF_B" "$@" >/dev/null
  fi
}

if [[ ! -d $NODE_DIR/node_modules/@supabase ]]; then
  log "installing supabase-js $SUPABASE_JS"
  mkdir -p "$NODE_DIR"
  (cd "$NODE_DIR" && echo '{"name":"fnverify","private":true,"type":"module"}' >package.json &&
    npm install --no-audit --no-fund --save-exact "@supabase/supabase-js@$SUPABASE_JS" >/dev/null)
fi
cp "$HERE/verify.mjs" "$NODE_DIR/verify.mjs"

for p in a b; do
  rm -rf "$WORK/work-$p"
  cp -R "$HERE/fixtures/$p" "$WORK/work-$p"
done

log "collecting the keys of both projects"
keys() { # REF NAME -> JSON object for the verify config
  sbctl projects get "$1" --json --show-keys | python3 -c '
import json, sys
ref, name, tpl = sys.argv[1:4]
d = json.load(sys.stdin); k = d["keys"]
print(json.dumps({"ref": ref, "name": name, "url": tpl.replace("{ref}", ref), "anon": k["anon_key"], "service": k["service_role_key"],
  "publishable": k["publishable_key"], "secret": k["secret_key"], "jwtSecret": k["jwt_secret"]}))' "$1" "$2" "$PROJECT_URL"
}
proxy_token=$(asnode cat "$PROXY_TOKEN_FILE" 2>/dev/null || true)
python3 - "$WORK/config.json" "$API_URL" "$PAT" "${RUNTIME_URL:-}" "$(keys "$REF_A" a)" "$(keys "$REF_B" b)" "$STATE_DIR" "$proxy_token" "${MAX_PER_PROJECT:-0}" "${TMP_QUOTA_MB:-64}" <<'PY'
import json, sys
out, api, pat, rt, a, b, _, tok, cap, quota = sys.argv[1:11]
json.dump({"apiUrl": api, "pat": pat, "runtimeUrl": rt or None, "stateDir": sys.argv[7], "proxyToken": tok or None,
  "maxPerProject": int(cap) or None, "tmpQuotaMb": int(quota), "projects": {"a": json.loads(a), "b": json.loads(b)}}, open(out, "w"))
PY
chmod 600 "$WORK/config.json"

log "deploying project A (bundled here, DEPLOY_VIA=$DEPLOY_VIA)"
for slug in hello onlya dbcheck crash spin hog readfs escape callout tmpwrite stream; do
  deploy_a "$slug" || fail "deploy A/$slug"
done
deploy_a open --no-verify-jwt || fail "deploy A/open"
log "deploying project B (sources uploaded with --use-api, bundled by the node): the same slugs with different code"
for slug in hello dbcheck; do
  deploy_b "$slug" || fail "deploy B/$slug"
done
deploy_b open --no-verify-jwt || fail "deploy B/open"
if [[ $SANDBOXED_BUNDLER == 1 ]]; then
  log "the node's bundler cannot import another project's files"
  jwt_a=$(jget 'd["projects"]["a"]["jwtSecret"]' <"$WORK/config.json")
  # An upload whose entrypoint imports $2 (a JSON module), sent to project B. Bundled outside the
  # sandbox it would resolve to project A's environment file (JWT secret, service key, database
  # password) and the bundle would carry it out; inside, the file is not there or cannot be opened.
  try_steal() { # NAME IMPORT-SPECIFIER
    printf 'import secret from "%s" with { type: "json" }\nDeno.serve(() => Response.json(secret))\n' "$2" >"$WORK/steal-index.ts"
    out=$(curl -s -w '\n%{http_code}' -X POST -H "Authorization: Bearer $PAT" \
      -F 'metadata={"entrypoint_path":"index.ts","name":"steal"};type=application/json' \
      -F "file=@$WORK/steal-index.ts;filename=index.ts" "$API_URL/v1/projects/$REF_B/functions/deploy?slug=steal") || fail "curl: steal upload ($1)"
    [[ $(tail -n1 <<<"$out") == 400 ]] || fail "an upload that imports another project's file ($1) answered: $out"
    grep -q "Could not bundle" <<<"$out" || fail "the refusal ($1) does not say why: $out"
    if grep -qF "$jwt_a" <<<"$out"; then fail "the bundler's error ($1) shows project A's JWT secret"; fi
    if grep -q "SBCTL_FUNCTIONS_\|EDGE_RUNTIME_PORT" <<<"$out"; then fail "the bundler's error ($1) shows the environment of the runtime: $out"; fi
    [[ $(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $PAT" "$API_URL/v1/projects/$REF_B/functions/steal") == 404 ]] || fail "the refused upload ($1) was stored"
  }
  try_steal "a relative path" "../../../../../../../../../..$TENANTS/$REF_A/functions-env.json"
  # The same file through /proc/<pid>/root of a process of the node: a mount namespace does not
  # hide it from a process with the uid of that process, so the bundler must run under another uid
  # (and not see these processes at all). PROC_ESCAPE_PIDS lists the pids to try: the runtime's
  # and the daemon's.
  for pid in ${PROC_ESCAPE_PIDS:-}; do
    try_steal "through /proc/$pid/root" "/proc/$pid/root$TENANTS/$REF_A/functions-env.json"
    try_steal "through /proc/$pid/environ" "/proc/$pid/environ"
  done
fi

log "setting secrets with the CLI"
sb "$WORK/work-a" secrets set MY_SECRET=secret-for-a --project-ref "$REF_A" >/dev/null || fail "secrets set A"
sb "$WORK/work-b" secrets set MY_SECRET=secret-for-b --project-ref "$REF_B" >/dev/null || fail "secrets set B"
names=$(sb "$WORK/work-a" secrets list --project-ref "$REF_A" --output-format json 2>/dev/null) || fail "secrets list"
grep -q MY_SECRET <<<"$names" || fail "secrets list does not show MY_SECRET: $names"

log "functions list (CLI) and sbctl functions list"
# Text on a terminal or in CI, JSON for agents: ask for JSON, the one format a script can read.
sb "$WORK/work-a" functions list --project-ref "$REF_A" --output-format json | grep -q '"slug":"hello"' || fail "supabase functions list lacks hello"
live=$(sbctl functions list "$REF_A" --json | jget 'sum(1 for r in d if r["live"])')
[[ $live -eq 12 ]] || fail "sbctl functions list: $live of 12 functions live"

log "files on disk"
for ref in "$REF_A" "$REF_B"; do
  env_file="$TENANTS/$ref/functions-env.json"
  mode=$(asnode stat -c '%a' "$env_file" 2>/dev/null || asnode stat -f '%Lp' "$env_file")
  [[ $mode == 600 ]] || fail "$env_file has mode $mode, want 600"
done
if asnode grep -rlF "$(jget 'd["projects"]["b"]["jwtSecret"]' <"$WORK/config.json")" "$TENANTS/$REF_A" >/dev/null 2>&1; then
  fail "project A's files hold project B's JWT secret"
fi

log "seeding a table in each project's database (psql over loopback)"
PSQL=$(ls -d "$STATE_DIR"/artifacts/postgres/*/bin/psql | head -1)
for pair in "$REF_A:a" "$REF_B:b"; do
  ref=${pair%%:*}; name=${pair##*:}
  view=$(sbctl projects get "$ref" --json --show-keys)
  pw=$(jget 'd["keys"]["db_password"]' <<<"$view"); port=$(jget 'd["ports"]["Postgres"]' <<<"$view")
  PGPASSWORD=$pw "$PSQL" "host=127.0.0.1 port=$port user=postgres dbname=postgres sslmode=disable connect_timeout=10" -v ON_ERROR_STOP=1 -q <<SQL || fail "seeding $ref"
create table if not exists public.fn_probe (id int primary key, who text);
insert into public.fn_probe values (1, 'project-$name') on conflict (id) do nothing;
grant all on public.fn_probe to service_role;
notify pgrst, 'reload schema';
SQL
done

log "verify: main checks"
(cd "$NODE_DIR" && node verify.mjs "$WORK/config.json" main) || fail "verify main"

log "a changed secret reaches the next request"
sb "$WORK/work-a" secrets set MY_SECRET=rotated-secret-for-a --project-ref "$REF_A" >/dev/null || fail "secrets set (rotate)"
(cd "$NODE_DIR" && node verify.mjs "$WORK/config.json" secrets) || fail "verify secrets"

log "a redeploy replaces the code"
sed -i.bak "s/code: 'A1'/code: 'A2'/" "$WORK/work-a/supabase/functions/hello/index.ts" && rm -f "$WORK/work-a/supabase/functions/hello/index.ts.bak"
grep -q "code: 'A2'" "$WORK/work-a/supabase/functions/hello/index.ts" || fail "could not edit the fixture"
deploy_a hello || fail "redeploy A/hello"
(cd "$NODE_DIR" && node verify.mjs "$WORK/config.json" redeploy) || fail "verify redeploy"

log "a deleted function is gone"
sb "$WORK/work-a" functions delete hello --project-ref "$REF_A" --yes >/dev/null || fail "delete A/hello"
(cd "$NODE_DIR" && node verify.mjs "$WORK/config.json" deleted) || fail "verify deleted"
asnode test ! -e "$TENANTS/$REF_A/functions/hello" || fail "the deleted function is still on disk"

log "pausing a project takes its keys and functions off the disk at once; resuming brings them back"
api_post() { curl -s -o /dev/null -w '%{http_code}' --max-time 300 -X POST -H "Authorization: Bearer $PAT" "$API_URL/v1/projects/$1/$2"; }
[[ $(api_post "$REF_B" pause) == 200 ]] || fail "pause $REF_B"
asnode test ! -e "$TENANTS/$REF_B" || fail "a paused project's files are still on disk"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "${PROJECT_URL//\{ref\}/$REF_B}/functions/v1/open" || true)
[[ $code == 503 ]] || fail "a paused project answered $code on /functions/v1"
[[ $(api_post "$REF_B" restore) == 200 ]] || fail "restore $REF_B"
asnode test -f "$TENANTS/$REF_B/functions-env.json" || fail "a resumed project's environment file is not back"
for ((i = 0; i < 60; i++)); do
  [[ $(curl -s --max-time 20 "${PROJECT_URL//\{ref\}/$REF_B}/functions/v1/open" || true) == open-b ]] && break
  sleep 1
done
[[ $(curl -s --max-time 20 "${PROJECT_URL//\{ref\}/$REF_B}/functions/v1/open") == open-b ]] || fail "a resumed project does not serve its functions"

log "deleting project B removes everything of it"
sbctl projects delete "$REF_B" --skip-final-backup >/dev/null || fail "project delete"
# `sbctl projects delete` removes the tree itself; on a node that runs the API server the
# reconcile would too.
asnode test ! -e "$TENANTS/$REF_B" || fail "the functions of $REF_B survived the project delete"
asnode test ! -e "$STATE_DIR/projects/$REF_B" || fail "projects/$REF_B survived the delete"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -H "Authorization: Bearer $(jget 'd["projects"]["b"]["anon"]' <"$WORK/config.json")" "${PROJECT_URL//\{ref\}/$REF_B}/functions/v1/hello" || true)
[[ $code == 404 || $code == 503 ]] || fail "a deleted project answered $code on /functions/v1"
(cd "$NODE_DIR" && node --input-type=module -e "
import fs from 'node:fs'
const c = JSON.parse(fs.readFileSync(process.argv[1], 'utf8')).projects.a
const r = await fetch(c.url + '/functions/v1/open')
if ((await r.text()) !== 'open-a') { console.error('project A broke when B was deleted'); process.exit(1) }
" "$WORK/config.json") || fail "project A after deleting B"

log "functions end-to-end checks passed (scratch: $WORK)"
