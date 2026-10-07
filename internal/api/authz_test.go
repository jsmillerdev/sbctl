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

	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
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
		rc("YNNNNN", "POST", org+"/sso", map[string]any{}),
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
		rd("YYNNNN", "POST", pp+"/restart", nil),
		rd("YYNNNN", "POST", p+"/restore", nil),
		rc("YYNNNN", "PATCH", p+"/config/auth", map[string]any{"site_url": "https://app.example.test"}),
		rc("YYNNNN", "PATCH", pp+"/config/postgrest", map[string]any{"max_rows": 500}),
		rc("YYNNNN", "PUT", p+"/config/database/postgres", map[string]any{"max_connections": 60}),
		rc("YYNNNN", "PATCH", p+"/postgrest", map[string]any{"max_rows": 500}),
		rd("YYNNNN", "PATCH", p+"/database/password", map[string]any{"password": "a-new-database-password"}),
		rc("YYNNNN", "POST", pp+"/disk", map[string]any{}), // a stub: unnamed project writes need the settings permission
		rc("YYNNNN", "POST", p+"/upgrade", map[string]any{}),
		rc("YYNNNN", "POST", p+"/secrets", []any{map[string]any{"name": "FOO", "value": "bar"}}),
		rc("YYNNNN", "DELETE", p+"/secrets", []any{"FOO"}),
		rc("YYNNNN", "POST", p+"/api-keys", map[string]any{"type": "publishable", "name": "matrix_key"}),
		rc("YYNNNN", "PUT", p+"/api-keys/legacy?enabled=true", nil),
		// project: database content (Owner, Administrator, Developer)
		rc("YYYYYN", "POST", p+"/database/query", sql),
		rc("YYYYYN", "POST", p+"/database/query/read-only", sql),
		rc("YYYYYN", "POST", "/platform/pg-meta/"+testRef+"/query", sql),
		rc("YYYYYN", "GET", "/platform/pg-meta/"+testRef+"/tables", nil),
		rc("YYYYYN", "GET", p+"/database/migrations", nil),
		rc("YYYNYN", "POST", p+"/database/migrations", map[string]any{"query": "select 1", "name": "m"}),
		rc("YYYYYN", "GET", p+"/types/typescript", nil),
		rc("YYYYYN", "POST", p+"/cli/login-role", map[string]any{"read_only": false}),
		rc("YYYNYN", "DELETE", p+"/functions/nope", nil),
		rc("YYYNYN", "POST", p+"/branches", map[string]any{"branch_name": "x"}),
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
