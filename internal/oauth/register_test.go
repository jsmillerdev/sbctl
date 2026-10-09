package oauth

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/supavise/supavise/internal/secrets"
)

// TestRegisterValidation is R1: what registration accepts and refuses, field by field (section 2.4).
func TestRegisterValidation(t *testing.T) {
	fx := newSvc(t)
	ten := make([]string, 10)
	for i := range ten {
		ten[i] = "https://app.example.test/cb" + string(rune('a'+i))
	}
	tests := []struct {
		name string
		mod  func(*RegisterRequest)
		code string // "" = accepted
	}{
		{"minimal", func(r *RegisterRequest) {}, ""},
		{"ten redirect URIs", func(r *RegisterRequest) { r.RedirectURIs = ten }, ""},
		{"https and loopback together", func(r *RegisterRequest) {
			r.RedirectURIs = []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost/callback", "http://127.0.0.1/callback", "http://[::1]/callback"}
		}, ""},
		{"no name", func(r *RegisterRequest) { r.ClientName = "" }, CodeInvalidClientMetadata},
		{"blank name", func(r *RegisterRequest) { r.ClientName = " \t\n" }, CodeInvalidClientMetadata},
		{"name of control characters", func(r *RegisterRequest) { r.ClientName = "‮​\x00" }, CodeInvalidClientMetadata},
		{"name of 101 characters", func(r *RegisterRequest) { r.ClientName = strings.Repeat("a", 101) }, CodeInvalidClientMetadata},
		{"name that is not UTF-8", func(r *RegisterRequest) { r.ClientName = string([]byte{0xc3, 0x28}) }, CodeInvalidClientMetadata},
		{"no redirect URIs", func(r *RegisterRequest) { r.RedirectURIs = nil }, CodeInvalidRedirectURI},
		{"eleven redirect URIs", func(r *RegisterRequest) { r.RedirectURIs = append(slices.Clone(ten), "https://app.example.test/cbk") }, CodeInvalidRedirectURI},
		{"javascript redirect", func(r *RegisterRequest) { r.RedirectURIs = []string{"javascript:alert(1)"} }, CodeInvalidRedirectURI},
		{"custom scheme redirect", func(r *RegisterRequest) {
			r.RedirectURIs = []string{"cursor://anysphere.cursor-retrieval/oauth/callback"}
		}, CodeInvalidRedirectURI},
		{"vscode redirect", func(r *RegisterRequest) {
			r.RedirectURIs = []string{"vscode://vscode.github-authentication/did-authenticate"}
		}, CodeInvalidRedirectURI},
		{"http to a non-loopback host", func(r *RegisterRequest) { r.RedirectURIs = []string{"http://app.example.test/cb"} }, CodeInvalidRedirectURI},
		{"fragment in a redirect", func(r *RegisterRequest) { r.RedirectURIs = []string{"https://app.example.test/cb#x"} }, CodeInvalidRedirectURI},
		{"userinfo in a redirect", func(r *RegisterRequest) { r.RedirectURIs = []string{"https://u:p@app.example.test/cb"} }, CodeInvalidRedirectURI},
		{"one bad redirect among good ones", func(r *RegisterRequest) { r.RedirectURIs = []string{"https://app.example.test/cb", "ftp://x/y"} }, CodeInvalidRedirectURI},
		{"redirect of 2049 characters", func(r *RegisterRequest) {
			r.RedirectURIs = []string{"https://app.example.test/" + strings.Repeat("a", MaxRedirectURILen-24)}
		}, CodeInvalidRedirectURI},
		{"grant types: code only", func(r *RegisterRequest) { r.GrantTypes = []string{GrantTypeAuthorizationCode} }, ""},
		{"grant types: both", func(r *RegisterRequest) { r.GrantTypes = []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken} }, ""},
		{"grant types: implicit", func(r *RegisterRequest) { r.GrantTypes = []string{"implicit"} }, CodeInvalidClientMetadata},
		{"grant types: password", func(r *RegisterRequest) { r.GrantTypes = []string{"password"} }, CodeInvalidClientMetadata},
		{"grant types: jwt-bearer", func(r *RegisterRequest) { r.GrantTypes = []string{GrantTypeJWTBearer} }, CodeInvalidClientMetadata},
		{"grant types: client_credentials with code", func(r *RegisterRequest) {
			r.GrantTypes = []string{GrantTypeAuthorizationCode, "client_credentials"}
		}, CodeInvalidClientMetadata},
		{"response types: code", func(r *RegisterRequest) { r.ResponseTypes = []string{"code"} }, ""},
		{"response types: token", func(r *RegisterRequest) { r.ResponseTypes = []string{"token"} }, CodeInvalidClientMetadata},
		{"response types: code token", func(r *RegisterRequest) { r.ResponseTypes = []string{"code", "token"} }, CodeInvalidClientMetadata},
		{"auth method none", func(r *RegisterRequest) { r.TokenEndpointAuthMethod = AuthMethodNone }, ""},
		{"auth method post", func(r *RegisterRequest) { r.TokenEndpointAuthMethod = AuthMethodPost }, ""},
		{"auth method basic", func(r *RegisterRequest) { r.TokenEndpointAuthMethod = AuthMethodBasic }, ""},
		{"auth method private_key_jwt", func(r *RegisterRequest) { r.TokenEndpointAuthMethod = "private_key_jwt" }, CodeInvalidClientMetadata},
		{"scope: one", func(r *RegisterRequest) { r.Scope = "projects:read" }, ""},
		{"scope: some unknown", func(r *RegisterRequest) { r.Scope = "projects:read bogus:scope" }, ""},
		{"scope: only unknown", func(r *RegisterRequest) { r.Scope = "bogus:scope" }, CodeInvalidClientMetadata},
		{"scope: valid but not advertised", func(r *RegisterRequest) { r.Scope = "organizations:write auth:write" }, CodeInvalidClientMetadata},
		{"client_uri https", func(r *RegisterRequest) { r.ClientURI = "https://client.example.test" }, ""},
		{"client_uri http", func(r *RegisterRequest) { r.ClientURI = "http://client.example.test" }, CodeInvalidClientMetadata},
		{"client_uri javascript", func(r *RegisterRequest) { r.ClientURI = "javascript:alert(1)" }, CodeInvalidClientMetadata},
		{"logo_uri https", func(r *RegisterRequest) { r.LogoURI = "https://client.example.test/logo.png" }, ""},
		{"logo_uri data", func(r *RegisterRequest) { r.LogoURI = "data:image/png;base64,AAAA" }, CodeInvalidClientMetadata},
		{"logo_uri http", func(r *RegisterRequest) { r.LogoURI = "http://client.example.test/logo.png" }, CodeInvalidClientMetadata},
	}
	accepted := 0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := RegisterRequest{ClientName: "Claude Code", RedirectURIs: []string{"http://localhost:8976/callback"}}
			tc.mod(&req)
			ra, err := fx.svc.Register(fx.ctx(), req)
			if tc.code != "" {
				wantCode(t, err, tc.code)
				if e := oauthErr(t, err); e.HTTPStatus() != http.StatusBadRequest {
					t.Errorf("status = %d", e.HTTPStatus())
				}
				return
			}
			mustNoErr(t, err)
			accepted++
			if _, ok := canonUUID(ra.App.ID); !ok || ra.App.ID != strings.ToLower(ra.App.ID) {
				t.Errorf("client_id %q is not a UUID", ra.App.ID)
			}
		})
	}
	// Nothing that was refused left a row behind.
	if n, _ := fx.store.CountDynamicApps(fx.ctx()); n != accepted {
		t.Errorf("%d apps stored, %d registrations accepted", n, accepted)
	}
}

