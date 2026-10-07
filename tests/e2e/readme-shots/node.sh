#!/usr/bin/env bash
# Installs a Supavise node on this (ephemeral CI) VM with our platform-mode Studio build, claims it
# as the organization "Acme" and writes what seed.mjs and shots.mjs need to $SHOTS_DIR/env.json.
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/tmp/supavise STUDIO_TARBALL=/path/supavise-studio-...tar.zst \
#        SHOTS_DIR=/tmp/shots tests/e2e/readme-shots/node.sh
#
# The node is `--tls off` on the domain supavise.example, which /etc/hosts maps to 127.0.0.1
# (seed.mjs adds each project's host), so the dashboard reads as a real install and nothing needs
# DNS. Do not run it on a machine you care about: it creates the supavise user, writes
# /etc/supavise and /etc/hosts, installs units and starts real clusters.
export SUPAVISE_DOMAIN=supavise.example
# shellcheck source=../../linux/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../linux/lib.sh"

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SHOTS_DIR=${SHOTS_DIR:-/tmp/shots}
DOMAIN=$SUPAVISE_DOMAIN
STUDIO_PORT=38801
ADMIN_EMAIL=alex.rivera@example.com
ADMIN_PASSWORD="Acme-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
WORK=$(mktemp -d)
SRV_PID=""
: "${SUPAVISE_BIN:?}" "${STUDIO_TARBALL:?}"
[[ -f $STUDIO_TARBALL ]] || fail "no Studio build at $STUDIO_TARBALL"

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
mkdir -p "$SHOTS_DIR"
cd "$REPO_ROOT" || exit 1

systemctl stop apache2 nginx postgresql mysql 2>/dev/null || true
for p in 80 443 5432 6543 5433 9999 7000 3000 8080 4000 5000 9000; do
  if ss -ltnH "sport = :$p" | grep -q .; then ss -ltnp "sport = :$p" >&2; fail "port $p is already in use on this VM"; fi
done
printf '127.0.0.1 %s api.%s studio.%s pooler.%s\n' "$DOMAIN" "$DOMAIN" "$DOMAIN" "$DOMAIN" >>/etc/hosts

# Our Studio build, served on loopback for the installer (it downloads the artifact by URL).
STUDIO_DIR=$(dirname "$STUDIO_TARBALL")
STUDIO_NAME=$(basename "$STUDIO_TARBALL")
STUDIO_SHA=$(sha256sum "$STUDIO_TARBALL" | awk '{print $1}')
(cd "$STUDIO_DIR" && exec python3 -m http.server "$STUDIO_PORT" --bind 127.0.0.1 >"$WORK/http.log" 2>&1) &
SRV_PID=$!
for ((i = 0; i < 20; i++)); do [[ $(http_code "http://127.0.0.1:$STUDIO_PORT/$STUDIO_NAME") == 200 ]] && break; sleep 0.5; done

log "install.sh (fresh node, our Studio build, Edge Functions on by default)"
deploy/install.sh --binary "$SUPAVISE_BIN" --domain "$DOMAIN" --public-ip 127.0.0.1 --tls off --email ci@example.com \
  --firewall none --studio-url "http://127.0.0.1:$STUDIO_PORT/$STUDIO_NAME" --studio-sha256 "$STUDIO_SHA" \
  --claim-token-file "$WORK/claim-token" 2>&1 | tee "$LOG_DIR/install.log"
grep -q "Supavise is running" "$LOG_DIR/install.log" || fail "install.sh did not report success"
kill "$SRV_PID" 2>/dev/null || true; SRV_PID=""
for u in supavise.service supavise-postgres@system.service supavise-gotrue@system.service supavise-pgmeta.service supavise-supavisor.service supavise-realtime.service supavise-storage.service supavise-studio.service supavise-edge-runtime.service; do
  wait_active "$u" 180
done
for ((i = 0; i < 60; i++)); do
  [[ $(http_code -H "Host: studio.$DOMAIN" http://127.0.0.1/api/get-utc-time) == 200 ]] && break
  sleep 2
done
[[ $(http_code -H "Host: studio.$DOMAIN" http://127.0.0.1/api/get-utc-time) == 200 ]] || fail "Studio is not served through studio.$DOMAIN"

log "claim as Acme, sign in, personal access token"
TOKEN=$(tr -d '[:space:]' <"$WORK/claim-token")
[[ $(api POST /claim -H 'Content-Type: application/json' -o "$WORK/claim.json" -w '%{http_code}' \
  -d "{\"token\":\"$TOKEN\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\",\"organization_name\":\"Acme\"}") == 201 ]] \
  || { sed -E 's/"(token|password)":"[^"]*"/"\1":"[redacted]"/g' "$WORK/claim.json" >&2; fail "claim"; }
JWT=$(api POST '/auth/v1/token?grant_type=password' -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | json_get 'd["access_token"]') || fail "dashboard sign-in"
PAT=$(api POST /platform/profile/access-tokens -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' -d '{"name":"readme-shots"}' | json_get 'd["token"]') \
  || fail "creating a personal access token"
[[ $PAT == sbp_* ]] || fail "personal access token did not look like one"
ORG=$(papi GET /v1/organizations | json_get 'd[0]["slug"]') || fail "listing organizations"
supavise api profile --format yaml >"$SHOTS_DIR/profile.yaml" || fail "supavise api profile"

python3 - "$SHOTS_DIR/env.json" "$DOMAIN" "$ADMIN_EMAIL" "$ADMIN_PASSWORD" "$PAT" "$ORG" <<'PY'
import json, sys
out, domain, email, password, pat, org = sys.argv[1:]
json.dump({"domain": domain, "apiUrl": f"http://api.{domain}", "studioUrl": f"http://studio.{domain}",
           "adminEmail": email, "adminPassword": password, "pat": pat, "org": org}, open(out, "w"))
PY
chmod 600 "$SHOTS_DIR/env.json" "$SHOTS_DIR/profile.yaml"
[[ -n ${SUDO_USER:-} ]] && chown "$SUDO_USER" "$SHOTS_DIR" "$SHOTS_DIR/env.json" "$SHOTS_DIR/profile.yaml"
log "node is up: organization $ORG, dashboard http://studio.$DOMAIN"
