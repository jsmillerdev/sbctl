package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/oauth"
)

// NewServer gives the accounts the OAuth service. Removing a user, and a single sign-on denial or a
// provider removal (they share these Accounts), then ends the user's grants through the real service:
// the refresh token stops working and oauth.grant_revoked records the reason and the actor.
func TestServerWiresTheOAuthServiceIntoTheAccounts(t *testing.T) {
	f := newFlowFixture(t)
	if f.srv.accounts.OAuth == nil {
		t.Fatal("Accounts.OAuth is not set by NewServer")
	}
	if f.srv.sso.Accounts != f.srv.accounts {
		t.Fatal("the dashboard SSO uses other Accounts than the server")
	}
	app := f.register(t, "")
	tok := f.redeem(t, app)
	refresh := tok["refresh_token"].(string)

	n, err := f.srv.accounts.revokeGrants(context.Background(), f.userID, oauth.ActorOperator)
	if err != nil || n != 1 {
		t.Fatalf("revokeGrants = %d, %v; want 1, nil", n, err)
	}
	rec := f.token(app, "grant_type", "refresh_token", "refresh_token", refresh)
	if rec.Code != http.StatusBadRequest || jsonMap(t, rec)["error"] != "invalid_grant" {
		t.Errorf("the grant of a removed user still refreshes: %d %s", rec.Code, rec.Body)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, e := range f.events {
		if e["kind"] != oauth.EventGrantRevoked {
			continue
		}
		p := e["payload"].(map[string]any)
		if p["reason"] == oauth.ReasonUserRemoved && p["actor"] == oauth.ActorOperator && p["user_id"] == f.userID {
			found = true
		}
	}
	if !found {
		t.Errorf("no oauth.grant_revoked event with the reason and the actor: %v", f.events)
	}
}

// A token that the real service issued works where the lanes' own tests use stand-ins: the
// Management API takes it on a route that is open to tokens (oauth_authn.go), refuses it on /platform,
// and the /mcp gate lets it through (mcp.go). Revoking the grant ends it in both places at once.
func TestIssuedOAuthTokenReachesTheAPIAndTheMCPGate(t *testing.T) {
	f := newFlowFixture(t)
	app := f.register(t, "")
	tok := f.redeem(t, app)
	access := tok["access_token"].(string)
	bearer := []string{"Authorization", "Bearer " + access}

	gate := func() (int, bool) {
		req := httptest.NewRequest(http.MethodPost, "https://api.example.test/mcp?project_ref="+testRef, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+access)
		rec := httptest.NewRecorder()
		_, ok := f.srv.MCPGate(rec, req)
		return rec.Code, ok
	}

	if rec := f.epGet(epAddr, "/v1/projects", bearer...); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/projects with an OAuth token: %d %s", rec.Code, rec.Body)
	}
	if rec := f.epGet(epAddr, "/platform/profile", bearer...); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /platform/profile with an OAuth token: %d %s", rec.Code, rec.Body)
	}
	if code, ok := gate(); !ok {
		t.Errorf("the MCP gate refused an issued token: %d", code)
	}

	if n, err := f.svc.RevokeUser(context.Background(), f.userID, oauth.ReasonUserRemoved, oauth.ActorOperator); err != nil || n != 1 {
		t.Fatalf("RevokeUser = %d, %v; want 1, nil", n, err)
	}
	if rec := f.epGet(epAddr, "/v1/projects", bearer...); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/projects after the grant was revoked: %d %s", rec.Code, rec.Body)
	}
	if code, ok := gate(); ok || code != http.StatusUnauthorized {
		t.Errorf("the MCP gate after the grant was revoked: %d, ok=%v", code, ok)
	}
}
