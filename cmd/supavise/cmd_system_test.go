package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
)

// runSplit is runRoot with stdout and stderr apart.
func runSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	err = rootCmd.Execute()
	rootCmd.SetArgs(nil)
	return out.String(), errOut.String(), err
}

// convergeNode is a config with the exec supervisor in a temporary state directory, and a unit
// directory to install into: the only thing converge does there is the unit files and the marker.
func convergeNode(t *testing.T) (cfgPath, units, state string) {
	t.Helper()
	dir := t.TempDir()
	state = filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o750); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "config.toml")
	body := "supervisor = \"exec\"\nstate_dir = " + `"` + state + `"` + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, filepath.Join(dir, "units"), state
}

func TestConvergeCommandsAreRegistered(t *testing.T) {
	for _, name := range []string{"converge", "install-units"} {
		c, _, err := rootCmd.Find([]string{"system", name})
		if err != nil || c == nil || c.Name() != name {
			t.Fatalf("system %s is not a command: %v", name, err)
		}
		for _, f := range []string{"check", "json", "unit-dir", "polkit-dir"} {
			if c.Flags().Lookup(f) == nil {
				t.Errorf("system %s has no --%s", name, f)
			}
		}
	}
}

// `install-units` is converge: what an older `supavise upgrade` runs on the new binary right after
// the swap installs the units and records the revision.
func TestInstallUnitsRunsConverge(t *testing.T) {
	cfg, units, state := convergeNode(t)
	args := []string{"--config", cfg, "system", "install-units", "--unit-dir", units, "--polkit-dir", "", "--check=false", "--json=false"}
	out, err := runRoot(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "installed "+filepath.Join(units, "supavise.service")) || !strings.Contains(out, "recorded converge revision") {
		t.Errorf("output:\n%s", out)
	}
	m, err := hostsetup.ReadMarker(state)
	if err != nil || m.Revision != hostsetup.Revision {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	// Again: nothing changes.
	out, err = runRoot(t, args...)
	if err != nil || !strings.Contains(out, "units are up to date") || !strings.Contains(out, "host is converged") || strings.Contains(out, "recorded") {
		t.Errorf("second run: %v\n%s", err, out)
	}
	// converge is the same thing under its own name.
	if out, err := runRoot(t, "--config", cfg, "system", "converge", "--unit-dir", units, "--polkit-dir", "", "--check=false", "--json=false"); err != nil || !strings.Contains(out, "host is converged") {
		t.Errorf("converge: %v\n%s", err, out)
	}
}

// --check --json is a list of {id, title, pending, needs_root}, and changes nothing.
func TestConvergeCheckJSON(t *testing.T) {
	cfg, units, state := convergeNode(t)
	check := func() []map[string]any {
		t.Helper()
		out, errOut, err := runSplit(t, "--config", cfg, "system", "converge", "--unit-dir", units, "--polkit-dir", "", "--check", "--json")
		if err != nil {
			t.Fatalf("%v\n%s", err, errOut)
		}
		var rs []map[string]any
		if err := json.Unmarshal([]byte(out), &rs); err != nil {
			t.Fatalf("%v in %q", err, out)
		}
		return rs
	}
	rs := check()
	pending := map[string]bool{}
	for _, r := range rs {
		for _, k := range []string{"id", "title", "pending", "needs_root"} {
			if _, ok := r[k]; !ok {
				t.Errorf("a step lacks %q: %v", k, r)
			}
		}
		pending[r["id"].(string)] = r["pending"].(bool)
	}
	if !pending["units"] || !pending["marker"] || len(rs) < 3 || rs[len(rs)-1]["id"] != "marker" {
		t.Fatalf("before a converge: %v", rs)
	}
	if _, err := os.Stat(units); err == nil {
		t.Error("--check wrote the unit directory")
	}
	if _, err := os.Stat(hostsetup.MarkerPath(state)); err == nil {
		t.Error("--check wrote the marker")
	}

	if out, err := runRoot(t, "--config", cfg, "system", "converge", "--unit-dir", units, "--polkit-dir", "", "--check=false", "--json=false"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, r := range check() {
		if r["pending"].(bool) {
			t.Errorf("pending after a converge: %v", r)
		}
	}
}

// With --json the standard output is the JSON and nothing else, also for a run that changes things.
func TestConvergeJSONKeepsProgressOffStdout(t *testing.T) {
	cfg, units, _ := convergeNode(t)
	out, errOut, err := runSplit(t, "--config", cfg, "system", "converge", "--unit-dir", units, "--polkit-dir", "", "--check=false", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	var rs []hostsetup.Result
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if len(rs[0].Changed) == 0 || !strings.Contains(errOut, "installed") {
		t.Errorf("results %+v, stderr %q", rs, errOut)
	}
}

func TestConvergeNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	cfg, _, _ := convergeNode(t)
	// Real unit directory, so not a test directory: the host is not ours to change.
	_, err := runRoot(t, "--config", cfg, "system", "converge", "--unit-dir", defaultUnitDir, "--polkit-dir", "", "--check=false", "--json=false")
	if err == nil || !strings.Contains(err.Error(), "run as root") {
		t.Errorf("err = %v", err)
	}
}

// The pending steps are what `supavise status` would show for the host.
func TestPendingStepsOfTheHostStatus(t *testing.T) {
	cfgPath, units, _ := convergeNode(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newConverger(cfg, units, "", true, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	pending := func() (ids []string) {
		for _, r := range c.Check(context.Background()) {
			if r.Pending {
				ids = append(ids, r.ID)
			}
		}
		return ids
	}
	if got := strings.Join(pending(), ","); got != "units,marker" {
		t.Errorf("before a converge: %s", got)
	}
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pending(); len(got) != 0 {
		t.Errorf("after: %v", got)
	}
}
