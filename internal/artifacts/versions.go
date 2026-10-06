package artifacts

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	sbctl "github.com/OWNER/sbctl"
	"github.com/OWNER/sbctl/internal/config"
)

// Versions is the parsed versions.yaml: the slim-services release tag of every
// artifact, the Studio build tag and the CLI version the conformance suite tested.
type Versions struct {
	// Artifacts maps the slim-services release name (config.ArtifactName) to its tag,
	// for example "postgres" -> "postgres-17.11.0.004-r1".
	Artifacts map[string]string `yaml:"artifacts"`
	Studio    struct {
		Tag string `yaml:"tag"`
	} `yaml:"studio"`
	CLI struct {
		VersionTested string `yaml:"version_tested"`
	} `yaml:"cli"`
}

// ParseVersions parses a versions.yaml document.
func ParseVersions(b []byte) (*Versions, error) {
	var v Versions
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("artifacts: parse versions.yaml: %w", err)
	}
	if len(v.Artifacts) == 0 {
		return nil, fmt.Errorf("artifacts: versions.yaml lists no artifacts")
	}
	return &v, nil
}

// LoadVersions returns the versions.yaml named by cfg.Artifacts.VersionsFile, or the
// copy embedded in the binary when that is empty.
func LoadVersions(cfg *config.Config) (*Versions, error) {
	if f := cfg.Artifacts.VersionsFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("artifacts: %w", err)
		}
		return ParseVersions(b)
	}
	return ParseVersions(sbctl.VersionsYAML)
}

// Tag returns the pinned release tag of service svc (a config.Svc* name).
func (v *Versions) Tag(svc string) (string, error) {
	if svc == config.SvcStudio {
		if v.Studio.Tag == "" {
			return "", fmt.Errorf("artifacts: versions.yaml has no studio.tag")
		}
		return v.Studio.Tag, nil
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
