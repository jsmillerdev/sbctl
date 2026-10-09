package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/selfupdate"
)

// The dashboard a node runs is one Studio build, named by the upstream tag and the patch set
// (artifacts.Versions.StudioBuild) and unpacked under artifacts/studio/<build>. `supavise upgrade`
// moves it like a shared service: the plan compares the build the node's unit runs with the one
// the new binary reports, the new binary fetches the release's asset before anything stops (the
// signed checksum list gives the driver its URL and SHA-256), and the new daemon restarts Studio on
// it. What follows is the part the new binary does on its own, in `system converge`, which every
// driver runs on it right after the swap (a v0.1.x driver as `install-units`), before the daemon
// restarts: make sure the build it pins is on disk and that config.toml names that build.

// studioSetup is the Studio step of `system converge`.
type studioSetup struct {
	cfg     *config.Config // as loaded, environment included
	cfgPath string         // the file root edits
	out     io.Writer
	errw    io.Writer
	// fetch runs `supavise artifacts fetch --studio` with env added, as the supavise user.
	fetch func(ctx context.Context, env []string) error
	// release returns the URL and SHA-256 of build for platform from the signed checksum list of
	// this binary's own release.
	release func(ctx context.Context, build, platform string) (url, sha string, err error)
}

// newStudioSetup is the Studio step for the installed binary that runs it.
func newStudioSetup(cfg *config.Config, cfgPath string, out, errw io.Writer) *studioSetup {
	return &studioSetup{cfg: cfg, cfgPath: cfgPath, out: out, errw: errw,
		fetch: func(ctx context.Context, env []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			c, err := supaviseCommand(ctx, os.Geteuid() == 0, cfgPath, cfg.StateDir, env, exe, "artifacts", "fetch", "--studio")
			if err != nil {
				return err
			}
			c.Stdout, c.Stderr = out, errw
			return c.Run()
		},
		release: ownReleaseStudio,
	}
}

// run puts the pinned build in place when the node runs a dashboard and does not have it, then
// points config.toml at the installed build. It never fails converge: a Studio that cannot be
// brought forward keeps running the build it ran, and the warning says so.
//
// The upgrade's prefetch has normally fetched the build already, and then only config.toml
// changes. The build is missing when the binary came another way (`supavise self-update`), or when
// its directory was removed. The fetch first tries what is on disk and in config.toml (the build
// under its directory from before the patch set, or a URL that names the build); a config.toml
// that names another build is refused by the fetch, and then the build comes from this binary's
// own release: its checksum list is verified against the release key built into the binary, so the
// SHA-256 is one the binary can trust. A binary that is not a release, or a release that cannot be
// reached, leaves Studio as it is.
func (s *studioSetup) run(ctx context.Context) {
	file, existed, err := readConfigFile(s.cfgPath)
	if err != nil {
		fmt.Fprintf(s.errw, "warning: studio: %v\n", err)
		return
	}
	if !existed || file.Studio.ArtifactURL == "" {
		return // no dashboard on this node
	}
	store, err := artifacts.New(s.cfg)
	if err != nil {
		fmt.Fprintf(s.errw, "warning: studio: %v\n", err)
		return
	}
	build, err := store.Tag(config.SvcStudio)
	if err != nil {
		return
	}
	dir, err := store.Dir(config.SvcStudio)
	if err != nil {
		// A node that has not rendered Studio yet (an install that has not reached `fleet start`)
		// gets the build there.
		if _, rerr := fleet.RenderedTag(s.cfg, config.SvcStudio); rerr != nil {
			return
		}
		fmt.Fprintf(s.out, "studio: fetching the dashboard build %s\n", build)
		if dir, err = s.bringForward(ctx, store, build); err != nil {
			fmt.Fprintf(s.errw, "warning: studio: the dashboard build %s is not installed (%v); Studio keeps running the build it ran. `sudo supavise system converge` tries again.\n", build, err)
			return
		}
	}
	if _, err := syncStudioConfig(s.cfgPath, dir, s.out); err != nil {
		fmt.Fprintf(s.errw, "warning: studio: %v\n", err)
	}
}

