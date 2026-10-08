package fleet

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownstreamCertIsCreatedPrivateAndReused(t *testing.T) {
	n := newTestNode(t)
	now := time.Now()
	sum, err := ensureDownstreamCert(n.cfg, now)
	if err != nil || len(sum) != 64 {
		t.Fatalf("sum %q, err %v", sum, err)
	}
	crt, key := DownstreamCertPaths(n.cfg)
	if st, err := os.Stat(key); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key: %v %v", st, err)
	}
	pair, err := tls.LoadX509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if err := leaf.VerifyHostname("pooler.supavise.test"); err != nil {
		t.Errorf("the certificate must list the pooler host: %v", err)
	}
	if again, err := ensureDownstreamCert(n.cfg, now.Add(24*time.Hour)); err != nil || again != sum {
		t.Fatalf("a valid certificate must be kept: %q vs %q (%v)", again, sum, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(crt))
	if len(ents) != 2 {
		t.Errorf("temporary files left behind: %v", ents)
	}
}

func TestDownstreamCertIsReplacedWhenItNearsItsEndOrBreaks(t *testing.T) {
	n := newTestNode(t)
	now := time.Now()
	first, err := ensureDownstreamCert(n.cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	// 20 days before the end: inside the renewal window.
	second, err := ensureDownstreamCert(n.cfg, now.Add(downstreamCertValidity-20*24*time.Hour))
	if err != nil || second == first {
		t.Fatalf("an expiring certificate must be replaced (%q, %v)", second, err)
	}
	// A key that does not match the certificate (a crash between the two writes).
	_, key := DownstreamCertPaths(n.cfg)
	if err := os.WriteFile(key, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := ensureDownstreamCert(n.cfg, now)
	if err != nil || third == second {
		t.Fatalf("a broken pair must be replaced (%q, %v)", third, err)
	}
	// The pooler host changed (a new domain).
	n.cfg.Domain = "other.test"
	if fourth, err := ensureDownstreamCert(n.cfg, now); err != nil || fourth == third {
		t.Fatalf("a certificate for another host must be replaced: %v", err)
	}
}

// The unit must restart when the certificate is replaced: the hash is in its environment.
func TestSupavisorSpecCarriesTheCertificateHash(t *testing.T) {
	n := newTestNode(t)
	c := testCreds(t, n)
	d := n.deps()
	d.Artifacts, d.Supervisor = allArtifacts(), newFakeSupervisor()
	m, err := NewManager(d)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := m.spec("supavisor", c, false)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := m.spec("supavisor", c, false)
	if len(s1.Env[downstreamCertMarkerEnv]) != 64 || s1.Env[downstreamCertMarkerEnv] != s2.Env[downstreamCertMarkerEnv] {
		t.Fatalf("marker %q / %q", s1.Env[downstreamCertMarkerEnv], s2.Env[downstreamCertMarkerEnv])
	}
	crt, key := DownstreamCertPaths(n.cfg)
	os.Remove(crt)
	os.Remove(key)
	s3, err := m.spec("supavisor", c, false)
	if err != nil || s3.Env[downstreamCertMarkerEnv] == s1.Env[downstreamCertMarkerEnv] {
		t.Fatalf("a new certificate must change the environment: %v", err)
	}
}
