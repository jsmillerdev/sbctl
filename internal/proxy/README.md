# internal/proxy

The public edge of `supavise`: HTTPS on :443 and ACME on :80, host to project routing, `apikey` handling and HTTP/WebSocket reverse proxying. It replaces the self-hosted Envoy or Kong gateway and follows its behavior route by route.

```go
srv, err := proxy.New(proxy.Options{
    Config: cfg, Registry: reg, Keys: lifecycleManager, // anything with Keys(ctx, ref)
    APIHandler: managementAPI,                          // served at api.<domain>
    Waker: nil,                                         // idle-wake hook; nil = no-op
})
err = srv.Run(ctx) // or srv.Serve(ctx, httpLn, httpsLn); srv.Handler() for tests
```

## Routing

`Host` decides the target: `api.<domain>` is the in-process Management API (except `/auth/v1/*`, below), `studio.<domain>` is Studio on `127.0.0.1:Ports.Studio`, `<ref>.api.<domain>` (or a `routes` row of the registry) is a project. Two more hosts exist while a project has a read replica: `<identifier>.api.<domain>` is that replica's Data API and `<ref>-lb.api.<domain>` is the project's load balancer (below). Anything else is `404`. The `system` project, and the standby of its cluster, are never reachable.

**Custom hostnames and vanity subdomains** (`internal/domains`) are `routes` rows, kind `custom` (a customer's hostname, written when it is activated) and `vanity` (`<name>.api.<domain>`). They reach the project like its derived host (same keys and routes; Storage always gets `x-forwarded-host: <ref>.api.<domain>`, because its tenant regexp needs a ref). The client's own host goes in `X-Supavise-Client-Host`, which Storage's SigV4 check reads (`S3_PROTOCOL_NON_CANONICAL_HOST_HEADER`, set by the fleet) so S3 clients that signed the custom hostname verify; REST and Auth see it in `X-Forwarded-Host`, and GoTrue builds its links from `API_EXTERNAL_URL`. A vanity row may claim a `<name>.api.<domain>` host only when `<name>` is a valid vanity name (not ref-shaped, not reserved), and the derived host always wins, so no route can shadow a project.

| Client path | Upstream | Access |
|---|---|---|
| `/rest/v1/*` | project PostgREST, prefix stripped | key |
| `/rest/v1/` (OpenAPI document) | same | service_role only |
| `/graphql/v1` | PostgREST `/rpc/graphql`, `Content-Profile: graphql_public` forced | key |
| `/auth/v1/*` | project GoTrue, prefix stripped | key |
| `/auth/v1/{verify,callback,authorize,.well-known/jwks.json,sso/saml/acs,sso/saml/metadata}`, `/.well-known/oauth-authorization-server` | GoTrue | open |
| `/realtime/v1/api/*` | Realtime `/api/*`, `Host: <ref>.realtime.internal` | key |
| `/realtime/v1/*` (WebSocket and long poll) | Realtime `/socket/*`, same Host, `x-api-key` set, `apikey` query rewritten | key; WebSocket only (`/realtime/v1/websocket`) while the legacy keys are disabled, anything else `403` |
| `/realtime/v1/api/tenants*`, `/realtime/v1/api/openapi*` | never forwarded | `403` |
| `/storage/v1/*`, `/storage/v1/s3/*` | Storage, `x-forwarded-host: <ref>.api.<domain>` | open (S3 SigV4 untouched; legacy session token refused while the legacy keys are off) |
| `/functions/v1/*` | edge runtime, `X-Supavise-Project-Ref` set; `503` JSON unless `FunctionsEnabled` | open, runtime verifies JWTs |

The table is `routes.go` (sources: the Envoy template `docker/volumes/api/envoy/lds.template.yaml` in `supabase/supabase`, and the stack proxy in `supabase/cli` `packages/stack`). Paths are normalized before matching (`..`, `//`, escaped slashes are rejected or collapsed), responses carry one CORS policy (the project API allows any origin; the keys are the access control), and every request gets an `X-Request-Id`. Client-sent `X-Forwarded-*`, `X-Real-Ip` and `X-Supavise-Project-Ref` headers are replaced.

Edge Functions: `Options.FunctionsEnabled` is `cfg.Functions.Enabled` (`internal/functions/README.md`). On `/functions/v1` the proxy replaces a client-supplied `X-Supavise-Project-Ref` or `X-Supavise-Proxy-Token` with its own, and removes both on every other route. The token is the node's proxy secret (`config.LoadFunctionsProxyToken`, or `Options.FunctionsProxyToken`); the runtime's port is reachable from the function workers it runs, so the main service serves only callers that have it.

## Read replicas and the load balancer

A replica's PostgREST listens on `127.0.0.1:<replica port>` (`config.ReplicaPorts`) on every node: it is the replica itself on its node and a forwarder to it everywhere else (`internal/mesh`), so the proxy never needs to know where a replica runs. Any node answers the replica and balancer hosts of any project; DNS decides which node a client reaches.

| Host | Reaches |
|---|---|
| `<identifier>.api.<domain>` | the replica's PostgREST, for `/rest/v1` and `/graphql/v1` only; every other path is `404` `no Route matched with those values` |
| `<ref>-lb.api.<domain>` | `GET` and `HEAD` on `/rest/v1/*`: a database chosen as below; everything else (other methods, `/graphql/v1`, Auth, Storage, Realtime, Functions): the primary |
| custom and vanity hosts | the primary, always |

- **Replica endpoint.** Keys, the OpenAPI document (service_role only), CORS and the legacy-key guard are the project's. Every method goes through to PostgREST, which refuses writes on the standby; the proxy has no method policy. A replica that is `INIT_READ_REPLICA`, `INIT_READ_REPLICA_FAILED` or `GOING_DOWN` answers `503`; the other statuses are passed to PostgREST. A project that is not servable answers `503` as on its own host.
- **Load balancer.** It exists exactly while the project has a replica row, in any status. The candidates are the primary and every replica that is `ACTIVE_HEALTHY` and runs on an active node; with `[replicas] lb_max_lag_seconds` above zero, a replica whose lag is above the limit, or unknown, is left out. The pick is the database on the node that received the request; otherwise the one on the node with the lowest round-trip time in the peer heartbeats (a node with no reading is passed over); otherwise the candidates in turn, per project. So a balancer host that DNS resolves to the primary's node always answers from the primary: point `<ref>-lb` at the nodes (latency-routed records) to spread reads.
- **Evidence.** Every response from a balancer host carries `X-Supavise-Route: <identifier>` (the ref for the primary), and the access log line of a request on it has `load_balancer_redirect_identifier` with the same value. A replica endpoint sets neither.
- **Failures.** A replica that stops answering between two status reports gives `502` until its status changes; the balancer does not retry on the primary.

`proxy.Options.Cluster` (`Cluster`) carries what the balancer asks of the cluster: this node's id, the heartbeat round-trip times (`mesh.Mesh.RTT`) and the replicas' lag (`ReplicaLag` over `replicas.Service`, which only the leader can answer). A server on its own leaves it nil.

## Studio and the dashboard's GoTrue

- `api.<domain>/auth/v1/*` goes to the system project's GoTrue (`Ports.SystemGoTrue`, prefix stripped): it is Studio's `NEXT_PUBLIC_GOTRUE_URL` and the `API_EXTERNAL_URL` GoTrue is configured with. No apikey is needed (gotrue-js in Studio sends none). CORS is limited to the dashboard's origins (the public dashboard URL and `[api] allowed_origins`, credentials allowed); any other origin gets no CORS headers and its preflight is refused.
- `GET studio.<domain>/api/check-cname?domain=<host>` is answered by the proxy (`studio_cname.go`). Studio's Custom Domains page asks it before a setup (Studio's own version asks Cloudflare's DNS-over-HTTPS). The proxy answers in the same JSON shape from the node's resolver (`Options.Resolver`): a CNAME or an A or AAAA record is an `Answer`, nothing is `Status` 3, an invalid name is 400. The route is unauthenticated, as Studio's is, and answers at most 20 lookups a minute per client address and 300 for the node.
- `GET studio.<domain>/api/incident-banner` is answered by the proxy with `{"incidents":[]}`, always. Studio's own route asks incident.io and answers 500 without a key, which delays the sign-in form; and this Studio build draws any incident as an outage, so the operator's notices stay in `supavise status` and `/healthz/detail` (`internal/notice`).
- Studio's Content-Security-Policy is passed through with every `usercentrics.eu` source removed. Studio calls `api.usercentrics.eu` on every page load even with no ruleset id configured (a 403 that fails soft); the CSP makes the browser refuse that request, so no third party is contacted, and the console logs the refusal.

## Keys

**Accepted keys.** The project's active opaque keys (`secrets.ProjectKeys.OpaqueKeys`: the default pair unless revoked, plus those created through `/v1/projects/{ref}/api-keys`), the legacy JWTs unless `LegacyDisabled`, and the dashboard's short-lived temporary key (`supavise_tmp` claim), which stays valid with the legacy keys off. The key comes from the `apikey` header, else `?apikey=` (removed from the forwarded query; other parameters are forwarded byte for byte), else an `Authorization: Bearer sb_...` value. `sb_publishable_*` becomes the project's anon JWT and `sb_secret_*` its service_role JWT, in `apikey` and, unless the caller sent a real JWT, in `Authorization`. A legacy key equals the project's anon or service_role key or is an HS256 JWT signed with the project secret with role `anon` or `service_role`; a `ref` claim is optional and, when present, must equal the project ref.

**Failures.** A missing or invalid key on a protected route is `401 Unauthorized` (text/plain, as upstream); a valid anon key on an admin route is `403 RBAC: access denied`. Comparisons are constant time. A failing key lookup does not take down open routes (GoTrue verify and callback, public storage objects): they are forwarded with the credentials untouched, while a protected route fails closed with 503. The project status gate still applies.

**With the legacy keys disabled** (`PUT .../api-keys/legacy?enabled=false`, see `internal/api/README.md`), the proxy refuses a legacy key wherever an upstream might read it. Other JWTs signed with the project's JWT secret (users' sessions) stay valid in `Authorization` until the secret is rotated, as on hosted.

