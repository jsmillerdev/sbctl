#!/usr/bin/env bash
# The Supabase CLI against a node, as a customer uses it: link, migrations, db push through the
# pooler, gen types, functions deploy, secrets. Run by run.sh; it needs a node that is up.
#
# The CLI reaches a node through a profile file (--profile), which `sbctl api profile` prints:
# the Management API URL, the project host and the pooler's registrable domain. That is the
# documented way to point the CLI at a self-hosted Management API, and it is what a node's
# owner is told to do. (The CLI also reads SUPABASE_PROFILE, with the same value.) No Docker:
# functions are bundled by the node (`functions deploy --use-api`); Docker-based bundling is
# covered by tests/linux/functions-smoke.sh.
#
# Environment: SUPABASE_CLI (the binary), PROFILE (the file), API_URL, PAT, REF, DBPASS,
# PROJECT_URL, WORK (scratch), SUPABASE_CLI_VERSION (checked against `supabase --version`).
# Every step runs; the exit status is non-zero when any failed.
set -uo pipefail

: "${SUPABASE_CLI:?}" "${PROFILE:?}" "${API_URL:?}" "${PAT:?}" "${REF:?}" "${DBPASS:?}" "${PROJECT_URL:?}" "${WORK:?}"
PROJ=$WORK/cli-project
OUT=$WORK/cli-out
mkdir -p "$PROJ" "$OUT" "$WORK/cli-home"
FAILS=0
N=0

log() { printf '%s cli: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
bad() { log "FAIL: $*"; FAILS=$((FAILS + 1)); }

# cli ARGS...: the CLI in the project directory with the node's profile and the token in the
# environment (never in argv). The keyring and telemetry are off; there is no terminal.
cli() {
  (cd "$PROJ" && env HOME="$WORK/cli-home" SUPABASE_ACCESS_TOKEN="$PAT" SUPABASE_DB_PASSWORD="$DBPASS" \
    SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1 SUPABASE_DISABLE_UPDATE_CHECK=1 \
    "$SUPABASE_CLI" --profile "$PROFILE" "$@" </dev/null)
}

# step NAME ARGS...: runs the CLI, keeps its output in $OUT/<n>-<name>.log and sets LAST to the
# path. A non-zero exit is a failure and its output is printed.
step() {
  local name=$1; shift
  N=$((N + 1))
  LAST=$OUT/$(printf '%02d' "$N")-$name.log
  log "supabase $*"
  if ! cli "$@" >"$LAST" 2>&1; then
    bad "supabase $* exited with status $?"
    sed 's/^/    /' "$LAST" | tail -40 >&2
    return 1
  fi
}

# has FILE TEXT DESCRIPTION: the file contains the text (fixed string).
has() {
  grep -qF -- "$2" "$1" || { bad "$3 (looked for '$2' in $(basename "$1"))"; sed 's/^/    /' "$1" | tail -20 >&2; return 1; }
}

mapi() { # METHOD PATH [curl args]: the Management API
  local m=$1 p=$2; shift 2
  curl -sS -m 60 -X "$m" -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' "$@" "$API_URL$p"
}
query() { # SQL: rows as JSON through database/query
  mapi POST "/v1/projects/$REF/database/query" -d "$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$1")"
}

step version --version && has "$LAST" "${SUPABASE_CLI_VERSION:-.}" "the CLI is not the pinned version"

step init init --force
[[ -f $PROJ/supabase/config.toml ]] || bad "init wrote no supabase/config.toml"

step projects projects list -o json && has "$LAST" "$REF" "projects list does not show the project"

step link link --project-ref "$REF" --yes
if [[ $(cat "$PROJ/supabase/.temp/project-ref" 2>/dev/null) != "$REF" ]]; then bad "link did not record the project ref"; fi
POOLER_URL=$(cat "$PROJ/supabase/.temp/pooler-url" 2>/dev/null || true)
# The file holds the user, host and port of the pooler (the CLI drops the password placeholder).
[[ $POOLER_URL == *"postgres.$REF"*"@pooler."* ]] || bad "link recorded no pooler URL for $REF (got '$POOLER_URL')"

