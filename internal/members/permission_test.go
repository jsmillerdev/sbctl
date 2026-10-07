package members

import (
	"testing"
)

var testOrg = OrgRef{ID: 1, Slug: "acme"}

func accessWith(role int, scoped ...ProjectRole) *Access {
	return &Access{UserID: "u", Memberships: []Membership{{OrgID: 1, RoleID: role, Scoped: scoped}}, ownerScoped: map[int64][]int64{1: {1001}}}
}

func TestCheckSemantics(t *testing.T) {
	// Studio's rules: empty list denies, % wildcards, restrictive beats grant, scoped entries
	// take over for their project.
	if Check(nil, "read:Read", "x", nil, "acme", "") {
		t.Fatal("empty list must deny")
	}
	perms := []Permission{
		{Actions: []string{"%"}, Resources: []string{"%"}, OrganizationSlug: "acme"},
		{Actions: []string{"write:Update"}, Resources: []string{"organizations"}, OrganizationSlug: "acme", Restrictive: true},
		{Actions: []string{"read:Read"}, Resources: []string{"projects"}, OrganizationSlug: "acme", ProjectRefs: []string{"p1"}},
	}
	for _, c := range []struct {
		action, res, ref, org string
		want                  bool
	}{
		{"write:Create", "projects", "", "acme", true},
		{"write:Update", "organizations", "", "acme", false},  // restrictive
		{"write:Update", "organizations", "", "other", false}, // other organization
		{"read:Read", "projects", "p1", "acme", true},
		{"write:Update", "projects", "p1", "acme", true}, // no scoped entry matches this action: org-wide applies
		{"read:Read", "a.b", "", "acme", true},
	} {
		if got := Check(perms, c.action, c.res, nil, c.org, c.ref); got != c.want {
			t.Errorf("%s %s ref=%q org=%s: got %v want %v", c.action, c.res, c.ref, c.org, got, c.want)
		}
	}
	// Once a scoped entry matches, only scoped entries count.
	scoped := []Permission{
		{Actions: []string{"%"}, Resources: []string{"%"}, OrganizationSlug: "acme"},
		{Actions: []string{"read:Read"}, Resources: []string{"%"}, OrganizationSlug: "acme", ProjectRefs: []string{"p1"}},
		{Actions: []string{"read:Read"}, Resources: []string{"secret"}, OrganizationSlug: "acme", ProjectRefs: []string{"p1"}, Restrictive: true},
	}
	if Check(scoped, "read:Read", "secret", nil, "acme", "p1") {
		t.Error("restrictive scoped entry must deny")
	}
	if !Check(scoped, "read:Read", "secret", nil, "acme", "p2") {
		t.Error("another project uses the org-wide entries")
	}
}

func TestJSONLogic(t *testing.T) {
	rule := map[string]any{"in": []any{map[string]any{"var": "resource.role_id"}, []any{float64(1), float64(1001)}}}
	for _, c := range []struct {
		id   any
		want bool
	}{{float64(1), true}, {int64(1001), true}, {float64(2), false}, {nil, false}} {
		got := truthy(applyLogic(rule, map[string]any{"resource": map[string]any{"role_id": c.id}}))
		if got != c.want {
			t.Errorf("role_id %v: got %v", c.id, got)
		}
	}
	and := map[string]any{"and": []any{map[string]any{"==": []any{1, 1}}, map[string]any{"!": []any{false}}}}
	if !truthy(applyLogic(and, nil)) {
		t.Error("and")
	}
	if truthy(applyLogic(map[string]any{"unknown": []any{1}}, nil)) {
		t.Error("unknown operators must be false")
	}
}

// can is shorthand for the role matrix below.
func can(a *Access, ref, action, resource string) bool {
	return a.Can(testOrg, ref, action, resource, nil)
}

