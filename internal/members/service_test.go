package members

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// env is what the service tests run over: a service, two organizations and three project refs.
type env struct {
	svc        *Service
	a, b       OrgRef
	refs       []string
	now        *time.Time
	newUser    func() string
	setCutoff  func(time.Time)
	cleanupOrg func()
}

var userSeq int

func memUser() string {
	userSeq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", userSeq)
}

func newMemEnv(t *testing.T) *env {
	t.Helper()
	m := NewMemory()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	e := &env{a: OrgRef{ID: 1, Slug: "acme"}, b: OrgRef{ID: 2, Slug: "beta"}, refs: []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccc"}, now: &now, newUser: memUser}
	e.svc = &Service{Store: m, Now: func() time.Time { return *e.now }, Orgs: func(context.Context) ([]OrgRef, error) { return []OrgRef{e.a, e.b}, nil }}
	e.setCutoff = func(c time.Time) { m.Cutoff = c }
	return e
}

func (e *env) owner(t *testing.T, org OrgRef) string {
	t.Helper()
	u := e.newUser()
	if err := e.svc.EnsureOwner(context.Background(), org, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *env) member(t *testing.T, org OrgRef, role int) string {
	t.Helper()
	u := e.newUser()
	if err := e.svc.Store.Update(context.Background(), org.ID, func(ops Ops) error {
		return ops.PutMember(context.Background(), Member{OrgID: org.ID, UserID: u, RoleID: role})
	}); err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *env) access(t *testing.T, u string) *Access {
	t.Helper()
	a, err := e.svc.Access(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testService(t *testing.T, mk func(*testing.T) *env) {
	t.Run("last owner", func(t *testing.T) { testLastOwner(t, mk(t)) })
	t.Run("who may change whom", func(t *testing.T) { testWhoMayChangeWhom(t, mk(t)) })
	t.Run("project roles", func(t *testing.T) { testProjectRoles(t, mk(t)) })
	t.Run("invitations", func(t *testing.T) { testInvitations(t, mk(t)) })
	t.Run("sso defaults", func(t *testing.T) { testSSODefaults(t, mk(t)) })
	t.Run("remove user", func(t *testing.T) { testRemoveUser(t, mk(t)) })
	t.Run("concurrent demotions", func(t *testing.T) { testConcurrentOwners(t, mk(t)) })
	t.Run("legacy accounts", func(t *testing.T) { testLegacyAccounts(t, mk) })
	t.Run("scoped owner invitations", func(t *testing.T) { testScopedOwnerInvitations(t, mk(t)) })
	t.Run("stand-in owner", func(t *testing.T) { testStandInOwner(t, mk(t)) })
	t.Run("remove user except", func(t *testing.T) { testRemoveUserExcept(t, mk(t)) })
	t.Run("delete organization", func(t *testing.T) { testDeleteOrganization(t, mk(t)) })
}

func TestServiceMemory(t *testing.T) { testService(t, newMemEnv) }

func testLastOwner(t *testing.T, e *env) {
	ctx := context.Background()
	o1, o2 := e.owner(t, e.a), e.owner(t, e.a)
	op := e.access(t, o1)
	if err := e.svc.SetOrgRole(ctx, op, e.a, o2, RoleDeveloper); err != nil {
		t.Fatalf("demote with another owner left: %v", err)
	}
	for name, fn := range map[string]func() error{
		"demote":      func() error { return e.svc.SetOrgRole(ctx, op, e.a, o1, RoleAdministrator) },
		"remove role": func() error { return e.svc.RemoveRole(ctx, op, e.a, o1, RoleOwner) },
		"leave":       func() error { return e.svc.RemoveMember(ctx, op, e.a, o1) },
		"operator":    func() error { return e.svc.SetOrgRole(ctx, nil, e.a, o1, RoleReadOnly) },
		"remove user": func() error { return e.svc.RemoveUser(ctx, o1, false) },
	} {
		if err := fn(); !errors.Is(err, ErrLastOwner) {
			t.Errorf("%s the last owner: got %v, want ErrLastOwner", name, err)
		}
	}
	var lo *LastOwnerError
	if err := e.svc.RemoveUser(ctx, o1, false); !errors.As(err, &lo) || lo.OrgID != e.a.ID {
		t.Errorf("RemoveUser error: %v", err)
	}
	// Another Owner restores the options.
	if err := e.svc.SetOrgRole(ctx, op, e.a, o2, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetOrgRole(ctx, op, e.a, o1, RoleDeveloper); err != nil {
		t.Fatalf("demote with two owners: %v", err)
	}
	// The organization keeps its one Owner; the other organization is separate.
	if n, _ := e.svc.Store.CountOwners(ctx, e.a.ID); n != 1 {
		t.Fatalf("owners: %d", n)
	}
	// force removes the last owner (operator break-glass).
	if err := e.svc.RemoveUser(ctx, o2, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.Store.CountOwners(ctx, e.a.ID); n != 0 {
		t.Fatalf("owners after force: %d", n)
	}
}

func testWhoMayChangeWhom(t *testing.T, e *env) {
	ctx := context.Background()
	owner := e.owner(t, e.a)
	admin := e.member(t, e.a, RoleAdministrator)
	dev := e.member(t, e.a, RoleDeveloper)
	ro := e.member(t, e.a, RoleReadOnly)
	target := e.member(t, e.a, RoleReadOnly)

	adminA, devA, roA := e.access(t, admin), e.access(t, dev), e.access(t, ro)
	// Developer and Read-only change nothing.
	for _, a := range []*Access{devA, roA} {
		if err := e.svc.SetOrgRole(ctx, a, e.a, target, RoleDeveloper); !errors.Is(err, ErrForbidden) {
			t.Errorf("non-admin changes a role: %v", err)
		}
		if err := e.svc.RemoveMember(ctx, a, e.a, target); !errors.Is(err, ErrForbidden) {
			t.Errorf("non-admin removes a member: %v", err)
		}
		if _, _, err := e.svc.Invite(ctx, a, e.a, InviteInput{Email: "x@example.test", RoleID: RoleReadOnly}); !errors.Is(err, ErrForbidden) {
			t.Errorf("non-admin invites: %v", err)
		}
	}
	// An Administrator manages Administrator, Developer and Read-only, never Owner.
	if err := e.svc.SetOrgRole(ctx, adminA, e.a, target, RoleAdministrator); err != nil {
		t.Fatalf("admin promotes to administrator: %v", err)
	}
	if err := e.svc.SetOrgRole(ctx, adminA, e.a, target, RoleOwner); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin makes an owner: %v", err)
	}
	if err := e.svc.SetOrgRole(ctx, adminA, e.a, admin, RoleOwner); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin promotes itself: %v", err)
	}
	if err := e.svc.SetOrgRole(ctx, adminA, e.a, owner, RoleDeveloper); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin demotes an owner: %v", err)
	}
	if err := e.svc.RemoveMember(ctx, adminA, e.a, owner); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin removes an owner: %v", err)
	}
	if _, _, err := e.svc.Invite(ctx, adminA, e.a, InviteInput{Email: "o@example.test", RoleID: RoleOwner}); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin invites an owner: %v", err)
	}
	if _, _, err := e.svc.Invite(ctx, adminA, e.a, InviteInput{Email: "d@example.test", RoleID: RoleDeveloper}); err != nil {
		t.Errorf("admin invites a developer: %v", err)
	}
	if err := e.svc.RemoveMember(ctx, adminA, e.a, target); err != nil {
		t.Errorf("admin removes a member: %v", err)
	}
	// An Owner manages everyone; a member may leave.
	ownerA := e.access(t, owner)
	if err := e.svc.SetOrgRole(ctx, ownerA, e.a, admin, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RemoveMember(ctx, roA, e.a, ro); err != nil {
		t.Errorf("a member leaves the team: %v", err)
	}
	// Not a member: 404, not a permission error.
	if err := e.svc.SetOrgRole(ctx, ownerA, e.a, e.newUser(), RoleDeveloper); !errors.Is(err, ErrNotFound) {
		t.Errorf("non-member: %v", err)
	}
	if err := e.svc.SetOrgRole(ctx, ownerA, e.a, admin, 9); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown role: %v", err)
	}
}

func testProjectRoles(t *testing.T, e *env) {
	ctx := context.Background()
	owner := e.owner(t, e.a)
	ownerA := e.access(t, owner)
	u := e.member(t, e.a, 0)
	r := e.refs
	if err := e.svc.AssignProjectRole(ctx, ownerA, e.a, u, RoleDeveloper, []string{r[0], r[1]}); err != nil {
		t.Fatal(err)
	}
	// A project holds one role per member: r1 moves to Read-only.
	if err := e.svc.AssignProjectRole(ctx, ownerA, e.a, u, RoleReadOnly, []string{r[1]}); err != nil {
		t.Fatal(err)
	}
	roles, _ := e.svc.Store.ProjectRoles(ctx, e.a.ID)
	byBase := map[int][]string{}
	ids := map[int]int64{}
	for _, x := range roles {
		byBase[x.BaseRoleID] = x.Refs
		ids[x.BaseRoleID] = x.ID
		if x.ID < ProjectRoleIDBase {
			t.Fatalf("project-scoped role id %d overlaps the organization roles", x.ID)
		}
	}
	if fmt.Sprint(byBase[RoleDeveloper]) != fmt.Sprint([]string{r[0]}) || fmt.Sprint(byBase[RoleReadOnly]) != fmt.Sprint([]string{r[1]}) {
		t.Fatalf("roles: %v", byBase)
	}
	// Permissions follow: Developer on r0, Read-only on r1, nothing on r2.
	a := e.access(t, u)
	if !a.Can(e.a, r[0], ActSQLInsert, ResAny, nil) || a.Can(e.a, r[1], ActSQLInsert, ResAny, nil) || a.Can(e.a, r[2], ActRead, ResProjects, nil) {
		t.Fatalf("permissions: %+v", a.Permissions([]OrgRef{e.a}))
	}
	// Members list carries the scoped role ids.
	mv, _ := e.svc.Members(ctx, e.a.ID)
	var got []int64
	for _, m := range mv {
		if m.UserID == u {
			got = m.RoleIDs
		}
	}
	if len(got) != 2 {
		t.Fatalf("role ids of the member: %v", got)
	}
	// Changing the projects of a role; an empty set removes it.
	if err := e.svc.SetProjectRoleRefs(ctx, ownerA, e.a, u, ids[RoleDeveloper], []string{r[0], r[2]}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetProjectRoleRefs(ctx, ownerA, e.a, u, ids[RoleReadOnly], nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RemoveRole(ctx, ownerA, e.a, u, ids[RoleDeveloper]); err != nil {
		t.Fatal(err)
	}
	if roles, _ := e.svc.Store.ProjectRoles(ctx, e.a.ID); len(roles) != 0 {
		t.Fatalf("roles left: %+v", roles)
	}
	// Another member's role is not reachable through this member.
	other := e.member(t, e.a, 0)
	_ = e.svc.AssignProjectRole(ctx, ownerA, e.a, other, RoleDeveloper, []string{r[0]})
	orole, _ := e.svc.Store.ProjectRolesOf(ctx, other)
	if err := e.svc.RemoveRole(ctx, ownerA, e.a, u, orole[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("role of another member: %v", err)
	}
	// An Administrator cannot hand out or edit project-scoped Owner roles.
	admin := e.member(t, e.a, RoleAdministrator)
	adminA := e.access(t, admin)
	if err := e.svc.AssignProjectRole(ctx, adminA, e.a, u, RoleOwner, []string{r[0]}); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin assigns a scoped owner: %v", err)
	}
	if err := e.svc.AssignProjectRole(ctx, ownerA, e.a, u, RoleOwner, []string{r[0]}); err != nil {
		t.Fatal(err)
	}
	adminA = e.access(t, admin) // reload: the Owner-scoped ids are part of the Administrator's entries
	sc, _ := e.svc.Store.ProjectRolesOf(ctx, u)
	if err := e.svc.SetProjectRoleRefs(ctx, adminA, e.a, u, sc[0].ID, []string{r[1]}); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin edits a scoped owner: %v", err)
	}
	if err := e.svc.RemoveMember(ctx, adminA, e.a, u); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin removes a scoped owner: %v", err)
	}
	// Removing the member removes the roles.
	if err := e.svc.RemoveMember(ctx, ownerA, e.a, u); err != nil {
		t.Fatal(err)
	}
	if roles, _ := e.svc.Store.ProjectRolesOf(ctx, u); len(roles) != 0 {
		t.Fatalf("roles after removal: %+v", roles)
	}
	if err := e.svc.AssignProjectRole(ctx, ownerA, e.a, other, RoleDeveloper, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("no projects: %v", err)
	}
}

func testInvitations(t *testing.T, e *env) {
	ctx := context.Background()
	owner := e.owner(t, e.a)
	ownerA := e.access(t, owner)
	inv, token, err := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "New.User@Example.test", RoleID: RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Email != "new.user@example.test" || len(token) < 40 || token[:4] != InvitationPrefix || inv.InvitedBy != owner || !inv.ExpiresAt.Equal(e.now.Add(DefaultInvitationTTL)) {
		t.Fatalf("invitation: %+v %q", inv, token)
	}
	list, _ := e.svc.Invitations(ctx, e.a.ID)
	if len(list) != 1 || list[0].ID != inv.ID {
		t.Fatalf("list: %+v", list)
	}
	// Inviting again replaces the pending invitation: the old token stops working.
	inv2, token2, err := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "new.user@example.test", RoleID: RoleReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := e.svc.InvitationState(ctx, e.a, token, "new.user@example.test"); !st.TokenNotFound {
		t.Errorf("replaced token: %+v", st)
	}
	user := e.newUser()
	if st, _ := e.svc.InvitationState(ctx, e.a, token2, "NEW.user@example.test"); st.TokenNotFound || !st.EmailMatch || st.Expired || st.InviteID != inv2.ID {
		t.Errorf("state: %+v", st)
	}
	if st, _ := e.svc.InvitationState(ctx, e.b, token2, "new.user@example.test"); !st.TokenNotFound {
		t.Errorf("token of another organization: %+v", st)
	}
	if st, _ := e.svc.InvitationState(ctx, e.a, token2, "other@example.test"); st.EmailMatch {
		t.Errorf("email mismatch: %+v", st)
	}
	// Wrong address, wrong organization.
	if err := e.svc.AcceptInvitation(ctx, e.a, token2, user, "other@example.test", nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("wrong address: %v", err)
	}
	if err := e.svc.AcceptInvitation(ctx, e.b, token2, user, "new.user@example.test", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong organization: %v", err)
	}
	if err := e.svc.AcceptInvitation(ctx, e.a, token2, user, "new.user@example.test", nil); err != nil {
		t.Fatal(err)
	}
	if got := e.access(t, user).OrgRole(e.a.ID); got != RoleReadOnly {
		t.Fatalf("role after accepting: %d", got)
	}
	// Single use.
	if err := e.svc.AcceptInvitation(ctx, e.a, token2, e.newUser(), "new.user@example.test", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("second use: %v", err)
	}
	if st, _ := e.svc.InvitationState(ctx, e.a, token2, "new.user@example.test"); !st.Accepted {
		t.Errorf("accepted state: %+v", st)
	}
	if list, _ := e.svc.Invitations(ctx, e.a.ID); len(list) != 0 {
		t.Errorf("accepted invitations are not pending: %+v", list)
	}
	// Expiry.
	_, token3, _ := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "late@example.test", RoleID: RoleDeveloper})
	*e.now = e.now.Add(DefaultInvitationTTL + time.Minute)
	if st, _ := e.svc.InvitationState(ctx, e.a, token3, "late@example.test"); !st.Expired {
		t.Errorf("expired state: %+v", st)
	}
	if err := e.svc.AcceptInvitation(ctx, e.a, token3, e.newUser(), "late@example.test", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired: %v", err)
	}
	// A member who accepts again gets ErrAlreadyMember and the invitation stays pending.
	_, token4, _ := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "again@example.test", RoleID: RoleDeveloper})
	if err := e.svc.AcceptInvitation(ctx, e.a, token4, owner, "again@example.test", nil); !errors.Is(err, ErrAlreadyMember) {
		t.Errorf("already a member: %v", err)
	}
	if st, _ := e.svc.InvitationState(ctx, e.a, token4, "again@example.test"); st.Accepted {
		t.Errorf("a failed acceptance must not consume the invitation: %+v", st)
	}
	// Project-scoped invitation: the member gets only the project role; projects that no longer
	// exist are dropped; none left is refused.
	scoped, token5, err := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "scoped@example.test", RoleID: RoleDeveloper, Refs: []string{e.refs[0], e.refs[1]}})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.ListedRoleID() != InvitationRoleIDBase+scoped.ID {
		t.Errorf("listed role id: %d", scoped.ListedRoleID())
	}
	sr, _ := e.svc.ScopedRoles(ctx, e.a.ID)
	if len(sr) != 1 || sr[0].ID != scoped.ListedRoleID() || sr[0].BaseRoleID != RoleDeveloper {
		t.Errorf("scoped roles list: %+v", sr)
	}
	su := e.newUser()
	if err := e.svc.AcceptInvitation(ctx, e.a, token5, su, "scoped@example.test", func([]string) []string { return []string{e.refs[0]} }); err != nil {
		t.Fatal(err)
	}
	sa := e.access(t, su)
	if sa.OrgRole(e.a.ID) != 0 || !sa.IsMember(e.a.ID) || !sa.Can(e.a, e.refs[0], ActRead, ResProjects, nil) || sa.Can(e.a, e.refs[1], ActRead, ResProjects, nil) {
		t.Errorf("scoped member: %+v", sa.Memberships)
	}
	_, token6, _ := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "gone@example.test", RoleID: RoleReadOnly, Refs: []string{e.refs[2]}})
	if err := e.svc.AcceptInvitation(ctx, e.a, token6, e.newUser(), "gone@example.test", func([]string) []string { return nil }); !errors.Is(err, ErrInvalid) {
		t.Errorf("no live project: %v", err)
	}
	// A claim link accepts the one invitation it was issued for and no other of the address.
	invA, _, _ := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "fresh@example.test", RoleID: RoleAdministrator})
	invB, _, _ := e.svc.Invite(ctx, e.access(t, e.owner(t, e.b)), e.b, InviteInput{Email: "fresh@example.test", RoleID: RoleReadOnly})
	fu := e.newUser()
	if _, err := e.svc.AcceptInvitationByID(ctx, invA.ID, fu, "someone-else@example.test", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another address: %v", err)
	}
	if _, err := e.svc.AcceptInvitationByID(ctx, invA.ID, fu, "Fresh@example.test", nil); err != nil {
		t.Fatalf("accept by id: %v", err)
	}
	fa := e.access(t, fu)
	if fa.OrgRole(e.a.ID) != RoleAdministrator || fa.IsMember(e.b.ID) {
		t.Errorf("roles from one invitation: %+v", fa.Memberships)
	}
	if _, err := e.svc.PendingInvitation(ctx, invB.ID, "fresh@example.test"); err != nil {
		t.Errorf("the other invitation was consumed: %v", err)
	}
	if _, err := e.svc.AcceptInvitationByID(ctx, invA.ID, fu, "fresh@example.test", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("accepted twice: %v", err)
	}
	// Revoking.
	inv7, token7, _ := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "revoked@example.test", RoleID: RoleReadOnly})
	devA := e.access(t, e.member(t, e.a, RoleDeveloper))
	if err := e.svc.RevokeInvitation(ctx, devA, e.a, inv7.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("developer revokes: %v", err)
	}
	if err := e.svc.RevokeInvitation(ctx, ownerA, e.a, inv7.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeInvitation(ctx, ownerA, e.a, inv7.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke twice: %v", err)
	}
	if err := e.svc.AcceptInvitation(ctx, e.a, token7, e.newUser(), "revoked@example.test", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token: %v", err)
	}
	if _, _, err := e.svc.Invite(ctx, ownerA, e.a, InviteInput{Email: "not an address", RoleID: RoleReadOnly}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad address: %v", err)
	}
}

