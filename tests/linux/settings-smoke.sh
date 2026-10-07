#!/usr/bin/env bash
# Dashboard writes under real systemd units: every kind of project setting is saved through
# the Management API of the daemon (sbctl.service) with a personal access token, and the
# behavior of the running service is observed to change.
#
#   sudo SBCTL_BIN=/path/to/sbctl-linux-amd64 tests/linux/settings-smoke.sh [--teardown]
#
# Auth (a redirect URL the allow list accepts, a refused sign-up, SMTP and a custom mail
# template reaching a mail server, a provider's client id), PostgREST (a newly exposed
# schema), Storage (a bigger upload), Realtime (private channels only), Postgres settings
# (applied with ALTER SYSTEM, a restart-requiring one with a restart), API keys (a revoked
# secret key and the disabled legacy keys are refused at once), the database password reset
# (new works directly and through the pooler, the old one fails), and persistence: a pause
# and resume keeps every saved setting.
#
# Without SBCTL_BIN the script builds sbctl with the go toolchain. It needs network access
# for the artifact downloads. Not run in development (root, systemd and Linux required); CI
# runs it on an ephemeral Ubuntu 24.04 VM (amd64 and arm64).
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
WORK=$(mktemp -d)
SMTP_PID=""
trap 'rc=$?; [[ -n $SMTP_PID ]] && kill "$SMTP_PID" 2>/dev/null; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; rm -rf "$WORK"; exit $rc' EXIT

# Ports away from anything the runner may listen on, as in fleet-smoke.sh.
P_SESSION=15432 P_TRANSACTION=16543 P_REALTIME=14000 P_STORAGE=15000 P_STORAGE_ADMIN=15001 P_PGMETA=18080 P_API=14001 P_STUDIO=13000
P_SMTP=12525
ADMIN=http://127.0.0.1:7000
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=settings-correct-horse-battery

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
CONF

# Same VM-local workaround as fleet-smoke.sh: the Postgres launcher writes into the artifact
# directory on its first boot, which ProtectSystem=strict forbids.
install -d /etc/systemd/system/sb-postgres@.service.d
cat >/etc/systemd/system/sb-postgres@.service.d/10-settings-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/sbctl/artifacts
CONF
systemctl daemon-reload

