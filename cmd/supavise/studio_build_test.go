package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/selfupdate"
	"github.com/supavise/supavise/internal/units"
	"github.com/supavise/supavise/internal/versions"
)

// pinned is the Studio of this tree's versions.yaml: the upstream tag and the build.
func pinned(t *testing.T) (v *artifacts.Versions, tag, build string) {
	t.Helper()
	v, err := artifacts.ParseVersions(versions.VersionsYAML)
	if err != nil {
		t.Fatal(err)
	}
	if v.StudioBuild() == v.Studio.Tag {
		t.Fatal("versions.yaml has no Studio patch set")
	}
	return v, v.Studio.Tag, v.StudioBuild()
}

// renderStudio writes Studio's launcher script the way the daemon renders it, running the build
// under artifacts/studio/<dir>.
func renderStudio(t *testing.T, cfg *config.Config, dir string) {
	t.Helper()
	run := units.FilesFor(cfg, units.Spec{Service: config.SvcStudio}).Run
	if err := os.MkdirAll(filepath.Dir(run), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("#!/bin/sh\ncd '%s'\nexec '%s/bin/studio'\n", cfg.Paths().Artifact(config.SvcStudio, dir), cfg.Paths().Artifact(config.SvcStudio, dir))
	if err := os.WriteFile(run, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func studioSums(build, platform string) (*selfupdate.Verified, string, string) {
	name := artifacts.StudioAsset(build, platform)
	sha := strings.Repeat("c", 64)
	url := "https://github.com/supavise/supavise/releases/download/v0.2.1/" + name
	return &selfupdate.Verified{
		Release:  &selfupdate.Release{Tag: "v0.2.1", Assets: map[string]string{name: url}},
		Sums:     []byte(strings.Repeat("0", 64) + "  supavise-linux-amd64\n" + sha + "  " + name + "\n"),
		Manifest: &selfupdate.Manifest{Schema: 1, Version: "v0.2.1", MinUpgradeFrom: "v0.0.0", Studio: build},
	}, url, sha
}

// The upgrade to the release after v0.2.0 is driven by v0.2.0's binary, whose plan, prefetch and
// wait are this tree's code unchanged (cmd/supavise/upgrade_host.go, internal/nodeupgrade,
// internal/fleet/tags.go have not changed there since v0.2.0). The two sides of its Studio
// comparison come from two binaries: the node's pin from the directory the old daemon's unit runs
// (the tag alone), the release's from the new binary's `release-info` (the build). So the old
// driver sees a release that changes only the patch set as a Studio move, hands the new binary the
// release's own asset from the signed list, and waits until the new daemon runs Studio from the
// build's directory.
func TestOldDriverSeesAPatchsetOnlyStudioMove(t *testing.T) {
	v, tag, build := pinned(t)
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	renderStudio(t, cfg, tag) // what v0.1.3 and v0.2.0 rendered

	// Inspect, on the old driver: the node's pins come from the rendered units.
	old := *v
	old.Studio.Patchset = 0
	h, out, _ := stubHost("")
	h.cfg = cfg
	n := &nodeupgrade.Node{Version: "v0.2.0", Platform: "linux-amd64", BinaryInfo: &nodeupgrade.Info{Version: "v0.2.0", Pins: nodeupgrade.PinsOf(&old)}}
	h.nodePins(n)
	if n.Pins[config.SvcStudio] != tag {
		t.Fatalf("node's Studio pin = %q, want the directory its unit runs (%s)", n.Pins[config.SvcStudio], tag)
	}

	// Stage: the new binary describes itself; the signed manifest the release tool writes agrees.
	to, err := nodeupgrade.OwnInfo("v0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if to.Pins[config.SvcStudio] != build {
		t.Fatalf("release-info pins Studio %q, want %s", to.Pins[config.SvcStudio], build)
	}
	ver, url, sha := studioSums(build, "linux-amd64")
	if err := nodeupgrade.CheckManifestPins(to, v.Artifacts, ver.Manifest.Studio); err != nil {
		t.Fatal(err)
	}

	// Plan: Studio moves, from the tag to the build.
	p := nodeupgrade.BuildPlan(n, to, nodeupgrade.PlanOptions{Canary: 1, Batch: 5})
	m, ok := moved(p.Shared, config.SvcStudio)
	if !ok || m.From != tag || m.To != build {
		t.Fatalf("plan's shared moves = %+v", p.Shared)
	}

	// Prefetch: the staged binary fetches Studio, and only Studio, from the release's asset.
	bin := filepath.Join(t.TempDir(), "supavise")
	record := bin + ".calls"
	script := "#!/bin/sh\necho \"$* url=$SUPAVISE_STUDIO_ARTIFACT_URL sha=$SUPAVISE_STUDIO_ARTIFACT_SHA256\" >>" + record + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sd := &staged{st: &selfupdate.Staged{Path: bin}}
	sd.studio.name, sd.studio.url, sd.studio.sha, _ = ver.Studio("linux-amd64")
	if err := h.Prefetch(context.Background(), &nodeupgrade.Staged{Info: to, Data: sd}, n, p); err != nil {
		t.Fatalf("prefetch: %v\n%s", err, out)
	}
	if got := calls(t, record); !strings.HasSuffix(got, "artifacts fetch --studio url="+url+" sha="+sha) {
		t.Fatalf("prefetch ran %q", got)
	}

	// After the swap the new daemon renders Studio from the build's directory, which is what the
	// old driver's WaitShared waits for.
	renderStudio(t, cfg, build)
	if got, err := fleet.RenderedTag(cfg, config.SvcStudio); err != nil || got != m.To {
		t.Fatalf("rendered %q, %v; the wait wants %s", got, err, m.To)
	}
}

// studioNode is a node with config.toml at cfgPath naming url and sha for Studio.
func studioNode(t *testing.T, url, sha string) (*config.Config, string) {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Platform = "linux-amd64"
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("state_dir = %q\nplatform = \"linux-amd64\"\n", cfg.StateDir)
	if url != "" {
		body += fmt.Sprintf("\n[studio]\nartifact_url = %q\nartifact_sha256 = %q\n", url, sha)
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, cfgPath
}

// installBuild puts a build directory with a marker in place, as the fetch does.
func installBuild(t *testing.T, cfg *config.Config, dir, source, sha string) {
	t.Helper()
	d := cfg.Paths().Artifact(config.SvcStudio, dir)
	if err := os.MkdirAll(filepath.Join(d, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(artifacts.Marker{Service: config.SvcStudio, Tag: dir, Platform: "linux-amd64", SHA256: sha, Source: source})
	if err := os.WriteFile(filepath.Join(d, ".supavise-artifact.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func studioConfigOf(t *testing.T, cfgPath string) config.Studio {
	t.Helper()
	c, _, err := readConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return c.Studio
}

func noFetch(t *testing.T) func(context.Context, []string) error {
	return func(context.Context, []string) error { t.Error("fetch was called"); return nil }
}

func noRelease(t *testing.T) func(context.Context, string, string) (string, string, error) {
	return func(context.Context, string, string) (string, string, error) {
		t.Error("the release was asked")
		return "", "", errors.New("no")
	}
}

// After the upgrade fetched the build through the environment, converge makes config.toml name it.
func TestStudioSetupPointsConfigAtTheInstalledBuild(t *testing.T) {
	_, tag, build := pinned(t)
	oldURL := "https://github.com/supavise/supavise/releases/download/v0.1.3/" + artifacts.StudioAsset(tag+"-p2", "linux-amd64")
	cfg, cfgPath := studioNode(t, oldURL, strings.Repeat("a", 64))
	newURL := "https://github.com/supavise/supavise/releases/download/v0.2.1/" + artifacts.StudioAsset(build, "linux-amd64")
	installBuild(t, cfg, build, newURL, strings.Repeat("B", 64))
	var out bytes.Buffer
	s := &studioSetup{cfg: cfg, cfgPath: cfgPath, out: &out, errw: &out, fetch: noFetch(t), release: noRelease(t)}
	s.run(context.Background())
	if got := studioConfigOf(t, cfgPath); got.ArtifactURL != newURL || got.ArtifactSHA256 != strings.Repeat("b", 64) {
		t.Fatalf("config studio = %+v\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "names the dashboard build "+build) {
		t.Errorf("output: %s", out.String())
	}
	out.Reset()
	s.run(context.Background())
	if out.Len() != 0 {
		t.Errorf("a second run changed something: %s", out.String())
	}
}

// A binary that came without the upgrade's prefetch (self-update), on a node whose config.toml
// still names the old build: the fetch refuses the stale URL, and the build comes from the signed
// list of this binary's own release.
func TestStudioSetupFetchesTheBuildOfItsOwnRelease(t *testing.T) {
	_, tag, build := pinned(t)
	oldURL := "https://h/" + artifacts.StudioAsset(tag+"-p2", "linux-amd64")
	cfg, cfgPath := studioNode(t, oldURL, strings.Repeat("a", 64))
	renderStudio(t, cfg, tag)
	_, url, sha := studioSums(build, "linux-amd64")
	var envs [][]string
	var out bytes.Buffer
	s := &studioSetup{cfg: cfg, cfgPath: cfgPath, out: &out, errw: &out,
		fetch: func(_ context.Context, env []string) error {
			envs = append(envs, env)
			if env == nil {
				return errors.New("studio.artifact_url names the Studio build " + tag + "-p2")
			}
			installBuild(t, cfg, build, url, sha)
			return nil
		},
		release: func(_ context.Context, b, platform string) (string, string, error) {
			if b != build || platform != "linux-amd64" {
				t.Errorf("asked the release for %s %s", b, platform)
			}
			return url, sha, nil
		},
	}
	s.run(context.Background())
	if len(envs) != 2 || envs[1][0] != "SUPAVISE_STUDIO_ARTIFACT_URL="+url || envs[1][1] != "SUPAVISE_STUDIO_ARTIFACT_SHA256="+sha {
		t.Fatalf("fetches = %q\n%s", envs, out.String())
	}
	if got := studioConfigOf(t, cfgPath); got.ArtifactURL != url || got.ArtifactSHA256 != sha {
		t.Fatalf("config studio = %+v", got)
	}

	// Without a release to ask, Studio stays as it is and config.toml too.
	cfg, cfgPath = studioNode(t, oldURL, strings.Repeat("a", 64))
	renderStudio(t, cfg, tag)
	out.Reset()
	s = &studioSetup{cfg: cfg, cfgPath: cfgPath, out: &out, errw: &out,
		fetch:   func(context.Context, []string) error { return errors.New("stale") },
		release: func(context.Context, string, string) (string, string, error) { return "", "", errors.New("offline") }}
	s.run(context.Background())
	if !strings.Contains(out.String(), "keeps running the build it ran") || studioConfigOf(t, cfgPath).ArtifactURL != oldURL {
		t.Fatalf("output %s, config %+v", out.String(), studioConfigOf(t, cfgPath))
	}
}

// Converge leaves alone a node without a dashboard and one that has not rendered Studio yet (an
// install, whose `fleet start` fetches it).
func TestStudioSetupLeavesNodesWithoutARunningDashboard(t *testing.T) {
	cfg, cfgPath := studioNode(t, "", "")
	s := &studioSetup{cfg: cfg, cfgPath: cfgPath, out: &bytes.Buffer{}, errw: &bytes.Buffer{}, fetch: noFetch(t), release: noRelease(t)}
	s.run(context.Background())
	if studioConfigOf(t, cfgPath).ArtifactURL != "" {
		t.Fatal("a node without a dashboard got one")
	}
	cfg, cfgPath = studioNode(t, "https://h/studio.tar.zst", strings.Repeat("a", 64))
	s = &studioSetup{cfg: cfg, cfgPath: cfgPath, out: &bytes.Buffer{}, errw: &bytes.Buffer{}, fetch: noFetch(t), release: noRelease(t)}
	s.run(context.Background())
}

// A rollback gives config.toml back the build the restored binary runs, from the directory it left.
func TestSyncStudioConfigToTheRestoredBuild(t *testing.T) {
	_, tag, build := pinned(t)
	newURL := "https://h/" + artifacts.StudioAsset(build, "linux-amd64")
	cfg, cfgPath := studioNode(t, newURL, strings.Repeat("b", 64))
	oldURL := "https://h/" + artifacts.StudioAsset(tag+"-p2", "linux-amd64")
	installBuild(t, cfg, tag, oldURL, strings.Repeat("a", 64))
	var out bytes.Buffer
	if changed, err := syncStudioConfigTo(cfg, cfgPath, tag, &out); err != nil || !changed {
		t.Fatalf("%v, %v", changed, err)
	}
	if got := studioConfigOf(t, cfgPath); got.ArtifactURL != oldURL || got.ArtifactSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("config studio = %+v", got)
	}
	for _, pin := range []string{"", "2026.01.01-sha-0000000"} { // none, or not on disk
		if changed, err := syncStudioConfigTo(cfg, cfgPath, pin, &out); err != nil || changed {
			t.Fatalf("pin %q: %v, %v", pin, changed, err)
		}
	}
}

func TestStudioOfRelease(t *testing.T) {
	_, tag, build := pinned(t)
	ver, url, sha := studioSums(build, "linux-amd64")
	if u, s, err := studioOfRelease(ver, build, "linux-amd64"); err != nil || u != url || s != sha {
		t.Fatalf("%s %s %v", u, s, err)
	}
	if _, _, err := studioOfRelease(ver, build, "linux-arm64"); err == nil {
		t.Error("a platform the release lists no build for")
	}
	if _, _, err := studioOfRelease(ver, tag+"-p9", "linux-amd64"); err == nil {
		t.Error("a release that ships another build")
	}
	other, _, _ := studioSums(build, "linux-amd64")
	other.Manifest.Studio = tag
	if _, _, err := studioOfRelease(other, build, "linux-amd64"); err == nil {
		t.Error("a manifest that pins another build")
	}
}

// `supavise artifacts fetch --studio` with no service fetches Studio and nothing else: it is what
// the upgrade runs when only Studio moves, and the other artifacts are not the upgrade's to fetch.
func TestArtifactsFetchStudioAlone(t *testing.T) {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "bin/studio", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	var zb bytes.Buffer
	zw, _ := zstd.NewWriter(&zb)
	_, _ = zw.Write(tb.Bytes())
	_ = zw.Close()
	arch := zb.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(arch) }))
	defer srv.Close()
	sum := sha256.Sum256(arch)
	cfg, cfgPath := studioNode(t, srv.URL+"/studio.tar.zst", hex.EncodeToString(sum[:]))
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\n[artifacts]\nbase_url = \"http://127.0.0.1:1\"\n") // a slim artifact fetch would fail
	f.Close()
	oldPath, oldStudio := configPath, artifactsFetchStudio
	configPath, artifactsFetchStudio = cfgPath, true
	defer func() { configPath, artifactsFetchStudio = oldPath, oldStudio }()
	var out bytes.Buffer
	artifactsFetchCmd.SetOut(&out)
	artifactsFetchCmd.SetContext(context.Background())
	defer artifactsFetchCmd.SetOut(nil)
	if err := artifactsFetchCmd.RunE(artifactsFetchCmd, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	_, _, build := pinned(t)
	if got := strings.TrimSpace(out.String()); got != fmt.Sprintf("%-12s %s", config.SvcStudio, cfg.Paths().Artifact(config.SvcStudio, build)) {
		t.Fatalf("output %q", got)
	}
}