func testSSODefaults(t *testing.T, e *env) {
	ctx := context.Background()
	if g, err := e.svc.GrantSSODefault(ctx, e.newUser(), "x@corp.example.test"); g != nil || err != nil {
		t.Fatalf("no rule: %+v %v", g, err)
	}
	if err := e.svc.SetDomainDefault(ctx, "@Corp.Example.test", e.a.ID, RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetDomainDefault(ctx, "corp.example.test", e.a.ID, 7); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad role: %v", err)
	}
	u := e.newUser()
	g, err := e.svc.GrantSSODefault(ctx, u, "Someone@CORP.example.test")
	if err != nil || g == nil || g.OrgID != e.a.ID || g.RoleID != RoleDeveloper {
		t.Fatalf("grant: %+v %v", g, err)
	}
	if e.access(t, u).OrgRole(e.a.ID) != RoleDeveloper {
		t.Fatal("not a member")
	}
	// An existing membership is never changed.
	_ = e.svc.SetOrgRole(ctx, nil, e.a, u, RoleReadOnly)
	if g, err := e.svc.GrantSSODefault(ctx, u, "someone@corp.example.test"); g != nil || err != nil {
		t.Fatalf("second sign-in: %+v %v", g, err)
	}
	if e.access(t, u).OrgRole(e.a.ID) != RoleReadOnly {
		t.Fatal("the role was reset")
	}
	if list, _ := e.svc.DomainDefaults(ctx); len(list) != 1 || list[0].Domain != "corp.example.test" {
		t.Fatalf("list: %+v", list)
	}
	if err := e.svc.RemoveDomainDefault(ctx, "corp.example.test"); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.svc.GrantSSODefault(ctx, e.newUser(), "y@corp.example.test"); g != nil {
		t.Fatal("removed rule still grants")
	}
}

