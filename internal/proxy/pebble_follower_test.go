package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// TestPebbleFollowerMirrorsTheLeader takes real certificates from a Pebble ACME test server on a node
// that leads, stops it, and starts a second node that follows it: the follower serves the leader's
// certificates without a call to the CA, refuses a name the leader never issued for, and after a
// promotion issues that one itself, under the ACME account it mirrored. It is CI only: see
// internal/proxy/pebble-test.sh, and TestPebbleIssuance for the environment.
func TestPebbleFollowerMirrorsTheLeader(t *testing.T) {
	dir := os.Getenv("SUPAVISE_TEST_PEBBLE_URL")
	if dir == "" {
		t.Skip("SUPAVISE_TEST_PEBBLE_URL not set (CI-only test, see internal/proxy/pebble-test.sh)")
	}
	httpPort := envPort(t, "SUPAVISE_TEST_PEBBLE_HTTP_PORT", 5002)
	tlsPort := envPort(t, "SUPAVISE_TEST_PEBBLE_TLS_PORT", 5001)
	const second = "mmmmmmmmmmnnnnnnnnnn"
	ctx := context.Background()

	reg := registry.NewMemory()
	for _, ref := range []string{testRef, second} {
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref, Status: registry.StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
	}
	node := func(mut func(*Options, *config.Config)) (*Server, *config.Config) {
		cfg := config.Default()
		cfg.StateDir = t.TempDir()
		cfg.Domain, cfg.TLS.Mode = "example.test", "http01"
		cfg.TLS.CA, cfg.TLS.CACert, cfg.TLS.Email = dir, os.Getenv("SUPAVISE_TEST_PEBBLE_CA"), "ci@example.test"
		o := Options{Config: cfg, Registry: reg, Keys: newFakeKeys(), Logger: quietLog()}
		if mut != nil {
			mut(&o, cfg)
		}
		s, err := New(o)
		if err != nil {
			t.Fatal(err)
		}
		return s, cfg
	}
	start := func(s *Server) (addr string, stop func()) {
		httpLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(httpPort))
		if err != nil {
			t.Fatal(err)
		}
		httpsLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(tlsPort))
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- s.Serve(cctx, httpLn, httpsLn) }()
		return httpsLn.Addr().String(), func() { cancel(); <-done }
	}
	handshake := func(addr, name string, wait time.Duration) *x509.Certificate {
		deadline := time.Now().Add(wait)
		for {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr,
				&tls.Config{ServerName: name, InsecureSkipVerify: true})
			if err == nil {
				c := conn.ConnectionState().PeerCertificates[0]
				conn.Close()
				if len(c.DNSNames) > 0 && c.DNSNames[0] == name {
					return c
				}
			}
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(time.Second)
		}
	}
	host := func(ref string) string { return ref + ".api.example.test" }

	// The leader obtains the certificates of api. and of the first project.
	leader, leaderCfg := node(nil)
	addr, stop := start(leader)
	serials := map[string]*big.Int{}
	for _, name := range []string{"api.example.test", host(testRef)} {
		c := handshake(addr, name, 90*time.Second)
		if c == nil {
			stop()
			t.Fatalf("the leader got no certificate for %s within 90 s", name)
		}
		serials[name] = c.SerialNumber
	}
	stop()

	// The follower reads the leader's store from disk, as the peer API would deliver it.
	leaderStore := leaderCfg.Paths().Certs()
	source := sourceFunc(func(context.Context, string) (peerapi.CertSnapshot, error) {
		files, err := readCertStore(leaderStore, quietLog())
		return peerapi.CertSnapshot{Files: files}, err
	})
	role := NewCertRole(false)
	follower, followerCfg := node(func(o *Options, _ *config.Config) {
		o.Cluster = &Cluster{Certs: &CertSync{Role: role, Source: source, Interval: 200 * time.Millisecond}}
	})
	addr, stop = start(follower)
	defer stop()
	for name, serial := range serials {
		c := handshake(addr, name, 30*time.Second)
		if c == nil || c.SerialNumber.Cmp(serial) != 0 {
			t.Fatalf("the follower does not serve the leader's certificate for %s", name)
		}
	}

	// Nothing was issued for the second project, so the follower has nothing to serve for it, and it does
	// not go and get one.
	if c := handshake(addr, host(second), 5*time.Second); c != nil {
		t.Fatalf("a node that follows issued a certificate for %s", host(second))
	}
	account := func(cfg *config.Config) string {
		matches, _ := filepath.Glob(filepath.Join(cfg.Paths().Certs(), "acme", "*", "users", "*", "*.json"))
		if len(matches) != 1 {
			t.Fatalf("%d ACME accounts in %s", len(matches), cfg.Paths().Certs())
		}
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	leaderAccount := account(leaderCfg)
	if account(followerCfg) != leaderAccount {
		t.Error("the follower does not hold the leader's ACME account")
	}

	// After the promotion it issues for that name, under the account it mirrored, and keeps serving
	// the certificates it mirrored.
	role.Promote()
	c := handshake(addr, host(second), 90*time.Second)
	if c == nil {
		t.Fatalf("the promoted node got no certificate for %s within 90 s", host(second))
	}
	if iss := c.Issuer.CommonName; !strings.Contains(strings.ToLower(iss), "pebble") {
		t.Errorf("issuer %q is not Pebble", iss)
	}
	if got := account(followerCfg); got != leaderAccount {
		t.Error("the promoted node registered an ACME account of its own")
	}
	for name, serial := range serials {
		if c := handshake(addr, name, 10*time.Second); c == nil || c.SerialNumber.Cmp(serial) != 0 {
			t.Errorf("after the promotion the node does not serve the certificate it mirrored for %s", name)
		}
	}
}
