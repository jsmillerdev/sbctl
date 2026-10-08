#!/usr/bin/env bash
# The cluster identity of a leader under real systemd: what `supavise node token` writes and who owns
# it, the daemon's one restart into cluster mode with no project restarted, the peer port speaking TLS
# 1.3 with the mesh's ALPN and a chain that ends in the CA the token pins, `node ls` and the cluster
# block of `status`, and the fenced mode (a node that finds itself replaced stops its clusters, removes
# their run scripts and answers 503).
#
#   sudo env "PATH=$PATH" SUPAVISE_BIN=/path/to/supavise-linux-amd64 \
#     S3_ENDPOINT=http://127.0.0.1:9000 S3_BUCKET=supavise-test S3_ACCESS_KEY=... S3_SECRET_KEY=... \
#     tests/linux/cluster-identity.sh [--teardown]
#
# `node token` refuses a file:// backup backend (a second server could not read its first copy), so the
# node runs against an S3-compatible store. The join itself, between two nodes, is exercised by the Go
# tests (internal/cluster) over real TLS sockets and by the two-server harness (tests/linux/multi).
#
# Not run in development (root, systemd and Linux required); CI runs it on an ephemeral Ubuntu 24.04 VM.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TEARDOWN=0
[[ ${1:-} == --teardown ]] && TEARDOWN=1
WORK=$(mktemp -d)
trap 'rc=$?; collect_logs; [[ $TEARDOWN -eq 1 ]] && teardown; rm -rf "$WORK"; exit $rc' EXIT

: "${S3_ENDPOINT:?set S3_ENDPOINT to an S3-compatible endpoint}"
: "${S3_BUCKET:?set S3_BUCKET}"
: "${S3_ACCESS_KEY:?set S3_ACCESS_KEY}"
: "${S3_SECRET_KEY:?set S3_SECRET_KEY}"
PEER_PORT=17443
CLUSTER_DIR=/etc/supavise/cluster

preflight
for c in openssl ss; do command -v "$c" >/dev/null || fail "missing $c"; done
install_binary
setup_node
cat >>"$SUPAVISE_CONF" <<CONF

[backup]
backend = "s3://$S3_BUCKET/cluster-identity"
s3_endpoint = "$S3_ENDPOINT"
s3_region = "us-east-1"
s3_force_path_style = true
s3_access_key_id = "$S3_ACCESS_KEY"
s3_secret_access_key = "$S3_SECRET_KEY"

[node]
peer_listen = "127.0.0.1:$PEER_PORT"
peer_address = "127.0.0.1:$PEER_PORT"

# The S3 service of the job listens on 9000, which is where the Edge Runtime goes by default.
[ports]
edge_runtime = 19000
CONF
chown "$SUPAVISE_USER:$SUPAVISE_USER" "$SUPAVISE_CONF"
chmod 0600 "$SUPAVISE_CONF" # it holds the S3 secret key

log "system init and one project"
system_init
wait_active supavise-postgres@system.service 30
REF=$(create_project ident micro)
[[ $REF =~ ^[a-z]{20}$ ]] || fail "bad ref '$REF'"
PGU="supavise-postgres@$REF.service"
wait_active "$PGU" 30
entered() { systemctl show -p ActiveEnterTimestampMonotonic --value "$1"; }

