#!/usr/bin/env bash
# Single sign-on under real systemd units, against a real SAML 2.0 identity provider.
#
#   sudo env "PATH=$PATH" SBCTL_BIN=/path/to/sbctl-linux-amd64 [SUPABASE_CLI=/path/to/supabase] \
#     tests/linux/sso-smoke.sh [--teardown]
#
# The identity provider is SimpleSAMLphp in a container (kristophjunge/test-saml-idp, which
# the runner pulls and starts, amd64 only), with a few users and the node's service providers
# (the dashboard's and one project's) as its known SPs. "A person" is tests/linux/sso/saml_walk.py:
# what a browser does at "Continue with SSO", through the node's own proxy.
#
# - the dashboard: `sbctl sso add` registers the provider from its metadata URL; a person of the
#   allowed domain signs in and is a Developer (the domain's default role) and may do what a
#   Developer may; a person the identity provider vouches for with an address of any other domain is
#   refused on every route, listed by `sbctl sso pending` and let in by `sbctl sso approve`; a
#   domain without a provider cannot start a sign-in; nobody can sign up any other way (GoTrue's
#   sign-up is open and the daemon's hook refuses it); Studio's unit offers "Continue with SSO"
#   only while a provider exists; removing the provider ends the session;
# - a project: SAML is off until it is enabled in the project's Auth settings (the specs' 404),
#   then the real Supabase CLI adds, lists, shows, updates and removes an identity provider, and
#   an end user of the project signs in through it.
#
# Without SBCTL_BIN the script builds sbctl with the go toolchain. It needs network access for
# the artifact downloads and the container image. Not run in development (root, systemd, docker
# and Linux required); CI runs it on an ephemeral Ubuntu 24.04 VM.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
WORK=$(mktemp -d)
IDP_NAME=sbctl-saml-idp
P_IDP=8080
P_STUDIO=13000
ADMIN=http://127.0.0.1:7000
ORG=default   # the claim keeps the organization the first project created
trap 'rc=$?; docker logs "$IDP_NAME" >"$LOG_DIR/idp.log" 2>&1; docker rm -f "$IDP_NAME" >/dev/null 2>&1; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; rm -rf "$WORK"; exit $rc' EXIT

command -v docker >/dev/null || fail "docker is needed for the identity provider"
mkdir -p "$LOG_DIR"
preflight
install_binary
setup_node
cat >>"$SBCTL_CONF" <<CONF

[ports]
studio = $P_STUDIO
CONF

# Same VM-local workaround as settings-smoke.sh: the Postgres launcher writes into the artifact
# directory on its first boot, which ProtectSystem=strict forbids.
install -d /etc/systemd/system/sb-postgres@.service.d
cat >/etc/systemd/system/sb-postgres@.service.d/10-sso-smoke.conf <<'CONF'
[Service]
ReadWritePaths=/var/lib/sbctl/artifacts
CONF
systemctl daemon-reload

