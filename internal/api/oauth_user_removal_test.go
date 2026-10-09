package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
)

// grantRecorder stands in for the OAuth service where a user is removed: it records every RevokeUser
// call and, through check, lets a test look at the world at the moment the call is made.
type grantRecorder struct {
	mu    sync.Mutex
	calls []revokeCall
	err   error
	check func()
}

type revokeCall struct{ user, reason, actor string }

var _ GrantRevoker = (*grantRecorder)(nil)

// A real service satisfies the same interface, so the field can be set from Server.oauth.
var _ GrantRevoker = oauth.Authority(nil)

func (g *grantRecorder) RevokeUser(_ context.Context, userID, reason, actor string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.check != nil {
		g.check()
	}
	g.calls = append(g.calls, revokeCall{userID, reason, actor})
	if g.err != nil {
		return 0, g.err
	}
	return 2, nil
}

func (g *grantRecorder) got() []revokeCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]revokeCall(nil), g.calls...)
}

func (g *grantRecorder) fail(err error) { g.mu.Lock(); g.err = err; g.mu.Unlock() }

// `supavise users remove` ends the user's OAuth grants, after the user is recorded as removed (so the
// API refuses the grants' access tokens from that moment) and before the GoTrue account goes (so a
// failure leaves the account findable and the command can be run again).
func TestRemoveUserRevokesTheOAuthGrants(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, _ := f.acc.IssueClaimToken(ctx, 0, false)
	var res RedeemResult
	_ = json.Unmarshal(f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword}).Body.Bytes(), &res)

	rec := &grantRecorder{}
	rec.check = func() {
		if gone, _ := f.acc.Store.UserRemoved(ctx, res.UserID); !gone {
			t.Error("the grants were revoked before the user was recorded as removed")
		}
		if !f.gt.has(res.UserID) {
			t.Error("the grants were revoked after the GoTrue account was deleted")
		}
	}
	f.acc.OAuth = rec

	// A failure to revoke stops the removal before GoTrue's account goes, and the second run finishes.
	rec.fail(errors.New("registry unavailable"))
	if _, err := f.acc.RemoveUser(ctx, "a@example.test", true); err == nil || !strings.Contains(err.Error(), "OAuth grants") {
		t.Fatalf("a failed revocation: %v", err)
	}
	if us, _ := f.acc.ListUsers(ctx); len(us) != 1 {
		t.Fatalf("the account must stay findable so that the command can be run again: %+v", us)
	}
	rec.fail(nil)
	if _, err := f.acc.RemoveUser(ctx, "a@example.test", true); err != nil {
		t.Fatalf("the second run: %v", err)
	}
	if us, _ := f.acc.ListUsers(ctx); len(us) != 0 {
		t.Fatalf("user left after the second run: %+v", us)
	}
	want := revokeCall{res.UserID, oauth.ReasonUserRemoved, oauth.ActorOperator}
	if got := rec.got(); len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("RevokeUser calls = %+v, want two of %+v", got, want)
	}
}

// A server without OAuth wired (a nil revoker) removes users as before.
func TestRemoveUserWithoutAnOAuthRevoker(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	token, _, _ := f.acc.IssueClaimToken(ctx, 0, false)
	_ = f.post(RedeemRequest{Token: token, Email: "a@example.test", Password: goodPassword})
	f.acc.OAuth = nil
	if _, err := f.acc.RemoveUser(ctx, "a@example.test", true); err != nil {
		t.Fatal(err)
	}
}

// Denying a pending SSO user revokes the grants it may hold, in the name of the administrator who
// denied it. A failure is logged only: the user is recorded as removed, so nothing of the grants works.
func TestSSODenyRevokesTheOAuthGrants(t *testing.T) {
	f := newSSOFixture(t)
	rec := &grantRecorder{}
	f.srv.accounts.OAuth = rec
	id := f.addProvider(acmeIdP, "", "acme.test") // no default role: everybody waits
	tok := f.ssoToken(ssoUser3, "carol@acme.test", id)
	f.gt.addUser(ssoUser3, "carol@acme.test", time.Now())
	if r := f.doAs(tok, "GET", "/platform/profile", nil); r.Code != 403 {
		t.Fatalf("pending: %d", r.Code)
	}
	rec.fail(errors.New("registry unavailable"))
	if r := f.as("owner", "DELETE", orgSSO+"/pending/"+ssoUser3, nil); r.Code != 200 {
		t.Fatalf("deny with a failing revocation: %d %s", r.Code, r.Body)
	}
	want := revokeCall{ssoUser3, oauth.ReasonUserRemoved, f.ids["owner"]}
	if got := rec.got(); len(got) != 1 || got[0] != want {
		t.Fatalf("RevokeUser calls = %+v, want %+v", got, want)
	}
	if r := f.doAs(tok, "GET", "/platform/profile", nil); r.Code != 401 {
		t.Fatalf("after the denial: %d, want 401", r.Code)
	}
}

// Removing a provider revokes the grants of each of its users, and a grant that cannot be revoked
// keeps the provider record so that the removal can be run again (the rule for personal access tokens).
func TestSSORemovingAProviderRevokesItsUsersGrants(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	rec := &grantRecorder{}
	f.srv.accounts.OAuth = rec
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	for u, email := range map[string]string{ssoUser1: "alice@acme.test", ssoUser2: "bob@acme.test"} {
		if r := f.doAs(f.ssoToken(u, email, id), "GET", "/platform/projects", nil); r.Code != 200 {
			t.Fatalf("first sign-in of %s: %d", email, r.Code)
		}
	}

	rec.fail(errors.New("registry unavailable"))
	r := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil)
	if r.Code != 502 || !strings.Contains(r.Body.String(), "OAuth grants") {
		t.Fatalf("a failing revocation: %d %s, want 502 naming the grants", r.Code, r.Body)
	}
	if row, err := f.srv.sso.Store.GetProvider(ctx, id); err != nil || row == nil {
		t.Fatalf("the provider record must stay for the retry: %v", err)
	}

	rec.fail(nil)
	if r := f.as("owner", "DELETE", orgSSO+"/providers/"+id, nil); r.Code != 200 {
		t.Fatalf("retry: %d %s", r.Code, r.Body)
	}
	byUser := map[string]revokeCall{}
	for _, c := range rec.got() {
		byUser[c.user] = c
	}
	for _, u := range []string{ssoUser1, ssoUser2} {
		if c, ok := byUser[u]; !ok || c.reason != oauth.ReasonUserRemoved || c.actor != f.ids["owner"] {
			t.Errorf("RevokeUser for %s = %+v (%v), want reason %q by the Owner", u, c, ok, oauth.ReasonUserRemoved)
		}
	}
}

// The operator's removal of a provider (no acting user) is in the operator's name.
func TestSSOProviderRemovalByTheOperatorRevokesInTheOperatorsName(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	rec := &grantRecorder{}
	f.srv.accounts.OAuth = rec
	id := f.addProvider(acmeIdP, "developer", "acme.test")
	if r := f.doAs(f.ssoToken(ssoUser1, "alice@acme.test", id), "GET", "/platform/projects", nil); r.Code != 200 {
		t.Fatalf("first sign-in: %d", r.Code)
	}
	if _, err := f.srv.sso.Remove(ctx, (*members.Access)(nil), id, 0); err != nil {
		t.Fatal(err)
	}
	want := revokeCall{ssoUser1, oauth.ReasonUserRemoved, oauth.ActorOperator}
	if got := rec.got(); len(got) != 1 || got[0] != want {
		t.Fatalf("RevokeUser calls = %+v, want %+v", got, want)
	}
}
