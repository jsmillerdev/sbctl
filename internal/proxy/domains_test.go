package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/domains"
	"github.com/supavise/supavise/internal/registry"
)

// staticDNS answers the checks of a verified custom hostname: the TXT token and an A record
// at the node's address.
type staticDNS struct {
	txt   map[string][]string
	host  map[string][]string
	cname map[string]string
}

func (d *staticDNS) LookupCNAME(_ context.Context, h string) (string, error) {
	if c, ok := d.cname[h]; ok {
		return c + ".", nil
	}
	if _, ok := d.host[h]; ok {
		return h + ".", nil // no CNAME: the resolver answers with the name itself
	}
	return "", &net.DNSError{Err: "no such host", IsNotFound: true}
}
func (d *staticDNS) LookupTXT(_ context.Context, n string) ([]string, error) {
	if v, ok := d.txt[n]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}
func (d *staticDNS) LookupHost(_ context.Context, h string) ([]string, error) {
	if c, ok := d.cname[h]; ok {
		h = c
	}
	if v, ok := d.host[h]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

// activateHostname runs a hostname through initialize, reverify and activate for ref.
func activateHostname(t *testing.T, svc *domains.Service, dns *staticDNS, ref, hostname string) {
	t.Helper()
	ctx := context.Background()
	st, err := svc.Initialize(ctx, ref, hostname)
	if err != nil {
		t.Fatal(err)
	}
	dns.txt[st.TXTName] = []string{st.TXTValue}
	dns.host[st.Hostname] = []string{"203.0.113.7"}
	if st, err = svc.Reverify(ctx, ref); err != nil || st.Status != registry.HostnameOriginReady {
		t.Fatalf("reverify: %+v %v", st, err)
	}
	if _, _, err := svc.Activate(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

// TestAllowHostCustomHostnames: the certificate gate allows a custom hostname only while it
// is active, and a vanity subdomain only where the wildcard does not cover it.
func TestAllowHostCustomHostnames(t *testing.T) {
	const base = "example.com"
	const host = "docs.customer.example"
	ctx := context.Background()
	for _, tc := range []struct {
		mode               string
		provider           bool
		customOK, vanityOK bool
	}{
		{"http01", false, true, true},
		{"auto", true, true, false}, // the wildcard covers <name>.api.<domain>
		{"dns01", true, false, false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s := tlsServer(t, func(c *config.Config) {
				c.Domain, c.TLS.Mode, c.PublicIP = base, tc.mode, "203.0.113.7"
				if tc.provider {
					c.TLS.DNSProvider = "route53"
				}
			})
			dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{}}
			svc := domains.New(domains.Options{Reg: s.reg(), Config: s.cfg, Resolver: dns})
			reload := func() {
				t.Helper()
				if err := s.table.reload(ctx); err != nil {
					t.Fatal(err)
				}
			}
			deny := func(why string) {
				t.Helper()
				if err := s.allowHost(ctx, host); err == nil {
					t.Fatalf("%s: certificate allowed for %s", why, host)
				}
			}

			// Nothing claimed: refused, like any host we do not serve.
			deny("unknown host")

			// A claim that nobody verified, a verified one that is not active, and a claim whose
			// DNS proves nothing never get a certificate. (dns01 refuses to start a claim at all.)
			if tc.mode != "dns01" {
				st, err := svc.Initialize(ctx, testRef, host)
				if err != nil {
					t.Fatal(err)
				}
				reload()
				deny("claimed")
				dns.txt[st.TXTName] = []string{st.TXTValue}
				dns.host[host] = []string{"203.0.113.7"}
				if st, err = svc.Reverify(ctx, testRef); err != nil || st.Status != registry.HostnameOriginReady {
					t.Fatalf("reverify: %+v %v", st, err)
				}
				reload()
				deny("verified but not activated")
				if _, _, err := svc.Activate(ctx, testRef); err != nil {
					t.Fatal(err)
				}
			} else {
				// A route written behind the service's back on a dns01 node: still no HTTP-01.
				if err := s.reg().PutRoute(ctx, registry.Route{Host: host, Ref: testRef, Kind: registry.RouteCustom}); err != nil {
					t.Fatal(err)
				}
			}
			reload()
			if err := s.allowHost(ctx, host); (err == nil) != tc.customOK {
				t.Errorf("active custom hostname: allowed=%v, want %v (%v)", err == nil, tc.customOK, err)
			}
			if tc.customOK {
				// Case and a trailing dot do not matter; a name under it or next to it is not ours.
				for _, n := range []string{strings.ToUpper(host), host + "."} {
					if err := s.allowHost(ctx, n); err != nil {
						t.Errorf("allowHost(%q): %v", n, err)
					}
				}
				for _, n := range []string{"x." + host, "docs2.customer.example", "customer.example"} {
					if err := s.allowHost(ctx, n); err == nil {
						t.Errorf("allowHost(%q) allowed", n)
					}
				}
				// Paused project: nothing is served, so no certificate.
				if err := s.reg().SetProjectStatus(ctx, testRef, registry.StatusInactive); err != nil {
					t.Fatal(err)
				}
				reload()
				deny("paused project")
				if err := s.reg().SetProjectStatus(ctx, testRef, registry.StatusActiveHealthy); err != nil {
					t.Fatal(err)
				}
				// Deleting the hostname takes its route and with it the right to a certificate.
				if _, err := svc.Delete(ctx, testRef); err != nil {
					t.Fatal(err)
				}
				reload()
				deny("deleted")
			}

			vhost, err := svc.ActivateVanity(ctx, testRef, "acme")
			if err != nil {
				t.Fatal(err)
			}
			reload()
			if err := s.allowHost(ctx, vhost); (err == nil) != tc.vanityOK {
				t.Errorf("vanity host: allowed=%v, want %v (%v)", err == nil, tc.vanityOK, err)
			}
			if err := svc.DeleteVanity(ctx, testRef); err != nil {
				t.Fatal(err)
			}
			reload()
			if err := s.allowHost(ctx, vhost); err == nil {
				t.Error("vanity host allowed after it was removed")
			}
			// Never for names the node serves itself or for an arbitrary name.
			for _, n := range []string{"evil.example.org", "pooler." + base, "x.studio." + base, "acme2.api." + base} {
				if err := s.allowHost(ctx, n); err == nil {
					t.Errorf("allowHost(%q) allowed", n)
				}
			}
		})
	}
}

// TestCustomHostRouting: an active custom hostname and a vanity subdomain reach the project
// like <ref>.api.<domain>, with what Realtime, Storage and Auth need rewritten, and the
// derived host keeps working.
func TestCustomHostRouting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{}}
	h.cfg.PublicIP = "203.0.113.7"
	svc := domains.New(domains.Options{Reg: h.reg, Config: h.cfg, Resolver: dns})
	const custom = "api.customer.example"
	activateHostname(t, svc, dns, h.ref, custom)
	// The service keeps a project to one of the two; the store does not, and the proxy serves both.
	vanity := svc.VanityHost("acme")
	if err := h.reg.PutVanitySubdomain(ctx, h.ref, "acme", vanity); err != nil {
		t.Fatal(err)
	}
	eventually(t, "routes reach the table", func() bool {
		return h.srv.table.routeKind(custom) == "custom" && h.srv.table.routeKind(vanity) == "vanity"
	})

	for _, host := range []string{custom, vanity, h.host(h.ref)} {
		t.Run(host, func(t *testing.T) {
			// REST: the key is the project's, the path loses its prefix, the client's host is forwarded.
			resp, _ := h.req("GET", host, "/rest/v1/todos?select=*", "apikey", h.k.PublishableKey)
			if resp.StatusCode != 200 {
				t.Fatalf("rest: %d", resp.StatusCode)
			}
			got := h.ups[svcRest].last(t)
			if got.Path != "/todos" || got.Header.Get("X-Forwarded-Host") != host || got.Header.Get("Apikey") != h.k.AnonKey {
				t.Errorf("rest upstream saw %+v", got)
			}
			// Auth: open and protected routes reach the project's GoTrue.
			if resp, _ := h.req("GET", host, "/auth/v1/verify?token=abc"); resp.StatusCode != 200 {
				t.Fatalf("auth verify: %d", resp.StatusCode)
			}
			if got := h.ups[svcAuth].last(t); got.Path != "/verify" || got.Header.Get("X-Forwarded-Host") != host {
				t.Errorf("auth upstream saw %+v", got)
			}
			if resp, _ := h.req("POST", host, "/auth/v1/token?grant_type=password", "apikey", h.k.PublishableKey); resp.StatusCode != 200 {
				t.Fatalf("auth token: %d", resp.StatusCode)
			}
			// Storage: the tenant comes from the derived host in x-forwarded-host (the regexp
			// has no ref to find in a custom host); the host the client signed goes next to it.
			h.req("POST", host, "/storage/v1/object/b/f", "apikey", h.k.PublishableKey, config.StorageClientHostHeader, "spoofed.example")
			got = h.ups[svcStorage].last(t)
			if got.Header.Get("X-Forwarded-Host") != h.host(h.ref) || got.Header.Get(config.StorageClientHostHeader) != host {
				t.Errorf("storage upstream saw x-forwarded-host %q, client host %q; want %q and %q",
					got.Header.Get("X-Forwarded-Host"), got.Header.Get(config.StorageClientHostHeader), h.host(h.ref), host)
			}
			// Realtime: the tenant is the first label of Host, the project's ref.
			h.req("POST", host, "/realtime/v1/api/broadcast", "apikey", h.k.PublishableKey)
			if got := h.ups[svcRealtime].last(t); got.Host != h.ref+".realtime.internal" {
				t.Errorf("realtime host %q", got.Host)
			}
		})
	}
	// The wrong key is refused on a custom host too: the keys are the project's.
	if resp, _ := h.req("GET", custom, "/rest/v1/todos"); resp.StatusCode != 401 {
		t.Errorf("rest without a key: %d", resp.StatusCode)
	}

	// Deleting both takes the hosts away; the derived host still works.
	if _, err := svc.Delete(ctx, h.ref); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteVanity(ctx, h.ref); err != nil {
		t.Fatal(err)
	}
	eventually(t, "routes leave the table", func() bool {
		return h.srv.table.routeKind(custom) == "" && h.srv.table.routeKind(vanity) == ""
	})
	for _, host := range []string{custom, vanity} {
		if resp, _ := h.req("GET", host, "/rest/v1/todos", "apikey", h.k.PublishableKey); resp.StatusCode != 404 {
			t.Errorf("%s after delete: %d", host, resp.StatusCode)
		}
	}
	if resp, _ := h.project("GET", "/rest/v1/todos", "apikey", h.k.PublishableKey); resp.StatusCode != 200 {
		t.Errorf("derived host after delete: %d", resp.StatusCode)
	}
}

