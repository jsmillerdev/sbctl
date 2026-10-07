package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/domains"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// TestPebbleCustomHostname takes a custom hostname from initialize to activation against a Pebble
// ACME test server and a local DNS stub, fetches REST and Auth through it over HTTPS with the
// certificate Pebble issued, and deletes it again. It is CI only: see internal/proxy/pebble-test.sh,
// which also runs TestPebbleIssuance. Environment, besides the variables of TestPebbleIssuance:
//
//	SUPAVISE_TEST_CHALLTESTSRV_URL  pebble-challtestsrv management API, e.g. http://127.0.0.1:8055
//	SUPAVISE_TEST_CHALLTESTSRV_DNS  its DNS server, e.g. 127.0.0.1:8053 (answers every A query
//	                                with 127.0.0.1 and serves the TXT records the test sets)
//
// The challenge test server is the DNS stub: the node's resolver in this test is a net.Resolver
// pointed at it, and Pebble resolves the same names through it.
func TestPebbleCustomHostname(t *testing.T) {
	dir := os.Getenv("SUPAVISE_TEST_PEBBLE_URL")
	mgmt := os.Getenv("SUPAVISE_TEST_CHALLTESTSRV_URL")
	dnsAddr := os.Getenv("SUPAVISE_TEST_CHALLTESTSRV_DNS")
	if dir == "" || mgmt == "" || dnsAddr == "" {
		t.Skip("SUPAVISE_TEST_PEBBLE_URL, SUPAVISE_TEST_CHALLTESTSRV_URL or _DNS not set (CI-only test, see internal/proxy/pebble-test.sh)")
	}
	httpPort := envPort(t, "SUPAVISE_TEST_PEBBLE_HTTP_PORT", 5002)
	tlsPort := envPort(t, "SUPAVISE_TEST_PEBBLE_TLS_PORT", 5001)
	const hostname = "docs.example.test"
	ctx := context.Background()

	// REST and Auth stand-ins that report what they were asked.
	var mu sync.Mutex
	seen := map[string]http.Header{}
	up := func(name string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = r.Header.Clone()
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"service": name, "path": r.URL.Path})
		}))
		t.Cleanup(s.Close)
		return strings.TrimPrefix(s.URL, "http://")
	}
	restAddr, authAddr := up("rest"), up("auth")

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain, cfg.TLS.Mode = "example.test", "http01"
	cfg.TLS.CA, cfg.TLS.CACert, cfg.TLS.Email = dir, os.Getenv("SUPAVISE_TEST_PEBBLE_CA"), "ci@example.test"
	reg := registry.NewMemory()
	if err := reg.CreateProject(ctx, &registry.Project{Ref: testRef, Name: "p", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	keys := newFakeKeys()
	k := testKeys(t, testRef)
	keys.set(testRef, k)
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", dnsAddr)
	}}
	s, err := New(Options{Config: cfg, Registry: reg, Keys: keys, Logger: quietLog(), Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	s.upstreamFn = func(svc service, _ project) string {
		if svc == svcAuth {
			return authAddr
		}
		return restAddr
	}
	httpLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(httpPort))
	if err != nil {
		t.Fatal(err)
	}
	httpsLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(tlsPort))
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Serve(sctx, httpLn, httpsLn) }()
	defer func() { cancel(); <-done }()

	svc := domains.New(domains.Options{Reg: reg, Config: cfg, Resolver: resolver})
	handshake := func() (*tls.ConnectionState, error) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", httpsLn.Addr().String(),
			&tls.Config{ServerName: hostname, InsecureSkipVerify: true})
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		st := conn.ConnectionState()
		return &st, nil
	}

	// Claimed, verified, not activated: the node never asks the CA for it.
	st, err := svc.Initialize(ctx, testRef, hostname)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handshake(); err == nil {
		t.Fatal("a certificate was issued for a hostname that is only claimed")
	}
	setTXT(t, mgmt, domains.ChallengeName(hostname)+".", st.TXTValue)
	// DNS answers every name with 127.0.0.1, which is where the project's own host points too.
	if st, err = svc.Reverify(ctx, testRef); err != nil || st.Status != registry.HostnameOriginReady {
		t.Fatalf("reverify: %+v %v", st, err)
	}
	if _, err := handshake(); err == nil {
		t.Fatal("a certificate was issued for a hostname that is verified but not activated")
	}

	if _, _, err := svc.Activate(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	// Activation routes the host; the proxy asks Pebble for its certificate at once.
	var cs *tls.ConnectionState
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := handshake(); err == nil && len(c.PeerCertificates) > 0 && len(c.PeerCertificates[0].DNSNames) > 0 && c.PeerCertificates[0].DNSNames[0] == hostname {
			cs = c
			break
		}
		time.Sleep(time.Second)
	}
	if cs == nil {
		t.Fatalf("no certificate for %s within 90 s of activating it", hostname)
	}
	if iss := cs.PeerCertificates[0].Issuer.CommonName; !strings.Contains(strings.ToLower(iss), "pebble") {
		t.Errorf("issuer %q is not Pebble", iss)
	}

	// REST and Auth through the custom hostname, over TLS with that certificate.
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", httpsLn.Addr().String())
		},
		TLSClientConfig: &tls.Config{ServerName: hostname, InsecureSkipVerify: true},
	}}
	get := func(path string, hdr ...string) (int, map[string]string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "https://"+hostname+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]string
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	if code, out := get("/rest/v1/todos", "apikey", k.PublishableKey); code != 200 || out["service"] != "rest" || out["path"] != "/todos" {
		t.Fatalf("REST through %s: %d %v", hostname, code, out)
	}
	if code, out := get("/auth/v1/settings", "apikey", k.PublishableKey); code != 200 || out["service"] != "auth" || out["path"] != "/settings" {
		t.Fatalf("Auth through %s: %d %v", hostname, code, out)
	}
	if code, out := get("/auth/v1/verify?token=abc"); code != 200 || out["service"] != "auth" {
		t.Fatalf("Auth verify through %s: %d %v", hostname, code, out)
	}
	mu.Lock()
	if fh := seen["auth"].Get("X-Forwarded-Host"); fh != hostname || seen["rest"].Get("X-Forwarded-Proto") != "https" {
		t.Errorf("upstream saw X-Forwarded-Host %q and proto %q", fh, seen["rest"].Get("X-Forwarded-Proto"))
	}
	mu.Unlock()
	if code, _ := get("/rest/v1/todos"); code != 401 {
		t.Errorf("REST without a key through the custom hostname: %d, want 401", code)
	}

	// Delete: the route goes, and with it the certificate.
	if _, err := svc.Delete(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	gone := false
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if _, err := handshake(); err != nil {
			gone = true
			break
		}
	}
	if !gone {
		t.Fatal("the node still serves a certificate for a deleted hostname")
	}
}

// setTXT publishes a TXT record on the challenge test server.
func setTXT(t *testing.T, mgmt, host, value string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"host": host, "value": value})
	resp, err := http.Post(strings.TrimSuffix(mgmt, "/")+"/set-txt", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("set-txt: %d %s", resp.StatusCode, b)
	}
}
