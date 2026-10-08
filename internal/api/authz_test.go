package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// rolesFixture is a fixture with one signed-in user per role, a project-scoped Developer and a
// user who belongs to no organization.
type rolesFixture struct {
	*fixture
	tokens map[string]string // by name in roleNames
	ids    map[string]string
}

// roleNames are the signed-in users of the matrix: an Owner, an Administrator, a Developer, a
// Read-only member, a Developer scoped to testRef only, and a user without any membership.
var roleNames = []string{"owner", "admin", "dev", "ro", "scoped", "stranger"}

const secondRef = "bbbbbbbbbbbbbbbbbbbb"

func newRolesFixture(t testing.TB) *rolesFixture {
	t.Helper()
	f := newFixture(t)
	rf := &rolesFixture{fixture: f, tokens: map[string]string{"owner": f.jwt}, ids: map[string]string{"owner": f.userID}}
	for i, n := range roleNames[1:] {
		id := fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", i+1)
		rf.ids[n] = id
		rf.tokens[n] = f.signJWT(map[string]any{"sub": id, "email": n + "@example.test", "role": "authenticated"})
		f.gt.addUser(id, n+"@example.test", time.Now().Add(-time.Hour))
	}
	f.addMember(rf.ids["admin"], members.RoleAdministrator)
	f.addMember(rf.ids["dev"], members.RoleDeveloper)
	f.addMember(rf.ids["ro"], members.RoleReadOnly)
	f.addMember(rf.ids["scoped"], 0)
	if err := f.srv.members.AssignProjectRole(context.Background(), nil, members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}, rf.ids["scoped"], members.RoleDeveloper, []string{testRef}); err != nil {
		t.Fatal(err)
	}
	// A second project that only the organization-wide roles can see.
	f.mgr.addProject(t, secondRef, "Second project", f.org.ID, registry.StatusActiveHealthy)
	return rf
}

func (rf *rolesFixture) as(role, method, path string, body any) *httptest.ResponseRecorder {
	rf.t.Helper()
	return rf.doAs(rf.tokens[role], method, path, body)
}

type routeCase struct {
	method, path string
	body         any
	// want has one letter per role of roleNames: Y the route is allowed (any answer but 401 and
	// 403), N it is refused with 403.
	want string
	// onlyDenied: the allowed outcome changes state that later cases depend on, so only the
	// refusals are checked.
	onlyDenied bool
}

func (c routeCase) String() string { return c.method + " " + c.path }

func rc(want, method, path string, body any) routeCase {
	return routeCase{method, path, body, want, false}
}
func rd(want, method, path string, body any) routeCase {
	return routeCase{method, path, body, want, true}
}

