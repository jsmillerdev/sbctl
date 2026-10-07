package api

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/domains"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// dnsStub is the node's resolver in these tests: fixed records, NXDOMAIN for everything else.
type dnsStub struct {
	txt  map[string][]string
	host map[string][]string
}

func (d *dnsStub) LookupCNAME(_ context.Context, h string) (string, error) {
	if _, ok := d.host[h]; ok {
		return h + ".", nil
	}
	return "", &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (d *dnsStub) LookupTXT(_ context.Context, n string) ([]string, error) {
	if v, ok := d.txt[n]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (d *dnsStub) LookupHost(_ context.Context, h string) ([]string, error) {
	if v, ok := d.host[h]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (f *fixture) withDNS() *dnsStub {
	f.t.Helper()
	dns := &dnsStub{txt: map[string][]string{}, host: map[string][]string{}}
	f.cfg.PublicIP = "203.0.113.7"
	f.srv.domains = domains.New(domains.Options{Reg: f.reg, Config: f.cfg, Resolver: dns})
	return dns
}

func (f *fixture) routeFor(host string) *registry.Route {
	rs, _ := f.reg.ListRoutes(context.Background())
	for _, r := range rs {
		if r.Host == host {
			return &r
		}
	}
	return nil
}

func mapAt(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("at %q: %v is not an object", p, v)
		}
		v = m[p]
	}
	return v
}

func TestCustomHostnameAPI(t *testing.T) {
	f := newFixture(t)
	dns := f.withDNS()
	const p = "/v1/projects/" + testRef + "/custom-hostname"

	// Nothing configured: Studio keys on this message to show the "configure hostname" step.
	rec := f.do("GET", p, nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "custom hostname configuration") {
		t.Fatalf("GET before initialize: %d %s", rec.Code, rec.Body)
	}

	for _, bad := range []string{"", "api.example.test", "10.0.0.1", "not a host"} {
		if rec := f.do("POST", p+"/initialize", map[string]any{"custom_hostname": bad}); rec.Code != 400 {
			t.Errorf("initialize %q: %d %s", bad, rec.Code, rec.Body)
		}
	}

	rec = f.do("POST", p+"/initialize", map[string]any{"custom_hostname": "Docs.Example.org"})
	if rec.Code != 201 {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, keyHostnameNew, rec.Body.Bytes())
	body := decodeBody(t, rec)
	if mapAt(t, body, "status") != "1_not_started" && mapAt(t, body, "status") != "2_initiated" {
		t.Fatalf("status: %v", body)
	}
	res := mapAt(t, body, "data", "result")
	if mapAt(t, res, "hostname") != "docs.example.org" || mapAt(t, res, "custom_origin_server") != testRef+".api.example.test" {
		t.Fatalf("result: %v", res)
	}
	txtName, txtValue := mapAt(t, res, "ssl", "txt_name").(string), mapAt(t, res, "ssl", "txt_value").(string)
	if txtName != "_supavise-challenge.docs.example.org" || !strings.HasPrefix(txtValue, "supavise-verify=") || mapAt(t, res, "ssl", "status") != "pending_validation" {
		t.Fatalf("records: %v", res)
	}
	if errs, _ := mapAt(t, res, "verification_errors").([]any); len(errs) == 0 || errs[0] != domains.CNAMEMismatch {
		t.Fatalf("verification_errors: %v", errs)
	}
	if mapAt(t, res, "ownership_verification", "type") != "txt" || mapAt(t, res, "ownership_verification", "value") != txtValue {
		t.Fatalf("ownership_verification: %v", res)
	}

	if rec := f.do("POST", p+"/activate", nil); rec.Code != 409 {
		t.Fatalf("activate before verification: %d %s", rec.Code, rec.Body)
	}
	rec = f.do("POST", p+"/reverify", nil)
	if rec.Code != 201 || mapAt(t, decodeBody(t, rec), "status") != "2_initiated" {
		t.Fatalf("reverify without DNS: %d %s", rec.Code, rec.Body)
	}

	dns.txt[txtName] = []string{txtValue}
	dns.host["docs.example.org"] = []string{"203.0.113.7"}
	rec = f.do("POST", p+"/reverify", nil)
	if rec.Code != 201 || mapAt(t, decodeBody(t, rec), "status") != "4_origin_setup_completed" {
		t.Fatalf("reverify with DNS: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, keyHostnameVer, rec.Body.Bytes())
	if f.routeFor("docs.example.org") != nil {
		t.Fatal("routed before activation")
	}

	rec = f.do("POST", p+"/activate", nil)
	if rec.Code != 201 || mapAt(t, decodeBody(t, rec), "status") != "5_services_reconfigured" {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, keyHostnameAct, rec.Body.Bytes())
	if rt := f.routeFor("docs.example.org"); rt == nil || rt.Ref != testRef || rt.Kind != registry.RouteCustom {
		t.Fatalf("route: %+v", rt)
	}
	// GoTrue is restarted so that it renders the new external URL.
	if got := f.mgr.applied; len(got) != 1 || got[0] != testRef+" auth" {
		t.Fatalf("applied: %v", got)
	}
	rec = f.do("GET", p, nil)
	if rec.Code != 200 || mapAt(t, decodeBody(t, rec), "status") != "5_services_reconfigured" {
		t.Fatalf("GET after activate: %d %s", rec.Code, rec.Body)
	}
	validateAgainstSpec(t, keyHostnameGet, rec.Body.Bytes())

	// Another project cannot take an active hostname.
	f.mgr.addProject(t, secondRef, "Second", f.org.ID, registry.StatusActiveHealthy)
	if rec := f.do("POST", "/v1/projects/"+secondRef+"/custom-hostname/initialize", map[string]any{"custom_hostname": "docs.example.org"}); rec.Code != 409 {
		t.Fatalf("hostname held by another project: %d %s", rec.Code, rec.Body)
	}
	// And the project cannot set up a second one while the first is active.
	if rec := f.do("POST", p+"/initialize", map[string]any{"custom_hostname": "other.example.org"}); rec.Code != 409 {
		t.Fatalf("second hostname: %d %s", rec.Code, rec.Body)
	}

	rec = f.do("DELETE", p, nil)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if f.routeFor("docs.example.org") != nil {
		t.Fatal("route survived the delete")
	}
	if got := f.mgr.applied; len(got) != 2 {
		t.Fatalf("Auth not reconfigured after the delete: %v", got)
	}
	if rec := f.do("GET", p, nil); rec.Code != 400 {
		t.Fatalf("GET after delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("DELETE", p, nil); rec.Code != 400 {
		t.Fatalf("second delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/v1/projects/zzzzzzzzzzzzzzzzzzzz/custom-hostname/initialize", map[string]any{"custom_hostname": "a.example.org"}); rec.Code != 404 {
		t.Fatalf("unknown project: %d", rec.Code)
	}
}

// A failed restart of GoTrue leaves the domain saved; activating again retries it.
func TestCustomHostnameActivateRetriesAuth(t *testing.T) {
	f := newFixture(t)
	dns := f.withDNS()
	const p = "/v1/projects/" + testRef + "/custom-hostname"
	st, err := f.srv.domains.Initialize(context.Background(), testRef, "docs.example.org")
	if err != nil {
		t.Fatal(err)
	}
	dns.txt[st.TXTName] = []string{st.TXTValue}
	dns.host["docs.example.org"] = []string{"203.0.113.7"}
	if rec := f.do("POST", p+"/reverify", nil); rec.Code != 201 {
		t.Fatalf("reverify: %d %s", rec.Code, rec.Body)
	}
	fail := true
	f.mgr.applyHook = func(_ projectconfig.Service) error {
		if fail {
			return context.DeadlineExceeded
		}
		return nil
	}
	if rec := f.do("POST", p+"/activate", nil); rec.Code != 500 || !strings.Contains(rec.Body.String(), "repeat the request") {
		t.Fatalf("activate with a failing Auth restart: %d %s", rec.Code, rec.Body)
	}
	if f.routeFor("docs.example.org") == nil {
		t.Fatal("the domain should stay saved")
	}
	fail = false
	if rec := f.do("POST", p+"/activate", nil); rec.Code != 201 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if len(f.mgr.applied) != 2 {
		t.Fatalf("applied: %v", f.mgr.applied)
	}
}

func TestCustomHostnameVerifyRateLimit(t *testing.T) {
	f := newFixture(t)
	f.withDNS()
	const p = "/v1/projects/" + testRef + "/custom-hostname"
	if rec := f.do("POST", p+"/initialize", map[string]any{"custom_hostname": "docs.example.org"}); rec.Code != 201 {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	for i := 0; i < 5; i++ {
		if rec := f.do("POST", p+"/reverify", nil); rec.Code != 201 {
			t.Fatalf("attempt %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := f.do("POST", p+"/reverify", nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("sixth attempt: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestVanitySubdomainAPI(t *testing.T) {
	f := newFixture(t)
	f.withDNS()
	const p = "/v1/projects/" + testRef + "/vanity-subdomain"
	f.mgr.addProject(t, secondRef, "Second", f.org.ID, registry.StatusActiveHealthy)

	rec := f.do("GET", p, nil)
	if rec.Code != 200 || mapAt(t, decodeBody(t, rec), "status") != "not-used" {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	check := func(ref, name string) (int, any) {
		rec := f.do("POST", "/v1/projects/"+ref+"/vanity-subdomain/check-availability", map[string]any{"vanity_subdomain": name})
		if rec.Code != 201 {
			return rec.Code, nil
		}
		return rec.Code, mapAt(t, decodeBody(t, rec), "available")
	}
	if code, av := check(testRef, "acme"); code != 201 || av != true {
		t.Fatalf("check acme: %d %v", code, av)
	}
	if code, av := check(testRef, "api"); code != 201 || av != false {
		t.Fatalf("check api: %d %v", code, av)
	}
	if code, _ := check(testRef, "no good"); code != 400 {
		t.Fatalf("check malformed: %d", code)
	}

	rec = f.do("POST", p+"/activate", map[string]any{"vanity_subdomain": "Acme"})
	if rec.Code != 201 || mapAt(t, decodeBody(t, rec), "custom_domain") != "acme.api.example.test" {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	if rt := f.routeFor("acme.api.example.test"); rt == nil || rt.Ref != testRef || rt.Kind != registry.RouteVanity {
		t.Fatalf("route: %+v", rt)
	}
	if len(f.mgr.applied) != 1 {
		t.Fatalf("Auth not restarted onto the vanity host: %v", f.mgr.applied)
	}
	rec = f.do("GET", p, nil)
	body := decodeBody(t, rec)
	if rec.Code != 200 || mapAt(t, body, "status") != "active" || mapAt(t, body, "custom_domain") != "acme.api.example.test" {
		t.Fatalf("GET after activate: %d %s", rec.Code, rec.Body)
	}
	if code, av := check(secondRef, "acme"); code != 201 || av != false {
		t.Fatalf("check taken name: %d %v", code, av)
	}
	if rec := f.do("POST", "/v1/projects/"+secondRef+"/vanity-subdomain/activate", map[string]any{"vanity_subdomain": "acme"}); rec.Code != 409 {
		t.Fatalf("taking a name: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"studio", "pooler", "abcdefghijklmnopqrst", "", "a.b"} {
		if rec := f.do("POST", "/v1/projects/"+secondRef+"/vanity-subdomain/activate", map[string]any{"vanity_subdomain": bad}); rec.Code != 400 {
			t.Errorf("activate %q: %d %s", bad, rec.Code, rec.Body)
		}
	}

	if rec := f.do("DELETE", p, nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if f.routeFor("acme.api.example.test") != nil {
		t.Fatal("route survived the delete")
	}
	if len(f.mgr.applied) != 2 {
		t.Fatalf("Auth not restarted back: %v", f.mgr.applied)
	}
	rec = f.do("GET", p, nil)
	if mapAt(t, decodeBody(t, rec), "status") != "not-used" {
		t.Fatalf("GET after delete: %s", rec.Body)
	}
	if rec := f.do("DELETE", p, nil); rec.Code != 400 {
		t.Fatalf("second delete: %d", rec.Code)
	}
}

// A custom domain and a vanity subdomain are mutually exclusive: the platform reports
// "custom-domain-used" and refuses a vanity subdomain while a custom domain is active.
func TestVanityWithCustomDomain(t *testing.T) {
	f := newFixture(t)
	dns := f.withDNS()
	ctx := context.Background()
	st, _ := f.srv.domains.Initialize(ctx, testRef, "docs.example.org")
	dns.txt[st.TXTName] = []string{st.TXTValue}
	dns.host["docs.example.org"] = []string{"203.0.113.7"}
	if _, err := f.srv.domains.Reverify(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	if rec := f.do("POST", "/v1/projects/"+testRef+"/custom-hostname/activate", nil); rec.Code != 201 {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	applied := len(f.mgr.applied)
	if rec := f.do("POST", "/v1/projects/"+testRef+"/vanity-subdomain/activate", map[string]any{"vanity_subdomain": "acme"}); rec.Code != 409 {
		t.Fatalf("vanity activate next to a custom domain: %d %s", rec.Code, rec.Body)
	}
	if len(f.mgr.applied) != applied {
		t.Fatalf("Auth restarted by a refused request: %v", f.mgr.applied)
	}
	rec := f.do("GET", "/v1/projects/"+testRef+"/vanity-subdomain", nil)
	body := decodeBody(t, rec)
	validateAgainstSpec(t, "GET /v1/projects/{ref}/vanity-subdomain", rec.Body.Bytes())
	if mapAt(t, body, "status") != "custom-domain-used" || mapAt(t, body, "custom_domain") != "docs.example.org" {
		t.Fatalf("GET: %s", rec.Body)
	}
}

func TestCustomDomainsAddon(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/platform/projects/" + testRef + "/billing/addons", "/v1/projects/" + testRef + "/billing/addons"} {
		rec := f.do("GET", path, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"custom_domain"`) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}