log "daemon: supavise.service"
systemctl start supavise.service
for ((i = 0; i < 60; i++)); do
  [[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] && break
  sleep 1
done
[[ $(http_code http://127.0.0.1:7000/v1/projects) == 401 ]] || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "the Management API does not answer"; }

log "a single server: no cluster identity, no peer port, no cluster block"
[[ ! -e $CLUSTER_DIR/node.crt ]] || fail "$CLUSTER_DIR/node.crt exists before any token"
if ss -ltn "sport = :$PEER_PORT" | grep -q ":$PEER_PORT"; then fail "something listens on the peer port of a single server"; fi
OUT=$(supavise status 2>&1 || true)
if grep -q '^cluster   ' <<<"$OUT"; then fail "status shows a cluster block on a single server: $OUT"; fi
# The daemon's first start may restart a unit whose files it renders differently; what counts is what the
# daemon's restart into cluster mode does, so the clocks are read once the first start has settled.
sleep 10
INV0=$(systemctl show -p InvocationID --value supavise.service)
SYS_ENTERED=$(entered supavise-postgres@system.service)
PRJ_ENTERED=$(entered "$PGU")

log "supavise node token: the token, the identity files and who owns them"
supavise node token --ttl 30m --name replica-1 >"$WORK/token" 2>"$WORK/token.err" || { cat "$WORK/token.err" >&2; fail "node token"; }
TOKEN=$(<"$WORK/token")
[[ $TOKEN == svj1.* && $(wc -l <"$WORK/token") -eq 1 ]] || fail "the token is not one svj1. line on stdout: $TOKEN"
grep -q 'cluster identity' "$WORK/token.err" || fail "node token did not say it gave the server an identity: $(cat "$WORK/token.err")"
read -r CA_FPR LEADER < <(python3 - "$TOKEN" <<'PY'
import base64, json, sys
t = sys.argv[1][len("svj1."):]
d = json.loads(base64.urlsafe_b64decode(t + "=" * (-len(t) % 4)))
assert d["v"] == 1 and d["name"] == "replica-1", d
print(d["ca_fpr"], d["leader"])
PY
) || fail "the token does not decode"
[[ $LEADER == "127.0.0.1:$PEER_PORT" ]] || fail "the token names the leader $LEADER"
mode() { stat -c '%U:%G %a' "$1"; }
[[ $(mode $CLUSTER_DIR) == "$SUPAVISE_USER:$SUPAVISE_USER 750" ]] || fail "$CLUSTER_DIR is $(mode $CLUSTER_DIR)"
[[ $(mode $CLUSTER_DIR/node.key) == "$SUPAVISE_USER:$SUPAVISE_USER 600" ]] || fail "node.key is $(mode $CLUSTER_DIR/node.key)"
[[ $(mode $CLUSTER_DIR/node.crt) == "$SUPAVISE_USER:$SUPAVISE_USER 644" ]] || fail "node.crt is $(mode $CLUSTER_DIR/node.crt)"
openssl x509 -in $CLUSTER_DIR/node.crt -noout -ext subjectAltName | grep -q 'URI:supavise://node/n1' || fail "node.crt does not name node n1"
[[ $(openssl x509 -in $CLUSTER_DIR/ca.crt -outform DER | sha256sum | cut -d' ' -f1) == "$CA_FPR" ]] || fail "ca.crt is not the CA the token pins"
openssl verify -CAfile $CLUSTER_DIR/ca.crt -purpose sslserver $CLUSTER_DIR/node.crt >/dev/null || fail "node.crt does not verify against ca.crt"

log "the daemon restarts once into cluster mode and listens on the peer port"
for ((i = 0; i < 60; i++)); do
  [[ $(systemctl show -p InvocationID --value supavise.service) != "$INV0" ]] && ss -ltn "sport = :$PEER_PORT" | grep -q ":$PEER_PORT" && break
  sleep 1
done
ss -ltn "sport = :$PEER_PORT" | grep -q ":$PEER_PORT" || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "nothing listens on the peer port after a token"; }
journalctl --no-pager -u supavise.service | grep -q "role in the cluster changed" || fail "the restart was not the role change"
wait_active supavise.service 30
[[ $(entered supavise-postgres@system.service) == "$SYS_ENTERED" ]] || fail "the system cluster restarted with the daemon"
[[ $(entered "$PGU") == "$PRJ_ENTERED" ]] || fail "a project's Postgres restarted with the daemon (invariant I4)"

log "the peer port: TLS 1.3, the mesh's ALPN, a chain that ends in the pinned CA"
SC=$(openssl s_client -connect 127.0.0.1:$PEER_PORT -tls1_3 -alpn supavise-mesh/1 -showcerts </dev/null 2>&1 || true)
[[ $SC == *"TLSv1.3"* ]] || fail "the peer port did not negotiate TLS 1.3: $SC"
[[ $SC == *"supavise-mesh/1"* ]] || fail "the peer port did not select the mesh ALPN: $SC"
cat >"$WORK/chain.py" <<'PY'
import base64, hashlib, re, sys
blocks = re.findall(r"-----BEGIN CERTIFICATE-----(.*?)-----END CERTIFICATE-----", sys.stdin.read(), re.S)
assert len(blocks) >= 2, "chain has %d certificates" % len(blocks)
der = base64.b64decode("".join(blocks[-1].split()))
assert hashlib.sha256(der).hexdigest() == sys.argv[1], "root fingerprint differs"
PY
python3 "$WORK/chain.py" "$CA_FPR" <<<"$SC" || fail "the chain the peer port presents does not end in the pinned CA"

log "node ls and the cluster block of status"
OUT=$(supavise node ls)
[[ $OUT == *n1* && $OUT == *leader* && $OUT == *active* ]] || fail "node ls: $OUT"
[[ $(supavise node ls --json | json_get 'd["leader"]') == n1 ]] || fail "node ls --json does not name n1 as leader"
for ((i = 0; i < 30; i++)); do
  OUT=$(supavise status 2>&1 || true)
  grep -q '^cluster   .*leader n1' <<<"$OUT" && [[ $OUT != *"not current"* ]] && break
  sleep 2
done
grep -q '^cluster   .*leader n1' <<<"$OUT" || fail "status has no cluster block: $OUT"
[[ -s $SUPAVISE_STATE/cluster-status.json ]] || fail "the daemon wrote no cluster-status.json"
supavise node token --ttl 5m >/dev/null 2>&1 || fail "a second token"
[[ $(supavise node ls --json | json_get 'len(d["nodes"])') -eq 1 ]] || fail "a token created a node"

log "fenced mode: a node that finds itself replaced stops its clusters and answers 503"
RUN="$SUPAVISE_STATE/projects/$REF/postgres.run"
[[ -e $RUN ]] || fail "$RUN does not exist"
sudo -u "$SUPAVISE_USER" tee "$SUPAVISE_STATE/fenced.json" >/dev/null <<JSON
{"epoch": 5, "leader": "n2", "reason": "node n2 says node n2 leads at epoch 5; this node's epoch is 1", "at": "$(date -u +%FT%TZ)"}
JSON
systemctl restart supavise.service
CODE=000
for ((i = 0; i < 60; i++)); do
  CODE=$(http_code http://127.0.0.1/)
  [[ $CODE == 503 ]] && break
  sleep 1
done
[[ $CODE == 503 ]] || { journalctl --no-pager -u supavise.service | tail -30 >&2; fail "a fenced node answers $CODE, want 503"; }
curl -s --max-time 5 http://127.0.0.1/ | grep -q 'fenced' || fail "the 503 does not say why"
[[ $(http_code http://127.0.0.1:7000/v1/projects) != 401 ]] || fail "a fenced node serves the Management API"
for ((i = 0; i < 30; i++)); do
  [[ $(unit_state "$PGU") != active && ! -e $RUN ]] && break
  sleep 1
done
[[ $(unit_state "$PGU") != active ]] || fail "a fenced node left a project's Postgres running"
[[ ! -e $RUN ]] || fail "a fenced node left the run script of a project: systemd would start it again at boot"
[[ $(unit_state supavise-postgres@system.service) != active ]] || fail "a fenced node left the system cluster running"
OUT=$(supavise status 2>&1 || true)
grep -q '^cluster   FENCED' <<<"$OUT" || fail "status does not show FENCED: $OUT"
journalctl --no-pager -u supavise.service | grep -q 'fenced and starts no primary' || fail "the journal does not say the node is fenced"
log "cluster identity: ok"
