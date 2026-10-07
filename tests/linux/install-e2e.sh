#!/usr/bin/env bash
# install end to end: deploy/install.sh on a fresh Linux VM, then the dashboard claim flow, a
# project through the Management API with a personal access token, a REST call through the
# proxy, the pooler, a re-run of the installer and `sbctl self-update`.
#
#   sudo SBCTL_BIN=/path/to/sbctl tests/linux/install-e2e.sh
#
# SBCTL_BIN is a Linux build of this checkout with -X main.version=v0.0.1, SBCTL_BIN_V2 one
# with v0.0.2 (the installer upgrade target) and SBCTL_BIN_V3 one with v0.0.3 (the self-update
# target); without them the script builds them with go. Needs
# root, systemd, cgroup v2 and network access (artifact downloads). The node is configured with `--tls off --public-ip 127.0.0.1`, so every name is
# <something>.127.0.0.1.sslip.io and the script reaches the proxy on 127.0.0.1 with Host
# headers; nothing needs DNS. E2E_STUDIO=0 skips the dashboard build (a stand-in: the slim
# Studio artifact of the pinned upstream version, not our platform build).
#
# Do not run it on a machine you care about: it creates the sbctl user, writes /etc/sbctl,
# installs units and starts real clusters. Exit status is non-zero on the first failure.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

E2E_STUDIO=${E2E_STUDIO:-1}
WORK=$(mktemp -d)
BASE=127.0.0.1.sslip.io
SRV_PORT=38800
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=e2e-correct-horse-battery
SRV_PID=""

