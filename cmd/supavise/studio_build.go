package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

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

// studioStepTimeout bounds the Studio step of converge, and studioReleaseTimeout the lookup of the
// binary's own release in it. Converge runs in the upgrade's swap, in `self-update`, `install` and
// `update config`, and none of them should wait long for a dashboard build a host cannot reach:
// past the bound the step gives up with its warning, and Studio keeps the build it runs.
const (
	studioStepTimeout    = 10 * time.Minute
	studioReleaseTimeout = 2 * time.Minute
)

// studioRestartHint says when Studio runs a build the step installed: converge renders no unit of
// the daemon's and restarts nothing.
const studioRestartHint = "Studio runs it once supavise.service restarts: `supavise upgrade` and `supavise self-update` restart it, and after `sudo supavise system converge` alone, `sudo systemctl restart supavise.service` does (the projects keep running)"

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
	ctx, cancel := context.WithTimeout(ctx, studioStepTimeout)
	defer cancel()
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
			fmt.Fprintf(s.errw, "warning: studio: the dashboard build %s is not installed (%v); Studio keeps running the build it ran. `sudo supavise upgrade` installs it and restarts Studio on it (as do `sudo supavise system converge`, which tries again, and then `sudo systemctl restart supavise.service`).\n", build, err)
			return
		}
	}
	if _, err := syncStudioConfig(s.cfgPath, dir, s.out); err != nil {
		fmt.Fprintf(s.errw, "warning: studio: %v\n", err)
	}
	if runs, err := fleet.RenderedTag(s.cfg, config.SvcStudio); err == nil && runs != build {
		fmt.Fprintf(s.out, "studio: the dashboard build %s is installed and Studio runs %s. %s.\n", build, runs, studioRestartHint)
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
//
// It asks the default release repository with the release keys built into the binary: a node that
// upgrades from another repository (`--repo`) gets the build from `supavise upgrade`, whose prefetch
// uses the options of that run.
func ownReleaseStudio(ctx context.Context, build, platform string) (string, string, error) {
	if _, ok := selfupdate.ParseVersion(version); !ok {
		return "", "", fmt.Errorf("this binary (%s) is not a release, so it has no release to fetch Studio from", version)
	}
	ctx, cancel := context.WithTimeout(ctx, studioReleaseTimeout)
	defer cancel()
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
// name the build installed in dir, when they name another build: the file then gets the URL and
// digest the build's marker says it came from. The fetch takes a build's URL from the environment
// during an upgrade, so this is how the file catches up with the build the node runs (after an
// upgrade, at converge) and with the build it runs again (after a rollback).
//
// Only a URL whose file name is a release asset of another build (artifacts.StudioBuildOfURL) is
// replaced. A pair that names the installed build (a mirror, or the same build from an earlier
// release), a URL that names no build (a stand-in, a deliberate choice), a file without a Studio URL
// (a node without a dashboard) and a build whose marker names no URL are left alone. The two keys
// are edited in place, so the rest of the file keeps its bytes, comments included. It reports
// whether the file changed.
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
	platform := m.Platform
	if platform == "" {
		platform = file.Platform
	}
	named, recognized := artifacts.StudioBuildOfURL(file.Studio.ArtifactURL, platform)
	installed := m.Tag
	if b, ok := artifacts.StudioBuildOfURL(m.Source, platform); ok {
		installed = b
	}
	if !recognized || named == installed {
		return false, nil
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		return false, err
	}
	edited, err := setStudioArtifact(body, m.Source, sha)
	if err == nil {
		err = checkStudioEdit(file, edited, m.Source, sha)
	}
	if err != nil {
		return false, fmt.Errorf("%s names the dashboard build %s and the node runs %s; the file was not edited (%v): set [studio] artifact_url = %q and artifact_sha256 = %q", cfgPath, named, installed, err, m.Source, sha)
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(cfgPath); err == nil {
		mode = fi.Mode().Perm()
	}
	uid, gid := supaviseOwner()
	if err := writeFileAtomic(cfgPath, edited, mode, uid, gid); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "studio: %s names the dashboard build %s (%s)\n", cfgPath, installed, m.Source)
	return true, nil
}

// checkStudioEdit makes sure that the edited file reads as the file did with the Studio URL and
// digest set to url and sha, and differs in nothing else.
func checkStudioEdit(before *config.Config, edited []byte, url, sha string) error {
	got := config.Default()
	if err := config.DecodeTOML(edited, got); err != nil {
		return err
	}
	want := *before
	want.Studio.ArtifactURL, want.Studio.ArtifactSHA256 = url, sha
	if !reflect.DeepEqual(got, &want) {
		return errors.New("the edit would change more than [studio] artifact_url and artifact_sha256")
	}
	return nil
}

// setStudioArtifact returns the config file body with [studio] artifact_url and artifact_sha256
// set to url and sha, and every other byte as it was. It replaces the value of each key where the
// [studio] table holds it, and adds a key the table lacks right below its header. A file that holds
// them another way (dotted keys, an inline table, a multi-line string) is an error.
func setStudioArtifact(body []byte, url, sha string) ([]byte, error) {
	keys := []string{"artifact_url", "artifact_sha256"}
	values := map[string]string{"artifact_url": url, "artifact_sha256": sha}
	lines := strings.SplitAfter(string(body), "\n")
	section, header := "", -1
	found := map[string]int{}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			section = tomlTableName(t)
			if section == "studio" {
				if header >= 0 {
					return nil, errors.New("the file has two [studio] tables")
				}
				header = i
			}
			continue
		}
		if section != "studio" {
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		for _, key := range keys {
			if strings.TrimSpace(k) == key {
				if _, dup := found[key]; dup {
					return nil, fmt.Errorf("[studio] sets %s twice", key)
				}
				found[key] = i
			}
		}
	}
	if header < 0 {
		return nil, errors.New("the file has no [studio] table")
	}
	for key, i := range found {
		l, err := setTOMLValue(lines[i], values[key])
		if err != nil {
			return nil, fmt.Errorf("[studio] %s: %w", key, err)
		}
		lines[i] = l
	}
	var add []string
	for _, key := range keys {
		if _, ok := found[key]; !ok {
			add = append(add, key+" = "+tomlQuote(values[key])+"\n")
		}
	}
	if len(add) > 0 {
		if !strings.HasSuffix(lines[header], "\n") {
			lines[header] += "\n"
		}
		lines = append(lines[:header+1], append(add, lines[header+1:]...)...)
	}
	return []byte(strings.Join(lines, "")), nil
}