// matrixCases are representative routes of every family of the route table, with what each
// role may do (hosted's access-control documentation and Studio's role descriptions):
//
//	owner admin dev ro scoped stranger
//
// "scoped" is a Developer on testRef only. A user without a membership may only call the
// routes about their own account.
func matrixCases() []routeCase {
	const p = "/v1/projects/" + testRef
	const pp = "/platform/projects/" + testRef
	const org = "/platform/organizations/default"
	sql := map[string]any{"query": "select 1"}
	return []routeCase{
		// the caller's own account: everyone
		rc("YYYYYY", "GET", "/platform/profile", nil),
		rc("YYYYYY", "GET", "/platform/profile/permissions", nil),
		rc("YYYYYY", "POST", "/platform/profile/access-tokens", map[string]any{"name": "t"}),
		rc("YYYYYY", "GET", "/v1/projects", nil),
		rc("YYYYYY", "GET", "/platform/organizations", nil),
		rc("YYYYYY", "GET", "/platform/projects", nil),
		rc("YYYYYY", "POST", "/platform/telemetry/event", map[string]any{}),
		// the node's health in detail: Owners and Administrators
		rc("YYNNNN", "GET", "/healthz/detail", nil),
		// organization
		rc("YYYYYN", "GET", org, nil),
		rc("YNNNNN", "PATCH", org, map[string]any{"name": "Renamed"}),
		rd("YNNNNN", "DELETE", org, nil),
		rc("YYYYYN", "GET", org+"/members", nil),
		rc("YYYYYN", "GET", org+"/roles", nil),
		rc("YYYYNN", "GET", org+"/members/invitations", nil),
		rc("YYYYYN", "GET", org+"/members/mfa/enforcement", nil),
		rc("YNNNNN", "PATCH", org+"/members/mfa/enforcement", map[string]any{"enforced": false}),
		rc("YYYYNN", "GET", org+"/billing/subscription", nil),
		rc("YYNNNN", "PUT", org+"/billing/subscription", map[string]any{}),
		rc("YYYYNN", "GET", org+"/usage", nil),
		// single sign-on: Owners and Administrators manage it (the domains' default role is
		// checked against what the caller may grant, see TestSSOAdministratorsCannotMakeOwners)
		rc("YYNNNN", "POST", org+"/sso", map[string]any{}),
		rc("YYNNNN", "GET", org+"/sso", nil),
		rc("YYNNNN", "GET", org+"/sso/providers", nil),
		rc("YYNNNN", "POST", org+"/sso/providers", map[string]any{}),
		rc("YYNNNN", "GET", org+"/sso/pending", nil),
		rc("YYNNNN", "DELETE", org+"/sso/pending/a0000000-0000-4000-8000-000000000009", nil),
		// a project's own identity providers: read like the project's config, written like its Auth settings
		rc("YYYYYN", "GET", "/v1/projects/"+testRef+"/config/auth/sso/providers", nil),
		rc("YYNNNN", "POST", "/v1/projects/"+testRef+"/config/auth/sso/providers", map[string]any{"type": "saml"}),
		rc("YYNNNN", "DELETE", "/v1/projects/"+testRef+"/config/auth/sso/providers/a0000000-0000-4000-8000-000000000001", nil),
		rc("YYNNNN", "POST", org+"/oauth/apps", map[string]any{}),
		rc("YYYYYN", "GET", "/v1/organizations/default/members", nil),
		rc("YYYYYN", "GET", "/v2/organizations/default/roles", nil),
		// project: reading
		rc("YYYYYN", "GET", pp, nil),
		rc("YYYYYN", "GET", pp+"/status", nil),
		rc("YYYYYN", "GET", pp+"/settings", nil),
		rc("YYYYYN", "GET", pp+"/members", nil),
		rc("YYYYYN", "GET", p+"/health", nil),
		rc("YYYYYN", "GET", p+"/config/auth", nil),
		rc("YYYYYN", "GET", p+"/postgrest", nil),
		rc("YYYYYN", "GET", p+"/config/database/pooler", nil),
		rc("YYYYYN", "GET", p+"/advisors/security", nil),
		rc("YYYYYN", "GET", p+"/api-keys", nil),
		rc("YYYNYN", "GET", p+"/api-keys?reveal=true", nil),
		rc("YYYNYN", "POST", pp+"/api-keys/temporary", map[string]any{}),
		rc("YYYYYN", "GET", p+"/secrets", nil),
		rc("YYYYYN", "GET", p+"/functions", nil),
		rc("YYYYYN", "GET", p+"/branches", nil),
		// project: lifecycle and settings (Owner and Administrator)
		rc("YYNNNN", "PATCH", pp, map[string]any{"name": "Renamed"}),
		rd("YYNNNN", "DELETE", pp, nil),
		rd("YYNNNN", "POST", pp+"/pause", nil),
		rd("YYYNYN", "POST", pp+"/restart", nil), // hosted lists Restart for Developers
		rd("YYYNYN", "POST", p+"/restart", nil),
		rd("YYYNYN", "POST", pp+"/restart-services", map[string]any{}),
		// restoring a backup or a point in time overwrites the data: Owner and Administrator only
		rc("YYYYYN", "GET", "/platform/database/"+testRef+"/backups", nil),
		rc("YYYYYN", "GET", p+"/database/backups", nil),
		rc("YYYYYN", "GET", pp+"/billing/addons", nil),
		rc("YYYYYN", "GET", p+"/billing/addons", nil),
		// read replicas: every role that sees the project reads its databases and their lag; adding
		// and removing a replica changes the project's infrastructure, like its size
		rc("YYYYYN", "GET", pp+"/databases", nil),
		rc("YYYYYN", "GET", pp+"/databases-statuses", nil),
		rc("YYYYYN", "GET", pp+"/load-balancers", nil),
		rc("YYYYYN", "GET", pp+"/infra-monitoring", nil),
		rc("YYYYYN", "GET", pp+"/config/supavisor", nil),
		rd("YYNNNN", "POST", p+"/read-replicas/setup", map[string]any{"read_replica_region": "eu-west-1"}),
		rd("YYNNNN", "POST", p+"/read-replicas/remove", map[string]any{"database_identifier": testRef + "-rr-eu-west-1-abcdef"}),
		rd("YYNNNN", "POST", "/platform/database/"+testRef+"/backups/restore", map[string]any{"id": 1}),
		rd("YYNNNN", "POST", "/platform/database/"+testRef+"/backups/restore-physical", map[string]any{"id": 1}),
		rd("YYNNNN", "POST", "/platform/database/"+testRef+"/backups/pitr", map[string]any{"recovery_time_target_unix": 1}),
		rd("YYNNNN", "POST", p+"/database/backups/restore", map[string]any{"id": 1}),
		rd("YYNNNN", "POST", p+"/database/backups/restore-pitr", map[string]any{"recovery_time_target_unix": 1}),
		rd("YYNNNN", "POST", p+"/restore", nil),
		rc("YYNNNN", "PATCH", p+"/config/auth", map[string]any{"site_url": "https://app.example.test"}),
		rc("YYNNNN", "PATCH", pp+"/config/postgrest", map[string]any{"max_rows": 500}),
		rc("YYNNNN", "PUT", p+"/config/database/postgres", map[string]any{"max_connections": 60}),
		rc("YYNNNN", "PATCH", p+"/postgrest", map[string]any{"max_rows": 500}),
		rc("YYNNNN", "PATCH", p+"/config/database/pooler", map[string]any{"default_pool_size": 20}),
		rc("YYNNNN", "PATCH", pp+"/config/pgbouncer", map[string]any{"default_pool_size": 20}),
		rd("YYNNNN", "PATCH", p+"/database/password", map[string]any{"password": "a-new-database-password"}),
		// compute size and disk: every role that sees the project reads them, Owners and Administrators change them
		rc("YYYYYN", "GET", pp+"/disk", nil),
		rc("YYYYYN", "GET", pp+"/disk/util", nil),
		rc("YYYYYN", "GET", p+"/config/disk", nil),
		rc("YYYYYN", "GET", p+"/config/disk/util", nil),
		rc("YYNNNN", "POST", pp+"/billing/addons", map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_micro"}),
		rc("YYNNNN", "PATCH", p+"/billing/addons", map[string]any{"addon_type": "compute_instance", "addon_variant": "ci_micro"}),
		rd("YYNNNN", "DELETE", pp+"/billing/addons/ci_micro", nil),
		rc("YYNNNN", "POST", pp+"/disk", map[string]any{"attributes": map[string]any{"type": "gp3", "size_gb": 1, "iops": 3000, "throughput_mbps": 125}}),
		rc("YYNNNN", "POST", p+"/config/disk", map[string]any{"attributes": map[string]any{"type": "gp3", "size_gb": 1, "iops": 3000}}),
		rc("YYNNNN", "POST", pp+"/disk/custom-config", map[string]any{}),
		rc("YYNNNN", "POST", pp+"/resize", map[string]any{"volume_size_gb": 1}),
		rc("YYNNNN", "POST", p+"/upgrade", map[string]any{}),
		// upgrade state is readable by every member; only Owners and Administrators upgrade
		rc("YYYYYN", "GET", p+"/upgrade/eligibility", nil),
		rc("YYYYYN", "GET", p+"/upgrade/status", nil),
		rc("YYYYYN", "GET", pp+"/service-versions", nil),
		rc("YYNNNN", "POST", p+"/secrets", []any{map[string]any{"name": "FOO", "value": "bar"}}),
		rc("YYNNNN", "DELETE", p+"/secrets", []any{"FOO"}),
		rc("YYNNNN", "POST", p+"/api-keys", map[string]any{"type": "publishable", "name": "matrix_key"}),
		rc("YYNNNN", "PUT", p+"/api-keys/legacy?enabled=true", nil),
		// custom domains: every role reads them; Owners and Administrators change them
		rc("YYYYYN", "GET", p+"/custom-hostname", nil),
		rc("YYYYYN", "GET", p+"/vanity-subdomain", nil),
		rc("YYYYYN", "POST", p+"/vanity-subdomain/check-availability", map[string]any{"vanity_subdomain": "acme"}),
		rd("YYNNNN", "POST", p+"/custom-hostname/initialize", map[string]any{"custom_hostname": "docs.example.org"}),
		rd("YYNNNN", "POST", p+"/custom-hostname/reverify", nil),
		rd("YYNNNN", "POST", p+"/custom-hostname/activate", nil),
		rd("YYNNNN", "DELETE", p+"/custom-hostname", nil),
		rd("YYNNNN", "POST", p+"/vanity-subdomain/activate", map[string]any{"vanity_subdomain": "acme"}),
		rd("YYNNNN", "DELETE", p+"/vanity-subdomain", nil),
		// project: database content (Owner, Administrator, Developer)
		rc("YYYYYN", "POST", p+"/database/query", sql),
		rc("YYYYYN", "POST", p+"/database/query/read-only", sql),
		rc("YYYYYN", "POST", "/platform/pg-meta/"+testRef+"/query", sql),
		rc("YYYYYN", "GET", "/platform/pg-meta/"+testRef+"/tables", nil),
		rc("YYYYYN", "GET", p+"/database/migrations", nil),
		rc("YYYNYN", "POST", p+"/database/migrations", map[string]any{"query": "select 1", "name": "m"}),
		rc("YYYYYN", "GET", p+"/types/typescript", nil),
		rc("YYYYYN", "POST", p+"/cli/login-role", map[string]any{"read_only": false}),
		// dropping the project's login roles drops every member's, so it needs the right to change roles
		rc("YYYNYN", "DELETE", p+"/cli/login-role", nil),
		rc("YYYNYN", "DELETE", p+"/functions/nope", nil),
		rc("YYYNYN", "POST", p+"/branches", map[string]any{"branch_name": "x"}),
		// a branch with data copies production data: Owner and Administrator only
		rc("YYYNYN", "POST", p+"/branches", map[string]any{"branch_name": "x2", "with_data": false}),
		rc("YYNNNN", "POST", p+"/branches", map[string]any{"branch_name": "x3", "with_data": true}),
		// project: users, storage
		rc("YYYYYN", "GET", "/platform/auth/"+testRef+"/users", nil),
		rc("YYYNYN", "POST", "/platform/auth/"+testRef+"/users", map[string]any{"email": "u@example.test", "password": "pw-pw-pw-pw"}),
		rc("YYYNYN", "DELETE", "/platform/auth/"+testRef+"/users/abc", nil),
		rc("YYYNYN", "POST", "/platform/auth/"+testRef+"/invite", map[string]any{"email": "u@example.test"}),
		rc("YYYYYN", "GET", "/platform/storage/"+testRef+"/buckets", nil),
		rc("YYYNYN", "POST", "/platform/storage/"+testRef+"/buckets", map[string]any{"id": "b", "type": "STANDARD"}),
		rc("YYYNYN", "GET", "/platform/storage/"+testRef+"/credentials", nil),
		rc("YYNNNN", "POST", "/platform/storage/"+testRef+"/credentials", map[string]any{"description": "d"}),
		rc("YYYYYN", "POST", "/platform/storage/"+testRef+"/buckets/b/objects/list", map[string]any{"path": ""}),
		rc("YYYNYN", "POST", "/platform/storage/"+testRef+"/buckets/b/objects/move", map[string]any{"from": "a", "to": "b"}),
		// project: saved content belongs to every member
		rc("YYYYYN", "GET", pp+"/content", nil),
		rc("YYYYYN", "PUT", pp+"/content", map[string]any{"name": "mine", "type": "sql", "visibility": "user", "content": map[string]any{"sql": "select 1"}}),
		rc("YYYYYN", "POST", pp+"/content/folders", map[string]any{"name": "mine"}),
		rc("YYYYYN", "PUT", pp+"/content", map[string]any{"name": "a report", "type": "log_sql", "visibility": "user", "content": map[string]any{}}),
		// hosted: reports belong to Developers and up
		rc("YYYNYN", "PUT", pp+"/content", map[string]any{"name": "a report", "type": "report", "visibility": "user", "content": map[string]any{}}),
		rc("YYYYYN", "POST", pp+"/analytics/endpoints/logs.all", map[string]any{}),
		// a project of the organization that the scoped member has no role on
		rc("YYYYNN", "GET", "/platform/projects/"+secondRef, nil),
		rc("YYYYNN", "POST", "/v1/projects/"+secondRef+"/database/query", sql),
		rc("YYYYNN", "GET", "/v1/projects/"+secondRef+"/api-keys", nil),
		// default deny: a write nobody named needs the Owner role, or the right over the project
		rc("YNNNNN", "POST", "/platform/zzz-unknown", map[string]any{}),
		rc("YNNNNN", "DELETE", "/platform/organizations/default/audit-placeholder", nil),
	}
}

func isAllowed(rec *httptest.ResponseRecorder) bool { return rec.Code != 401 && rec.Code != 403 }

func TestRoleMatrix(t *testing.T) {
	rf := newRolesFixture(t)
	cases := matrixCases()
	// Cases that change state run after the others, one role at a time in a fixed order.
	for _, c := range cases {
		if len(c.want) != len(roleNames) {
			t.Fatalf("%s: want %q needs %d letters", c, c.want, len(roleNames))
		}
		for i, role := range roleNames {
			allowed := c.want[i] == 'Y'
			if c.onlyDenied && allowed {
				continue
			}
			rec := rf.as(role, c.method, c.path, c.body)
			if allowed && !isAllowed(rec) {
				t.Errorf("%-8s %s %s: refused (%d %s), want allowed", role, c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if !allowed && rec.Code != 403 {
				t.Errorf("%-8s %s %s: %d %s, want 403", role, c.method, c.path, rec.Code, truncate(strings.TrimSpace(rec.Body.String()), 120))
			}
		}
	}
	// The destructive routes work for the roles that may use them (checked once, last).
	if rec := rf.as("admin", "POST", "/platform/projects/"+testRef+"/pause", nil); !isAllowed(rec) {
		t.Errorf("admin pause: %d %s", rec.Code, rec.Body)
	}
	if rec := rf.as("admin", "DELETE", "/platform/projects/"+testRef, nil); !isAllowed(rec) {
		t.Errorf("admin delete project: %d %s", rec.Code, rec.Body)
	}
	if rec := rf.as("owner", "DELETE", "/platform/projects/"+secondRef, nil); !isAllowed(rec) {
		t.Errorf("owner delete project: %d %s", rec.Code, rec.Body)
	}
}

// Every operation of the three specs resolves to a requirement, and every rule of the table is
// the first match of at least one operation (a shadowed rule is a mistake).
func TestRouteTableCoversTheSpecs(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	// The operations of the specs, plus the routes the server adds that no spec lists.
	type route struct{ method, path string }
	var routes []route
	for _, op := range ops {
		routes = append(routes, route{op.Method, op.Path})
	}
	for key := range newFixture(t).srv.implemented() {
		m, p, _ := strings.Cut(key, " ")
		routes = append(routes, route{m, p})
	}
	used := map[int]int{}
	for _, op := range routes {
		matched := false
		for i, rr := range routeRules {
			if rr.matches(op.method, op.path) {
				used[i]++
				matched = true
				break
			}
		}
		_ = matched
		n := routeNeed(op.method, op.path)
		if n.kind == needCheck && n.action == "" && n.resourceFn == nil {
			t.Errorf("%s %s: empty action", op.method, op.path)
		}
		// A write must never fall to "any user".
		if n.kind == needAny && op.method != "GET" && op.method != "HEAD" && !allowedUserWrite(op.path) {
			t.Errorf("%s %s: a write open to every signed-in user", op.method, op.path)
		}
	}
	for i, rr := range routeRules {
		if used[i] == 0 {
			t.Errorf("rule %d (%s %s) is the first match of no operation of the specs", i, rr.methods, rr.re)
		}
	}
}

// userWritePaths are the writes open to every signed-in user: their own profile, tokens,
// telemetry and feedback, and the invitation they hold the token of.
var userWritePaths = regexp.MustCompile(`^/(platform/(profile|cli/login|notifications|telemetry|feedback|support|reset-password|update-email|signup|organizations/(preview-creation|onboarding-survey|\{slug\}/members/invitations/\{token\}))|v1/oauth)`)

func allowedUserWrite(path string) bool { return userWritePaths.MatchString(path) }

// What Studio sees is what the server enforces: the entries of GET /platform/profile/permissions,
// decoded from JSON and evaluated like Studio's doPermissionsCheck, give the same answers as the
// server's own check for every role.
func TestPermissionsEndpointMatchesEnforcement(t *testing.T) {
	rf := newRolesFixture(t)
	checks := []struct{ action, resource string }{
		{members.ActRead, members.ResOrg}, {members.ActUpdate, members.ResOrg}, {members.ActCreate, members.ResProjects},
		{members.ActUpdate, members.ResProjects}, {members.ActDelete, members.ResProjects}, {members.ActRead, members.ResServiceKeys},
		{members.ActSQLQuery, members.ResAny}, {members.ActSQLInsert, members.ResAny}, {members.ActSQLAdminWrite, "tables"},
		{members.ActAuthExecute, "create_user"}, {members.ActFunctionsWrite, members.ResAny}, {members.ActCreate, members.ResUserContent},
		{members.ActInfraExecute, "reboot"}, {members.ActBillingWrite, "stripe.subscriptions"}, {members.ActSecretsWrite, members.ResAny},
	}
	for _, role := range roleNames {
		rec := rf.as(role, "GET", "/platform/profile/permissions", nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", role, rec.Code)
		}
		validateAgainstSpec(t, "GET /platform/profile/permissions", rec.Body.Bytes())
		var rows []struct {
			Actions   []string `json:"actions"`
			Resources []string `json:"resources"`
			Condition any      `json:"condition"`
			OrgID     int64    `json:"organization_id"`
			OrgSlug   string   `json:"organization_slug"`
			Refs      []string `json:"project_refs"`
			Ids       []any    `json:"project_ids"`
			Restr     bool     `json:"restrictive"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if role == "stranger" {
			if len(rows) != 0 {
				t.Errorf("a user without memberships must get an empty list: %s", rec.Body)
			}
			continue
		}
		var perms []members.Permission
		for _, r := range rows {
			perms = append(perms, members.Permission{Actions: r.Actions, Resources: r.Resources, Condition: r.Condition,
				OrganizationID: r.OrgID, OrganizationSlug: r.OrgSlug, ProjectRefs: r.Refs, Restrictive: r.Restr})
		}
		a, err := rf.srv.members.Access(context.Background(), rf.ids[role])
		if err != nil {
			t.Fatal(err)
		}
		org := members.OrgRef{ID: rf.org.ID, Slug: rf.org.Slug}
		for _, ref := range []string{"", testRef, secondRef} {
			for _, c := range checks {
				studio := members.Check(perms, c.action, c.resource, nil, "default", ref)
				server := a.Can(org, ref, c.action, c.resource, nil)
				if studio != server {
					t.Errorf("%s ref=%q %s on %s: Studio's check says %v, the server's %v", role, ref, c.action, c.resource, studio, server)
				}
			}
		}
	}
	// Role-management checks carry the role id in the condition.
	rec := rf.as("admin", "GET", "/platform/profile/permissions", nil)
	if !strings.Contains(rec.Body.String(), `"resource.role_id"`) {
		t.Errorf("an Administrator's entries must restrict Owner role changes by condition: %s", rec.Body)
	}
	if body := rf.as("owner", "GET", "/platform/profile/permissions", nil).Body.String(); !strings.Contains(body, `"actions":["%"]`) || !strings.Contains(body, `"condition":null`) {
		t.Errorf("owner entries: %s", body)
	}
}

// A hand-written route open to every signed-in user is one of a short list (the caller's own
// account, the lists that filter themselves, an invitation by its token). Anything else, such as
// a route of a family added later, needs a rule: otherwise a user without a role could use it.
func TestImplementedRoutesOpenToEveryUserAreAllowlisted(t *testing.T) {
	open := regexp.MustCompile(`^(/platform/(profile|cli/login)(/|$)|/v1/profile$|/(platform|v1)/(organizations|projects)$|/(platform|v1)/projects/available-regions$|/platform/organizations/\{slug\}/members/invitations/\{token\}$)`)
	for key := range newFixture(t).srv.implemented() {
		m, p, _ := strings.Cut(key, " ")
		if n := routeNeed(m, p); n.kind == needAny && !open.MatchString(p) {
			t.Errorf("%s is open to every signed-in user; give it a rule in routeRules", key)
		}
	}
}

// A branch with data copies the parent's data, so it needs the Owner or Administrator role;
// schema-only branches keep the Developer permission. The refusal has the shape of every other
// role denial, and nothing is created.
func TestBranchWithDataNeedsOwnerOrAdministrator(t *testing.T) {
	rf := newRolesFixture(t)
	path := "/v1/projects/" + testRef + "/branches"
	list := func() int {
		rec := rf.as("owner", "GET", path, nil)
		var bs []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &bs); err != nil {
			t.Fatal(err)
		}
		return len(bs)
	}
	before := list()
	for _, role := range []string{"dev", "ro", "scoped"} {
		rec := rf.as(role, "POST", path, map[string]any{"branch_name": "data-" + role, "with_data": true})
		if rec.Code != 403 {
			t.Fatalf("%s: with_data answered %d %s, want 403", role, rec.Code, rec.Body)
		}
		// Read-only is already refused by the route's own rule; the others reach this check.
		if msg, _ := jsonField(t, rec, "message").(string); !strings.HasPrefix(msg, "Your role does not allow this action") || (role != "ro" && !strings.Contains(msg, "Owner or Administrator")) {
			t.Errorf("%s: message %q", role, msg)
		}
	}
	if n := list(); n != before {
		t.Fatalf("a refused request created a branch (%d before, %d after)", before, n)
	}
	// The refusal comes before the body is acted on, whatever else it says.
	if rec := rf.as("dev", "POST", path, map[string]any{"branch_name": "data-dev", "with_data": true, "persistent": true}); rec.Code != 403 {
		t.Errorf("dev with_data and persistent: %d", rec.Code)
	}
	// Developers still create schema-only branches, with the field absent or false.
	for i, body := range []map[string]any{{"branch_name": "dev-a"}, {"branch_name": "dev-b", "with_data": false}} {
		if rec := rf.as("dev", "POST", path, body); rec.Code != 201 {
			t.Errorf("dev schema-only branch %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	for _, role := range []string{"owner", "admin"} {
		rec := rf.as(role, "POST", path, map[string]any{"branch_name": "data-" + role, "with_data": true})
		if rec.Code == 403 {
			t.Errorf("%s: with_data refused: %s", role, rec.Body)
		}
	}
}

// A reset clones the parent's current data again into a branch that has data, so it needs the same
// role as creating one: a Developer, who could otherwise pull a fresh copy of production into a
// branch an Owner made, is refused. Resetting a schema-only branch stays open to Developers.
func TestResetOfBranchWithDataNeedsOwnerOrAdministrator(t *testing.T) {
	rf := newRolesFixture(t)
	mk := func(name string, withData bool) string {
		rec := rf.status(201, "owner", "POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": name})
		b := decodeBody(t, rec).(map[string]any)
		rf.waitBranch(t, name, "MIGRATIONS_PASSED")
		if withData {
			// The fixture's node cannot clone data, so a branch that was created without it is
			// marked as having it: what the reset route looks at is the registry row.
			ref := b["project_ref"].(string)
			p, err := rf.reg.GetProject(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			nb := *p.Branch
			nb.WithData = true
			if err := rf.reg.UpdateBranch(context.Background(), ref, &nb); err != nil {
				t.Fatal(err)
			}
		}
		return b["id"].(string)
	}
	data, plain := mk("data", true), mk("plain", false)
	for _, role := range []string{"dev", "ro", "scoped"} {
		rec := rf.as(role, "POST", "/v1/branches/"+data+"/reset", map[string]any{})
		if rec.Code != 403 {
			t.Fatalf("%s: reset of a branch with data answered %d %s, want 403", role, rec.Code, rec.Body)
		}
		// Read-only is already refused by the route's own rule; the others reach this check.
		if msg, _ := jsonField(t, rec, "message").(string); !strings.HasPrefix(msg, "Your role does not allow this action") || (role != "ro" && !strings.Contains(msg, "Owner or Administrator")) {
			t.Errorf("%s: message %q", role, msg)
		}
	}
	// The schema-only branch resets for the Developer, the project-scoped one included.
	for _, role := range []string{"dev", "scoped"} {
		rf.status(201, role, "POST", "/v1/branches/"+plain+"/reset", map[string]any{})
		rf.waitBranch(t, "plain", "MIGRATIONS_PASSED")
	}
	// Owner and Administrator pass the check. The reset itself then needs a node that can clone,
	// which the fixture is not, so only the absence of a 403 is asserted.
	for _, role := range []string{"owner", "admin"} {
		if rec := rf.as(role, "POST", "/v1/branches/"+data+"/reset", map[string]any{}); rec.Code == 403 {
			t.Errorf("%s: reset of a branch with data refused: %s", role, rec.Body)
		}
	}
}
