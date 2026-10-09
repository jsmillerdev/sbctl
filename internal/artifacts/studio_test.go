package artifacts

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/versions"
)

const studioTag = "2026.10.05-sha-94b8b06"

// studioVersions pins Studio at studioTag with patch set n (0: a versions.yaml from before the
// field, whose build is the tag alone).
func studioVersions(n int) *Versions {
	v := testVersions("auth-v2.195.0-r1")
	v.Studio.Tag, v.Studio.Patchset = studioTag, n
	return v
}

// studioArchive is a Studio build of patch set n, with the build-info studio/build.sh writes (none
// when n is 0: a stand-in).
func studioArchive(t *testing.T, n int) []byte {
	t.Helper()
	es := []entry{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./bin/", typ: tar.TypeDir, mode: 0o555},
		{name: "./bin/studio", typ: tar.TypeReg, mode: 0o755, body: fmt.Sprintf("#!/bin/sh\necho p%d\n", n)},
		{name: "./bin/.runtime-env.sh", typ: tar.TypeSymlink, linkname: "studio", mode: 0o777},
		{name: "./app/", typ: tar.TypeDir, mode: 0o755},
		{name: "./app/server.js", typ: tar.TypeReg, mode: 0o644, body: "listen()"},
	}
	if n > 0 {
		es = append(es, entry{name: "./share/supavise/build-info.json", typ: tar.TypeReg, mode: 0o644,
			body: fmt.Sprintf(`{"service":"supavise-studio","upstream_tag":%q,"patchset":%d,"platform":"linux-arm64"}`, studioTag, n)})
	}
	return makeArchive(t, es)
}

