package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// bravo adds a second organization owned by the fixture's Owner: the node's last organization
// is never deleted.
func (rf *rolesFixture) bravo() *registry.Organization {
	rf.t.Helper()
	ctx := context.Background()
	o, err := rf.reg.CreateOrganization(ctx, "bravo", "Bravo")
	if err != nil {
		rf.t.Fatal(err)
	}
	if err := rf.srv.members.EnsureOwner(ctx, members.OrgRef{ID: o.ID, Slug: o.Slug}, rf.ids["owner"]); err != nil {
		rf.t.Fatal(err)
	}
	return o
}

func (m *fakeManager) deletedRefs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.deleted...)
}

// failingDelete is a manager whose Delete fails for the refs in fail.
type failingDelete struct {
	*fakeManager
	mu   sync.Mutex
	fail map[string]error
}

func (m *failingDelete) setFail(ref string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail == nil {
		m.fail = map[string]error{}
	}
	m.fail[ref] = err
}

func (m *failingDelete) Delete(ctx context.Context, ref string) error {
	m.mu.Lock()
	err := m.fail[ref]
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return m.fakeManager.Delete(ctx, ref)
}

// Only an Owner deletes an organization. Everything of the organization goes with it: its
// projects (through the lifecycle manager), its members and roles; the other organization stays.
func TestDeleteOrganizationNeedsAnOwner(t *testing.T) {
	rf := newRolesFixture(t)
	ctx := context.Background()
	bravo := rf.bravo()

	for _, who := range []string{"admin", "dev", "ro", "scoped", "stranger"} {
		rf.status(403, who, "DELETE", orgBase, nil)
	}
	if got := rf.mgr.deletedRefs(); len(got) != 0 {
		t.Fatalf("a refused request deleted projects: %v", got)
	}
	if _, err := rf.reg.GetOrganization(ctx, "default"); err != nil {
		t.Fatal(err)
	}

	rf.status(200, "owner", "DELETE", orgBase, nil)
	if _, err := rf.reg.GetOrganization(ctx, "default"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the organization is still there: %v", err)
	}
	if got := rf.mgr.deletedRefs(); len(got) != 2 {
		t.Fatalf("deleted projects %v, want %s and %s", got, testRef, secondRef)
	}
	for _, ref := range []string{testRef, secondRef} {
		if _, err := rf.reg.GetProject(ctx, ref); !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("project %s: %v", ref, err)
		}
	}
	if _, err := rf.reg.GetProject(ctx, config.SystemRef); err != nil {
		t.Errorf("the system project went: %v", err)
	}
	for role, id := range rf.ids {
		ms, err := rf.srv.members.Store.MembershipsOf(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m.OrgID != bravo.ID {
				t.Errorf("%s still has a membership of organization %d", role, m.OrgID)
			}
		}
	}
	if rs, _ := rf.srv.members.Store.ProjectRolesOf(ctx, rf.ids["scoped"]); len(rs) != 0 {
		t.Errorf("the scoped member's project role is left: %+v", rs)
	}
	// The other organization is whole, and the audit trail says what happened.
	if m, err := rf.srv.members.Store.GetMember(ctx, bravo.ID, rf.ids["owner"]); err != nil || m.RoleID != members.RoleOwner {
		t.Errorf("bravo's Owner: %+v, %v", m, err)
	}
	es, _ := rf.reg.ListEvents(ctx, config.SystemRef, 20)
	found := false
	for _, e := range es {
		found = found || e.Kind == "org.deleted"
	}
	if !found {
		t.Errorf("no org.deleted event: %+v", es)
	}
	// A second request finds nothing.
	rf.status(404, "owner", "DELETE", orgBase, nil)
}

