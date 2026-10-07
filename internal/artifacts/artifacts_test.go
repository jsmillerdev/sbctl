package artifacts

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/OWNER/sbctl/internal/config"
)

type entry struct {
	name     string
	typ      byte
	body     string
	mode     int64
	linkname string
}

func makeArchive(t *testing.T, entries []entry) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.linkname, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	zw.Write(tarBuf.Bytes())
	zw.Close()
	return out.Bytes()
}

var goodEntries = []entry{
	{name: "./", typ: tar.TypeDir, mode: 0o755},
	{name: "./bin/", typ: tar.TypeDir, mode: 0o555},
	{name: "./bin/auth", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho auth\n"},
	{name: "./bin/gotrue", typ: tar.TypeSymlink, linkname: "auth", mode: 0o777},
	{name: "./share/doc.txt", typ: tar.TypeReg, mode: 0o4644, body: "doc"},
}

func TestUnpackPreservesModesAndSymlinks(t *testing.T) {
	dest := t.TempDir()
	st, err := Unpack(bytes.NewReader(makeArchive(t, goodEntries)), dest)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Symlinks != 1 {
		t.Fatalf("stats = %+v", st)
	}
	fi, err := os.Stat(filepath.Join(dest, "bin/auth"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("bin/auth mode = %v, %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(dest, "share/doc.txt")); fi.Mode()&os.ModeSetuid != 0 {
		t.Fatal("setuid bit must be dropped")
	}
	if fi, _ := os.Stat(filepath.Join(dest, "bin")); fi.Mode().Perm() != 0o555 {
		t.Fatalf("bin dir mode = %v, want 0555 (applied after extraction)", fi.Mode().Perm())
	}
	if l, err := os.Readlink(filepath.Join(dest, "bin/gotrue")); err != nil || l != "auth" {
		t.Fatalf("symlink = %q, %v", l, err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dest, "bin"), 0o755) })
}

func TestUnpackRejectsUnsafeEntries(t *testing.T) {
	cases := map[string][]entry{
		"dotdot":          {{name: "../evil", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"nested dotdot":   {{name: "bin/../../evil", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"absolute":        {{name: "/etc/evil", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"abs symlink":     {{name: "bin/x", typ: tar.TypeSymlink, linkname: "/etc/passwd"}},
		"escaping link":   {{name: "bin/x", typ: tar.TypeSymlink, linkname: "../../etc/passwd"}},
		"link chain out":  {{name: "s", typ: tar.TypeSymlink, linkname: "."}, {name: "l", typ: tar.TypeSymlink, linkname: "s/.."}},
		"link chain late": {{name: "l", typ: tar.TypeSymlink, linkname: "s/.."}, {name: "s", typ: tar.TypeSymlink, linkname: "."}},
		"link loop":       {{name: "a", typ: tar.TypeSymlink, linkname: "b"}, {name: "b", typ: tar.TypeSymlink, linkname: "a"}},
		"write via link":  {{name: "d", typ: tar.TypeSymlink, linkname: "."}, {name: "d/f", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"hardlink escape": {{name: "h", typ: tar.TypeLink, linkname: "../outside"}},
		"fifo":            {{name: "p", typ: tar.TypeFifo, mode: 0o644}},
		"char device":     {{name: "c", typ: tar.TypeChar, mode: 0o644}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "root")
			os.Mkdir(dest, 0o755)
			if _, err := Unpack(bytes.NewReader(makeArchive(t, entries)), dest); err == nil {
				t.Fatal("expected an error")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); err == nil {
				t.Fatal("file escaped the destination")
			}
		})
	}
}

// release serves one tag the way github.com/supabase/slim-services/releases does.
func release(t *testing.T, tag, platform string, archive []byte, sumOverride string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	sum := sha256.Sum256(archive)
	name := tag + "-" + platform + ".tar.zst"
	sums := fmt.Sprintf("%s  %s\n%s  %s\n", strings.Repeat("0", 64), "other-file.sbom.spdx.json", hex.EncodeToString(sum[:]), name)
	if sumOverride != "" {
		sums = fmt.Sprintf("%s  %s\n", sumOverride, name)
	}
	var downloads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/"+tag+"/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	mux.HandleFunc("/"+tag+"/"+name, func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func testConfig(t *testing.T, base string) *config.Config {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Platform = "linux-arm64"
	cfg.Artifacts.BaseURL = base
	return cfg
}

func TestFetchVerifiesUnpacksAndCaches(t *testing.T) {
	tag := "auth-v2.195.0-r1"
	srv, downloads := release(t, tag, "linux-arm64", makeArchive(t, goodEntries), "")
	cfg := testConfig(t, srv.URL)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := s.Fetch(context.Background(), config.SvcGoTrue)
	if err != nil {
		t.Fatal(err)
	}
	if want := cfg.Paths().Artifact(config.SvcGoTrue, tag); dir != want {
		t.Fatalf("dir = %s, want %s", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/auth")); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMarker(dir)
	if err != nil || m.Tag != tag || m.Platform != "linux-arm64" {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("artifact root mode = %v, %v; the sbctl user must be able to traverse a root-fetched artifact", fi, err)
	}
	// Second fetch is a no-op.
	if _, err := s.Fetch(context.Background(), config.SvcGoTrue); err != nil || downloads.Load() != 1 {
		t.Fatalf("refetch: %v, downloads = %d", err, downloads.Load())
	}
	// Remove the unpacked copy: the cached archive is reused without a download.
	os.RemoveAll(dir)
	os.Chmod(filepath.Join(dir, "bin"), 0o755)
	if _, err := s.Fetch(context.Background(), config.SvcGoTrue); err != nil || downloads.Load() != 1 {
		t.Fatalf("cached refetch: %v, downloads = %d", err, downloads.Load())
	}
	if got, err := s.Dir(config.SvcGoTrue); err != nil || got != dir {
		t.Fatalf("Dir = %s, %v", got, err)
	}
	// Leave no read-only dirs behind for TempDir cleanup.
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "bin"), 0o755) })
}

func TestFetchRejectsWrongDigest(t *testing.T) {
	tag := "auth-v2.195.0-r1"
	srv, _ := release(t, tag, "linux-arm64", makeArchive(t, goodEntries), strings.Repeat("a", 64))
	cfg := testConfig(t, srv.URL)
	s, _ := New(cfg)
	_, err := s.Fetch(context.Background(), config.SvcGoTrue)
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v, want digest mismatch", err)
	}
	dir, _ := s.Path(config.SvcGoTrue)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("artifact dir must not exist after a failed verification")
	}
	if _, err := s.Dir(config.SvcGoTrue); err == nil {
		t.Fatal("Dir must report ErrNotFetched")
	}
}

func TestFetchStudio(t *testing.T) {
	arch := makeArchive(t, []entry{{name: "bin/studio", typ: tar.TypeReg, mode: 0o755, body: "x"}})
	sum := sha256.Sum256(arch)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(arch) }))
	defer srv.Close()
	cfg := testConfig(t, "http://unused.invalid")
	s, _ := New(cfg)
	if _, err := s.Fetch(context.Background(), config.SvcStudio); err == nil {
		t.Fatal("studio without artifact_url must fail")
	}
	cfg.Studio.ArtifactURL = srv.URL + "/studio.tar.zst"
	cfg.Studio.ArtifactSHA256 = hex.EncodeToString(sum[:])
	dir, err := s.Fetch(context.Background(), config.SvcStudio)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/studio")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dir, "studio") {
		t.Fatal(dir)
	}
}

func TestVersionsAndPlatform(t *testing.T) {
	cfg := config.Default()
	v, err := LoadVersions(cfg) // embedded versions.yaml
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{config.SvcPostgres, config.SvcGoTrue, config.SvcPostgREST, config.SvcSupavisor, config.SvcRealtime, config.SvcStorage, config.SvcPGMeta, config.SvcStudio} {
		if tag, err := v.Tag(svc); err != nil || tag == "" {
			t.Errorf("Tag(%s) = %q, %v", svc, tag, err)
		}
	}
	if tag, _ := v.Tag(config.SvcGoTrue); !strings.HasPrefix(tag, "auth-") {
		t.Errorf("gotrue maps to artifact auth, got %q", tag)
	}
	if _, err := v.Tag("nope"); err == nil {
		t.Error("unknown service must fail")
	}
	cfg.Platform = "windows-amd64"
	if _, err := Platform(cfg); err == nil {
		t.Error("windows has no build")
	}
	// versions_file overrides the embedded copy.
	f := filepath.Join(t.TempDir(), "v.yaml")
	os.WriteFile(f, []byte("artifacts:\n  postgres: postgres-9.9-r0\nstudio:\n  tag: s1\n"), 0o644)
	cfg = config.Default()
	cfg.Artifacts.VersionsFile = f
	if v, err := LoadVersions(cfg); err != nil || v.Artifacts["postgres"] != "postgres-9.9-r0" {
		t.Fatalf("override: %+v, %v", v, err)
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		cfg.Platform = ""
		if p, err := Platform(cfg); err != nil || p != "darwin-arm64" {
			t.Fatalf("platform = %s, %v", p, err)
		}
	}
}

func TestParseSHA256SUMS(t *testing.T) {
	body := []byte(strings.Repeat("b", 64) + " *x.tar.zst\n" + strings.Repeat("c", 64) + "  y.tar.zst\n")
	if h, err := parseSHA256SUMS(body, "x.tar.zst"); err != nil || h != strings.Repeat("b", 64) {
		t.Fatal(h, err)
	}
	if _, err := parseSHA256SUMS(body, "z"); err == nil {
		t.Fatal("missing file must fail")
	}
}

// TestRealArchive unpacks a real slim-services archive from SBCTL_TEST_ARCHIVE
// (for example ~/.cache/sbctl/archives/auth-v2.195.0-r1-darwin-arm64.tar.zst).
func TestRealArchive(t *testing.T) {
	p := os.Getenv("SBCTL_TEST_ARCHIVE")
	if p == "" {
		t.Skip("set SBCTL_TEST_ARCHIVE to a slim-services .tar.zst")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dest := t.TempDir()
	st, err := Unpack(f, dest)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("unpacked %+v", st)
	if _, err := os.Stat(filepath.Join(dest, "bin")); err != nil {
		t.Fatal(err)
	}
}
