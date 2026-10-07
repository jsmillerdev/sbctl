package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// A stub `gh api PATH` answers from a fixture file named after the path.
func gateRig(t *testing.T) (dir string, run func(env ...string) (string, error)) {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir = t.TempDir()
	stub := filepath.Join(dir, "bin", "gh")
	if err := os.MkdirAll(filepath.Dir(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env bash
[[ $1 == api ]] || exit 2
f="$GATE_FIXTURES/$(printf '%s' "$2" | tr -c 'A-Za-z0-9' _).json"
[[ -f $f ]] || { echo "no fixture for $2 ($f)" >&2; exit 1; }
cat "$f"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "fx"), 0o755); err != nil {
		t.Fatal(err)
	}
	run = func(env ...string) (string, error) {
		c := exec.Command("bash", "../release-gate.sh", sha)
		c.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "GATE_FIXTURES="+filepath.Join(dir, "fx"),
			"GITHUB_REPOSITORY=o/r", "GATE_POLL_SECONDS=0", "GATE_GRACE_SECONDS=0", "GATE_TIMEOUT_SECONDS=1")
		c.Env = append(c.Env, env...)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	return dir, run
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

func fixture(t *testing.T, dir, path, body string) {
	t.Helper()
	name := nonAlnum.ReplaceAllString(path, "_") + ".json"
	if err := os.WriteFile(filepath.Join(dir, "fx", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runs(workflow string, items ...string) (string, string) {
	return fmt.Sprintf("repos/o/r/actions/workflows/%s/runs?head_sha=%s&per_page=100", workflow, sha),
		`{"workflow_runs":[` + strings.Join(items, ",") + `]}`
}

func run(id int, status, conclusion, created string) string {
	c := "null"
	if conclusion != "" {
		c = `"` + conclusion + `"`
	}
	return fmt.Sprintf(`{"id":%d,"head_sha":"%s","status":"%s","conclusion":%s,"created_at":"%s"}`, id, sha, status, c, created)
}

func jobs(id int, items ...string) (string, string) {
	return fmt.Sprintf("repos/o/r/actions/runs/%d/jobs?per_page=100", id), `{"jobs":[` + strings.Join(items, ",") + `]}`
}

func job(name, conclusion string) string {
	return fmt.Sprintf(`{"name":%q,"conclusion":%q}`, name, conclusion)
}

func set(t *testing.T, dir string, path, body string) { t.Helper(); fixture(t, dir, path, body) }

func allGreen(t *testing.T, dir string) {
	t.Helper()
	p, b := runs("ci.yml", run(1, "completed", "success", "2026-10-07T01:00:00Z"))
	set(t, dir, p, b)
	p, b = runs("linux.yml", run(2, "completed", "success", "2026-10-07T01:00:00Z"))
	set(t, dir, p, b)
	p, b = jobs(2, job("systemd-smoke (ubuntu-24.04, amd64)", "success"), job("upgrade-e2e (ubuntu-24.04, amd64)", "success"), job("upgrade-e2e (ubuntu-24.04-arm, arm64)", "success"))
	set(t, dir, p, b)
	p, b = runs("conformance.yml", run(3, "completed", "success", "2026-10-07T01:00:00Z"))
	set(t, dir, p, b)
	p, b = jobs(3, job("suites (ubuntu-24.04, amd64)", "success"), job("suites (ubuntu-24.04-arm, arm64)", "success"), job("specdiff", "skipped"))
	set(t, dir, p, b)
}

func TestGatePassesWhenEveryRequiredRunSucceeded(t *testing.T) {
	dir, gate := gateRig(t)
	allGreen(t, dir)
	out, err := gate()
	if err != nil || !strings.Contains(out, "release gate passed") {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "2 job(s) of conformance.yml named suites*") || !strings.Contains(out, "2 job(s) of linux.yml named upgrade*") {
		t.Errorf("the gate should say which jobs it saw:\n%s", out)
	}
}

func TestGateRefuses(t *testing.T) {
	cases := map[string]struct {
		mutate func(t *testing.T, dir string)
		want   string
	}{
		"the conformance suite failed": {func(t *testing.T, dir string) {
			p, b := jobs(3, job("suites (ubuntu-24.04, amd64)", "success"), job("suites (ubuntu-24.04-arm, arm64)", "failure"))
			set(t, dir, p, b)
		}, "suites (ubuntu-24.04-arm, arm64) (failure)"},
		"the upgrade test failed": {func(t *testing.T, dir string) {
			p, b := jobs(2, job("upgrade-e2e (ubuntu-24.04, amd64)", "failure"))
			set(t, dir, p, b)
		}, "upgrade-e2e (ubuntu-24.04, amd64) (failure)"},
		"the upgrade test is missing": {func(t *testing.T, dir string) {
			p, b := jobs(2, job("systemd-smoke (ubuntu-24.04, amd64)", "success"))
			set(t, dir, p, b)
		}, "no job named upgrade*"},
		"the upgrade test was skipped": {func(t *testing.T, dir string) {
			p, b := jobs(2, job("upgrade-e2e (ubuntu-24.04, amd64)", "skipped"))
			set(t, dir, p, b)
		}, "(skipped)"},
		"ci failed": {func(t *testing.T, dir string) {
			p, b := runs("ci.yml", run(1, "completed", "failure", "2026-10-07T01:00:00Z"))
			set(t, dir, p, b)
		}, "ci.yml run 1"},
		"no run of linux.yml on the commit": {func(t *testing.T, dir string) {
			p, b := runs("linux.yml")
			set(t, dir, p, b)
		}, "linux.yml has no run on " + sha},
		"a run still going after the timeout": {func(t *testing.T, dir string) {
			p, b := runs("conformance.yml", run(3, "in_progress", "", "2026-10-07T01:00:00Z"))
			set(t, dir, p, b)
		}, "gave up waiting for conformance.yml"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, gate := gateRig(t)
			allGreen(t, dir)
			c.mutate(t, dir)
			out, err := gate()
			if err == nil {
				t.Fatalf("the gate passed:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("want %q in:\n%s", c.want, out)
			}
		})
	}
}

func TestGateUsesTheNewestRun(t *testing.T) {
	dir, gate := gateRig(t)
	allGreen(t, dir)
	// A failed run followed by a successful re-run: the newest counts.
	p, b := runs("ci.yml", run(1, "completed", "failure", "2026-10-07T01:00:00Z"), run(9, "completed", "success", "2026-10-07T02:00:00Z"))
	set(t, dir, p, b)
	if out, err := gate(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// A success followed by a failure is a failure.
	p, b = runs("ci.yml", run(1, "completed", "success", "2026-10-07T01:00:00Z"), run(9, "completed", "failure", "2026-10-07T02:00:00Z"))
	set(t, dir, p, b)
	if out, err := gate(); err == nil {
		t.Fatalf("a newer failure was ignored:\n%s", out)
	}
}

func TestGateRefusesAMalformedCommit(t *testing.T) {
	_, gate := gateRig(t)
	_ = gate
	c := exec.Command("bash", "../release-gate.sh", "main")
	c.Env = append(os.Environ(), "GITHUB_REPOSITORY=o/r")
	if out, err := c.CombinedOutput(); err == nil {
		t.Errorf("a branch name passed as the commit: %s", out)
	}
}
