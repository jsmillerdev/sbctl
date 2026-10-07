#!/usr/bin/env bash
# Members and roles under real systemd units: users with each organization role, what
# Studio's permission list says about them, what the Management API lets them do through
# personal access tokens, and the real Supabase CLI and MCP server with a Read-only user's token.
#
#   sudo env "PATH=$PATH" SBCTL_BIN=/path/to/sbctl-linux-amd64 [SUPABASE_CLI=/path/to/supabase] [REQUIRE_CLIENTS=1] \
#     tests/linux/roles-smoke.sh [--teardown]
#
# - the claimed first user is Owner; `sbctl users invite --role` makes an Administrator, a
#   Developer and a Read-only member through the claim page (the link carries the token);
# - GET /platform/profile/permissions answers what each role may do in Studio's own terms;
# - with a PAT, Read-only reads (SELECT, list, types) and is refused every write (secrets,
#   migrations, settings, keys, lifecycle; SQL fails in the database), a Developer writes SQL and
#   migrations but not settings or secrets, an Administrator manages settings and members but not
#   Owners, the last Owner cannot be demoted or removed;
# - the MCP server (apply_migration, execute_sql) and, when SUPABASE_CLI is set, the Supabase CLI
#   (secrets set) list as Read-only and are refused writes; an Owner's can;
# - [mail] reaches sb-gotrue@system: an invitation to a new address (GoTrue invite) and to an
#   existing account (sign-in link) arrive at a mail sink, and following each link signs the person
#   in on the invitation, which they accept and join with the invited role.
#
# Without SBCTL_BIN the script builds sbctl with the go toolchain. It needs network access for the
# artifact downloads and, for the MCP server, npm. Not run in development (root, systemd and Linux
# required); CI runs it on an ephemeral Ubuntu 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
WORK=$(mktemp -d)
SMTP_PID=""
trap 'rc=$?; [[ -n $SMTP_PID ]] && kill "$SMTP_PID" 2>/dev/null; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; rm -rf "$WORK"; exit $rc' EXIT

# Ports away from anything the runner may listen on, as in settings-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000
P_SMTP=12526
ADMIN=http://127.0.0.1:7000
PASSWORD=roles-correct-horse-battery
ORG=default   # the claim keeps the organization the first project created

preflight
install_binary
setup_node
cat >>"$SBCTL_CONF" <<CONF

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

# sb-gotrue@system sends the dashboard's mail through this relay (a sink started below).
[mail]
smtp_host = "127.0.0.1"
smtp_port = $P_SMTP
smtp_from = "sbctl@example.com"
smtp_name = "Roles smoke"
CONF

# Same VM-local workaround as settings-smoke.sh.
install -d /etc/systemd/system/sb-postgres@.service.d
cat >/etc/systemd/system/sb-postgres@.service.d/10-roles-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/sbctl/artifacts
CONF
systemctl daemon-reload

# The mail sink (the same one settings-smoke.sh uses): messages are appended to a file.
cat >"$WORK/smtp.py" <<'PY'
import socketserver, sys
out = sys.argv[2]
class H(socketserver.StreamRequestHandler):
    def handle(self):
        w = lambda s: self.wfile.write((s + "\r\n").encode())
        w("220 sink ESMTP")
        data = False; buf = []
        for raw in self.rfile:
            line = raw.decode(errors="replace")
            if data:
                if line == ".\r\n":
                    data = False
                    open(out, "a").write("".join(buf) + "\n=====\n")
                    w("250 queued"); buf = []
                else:
                    buf.append(line)
                continue
            u = line.upper()
            if u.startswith(("EHLO", "HELO")): w("250 sink")
            elif u.startswith("DATA"): data = True; w("354 go")
            elif u.startswith("QUIT"): w("221 bye"); return
            else: w("250 ok")