func testRemoveUser(t *testing.T, e *env) {
	ctx := context.Background()
	o := e.owner(t, e.a)
	u := e.member(t, e.a, RoleDeveloper)
	_ = e.svc.Store.Update(ctx, e.b.ID, func(ops Ops) error {
		return ops.PutMember(ctx, Member{OrgID: e.b.ID, UserID: u, RoleID: 0})
	})
	_ = e.svc.AssignProjectRole(ctx, e.access(t, o), e.a, u, RoleReadOnly, []string{e.refs[0]})
	if err := e.svc.RemoveUser(ctx, u, false); err != nil {
		t.Fatal(err)
	}
	if ms, _ := e.svc.Store.MembershipsOf(ctx, u); len(ms) != 0 {
		t.Fatalf("memberships left: %+v", ms)
	}
	if rs, _ := e.svc.Store.ProjectRolesOf(ctx, u); len(rs) != 0 {
		t.Fatalf("roles left: %+v", rs)
	}
	if err := e.svc.RemoveUser(ctx, e.newUser(), false); err != nil {
		t.Fatalf("a user without memberships: %v", err)
	}
	if a := e.access(t, u); len(a.Memberships) != 0 || a.Can(e.a, "", ActRead, ResOrg, nil) {
		t.Fatalf("access after removal: %+v", a)
	}
}

