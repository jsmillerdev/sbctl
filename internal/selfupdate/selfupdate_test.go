package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type releaseServer struct {
	*httptest.Server
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	tag  string
	bin  []byte
	// tamper hooks
	sums, sig []byte
	manifest  []byte
	omit      map[string]bool
	calls     map[string]int
}

// rebuild writes the manifest for the server's tag (unless a test set its own) and signs a fresh
// checksum list over the binary and the manifest with the server's key.
func (r *releaseServer) rebuild(t *testing.T) {
	t.Helper()
	if r.manifest == nil {
		m := &Manifest{Schema: ManifestSchema, Version: r.tag, MinUpgradeFrom: "v0.0.0", Studio: "2026.10.05-sha-94b8b06",
			Artifacts: map[string]string{"auth": "auth-v2.195.0-r1"}}
		b, err := m.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		r.manifest = b
	}
	r.sums = r.sumsFor(r.bin, r.manifest)
	r.sig = ed25519.Sign(r.priv, r.sums)
}

func (r *releaseServer) sumsFor(bin, manifest []byte) []byte {
	sum, msum := sha256.Sum256(bin), sha256.Sum256(manifest)
	return []byte(fmt.Sprintf("%s  supavise-linux-amd64\n%s  supavise-linux-arm64\n%s  studio-x.tar.zst\n%s  supavise-release.json\n",
		hex.EncodeToString(sum[:]), strings.Repeat("a", 64), strings.Repeat("b", 64), hex.EncodeToString(msum[:])))
}