class S(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
S(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
python3 "$WORK/smtp.py" "$P_SMTP" "$WORK/mails.txt" &
SMTP_PID=$!

log "system init, fleet, one project"
system_init
wait_active sb-postgres@system.service 30
sbctl fleet start || fail "fleet start"
REF=$(create_project roles micro)
[[ $REF =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF'"
sbctl fleet ensure-tenant "$REF" || fail "ensure-tenant"
CFG="/v1/projects/$REF"
PGPORT=$(project_field "$REF" 'd["ports"]["Postgres"]')
PSQL=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
sql() { sudo -u "$SBCTL_USER" "$PSQL" "host=$SBCTL_STATE/projects/$REF/postgres/sock port=$PGPORT user=supabase_admin dbname=postgres" -Atc "$1" </dev/null; }

log "daemon: sbctl.service"
systemctl start sbctl.service
for ((i = 0; i < 60; i++)); do
  [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && break
  sleep 1
done
[[ $(http_code "$ADMIN/v1/projects") == 401 ]] || { journalctl --no-pager -u sbctl.service | tail -30 >&2; fail "the Management API does not answer on the admin listener"; }

dash() { curl -sS -m 60 -X "$1" -H "Host: api.$SBCTL_DOMAIN" "${@:3}" "http://127.0.0.1$2"; }
sign_in() { dash POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' -d "{\"email\":\"$1\",\"password\":\"$PASSWORD\"}" | json_get 'd["access_token"]'; }
jwt_sub() { python3 -c 'import base64,json,sys; p=sys.argv[1].split(".")[1]; print(json.loads(base64.urlsafe_b64decode(p+"="*(-len(p)%4)))["sub"])' "$1"; }
declare -A JWT PAT UID_OF

# st TOKEN METHOD PATH [BODY]: the status of a Management API call. expect WANT TOKEN METHOD PATH [BODY].
st() {
  local tok=$1 m=$2 p=$3 b=${4:-}
  if [[ -n $b ]]; then
    curl -s -o /dev/null -w '%{http_code}' -m 300 -X "$m" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' -d "$b" "$ADMIN$p" || true
  else
    curl -s -o /dev/null -w '%{http_code}' -m 300 -X "$m" -H "Authorization: Bearer $tok" "$ADMIN$p" || true
  fi
}
body() { # TOKEN METHOD PATH [BODY]: the response body
  local tok=$1 m=$2 p=$3 b=${4:-}
  curl -sS -m 300 -X "$m" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' ${b:+-d "$b"} "$ADMIN$p"
}
expect() { # WANT TOKEN METHOD PATH [BODY]
  local want=$1 got; shift
  got=$(st "$@")
  [[ $got == "$want" ]] || { log "response: $(body "$@" 2>&1 | head -c 400)"; fail "$2 $3 answered $got, want $want"; }
}
papi() { # JWT METHOD PATH [BODY]: a /platform call with a dashboard session; the body goes to $WORK/last.json
  local j=$1 m=$2 p=$3 b=${4:-}
  curl -sS -m 120 -o "$WORK/last.json" -w '%{http_code}' -X "$m" -H "Authorization: Bearer $j" -H 'Content-Type: application/json' ${b:+-d "$b"} "$ADMIN$p" || true
}

log "the first user is the Owner"
CLAIM=$(sbctl claim token | tr -d '[:space:]')
[[ $CLAIM =~ ^sbc_[0-9a-f]{48}$ ]] || fail "claim token '$CLAIM'"
[[ $(dash POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
  -d "{\"token\":\"$CLAIM\",\"email\":\"owner@example.com\",\"password\":\"$PASSWORD\",\"organization_name\":\"Roles\"}") == 201 ]] || fail "claim"
mkauth() { # NAME: JWT, PAT and id of the signed-in user NAME
  local n=$1
  JWT[$n]=$(sign_in "$n@example.com") || fail "sign-in $n"
  UID_OF[$n]=$(jwt_sub "${JWT[$n]}")
  PAT[$n]=$(dash POST /platform/profile/access-tokens -H "Authorization: Bearer ${JWT[$n]}" -H 'Content-Type: application/json' -d "{\"name\":\"roles-$n\"}" | json_get 'd["token"]') || fail "PAT $n"
  [[ ${PAT[$n]} =~ ^sbp_[0-9a-f]{40}$ ]] || fail "PAT $n '${PAT[$n]}'"
}
mkauth owner
sbctl users list >"$WORK/users.txt"
grep -q "owner@example.com.*$ORG:owner" "$WORK/users.txt" || { cat "$WORK/users.txt" >&2; fail "users list does not show the owner"; }
[[ $(body "${PAT[owner]}" GET "/v1/organizations/$ORG/members" | json_get 'd[0]["role_name"]') == Owner ]] || fail "the first user is not Owner"

log "invite an Administrator, a Developer and a Read-only member (the claim page; --no-mail keeps the link local)"
mkuser() { # NAME ROLE
  local n=$1 role=$2 link tok
  link=$(sbctl users invite "$n@example.com" --role "$role" --org "$ORG" --no-mail 2>"$WORK/invite.err") || { cat "$WORK/invite.err" >&2; fail "invite $n"; }
  [[ $link == */claim#* ]] || fail "invite link for a new address is not the claim page: $link"
  tok=$(python3 -c 'import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).fragment)["token"][0])' "$link")
  [[ $tok =~ ^sbi_[0-9a-f]{48}$ ]] || fail "invite token '$tok'"
  [[ $(dash POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
    -d "{\"token\":\"$tok\",\"email\":\"$n@example.com\",\"password\":\"$PASSWORD\"}") == 201 ]] || fail "claim page for $n"
  mkauth "$n"
}
mkuser admin administrator
mkuser dev developer
mkuser ro read-only
mkuser outsider read-only
sbctl users list >"$WORK/users.txt"
for pair in "admin administrator" "dev developer" "ro read-only"; do
  set -- $pair
  grep -q "$1@example.com.*$ORG:$2" "$WORK/users.txt" || { cat "$WORK/users.txt" >&2; fail "users list: $1 is not $2"; }
done
sbctl users invite dev@example.com --role developer --org "$ORG" --no-mail >"$WORK/again.out" 2>&1 && fail "inviting a member again was not refused"
grep -q "already a member" "$WORK/again.out" || { cat "$WORK/again.out" >&2; fail "inviting a member again: unexpected message"; }

log "Studio: what the permission list says (Studio's own matching rule)"
cat >"$WORK/perm.py" <<'PY'
import json, re, sys
role, slug = sys.argv[1], sys.argv[2]
perms = json.load(sys.stdin)
def rx(p): return re.compile("^" + ".*".join(re.escape(x) for x in p.split("%")) + "$")
def match(p, action, resource): return any(rx(a).match(action) for a in p["actions"]) and any(rx(r).match(resource) for r in p["resources"])
def can(action, resource):
    ps = [p for p in perms if p["organization_slug"] == slug and not p["project_refs"] and p["condition"] is None and match(p, action, resource)]
    if any(p["restrictive"] for p in ps): return False
    return any(not p["restrictive"] for p in ps)
want = {
  "owner": {("write:Update", "organizations"): True, ("write:Create", "projects"): True, ("read:Read", "service_api_keys"): True, ("tenant:Sql:Write:Insert", "*"): True},
  "admin": {("write:Update", "organizations"): False, ("write:Create", "projects"): True, ("read:Read", "service_api_keys"): True, ("tenant:Sql:Write:Insert", "*"): True},
  "dev": {("write:Update", "organizations"): False, ("write:Create", "projects"): False, ("read:Read", "service_api_keys"): True, ("tenant:Sql:Write:Insert", "*"): True, ("write:Update", "custom_config_gotrue"): False},
  "ro": {("write:Update", "organizations"): False, ("write:Create", "projects"): False, ("read:Read", "service_api_keys"): False, ("tenant:Sql:Write:Insert", "*"): False, ("tenant:Sql:Query", "*"): True, ("read:Read", "organizations"): True, ("write:Create", "user_content"): True},
}[role]
bad = [f"{a} on {r}: want {w}" for (a, r), w in want.items() if can(a, r) != w]
if bad: sys.exit(role + ": " + "; ".join(bad))
if role == "owner" and not any(p["actions"] == ["%"] and p["resources"] == ["%"] and not p["restrictive"] and p["condition"] is None for p in perms):
    sys.exit("owner has no unconditional % on %")
PY
for r in owner admin dev ro; do
  curl -sS -m 30 -H "Authorization: Bearer ${JWT[$r]}" "$ADMIN/platform/profile/permissions" | python3 "$WORK/perm.py" "$r" "$ORG" || fail "permissions of $r"
done
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[owner]}" "$ADMIN/platform/organizations" | json_get 'd[0]["is_owner"]') == True ]] || fail "organization list: owner is_owner"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/organizations" | json_get 'd[0]["is_owner"]') == False ]] || fail "organization list: read-only is_owner"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/organizations/$ORG/roles" | json_get '",".join(r["name"] for r in d["org_scoped_roles"])') == Owner,Administrator,Developer,Read-only ]] || fail "roles"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/organizations/$ORG/members" | json_get 'len(d)') == 5 ]] || fail "members"

log "Read-only: reads work, every write is refused"
# The table is made through the API (as the project's postgres role, like every role's SQL), so
# that the Developer's writes below meet the privileges they will have in real use.
expect 201 "${PAT[owner]}" POST "$CFG/database/query" '{"query":"create table if not exists public.roles_t (id int primary key, v text); insert into public.roles_t values (1, '"'"'a'"'"') on conflict do nothing"}'
for r in ro dev admin owner; do
  expect 200 "${PAT[$r]}" GET /v1/projects
  expect 200 "${PAT[$r]}" GET "/v1/organizations/$ORG/members"
  expect 200 "${PAT[$r]}" GET "$CFG/secrets"
  expect 200 "${PAT[$r]}" GET "$CFG/database/migrations"
  expect 200 "${PAT[$r]}" GET "$CFG/types/typescript"
  expect 201 "${PAT[$r]}" POST "$CFG/database/query" '{"query":"select v from public.roles_t"}'
done
[[ $(body "${PAT[ro]}" GET /v1/projects | json_get 'len(d)') == 1 ]] || fail "read-only lists the project"
expect 403 "${PAT[ro]}" POST "$CFG/secrets" '[{"name":"RO_SECRET","value":"x"}]'
expect 403 "${PAT[ro]}" DELETE "$CFG/secrets" '["RO_SECRET"]'
expect 403 "${PAT[ro]}" POST "$CFG/database/migrations" '{"query":"create table public.ro_migration (id int)","name":"ro_migration"}'
expect 403 "${PAT[ro]}" PATCH "$CFG/config/auth" '{"site_url":"https://ro.example.com"}'
expect 403 "${PAT[ro]}" POST "$CFG/api-keys" '{"type":"publishable","name":"ro_key"}'
expect 403 "${PAT[ro]}" GET "$CFG/api-keys?reveal=true"
expect 403 "${PAT[ro]}" POST "$CFG/pause"
expect 403 "${PAT[ro]}" DELETE "$CFG"
expect 403 "${PAT[ro]}" PATCH "$CFG" '{"name":"renamed"}'
for q in "insert into public.roles_t values (2, 'b')" "update public.roles_t set v = 'x'" "delete from public.roles_t" "drop table public.roles_t" \
         "begin read write; delete from public.roles_t; commit" "reset role; delete from public.roles_t"; do
  code=$(st "${PAT[ro]}" POST "$CFG/database/query" "{\"query\":\"$q\"}")
  [[ $code =~ ^4 ]] || fail "read-only SQL '$q' answered $code"
done
[[ $(sql "select count(*) from public.roles_t") == 1 && $(sql "select v from public.roles_t") == a ]] || fail "a read-only statement changed data"
[[ $(sql "select to_regclass('public.ro_migration') is null") == t ]] || fail "a read-only migration ran"
# The login role the CLI uses for `db dump` is the read-only one, even when read-write is asked.
LR=$(body "${PAT[ro]}" POST "$CFG/cli/login-role" '{"read_only":false}')
[[ $(json_get 'd["role"].startswith("sbctl_cli_ro_")' <<<"$LR") == True ]] || fail "read-only member got a read-write login role: $LR"
# Dropping the project's login roles would break every member's CLI session.
expect 403 "${PAT[ro]}" DELETE "$CFG/cli/login-role"
# Shared saved items belong to their owner: a Read-only member cannot rewrite the snippet an Owner shares.
SNIP=44444444-4444-4444-8444-444444444444
code=$(papi "${JWT[owner]}" PUT "/platform/projects/$REF/content" "{\"id\":\"$SNIP\",\"name\":\"shared\",\"type\":\"sql\",\"visibility\":\"project\",\"content\":{\"sql\":\"select 1\"}}")
[[ $code == 200 ]] || fail "the Owner could not share a snippet: $code"
code=$(papi "${JWT[ro]}" PUT "/platform/projects/$REF/content" "{\"id\":\"$SNIP\",\"name\":\"shared\",\"type\":\"sql\",\"visibility\":\"project\",\"content\":{\"sql\":\"drop table public.roles_t\"}}")
[[ $code == 403 ]] || fail "a read-only member rewrote a shared snippet: $code"
code=$(papi "${JWT[ro]}" DELETE "/platform/projects/$REF/content?ids=$SNIP")
[[ $code == 403 ]] || fail "a read-only member deleted a shared snippet: $code"
papi "${JWT[owner]}" GET "/platform/projects/$REF/content/item/$SNIP" >/dev/null
[[ $(json_get 'd["content"]["sql"]' <"$WORK/last.json") == "select 1" ]] || fail "the shared snippet changed"
# The same SQL through Studio's pg-meta route (a dashboard session).
code=$(papi "${JWT[ro]}" POST "/platform/pg-meta/$REF/query" '{"query":"delete from public.roles_t"}')
[[ $code =~ ^4 ]] || fail "read-only pg-meta delete answered $code"
[[ $(sql "select count(*) from public.roles_t") == 1 ]] || fail "pg-meta changed data for a read-only member"
code=$(papi "${JWT[ro]}" POST "/platform/pg-meta/$REF/query" '{"query":"select v from public.roles_t"}')
[[ $code == 201 || $code == 200 ]] || fail "read-only pg-meta select answered $code"
# Secrets stay with the roles that may read them.
SETTINGS=$(curl -sS -m 30 -H "Authorization: Bearer ${JWT[ro]}" "$ADMIN/platform/projects/$REF/settings")
[[ $(json_get '"jwt_secret" in d' <<<"$SETTINGS") == False ]] || fail "the JWT secret is shown to a read-only member"
[[ $(json_get 'len(d["service_api_keys"])' <<<"$SETTINGS") == 1 ]] || fail "the service key is shown to a read-only member"
[[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[dev]}" "$ADMIN/platform/projects/$REF/settings" | json_get 'len(d["service_api_keys"])') == 2 ]] || fail "a developer must see the service key"

log "Developer: content yes, settings and secrets no"
expect 201 "${PAT[dev]}" POST "$CFG/database/query" '{"query":"insert into public.roles_t values (2, '"'"'dev'"'"')"}'
[[ $(sql "select v from public.roles_t where id = 2") == dev ]] || fail "the developer's insert is missing"
code=$(st "${PAT[dev]}" POST "$CFG/database/migrations" '{"query":"create table public.dev_migration (id int)","name":"dev_migration"}')
[[ $code == 200 || $code == 201 ]] || fail "the developer's migration answered $code"
[[ $(sql "select to_regclass('public.dev_migration') is not null") == t ]] || fail "the developer's migration did not run"
expect 403 "${PAT[dev]}" POST "$CFG/secrets" '[{"name":"DEV_SECRET","value":"x"}]'
expect 403 "${PAT[dev]}" PATCH "$CFG/config/auth" '{"site_url":"https://dev.example.com"}'
expect 403 "${PAT[dev]}" POST "$CFG/api-keys" '{"type":"publishable","name":"dev_key"}'
expect 403 "${PAT[dev]}" POST "$CFG/pause"
expect 403 "${PAT[dev]}" PATCH "$CFG/database/password" '{"password":"a-new-database-password"}'
expect 403 "${PAT[dev]}" POST "/v1/projects" "{\"name\":\"nope\",\"organization_slug\":\"$ORG\",\"db_pass\":\"correct-horse-battery\"}"

log "Administrator: settings and members, never Owners"
expect 201 "${PAT[admin]}" POST "$CFG/secrets" '[{"name":"ADMIN_SECRET","value":"x"}]'
expect 200 "${PAT[admin]}" PATCH "$CFG/config/auth" '{"site_url":"https://admin.example.com"}'
expect 201 "${PAT[admin]}" POST "$CFG/api-keys" '{"type":"publishable","name":"admin_key"}'
M="/platform/organizations/$ORG/members"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[owner]}" '{"role_id":3}') == 403 ]] || fail "an administrator demoted an owner"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[dev]}" '{"role_id":1}') == 403 ]] || fail "an administrator made an owner"
[[ $(papi "${JWT[admin]}" DELETE "$M/${UID_OF[owner]}") == 403 ]] || fail "an administrator removed an owner"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[dev]}" '{"role_id":2}') == 200 ]] || fail "an administrator could not promote a developer"
[[ $(papi "${JWT[admin]}" PATCH "$M/${UID_OF[dev]}" '{"role_id":3}') == 200 ]] || fail "an administrator could not demote an administrator"
[[ $(papi "${JWT[dev]}" PATCH "$M/${UID_OF[ro]}" '{"role_id":3}') == 403 ]] || fail "a developer changed a role"
[[ $(papi "${JWT[ro]}" DELETE "$M/${UID_OF[dev]}") == 403 ]] || fail "a read-only member removed a member"
[[ $(papi "${JWT[admin]}" PATCH "/platform/organizations/$ORG" '{"name":"Renamed"}') == 403 ]] || fail "an administrator changed the organization"
[[ $(papi "${JWT[owner]}" PATCH "/platform/organizations/$ORG" '{"name":"Roles"}') == 200 ]] || fail "an owner could not change the organization"

