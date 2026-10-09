package versions

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// studio/PATCHSET names the patch set in the build's asset (studio/build.sh) and versions.yaml
// names it in the binary (the Studio build a node runs, `supavise upgrade` compares and the release
// manifest states). A difference would ship a binary that pins a build the release does not carry.
func TestStudioPatchsetMatchesStudioPATCHSET(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "studio", "PATCHSET"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("studio/PATCHSET: %v", err)
	}
	var v struct {
		Studio struct {
			Tag      string `yaml:"tag"`
			Patchset int    `yaml:"patchset"`
		} `yaml:"studio"`
	}
	if err := yaml.Unmarshal(VersionsYAML, &v); err != nil {
		t.Fatal(err)
	}
	if v.Studio.Tag == "" || v.Studio.Patchset <= 0 {
		t.Fatalf("versions.yaml studio = %+v: want a tag and a patch set", v.Studio)
	}
	if v.Studio.Patchset != file {
		t.Fatalf("versions.yaml studio.patchset is %d and studio/PATCHSET is %d: change them together", v.Studio.Patchset, file)
	}
}
