package artifacts

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/versions"
)

// Versions is the parsed internal/versions/versions.yaml: the slim-services release tag of every
// artifact, the Studio build and the CLI version the conformance suite tested.
type Versions struct {
	// Artifacts maps the slim-services release name (config.ArtifactName) to its tag,
	// for example "postgres" -> "postgres-17.11.0.004-r1".
	Artifacts map[string]string `yaml:"artifacts"`
	Studio    struct {
		// Tag is the upstream Studio git ref the build is made from.
		Tag string `yaml:"tag"`
		// Patchset is the revision of our patches and runtime on top of Tag (studio/PATCHSET, which
		// a test and the release tool hold equal to it). 0, a versions.yaml written before the field,
		// means the build is named by Tag alone, as every release up to v0.2.0 named it.
		Patchset int `yaml:"patchset"`
	} `yaml:"studio"`
	CLI struct {
		VersionTested string `yaml:"version_tested"`
	} `yaml:"cli"`
}

// StudioBuild is the name of the Studio build these versions pin: the upstream tag and the patch
// set, "2026.10.05-sha-94b8b06-p3", which is also what the release asset carries
// (StudioAsset). It is the build's identity everywhere a node decides whether it has the right
// Studio: the directory under artifacts/studio, the pin the upgrade plan and the GC compare, and
// the release manifest's "studio". Two builds of one upstream tag with different patches are two
// builds. Without a patch set it is the tag alone ("" without a tag).
func (v *Versions) StudioBuild() string {
	return StudioBuildName(v.Studio.Tag, v.Studio.Patchset)
}

// StudioBuildName names the Studio build of an upstream tag and a patch set (see StudioBuild).
func StudioBuildName(tag string, patchset int) string {
	if tag == "" || patchset <= 0 {
		return tag
	}
	return tag + "-p" + strconv.Itoa(patchset)
}

// studioAssetPrefix starts the name of every Studio build a release ships (studio/build.sh).
const studioAssetPrefix = "supavise-studio-"

// StudioAsset is the release asset name of a Studio build for platform:
// supavise-studio-<build>-<platform>.tar.zst, as studio/build.sh writes it.
func StudioAsset(build, platform string) string {
	return studioAssetPrefix + build + "-" + platform + ".tar.zst"
}

// StudioBuildOfURL returns the build that a Studio download URL names when its last path element
// is a release asset name (StudioAsset) for platform. ok is false for any other name, such as a
// stand-in build an operator or a test points at, which names no build that can be compared.
func StudioBuildOfURL(url, platform string) (build string, ok bool) {
	name := url
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	name = name[strings.LastIndex(name, "/")+1:]
	rest, ok := strings.CutPrefix(name, studioAssetPrefix)
	if !ok {
		return "", false
	}
	build, ok = strings.CutSuffix(rest, "-"+platform+".tar.zst")
	if !ok || build == "" {
		return "", false
	}
	return build, true
}

// ParseVersions parses a internal/versions/versions.yaml document.
func ParseVersions(b []byte) (*Versions, error) {
	var v Versions
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("artifacts: parse versions.yaml: %w", err)
	}
	if len(v.Artifacts) == 0 {
		return nil, fmt.Errorf("artifacts: versions.yaml lists no artifacts")
	}
	if v.Studio.Patchset < 0 {
		return nil, fmt.Errorf("artifacts: versions.yaml studio.patchset %d is negative", v.Studio.Patchset)
	}
	return &v, nil
}

// LoadVersions returns the internal/versions/versions.yaml named by cfg.Artifacts.VersionsFile, or the
// copy embedded in the binary when that is empty.
func LoadVersions(cfg *config.Config) (*Versions, error) {
	if f := cfg.Artifacts.VersionsFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("artifacts: %w", err)
		}
		return ParseVersions(b)
	}
	return ParseVersions(versions.VersionsYAML)
}

// Tag returns the pinned release tag of service svc (a config.Svc* name). For Studio it is the
// build (StudioBuild), not the upstream tag alone.
func (v *Versions) Tag(svc string) (string, error) {
	if svc == config.SvcStudio {
		if v.Studio.Tag == "" {
			return "", fmt.Errorf("artifacts: versions.yaml has no studio.tag")
		}
		return v.StudioBuild(), nil
	}
	tag := v.Artifacts[config.ArtifactName(svc)]
	if tag == "" {
		return "", fmt.Errorf("artifacts: versions.yaml pins no artifact for %q", svc)
	}
	return tag, nil
}

// Platform returns cfg.Platform, or "<goos>-<goarch>" of the running binary. Only
// linux-amd64, linux-arm64 and darwin-arm64 have builds.
func Platform(cfg *config.Config) (string, error) {
	p := cfg.Platform
	if p == "" {
		p = runtime.GOOS + "-" + runtime.GOARCH
	}
	switch p {
	case "linux-amd64", "linux-arm64", "darwin-arm64":
		return p, nil
	}
	return "", fmt.Errorf("artifacts: no slim-services build for platform %q", p)
}

// archiveName is the release asset name of tag for platform.
func archiveName(tag, platform string) string { return tag + "-" + platform + ".tar.zst" }

// parseSHA256SUMS returns the digest listed for file in a coreutils-format SHA256SUMS body.
func parseSHA256SUMS(body []byte, file string) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		sum, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name = strings.TrimLeft(name, " *")
		if name == file {
			if len(sum) != 64 {
				return "", fmt.Errorf("artifacts: malformed digest for %s in SHA256SUMS", file)
			}
			return strings.ToLower(sum), nil
		}
	}
	return "", fmt.Errorf("artifacts: %s is not listed in SHA256SUMS", file)
}
