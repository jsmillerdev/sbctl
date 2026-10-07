package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func body[T any](t testing.TB, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body is not the expected JSON: %v: %q", err, rec.Body.String())
	}
	return v
}

func (rf *rolesFixture) pat(role string) string {
	rf.t.Helper()
	tok := secrets.NewPAT()
	err := rf.reg.CreateAccessToken(context.Background(), &registry.AccessToken{UserID: rf.ids[role], Name: "t-" + role, Hash: secrets.HashToken(tok), Prefix: tok[:8]})
	if err != nil {
		rf.t.Fatal(err)
	}
	return tok
}

func (rf *rolesFixture) status(want int, role, method, path string, in any) *httptest.ResponseRecorder {
	rf.t.Helper()
	rec := rf.as(role, method, path, in)
	if rec.Code != want {
		rf.t.Fatalf("%s %s %s: %d %s, want %d", role, method, path, rec.Code, strings.TrimSpace(rec.Body.String()), want)
	}
	return rec
}

const orgBase = "/platform/organizations/default"

func TestReadOnlyRunsAsReadOnlyDatabaseRole(t *testing.T) {
	rf := newRolesFixture(t)
	dsnUser := func() string {
		d := rf.meta.last().DSN
		return strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(d, "postgres://"), "postgresql://"), ":", 2)[0]
	}
	sql := map[string]any{"query": "select 1"}
	// Studio's SQL editor and table editor go through pg-meta.
	for role, want := range map[string]string{"owner": "postgres", "admin": "postgres", "dev": "postgres", "scoped": "postgres", "ro": roleReadOnly} {
		rf.status(200, role, "POST", "/platform/pg-meta/"+testRef+"/query", sql)
		if got := dsnUser(); got != want {
			t.Errorf("%s: pg-meta ran as %s, want %s", role, got, want)
		}
		rf.status(200, role, "GET", "/platform/pg-meta/"+testRef+"/tables", nil)
		if got := dsnUser(); got != want {
			t.Errorf("%s: pg-meta GET ran as %s, want %s", role, got, want)
		}
	}
	// The Management API's SQL route (the MCP server's execute_sql): a Read-only member's
	// statement runs read-only even without asking.
	for role, want := range map[string]string{"dev": "postgres", "ro": roleReadOnly} {
		rf.status(201, role, "POST", "/v1/projects/"+testRef+"/database/query", sql)
		if got := dsnUser(); got != want {
			t.Errorf("%s: database/query ran as %s, want %s", role, got, want)
		}
	}
	// The CLI's login role is read-only for a Read-only member, whatever the request says.
	loginRoleSQL := func() string {
		rf.meta.mu.Lock()
		defer rf.meta.mu.Unlock()
		for i := len(rf.meta.Requests) - 1; i >= 0; i-- {
			if strings.Contains(rf.meta.Requests[i].Body, "create role") && strings.Contains(rf.meta.Requests[i].Body, "cli_") {
				return rf.meta.Requests[i].Body
			}
		}
		return ""
	}
	rf.status(201, "ro", "POST", "/v1/projects/"+testRef+"/cli/login-role", map[string]any{"read_only": false})
	if q := loginRoleSQL(); !strings.Contains(q, "sbctl_cli_ro_") || !strings.Contains(q, "pg_read_all_data") || strings.Contains(q, "in role postgres") {
		t.Errorf("a Read-only member got a read-write login role: %s", q)
	}
	rf.status(201, "dev", "POST", "/v1/projects/"+testRef+"/cli/login-role", map[string]any{"read_only": false})
	if q := loginRoleSQL(); !strings.Contains(q, "cli_login_") || !strings.Contains(q, "in role postgres") {
		t.Errorf("a Developer must get the read-write login role: %s", q)
	}
}

