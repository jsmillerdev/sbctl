package main

import (
	"context"
	"os"
	"path/filepath"
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