func testConcurrentOwners(t *testing.T, e *env) {
	ctx := context.Background()
	const n = 6
	owners := make([]string, n)
	for i := range owners {
		owners[i] = e.owner(t, e.a)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every owner is demoted at once, each by the operator.
			errs[i] = e.svc.SetOrgRole(ctx, nil, e.a, owners[i], RoleDeveloper)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrLastOwner):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if owners, _ := e.svc.Store.CountOwners(ctx, e.a.ID); owners != 1 || ok != n-1 {
		t.Fatalf("owners left %d, demotions %d: the last-owner rule raced", owners, ok)
	}
}

func testLegacyAccounts(t *testing.T, mk func(*testing.T) *env) {
	ctx := context.Background()
	e := mk(t)
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e.setCutoff(cutoff)
	created := map[string]time.Time{}
	var failing bool
	calls := 0
	e.svc.AccountCreatedAt = func(_ context.Context, u string) (time.Time, error) {
		calls++
		if failing {
			return time.Time{}, errors.New("sign-in service down")
		}
		return created[u], nil
	}
	// An organization created after the cutoff is not granted to accounts that predate it.
	late := OrgRef{ID: 9, Slug: "late", CreatedAt: cutoff.Add(time.Hour)}
	early := OrgRef{ID: 1, Slug: "acme", CreatedAt: cutoff.Add(-time.Hour)}
	e.svc.Orgs = func(context.Context) ([]OrgRef, error) { return []OrgRef{early, e.b, late}, nil }
	old, young, flaky := e.newUser(), e.newUser(), e.newUser()
	created[old], created[young], created[flaky] = cutoff.Add(-24*time.Hour), cutoff.Add(time.Hour), cutoff.Add(-time.Hour)

	a := e.access(t, old)
	if a.OrgRole(e.a.ID) != RoleOwner || a.OrgRole(e.b.ID) != RoleOwner {
		t.Fatalf("an account from before roles becomes Owner everywhere: %+v", a.Memberships)
	}
	if a.IsMember(late.ID) {
		t.Fatalf("an organization created after roles must not be granted by the legacy rule: %+v", a.Memberships)
	}
	if a := e.access(t, young); len(a.Memberships) != 0 {
		t.Fatalf("an account created after: %+v", a.Memberships)
	}
	// Looked at once: removing the old account's membership sticks, and nobody is asked again.
	before := calls
	if err := e.svc.RemoveUser(ctx, old, true); err != nil {
		t.Fatal(err)
	}
	if a := e.access(t, old); len(a.Memberships) != 0 || calls != before {
		t.Fatalf("an account removed from every organization must stay removed: %+v calls=%d/%d", a.Memberships, calls, before)
	}
	// The sign-in service is down: no access, no marking, a retry after a minute.
	failing = true
	if a := e.access(t, flaky); len(a.Memberships) != 0 {
		t.Fatal("no answer must not grant")
	}
	n := calls
	_ = e.access(t, flaky)
	if calls != n {
		t.Fatal("a failed lookup must back off")
	}
	failing = false
	*e.now = e.now.Add(2 * time.Minute)
	if a := e.access(t, flaky); a.OrgRole(e.a.ID) != RoleOwner {
		t.Fatalf("retry: %+v", a.Memberships)
	}
	// Without a cutoff nobody is legacy.
	e2 := mk(t)
	e2.setCutoff(time.Time{})
	e2.svc.AccountCreatedAt = func(context.Context, string) (time.Time, error) { return time.Time{}, nil }
	if a := e2.access(t, e2.newUser()); len(a.Memberships) != 0 {
		t.Fatal("no cutoff, no legacy accounts")
	}
}

