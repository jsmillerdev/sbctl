package lifecycle

import (
	"context"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// settingsEcho is a Settings that reports the external URL it was given, the way the saved auth
// settings turn it into OAuth redirect URIs.
type settingsEcho struct{ fakeSettings }

func (s *settingsEcho) AuthEnv(_ context.Context, _, externalURL string) (map[string]string, error) {
	return map[string]string{"GOTRUE_EXTERNAL_GITHUB_REDIRECT_URI": externalURL + "/callback"}, nil
}

// TestAPISpecsPresentTheProjectsDomain: an active custom hostname, or else a vanity subdomain,
// is what GoTrue builds its links, OAuth callbacks and SAML endpoints on (API_EXTERNAL_URL and the
// mail link paths). It is the only change: the token issuer, the site URL and the PostgREST unit
// keep their values, so sessions and clients survive adding and removing a domain, and the
// project's original host keeps working.
func TestAPISpecsPresentTheProjectsDomain(t *testing.T) {
	ctx := context.Background()
	pl, cfg := testPlane(t)
	pl.opts.Settings = &settingsEcho{}
	const ref = "abcdefghijklmnopqrst"
	if err := pl.reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: "p", Engine: registry.EnginePostgres}); err != nil {
		t.Fatal(err)
	}
	d := registry.Domains(pl.reg)
	p := testProject(cfg, ref, 2)
	keys := testKeys(t, ref)

	render := func() (auth, rest map[string]string) {
		t.Helper()
		specs, err := pl.apiSpecs(ctx, p, keys)
		if err != nil || len(specs) != 2 {
			t.Fatalf("apiSpecs: %d specs, %v", len(specs), err)
		}
		return specs[0].Env, specs[1].Env
	}
	derived := "https://" + ref + ".api.example.test/auth/v1"
	check := func(what, ext string) {
		t.Helper()
		auth, rest := render()
		if auth["API_EXTERNAL_URL"] != ext {
			t.Errorf("%s: API_EXTERNAL_URL = %q, want %q", what, auth["API_EXTERNAL_URL"], ext)
		}
		for _, k := range []string{"INVITE", "CONFIRMATION", "RECOVERY", "EMAIL_CHANGE"} {
			if got := auth["GOTRUE_MAILER_URLPATHS_"+k]; got != ext+"/verify" {
				t.Errorf("%s: %s = %q, want %q", what, k, got, ext+"/verify")
			}
		}
		if got := auth["GOTRUE_EXTERNAL_GITHUB_REDIRECT_URI"]; got != ext+"/callback" {
			t.Errorf("%s: OAuth redirect URI %q does not follow the external URL %q", what, got, ext)
		}
		// What hosted leaves alone stays: the issuer is the derived URL for good.
		if auth["GOTRUE_JWT_ISSUER"] != derived || auth["GOTRUE_SITE_URL"] != "http://localhost:3000" {
			t.Errorf("%s: issuer %q, site URL %q", what, auth["GOTRUE_JWT_ISSUER"], auth["GOTRUE_SITE_URL"])
		}
		if rest["PGRST_OPENAPI_SERVER_PROXY_URI"] != "https://"+ref+".api.example.test/rest/v1" {
			t.Errorf("%s: PostgREST OpenAPI URI %q", what, rest["PGRST_OPENAPI_SERVER_PROXY_URI"])
		}
	}

	check("no domain", derived)

	if err := d.PutVanitySubdomain(ctx, ref, "acme", cfg.VanityHost("acme")); err != nil {
		t.Fatal(err)
	}
	check("vanity", "https://acme.api.example.test/auth/v1")

	// A claim that is not active changes nothing; an active custom hostname wins over the vanity.
	if err := d.PutCustomHostname(ctx, &registry.CustomHostname{Ref: ref, Hostname: "docs.customer.example", Status: registry.HostnameInitiated, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	check("claimed hostname", "https://acme.api.example.test/auth/v1")
	h, _ := d.GetCustomHostname(ctx, ref)
	h.Status, h.CNAMEOK, h.TXTOK = registry.HostnameOriginReady, true, true
	if err := d.UpdateCustomHostname(ctx, h); err != nil {
		t.Fatal(err)
	}
	check("verified hostname", "https://acme.api.example.test/auth/v1")
	if _, err := d.ActivateCustomHostname(ctx, ref); err != nil {
		t.Fatal(err)
	}
	check("custom hostname", "https://docs.customer.example/auth/v1")

	// Deleting them gives the derived URL back.
	if err := d.DeleteCustomHostname(ctx, ref); err != nil {
		t.Fatal(err)
	}
	check("vanity again", "https://acme.api.example.test/auth/v1")
	if err := d.DeleteVanitySubdomain(ctx, ref); err != nil {
		t.Fatal(err)
	}
	check("derived again", derived)

	// Plain HTTP (tls.mode off, dev and tests) keeps its scheme.
	cfg.TLS.Mode = "off"
	if err := d.PutVanitySubdomain(ctx, ref, "acme", cfg.VanityHost("acme")); err != nil {
		t.Fatal(err)
	}
	if auth, _ := render(); auth["API_EXTERNAL_URL"] != "http://acme.api.example.test/auth/v1" {
		t.Errorf("tls off: %q", auth["API_EXTERNAL_URL"])
	}

	// The system project never takes a domain: it is the dashboard's sign-in service.
	sys := testProject(cfg, config.SystemRef, 0)
	sys.Class = ClassSystem
	specs, err := pl.apiSpecs(ctx, sys, testKeys(t, config.SystemRef))
	if err != nil || specs[0].Env["API_EXTERNAL_URL"] != "http://api.example.test/auth/v1" {
		t.Errorf("system project: %q %v", specs[0].Env["API_EXTERNAL_URL"], err)
	}
}