cleanup() {
  local rc=$?
  [[ -n $SRV_PID ]] && kill "$SRV_PID" 2>/dev/null || true
  collect_logs
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

need_root
preflight
ARCH=$(dpkg --print-architecture)
cd "$REPO_ROOT"

if [[ -z $SBCTL_BIN ]]; then
  command -v go >/dev/null || fail "no SBCTL_BIN and no go toolchain"
  SBCTL_BIN=$WORK/sbctl
  CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.1" -o "$SBCTL_BIN" ./cmd/sbctl
fi
[[ $("$SBCTL_BIN" --version) == *v0.0.1* ]] || fail "SBCTL_BIN must be built with -X main.version=v0.0.1 (the self-update step updates from it): $("$SBCTL_BIN" --version)"

# curl helpers: the proxy on :80 with a Host header.
api() { # METHOD PATH [curl args...]  (Host api.<base>)
  local m=$1 p=$2; shift 2
  curl -sS -m 60 -X "$m" -H "Host: api.$BASE" "$@" "http://127.0.0.1$p"
}
code() { # curl args... URL -> status code
  curl -s -o /dev/null -w '%{http_code}' -m 30 "$@" || true
}
jq_() { python3 -c 'import json,sys; d=json.load(sys.stdin); print('"$1"')'; }

# ---- 1. static checks ------------------------------------------------------------------
log "static checks"
bash -n deploy/install.sh deploy/release-assets.sh tests/linux/install-e2e.sh
if command -v shellcheck >/dev/null; then
  shellcheck -S warning deploy/install.sh deploy/release-assets.sh
else
  log "shellcheck is not installed; skipped"
fi
"$SBCTL_BIN" install --help >/dev/null
"$SBCTL_BIN" self-update --help >/dev/null

# ---- 2. the release path of install.sh: signature and checksum checks --------------------
log "release assets signed with a throwaway key (deploy/release-assets.sh)"
KEYS=$WORK/keys; mkdir -p "$KEYS"
openssl genpkey -algorithm ed25519 -out "$KEYS/sign.pem"
openssl pkey -in "$KEYS/sign.pem" -pubout -out "$KEYS/pub.pem"
openssl genpkey -algorithm ed25519 -out "$KEYS/other.pem"
openssl pkey -in "$KEYS/other.pem" -pubout -out "$KEYS/other.pub"

make_release() { # TAG BINARY-FILE [nolatest]: builds $WORK/srv/download/TAG and the fake GitHub API files for repo o/r
  local tag=$1 bin=$2 latest=${3:-latest} d=$WORK/srv/download/$1
  rm -rf "$d"; mkdir -p "$d"
  cp "$bin" "$d/sbctl-linux-$ARCH"
  if [[ $ARCH == amd64 ]]; then echo other-arch >"$d/sbctl-linux-arm64"; else echo other-arch >"$d/sbctl-linux-amd64"; fi
  echo "not a real studio build" >"$d/sbctl-studio-test-p1-linux-$ARCH.tar.zst"
  deploy/release-assets.sh "$d" "$KEYS/sign.pem" "$KEYS/pub.pem" >/dev/null
  mkdir -p "$WORK/srv/repos/o/r/releases/tags"
  python3 - "$tag" "$d" "$SRV_PORT" "$WORK/srv/repos/o/r/releases" "$latest" <<'PY'
import json, os, sys
tag, d, port, out, latest = sys.argv[1:]
rel = {"tag_name": tag, "assets": [{"name": n, "browser_download_url": f"http://127.0.0.1:{port}/download/{tag}/{n}"} for n in sorted(os.listdir(d))]}
for name in (("latest",) if latest == "latest" else ()) + (f"tags/{tag}",):
    with open(os.path.join(out, name), "w") as f:
        json.dump(rel, f)
PY
}
mkdir -p "$WORK/srv"
make_release v0.0.1 "$SBCTL_BIN"
(cd "$WORK/srv" && exec python3 -m http.server "$SRV_PORT" --bind 127.0.0.1 >"$WORK/http.log" 2>&1) &
SRV_PID=$!
for ((i = 0; i < 20; i++)); do [[ $(code "http://127.0.0.1:$SRV_PORT/download/v0.0.1/SHA256SUMS") == 200 ]] && break; sleep 0.5; done

installer() { # extra env is set by the caller
  SBCTL_INSTALL_BASE_URL="http://127.0.0.1:$SRV_PORT" SBCTL_INSTALL_PUBKEY_B64=$(base64 -w0 "$KEYS/pub.pem") "$@"
}
log "install.sh --verify-only accepts the signed release"
out=$(installer deploy/install.sh --version v0.0.1 --verify-only) || fail "verify-only failed on a good release: $out"
[[ $out == *"verified v0.0.1 sbctl-linux-$ARCH"* ]] || fail "unexpected verify-only output: $out"
[[ $out == *"dashboard build: sbctl-studio-test-p1-linux-$ARCH.tar.zst"* ]] || fail "install.sh did not pick the Studio asset out of the signed list: $out"

expect_refusal() { # DESCRIPTION EXPECTED-TEXT env... -- the command runs with the caller's mutation in place
  local what=$1 text=$2 out
  if out=$(installer deploy/install.sh --version v0.0.1 --verify-only 2>&1); then fail "install.sh accepted $what"; fi
  [[ $out == *"$text"* ]] || fail "install.sh refused $what with the wrong message: $out"
  [[ ! -e /usr/local/bin/sbctl ]] || fail "install.sh installed a binary despite refusing $what"
  log "refused $what"
}
D=$WORK/srv/download/v0.0.1
cp "$D/sbctl-linux-$ARCH" "$WORK/good-binary"
echo tampered >>"$D/sbctl-linux-$ARCH"
expect_refusal "a binary that does not match the signed checksum" "does not match its checksum"
cp "$WORK/good-binary" "$D/sbctl-linux-$ARCH"
cp "$D/SHA256SUMS" "$WORK/good-sums"
echo "0000000000000000000000000000000000000000000000000000000000000000  evil" >>"$D/SHA256SUMS"
expect_refusal "a checksum list changed after signing" "signature of SHA256SUMS does not verify"
cp "$WORK/good-sums" "$D/SHA256SUMS"
cp "$D/SHA256SUMS.sig" "$WORK/good-sig"
printf 'x%.0s' $(seq 1 64) >"$D/SHA256SUMS.sig"
expect_refusal "a bad signature" "does not verify"
cp "$WORK/good-sig" "$D/SHA256SUMS.sig"
if out=$(SBCTL_INSTALL_BASE_URL="http://127.0.0.1:$SRV_PORT" SBCTL_INSTALL_PUBKEY_B64=$(base64 -w0 "$KEYS/other.pub") deploy/install.sh --version v0.0.1 --verify-only 2>&1); then
  fail "install.sh accepted a release signed by another key"
fi
[[ $out == *"does not verify"* ]] || fail "wrong-key refusal message: $out"
if out=$(deploy/install.sh --version v0.0.1 --verify-only 2>&1); then fail "the repository copy of install.sh (no release key) downloaded a release"; fi
[[ $out == *"no release signing key"* ]] || fail "unstamped install.sh message: $out"
installer deploy/install.sh --version v0.0.1 --verify-only >/dev/null || fail "restored release no longer verifies"
# Through a pipe, the way the documented one-liner runs it.
out=$(cat deploy/install.sh | installer bash -s -- --version v0.0.1 --verify-only 2>&1) || fail "install.sh through a pipe: $out"
[[ $out == *"verified v0.0.1"* ]] || fail "install.sh through a pipe: $out"
# An older signed binary attached to a newer tag (the signature covers the checksums, not the tag).
make_release v0.0.9 "$SBCTL_BIN" nolatest
if out=$(installer deploy/install.sh --version v0.0.9 --verify-only 2>&1); then fail "install.sh accepted v0.0.1's binary under the tag v0.0.9"; fi
[[ $out == *"ships a binary that reports"* ]] || fail "install.sh downgrade refusal message: $out"
log "refused a signed older binary under a newer tag"
bash -n "$D/install.sh"
grep -q __SBCTL_RELEASE_PUBKEY_B64__ "$D/install.sh" && fail "the stamped install.sh still has the key marker"

# ---- 3. install ------------------------------------------------------------------------
# A runner image may ship services on our ports.
systemctl stop apache2 nginx postgresql mysql 2>/dev/null || true
for p in 80 443 5432 6543 5433 9999 7000 3000 8080 4000 5000; do
  if ss -ltnH "sport = :$p" | grep -q .; then ss -ltnp "sport = :$p" >&2; fail "port $p is already in use on this VM"; fi
done

STUDIO_ARGS=(--no-studio)
if [[ $E2E_STUDIO == 1 ]]; then
  stag=$(awk '/^studio:/{f=1;next} f&&/^ *tag:/{print $2;exit}' versions.yaml)
  slim=studio-$stag-r0
  surl=https://github.com/supabase/slim-services/releases/download/$slim
  if ssum=$(curl -fsSL --retry 3 "$surl/SHA256SUMS" | awk -v n="$slim-linux-$ARCH.tar.zst" '$2==n || $2=="*"n {print $1}') && [[ -n $ssum ]]; then
    STUDIO_ARGS=(--studio-url "$surl/$slim-linux-$ARCH.tar.zst" --studio-sha256 "$ssum")
    log "dashboard stand-in: $slim"
  else
    log "could not read the slim Studio checksum; running without a dashboard"
    E2E_STUDIO=0
  fi
fi

log "install.sh --binary (fresh node)"
deploy/install.sh --binary "$SBCTL_BIN" --public-ip 127.0.0.1 --tls off --email ci@example.com --firewall none \
  --claim-token-file "$WORK/claim-token" "${STUDIO_ARGS[@]}" 2>&1 | tee "$WORK/install.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "install.sh failed"
grep -q "sbctl is running" "$WORK/install.log" || fail "install.sh did not report success"
grep -q "Dashboard   http://studio.$BASE" "$WORK/install.log" || fail "install.sh printed no dashboard URL"
TOKEN=$(tr -d '[:space:]' <"$WORK/claim-token")
[[ $TOKEN =~ ^sbc_[0-9a-f]{48}$ ]] || fail "the claim token file does not hold a claim token"
grep -q "$TOKEN" "$WORK/install.log" && fail "install.sh printed the claim token although --claim-token-file was given (an unattended install logs its output)"
grep -q "$WORK/claim-token" "$WORK/install.log" || fail "install.sh did not say where the claim token is"
[[ $(stat -c '%a' "$WORK/claim-token") == 600 ]] || fail "the claim token file is not 0600"

log "host state"
[[ $(stat -c '%U:%a' /etc/sbctl/config.toml) == sbctl:600 ]] || fail "config.toml is $(stat -c '%U:%a' /etc/sbctl/config.toml), want sbctl:600"
[[ $(stat -c '%U:%a' /etc/sbctl/master.key) == sbctl:600 ]] || fail "master.key is $(stat -c '%U:%a' /etc/sbctl/master.key), want sbctl:600"
grep -q "public_ip = '127.0.0.1'" /etc/sbctl/config.toml || fail "config.toml lacks the public IP"
grep -q "mode = 'off'" /etc/sbctl/config.toml || fail "config.toml lacks tls mode off"
[[ $(systemctl is-enabled sbctl.service) == enabled ]] || fail "sbctl.service is not enabled for boot"
UNITS=(sbctl.service sb-postgres@system.service sb-gotrue@system.service sb-pgmeta.service sb-supavisor.service sb-realtime.service sb-storage.service)
[[ $E2E_STUDIO == 1 ]] && UNITS+=(sb-studio.service)
for u in "${UNITS[@]}"; do wait_active "$u" 60; done
[[ $(systemctl show -p User --value sbctl.service) == sbctl ]] || fail "sbctl.service does not run as sbctl"
[[ $(systemctl show -p MainPID --value sb-postgres@system.service) -gt 0 ]] || fail "no system Postgres"
for p in 80 443 5432 6543; do ss -ltnH "sport = :$p" | grep -q . || fail "nothing listens on public port $p"; done
for p in 7000 5433 9999; do
  ss -ltnH "sport = :$p" | awk '{print $4}' | grep -q "^127.0.0.1:$p$" || fail "port $p is not loopback only"
done

log "dashboard through the proxy"
[[ $(code -H "Host: studio.$BASE" http://127.0.0.1/api/incident-banner) == 200 ]] || fail "studio host: the proxy does not answer its banner route"
if [[ $E2E_STUDIO == 1 ]]; then
  for ((i = 0; i < 30; i++)); do
    [[ $(code -H "Host: studio.$BASE" http://127.0.0.1/api/get-utc-time) == 200 ]] && break
    sleep 2
  done
  [[ $(code -H "Host: studio.$BASE" http://127.0.0.1/api/get-utc-time) == 200 ]] || { journalctl --no-pager -u sb-studio -n 40 >&2; fail "Studio is not served through studio.<domain>"; }
  [[ $(code -H "Host: studio.$BASE" http://127.0.0.1/) =~ ^(200|307|308)$ ]] || fail "Studio's front page does not answer through the proxy"
fi
[[ $(code -H "Host: nothing.$BASE" http://127.0.0.1/) == 404 ]] || fail "an unknown host did not get a 404"

# ---- 4. claim --------------------------------------------------------------------------
log "claim page and token"
[[ $(code -H "Host: api.$BASE" http://127.0.0.1/claim) == 200 ]] || fail "GET /claim"
[[ $(api GET /claim -D - -o /dev/null | tr -d '\r' | grep -i '^content-security-policy:' | wc -l) -eq 1 ]] || fail "the claim page has no CSP"
[[ $(api GET /v1/projects -o /dev/null -w '%{http_code}') == 401 ]] || fail "the Management API answered without credentials"
claim_body() { printf '{"token":"%s","email":"%s","password":"%s","organization_name":"E2E"}' "$1" "$ADMIN_EMAIL" "$ADMIN_PASSWORD"; }
[[ $(api POST /claim -H 'Content-Type: application/json' -d "$(claim_body "sbc_$(printf '0%.0s' $(seq 1 48))")" -o /dev/null -w '%{http_code}') == 403 ]] || fail "a wrong token was not refused"
[[ $(api POST /claim -H 'Content-Type: application/json' -d "$(claim_body "$TOKEN")" -o "$WORK/claim.json" -w '%{http_code}') == 201 ]] || { cat "$WORK/claim.json" >&2; fail "claiming with the install token failed"; }
[[ $(jq_ 'd["email"]' <"$WORK/claim.json") == "$ADMIN_EMAIL" ]] || fail "claim answer: $(cat "$WORK/claim.json")"
[[ $(api POST /claim -H 'Content-Type: application/json' -d "$(claim_body "$TOKEN")" -o /dev/null -w '%{http_code}') == 403 ]] || fail "the claim token worked twice"
[[ $(sbctl claim status) == claimed ]] || fail "claim status after the claim"
if sbctl claim token >/dev/null 2>&1; then fail "a second claim token was issued after the claim"; fi

log "public sign-up is disabled"
st=$(api POST /auth/v1/signup -H 'Content-Type: application/json' -d '{"email":"stranger@example.com","password":"stranger-password-1"}' -o /dev/null -w '%{http_code}')
[[ $st =~ ^4 ]] || fail "sign-up on the dashboard GoTrue answered $st, want a 4xx"

log "sign in and use the Management API with the session"
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | jq_ 'd["access_token"]') || fail "sign-in"
[[ $JWT == ey* ]] || fail "no access token from the dashboard GoTrue"
[[ $(api GET /platform/profile -H "Authorization: Bearer $JWT" -o /dev/null -w '%{http_code}') == 200 ]] || fail "/platform/profile with the session"

log "invite a second user, redeem the invite, list, remove"
INVITE_LINK=$(sbctl users invite invitee@example.com --role developer --no-mail 2>/dev/null)
[[ $INVITE_LINK == */claim#* ]] || fail "invite link: $INVITE_LINK"
INVITE=$(python3 -c 'import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).fragment)["token"][0])' "$INVITE_LINK")
[[ $INVITE =~ ^sbi_[0-9a-f]{48}$ ]] || fail "invite token: $INVITE"
[[ $(api POST /claim -H 'Content-Type: application/json' -d "{\"token\":\"$INVITE\",\"password\":\"$ADMIN_PASSWORD\"}" -o "$WORK/inv.json" -w '%{http_code}') == 201 ]] || { cat "$WORK/inv.json" >&2; fail "redeeming the invite"; }
sbctl users list | grep "invitee@example.com.*:developer" >/dev/null || fail "the invited user is not listed as a developer"
sbctl users remove invitee@example.com | grep removed >/dev/null || fail "users remove"
if sbctl users list | grep invitee@example.com >/dev/null; then fail "the removed user is still listed"; fi

# ---- 5. a project through the API with a PAT ---------------------------------------------
log "personal access token"
PAT=$(api POST /platform/profile/access-tokens -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' -d '{"name":"e2e"}' | jq_ 'd["token"]') || fail "creating a personal access token"
[[ $PAT =~ ^sbp_[0-9a-f]{40}$ ]] || fail "personal access token: $PAT"
papi() { local m=$1 p=$2; shift 2; api "$m" "$p" -H "Authorization: Bearer $PAT" "$@"; }
ORG=$(papi GET /v1/organizations | jq_ 'd[0]["slug"]') || fail "listing organizations with the PAT"
[[ -n $ORG ]] || fail "no organization after the claim"

log "create a project (first project downloads nothing new but initializes a cluster)"
DBPASS=E2e-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
REF=$(papi POST /v1/projects -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e\",\"organization_slug\":\"$ORG\",\"db_pass\":\"$DBPASS\",\"region\":\"us-east-1\"}" | jq_ 'd["ref"]') || fail "creating a project"
[[ $REF =~ ^[a-z]{20}$ ]] || fail "project ref: $REF"
status=""
for ((i = 0; i < 180; i++)); do
  status=$(papi GET "/v1/projects/$REF" | jq_ 'd["status"]' 2>/dev/null || true)
  [[ $status == ACTIVE_HEALTHY || $status == INIT_FAILED ]] && break
  sleep 3
done
if [[ $status != ACTIVE_HEALTHY ]]; then journalctl --no-pager -u sbctl.service -n 60 >&2; fail "project is $status"; fi
log "project $REF is ACTIVE_HEALTHY"
KEYS_JSON=$(papi GET "/v1/projects/$REF/api-keys?reveal=true")
PUB=$(printf '%s' "$KEYS_JSON" | jq_ '[k["api_key"] for k in d if str(k.get("api_key","")).startswith("sb_publishable_")][0]') || fail "no publishable key in $KEYS_JSON"
SEC=$(printf '%s' "$KEYS_JSON" | jq_ '[k["api_key"] for k in d if str(k.get("api_key","")).startswith("sb_secret_")][0]') || fail "no secret key in $KEYS_JSON"
[[ $PUB == sb_publishable_* && $SEC == sb_secret_* ]] || fail "project keys: $KEYS_JSON"

log "REST through the proxy with the publishable key"
SQL='create table public.e2e_items (id int primary key, label text); insert into public.e2e_items values (1, '"'one'"'), (2, '"'two'"'); grant select on public.e2e_items to anon; notify pgrst, '"'reload schema'"';'
papi POST "/v1/projects/$REF/database/query" -H 'Content-Type: application/json' -d "$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$SQL")" >/dev/null || fail "create table through database/query"
rest() { curl -sS -m 30 -H "Host: $REF.api.$BASE" "$@"; }
n=""
for ((i = 0; i < 30; i++)); do
  n=$(rest -H "apikey: $PUB" "http://127.0.0.1/rest/v1/e2e_items?select=id" | jq_ 'len(d)' 2>/dev/null || true)
  [[ $n == 2 ]] && break
  sleep 2
done
[[ $n == 2 ]] || fail "REST /rest/v1/e2e_items with the publishable key returned '$n' rows, want 2"
[[ $(code -H "Host: $REF.api.$BASE" http://127.0.0.1/rest/v1/e2e_items) == 401 ]] || fail "REST without a key was not a 401"
[[ $(code -H "Host: $REF.api.$BASE" -H "apikey: sb_publishable_wrong" http://127.0.0.1/rest/v1/e2e_items) == 401 ]] || fail "REST with a wrong key was not a 401"
[[ $(code -H "Host: $REF.api.$BASE" -H "apikey: $SEC" http://127.0.0.1/storage/v1/bucket) == 200 ]] || fail "Storage tenant: GET /storage/v1/bucket with the secret key (the daemon did not register the project with Storage)"

log "pooler login as postgres.$REF on both ports"
PSQL=$(ls -d "$SBCTL_STATE"/artifacts/postgres/*/bin/psql | head -1)
for port in 5432 6543; do
  got=$(PGPASSWORD=$DBPASS "$PSQL" "host=127.0.0.1 port=$port user=postgres.$REF dbname=postgres sslmode=disable connect_timeout=10" -Atc 'select count(*) from public.e2e_items' </dev/null) \
    || fail "pooler login on $port"
  [[ $got == 2 ]] || fail "pooler on $port returned '$got'"
done

# ---- 6. re-run: idempotent, secrets kept -------------------------------------------------
log "re-running install.sh keeps everything"
KEY_SUM=$(sha256sum /etc/sbctl/master.key)
CFG_SUM=$(sha256sum /etc/sbctl/config.toml)
ENTER=$(systemctl show -p ActiveEnterTimestampMonotonic --value sbctl.service)
PG_PID=$(systemctl show -p MainPID --value "sb-postgres@$REF.service")
fleet_pids() { for u in sb-pgmeta sb-supavisor sb-realtime sb-storage sb-postgres@system sb-gotrue@system; do systemctl show -p MainPID --value "$u.service"; done | tr '\n' ' '; }
FLEET_PIDS=$(fleet_pids)
# Through a pipe, the way the documented one-liner runs it.
cat deploy/install.sh | bash -s -- --binary "$SBCTL_BIN" 2>&1 | tee "$WORK/install2.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "second install.sh run failed"
[[ $(sha256sum /etc/sbctl/master.key) == "$KEY_SUM" ]] || fail "the master key changed"
[[ $(sha256sum /etc/sbctl/config.toml) == "$CFG_SUM" ]] || fail "config.toml changed on a no-flag re-run"
grep -q "already exists" "$WORK/install2.log" || fail "the re-run did not say the first administrator exists"
grep -q "$TOKEN" "$WORK/install2.log" && fail "the re-run printed the spent claim token"
[[ $(systemctl show -p ActiveEnterTimestampMonotonic --value sbctl.service) == "$ENTER" ]] || fail "a no-change re-run restarted sbctl.service"
[[ $(systemctl show -p MainPID --value "sb-postgres@$REF.service") == "$PG_PID" ]] || fail "the re-run restarted the project's Postgres"
[[ $(fleet_pids) == "$FLEET_PIDS" ]] || fail "the re-run restarted a shared service or the system project: $FLEET_PIDS -> $(fleet_pids)"
[[ $(papi GET "/v1/projects/$REF" | jq_ 'd["status"]') == ACTIVE_HEALTHY ]] || fail "project unhealthy after the re-run"
log "re-running with one flag changes only that setting"
deploy/install.sh --binary "$SBCTL_BIN" --email changed@example.com >/dev/null 2>&1 || fail "re-run with --email failed"
grep -q "email = 'changed@example.com'" /etc/sbctl/config.toml || fail "--email did not change config.toml"
grep -q "public_ip = '127.0.0.1'" /etc/sbctl/config.toml || fail "--email dropped the public IP"
[[ $(sha256sum /etc/sbctl/master.key) == "$KEY_SUM" ]] || fail "the master key changed on the second re-run"
wait_active sbctl.service 120
for ((i = 0; i < 60; i++)); do [[ $(papi GET "/v1/projects/$REF" -o /dev/null -w '%{http_code}') == 200 ]] && break; sleep 2; done
[[ $(fleet_pids) == "$FLEET_PIDS" ]] || fail "a config change restarted a shared service or the system project: $FLEET_PIDS -> $(fleet_pids)"

# ---- 6b. re-running install.sh with a newer binary restarts the daemon onto it ------------
build_version() { # VERSION OUT [GIVEN-PATH]
  local v=$1 out=$2 given=${3:-}
  if [[ -n $given ]]; then cp "$given" "$out"
  elif command -v go >/dev/null; then CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$v" -o "$out" ./cmd/sbctl
  else fail "no SBCTL_BIN for $v and no go toolchain"; fi
  [[ $("$out" --version) == *"$v"* ]] || fail "the $v binary reports: $("$out" --version)"
}
V2=$WORK/sbctl-v2; build_version v0.0.2 "$V2" "${SBCTL_BIN_V2:-}"
V3=$WORK/sbctl-v3; build_version v0.0.3 "$V3" "${SBCTL_BIN_V3:-}"
daemon_version() { "/proc/$(systemctl show -p MainPID --value sbctl.service)/exe" --version; }
[[ $(daemon_version) == *v0.0.1* ]] || fail "the daemon does not run v0.0.1 before the upgrade: $(daemon_version)"
log "re-running install.sh with the v0.0.2 binary moves the daemon onto it"
ENTER=$(systemctl show -p ActiveEnterTimestampMonotonic --value sbctl.service)
deploy/install.sh --binary "$V2" 2>&1 | tee "$WORK/install3.log"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "install.sh with the v0.0.2 binary failed"
[[ $(/usr/local/bin/sbctl --version) == *v0.0.2* ]] || fail "install.sh did not install the v0.0.2 binary"
wait_active sbctl.service 120
[[ $(systemctl show -p ActiveEnterTimestampMonotonic --value sbctl.service) != "$ENTER" ]] || fail "install.sh left the daemon running the old binary"
[[ $(daemon_version) == *v0.0.2* ]] || fail "the daemon still reports $(daemon_version) after install.sh installed v0.0.2"
[[ $(fleet_pids) == "$FLEET_PIDS" ]] || fail "the binary upgrade restarted a shared service or the system project: $FLEET_PIDS -> $(fleet_pids)"
[[ $(systemctl show -p MainPID --value "sb-postgres@$REF.service") == "$PG_PID" ]] || fail "the binary upgrade restarted the project's Postgres"
for ((i = 0; i < 60; i++)); do [[ $(papi GET "/v1/projects/$REF" -o /dev/null -w '%{http_code}') == 200 ]] && break; sleep 2; done
[[ $(papi GET "/v1/projects/$REF" | jq_ 'd["status"]') == ACTIVE_HEALTHY ]] || fail "project unhealthy after the binary upgrade"

# ---- 7. self-update ----------------------------------------------------------------------
if [[ -n $V3 ]]; then
  log "self-update: sign v0.0.3 and update from the local release server"
  make_release v0.0.3 "$V3"
  SU=(/usr/local/bin/sbctl self-update --repo o/r --api-base "http://127.0.0.1:$SRV_PORT" --public-key-file "$KEYS/pub.pem")
  [[ $("${SU[@]}" --check) == *"update available"* ]] || fail "self-update --check did not see v0.0.3: $("${SU[@]}" --check)"
  D2=$WORK/srv/download/v0.0.3
  cp "$D2/sbctl-linux-$ARCH" "$WORK/good-v3"
  echo tampered >>"$D2/sbctl-linux-$ARCH"
  if out=$("${SU[@]}" 2>&1); then fail "self-update installed a tampered binary"; fi
  [[ $out == *"does not match its checksum"* ]] || fail "self-update tamper message: $out"
  [[ $(/usr/local/bin/sbctl --version) == *v0.0.2* ]] || fail "the binary changed although the update was refused"
  cp "$WORK/good-v3" "$D2/sbctl-linux-$ARCH"
  if out=$("${SU[@]}" --version v0.0.9 --force 2>&1); then fail "self-update installed an older signed binary under the newer tag v0.0.9"; fi
  [[ $out == *"refusing to install"* ]] || fail "self-update downgrade message: $out"
  [[ $(/usr/local/bin/sbctl --version) == *v0.0.2* ]] || fail "the binary changed although the downgrade was refused"
  if out=$(/usr/local/bin/sbctl self-update --repo o/r --api-base "http://127.0.0.1:$SRV_PORT" --public-key-file "$KEYS/other.pub" 2>&1); then fail "self-update accepted a release signed by another key"; fi
  [[ $out == *"does not verify"* ]] || fail "self-update wrong-key message: $out"
  "${SU[@]}" 2>&1 | tee "$WORK/selfupdate.log"
  [[ ${PIPESTATUS[0]} -eq 0 ]] || fail "self-update failed"
  [[ $(/usr/local/bin/sbctl --version) == *v0.0.3* ]] || fail "the binary is not v0.0.3 after self-update"
  [[ $(daemon_version) == *v0.0.3* ]] || fail "the daemon does not run v0.0.3 after self-update: $(daemon_version)"
  [[ -x /usr/local/bin/sbctl.prev ]] || fail "the previous binary was not kept"
  wait_active sbctl.service 120
  [[ $(systemctl show -p MainPID --value "sb-postgres@$REF.service") == "$PG_PID" ]] || fail "self-update restarted the project's Postgres"
  [[ $(fleet_pids) == "$FLEET_PIDS" ]] || fail "self-update restarted a shared service or the system project: $FLEET_PIDS -> $(fleet_pids)"
  for ((i = 0; i < 60; i++)); do [[ $(papi GET "/v1/projects/$REF" -o /dev/null -w '%{http_code}') == 200 ]] && break; sleep 2; done
  [[ $(papi GET "/v1/projects/$REF" | jq_ 'd["status"]') == ACTIVE_HEALTHY ]] || fail "project not healthy after self-update"
  n=$(rest -H "apikey: $PUB" "http://127.0.0.1/rest/v1/e2e_items?select=id" | jq_ 'len(d)') || true
  [[ $n == 2 ]] || fail "REST after self-update returned '$n' rows"
  [[ $("${SU[@]}" 2>&1) == *"already up to date"* ]] || fail "a second self-update did not report up to date"

  # A release whose daemon dies after it forks: systemd reports the unit active, so only the
  # readiness probe notices. self-update must put v0.0.3 back and re-render its units.
  log "self-update rollback: v0.0.4 starts but its daemon exits"
  BAD=$WORK/sbctl-bad
  printf '%s\n' '#!/bin/sh' 'case "$1" in' '  --version) echo "sbctl version v0.0.4" ;;' \
    '  serve) echo "simulated crash" >&2; exit 1 ;;' '  *) exit 0 ;;' 'esac' >"$BAD"
  chmod 755 "$BAD"
  make_release v0.0.4 "$BAD"
  if out=$("${SU[@]}" --wait 20s 2>&1); then fail "self-update reported success for a daemon that exits"; fi
  [[ $out == *"rolled back to the previous binary, which is running"* ]] || fail "self-update rollback message: $out"
  [[ $(/usr/local/bin/sbctl --version) == *v0.0.3* ]] || fail "the binary was not rolled back: $(/usr/local/bin/sbctl --version)"
  wait_active sbctl.service 120
  [[ $(daemon_version) == *v0.0.3* ]] || fail "the daemon does not run the restored binary: $(daemon_version)"
  for ((i = 0; i < 60; i++)); do [[ $(papi GET "/v1/projects/$REF" -o /dev/null -w '%{http_code}') == 200 ]] && break; sleep 2; done
  [[ $(papi GET "/v1/projects/$REF" | jq_ 'd["status"]') == ACTIVE_HEALTHY ]] || fail "project not healthy after the rollback"
  [[ $(systemctl show -p MainPID --value "sb-postgres@$REF.service") == "$PG_PID" ]] || fail "the rollback restarted the project's Postgres"
else
  fail "no v0.0.3 binary: the self-update step cannot run"
fi

log "install end to end: all checks passed"