- **Guard** (`legacyguard.go`): any request to the project host that carries the signing input of its anon or service_role key (the `<header>.<payload>`, whatever follows) in any header value or the raw query (undecoded and percent-decoded) gets `401` with a JSON `Invalid API key` message, or `403` with an XML `AccessDenied` document on the S3 route. It runs before any route-specific rule on every route of the project host, so how an upstream reads a JWT does not matter, and it matches the signing input, not the whole key, because a signature has other spellings that verifiers accept. Publishable and secret keys, users' session JWTs and S3 access keys are untouched; a CORS preflight is not checked; a failed key lookup on an open route forwards the request unchecked.
- **Functions:** a legacy JWT in `apikey` gets `401 Invalid API key` (the runtime would accept it), and so does the exact anon or service_role key as the bearer of a protected route.
- **Storage S3** accepts any JWT of the project as the session token (`X-Amz-Security-Token` or presigned query), so a legacy key there gets `403` and an XML `AccessDenied` document. The signed request is never rewritten, S3 access keys and user sessions keep working, and browser POST (multipart) uploads, whose token sits in a form field the proxy does not parse, are refused while the switch is off.
- **Realtime WebSocket:** Realtime also reads a JWT from inside the socket (`phx_join` or `access_token` payloads). While the keys are off, the proxy unmasks client-to-server text frames (`wsguard.go`) and, when one carries a legacy key (`<header>.<payload>.`), closes the connection without forwarding the rest of that frame, sending close `1008` "legacy API keys are disabled for this project" first when the last relayed byte ended a frame. Frames are never buffered (a small scratch buffer with JSON escapes decoded finds split keys). Binary frames are not inspected (Realtime takes only broadcast pushes in binary), `Sec-WebSocket-Extensions` is not forwarded and a reserved bit ends the connection. A JWT re-signed with the project secret is not looked for in a socket: the guard matches only the legacy keys' signing input, as that is what a leak looks like.
- **Realtime long poll does not work** while a project's legacy keys are disabled: a long-poll session keeps the credentials it connected with, so one opened with a legacy key would keep using it. The proxy accepts only a WebSocket handshake (`GET`, `Connection: upgrade`, `Upgrade: websocket`, no body) to exactly `/realtime/v1/websocket` and refuses every other request on `/realtime/v1` with `403` and `Realtime long-polling is unavailable while legacy API keys are disabled; use WebSocket`. `/realtime/v1/api/*` is unaffected, and long poll works again when the legacy keys are enabled.
- **Open sockets:** a socket opened while the legacy keys were on is not inspected, so the proxy tracks it per project and closes it when the project's keys change to legacy-disabled; the client reconnects through the guarded path. The tracking is in memory: sockets opened before a proxy restart are not closed, and a request in flight when the switch lands finishes.