// An Administrator may not revoke a pending project-scoped Owner invitation, and Studio, which
// checks the id the invitation is listed under, agrees with the server.
func testScopedOwnerInvitations(t *testing.T, e *env) {
	ctx := context.Background()
	owner := e.owner(t, e.a)
	admin := e.member(t, e.a, RoleAdministrator)
	inv, _, err := e.svc.Invite(ctx, e.access(t, owner), e.a, InviteInput{Email: "scopedowner@example.test", RoleID: RoleOwner, Refs: []string{e.refs[0]}})
	if err != nil {
		t.Fatal(err)
	}
	a := e.access(t, admin)
	if a.CanRole(e.a, ActDelete, ResUserInvites, inv.ListedRoleID()) {
		t.Error("Studio's check (the listed id) lets an Administrator revoke a scoped Owner invitation")
	}
	if err := e.svc.RevokeInvitation(ctx, a, e.a, inv.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("the server must refuse too: %v", err)
	}
	if err := e.svc.RevokeInvitation(ctx, e.access(t, owner), e.a, inv.ID); err != nil {
		t.Errorf("an Owner revokes it: %v", err)
	}
}

func TestParseRole(t *testing.T) {
	for in, want := range map[string]int{"owner": 1, "Owner": 1, "administrator": 2, "Developer": 3, "read-only": 4, "Read-only": 4, "readonly": 4, "read_only": 4} {
		r, err := ParseRole(in)
		if err != nil || r.ID != want {
			t.Errorf("%q: %+v %v", in, r, err)
		}
	}
	if _, err := ParseRole("root"); err == nil {
		t.Error("unknown role accepted")
	}
	if _, err := NormalizeEmail("a b@example.test"); err == nil {
		t.Error("bad email accepted")
	}
	if DomainOf("A@Example.TEST") != "example.test" || DomainOf("nope") != "" {
		t.Error("DomainOf")
	}
}

