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
	for _, flag := range []string{"all", "yes", "dry-run", "no-gc", "to", "allow-older"} {
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

func TestUpgradeRequestFromFlags(t *testing.T) {
	defer func() { upTo, upAllowOlder = nil, false }()
	upTo, upAllowOlder = []string{"auth=auth-v2.195.0-r1", "postgrest=postgrest-v16.4-r0"}, true
	req, err := upgradeRequest()
	if err != nil || !req.AllowOlder || req.Target["gotrue"] != "auth-v2.195.0-r1" || req.Target["postgrest"] != "postgrest-v16.4-r0" || len(req.Target) != 2 {
		t.Fatalf("request = %+v, %v (the release name auth is the service gotrue)", req, err)
	}
	upTo, upAllowOlder = nil, false
	if req, err = upgradeRequest(); err != nil || req.Target != nil || req.AllowOlder {
		t.Fatalf("no flags: %+v, %v", req, err)
	}
	for _, bad := range []string{"gotrue", "gotrue=", "realtime=realtime-v1-r0", "=x"} {
		upTo = []string{bad}
		if _, err := upgradeRequest(); err == nil {
			t.Errorf("--to %q was accepted", bad)
		}
	}
}