## Caches

Host to project, each project's home node and replicas, the state of every node, and project keys are in memory and follow registry changes (`Subscribe`): project, route, node and replica changes, and `project_secrets` changes (key rotation), update or drop the cached entries. A registry opened read-only (a follower's) cannot tell which row changed; it sends `Op: "reload"` with no key for each table when the cluster's change counter moves, and the table reads that table again. When the change stream closes the proxy resubscribes with backoff and reloads everything. Keys also expire after five minutes and the host table is reloaded every five minutes, because LISTEN/NOTIFY delivery is best effort.

## TLS

`tls.mode` resolves against `domain`, `public_ip` and `dns_provider` (`resolveTLSMode`):

| Effective mode | Certificates |
|---|---|
| `dns01` | `*.api.<domain>`, `api.<domain>`, `studio.<domain>` by DNS-01 only; route53 (instance role or keys), cloudflare, hetzner, digitalocean |
| `auto` (provider and domain set) | the same, plus on-demand HTTP-01 or TLS-ALPN-01 for registry routes with custom hostnames, from a second CertMagic config |
| `http01` (no provider, or forced) | `api.`, `studio.` at startup; project hosts on first handshake |
| `http01` on `<ip>.sslip.io` (no domain) | same, no wildcard possible |
| `off` | plain HTTP on both listeners (tests, or a TLS-terminating front end) |

