# internal/proxy

The public edge of `sbctl`: HTTPS on :443 and ACME on :80, host to project routing, `apikey` handling and HTTP/WebSocket reverse proxying. It replaces the self-hosted Envoy or Kong gateway and follows its behavior route by route.

```go
srv, err := proxy.New(proxy.Options{
    Config: cfg, Registry: reg, Keys: lifecycleManager, // anything with Keys(ctx, ref)
    APIHandler: managementAPI,                          // served at api.<domain>
    Waker: nil,                                         // idle-wake hook; nil = no-op
})
err = srv.Run(ctx) // or srv.Serve(ctx, httpLn, httpsLn); srv.Handler() for tests
```

## Routing

`Host` decides the target: `api.<domain>` is the in-process Management API (except `/auth/v1/*`, below), `studio.<domain>` is Studio on `127.0.0.1:Ports.Studio`, `<ref>.api.<domain>` (or a `routes` row of the registry) is a project. Anything else is `404`. The `system` project is never reachable.

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
| `/functions/v1/*` | edge runtime, `X-Sbctl-Project-Ref` set; `503` JSON unless `FunctionsEnabled` | open, runtime verifies JWTs |

The table is `routes.go`; its sources are the Envoy template `docker/volumes/api/envoy/lds.template.yaml` in `supabase/supabase` and the stack proxy in `supabase/cli` `packages/stack`. Paths are normalized before matching (`..`, `//`, escaped slashes are rejected or collapsed), responses carry one CORS policy (the project API allows any origin; the keys are the access control), and every request gets an `X-Request-Id`. Spoofable `X-Forwarded-*`, `X-Real-Ip` and `X-Sbctl-Project-Ref` headers from clients are replaced.

## Studio and the dashboard's GoTrue

- `api.<domain>/auth/v1/*` goes to the system project's GoTrue (`Ports.SystemGoTrue`, prefix stripped): it is Studio's `NEXT_PUBLIC_GOTRUE_URL` and the `API_EXTERNAL_URL` that GoTrue is configured with. No apikey is needed (gotrue-js in Studio sends none). CORS is limited to the dashboard's origins (the public dashboard URL and `[api] allowed_origins`, credentials allowed); any other origin gets no CORS headers and its preflight is refused. `..` cannot step out of the prefix.
- `GET studio.<domain>/api/incident-banner` is answered by the proxy with `{"incidents":[]}`. Studio's own route asks incident.io, answers 500 without a key and is retried after 1, 4 and 16 seconds while the sign-in form awaits it (22 seconds in the spike, about one second with the static answer). Doing it here keeps the Studio artifact at its three patches and nothing else.
- Studio's Content-Security-Policy is passed through with every `usercentrics.eu` source removed. Studio calls `api.usercentrics.eu/settings//latest/languages.json` on every page load even with no ruleset id configured (the answer is a 403 and it fails soft); the CSP now makes the browser refuse that request, so no third party is contacted. The browser console logs the refusal. Proposed upstream: skip initialization when `NEXT_PUBLIC_USERCENTRICS_RULESET_ID` is unset.

## Keys

