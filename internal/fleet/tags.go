package fleet

import (
	"fmt"
	"os"
	"regexp"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/units"
)

// RenderedTag returns the release tag of svc that the node's unit is set to run: the artifact
// directory its launcher script (the .run file `Render` wrote) executes. svc is a shared service
// or the system project's postgres or gotrue. This is what the files say, not what the process
// maps; for a unit that is active and healthy after the Manager started it, they are the same,
// because Start restarts a unit whose files changed. `supavise upgrade` reads it to learn which
// release a node runs without asking the binary that rendered it, and to see that a shared
// service moved onto the new release.
func RenderedTag(cfg *config.Config, svc string) (string, error) {
	files := units.FilesFor(cfg, units.Spec{Service: svc})
	b, err := os.ReadFile(files.Run)
	if err != nil {
		return "", fmt.Errorf("fleet: %s has not been rendered: %w", svc, err)
	}
	return tagFromScript(cfg.Paths().Artifacts(), config.ArtifactName(svc), string(b))
}

func tagFromScript(artifactsDir, name, script string) (string, error) {
	re := regexp.MustCompile(regexp.QuoteMeta(artifactsDir) + `/` + regexp.QuoteMeta(name) + `/([^/'"\s]+)/`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		return "", fmt.Errorf("fleet: the launcher script of %s does not run an artifact under %s/%s", name, artifactsDir, name)
	}
	return m[1], nil
}