// tomlTableName is the name of the table a header line opens ("[studio] # c" is "studio"), or ""
// for any other line that starts with a bracket (an array of tables, a quoted name).
func tomlTableName(t string) string {
	if strings.HasPrefix(t, "[[") {
		return ""
	}
	name, rest, ok := strings.Cut(t[1:], "]")
	if rest = strings.TrimSpace(rest); !ok || (rest != "" && !strings.HasPrefix(rest, "#")) {
		return ""
	}
	return strings.TrimSpace(name)
}

// setTOMLValue replaces the one-line string value of the key = value line with v, keeping the key,
// the spacing and a comment after the value.
func setTOMLValue(line, v string) (string, error) {
	eq := strings.Index(line, "=")
	prefix, rest := line[:eq+1], line[eq+1:]
	val := strings.TrimLeft(rest, " \t")
	ws := rest[:len(rest)-len(val)]
	if ws == "" {
		ws = " "
	}
	end := -1
	switch {
	case strings.HasPrefix(val, `"""`), strings.HasPrefix(val, "'''"):
		return "", errors.New("a multi-line string")
	case strings.HasPrefix(val, `"`):
		for i := 1; i < len(val); i++ {
			if val[i] == '\\' {
				i++
			} else if val[i] == '"' {
				end = i
				break
			}
		}
	case strings.HasPrefix(val, "'"):
		if i := strings.Index(val[1:], "'"); i >= 0 {
			end = i + 1
		}
	}
	if end < 0 {
		return "", errors.New("not a one-line string")
	}
	return prefix + ws + tomlQuote(v) + val[end+1:], nil
}

// tomlQuote is s as a TOML basic string.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
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
