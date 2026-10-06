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

`Host` decides the target: `api.<domain>` is the in-process Management API, `studio.<domain>` is Studio on `127.0.0.1:Ports.Studio`, `<ref>.api.<domain>` (or a `routes` row of the registry) is a project. Anything else is `404`. The `system` project is never reachable.

| Client path | Upstream | Access |
|---|---|---|
| `/rest/v1/*` | project PostgREST, prefix stripped | key |
| `/rest/v1/` (OpenAPI document) | same | service_role only |
| `/graphql/v1` | PostgREST `/rpc/graphql`, `Content-Profile: graphql_public` forced | key |
| `/auth/v1/*` | project GoTrue, prefix stripped | key |
| `/auth/v1/{verify,callback,authorize,.well-known/jwks.json,sso/saml/acs,sso/saml/metadata}`, `/.well-known/oauth-authorization-server` | GoTrue | open |
| `/realtime/v1/api/*` | Realtime `/api/*`, `Host: <ref>.realtime.internal` | key |
| `/realtime/v1/*` (WebSocket and long poll) | Realtime `/socket/*`, same Host, `x-api-key` set, `apikey` query rewritten | key |
| `/realtime/v1/api/tenants*`, `/realtime/v1/api/openapi*` | never forwarded | `403` |
| `/storage/v1/*`, `/storage/v1/s3/*` | Storage, `x-forwarded-host: <ref>.api.<domain>` | open (S3 SigV4 untouched) |
| `/functions/v1/*` | edge runtime, `X-Sbctl-Project-Ref` set; `503` JSON unless `FunctionsEnabled` | open, runtime verifies JWTs |

The table is `routes.go`; its sources are the Envoy template `docker/volumes/api/envoy/lds.template.yaml` in `supabase/supabase` and the stack proxy in `supabase/cli` `packages/stack`. Paths are normalized before matching (`..`, `//`, escaped slashes are rejected or collapsed), responses carry one CORS policy (the project API allows any origin; the keys are the access control), and every request gets an `X-Request-Id`. Spoofable `X-Forwarded-*`, `X-Real-Ip` and `X-Sbctl-Project-Ref` headers from clients are replaced.

## Keys

`apikey` header, else `?apikey=` (removed from the forwarded query; other parameters are forwarded byte for byte), else an `Authorization: Bearer sb_...` value. `sb_publishable_*` becomes the project's anon JWT and `sb_secret_*` its service_role JWT, in `apikey` and, unless the caller sent a real JWT, in `Authorization`. Legacy keys are accepted when they equal the project's anon or service_role key or are HS256 JWTs signed with the project secret with role `anon` or `service_role` and a matching `ref` claim. A missing or invalid key on a protected route is `401 Unauthorized` (text/plain, as upstream); a valid anon key on an admin route is `403 RBAC: access denied`. Comparisons are constant time.

## Caches

Host to project and project keys are in memory. They follow registry changes (`Subscribe`): project insert, update and delete, route changes, and `project_secrets` changes (key rotation) drop the cached keys. When the change stream closes the proxy resubscribes with backoff and reloads everything. Keys also expire after five minutes and the host table is reloaded every five minutes, because LISTEN/NOTIFY delivery is best effort.

## TLS

`tls.mode` resolves against `domain`, `public_ip` and `dns_provider` (`resolveTLSMode`):

| Effective mode | Certificates |
|---|---|
| `dns01` | `*.api.<domain>`, `api.<domain>`, `studio.<domain>` by DNS-01 only; route53 (instance role or keys), cloudflare, hetzner, digitalocean |
| `auto` (provider and domain set) | the same, plus on-demand HTTP-01 or TLS-ALPN-01 for registry routes with custom hostnames |
| `http01` (no provider, or forced) | `api.`, `studio.` at startup; project hosts on first handshake |
| `http01` on `<ip>.sslip.io` (no domain) | same, no wildcard possible |
| `off` | plain HTTP on both listeners (tests, or a TLS-terminating front end) |

On-demand issuance is gated by `allowHost`: only `api.`, `studio.`, derived project hosts of registered projects and registry routes qualify, and never a host the wildcard already covers. Provider credentials come from `[tls] credentials` or `SBCTL_TLS_CREDENTIALS_<KEY>` (`api_token`; route53 optional `region`, `hosted_zone_id`, `access_key_id`, `secret_access_key`). `tls.ca` selects another ACME directory and `tls.ca_cert` the root that signs that directory's own HTTPS certificate (Pebble, private CAs). Certificates live in `Paths.Certs()`. Running with TLS enabled accepts the CA's subscriber agreement.

`sbctl proxy --registry-dsn ...` runs the edge alone for development.

## Tests

```
go test ./internal/proxy/                       # unit and httptest tests, no network
SBCTL_TEST_DATABASE_URL=postgres://... go test -run TestPostgresRegistry ./internal/proxy/
internal/proxy/pebble-test.sh                   # CI only: real ACME against Pebble
```

The unit tests cover every route and rewrite against `httptest` upstreams, the key translation table, query stripping, CORS, spoofed headers, streaming, upstream failures, the Waker hook, project status gating, WebSocket passthrough, cache invalidation, resubscribe, the TLS mode and issuance decision logic, certificate manager configuration and the :80 handler. `TestPostgresRegistry` needs an empty database in a throwaway cluster and exercises real LISTEN/NOTIFY, including a killed LISTEN connection.

## Not done

- Idle sleep: only the `Waker.Start(ctx, ref)` call exists; it runs after authorization and before forwarding.
- Edge Functions: the route and tenant header exist, the runtime does not (phase 2).
- DNS-01 against a real provider and HTTP-01 against a real CA have not been run; the Pebble test covers HTTP-01 and TLS-ALPN-01 in CI only.
- The `/pg/` (postgres-meta) and `/mcp` routes of the self-hosted gateway are not exposed; Studio reaches pg-meta through the Management API.
- No rate limiting, request size limits or access log persistence beyond slog.
