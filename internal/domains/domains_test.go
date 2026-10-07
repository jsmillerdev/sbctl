package domains

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
)

// fakeDNS is a Resolver over fixed records; every name is NXDOMAIN until set.
type fakeDNS struct {
	cname map[string]string
	txt   map[string][]string
	host  map[string][]string
	calls int
}

func newFakeDNS() *fakeDNS {
	return &fakeDNS{cname: map[string]string{}, txt: map[string][]string{}, host: map[string][]string{}}
}

var errNX = &net.DNSError{Err: "no such host", IsNotFound: true}

func (f *fakeDNS) LookupCNAME(_ context.Context, h string) (string, error) {
	f.calls++
	if c, ok := f.cname[h]; ok {
		return c + ".", nil
	}
	if _, ok := f.host[h]; ok {
		return h + ".", nil // no CNAME: the resolver answers with the name itself
	}
	return "", errNX
}

func (f *fakeDNS) LookupTXT(_ context.Context, n string) ([]string, error) {
	f.calls++
	if v, ok := f.txt[n]; ok {
		return v, nil
	}
	return nil, errNX
}

func (f *fakeDNS) LookupHost(_ context.Context, h string) ([]string, error) {
	f.calls++
	if c, ok := f.cname[h]; ok {
		h = c
	}
	if v, ok := f.host[h]; ok {
		return v, nil
	}
	return nil, errNX
}

type env struct {
	svc *Service
	reg *registry.Memory
	dns *fakeDNS
	cfg *config.Config
	now time.Time
}

func newEnv(t *testing.T, mut func(*config.Config)) *env {
	t.Helper()
	cfg := &config.Config{Domain: "example.com", PublicIP: "203.0.113.7"}
	cfg.TLS.Mode = "auto"
	if mut != nil {
		mut(cfg)
	}
	reg := registry.NewMemory()
	ctx := context.Background()
	org, err := reg.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{refA, refB} {
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: org.ID, Name: ref, Engine: registry.EnginePostgres, Status: registry.StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{reg: reg, dns: newFakeDNS(), cfg: cfg, now: time.Unix(1_700_000_000, 0)}
	e.svc = New(Options{Reg: reg, Config: cfg, Resolver: e.dns, Now: func() time.Time { return e.now }})
	return e
}

func kindOf(t *testing.T, err error) Kind {
	t.Helper()
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("error %v is not a domains.Error", err)
	}
	return e.Kind
}