// TestRegisterResult checks what a registration stores and returns.
func TestRegisterResult(t *testing.T) {
	fx := newSvc(t)
	ra, err := fx.svc.Register(fx.ctx(), RegisterRequest{
		ClientName: "  Claude‮  Code\t(supabase) ", ClientURI: "https://client.example.test", LogoURI: "https://client.example.test/logo.png",
		RedirectURIs: []string{"http://localhost:8976/callback", "http://localhost:8976/callback", "https://claude.ai/api/mcp/auth_callback"},
		Scope:        "database:write projects:read database:write bogus", TokenEndpointAuthMethod: AuthMethodNone,
	})
	mustNoErr(t, err)

	if ra.App.Name != "Claude Code (supabase)" {
		t.Errorf("name = %q", ra.App.Name)
	}
	if !secrets.IsClientSecret(ra.ClientSecret) {
		t.Errorf("client secret %q has the wrong shape", ra.ClientSecret)
	}
	if !ra.IssuedAt.Equal(fx.clock.Now()) {
		t.Errorf("issued at %v", ra.IssuedAt)
	}
	if !slices.Equal(ra.GrantTypes, []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken}) || !slices.Equal(ra.ResponseTypes, []string{"code"}) {
		t.Errorf("grant types %v, response types %v", ra.GrantTypes, ra.ResponseTypes)
	}
	if ra.App.Icon != "" {
		t.Errorf("the logo is returned: %q", ra.App.Icon)
	}

	stored, err := fx.store.GetApp(fx.ctx(), ra.App.ID)
	mustNoErr(t, err)
	if stored.RegistrationType != RegistrationDynamic || stored.OrgID != 0 || stored.CreatedBy != "" {
		t.Errorf("registration %q org %d created_by %q", stored.RegistrationType, stored.OrgID, stored.CreatedBy)
	}
	if stored.Icon != "https://client.example.test/logo.png" || stored.Website != "https://client.example.test" {
		t.Errorf("the logo and the client_uri are stored: icon %q website %q", stored.Icon, stored.Website)
	}
	if !slices.Equal(stored.RedirectURIs, []string{"http://localhost:8976/callback", "https://claude.ai/api/mcp/auth_callback"}) {
		t.Errorf("redirect URIs %v", stored.RedirectURIs)
	}
	if !slices.Equal(stored.Scopes, []string{ScopeDatabaseWrite, ScopeProjectsRead}) {
		t.Errorf("scopes %v", stored.Scopes)
	}
	if stored.TokenEndpointAuthMethod != AuthMethodNone {
		t.Errorf("auth method %q", stored.TokenEndpointAuthMethod)
	}
	if !stored.CreatedAt.Equal(fx.clock.Now()) {
		t.Errorf("created at %v", stored.CreatedAt)
	}

	list, err := fx.store.ListSecrets(fx.ctx(), ra.App.ID)
	mustNoErr(t, err)
	if len(list) != 1 {
		t.Fatalf("want one secret, got %d", len(list))
	}
	if string(list[0].Hash) != string(secrets.HashToken(ra.ClientSecret)) || list[0].Alias != secrets.ClientSecretAlias(ra.ClientSecret) {
		t.Errorf("the secret is not stored as its hash and alias: %+v", list[0])
	}
	if got := fx.logbuf.String(); !strings.Contains(got, "client registered") || strings.Contains(got, ra.ClientSecret) {
		t.Errorf("log: %s", got)
	}
	if len(fx.eventsOf(EventAppCreated)) != 0 || len(fx.events) != 0 {
		t.Errorf("a dynamic registration is logged, not audited: %v", fx.events)
	}

	// Defaults: every advertised scope, both grants, the basic method.
	def := fx.register()
	if !slices.Equal(def.App.Scopes, NormalizeScopes(AdvertisedScopes)) || def.App.TokenEndpointAuthMethod != AuthMethodBasic {
		t.Errorf("defaults: scopes %v method %q", def.App.Scopes, def.App.TokenEndpointAuthMethod)
	}
	// Two registrations never share an id or a secret.
	if def.App.ID == ra.App.ID || def.ClientSecret == ra.ClientSecret {
		t.Error("registrations collide")
	}
}

