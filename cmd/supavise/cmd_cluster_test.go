package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/health"
	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/notimpl"
)

// run executes the CLI with args and returns what it printed and the error. The commands are
// package variables, so a flag one run sets would still be set in the next: run puts back the
// flags it set.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	defer func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil) }()
	err := rootCmd.ExecuteContext(context.Background())
	if c, _, ferr := rootCmd.Find(args); ferr == nil {
		for _, a := range args {
			if name, ok := strings.CutPrefix(a, "--"); ok {
				if f := c.Flags().Lookup(name); f != nil {
					_ = f.Value.Set(f.DefValue)
					f.Changed = false
				}
			}
		}
	}
	return out.String(), err
}

func TestHelpListsTheClusterCommands(t *testing.T) {
	top, err := run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node", "replicas", "failover", "storage"} {
		if !hasCommandLine(top, name) {
			t.Errorf("supavise --help does not list %q:\n%s", name, top)
		}
	}
	projects, _ := run(t, "projects", "--help")
	if !hasCommandLine(projects, "failover") {
		t.Errorf("supavise projects --help does not list failover:\n%s", projects)
	}
	node, _ := run(t, "node", "--help")
	for _, name := range []string{"token", "join", "ls", "rm", "rejoin"} {
		if !hasCommandLine(node, name) {
			t.Errorf("supavise node --help does not list %q:\n%s", name, node)
		}
	}
}

// hasCommandLine reports whether help text lists a command called name.
func hasCommandLine(help, name string) bool {
	for _, line := range strings.Split(help, "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == name {
			return true
		}
	}
	return false
}

// Every command of the cluster work is registered and says plainly that it is not there yet.
func TestClusterCommandsAreStubs(t *testing.T) {
	for _, args := range [][]string{
		{"node", "token", "--ttl", "30m"},
		{"node", "join", "svj1.example"},
		{"node", "join", "--resume"},
		{"node", "ls", "--dns"},
		{"node", "rm", "n2", "--yes"},
		{"node", "rejoin", "--leader", "10.0.0.1:7443"},
		{"replicas", "ls"},
		{"replicas", "add", "abcdefghijklmnopqrst", "--region", "eu-west-1", "--node", "n2"},
		{"replicas", "rm", "abcdefghijklmnopqrst-rr-eu-west-1-k3j9d2"},
		{"failover", "--dry-run"},
		{"failover", "--to", "n2", "--force", "--restore-missing", "--old-primary-is-down", "--yes"},
		{"projects", "failover", "abcdefghijklmnopqrst", "--to", "n2", "--dry-run"},
	} {
		_, err := run(t, args...)
		if !errors.Is(err, notimpl.Err) {
			t.Errorf("supavise %s: %v, want not implemented yet", strings.Join(args, " "), err)
		}
	}
	if _, err := run(t, "replicas", "add", "abcdefghijklmnopqrst"); err == nil || errors.Is(err, notimpl.Err) {
		t.Errorf("replicas add without --region: %v", err)
	}
}

// A node that is not in a cluster and not behind on its stack prints exactly the node report, in
// text and in JSON.
func TestStatusSectionsAreAbsentOnASingleNode(t *testing.T) {
	cfg := config.Default()
	sections := collectStatusSections(context.Background(), cfg)
	if sections.Cluster != nil || sections.Failover != nil || sections.Infrastructure != nil || len(sections.Errors) != 0 {
		t.Fatalf("sections on a single node: %+v", sections)
	}
	var b bytes.Buffer
	sections.render(&b)
	if b.Len() != 0 {
		t.Fatalf("rendered %q", b.String())
	}
	rep := &health.Report{CheckedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Version: "v0.1.1",
		Components: []health.Component{{Name: "daemon", State: health.OK, Critical: true, Detail: "up"}}}
	rep.Finish()
	var plain, wrapped bytes.Buffer
	if err := printJSON(&plain, rep); err != nil {
		t.Fatal(err)
	}
	if err := printJSON(&wrapped, statusJSON{Report: rep, statusSections: sections}); err != nil {
		t.Fatal(err)
	}
	if plain.String() != wrapped.String() {
		t.Fatalf("the JSON of a single node changed:\n%s\nvs\n%s", plain.String(), wrapped.String())
	}
}

func TestStatusSectionsRenderAndMarshal(t *testing.T) {
	s := statusSections{
		Cluster:        &clusterBlock{},
		Failover:       &failover.Readiness{Ready: true, Fencer: "aws", FencerStatus: "DryRun OK", EpochMarker: "store reachable"},
		Infrastructure: &infra.Report{Platform: "aws", Stack: "supavise", Have: 1, Need: 2, Fix: "sudo -E supavise upgrade --aws"},
		Errors:         map[string]string{"cluster": "registry unreachable"},
	}
	var b bytes.Buffer
	s.render(&b)
	for _, want := range []string{"failover  READY", "fencer: aws (DryRun OK)", "Infrastructure  AWS stack \"supavise\" is at revision 1", "Fix: sudo -E supavise upgrade --aws", "cluster  could not be read: registry unreachable"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
	rep := &health.Report{}
	rep.Finish()
	var j bytes.Buffer
	if err := printJSON(&j, statusJSON{Report: rep, statusSections: s}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"status"`, `"cluster"`, `"failover"`, `"infrastructure"`, `"errors"`} {
		if !strings.Contains(j.String(), key) {
			t.Errorf("JSON lacks %s:\n%s", key, j.String())
		}
	}
}
