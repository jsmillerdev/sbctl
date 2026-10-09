# Studio platform calls

What the platform-mode Studio asks of the Management API, GoTrue and the project hosts, and which answers it needs. It describes Studio at `supabase/supabase@94b8b06` (`internal/versions/versions.yaml`, `studio.tag` `2026.10.05-sha-94b8b06`), built with `NEXT_PUBLIC_IS_PLATFORM=true`.

To refresh it after a Studio bump, run `studio/spike.sh`: it writes `request-log.jsonl` and `calls.md` into `$SPIKE_OUT`, and the studio mock's `summarize` command prints the rows of calls seen.

## Method

- Scanned every string literal that looks like `/platform/...`, `/v1/...` or `/v2/...` in `apps/studio/{data,lib,components,hooks,state,pages,app,routes}` and `packages/{common,ui,ui-patterns}`, excluding tests, fixtures, comments, `pages/api/**` (Studio's own routes, see "Studio's own /api routes") and scope-map files. Each was joined with the three OpenAPI specs.
- Calls with a computed path (pg-meta `query`, `logs.all[.otel]`, the five `telemetry` calls, `HEAD .../api/rest`) were added by hand.
- Result: 386 (method, path) pairs over 292 paths. The specs hold 446 paths; Studio never calls the other 155. Priorities: P0 breaks sign-in, project list, table editor or SQL editor; P1 degrades a project or organization page; P2 is billing, integrations and other pages outside the P0 flows.
- Statements about what Studio tolerates come from reading the code unless "Findings from running Studio" confirms them.
- Not covered by the scan: GoTrue, calls from the browser straight to a project, and Studio's own `/api/*` routes. Each has a section below.

## Wire contract

- **Base URL.** `NEXT_PUBLIC_API_URL` ends in `/platform` (hosted: `https://api.supabase.com/platform`). The openapi-fetch client strips that suffix and uses absolute paths, so `/platform/...`, `/v1/...` and `/v2/...` all hang off one origin. A few callers use the value as is, and `common` adds the suffix when missing. The Studio artifact's launcher normalizes the configured URL to end in `/platform`. Do not give the API host a name that starts with `platform.`: the client removes the first occurrence of the text `/platform`, and `https://platform.example.com/platform` contains an earlier one.
- **Request headers on every call.** `Authorization: Bearer <GoTrue access token>` (when a session exists), `X-Request-Id`, `Accept: application/json`, `credentials: 'include'`, `referrerPolicy: no-referrer-when-downgrade`. `GET /platform/profile` also sends `Version: 2`. Calls to `/platform/pg-meta/*` add `x-connection-encrypted` (the project's `connectionString`) and `x-pg-application-name` (`supabase/dashboard`, or `supabase/dashboard-query-editor` when the statement timeout is disabled), plus a client-side `?key=<query key>` parameter.
- **`x-connection-encrypted` is untrusted input.** Studio only checks that the project's `connectionString` is a non-empty string (`pgMetaGuard`) and sends it back unchanged. The API server returns an opaque, non-secret token there (random or constant; never the database URL, encrypted or not, because it reaches every dashboard user). On `/platform/pg-meta/{ref}/query` and the other pg-meta routes it derives the postgres-meta connection from the authorized `{ref}` on the server, after checking that the caller may access that project, and never decrypts, parses or forwards the client-supplied header. Otherwise a user of project A could replay A's value on `/platform/pg-meta/{B}/...` and bypass the per-ref check. The mock does this (`studio/mock/pgmeta.go`).
- **CORS.** The browser calls the API cross-origin (`studio.<domain>` to `api.<domain>`) with credentials mode `include` and non-simple headers. Every route needs an `OPTIONS` answer, and responses need `Access-Control-Allow-Origin` echoing the Studio origin (a literal `*` is rejected with credentials), `Access-Control-Allow-Credentials: true`, `Access-Control-Allow-Headers` covering `authorization, content-type, x-request-id, x-connection-encrypted, x-pg-application-name, version`, methods `GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS`, and `Access-Control-Expose-Headers: Retry-After, X-RateLimit-Reset, X-Total-Count`.
- **Success.** Any 2xx counts as success and Studio does not branch on the exact code; match the spec's codes anyway, because the CLI is strict. An empty 2xx body is fine (`204`, or `Content-Length: 0`).
- **Errors.** Any non-2xx is an error. Studio reads a string from the JSON field `message` (or `msg`), takes the code from the HTTP status, and reads `Retry-After` or `X-RateLimit-Reset` for 429. Use the envelope `{"message": "..."}`. Two statuses have behavior attached: a **401 on `GET /platform/profile` signs the user out**, and a `GET /platform/profile` error whose `message` is exactly `User's profile not found` makes Studio `POST /platform/profile` to create the profile.
- **Identifiers.** `{slug}` is the organization slug, `{ref}` the 20-letter project ref. `organization_id` and `id` fields are numbers, so the registry needs a numeric id per organization and project besides the ref.
- **Disable levers.** ConfigCat (`NEXT_PUBLIC_CONFIGCAT_SDK_KEY`, `NEXT_PUBLIC_CONFIGCAT_PROXY_URL`), PostHog (`NEXT_PUBLIC_POSTHOG_KEY`), Sentry (`NEXT_PUBLIC_SENTRY_DSN`) and Usercentrics (`NEXT_PUBLIC_USERCENTRICS_RULESET_ID`) stay off when their `NEXT_PUBLIC_*` keys are unset; the artifact build sets none of them. With `NEXT_PUBLIC_ENVIRONMENT` set to `local` or `staging`, Studio grants telemetry consent automatically, so never set it to either (`studio/build.sh` sets `prod`). `GET /platform/telemetry/feature-flags` is requested on every page and cannot be turned off; an empty object or a 404 is tolerated. Billing screens are hidden by returning `disabled_features: ["billing:all", ...]` from `GET /platform/profile`.

## Dashboard sign-in (GoTrue)

`NEXT_PUBLIC_GOTRUE_URL` points at one `@supabase/auth-js` client (`packages/common/gotrue.ts`; storage key `supabase.dashboard.auth.token`, `localStorage`). These calls go to `supavise-gotrue@system`, not to the Management API. They carry no `apikey` header, so a front that insists on one breaks them. GoTrue answers CORS itself.

| Prio | Call | Used for |
|---|---|---|
| P0 | `POST /token?grant_type=password` | e-mail and password sign-in; the body carries `gotrue_meta_security.captcha_token` only when an hCaptcha key is set |
| P0 | `POST /token?grant_type=refresh_token` | session refresh (runs in the background when the token nears expiry) |
| P0 | `POST /logout` | sign-out |
| P1 | `GET /user` | profile menu and account preferences; also the fallback of auth-js `getClaims` for an HS256 token. The MFA assurance-level check after sign-in is computed locally and sends no request |
| P1 | `GET /.well-known/jwks.json` | `getClaims` when the project signs with an asymmetric key |
| P2 | `POST /recover`, `PUT /user` | forgot password, account preferences |
| P2 | `GET/POST /factors`, `POST /factors/{id}/challenge`, `/verify`, `DELETE /factors/{id}` | MFA enrollment in account settings |
| P2 | `POST /signup`, `/otp`, `/verify`, `/sso`, `/authorize`, `/token?grant_type=id_token` | sign-up, magic link, SSO, GitHub and ChatGPT sign-in; hidden by the `dashboard_auth:*` flags in the Supavise build |

After sign-in, `GET /platform/profile` must accept the GoTrue access token (a 401 signs the user out again).

## The P0 flows

1. **Sign-in page** (`/sign-in`): GoTrue `POST /token?grant_type=password`, then the session lands in `localStorage`. No Management API call happens before login. With the Studio patches the page shows e-mail sign-in only and renders no captcha.
2. **App shell after login:** `GET /platform/profile` (header `Version: 2`; Studio waits for it and the permissions query), `GET /platform/profile/permissions` (gates every action check), `GET /platform/organizations` (starts once the profile has loaded). In parallel and not blocking: `GET /platform/telemetry/feature-flags`, `GET /platform/notifications`, `GET /platform/projects-resource-warnings`.
3. **Project list** (`/organizations`, `/org/{slug}`): `GET /platform/organizations/{slug}`, `GET /platform/organizations/{slug}/projects?limit=96&offset=0&sort=name_asc[&search=&statuses=]` (the cards need `pagination.count` and each project's `databases`, `status`, `ref`, `name`, `region`, `cloud_provider`, `inserted_at`, `is_branch`, `integration_source`), `GET /platform/projects` (project switcher), and the organization's `entitlements`, `billing/subscription`, `usage`, `members` and `roles` (organization pages degrade without them).
4. **Open a project, table editor** (`/project/{ref}/editor`): `GET /platform/projects/{ref}` must return a non-empty `connectionString` (see the wire contract). `status` must be `ACTIVE_HEALTHY`, or Studio shows the connecting screen and polls `GET /platform/projects/{ref}/status` and `HEAD /platform/projects/{ref}/api/rest`; `is_hibernating: true` makes it `POST .../wake` first. Then `GET /platform/projects/{ref}/settings`, and **every table, column, schema, role, policy and row read is `POST /platform/pg-meta/{ref}/query`** with SQL in the body. Also on open: `GET /v1/projects/{ref}/api-keys`, `POST /platform/projects/{ref}/api-keys/temporary`, `GET /v1/projects/{ref}/branches` (return `[]` if there are none), `GET /v1/projects/{ref}/health`, `GET /v2/projects/{ref}/config`, `GET .../config/postgrest`, `GET .../config/storage`, `GET /platform/projects/{ref}/content`, `GET .../analytics/endpoints/usage.api-counts`.
5. **SQL editor** (`/project/{ref}/sql/...`): `POST /platform/pg-meta/{ref}/query` with body `{"query": string, "disable_statement_timeout": boolean}` (an optional `explain <sql>` preflight uses `?key=preflight-check`); snippets use `GET/PUT/POST/DELETE /platform/projects/{ref}/content` and `.../content/folders`. The response must be the JSON array of row objects exactly as pg-meta returns it. Pass pg-meta's status code and error body (`{error, formattedError, ...}`) through unchanged: the editor shows `formattedError` and parses the `LINE n:` text.

Pitfalls in the required fields of the P0 responses (the full shapes are in the generated types):

| Response | Pitfalls |
|---|---|
| `ProfileResponse_Output` | `id` number, `gotrue_id` = GoTrue user id, `auth0_id` string (Studio checks `startsWith('github')` for the avatar), `username`, `primary_email`, `is_alpha_user`, `free_project_limit` (number or null); optional `disabled_features` (string array, merged with the build-time list) |
| `AccessControlPermission[]` | `actions`, `resources`, `condition`, `organization_slug`, `organization_id`, `project_ids`, `project_refs`, `restrictive`. `%` is a wildcard and `condition: null` is unconditional; an empty list denies every action |
| `OrganizationResponse_Output[]` | `plan` is an inline object `{id: 'free'|'pro'|'team'|'enterprise'|'platform', name}`; `restriction_data` and `restriction_status` are `null` when unrestricted; `opt_in_tags` array; `slug` and numeric `id` |
| `OrganizationProjectsResponse_Output` | `{pagination: {count, limit, offset}, projects: [...]}`; each project needs `databases[]` (`identifier`, `region`, `status`, `type: 'PRIMARY'`, `cloud_provider`) and the card fields above |
| `ListProjectsPaginatedResponse_Output` | `{pagination, projects: [...]}`; each project adds `organization_id`, `organization_slug`, `subscription_id`, `is_branch_enabled`, `is_physical_backups_enabled`, `preview_branch_refs` |
| `ProjectDetailResponse_Output` | `connectionString` (see the wire contract), `restUrl`, `db_host`, `status` (enum: `ACTIVE_HEALTHY`, `COMING_UP`, `INACTIVE`, `UNKNOWN`, `GOING_DOWN`, `INIT_FAILED`, `REMOVED`, `RESTORING`, `UPGRADING`, `PAUSING`, `RESTORE_FAILED`, `RESTARTING`, `PAUSE_FAILED`, `RESIZING`, `ACTIVE_UNHEALTHY`), `subscription_id` string, numeric `id` and `organization_id`; optional `is_hibernating`, `dbVersion` |
| `ProjectSettingsResponse_Output` | `db_host`, `db_dns_name`, `db_name`, `db_user`, `db_port`, `ssl_enforced`, `status`, `ref`, `name`, `region`, `cloud_provider`, `inserted_at`; optional but used: `app_config` (endpoint and protocol build the project URL), `jwt_secret`, `service_api_keys` |
| `GET /platform/projects/{ref}/status` | body `{"status": "..."}`; the spec documents no body but Studio reads `data.status` |

## Calls that need a real answer

These are the documented routes for which an empty stub is not enough: the page needs real data, or a mutation must persist or act. Every other documented route gets the spec's empty value from the generic stub (arrays become `[]`, objects their required fields with neutral values).

| Prio | Method | Path | What Studio needs |
|---|---|---|---|
| P1 | GET | `/platform/auth/{ref}/config` | 200 `GoTrueConfigResponse` |
| P1 | PATCH | `/platform/auth/{ref}/config` | 200 `GoTrueConfigResponse` |
| P1 | PATCH | `/platform/auth/{ref}/config/hooks` | 200 `GoTrueConfigResponse` |
| P1 | POST | `/platform/auth/{ref}/invite` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/magiclink` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/otp` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/recover` | 201 no body |
| P1 | POST | `/platform/auth/{ref}/templates/{template}/reset` | 200 `GoTrueConfigResponse` |
| P1 | POST | `/platform/auth/{ref}/users` | 201 `CreateUserResponse_Output` |
| P1 | PATCH | `/platform/auth/{ref}/users/{id}` | 200 `UpdateUserResponse_Output` |
| P1 | DELETE | `/platform/auth/{ref}/users/{id}` | 200 no body |
| P1 | DELETE | `/platform/auth/{ref}/users/{id}/factors` | 200 no body |
| P1 | POST | `/platform/auth/{ref}/validate/spam` | 200 `ValidateSpamResponse_Output` |
| P1 | POST | `/platform/cli/login` | 201 no body |
| P0 | GET | `/platform/organizations` | 200 array of `OrganizationResponse_Output` |
| P0 | GET | `/platform/organizations/{slug}` | 200 `OrganizationSlugResponse_Output` |
| P2 | HEAD | `/platform/organizations/{slug}/billing/invoices` | 200 no body |
| P1 | GET | `/platform/organizations/{slug}/members` | 200 array of `Member_Output` |
| P1 | GET | `/platform/organizations/{slug}/members/invitations` | 200 `InvitationResponse_Output` |
| P1 | POST | `/platform/organizations/{slug}/members/invitations` | 201 inline object |
| P1 | DELETE | `/platform/organizations/{slug}/members/invitations/{id}` | 200 no body |
| P1 | GET | `/platform/organizations/{slug}/members/invitations/{token}` | 200 `InvitationByTokenResponse_Output` |
| P1 | POST | `/platform/organizations/{slug}/members/invitations/{token}` | 201 no body |
| P1 | GET | `/platform/organizations/{slug}/members/mfa/enforcement` | 201 `MfaStatusResponse_Output` |
| P1 | PATCH | `/platform/organizations/{slug}/members/mfa/enforcement` | 201 `MfaStatusResponse_Output` |
| P1 | GET | `/platform/organizations/{slug}/members/reached-free-project-limit` | 200 array of `MemberWithFreeProjectLimit_Output` (requires `free_project_limit`, `primary_email`, `username`) |
| P1 | PATCH | `/platform/organizations/{slug}/members/{gotrue_id}` | 200 no body |
| P1 | DELETE | `/platform/organizations/{slug}/members/{gotrue_id}` | 200 no body |
| P1 | DELETE | `/platform/organizations/{slug}/members/{gotrue_id}/roles/{role_id}` | 200 no body |
| P0 | GET | `/platform/organizations/{slug}/projects` | 200 `OrganizationProjectsResponse_Output` (requires `pagination`, `projects`) |
| P0 | POST | `/platform/pg-meta/{ref}/query` | 200 JSON array of row objects (pg-meta passthrough). Failure: 4xx/5xx JSON `{error, formattedError, ...}` as pg-meta sends it. Spec documents 201 without a body |
| P0 | GET | `/platform/profile` | 200 `ProfileResponse_Output` |
| P0 | POST | `/platform/profile` | 201 `ProfileResponse_Output` |
| P1 | GET | `/platform/profile/access-tokens` | 200 array of `AccessToken_Output` |
| P1 | POST | `/platform/profile/access-tokens` | 201 `CreateAccessTokenResponse_Output` |
| P1 | DELETE | `/platform/profile/access-tokens/{id}` | 200 `AccessToken_Output` |
| P0 | GET | `/platform/profile/permissions` | 200 array of `AccessControlPermission` |
| P1 | GET | `/platform/profile/scoped-access-tokens` | 200 `GetScopedAccessTokensResponse_Output` |
| P1 | POST | `/platform/profile/scoped-access-tokens` | 201 `CreateScopedAccessTokenResponse_Output` |
| P1 | GET | `/platform/profile/scoped-access-tokens/{id}` | 200 `GetScopedAccessTokenResponse_Output` |
| P1 | DELETE | `/platform/profile/scoped-access-tokens/{id}` | 200 no body |
| P0 | GET | `/platform/projects` | 200 `ListProjectsPaginatedResponse_Output` (requires `pagination`, `projects`) |
| P0 | GET | `/platform/projects/{ref}` | 200 `ProjectDetailResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}` | 200 `ProjectRefResponse_Output` (requires `id`, `name`, `ref`) |
| P1 | POST | `/platform/projects/{ref}/api-keys/temporary` | 201 `TemporaryApiKeyResponse_Output` |
| P1 | HEAD | `/platform/projects/{ref}/api/rest` | any 2xx means PostgREST is up; not in the spec (the spec has GET only) |
| P1 | GET | `/platform/projects/{ref}/config/pgbouncer` | 200 `PgbouncerConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/pgbouncer` | 200 `UpdatePoolingConfigResponse_Output` (requires `pgbouncer_enabled`, `pgbouncer_status`) |
| P1 | GET | `/platform/projects/{ref}/config/postgrest` | 200 `GetPostgrestConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/postgrest` | 200 `UpdatePostgrestConfigResponse_Output` |
| P1 | GET | `/platform/projects/{ref}/config/realtime` | 200 `RealtimeConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/realtime` | 204 no body |
| P1 | GET | `/platform/projects/{ref}/config/storage` | 200 `StorageConfigResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/config/storage` | 200 `StorageConfigResponse_Output` |
| P1 | GET | `/platform/projects/{ref}/content` | 200 `GetUserContentResponse_Output` |
| P1 | PUT | `/platform/projects/{ref}/content` | 200 no body |
| P1 | DELETE | `/platform/projects/{ref}/content` | 200 array of `BulkDeleteUserContentResponse_Output` |
| P1 | GET | `/platform/projects/{ref}/content/count` | 200 `GetContentCountV2Response_Output` (requires `favorites`, `private`, `shared`) |
| P1 | GET | `/platform/projects/{ref}/content/folders` | 200 `GetUserContentFolderResponse_Output` |
| P1 | POST | `/platform/projects/{ref}/content/folders` | 201 `CreateUserContentFolderResponse_Output` |
| P1 | DELETE | `/platform/projects/{ref}/content/folders` | 200 no body |
| P1 | GET | `/platform/projects/{ref}/content/folders/{id}` | 200 `GetUserContentFolderResponse_Output` |
| P1 | GET | `/platform/projects/{ref}/content/item/{id}` | 200 `GetUserContentByIdResponse_Output` |
| P1 | PATCH | `/platform/projects/{ref}/db-password` | 200 `UpdatePasswordResponse_Output` |
| P1 | POST | `/platform/projects/{ref}/pause` | 201 no body |
| P1 | POST | `/platform/projects/{ref}/restart` | 201 no body |
| P1 | POST | `/platform/projects/{ref}/restart-services` | 201 no body |
| P1 | POST | `/platform/projects/{ref}/restore` | 200 `UnpauseProjectResponse_Output` |
| P0 | GET | `/platform/projects/{ref}/settings` | 200 `ProjectSettingsResponse_Output` |
| P0 | GET | `/platform/projects/{ref}/status` | 200 `{"status": "<project status>"}` (the spec documents no body; Studio reads `data.status`) |
| P1 | POST | `/platform/projects/{ref}/wake` | 201 `ProjectWakeResponse_Output` (requires `connection_string`, `connection_string_read_only`) |
| P1 | GET | `/platform/storage/{ref}/buckets` | 200 array of `StorageBucketResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets` | 201 no body |
| P1 | GET | `/platform/storage/{ref}/buckets/{id}` | 200 `StorageBucketResponse_Output` |
| P1 | PATCH | `/platform/storage/{ref}/buckets/{id}` | 200 no body |
| P1 | DELETE | `/platform/storage/{ref}/buckets/{id}` | 200 no body |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/empty` | 201 no body |
| P1 | DELETE | `/platform/storage/{ref}/buckets/{id}/objects` | 200 no body |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/list` | 201 array of `StorageObject_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/list-v2` | 201 `StorageListResponseV2_Output` (requires `folders`, `hasNext`, `objects`) |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/move` | 201 no body |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/public-url` | 201 `PublicUrlResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/sign` | 201 `SignedUrlResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/buckets/{id}/objects/sign-multi` | 201 array of `SignedUrlsResponse_Output` (requires `error`, `path`, `signedUrl`) |
| P1 | GET | `/platform/storage/{ref}/credentials` | 200 `GetStorageCredentialsResponse_Output` |
| P1 | POST | `/platform/storage/{ref}/credentials` | 201 `CreateStorageCredentialResponse_Output` |
| P1 | DELETE | `/platform/storage/{ref}/credentials/{id}` | 204 no body |
| P1 | GET | `/v1/projects/{ref}/api-keys` | 200 array of `ApiKeyResponse_Output` |
| P1 | POST | `/v1/projects/{ref}/api-keys` | 201 `ApiKeyResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/api-keys/legacy` | 200 `LegacyApiKeysResponse_Output` |
| P1 | PUT | `/v1/projects/{ref}/api-keys/legacy` | 200 `LegacyApiKeysResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/api-keys/{id}` | 200 `ApiKeyResponse_Output` |
| P1 | DELETE | `/v1/projects/{ref}/api-keys/{id}` | 200 `ApiKeyResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/config/auth/signing-keys` | 200 `SigningKeysResponse_Output` |
| P1 | POST | `/v1/projects/{ref}/config/auth/signing-keys` | 201 `SigningKeyResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/config/auth/signing-keys/legacy` | 200 `SigningKeyResponse_Output` |
| P1 | POST | `/v1/projects/{ref}/config/auth/signing-keys/legacy` | 201 `SigningKeyResponse_Output` |
| P1 | PATCH | `/v1/projects/{ref}/config/auth/signing-keys/{id}` | 200 `SigningKeyResponse_Output` |
| P1 | DELETE | `/v1/projects/{ref}/config/auth/signing-keys/{id}` | 200 `SigningKeyResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/config/auth/third-party-auth` | 200 array of `ThirdPartyAuth_Output` |
| P1 | POST | `/v1/projects/{ref}/config/auth/third-party-auth` | 201 `ThirdPartyAuth_Output` |
| P1 | DELETE | `/v1/projects/{ref}/config/auth/third-party-auth/{tpa_id}` | 200 `ThirdPartyAuth_Output` |
| P1 | GET | `/v1/projects/{ref}/config/database/postgres` | 200 `PostgresConfigResponse_Output` |
| P1 | PUT | `/v1/projects/{ref}/config/database/postgres` | 200 `PostgresConfigResponse_Output` |
| P1 | PUT | `/v1/projects/{ref}/database/migrations` | 200 no body |
| P1 | GET | `/v1/projects/{ref}/functions` | 200 array of `FunctionResponse_Output` |
| P1 | POST | `/v1/projects/{ref}/functions/deploy` | 201 `DeployFunctionResponse_Output` |
| P1 | GET | `/v1/projects/{ref}/functions/{function_slug}` | 200 `FunctionSlugResponse_Output` |
| P1 | PATCH | `/v1/projects/{ref}/functions/{function_slug}` | 200 `FunctionSlugResponse_Output` |
| P1 | DELETE | `/v1/projects/{ref}/functions/{function_slug}` | 200 no body |
| P1 | GET | `/v1/projects/{ref}/functions/{function_slug}/body` | 200 `StreamableFile` |
| P1 | GET | `/v1/projects/{ref}/secrets` | 200 array of `SecretResponse_Output` (requires `name`, `value`) |
| P1 | POST | `/v1/projects/{ref}/secrets` | 201 no body |
| P1 | DELETE | `/v1/projects/{ref}/secrets` | 200 no body |
| P1 | GET | `/v1/projects/{ref}/types/typescript` | 200 `TypescriptResponse_Output` |

## Read replicas

The Infrastructure page, the SQL editor's source selector and the reports read these calls. Supavise answers them from the registry and the replica controller (`internal/api`, `replicas.go`, `databases.go`, `load_balancers.go`, `infra_monitoring.go`). The answers use the shapes of the three specs, with no new fields. A node that has no replica controller lists the primary alone and answers `503` ("Read replicas are not set up on this Supavise server") to setup, remove and the restart of a replica.

| Call | What Studio does with it | What Supavise answers |
|---|---|---|
| `GET /platform/profile` | hides the Infrastructure page, the source selector and the replica rows of the reports when `disabled_features` holds `infrastructure:read_replicas` | The key is in `disabled_features` until a replica controller is wired and two nodes are active. It is a feature flag, not a permission check, and costs one registry read of the nodes per profile request once a controller is wired. |
| `GET /platform/projects/{ref}` | decides whether the Add button is enabled | `dbVersion` is `supabase-postgres-<version>` (a bare version counts as below Postgres 15); `is_physical_backups_enabled` is true when the node has a backup service; `cloud_provider` is `AWS`; `high_availability` is false |
| `GET /platform/organizations/{slug}/projects` | the project cards | `databases[]` has one element per replica, `type: 'READ_REPLICA'`, read for the whole page in one query |
| `GET /platform/projects/{ref}/databases` | the Infrastructure page and the source selector | The primary first, then each replica oldest first. A replica row has the primary's shape: its identifier, the region of its node, `restUrl` `https://<identifier>.api.<domain>/rest/v1/`, `db_host` `db.<identifier>.api.<domain>`, and `connectionString` and `connection_string_read_only` with a `[YOUR-PASSWORD]` placeholder. `size` is the project's size. |
| `GET /platform/projects/{ref}/databases-statuses` | polls until a replica is up | The same databases, the primary included (Studio refetches until the two lists have the same length). A replica has `replicaInitializationStatus`: `in_progress` with the step reached and the estimates, `completed`, or `failed` with the step's error. |
| `GET /platform/projects/{ref}/load-balancers` | the "API Load Balancer" node | `[]` without a replica, otherwise one balancer: `https://<ref>-lb.api.<domain>` and every database with its type and status |
| `POST /v1/projects/{ref}/read-replicas/setup` `{read_replica_region}` | the Add button | `204` once the controller records the replica; it comes up in the background. Owners and Administrators only. A refusal is `400` with a message that Studio shows as it is. |
| `POST /v1/projects/{ref}/read-replicas/remove` `{database_identifier}` | remove a replica | `204`; `404` "Read replica not found" for an identifier that is not one of the project's replicas, the primary's included |
| `POST /platform/projects/{ref}/restart` | restart a database | With a `database_identifier` that is a replica of the project, restarts that replica alone (`201`, once the replica is `RESTARTING`; the units restart in the background and databases-statuses shows `RESTARTING`, then `ACTIVE_HEALTHY` or `ACTIVE_UNHEALTHY`); `404` for another identifier. Without one, or with the project's own, it restarts the project, which is the primary only. A body that is not JSON is `400` on a node with a replica controller. A node without one ignores a body it cannot parse, and answers `503` to a replica identifier. |
| `GET /platform/projects/{ref}/config/supavisor`, `GET /v1/projects/{ref}/config/database/pooler` | the connect strings of each database | One entry per database. A replica entry has `database_type: 'READ_REPLICA'`, `db_user: 'postgres.<identifier>'`, `db_host` the node's public host, the transaction port and the project's pool settings. |
| `GET /platform/projects/{ref}/infra-monitoring` | the replication lag chart | For `databaseIdentifier` and `physical_replication_lag_physical_replication_lag_seconds`: the mean of the controller's one-minute samples per `interval`, as strings. One attribute answers in the single-attribute shape, several in the multi-attribute shape with an `errors` entry for each metric Supavise does not collect. The primary has an empty series. |
| `POST /platform/pg-meta/{ref}/query` and the other pg-meta routes | run SQL against the database that is selected | The header `x-connection-encrypted` selects the database and is never a credential (see below). |

**The header selects a database.** Studio sends back the `connectionString` (or `connection_string_read_only`) of the database it is working on. The host of that string, `db.<identifier>.api.<domain>`, names the project or one of its replicas. The handler authorizes by `{ref}`: an identifier that is neither the project nor one of its replicas answers `403`, a replica that is not `ACTIVE_*` answers `503`, and a header that is not a database host of this node, or is absent, selects the primary. The connection is built from the project's own credentials at the replica's port on this node. The user named in the string is not read. A caller keeps the role it has on the primary, so Studio's reports read `pg_stat_statements` as `postgres` and no role has to reach the replica with the WAL first. A caller who may only query runs as `supavise_read_only` on both. `supabase_read_only_user`, hosted's name, is created by the pinned Postgres artifact; the API only names it.

**The Add button.** Studio enables it when the project has fewer replicas than its size allows (none up to Micro, four from Small to Large, five above), reports Postgres 15 or later and the cloud provider AWS, has `is_physical_backups_enabled`, is not high availability and the organization has the `instances.read_replicas` entitlement, which Supavise grants. `internal/api/studio_eligibility_test.go` ports these rules at the pinned Studio version and evaluates them against the handlers. Studio also tells a user who opens Database, Backups, Point in time on a project with a replica to remove the replicas first.

Not a Studio call: `GET /supavise/v1/failover/readiness` (Owners and Administrators) returns what the failover block of `supavise status` shows. A server that is not part of a cluster answers `404`.

## OAuth sign-in for MCP clients

The consent page (`pages/authorize.tsx`), the organization's OAuth Apps page (`pages/org/[slug]/apps.tsx`) and the Connect > MCP panel work with the pinned Studio unchanged. Supavise answers their calls from `internal/api` (`oauth_consent.go`, `oauth_apps_api.go`) over `internal/oauth`, with the shapes of the platform spec. [design.md](../design.md#13-oauth-sign-in-and-the-remote-mcp-endpoint) section 13 has the rules.

| Call | What Studio does with it | What Supavise answers |
|---|---|---|
| (not Studio) `GET /v1/oauth/authorize` | The MCP client opens it. | `302` to `studio.<domain>/authorize?auth_id=<uuid>[&organization_slug=<slug>]`, where Studio's `withAuth` signs the user in through `api.<domain>/auth/v1` and returns to the page. Whether that return works through SSO and MFA sign-in is unverified. |
| `GET /platform/oauth/authorizations/{id}` | The page shows the client's name, the redirect host, the scopes and an organization picker. | `200 {name, website, icon?, domain, redirect_uri, expires_at, approved_at?, approved_organization_slug?, scopes, registration_type}`. `domain` is the host of the requested redirect URI, and `icon` is left out for a dynamic app. An unknown id is `404`. An expired request still answers `200`, because Studio computes "expired" from `expires_at`. Owners and Administrators only, so a Developer gets `403` (how Studio shows that is unverified). |
| `POST /platform/organizations/{slug}/oauth/authorizations/{id}?skip_browser_redirect=true` | Approve. Studio sets `window.location.href` to the returned `url`. | `201 {url}` with `<redirect_uri>?code=…&state=…&iss=…`, and `Cache-Control: no-store`. `409` for a request decided already, `410` for an expired one, `403` when `{slug}` differs from the request's `organization_slug` or the caller is not an Owner or Administrator of it. A session below aal2 is refused when the caller belongs to an organization that enforces MFA. |
| `DELETE /platform/organizations/{slug}/oauth/authorizations/{id}` | Decline. Studio shows a toast and goes to `/organizations`. | `200 {id}`. The client is not told and waits for its own timeout; hosted behaves the same way. |
| `GET /platform/organizations/{slug}/oauth/apps?type=authorized` | The Authorized tab. | One item per app with a live grant in the organization: `id`, `app_id` and `client_id` are the app's UUID; `authorized_at` is the newest live grant's creation time; `scopes` are the union of the grants' effective scopes. A dynamic app has no `icon` and an empty `website`. |
| `GET …/oauth/apps?type=published` | The Published tab. | The organization's manual apps. |
| `POST …/oauth/apps`, `PUT` and `DELETE …/oauth/apps/{id}` | Publish, edit and delete an app. | `201 {id, client_id, client_secret, client_secret_expires_at: 0, redirect_uris}` (the plaintext secret appears here only); `200`; `200`, which also revokes the app's grants. |
| `POST …/oauth/apps/{id}/revoke` | The Revoke button. | `201 {id, name, website, icon?, authorized_at?}`; revokes the app's live grants in this organization for every user. |
| `GET`, `POST` `…/oauth/apps/{app_id}/client-secrets`, `DELETE …/client-secrets/{secret_id}` | A manual app's secrets. | Secret ids are UUIDs, the list shows `sba_xxxx********` aliases only, and the plaintext appears once, on create. A dynamic app's id answers `404`. |

The Owner and Administrator rule of the existing `oauth_apps` permission covers the organization routes, so no permission list changes.

**The Connect > MCP panel.** It reads `NEXT_PUBLIC_MCP_URL` (patch 0004 adds the placeholder; `studio/placeholders.json`), which the fleet sets to `<api url>/mcp`, and shows `<url>?project_ref=<ref>` with the Claude Code, Cursor, VS Code and Codex snippets. The panel's own text, "OAuth 2.1 with dynamic client registration", describes what the node does.

## Where the spec and Studio disagree

- `GET /platform/projects/{ref}/status`: the spec documents a 200 without a body; Studio reads `{status}`.
- `POST /platform/pg-meta/{ref}/query`: the spec documents 201 and no body; Studio needs the row array. The spec also lists ten `GET /platform/pg-meta/{ref}/...` routes (tables, columns, ...) that Studio's client does not call in this version.
- `HEAD /platform/projects/{ref}/api/rest`: only `GET` is in the spec; Studio sends `HEAD` to check that PostgREST is up. `GET /platform/projects/{ref}/api/rest` returns the PostgREST OpenAPI document (API docs page).
- `GET /platform/projects/{ref}/daily-stats`, `.../infra-monitoring`, `GET /platform/organizations/{slug}/sso` and `GET /v1/projects/{ref}/jit-access`: the spec documents no body. Supavise serves the lag series of `infra-monitoring` for a replica (see "Read replicas") and keeps the empty answer for the rest.

## Studio's own /api routes

`apps/studio/proxy.ts` answers 404 for every `/api/*` path except the allowlist in `lib/hosted-api-allowlist.ts`. The Studio process serves those, not the Management API. `studio/verify.sh` checks that `/api/platform/profile` answers 404 and `/api/get-utc-time` answers 200 in the packaged build.

| Route | What it does | Needs outside access? |
|---|---|---|
| `/api/get-utc-time`, `/api/get-ip-address`, `/api/get-deployment-commit` | clock, caller IP, build commit | no |
| `/api/mcp` | Studio's MCP server (`POST` only, stateless Streamable HTTP) | in platform mode patch 0004 serves it, built on the bearer token it receives and calling the Management API on loopback (`SUPAVISE_MANAGEMENT_API_URL`). The proxy answers `404` for it on `studio.<domain>` and forwards to it only from `api.<domain>/mcp`, after the gate has authenticated the caller. `search_docs` reaches `supabase.com` |
| `/api/incident-banner` | incident.io banner list, requested on every page | yes: incident.io. Answers 500 without a key and delays sign-in; the Supavise proxy answers it with a static `{"incidents": []}` on the Studio host (see "Findings from running Studio") |
| `/api/incident-status`, `/api/status-page`, `/api/status-override` | status banner and incident pages | yes: statuspage.io, incident.io; failure behavior unverified |
| `/api/ai/*` | AI assistant | yes: needs `OPENAI_API_KEY` (server env); an absent key disables the assistant (from reading the code, unverified) |
| `/api/check-cname`, `/api/edge-functions/test`, `/api/edge-functions/body`, `/api/generate-attachment-url`, `/api/content/graphql`, `/api/parse-query`, `/api/scoped-access-token-permissions`, `/api/integrations/stripe-sync` | custom domains, edge function tester, support attachments, docs search, SQL parsing, token scopes, Stripe | mixed; none is on the P0 flows |

## Calls from the browser to a project

These are not Management API calls. They need the project hosts (`<ref>.api.<domain>`) reachable from the browser and allowed by the CSP (`CSP_EXTRA_PROJECT_HOSTS`, Studio patch 0003).

| Call | Feature |
|---|---|
| `wss://<ref-host>/realtime/v1` | Realtime inspector (P2) |
| `<ref-host>/functions/v1/<slug>` | edge function details and AI tester (P2) |
| `<ref-host>/auth/v1/admin/oauth/*`, `/auth/v1/.well-known/*` | OAuth server apps, custom providers and JWKS dialog (P2); the client uses a temporary API key from `POST /platform/projects/{ref}/api-keys/temporary` |
| `<ref-host>/rest/v1/`, `/storage/v1/s3`, `/storage/v1/iceberg`, `/storage/v1/vector` | shown as copyable URLs, not called |
| `https://obuldanrptloktxcffvn.supabase.co/functions/v1/health-check` | Supabase's own edge-functions health probe: hard-coded host, blocked by the CSP, fails soft |

## Findings from running Studio

`studio/spike.sh` runs this Studio build against the mock Management API with a real Postgres, GoTrue and postgres-meta, and drives it with headless Chrome. It visits sign-in, the project list, and for each of two projects the table editor, SQL editor, project home, Auth users, Storage files, Database tables and Settings. Other pages (Realtime, Edge Functions, Logs, Advisors, Integrations, Reports, organization and account pages) are readings of the code only. The findings:

1. **Sign-in waits for the incident banner.** After the token call Studio waits for `GET /api/incident-banner`, which answers 500 without an incident.io key; react-query retries after 1, 4 and 16 s, and the sign-in form awaits the query cache reset, so sign-in took about 22 s. The Supavise proxy answers `GET studio.<domain>/api/incident-banner` with a static `{"incidents": []}` (`internal/proxy/studio.go`), which needs no patch to Studio. Sign-in then takes about 1 s.
2. **`null` in an array field crashes a page.** `GET /v1/projects/{ref}/upgrade/eligibility` with `validation_errors: null` threw in Settings, General. Stubs return `[]` for array fields. The analytics endpoints (`usage.api-counts`) need `{"result": []}`: a bare `{}` shows "Failed to load project usage" on the project home.
3. **Studio calls Usercentrics on every page load** (`https://api.usercentrics.eu/settings//latest/languages.json`, with an empty settings id) even though `NEXT_PUBLIC_USERCENTRICS_RULESET_ID` is unset. Studio's CSP allows the host by default, so the proxy removes every `usercentrics.eu` source from the `Content-Security-Policy` it forwards and the browser refuses the request. A patch that skips initialization when the ruleset id is unset would remove the cause; none exists.
4. **Health status values.** `/v1/projects/{ref}/health` entries use `status: "ACTIVE_HEALTHY"` (enum `COMING_UP | ACTIVE_HEALTHY | UNHEALTHY`). Any other value shows an alert icon and the raw text per service and re-polls every 5 s.
5. **Other values the P0 handlers must get right.** `GET /platform/projects/{ref}/databases` must list the primary database (an empty list makes the SQL editor offer a "Read Replica" source), and one more entry for each replica. The project `region` must be a real AWS region code (an unknown one requests `/img/regions/undefined.svg`). The project database needs GoTrue's migrated `auth` schema, or the Users page reports `column users.banned_until does not exist`.
6. **Dashboard sessions are identified by `aud` and signature.** GoTrue's admin-created user has an empty `auth.users.role`, so its access token carries `"role": ""`. The API server accepts a session when `aud` is `authenticated` and the signature is valid, and does not read the role claim.
7. **postgres-meta closes idle keep-alive connections after 5 s.** A Go client that reuses a connection at that moment sees `EOF`, so `IdleConnTimeout` stays below 5 s (the mock uses 2 s) or the call is retried. A browser navigation cancels in-flight queries; that is not a server error.
8. **Console noise that does not matter:** `Minified React error #418` once on `/sign-in` (hydration mismatch from the "last used" badge), Monaco `Canceled` rejections, one 401 from `telemetry/feature-flags` before the session exists, and the blocked edge-functions health probe.