log "system init, one project"
system_init
wait_active sb-postgres@system.service 30
wait_active sb-gotrue@system.service 30
REF=$(create_project shop micro)
[[ $REF =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF'"
log "the system GoTrue runs SAML with a sealed key and asks the daemon about every sign-up"
ENVF="$SBCTL_STATE/projects/system/gotrue.env"
for k in GOTRUE_SAML_ENABLED GOTRUE_SAML_PRIVATE_KEY GOTRUE_HOOK_BEFORE_USER_CREATED_ENABLED GOTRUE_HOOK_BEFORE_USER_CREATED_URI; do
  sudo grep -q "^$k=" "$ENVF" || fail "$ENVF has no $k"
done
[[ $(sudo stat -c %a "$ENVF") == 600 ]] || fail "the dashboard's GoTrue environment is not private: $(sudo stat -c %a "$ENVF")"

# A stand-in for Studio: the unit is rendered like the real one (its environment decides whether
# the sign-in page offers SSO) and answers the health check.
STAG=$(awk '/^studio:/{f=1;next} f&&/^ *tag:/{print $2;exit}' "$REPO_ROOT/versions.yaml")
SDIR="$SBCTL_STATE/artifacts/studio/$STAG"
install -d -o "$SBCTL_USER" -g "$SBCTL_USER" "$SDIR/bin"
cat >"$SDIR/bin/studio" <<'STUB'
#!/bin/sh
exec python3 -c "
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b'ok')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', int(os.environ['PORT'])), H).serve_forever()
"
STUB
chmod 0755 "$SDIR/bin/studio"; chown "$SBCTL_USER:$SBCTL_USER" "$SDIR/bin/studio"
sbctl fleet start --no-fetch --skip pgmeta,supavisor,realtime,storage || fail "fleet start (Studio only)"
STUDIO_ENV="$SBCTL_STATE/projects/system/studio.env"
sudo test -f "$STUDIO_ENV" || fail "no Studio environment at $STUDIO_ENV"
! sudo grep -q '^NEXT_PUBLIC_DISABLED_FEATURES=' "$STUDIO_ENV" || fail "Studio is configured before any provider exists"

log "daemon: sbctl.service"
systemctl start sbctl.service
for ((i = 0; i < 60; i++)); do
  [[ $(http_code "$ADMIN/v1/projects") == 401 ]] && break
  sleep 1
done
[[ $(http_code "$ADMIN/v1/projects") == 401 ]] || { journalctl --no-pager -u sbctl.service | tail -30 >&2; fail "the Management API does not answer on the admin listener"; }

claim_and_token   # sets PAT and ORG (the first project's organization)
[[ $ORG == default ]] || fail "the claim made organization '$ORG', want 'default'"
OWNER_JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' -d '{"email":"smoke@example.com","password":"smoke-correct-horse-battery"}' | json_get 'd["access_token"]') || fail "owner sign-in"
dash_code() { curl -s -o /dev/null -w '%{http_code}' -m 30 "${@:2}" "$ADMIN$1" || true; }  # PATH [curl args]

log "nobody can sign up on the dashboard: the hook refuses (GoTrue's own switch is off for SSO)"
SIGNUP=$(api POST /auth/v1/signup -H 'Content-Type: application/json' -d '{"email":"intruder@acme.test","password":"intruder-password-1"}' -w '\n%{http_code}')
[[ ${SIGNUP##*$'\n'} =~ ^4 ]] || fail "a public sign-up answered ${SIGNUP##*$'\n'}: ${SIGNUP%$'\n'*}"
grep -q "Sign-up is closed" <<<"$SIGNUP" || fail "the sign-up was not refused by the hook: $SIGNUP"
OTP=$(api POST /auth/v1/otp -H 'Content-Type: application/json' -d '{"email":"intruder@acme.test","create_user":true}' -o /dev/null -w '%{http_code}')
[[ $OTP =~ ^4 ]] || fail "a sign-up by one-time code answered $OTP"
! sbctl users list | grep -q intruder || fail "a refused sign-up left an account"
# Only the daemon's own loopback caller gets an answer from the hook route.
[[ $(http_code -X POST -H 'Content-Type: application/json' -d '{}' "$ADMIN/internal/hooks/before-user-created") == 401 ]] || fail "the hook route answered an unsigned call"
[[ $(http_code -X POST -H "Host: api.$SBCTL_DOMAIN" -d '{}' "http://127.0.0.1/internal/hooks/before-user-created") != 200 ]] || fail "the hook route is reachable through the public proxy"

log "the identity provider (SimpleSAMLphp in a container)"
SPS="$WORK/saml20-sp-remote.php"
{
  echo '<?php'
  for h in "api.$SBCTL_DOMAIN" "$REF.api.$SBCTL_DOMAIN"; do
    cat <<PHP
\$metadata['http://$h/auth/v1/sso/saml/metadata'] = array(
    'AssertionConsumerService' => 'http://$h/auth/v1/sso/saml/acs',
    'simplesaml.nameidattribute' => 'uid',
    'NameIDFormat' => 'urn:oasis:names:tc:SAML:2.0:nameid-format:persistent',
    'saml20.sign.response' => true,
    'saml20.sign.assertion' => true,
);
PHP
  done
} >"$SPS"
cat >"$WORK/authsources.php" <<'PHP'
<?php
$config = array(
    'admin' => array('core:AdminPassword'),
    'example-userpass' => array(
        'exampleauth:UserPass',
        'alice:alicepass' => array('uid' => array('alice'), 'email' => array('alice@acme.test')),
        'bob:bobpass' => array('uid' => array('bob'), 'email' => array('bob@contractor.test')),
        'carol:carolpass' => array('uid' => array('carol'), 'email' => array('carol@shop.test')),
    ),
);
PHP
chmod 0644 "$SPS" "$WORK/authsources.php"
docker rm -f "$IDP_NAME" >/dev/null 2>&1 || true
docker run -d --name "$IDP_NAME" -p "127.0.0.1:$P_IDP:8080" \
  -v "$WORK/authsources.php:/var/www/simplesamlphp/config/authsources.php" \
  -v "$SPS:/var/www/simplesamlphp/metadata/saml20-sp-remote.php" \
  kristophjunge/test-saml-idp >/dev/null || fail "the identity provider container did not start"
IDP_META="http://127.0.0.1:$P_IDP/simplesaml/saml2/idp/metadata.php"
for ((i = 0; i < 60; i++)); do
  [[ $(http_code "$IDP_META") == 200 ]] && break
  sleep 1
done
[[ $(http_code "$IDP_META") == 200 ]] || { docker logs "$IDP_NAME" >&2; fail "the identity provider does not answer at $IDP_META"; }
curl -fsS "$IDP_META" >"$WORK/idp-metadata.xml"
log "IdP entity: $(grep -o 'entityID="[^"]*"' "$WORK/idp-metadata.xml" | head -1)"

walk() { # API_HOST EMAIL USER PASSWORD [extra args]: the JSON of a sign-in walk
  local host=$1 email=$2 user=$3 pw=$4; shift 4
  python3 "$REPO_ROOT/tests/linux/sso/saml_walk.py" --api-base http://127.0.0.1 --api-host "$host" --email "$email" --user "$user" --password "$pw" "$@"
}
walk_token() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("access_token",""))'; }
profile_code() { dash_code /platform/profile -H "Authorization: Bearer $1"; }

log "dashboard: a domain without a provider cannot start a sign-in"
[[ $(api POST /auth/v1/sso -H 'Content-Type: application/json' -d '{"domain":"acme.test","skip_http_redirect":true}' -o /dev/null -w '%{http_code}') == 404 ]] || fail "SSO for a domain without a provider did not answer 404"

log "dashboard: sbctl sso add from the identity provider's metadata URL"
sbctl sso add --metadata-url "$IDP_META" --domain acme.test --default-role developer >"$WORK/add.out" 2>"$WORK/add.err" || { cat "$WORK/add.err" >&2; fail "sbctl sso add"; }
PROV=$(head -1 "$WORK/add.out")
[[ $PROV =~ ^[0-9a-f-]{36}$ ]] || fail "sbctl sso add printed '$PROV'"
grep -q "ACS URL.*http://api.$SBCTL_DOMAIN/auth/v1/sso/saml/acs" "$WORK/add.err" || { cat "$WORK/add.err" >&2; fail "sbctl sso add does not say where the identity provider posts the assertion"; }
sbctl sso list >"$WORK/list.txt"
grep -q "$PROV.*$ORG.*acme.test.*developer" "$WORK/list.txt" || { cat "$WORK/list.txt" >&2; fail "sbctl sso list"; }
[[ $(sbctl sso list --json | json_get 'len(d)') == 1 ]] || fail "sbctl sso list --json"
log "Studio's sign-in page offers SSO now (the unit was rendered again)"
sudo grep -q '^NEXT_PUBLIC_DISABLED_FEATURES=' "$STUDIO_ENV" || fail "Studio was not re-rendered after the first provider"
! sudo grep '^NEXT_PUBLIC_DISABLED_FEATURES=' "$STUDIO_ENV" | grep -q sign_in_with_sso || fail "Studio still hides the SSO sign-in: $(sudo grep '^NEXT_PUBLIC_DISABLED_FEATURES=' "$STUDIO_ENV")"
sudo grep '^NEXT_PUBLIC_DISABLED_FEATURES=' "$STUDIO_ENV" | grep -q dashboard_auth:sign_up || fail "Studio lost its other disabled features"

log "dashboard: SP metadata through the proxy"
META=$(api GET /auth/v1/sso/saml/metadata)
grep -q "entityID=\"http://api.$SBCTL_DOMAIN/auth/v1/sso/saml/metadata\"" <<<"$META" || fail "the dashboard's SP metadata has another entity id: ${META:0:300}"

log "dashboard: alice (acme.test) signs in through the identity provider and is a Developer"
walk "api.$SBCTL_DOMAIN" alice@acme.test alice alicepass --redirect-to "http://studio.$SBCTL_DOMAIN/sign-in-mfa?method=sso" >"$WORK/alice.json" \
  || { cat "$WORK/alice.json" >&2; docker logs "$IDP_NAME" 2>&1 | tail -20 >&2; fail "alice's sign-in did not reach the ACS"; }
ALICE=$(walk_token <"$WORK/alice.json")
[[ -n $ALICE ]] || { sed -e 's/access_token": "[^"]*/access_token": "…/' "$WORK/alice.json" >&2; journalctl --no-pager -u sb-gotrue@system -n 20 >&2; fail "no session came back for alice"; }
[[ $(python3 -c 'import json,sys; print(json.load(sys.stdin)["location"].split("#")[0])' <"$WORK/alice.json") == "http://studio.$SBCTL_DOMAIN/sign-in-mfa?method=sso" ]] || fail "alice is sent to $(python3 -c 'import json,sys; print(json.load(sys.stdin)["location"].split("#")[0])' <"$WORK/alice.json")"
[[ $(profile_code "$ALICE") == 200 ]] || fail "alice's first request: $(profile_code "$ALICE")"
sbctl users list >"$WORK/users.txt"
grep -q "alice@acme.test.*$ORG:developer" "$WORK/users.txt" || { cat "$WORK/users.txt" >&2; fail "alice is not a Developer"; }
[[ $(dash_code "/v1/projects/$REF/config/auth" -X PATCH -H "Authorization: Bearer $ALICE" -H 'Content-Type: application/json' -d '{"site_url":"https://x.test"}') == 403 ]] || fail "a Developer saved Auth settings"
[[ $(dash_code "/v1/projects/$REF/api-keys?reveal=true" -H "Authorization: Bearer $ALICE") == 200 ]] || fail "a Developer cannot read the project's keys"
# An SSO session is no password session: the personal access token alice makes carries her role.
APAT=$(curl -sS -m 30 -X POST -H "Authorization: Bearer $ALICE" -H 'Content-Type: application/json' -d '{"name":"alice"}' "$ADMIN/platform/profile/access-tokens" | json_get 'd["token"]') || fail "alice's token"
[[ $(dash_code "/v1/projects/$REF/secrets" -X POST -H "Authorization: Bearer $APAT" -H 'Content-Type: application/json' -d '[{"name":"A","value":"b"}]') == 403 ]] || fail "alice's token wrote secrets"

log "dashboard: an address the provider vouches for, of a domain nobody mapped, waits for approval"
walk "api.$SBCTL_DOMAIN" alice@acme.test bob bobpass >"$WORK/bob.json" || { cat "$WORK/bob.json" >&2; fail "bob's sign-in did not reach the ACS"; }
BOB=$(walk_token <"$WORK/bob.json")
[[ -n $BOB ]] || fail "no session came back for bob: $(cat "$WORK/bob.json" | head -c 600)"
for p in /platform/profile /platform/projects /v1/projects /platform/organizations; do
  [[ $(dash_code "$p" -H "Authorization: Bearer $BOB") == 403 ]] || fail "a user waiting for approval got $(dash_code "$p" -H "Authorization: Bearer $BOB") on $p"
done
sbctl sso pending >"$WORK/pending.txt"
grep -q "bob@contractor.test" "$WORK/pending.txt" || { cat "$WORK/pending.txt" >&2; fail "bob is not listed as pending"; }
sbctl sso approve bob@contractor.test --role read-only >/dev/null || fail "sbctl sso approve"
[[ $(profile_code "$BOB") == 200 ]] || fail "bob after the approval: $(profile_code "$BOB")"
[[ $(dash_code "/v1/projects/$REF/secrets" -X POST -H "Authorization: Bearer $BOB" -H 'Content-Type: application/json' -d '[{"name":"A","value":"b"}]') == 403 ]] || fail "a Read-only member wrote secrets"
[[ $(dash_code "/v1/projects/$REF/api-keys?reveal=true" -H "Authorization: Bearer $BOB") == 403 ]] || fail "a Read-only member revealed the project's keys"
sbctl users list | grep -q "bob@contractor.test.*$ORG:read-only" || fail "bob is not Read-only"

log "dashboard: the Management API answers the same (Studio's organization page and sbctl's routes)"
code=$(dash_code "/platform/organizations/$ORG/sso" -H "Authorization: Bearer $OWNER_JWT"); [[ $code == 200 ]] || fail "GET organization SSO: $code"
[[ $(curl -sS -m 30 -H "Authorization: Bearer $OWNER_JWT" "$ADMIN/platform/organizations/$ORG/sso" | json_get 'd["join_org_on_signup_role"]') == Developer ]] || fail "the organization page shows another default role"
[[ $(dash_code "/platform/organizations/$ORG/sso/providers" -H "Authorization: Bearer $ALICE") == 403 ]] || fail "a Developer reads the SSO providers"

log "dashboard: removing the provider ends the session; Studio loses the button"
sbctl sso remove acme.test >/dev/null || fail "sbctl sso remove"
! sudo grep -q '^NEXT_PUBLIC_DISABLED_FEATURES=' "$STUDIO_ENV" || fail "Studio still offers SSO without a provider"
[[ $(sbctl sso list --json | json_get 'len(d)') == 0 ]] || fail "the provider is still listed"
for ((i = 0; i < 20; i++)); do
  [[ $(profile_code "$ALICE") == 403 ]] && break
  sleep 1
done
[[ $(profile_code "$ALICE") == 403 ]] || fail "alice's session outlived the provider: $(profile_code "$ALICE")"
[[ $(dash_code "/v1/projects" -H "Authorization: Bearer $APAT") == 401 ]] || fail "alice's token outlived the provider"

log "project: SAML is off until it is enabled in the Auth settings"
PROFILE="$WORK/profile.yaml"
cat >"$PROFILE" <<YAML
name: sbctl-test
api_url: $ADMIN
dashboard_url: http://127.0.0.1:1
project_host: api.$SBCTL_DOMAIN
pooler_host: pooler.$SBCTL_DOMAIN
YAML
CLI=${SUPABASE_CLI:-supabase}
command -v "$CLI" >/dev/null || fail "the Supabase CLI is needed (SUPABASE_CLI)"
export SUPABASE_ACCESS_TOKEN=$PAT SUPABASE_NO_KEYRING=1 DO_NOT_TRACK=1
sb() { (cd "$WORK" && "$CLI" --profile="$PROFILE" "$@" 2>&1); }
OUT=$(sb sso add --project-ref "$REF" --type saml --metadata-file "$WORK/idp-metadata.xml" --domains shop.test || true)
grep -q "SAML 2.0 support is not enabled" <<<"$OUT" || fail "supabase sso add with SAML off: $OUT"
[[ $(papi GET "/v1/projects/$REF/config/auth/sso/providers" -o /dev/null -w '%{http_code}') == 404 ]] || fail "the project's providers answer without SAML"
[[ $(papi PATCH "/v1/projects/$REF/config/auth" -H 'Content-Type: application/json' -d '{"saml_enabled":true}' -o /dev/null -w '%{http_code}') == 200 ]] || fail "enabling SAML for the project"
PENV="$SBCTL_STATE/projects/$REF/gotrue.env"
sudo grep -q '^GOTRUE_SAML_ENABLED="\?true' "$PENV" || fail "the project's GoTrue is not told to run SAML"
sudo grep -q '^GOTRUE_SAML_PRIVATE_KEY=' "$PENV" || fail "the project's GoTrue has no signing key"
[[ $(sudo grep '^GOTRUE_SAML_PRIVATE_KEY=' "$PENV") != $(sudo grep '^GOTRUE_SAML_PRIVATE_KEY=' "$ENVF") ]] || fail "the project signs with the dashboard's key"
PHOST="$REF.api.$SBCTL_DOMAIN"
project_keys "$REF"   # PUB, SEC
for ((i = 0; i < 30; i++)); do
  [[ $(curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Host: $PHOST" "http://127.0.0.1/auth/v1/sso/saml/metadata") == 200 ]] && break
  sleep 1
done
PMETA=$(curl -sS -m 10 -H "Host: $PHOST" "http://127.0.0.1/auth/v1/sso/saml/metadata")
grep -q "entityID=\"http://$PHOST/auth/v1/sso/saml/metadata\"" <<<"$PMETA" || fail "the project's SP metadata: ${PMETA:0:300}"

log "project: the Supabase CLI adds, lists, shows, updates and removes an identity provider"
echo '{"keys":{"email":{"name":"email"}}}' >"$WORK/mapping.json"
sb sso add --project-ref "$REF" --type saml --metadata-file "$WORK/idp-metadata.xml" --domains shop.test \
  --attribute-mapping-file "$WORK/mapping.json" -o json >"$WORK/cli-add.json" || { cat "$WORK/cli-add.json" >&2; fail "supabase sso add"; }
PPROV=$(grep -Eo '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' "$WORK/cli-add.json" | head -1)
[[ -n $PPROV ]] || { cat "$WORK/cli-add.json" >&2; fail "supabase sso add printed no provider id"; }
sb sso list --project-ref "$REF" -o json | grep -q "$PPROV" || fail "supabase sso list does not show $PPROV"
sb sso show "$PPROV" --project-ref "$REF" -o json | grep -q '"shop.test"' || fail "supabase sso show"
sb sso update "$PPROV" --project-ref "$REF" --domains shop.test,labs.shop.test >/dev/null || fail "supabase sso update"
sb sso show "$PPROV" --project-ref "$REF" -o json | grep -q '"labs.shop.test"' || fail "supabase sso update did not add the domain"

log "project: an end user of the project signs in through the project's provider"
walk "$PHOST" carol@shop.test carol carolpass --apikey "$PUB" >"$WORK/carol.json" || { cat "$WORK/carol.json" >&2; fail "carol's sign-in did not reach the ACS"; }
CAROL=$(walk_token <"$WORK/carol.json")
[[ -n $CAROL ]] || fail "no session came back for the project's end user: $(head -c 600 "$WORK/carol.json")"
CU=$(curl -sS -m 30 -H "Host: $PHOST" -H "apikey: $PUB" -H "Authorization: Bearer $CAROL" "http://127.0.0.1/auth/v1/user")
[[ $(json_get 'd["email"]' <<<"$CU") == carol@shop.test ]] || fail "the project's GoTrue does not know carol: $CU"
[[ $(json_get 'd["app_metadata"]["provider"]' <<<"$CU") == "sso:$PPROV" ]] || fail "carol came from another provider: $CU"
# A project's end user is nobody on the dashboard.
[[ $(profile_code "$CAROL") == 401 ]] || fail "a project's end user on the dashboard API: $(profile_code "$CAROL")"

sb sso remove "$PPROV" --project-ref "$REF" >/dev/null || fail "supabase sso remove"
! sb sso list --project-ref "$REF" -o json | grep -q "$PPROV" || fail "the project's provider is still listed"

log "sso smoke passed"