func newReleaseServer(t *testing.T, tag, binary string) *releaseServer {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &releaseServer{pub: pub, priv: priv, tag: tag, bin: []byte(binary), omit: map[string]bool{}, calls: map[string]int{}}
	r.rebuild(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/", func(w http.ResponseWriter, req *http.Request) {
		r.calls["meta"]++
		path := strings.TrimPrefix(req.URL.Path, "/repos/o/r/releases/")
		if path != "latest" && path != "tags/"+r.tag {
			http.NotFound(w, req)
			return
		}
		assets := []map[string]string{}
		for _, n := range []string{"SHA256SUMS", "SHA256SUMS.sig", "supavise-release.json", "supavise-linux-amd64", "supavise-linux-arm64"} {
			if !r.omit[n] {
				assets = append(assets, map[string]string{"name": n, "browser_download_url": r.URL + "/dl/" + n})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": r.tag, "assets": assets})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, req *http.Request) {
		n := strings.TrimPrefix(req.URL.Path, "/dl/")
		r.calls[n]++
		switch n {
		case "SHA256SUMS":
			_, _ = w.Write(r.sums)
		case "SHA256SUMS.sig":
			_, _ = w.Write(r.sig)
		case "supavise-release.json":
			_, _ = w.Write(r.manifest)
		case "supavise-linux-amd64":
			_, _ = w.Write(r.bin)
		default:
			http.NotFound(w, req)
		}
	})
	r.Server = httptest.NewServer(mux)
	t.Cleanup(r.Close)
	return r
}

func (r *releaseServer) opts(t *testing.T, current string) (Options, string) {
	exe := filepath.Join(t.TempDir(), "supavise")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{Repo: "o/r", APIBase: r.URL, Platform: "linux-amd64", Current: current, ExecPath: exe, Key: r.pub,
		Probe: func(context.Context, string) (string, error) { return "supavise version " + r.tag, nil }}, exe
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateReplacesTheBinary(t *testing.T) {
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, exe := r.opts(t, "v1.1.0")
	res, err := Update(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replaced || res.Tag != "v1.2.0" {
		t.Fatalf("result %+v", res)
	}
	if read(t, exe) != "new binary" {
		t.Fatal("binary not replaced")
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if res.Previous == "" || read(t, res.Previous) != "old binary" {
		t.Fatalf("previous binary not kept: %q", res.Previous)
	}
	ents, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".new-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestUpdateUpToDate(t *testing.T) {
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, exe := r.opts(t, "v1.2.0")
	res, err := Update(context.Background(), o)
	if err != nil || res.Replaced {
		t.Fatalf("%+v %v", res, err)
	}
	if read(t, exe) != "old binary" || r.calls["supavise-linux-amd64"] != 0 {
		t.Fatal("downloaded or replaced although current")
	}
	o.Force = true
	if res, err := Update(context.Background(), o); err != nil || !res.Replaced {
		t.Fatalf("forced: %+v %v", res, err)
	}
}

func TestUpdateRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(r *releaseServer)
		want   string
	}{
		"bad signature": {func(r *releaseServer) { r.sig[0] ^= 1 }, "does not verify"},
		"signed by another key": {func(r *releaseServer) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			r.sig = ed25519.Sign(other, r.sums)
		}, "does not verify"},
		"sums tampered after signing": {func(r *releaseServer) { r.sums = append([]byte("# x\n"), r.sums...) }, "does not verify"},
		"no manifest asset":           {func(r *releaseServer) { r.omit["supavise-release.json"] = true }, "no asset supavise-release.json"},
		"short signature":             {func(r *releaseServer) { r.sig = r.sig[:10] }, "ed25519 signature"},
		"binary tampered":             {func(r *releaseServer) { r.bin = []byte("evil binary") }, "does not match its checksum"},
		"manifest tampered": {func(r *releaseServer) {
			r.manifest = []byte(strings.Replace(string(r.manifest), `"v0.0.0"`, `"v0.0.1"`, 1))
		}, "supavise-release.json does not match its checksum"},
		"manifest of another release": {func(t *releaseServer) {
			t.manifest = []byte(strings.Replace(string(t.manifest), `"version": "v1.2.0"`, `"version": "v1.0.0"`, 1))
			t.sums = t.sumsFor(t.bin, t.manifest)
			t.sig = ed25519.Sign(t.priv, t.sums)
		}, "carries the signed manifest of v1.0.0"},
		"manifest not in the signed list": {func(r *releaseServer) {
			r.sums = []byte(strings.Repeat("a", 64) + "  supavise-linux-amd64\n")
			r.sig = ed25519.Sign(r.priv, r.sums)
		}, "lists no checksum for supavise-release.json"},
		"no signature asset": {func(r *releaseServer) { r.omit["SHA256SUMS.sig"] = true }, "no asset SHA256SUMS.sig"},
		"no sums asset":      {func(r *releaseServer) { r.omit["SHA256SUMS"] = true }, "no asset SHA256SUMS"},
		"no binary":          {func(r *releaseServer) { r.omit["supavise-linux-amd64"] = true }, "no asset supavise-linux-amd64"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReleaseServer(t, "v1.2.0", "new binary")
			tc.mutate(r)
			o, exe := r.opts(t, "v1.0.0")
			_, err := Update(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if read(t, exe) != "old binary" {
				t.Fatal("the installed binary was touched")
			}
			ents, _ := os.ReadDir(filepath.Dir(exe))
			if len(ents) != 1 {
				t.Fatalf("files left next to the binary: %v", ents)
			}
		})
	}
}

func TestUpdateChecksumListWithoutThePlatform(t *testing.T) {
	r := newReleaseServer(t, "v1.2.0", "new binary")
	msum := sha256.Sum256(r.manifest)
	r.sums = []byte(strings.Repeat("a", 64) + "  supavise-linux-arm64\n" + hex.EncodeToString(msum[:]) + "  supavise-release.json\n")
	r.sig = ed25519.Sign(r.priv, r.sums)
	o, _ := r.opts(t, "v1.0.0")
	if _, err := Update(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no checksum for supavise-linux-amd64") {
		t.Fatalf("%v", err)
	}
}

func TestUpdateProbeFailureKeepsTheOldBinary(t *testing.T) {
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, exe := r.opts(t, "v1.0.0")
	o.Probe = func(context.Context, string) (string, error) { return "", fmt.Errorf("exec format error") }
	if _, err := Update(context.Background(), o); err == nil || !strings.Contains(err.Error(), "does not start") {
		t.Fatalf("%v", err)
	}
	if read(t, exe) != "old binary" {
		t.Fatal("old binary replaced")
	}
}

func TestUpdateExplicitTag(t *testing.T) {
	r := newReleaseServer(t, "v1.0.5", "older binary")
	o, exe := r.opts(t, "v1.2.0")
	o.Tag = "v1.0.5"
	if res, err := Update(context.Background(), o); err != nil || !res.Replaced {
		t.Fatalf("pinned older tag: %+v %v", res, err)
	}
	if read(t, exe) != "older binary" {
		t.Fatal("not installed")
	}
	o.Tag = "v9.9.9"
	if _, err := Update(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
}

func TestNoKeyRefusesToUpdate(t *testing.T) {
	r := newReleaseServer(t, "v1.2.0", "new binary")
	o, exe := r.opts(t, "v1.0.0")
	o.Key = nil
	withoutEmbeddedKeys(t) // as in a checkout whose release key is still the placeholder
	if _, err := Update(context.Background(), o); err == nil || err != ErrNoKey {
		t.Fatalf("%v", err)
	}
	if read(t, exe) != "old binary" || r.calls["meta"] != 0 {
		t.Fatal("went to the network without a key")
	}
}

func TestParsePublicKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	got, err := ParsePublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if err != nil || !got.Equal(pub) {
		t.Fatalf("%v", err)
	}
	if _, err := ParsePublicKey([]byte("UNSET\n")); err != ErrNoKey {
		t.Fatalf("placeholder: %v", err)
	}
	if _, err := EmbeddedKey(); err != ErrNoKey {
		t.Logf("embedded key is set (%v): the release key was committed", err)
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		tag, cur string
		want     bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.10.0", false},
		{"v1.10.0", "v1.9.9", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.2.0", "dev", true},
		{"v1.2.0", "", true},
		{"v1.2.0", "v1.2.0-3-gabcdef", false},
		{"v1.2.1", "v1.2.0-3-gabcdef-dirty", true},
		{"nightly", "v1.0.0", false},
	} {
		if got := Newer(tc.tag, tc.cur); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.tag, tc.cur, got, tc.want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte(strings.Repeat("a", 64) + "  one\n" + strings.Repeat("B", 64) + " *two\nbad line\n" + "zz" + strings.Repeat("c", 62) + "  three\n")
	if h, err := ChecksumFor(sums, "one"); err != nil || h != strings.Repeat("a", 64) {
		t.Fatalf("%q %v", h, err)
	}
	if h, err := ChecksumFor(sums, "two"); err != nil || h != strings.Repeat("b", 64) {
		t.Fatalf("binary-mode marker and case: %q %v", h, err)
	}
	if _, err := ChecksumFor(sums, "three"); err == nil {
		t.Fatal("non-hex checksum accepted")
	}
	if _, err := ChecksumFor(sums, "four"); err == nil {
		t.Fatal("missing name accepted")
	}
}

// TestVerifiesWhatOpenSSLSigned pins the interoperability the release depends on:
// deploy/release-assets.sh signs with `openssl pkeyutl -sign -rawin`, install.sh verifies
// with openssl and `supavise self-update` verifies with Go's crypto/ed25519. The fixture was
// made by release-assets.sh with a throwaway key whose private half was discarded.
func TestVerifiesWhatOpenSSLSigned(t *testing.T) {
	pemB, _ := os.ReadFile("testdata/openssl-test-key.pub.pem")
	sums, _ := os.ReadFile("testdata/openssl-SHA256SUMS")
	sig, _ := os.ReadFile("testdata/openssl-SHA256SUMS.sig")
	key, err := ParsePublicKey(pemB)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySums(key, sums, sig); err != nil {
		t.Fatalf("openssl signature rejected: %v", err)
	}
	sums[0] ^= 1
	if err := VerifySums(key, sums, sig); err == nil {
		t.Fatal("tampered list accepted")
	}
	// The fixture is signed, so its asset names stay the names it was signed with.
	if h, err := ChecksumFor(sums, "sbctl-linux-arm64"); err != nil || len(h) != 64 {
		t.Fatalf("%q %v", h, err)
	}
}

func TestUpdateRefusesAnOlderBinaryUnderANewerTag(t *testing.T) {
	// An attacker who can publish a release attaches the signed assets of v1.0.0 to v9.0.0.
	r := newReleaseServer(t, "v9.0.0", "old signed binary")
	o, exe := r.opts(t, "v1.1.0")
	o.Probe = func(context.Context, string) (string, error) { return "supavise version v1.0.0\n", nil }
	_, err := Update(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "refusing to install") || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("%v", err)
	}
	if read(t, exe) != "old binary" {
		t.Fatal("the installed binary was replaced")
	}
	if entries, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".supavise.new-*")); len(entries) != 0 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestReportsVersion(t *testing.T) {
	for _, tc := range []struct {
		out, tag string
		ok       bool
	}{
		{"supavise version v1.2.3\n", "v1.2.3", true},
		{"supavise version v1.2.30", "v1.2.3", false},
		{"supavise version dev", "v1.2.3", false},
		{"", "v1.2.3", false},
	} {
		if got := ReportsVersion(tc.out, tc.tag); got != tc.ok {
			t.Errorf("%q vs %s: %v", tc.out, tc.tag, got)
		}
	}
}

// withoutEmbeddedKeys makes the embedded release keys placeholders for the rest of the test, so
// the no-key paths are tested whatever key this checkout carries.
func withoutEmbeddedKeys(t *testing.T) {
	t.Helper()
	cur, next := releaseKeyPEM, releaseKeyNextPEM
	releaseKeyPEM, releaseKeyNextPEM = []byte("UNSET\n"), []byte("UNSET\n")
	t.Cleanup(func() { releaseKeyPEM, releaseKeyNextPEM = cur, next })
}