log "system init, fleet, one project"
system_init
wait_active sb-postgres@system.service 30
sbctl fleet start || fail "fleet start"
REF=$(create_project settings micro)
[[ $REF =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF'"
sbctl fleet ensure-tenant "$REF" || fail "ensure-tenant"
HOST="$REF.api.$SBCTL_DOMAIN"
PGPORT=$(project_field "$REF" 'd["ports"]["Postgres"]')
PSQL=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
PUB=$(project_field "$REF" 'd["keys"]["publishable_key"]' --show-keys)
SEC=$(project_field "$REF" 'd["keys"]["secret_key"]' --show-keys)
ANON=$(project_field "$REF" 'd["keys"]["anon_key"]' --show-keys)
SVC=$(project_field "$REF" 'd["keys"]["service_role_key"]' --show-keys)
DBPW=$(project_field "$REF" 'd["keys"]["db_password"]' --show-keys)

# sql SQL: as supabase_admin over the project's unix socket (trusted, only the sbctl user reaches it).
sql() { sudo -u "$SBCTL_USER" "$PSQL" "host=$SBCTL_STATE/projects/$REF/postgres/sock port=$PGPORT user=supabase_admin dbname=postgres" -Atc "$1" </dev/null; }
# sysql DB SQL: the same on the system cluster (Realtime's tenant table lives there).
sysql() { sudo -u "$SBCTL_USER" "$PSQL" "host=$SBCTL_STATE/projects/system/postgres/sock port=5433 user=supabase_admin dbname=$1" -Atc "$2" </dev/null; }
direct_login() { PGPASSWORD=$1 "$PSQL" "host=127.0.0.1 port=$PGPORT user=postgres dbname=postgres sslmode=disable connect_timeout=10" -Atc 'select 1' </dev/null >/dev/null 2>&1; }
pooler_login() { PGPASSWORD=$1 "$PSQL" "host=127.0.0.1 port=$P_SESSION user=postgres.$REF dbname=postgres sslmode=disable connect_timeout=10" -Atc 'select 1' </dev/null >/dev/null 2>&1; }

log "daemon: sbctl.service"
systemctl start sbctl.service
for ((i = 0; i < 60; i++)); do
  [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && break
  sleep 1
done
[[ $(http_code "$ADMIN/v1/projects") == 401 ]] || { journalctl --no-pager -u sbctl.service | tail -30 >&2; fail "the Management API does not answer on the admin listener"; }

# A personal access token through the claim flow, as the installer's users get one.
dash() { curl -sS -m 60 -X "$1" -H "Host: api.$SBCTL_DOMAIN" "${@:3}" "http://127.0.0.1$2"; }
TOKEN=$(sbctl claim token | tr -d '[:space:]')
[[ $TOKEN =~ ^sbc_[0-9a-f]{48}$ ]] || fail "claim token '$TOKEN'"
[[ $(dash POST /claim -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
  -d "{\"token\":\"$TOKEN\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\",\"organization_name\":\"Settings\"}") == 201 ]] || fail "claim"
JWT=$(dash POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | json_get 'd["access_token"]') || fail "sign-in"
PAT=$(dash POST /platform/profile/access-tokens -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' -d '{"name":"settings"}' | json_get 'd["token"]') || fail "PAT"
[[ $PAT =~ ^sbp_[0-9a-f]{40}$ ]] || fail "PAT '$PAT'"

# api METHOD PATH [curl args]: the Management API with the PAT. code METHOD PATH BODY: its status.
api() { local m=$1 p=$2; shift 2; curl -sS -m 300 -X "$m" -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' "$@" "$ADMIN$p"; }
code() { local m=$1 p=$2 b=${3:-}; if [[ -n $b ]]; then api "$m" "$p" -o /dev/null -w '%{http_code}' -d "$b"; else api "$m" "$p" -o /dev/null -w '%{http_code}'; fi; }
must() { # STATUS METHOD PATH [BODY]: fail unless the API answers STATUS
  local want=$1 m=$2 p=$3 b=${4:-} got
  got=$(code "$m" "$p" "$b")
  [[ $got == "$want" ]] || { log "response: $(api "$m" "$p" ${b:+-d "$b"} 2>&1 | head -c 500)"; fail "$m $p answered $got, want $want"; }
}
# proj METHOD PATH [curl args]: the project through the proxy on :80.
proj() { local m=$1 p=$2; shift 2; curl -sS -m 60 -X "$m" -H "Host: $HOST" "$@" "http://127.0.0.1$p"; }
pcode() { local m=$1 p=$2; shift 2; curl -s -o /dev/null -w '%{http_code}' -m 60 -X "$m" -H "Host: $HOST" "$@" "http://127.0.0.1$p" || true; }
CFG="/v1/projects/$REF"
for ((i = 0; i < 60; i++)); do
  [[ $(pcode GET /auth/v1/settings -H "apikey: $PUB") == 200 ]] && break
  sleep 1
done
[[ $(pcode GET /auth/v1/settings -H "apikey: $PUB") == 200 ]] || fail "project API through the proxy"

log "auth: a new redirect URL, a provider, sign-ups"
must 200 PATCH "$CFG/config/auth" '{"site_url":"https://site.example.com","external_github_enabled":true,"external_github_client_id":"client-id","external_github_secret":"client-secret"}'
location() { # REDIRECT_TO: where GoTrue sends the holder of an invalid link
  curl -s -o /dev/null -D - -m 30 -H "Host: $HOST" "http://127.0.0.1/auth/v1/verify?type=magiclink&token=nope&redirect_to=$1" | tr -d '\r' | awk 'tolower($1)=="location:" {print $2}'
}
[[ $(location https%3A%2F%2Fnew.example.com%2Fcb) == https://site.example.com* ]] || fail "a redirect URL outside the allow list was accepted"
must 200 PATCH "$CFG/config/auth" '{"uri_allow_list":"https://new.example.com/**"}'
[[ $(location https%3A%2F%2Fnew.example.com%2Fcb) == https://new.example.com/cb* ]] || fail "the new redirect URL is not accepted: $(location https%3A%2F%2Fnew.example.com%2Fcb)"
GH=$(curl -s -o /dev/null -D - -m 30 -H "Host: $HOST" "http://127.0.0.1/auth/v1/authorize?provider=github" | tr -d '\r' | awk 'tolower($1)=="location:" {print $2}')
[[ $GH == https://github.com/login/oauth/authorize?* && $GH == *client_id=client-id* ]] || fail "GoTrue did not take the provider settings: $GH"
GOT=$(api GET "$CFG/config/auth")
[[ $(json_get 'd["uri_allow_list"]' <<<"$GOT") == "https://new.example.com/**" ]] || fail "GET config/auth does not return what was saved"
[[ $(json_get 'd["external_github_secret"]' <<<"$GOT") != client-secret ]] || fail "GET config/auth returned a secret in plaintext"
signup() { pcode POST /auth/v1/signup -H "apikey: $PUB" -H 'Content-Type: application/json' -d "{\"email\":\"$1\",\"password\":\"correct-horse-battery\"}"; }
[[ $(signup a@example.com) == 200 ]] || fail "sign-up with sign-ups on"
must 200 PATCH "$CFG/config/auth" '{"disable_signup":true}'
[[ $(signup b@example.com) == 422 ]] || fail "a disabled sign-up was not refused"
must 400 PATCH "$CFG/config/auth" '{"hook_send_email_uri":"http://example.com/hook"}'
must 400 PATCH "$CFG/config/auth" '{"jwt_exp":99999999}'

log "auth: SMTP and a custom recovery template reach the mail server"
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
sleep 1
must 200 PATCH "$CFG/config/auth" "{\"disable_signup\":false,\"smtp_host\":\"127.0.0.1\",\"smtp_port\":\"$P_SMTP\",\"smtp_admin_email\":\"no-reply@example.com\",\"smtp_sender_name\":\"Acme\",\"mailer_subjects_recovery\":\"Acme password reset\",\"mailer_templates_recovery_content\":\"<h2>Reset for {{ .Email }}</h2>\"}"
ok=0
for ((i = 0; i < 30; i++)); do
  pcode POST /auth/v1/recover -H "apikey: $PUB" -H 'Content-Type: application/json' -d '{"email":"a@example.com"}' >/dev/null
  if [[ -s $WORK/mails.txt ]]; then ok=1; break; fi
  sleep 2
done
[[ $ok -eq 1 ]] || fail "no mail reached the SMTP server"
FLAT=$(sed -e ':a;N;$!ba;s/=\r\?\n//g' -e 's/=3D/=/g' "$WORK/mails.txt")
grep -q 'Reset for a@example.com' <<<"$FLAT" || fail "the mail does not carry the saved template"
grep -q 'Subject: Acme password reset' <<<"$FLAT" || fail "the mail does not carry the saved subject"
# The template is served to the loopback only.
[[ $(http_code "$ADMIN/internal/templates/$REF/recovery") == 200 ]] || fail "the daemon does not serve the template to the loopback"
[[ $(http_code -H "X-Forwarded-For: 203.0.113.9" "$ADMIN/internal/templates/$REF/recovery") == 404 ]] || fail "the template route answered a proxied request"
[[ $(http_code -H "Host: api.$SBCTL_DOMAIN" "http://127.0.0.1/internal/templates/$REF/recovery") == 404 ]] || fail "the template is reachable through the public proxy"
must 200 PATCH "$CFG/config/auth" '{"disable_signup":true}'

log "postgrest: a newly exposed schema"
sql "create schema if not exists api_extra; create table if not exists api_extra.items (id int primary key); insert into api_extra.items values (1) on conflict do nothing; grant usage on schema api_extra to anon; grant select on api_extra.items to anon;" >/dev/null
[[ $(pcode GET /rest/v1/items -H "apikey: $PUB" -H 'Accept-Profile: api_extra') != 200 ]] || fail "an unexposed schema is served"
must 200 PATCH "$CFG/postgrest" '{"db_schema":"public, graphql_public, api_extra","max_rows":7}'
ok=0
for ((i = 0; i < 20; i++)); do
  [[ $(pcode GET /rest/v1/items -H "apikey: $PUB" -H 'Accept-Profile: api_extra') == 200 ]] && { ok=1; break; }
  sleep 1
done
[[ $ok -eq 1 ]] || fail "PostgREST does not serve the newly exposed schema"
[[ $(api GET "$CFG/postgrest" | json_get 'd["max_rows"]') == 7 ]] || fail "GET postgrest does not return max_rows"
must 400 PATCH "$CFG/postgrest" '{"db_schema":""}'

log "storage: a bigger upload"
head -c 2097152 /dev/urandom >"$WORK/2m.bin"
upload() { pcode POST "/storage/v1/object/files/$1" -H "apikey: $SEC" -H "Authorization: Bearer $SEC" -H 'Content-Type: application/octet-stream' --data-binary "@$WORK/2m.bin"; }
[[ $(pcode POST /storage/v1/bucket -H "apikey: $SEC" -H "Authorization: Bearer $SEC" -H 'Content-Type: application/json' -d '{"id":"files","name":"files","public":false}') == 200 ]] || fail "create bucket"
must 200 PATCH "$CFG/config/storage" '{"fileSizeLimit":1048576}'
ok=0
for ((i = 0; i < 20; i++)); do
  c=$(upload big1.bin)
  [[ $c == 413 || $c == 400 ]] && { ok=1; break; }
  sleep 1
done
[[ $ok -eq 1 ]] || fail "the upload limit is not enforced (last status $c)"
must 200 PATCH "$CFG/config/storage" '{"fileSizeLimit":10485760}'
ok=0
for ((i = 0; i < 20; i++)); do
  [[ $(upload big2.bin) == 200 ]] && { ok=1; break; }
  sleep 1
done
[[ $ok -eq 1 ]] || fail "the larger upload is not accepted"

log "realtime: tenant limits and private channels only"
ws_join "$P_REALTIME" "$REF.realtime.internal" "$ANON" || fail "realtime join before the change"
RT=$(api PATCH "$CFG/config/realtime" -o /dev/null -w '%{http_code}' -d '{"max_concurrent_users":123,"private_only":true}')
[[ $RT == 204 ]] || fail "PATCH config/realtime answered $RT"
ROW=$(sysql _realtime "select max_concurrent_users || ',' || private_only::text from _realtime.tenants where external_id = '$REF'" 2>&1 || true)
[[ $ROW == "123,true" ]] || fail "the Realtime tenant row did not take the settings (got '$ROW')"
if ws_join "$P_REALTIME" "$REF.realtime.internal" "$ANON" >/dev/null 2>&1; then fail "a public channel was joined with private_only on"; fi
[[ $(api GET "$CFG/config/realtime" | json_get 'd["private_only"]') == True ]] || fail "GET config/realtime does not return private_only"
api PATCH "$CFG/config/realtime" -o /dev/null -d '{"private_only":false}'
ws_join "$P_REALTIME" "$REF.realtime.internal" "$ANON" || fail "realtime join after private_only was switched off"

log "postgres: ALTER SYSTEM settings, an unsafe value, a setting that needs a restart"
must 200 PUT "$CFG/config/database/postgres" '{"statement_timeout":"45s","work_mem":"8MB"}'
[[ $(sql "show statement_timeout") == 45s && $(sql "show work_mem") == 8MB ]] || fail "the cluster did not take the settings"
must 400 PUT "$CFG/config/database/postgres" '{"max_connections":3}'
must 400 PUT "$CFG/config/database/postgres" '{"work_mem":"16"}'
BEFORE=$(systemctl show -p MainPID --value "sb-postgres@$REF.service")
must 200 PUT "$CFG/config/database/postgres" '{"max_connections":40,"restart_database":true}'
for ((i = 0; i < 60; i++)); do
  [[ $(sql "show max_connections" 2>/dev/null || true) == 40 ]] && break
  sleep 1
done
[[ $(sql "show max_connections") == 40 ]] || fail "max_connections was not applied by the restart"
[[ $(systemctl show -p MainPID --value "sb-postgres@$REF.service") != "$BEFORE" ]] || fail "the cluster was not restarted"
[[ $(sql "show statement_timeout") == 45s ]] || fail "statement_timeout was lost by the restart"
for ((i = 0; i < 60; i++)); do
  [[ $(pcode GET /rest/v1/ -H "apikey: $SEC") == 200 && $(pcode GET /auth/v1/settings -H "apikey: $PUB") == 200 ]] && break
  sleep 1
done
[[ $(pcode GET /rest/v1/ -H "apikey: $SEC") == 200 ]] || fail "PostgREST did not recover after the database restart"
[[ $(api GET "$CFG/config/database/postgres" | json_get 'd["max_connections"]') == 40 ]] || fail "GET returns another max_connections"

log "api keys: a revoked secret key and the legacy switch"
NEW=$(api POST "$CFG/api-keys" -d '{"type":"secret","name":"ci"}')
KEY=$(json_get 'd["api_key"]' <<<"$NEW")
KID=$(json_get 'd["id"]' <<<"$NEW")
[[ $KEY == sb_secret_* ]] || fail "created key '$KEY'"
[[ $(pcode GET /rest/v1/ -H "apikey: $KEY") == 200 ]] || fail "the new secret key is refused"
must 200 DELETE "$CFG/api-keys/$KID"
[[ $(pcode GET /rest/v1/ -H "apikey: $KEY") == 401 ]] || fail "a revoked key is still accepted"
[[ $(pcode GET /rest/v1/ -H "apikey: $SEC") == 200 ]] || fail "revoking one key took the default secret key down"
must 200 PUT "$CFG/api-keys/legacy?enabled=false"
[[ $(pcode GET /rest/v1/ -H "apikey: $SVC") == 401 ]] || fail "a legacy key is accepted while disabled"
[[ $(pcode GET /auth/v1/settings -H "apikey: $ANON") == 401 ]] || fail "the legacy anon key is accepted while disabled"
[[ $(pcode GET /rest/v1/ -H "apikey: $SEC") == 200 ]] || fail "the secret key stopped working with the legacy keys off"
must 200 PUT "$CFG/api-keys/legacy?enabled=true"
[[ $(pcode GET /rest/v1/ -H "apikey: $SVC") == 200 ]] || fail "the legacy keys did not come back"

log "database password reset"
NEWPW='a-brand-new-password-42'
pooler_login "$DBPW" || fail "pooler login with the original password"
must 200 PATCH "$CFG/database/password" "{\"password\":\"$NEWPW\"}"
direct_login "$NEWPW" || fail "the new password does not work directly"
if direct_login "$DBPW"; then fail "the old password still works directly"; fi
pooler_login "$NEWPW" || fail "the new password does not work through the pooler"
if pooler_login "$DBPW"; then fail "the old password still works through the pooler"; fi
[[ $(pcode GET /rest/v1/ -H "apikey: $SEC") == 200 ]] || fail "PostgREST after the password change"
[[ $(pcode GET /auth/v1/settings -H "apikey: $PUB") == 200 ]] || fail "GoTrue after the password change"
must 201 POST "$CFG/database/query" '{"query":"select 1"}' # the API connects with the new password

log "persistence: pause and resume keep every saved setting"
must 200 POST "$CFG/pause"
for ((i = 0; i < 60; i++)); do
  [[ $(api GET "$CFG" | json_get 'd["status"]') == INACTIVE ]] && break
  sleep 2
done
# Settings can be saved while the project is paused and apply when it resumes.
must 200 PATCH "$CFG/postgrest" '{"max_rows":9}'
must 200 POST "$CFG/restore"
for ((i = 0; i < 90; i++)); do
  [[ $(api GET "$CFG" | json_get 'd["status"]') == ACTIVE_HEALTHY ]] && break
  sleep 2
done
[[ $(api GET "$CFG" | json_get 'd["status"]') == ACTIVE_HEALTHY ]] || fail "the project did not come back"
[[ $(signup c@example.com) == 422 ]] || fail "disable_signup was lost by the pause"
[[ $(location https%3A%2F%2Fnew.example.com%2Fcb) == https://new.example.com/cb* ]] || fail "the redirect allow list was lost by the pause"
[[ $(pcode GET /rest/v1/items -H "apikey: $PUB" -H 'Accept-Profile: api_extra') == 200 ]] || fail "the exposed schema was lost by the pause"
[[ $(sql "show statement_timeout") == 45s && $(sql "show max_connections") == 40 ]] || fail "the Postgres settings were lost by the pause"
[[ $(api GET "$CFG/postgrest" | json_get 'd["max_rows"]') == 9 ]] || fail "a setting saved while paused was not kept"
[[ $(pcode GET /rest/v1/ -H "apikey: $KEY") == 401 ]] || fail "a revoked key came back after the pause"

log "settings smoke test passed"
