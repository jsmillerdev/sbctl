package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ManifestAsset is the release asset that describes the release to the software that installs
// it. `deploy/release-assets.sh` writes it and lists it in SHA256SUMS, so the signature covers
// it: a manifest the signed list does not name is not trusted.
const ManifestAsset = "release.json"

// Manifest is the content of release.json:
//
//	{"version": "v1.4.0", "min_upgrade_from": "v1.2.0"}
//
// version is the tag the release is published under. min_upgrade_from is the oldest release
// that may install this one directly (an earlier node installs an intermediate release first);
// a release that has no such limit omits it. Unknown fields are ignored, so later releases can
// add to the file.
type Manifest struct {
	Version        string `json:"version,omitempty"`
	MinUpgradeFrom string `json:"min_upgrade_from,omitempty"`
}

// ErrUnsupportedJump means the release's manifest does not allow installing it over the
// running version.
var ErrUnsupportedJump = errors.New("this release cannot be installed over the running version")

// ParseManifest reads a release.json.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestAsset, err)
	}
	if m.MinUpgradeFrom != "" {
		if _, ok := parseVersion(m.MinUpgradeFrom); !ok {
			return nil, fmt.Errorf("%s: min_upgrade_from %q is not a version like v1.2.3", ManifestAsset, m.MinUpgradeFrom)
		}
	}
	return &m, nil
}

// Check applies the manifest of the release published under tag to the running version current.
// A manifest that names another version is refused (assets re-attached to a different tag). A
// current version that is not a release ("dev", a commit-describe string) cannot be placed, and
// is allowed through: it is a developer's build, not a node.
func (m *Manifest) Check(tag, current string) error {
	if m.Version != "" && m.Version != tag {
		return fmt.Errorf("release %s ships a manifest for %s: refusing to install", tag, m.Version)
	}
	if m.MinUpgradeFrom == "" {
		return nil
	}
	cur, ok := parseVersion(current)
	if !ok {
		return nil
	}
	min, _ := parseVersion(m.MinUpgradeFrom)
	if compareVersions(cur, min) < 0 {
		return fmt.Errorf("%w: %s can be installed from %s or newer, and this node runs %s; install %s (or a later release below %s) first",
			ErrUnsupportedJump, tag, m.MinUpgradeFrom, strings.TrimSpace(current), m.MinUpgradeFrom, tag)
	}
	return nil
}

func compareVersions(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Compare orders two release tags: negative when a is older than b, positive when it is newer.
// ok is false when either is not a release version.
func Compare(a, b string) (c int, ok bool) {
	x, ok1 := parseVersion(a)
	y, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return compareVersions(x, y), true
}