log "the organization keeps its last Owner"
[[ $(papi "${JWT[owner]}" PATCH "$M/${UID_OF[owner]}" '{"role_id":2}') == 400 ]] || fail "the last owner was demoted"
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[owner]}") == 400 ]] || fail "the last owner left"
if sbctl users remove owner@example.com >"$WORK/rm.out" 2>&1; then fail "users remove deleted the only owner"; fi
grep -q "only Owner" "$WORK/rm.out" || { cat "$WORK/rm.out" >&2; fail "users remove: unexpected refusal"; }

log "a project-scoped role: only that project's rights"
[[ $(papi "${JWT[owner]}" PATCH "$M/${UID_OF[outsider]}" "{\"role_id\":3,\"role_scoped_projects\":[\"$REF\"]}") == 200 ]] || fail "scoped role"
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[outsider]}/roles/4") == 200 ]] || fail "dropping the organization-wide role"
expect 201 "${PAT[outsider]}" POST "$CFG/database/query" '{"query":"select 1"}'
expect 403 "${PAT[outsider]}" POST "$CFG/secrets" '[{"name":"X","value":"y"}]'
[[ $(papi "${JWT[outsider]}" GET "/platform/organizations/$ORG/members/invitations") == 403 ]] || fail "a project-scoped member read the invitations"
# Removing the member takes the access of the tokens they made with it.
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[outsider]}") == 200 ]] || fail "removing a member"
expect 403 "${PAT[outsider]}" POST "$CFG/database/query" '{"query":"select 1"}'
[[ $(body "${PAT[outsider]}" GET /v1/projects | json_get 'len(d)') == 0 ]] || fail "a removed member lists projects"