func TestRoleCapabilities(t *testing.T) {
	type check struct {
		action, resource      string
		owner, admin, dev, ro bool
	}
	checks := []check{
		// organization
		{ActRead, ResOrg, true, true, true, true},
		{ActUpdate, ResOrg, true, false, false, false},
		{ActDelete, ResOrg, true, false, false, false},
		{ActUpdate, ResProjectTransfer, true, false, false, false},
		// projects
		{ActCreate, ResProjects, true, true, false, false},
		{ActUpdate, ResProjects, true, true, false, false},
		{ActDelete, ResProjects, true, true, false, false},
		{ActRead, ResProjects, true, true, true, true},
		{ActInfraExecute, "reboot", true, true, true, false}, // hosted: Restart is for Developers too
		{ActInfraExecute, "queue_jobs.projects.pause", true, true, false, false},
		{ActInfraExecute, "queue_job.walg.prepare_restore", true, true, false, false}, // a point-in-time restore overwrites production data
		{ActInfraExecute, "queue_job.restore.prepare", true, true, false, false},
		// settings and keys
		{ActUpdate, "custom_config_gotrue", true, true, false, false},
		{ActCreate, ResServiceKeys, true, true, false, false},
		{ActRead, ResServiceKeys, true, true, true, false},
		{ActRead, ResJWTSecret, true, true, true, false},
		{ActRead, ResS3Credentials, true, true, true, false},
		{ActRead, "custom_config_gotrue", true, true, true, true},
		{ActSecretsWrite, ResAny, true, true, false, false},
		{ActSecretsRead, ResAny, true, true, true, true},
		// SQL, schema and content
		{ActSQLQuery, ResAny, true, true, true, true},
		{ActSQLInsert, ResAny, true, true, true, false},
		{ActSQLAdminWrite, "tables", true, true, true, false},
		{ActSQLAdminRead, "tables", true, true, true, true},
		{ActAuthExecute, "create_user", true, true, true, false},
		{ActSQLDelete, "auth.users", true, true, true, false},
		{ActStorageAdminWrite, ResAny, true, true, true, false},
		{ActStorageAdminRead, ResAny, true, true, true, true},
		{ActFunctionsWrite, ResAny, true, true, true, false},
		{ActFunctionsRead, ResAny, true, true, true, true},
		{ActCreate, ResUserContent, true, true, true, true},
		{ActCreate, ResPreviewBranches, true, true, true, false},
		// billing
		{ActBillingRead, "stripe.subscriptions", true, true, true, true},
		{ActBillingWrite, "stripe.subscriptions", true, true, false, false},
	}
	for _, c := range checks {
		for role, want := range map[int]bool{RoleOwner: c.owner, RoleAdministrator: c.admin, RoleDeveloper: c.dev, RoleReadOnly: c.ro} {
			if got := can(accessWith(role), "", c.action, c.resource); got != want {
				t.Errorf("%s: %s on %s: got %v want %v", RoleName(role), c.action, c.resource, got, want)
			}
		}
	}
}

// Saved content: every role creates, a Developer or Read-only member changes only their own
// (Studio passes the item's owner_id and subject.id), and Read-only has no reports.
func TestSavedContentPermissions(t *testing.T) {
	item := func(typ string, owner, me int64) map[string]any { return OwnContentData(typ, "project", owner, me) }
	for _, c := range []struct {
		role   int
		action string
		data   map[string]any
		want   bool
	}{
		{RoleOwner, ActUpdate, item("sql", 1, 2), true},
		{RoleAdministrator, ActDelete, item("sql", 1, 2), true},
		{RoleDeveloper, ActUpdate, item("sql", 2, 2), true},
		{RoleDeveloper, ActUpdate, item("sql", 1, 2), false},
		{RoleDeveloper, ActDelete, item("report", 1, 2), false},
		{RoleDeveloper, ActCreate, item("report", 2, 2), true},
		{RoleReadOnly, ActUpdate, item("sql", 2, 2), true},
		{RoleReadOnly, ActDelete, item("sql", 1, 2), false},
		{RoleReadOnly, ActUpdate, item("report", 2, 2), false},
		{RoleReadOnly, ActCreate, item("sql", 2, 2), true},
		{RoleReadOnly, ActCreate, item("report", 2, 2), false},
	} {
		if got := accessWith(c.role).Can(testOrg, "", c.action, ResUserContent, c.data); got != c.want {
			t.Errorf("%s %s %v: got %v want %v", RoleName(c.role), c.action, c.data, got, c.want)
		}
	}
}