// The stand-in of `functions dev` holds Owner seats but is never an Owner for the last-owner rule:
// a seat that a crash left behind cannot stand in for the last real Owner.
func testStandInOwner(t *testing.T, e *env) {
	ctx := context.Background()
	real := e.owner(t, e.a)
	if err := e.svc.EnsureOwner(ctx, e.a, StandInOwnerID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.EnsureOwner(ctx, e.b, StandInOwnerID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.Store.CountOwners(ctx, e.a.ID); err != nil || n != 1 {
		t.Fatalf("owners of a with the stand-in: %d, %v; want 1", n, err)
	}
	// Without a real Owner the organization has none, whoever else holds the seat.
	if n, err := e.svc.Store.CountOwners(ctx, e.b.ID); err != nil || n != 0 {
		t.Fatalf("owners of b with only the stand-in: %d, %v; want 0", n, err)
	}
	if err := e.svc.RemoveUser(ctx, real, false); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the last real Owner next to a stand-in: %v, want ErrLastOwner", err)
	}
	if err := e.svc.SetOrgRole(ctx, nil, e.a, real, RoleDeveloper); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the last real Owner next to a stand-in: %v, want ErrLastOwner", err)
	}
	if err := e.svc.RemoveMember(ctx, nil, e.a, real); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the last real Owner as a member next to a stand-in: %v, want ErrLastOwner", err)
	}
	// The stand-in itself always goes, whatever the count says.
	if err := e.svc.RemoveUser(ctx, StandInOwnerID, false); err != nil {
		t.Fatalf("removing the stand-in: %v", err)
	}
	if ms, _ := e.svc.Store.MembershipsOf(ctx, StandInOwnerID); len(ms) != 0 {
		t.Fatalf("stand-in memberships left: %+v", ms)
	}
	if n, _ := e.svc.Store.CountOwners(ctx, e.a.ID); n != 1 {
		t.Fatalf("the real Owner was counted wrong after the sweep: %d", n)
	}
}