// TestUnverifiedHostsAreNotRouted: only an activated hostname has a route, so a claim, even a
// verified one, serves nothing; and a vanity row cannot take a ref-shaped or reserved name.
func TestUnverifiedHostsAreNotRouted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{}}
	h.cfg.PublicIP = "203.0.113.7"
	svc := domains.New(domains.Options{Reg: h.reg, Config: h.cfg, Resolver: dns})
	st, err := svc.Initialize(ctx, h.ref, "claimed.customer.example")
	if err != nil {
		t.Fatal(err)
	}
	dns.txt[st.TXTName] = []string{st.TXTValue}
	dns.host["claimed.customer.example"] = []string{"203.0.113.7"}
	if _, err := svc.Reverify(ctx, h.ref); err != nil {
		t.Fatal(err)
	}
	// Route rows a hostile or buggy writer could add: vanity rows for a ref-shaped label, for
	// a reserved one, and for a deeper name.
	for _, host := range []string{"zyxwvutsrqponmlkjihg.api." + testDomain, "studio.api." + testDomain, "a.b.api." + testDomain} {
		if err := h.reg.PutRoute(ctx, registry.Route{Host: host, Ref: h.ref, Kind: registry.RouteVanity}); err != nil {
			t.Fatal(err)
		}
	}
	// A legitimate vanity subdomain, added last, proves the table has processed the earlier rows.
	if _, err := svc.ActivateVanity(ctx, h.ref, "ok"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "routes reach the table", func() bool { return h.srv.table.routeKind("ok.api."+testDomain) == "vanity" })
	for _, host := range []string{"claimed.customer.example", "zyxwvutsrqponmlkjihg.api." + testDomain, "studio.api." + testDomain, "a.b.api." + testDomain} {
		if k := h.srv.table.routeKind(host); k != "" {
			t.Errorf("%s is routed as %q", host, k)
		}
		if resp, _ := h.req("GET", host, "/rest/v1/todos", "apikey", h.k.PublishableKey); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", host, resp.StatusCode)
		}
	}
}

