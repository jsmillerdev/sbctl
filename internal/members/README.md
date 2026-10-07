# internal/members: organization members, roles and permissions

Who belongs to which organization with which role, project-scoped roles, invitations, and the
permissions those roles grant, in the shape Studio and the Management API use (hosted's
`PermissionAction` model). The API server (`internal/api`, `authz.go` and `members_api.go`) enforces
them on every `/platform` and `/v1` route and serves them to Studio; `supavise users` (`cmd/supavise`) and
the SSO workstream use the same `Service`. The route table, the capability table and the HTTP
behavior are documented in `internal/api/README.md` (Members, roles and permissions).

| File | What |
|---|---|
| `roles.go` | the four roles and their ids (Owner 1, Administrator 2, Developer 3, Read-only 4), project-scoped role ids from 1000, parsing of names |
| `grants.go` | the permission entries of each role: Owner `%` on `%`; Administrator the same minus restrictive entries for organization settings, project transfer and Owner role changes (a json-logic condition on `resource.role_id`); Developer and Read-only allow lists |
| `permission.go` | `Permission` (the spec's `AccessControlPermission`), `Check` with the semantics of Studio's `doPermissionsCheck`, a small json-logic evaluator |
| `store.go`, `memory.go`, `pg.go` | the `Store` interface, a memory and a Postgres implementation (registry migration `0900_members.sql`); `Store.Update` runs a change in a transaction that locks the organization |
| `service.go` | `Service`: `Access` (what a user may do), role changes with the checks Studio's Team page implies, the last-Owner rule, invitations (token hash, expiry, single use), the legacy-account rule, SSO default roles |

```go
svc := &members.Service{Store: members.NewPG(pool), Orgs: listOrgs, AccountCreatedAt: lookup}
access, _ := svc.Access(ctx, userID)
access.Can(members.OrgRef{ID: 1, Slug: "acme"}, projectRef, members.ActSQLAdminWrite, "migrations", nil)

// the SSO workstream, on a user's first sign-in:
grant, _ := svc.GrantSSODefault(ctx, userID, email) // nil when the domain has no rule or the user is a member already
```

`go test ./internal/members` runs the service tests on the memory store, and on Postgres too when
`SUPAVISE_TEST_DATABASE_URL` is set (each run uses a throwaway database next to it): role changes by every
role, the last Owner under concurrent demotions, project-scoped roles, invitations (expiry, single use, a
failed acceptance does not consume the invitation), the legacy rule, SSO defaults, removal of a project.