CertMagic uses DNS-01 exclusively for any issuer that has a DNS solver, so `auto` runs two configs on one certificate cache: one with the DNS solver for `*.api.<domain>`, `api.` and `studio.`, one without it for custom hostnames (the only one with on-demand issuance). The handshake picks the config by SNI, renewals by the certificate's names; :80 answers HTTP-01 challenges.

On-demand issuance is gated by `allowHost`: only `api.`, `studio.`, derived hosts of servable projects (not `REMOVED`, `INIT_FAILED`, `INACTIVE`, so a dead project cannot spend the CA's rate limit) and registry routes qualify, and never a host the wildcard already covers. A custom hostname has a route only once it is activated, so a claimed or merely verified hostname gets no certificate; in `tls.mode = "dns01"` custom hosts are refused outright. When a custom or vanity route appears the proxy obtains its certificate at once (`warmCertificates`); when it disappears the certificate leaves the cache and storage (`forgetCertificates`). Provider credentials come from `[tls] credentials` or `SUPAVISE_TLS_CREDENTIALS_<KEY>` (`api_token`; route53 optional `region`, `hosted_zone_id`, `access_key_id`, `secret_access_key`). `tls.ca` selects another ACME directory and `tls.ca_cert` the root that signs that directory's own HTTPS certificate (Pebble, private CAs). Certificates live in `Paths.Certs()`. Running with TLS enabled accepts the CA's subscriber agreement.

### On a cluster

The leader obtains and renews every certificate, as a server on its own does. A follower issues none: it mirrors the leader's CertMagic store and serves what it mirrored, so two nodes never ask the CA for the same name, and a custom hostname whose DNS still points at the old server cannot fail HTTP-01 on the new one.

- **Leader side.** `CertsHandler` serves `GET /peer/v1/certs` (registered by `internal/app/wire_proxy.go` with `mesh.Handle`): the files under `certificates/` and `acme/` of the store (the ACME account included) as a `peerapi.CertSnapshot`, with an `ETag` that hashes every path and content. It honors `If-None-Match` and an `etag` query parameter with `304`. It answers only a node that authenticated with its certificate, and only on the leader (`409` `not_leader` otherwise). A certificate it finds half written is left out of the snapshot. Locks, OCSP staples and pending challenge tokens are not offered.
- **Follower side.** `CertSync` fetches the store through `mesh.RPC` (`MeshCerts`) once a minute (`Interval`), and at once when a handshake asks for a name no mirrored certificate covers, at most every ten seconds. It writes the files that differ (0600, atomically), removes the ones the leader no longer has, and loads each complete certificate into CertMagic's cache as an unmanaged one, which CertMagic never renews; a renewed certificate replaces the old one when the next fetch brings it. A file missing from two fetches in a row is removed, not from one; an empty snapshot removes nothing. Paths outside the two trees are refused.
- **Issuing.** While synced, the on-demand decision refuses every name that has no cached certificate, route changes obtain nothing, and no management starts. Nothing reaches the CA.
- **Role.** `CertRole` says managing or synced. `wire_proxy.go` sets it from `cluster.Membership` (the leader manages; a follower or a fenced node is synced) and provides it as `*proxy.CertRole`; `Promote` and `Demote` switch the proxy at once. A promotion loads the mirrored tree as managed certificates and starts management, with the ACME account the tree holds; a demotion drops the managed certificates from the cache and mirrors again. A node without `Options.Cluster.Certs` always manages.

`supavise proxy` runs the edge alone for development. It reads the existing master key (`key_path`) and never creates one; prefer `SUPAVISE_REGISTRY_DSN` over `--registry-dsn`, which shows in the process list.

## Tests

The `ci.yml` job `test` runs the unit and `httptest` tests (`go test ./internal/proxy/`, no network) with a Postgres service. They cover the host classes (replica, balancer, custom, vanity), the replica endpoint's paths, the balancer's rules and its headers and log field, the table's reload operations, the certificate snapshot, the mirror by ETag, and a certificate manager that serves mirrored certificates, refuses to issue while synced and issues through a fake CA after a promotion. `TestPostgresRegistry` and `TestPostgresReadOnlyRegistry` need `SUPAVISE_TEST_DATABASE_URL` and a role that may create databases; each runs in a database of its own. The first exercises real LISTEN/NOTIFY, the second a registry opened read-only that polls the change counter. The `linux.yml` job `pebble` runs `internal/proxy/pebble-test.sh`: real ACME against Pebble, including a customer's custom hostname from initialize to activation, and a follower that serves the leader's certificates and issues after a promotion (`tests/functions/verify.mjs` covers the Functions route against the real runtime).

## Limits

- Idle sleep: only the `Waker.Start(ctx, ref)` call exists; it runs after authorization and before forwarding.
- DNS-01 against a real provider and HTTP-01 against a real CA have not been run; the Pebble job covers HTTP-01 and TLS-ALPN-01.
- The `/pg/` (postgres-meta) and `/mcp` routes of the self-hosted gateway are not exposed; Studio reaches pg-meta through the Management API.
- No rate limiting (beyond `check-cname`), request size limits or access log persistence beyond slog.
- The replica lag the balancer limits on comes from the replica controller, which runs on the leader: on a follower `lb_max_lag_seconds` leaves every replica out and reads go to the primary.
- `api.<domain>` is the in-process Management API on every node; the proxy does not forward it to the leader's admin listener.
