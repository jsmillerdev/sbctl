package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// TestPebbleIssuance obtains real certificates from a Pebble ACME test server
// over HTTP-01 and TLS-ALPN-01, through our own listeners. It is CI only: see
// internal/proxy/pebble-test.sh. Required environment:
//
//	SUPAVISE_TEST_PEBBLE_URL        ACME directory, e.g. https://localhost:14000/dir
//	SUPAVISE_TEST_PEBBLE_CA         PEM root that signs Pebble's own HTTPS certificate
//	SUPAVISE_TEST_PEBBLE_HTTP_PORT  Pebble's httpPort (default 5002)
//	SUPAVISE_TEST_PEBBLE_TLS_PORT   Pebble's tlsPort (default 5001)
//
// Pebble must resolve every name under example.test to 127.0.0.1 (pebble-challtestsrv
// with -defaultIPv4 127.0.0.1, or pebble -dnsserver).
func TestPebbleIssuance(t *testing.T) {
	dir := os.Getenv("SUPAVISE_TEST_PEBBLE_URL")
	if dir == "" {
		t.Skip("SUPAVISE_TEST_PEBBLE_URL not set (CI-only test, see internal/proxy/pebble-test.sh)")
	}
	httpPort := envPort(t, "SUPAVISE_TEST_PEBBLE_HTTP_PORT", 5002)
	tlsPort := envPort(t, "SUPAVISE_TEST_PEBBLE_TLS_PORT", 5001)

	s := tlsServer(t, func(c *config.Config) {
		c.Domain, c.TLS.Mode = "example.test", "http01"
		c.TLS.CA, c.TLS.CACert, c.TLS.Email = dir, os.Getenv("SUPAVISE_TEST_PEBBLE_CA"), "ci@example.test"
	})
	httpLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(httpPort))
	if err != nil {
		t.Fatal(err)
	}
	httpsLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(tlsPort))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, httpLn, httpsLn) }()
	defer func() { cancel(); <-done }()

	// api.example.test is a startup name; the project host is issued on demand at the first handshake.
	for _, name := range []string{"api.example.test", testRef + ".api.example.test"} {
		var got *tls.ConnectionState
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", httpsLn.Addr().String(),
				&tls.Config{ServerName: name, InsecureSkipVerify: true})
			if err == nil {
				st := conn.ConnectionState()
				conn.Close()
				if len(st.PeerCertificates) > 0 && len(st.PeerCertificates[0].DNSNames) > 0 && st.PeerCertificates[0].DNSNames[0] == name {
					got = &st
					break
				}
			}
			time.Sleep(time.Second)
		}
		if got == nil {
			t.Fatalf("no certificate for %s within 90 s", name)
		}
		if iss := got.PeerCertificates[0].Issuer.CommonName; !strings.Contains(strings.ToLower(iss), "pebble") {
			t.Errorf("%s: issuer %q is not Pebble", name, iss)
		}
	}

	// A name we do not serve is refused without contacting the CA.
	if conn, err := tls.Dial("tcp", httpsLn.Addr().String(), &tls.Config{ServerName: "evil.example.test", InsecureSkipVerify: true}); err == nil {
		conn.Close()
		t.Error("certificate issued for a host we do not serve")
	}
}

func envPort(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}
