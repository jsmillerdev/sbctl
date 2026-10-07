package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	hetzner "github.com/libdns/hetzner/v2"
	"github.com/libdns/route53"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestResolveTLSMode(t *testing.T) {
	cases := []struct {
		name                       string
		mode, provider, domain, ip string
		want                       string
		wantErr                    bool
	}{
		{name: "off", mode: "off", domain: "example.com", want: tlsOff},
		{name: "off needs nothing", mode: "off", want: tlsOff},
		{name: "auto with provider", mode: "auto", provider: "cloudflare", domain: "example.com", want: tlsAuto},
		{name: "auto without provider", mode: "auto", domain: "example.com", want: tlsHTTP01},
		{name: "auto, sslip.io", mode: "auto", ip: "203.0.113.7", want: tlsHTTP01},
		{name: "auto, provider but only sslip.io", mode: "auto", provider: "cloudflare", ip: "203.0.113.7", want: tlsHTTP01},
		{name: "dns01", mode: "dns01", provider: "route53", domain: "example.com", want: tlsDNS01},
		{name: "dns01 on sslip.io cannot work", mode: "dns01", provider: "route53", ip: "203.0.113.7", wantErr: true},
		{name: "http01 forced", mode: "http01", provider: "cloudflare", domain: "example.com", want: tlsHTTP01},
		{name: "no domain and no ip", mode: "auto", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.TLS.Mode, cfg.TLS.DNSProvider, cfg.Domain, cfg.PublicIP = c.mode, c.provider, c.domain, c.ip
			got, err := resolveTLSMode(cfg)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %q, %v; want %q (err %v)", got, err, c.want, c.wantErr)
			}
		})
	}
}

func TestManagedNames(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.com"
	for mode, want := range map[string][]string{
		tlsDNS01:  {"*.api.example.com", "api.example.com", "studio.example.com"},
		tlsAuto:   {"*.api.example.com", "api.example.com", "studio.example.com"},
		tlsHTTP01: {"api.example.com", "studio.example.com"},
	} {
		if got := managedNames(cfg, mode); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", mode, got, want)
		}
	}
	cfg.Domain, cfg.PublicIP = "", "203.0.113.7"
	if got := managedNames(cfg, tlsHTTP01); !reflect.DeepEqual(got, []string{"api.203.0.113.7.sslip.io", "studio.203.0.113.7.sslip.io"}) {
		t.Errorf("sslip.io: %v", got)
	}
}

func TestNewDNSProvider(t *testing.T) {
	good := []struct {
		name string
		cred map[string]string
		typ  any
	}{
		{"route53", nil, &route53.Provider{}},
		{"route53", map[string]string{"region": "eu-west-1", "hosted_zone_id": "Z123"}, &route53.Provider{}},
		{"cloudflare", map[string]string{"api_token": "t"}, &cloudflare.Provider{}},
		{"hetzner", map[string]string{"api_token": "t"}, &hetzner.Provider{}},
		{"digitalocean", map[string]string{"api_token": "t"}, &digitalocean.Provider{}},
	}
	for _, c := range good {
		p, err := newDNSProvider(c.name, c.cred)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if reflect.TypeOf(p) != reflect.TypeOf(c.typ) {
			t.Errorf("%s: got %T", c.name, p)
		}
	}
	p, _ := newDNSProvider("route53", map[string]string{"region": "eu-west-1", "hosted_zone_id": "Z123"})
	if r := p.(*route53.Provider); r.Region != "eu-west-1" || r.HostedZoneID != "Z123" {
		t.Errorf("route53 options not applied: %+v", r)
	}
	p, _ = newDNSProvider("cloudflare", map[string]string{"api_token": " tok ", "zone_token": "z"})
	if c := p.(*cloudflare.Provider); c.APIToken != "tok" || c.ZoneToken != "z" {
		t.Errorf("cloudflare options: %+v", c)
	}
	for _, name := range []string{"cloudflare", "hetzner", "digitalocean"} {
		if _, err := newDNSProvider(name, map[string]string{"api_token": "  "}); err == nil || !strings.Contains(err.Error(), "SBCTL_TLS_CREDENTIALS_API_TOKEN") {
			t.Errorf("%s without a token: %v", name, err)
		}
	}
	if _, err := newDNSProvider("godaddy", nil); err == nil || !strings.Contains(err.Error(), "route53") {
		t.Errorf("unknown provider: %v", err)
	}
	if _, err := newDNSProvider("", nil); err == nil {
		t.Error("empty provider accepted")
	}
}