log "the real MCP server: Read-only lists and is refused writes, Owner writes"
if command -v node >/dev/null && command -v npx >/dev/null; then
  SUPABASE_ACCESS_TOKEN=${PAT[owner]} node "$REPO_ROOT/internal/api/testdata/mcp-roles.mjs" "$ADMIN" "$REF" owner || fail "MCP as Owner"
  SUPABASE_ACCESS_TOKEN=${PAT[ro]} node "$REPO_ROOT/internal/api/testdata/mcp-roles.mjs" "$ADMIN" "$REF" read-only || fail "MCP as Read-only"
else
  [[ ${REQUIRE_CLIENTS:-} == 1 ]] && fail "node and npx are required (REQUIRE_CLIENTS=1)"
  log "node is not installed: the MCP checks are skipped"
fi

log "the real Supabase CLI: Read-only lists and is refused secrets set, Owner sets"
if [[ -n ${SUPABASE_CLI:-} ]]; then
  mkdir -p "$WORK/proj" "$WORK/home"
  cat >"$WORK/profile.yaml" <<PROFILE
name: sbctl-roles
api_url: $ADMIN
dashboard_url: http://127.0.0.1:1
project_host: api.$SBCTL_DOMAIN
pooler_host: pooler.$SBCTL_DOMAIN
PROFILE
  cli() { local t=$1; shift; (cd "$WORK/proj" && HOME="$WORK/home" SUPABASE_ACCESS_TOKEN=$t SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1 "$SUPABASE_CLI" --profile="$WORK/profile.yaml" "$@"); }
  cli "${PAT[ro]}" projects list >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI projects list as Read-only"; }
  grep -q "$REF" "$WORK/cli.out" || fail "CLI projects list does not show the project"
  cli "${PAT[ro]}" secrets list --project-ref "$REF" >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI secrets list as Read-only"; }
  cli "${PAT[ro]}" gen types typescript --project-id "$REF" >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI gen types as Read-only"; }
  if cli "${PAT[ro]}" secrets set CLI_RO=1 --project-ref "$REF" >"$WORK/cli.out" 2>&1; then cat "$WORK/cli.out" >&2; fail "CLI secrets set worked for a Read-only member"; fi
  grep -q "does not allow" "$WORK/cli.out" || { cat "$WORK/cli.out" >&2; fail "CLI secrets set: no explanation in the refusal"; }
  cli "${PAT[owner]}" secrets set CLI_OWNER=1 --project-ref "$REF" >"$WORK/cli.out" 2>&1 || { cat "$WORK/cli.out" >&2; fail "CLI secrets set as Owner"; }
  cli "${PAT[ro]}" secrets list --project-ref "$REF" >"$WORK/cli.out" 2>&1 || fail "CLI secrets list after the Owner's set"
  grep -q CLI_OWNER "$WORK/cli.out" || { cat "$WORK/cli.out" >&2; fail "the Owner's secret is not listed"; }