// The node's last organization stays, whatever the caller owns.
func TestDeleteOrganizationKeepsTheLastOne(t *testing.T) {
	rf := newRolesFixture(t)
	rec := rf.status(409, "owner", "DELETE", orgBase, nil)
	if !strings.Contains(rec.Body.String(), "last organization") {
		t.Errorf("the refusal does not say why: %s", rec.Body)
	}
	if got := rf.mgr.deletedRefs(); len(got) != 0 {
		t.Fatalf("a refused deletion removed projects: %v", got)
	}
	if _, err := rf.reg.GetOrganization(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
}

// Branches go before their parents, and a project that cannot be deleted stops the run with the
// organization, its members and the projects not yet reached in place; the next run finishes.
func TestDeleteOrganizationStopsAtAProjectAndFinishesOnRetry(t *testing.T) {
	rf := newRolesFixture(t)
	ctx := context.Background()
	rf.bravo()
	fd := &failingDelete{fakeManager: rf.mgr}
	rf.srv.mgr = fd

	rf.status(201, "owner", "POST", "/v1/projects/"+testRef+"/branches", map[string]any{"branch_name": "feat"})
	rf.waitBranch(t, "feat", "MIGRATIONS_PASSED")
	var branchRef string
	ps, _ := rf.reg.ListProjects(ctx)
	for _, p := range ps {
		if p.Branch != nil {
			branchRef = p.Ref
		}
	}
	if branchRef == "" {
		t.Fatal("no branch was created")
	}

	fd.setFail(secondRef, errors.New("systemctl said no"))
	rec := rf.status(500, "owner", "DELETE", orgBase, nil)
	if !strings.Contains(rec.Body.String(), secondRef) || strings.Contains(rec.Body.String(), "systemctl") {
		t.Errorf("the answer must name the project and keep the detail in the log: %s", rec.Body)
	}
	got := rf.mgr.deletedRefs()
	if len(got) != 2 || got[0] != branchRef || got[1] != testRef {
		t.Fatalf("deleted %v, want the branch %s before its parent %s", got, branchRef, testRef)
	}
	if _, err := rf.reg.GetOrganization(ctx, "default"); err != nil {
		t.Fatalf("the organization went although a project did not: %v", err)
	}
	if m, err := rf.srv.members.Store.GetMember(ctx, rf.org.ID, rf.ids["owner"]); err != nil || m.RoleID != members.RoleOwner {
		t.Fatalf("the Owner lost the seat that a retry needs: %+v, %v", m, err)
	}

	fd.setFail(secondRef, fmt.Errorf("%w: busy", lifecycle.ErrInvalidState))
	rf.status(409, "owner", "DELETE", orgBase, nil)

	fd.setFail(secondRef, nil)
	rf.status(200, "owner", "DELETE", orgBase, nil)
	if _, err := rf.reg.GetOrganization(ctx, "default"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the retry left the organization: %v", err)
	}
	if got := rf.mgr.deletedRefs(); len(got) != 3 {
		t.Fatalf("deleted %v", got)
	}
}

// Everything keyed by the organization goes: SSO providers (from GoTrue too) with their users,
// refusals and the tokens of those users, memberships in other organizations that the provider's
// users held, invitations with their invite tokens, the MFA setting and the default-role rules.
func TestDeleteOrganizationRemovesWhatIsKeyedByIt(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	bravo := f.bravo()
	id := f.addProvider(acmeIdP, "owner", "acme.test")
	f.waitRefreshes(1)

	alice := f.ssoToken(ssoUser1, "alice@acme.test", id)
	if rec := f.doAs(alice, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first sign-in: %d %s", rec.Code, rec.Body)
	}
	if f.roleOf(ssoUser1) != members.RoleOwner {
		t.Fatal("the SSO user did not become Owner")
	}
	bob := f.ssoToken(ssoUser2, "bob@elsewhere.test", id)
	if rec := f.doAs(bob, "GET", "/platform/projects", nil); rec.Code != 403 {
		t.Fatalf("a user of a domain the provider does not vouch for: %d", rec.Code)
	}
	if err := f.srv.sso.Store.DenySSOEmail(ctx, id, "denied@acme.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	// The SSO user also works in bravo.
	if err := f.srv.members.Store.Update(ctx, bravo.ID, func(ops members.Ops) error {
		return ops.PutMember(ctx, members.Member{OrgID: bravo.ID, UserID: ssoUser1, RoleID: members.RoleDeveloper})
	}); err != nil {
		t.Fatal(err)
	}
	tok := secrets.NewPAT()
	if err := f.reg.CreateAccessToken(ctx, &registry.AccessToken{UserID: ssoUser1, Name: "cli", Hash: secrets.HashToken(tok), Prefix: tok[:8]}); err != nil {
		t.Fatal(err)
	}
	// An invitation with its invite token, an MFA requirement, and (from the provider) a
	// default-role rule exist in the organization.
	invite := f.claimLink("owner", "default", "new@example.test", members.RoleDeveloper)
	if err := f.srv.members.SetMFAEnforced(ctx, f.org.ID, true); err != nil {
		t.Fatal(err)
	}

	// The organization requires MFA, so the session that deletes it carries aal2.
	aal2 := f.signJWT(map[string]any{"sub": f.userID, "email": "dev@example.test", "role": "authenticated", "aal": "aal2"})
	if rec := f.doAs(aal2, "DELETE", orgBase, nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	f.waitRefreshes(2)

	if _, err := f.reg.GetOrganization(ctx, "default"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the organization is still there: %v", err)
	}
	if ids := f.gt.sso.ids(); len(ids) != 0 {
		t.Errorf("GoTrue still has providers %v", ids)
	}
	if rows, _ := f.srv.sso.Store.ListProviders(ctx); len(rows) != 0 {
		t.Errorf("provider records left: %+v", rows)
	}
	if us, _ := f.srv.sso.Store.ListSSOUsers(ctx, "", nil); len(us) != 0 {
		t.Errorf("SSO users left (pending ones too): %+v", us)
	}
	if denied, _ := f.srv.sso.Store.SSOEmailDenied(ctx, id, "denied@acme.test"); denied {
		t.Error("a refusal of the removed provider is left")
	}
	if ts, _ := f.reg.ListAccessTokens(ctx, ssoUser1); len(ts) != 0 {
		t.Errorf("the SSO user's personal access tokens are left: %+v", ts)
	}
	if ms, _ := f.srv.members.Store.MembershipsOf(ctx, ssoUser1); len(ms) != 0 {
		t.Errorf("the SSO user keeps seats: %+v", ms)
	}
	if ms, _ := f.srv.members.Store.ListMembers(ctx, f.org.ID); len(ms) != 0 {
		t.Errorf("members of the deleted organization: %+v", ms)
	}
	if is, _ := f.srv.members.Store.ListInvitations(ctx, f.org.ID); len(is) != 0 {
		t.Errorf("invitations left: %+v", is)
	}
	if _, err := f.srv.accounts.Store.LookupClaimToken(ctx, secrets.HashToken(invite), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("the invite token of a deleted invitation is left: %v", err)
	}
	if rec := f.redeem(invite, "new@example.test"); rec.Code < 400 {
		t.Errorf("the invite token still redeems: %d %s", rec.Code, rec.Body)
	}
	if on, _ := f.srv.members.MFAEnforced(ctx, f.org.ID); on {
		t.Error("the MFA requirement is left")
	}
	if _, err := f.srv.members.Store.GetDomainDefault(ctx, "acme.test"); !errors.Is(err, members.ErrNotFound) {
		t.Errorf("the default-role rule is left: %v", err)
	}
	// Bravo is whole except for the provider's user, who cannot sign in any more.
	if m, err := f.srv.members.Store.GetMember(ctx, bravo.ID, f.ids["owner"]); err != nil || m.RoleID != members.RoleOwner {
		t.Errorf("bravo's Owner: %+v, %v", m, err)
	}
}

// The caller is the only Owner of the organization and signs in through its own provider.
// Removing the provider takes the caller's seat, which the last-owner rule would refuse for any
// other removal; it does not hold back the deletion of the organization the seat is in.
func TestDeleteOrganizationIgnoresItsOwnLastOwnerRule(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	f.bravo()
	id := f.addProvider(acmeIdP, "owner", "acme.test")
	alice := f.ssoToken(ssoUser1, "alice@acme.test", id)
	if rec := f.doAs(alice, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first sign-in: %d %s", rec.Code, rec.Body)
	}
	if err := f.srv.members.SetOrgRole(ctx, nil, members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}, f.userID, members.RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.srv.members.Store.CountOwners(ctx, f.org.ID); n != 1 {
		t.Fatalf("owners: %d, want the SSO user alone", n)
	}
	// Removing the provider alone is refused for that reason.
	if rec := f.doAs(alice, "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 409 {
		t.Fatalf("removing the provider of the only Owner: %d %s, want 409", rec.Code, rec.Body)
	}
	if rec := f.doAs(alice, "DELETE", orgBase, nil); rec.Code != 200 {
		t.Fatalf("deleting the organization: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.reg.GetOrganization(ctx, "default"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("the organization is still there: %v", err)
	}
	if ms, _ := f.srv.members.Store.MembershipsOf(ctx, ssoUser1); len(ms) != 0 {
		t.Errorf("the SSO user keeps seats: %+v", ms)
	}
	if rows, _ := f.srv.sso.Store.ListProviders(ctx); len(rows) != 0 {
		t.Errorf("provider records left: %+v", rows)
	}
}

// failingTokenDelete is a registry whose DeleteAccessToken fails while fail is set.
type failingTokenDelete struct {
	registry.Registry
	mu   sync.Mutex
	fail bool
}

func (r *failingTokenDelete) DeleteAccessToken(ctx context.Context, userID string, id int64) error {
	r.mu.Lock()
	fail := r.fail
	r.mu.Unlock()
	if fail {
		return errors.New("registry is read only")
	}
	return r.Registry.DeleteAccessToken(ctx, userID, id)
}

// A provider is removed only once the personal access tokens of its users are revoked: a token
// without a provider record would pass AdmitUser. A failed revocation answers 502 and keeps the
// record, so that a retry finishes the job.
func TestSSORemoveFailsWhenATokenCannotBeRevoked(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first sign-in: %d %s", rec.Code, rec.Body)
	}
	tok := secrets.NewPAT()
	if err := f.reg.CreateAccessToken(ctx, &registry.AccessToken{UserID: ssoUser1, Name: "cli", Hash: secrets.HashToken(tok), Prefix: tok[:8]}); err != nil {
		t.Fatal(err)
	}
	failing := &failingTokenDelete{Registry: f.reg, fail: true}
	f.srv.sso.Reg = failing

	rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil)
	if rec.Code != 502 {
		t.Fatalf("remove with a failing revocation: %d %s, want 502", rec.Code, rec.Body)
	}
	if _, err := f.srv.sso.Store.GetProvider(ctx, id); err != nil {
		t.Fatalf("the provider record went although a token survived: %v", err)
	}
	if u, err := f.srv.sso.Store.GetSSOUser(ctx, ssoUser1); err != nil || u.ProviderID != id {
		t.Fatalf("the SSO user's record went: %+v, %v", u, err)
	}
	if ts, _ := f.reg.ListAccessTokens(ctx, ssoUser1); len(ts) != 1 {
		t.Fatalf("tokens %+v, want the one that could not be revoked", ts)
	}
	// With its record kept, the token still goes through the SSO check (the owner is known).
	if err := f.srv.sso.AdmitUser(ctx, ssoUser1); err != nil {
		t.Fatalf("AdmitUser of a user whose provider is still recorded: %v", err)
	}

	failing.mu.Lock()
	failing.fail = false
	failing.mu.Unlock()
	if rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 200 {
		t.Fatalf("the retry: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.srv.sso.Store.GetProvider(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the record is still there after the retry: %v", err)
	}
	if ts, _ := f.reg.ListAccessTokens(ctx, ssoUser1); len(ts) != 0 {
		t.Errorf("the token survived the retry: %+v", ts)
	}
	if _, err := f.srv.sso.Store.GetSSOUser(ctx, ssoUser1); !errors.Is(err, ErrNotFound) {
		t.Errorf("the SSO user's record is left: %v", err)
	}
}

// The stand-in of `functions dev` that a crash left behind is swept away, with its token.
func TestSweepStandIn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}
	if err := f.srv.members.EnsureOwner(ctx, org, members.StandInOwnerID); err != nil {
		t.Fatal(err)
	}
	tok := secrets.NewPAT()
	if err := f.reg.CreateAccessToken(ctx, &registry.AccessToken{UserID: members.StandInOwnerID, Name: "functions dev", Hash: secrets.HashToken(tok), Prefix: tok[:8]}); err != nil {
		t.Fatal(err)
	}
	// It is not an Owner for the last-owner rule: the real Owner cannot be removed because the
	// stand-in is there.
	if err := f.srv.members.RemoveUser(ctx, f.userID, false); !errors.Is(err, members.ErrLastOwner) {
		t.Fatalf("removing the only real Owner: %v, want ErrLastOwner", err)
	}
	m, n, err := SweepStandIn(ctx, f.reg, f.srv.members)
	if err != nil || m != 1 || n != 1 {
		t.Fatalf("swept %d memberships and %d tokens, %v; want 1 and 1", m, n, err)
	}
	if ms, _ := f.srv.members.Store.MembershipsOf(ctx, members.StandInOwnerID); len(ms) != 0 {
		t.Errorf("stand-in seats left: %+v", ms)
	}
	if ts, _ := f.reg.ListAccessTokens(ctx, members.StandInOwnerID); len(ts) != 0 {
		t.Errorf("stand-in tokens left: %+v", ts)
	}
	if m, n, err := SweepStandIn(ctx, f.reg, f.srv.members); err != nil || m != 0 || n != 0 {
		t.Fatalf("a second sweep: %d, %d, %v", m, n, err)
	}
	if n, _ := f.srv.members.Store.CountOwners(ctx, f.org.ID); n != 1 {
		t.Fatalf("the real Owner: %d", n)
	}
}
