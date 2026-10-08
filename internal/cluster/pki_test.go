package cluster

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/secrets"
)

func newSecrets(t testing.TB) *secrets.AESGCM {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Two nodes that hold the same master key compute the same CA, byte for byte, and so the same
// pin; another key gives another CA.
func TestCAIsIdenticalOnEveryNodeThatHoldsTheKey(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	a, _ := secrets.New(key)
	b, _ := secrets.New(key)
	ca1, err := NewCA(a)
	if err != nil {
		t.Fatal(err)
	}
	ca2, _ := NewCA(b)
	if !bytes.Equal(ca1.DER(), ca2.DER()) || ca1.Fingerprint() != ca2.Fingerprint() || len(ca1.Fingerprint()) != 64 {
		t.Fatal("the CA differs between two nodes with one key")
	}
	other, _ := NewCA(newSecrets(t))
	if bytes.Equal(ca1.DER(), other.DER()) {
		t.Fatal("two keys gave one CA")
	}
	c := ca1.Cert()
	if !c.IsCA || c.SerialNumber.Int64() != 1 || !c.NotAfter.After(time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)) || c.Subject.CommonName == "" {
		t.Fatalf("the CA certificate: %+v", c)
	}
	// The same key built a second time long after gives the same bytes: nothing in it depends on the clock.
	time.Sleep(10 * time.Millisecond)
	if again, _ := NewCA(a); !bytes.Equal(again.DER(), ca1.DER()) {
		t.Fatal("the CA changed over time")
	}
	if want := mesh.Fingerprint(ca1.DER()); ca1.Fingerprint() != want {
		t.Fatal("fingerprint")
	}
}

type shortDeriver struct{ n int }

func (d shortDeriver) Derive(label string) []byte { return bytes.Repeat([]byte{9}, d.n) }

// A Derive that returns other than 32 bytes still gives one stable CA, through HKDF-expand.
func TestCASeedExpandsWhateverDeriveReturns(t *testing.T) {
	for _, n := range []int{16, 48, 64} {
		a, err := NewCA(shortDeriver{n})
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		b, _ := NewCA(shortDeriver{n})
		if !bytes.Equal(a.DER(), b.DER()) {
			t.Errorf("%d bytes: not stable", n)
		}
	}
	x, _ := NewCA(shortDeriver{16})
	y, _ := NewCA(shortDeriver{48})
	if bytes.Equal(x.DER(), y.DER()) {
		t.Error("different material gave one CA")
	}
	if _, err := NewCA(shortDeriver{0}); err == nil {
		t.Error("a Derive of nothing gave a CA")
	}
}

func TestIssuedCertificatesVerifyAndNameTheirNode(t *testing.T) {
	ca, _ := NewCA(newSecrets(t))
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	iss, err := ca.Issue(pub, "n7", now, NodeCertTTL)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(iss.DER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert())
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			t.Errorf("usage %v: %v", usage, err)
		}
	}
	if id, ok := mesh.NodeIDOf(leaf); !ok || id != "n7" {
		t.Fatalf("node id %q %v", id, ok)
	}
	if iss.Serial != mesh.SerialString(leaf.SerialNumber) || leaf.SerialNumber.Int64() == 1 {
		t.Fatalf("serial %s", iss.Serial)
	}
	if got := leaf.NotAfter.Sub(now); got < NodeCertTTL-time.Minute || got > NodeCertTTL+time.Minute {
		t.Fatalf("validity %s", got)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now.Add(NodeCertTTL + time.Hour)}); err == nil {
		t.Fatal("an expired certificate verified")
	}
	// Another cluster's CA does not vouch for it.
	other, _ := NewCA(newSecrets(t))
	pool := x509.NewCertPool()
	pool.AddCert(other.Cert())
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now}); err == nil {
		t.Fatal("a certificate verified against another CA")
	}
	if _, err := ca.Issue(pub, "node-7", now, time.Hour); err == nil {
		t.Fatal("a certificate for a name that is not a node id")
	}

	// A request: only its key counts; a request signed with another key, or an RSA one, is refused.
	csr, _ := NewCSR(key, "whatever-name")
	got, err := ca.IssueCSR(csr, "n8", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := x509.ParseCertificate(got.DER); c.Subject.CommonName == "whatever-name" || !c.PublicKey.(ed25519.PublicKey).Equal(key.Public()) {
		t.Fatalf("the certificate takes more than the key from the request: %q", c.Subject.CommonName)
	}
	if _, err := ca.IssueCSR([]byte("junk"), "n8", now, time.Hour); err == nil {
		t.Fatal("junk is not a request")
	}
	bad := append([]byte(nil), csr...)
	bad[len(bad)-1] ^= 0xff
	if _, err := ca.IssueCSR(bad, "n8", now, time.Hour); err == nil {
		t.Fatal("a request with a broken signature")
	}
}