func (s *studioSetup) bringForward(ctx context.Context, store *artifacts.Store, build string) (string, error) {
	first := s.fetch(ctx, nil)
	if first == nil {
		return store.Dir(config.SvcStudio)
	}
	url, sha, err := s.release(ctx, build, store.Platform())
	if err != nil {
		return "", fmt.Errorf("%v; and from this binary's release: %v", first, err)
	}
	if err := s.fetch(ctx, []string{"SUPAVISE_STUDIO_ARTIFACT_URL=" + url, "SUPAVISE_STUDIO_ARTIFACT_SHA256=" + sha}); err != nil {
		return "", err
	}
	return store.Dir(config.SvcStudio)
}

// ownReleaseStudio reads the signed checksum list of the release this binary is (version) and
// returns its Studio build for platform.
func ownReleaseStudio(ctx context.Context, build, platform string) (string, string, error) {
	if _, ok := selfupdate.ParseVersion(version); !ok {
		return "", "", fmt.Errorf("this binary (%s) is not a release, so it has no release to fetch Studio from", version)
	}
	ver, err := selfupdate.Fetch(ctx, selfupdate.Options{Tag: version, Current: version, Platform: platform})
	if err != nil {
		return "", "", err
	}
	return studioOfRelease(ver, build, platform)
}

// studioOfRelease returns the URL and SHA-256 of the Studio asset a verified release lists for
// platform, which must be build: the release of a binary ships the build the binary pins.
func studioOfRelease(ver *selfupdate.Verified, build, platform string) (string, string, error) {
	name, url, sha, ok := ver.Studio(platform)
	if !ok {
		return "", "", fmt.Errorf("release %s lists no Studio build for %s", ver.Release.Tag, platform)
	}
	if want := artifacts.StudioAsset(build, platform); name != want {
		return "", "", fmt.Errorf("release %s ships %s and this binary runs the build %s", ver.Release.Tag, name, build)
	}
	if ver.Manifest != nil && ver.Manifest.Studio != "" && ver.Manifest.Studio != build {
		return "", "", fmt.Errorf("the manifest of release %s pins Studio %s and this binary runs %s", ver.Release.Tag, ver.Manifest.Studio, build)
	}
	return url, sha, nil
}

// syncStudioConfig makes [studio] artifact_url and artifact_sha256 of the config file at cfgPath
// name the build installed in dir: the URL and digest its marker says it came from. The fetch
// takes a build's URL from the environment during an upgrade, so this is how the file catches up
// with the build the node runs (after an upgrade, at converge) and with the build it runs again
// (after a rollback). A file without a Studio URL (a node without a dashboard) and a build whose
// marker names no URL are left alone. It reports whether the file changed.
func syncStudioConfig(cfgPath, dir string, out io.Writer) (bool, error) {
	m, err := artifacts.ReadMarker(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("the marker of %s: %w", dir, err)
	}
	if (!strings.HasPrefix(m.Source, "https://") && !strings.HasPrefix(m.Source, "http://")) || len(m.SHA256) != 64 {
		return false, nil
	}
	file, existed, err := readConfigFile(cfgPath)
	if err != nil {
		return false, err
	}
	if !existed || file.Studio.ArtifactURL == "" {
		return false, nil
	}
	sha := strings.ToLower(m.SHA256)
	if file.Studio.ArtifactURL == m.Source && strings.ToLower(file.Studio.ArtifactSHA256) == sha {
		return false, nil
	}
	file.Studio.ArtifactURL, file.Studio.ArtifactSHA256 = m.Source, sha
	body, err := renderConfig(file)
	if err != nil {
		return false, err
	}
	uid, gid := supaviseOwner()
	if err := writeFileAtomic(cfgPath, body, 0o600, uid, gid); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "studio: %s names the dashboard build %s (%s)\n", cfgPath, m.Tag, m.Source)
	return true, nil
}

// syncStudioConfigTo is syncStudioConfig for the Studio build pin, when it is installed: a rollback
// gives config.toml back the build the restored binary runs.
func syncStudioConfigTo(cfg *config.Config, cfgPath, pin string, out io.Writer) (bool, error) {
	if pin == "" {
		return false, nil
	}
	dir := cfg.Paths().Artifact(config.SvcStudio, pin)
	if _, err := os.Stat(dir); err != nil {
		return false, nil
	}
	return syncStudioConfig(cfgPath, dir, out)
}
