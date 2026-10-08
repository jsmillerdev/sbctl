package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
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

// Naming a project selects that project and no other, whatever its place in the registry: the
// first ref found used to empty the wanted set, which then let every later project through, so
// `projects upgrade <ref>` upgraded the projects registered after <ref> too.
func TestSelectProjectsNamesOnlyTheRef(t *testing.T) {
	ps := []registry.Project{{Ref: config.SystemRef}, {Ref: "aaa"}, {Ref: "bbb"}, {Ref: "ccc"}}
	refsOf := func(sel []*registry.Project) []string {
		var out []string
		for _, p := range sel {
			out = append(out, p.Ref)
		}
		return out
	}
	for _, c := range []struct {
		refs, want []string
	}{
		{[]string{"aaa"}, []string{"aaa"}},
		{[]string{"bbb"}, []string{"bbb"}},
		{[]string{"ccc"}, []string{"ccc"}},
		{[]string{"ccc", "aaa"}, []string{"aaa", "ccc"}},
		{nil, []string{"aaa", "bbb", "ccc"}},
	} {
		sel, err := selectProjects(ps, c.refs)
		if err != nil || !slices.Equal(refsOf(sel), c.want) {
			t.Errorf("refs %v: selected %v, %v; want %v", c.refs, refsOf(sel), err, c.want)
		}
	}
	for _, refs := range [][]string{{"zzz"}, {"aaa", "zzz"}, {config.SystemRef}} {
		if _, err := selectProjects(ps, refs); !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("refs %v: err = %v, want not found", refs, err)
		}
	}
}

// Without a ref, a node upgrades the projects it runs; a project homed on another node is that
// node's, and the rollout does not halt on it.
func TestSplitByHomeLeavesOtherNodesProjectsToThem(t *testing.T) {
	ps := []*registry.Project{
		{Ref: "aaaaaaaaaaaaaaaaaaaa", NodeID: "n1"},
		{Ref: "bbbbbbbbbbbbbbbbbbbb", NodeID: "n2"},
		{Ref: "cccccccccccccccccccc"}, // a registry that predates homes
	}
	here, elsewhere := splitByHome(ps, "n1")
	if len(here) != 2 || here[0].Ref != "aaaaaaaaaaaaaaaaaaaa" || here[1].Ref != "cccccccccccccccccccc" || len(elsewhere) != 1 || elsewhere[0] != "bbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("here %v, elsewhere %v", here, elsewhere)
	}
	if here, elsewhere := splitByHome(ps, ""); len(here) != 3 || len(elsewhere) != 0 {
		t.Errorf("an engine that does not know its node keeps every project: %v %v", here, elsewhere)
	}
}
