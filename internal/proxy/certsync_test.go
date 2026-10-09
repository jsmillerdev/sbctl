package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// testCert makes a self-signed certificate for names, valid for sixty days, with a serial of its own.
func testCert(t *testing.T, names ...string) (crtPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signFor(t, key, names)
}

func signFor(t *testing.T, key *ecdsa.PrivateKey, names []string) (crtPEM, keyPEM []byte) {
	t.Helper()
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(60 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
}

// writeSite stores a certificate the way CertMagic's file storage does.
func writeSite(t *testing.T, dir, issuerKey, name string, crt, key []byte) {
	t.Helper()
	safe := certmagic.StorageKeys.Safe(name)
	site := filepath.Join(dir, "certificates", certmagic.StorageKeys.Safe(issuerKey), safe)
	if err := os.MkdirAll(site, 0o700); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"sans": []string{name}})
	for ext, data := range map[string][]byte{".crt": crt, ".key": key, ".json": meta} {
		if err := os.WriteFile(filepath.Join(site, safe+ext), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tree lists the files below dir with their contents, for comparing two stores.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return out
}

// muxRPC is a mesh.RPC whose far end is a Mux in this process: the request goes through the peer
// handler as if the node n2 had sent it.
type muxRPC struct {
	mux  *mesh.Mux
	peer string

	mu    sync.Mutex
	calls []string // path and query of every call
}

func (m *muxRPC) Call(ctx context.Context, node, method, path string, in, out any) error {
	m.mu.Lock()
	m.calls = append(m.calls, path)
	m.mu.Unlock()
	req := httptest.NewRequest(method, path, nil)
	if m.peer != "" {
		req = req.WithContext(mesh.WithPeer(req.Context(), mesh.Peer{Node: m.peer}))
	}
	rec := httptest.NewRecorder()
	m.mux.ServeHTTP(rec, req)
	if rec.Code/100 != 2 {
		var pe peerapi.Error
		_ = json.Unmarshal(rec.Body.Bytes(), &pe)
		return &mesh.RemoteError{Node: node, Status: rec.Code, Message: pe.Message}
	}
	if out != nil && rec.Body.Len() > 0 {
		return json.Unmarshal(rec.Body.Bytes(), out)
	}
	return nil
}

func (m *muxRPC) paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// leaderStore is a leader's CertMagic store behind the real handler.
type leaderStore struct {
	dir    string
	leader atomic.Bool
	rpc    *muxRPC
	src    MeshCerts
}

func newLeaderStore(t *testing.T) *leaderStore {
	t.Helper()
	l := &leaderStore{dir: t.TempDir()}
	l.leader.Store(true)
	mux := mesh.NewMux()
	mux.Handle("GET "+peerapi.PathCerts, CertsHandler(l.dir, l.leader.Load, quietLog()))
	l.rpc = &muxRPC{mux: mux, peer: "n2"}
	l.src = MeshCerts{RPC: l.rpc, Leader: func() (string, bool) { return "n1", true }}
	return l
}

func TestCertsHandler(t *testing.T) {
	l := newLeaderStore(t)
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, "acme.test-dir", "api.example.com", crt, key)
	writeFile(t, filepath.Join(l.dir, "acme", "acme.test-dir", "users", "default", "default.key"), "account key")
	// What belongs to the node that makes it is not offered.
	writeFile(t, filepath.Join(l.dir, "locks", "issue_cert_api.example.com.lock"), "lock")
	writeFile(t, filepath.Join(l.dir, "ocsp", "api.example.com-abc"), "staple")
	writeFile(t, filepath.Join(l.dir, "acme", "acme.test-dir", "challenge_tokens", "api.example.com.json"), "token")

	get := func(headers map[string]string, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", peerapi.PathCerts+query, nil)
		req = req.WithContext(mesh.WithPeer(req.Context(), mesh.Peer{Node: "n2"}))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		l.rpc.mux.ServeHTTP(rec, req)
		return rec
	}

	rec := get(nil, "")
	if rec.Code != 200 || rec.Header().Get("ETag") == "" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var snap peerapi.CertSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range snap.Files {
		paths = append(paths, f.Path)
		if f.Mode != 0o600 {
			t.Errorf("%s: mode %o", f.Path, f.Mode)
		}
	}
	want := []string{
		"acme/acme.test-dir/users/default/default.key",
		"certificates/acme.test-dir/api.example.com/api.example.com.crt",
		"certificates/acme.test-dir/api.example.com/api.example.com.json",
		"certificates/acme.test-dir/api.example.com/api.example.com.key",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("files:\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
	etag := rec.Header().Get("ETag")
	if etag != `"`+snapshotTag(snap)+`"` {
		t.Errorf("ETag %s is not the tag of the snapshot %s", etag, snapshotTag(snap))
	}

	// The tag in If-None-Match, weak or in a list, or in the etag parameter, ends in 304.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"If-None-Match":      get(map[string]string{"If-None-Match": etag}, ""),
		"weak, in a list":    get(map[string]string{"If-None-Match": `"other", W/` + etag}, ""),
		"etag parameter":     get(nil, "?etag="+strings.Trim(etag, `"`)),
		"star":               get(map[string]string{"If-None-Match": "*"}, ""),
		"etag, quoted, wild": get(nil, "?etag="+`%22`+strings.Trim(etag, `"`)+`%22`),
	} {
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
	if rec := get(map[string]string{"If-None-Match": `"stale"`}, ""); rec.Code != 200 {
		t.Errorf("a stale tag: %d", rec.Code)
	}
	// A change of content changes the tag.
	crt2, key2 := testCert(t, "api.example.com")
	writeSite(t, l.dir, "acme.test-dir", "api.example.com", crt2, key2)
	if rec := get(map[string]string{"If-None-Match": etag}, ""); rec.Code != 200 || rec.Header().Get("ETag") == etag {
		t.Errorf("after a renewal: %d %s", rec.Code, rec.Header().Get("ETag"))
	}

	// Only a node that authenticated with a certificate may ask, and only of the leader.
	req := httptest.NewRequest("GET", peerapi.PathCerts, nil)
	anon := httptest.NewRecorder()
	l.rpc.mux.ServeHTTP(anon, req)
	if anon.Code != http.StatusForbidden || strings.Contains(anon.Body.String(), "BEGIN") {
		t.Errorf("without a node certificate: %d %s", anon.Code, anon.Body.String())
	}
	req = req.WithContext(mesh.WithPeer(req.Context(), mesh.Peer{}))
	anon = httptest.NewRecorder()
	l.rpc.mux.ServeHTTP(anon, req)
	if anon.Code != http.StatusForbidden {
		t.Errorf("a peer with no node id: %d", anon.Code)
	}
	l.leader.Store(false)
	rec = get(nil, "")
	var pe peerapi.Error
	_ = json.Unmarshal(rec.Body.Bytes(), &pe)
	if rec.Code != http.StatusConflict || pe.Code != "not_leader" || strings.Contains(rec.Body.String(), "BEGIN") {
		t.Errorf("a node that is not the leader: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCertStoreLeavesOutSitesBeingWritten(t *testing.T) {
	dir := t.TempDir()
	issuer := "acme.test-dir"
	crt, key := testCert(t, "good.example.com")
	writeSite(t, dir, issuer, "good.example.com", crt, key)
	// A renewal in the middle: the certificate is new, the key is still the old one.
	crt2, _ := testCert(t, "torn.example.com")
	_, key3 := testCert(t, "torn.example.com")
	writeSite(t, dir, issuer, "torn.example.com", crt2, key3)
	// A site with no metadata yet, and one with no key.
	crt4, key4 := testCert(t, "nometa.example.com")
	writeSite(t, dir, issuer, "nometa.example.com", crt4, key4)
	if err := os.Remove(filepath.Join(dir, "certificates", issuer, "nometa.example.com", "nometa.example.com.json")); err != nil {
		t.Fatal(err)
	}
	crt5, key5 := testCert(t, "nokey.example.com")
	writeSite(t, dir, issuer, "nokey.example.com", crt5, key5)
	if err := os.Remove(filepath.Join(dir, "certificates", issuer, "nokey.example.com", "nokey.example.com.key")); err != nil {
		t.Fatal(err)
	}
	files, err := readCertStore(dir, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.Contains(f.Path, "good.example.com") {
			t.Errorf("%s is in the snapshot", f.Path)
		}
	}
	if len(files) != 3 {
		t.Errorf("%d files, want the three of the good site", len(files))
	}
	// A store that does not exist yet is an empty snapshot, not an error.
	if files, err := readCertStore(filepath.Join(dir, "nowhere"), quietLog()); err != nil || len(files) != 0 {
		t.Errorf("missing store: %d files, %v", len(files), err)
	}
}

func newMirrorOf(l *leaderStore, dir string) *certMirror {
	return newCertMirror(dir, &CertSync{Source: l.src, Interval: time.Hour}, quietLog())
}

// A snapshot that could not be written is applied again on the next fetch, even though the leader
// answers 304 to its tag; once it is on disk, a 304 does nothing.
func TestMirrorAppliesAFailedSnapshotOnTheNextNotModified(t *testing.T) {
	ctx := context.Background()
	l := newLeaderStore(t)
	issuer := "acme.test-dir"
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, issuer, "api.example.com", crt, key)

	local := t.TempDir()
	m := newMirrorOf(l, local)
	told := 0
	m.onSnapshot = func(context.Context, peerapi.CertSnapshot) { told++ }
	if err := m.sync(ctx); err != nil || told != 1 {
		t.Fatalf("the first fetch: %v, onSnapshot told %d times", err, told)
	}

	// A renewal arrives while a file sits where the site's directory belongs: the apply fails.
	site := filepath.Join(local, "certificates", issuer, "api.example.com")
	if err := os.RemoveAll(site); err != nil {
		t.Fatal(err)
	}
	writeFile(t, site, "in the way")
	crtN, keyN := testCert(t, "api.example.com")
	writeSite(t, l.dir, issuer, "api.example.com", crtN, keyN)
	if err := m.sync(ctx); err == nil {
		t.Fatal("the renewal was applied over a file")
	}
	if told != 1 {
		t.Fatalf("onSnapshot told %d times of a snapshot that is not on disk", told)
	}
	if err := os.Remove(site); err != nil {
		t.Fatal(err)
	}
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if paths := l.rpc.paths(); len(paths) != 3 || !strings.HasPrefix(paths[2], peerapi.PathCerts+"?etag=") {
		t.Fatalf("fetches: %v, want the third to carry the tag", paths)
	}
	if b, _ := os.ReadFile(filepath.Join(site, "api.example.com.crt")); string(b) != string(crtN) || told != 2 {
		t.Fatalf("after the retry: the renewal is on disk: %v, onSnapshot told %d times", string(b) == string(crtN), told)
	}
	if err := m.sync(ctx); err != nil || told != 2 {
		t.Fatalf("a 304 on an applied snapshot: %v, onSnapshot told %d times", err, told)
	}
}

func TestMirrorCopiesTheLeadersStoreByETag(t *testing.T) {
	ctx := context.Background()
	l := newLeaderStore(t)
	issuer := "acme.test-dir"
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, issuer, "api.example.com", crt, key)
	crtW, keyW := testCert(t, "*.api.example.com")
	writeSite(t, l.dir, issuer, "*.api.example.com", crtW, keyW)
	writeFile(t, filepath.Join(l.dir, "acme", issuer, "users", "default", "default.key"), "account")
	writeFile(t, filepath.Join(l.dir, "locks", "x.lock"), "lock")

	local := t.TempDir()
	// What the follower made itself stays: OCSP staples and locks are not mirrored.
	writeFile(t, filepath.Join(local, "ocsp", "staple"), "mine")
	writeFile(t, filepath.Join(local, "locks", "y.lock"), "mine")
	// What it had from an earlier life as a leader, and the leader does not have, goes.
	crtOld, keyOld := testCert(t, "old.example.com")
	writeSite(t, local, issuer, "old.example.com", crtOld, keyOld)

	m := newMirrorOf(l, local)
	var told []int
	m.onSnapshot = func(_ context.Context, snap peerapi.CertSnapshot) { told = append(told, len(snap.Files)) }
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	got := tree(t, local)
	if _, ok := got["certificates/"+issuer+"/old.example.com/old.example.com.crt"]; ok {
		t.Error("a certificate the leader does not have stayed")
	}
	if got["ocsp/staple"] != "mine" || got["locks/y.lock"] != "mine" {
		t.Errorf("the follower's own files were touched: %v", got)
	}
	if _, ok := got["locks/x.lock"]; ok {
		t.Error("a lock was mirrored")
	}
	for p, content := range tree(t, l.dir) {
		if strings.HasPrefix(p, "locks/") {
			continue
		}
		if got[p] != content {
			t.Errorf("%s differs", p)
		}
	}
	if _, err := os.Stat(filepath.Join(local, "certificates", issuer, "old.example.com")); !os.IsNotExist(err) {
		t.Errorf("the empty directory of the removed site stayed: %v", err)
	}
	fi, err := os.Stat(filepath.Join(local, "acme", issuer, "users", "default", "default.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("account key: %v %v", fi, err)
	}
	if paths := l.rpc.paths(); len(paths) != 1 || paths[0] != peerapi.PathCerts {
		t.Errorf("first fetch: %v", paths)
	}

	// Nothing changed: the follower sends the tag and the leader answers 304.
	before := tree(t, local)
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	paths := l.rpc.paths()
	if len(paths) != 2 || !strings.HasPrefix(paths[1], peerapi.PathCerts+"?etag=") {
		t.Errorf("second fetch: %v", paths)
	}
	if len(told) != 1 {
		t.Errorf("onSnapshot told %v: a 304 on a snapshot that is fully applied has nothing new to tell", told)
	}
	for p, c := range tree(t, local) {
		if before[p] != c {
			t.Errorf("%s changed on a 304", p)
		}
	}

	// A renewal: the new files arrive, the old ones are replaced.
	crtN, keyN := testCert(t, "api.example.com")
	writeSite(t, l.dir, issuer, "api.example.com", crtN, keyN)
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(local, "certificates", issuer, "api.example.com", "api.example.com.crt"))
	if string(b) != string(crtN) {
		t.Error("the renewed certificate was not mirrored")
	}
	// A certificate the leader removed goes.
	// (One absence is not enough: the leader may have been in the middle of writing it.)
	if err := os.RemoveAll(filepath.Join(l.dir, "certificates", issuer, "wildcard_.api.example.com")); err != nil {
		t.Fatal(err)
	}
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(local, "certificates", issuer, "wildcard_.api.example.com")); err != nil {
		t.Errorf("a certificate the leader did not send once was removed: %v", err)
	}
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(local, "certificates", issuer, "wildcard_.api.example.com")); !os.IsNotExist(err) {
		t.Errorf("a removed certificate stayed: %v", err)
	}

	// A leader with an empty store (it has not issued yet) does not empty the follower's.
	if err := os.RemoveAll(filepath.Join(l.dir, "certificates")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(l.dir, "acme")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(local, "certificates", issuer, "api.example.com", "api.example.com.crt")); err != nil {
		t.Errorf("an empty snapshot emptied the store: %v", err)
	}
}