// denyTransport records every attempt to leave the process.
type denyTransport struct{ hits *atomic.Int32 }

func (d denyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	d.hits.Add(1)
	return nil, errors.New("egress denied by the test")
}

// TestRegisterNoOutboundFetch is R1: client_uri and logo_uri are never fetched, not at registration and
// not when the consent page is described.
func TestRegisterNoOutboundFetch(t *testing.T) {
	var hits atomic.Int32
	oldT, oldC := http.DefaultTransport, http.DefaultClient.Transport
	http.DefaultTransport = denyTransport{&hits}
	http.DefaultClient.Transport = denyTransport{&hits}
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient.Transport = oldT, oldC })

	fx := newSvc(t)
	ra, err := fx.svc.Register(fx.ctx(), RegisterRequest{
		ClientName: "Fetcher", ClientURI: "https://client.invalid/about", LogoURI: "https://logo.invalid/logo.png",
		RedirectURIs: []string{"http://localhost/callback"},
	})
	mustNoErr(t, err)
	req := authorizeReq(&ra.App)
	authID := fx.start(req)
	if _, err := fx.svc.Describe(fx.ctx(), authID); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.ListAuthorizedApps(fx.ctx(), testOrgAcme); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the service tried %d outbound requests", n)
	}
}

// TestRegisterAppCap is R2: the cap on stored dynamic apps.
func TestRegisterAppCap(t *testing.T) {
	fx := newSvc(t)
	for i := 0; i < MaxDynamicApps-1; i++ {
		app := App{ID: newUUID(), RegistrationType: RegistrationDynamic, Name: "filler", RedirectURIs: []string{"http://localhost/cb"},
			Scopes: []string{ScopeProjectsRead}, TokenEndpointAuthMethod: AuthMethodBasic, CreatedAt: fx.clock.Now(), UpdatedAt: fx.clock.Now()}
		if err := fx.store.CreateApp(fx.ctx(), app, nil); err != nil {
			t.Fatal(err)
		}
	}
	// One slot is left.
	fx.register()
	_, err := fx.svc.Register(fx.ctx(), RegisterRequest{ClientName: "One too many", RedirectURIs: []string{"http://localhost/cb"}})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("want ErrLimit, got %v", err)
	}
	if n, _ := fx.store.CountDynamicApps(fx.ctx()); n != MaxDynamicApps {
		t.Errorf("count = %d", n)
	}
}
