package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/sso"
)

const (
	acmeIdP  = "https://idp.acme.test/saml"
	otherIdP = "https://idp.other.test/saml"
)

// ssoFixture is the roles fixture with a counter of the times Studio's unit was told to refresh.
type ssoFixture struct {
	*rolesFixture
	tt        *testing.T
	refreshes atomic.Int32
}

func newSSOFixture(t *testing.T) *ssoFixture {
	t.Helper()
	f := &ssoFixture{rolesFixture: newRolesFixture(t), tt: t}
	f.srv.studioRefresh = func(context.Context) error { f.refreshes.Add(1); return nil }
	return f
}

func (f *ssoFixture) waitRefreshes(n int32) {
	f.t.Helper()
	for i := 0; i < 200; i++ {
		if f.refreshes.Load() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("Studio was told %d times, want %d", f.refreshes.Load(), n)
}

const orgSSO = "/platform/organizations/default/sso"

// addProvider registers a provider through the API as the Owner and returns its id.
func (f *ssoFixture) addProvider(entity string, role string, domains ...string) string {
	f.t.Helper()
	body := map[string]any{"type": "saml", "metadata_xml": testIdPMetadata(entity), "domains": domains}
	if role != "" {
		body["default_role"] = role
	}
	rec := f.as("owner", "POST", orgSSO+"/providers", body)
	if rec.Code != 201 {
		f.t.Fatalf("add provider: %d %s", rec.Code, rec.Body)
	}
	return jsonField(f.tt, rec, "id").(string)
}

// ssoToken is a dashboard session of a user whose account came from SSO through provider.
func (f *ssoFixture) ssoToken(userID, email, provider string) string {
	return f.signJWT(map[string]any{
		"sub": userID, "email": email, "role": "authenticated",
		"app_metadata":  map[string]any{"provider": "sso:" + provider, "providers": []string{"sso:" + provider}},
		"user_metadata": map[string]any{"full_name": "Sso User"},
	})
}

func (f *ssoFixture) roleOf(userID string) int {
	f.t.Helper()
	m, err := f.srv.members.Store.GetMember(context.Background(), f.org.ID, userID)
	if err != nil {
		return 0
	}
	return m.RoleID
}

const (
	ssoUser1 = "bbbbbbbb-0000-4000-8000-000000000001"
	ssoUser2 = "bbbbbbbb-0000-4000-8000-000000000002"
	ssoUser3 = "bbbbbbbb-0000-4000-8000-000000000003"
)

// An Owner registers a provider through the API: GoTrue has it, supavise records its organization,
// domains and default role, the domains' default-role rules exist, and Studio is told that the
// sign-in page now has an SSO button. Removing it undoes all of it.
func TestSSOProviderLifecycleThroughTheAPI(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "Acme.Test", "contractors.acme.test")
	f.waitRefreshes(1)

	if got := f.gt.sso.ids(); len(got) != 1 || got[0] != id {
		t.Fatalf("GoTrue has providers %v, want [%s]", got, id)
	}
	row, err := f.srv.sso.Store.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.OrgID != f.org.ID || row.DefaultRole != members.RoleDeveloper || strings.Join(row.Domains, ",") != "acme.test,contractors.acme.test" {
		t.Fatalf("recorded %+v", row)
	}
	for _, d := range []string{"acme.test", "contractors.acme.test"} {
		rule, err := f.srv.members.Store.GetDomainDefault(context.Background(), d)
		if err != nil || rule.OrgID != f.org.ID || rule.RoleID != members.RoleDeveloper {
			t.Fatalf("default-role rule of %s: %+v, %v", d, rule, err)
		}
	}

	rec := f.as("owner", "GET", orgSSO+"/providers", nil)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if n := len(jsonField(t, rec, "items").([]any)); n != 1 {
		t.Fatalf("list has %d items", n)
	}
	if got := jsonField(t, rec, "service_provider.acs_url"); got != "https://api.example.test/auth/v1/sso/saml/acs" {
		t.Fatalf("the identity provider is told to post to %v", got)
	}
	rec = f.as("owner", "GET", orgSSO+"/providers/"+id, nil)
	if rec.Code != 200 || jsonField(t, rec, "default_role") != "Developer" || jsonField(t, rec, "organization_slug") != "default" ||
		jsonField(t, rec, "saml.entity_id") != acmeIdP {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}

	// Update: another domain, no default role any more.
	rec = f.as("owner", "PUT", orgSSO+"/providers/"+id, map[string]any{"domains": []string{"acme.test", "new.acme.test"}, "default_role": "none"})
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.srv.members.Store.GetDomainDefault(context.Background(), "acme.test"); err == nil {
		t.Error("the domain still has a default role after the provider's default role was cleared")
	}
	row, _ = f.srv.sso.Store.GetProvider(context.Background(), id)
	if row.DefaultRole != 0 || strings.Join(row.Domains, ",") != "acme.test,new.acme.test" {
		t.Fatalf("recorded after the update %+v", row)
	}

	// Domains belong to one provider.
	rec = f.as("owner", "POST", orgSSO+"/providers", map[string]any{"type": "saml", "metadata_xml": testIdPMetadata(otherIdP), "domains": []string{"ACME.test"}})
	if rec.Code != 409 {
		t.Fatalf("a second provider for a taken domain: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.gt.sso.ids()); n != 1 {
		t.Fatalf("the refused provider reached GoTrue: %v", f.gt.sso.ids())
	}

	rec = f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	f.waitRefreshes(2)
	if n := len(f.gt.sso.ids()); n != 0 {
		t.Fatalf("GoTrue still has %v", f.gt.sso.ids())
	}
	if _, err := f.srv.sso.Store.GetProvider(context.Background(), id); err == nil {
		t.Error("the provider's record is still there")
	}
	if rec = f.as("owner", "GET", orgSSO+"/providers/"+id, nil); rec.Code != 404 {
		t.Fatalf("get after delete: %d", rec.Code)
	}
}

// Input checks happen before anything reaches GoTrue.
func TestSSOProviderInputIsChecked(t *testing.T) {
	f := newSSOFixture(t)
	for name, body := range map[string]map[string]any{
		"no metadata":     {"domains": []string{"acme.test"}},
		"no domain":       {"metadata_xml": testIdPMetadata(acmeIdP)},
		"bad domain":      {"metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"not a domain"}},
		"duplicate":       {"metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"a.test", "A.test"}},
		"bad metadata":    {"metadata_xml": "<html/>", "domains": []string{"a.test"}},
		"an SP, not IdP":  {"metadata_xml": `<EntityDescriptor entityID="x"><SPSSODescriptor/></EntityDescriptor>`, "domains": []string{"a.test"}},
		"unknown role":    {"metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"a.test"}, "default_role": "emperor"},
		"plain http URL":  {"metadata_url": "http://idp.acme.test/metadata.xml", "domains": []string{"a.test"}},
		"url and xml":     {"metadata_url": "https://idp.acme.test/metadata.xml", "metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"a.test"}},
		"name id invalid": {"metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"a.test"}, "name_id_format": "mail"},
	} {
		rec := f.as("owner", "POST", orgSSO+"/providers", body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	if n := len(f.gt.sso.ids()); n != 0 {
		t.Fatalf("GoTrue has %d providers after nothing but refusals", n)
	}
	// GoTrue's own refusal reaches the caller with its message and status.
	f.gt.sso.mu.Lock()
	f.gt.sso.failNext = 500
	f.gt.sso.mu.Unlock()
	rec := f.as("owner", "POST", orgSSO+"/providers", map[string]any{"metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"a.test"}})
	if rec.Code != 502 {
		t.Fatalf("a failing GoTrue: %d %s", rec.Code, rec.Body)
	}
	if rows, _ := f.srv.sso.Store.ListProviders(context.Background()); len(rows) != 0 {
		t.Fatalf("a provider is recorded although GoTrue refused it: %+v", rows)
	}
}

// An Administrator manages providers, but cannot hand out a role they cannot grant: a default
// role is a grant to everyone the identity provider vouches for.
func TestSSOAdministratorsCannotMakeOwners(t *testing.T) {
	f := newSSOFixture(t)
	mk := func(role string) *httptest.ResponseRecorder {
		return f.as("admin", "POST", orgSSO+"/providers", map[string]any{"type": "saml", "metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"acme.test"}, "default_role": role})
	}
	if rec := mk("owner"); rec.Code != 403 {
		t.Fatalf("an Administrator making Owners: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.gt.sso.ids()); n != 0 {
		t.Fatal("the refused provider reached GoTrue")
	}
	rec := mk("developer")
	if rec.Code != 201 {
		t.Fatalf("an Administrator adding a Developer provider: %d %s", rec.Code, rec.Body)
	}
	id := jsonField(t, rec, "id").(string)
	// Raising it to Owner is refused, too.
	if rec := f.as("admin", "PUT", orgSSO+"/providers/"+id, map[string]any{"default_role": "owner"}); rec.Code != 403 {
		t.Fatalf("raising to Owner: %d %s", rec.Code, rec.Body)
	}
	// An Owner makes the provider hand out Owner; from then on an Administrator cannot touch it
	// (adding a domain to it would make that domain's users Owners).
	if rec := f.as("owner", "PUT", orgSSO+"/providers/"+id, map[string]any{"default_role": "owner"}); rec.Code != 200 {
		t.Fatalf("owner: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("admin", "PUT", orgSSO+"/providers/"+id, map[string]any{"domains": []string{"acme.test", "evil.test"}}); rec.Code != 403 {
		t.Fatalf("an Administrator adding a domain to an Owner provider: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("admin", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 403 {
		t.Fatalf("an Administrator removing an Owner provider: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("admin", "POST", orgSSO+"/pending/"+ssoUser1, map[string]any{"role": "owner"}); rec.Code != 403 {
		t.Fatalf("an Administrator approving an Owner: %d %s", rec.Code, rec.Body)
	}
	// Developers, Read-only members and a project-scoped Developer see none of it.
	for _, who := range []string{"dev", "ro", "scoped", "stranger"} {
		for _, path := range []string{orgSSO + "/providers", orgSSO, orgSSO + "/pending"} {
			if rec := f.as(who, "GET", path, nil); rec.Code != 403 {
				t.Errorf("%s GET %s: %d", who, path, rec.Code)
			}
		}
	}
}

// The first request of an SSO user applies the default role of their email domain.
func TestSSOFirstSignInGetsTheDomainsDefaultRole(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	tok := f.ssoToken(ssoUser1, "Alice@Acme.Test", id)

	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body)
	}
	if got := f.roleOf(ssoUser1); got != members.RoleDeveloper {
		t.Fatalf("role after the first sign-in: %d, want Developer", got)
	}
	// They are a Developer in what they can do, too: SQL yes, settings no.
	if rec := f.doAs(tok, "POST", "/v1/projects/"+testRef+"/database/query", map[string]any{"query": "select 1"}); rec.Code == 403 {
		t.Fatalf("a Developer's SQL: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doAs(tok, "PATCH", "/v1/projects/"+testRef+"/config/auth", map[string]any{"site_url": "https://x.test"}); rec.Code != 403 {
		t.Fatalf("a Developer saving settings: %d %s", rec.Code, rec.Body)
	}
	// An Owner who is promoted later stays promoted: the default role is for the first sign-in.
	if err := f.srv.members.SetOrgRole(context.Background(), nil, members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}, ssoUser1, members.RoleAdministrator); err != nil {
		t.Fatal(err)
	}
	f.srv.sso.forget(id)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("second request: %d", rec.Code)
	}
	if got := f.roleOf(ssoUser1); got != members.RoleAdministrator {
		t.Fatalf("role after the second request: %d", got)
	}
	// The user is recorded for the dashboard like any other.
	if u, err := f.srv.store.GetUser(context.Background(), ssoUser1); err != nil || u.Email != "Alice@Acme.Test" {
		t.Fatalf("api user: %+v, %v", u, err)
	}
}

// Anyone else is refused on every route, listed for an administrator, and let in when approved.
func TestSSOUserOfAnUnmappedDomainWaitsForApproval(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	tok := f.ssoToken(ssoUser2, "mallory@contractor.test", id)

	for _, path := range []string{"/platform/projects", "/v1/projects", "/platform/profile", "/platform/profile/permissions", "/platform/organizations", "/v1/organizations"} {
		rec := f.doAs(tok, "GET", path, nil)
		if rec.Code != 403 {
			t.Fatalf("GET %s: %d %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if rec := f.doAs(tok, "POST", "/platform/profile/access-tokens", map[string]any{"name": "t"}); rec.Code != 403 {
		t.Fatalf("a pending user minting a token: %d", rec.Code)
	}
	if got := f.roleOf(ssoUser2); got != 0 {
		t.Fatalf("a user of an unmapped domain became %d", got)
	}

	rec := f.as("owner", "GET", orgSSO+"/pending", nil)
	items := jsonField(t, rec, "items").([]any)
	if rec.Code != 200 || len(items) != 1 || items[0].(map[string]any)["email"] != "mallory@contractor.test" {
		t.Fatalf("pending: %d %s", rec.Code, rec.Body)
	}
	// Approve as Read-only.
	if rec := f.as("owner", "POST", orgSSO+"/pending/"+ssoUser2, map[string]any{"role": "read-only"}); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("after the approval: %d %s", rec.Code, rec.Body)
	}
	if got := f.roleOf(ssoUser2); got != members.RoleReadOnly {
		t.Fatalf("approved role %d", got)
	}
	if rec := f.doAs(tok, "POST", "/v1/projects/"+testRef+"/secrets", []map[string]any{{"name": "A", "value": "b"}}); rec.Code != 403 {
		t.Fatalf("a Read-only member writing secrets: %d", rec.Code)
	}
	rec = f.as("owner", "GET", orgSSO+"/pending", nil)
	if n := len(jsonField(t, rec, "items").([]any)); n != 0 {
		t.Fatalf("the approved user is still pending: %s", rec.Body)
	}
	// Approving someone who is not waiting, or twice, is a 404.
	if rec := f.as("owner", "POST", orgSSO+"/pending/"+ssoUser2, map[string]any{"role": "developer"}); rec.Code != 404 {
		t.Fatalf("approving an approved user: %d", rec.Code)
	}
}

// Denying removes the account and ends the session.
func TestSSODenyingAPendingUser(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "", "acme.test") // no default role: everybody waits
	tok := f.ssoToken(ssoUser3, "carol@acme.test", id)
	f.gt.addUser(ssoUser3, "carol@acme.test", time.Now())
	if rec := f.doAs(tok, "GET", "/platform/profile", nil); rec.Code != 403 {
		t.Fatalf("pending: %d", rec.Code)
	}
	if rec := f.as("owner", "DELETE", orgSSO+"/pending/"+ssoUser3, nil); rec.Code != 200 {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body)
	}
	// 401: the account is gone, whatever the token says.
	if rec := f.doAs(tok, "GET", "/platform/profile", nil); rec.Code != 401 {
		t.Fatalf("after the denial: %d, want 401", rec.Code)
	}
	f.gt.mu.Lock()
	_, still := f.gt.users[ssoUser3]
	f.gt.mu.Unlock()
	if still {
		t.Error("the GoTrue account of the denied user is still there")
	}
	rec := f.as("owner", "GET", orgSSO+"/pending", nil)
	if n := len(jsonField(t, rec, "items").([]any)); n != 0 {
		t.Fatalf("pending after the denial: %s", rec.Body)
	}
}

// An SSO session of a provider that Supavise did not register is refused, whatever its claims say.
func TestSSOSessionOfAnUnregisteredProviderIsRefused(t *testing.T) {
	f := newSSOFixture(t)
	f.addProvider(acmeIdP, "developer", "acme.test")
	stray := f.gt.sso.add("https://idp.stray.test/saml", "stray.test") // in GoTrue, never registered with Supavise
	for _, id := range []string{stray, "a0000000-0000-4000-8000-0000000000ff"} {
		tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
		if rec := f.doAs(tok, "GET", "/platform/profile", nil); rec.Code != 403 {
			t.Errorf("provider %s: %d %s", id, rec.Code, rec.Body)
		}
	}
	if got := f.roleOf(ssoUser1); got != 0 {
		t.Fatalf("a session of an unregistered provider made the user %d", got)
	}
	// The unregistered provider shows in the listing, flagged, so that an operator can remove it.
	rec := f.as("owner", "GET", orgSSO+"/providers", nil)
	if n := len(jsonField(t, rec, "items").([]any)); n != 1 {
		t.Fatalf("the organization's list has %d providers, want only its own", n)
	}
	all, err := f.srv.sso.List(context.Background(), 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("the operator's list: %v, %v", all, err)
	}
	var unreg int
	for _, p := range all {
		if !p.Registered {
			unreg++
		}
	}
	if unreg != 1 {
		t.Fatalf("%d unregistered providers listed, want 1", unreg)
	}
	// The operator removes it; supavise never recorded it.
	if _, err := f.srv.sso.Remove(context.Background(), nil, stray, 0); err != nil {
		t.Fatal(err)
	}
	if n := len(f.gt.sso.ids()); n != 1 {
		t.Fatalf("GoTrue has %v", f.gt.sso.ids())
	}
}

// A provider vouches for its own domains only: a second provider cannot give its users the
// first one's default role by asserting the first one's addresses.
func TestSSOProviderCannotClaimAnotherProvidersDomain(t *testing.T) {
	f := newSSOFixture(t)
	f.addProvider(acmeIdP, "administrator", "acme.test")
	other := f.addProvider(otherIdP, "read-only", "other.test")
	tok := f.ssoToken(ssoUser2, "boss@acme.test", other)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 403 {
		t.Fatalf("the other provider's user asserting an acme.test address: %d %s", rec.Code, rec.Body)
	}
	if got := f.roleOf(ssoUser2); got != 0 {
		t.Fatalf("got the role %d", got)
	}
	// Its own domain works.
	tok = f.ssoToken(ssoUser3, "x@other.test", other)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("its own domain: %d", rec.Code)
	}
	if got := f.roleOf(ssoUser3); got != members.RoleReadOnly {
		t.Fatalf("role %d", got)
	}
}

// The default role is for the first sign-in only. A user who lost every membership waits for
// approval again.
func TestSSOUserWhoLosesEveryMembershipWaitsAgain(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	org := members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}
	if err := f.srv.members.RemoveMember(context.Background(), nil, org, ssoUser1); err != nil {
		t.Fatal(err)
	}
	f.srv.sso.forget(id)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 403 {
		t.Fatalf("after the removal: %d, want 403", rec.Code)
	}
	if got := f.roleOf(ssoUser1); got != 0 {
		t.Fatalf("the default role was granted again: %d", got)
	}
	u, err := f.srv.sso.Store.GetSSOUser(context.Background(), ssoUser1)
	if err != nil || u.State != SSOPending {
		t.Fatalf("state %+v, %v", u, err)
	}
}

// Someone who is made a member by other means (invited, `supavise users role`) is let in at once.
func TestSSOUserWhoIsInvitedIsLetIn(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "", "acme.test")
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 403 {
		t.Fatalf("before: %d", rec.Code)
	}
	f.addMember(ssoUser1, members.RoleReadOnly)
	f.srv.sso.forget(id)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("after being made a member: %d %s", rec.Code, rec.Body)
	}
	if u, _ := f.srv.sso.Store.GetSSOUser(context.Background(), ssoUser1); u == nil || u.State != SSOActive {
		t.Fatalf("state %+v", u)
	}
}

// Removing a provider ends the sessions of its users and revokes their personal access tokens.
func TestSSORemovingAProviderEndsItsUsersAccess(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	rec := f.doAs(tok, "POST", "/platform/profile/access-tokens", map[string]any{"name": "ci"})
	if rec.Code != 201 && rec.Code != 200 {
		t.Fatalf("minting a token: %d %s", rec.Code, rec.Body)
	}
	pat := jsonField(t, rec, "token").(string)
	if r := f.doAs(pat, "GET", "/v1/projects", nil); r.Code != 200 {
		t.Fatalf("the token: %d", r.Code)
	}
	if r := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); r.Code != 200 {
		t.Fatalf("remove: %d %s", r.Code, r.Body)
	}
	if r := f.doAs(tok, "GET", "/platform/projects", nil); r.Code != 403 {
		t.Fatalf("the session after the removal: %d", r.Code)
	}
	if r := f.doAs(pat, "GET", "/v1/projects", nil); r.Code != 401 {
		t.Fatalf("the token after the removal: %d, want 401", r.Code)
	}
}

// ssoProviderOf recognizes an account that came from SSO only.
func TestSSOProviderOfClaims(t *testing.T) {
	id := "a0000000-0000-4000-8000-000000000001"
	for name, tc := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"sso":                   {map[string]any{"app_metadata": map[string]any{"provider": "sso:" + id, "providers": []any{"sso:" + id}}}, id},
		"upper case":            {map[string]any{"app_metadata": map[string]any{"provider": "sso:" + strings.ToUpper(id)}}, id},
		"email":                 {map[string]any{"app_metadata": map[string]any{"provider": "email", "providers": []any{"email"}}}, ""},
		"linked to email":       {map[string]any{"app_metadata": map[string]any{"provider": "sso:" + id, "providers": []any{"sso:" + id, "email"}}}, ""},
		"no app_metadata":       {map[string]any{}, ""},
		"empty id":              {map[string]any{"app_metadata": map[string]any{"provider": "sso:"}}, ""},
		"only the user's claim": {map[string]any{"user_metadata": map[string]any{"provider": "sso:" + id}}, ""},
	} {
		if got := ssoProviderOf(tc.claims); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// A session whose claims say "sso" but whose account is a password account gets nothing from
// the claim: users edit user_metadata, not app_metadata, and an supavise_admin user who is not an
// SSO user is admitted the old way.
func TestSSOClaimsInUserMetadataDoNotAdmit(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "owner", "acme.test")
	tok := f.signJWT(map[string]any{"sub": ssoUser1, "email": "x@acme.test", "role": "authenticated",
		"app_metadata":  map[string]any{"provider": "email"},
		"user_metadata": map[string]any{"provider": "sso:" + id}})
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 403 {
		t.Fatalf("a password account claiming SSO in user_metadata: %d", rec.Code)
	}
}

// ---- the sign-up hook --------------------------------------------------------

func (f *ssoFixture) hook(user map[string]any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "before-user-created"}, "user": user})
	secret, err := sso.HookSecret(f.srv.sec)
	if err != nil {
		f.t.Fatal(err)
	}
	h, err := sso.SignWebhook(secret, "msg_1", time.Now(), body)
	if err != nil {
		f.t.Fatal(err)
	}
	req := httptest.NewRequest("POST", sso.HookPath, strings.NewReader(string(body)))
	req.RemoteAddr = "127.0.0.1:51234"
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func refusal(t testing.TB, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("hook answered %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("hook content type %q", ct)
	}
	var out struct {
		Error *struct {
			Code    int    `json:"http_code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil {
		return ""
	}
	if out.Error.Code != 403 {
		t.Fatalf("refusal code %d", out.Error.Code)
	}
	return out.Error.Message
}

func TestBeforeUserCreatedHook(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	user := func(provider, email string, meta map[string]any) map[string]any {
		return map[string]any{"email": email, "app_metadata": map[string]any{"provider": provider, "providers": []string{provider}}, "user_metadata": meta}
	}
	// A user of a registered provider.
	if msg := refusal(t, f.hook(user("sso:"+id, "a@acme.test", nil), nil)); msg != "" {
		t.Fatalf("a registered provider is refused: %s", msg)
	}
	// An unregistered one, an e-mail sign-up, and an e-mail sign-up pretending to be SSO in its own metadata.
	for name, u := range map[string]map[string]any{
		"unregistered provider": user("sso:a0000000-0000-4000-8000-0000000000ff", "a@acme.test", nil),
		"email sign-up":         user("email", "a@acme.test", nil),
		"email, fake grant":     user("email", "a@acme.test", map[string]any{sso.GrantKey: "sbg_made_up"}),
		"email, fake sso claim": user("email", "a@acme.test", map[string]any{"provider": "sso:" + id}),
		"phone":                 user("phone", "", nil),
		"anonymous":             user("anonymous", "", nil),
	} {
		if msg := refusal(t, f.hook(u, nil)); msg == "" {
			t.Errorf("%s was allowed", name)
		}
	}
	// A removed provider no longer creates users.
	if rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if msg := refusal(t, f.hook(user("sso:"+id, "a@acme.test", nil), nil)); msg == "" {
		t.Fatal("a removed provider still creates users")
	}
}

// The hook answers GoTrue on the loopback interface, signed, and nobody else.
func TestBeforeUserCreatedHookRefusesStrangers(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	u := map[string]any{"email": "a@acme.test", "app_metadata": map[string]any{"provider": "sso:" + id}}
	for name, tc := range map[string]struct {
		mutate func(*http.Request)
		want   int
	}{
		"through the edge proxy": {func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.9") }, 404},
		"from another host":      {func(r *http.Request) { r.RemoteAddr = "203.0.113.9:4444" }, 404},
		"no signature":           {func(r *http.Request) { r.Header.Del("webhook-signature") }, 401},
		"wrong signature":        {func(r *http.Request) { r.Header.Set("webhook-signature", "v1,AAAA") }, 401},
		"old timestamp":          {func(r *http.Request) { r.Header.Set("webhook-timestamp", "1000") }, 401},
	} {
		if rec := f.hook(u, tc.mutate); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
	// The same body with a different payload than the one that was signed.
	body := `{"user":{"email":"a@acme.test","app_metadata":{"provider":"sso:` + id + `"}}}`
	secret, _ := sso.HookSecret(f.srv.sec)
	h, _ := sso.SignWebhook(secret, "m", time.Now(), []byte(body))
	req := httptest.NewRequest("POST", sso.HookPath, strings.NewReader(body+" "))
	req.RemoteAddr = "127.0.0.1:1"
	req.Header = h
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("a tampered body: %d", rec.Code)
	}
}

// An invitation by mail makes GoTrue create the user, which the hook allows with the one-time
// grant the daemon put in the invite: once, and for that address only.
func TestInvitationMailCarriesAOneTimeSignupGrant(t *testing.T) {
	f := newSSOFixture(t)
	f.cfg.Mail = config.Mail{SMTPHost: "smtp.example.test", SMTPPort: 587, SMTPFrom: "supavise@example.test"}
	if rec := f.as("owner", "POST", "/platform/organizations/default/members/invitations", map[string]any{"emails": []string{"New@Example.Test"}, "role_id": members.RoleDeveloper}); rec.Code != 201 {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	f.gt.mu.Lock()
	invites := append([]map[string]any(nil), f.gt.invites...)
	f.gt.mu.Unlock()
	if len(invites) != 1 {
		t.Fatalf("GoTrue got %d invites", len(invites))
	}
	data, _ := invites[0]["data"].(map[string]any)
	grant, _ := data[sso.GrantKey].(string)
	if !strings.HasPrefix(grant, "sbg_") {
		t.Fatalf("the invite carries %v", invites[0])
	}
	meta := map[string]any{sso.GrantKey: grant}
	email := func(e string, m map[string]any) map[string]any {
		return map[string]any{"email": e, "app_metadata": map[string]any{"provider": "email"}, "user_metadata": m}
	}
	if msg := refusal(t, f.hook(email("someone-else@example.test", meta), nil)); msg == "" {
		t.Fatal("a grant issued for one address created a user for another")
	}
	if msg := refusal(t, f.hook(email("new@example.test", meta), nil)); msg != "" {
		t.Fatalf("the invited address was refused: %s", msg)
	}
	if msg := refusal(t, f.hook(email("new@example.test", meta), nil)); msg == "" {
		t.Fatal("a grant was used twice")
	}
}

// ---- Studio's organization page ---------------------------------------------

func TestOrgSSOForStudio(t *testing.T) {
	f := newSSOFixture(t)
	// Not set up: the 404 whose message Studio reads as "no provider".
	rec := f.as("owner", "GET", orgSSO, nil)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Failed to find an existing SSO Provider") {
		t.Fatalf("not set up: %d %s", rec.Code, rec.Body)
	}
	f.run(t, []step{
		{key: "POST /platform/organizations/{slug}/sso", path: orgSSO, status: 201, body: map[string]any{
			"enabled": true, "metadata_xml_file": testIdPMetadata(acmeIdP), "domains": []string{"acme.test"},
			"email_mapping": []string{"mail"}, "user_name_mapping": []string{}, "first_name_mapping": []string{"givenName"}, "last_name_mapping": []string{},
			"join_org_on_signup_enabled": true, "join_org_on_signup_role": "Developer",
		}, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "join_org_on_signup_role") != "Developer" || jsonField(t, rec, "domains.0") != "acme.test" ||
				jsonField(t, rec, "email_mapping.0") != "mail" || jsonField(t, rec, "first_name_mapping.0") != "givenName" {
				t.Fatalf("created: %s", rec.Body)
			}
		}},
		{key: "GET /platform/organizations/{slug}/sso", path: orgSSO, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "enabled") != true || jsonField(t, rec, "metadata_xml_file") != testIdPMetadata(acmeIdP) {
				t.Fatalf("get: %s", rec.Body)
			}
		}},
	})
	// Studio's form posts what it was given back, with changes: here the role and a domain.
	f.run(t, []step{
		{key: "PUT /platform/organizations/{slug}/sso", path: orgSSO, body: map[string]any{
			"enabled": true, "metadata_xml_file": testIdPMetadata(acmeIdP), "domains": []string{"acme.test", "labs.acme.test"},
			"email_mapping": []string{"mail"}, "join_org_on_signup_enabled": true, "join_org_on_signup_role": "Read-only",
		}, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			if jsonField(t, rec, "join_org_on_signup_role") != "Read-only" || jsonField(t, rec, "domains.1") != "labs.acme.test" {
				t.Fatalf("updated: %s", rec.Body)
			}
		}},
	})
	// A second provider cannot be created through the single-provider route.
	if rec := f.as("owner", "POST", orgSSO, map[string]any{"metadata_xml_file": testIdPMetadata(otherIdP), "domains": []string{"other.test"}, "email_mapping": []string{"email"}, "enabled": true, "join_org_on_signup_enabled": false}); rec.Code != 409 {
		t.Fatalf("second: %d %s", rec.Code, rec.Body)
	}
	// Disabling it: GoTrue refuses sign-ins through it; the page shows it as off.
	rec = f.as("owner", "PUT", orgSSO, map[string]any{"enabled": false, "metadata_xml_file": testIdPMetadata(acmeIdP), "domains": []string{"acme.test"}, "email_mapping": []string{"mail"}, "join_org_on_signup_enabled": false, "join_org_on_signup_role": "None"})
	if rec.Code != 200 || jsonField(t, rec, "enabled") != false || jsonField(t, rec, "join_org_on_signup_enabled") != false {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	f.run(t, []step{{key: "DELETE /platform/organizations/{slug}/sso", path: orgSSO}})
	if rec := f.as("owner", "GET", orgSSO, nil); rec.Code != 404 {
		t.Fatalf("after delete: %d", rec.Code)
	}
	if n := len(f.gt.sso.ids()); n != 0 {
		t.Fatalf("GoTrue still has %v", f.gt.sso.ids())
	}
}

// ---- a project's own providers -----------------------------------------------

func TestProjectSSOProviders(t *testing.T) {
	f := newSSOFixture(t)
	f.pgt = newFakeGoTrue(t)
	const base = "/v1/projects/" + testRef + "/config/auth/sso/providers"
	// Hosted answers 404 until SAML is enabled for the project.
	for _, c := range []struct{ m, p string }{{"GET", base}, {"POST", base}, {"GET", base + "/a0000000-0000-4000-8000-000000000001"}, {"PUT", base + "/a0000000-0000-4000-8000-000000000001"}, {"DELETE", base + "/a0000000-0000-4000-8000-000000000001"}} {
		rec := f.as("owner", c.m, c.p, map[string]any{"type": "saml", "metadata_url": "https://idp.acme.test/metadata.xml"})
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "SAML 2.0 support is not enabled") {
			t.Fatalf("%s %s with SAML off: %d %s", c.m, c.p, rec.Code, rec.Body)
		}
	}
	if rec := f.as("owner", "PATCH", "/v1/projects/"+testRef+"/config/auth", map[string]any{"saml_enabled": true}); rec.Code != 200 {
		t.Fatalf("enabling SAML: %d %s", rec.Code, rec.Body)
	}
	var id string
	f.run(t, []step{
		{key: "POST /v1/projects/{ref}/config/auth/sso/providers", body: map[string]any{
			"type": "saml", "metadata_url": "https://idp.acme.test/metadata.xml", "domains": []string{"acme.test"},
			"attribute_mapping": map[string]any{"keys": map[string]any{"email": map[string]any{"name": "mail"}}},
			"name_id_format":    "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
		}, check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			id = jsonField(t, rec, "id").(string)
			if jsonField(t, rec, "saml.entity_id") != "https://idp.acme.test" || jsonField(t, rec, "domains.0.domain") != "acme.test" ||
				jsonField(t, rec, "saml.attribute_mapping.keys.email.name") != "mail" {
				t.Fatalf("created: %s", rec.Body)
			}
			if v := jsonField(t, rec, ""); v.(map[string]any)["disabled"] != nil {
				t.Fatal("GoTrue's own fields leak into the Management API's object")
			}
		}},
	})
	if rec := f.as("owner", "GET", "/v1/projects/"+testRef+"/config/auth/sso/providers", nil); rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	p := base + "/" + id
	f.run(t, []step{
		{key: "GET /v1/projects/{ref}/config/auth/sso/providers", check: func(t *testing.T, rec *httptest.ResponseRecorder) {
			// The list carries the metadata document, which GoTrue's own list leaves out.
			if jsonField(t, rec, "items.0.saml.metadata_xml") == nil {
				t.Fatalf("list: %s", rec.Body)
			}
		}},
		{key: "GET /v1/projects/{ref}/config/auth/sso/providers/{provider_id}", path: p, check: want("domains.0.domain", "acme.test")},
		{key: "PUT /v1/projects/{ref}/config/auth/sso/providers/{provider_id}", path: p, body: map[string]any{"domains": []string{"acme.test", "labs.acme.test"}}, check: want("domains.1.domain", "labs.acme.test")},
		{key: "DELETE /v1/projects/{ref}/config/auth/sso/providers/{provider_id}", path: p, check: want("id", id)},
	})
	if rec := f.as("owner", "GET", p, nil); rec.Code != 404 {
		t.Fatalf("after delete: %d", rec.Code)
	}
	// Errors of the project's GoTrue reach the caller as its own status and message.
	rec := f.as("owner", "POST", base, map[string]any{"type": "saml", "metadata_url": "http://idp.acme.test/metadata.xml"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "HTTPS") {
		t.Fatalf("plain http: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("owner", "POST", base, map[string]any{"type": "oidc", "metadata_url": "https://x.test/m.xml"}); rec.Code != 400 {
		t.Fatalf("a type that is not saml: %d", rec.Code)
	}
	if rec := f.as("owner", "GET", base+"/not-a-uuid", nil); rec.Code != 404 {
		t.Fatalf("a malformed id: %d", rec.Code)
	}
	// Writes need the right to change Auth settings; reading needs only to see the project.
	for who, want := range map[string]int{"admin": 201, "dev": 403, "ro": 403, "scoped": 403} {
		rec := f.as(who, "POST", base, map[string]any{"type": "saml", "metadata_url": "https://idp-" + who + ".test/metadata.xml"})
		if rec.Code != want {
			t.Errorf("%s POST: %d %s, want %d", who, rec.Code, rec.Body, want)
		}
	}
	for _, who := range []string{"dev", "ro", "scoped"} {
		if rec := f.as(who, "GET", base, nil); rec.Code != 200 {
			t.Errorf("%s GET: %d", who, rec.Code)
		}
	}
	// A paused project says so.
	if err := f.reg.SetProjectStatus(context.Background(), testRef, registry.StatusInactive); err != nil {
		t.Fatal(err)
	}
	if rec := f.as("owner", "GET", base, nil); rec.Code != 409 {
		t.Fatalf("paused: %d %s", rec.Code, rec.Body)
	}
}

// ---- stores ------------------------------------------------------------------

func TestMemorySSOStore(t *testing.T) { testSSOStore(t, NewMemorySSOStore(), 1) }

// TestPGSSOStore runs the same checks against a real database (CI provides one).
func TestPGSSOStore(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := registry.Migrate(ctx, r.Pool()); err != nil {
		t.Fatal(err)
	}
	org, err := r.CreateOrganization(ctx, fmt.Sprintf("ssostore-%d", time.Now().UnixNano()), "SSO store")
	if err != nil {
		t.Fatal(err)
	}
	testSSOStore(t, NewPGSSOStore(r.Pool()), org.ID)
	// Deleting the organization takes its providers and their users along.
	if _, err := r.Pool().Exec(ctx, `delete from supavise.organizations where id = $1`, org.ID); err != nil {
		t.Fatal(err)
	}
}

// testSSOStore is the conformance suite of SSOStore: the memory store, and Postgres where a
// database is given. org is an organization the providers may belong to.
func testSSOStore(t *testing.T, s SSOStore, org int64) {
	ctx := context.Background()
	const a, b = "a0000000-0000-4000-8000-000000000001", "a0000000-0000-4000-8000-000000000002"
	const u1, u2, u3 = "bbbbbbbb-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002", "bbbbbbbb-0000-4000-8000-000000000003"
	if _, err := s.GetProvider(ctx, a); err != ErrNotFound {
		t.Fatalf("get missing: %v", err)
	}
	if _, err := s.GetProvider(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("get malformed: %v", err)
	}
	for _, p := range []SSOProviderRow{
		{ID: a, OrgID: org, EntityID: "ea", Domains: []string{"a.test", "a2.test"}, DefaultRole: members.RoleDeveloper, CreatedBy: u1},
		{ID: b, OrgID: org, EntityID: "eb", Domains: nil},
	} {
		if err := s.PutProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetProvider(ctx, a)
	if err != nil || got.OrgID != org || got.EntityID != "ea" || got.DefaultRole != members.RoleDeveloper || len(got.Domains) != 2 || got.CreatedBy != u1 || got.CreatedAt.IsZero() {
		t.Fatalf("get: %+v, %v", got, err)
	}
	// An update keeps who created it and when.
	if err := s.PutProvider(ctx, SSOProviderRow{ID: a, OrgID: org, EntityID: "ea", Domains: []string{"a.test"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetProvider(ctx, a); got.DefaultRole != 0 || len(got.Domains) != 1 || got.CreatedBy != u1 {
		t.Fatalf("after update: %+v", got)
	}
	if all, err := s.ListProviders(ctx); err != nil || len(all) < 2 {
		t.Fatalf("list: %v, %v", all, err)
	}
	if got, _ = s.GetProvider(ctx, b); len(got.Domains) != 0 {
		t.Fatalf("no domains: %+v", got)
	}

	now := time.Now().UTC().Truncate(time.Second)
	for i, u := range []SSOUser{{UserID: u1, ProviderID: a, Email: "1@a.test", State: SSOPending, FirstSeen: now}, {UserID: u2, ProviderID: a, Email: "2@a.test", State: SSOActive, FirstSeen: now.Add(time.Second)}, {UserID: u3, ProviderID: b, Email: "3@b.test", State: SSOPending, FirstSeen: now.Add(2 * time.Second)}} {
		ok, err := s.InsertSSOUser(ctx, u)
		if err != nil || !ok {
			t.Fatalf("insert %d: %v, %v", i, ok, err)
		}
	}
	if ok, err := s.InsertSSOUser(ctx, SSOUser{UserID: u1, ProviderID: b, Email: "x", State: SSOActive, FirstSeen: now}); err != nil || ok {
		t.Fatalf("a known user is inserted again: %v, %v", ok, err)
	}
	if u, err := s.GetSSOUser(ctx, u1); err != nil || u.ProviderID != a || u.State != SSOPending {
		t.Fatalf("the second insert changed the user: %+v, %v", u, err)
	}
	list := func(state string, providers []string) []string {
		us, err := s.ListSSOUsers(ctx, state, providers)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range us {
			out = append(out, u.UserID[len(u.UserID)-1:])
		}
		return out
	}
	if got := strings.Join(list(SSOPending, nil), ","); got != "1,3" {
		t.Fatalf("pending %s", got)
	}
	if got := strings.Join(list("", []string{a}), ","); got != "1,2" {
		t.Fatalf("of a: %s", got)
	}
	if got := list(SSOPending, []string{}); len(got) != 0 {
		t.Fatalf("of no provider: %v", got)
	}
	if err := s.SetSSOUserState(ctx, u1, SSOActive, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSSOUserState(ctx, "bbbbbbbb-0000-4000-8000-0000000000ff", SSOActive, now); err != ErrNotFound {
		t.Fatalf("state of an unknown user: %v", err)
	}
	if got := strings.Join(list(SSOPending, nil), ","); got != "3" {
		t.Fatalf("pending after approval %s", got)
	}
	if err := s.DeleteSSOUser(ctx, u3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSSOUser(ctx, u3); err != ErrNotFound {
		t.Fatalf("deleted user: %v", err)
	}
	// Refusals are kept by provider and lower-cased address; an unregistered provider keeps none.
	const nobody = "a0000000-0000-4000-8000-0000000000ee"
	for _, p := range []string{a, b, nobody} {
		if err := s.DenySSOEmail(ctx, p, "Alice@A.test", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		provider, email string
		want            bool
	}{{a, "alice@a.test", true}, {a, "ALICE@a.test", true}, {b, "alice@a.test", true}, {a, "bob@a.test", false}, {nobody, "alice@a.test", false}} {
		if got, err := s.SSOEmailDenied(ctx, c.provider, c.email); err != nil || got != c.want {
			t.Fatalf("denied(%s, %s) = %v, %v; want %v", c.provider[len(c.provider)-1:], c.email, got, err, c.want)
		}
	}
	if err := s.DenySSOEmail(ctx, a, "alice@a.test", now); err != nil {
		t.Fatalf("denying twice: %v", err)
	}
	if ok, err := s.AllowSSOEmail(ctx, b, "alice@A.test"); err != nil || !ok {
		t.Fatalf("allow: %v, %v", ok, err)
	}
	if ok, err := s.AllowSSOEmail(ctx, b, "alice@a.test"); err != nil || ok {
		t.Fatalf("allow twice: %v, %v", ok, err)
	}
	if ok, err := s.AllowSSOEmail(ctx, "nope", "alice@a.test"); err != nil || ok {
		t.Fatalf("allow at a malformed provider: %v, %v", ok, err)
	}
	// Deleting a provider returns and forgets its users, and its refusals.
	gone, err := s.DeleteProvider(ctx, a)
	if err != nil || len(gone) != 2 {
		t.Fatalf("delete provider: %v, %v", gone, err)
	}
	if _, err := s.GetSSOUser(ctx, u1); err != ErrNotFound {
		t.Fatalf("a user of a deleted provider: %v", err)
	}
	if _, err := s.DeleteProvider(ctx, a); err != ErrNotFound {
		t.Fatalf("delete twice: %v", err)
	}
	if got, err := s.SSOEmailDenied(ctx, a, "alice@a.test"); err != nil || got {
		t.Fatalf("a deleted provider keeps a refusal: %v, %v", got, err)
	}
	if _, err := s.DeleteProvider(ctx, b); err != nil {
		t.Fatal(err)
	}
}

func TestMemorySignupGrants(t *testing.T) { testSignupGrants(t, NewMemoryClaimStore()) }

func TestPGSignupGrants(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := registry.Migrate(ctx, r.Pool()); err != nil {
		t.Fatal(err)
	}
	testSignupGrants(t, NewPGClaimStore(r.Pool()))
}

// A grant is for one address, works once and expires.
func testSignupGrants(t *testing.T, s ClaimStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	h := func(x string) []byte { v := sha256.Sum256([]byte(x)); return v[:] }
	if err := s.CreateSignupGrant(ctx, "New@Example.test", h("a"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSignupGrant(ctx, "old@example.test", h("old"), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		email string
		token string
		at    time.Time
	}{
		"another address": {"other@example.test", "a", now},
		"another token":   {"new@example.test", "b", now},
		"expired":         {"old@example.test", "old", now},
		"too late":        {"new@example.test", "a", now.Add(2 * time.Minute)},
	} {
		if ok, err := s.ConsumeSignupGrant(ctx, tc.email, h(tc.token), tc.at); err != nil || ok {
			t.Errorf("%s: %v %v", name, ok, err)
		}
	}
	if err := s.CreateSignupGrant(ctx, "new@example.test", h("a"), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ConsumeSignupGrant(ctx, "NEW@example.test", h("a"), now); err != nil || !ok {
		t.Fatalf("the grant: %v %v", ok, err)
	}
	if ok, err := s.ConsumeSignupGrant(ctx, "new@example.test", h("a"), now); err != nil || ok {
		t.Fatalf("the grant twice: %v %v", ok, err)
	}
}

// `supavise users remove` takes a waiting person off the pending list.
func TestSSOUserRemovalClearsThePendingList(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "", "acme.test")
	f.gt.addUser(ssoUser1, "alice@acme.test", time.Now())
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/profile", nil); rec.Code != 403 {
		t.Fatalf("pending: %d", rec.Code)
	}
	if us, _ := f.srv.sso.Pending(context.Background(), f.org.ID); len(us) != 1 {
		t.Fatalf("pending: %+v", us)
	}
	if _, err := f.srv.accounts.RemoveUser(context.Background(), "alice@acme.test", false); err != nil {
		t.Fatal(err)
	}
	if us, _ := f.srv.sso.Pending(context.Background(), f.org.ID); len(us) != 0 {
		t.Fatalf("a removed account is still pending: %+v", us)
	}
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/profile", nil); rec.Code != 401 {
		t.Fatalf("the removed account's session: %d", rec.Code)
	}
}

// An organization that requires MFA still lets its SSO users in: the identity provider is where
// strong authentication is enforced, and GoTrue marks an SSO session aal1 whatever the provider did.
func TestSSOSessionsMeetTheMFARequirement(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	if err := f.srv.members.SetMFAEnforced(context.Background(), f.org.ID, true); err != nil {
		t.Fatal(err)
	}
	if rec := f.doAs(tok, "GET", "/platform/organizations/default", nil); rec.Code != 200 {
		t.Fatalf("an SSO session under the MFA requirement: %d %s", rec.Code, rec.Body)
	}
	// A password session below aal2 is still refused: the requirement is on.
	pw := f.signJWT(map[string]any{"sub": f.userID, "email": "dev@example.test", "role": "authenticated", "aal": "aal1"})
	if rec := f.doAs(pw, "GET", "/platform/organizations/default", nil); rec.Code != 403 {
		t.Fatalf("a password session without a second factor: %d", rec.Code)
	}
}

// Studio's form may send the metadata address and the document: the address wins.
func TestOrgSSOCreateTakesTheAddressWhenBothAreSent(t *testing.T) {
	f := newSSOFixture(t)
	rec := f.as("owner", "POST", orgSSO, map[string]any{
		"enabled": true, "metadata_xml_url": "https://idp.acme.test/metadata.xml", "metadata_xml_file": testIdPMetadata(otherIdP),
		"domains": []string{"acme.test"}, "email_mapping": []string{"email"}, "join_org_on_signup_enabled": false,
	})
	if rec.Code != 201 || jsonField(t, rec, "metadata_xml_url") != "https://idp.acme.test/metadata.xml" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got, err := f.srv.sso.List(context.Background(), f.org.ID)
	if err != nil || len(got) != 1 || got[0].SAML.EntityID != "https://idp.acme.test" {
		t.Fatalf("GoTrue registered the document instead of the address: %+v %v", got, err)
	}
}

// The registry's event log says who did what to the dashboard's identity providers and which
// users came through them.
func TestSSOAuditEvents(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "", "acme.test")
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	f.doAs(tok, "GET", "/platform/profile", nil) // pending
	if rec := f.as("owner", "POST", orgSSO+"/pending/"+ssoUser1, map[string]any{"role": "developer"}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	evs, err := f.reg.ListEvents(context.Background(), "system", 50)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
		if strings.Contains(string(e.Payload), "idp.acme.test") && !strings.Contains(e.Kind, "added") {
			t.Errorf("%s carries more than ids: %s", e.Kind, e.Payload)
		}
	}
	got := strings.Join(kinds, ",")
	for _, want := range []string{"sso.provider.added", "sso.user.first_sign_in", "sso.user.approved", "sso.provider.removed"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s event: %s", want, got)
		}
	}
}

// Whoever controls a provider controls the accounts of its users: an Administrator cannot swap
// the metadata or attribute mapping of a provider with an Owner among its users, switch it off or
// remove it, even though its default role is one the Administrator may hand out.
func TestSSOAdministratorsCannotTakeOverOrLockOutAnOwnerThroughAProvider(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	rec := f.as("admin", "POST", orgSSO+"/providers", map[string]any{"type": "saml", "metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"acme.test"}, "default_role": "developer"})
	if rec.Code != 201 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	id := jsonField(t, rec, "id").(string)
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first sign-in: %d %s", rec.Code, rec.Body)
	}
	// While the users are Developers, an Administrator may still swap the metadata.
	if rec := f.as("admin", "PUT", orgSSO+"/providers/"+id, map[string]any{"metadata_xml": testIdPMetadata(acmeIdP)}); rec.Code != 200 {
		t.Fatalf("metadata change with Developer users: %d %s", rec.Code, rec.Body)
	}
	// The user is promoted to Owner later.
	org := members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}
	if err := f.srv.members.SetOrgRole(ctx, nil, org, ssoUser1, members.RoleOwner); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]map[string]any{
		"metadata":          {"metadata_xml": testIdPMetadata(acmeIdP)},
		"metadata address":  {"metadata_url": "https://idp.evil.test/metadata"},
		"attribute mapping": {"attribute_mapping": map[string]any{"keys": map[string]any{"email": map[string]any{"name": "mail"}}}},
		"name id format":    {"name_id_format": "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"},
	} {
		if rec := f.as("admin", "PUT", orgSSO+"/providers/"+id, body); rec.Code != 403 {
			t.Errorf("an Administrator changing the %s of a provider with an Owner user: %d %s", name, rec.Code, rec.Body)
		}
	}
	// Studio's organization page is the same route to the same provider.
	if rec := f.as("admin", "PUT", orgSSO, map[string]any{"metadata_xml_file": testIdPMetadata(acmeIdP), "email_mapping": []string{"mail"}}); rec.Code != 403 {
		t.Errorf("an Administrator through the organization page: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("admin", "PUT", orgSSO, map[string]any{"enabled": false}); rec.Code != 403 {
		t.Errorf("an Administrator switching the provider off: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("admin", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 403 {
		t.Errorf("an Administrator removing the provider: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("admin", "DELETE", orgSSO, nil); rec.Code != 403 {
		t.Errorf("an Administrator removing the provider through the organization page: %d %s", rec.Code, rec.Body)
	}
	if got := f.gt.sso.ids(); len(got) != 1 {
		t.Fatalf("the provider is gone from GoTrue: %v", got)
	}
	// The Owner's session still works, and an Owner may do all of it.
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("the Owner's session: %d", rec.Code)
	}
	if rec := f.as("owner", "PUT", orgSSO+"/providers/"+id, map[string]any{"metadata_xml": testIdPMetadata(acmeIdP)}); rec.Code != 200 {
		t.Fatalf("an Owner changing the metadata: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 200 {
		t.Fatalf("an Owner removing the provider: %d %s", rec.Code, rec.Body)
	}
}

// A user who holds an Owner role only on some projects, or in another organization, counts too.
func TestSSOProviderUsersWithScopedOrOtherOrganizationRoles(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	org := members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first sign-in: %d", rec.Code)
	}
	// Owner of a project in the organization.
	if err := f.srv.members.AssignProjectRole(ctx, nil, org, ssoUser1, members.RoleOwner, []string{testRef}); err != nil {
		t.Fatal(err)
	}
	if rec := f.as("admin", "PUT", orgSSO+"/providers/"+id, map[string]any{"name_id_format": "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"}); rec.Code != 403 {
		t.Fatalf("project-scoped Owner: %d %s", rec.Code, rec.Body)
	}
	// Owner of another organization the Administrator does not belong to.
	f.srv.sso.forget(id)
	bravo, err := f.reg.CreateOrganization(ctx, "bravo", "Bravo")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.members.EnsureOwner(ctx, members.OrgRef{ID: bravo.ID, Slug: bravo.Slug}, ssoUser1); err != nil {
		t.Fatal(err)
	}
	if rec := f.as("admin", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 403 {
		t.Fatalf("Owner of another organization: %d %s", rec.Code, rec.Body)
	}
}

// The provider's own default role is what a first-time user gets: a rule of the organization that
// an operator set for the domain before the provider was added cannot hand out more.
func TestSSOProviderRoleOverridesAnOperatorsRule(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	if err := f.srv.members.SetDomainDefault(ctx, "acme.test", f.org.ID, members.RoleOwner); err != nil {
		t.Fatal(err)
	}
	// No default role: the rule goes, and the user waits.
	rec := f.as("admin", "POST", orgSSO+"/providers", map[string]any{"type": "saml", "metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"acme.test"}})
	if rec.Code != 201 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	id := jsonField(t, rec, "id").(string)
	if _, err := f.srv.members.Store.GetDomainDefault(ctx, "acme.test"); err == nil {
		t.Fatal("the operator's Owner rule outlived a provider without a default role")
	}
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 403 {
		t.Fatalf("a user of a provider without a default role: %d %s", rec.Code, rec.Body)
	}
	if got := f.roleOf(ssoUser1); got != 0 {
		t.Fatalf("role %d", got)
	}
	// A rule changed on its own later does not raise the provider's role.
	if rec := f.as("owner", "PUT", orgSSO+"/providers/"+id, map[string]any{"default_role": "developer"}); rec.Code != 200 {
		t.Fatalf("set the role: %d %s", rec.Code, rec.Body)
	}
	if err := f.srv.members.SetDomainDefault(ctx, "acme.test", f.org.ID, members.RoleOwner); err != nil {
		t.Fatal(err)
	}
	f.srv.sso.forget(id)
	if rec := f.doAs(f.ssoToken(ssoUser2, "bob@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("second user: %d %s", rec.Code, rec.Body)
	}
	if got := f.roleOf(ssoUser2); got != members.RoleDeveloper {
		t.Fatalf("role of a user whose domain rule drifted to Owner: %d, want Developer", got)
	}
}

// A domain that another organization's rule holds is not the Administrator's to take, with a
// default role or without one.
func TestSSOProviderCannotOverwriteAnotherOrganizationsRule(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	bravo, err := f.reg.CreateOrganization(ctx, "bravo", "Bravo")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.members.SetDomainDefault(ctx, "acme.test", bravo.ID, members.RoleReadOnly); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"admin", "owner"} {
		rec := f.as(who, "POST", orgSSO+"/providers", map[string]any{"type": "saml", "metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"acme.test"}, "default_role": "developer"})
		if rec.Code != 409 {
			t.Fatalf("%s adding a provider for a domain of another organization's rule: %d %s", who, rec.Code, rec.Body)
		}
	}
	if n := len(f.gt.sso.ids()); n != 0 {
		t.Fatal("the refused provider reached GoTrue")
	}
	if r, err := f.srv.members.Store.GetDomainDefault(ctx, "acme.test"); err != nil || r.OrgID != bravo.ID || r.RoleID != members.RoleReadOnly {
		t.Fatalf("bravo's rule: %+v, %v", r, err)
	}
	// A provider without a role sets no rule, but it still takes over the sign-in of the domain's
	// addresses (GoTrue finds the provider by domain, node-wide), so it is refused as well; so is
	// moving a provider onto the domain later.
	rec := f.as("admin", "POST", orgSSO+"/providers", map[string]any{"type": "saml", "metadata_xml": testIdPMetadata(acmeIdP), "domains": []string{"acme.test"}})
	if rec.Code != 409 {
		t.Fatalf("add without a role for a domain of another organization's rule: %d %s", rec.Code, rec.Body)
	}
	id := f.addProvider(acmeIdP, "", "other.acme.test")
	if rec := f.as("admin", "PUT", orgSSO+"/providers/"+id, map[string]any{"domains": []string{"acme.test"}}); rec.Code != 409 {
		t.Fatalf("moving a provider onto another organization's domain: %d %s", rec.Code, rec.Body)
	}
	if r, err := f.srv.members.Store.GetDomainDefault(ctx, "acme.test"); err != nil || r.OrgID != bravo.ID {
		t.Fatalf("bravo's rule after the refusals: %+v, %v", r, err)
	}
}

// ---- one address, several accounts -------------------------------------------

// addSSOAccount puts an account that came from single sign-on in the fake GoTrue, whose uniqueness
// index (like GoTrue's) does not count it.
func (g *fakeGoTrue) addSSOAccount(id, email, provider string, created time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[id] = map[string]any{"id": id, "email": email, "created_at": created.UTC().Format(time.RFC3339),
		"app_metadata": map[string]any{"provider": "sso:" + provider, "providers": []string{"sso:" + provider}}}
}

func (g *fakeGoTrue) has(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.users[id]
	return ok
}

const (
	pwUser     = "cccccccc-0000-4000-8000-000000000001"
	shadowUser = "cccccccc-0000-4000-8000-000000000002"
	shadow2    = "cccccccc-0000-4000-8000-000000000003"
)

// An identity provider can vouch for the address of a password account, and the newer SSO account
// then comes first in GoTrue's list. The operator's commands and the invitation path must never
// take it for the password account: `users role ... owner` would make it Owner.
func TestSSOAccountOfAnExistingAddressDoesNotShadowThePasswordAccount(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	acc := f.srv.accounts
	other := "https://idp.other.test/saml"
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	id2 := f.gt.sso.add(other, "other.test")
	f.gt.addUser(pwUser, "carol@acme.test", time.Now().Add(-48*time.Hour))
	f.addMember(pwUser, members.RoleDeveloper)
	f.gt.addSSOAccount(shadowUser, "Carol@Acme.test", id, time.Now())
	f.gt.addSSOAccount(shadow2, "carol@acme.test", id2, time.Now().Add(-time.Hour))

	users, err := acc.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]DashboardUser{}
	for _, u := range users {
		seen[u.ID] = u
	}
	if seen[shadowUser].SSOProvider != id || seen[shadow2].SSOProvider != id2 || seen[pwUser].SSOProvider != "" {
		t.Fatalf("the list does not say how each account signs in: %+v", seen)
	}

	// Resolution: the password account, unless the operator names another.
	for name, tc := range map[string]struct {
		sel  UserSelector
		want string
	}{
		"no selector":        {UserSelector{}, pwUser},
		"password provider":  {UserSelector{Provider: "email"}, pwUser},
		"the SSO provider":   {UserSelector{Provider: id}, shadowUser},
		"with the prefix":    {UserSelector{Provider: "sso:" + id2}, shadow2},
		"by id":              {UserSelector{UserID: shadowUser}, shadowUser},
		"by id and provider": {UserSelector{UserID: shadow2, Provider: id2}, shadow2},
	} {
		u, err := acc.ResolveUser(ctx, "carol@acme.test", tc.sel)
		if err != nil || u == nil || u.ID != tc.want {
			t.Errorf("%s: %+v, %v; want %s", name, u, err, tc.want)
		}
	}
	if _, err := acc.ResolveUser(ctx, "carol@acme.test", UserSelector{UserID: pwUser, Provider: id}); err == nil {
		t.Error("an id and a provider of different accounts resolved")
	}

	// `users role`: the password account gets the role, the shadow does not.
	if _, u, err := acc.SetRoleOf(ctx, "carol@acme.test", "", "owner", UserSelector{}); err != nil || u.ID != pwUser {
		t.Fatalf("set role: %+v, %v", u, err)
	}
	if got := f.roleOf(pwUser); got != members.RoleOwner {
		t.Fatalf("the password account is %d, want Owner", got)
	}
	if got := f.roleOf(shadowUser); got != 0 {
		t.Fatalf("the single sign-on account got role %d", got)
	}
	// Naming the SSO account is how to give it a role.
	if _, u, err := acc.SetRoleOf(ctx, "carol@acme.test", "", "read-only", UserSelector{Provider: id}); err != nil || u.ID != shadowUser {
		t.Fatalf("set role of the SSO account: %+v, %v", u, err)
	}
	if got := f.roleOf(shadowUser); got != members.RoleReadOnly || f.roleOf(pwUser) != members.RoleOwner {
		t.Fatalf("roles: SSO %d, password %d", got, f.roleOf(pwUser))
	}

	// `users invite`: the address has a password account, so the invitee is an existing user (no
	// claim link); the SSO accounts do not count as "already a member" either.
	if _, err := acc.InviteToOrganization(ctx, nil, members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}, members.InviteInput{Email: "carol@acme.test", RoleID: members.RoleDeveloper}); !errors.Is(err, members.ErrAlreadyMember) {
		t.Fatalf("inviting the password account, a member: %v", err)
	}

	// `users remove` removes the password account only.
	if _, err := acc.RemoveUser(ctx, "carol@acme.test", true); err != nil {
		t.Fatal(err)
	}
	if f.gt.has(pwUser) || !f.gt.has(shadowUser) || !f.gt.has(shadow2) {
		t.Fatal("remove did not take exactly the password account")
	}
	// What is left is two SSO accounts: ambiguous without a selector.
	var amb *AmbiguousUserError
	if _, err := acc.ResolveUser(ctx, "carol@acme.test", UserSelector{}); !errors.As(err, &amb) || len(amb.Matches) != 2 ||
		!strings.Contains(err.Error(), "--user-id") || !strings.Contains(err.Error(), shadowUser) {
		t.Fatalf("two SSO accounts without a selector: %v", err)
	}
	if _, err := acc.RemoveUser(ctx, "carol@acme.test", true); !errors.As(err, &amb) || !f.gt.has(shadowUser) || !f.gt.has(shadow2) {
		t.Fatalf("remove with an ambiguous address: %v", err)
	}
	if _, err := acc.SetRole(ctx, "carol@acme.test", "", "owner"); !errors.As(err, &amb) {
		t.Fatalf("set role with an ambiguous address: %v", err)
	}
	// Naming one removes that one, and the address then has a single account.
	if u, _, err := acc.RemoveUserBy(ctx, "carol@acme.test", true, UserSelector{Provider: id2}); err != nil || u.ID != shadow2 {
		t.Fatalf("remove by provider: %+v, %v", u, err)
	}
	if u, err := acc.ResolveUser(ctx, "carol@acme.test", UserSelector{}); err != nil || u == nil || u.ID != shadowUser {
		t.Fatalf("the one account left: %+v, %v", u, err)
	}
	if u, err := acc.ResolveUser(ctx, "nobody@acme.test", UserSelector{}); err != nil || u != nil {
		t.Fatalf("an unknown address: %+v, %v", u, err)
	}
}

// An address that only an identity provider has an account for is invited like a new address: the
// invitee gets the link that creates a password account.
func TestInvitingAnAddressThatOnlyAnSSOAccountHas(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "", "acme.test")
	f.gt.addSSOAccount(shadowUser, "dave@acme.test", id, time.Now())
	f.srv.accounts.NoMail = true
	res, org, err := f.srv.accounts.InviteByEmail(ctx, "dave@acme.test", "", "developer", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ClaimURL == "" {
		t.Fatalf("no link that creates the password account: %+v", res)
	}
	// The SSO account stays what it was: not a member.
	if m, err := f.srv.members.Store.GetMember(ctx, org.ID, shadowUser); err == nil && m != nil {
		t.Fatalf("the SSO account became a member: %+v", m)
	}
}

// A first sign-in whose address has a password account is recorded as such in the audit trail.
func TestSSOFirstSignInWithAPasswordAccountsAddressIsAudited(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	f.gt.addUser(pwUser, "alice@acme.test", time.Now().Add(-time.Hour))
	f.addMember(pwUser, members.RoleOwner)
	f.gt.addSSOAccount(ssoUser1, "alice@acme.test", id, time.Now())
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first sign-in: %d %s", rec.Code, rec.Body)
	}
	evs, err := f.reg.ListEvents(ctx, config.SystemRef, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Kind == "sso.user.first_sign_in" && strings.Contains(string(e.Payload), `"shares_email_with":"`+pwUser+`"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no first-sign-in event names the password account: %+v", evs)
	}
	// The SSO user did not take the password account's Owner role.
	if f.roleOf(ssoUser1) != members.RoleDeveloper || f.roleOf(pwUser) != members.RoleOwner {
		t.Fatalf("roles: SSO %d, password %d", f.roleOf(ssoUser1), f.roleOf(pwUser))
	}
}

// Denying a user who joined an organization since the last request does not delete the account.
func TestSSODenyRefusesAUserWhoIsAMemberNow(t *testing.T) {
	f := newSSOFixture(t)
	id := f.addProvider(acmeIdP, "", "acme.test")
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	f.gt.addUser(ssoUser1, "alice@acme.test", time.Now())
	if rec := f.doAs(tok, "GET", "/platform/profile", nil); rec.Code != 403 {
		t.Fatalf("pending: %d", rec.Code)
	}
	// Made a member by other means (`supavise users role`) before the user's next request.
	f.addMember(ssoUser1, members.RoleDeveloper)
	if rec := f.as("owner", "DELETE", orgSSO+"/pending/"+ssoUser1, nil); rec.Code != 409 {
		t.Fatalf("deny a member: %d %s", rec.Code, rec.Body)
	}
	if !f.gt.has(ssoUser1) {
		t.Fatal("the member's account was deleted")
	}
	if u, err := f.srv.sso.Store.GetSSOUser(context.Background(), ssoUser1); err != nil || u.State != SSOActive {
		t.Fatalf("state %+v, %v", u, err)
	}
	if rec := f.doAs(tok, "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("the member's session: %d %s", rec.Code, rec.Body)
	}
	if n := len(jsonField(t, f.as("owner", "GET", orgSSO+"/pending", nil), "items").([]any)); n != 0 {
		t.Fatalf("the member is still listed as pending")
	}
}

// Removing a provider removes its users' memberships (they cannot sign in any more), and refuses
// when one of them is the only Owner of an organization.
func TestSSORemovingAProviderRemovesItsUsersMemberships(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	org := members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	for u, email := range map[string]string{ssoUser1: "alice@acme.test", ssoUser2: "bob@acme.test"} {
		if rec := f.doAs(f.ssoToken(u, email, id), "GET", "/platform/projects", nil); rec.Code != 200 {
			t.Fatalf("first sign-in of %s: %d", email, rec.Code)
		}
	}
	if err := f.srv.members.AssignProjectRole(ctx, nil, org, ssoUser2, members.RoleReadOnly, []string{testRef}); err != nil {
		t.Fatal(err)
	}
	// Bob is promoted to Owner; the original Owner steps down, so Bob is the only one.
	if err := f.srv.members.SetOrgRole(ctx, nil, org, ssoUser2, members.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.members.SetOrgRole(ctx, nil, org, f.userID, members.RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.sso.Remove(ctx, nil, id, 0); err == nil || !strings.Contains(err.Error(), "only Owner") {
		t.Fatalf("removing the provider of the only Owner: %v", err)
	}
	if got := f.gt.sso.ids(); len(got) != 1 {
		t.Fatalf("the refused removal reached GoTrue: %v", got)
	}
	if f.roleOf(ssoUser2) != members.RoleOwner {
		t.Fatal("the only Owner lost the role in a refused removal")
	}
	// With another Owner, the removal goes through and takes every SSO user's memberships.
	if err := f.srv.members.SetOrgRole(ctx, nil, org, f.userID, members.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.sso.Remove(ctx, nil, id, 0); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, u := range []string{ssoUser1, ssoUser2} {
		if ms, _ := f.srv.members.Store.MembershipsOf(ctx, u); len(ms) != 0 {
			t.Errorf("%s still has memberships: %+v", u, ms)
		}
		if prs, _ := f.srv.members.Store.ProjectRolesOf(ctx, u); len(prs) != 0 {
			t.Errorf("%s still has project roles: %+v", u, prs)
		}
	}
	if n, _ := f.srv.members.Store.CountOwners(ctx, f.org.ID); n != 1 {
		t.Errorf("the organization has %d Owners, want the one password Owner", n)
	}
}

// ---- refusals are kept by address -----------------------------------------------

// ssoRefusedSignIn is a person who had the default role, was refused (by afterwards), and signs in
// again with the address under a new GoTrue account: the new account waits and has no role.
func ssoRefusedSignIn(t *testing.T, f *ssoFixture, refuse func(id string), newUser string) (providerID string) {
	t.Helper()
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	f.gt.addSSOAccount(ssoUser1, "alice@acme.test", id, time.Now())
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 || f.roleOf(ssoUser1) != members.RoleDeveloper {
		t.Fatalf("first sign-in: %d %s, role %d", rec.Code, rec.Body, f.roleOf(ssoUser1))
	}
	refuse(id)
	if f.gt.has(ssoUser1) {
		t.Fatal("the account is still in GoTrue")
	}
	// The identity provider vouches for the address again; GoTrue makes a new account.
	f.gt.addSSOAccount(newUser, "Alice@Acme.Test", id, time.Now())
	tok := f.ssoToken(newUser, "Alice@Acme.Test", id)
	for _, path := range []string{"/platform/projects", "/v1/projects"} {
		if rec := f.doAs(tok, "GET", path, nil); rec.Code != 403 {
			t.Fatalf("GET %s of the refused address's new account: %d %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if got := f.roleOf(newUser); got != 0 {
		t.Fatalf("the refused address got the default role again: %d", got)
	}
	u, err := f.srv.sso.Store.GetSSOUser(ctx, newUser)
	if err != nil || u.State != SSOPending {
		t.Fatalf("new account %+v, %v", u, err)
	}
	evs, err := f.reg.ListEvents(ctx, config.SystemRef, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Kind == "sso.user.first_sign_in" && strings.Contains(string(e.Payload), newUser) && strings.Contains(string(e.Payload), "default_role_withheld") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the first-sight event does not say why the default role was withheld: %+v", evs)
	}
	return id
}

// An active default-role user who is denied (once waiting) and signs in again under a new GoTrue
// user id stays pending; approving the new account lets them in.
func TestSSODeniedUserWithADefaultRoleStaysOutAfterSigningInAgain(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	org := members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}
	id := ssoRefusedSignIn(t, f, func(id string) {
		// An administrator takes the membership away; the user waits, and is denied.
		if err := f.srv.members.RemoveMember(ctx, nil, org, ssoUser1); err != nil {
			t.Fatal(err)
		}
		f.srv.sso.forget(id)
		if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 403 {
			t.Fatalf("after losing the membership: %d", rec.Code)
		}
		if rec := f.as("owner", "DELETE", orgSSO+"/pending/"+ssoUser1, nil); rec.Code != 200 {
			t.Fatalf("deny: %d %s", rec.Code, rec.Body)
		}
	}, ssoUser2)
	// The new account is listed, and approving it lets the person in (and lifts the refusal).
	if us, _ := f.srv.sso.Pending(ctx, f.org.ID); len(us) != 1 || us[0].UserID != ssoUser2 {
		t.Fatalf("pending: %+v", us)
	}
	if rec := f.as("owner", "POST", orgSSO+"/pending/"+ssoUser2, map[string]any{"role": "read-only"}); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doAs(f.ssoToken(ssoUser2, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("after the approval: %d %s", rec.Code, rec.Body)
	}
	if denied, _ := f.srv.sso.Store.SSOEmailDenied(ctx, id, "alice@acme.test"); denied {
		t.Fatal("the approval left the refusal in place")
	}
}

// The same for removal with `supavise users remove`, and the operator's `sso allow` lifts it.
func TestSSORemovedUserWithADefaultRoleStaysOutAfterSigningInAgain(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := ssoRefusedSignIn(t, f, func(string) {
		if _, err := f.srv.accounts.RemoveUser(ctx, "alice@acme.test", false); err != nil {
			t.Fatal(err)
		}
	}, ssoUser2)
	// Nothing to lift for an address that was never refused.
	if err := f.srv.sso.Allow(ctx, id, "nobody@acme.test"); err == nil {
		t.Fatal("allowing an address that was not refused succeeded")
	}
	if err := f.srv.sso.Allow(ctx, id, "ALICE@acme.test"); err != nil {
		t.Fatalf("allow: %v", err)
	}
	// The waiting account is forgotten, so its next request is a first sight again.
	f.srv.sso.forget(id)
	if rec := f.doAs(f.ssoToken(ssoUser2, "Alice@Acme.Test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("after allow: %d %s", rec.Code, rec.Body)
	}
	if got := f.roleOf(ssoUser2); got != members.RoleDeveloper {
		t.Fatalf("role after allow: %d, want the default role", got)
	}
}

// A refusal is by address and provider: another address of the domain still gets the default role.
func TestSSORefusalOfOneAddressDoesNotTouchOthers(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	if err := f.srv.sso.Store.DenySSOEmail(ctx, id, "alice@acme.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := f.doAs(f.ssoToken(ssoUser3, "bob@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("another address: %d %s", rec.Code, rec.Body)
	}
}

// A personal access token of an SSO user who waits for approval is refused, like the session.
func TestSSOPendingUsersPersonalAccessTokenIsRefused(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	tok := f.ssoToken(ssoUser1, "alice@acme.test", id)
	rec := f.doAs(tok, "POST", "/platform/profile/access-tokens", map[string]any{"name": "ci"})
	if rec.Code != 201 && rec.Code != 200 {
		t.Fatalf("minting a token: %d %s", rec.Code, rec.Body)
	}
	pat := jsonField(t, rec, "token").(string)
	if r := f.doAs(pat, "GET", "/v1/projects", nil); r.Code != 200 {
		t.Fatalf("the token of a member: %d", r.Code)
	}
	if err := f.srv.members.RemoveMember(ctx, nil, members.OrgRef{ID: f.org.ID, Slug: f.org.Slug}, ssoUser1); err != nil {
		t.Fatal(err)
	}
	f.srv.sso.forget(id)
	if r := f.doAs(pat, "GET", "/v1/projects", nil); r.Code != 403 {
		t.Fatalf("the token of a user who waits: %d %s, want 403", r.Code, r.Body)
	}
	// Approved again, the token works again; a token of a password account never asks.
	if rec := f.as("owner", "POST", orgSSO+"/pending/"+ssoUser1, map[string]any{"role": "developer"}); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if r := f.doAs(pat, "GET", "/v1/projects", nil); r.Code != 200 {
		t.Fatalf("the token after the approval: %d %s", r.Code, r.Body)
	}
}

// A provider removal that stops at GoTrue can be run again: supavise's record is still there. A
// provider that GoTrue has lost already is as good as removed.
func TestSSORemovingAProviderThatGoTrueRefusedCanBeRetried(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	if rec := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); rec.Code != 200 {
		t.Fatalf("first sign-in: %d", rec.Code)
	}
	f.gt.sso.failNext = 500
	if rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 502 {
		t.Fatalf("removal while GoTrue fails: %d %s, want 502", rec.Code, rec.Body)
	}
	if _, err := f.srv.sso.Store.GetProvider(ctx, id); err != nil {
		t.Fatalf("supavise's record went with a removal that failed at GoTrue: %v", err)
	}
	if rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 200 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if got := f.gt.sso.ids(); len(got) != 0 {
		t.Fatalf("GoTrue still has %v", got)
	}
	if _, err := f.srv.sso.Store.GetProvider(ctx, id); err == nil {
		t.Fatal("the record is still there after the retry")
	}
	if rule, err := f.srv.members.Store.GetDomainDefault(ctx, "acme.test"); err == nil {
		t.Fatalf("the domain's rule outlived the provider: %+v", rule)
	}

	// GoTrue lost the provider (an earlier removal got that far): the removal finishes.
	id = f.addProvider(otherIdP, "", "other.test")
	f.gt.sso.mu.Lock()
	delete(f.gt.sso.providers, id)
	f.gt.sso.mu.Unlock()
	if rec := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); rec.Code != 200 {
		t.Fatalf("removal of a provider GoTrue no longer has: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.srv.sso.Store.GetProvider(ctx, id); err == nil {
		t.Fatal("the record is still there")
	}
}

// countingSSOStore counts the lookups of an SSO user's record.
type countingSSOStore struct {
	SSOStore
	gets atomic.Int32
}

func (c *countingSSOStore) GetSSOUser(ctx context.Context, userID string) (*SSOUser, error) {
	c.gets.Add(1)
	return c.SSOStore.GetSSOUser(ctx, userID)
}

// A user with no SSO record (every password account that holds a token) is looked up once per
// ssoCacheTTL, the way an admitted SSO user is, and the answer goes the moment the user is recorded.
func TestAdmitUserRemembersThatAUserIsNotAnSSOUser(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	counter := &countingSSOStore{SSOStore: f.srv.sso.Store}
	f.srv.sso.Store = counter
	clock := time.Now()
	f.srv.sso.Now = func() time.Time { return clock }

	for i := 0; i < 5; i++ {
		if err := f.srv.sso.AdmitUser(ctx, f.userID); err != nil {
			t.Fatalf("a password account: %v", err)
		}
	}
	if n := counter.gets.Load(); n != 1 {
		t.Fatalf("%d lookups for five requests of one user, want 1", n)
	}
	clock = clock.Add(ssoCacheTTL)
	if err := f.srv.sso.AdmitUser(ctx, f.userID); err != nil || counter.gets.Load() != 2 {
		t.Fatalf("after the TTL: %v, %d lookups, want a second lookup", err, counter.gets.Load())
	}

	// The same user id signs in through the provider and waits for approval: the next token request
	// sees the record and refuses, not the answer remembered for the user before.
	if err := f.srv.sso.AdmitUser(ctx, ssoUser2); err != nil {
		t.Fatalf("before the user is recorded: %v", err)
	}
	if err := f.srv.sso.Admit(ctx, ssoUser2, "mallory@contractor.test", id); err == nil {
		t.Fatal("a user of an unmapped domain was admitted")
	}
	if err := f.srv.sso.AdmitUser(ctx, ssoUser2); err == nil {
		t.Fatal("a token of a user who waits for approval passed on the answer remembered before the user was recorded")
	}
}