func TestSecretsAreHiddenFromReadOnly(t *testing.T) {
	rf := newRolesFixture(t)
	settings := func(role string) map[string]any {
		return body[map[string]any](t, rf.status(200, role, "GET", "/platform/projects/"+testRef+"/settings", nil))
	}
	for _, role := range []string{"owner", "admin", "dev", "scoped"} {
		s := settings(role)
		if s["jwt_secret"] == nil || len(s["service_api_keys"].([]any)) != 2 {
			t.Errorf("%s must see the JWT secret and the service key: %v", role, s)
		}
	}
	ro := settings("ro")
	if ro["jwt_secret"] != nil {
		t.Error("a Read-only member must not see the JWT secret")
	}
	list := ro["service_api_keys"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["tags"] != "anon" {
		t.Errorf("a Read-only member sees the anon key only: %v", list)
	}
	// PostgREST's config carries the JWT secret.
	for role, secretShown := range map[string]bool{"dev": true, "ro": false} {
		rec := rf.status(200, role, "GET", "/platform/projects/"+testRef+"/config/postgrest", nil)
		got, _ := body[map[string]any](t, rec)["jwt_secret"].(string)
		if (got != "") != secretShown {
			t.Errorf("%s: jwt_secret %q, shown=%v", role, got, secretShown)
		}
	}
	// Secret keys are listed masked, and cannot be revealed.
	rec := rf.status(200, "ro", "GET", "/v1/projects/"+testRef+"/api-keys", nil)
	for _, k := range body[[]map[string]any](t, rec) {
		if key, _ := k["api_key"].(string); k["name"] == "service_role" && strings.HasPrefix(key, "eyJ") {
			t.Errorf("the service_role key is listed in full to a Read-only member: %v", k)
		}
	}
	rf.status(403, "ro", "GET", "/v1/projects/"+testRef+"/api-keys/anything?reveal=true", nil)
	rf.status(403, "ro", "POST", "/platform/projects/"+testRef+"/api-keys/temporary", map[string]any{})
}

func TestPersonalAccessTokensCarryTheirOwnersPermissions(t *testing.T) {
	rf := newRolesFixture(t)
	ro, owner := rf.pat("ro"), rf.pat("owner")
	p := "/v1/projects/" + testRef
	list := func(tok string) int {
		rec := rf.doAs(tok, "GET", "/v1/projects", nil)
		if rec.Code != 200 {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return len(body[[]map[string]any](t, rec))
	}
	if n := list(ro); n != 2 {
		t.Fatalf("a Read-only token lists the projects: %d", n)
	}
	// Reads work, writes are refused, as for the CLI (secrets set, db push) and the MCP server
	// (apply_migration, execute_sql with writes).
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", p + "/secrets", nil, 200},
		{"GET", p + "/database/migrations", nil, 200},
		{"GET", p + "/types/typescript", nil, 200},
		{"POST", p + "/database/query", map[string]any{"query": "select 1"}, 201},
		{"POST", p + "/secrets", []any{map[string]any{"name": "A", "value": "b"}}, 403},
		{"DELETE", p + "/secrets", []any{"A"}, 403},
		{"POST", p + "/database/migrations", map[string]any{"query": "create table t()", "name": "m"}, 403},
		{"PATCH", p + "/config/auth", map[string]any{"site_url": "https://x.example.test"}, 403},
		{"POST", p + "/api-keys", map[string]any{"type": "publishable", "name": "k"}, 403},
		{"POST", p + "/functions/deploy?slug=f", nil, 403},
		{"DELETE", p, nil, 403},
		{"POST", p + "/pause", nil, 403},
		{"GET", "/v1/organizations/default/members", nil, 200},
	} {
		rec := rf.doAs(ro, c.method, c.path, c.body)
		if rec.Code != c.want {
			t.Errorf("read-only token %s %s: %d %s, want %d", c.method, c.path, rec.Code, truncate(rec.Body.String(), 100), c.want)
		}
	}
	// An Owner's token may do them.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", p + "/secrets", []any{map[string]any{"name": "A", "value": "b"}}},
		{"POST", p + "/database/migrations", map[string]any{"query": "select 1", "name": "m"}},
	} {
		if rec := rf.doAs(owner, c.method, c.path, c.body); !isAllowed(rec) {
			t.Errorf("owner token %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// A token does not outlive its owner's membership: no role, no access.
	rf.status(200, "owner", "DELETE", orgBase+"/members/"+rf.ids["ro"], nil)
	if rec := rf.doAs(ro, "GET", p+"/secrets", nil); rec.Code != 403 {
		t.Errorf("token of a removed member: %d", rec.Code)
	}
	if n := list(ro); n != 0 {
		t.Errorf("a removed member's token lists %d projects", n)
	}
	// Roles changed in place apply to the token at once.
	rf.status(200, "owner", "PATCH", orgBase+"/members/"+rf.ids["dev"], map[string]any{"role_id": members.RoleReadOnly})
	dev := rf.pat("dev")
	if rec := rf.doAs(dev, "POST", p+"/secrets", []any{map[string]any{"name": "A", "value": "b"}}); rec.Code != 403 {
		t.Errorf("a demoted member's token: %d", rec.Code)
	}
}

func TestListsShowOnlyWhatTheRolesAllow(t *testing.T) {
	rf := newRolesFixture(t)
	refs := func(role, path string) []string {
		rec := rf.status(200, role, "GET", path, nil)
		var out []string
		switch v := decodeBody(t, rec).(type) {
		case []any:
			for _, p := range v {
				out = append(out, p.(map[string]any)["ref"].(string))
			}
		case map[string]any:
			for _, p := range v["projects"].([]any) {
				out = append(out, p.(map[string]any)["ref"].(string))
			}
		}
		return out
	}
	for _, path := range []string{"/v1/projects", "/platform/projects", "/platform/organizations/default/projects"} {
		for role, want := range map[string]int{"owner": 2, "admin": 2, "dev": 2, "ro": 2, "scoped": 1, "stranger": 0} {
			if role == "stranger" && strings.Contains(path, "organizations") {
				rf.status(403, role, "GET", path, nil) // not even the organization's page
				continue
			}
			got := refs(role, path)
			if len(got) != want {
				t.Errorf("%s %s lists %v, want %d", role, path, got, want)
			}
		}
	}
	if got := refs("scoped", "/v1/projects"); len(got) != 1 || got[0] != testRef {
		t.Errorf("scoped: %v", got)
	}
	orgs := func(role string) []map[string]any {
		return body[[]map[string]any](t, rf.status(200, role, "GET", "/platform/organizations", nil))
	}
	if len(orgs("stranger")) != 0 {
		t.Error("a user without memberships sees no organization")
	}
	if o := orgs("scoped"); len(o) != 1 || o[0]["is_owner"] != false {
		t.Errorf("scoped: %v", o)
	}
	if o := orgs("owner"); len(o) != 1 || o[0]["is_owner"] != true {
		t.Errorf("owner: %v", o)
	}
	if o := orgs("admin"); o[0]["is_owner"] != false {
		t.Errorf("admin is not an owner: %v", o)
	}
	var v1 []map[string]any
	_ = json.Unmarshal(rf.status(200, "stranger", "GET", "/v1/organizations", nil).Body.Bytes(), &v1)
	if len(v1) != 0 {
		t.Errorf("v1 organizations of a stranger: %v", v1)
	}
	// Project members: the roles that see a project.
	rec := rf.status(200, "ro", "GET", "/platform/projects/"+testRef+"/members", nil)
	if n := len(body[map[string]any](t, rec)["members"].([]any)); n != 5 {
		t.Errorf("members of the project: %d (owner, admin, developer, read-only and the scoped developer)", n)
	}
	rec = rf.status(200, "ro", "GET", "/platform/projects/"+secondRef+"/members", nil)
	if n := len(body[map[string]any](t, rec)["members"].([]any)); n != 4 {
		t.Errorf("members of the second project: %d", n)
	}
}

func TestOrganizationCreation(t *testing.T) {
	rf := newRolesFixture(t)
	for _, role := range []string{"admin", "dev", "ro", "scoped", "stranger"} {
		rf.status(403, role, "POST", "/platform/organizations", map[string]any{"name": "Mine " + role})
	}
	rec := rf.status(201, "owner", "POST", "/platform/organizations", map[string]any{"name": "Second Org"})
	validateAgainstSpec(t, "POST /platform/organizations", rec.Body.Bytes())
	a, _ := rf.srv.members.Access(context.Background(), rf.ids["owner"])
	var second *registry.Organization
	orgs, _ := rf.reg.ListOrganizations(context.Background())
	for i := range orgs {
		if orgs[i].Slug == "second-org" {
			second = &orgs[i]
		}
	}
	if second == nil || a.OrgRole(second.ID) != members.RoleOwner {
		t.Fatalf("the creator owns the new organization: %+v", a.Memberships)
	}
	// Nobody else sees it, and no one else has a role in it.
	rf.status(403, "admin", "GET", "/platform/organizations/second-org", nil)
	rf.status(403, "admin", "GET", "/platform/organizations/second-org/members", nil)
	// Projects go to an organization the caller may create in.
	rf.status(403, "dev", "POST", "/v1/projects", map[string]any{"name": "p", "organization_slug": "default", "db_pass": "pw-pw-pw-pw"})
	rf.status(403, "admin", "POST", "/v1/projects", map[string]any{"name": "p", "organization_slug": "second-org", "db_pass": "pw-pw-pw-pw"})
	if rec := rf.as("admin", "POST", "/v1/projects", map[string]any{"name": "p", "organization_slug": "default", "db_pass": "pw-pw-pw-pw"}); !isAllowed(rec) {
		t.Errorf("admin creates a project: %d %s", rec.Code, rec.Body)
	}
	if rec := rf.as("owner", "POST", "/v1/projects", map[string]any{"name": "p2", "db_pass": "pw-pw-pw-pw"}); !isAllowed(rec) {
		t.Errorf("a create without an organization picks one the caller may use: %d %s", rec.Code, rec.Body)
	}
	rf.status(403, "scoped", "POST", "/platform/projects", map[string]any{"name": "p", "organization_slug": "default"})
}

func TestMFAEnforcement(t *testing.T) {
	rf := newRolesFixture(t)
	aal2 := func(role string) string {
		return rf.signJWT(map[string]any{"sub": rf.ids[role], "email": role + "@example.test", "role": "authenticated", "aal": "aal2"})
	}
	if rec := rf.as("owner", "GET", orgBase+"/members/mfa/enforcement", nil); rec.Code != 201 || body[map[string]any](t, rec)["enforced"] != false {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	// Requiring MFA from a session without it would lock the caller out.
	rf.status(400, "owner", "PATCH", orgBase+"/members/mfa/enforcement", map[string]any{"enforced": true})
	if rec := rf.doAs(aal2("owner"), "PATCH", orgBase+"/members/mfa/enforcement", map[string]any{"enforced": true}); rec.Code != 201 || body[map[string]any](t, rec)["enforced"] != true {
		t.Fatalf("enforce: %d %s", rec.Code, rec.Body)
	}
	// Dashboard sessions without aal2 are refused in the organization's resources, but can
	// still see that MFA is required and sign out.
	rf.status(403, "admin", "GET", orgBase, nil)
	rec := rf.status(403, "admin", "GET", "/platform/projects/"+testRef, nil)
	if !strings.Contains(rec.Body.String(), "MFA required") {
		t.Errorf("message: %s", rec.Body)
	}
	orgs := body[[]map[string]any](t, rf.status(200, "admin", "GET", "/platform/organizations", nil))
	if len(orgs) != 1 || orgs[0]["organization_requires_mfa"] != true {
		t.Errorf("Studio needs organization_requires_mfa: %v", orgs)
	}
	rf.status(200, "admin", "GET", "/platform/profile/permissions", nil)
	if rec := rf.doAs(aal2("admin"), "GET", orgBase, nil); rec.Code != 200 {
		t.Errorf("aal2 session: %d", rec.Code)
	}
	// A token is not an interactive session: it is not held to the requirement.
	if rec := rf.doAs(rf.pat("admin"), "GET", "/v1/organizations/default/members", nil); rec.Code != 200 {
		t.Errorf("token: %d", rec.Code)
	}
	// Joining needs the second factor too.
	if rec := rf.doAs(aal2("owner"), "PATCH", orgBase+"/members/mfa/enforcement", map[string]any{"enforced": false}); rec.Code != 201 {
		t.Fatal(rec.Code)
	}
}

func TestAnAdministratorCannotTouchOwners(t *testing.T) {
	rf := newRolesFixture(t)
	owner2 := "bbbbbbbb-0000-4000-8000-000000000001"
	rf.addMember(owner2, members.RoleOwner)
	m := orgBase + "/members/"
	// Role changes.
	rf.status(403, "admin", "PATCH", m+owner2, map[string]any{"role_id": members.RoleDeveloper})
	rf.status(403, "admin", "PATCH", m+rf.ids["ro"], map[string]any{"role_id": members.RoleOwner})
	rf.status(403, "admin", "PATCH", m+rf.ids["admin"], map[string]any{"role_id": members.RoleOwner})
	rf.status(403, "admin", "DELETE", m+owner2, nil)
	rf.status(403, "admin", "DELETE", m+owner2+"/roles/1", nil)
	// Project-scoped Owner roles are Owner roles.
	rf.status(403, "admin", "PATCH", m+rf.ids["ro"], map[string]any{"role_id": members.RoleOwner, "role_scoped_projects": []string{testRef}})
	rf.status(200, "owner", "PATCH", m+rf.ids["ro"], map[string]any{"role_id": members.RoleOwner, "role_scoped_projects": []string{testRef}})
	roles := body[map[string]any](t, rf.status(200, "admin", "GET", orgBase+"/roles", nil))
	var ownerScoped float64
	for _, r := range roles["project_scoped_roles"].([]any) {
		if r.(map[string]any)["base_role_id"] == float64(members.RoleOwner) {
			ownerScoped = r.(map[string]any)["id"].(float64)
		}
	}
	if ownerScoped < members.ProjectRoleIDBase {
		t.Fatalf("scoped owner role not listed: %v", roles)
	}
	rf.status(403, "admin", "DELETE", m+rf.ids["ro"]+"/roles/"+itoa(int64(ownerScoped)), nil)
	rf.status(403, "admin", "PUT", m+rf.ids["ro"]+"/roles/"+itoa(int64(ownerScoped)), map[string]any{"name": "x", "role_scoped_projects": []string{testRef}})
	rf.status(403, "admin", "DELETE", m+rf.ids["ro"], nil)
	// Owners can.
	rf.status(200, "owner", "PATCH", m+owner2, map[string]any{"role_id": members.RoleDeveloper})
	// Administrators manage the other roles, including promoting to Administrator.
	rf.status(200, "admin", "PATCH", m+owner2, map[string]any{"role_id": members.RoleAdministrator})
	rf.status(200, "admin", "PATCH", m+rf.ids["dev"], map[string]any{"role_id": members.RoleReadOnly})
	rf.status(200, "admin", "DELETE", m+owner2, nil)
	// Developers and Read-only members manage nobody.
	for _, role := range []string{"dev", "ro", "scoped"} {
		rf.status(403, role, "PATCH", m+rf.ids["admin"], map[string]any{"role_id": members.RoleDeveloper})
		rf.status(403, role, "DELETE", m+rf.ids["admin"], nil)
	}
	// Anyone can leave, except the last Owner.
	rf.status(200, "scoped", "DELETE", m+rf.ids["scoped"], nil)
	rf.status(400, "owner", "DELETE", m+rf.ids["owner"], nil)
	rf.status(400, "owner", "PATCH", m+rf.ids["owner"], map[string]any{"role_id": members.RoleAdministrator})
	rec := rf.status(400, "owner", "DELETE", m+rf.ids["owner"]+"/roles/1", nil)
	if !strings.Contains(rec.Body.String(), "at least one Owner") {
		t.Errorf("message: %s", rec.Body)
	}
}

func TestMemberAndRoleRoutesFollowTheSpecs(t *testing.T) {
	rf := newRolesFixture(t)
	m := orgBase + "/members"
	rec := rf.status(200, "owner", "GET", m, nil)
	validateAgainstSpec(t, "GET /platform/organizations/{slug}/members", rec.Body.Bytes())
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 5 {
		t.Fatalf("members: %v", list)
	}
	byID := map[string]map[string]any{}
	for _, x := range list {
		byID[x["gotrue_id"].(string)] = x
	}
	if ids := byID[rf.ids["dev"]]["role_ids"].([]any); len(ids) != 1 || ids[0] != float64(members.RoleDeveloper) {
		t.Errorf("role_ids of the developer: %v", ids)
	}
	if ids := byID[rf.ids["scoped"]]["role_ids"].([]any); len(ids) != 1 || ids[0].(float64) < members.ProjectRoleIDBase {
		t.Errorf("role_ids of the scoped member list the project-scoped role: %v", ids)
	}
	rec = rf.status(200, "owner", "GET", orgBase+"/roles", nil)
	validateAgainstSpec(t, "GET /platform/organizations/{slug}/roles", rec.Body.Bytes())
	roles := body[map[string]any](t, rec)
	names := []string{}
	for _, r := range roles["org_scoped_roles"].([]any) {
		names = append(names, r.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "Owner,Administrator,Developer,Read-only" {
		t.Errorf("roles Studio sorts and filters by name: %v", names)
	}
	sr := roles["project_scoped_roles"].([]any)[0].(map[string]any)
	if sr["base_role_id"] != float64(members.RoleDeveloper) || sr["projects"].([]any)[0].(map[string]any)["ref"] != testRef {
		t.Errorf("scoped role: %v", sr)
	}
	// Studio's "manage access" for a member: scoped role on another project, then update, then unassign.
	devPatch := map[string]any{"role_id": members.RoleReadOnly, "role_scoped_projects": []string{secondRef}}
	rf.status(200, "owner", "PATCH", m+"/"+rf.ids["scoped"], devPatch)
	roles = body[map[string]any](t, rf.status(200, "owner", "GET", orgBase+"/roles", nil))
	var roID float64
	for _, r := range roles["project_scoped_roles"].([]any) {
		if r.(map[string]any)["base_role_id"] == float64(members.RoleReadOnly) {
			roID = r.(map[string]any)["id"].(float64)
		}
	}
	if roID == 0 {
		t.Fatalf("no read-only scoped role: %v", roles)
	}
	rf.status(200, "owner", "PUT", m+"/"+rf.ids["scoped"]+"/roles/"+itoa(int64(roID)), map[string]any{"name": "Read-only", "role_scoped_projects": []string{secondRef}})
	// The scoped member now reads the second project and still works on the first.
	rf.status(200, "scoped", "GET", "/platform/projects/"+secondRef, nil)
	rf.status(403, "scoped", "POST", "/v1/projects/"+secondRef+"/secrets", []any{})
	rf.status(200, "owner", "DELETE", m+"/"+rf.ids["scoped"]+"/roles/"+itoa(int64(roID)), nil)
	rf.status(403, "scoped", "GET", "/platform/projects/"+secondRef, nil)
	// Bad input.
	rf.status(400, "owner", "PATCH", m+"/"+rf.ids["dev"], map[string]any{"role_id": 99})
	rf.status(400, "owner", "PATCH", m+"/"+rf.ids["dev"], map[string]any{"role_id": 3, "role_scoped_projects": []string{"zzzzzzzzzzzzzzzzzzzz"}})
	rf.status(404, "owner", "PATCH", m+"/aaaaaaaa-9999-4000-8000-000000000000", map[string]any{"role_id": 3})
	rf.status(400, "owner", "PUT", m+"/"+rf.ids["dev"]+"/roles/3", map[string]any{"name": "Developer", "role_scoped_projects": []string{testRef}})
	rf.status(404, "owner", "DELETE", m+"/"+rf.ids["dev"]+"/roles/1999", nil)
	rec = rf.status(200, "owner", "GET", m+"/reached-free-project-limit", nil)
	validateAgainstSpec(t, "GET /platform/organizations/{slug}/members/reached-free-project-limit", rec.Body.Bytes())
	// The /v1 route the CLI and MCP server use, and the /v2 ones.
	rec = rf.status(200, "ro", "GET", "/v1/organizations/default/members", nil)
	validateAgainstSpec(t, "GET /v1/organizations/{slug}/members", rec.Body.Bytes())
	for _, x := range body[[]map[string]any](t, rec) {
		if x["user_id"] == rf.ids["owner"] && x["role_name"] != "Owner" {
			t.Errorf("v1 role_name: %v", x)
		}
	}
	rec = rf.status(200, "ro", "GET", "/v2/organizations/default/members", nil)
	validateAgainstSpec(t, "GET /v2/organizations/{slug}/members", rec.Body.Bytes())
	rec = rf.status(200, "ro", "GET", "/v2/organizations/default/roles", nil)
	validateAgainstSpec(t, "GET /v2/organizations/{slug}/roles", rec.Body.Bytes())
	rec = rf.status(200, "admin", "PATCH", "/v2/organizations/default/members/"+rf.ids["ro"]+"/roles",
		map[string]any{"data": map[string]any{"type": "organization_member_role", "attributes": map[string]any{"role": "developer"}}})
	validateAgainstSpec(t, "PATCH /v2/organizations/{slug}/members/{user_id}/roles", rec.Body.Bytes())
	if a, _ := rf.srv.members.Access(context.Background(), rf.ids["ro"]); a.OrgRole(rf.org.ID) != members.RoleDeveloper {
		t.Error("v2 assignment did not change the role")
	}
	rf.status(403, "dev", "PATCH", "/v2/organizations/default/members/"+rf.ids["ro"]+"/roles",
		map[string]any{"data": map[string]any{"type": "organization_member_role", "attributes": map[string]any{"role": "owner"}}})
	rec = rf.status(200, "admin", "PATCH", "/v2/organizations/default/members/"+rf.ids["ro"]+"/roles",
		map[string]any{"data": map[string]any{"type": "organization_member_role", "attributes": map[string]any{"role": "read-only", "projects": []map[string]any{{"ref": testRef}}}}})
	validateAgainstSpec(t, "PATCH /v2/organizations/{slug}/members/{user_id}/roles", rec.Body.Bytes())
}

func TestInvitationsEndToEnd(t *testing.T) {
	rf := newRolesFixture(t)
	inv := orgBase + "/members/invitations"
	// An existing dashboard account gets a link to the invitation.
	inviteeID := "cccccccc-0000-4000-8000-000000000001"
	rf.gt.addUser(inviteeID, "new@example.test", time.Now())
	invitee := rf.signJWT(map[string]any{"sub": inviteeID, "email": "New@Example.test", "role": "authenticated"})
	rf.status(400, "admin", "POST", inv, map[string]any{"emails": []string{"new@example.test", "bad address"}, "role_id": members.RoleDeveloper})
	rec := rf.status(201, "admin", "POST", inv, map[string]any{"emails": []string{"new@example.test", "dev@example.test"}, "role_id": members.RoleDeveloper})
	// (The spec's email pattern uses look-ahead, which the test validator cannot compile, so the
	// shape is checked by hand: succeeded is a list of addresses, failed a list of {email, error}.)
	res := body[map[string]any](t, rec)
	for _, f := range res["failed"].([]any) {
		if m := f.(map[string]any); m["email"] == nil || m["error"] == nil {
			t.Errorf("failed entry: %v", m)
		}
	}
	if s := res["succeeded"].([]any); len(s) != 1 || s[0] != "new@example.test" {
		t.Fatalf("succeeded: %v", res)
	}
	if f := res["failed"].([]any); len(f) != 1 || f[0].(map[string]any)["email"] != "dev@example.test" {
		t.Fatalf("failed: %v (an existing member)", res["failed"])
	}
	link := res["invite_links"].([]any)[0].(map[string]any)
	u, err := url.Parse(link["url"].(string))
	if err != nil || u.Path != "/join" || u.Query().Get("slug") != "default" || !strings.HasPrefix(u.Query().Get("token"), members.InvitationPrefix) {
		t.Fatalf("link: %v", link)
	}
	token := u.Query().Get("token")
	rec = rf.status(200, "ro", "GET", inv, nil)
	validateAgainstSpec(t, "GET /platform/organizations/{slug}/members/invitations", rec.Body.Bytes())
	list := body[map[string]any](t, rec)["invitations"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["invited_email"] != "new@example.test" || list[0].(map[string]any)["role_id"] != float64(members.RoleDeveloper) {
		t.Fatalf("invitations: %v", list)
	}
	rf.status(403, "scoped", "GET", inv, nil) // project-scoped members do not read invitations
	// The token's page: the right account, a wrong one, and a stranger to the organization.
	rec = rf.doAs(invitee, "GET", inv+"/"+token, nil)
	validateAgainstSpec(t, "GET /platform/organizations/{slug}/members/invitations/{token}", rec.Body.Bytes())
	st := body[map[string]any](t, rec)
	if st["email_match"] != true || st["authorized_user"] != true || st["expired_token"] != false || st["token_does_not_exist"] != false || st["organization_name"] != "Default" {
		t.Errorf("state: %v", st)
	}
	if st := body[map[string]any](t, rf.status(200, "ro", "GET", inv+"/"+token, nil)); st["email_match"] != false || st["authorized_user"] != false {
		t.Errorf("another account: %v", st)
	}
	if st := body[map[string]any](t, rf.doAs(invitee, "GET", inv+"/sbo_nope", nil)); st["token_does_not_exist"] != true {
		t.Errorf("unknown token: %v", st)
	}
	rf.status(403, "ro", "POST", inv+"/"+token, nil) // the invitation is for another address
	if rec := rf.doAs(invitee, "POST", inv+"/"+token, nil); rec.Code != 201 {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	if a, _ := rf.srv.members.Access(context.Background(), inviteeID); a.OrgRole(rf.org.ID) != members.RoleDeveloper {
		t.Fatalf("the accepted invitation made no developer: %+v", a.Memberships)
	}
	if rec := rf.doAs(invitee, "POST", inv+"/"+token, nil); rec.Code != 409 {
		t.Errorf("second accept: %d", rec.Code)
	}
	if rec := rf.doAs(invitee, "GET", inv+"/"+token, nil); rec.Code != 401 || !strings.Contains(rec.Body.String(), "Failed to retrieve organization") {
		t.Errorf("an accepted invitation is reported as no longer available: %d %s", rec.Code, rec.Body)
	}
	if rec := rf.doAs(invitee, "GET", orgBase+"/projects", nil); rec.Code != 200 {
		t.Errorf("the new member lists projects: %d", rec.Code)
	}
	// Roles an Administrator may not invite; Developers invite nobody.
	rf.status(403, "admin", "POST", inv, map[string]any{"emails": []string{"o@example.test"}, "role_id": members.RoleOwner})
	rf.status(403, "dev", "POST", inv, map[string]any{"emails": []string{"o@example.test"}, "role_id": members.RoleReadOnly})
	rf.status(400, "owner", "POST", inv, map[string]any{"role_id": members.RoleReadOnly})
	// The API's own body shape, with a role name and projects.
	rec = rf.status(201, "owner", "POST", inv, map[string]any{"data": []map[string]any{{"attributes": map[string]any{"email": "scoped-new@example.test", "role": "developer", "projects": []map[string]any{{"ref": testRef}}}}}})
	if s := body[map[string]any](t, rec)["succeeded"].([]any); len(s) != 1 {
		t.Fatalf("data form: %s", rec.Body)
	}
	list = body[map[string]any](t, rf.status(200, "owner", "GET", inv, nil))["invitations"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["role_id"].(float64) < members.InvitationRoleIDBase {
		t.Fatalf("a project-scoped invitation is listed under a project-scoped role id: %v", list)
	}
	roles := body[map[string]any](t, rf.status(200, "owner", "GET", orgBase+"/roles", nil))["project_scoped_roles"].([]any)
	var found bool
	for _, r := range roles {
		if r.(map[string]any)["id"] == list[0].(map[string]any)["role_id"] {
			found = true
		}
	}
	if !found {
		t.Errorf("Studio looks the invitation's role up in the project-scoped roles: %v", roles)
	}
	// Revoke it.
	id := itoa(int64(list[0].(map[string]any)["id"].(float64)))
	rf.status(403, "dev", "DELETE", inv+"/"+id, nil)
	rf.status(200, "admin", "DELETE", inv+"/"+id, nil)
	rf.status(404, "admin", "DELETE", inv+"/"+id, nil)
	if l := body[map[string]any](t, rf.status(200, "owner", "GET", inv, nil))["invitations"].([]any); len(l) != 0 {
		t.Errorf("revoked: %v", l)
	}
	// v2 create and delete by address.
	rec = rf.status(201, "admin", "POST", "/v2/organizations/default/members/invitations", map[string]any{"data": []map[string]any{{"type": "organization_invitation", "attributes": map[string]any{"email": "v2@example.test", "role": "read-only"}}}})
	validateAgainstSpec(t, "POST /v2/organizations/{slug}/members/invitations", rec.Body.Bytes())
	rf.status(403, "admin", "POST", "/v2/organizations/default/members/invitations", map[string]any{"data": []map[string]any{{"type": "organization_invitation", "attributes": map[string]any{"email": "v2o@example.test", "role": "owner"}}}})
	rec = rf.status(200, "admin", "DELETE", "/v2/organizations/default/members/invitations", map[string]any{"data": []map[string]any{{"type": "organization_invitation", "attributes": map[string]any{"email": "v2@example.test"}}}})
	validateAgainstSpec(t, "DELETE /v2/organizations/{slug}/members/invitations", rec.Body.Bytes())
	// An expired invitation.
	rf.status(201, "owner", "POST", inv, map[string]any{"emails": []string{"late@example.test"}, "role_id": members.RoleReadOnly})
	pend, _ := rf.srv.members.Invitations(context.Background(), rf.org.ID)
	if len(pend) != 1 {
		t.Fatalf("pending: %+v", pend)
	}
}

func TestAnInviteForANewAddressCreatesTheAccountAndJoins(t *testing.T) {
	rf := newRolesFixture(t)
	rec := rf.status(201, "owner", "POST", orgBase+"/members/invitations", map[string]any{"emails": []string{"fresh@example.test"}, "role_id": members.RoleDeveloper})
	link := body[map[string]any](t, rec)["invite_links"].([]any)[0].(map[string]any)["url"].(string)
	u, err := url.Parse(link)
	if err != nil || u.Path != "/claim" {
		t.Fatalf("a new address is sent to the account page: %q", link)
	}
	frag, _ := url.ParseQuery(u.Fragment)
	token, email := frag.Get("token"), frag.Get("email")
	if !strings.HasPrefix(token, invitePrefix) || email != "fresh@example.test" {
		t.Fatalf("fragment: %v", frag)
	}
	cr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/claim", strings.NewReader(`{"token":"`+token+`","email":"`+email+`","password":"correct horse battery"}`))
	req.Header.Set("Content-Type", "application/json")
	rf.srv.ServeHTTP(cr, req)
	if cr.Code != 201 {
		t.Fatalf("claim: %d %s", cr.Code, cr.Body)
	}
	res := body[RedeemResult](t, cr)
	a, err := rf.srv.members.Access(context.Background(), res.UserID)
	if err != nil || a.OrgRole(rf.org.ID) != members.RoleDeveloper {
		t.Fatalf("the invited developer: %+v %v", a, err)
	}
	// The invitation was used up by the account creation.
	if l := body[map[string]any](t, rf.status(200, "owner", "GET", orgBase+"/members/invitations", nil))["invitations"].([]any); len(l) != 0 {
		t.Errorf("invitations: %v", l)
	}
	// The new account is recorded, so that its first sign-in is not a legacy account's.
	if _, err := rf.srv.store.GetUser(context.Background(), res.UserID); err != nil {
		t.Errorf("user not recorded: %v", err)
	}
}

func TestInvitationMail(t *testing.T) {
	rf := newRolesFixture(t)
	rf.cfg.Mail = config.Mail{SMTPHost: "smtp.example.test", SMTPPort: 587, SMTPFrom: "sbctl@example.test"}
	inv := orgBase + "/members/invitations"
	existing := "dddddddd-0000-4000-8000-000000000001"
	rf.gt.addUser(existing, "have@example.test", time.Now())
	rec := rf.status(201, "owner", "POST", inv, map[string]any{"emails": []string{"have@example.test", "brand-new@example.test"}, "role_id": members.RoleReadOnly})
	links := body[map[string]any](t, rec)["invite_links"].([]any)
	for _, l := range links {
		if l.(map[string]any)["emailed"] != true {
			t.Errorf("not emailed: %v", l)
		}
	}
	calls := rf.gt.callList()
	var magic, invite, mark int
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "POST /magiclink?"):
			magic++
			q, _ := url.ParseQuery(strings.TrimPrefix(c, "POST /magiclink?"))
			ru, _ := url.Parse(q.Get("redirect_to"))
			if ru.Path != "/join" || !strings.HasPrefix(ru.Query().Get("token"), members.InvitationPrefix) || ru.Host != "studio.example.test" {
				t.Errorf("magic link redirect: %s", q.Get("redirect_to"))
			}
		case strings.HasPrefix(c, "POST /invite?"):
			invite++
		case strings.HasPrefix(c, "PUT /admin/users/"):
			mark++
		}
	}
	if magic != 1 || invite != 1 || mark != 1 {
		t.Fatalf("calls to the sign-in service: %v", calls)
	}
	// The account GoTrue invited carries the dashboard claim, so that the session works.
	var invited map[string]any
	for _, u := range rf.gt.users {
		if u["email"] == "brand-new@example.test" {
			invited = u
		}
	}
	if invited == nil || invited["app_metadata"].(map[string]any)[AdminClaim] != true {
		t.Errorf("invited account: %v", invited)
	}
	// A relay that fails does not lose the invitation: the caller gets the link.
	rf.gt.mu.Lock()
	rf.gt.mailFail = true
	rf.gt.mu.Unlock()
	rec = rf.status(201, "owner", "POST", inv, map[string]any{"emails": []string{"unlucky@example.test"}, "role_id": members.RoleReadOnly})
	l := body[map[string]any](t, rec)["invite_links"].([]any)[0].(map[string]any)
	if l["emailed"] != false || !strings.Contains(l["url"].(string), "/claim#") {
		t.Errorf("fallback link: %v", l)
	}
	// Without a relay no mail is attempted.
	rf.cfg.Mail = config.Mail{}
	before := len(rf.gt.callList())
	rf.status(201, "owner", "POST", inv, map[string]any{"emails": []string{"nomail@example.test"}, "role_id": members.RoleReadOnly})
	if got := len(rf.gt.callList()); got != before {
		t.Errorf("mail attempted without SMTP: %v", rf.gt.callList()[before:])
	}
}

func TestAccountsFromBeforeRolesBecomeOwners(t *testing.T) {
	rf := newFixtureWithLegacy(t)
	old := "eeeeeeee-0000-4000-8000-000000000001"
	rf.gt.addUser(old, "old@example.test", time.Now().Add(-48*time.Hour))
	tok := rf.signJWT(map[string]any{"sub": old, "email": "old@example.test", "role": "authenticated"})
	if rec := rf.doAs(tok, "GET", "/platform/organizations/default", nil); rec.Code != 200 {
		t.Fatalf("a legacy account: %d %s", rec.Code, rec.Body)
	}
	if rec := rf.doAs(tok, "PATCH", "/platform/organizations/default", map[string]any{"name": "Still mine"}); rec.Code != 200 {
		t.Fatalf("a legacy account is Owner: %d %s", rec.Code, rec.Body)
	}
	young := "eeeeeeee-0000-4000-8000-000000000002"
	rf.gt.addUser(young, "young@example.test", time.Now())
	tok2 := rf.signJWT(map[string]any{"sub": young, "email": "young@example.test", "role": "authenticated"})
	if rec := rf.doAs(tok2, "GET", "/platform/organizations/default", nil); rec.Code != 403 {
		t.Fatalf("an account created after roles has no access until invited: %d", rec.Code)
	}
}

// newFixtureWithLegacy is a fixture whose store knows when roles began.
func newFixtureWithLegacy(t testing.TB) *fixture {
	f := newFixture(t)
	f.srv.members.Store.(*members.Memory).Cutoff = time.Now().Add(-time.Hour)
	return f
}