step api-keys projects api-keys --project-ref "$REF" && has "$LAST" "anon" "projects api-keys does not list the anon key"

# A migration, pushed through the pooler.
step migration-new migration new conf_widgets
MIG=$(ls "$PROJ"/supabase/migrations/*_conf_widgets.sql 2>/dev/null | head -1)
if [[ -z $MIG ]]; then
  bad "migration new wrote no file"
else
  VERSION=$(basename "$MIG" | cut -d_ -f1)
  cat >"$MIG" <<SQL
create table public.conf_widgets (id int primary key, name text not null);
insert into public.conf_widgets values (1, 'sprocket'), (2, 'gear');
SQL
  step migration-list-before migration list --linked
  line=$(grep -F "$VERSION" "$LAST" | head -1)
  [[ $(grep -o "$VERSION" <<<"$line" | wc -l) -eq 1 ]] || bad "before the push the migration should be local only: '$line'"

  step db-push db push --yes && has "$LAST" "Finished supabase db push" "db push did not finish"
  rows=$(query "select name from public.conf_widgets order by id")
  [[ $rows == *sprocket* && $rows == *gear* ]] || bad "the pushed migration did not create the rows: $rows"
  recorded=$(query "select version from supabase_migrations.schema_migrations")
  [[ $recorded == *"$VERSION"* ]] || bad "the migration is not recorded in supabase_migrations.schema_migrations: $recorded"

  step migration-list-after migration list --linked
  line=$(grep -F "$VERSION" "$LAST" | head -1)
  [[ $(grep -o "$VERSION" <<<"$line" | wc -l) -eq 2 ]] || bad "after the push the migration should be on both sides: '$line'"

  step db-push-again db push --yes && has "$LAST" "up to date" "a second db push should find nothing to do"
fi

# Types from the Management API.
step gen-types gen types typescript --project-id "$REF" && has "$LAST" "conf_widgets" "gen types does not include the pushed table"
step gen-types-linked gen types typescript --linked && has "$LAST" "conf_widgets" "gen types --linked does not include the pushed table"

# A function, bundled by the node, then called through the project's gateway.
SLUG=cliconf
mkdir -p "$PROJ/supabase/functions/$SLUG"
cat >"$PROJ/supabase/functions/$SLUG/index.ts" <<'TS'
Deno.serve(() => new Response(JSON.stringify({ from: 'cli', secret: Deno.env.get('CONF_CLI_SECRET') ?? null }), { headers: { 'Content-Type': 'application/json' } }))
TS
step secrets-set secrets set CONF_CLI_SECRET=from-the-cli
step secrets-list secrets list && has "$LAST" "CONF_CLI_SECRET" "secrets list does not show the secret"
step functions-deploy functions deploy "$SLUG" --use-api --project-ref "$REF"
step functions-list functions list --project-ref "$REF" && has "$LAST" "$SLUG" "functions list does not show the function"
ANON=$(mapi GET "/v1/projects/$REF/api-keys?reveal=true" | python3 -c 'import json,sys; print([k["api_key"] for k in json.load(sys.stdin) if k.get("name") == "anon"][0])')
answer=""
for ((i = 0; i < 60; i++)); do
  answer=$(curl -sS -m 20 -H "Authorization: Bearer $ANON" -H "apikey: $ANON" "$PROJECT_URL/functions/v1/$SLUG" 2>/dev/null || true)
  [[ $answer == *'"from":"cli"'* && $answer == *from-the-cli* ]] && break
  sleep 2
done
[[ $answer == *'"from":"cli"'* ]] || bad "the deployed function does not answer: $answer"
[[ $answer == *from-the-cli* ]] || bad "the function does not see the secret set with the CLI: $answer"
step functions-delete functions delete "$SLUG" --project-ref "$REF" --yes
step secrets-unset secrets unset CONF_CLI_SECRET --yes
step secrets-list-after secrets list
if grep -qF CONF_CLI_SECRET "$LAST"; then bad "secrets unset left the secret in the list"; fi

step unlink unlink --yes

log "$N steps, $FAILS failure(s)"
exit $((FAILS > 0))