func TestValidateHostname(t *testing.T) {
	cfg := &config.Config{Domain: "example.com"}
	ok := map[string]string{
		"app.example.org":       "app.example.org",
		"  Docs.Example.ORG. ":  "docs.example.org",
		"example.com":           "example.com",
		"www.example.com":       "www.example.com", // the node's domain is the operator's; only the node's own hosts are reserved
		"a-b.c.example.co.uk":   "a-b.c.example.co.uk",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"app.example.test":      "app.example.test",
	}
	for in, want := range ok {
		got, err := ValidateHostname(cfg, in)
		if err != nil || got != want {
			t.Errorf("ValidateHostname(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "   ", "localhost", "single", "10.0.0.1", "::1", "*.example.org", "a_b.example.org", "-a.example.org",
		"a-.example.org", "app..example.org", "app.example.123", "foo.localhost", "svc.internal", "x.local",
		strings.Repeat("a", 64) + ".example.org", strings.Repeat("a.", 130) + "org",
		"api.example.com", "studio.example.com", "pooler.example.com", "x.api.example.com",
		refA + ".api.example.com", "db." + refA + ".api.example.com", "a.studio.example.com", "https://app.example.org", "app.example.org/path", "app.example.org:443",
	}
	for _, in := range bad {
		if got, err := ValidateHostname(cfg, in); err == nil {
			t.Errorf("ValidateHostname(%q) = %q, want an error", in, got)
		} else if kindOf(t, err) != KindInvalid {
			t.Errorf("ValidateHostname(%q): kind %v", in, kindOf(t, err))
		}
	}
	// Without a domain the node lives under sslip.io, which the operator does not own.
	sslip := &config.Config{PublicIP: "203.0.113.7"}
	for _, in := range []string{"203.0.113.7.sslip.io", "x.203.0.113.7.sslip.io", "api.203.0.113.7.sslip.io"} {
		if _, err := ValidateHostname(sslip, in); err == nil {
			t.Errorf("sslip node accepted %q", in)
		}
	}
	if _, err := ValidateHostname(sslip, "app.example.org"); err != nil {
		t.Errorf("sslip node refused a real domain: %v", err)
	}
}

func TestValidateVanityName(t *testing.T) {
	for in, want := range map[string]string{"acme": "acme", "Acme-Prod": "acme-prod", "a": "a", "my2app": "my2app", strings.Repeat("a", 63): strings.Repeat("a", 63)} {
		if got, err := ValidateVanityName(in); err != nil || got != want {
			t.Errorf("ValidateVanityName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", " ", "-a", "a-", "a.b", "a_b", "a b", strings.Repeat("a", 64), "xn--abc", "api", "studio", "POOLER", "supabase", "system", refA, "abcdefghijklmnopqrst"} {
		if got, err := ValidateVanityName(in); err == nil {
			t.Errorf("ValidateVanityName(%q) = %q, want an error", in, got)
		}
	}
	// A twenty-letter name with a digit or a hyphen is not ref-shaped.
	if _, err := ValidateVanityName("abcdefghijklmnopqrs1"); err != nil {
		t.Error(err)
	}
}

func TestCustomHostnameFlow(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	if _, err := e.svc.Hostname(ctx, refA); kindOf(t, err) != KindNotConfigured || !strings.Contains(err.Error(), "custom hostname configuration") {
		t.Fatalf("no hostname yet: %v", err)
	}

	st, err := e.svc.Initialize(ctx, refA, "Docs.Example.org.")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != registry.HostnameInitiated || st.Hostname != "docs.example.org" || !strings.HasPrefix(st.TXTValue, "supavise-verify=") ||
		st.TXTName != "_supavise-challenge.docs.example.org" || st.Target != refA+".api.example.com" {
		t.Fatalf("initialize: %+v", st)
	}
	if len(st.Errors) != 2 || st.Errors[0] != CNAMEMismatch {
		t.Errorf("errors before any check: %v", st.Errors)
	}
	token := st.TXTValue
	// Running initialize again keeps the token the user may have published.
	if again, err := e.svc.Initialize(ctx, refA, "docs.example.org"); err != nil || again.TXTValue != token {
		t.Fatalf("second initialize: %+v %v", again, err)
	}

	// Nothing in DNS: still initiated.
	if st, err = e.svc.Reverify(ctx, refA); err != nil || st.Status != registry.HostnameInitiated {
		t.Fatalf("reverify without DNS: %+v %v", st, err)
	}
	// Activation before verification is refused.
	if _, _, err := e.svc.Activate(ctx, refA); kindOf(t, err) != KindState {
		t.Fatalf("activate before verify: %v", err)
	}

	// A TXT record with the wrong token proves nothing.
	e.dns.txt["_supavise-challenge.docs.example.org"] = []string{"supavise-verify=deadbeef"}
	e.now = e.now.Add(time.Minute)
	if st, _ = e.svc.Reverify(ctx, refA); st.Status != registry.HostnameInitiated {
		t.Fatalf("wrong token: %+v", st)
	}

	// The right TXT record, hostname elsewhere: challenge verified, origin missing.
	e.dns.txt["_supavise-challenge.docs.example.org"] = []string{"unrelated", `"` + token + `"`}
	e.dns.host["docs.example.org"] = []string{"198.51.100.9"}
	e.now = e.now.Add(time.Minute)
	if st, _ = e.svc.Reverify(ctx, refA); st.Status != registry.HostnameChallengeVerified || !st.TXTOK || st.CNAMEOK || len(st.Errors) != 1 || st.Errors[0] != CNAMEMismatch {
		t.Fatalf("txt only: %+v", st)
	}

	// CNAME to the project's host verifies the origin.
	e.dns.cname["docs.example.org"] = refA + ".api.example.com"
	e.dns.host[refA+".api.example.com"] = []string{"203.0.113.7"}
	e.now = e.now.Add(time.Minute)
	if st, _ = e.svc.Reverify(ctx, refA); st.Status != registry.HostnameOriginReady || st.VerifiedAt == nil || len(st.Errors) != 0 {
		t.Fatalf("cname: %+v", st)
	}

	// The DNS moves away again: the claim goes back, and it cannot be activated.
	delete(e.dns.cname, "docs.example.org")
	e.now = e.now.Add(time.Minute)
	if st, _ = e.svc.Reverify(ctx, refA); st.Status != registry.HostnameChallengeVerified {
		t.Fatalf("regress: %+v", st)
	}

	// An A record to the node's address works as well as a CNAME.
	e.dns.host["docs.example.org"] = []string{"203.0.113.7"}
	e.now = e.now.Add(time.Minute)
	if st, _ = e.svc.Reverify(ctx, refA); st.Status != registry.HostnameOriginReady {
		t.Fatalf("A record: %+v", st)
	}

	st, changed, err := e.svc.Activate(ctx, refA)
	if err != nil || !changed || st.Status != registry.HostnameActive || st.ActivatedAt == nil {
		t.Fatalf("activate: %+v %v %v", st, changed, err)
	}
	routes, _ := e.reg.ListRoutes(ctx)
	var found bool
	for _, r := range routes {
		if r.Host == "docs.example.org" {
			found = r.Ref == refA && r.Kind == registry.RouteCustom
		}
	}
	if !found {
		t.Fatalf("no custom route after activation: %+v", routes)
	}
	if host, err := ExternalHost(ctx, e.reg, e.cfg, refA); err != nil || host != "docs.example.org" {
		t.Fatalf("external host: %q %v", host, err)
	}
	// Activating twice is a no-op; reverify of an active hostname does not touch DNS.
	if _, changed, err := e.svc.Activate(ctx, refA); err != nil || changed {
		t.Fatalf("second activate: %v %v", changed, err)
	}
	calls := e.dns.calls
	if st, err := e.svc.Reverify(ctx, refA); err != nil || st.Status != registry.HostnameActive || e.dns.calls != calls {
		t.Fatalf("reverify active: %+v %v", st, err)
	}
	// A new hostname needs the active one deleted first.
	if _, err := e.svc.Initialize(ctx, refA, "other.example.org"); kindOf(t, err) != KindState {
		t.Fatalf("initialize over an active hostname: %v", err)
	}

	// Another project cannot take an active hostname.
	if _, err := e.svc.Initialize(ctx, refB, "docs.example.org"); kindOf(t, err) != KindConflict {
		t.Fatalf("hostname held by another project: %v", err)
	}

	was, err := e.svc.Delete(ctx, refA)
	if err != nil || !was {
		t.Fatalf("delete: %v %v", was, err)
	}
	routes, _ = e.reg.ListRoutes(ctx)
	for _, r := range routes {
		if r.Host == "docs.example.org" {
			t.Fatalf("route survived the delete: %+v", r)
		}
	}
	if host, _ := ExternalHost(ctx, e.reg, e.cfg, refA); host != "" {
		t.Fatalf("external host after delete: %q", host)
	}
	if _, err := e.svc.Delete(ctx, refA); kindOf(t, err) != KindNotConfigured {
		t.Fatalf("second delete: %v", err)
	}
	// Now project B may take it.
	if _, err := e.svc.Initialize(ctx, refB, "docs.example.org"); err != nil {
		t.Fatal(err)
	}
}

// Anyone may claim a hostname they do not control, but the claim never gets past the
// ownership check, and it does not block the real owner.
func TestHostnameSquatting(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	a, err := e.svc.Initialize(ctx, refA, "shop.example.org")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.Initialize(ctx, refB, "shop.example.org")
	if err != nil {
		t.Fatalf("a pending claim must not lock the hostname: %v", err)
	}
	if a.TXTValue == b.TXTValue {
		t.Fatal("two claims share a token")
	}
	e.dns.host["shop.example.org"] = []string{"203.0.113.7"}
	// The domain's real owner published B's token only.
	e.dns.txt["_supavise-challenge.shop.example.org"] = []string{b.TXTValue}
	if st, _ := e.svc.Reverify(ctx, refA); st.Status == registry.HostnameOriginReady {
		t.Fatalf("A verified with B's token: %+v", st)
	}
	if st, err := e.svc.Reverify(ctx, refB); err != nil || st.Status != registry.HostnameOriginReady {
		t.Fatalf("B did not verify: %+v %v", st, err)
	}
	// B holds it now; A, which published nothing, cannot get there even with the token.
	e.dns.txt["_supavise-challenge.shop.example.org"] = []string{b.TXTValue, a.TXTValue}
	e.now = e.now.Add(time.Minute)
	if _, err := e.svc.Reverify(ctx, refA); kindOf(t, err) != KindConflict {
		t.Fatalf("A verifying a hostname B holds: %v", err)
	}
}

func TestVerificationRateLimit(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if _, err := e.svc.Initialize(ctx, refA, "docs.example.org"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := e.svc.Reverify(ctx, refA); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := e.svc.Reverify(ctx, refA)
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindRateLimited || de.RetryAfter <= 0 || de.RetryAfter > 15*time.Second {
		t.Fatalf("sixth attempt: %v", err)
	}
	calls := e.dns.calls
	_, _ = e.svc.Reverify(ctx, refA)
	if e.dns.calls != calls {
		t.Fatal("a refused attempt still queried DNS")
	}
	// Another project has its own allowance.
	if _, err := e.svc.Initialize(ctx, refB, "docs2.example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Reverify(ctx, refB); err != nil {
		t.Fatalf("project B throttled by A: %v", err)
	}
	// One attempt comes back after the refill time.
	e.now = e.now.Add(15 * time.Second)
	if _, err := e.svc.Reverify(ctx, refA); err != nil {
		t.Fatalf("after the refill: %v", err)
	}
	if _, err := e.svc.Reverify(ctx, refA); kindOf(t, err) != KindRateLimited {
		t.Fatalf("only one attempt should have come back: %v", err)
	}
}

func TestNodeWideRateLimit(t *testing.T) {
	e := newEnv(t, nil)
	e.svc = New(Options{Reg: e.reg, Config: e.cfg, Resolver: e.dns, Now: func() time.Time { return e.now }, Attempts: 1, Refill: time.Hour})
	// One attempt per project, ten for the node: eleven projects cannot all verify at once.
	ctx := context.Background()
	org, _ := e.reg.GetOrganization(ctx, "acme")
	limited := 0
	for i := 0; i < 12; i++ {
		ref := strings.Repeat(string(rune('c'+i)), 20)
		if err := e.reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: org.ID, Name: ref, Engine: registry.EnginePostgres, Status: registry.StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Initialize(ctx, ref, "h"+ref[:1]+".example.org"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Reverify(ctx, ref); err != nil {
			if kindOf(t, err) != KindRateLimited {
				t.Fatal(err)
			}
			limited++
		}
	}
	if limited != 2 {
		t.Fatalf("%d of 12 attempts limited, want 2", limited)
	}
}

func TestInitializeRefusals(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	for _, h := range []string{"", "api.example.com", "10.1.2.3", "x"} {
		if _, err := e.svc.Initialize(ctx, refA, h); kindOf(t, err) != KindInvalid {
			t.Errorf("Initialize(%q): %v", h, err)
		}
	}
	if _, err := e.svc.Initialize(ctx, config.SystemRef, "docs.example.org"); kindOf(t, err) != KindInvalid {
		t.Errorf("system project: %v", err)
	}
	if _, err := e.svc.Initialize(ctx, "zzzzzzzzzzzzzzzzzzzz", "docs.example.org"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	dns01 := newEnv(t, func(c *config.Config) { c.TLS.Mode = "dns01" })
	if _, err := dns01.svc.Initialize(ctx, refA, "docs.example.org"); kindOf(t, err) != KindUnavailable {
		t.Errorf("dns01 node: %v", err)
	}
}

func TestVanity(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	if v, err := e.svc.Vanity(ctx, refA); err != nil || v.Status != VanityNotUsed {
		t.Fatalf("initial: %+v %v", v, err)
	}
	for name, want := range map[string]bool{"acme": true, "api": false, "studio": false, refA: false} {
		if got, err := e.svc.CheckVanity(ctx, refA, name); err != nil || got != want {
			t.Errorf("CheckVanity(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := e.svc.CheckVanity(ctx, refA, "not valid"); kindOf(t, err) != KindInvalid {
		t.Errorf("malformed name: %v", err)
	}

	host, err := e.svc.ActivateVanity(ctx, refA, "Acme")
	if err != nil || host != "acme.api.example.com" {
		t.Fatalf("activate: %q %v", host, err)
	}
	if v, _ := e.svc.Vanity(ctx, refA); v.Status != VanityActive || v.CustomDomain != host {
		t.Fatalf("after activate: %+v", v)
	}
	routes, _ := e.reg.ListRoutes(ctx)
	if len(routes) != 1 || routes[0].Host != host || routes[0].Kind != registry.RouteVanity || routes[0].Ref != refA {
		t.Fatalf("routes: %+v", routes)
	}
	if h, _ := ExternalHost(ctx, e.reg, e.cfg, refA); h != host {
		t.Fatalf("external host: %q", h)
	}
	// Unique across the node; the owner still sees it as available.
	if got, _ := e.svc.CheckVanity(ctx, refB, "acme"); got {
		t.Error("a taken name reported available")
	}
	if got, _ := e.svc.CheckVanity(ctx, refA, "acme"); !got {
		t.Error("the owner's own name reported unavailable")
	}
	if _, err := e.svc.ActivateVanity(ctx, refB, "acme"); kindOf(t, err) != KindConflict {
		t.Fatalf("taking a name: %v", err)
	}
	for _, bad := range []string{"api", "studio", "x.y", refA, ""} {
		if _, err := e.svc.ActivateVanity(ctx, refB, bad); kindOf(t, err) != KindInvalid {
			t.Errorf("ActivateVanity(%q): %v", bad, err)
		}
	}
	// Changing the name moves the route.
	if host, err = e.svc.ActivateVanity(ctx, refA, "acme-two"); err != nil {
		t.Fatal(err)
	}
	if routes, _ = e.reg.ListRoutes(ctx); len(routes) != 1 || routes[0].Host != "acme-two.api.example.com" {
		t.Fatalf("routes after rename: %+v", routes)
	}
	if got, _ := e.svc.CheckVanity(ctx, refB, "acme"); !got {
		t.Error("the released name is not available")
	}

	// A custom domain and a vanity subdomain are mutually exclusive, as on hosted.
	e.dns.host["docs.example.org"] = []string{"203.0.113.7"}
	st, _ := e.svc.Initialize(ctx, refA, "docs.example.org")
	e.dns.txt[st.TXTName] = []string{st.TXTValue}
	if _, err := e.svc.Reverify(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Activate(ctx, refA); kindOf(t, err) != KindState || !strings.Contains(err.Error(), "vanity subdomain") {
		t.Fatalf("activating a custom hostname next to a vanity subdomain: %v", err)
	}
	if err := e.svc.DeleteVanity(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Activate(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ActivateVanity(ctx, refA, "acme"); kindOf(t, err) != KindState || !strings.Contains(err.Error(), "custom domain") {
		t.Fatalf("activating a vanity subdomain next to a custom hostname: %v", err)
	}
	if v, _ := e.svc.Vanity(ctx, refA); v.Status != VanityCustomDomainUsed || v.CustomDomain != "docs.example.org" {
		t.Fatalf("with a custom domain: %+v", v)
	}
	if _, err := e.svc.Delete(ctx, refA); err != nil {
		t.Fatal(err)
	}
	// Both rows can exist only through the store directly; the custom hostname would win.
	if err := e.reg.PutVanitySubdomain(ctx, refA, "acme-two", "acme-two.api.example.com"); err != nil {
		t.Fatal(err)
	}
	if h, _ := ExternalHost(ctx, e.reg, e.cfg, refA); h != "acme-two.api.example.com" {
		t.Fatalf("external host falls back to the vanity host: %q", h)
	}

	if err := e.svc.DeleteVanity(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if routes, _ = e.reg.ListRoutes(ctx); len(routes) != 0 {
		t.Fatalf("routes after delete: %+v", routes)
	}
	if err := e.svc.DeleteVanity(ctx, refA); kindOf(t, err) != KindNotConfigured {
		t.Fatalf("second delete: %v", err)
	}
	if h, _ := ExternalHost(ctx, e.reg, e.cfg, refA); h != "" {
		t.Fatalf("external host after delete: %q", h)
	}
}

// Deleting a project removes its domain rows and routes.
func TestProjectDeleteCleansUp(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if _, err := e.svc.ActivateVanity(ctx, refA, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Initialize(ctx, refA, "docs.example.org"); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.DeleteProject(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if rs, _ := e.reg.ListRoutes(ctx); len(rs) != 0 {
		t.Fatalf("routes left: %+v", rs)
	}
	if hs, _ := e.reg.ListCustomHostnames(ctx); len(hs) != 0 {
		t.Fatalf("hostnames left: %+v", hs)
	}
	if got, _ := e.svc.CheckVanity(ctx, refB, "acme"); !got {
		t.Fatal("the vanity name was not released")
	}
}

// verified claims ref's host and makes the fake DNS prove it, leaving the claim at status 4.
func (e *env) verified(t *testing.T, ref, host string) *State {
	t.Helper()
	ctx := context.Background()
	st, err := e.svc.Initialize(ctx, ref, host)
	if err != nil {
		t.Fatal(err)
	}
	e.dns.txt[st.TXTName] = []string{st.TXTValue}
	e.dns.host[host] = []string{"203.0.113.7"}
	e.now = e.now.Add(time.Minute)
	if st, err = e.svc.Reverify(ctx, ref); err != nil || st.Status != registry.HostnameOriginReady {
		t.Fatalf("verify %s: %+v %v", host, st, err)
	}
	return st
}

// Activation looks at the DNS again: a claim verified earlier whose records are gone is not
// activated, and drops back to what the records now show.
func TestActivateChecksDNSAgain(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	st := e.verified(t, refA, "docs.example.org")

	delete(e.dns.txt, st.TXTName)
	e.now = e.now.Add(time.Minute)
	if _, _, err := e.svc.Activate(ctx, refA); kindOf(t, err) != KindState {
		t.Fatalf("activate with the TXT record gone: %v", err)
	}
	if got, _ := e.svc.Hostname(ctx, refA); got.Status != registry.HostnameInitiated {
		t.Fatalf("status after the failed activation: %+v", got)
	}
	if routes, _ := e.reg.ListRoutes(ctx); len(routes) != 0 {
		t.Fatalf("routed: %+v", routes)
	}

	// The CNAME/A record moved away.
	e.dns.txt[st.TXTName] = []string{st.TXTValue}
	e.now = e.now.Add(time.Minute)
	if got, err := e.svc.Reverify(ctx, refA); err != nil || got.Status != registry.HostnameOriginReady {
		t.Fatalf("verify again: %+v %v", got, err)
	}
	e.dns.host["docs.example.org"] = []string{"198.51.100.9"}
	e.now = e.now.Add(time.Minute)
	if _, _, err := e.svc.Activate(ctx, refA); kindOf(t, err) != KindState {
		t.Fatalf("activate with the hostname elsewhere: %v", err)
	}
	if got, _ := e.svc.Hostname(ctx, refA); got.Status != registry.HostnameChallengeVerified {
		t.Fatalf("status: %+v", got)
	}

	// Back in place: it activates.
	e.dns.host["docs.example.org"] = []string{"203.0.113.7"}
	e.now = e.now.Add(time.Minute)
	if _, err := e.svc.Reverify(ctx, refA); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := e.svc.Activate(ctx, refA); err != nil || !changed {
		t.Fatalf("activate: %v %v", changed, err)
	}

	// The check spends a verification attempt like Reverify does.
	e2 := newEnv(t, nil)
	e2.verified(t, refA, "docs.example.org")
	for i := 0; i < 4; i++ { // the attempt of verified() and these four use the burst of five
		if _, err := e2.svc.Reverify(ctx, refA); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	e2.dns.calls = 0
	if _, _, err := e2.svc.Activate(ctx, refA); kindOf(t, err) != KindRateLimited || e2.dns.calls != 0 {
		t.Fatalf("activate over the limit: %v (%d lookups)", err, e2.dns.calls)
	}
}

// A verified claim that nobody activates or verifies again stops holding the hostname after the
// claim TTL, so a former DNS controller cannot park a name for good.
func TestStaleVerifiedClaimReleasesHostname(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.verified(t, refA, "shop.example.org")

	if _, err := e.svc.Initialize(ctx, refB, "shop.example.org"); kindOf(t, err) != KindConflict {
		t.Fatalf("claiming a freshly verified hostname: %v", err)
	}
	e.now = e.now.Add(23 * time.Hour)
	if _, err := e.svc.Initialize(ctx, refB, "shop.example.org"); kindOf(t, err) != KindConflict {
		t.Fatalf("claiming it inside the TTL: %v", err)
	}

	// B, whose claim is pending, proves the name after the TTL: A's stale hold goes.
	e.now = e.now.Add(2 * time.Hour)
	b, err := e.svc.Initialize(ctx, refB, "shop.example.org")
	if err != nil {
		t.Fatalf("claiming it after the TTL: %v", err)
	}
	if a, _ := e.svc.Hostname(ctx, refA); a.Status != registry.HostnameInitiated || a.TXTOK || a.CNAMEOK {
		t.Fatalf("A's claim after the release: %+v", a)
	}
	e.dns.txt[b.TXTName] = []string{b.TXTValue}
	if st, err := e.svc.Reverify(ctx, refB); err != nil || st.Status != registry.HostnameOriginReady {
		t.Fatalf("B verifying: %+v %v", st, err)
	}

	// The same through Reverify: B pending first, A verified, A stale, B verifies.
	e2 := newEnv(t, nil)
	e2.verified(t, refA, "shop.example.org")
	b2, err := e2.svc.Initialize(ctx, refB, "shop.example.org")
	if err == nil {
		t.Fatalf("B claimed a held name: %+v", b2)
	}
	// A's last proof is renewed by each successful verification.
	e2.now = e2.now.Add(20 * time.Hour)
	if _, err := e2.svc.Reverify(ctx, refA); err != nil {
		t.Fatal(err)
	}
	e2.now = e2.now.Add(20 * time.Hour)
	if _, err := e2.svc.Initialize(ctx, refB, "shop.example.org"); kindOf(t, err) != KindConflict {
		t.Fatalf("a renewed claim lost its hold: %v", err)
	}
	// An active hostname never expires.
	if _, _, err := e2.svc.Activate(ctx, refA); err != nil {
		t.Fatal(err)
	}
	e2.now = e2.now.Add(30 * 24 * time.Hour)
	if _, err := e2.svc.Initialize(ctx, refB, "shop.example.org"); kindOf(t, err) != KindConflict {
		t.Fatalf("claiming an active hostname: %v", err)
	}
}

// Activations that order certificates are limited per project and per node, and the limit comes back
// when the window has passed.
func TestIssuanceLimits(t *testing.T) {
	ctx := context.Background()
	rebuild := func(e *env, o Options) {
		o.Reg, o.Config, o.Resolver = e.reg, e.cfg, e.dns
		o.Now = func() time.Time { return e.now }
		e.svc = New(o)
	}

	t.Run("custom hostname per project", func(t *testing.T) {
		e := newEnv(t, nil)
		rebuild(e, Options{IssuancePerProject: 2, IssuanceProjectWindow: time.Hour, Attempts: 100})
		for i := 0; i < 2; i++ {
			e.verified(t, refA, "docs.example.org")
			if _, _, err := e.svc.Activate(ctx, refA); err != nil {
				t.Fatalf("activation %d: %v", i, err)
			}
			if _, err := e.svc.Delete(ctx, refA); err != nil {
				t.Fatal(err)
			}
		}
		e.verified(t, refA, "docs.example.org")
		_, _, err := e.svc.Activate(ctx, refA)
		var de *Error
		if !errors.As(err, &de) || de.Kind != KindRateLimited || de.RetryAfter <= 0 || de.RetryAfter > time.Hour {
			t.Fatalf("third activation: %v", err)
		}
		if routes, _ := e.reg.ListRoutes(ctx); len(routes) != 0 {
			t.Fatalf("a refused activation was routed: %+v", routes)
		}
		// Activating an active hostname again is free.
		e.now = e.now.Add(time.Hour + time.Minute)
		if _, _, err := e.svc.Activate(ctx, refA); err != nil {
			t.Fatalf("after the window: %v", err)
		}
		if _, changed, err := e.svc.Activate(ctx, refA); err != nil || changed {
			t.Fatalf("repeat: %v %v", changed, err)
		}
	})

	t.Run("vanity per project", func(t *testing.T) {
		e := newEnv(t, nil)
		rebuild(e, Options{IssuancePerProject: 2, IssuanceProjectWindow: time.Hour})
		for _, n := range []string{"one", "two"} {
			if _, err := e.svc.ActivateVanity(ctx, refA, n); err != nil {
				t.Fatal(err)
			}
		}
		// The name the project has costs nothing; a taken name is refused as taken, not as a limit.
		if _, err := e.svc.ActivateVanity(ctx, refA, "two"); err != nil {
			t.Fatalf("same name again: %v", err)
		}
		if _, err := e.svc.ActivateVanity(ctx, refB, "two"); kindOf(t, err) != KindConflict {
			t.Fatalf("taken name: %v", err)
		}
		if _, err := e.svc.ActivateVanity(ctx, refA, "three"); kindOf(t, err) != KindRateLimited {
			t.Fatalf("third name: %v", err)
		}
		if v, _ := e.svc.Vanity(ctx, refA); v.Name != "two" {
			t.Fatalf("a refused change was applied: %+v", v)
		}
		e.now = e.now.Add(61 * time.Minute)
		if _, err := e.svc.ActivateVanity(ctx, refA, "three"); err != nil {
			t.Fatalf("after the window: %v", err)
		}
	})

	t.Run("node", func(t *testing.T) {
		e := newEnv(t, nil)
		rebuild(e, Options{IssuancePerProject: 10, IssuancePerNode: 3, IssuanceNodeWindow: 24 * time.Hour})
		for _, c := range []struct{ ref, name string }{{refA, "a1"}, {refA, "a2"}, {refB, "b1"}} {
			if _, err := e.svc.ActivateVanity(ctx, c.ref, c.name); err != nil {
				t.Fatal(err)
			}
		}
		_, err := e.svc.ActivateVanity(ctx, refB, "b2")
		var de *Error
		if !errors.As(err, &de) || de.Kind != KindRateLimited || !strings.Contains(de.Msg, "node") {
			t.Fatalf("fourth change on the node: %v", err)
		}
	})

	t.Run("a wildcard certificate covers vanity names", func(t *testing.T) {
		e := newEnv(t, func(c *config.Config) { c.TLS.DNSProvider = "route53" })
		rebuild(e, Options{IssuancePerProject: 10, IssuancePerNode: 1})
		for _, c := range []struct{ ref, name string }{{refA, "a1"}, {refA, "a2"}, {refB, "b1"}} {
			if _, err := e.svc.ActivateVanity(ctx, c.ref, c.name); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		}
	})
}