else
  [[ ${REQUIRE_CLIENTS:-} == 1 ]] && fail "SUPABASE_CLI is required (REQUIRE_CLIENTS=1)"
  log "SUPABASE_CLI is not set: the CLI checks are skipped"
fi

log "mail: invitations by email, to a new address and to an existing account"
cat >"$WORK/links.py" <<'PY'
import email, html, json, re, sys
raw = open(sys.argv[1]).read().split("\n=====\n")
out = []
for m in raw:
    if not m.strip(): continue
    msg = email.message_from_string(m)
    body = ""
    for part in (msg.walk() if msg.is_multipart() else [msg]):
        if part.get_content_type() in ("text/html", "text/plain"):
            body += part.get_payload(decode=True).decode(errors="replace")
    links = [html.unescape(h) for h in re.findall(r'href="([^"]+)"', body)] or re.findall(r"https?://\S+", body)
    out.append({"to": msg["To"], "subject": msg["Subject"], "links": links})
print(json.dumps(out))
PY
: >"$WORK/mails.txt"
mkuser outsider2 read-only   # an account that is then removed from the organization
[[ $(papi "${JWT[owner]}" DELETE "$M/${UID_OF[outsider2]}") == 200 ]] || fail "removing outsider2"
CODE=$(papi "${JWT[owner]}" POST "$M/invitations" '{"emails":["outsider2@example.com","newhire@example.com"],"role_id":3}')
[[ $CODE == 201 ]] || { cat "$WORK/last.json" >&2; fail "invitations answered $CODE"; }
[[ $(json_get 'sorted(d["succeeded"])' <"$WORK/last.json") == "['newhire@example.com', 'outsider2@example.com']" ]] || fail "invitations: $(cat "$WORK/last.json")"
for ((i = 0; i < 30; i++)); do
  [[ $(grep -c '^=====$' "$WORK/mails.txt" 2>/dev/null || echo 0) -ge 2 ]] && break
  sleep 2