// A vanity subdomain cannot take a derived project host, whatever the routes table says.
func TestVanityCannotShadowAProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const other = "bbbbbbbbbbbbbbbbbbbb"
	h.keys.set(other, testKeys(t, other))
	if err := h.reg.CreateProject(ctx, &registry.Project{Ref: other, Name: "p2", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.PutRoute(ctx, registry.Route{Host: h.host(other), Ref: h.ref, Kind: registry.RouteVanity}); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.PutRoute(ctx, registry.Route{Host: "marker.customer.example", Ref: h.ref, Kind: "custom"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "routes reach the table", func() bool { return h.srv.table.routeKind("marker.customer.example") == "custom" })
	if p, ok := h.srv.table.lookup(h.host(other)); !ok || p.ref != other {
		t.Fatalf("project host resolved to %q (ok=%v), want %q", p.ref, ok, other)
	}
}

// TestStudioCNAMECheckIsAnsweredByTheNode: Studio's pre-check route is answered from the node's
// resolver in DNS-over-HTTPS JSON, so no domain goes to Cloudflare.
func TestStudioCNAMECheckIsAnsweredByTheNode(t *testing.T) {
	dns := &staticDNS{txt: map[string][]string{}, host: map[string][]string{"node.example.net": {"203.0.113.7"}, "a.customer.example": {"203.0.113.7"}},
		cname: map[string]string{"alias.customer.example": "node.example.net"}}
	h := newHarness(t, func(o *Options) { o.Resolver = dns })
	before := h.ups[svcStudio].count()
	get := func(domain string) (int, map[string]any) {
		t.Helper()
		resp, body := h.req("GET", "studio."+testDomain, "/api/check-cname?domain="+domain)
		var out map[string]any
		_ = json.Unmarshal([]byte(body), &out)
		return resp.StatusCode, out
	}
	code, out := get("alias.customer.example")
	ans, _ := out["Answer"].([]any)
	if code != 200 || len(ans) != 1 || ans[0].(map[string]any)["type"] != float64(5) || ans[0].(map[string]any)["data"] != "node.example.net." {
		t.Fatalf("CNAME: %d %v", code, out)
	}
	// An A record is an answer too: Studio only needs to see that the name resolves.
	code, out = get("a.customer.example")
	ans, _ = out["Answer"].([]any)
	if code != 200 || len(ans) != 1 || ans[0].(map[string]any)["type"] != float64(1) {
		t.Fatalf("A: %d %v", code, out)
	}
	// Nothing there: no Answer, which Studio reports as "cannot be found".
	code, out = get("none.customer.example")
	if _, has := out["Answer"]; code != 200 || has || out["Status"] != float64(3) {
		t.Fatalf("NXDOMAIN: %d %v", code, out)
	}
	for _, bad := range []string{"", "10.0.0.1", "api." + testDomain, "x"} {
		if code, _ := get(bad); code != 400 {
			t.Errorf("domain %q: %d, want 400", bad, code)
		}
	}
	if h.ups[svcStudio].count() != before {
		t.Error("a check reached Studio")
	}
	// Other methods and the project host are not answered.
	if resp, _ := h.project("GET", "/api/check-cname?domain=a.customer.example"); resp.StatusCode != 404 {
		t.Errorf("project host: %d", resp.StatusCode)
	}
	// The route is limited.
	limited := false
	for i := 0; i < cnameCheckPerIPMinute+2; i++ {
		if code, _ := get("a.customer.example"); code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("no rate limit")
	}
}

// TestCNAMECheckLimiter: one client draining its own allowance does not stop the others, and the
// node as a whole has a ceiling.
func TestCNAMECheckLimiter(t *testing.T) {
	var l cnameLimiter
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < cnameCheckPerIPMinute; i++ {
		if !l.allow(now, "198.51.100.1") {
			t.Fatalf("request %d of one client refused", i)
		}
	}
	if l.allow(now, "198.51.100.1") {
		t.Fatal("a client over its allowance was served")
	}
	if !l.allow(now, "198.51.100.2") {
		t.Fatal("another client was refused because of the first")
	}
	// Refused requests do not use the node's budget.
	if l.n != cnameCheckPerIPMinute+1 {
		t.Fatalf("node count %d", l.n)
	}
	if !l.allow(now.Add(time.Minute), "198.51.100.1") {
		t.Fatal("the allowance did not come back")
	}
	// Many clients reach the node's ceiling.
	var m cnameLimiter
	served := 0
	for i := 0; i < cnameCheckPerMinute+50; i++ {
		if m.allow(now, fmt.Sprintf("10.%d.%d.1", i/250, i%250)) {
			served++
		}
	}
	if served != cnameCheckPerMinute {
		t.Fatalf("served %d, want %d", served, cnameCheckPerMinute)
	}
}

// TestForgetCertificates: when a custom hostname stops being routed its certificate and key leave
// storage; a name that is routed again, or that the wildcard covers, keeps its own.
func TestForgetCertificates(t *testing.T) {
	ctx := context.Background()
	s := tlsServer(t, func(c *config.Config) { c.Domain, c.TLS.Mode = "example.com", "http01" })
	cm, err := newCertManager(certOptions{cfg: s.cfg, mode: s.tlsMode, allow: s.allowHost, log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer cm.close()
	st := cm.http.Storage
	issuer := cm.http.Issuers[0].IssuerKey()
	put := func(host string) {
		t.Helper()
		for _, key := range []string{certmagic.StorageKeys.SiteCert(issuer, host), certmagic.StorageKeys.SitePrivateKey(issuer, host), certmagic.StorageKeys.SiteMeta(issuer, host)} {
			if err := st.Store(ctx, key, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	has := func(host string) bool { return st.Exists(ctx, certmagic.StorageKeys.SiteCert(issuer, host)) }
	put("docs.customer.example")
	put("kept.customer.example")
	put("other.customer.example")
	if err := s.reg().PutRoute(ctx, registry.Route{Host: "kept.customer.example", Ref: testRef, Kind: registry.RouteCustom}); err != nil {
		t.Fatal(err)
	}
	if err := s.table.reload(ctx); err != nil {
		t.Fatal(err)
	}
	s.forgetCertificates(ctx, cm, []string{"docs.customer.example", "kept.customer.example", "missing.customer.example"})
	if has("docs.customer.example") {
		t.Error("the certificate of a removed host stayed in storage")
	}
	if !has("kept.customer.example") {
		t.Error("the certificate of a host that is routed again was removed")
	}
	if !has("other.customer.example") {
		t.Error("a certificate that was not named was removed")
	}
}

// TestRouteChangesAreReported: the table tells the certificate code which hosts started and
// stopped being served, from registry changes.
func TestRouteChangesAreReported(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	type change struct{ added, removed []string }
	got := make(chan change, 8)
	h.srv.table.setRoutesChanged(func(a, r []string) { got <- change{a, r} })
	next := func() change {
		t.Helper()
		select {
		case c := <-got:
			return c
		case <-time.After(3 * time.Second):
			t.Fatal("no change reported")
			return change{}
		}
	}
	if err := h.reg.PutRoute(ctx, registry.Route{Host: "a.customer.example", Ref: h.ref, Kind: registry.RouteCustom}); err != nil {
		t.Fatal(err)
	}
	if c := next(); len(c.added) != 1 || c.added[0] != "a.customer.example" || len(c.removed) != 0 {
		t.Fatalf("after put: %+v", c)
	}
	if err := h.reg.DeleteRoute(ctx, "a.customer.example"); err != nil {
		t.Fatal(err)
	}
	if c := next(); len(c.removed) != 1 || c.removed[0] != "a.customer.example" || len(c.added) != 0 {
		t.Fatalf("after delete: %+v", c)
	}
	// Deleting the project takes its routes with it.
	if err := h.reg.PutRoute(ctx, registry.Route{Host: "b.customer.example", Ref: h.ref, Kind: registry.RouteCustom}); err != nil {
		t.Fatal(err)
	}
	next()
	if err := h.reg.DeleteProject(ctx, h.ref); err != nil {
		t.Fatal(err)
	}
	if c := next(); len(c.removed) == 0 {
		t.Fatalf("after project delete: %+v", c)
	}
}