The keys are the project's active opaque keys (`secrets.ProjectKeys.OpaqueKeys`: the default pair unless revoked, plus the ones created through `/v1/projects/{ref}/api-keys`), the legacy JWTs unless `LegacyDisabled`, and the dashboard's short-lived temporary key (`sbctl_tmp` claim), which stays valid with the legacy keys off. `apikey` header, else `?apikey=` (removed from the forwarded query; other parameters are forwarded byte for byte), else an `Authorization: Bearer sb_...` value. `sb_publishable_*` becomes the project's anon JWT and `sb_secret_*` its service_role JWT, in `apikey` and, unless the caller sent a real JWT, in `Authorization`. Legacy keys are accepted (unless disabled) when they equal the project's anon or service_role key or are HS256 JWTs signed with the project secret with role `anon` or `service_role`. With the legacy keys disabled, the proxy first refuses (`401` with a JSON `Invalid API key` message, `403` with an XML `AccessDenied` document on the S3 route) any request to that project's host that carries the signing input of its anon or service_role key, the `<header>.<payload>` of each (what `legacyNeedles` uses with a trailing dot), anywhere in the values of any request header (every value, not just the first) or in the raw query string, undecoded and percent-decoded (`legacyguard.go`). This runs before any route-specific rule, on every route of the project host (REST, GraphQL, Auth including admin, Storage including S3, Realtime including `/api`, Functions), so how an upstream service happens to read a JWT (a bearer with any scheme and spacing, a bare token, a duplicate header, an unrelated header, the query) does not matter. Publishable and secret keys, users' session JWTs (same signing secret, other payload) and S3 access keys are untouched. A CORS preflight is answered by the proxy and forwards nothing, so it is not checked. When the key lookup fails on an open route the request is forwarded as before (see below), unchecked. The route-specific rules that follow stay as a second line. With the legacy keys disabled, the switch also covers `/functions/v1` (the runtime would accept a legacy JWT in `apikey` itself, so the proxy answers `401 Invalid API key` for one) and the exact anon or service_role key sent as the `Authorization` bearer of a protected route. Storage's S3 endpoint (`/storage/v1/s3`) accepts any JWT of the project as the S3 session token (the `X-Amz-Security-Token` header or presigned query parameter), so with the legacy keys off a legacy key there is refused with `403` and an S3-style XML `AccessDenied` document before Storage sees the request; the signed request is never rewritten, S3 access keys and user sessions as session token keep working, and browser POST (multipart) uploads, whose token sits in a form field the proxy does not parse, are refused as well while the switch is off. Realtime reads a JWT from inside the socket as well (the `access_token` of a `phx_join` payload or of an `access_token` event), so for a `/realtime/v1` WebSocket opened while the legacy keys are off the proxy unmasks the client-to-server text frames (`wsguard.go`) and, when one carries a legacy key, tears the connection down (a close frame, `1008` "legacy API keys are disabled for this project", is sent first when the last byte relayed to the client ended a frame, which the proxy tracks; otherwise the connection is just closed) without forwarding the rest of that frame. A legacy key is recognized by its signing input everywhere (bearer, `apikey`, S3 session token, socket): a token is the project's anon or service_role key when its `<header>.<payload>` segments equal those of the key, whatever follows. The signature is a base64url string that has other spellings for the same bytes (the unused low bits of its last character, `=` padding), which verifiers upstream accept, so comparing the whole key text would let a re-encoded signature past the switch; in a socket the needle is `<header>.<payload>.` of each key. Frames are never buffered: the payload is checked in a small scratch buffer with JSON string escapes decoded, keys split over reads, frames or fragments are still found, and everything else streams untouched. Binary frames are not inspected (Realtime takes only broadcast pushes in binary), `Sec-WebSocket-Extensions` is not forwarded and a frame with a reserved bit ends the connection (compression would hide the text). Long poll, Realtime's other transport, cannot be inspected the same way: a Phoenix long-poll session is a process on the Realtime side that keeps the credentials it connected with and is resumed by a token (`?token=` or the `x-phoenix-longpoll-token` header) on later `GET` and `POST` requests, so a session opened with a legacy key would go on using it after the switch. So **while a project's legacy keys are disabled its Realtime route is WebSocket-only**: the proxy accepts only a real WebSocket handshake (a `GET` with `Connection: upgrade` and `Upgrade: websocket`, no body) to exactly `/realtime/v1/websocket`, and refuses every other request on `/realtime/v1` (long-poll `GET` and `POST` in any form, with a token in any spelling, a stale token or none, with or without a body or a key, any other method, any other subpath, the bare prefix) with `403` and the JSON message `Realtime long-polling is unavailable while legacy API keys are disabled; use WebSocket`, before anything reaches Realtime. Sessions opened while the keys were enabled die at the switch because every later request they make is refused; the proxy keeps no long-poll state. `/realtime/v1/api/*` is not long poll and is unaffected. The limitation is plain: **long poll does not work on a project with its legacy keys disabled**, and a client that falls back to long poll must use WebSocket. When the legacy keys are enabled again, long poll works as before. A socket opened while the legacy keys were still on is not inspected, so the proxy tracks it per project and closes it when that project's keys change to legacy-disabled (the key cache sees the change, and the periodic reload checks again); the client reconnects through the guarded path. The tracking is in memory: sockets opened before a proxy restart are not closed, and a request already in flight when the switch lands finishes. A JWT re-signed with the project secret is not looked for in a socket (the legacy keys are, as that is what a leak looks like). Other JWTs signed with the project's JWT secret stay valid in `Authorization` (GoTrue signs users' sessions with it) until the secret is rotated, as on hosted. A `ref` claim is optional (self-hosted keys made from the docs have none); when present it must equal the project ref. The signature already ties the token to the project. Open routes (GoTrue verify and callback, public storage objects) need no key, so a failing key lookup does not take them down: they are forwarded with the credentials untouched (a protected route still fails closed with 503), and the project status gate still applies. A missing or invalid key on a protected route is `401 Unauthorized` (text/plain, as upstream); a valid anon key on an admin route is `403 RBAC: access denied`. Comparisons are constant time.

## Caches

Host to project and project keys are in memory. They follow registry changes (`Subscribe`): project insert, update and delete, route changes, and `project_secrets` changes (key rotation) drop the cached keys. When the change stream closes the proxy resubscribes with backoff and reloads everything. Keys also expire after five minutes and the host table is reloaded every five minutes, because LISTEN/NOTIFY delivery is best effort.

## TLS

`tls.mode` resolves against `domain`, `public_ip` and `dns_provider` (`resolveTLSMode`):

| Effective mode | Certificates |
|---|---|
| `dns01` | `*.api.<domain>`, `api.<domain>`, `studio.<domain>` by DNS-01 only; route53 (instance role or keys), cloudflare, hetzner, digitalocean |
| `auto` (provider and domain set) | the same, plus on-demand HTTP-01 or TLS-ALPN-01 for registry routes with custom hostnames, from a second CertMagic config |
| `http01` (no provider, or forced) | `api.`, `studio.` at startup (obtained explicitly, CertMagic's `ManageAsync` only allowlists them when on-demand issuance is on); project hosts on first handshake |
| `http01` on `<ip>.sslip.io` (no domain) | same, no wildcard possible |
| `off` | plain HTTP on both listeners (tests, or a TLS-terminating front end) |

CertMagic uses DNS-01 exclusively for any issuer that has a DNS solver, so `auto` runs two configs on one certificate cache: one with the DNS solver for `*.api.<domain>`, `api.` and `studio.`, one without it for custom hostnames, which is the only one with on-demand issuance. The TLS handshake picks the config by SNI; renewals pick it by the certificate's names; :80 answers the HTTP-01 issuer's challenges.

On-demand issuance is gated by `allowHost`: only `api.`, `studio.`, derived project hosts of registered projects that are servable (not `REMOVED`, `INIT_FAILED`, `INACTIVE`, so a dead project cannot spend the CA's rate limit) and registry routes qualify, and never a host the wildcard already covers. Provider credentials come from `[tls] credentials` or `SBCTL_TLS_CREDENTIALS_<KEY>` (`api_token`; route53 optional `region`, `hosted_zone_id`, `access_key_id`, `secret_access_key`). `tls.ca` selects another ACME directory and `tls.ca_cert` the root that signs that directory's own HTTPS certificate (Pebble, private CAs). Certificates live in `Paths.Certs()`. Running with TLS enabled accepts the CA's subscriber agreement.

`sbctl proxy` runs the edge alone for development. It reads the existing master key (`key_path`) and never creates one; prefer `SBCTL_REGISTRY_DSN` over `--registry-dsn`, which shows up in the process list.

## Tests

```
go test ./internal/proxy/                       # unit and httptest tests, no network
SBCTL_TEST_DATABASE_URL=postgres://... go test -run TestPostgresRegistry ./internal/proxy/
internal/proxy/pebble-test.sh                   # CI only: real ACME against Pebble (the `pebble` job of the linux workflow)
```

The unit tests cover every route and rewrite against `httptest` upstreams, the key translation table, query stripping, CORS, spoofed headers, streaming, upstream failures, the Waker hook, project status gating, WebSocket passthrough, cache invalidation, resubscribe, the TLS mode and issuance decision logic, certificate manager configuration and the :80 handler. `TestPostgresRegistry` needs a role that may create databases in a throwaway cluster, runs in a database of its own (created and dropped by the test, so it cannot collide with other packages sharing `SBCTL_TEST_DATABASE_URL`) and exercises real LISTEN/NOTIFY, including a killed LISTEN connection.

## Not done

- Idle sleep: only the `Waker.Start(ctx, ref)` call exists; it runs after authorization and before forwarding.
- Edge Functions: the route and tenant header are served by the Edge Runtime and its main service (`functions-main/`, `internal/fleet/edgeruntime.go`); `Options.FunctionsEnabled` is `cfg.Functions.Enabled`, which `sbctl functions dev` forces and `sbctl serve` passes (`internal/app/serve.go`, see `internal/functions/README.md`). A client-supplied `X-Sbctl-Project-Ref` or `X-Sbctl-Proxy-Token` is replaced by the proxy's own on this route and removed on every other (`TestFunctions`, and `tests/functions/verify.mjs` against the real runtime). The token is the node's proxy secret (`config.LoadFunctionsProxyToken`, read on the first `/functions/v1` request, or `Options.FunctionsProxyToken`): the runtime's port is reachable from the function workers it runs, so the main service serves only callers that have it.
- DNS-01 against a real provider and HTTP-01 against a real CA have not been run; the Pebble test covers HTTP-01 and TLS-ALPN-01 in CI only.
- The `/pg/` (postgres-meta) and `/mcp` routes of the self-hosted gateway are not exposed; Studio reaches pg-meta through the Management API.
- No rate limiting, request size limits or access log persistence beyond slog.