done
python3 "$WORK/links.py" "$WORK/mails.txt" >"$WORK/mails.json"
[[ $(json_get 'len(d)' <"$WORK/mails.json") -ge 2 ]] || { cat "$WORK/mails.txt" >&2; fail "fewer than two invitation mails reached the sink"; }
accept_from_mail() { # ADDRESS: follow the mailed link, accept the invitation, check the role
  local addr=$1 link loc jwt tok sub
  link=$(json_get '[l for m in d if "'"$addr"'" in (m["to"] or "") for l in m["links"] if "/verify" in l][0]' <"$WORK/mails.json") || fail "no link mailed to $addr"
  loc=$(curl -s -o /dev/null -D - -m 30 --resolve "api.$SBCTL_DOMAIN:80:127.0.0.1" "$link" | tr -d '\r' | awk 'tolower($1)=="location:" {print $2}')
  [[ $loc == http://studio.$SBCTL_DOMAIN/join\?* ]] || fail "the mailed link for $addr does not lead to the invitation page: $loc"
  jwt=$(python3 -c 'import sys,urllib.parse as u; f=u.parse_qs(u.urlparse(sys.argv[1]).fragment); print(f["access_token"][0])' "$loc") || fail "the link for $addr carries no session: $loc"
  tok=$(python3 -c 'import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).query)["token"][0])' "$loc")
  [[ $tok =~ ^sbo_ ]] || fail "invitation token '$tok'"
  [[ $(papi "$jwt" GET "$M/invitations/$tok") == 200 ]] || { cat "$WORK/last.json" >&2; fail "invitation state for $addr"; }
  [[ $(json_get 'd["email_match"] and d["authorized_user"] and not d["expired_token"]' <"$WORK/last.json") == True ]] || fail "invitation state for $addr: $(cat "$WORK/last.json")"
  [[ $(papi "$jwt" POST "$M/invitations/$tok") == 201 ]] || { cat "$WORK/last.json" >&2; fail "accepting the invitation for $addr"; }
  [[ $(papi "$jwt" POST "$M/invitations/$tok") == 409 ]] || fail "a second accept for $addr was not refused"
  [[ $(papi "$jwt" PATCH "/platform/organizations/$ORG" '{"name":"x"}') == 403 ]] || fail "$addr changed the organization as a developer"
  sub=$(jwt_sub "$jwt")
  [[ $(curl -sS -m 30 -H "Authorization: Bearer ${JWT[owner]}" "$ADMIN$M" | json_get '[x for x in d if x["gotrue_id"]=="'"$sub"'"][0]["role_ids"]') == "[3]" ]] || fail "$addr is not a developer"
}
accept_from_mail outsider2@example.com
accept_from_mail newhire@example.com
sbctl users list >"$WORK/users.txt"
grep -q "newhire@example.com.*$ORG:developer" "$WORK/users.txt" || { cat "$WORK/users.txt" >&2; fail "users list does not show the invited user"; }

log "SSO default role rule"
sbctl users default-role set corp.example.com developer --org "$ORG" >/dev/null || fail "default-role set"
sbctl users default-role list >"$WORK/rules.txt"
grep -q "corp.example.com.*$ORG.*developer" "$WORK/rules.txt" || { cat "$WORK/rules.txt" >&2; fail "default-role list"; }
sbctl users default-role remove corp.example.com || fail "default-role remove"

log "roles smoke passed"
