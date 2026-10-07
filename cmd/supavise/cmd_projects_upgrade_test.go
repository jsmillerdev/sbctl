package main

import (
	"strings"
	"testing"
)

func TestProjectsUpgradeCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{{"projects", "upgrade"}, {"projects", "versions"}, {"artifacts", "gc"}} {
		cmd, _, err := rootCmd.Find(path)
		if err != nil || cmd == nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("%v is not a command: %v", path, err)
		}
	}
	up, _, _ := rootCmd.Find([]string{"projects", "upgrade"})
	for _, flag := range []string{"all", "yes", "dry-run", "no-gc"} {
		if up.Flags().Lookup(flag) == nil {
			t.Errorf("projects upgrade has no --%s", flag)
		}
	}
}

// The arguments are checked before a node is opened, so a typo costs nothing.
func TestProjectsUpgradeNeedsOneProjectOrAll(t *testing.T) {
	for _, args := range [][]string{
		{"projects", "upgrade", "--all=false"},
		{"projects", "upgrade", "--all", "abcdefghijklmnopqrst"},
		{"projects", "upgrade", "--all=false", "a", "b"},
	} {
		_, err := runRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "name one project, or pass --all") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	runRoot(t, "projects", "upgrade", "--all=false") // leave the flag as it was
}