// studioServer serves archives by file name and counts the downloads.
func studioServer(t *testing.T, files map[string][]byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		n.Add(1)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func studioStore(t *testing.T, cfg *config.Config, n int) *Store {
	t.Helper()
	s, err := New(cfg, WithVersions(studioVersions(n)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func chmodTree(t *testing.T, root string) {
	t.Cleanup(func() {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}

func TestStudioBuildIsTheTagAndThePatchset(t *testing.T) {
	v := studioVersions(3)
	build := studioTag + "-p3"
	if got := v.StudioBuild(); got != build {
		t.Fatalf("StudioBuild = %q", got)
	}
	if got, err := v.Tag(config.SvcStudio); err != nil || got != build {
		t.Fatalf("Tag(studio) = %q, %v: the directory and the pin are the build", got, err)
	}
	if got := v.Pins()[config.SvcStudio]; got != build {
		t.Fatalf("Pins()[studio] = %q", got)
	}
	// A versions.yaml from before the field names the build by its tag, as those releases did.
	if got := studioVersions(0).StudioBuild(); got != studioTag {
		t.Fatalf("no patch set: %q", got)
	}
	if got := StudioAsset(build, "linux-amd64"); got != "supavise-studio-"+studioTag+"-p3-linux-amd64.tar.zst" {
		t.Fatalf("StudioAsset = %q", got)
	}
	for url, want := range map[string]string{
		"https://github.com/supavise/supavise/releases/download/v0.2.1/supavise-studio-" + studioTag + "-p3-linux-amd64.tar.zst": build,
		"https://h/x/supavise-studio-" + studioTag + "-p2-linux-amd64.tar.zst?token=1":                                           studioTag + "-p2",
		"https://h/studio.tar.zst":                                           "",
		"https://h/studio-" + studioTag + "-r0-linux-amd64.tar.zst":          "", // the slim artifact: names no build of ours
		"https://h/supavise-studio-" + studioTag + "-p3-linux-arm64.tar.zst": "", // another platform
		"": "",
	} {
		got, ok := StudioBuildOfURL(url, "linux-amd64")
		if got != want || ok != (want != "") {
			t.Errorf("StudioBuildOfURL(%q) = %q, %v; want %q", url, got, ok, want)
		}
	}
	if _, err := ParseVersions([]byte("artifacts:\n  postgres: p-1-r0\nstudio:\n  tag: t\n  patchset: -1\n")); err == nil {
		t.Error("a negative patch set was accepted")
	}
	// The repository's own pins carry a patch set (internal/versions checks it against studio/PATCHSET).
	if v, err := ParseVersions(versions.VersionsYAML); err != nil || v.Studio.Patchset <= 0 || v.StudioBuild() == v.Studio.Tag {
		t.Fatalf("versions.yaml: %+v, %v", v, err)
	}
}

// A new node puts the build under its own name: artifacts/studio/<tag>-p<N>.
func TestFetchStudioInstallsUnderTheBuild(t *testing.T) {
	build := studioTag + "-p3"
	arch := studioArchive(t, 3)
	srv, downloads := studioServer(t, map[string][]byte{StudioAsset(build, "linux-arm64"): arch})
	cfg := testConfig(t, "http://unused.invalid")
	cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = srv.URL+"/"+StudioAsset(build, "linux-arm64"), digest(arch)
	dir, err := studioStore(t, cfg, 3).Fetch(context.Background(), config.SvcStudio)
	if err != nil {
		t.Fatal(err)
	}
	chmodTree(t, cfg.Paths().Artifacts())
	if want := cfg.Paths().Artifact(config.SvcStudio, build); dir != want {
		t.Fatalf("dir = %s, want %s", dir, want)
	}
	if m, err := ReadMarker(dir); err != nil || m.Tag != build || m.SHA256 != digest(arch) || m.Source != cfg.Studio.ArtifactURL || m.AdoptedFrom != "" {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	if downloads.Load() != 1 {
		t.Fatalf("downloads = %d", downloads.Load())
	}
}

// A node that a release before the patch set installed has the build under the tag alone. When
// that build is the pinned one, it is linked into its new directory, not downloaded again, and the
// old directory stays for the Studio that runs from it and for a rollback.
func TestFetchStudioAdoptsTheSameBuildFromTheTagDirectory(t *testing.T) {
	build := studioTag + "-p3"
	arch := studioArchive(t, 3)
	name := StudioAsset(build, "linux-arm64")
	srv, downloads := studioServer(t, map[string][]byte{name: arch})
	cfg := testConfig(t, "http://unused.invalid")
	cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = srv.URL+"/"+name, digest(arch)
	chmodTree(t, cfg.Paths().Artifacts())
	// What v0.2.0 left: the p3 build under the tag.
	old, err := studioStore(t, cfg, 0).Fetch(context.Background(), config.SvcStudio)
	if err != nil || old != cfg.Paths().Artifact(config.SvcStudio, studioTag) {
		t.Fatalf("old layout: %s, %v", old, err)
	}
	// The release after it: same build, a newer upload of it (another digest) that is never needed.
	cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = srv.URL+"/v0.2.1/"+name, strings.Repeat("b", 64)
	dir, err := studioStore(t, cfg, 3).Fetch(context.Background(), config.SvcStudio)
	if err != nil {
		t.Fatal(err)
	}
	if dir != cfg.Paths().Artifact(config.SvcStudio, build) || downloads.Load() != 1 {
		t.Fatalf("dir %s, downloads %d: want the build's directory and no new download", dir, downloads.Load())
	}
	a, _ := os.Stat(filepath.Join(old, "bin/studio"))
	b, err := os.Stat(filepath.Join(dir, "bin/studio"))
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("bin/studio is not a link of the old build's file: %v", err)
	}
	if l, err := os.Readlink(filepath.Join(dir, "bin/.runtime-env.sh")); err != nil || l != "studio" {
		t.Fatalf("symlink = %q, %v", l, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "bin")); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("bin mode = %v, %v", fi, err)
	}
	m, err := ReadMarker(dir)
	if err != nil || m.Tag != build || m.AdoptedFrom != old || m.SHA256 != digest(arch) || m.Source != srv.URL+"/"+name {
		t.Fatalf("marker = %+v, %v: the build keeps the source and digest it was installed from", m, err)
	}
	if om, err := ReadMarker(old); err != nil || om.Tag != studioTag {
		t.Fatalf("the old directory changed: %+v, %v", om, err)
	}
}

// The user's node: v0.1.3 put the p2 build under the tag, v0.2.0 kept it. The pinned p3 build is
// downloaded into its own directory; the p2 build stays where it is for a rollback.
func TestFetchStudioReplacesAnOlderBuildUnderTheTag(t *testing.T) {
	build := studioTag + "-p3"
	p2, p3 := studioArchive(t, 2), studioArchive(t, 3)
	n2, n3 := StudioAsset(studioTag+"-p2", "linux-arm64"), StudioAsset(build, "linux-arm64")
	srv, downloads := studioServer(t, map[string][]byte{n2: p2, n3: p3})
	cfg := testConfig(t, "http://unused.invalid")
	chmodTree(t, cfg.Paths().Artifacts())
	cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = srv.URL+"/"+n2, digest(p2)
	old, err := studioStore(t, cfg, 0).Fetch(context.Background(), config.SvcStudio)
	if err != nil {
		t.Fatal(err)
	}
	s := studioStore(t, cfg, 3)
	// config.toml still names the p2 build: it is never put where the binary looks for p3.
	if _, err := s.Fetch(context.Background(), config.SvcStudio); err == nil || !strings.Contains(err.Error(), "names the Studio build "+studioTag+"-p2") {
		t.Fatalf("a stale URL: %v", err)
	}
	if _, err := os.Stat(cfg.Paths().Artifact(config.SvcStudio, build)); err == nil || downloads.Load() != 1 {
		t.Fatalf("the stale build was installed (downloads %d)", downloads.Load())
	}
	// What `supavise upgrade` hands the fetch: the release's own asset.
	cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = srv.URL+"/"+n3, digest(p3)
	dir, err := s.Fetch(context.Background(), config.SvcStudio)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bin/studio")); !strings.Contains(string(b), "p3") || downloads.Load() != 2 {
		t.Fatalf("installed %q after %d downloads", b, downloads.Load())
	}
	if b, _ := os.ReadFile(filepath.Join(old, "bin/studio")); !strings.Contains(string(b), "p2") {
		t.Fatalf("the old build changed: %q", b)
	}
}

// An archive is the build its build-info says, whatever its URL is called.
func TestFetchStudioRefusesAnArchiveOfAnotherBuild(t *testing.T) {
	p2 := studioArchive(t, 2)
	srv, _ := studioServer(t, map[string][]byte{"studio.tar.zst": p2})
	cfg := testConfig(t, "http://unused.invalid")
	cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = srv.URL+"/studio.tar.zst", digest(p2)
	_, err := studioStore(t, cfg, 3).Fetch(context.Background(), config.SvcStudio)
	if err == nil || !strings.Contains(err.Error(), "is the build "+studioTag+"-p2") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(cfg.Paths().Artifact(config.SvcStudio, studioTag+"-p3")); err == nil {
		t.Fatal("the refused build was put in place")
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.Paths().Artifacts(), "studio", ".unpack-*")); len(left) != 0 {
		t.Fatalf("the refused archive left %v", left)
	}
}

// The GC keeps the build the binary pins and, while the release that ran it is kept, the build
// under the tag alone that a release before the patch set ran, which a rollback runs again.
func TestGCKeepsTheStudioBuildAndTheOneUnderTheTag(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	build := studioTag + "-p3"
	mkdirs(t, cfg, map[string][]string{"studio": {"2026.09.01-sha-0000000", studioTag, build}})
	// The daemons of the two releases before recorded their pins: Studio by the tag alone.
	for _, tag := range []string{"2026.09.01-sha-0000000", studioTag} {
		v := studioVersions(0)
		v.Studio.Tag = tag
		if _, err := studioStoreWith(t, cfg, v).RecordPins(); err != nil {
			t.Fatal(err)
		}
	}
	s := studioStore(t, cfg, 3)
	keep, err := s.KeepSet(2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !keep["studio/"+build] || !keep["studio/"+studioTag] || keep["studio/2026.09.01-sha-0000000"] {
		t.Fatalf("keep = %v", keep)
	}
	// keep_releases = 1, once the new daemon recorded its pins: the tag directory goes, the build stays.
	if _, err := s.RecordPins(); err != nil {
		t.Fatal(err)
	}
	keep, _ = s.KeepSet(1, nil)
	removed, err := s.GC(keep, false)
	if err != nil || len(removed) != 2 {
		t.Fatalf("GC = %+v, %v", removed, err)
	}
	if _, err := os.Stat(cfg.Paths().Artifact(config.SvcStudio, build)); err != nil {
		t.Fatalf("GC removed the pinned build: %v", err)
	}
	// A kept release (supavise rollback) names its Studio pin as the binary reported it.
	mkdirs(t, cfg, map[string][]string{"studio": {studioTag}})
	keep, _ = s.KeepSet(1, []map[string]string{{config.SvcStudio: studioTag}})
	if removed, _ := s.GC(keep, true); len(removed) != 0 {
		t.Fatalf("the kept release's build would go: %+v", removed)
	}
}

func studioStoreWith(t *testing.T, cfg *config.Config, v *Versions) *Store {
	t.Helper()
	s, err := New(cfg, WithVersions(v))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A stand-in without build-info (tests, an operator's own build) under the tag is the build the
// config names when the digests match, and then it is linked; a config that names another build
// does not let it through.
func TestFetchStudioStandInUnderTheTag(t *testing.T) {
	stand := studioArchive(t, 0)
	srv, downloads := studioServer(t, map[string][]byte{"studio.tar.zst": stand})
	cfg := testConfig(t, "http://unused.invalid")
	chmodTree(t, cfg.Paths().Artifacts())
	cfg.Studio.ArtifactURL, cfg.Studio.ArtifactSHA256 = srv.URL+"/studio.tar.zst", digest(stand)
	if _, err := studioStore(t, cfg, 0).Fetch(context.Background(), config.SvcStudio); err != nil {
		t.Fatal(err)
	}
	cfg.Studio.ArtifactURL = srv.URL + "/" + StudioAsset(studioTag+"-p2", "linux-arm64")
	if _, err := studioStore(t, cfg, 3).Fetch(context.Background(), config.SvcStudio); err == nil {
		t.Fatal("a stand-in was taken for the build while the config names another")
	}
	cfg.Studio.ArtifactURL = srv.URL + "/studio.tar.zst"
	dir, err := studioStore(t, cfg, 3).Fetch(context.Background(), config.SvcStudio)
	if err != nil || downloads.Load() != 1 {
		t.Fatalf("%s, %v, downloads %d", dir, err, downloads.Load())
	}
	if m, _ := ReadMarker(dir); m == nil || m.AdoptedFrom == "" {
		t.Fatalf("marker = %+v", m)
	}
}
