# studio/mock

A stand-in Management API for `studio/spike.sh`. It is not the API server (`internal/api`) and shares no code with it.

```bash
go run ./studio/mock serve -config mock.json        # serve
go run ./studio/mock summarize request-log.jsonl    # markdown table of the calls seen
```

`mock.json` (see `studio/spike.sh`, which writes one): `listen`, `jwt_secret` (GoTrue's JWT secret), `studio_origins`, `pgmeta_url`, `pgmeta_crypto_key` (postgres-meta's `CRYPTO_KEY`), `scheme`, `project_host`, `request_log`, optional `builtin_auth` (a minimal GoTrue token endpoint under `/auth/v1`, for runs without GoTrue), `projects` (`ref`, `name`, `db_url`), and `authorizations` (pending OAuth authorization requests: `id`, `name`, `redirect_uri`, optional `scopes`).

- **Auth.** `/platform`, `/v1` and `/v2` need a bearer token signed HS256 with `jwt_secret` and `aud` containing `authenticated` (the role claim is ignored: GoTrue's `admin createuser` leaves it empty). Anything else answers 401.
- **Real handlers** (`handlers.go`, `data.go`): profile and permissions, organization (one, `mock-org`, enterprise plan, every entitlement granted), both project lists, project detail, status, settings, databases, `config/postgrest`, `config/storage`, API keys, health, branches, and `POST /platform/pg-meta/{ref}/query`.
- **OAuth consent** (`oauth.go`). The three calls behind Studio's `/authorize` page. `GET /platform/oauth/authorizations/{id}` describes a request from `authorizations` (a dynamic client: no icon, no website, the redirect host as its domain, expiry ten minutes ahead; 404 for an unknown id). `POST /platform/organizations/{slug}/oauth/authorizations/{id}` approves it for the organization and answers `201 {"url"}`: the request's redirect URI with `code`, `state` and `iss` added, the answer the real server gives with `skip_browser_redirect`. A second approve is 409, and the request then reads as approved. `DELETE` on the same path declines (removes) a request that has not been approved. `GET /oauth-callback` answers a small page, so the browser has somewhere to land after approving; its query (the code) is not written to the request log. There is no authorization server behind these: no codes are issued or checked.
- **Management API queries.** `POST /v1/projects/{ref}/database/query`, the call the MCP `execute_sql` tool makes, runs the query through postgres-meta like the dashboard's route and answers its rows with 200 (the API answers 201). `read_only` in the body is accepted and not enforced.
- **pg-meta.** The mock builds `x-connection-encrypted` from the project's `db_url` (AES, crypto-js passphrase format, checked against crypto-js 4 in both directions), forwards the body to postgres-meta and returns its status and body unchanged. A browser abort is logged as 499, not 502.
- **Everything else** is answered from `shapes_gen.go`, generated from the three OpenAPI type files in Studio's `packages/api-types` (`gen_shapes.py`, then `gofmt -w`): the documented success status, an empty array, or an object holding the required fields with neutral values (arrays as `[]`, never `null`). Analytics endpoints answer `{"result": []}`. A route in no spec answers `GET {}` and other methods `204`.
- **Log.** One JSON line per request: method, path, matched template, status, handler (`real`, `stub`, `unknown`, `auth`, `preflight`), user, project ref, selected headers, and the first 300 characters of a pg-meta query.
- **Test.** `go test ./studio/mock/` checks every hand-written handler against the required fields in `shapes_gen.go`, the pg-meta proxy against a fake postgres-meta, CORS, auth, the log and the summary.

Not done: no write paths (snippets, config updates, key creation answer from the generic stubs and are not stored), no project lifecycle, no `sbp_` tokens.