// RemoveUserExcept skips the last-owner rule for the organization that is being deleted, and for
// that one only.
func testRemoveUserExcept(t *testing.T, e *env) {
	ctx := context.Background()
	u := e.owner(t, e.a)
	if err := e.svc.EnsureOwner(ctx, e.b, u); err != nil {
		t.Fatal(err)
	}
	e.owner(t, e.b) // b has a second Owner, a does not
	if err := e.svc.RemoveUser(ctx, u, false); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("the only Owner of a: %v, want ErrLastOwner", err)
	}
	if err := e.svc.RemoveUserExcept(ctx, u, false, e.a.ID); err != nil {
		t.Fatalf("with a being deleted: %v", err)
	}
	if ms, _ := e.svc.Store.MembershipsOf(ctx, u); len(ms) != 0 {
		t.Fatalf("memberships left: %+v", ms)
	}
	// Another organization's rule still holds.
	v := e.owner(t, e.a)
	if err := e.svc.EnsureOwner(ctx, e.b, v); err != nil {
		t.Fatal(err)
	}
	e.svc.Store.Update(ctx, e.b.ID, func(ops Ops) error { // v becomes the only Owner of b
		for _, m := range mustList(t, ops, e.b.ID) {
			if m.UserID != v && m.RoleID == RoleOwner {
				_ = ops.PutMember(ctx, Member{OrgID: e.b.ID, UserID: m.UserID, RoleID: RoleDeveloper})
			}
		}
		return nil
	})
	if err := e.svc.RemoveUserExcept(ctx, v, false, e.a.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("the only Owner of b while a is deleted: %v, want ErrLastOwner", err)
	}
}