// tlsServer builds a Server for a TLS strategy with one project, without listening.
func tlsServer(t *testing.T, mut func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	mut(cfg)
	reg := registry.NewMemory()
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Config: cfg, Registry: reg, Keys: newFakeKeys(), Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewRejectsBadTLSConfig(t *testing.T) {
	for name, mut := range map[string]func(*config.Config){
		"cloudflare without token": func(c *config.Config) {
			c.Domain, c.TLS.Mode, c.TLS.DNSProvider = "example.com", "dns01", "cloudflare"
		},
		"unknown provider": func(c *config.Config) {
			c.Domain, c.TLS.Mode, c.TLS.DNSProvider = "example.com", "auto", "godaddy"
		},
		"no domain": func(c *config.Config) { c.TLS.Mode = "auto" },
	} {
		cfg := config.Default()
		cfg.StateDir = t.TempDir()
		mut(cfg)
		if _, err := New(Options{Config: cfg, Registry: registry.NewMemory(), Keys: newFakeKeys(), Logger: quietLog()}); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestAllowHost(t *testing.T) {
	base := "example.com"
	proj := testRef + ".api." + base
	unknownProj := "zzzzzzzzzzzzzzzzzzzz.api." + base
	custom := "db.customer.example"

	cases := []struct {
		name  string
		mut   func(*config.Config)
		allow []string
		deny  []string
	}{
		{
			name:  "http01: every host we serve, nothing else",
			mut:   func(c *config.Config) { c.Domain, c.TLS.Mode = base, "http01" },
			allow: []string{"api." + base, "studio." + base, proj, strings.ToUpper(proj), proj + ".", proj + ":443"},
			deny:  []string{unknownProj, "evil.example.org", base, "pooler." + base, "x." + proj, "", "system.api." + base, "api.api." + base},
		},
		{
			name: "auto with a DNS provider: wildcard covers projects, so only api and studio",
			mut: func(c *config.Config) {
				c.Domain, c.TLS.Mode, c.TLS.DNSProvider = base, "auto", "cloudflare"
				c.TLS.Credentials = map[string]string{"api_token": "t"}
			},
			allow: []string{"api." + base, "studio." + base},
			deny:  []string{proj, unknownProj, "evil.example.org"},
		},
		{
			name: "dns01: never HTTP-01",
			mut: func(c *config.Config) {
				c.Domain, c.TLS.Mode, c.TLS.DNSProvider = base, "dns01", "route53"
			},
			allow: []string{"api." + base, "studio." + base},
			deny:  []string{proj, unknownProj, "evil.example.org"},
		},
		{
			name:  "sslip.io",
			mut:   func(c *config.Config) { c.PublicIP, c.TLS.Mode = "203.0.113.7", "auto" },
			allow: []string{"api.203.0.113.7.sslip.io", "studio.203.0.113.7.sslip.io", testRef + ".api.203.0.113.7.sslip.io"},
			deny:  []string{testRef + ".api.203.0.113.8.sslip.io", "evil.sslip.io"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := tlsServer(t, c.mut)
			for _, n := range c.allow {
				if err := s.allowHost(context.Background(), n); err != nil {
					t.Errorf("allowHost(%q) = %v, want nil", n, err)
				}
			}
			for _, n := range c.deny {
				if err := s.allowHost(context.Background(), n); err == nil {
					t.Errorf("allowHost(%q) allowed", n)
				}
			}
		})
	}

	// Registry routes with custom hostnames are certified by HTTP-01 unless the mode forbids it.
	for mode, wantAllowed := range map[string]bool{"http01": true, "auto": true, "dns01": false} {
		s := tlsServer(t, func(c *config.Config) {
			c.Domain, c.TLS.Mode = base, mode
			if mode != "http01" {
				c.TLS.DNSProvider = "route53"
			}
		})
		if err := s.reg().PutRoute(context.Background(), registry.Route{Host: custom, Ref: testRef, Kind: "custom"}); err != nil {
			t.Fatal(err)
		}
		if err := s.table.reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.allowHost(context.Background(), custom); (err == nil) != wantAllowed {
			t.Errorf("mode %s: custom host allowed=%v, want %v (%v)", mode, err == nil, wantAllowed, err)
		}
	}
}

func (s *Server) reg() registry.Registry { return s.opts.Registry }

func TestNewCertManager(t *testing.T) {
	cfg := config.Default()
	cfg.Domain, cfg.StateDir = "example.com", t.TempDir()
	allow := func(context.Context, string) error { return nil }
	prov := &cloudflare.Provider{APIToken: "t"}

	t.Run("http01", func(t *testing.T) {
		cm, err := newCertManager(certOptions{cfg: cfg, mode: tlsHTTP01, allow: allow, httpPort: 80, httpsPort: 443, log: quietLog()})
		if err != nil {
			t.Fatal(err)
		}
		defer cm.close()
		if cm.dns != nil || cm.dnsIssuer != nil {
			t.Error("http01 mode must not have a DNS-01 config")
		}
		if cm.httpIssuer.CA != certmagic.LetsEncryptProductionCA {
			t.Errorf("default CA %q", cm.httpIssuer.CA)
		}
		if cm.httpIssuer.DNS01Solver != nil || cm.httpIssuer.DisableHTTPChallenge {
			t.Error("http01 mode must not use DNS-01 and must keep HTTP-01")
		}
		if fs, ok := cm.http.Storage.(*certmagic.FileStorage); !ok || fs.Path != filepath.Join(cfg.StateDir, "certs") {
			t.Errorf("storage %#v, want file storage under state_dir/certs", cm.http.Storage)
		}
		if cm.http.OnDemand == nil || cm.http.OnDemand.DecisionFunc == nil {
			t.Error("on-demand issuance has no decision function")
		}
		if !cm.httpIssuer.Agreed {
			t.Error("issuer does not agree to the CA terms")
		}
	})
	t.Run("dns01 disables other challenges", func(t *testing.T) {
		cm, err := newCertManager(certOptions{cfg: cfg, mode: tlsDNS01, provider: prov, allow: allow, log: quietLog()})
		if err != nil {
			t.Fatal(err)
		}
		defer cm.close()
		if cm.dnsIssuer.DNS01Solver == nil || !cm.dnsIssuer.DisableHTTPChallenge || !cm.dnsIssuer.DisableTLSALPNChallenge {
			t.Error("dns01 mode must use only DNS-01")
		}
		if cm.http != nil || cm.httpIssuer != nil {
			t.Error("dns01 mode must not have an HTTP-01 config")
		}
		for _, n := range []string{"api.example.com", "x.api.example.com", "db.customer.example", ""} {
			if cm.configFor(n) != cm.dns {
				t.Errorf("dns01: %q is not served by the DNS config", n)
			}
		}
	})
	t.Run("auto: DNS-01 for the wildcard, HTTP-01 for custom hosts, in separate configs", func(t *testing.T) {
		cm, err := newCertManager(certOptions{cfg: cfg, mode: tlsAuto, provider: prov, allow: allow, log: quietLog()})
		if err != nil {
			t.Fatal(err)
		}
		defer cm.close()
		if cm.dnsIssuer == nil || cm.dnsIssuer.DNS01Solver == nil {
			t.Fatal("auto mode needs a DNS-01 issuer for the wildcard")
		}
		// CertMagic uses DNS-01 exclusively when an issuer has a DNS solver, so the
		// issuer for custom hosts must not have one.
		if cm.httpIssuer == nil || cm.httpIssuer.DNS01Solver != nil || cm.httpIssuer.DisableHTTPChallenge || cm.httpIssuer.DisableTLSALPNChallenge {
			t.Fatal("auto mode needs a separate HTTP-01 issuer without a DNS solver")
		}
		if cm.dns.OnDemand != nil || cm.http.OnDemand == nil || cm.http.OnDemand.DecisionFunc == nil {
			t.Error("only the HTTP-01 config issues on demand, through the decision function")
		}
		for name, want := range map[string]*certmagic.Config{
			"db.customer.example":     cm.http,
			"evil.example.org":        cm.http,
			"example.com":             cm.http,
			"api.example.com":         cm.dns,
			"studio.example.com":      cm.dns,
			"ref.api.example.com":     cm.dns,
			"REF.API.example.com.":    cm.dns,
			"x.y.api.example.com":     cm.dns,
			"":                        cm.dns,
			"notapi.example.com":      cm.http,
			"api.example.com.evil.io": cm.http,
		} {
			if got := cm.configFor(name); got != want {
				t.Errorf("configFor(%q) picked the wrong config", name)
			}
		}
		if cm.configForNames([]string{"*.api.example.com"}) != cm.dns || cm.configForNames([]string{"db.customer.example"}) != cm.http {
			t.Error("configForNames (renewal) picks the wrong config")
		}
		if cm.challengeIssuer() != cm.httpIssuer {
			t.Error(":80 must answer the HTTP-01 issuer's challenges")
		}
		if cm.tlsConfig().GetCertificate == nil {
			t.Error("no SNI dispatch on the TLS config")
		}
	})
	t.Run("dns without provider", func(t *testing.T) {
		if _, err := newCertManager(certOptions{cfg: cfg, mode: tlsDNS01, allow: allow, log: quietLog()}); err == nil {
			t.Error("dns01 without a provider was accepted")
		}
	})
	t.Run("custom CA and trusted root", func(t *testing.T) {
		c := *cfg
		c.TLS.CA = "https://acme.internal.example/dir"
		c.TLS.Email = "ops@example.com"
		c.TLS.CACert = filepath.Join(t.TempDir(), "root.pem")
		if err := os.WriteFile(c.TLS.CACert, selfSignedPEM(t), 0o600); err != nil {
			t.Fatal(err)
		}
		cm, err := newCertManager(certOptions{cfg: &c, mode: tlsHTTP01, allow: allow, log: quietLog()})
		if err != nil {
			t.Fatal(err)
		}
		defer cm.close()
		if is := cm.httpIssuer; is.CA != c.TLS.CA || is.Email != "ops@example.com" || is.TrustedRoots == nil {
			t.Errorf("issuer: CA=%q email=%q roots=%v", is.CA, is.Email, is.TrustedRoots)
		}
		if err := os.WriteFile(c.TLS.CACert, []byte("not pem"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newCertManager(certOptions{cfg: &c, mode: tlsHTTP01, allow: allow, log: quietLog()}); err == nil {
			t.Error("garbage ca_cert accepted")
		}
		c.TLS.CACert = filepath.Join(t.TempDir(), "missing.pem")
		if _, err := newCertManager(certOptions{cfg: &c, mode: tlsHTTP01, allow: allow, log: quietLog()}); err == nil {
			t.Error("missing ca_cert accepted")
		}
	})
}

func TestHTTPRedirectHandler(t *testing.T) {
	s := tlsServer(t, func(c *config.Config) { c.Domain, c.TLS.Mode = "example.com", "http01" })
	cm, err := newCertManager(certOptions{cfg: s.cfg, mode: s.tlsMode, allow: s.allowHost, log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer cm.close()
	proj := testRef + ".api.example.com"
	for _, c := range []struct {
		name, host, target string
		httpsPort          int
		status             int
		location           string
	}{
		{"project", proj, "/rest/v1/todos?select=*", 443, 308, "https://" + proj + "/rest/v1/todos?select=*"},
		{"api host", "api.example.com", "/v1/projects", 443, 308, "https://api.example.com/v1/projects"},
		{"studio with port", "studio.example.com:80", "/", 443, 308, "https://studio.example.com/"},
		{"non-default https port", proj, "/x", 8443, 308, "https://" + proj + ":8443/x"},
		{"acme path without a pending challenge still redirects", proj, "/.well-known/acme-challenge/nope", 443, 308, "https://" + proj + "/.well-known/acme-challenge/nope"},
		{"unknown host", "evil.example.org", "/", 443, 404, ""},
		{"unregistered project", "zzzzzzzzzzzzzzzzzzzz.api.example.com", "/", 443, 404, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "http://"+c.host+c.target, nil)
			s.redirectHandler(cm, c.httpsPort).ServeHTTP(rec, r)
			if rec.Code != c.status || rec.Header().Get("Location") != c.location {
				t.Fatalf("%d %q, want %d %q", rec.Code, rec.Header().Get("Location"), c.status, c.location)
			}
		})
	}
}

// TestServeTLSRefusesUnknownNames runs the real listeners in TLS mode against an
// unreachable ACME directory: a handshake for a name we do not serve is refused by
// the on-demand decision function, and plain HTTP redirects.
func TestServeTLSRefusesUnknownNames(t *testing.T) {
	s := tlsServer(t, func(c *config.Config) {
		c.Domain, c.TLS.Mode = "example.com", "http01"
		c.TLS.CA = "http://127.0.0.1:1/dir" // nothing listens: issuance can only fail
	})
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, httpLn, httpsLn) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	}()

	for _, name := range []string{"evil.example.org", "zzzzzzzzzzzzzzzzzzzz.api.example.com", "example.com"} {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", httpsLn.Addr().String(),
			&tls.Config{ServerName: name, InsecureSkipVerify: true})
		if err == nil {
			conn.Close()
			t.Errorf("handshake for %q succeeded", name)
		}
	}

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 5 * time.Second}
	req, _ := http.NewRequest("GET", "http://"+httpLn.Addr().String()+"/rest/v1/x?a=b", nil)
	req.Host = testRef + ".api.example.com"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	wantLoc := "https://" + testRef + ".api.example.com:" + strings.Split(httpsLn.Addr().String(), ":")[1] + "/rest/v1/x?a=b"
	if resp.StatusCode != 308 || resp.Header.Get("Location") != wantLoc {
		t.Errorf("redirect: %d %q, want %q", resp.StatusCode, resp.Header.Get("Location"), wantLoc)
	}
}

// selfSignedPEM returns a throwaway CA certificate in PEM form.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sbctl test root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestServeClosesListenersOnCertSetupFailure: a bad ca_cert fails Serve before any
// server owns the listeners, and Serve must not leak them.
func TestServeClosesListenersOnCertSetupFailure(t *testing.T) {
	s := tlsServer(t, func(c *config.Config) {
		c.Domain, c.TLS.Mode = "example.com", "http01"
		c.TLS.CACert = filepath.Join(t.TempDir(), "missing.pem")
	})
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(context.Background(), httpLn, httpsLn); err == nil {
		t.Fatal("Serve accepted a missing ca_cert")
	}
	for _, ln := range []net.Listener{httpLn, httpsLn} {
		if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
			c.Close()
			t.Errorf("listener %s still accepts connections", ln.Addr())
		}
	}
}
