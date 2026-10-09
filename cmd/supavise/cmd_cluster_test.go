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
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/infra"
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

// replicas add needs a region of its own: a missing flag is a usage error.
func TestReplicasAddNeedsARegion(t *testing.T) {
	if _, err := run(t, "replicas", "add", "abcdefghijklmnopqrst"); err == nil {
		t.Errorf("replicas add without --region: %v", err)
	}
}

// A node that is not in a cluster and not behind on its stack prints exactly the node report, in
// text and in JSON.
func TestStatusSectionsAreAbsentOnASingleNode(t *testing.T) {
	noHostBlock(t)
	cfg := config.Default()
	sections := collectStatusSections(context.Background(), cfg)
	if sections.Host != nil || sections.Cluster != nil || sections.Failover != nil || sections.Infrastructure != nil || len(sections.Errors) != 0 {
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

// noHostBlock keeps a test from checking the host layer of the machine it runs on.
func noHostBlock(t *testing.T) {
	t.Helper()
	old := hostBlockOf
	hostBlockOf = func(context.Context, *config.Config) (*hostBlock, error) { return nil, nil }
	t.Cleanup(func() { hostBlockOf = old })
}

// The status report shows the host layer: the steps with work, the command that does it, and the ones
// that could not be checked, in text and in JSON, and a host that cannot be read is named like any
// other block that fails.
func TestStatusShowsTheHostBlock(t *testing.T) {
	old := hostBlockOf
	t.Cleanup(func() { hostBlockOf = old })
	hostBlockOf = func(context.Context, *config.Config) (*hostBlock, error) {
		return &hostBlock{
			Pending:   []hostsetup.Result{{ID: "ufw", Title: "Open the mesh port in ufw when ufw is active", Pending: true}},
			Unchecked: []hostsetup.Result{{ID: "config-d", Title: "Refresh the cluster settings", Unknown: true, Detail: "the leader did not answer"}},
		}, nil
	}
	s := collectStatusSections(context.Background(), config.Default())
	var b bytes.Buffer
	s.render(&b)
	for _, want := range []string{"Host  1 step(s) of the host layer are pending", "pending  Open the mesh port", "Fix: sudo supavise system converge", "not checked  Refresh the cluster settings (the leader did not answer)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
	rep := &health.Report{}
	rep.Finish()
	var j bytes.Buffer
	if err := printJSON(&j, statusJSON{Report: rep, statusSections: s}); err != nil || !strings.Contains(j.String(), `"host"`) || !strings.Contains(j.String(), `"unchecked"`) {
		t.Fatalf("JSON: %v\n%s", err, j.String())
	}

	hostBlockOf = func(context.Context, *config.Config) (*hostBlock, error) {
		return nil, errors.New("converge cannot be built")
	}
	s = collectStatusSections(context.Background(), config.Default())
	b.Reset()
	s.render(&b)
	if !strings.Contains(b.String(), "host  could not be read: converge cannot be built") {
		t.Errorf("a host block that fails: %q", b.String())
	}
}