// TestMirrorKeepsACertificateTheLeaderWasWriting: a snapshot that lacks a site once (the leader read it
// between CertMagic's writes) does not remove the follower's copy, and one that lacks it twice does.
func TestMirrorKeepsACertificateTheLeaderWasWriting(t *testing.T) {
	ctx := context.Background()
	l := newLeaderStore(t)
	issuer := "acme.test-dir"
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, issuer, "api.example.com", crt, key)
	crt2, key2 := testCert(t, "studio.example.com")
	writeSite(t, l.dir, issuer, "studio.example.com", crt2, key2)
	local := t.TempDir()
	m := newMirrorOf(l, local)
	var heard []int
	m.onSnapshot = func(_ context.Context, snap peerapi.CertSnapshot) { heard = append(heard, len(snap.Files)) }
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	apiKey := filepath.Join(local, "certificates", issuer, "api.example.com", "api.example.com.key")

	// The leader is in the middle of a renewal: the new certificate is there, the key is not yet.
	crtN, keyN := testCert(t, "api.example.com")
	writeSite(t, l.dir, issuer, "api.example.com", crtN, key) // the old key with the new certificate
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(apiKey); err != nil || string(b) != string(key) {
		t.Errorf("the follower's key after one torn snapshot: %v", err)
	}
	if got := heard[len(heard)-1]; got != 6 {
		t.Errorf("the follower was told of %d files, want the six it had", got)
	}
	// The renewal finishes.
	writeSite(t, l.dir, issuer, "api.example.com", crtN, keyN)
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(apiKey); string(b) != string(keyN) {
		t.Error("the finished renewal was not mirrored")
	}
	// A site that stays torn for two fetches is dropped; the others are untouched.
	writeSite(t, l.dir, issuer, "api.example.com", crt, keyN)
	for i := 0; i < 2; i++ {
		if err := m.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(apiKey); !os.IsNotExist(err) {
		t.Errorf("a certificate that stayed incomplete for two fetches stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "certificates", issuer, "studio.example.com", "studio.example.com.key")); err != nil {
		t.Errorf("another site: %v", err)
	}
}

func TestMirrorWorksWhenTheTransportDropsTheTag(t *testing.T) {
	l := newLeaderStore(t)
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, "acme.test-dir", "api.example.com", crt, key)
	var calls int
	src := sourceFunc(func(ctx context.Context, _ string) (peerapi.CertSnapshot, error) {
		calls++
		return l.src.Certs(ctx, "") // a transport that cannot carry the tag
	})
	m := newCertMirror(t.TempDir(), &CertSync{Source: src}, quietLog())
	var told int
	m.onSnapshot = func(context.Context, peerapi.CertSnapshot) { told++ }
	for i := 0; i < 3; i++ {
		if err := m.sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 || told != 3 {
		t.Errorf("%d fetches, %d notifications", calls, told)
	}
}

type sourceFunc func(ctx context.Context, etag string) (peerapi.CertSnapshot, error)

func (f sourceFunc) Certs(ctx context.Context, etag string) (peerapi.CertSnapshot, error) {
	return f(ctx, etag)
}

func TestMirrorRefusesPathsOutsideTheStore(t *testing.T) {
	outside := t.TempDir()
	local := filepath.Join(outside, "store")
	for _, p := range []string{
		"../escape", "certificates/../../escape", "/etc/passwd", "certificates", "other/x/y", "locks/x.lock", "ocsp/x",
		"certificates//x", "certificates/./x", "acme/challenge_tokens/x", "certificates/a\\b/c", "",
	} {
		snap := peerapi.CertSnapshot{Files: []peerapi.CertFile{
			{Path: "acme/ok/file", Data: []byte("fine")},
			{Path: p, Data: []byte("bad")},
		}}
		if err := applySnapshot(local, snap); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	// Nothing was written, not even the harmless file that came first.
	if got := tree(t, outside); len(got) != 0 {
		t.Errorf("files written: %v", got)
	}
}

func TestMeshCerts(t *testing.T) {
	l := newLeaderStore(t)
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, "acme.test-dir", "api.example.com", crt, key)

	snap, err := l.src.Certs(context.Background(), "")
	if err != nil || len(snap.Files) != 3 {
		t.Fatalf("%d files, %v", len(snap.Files), err)
	}
	if _, err := l.src.Certs(context.Background(), snapshotTag(snap)); !errors.Is(err, ErrCertsNotModified) {
		t.Errorf("the current tag: %v", err)
	}
	// A leader that is not one any more is an error the mirror retries, not a snapshot.
	l.leader.Store(false)
	var re *mesh.RemoteError
	if _, err := l.src.Certs(context.Background(), ""); !errors.As(err, &re) || re.Status != http.StatusConflict {
		t.Errorf("not the leader: %v", err)
	}
	none := MeshCerts{RPC: l.rpc, Leader: func() (string, bool) { return "", false }}
	if _, err := none.Certs(context.Background(), ""); err == nil {
		t.Error("no leader known: no error")
	}
}

func TestCertRole(t *testing.T) {
	r := NewCertRole(false)
	ctx, cancel := context.WithCancel(context.Background())
	ch := r.Watch(ctx)
	if got := <-ch; got {
		t.Error("starts managing")
	}
	r.Promote()
	r.Promote() // already managing: no second notification
	if got := <-ch; !got || !r.Managing() {
		t.Error("Promote")
	}
	select {
	case v := <-ch:
		t.Errorf("a repeated Promote notified: %v", v)
	case <-time.After(20 * time.Millisecond):
	}
	// A reader that was away sees the latest.
	r.Demote()
	r.Promote()
	r.Demote()
	if got := <-ch; got || r.Managing() {
		t.Error("Demote")
	}
	cancel()
	for range ch { // closes
	}
	r.Promote() // a role without watchers still switches
	if !r.Managing() {
		t.Error("Promote after the watcher left")
	}
}

// fakeIssuer stands in for the CA: it signs whatever it is asked for with a throwaway key.
type fakeIssuer struct {
	t     *testing.T
	key   string
	calls atomic.Int32
}

func (f *fakeIssuer) IssuerKey() string { return f.key }

func (f *fakeIssuer) Issue(_ context.Context, csr *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	f.calls.Add(1)
	ca, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: csr.DNSNames[0]}, DNSNames: csr.DNSNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(60 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, csr.PublicKey, ca)
	if err != nil {
		return nil, err
	}
	return &certmagic.IssuedCertificate{Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// handshakeCert connects to cm's TLS configuration over a pipe and returns the certificate the client got.
func handshakeCert(t *testing.T, cm *certManager, serverName string) (*x509.Certificate, error) {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	deadline := time.Now().Add(20 * time.Second)
	_ = c1.SetDeadline(deadline)
	_ = c2.SetDeadline(deadline)
	srv := tls.Server(c1, cm.tlsConfig())
	go func() { _ = srv.Handshake() }()
	cli := tls.Client(c2, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := cli.Handshake(); err != nil {
		return nil, err
	}
	return cli.ConnectionState().PeerCertificates[0], nil
}

// certsFor counts the distinct certificates the cache holds for exactly name.
func certsFor(cm *certManager, name string) int {
	seen := map[string]bool{}
	for _, c := range cm.cache.AllMatchingCertificates(name) {
		for _, nm := range c.Names {
			if nm == name {
				seen[c.Hash()] = true
			}
		}
	}
	return len(seen)
}

// TestFollowerServesMirroredCertificatesAndIssuesNothing runs the certificate manager of a follower
// over a leader's store, with a fake CA behind the on-demand config: the mirrored certificates are
// served, a renewal on the leader replaces them, a name nobody mirrored is refused without a call to
// the CA, and after a promotion the node issues.
func TestFollowerServesMirroredCertificatesAndIssuesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Domain, cfg.StateDir = "example.com", t.TempDir()
	cm, err := newCertManager(certOptions{
		cfg: cfg, mode: tlsAuto, provider: &cloudflare.Provider{APIToken: "t"}, log: quietLog(), follower: true,
		allow: func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cm.close()
	fake := &fakeIssuer{t: t, key: cm.httpIssuer.IssuerKey()}
	cm.http.Issuers = []certmagic.Issuer{fake}

	l := newLeaderStore(t)
	for _, n := range []string{"*.api.example.com", "api.example.com", "studio.example.com", "docs.customer.example"} {
		crt, key := testCert(t, n)
		writeSite(t, l.dir, fake.key, n, crt, key)
	}
	writeFile(t, filepath.Join(l.dir, "acme", fake.key, "users", "default", "default.key"), "account")
	m := newMirrorOf(l, cfg.Paths().Certs())
	m.onSnapshot = cm.loadMirrored
	cm.mirror = m

	// Before the first fetch there is nothing to serve, and asking for a name wakes the mirror.
	if _, err := handshakeCert(t, cm, "api.example.com"); err == nil {
		t.Fatal("a certificate before the mirror ran")
	}
	if _, err := handshakeCert(t, cm, "docs.customer.example"); err == nil {
		t.Fatal("a certificate before the mirror ran")
	}
	if len(m.poke) != 1 {
		t.Error("a handshake that found no certificate did not ask the mirror to fetch")
	}
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	for sni, want := range map[string]string{
		"api.example.com": "api.example.com", "studio.example.com": "studio.example.com",
		"abcdefghijklmnopqrst.api.example.com":              "*.api.example.com",
		"abcdefghijklmnopqrst-rr-eu-abc123.api.example.com": "*.api.example.com",
		"docs.customer.example":                             "docs.customer.example",
	} {
		c, err := handshakeCert(t, cm, sni)
		if err != nil {
			t.Errorf("%s: %v", sni, err)
			continue
		}
		if c.DNSNames[0] != want {
			t.Errorf("%s: got the certificate of %s, want %s", sni, c.DNSNames[0], want)
		}
	}

	// A name nobody mirrored is refused, and the CA hears nothing of it.
	if _, err := handshakeCert(t, cm, "new.customer.example"); err == nil {
		t.Fatal("a certificate for a name that is not mirrored")
	}
	if fake.calls.Load() != 0 {
		t.Fatalf("the CA was asked %d times by a node that mirrors", fake.calls.Load())
	}

	// The leader renews the wildcard: after the next fetch the follower serves the new one, only.
	old, _ := handshakeCert(t, cm, "x.api.example.com")
	crtN, keyN := testCert(t, "*.api.example.com")
	writeSite(t, l.dir, fake.key, "*.api.example.com", crtN, keyN)
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	renewed, err := handshakeCert(t, cm, "x.api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if renewed.SerialNumber.Cmp(old.SerialNumber) == 0 {
		t.Error("after a renewal the follower still serves the old certificate")
	}
	if n := certsFor(cm, "*.api.example.com"); n != 1 {
		t.Errorf("%d wildcard certificates cached after a renewal, want 1", n)
	}
	// A certificate the leader dropped is dropped.
	if err := os.RemoveAll(filepath.Join(l.dir, "certificates", fake.key, "docs.customer.example")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := handshakeCert(t, cm, "docs.customer.example"); err == nil {
		t.Error("a certificate the leader dropped is still served")
	}

	// Promotion: the mirrored tree becomes the node's own, and it issues for a name nobody had.
	if err := cm.startManaging(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(cm.mirrored); n != 0 {
		t.Errorf("%d certificates are still held as mirrored", n)
	}
	c, err := handshakeCert(t, cm, "x.api.example.com")
	if err != nil || c.SerialNumber.Cmp(renewed.SerialNumber) != 0 {
		t.Errorf("after the promotion: %v, same certificate %v", err, err == nil && c.SerialNumber.Cmp(renewed.SerialNumber) == 0)
	}
	if n := certsFor(cm, "*.api.example.com"); n != 1 {
		t.Errorf("%d wildcard certificates cached after the promotion", n)
	}
	c, err = handshakeCert(t, cm, "new.customer.example")
	if err != nil || c.DNSNames[0] != "new.customer.example" || fake.calls.Load() != 1 {
		t.Fatalf("a name no one had, after the promotion: %v, %d calls to the CA", err, fake.calls.Load())
	}

	// A demotion stops it again: the managed certificates leave the cache, the mirror brings the leader's.
	cm.stopManaging(cfg.Paths().Certs())
	if _, err := handshakeCert(t, cm, "other.customer.example"); err == nil {
		t.Error("a node that mirrors again issued a certificate")
	}
	if fake.calls.Load() != 1 {
		t.Errorf("the CA was asked %d times, want 1", fake.calls.Load())
	}
	if _, err := handshakeCert(t, cm, "x.api.example.com"); err == nil {
		t.Error("a managed certificate stayed in the cache of a node that mirrors")
	}
	m.reset()
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c, err := handshakeCert(t, cm, "x.api.example.com"); err != nil || c.SerialNumber.Cmp(renewed.SerialNumber) != 0 {
		t.Errorf("after mirroring again: %v", err)
	}
	if fake.calls.Load() != 1 {
		t.Errorf("the CA was asked %d times, want 1", fake.calls.Load())
	}
}

// dialServe handshakes with a listener the way a client does and returns the leaf certificate it presented.
func dialServe(t *testing.T, addr, serverName string) (*x509.Certificate, error) {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr,
		&tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0], nil
}

func serialOfPEM(t *testing.T, crt []byte) *big.Int {
	t.Helper()
	b, _ := pem.Decode(crt)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c.SerialNumber
}

// TestServeFollowsTheCertificateRole runs a Server on real listeners as a follower of a leader's
// store, then promotes it and demotes it again. The CA it is configured for is unreachable, so any
// attempt to issue is a failure the test would notice as a missing certificate or an error in its log.
func TestServeFollowsTheCertificateRole(t *testing.T) {
	cfg := config.Default()
	cfg.Domain, cfg.TLS.Mode, cfg.StateDir = "example.test", "http01", t.TempDir()
	cfg.TLS.CA = "http://127.0.0.1:1/dir"
	probe, err := newCertManager(certOptions{cfg: cfg, mode: tlsHTTP01, allow: func(context.Context, string) error { return nil }, log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	issuer := probe.httpIssuer.IssuerKey()
	probe.close()

	l := newLeaderStore(t)
	serials := map[string]*big.Int{}
	renew := func(name string) {
		crt, key := testCert(t, name)
		writeSite(t, l.dir, issuer, name, crt, key)
		serials[name] = serialOfPEM(t, crt)
	}
	for _, n := range []string{"api.example.test", "studio.example.test", testRef + ".api.example.test"} {
		renew(n)
	}

	logs := &logBuffer{}
	role := NewCertRole(false)
	reg := newRegistryWithProject(t)
	s, err := New(Options{
		Config: cfg, Registry: reg, Keys: newFakeKeys(),
		Logger:  slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Cluster: &Cluster{Certs: &CertSync{Role: role, Source: l.src, Interval: 50 * time.Millisecond}},
	})
	if err != nil {
		t.Fatal(err)
	}
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

	serving := func(name string) bool {
		c, err := dialServe(t, httpsLn.Addr().String(), name)
		return err == nil && c.SerialNumber.Cmp(serials[name]) == 0
	}
	wait := func(what string, f func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if f() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	names := []string{"api.example.test", "studio.example.test", testRef + ".api.example.test"}
	wait("the mirrored certificates to be served", func() bool {
		for _, n := range names {
			if !serving(n) {
				return false
			}
		}
		return true
	})

	// While the node follows: a renewal on the leader reaches it, a route that appears gets no
	// certificate from the CA, and a name nobody covers is refused.
	renew("api.example.test")
	wait("a renewed certificate to be mirrored", func() bool { return serving("api.example.test") })
	if err := reg.PutRoute(context.Background(), registry.Route{Host: "docs.customer.example", Ref: testRef, Kind: registry.RouteCustom}); err != nil {
		t.Fatal(err)
	}
	wait("the route to reach the table", func() bool { return s.table.routeKind("docs.customer.example") == kindCustom })
	time.Sleep(300 * time.Millisecond)
	if _, err := dialServe(t, httpsLn.Addr().String(), "docs.customer.example"); err == nil {
		t.Error("a certificate for a name that is not mirrored")
	}
	if logs.has("newly routed") || logs.has("obtaining new certificate") || logs.has("could not obtain") {
		t.Error("a node that mirrors tried to obtain a certificate")
	}

	// Promotion: it serves what it mirrored, from its own store now, and the mirror has stopped.
	role.Promote()
	wait("the promotion to take effect", func() bool { return logs.has("obtains and renews") })
	for _, n := range names {
		if !serving(n) {
			t.Errorf("%s not served after the promotion", n)
		}
	}
	before := len(l.rpc.paths())
	time.Sleep(200 * time.Millisecond)
	if after := len(l.rpc.paths()); after != before {
		t.Errorf("the mirror kept fetching after the promotion (%d fetches)", after-before)
	}

	// Demotion: it mirrors again and picks up the leader's next renewal.
	role.Demote()
	renew("studio.example.test")
	wait("the mirror to resume", func() bool { return serving("studio.example.test") })
	for _, n := range names {
		if !serving(n) {
			t.Errorf("%s not served after the demotion", n)
		}
	}
}

// TestMirrorFetchesOnScheduleWhilePoked: a handshake for a name nobody mirrors pokes the mirror, and a
// client that keeps asking must not keep the scheduled fetch from happening. (A poke used to restart the
// wait, so pokes more often than the interval stopped the mirror altogether; a node that had just lost
// the cache to a demotion then never got its certificates back while a client retried.)
func TestMirrorFetchesOnScheduleWhilePoked(t *testing.T) {
	var calls atomic.Int32
	src := sourceFunc(func(context.Context, string) (peerapi.CertSnapshot, error) {
		calls.Add(1)
		return peerapi.CertSnapshot{}, nil
	})
	m := newCertMirror(t.TempDir(), &CertSync{Source: src, Interval: 30 * time.Millisecond}, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.run(ctx); close(done) }()
	for end := time.Now().Add(600 * time.Millisecond); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		m.wake()
	}
	cancel()
	<-done
	if n := calls.Load(); n < 8 {
		t.Errorf("%d fetches in 600 ms with an interval of 30 ms while pokes came every 2 ms, want at least 8", n)
	}
}

// TestMirrorPokeBringsTheNextFetchForward: a poke after a fetch makes the next one come pokeGap after it,
// not at the end of the interval, and only once.
func TestMirrorPokeBringsTheNextFetchForward(t *testing.T) {
	var calls atomic.Int32
	src := sourceFunc(func(context.Context, string) (peerapi.CertSnapshot, error) {
		calls.Add(1)
		return peerapi.CertSnapshot{}, nil
	})
	m := newCertMirror(t.TempDir(), &CertSync{Source: src, Interval: time.Hour}, quietLog())
	m.pokeGap = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, f func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if f() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the first fetch", func() bool { return calls.Load() == 1 })
	m.wake()
	waitFor("the fetch a poke asks for", func() bool { return calls.Load() == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Errorf("%d fetches: one poke asked for one", n)
	}
}

// followerOf is a certificate manager of a follower on cfg's store, with a CA it could not reach.
func followerOf(t *testing.T, cfg *config.Config) *certManager {
	t.Helper()
	cm, err := newCertManager(certOptions{
		cfg: cfg, mode: tlsHTTP01, log: quietLog(), follower: true,
		allow: func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	cm.http.Issuers = []certmagic.Issuer{&failingIssuer{key: cm.httpIssuer.IssuerKey()}}
	return cm
}

// failingIssuer is a CA that refuses.
type failingIssuer struct {
	key   string
	calls atomic.Int32
}

func (f *failingIssuer) IssuerKey() string { return f.key }

func (f *failingIssuer) Issue(context.Context, *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	f.calls.Add(1)
	return nil, errors.New("the CA is not reachable")
}

// TestMirrorStartsFromTheStoreOnDisk: a follower that restarts while the leader cannot be reached serves
// the certificates it mirrored before, and one that is half written is left out.
func TestMirrorStartsFromTheStoreOnDisk(t *testing.T) {
	cfg := config.Default()
	cfg.Domain, cfg.TLS.Mode, cfg.StateDir = "example.com", "http01", t.TempDir()
	cm := followerOf(t, cfg)
	defer cm.close()
	store := cfg.Paths().Certs()
	issuer := cm.httpIssuer.IssuerKey()
	crt, key := testCert(t, "api.example.com")
	writeSite(t, store, issuer, "api.example.com", crt, key)
	crtS, _ := testCert(t, "studio.example.com")
	_, keyS := testCert(t, "studio.example.com")
	writeSite(t, store, issuer, "studio.example.com", crtS, keyS) // a certificate and a key that do not belong together

	down := sourceFunc(func(context.Context, string) (peerapi.CertSnapshot, error) {
		return peerapi.CertSnapshot{}, errors.New("the leader is unreachable")
	})
	m := newCertMirror(store, &CertSync{Source: down, Interval: time.Hour}, quietLog())
	m.onSnapshot = cm.loadMirrored
	cm.mirror = m
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	var got *x509.Certificate
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if c, err := handshakeCert(t, cm, "api.example.com"); err == nil {
			got = c
			break
		}
	}
	if got == nil || got.SerialNumber.Cmp(serialOfPEM(t, crt)) != 0 {
		t.Fatal("the follower did not serve the certificate it had on disk")
	}
	if _, err := handshakeCert(t, cm, "studio.example.com"); err == nil {
		t.Error("a certificate whose key does not match was served")
	}
}

// TestMirrorEmptySnapshotLeavesTheCacheAlone: a leader that has issued nothing yet changes neither the
// follower's files nor what it serves.
func TestMirrorEmptySnapshotLeavesTheCacheAlone(t *testing.T) {
	ctx := context.Background()
	l := newLeaderStore(t)
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, "acme.test-dir", "api.example.com", crt, key)
	m := newMirrorOf(l, t.TempDir())
	told := 0
	m.onSnapshot = func(context.Context, peerapi.CertSnapshot) { told++ }
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(l.dir, "certificates")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if told != 1 {
		t.Errorf("onSnapshot was told %d times: an empty store carries nothing to the cache", told)
	}
}

// TestStartManagingKeepsAMirroredCopyItCannotTakeOver: a promotion that cannot load a mirrored certificate
// as a managed one leaves the mirrored copy in the cache, so that the name still has a certificate.
func TestStartManagingKeepsAMirroredCopyItCannotTakeOver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Default()
	cfg.Domain, cfg.TLS.Mode, cfg.StateDir = "example.com", "http01", t.TempDir()
	cm := followerOf(t, cfg)
	defer cm.close()
	defer cancel() // before close: the issuance that follows the promotion ends with the context

	l := newLeaderStore(t)
	crt, key := testCert(t, "api.example.com")
	writeSite(t, l.dir, cm.httpIssuer.IssuerKey(), "api.example.com", crt, key)
	m := newMirrorOf(l, cfg.Paths().Certs())
	m.onSnapshot = cm.loadMirrored
	cm.mirror = m
	if err := m.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := handshakeCert(t, cm, "api.example.com"); err != nil {
		t.Fatal(err)
	}

	// The files are gone when the node takes over, so CertMagic cannot load the managed copy.
	if err := os.RemoveAll(filepath.Join(cfg.Paths().Certs(), "certificates")); err != nil {
		t.Fatal(err)
	}
	if err := cm.startManaging(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := handshakeCert(t, cm, "api.example.com")
	if err != nil || c.SerialNumber.Cmp(serialOfPEM(t, crt)) != 0 {
		t.Errorf("after a promotion that could not load the managed copy: %v", err)
	}
	if n := certsFor(cm, "api.example.com"); n != 1 {
		t.Errorf("%d certificates cached for the name", n)
	}
}

// TestMirrorRecoversWhenClientsAskForASiteTheFirstFetchLacked is the failure TestServeFollowsTheCertificateRole
// showed on a slow machine, in order: the first fetch after a demotion reads the leader's store while a renewal
// is half written, so one site is missing and the node has no certificate for it; clients ask for it, each
// handshake pokes the mirror, and the next scheduled fetch has to happen anyway.
func TestMirrorRecoversWhenClientsAskForASiteTheFirstFetchLacked(t *testing.T) {
	cfg := config.Default()
	cfg.Domain, cfg.TLS.Mode, cfg.StateDir = "example.com", "http01", t.TempDir()
	cm := followerOf(t, cfg)
	defer cm.close()
	issuer := cm.httpIssuer.IssuerKey()

	l := newLeaderStore(t)
	for _, n := range []string{"api.example.com", "studio.example.com"} {
		crt, key := testCert(t, n)
		writeSite(t, l.dir, issuer, n, crt, key)
	}
	var calls atomic.Int32
	torn := sourceFunc(func(ctx context.Context, etag string) (peerapi.CertSnapshot, error) {
		snap, err := l.src.Certs(ctx, etag)
		if calls.Add(1) == 1 && err == nil {
			// The first answer lacks the studio site, as a read between CertMagic's writes would.
			var files []peerapi.CertFile
			for _, f := range snap.Files {
				if !strings.Contains(f.Path, "studio.example.com") {
					files = append(files, f)
				}
			}
			snap.Files = files
		}
		return snap, err
	})
	m := newCertMirror(cfg.Paths().Certs(), &CertSync{Source: torn, Interval: 50 * time.Millisecond}, quietLog())
	m.onSnapshot = cm.loadMirrored
	cm.mirror = m
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if _, err := handshakeCert(t, cm, "studio.example.com"); err == nil {
			return
		}
	}
	t.Fatalf("no certificate for the site the first fetch lacked after %d fetches", calls.Load())
}
