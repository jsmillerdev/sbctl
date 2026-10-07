#!/usr/bin/env bash
# Edge Functions end to end, against a node that is already up: the Management API and the
# proxy reachable at $API_URL, sb-edge-runtime running, and two projects. It deploys the
# fixtures in tests/functions/fixtures with the real `supabase functions deploy` (the
# default flow, which bundles and uploads an eszip; --use-api, which uploads sources, must be
# refused), sets secrets with `supabase secrets set`, calls the functions with supabase-js and fetch
# (verify.mjs), redeploys, deletes, and checks the files on disk.
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
#
# Needs: supabase CLI, node and npm, python3, curl, network access (npm packages that the
# fixtures import, supabase-js).
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
: "${SBCTL_RUN:?}" "${API_URL:?}" "${PAT_FILE:?}" "${PROJECT_URL:?}" "${REF_A:?}" "${REF_B:?}"
AS_SBCTL=${AS_SBCTL:-}
STATE_DIR=${STATE_DIR:-/var/lib/sbctl}
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
python3 - "$WORK/config.json" "$API_URL" "$PAT" "${RUNTIME_URL:-}" "$(keys "$REF_A" a)" "$(keys "$REF_B" b)" "$STATE_DIR" <<'PY'
import json, sys
out, api, pat, rt, a, b, _ = sys.argv[1:8]
json.dump({"apiUrl": api, "pat": pat, "runtimeUrl": rt or None, "stateDir": sys.argv[7], "projects": {"a": json.loads(a), "b": json.loads(b)}}, open(out, "w"))
PY
chmod 600 "$WORK/config.json"

log "deploying project A (bundled, the default flow)"
for slug in hello onlya dbcheck crash spin hog readfs escape; do
  sb "$WORK/work-a" functions deploy "$slug" --project-ref "$REF_A" >/dev/null || fail "deploy A/$slug"
done
sb "$WORK/work-a" functions deploy open --no-verify-jwt --project-ref "$REF_A" >/dev/null || fail "deploy A/open"
log "a source upload (--use-api) is refused: a function that runs from files could import other projects' files"
if sb "$WORK/work-a" functions deploy hello --use-api --project-ref "$REF_A" >/dev/null 2>&1; then
  fail "supabase functions deploy --use-api was accepted"
fi
printf 'Deno.serve(() => new Response("from source"))\n' >"$WORK/source-index.ts"
out=$(curl -s -w '\n%{http_code}' -X POST -H "Authorization: Bearer $PAT" \
  -F 'metadata={"entrypoint_path":"index.ts","name":"hello"};type=application/json' \
  -F "file=@$WORK/source-index.ts;filename=index.ts" "$API_URL/v1/projects/$REF_A/functions/deploy?slug=hello") || fail "curl: source upload"
[[ $(tail -n1 <<<"$out") == 400 ]] || fail "a multipart source upload answered: $out"
grep -q "does not accept source uploads" <<<"$out" || fail "the refusal does not say why: $out"
log "deploying project B: the same slugs with different code"
for slug in hello dbcheck; do
  sb "$WORK/work-b" functions deploy "$slug" --project-ref "$REF_B" >/dev/null || fail "deploy B/$slug"
done
sb "$WORK/work-b" functions deploy open --no-verify-jwt --project-ref "$REF_B" >/dev/null || fail "deploy B/open"

log "setting secrets with the CLI"
sb "$WORK/work-a" secrets set MY_SECRET=secret-for-a --project-ref "$REF_A" >/dev/null || fail "secrets set A"
sb "$WORK/work-b" secrets set MY_SECRET=secret-for-b --project-ref "$REF_B" >/dev/null || fail "secrets set B"
names=$(sb "$WORK/work-a" secrets list --project-ref "$REF_A" --output-format json 2>/dev/null) || fail "secrets list"
grep -q MY_SECRET <<<"$names" || fail "secrets list does not show MY_SECRET: $names"

log "functions list (CLI) and sbctl functions list"
# Text on a terminal or in CI, JSON for agents: ask for JSON, the one format a script can read.
sb "$WORK/work-a" functions list --project-ref "$REF_A" --output-format json | grep -q '"slug":"hello"' || fail "supabase functions list lacks hello"
live=$(sbctl functions list "$REF_A" --json | jget 'sum(1 for r in d if r["live"])')
[[ $live -eq 9 ]] || fail "sbctl functions list: $live of 9 functions live"

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
sb "$WORK/work-a" functions deploy hello --project-ref "$REF_A" >/dev/null || fail "redeploy A/hello"
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
