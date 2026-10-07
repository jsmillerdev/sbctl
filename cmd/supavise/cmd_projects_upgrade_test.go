package main

import (
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/lifecycle"
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

// A project that runs newer releases than the node pins is listed as ahead and never as an
// upgrade, and its plan shows no changes.
func TestAheadOfTheNodeIsListedAndSkipped(t *testing.T) {
	c := lifecycle.ServiceChange{Service: "gotrue", From: "auth-v2.195.0-r1", To: "auth-v2.100.0-r1"}
	el := &lifecycle.UpgradeEligibility{Changes: []lifecycle.ServiceChange{c}, Ahead: []lifecycle.ServiceChange{c},
		Blockers: []lifecycle.UpgradeBlocker{{Type: lifecycle.BlockerNoUpgradePath, Message: "newer"}}}
	r := upgradeRow{El: el}
	if got := r.state(); !strings.HasPrefix(got, "ahead of the node") {
		t.Fatalf("state = %q", got)
	}
	if got := changesText(el); got != "-" {
		t.Fatalf("changes = %q", got)
	}
}