func mustList(t *testing.T, ops Ops, org int64) []Member {
	t.Helper()
	ms, err := ops.ListMembers(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// DeleteOrganization removes everything the model keeps for one organization and nothing of the
// other.
func testDeleteOrganization(t *testing.T, e *env) {
	ctx := context.Background()
	o := e.owner(t, e.a)
	dev := e.member(t, e.a, RoleDeveloper)
	scoped := e.member(t, e.a, 0)
	if err := e.svc.AssignProjectRole(ctx, nil, e.a, scoped, RoleDeveloper, []string{e.refs[0]}); err != nil {
		t.Fatal(err)
	}
	inv, _, err := e.svc.Invite(ctx, nil, e.a, InviteInput{Email: "new@example.test", RoleID: RoleReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetMFAEnforced(ctx, e.a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetDomainDefault(ctx, "acme.example.test", e.a.ID, RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	// Organization b keeps all of the same.
	ob := e.owner(t, e.b)
	if err := e.svc.EnsureOwner(ctx, e.b, dev); err != nil {
		t.Fatal(err)
	}
	invB, _, err := e.svc.Invite(ctx, nil, e.b, InviteInput{Email: "new@example.test", RoleID: RoleReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetMFAEnforced(ctx, e.b.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetDomainDefault(ctx, "beta.example.test", e.b.ID, RoleReadOnly); err != nil {
		t.Fatal(err)
	}

	ids, err := e.svc.DeleteOrganization(ctx, e.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != inv.ID {
		t.Fatalf("removed invitations %v, want [%d]", ids, inv.ID)
	}
	if ms, _ := e.svc.Store.ListMembers(ctx, e.a.ID); len(ms) != 0 {
		t.Fatalf("members left: %+v", ms)
	}
	if rs, _ := e.svc.Store.ProjectRoles(ctx, e.a.ID); len(rs) != 0 {
		t.Fatalf("project roles left: %+v", rs)
	}
	if rs, _ := e.svc.Store.ProjectRolesOf(ctx, scoped); len(rs) != 0 {
		t.Fatalf("project roles of the scoped member left: %+v", rs)
	}
	if is, _ := e.svc.Store.ListInvitations(ctx, e.a.ID); len(is) != 0 {
		t.Fatalf("invitations left: %+v", is)
	}
	if on, _ := e.svc.Store.MFAEnforced(ctx, e.a.ID); on {
		t.Fatal("the MFA requirement is left")
	}
	if _, err := e.svc.Store.GetDomainDefault(ctx, "acme.example.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the default-role rule is left: %v", err)
	}
	for _, u := range []string{o, dev, scoped} {
		if ms, _ := e.svc.Store.MembershipsOf(ctx, u); func() bool {
			for _, m := range ms {
				if m.OrgID == e.a.ID {
					return true
				}
			}
			return false
		}() {
			t.Fatalf("user %s is still a member of the deleted organization", u)
		}
	}
	// Organization b is whole.
	if ms, _ := e.svc.Store.ListMembers(ctx, e.b.ID); len(ms) != 2 {
		t.Fatalf("members of b: %+v", ms)
	}
	if is, _ := e.svc.Store.ListInvitations(ctx, e.b.ID); len(is) != 1 || is[0].ID != invB.ID {
		t.Fatalf("invitations of b: %+v", is)
	}
	if on, _ := e.svc.Store.MFAEnforced(ctx, e.b.ID); !on {
		t.Fatal("b lost its MFA requirement")
	}
	if _, err := e.svc.Store.GetDomainDefault(ctx, "beta.example.test"); err != nil {
		t.Fatalf("b lost its default-role rule: %v", err)
	}
	_ = ob
	// Deleting again, or an organization that never existed, is not an error.
	if ids, err := e.svc.DeleteOrganization(ctx, e.a.ID); err != nil || len(ids) != 0 {
		t.Fatalf("second delete: %v, %v", ids, err)
	}
}
