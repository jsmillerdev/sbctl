package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// After a swap, self-update runs the host layer of the release it installed: `converge` when the
// release has one, `install-units` for an older one that --force installed (it has no converge, and
// `supavise system converge` there would print the help of `system` and succeed).
func TestHostLayerCommandFollowsTheRelease(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	info := func(rev string) string {
		return `[ "$1" = release-info ] && echo '{"version":"v1.2.0","pins":{"gotrue":"auth-v1"},"registry_schema":"1.sql","registry_migrations":["1.sql"],"converge_revision":` + rev + `,"infra_revision":0}' && exit 0
exit 1
`
	}
	for name, c := range map[string]struct {
		exe  string
		want string
	}{
		"a release with a host layer":          {script("new", info("2")), "converge"},
		"a release from before the host layer": {script("older", info("0")), "install-units"},
		"a release that has no release-info":   {script("oldest", "exit 2\n"), "install-units"},
		"a binary that cannot be run":          {filepath.Join(dir, "missing"), "install-units"},
		"a release that prints something else": {script("odd", "echo hello\n"), "install-units"},
	} {
		if got := hostLayerCommand(context.Background(), c.exe); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// self-update runs the host layer through the binary it installed, and the warning names the command
// that ran.
func TestApplyHostLayerRunsTheCommandOfTheRelease(t *testing.T) {
	dir := t.TempDir()
	stub := func(name, rev, systemExit string) (exe, argvFile string) {
		t.Helper()
		argvFile = filepath.Join(dir, name+".argv")
		body := `#!/bin/sh
if [ "$1" = release-info ]; then
  echo '{"version":"v1.2.0","pins":{"gotrue":"auth-v1"},"registry_schema":"1.sql","registry_migrations":["1.sql"],"converge_revision":` + rev + `,"infra_revision":0}'
  exit 0
fi
echo "$*" >> ` + argvFile + `
exit ` + systemExit + `
`
		exe = filepath.Join(dir, name)
		if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return exe, argvFile
	}
	argv := func(f string) string {
		b, _ := os.ReadFile(f)
		return strings.TrimSpace(string(b))
	}

	exe, rec := stub("new", "2", "0")
	var out, errOut bytes.Buffer
	applyHostLayer(context.Background(), exe, &out, &errOut)
	if got := argv(rec); got != "system converge" || errOut.Len() != 0 {
		t.Errorf("a release with a host layer ran %q (stderr %q), want system converge", got, errOut.String())
	}

	exe, rec = stub("older", "0", "0")
	errOut.Reset()
	applyHostLayer(context.Background(), exe, &out, &errOut)
	if got := argv(rec); got != "system install-units" || errOut.Len() != 0 {
		t.Errorf("a release from before the host layer ran %q (stderr %q), want system install-units", got, errOut.String())
	}

	exe, rec = stub("failing", "0", "1")
	errOut.Reset()
	applyHostLayer(context.Background(), exe, &out, &errOut)
	if got := argv(rec); got != "system install-units" || !strings.Contains(errOut.String(), "supavise system install-units failed") {
		t.Errorf("a failing host layer ran %q and warned %q, want a warning that names system install-units", got, errOut.String())
	}
}