func TestTokenRoundTripAndDamage(t *testing.T) {
	tok := Token{V: 1, ID: "k3j9d2h8a1x7q", Leader: "203.0.113.5:7443", CAFpr: strings.Repeat("ab", 32), Secret: "c2VjcmV0", Exp: time.Now().Add(time.Hour).Unix(), Region: "eu-west-1", Name: "replica"}
	enc := tok.Encode()
	if !strings.HasPrefix(enc, "svj1.") || strings.ContainsAny(enc, " \n=+/") {
		t.Fatalf("token %q", enc)
	}
	got, err := ParseToken("  " + enc + "\n")
	if err != nil || got != tok {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if !got.Expires().After(time.Now()) {
		t.Fatal("expires")
	}
	for name, s := range map[string]string{
		"empty":         "",
		"no prefix":     enc[5:],
		"not base64":    "svj1.!!!",
		"not json":      "svj1.bm90IGpzb24",
		"other version": Token{V: 2, ID: "x", CAFpr: "y", Secret: "z", Leader: "h:1"}.Encode(),
		"no id":         Token{V: 1, CAFpr: "y", Secret: "z", Leader: "h:1"}.Encode(),
		"no secret":     Token{V: 1, ID: "x", CAFpr: "y", Leader: "h:1"}.Encode(),
		"no address":    Token{V: 1, ID: "x", CAFpr: "y", Secret: "z", Leader: "nohost"}.Encode(),
	} {
		if _, err := ParseToken(s); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// The proof depends on every part, and parts cannot trade bytes.
func TestProofBindsNonceRequestAndName(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	base := Proof(secret, []byte("nonce"), []byte("csr"), "node-a")
	for name, other := range map[string][]byte{
		"nonce":  Proof(secret, []byte("nonce2"), []byte("csr"), "node-a"),
		"csr":    Proof(secret, []byte("nonce"), []byte("csr2"), "node-a"),
		"name":   Proof(secret, []byte("nonce"), []byte("csr"), "node-b"),
		"secret": Proof([]byte("another secret, also 32 bytes!!!"), []byte("nonce"), []byte("csr"), "node-a"),
		"moved":  Proof(secret, []byte("noncec"), []byte("sr"), "node-a"),
		"moved2": Proof(secret, []byte("nonce"), []byte("csrn"), "ode-a"),
	} {
		if bytes.Equal(base, other) {
			t.Errorf("a change of the %s kept the proof", name)
		}
	}
	if !bytes.Equal(base, Proof(secret, []byte("nonce"), []byte("csr"), "node-a")) {
		t.Fatal("the proof is not deterministic")
	}
	// The secret of a token is derived: the same id gives the same secret on any node with the key,
	// and different ids different secrets.
	s := newSecrets(t)
	if !bytes.Equal(tokenSecret(s, "abc"), tokenSecret(s, "abc")) || bytes.Equal(tokenSecret(s, "abc"), tokenSecret(s, "abd")) {
		t.Fatal("token secrets")
	}
}

func TestVersionWindow(t *testing.T) {
	pg17 := map[string]string{"postgres": "postgres-17.11.0.004-r1"}
	pg18 := map[string]string{"postgres": "postgres-18.1.0.001-r0"}
	for _, tc := range []struct {
		a, b   string
		pa, pb map[string]string
		ok     bool
	}{
		{"v0.2.0", "v0.2.0", pg17, pg17, true},
		{"v0.2.3", "v0.2.0", pg17, pg17, true},
		{"v0.3.0", "v0.2.9", pg17, pg17, true},
		{"v0.2.0", "v0.3.0", pg17, pg17, true},
		{"v0.4.0", "v0.2.0", pg17, pg17, false},
		{"v0.2.0", "v0.4.1", pg17, pg17, false},
		{"v1.0.0", "v0.9.0", pg17, pg17, false},
		{"v0.2.0-rc1", "v0.2.0+sha", pg17, pg17, true},
		{"dev", "v0.9.0", pg17, pg17, true},
		{"", "", nil, nil, true},
		{"v0.2.0", "v0.2.0", pg17, pg18, false},
		{"v0.2.0", "v0.2.0", pg17, nil, true},
		{"v0.2.0", "v0.2.0", map[string]string{"postgres": "weird"}, pg17, true},
	} {
		err := CheckVersionWindow(tc.a, tc.b, tc.pa, tc.pb)
		if (err == nil) != tc.ok {
			t.Errorf("%q vs %q (%v / %v): %v, want ok=%v", tc.a, tc.b, tc.pa, tc.pb, err, tc.ok)
		}
	}
}

// The CA a key derives must be the same bytes in every build of every release, because a join token
// pins its hash and nodes of different releases join one cluster. A toolchain whose certificate
// encoding differs would fail here instead of splitting a cluster.
func TestCAFingerprintIsStableAcrossBuilds(t *testing.T) {
	s, err := secrets.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := NewCA(s)
	if err != nil {
		t.Fatal(err)
	}
	const want = "256c9c793279f2e7cf0c9882fb0cead20a039cff0b66816951a18b39784e866d"
	if got := ca.Fingerprint(); got != want {
		t.Fatalf("the CA of a fixed key hashes to %s, want %s: certificate encoding changed, and nodes built differently would pin different CAs", got, want)
	}
}