func TestMemberManagementPermissions(t *testing.T) {
	// Owners manage every role, Administrators every role but Owner (organization-wide and
	// project-scoped), Developers and Read-only none.
	type c struct {
		role     int
		target   int64
		add, rem bool
	}
	for _, x := range []c{
		{RoleOwner, RoleOwner, true, true}, {RoleOwner, RoleReadOnly, true, true}, {RoleOwner, 1001, true, true},
		{RoleAdministrator, RoleOwner, false, false}, {RoleAdministrator, RoleAdministrator, true, true},
		{RoleAdministrator, RoleDeveloper, true, true}, {RoleAdministrator, RoleReadOnly, true, true},
		{RoleAdministrator, 1001, false, false}, // a project-scoped Owner role
		{RoleAdministrator, 1002, true, true},   // another project-scoped role
		{RoleDeveloper, RoleReadOnly, false, false}, {RoleReadOnly, RoleReadOnly, false, false},
	} {
		a := accessWith(x.role)
		if got := a.CanRole(testOrg, ActCreate, ResSubjectRoles, x.target); got != x.add {
			t.Errorf("%s add role %d: got %v want %v", RoleName(x.role), x.target, got, x.add)
		}
		if got := a.CanRole(testOrg, ActDelete, ResUserInvites, x.target); got != x.rem {
			t.Errorf("%s revoke invite of role %d: got %v want %v", RoleName(x.role), x.target, got, x.rem)
		}
	}
}

func TestProjectScopedPermissions(t *testing.T) {
	dev := accessWith(0, ProjectRole{ID: 1002, OrgID: 1, BaseRoleID: RoleDeveloper, Refs: []string{"pa"}})
	if !can(dev, "pa", ActRead, ResProjects) || !can(dev, "pa", ActSQLInsert, ResAny) {
		t.Error("a scoped Developer works on its project")
	}
	if can(dev, "pb", ActRead, ResProjects) || can(dev, "pb", ActSQLQuery, ResAny) {
		t.Error("a scoped member must not see other projects")
	}
	if can(dev, "pa", ActUpdate, ResProjects) {
		t.Error("a scoped Developer cannot change the project")
	}
	if !a2(dev, ActRead, ResOrg) || a2(dev, ActUpdate, ResOrg) {
		t.Error("a scoped member can read the organization and not change it")
	}
	ro := accessWith(RoleDeveloper, ProjectRole{ID: 1003, OrgID: 1, BaseRoleID: RoleReadOnly, Refs: []string{"pa"}})
	if can(ro, "pa", ActRead, ResServiceKeys) {
		t.Error("the scoped Read-only role takes over on its project: no service key")
	}
	if !can(ro, "pb", ActRead, ResServiceKeys) {
		t.Error("other projects use the organization-wide Developer role")
	}
	var none Access
	if none.Can(testOrg, "", ActRead, ResOrg, nil) {
		t.Error("a user without memberships may do nothing")
	}
}

func a2(a *Access, action, resource string) bool { return a.Can(testOrg, "", action, resource, nil) }

func TestPermissionEntriesShape(t *testing.T) {
	a := accessWith(RoleAdministrator, ProjectRole{ID: 1002, OrgID: 1, BaseRoleID: RoleDeveloper, Refs: []string{"pa"}})
	ps := a.Permissions([]OrgRef{testOrg})
	if len(ps) == 0 {
		t.Fatal("no entries")
	}
	var restrictive, scoped int
	for _, p := range ps {
		if p.OrganizationSlug != "acme" || p.OrganizationID != 1 {
			t.Fatalf("entry without organization: %+v", p)
		}
		if p.Restrictive {
			restrictive++
		}
		if len(p.ProjectRefs) > 0 {
			scoped++
		}
	}
	if restrictive != 3 || scoped != 6 {
		t.Fatalf("restrictive=%d scoped=%d: %+v", restrictive, scoped, ps)
	}
	if got := accessWith(RoleOwner).Permissions([]OrgRef{testOrg}); len(got) != 1 || got[0].Actions[0] != "%" || got[0].Resources[0] != "%" || got[0].Condition != nil {
		t.Fatalf("owner: %+v", got)
	}
}
