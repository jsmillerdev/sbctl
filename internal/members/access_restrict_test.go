package members

import (
	"context"
	"reflect"
	"testing"
)

// U7: Restrict removes every organization but one from the questions an Access answers.
func TestAccessRestrict(t *testing.T) {
	ctx := context.Background()
	e := newMemEnv(t)
	owner := e.owner(t, e.a)
	ownerA := e.access(t, owner)

	// One user: Owner of a, Administrator of b.
	u := e.owner(t, e.a)
	if err := e.svc.Store.Update(ctx, e.b.ID, func(ops Ops) error {
		return ops.PutMember(ctx, Member{OrgID: e.b.ID, UserID: u, RoleID: RoleAdministrator})
	}); err != nil {
		t.Fatal(err)
	}
	full := e.access(t, u)
	if !full.IsMember(e.a.ID) || !full.IsMember(e.b.ID) || !full.IsOwnerAnywhere() {
		t.Fatalf("fixture: %+v", full.Memberships)
	}

	inA := full.Restrict(e.a.ID)
	if inA == full {
		t.Fatal("Restrict must return a copy")
	}
	if inA.UserID != u || !inA.IsMember(e.a.ID) || inA.IsMember(e.b.ID) {
		t.Errorf("restricted to a: %+v", inA.Memberships)
	}
	if inA.OrgRole(e.a.ID) != RoleOwner || inA.OrgRole(e.b.ID) != 0 {
		t.Errorf("roles: a=%d b=%d", inA.OrgRole(e.a.ID), inA.OrgRole(e.b.ID))
	}
	if !inA.Can(e.a, "", ActUpdate, ResOrg, nil) {
		t.Error("an Owner of a lost a right in a")
	}
	if inA.Can(e.b, "", ActRead, ResOrg, nil) || inA.Can(e.b, e.refs[0], ActSQLQuery, ResAny, nil) {
		t.Error("the copy still allows something in b")
	}
	if got := inA.Permissions([]OrgRef{e.a, e.b}); len(got) == 0 {
		t.Error("no permissions in a")
	} else {
		for _, p := range got {
			if p.OrganizationID != e.a.ID {
				t.Errorf("permission of another organization: %+v", p)
			}
		}
	}

	// Restricted to b, the Owner of a is an Administrator: IsOwnerAnywhere and the Owner-only rights go.
	inB := full.Restrict(e.b.ID)
	if inB.IsOwnerAnywhere() || !inB.IsOperatorAnywhere() || inB.IsMember(e.a.ID) {
		t.Errorf("restricted to b: owner=%v operator=%v memberOfA=%v", inB.IsOwnerAnywhere(), inB.IsOperatorAnywhere(), inB.IsMember(e.a.ID))
	}
	if inB.Can(e.a, "", ActUpdate, ResOrg, nil) {
		t.Error("the copy still allows an Owner action in a")
	}
	// An Administrator keeps the restrictions that depend on the Owner-scoped role ids of its organization.
	want := full.Permissions([]OrgRef{e.b})
	if got := inB.Permissions([]OrgRef{e.b}); !reflect.DeepEqual(got, want) {
		t.Errorf("Administrator permissions in b changed by Restrict:\n got %+v\nwant %+v", got, want)
	}
	for _, role := range []int64{int64(RoleOwner), int64(RoleAdministrator), int64(RoleDeveloper)} {
		if inB.CanRole(e.b, ActCreate, ResSubjectRoles, role) != full.CanRole(e.b, ActCreate, ResSubjectRoles, role) {
			t.Errorf("role %d: the copy and the original disagree about changing it in b", role)
		}
	}

	// A user who is not a member of the organization gets an Access that allows nothing.
	none := e.access(t, e.member(t, e.b, RoleDeveloper)).Restrict(e.a.ID)
	if len(none.Memberships) != 0 || none.IsMember(e.a.ID) || none.IsOwnerAnywhere() || none.IsOperatorAnywhere() ||
		none.Can(e.a, "", ActRead, ResOrg, nil) || len(none.Permissions([]OrgRef{e.a, e.b})) != 0 {
		t.Errorf("a non-member's restricted access: %+v", none)
	}
	var nilAccess *Access
	if r := nilAccess.Restrict(e.a.ID); r == nil || r.IsMember(e.a.ID) {
		t.Errorf("Restrict on a nil Access: %+v", r)
	}

	// The original is untouched.
	if !full.IsMember(e.a.ID) || !full.IsMember(e.b.ID) || len(full.Memberships) != 2 {
		t.Errorf("Restrict changed its receiver: %+v", full.Memberships)
	}

	// Project-scoped roles come along for their organization and no other.
	s := e.member(t, e.a, 0)
	if err := e.svc.AssignProjectRole(ctx, ownerA, e.a, s, RoleDeveloper, []string{e.refs[0]}); err != nil {
		t.Fatal(err)
	}
	scoped := e.access(t, s)
	r := scoped.Restrict(e.a.ID)
	if !r.Can(e.a, e.refs[0], ActSQLInsert, ResAny, nil) || r.Can(e.a, e.refs[1], ActRead, ResProjects, nil) {
		t.Errorf("project-scoped role after Restrict: %+v", r.Permissions([]OrgRef{e.a}))
	}
	if r.Can(e.b, e.refs[0], ActSQLInsert, ResAny, nil) {
		t.Error("the project-scoped role leaked into b")
	}
	// Changing the copy does not reach the original.
	r.Memberships[0].Scoped[0].Refs[0] = "zzzzzzzzzzzzzzzzzzzz"
	if scoped.Memberships[0].Scoped[0].Refs[0] != e.refs[0] {
		t.Error("the copy shares its project refs with the original")
	}
}
