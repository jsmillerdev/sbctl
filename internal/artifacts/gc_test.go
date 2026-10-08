package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
)

func testVersions(auth string) *Versions {
	v := &Versions{Artifacts: map[string]string{"postgres": "postgres-17.1-r1", "auth": auth, "postgrest": "postgrest-v1-r0"}}
	v.Studio.Tag = "studio-1"
	return v
}

func mkdirs(t *testing.T, cfg *config.Config, tags map[string][]string) {
	t.Helper()
	for name, ts := range tags {
		for _, tag := range ts {
			if err := os.MkdirAll(filepath.Join(cfg.Paths().Artifacts(), name, tag, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestFetchTagAndDirFor(t *testing.T) {
	old, cur := "auth-v2.150.0-r0", "auth-v2.195.0-r1"
	srv, downloads := release(t, old, "linux-arm64", makeArchive(t, goodEntries), "")
	cfg := testConfig(t, srv.URL)
	s, err := New(cfg, WithVersions(testVersions(cur)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DirFor(config.SvcGoTrue, old); err == nil {
		t.Fatal("DirFor found an artifact that was never fetched")
	}
	dir, err := s.FetchTag(context.Background(), config.SvcGoTrue, old)
	if err != nil || dir != cfg.Paths().Artifact(config.SvcGoTrue, old) {
		t.Fatalf("FetchTag = %s, %v", dir, err)
	}
	if got, err := s.DirFor(config.SvcGoTrue, old); err != nil || got != dir {
		t.Fatalf("DirFor = %s, %v", got, err)
	}
	if _, err := s.FetchTag(context.Background(), config.SvcGoTrue, old); err != nil || downloads.Load() != 1 {
		t.Fatalf("second FetchTag: %v, downloads = %d", err, downloads.Load())
	}
	// The pinned tag is not the one fetched: Dir still reports it missing.
	if _, err := s.Dir(config.SvcGoTrue); err == nil {
		t.Fatal("Dir answered for the pinned tag, which is not on disk")
	}
	if _, err := s.FetchTag(context.Background(), config.SvcStudio, "x"); err == nil {
		t.Fatal("FetchTag accepted Studio")
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "bin"), 0o755) })
}

func TestGCKeepsPinsReleasesAndProjects(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	v1, v2, v3 := "auth-v1-r0", "auth-v2-r0", "auth-v3-r0"
	mkdirs(t, cfg, map[string][]string{
		"auth":      {"auth-v0-r0", v1, v2, v3},
		"postgres":  {"postgres-17.1-r1", "postgres-16.0-r1"},
		"postgrest": {"postgrest-v1-r0"},
		"studio":    {"studio-1"},
	})
	// A half-unpacked artifact and the archive cache are never touched.
	mkdirs(t, cfg, map[string][]string{".cache": {"x"}, "auth": {".unpack-auth-v9-123"}})

	// The node ran v1, then v2, and now pins v3.
	for _, auth := range []string{v1, v2} {
		s, _ := New(cfg, WithVersions(testVersions(auth)))
		if ok, err := s.RecordPins(); err != nil || !ok {
			t.Fatalf("RecordPins(%s) = %v, %v", auth, ok, err)
		}
		if ok, err := s.RecordPins(); err != nil || ok {
			t.Fatalf("recording the same pins twice appended: %v, %v", ok, err)
		}
	}
	s, _ := New(cfg, WithVersions(testVersions(v3)))

	// A project still runs v0 of auth and the old postgres.
	proj := []map[string]string{{config.SvcGoTrue: "auth-v0-r0", config.SvcPostgres: "postgres-16.0-r1"}}

	keep, err := s.KeepSet(2, proj)
	if err != nil {
		t.Fatal(err)
	}
	unused, err := s.FindUnused(keep)
	if err != nil {
		t.Fatal(err)
	}
	if len(unused) != 1 || unused[0].Name != "auth" || unused[0].Tag != v1 {
		t.Fatalf("unused with keep_releases=2 = %+v, want only auth %s (v0 is a project's, v2 the previous release, v3 pinned)", unused, v1)
	}
	if dry, err := s.GC(keep, true); err != nil || len(dry) != 1 {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if _, err := os.Stat(unused[0].Dir); err != nil {
		t.Fatal("a dry run removed an artifact")
	}
	removed, err := s.GC(keep, false)
	if err != nil || len(removed) != 1 {
		t.Fatalf("GC = %+v, %v", removed, err)
	}
	if _, err := os.Stat(unused[0].Dir); err == nil {
		t.Fatal("GC left the unused artifact")
	}
	for _, p := range []string{"auth/" + v2, "auth/" + v3, "auth/auth-v0-r0", "postgres/postgres-16.0-r1", "studio/studio-1", ".cache/x", "auth/.unpack-auth-v9-123"} {
		if _, err := os.Stat(filepath.Join(cfg.Paths().Artifacts(), p)); err != nil {
			t.Fatalf("GC removed %s: %v", p, err)
		}
	}

	// keep_releases = 1 keeps the pinned versions and the projects'. The daemon has not recorded
	// v3 yet, so the newest recorded release (v2, what it last started with) is kept as well:
	// this binary may be newer than the daemon whose services still run those artifacts.
	keep, _ = s.KeepSet(1, proj)
	if removed, err = s.GC(keep, false); err != nil || len(removed) != 0 {
		t.Fatalf("GC with keep_releases=1 before the daemon recorded the pins = %+v, %v (want nothing)", removed, err)
	}
	// Once the daemon runs v3 (the history ends with it), v2 goes.
	if ok, err := s.RecordPins(); err != nil || !ok {
		t.Fatalf("RecordPins(%s) = %v, %v", v3, ok, err)
	}
	keep, _ = s.KeepSet(1, proj)
	removed, err = s.GC(keep, false)
	if err != nil || len(removed) != 1 || removed[0].Tag != v2 {
		t.Fatalf("GC with keep_releases=1 = %+v, %v", removed, err)
	}
	// Once no project needs v0, it goes too.
	keep, _ = s.KeepSet(1, nil)
	removed, err = s.GC(keep, false)
	if err != nil || len(removed) != 2 {
		t.Fatalf("GC without projects = %+v, %v (want auth v0 and postgres 16)", removed, err)
	}
}
