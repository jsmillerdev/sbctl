package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/oauth"
)

// fakeOAuth is the way a test replaces the OAuth service: embed the interface, define the methods
// the test needs, and set Server.oauth. Handlers read Server.oauth when they run.
type fakeOAuth struct{ oauth.Authority }

var _ oauth.Authority = fakeOAuth{}

func TestOAuthServiceIsBuiltFromTheServer(t *testing.T) {
	f := newFixture(t)
	svc, ok := f.srv.oauth.(*oauth.Service)
	if !ok {
		t.Fatalf("Server.oauth is %T, want the *oauth.Service the server builds", f.srv.oauth)
	}
	if svc.Issuer != f.cfg.APIURL() || svc.Issuer != "https://api.example.test" {
		t.Errorf("Issuer = %q", svc.Issuer)
	}
	if svc.DashboardURL != f.cfg.DashboardURL() || svc.DashboardURL != "https://studio.example.test" {
		t.Errorf("DashboardURL = %q", svc.DashboardURL)
	}
	if _, ok := svc.Store.(*oauth.MemoryStore); !ok {
		t.Errorf("a memory registry gets a memory store, got %T", svc.Store)
	}
	if svc.Now == nil || svc.Admit == nil || svc.Audit == nil || svc.Log == nil {
		t.Errorf("the server must set Now, Admit, Audit and Log: %+v", svc)
	}
	if svc.Alert != nil {
		t.Error("Alert is the daemon's to wire; the server leaves it empty")
	}
	if f.srv.oauthDisabled() {
		t.Error("OAuth is on by default")
	}
	f.cfg.API.DisableOAuth = true
	if !f.srv.oauthDisabled() {
		t.Error("[api] disable_oauth is not read")
	}
}

func TestOAuthServiceKeepsWhatTheCallerSet(t *testing.T) {
	f := newFixture(t)
	store := oauth.NewMemoryStore()
	var alerted bool
	preset := &oauth.Service{
		Store:        store,
		Issuer:       "https://auth.other.example",
		DashboardURL: "https://studio.other.example",
		Alert:        func(context.Context, oauth.AlertEvent) { alerted = true },
	}
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.srv.sec, Manager: f.mgr, Config: f.cfg, Store: NewMemoryStore(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OAuth: preset})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := srv.oauth.(*oauth.Service); got != preset {
		t.Fatalf("the server must complete the service it is given in place, got %p want %p", got, preset)
	}
	if preset.Store != store || preset.Issuer != "https://auth.other.example" || preset.DashboardURL != "https://studio.other.example" {
		t.Errorf("what the caller set was changed: %+v", preset)
	}
	if preset.Admit == nil || preset.Audit == nil || preset.Now == nil || preset.Log == nil {
		t.Errorf("the empty fields were not filled: %+v", preset)
	}
	preset.Alert(context.Background(), oauth.AlertEvent{})
	if !alerted {
		t.Error("Alert was replaced")
	}
}

func TestOAuthAdmit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	svc := f.srv.oauth.(*oauth.Service)
	const developer = "aaaaaaaa-1111-4222-8333-444444444444"
	const stranger = "bbbbbbbb-1111-4222-8333-444444444444"
	f.addMember(developer, members.RoleDeveloper)

	other, err := f.reg.CreateOrganization(ctx, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}

	// Any role in the organization is admitted; a Developer cannot approve, but can hold a grant.
	for _, u := range []string{f.userID, developer} {
		if err := svc.Admit(ctx, u, f.org.ID); err != nil {
			t.Errorf("member %s: %v", u, err)
		}
	}
	// Not a member of that organization, or of any.
	if err := svc.Admit(ctx, developer, other.ID); !errors.Is(err, oauth.ErrNotMember) {
		t.Errorf("a member of another organization: %v", err)
	}
	if err := svc.Admit(ctx, stranger, f.org.ID); !errors.Is(err, oauth.ErrNotMember) {
		t.Errorf("a stranger: %v", err)
	}

	// An SSO user the provider no longer admits is refused; so is a failure to find out, but that one is
	// not reported as a refusal.
	was := f.srv.auth.ssoUser
	f.srv.auth.ssoUser = func(context.Context, string) error { return errSSOPending }
	if err := svc.Admit(ctx, developer, f.org.ID); !errors.Is(err, oauth.ErrNotAdmitted) {
		t.Errorf("an SSO user who waits for approval: %v", err)
	}
	boom := errors.New("registry unreachable")
	f.srv.auth.ssoUser = func(context.Context, string) error { return boom }
	if err := svc.Admit(ctx, developer, f.org.ID); !errors.Is(err, boom) || errors.Is(err, oauth.ErrNotAdmitted) {
		t.Errorf("a failed SSO check must come back as the failure: %v", err)
	}
	f.srv.auth.ssoUser = was

	// A removed user is refused whatever their memberships.
	if err := f.srv.accounts.Store.MarkUserRemoved(ctx, developer, "dev@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Admit(ctx, developer, f.org.ID); !errors.Is(err, oauth.ErrNotAdmitted) {
		t.Errorf("a removed user: %v", err)
	}
	// And a user who is not removed is still admitted.
	if err := svc.Admit(ctx, f.userID, f.org.ID); err != nil {
		t.Errorf("the owner after another user's removal: %v", err)
	}
}

func TestOAuthAuditWritesRegistryEvents(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	svc := f.srv.oauth.(*oauth.Service)
	// The event is written even when the request that caused it is gone.
	cancel()
	svc.Audit(ctx, oauth.EventGrantRevoked, map[string]any{"grant_id": 3, "reason": oauth.ReasonAdmin, "actor": oauth.ActorOperator})
	evs, err := f.reg.ListEvents(context.Background(), config.SystemRef, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind != oauth.EventGrantRevoked {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil || p["reason"] != oauth.ReasonAdmin {
			t.Fatalf("payload %s: %v", e.Payload, err)
		}
		return
	}
	t.Fatalf("no %s event on the system project: %+v", oauth.EventGrantRevoked, evs)
}
