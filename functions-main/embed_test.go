package functionsmain

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

var importRe = regexp.MustCompile(`(?m)(?:from|import)\s+'(\.[^']+)'`)

// A module that index.ts needs but the embed list lacks would only fail when the runtime
// starts, on a node. This test fails it in CI instead: the embedded files must be exactly
// the TypeScript files that are not tests, and every relative import must resolve inside them.
func TestEmbedHoldsTheDeployedModulesAndNothingElse(t *testing.T) {
	disk := map[string]bool{}
	for _, dir := range []string{".", "src"} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".ts") || strings.HasSuffix(n, "_test.ts") || n == "testutil.ts" {
				continue
			}
			disk[path.Join(dir, n)] = true // path.Join(".", "index.ts") is "index.ts"
		}
	}
	embedded := map[string]bool{}
	if err := fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			embedded[p] = true
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for p := range disk {
		if !embedded[p] {
			t.Errorf("%s is not embedded; add it to the go:embed line of embed.go", p)
		}
	}
	for p := range embedded {
		if !disk[p] {
			t.Errorf("%s is embedded but is not a deployed module", p)
		}
		body, err := fs.ReadFile(Files, p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range importRe.FindAllStringSubmatch(string(body), -1) {
			target := path.Join(path.Dir(p), m[1])
			if !embedded[target] {
				t.Errorf("%s imports %s, which is not embedded", p, m[1])
			}
		}
	}
	if len(embedded) < 7 {
		t.Errorf("only %d files embedded", len(embedded))
	}
}
