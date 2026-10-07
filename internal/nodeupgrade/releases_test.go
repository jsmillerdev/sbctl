package nodeupgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func keepRelease(t *testing.T, r Releases, version string, at time.Time) Record {
	t.Helper()
	src := filepath.Join(t.TempDir(), "supavise")
	if err := os.WriteFile(src, []byte("binary of "+version), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := r.Keep(Record{Version: version, Pins: map[string]string{"gotrue": "auth-" + version}, RegistrySchema: schemaV1, SchemaSource: SchemaFromBinary, InstalledAt: at}, src)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func versionsOf(t *testing.T, r Releases) string {
	t.Helper()
	all, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	var v []string
	for _, rec := range all {
		v = append(v, rec.Version)
	}
	return strings.Join(v, ",")
}

func TestReleasesKeepAndPrevious(t *testing.T) {
	r := Releases{Dir: filepath.Join(t.TempDir(), "releases")}
	if got := versionsOf(t, r); got != "" {
		t.Fatalf("an empty store lists %q", got)
	}
	if p, err := r.Previous("v1.0.0"); err != nil || p != nil {
		t.Fatalf("previous of nothing = %v, %v", p, err)
	}
	keepRelease(t, r, "v1.0.0", t0)
	keepRelease(t, r, "v1.1.0", t0.Add(time.Hour))
	if got := versionsOf(t, r); got != "v1.1.0,v1.0.0" {
		t.Fatalf("list = %s, want the newest installed first", got)
	}
	p, err := r.Previous("v1.1.0")
	if err != nil || p == nil || p.Version != "v1.0.0" {
		t.Fatalf("previous of v1.1.0 = %+v, %v", p, err)
	}
	if p, _ := r.Previous("v1.0.0"); p == nil || p.Version != "v1.1.0" {
		t.Fatalf("previous of v1.0.0 = %+v (the other kept release)", p)
	}
	// The record carries what a rollback needs, and the binary is what was kept.
	rec, err := r.Get("v1.0.0")
	if err != nil || rec.Pins["gotrue"] != "auth-v1.0.0" || rec.RegistrySchema != schemaV1 || rec.SHA256 == "" {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if err := r.Verify(rec); err != nil {
		t.Fatal(err)
	}
	bin, _ := r.BinaryPath("v1.0.0")
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("kept binary: %v %v", fi, err)
	}
	// A rollback to v1.0.0 makes it the newest.
	if err := r.Touch("v1.0.0", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := versionsOf(t, r); got != "v1.0.0,v1.1.0" {
		t.Fatalf("list after Touch = %s", got)
	}
}

func TestReleasesRefuseATamperedBinary(t *testing.T) {
	r := Releases{Dir: t.TempDir()}
	rec := keepRelease(t, r, "v1.0.0", t0)
	bin, _ := r.BinaryPath("v1.0.0")
	if err := os.WriteFile(bin, []byte("something else"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(&rec); err == nil || !strings.Contains(err.Error(), "does not match its record") {
		t.Fatalf("a replaced binary verified: %v", err)
	}
	os.Remove(bin)
	if err := r.Verify(&rec); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("a missing binary verified: %v", err)
	}
}

func TestReleasesRefuseUnsafeNames(t *testing.T) {
	r := Releases{Dir: t.TempDir()}
	for _, v := range []string{"", "..", ".", "../v1", "v1/../../x", "v 1", "-rf"} {
		if _, err := r.BinaryPath(v); err == nil {
			t.Errorf("%q names a directory", v)
		}
	}
	src := filepath.Join(t.TempDir(), "b")
	os.WriteFile(src, []byte("x"), 0o755)
	if _, err := r.Keep(Record{Version: "../escape"}, src); err == nil {
		t.Error("kept a release outside the directory")
	}
	for _, v := range []string{"v1.2.3", "v1.2.3-rc1", "dev", "v0.0.1+build.5"} {
		if _, err := r.BinaryPath(v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
}

// keep_releases counts the current release, the newest ones stay and the current one always does.
func TestReleasesGC(t *testing.T) {
	r := Releases{Dir: t.TempDir()}
	for i, v := range []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0", "v1.4.0"} {
		keepRelease(t, r, v, t0.Add(time.Duration(i)*time.Hour))
	}
	removed, err := r.GC(3, "v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(removed, ",") != "v1.1.0,v1.0.0" {
		t.Fatalf("removed %v, want the two oldest", removed)
	}
	if got := versionsOf(t, r); got != "v1.4.0,v1.3.0,v1.2.0" {
		t.Fatalf("kept %s", got)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "v1.0.0")); !os.IsNotExist(err) {
		t.Fatal("the directory of a removed release is still there")
	}
	// After a rollback the current release is an old one; it stays whatever keep says.
	if err := r.Touch("v1.2.0", t0.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	removed, _ = r.GC(1, "v1.2.0")
	if strings.Join(removed, ",") != "v1.4.0,v1.3.0" || versionsOf(t, r) != "v1.2.0" {
		t.Fatalf("removed %v, kept %s", removed, versionsOf(t, r))
	}
	// Never below one.
	if removed, _ = r.GC(0, "v1.2.0"); len(removed) != 0 || versionsOf(t, r) != "v1.2.0" {
		t.Fatalf("GC(0) removed %v", removed)
	}
	// The current release is kept even when keep leaves it out.
	keepRelease(t, r, "v1.5.0", t0.Add(20*time.Hour))
	removed, _ = r.GC(1, "v1.2.0")
	if len(removed) != 0 || versionsOf(t, r) != "v1.5.0,v1.2.0" {
		t.Fatalf("removed %v, kept %s", removed, versionsOf(t, r))
	}
}

func TestCheckManifestPins(t *testing.T) {
	info := &Info{Version: "v1.1.0", Pins: newPins()}
	good := map[string]string{"auth": authNew, "postgrest": restNew, "pooler": "pooler-v2.9.13-r1", "postgres": pgOld}
	if err := CheckManifestPins(info, good, "2026.10.05-sha-94b8b06"); err != nil {
		t.Fatalf("matching pins: %v", err)
	}
	if err := CheckManifestPins(info, nil, ""); err != nil {
		t.Fatalf("a manifest with no pins: %v", err)
	}
	bad := map[string]string{"auth": "auth-v2.196.0-r0"}
	if err := CheckManifestPins(info, bad, ""); err == nil || !strings.Contains(err.Error(), "not the one the signed manifest describes") {
		t.Fatalf("a binary with other pins: %v", err)
	}
	if err := CheckManifestPins(info, nil, "2026.11.01-sha-bbbbbbb"); err == nil {
		t.Fatal("a binary with another Studio build")
	}
}
