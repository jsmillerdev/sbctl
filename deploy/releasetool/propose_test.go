package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// proposeRig builds a bare "origin", a clone with internal/versions/versions.yaml on main, a stub
// `gh` that logs what it is asked and answers `pr list` from files, and the releasetool binary.
type proposeRig struct {
	t           *testing.T
	dir, clone  string
	origin      string
	bin, ghlog  string
	bumps       string
	openPRs     string // JSON answered for `pr list --state open`
	closedPRs   string
	script, git string
}

func newProposeRig(t *testing.T) *proposeRig {
	t.Helper()
	for _, tool := range []string{"bash", "git", "jq", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	r := &proposeRig{t: t, dir: t.TempDir(), openPRs: "[]", closedPRs: "[]"}
	r.origin = filepath.Join(r.dir, "origin.git")
	r.clone = filepath.Join(r.dir, "clone")
	r.ghlog = filepath.Join(r.dir, "gh.log")
	r.bumps = filepath.Join(r.dir, "bumps.json")
	r.script, _ = filepath.Abs(filepath.Join("..", "..", "tests", "conformance", "propose-bumps.sh"))
	r.git = "git"
	sh := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	sh(r.dir, "git", "init", "-q", "--bare", "-b", "main", r.origin)
	sh(r.dir, "git", "clone", "-q", r.origin, r.clone)
	if err := os.MkdirAll(filepath.Join(r.clone, "internal", "versions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.clone, "internal", "versions", "versions.yaml"), []byte(oldYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(r.clone, "git", "add", "-A")
	sh(r.clone, "git", "commit", "-q", "-m", "pins")
	sh(r.clone, "git", "push", "-q", "origin", "HEAD:main")

	r.bin = filepath.Join(r.dir, "releasetool")
	sh(".", "go", "build", "-o", r.bin, ".")

	stub := `#!/usr/bin/env bash
echo "gh $*" >>"$GH_LOG"
if [[ $1 == pr && $2 == list ]]; then
  f=$GH_CLOSED; [[ " $* " == *" --state open "* ]] && f=$GH_OPEN
  expr=""; prev=""
  for a in "$@"; do [[ $prev == --jq ]] && expr=$a; prev=$a; done
  if [[ -n $expr ]]; then jq -c "$expr" "$f"; else cat "$f"; fi
  exit 0
fi
exit 0
`
	if err := os.MkdirAll(filepath.Join(r.dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "bin", "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *proposeRig) write(name, body string) string {
	p := filepath.Join(r.dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *proposeRig) run(env ...string) (string, error) {
	open := r.write("open.json", r.openPRs)
	closed := r.write("closed.json", r.closedPRs)
	_ = os.WriteFile(r.ghlog, nil, 0o644)
	c := exec.Command("bash", r.script, r.bumps)
	c.Dir = r.clone
	c.Env = append(os.Environ(), "PATH="+filepath.Join(r.dir, "bin")+":"+os.Getenv("PATH"), "GITHUB_REPOSITORY=o/r",
		"GH_LOG="+r.ghlog, "GH_OPEN="+open, "GH_CLOSED="+closed, "RELEASETOOL="+r.bin,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	c.Env = append(c.Env, env...)
	out, err := c.CombinedOutput()
	return string(out), err
}

func (r *proposeRig) ghCalls() string { b, _ := os.ReadFile(r.ghlog); return string(b) }

func (r *proposeRig) remoteFile(branch, path string) (string, bool) {
	c := exec.Command("git", "--git-dir", r.origin, "show", branch+":"+path)
	out, err := c.Output()
	return string(out), err == nil
}

const bumpsJSON = `[
 {"name":"auth","pinned":"auth-v2.194.0-r1","status":"ok","install":true,"newest":"auth-v2.195.0-r1","newer":true},
 {"name":"postgres","pinned":"postgres-17.11.0.004-r1","status":"ok","install":true,"newest":"postgres-17.11.0.004-r1","newer":false},
 {"name":"storage","pinned":"storage-v1.79.36-r0","status":"ok","install":true,"newest":"storage-v1.80.0-rc.1-r0","newer":true},
 {"name":"cli (versions.yaml cli.version_tested)","pinned":"2.120.0","status":"ok","install":false,"newest":"2.121.0","newer":true}
]`

func TestProposeOpensOnePullRequestPerServiceAndStartsTheGates(t *testing.T) {
	r := newProposeRig(t)
	r.write("bumps.json", bumpsJSON)
	out, err := r.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "1 proposal(s)") || !strings.Contains(out, "storage: storage-v1.80.0-rc.1-r0 is a pre-release") {
		t.Errorf("output:\n%s", out)
	}
	// The branch of auth carries the one-pin change.
	got, ok := r.remoteFile("bump/auth", "internal/versions/versions.yaml")
	if !ok {
		t.Fatal("bump/auth was not pushed")
	}
	if want := strings.Replace(oldYAML, "auth-v2.194.0-r1", "auth-v2.195.0-r1", 1); got != want {
		t.Errorf("pushed file:\n%s", got)
	}
	for _, branch := range []string{"bump/storage", "bump/postgres"} {
		if _, ok := r.remoteFile(branch, "internal/versions/versions.yaml"); ok {
			t.Errorf("%s was pushed", branch)
		}
	}
	calls := r.ghCalls()
	if strings.Count(calls, "gh pr create") != 1 || !strings.Contains(calls, "--title Bump auth to auth-v2.195.0-r1") || !strings.Contains(calls, "--head bump/auth") {
		t.Errorf("gh calls:\n%s", calls)
	}
	for _, wf := range []string{"ci.yml", "conformance.yml", "linux.yml"} {
		if !strings.Contains(calls, "gh workflow run "+wf+" --repo o/r --ref bump/auth") {
			t.Errorf("the %s gate was not started on the branch:\n%s", wf, calls)
		}
	}
	if strings.Contains(calls, "pr merge") {
		t.Errorf("the proposal must never merge:\n%s", calls)
	}
}

func TestProposeIsIdempotentAndUpdatesAnOpenPullRequest(t *testing.T) {
	r := newProposeRig(t)
	r.write("bumps.json", bumpsJSON)
	if out, err := r.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	// The pull request for this very version is open: nothing more happens.
	r.openPRs = `[{"number":7,"title":"Bump auth to auth-v2.195.0-r1"}]`
	out, err := r.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if calls := r.ghCalls(); strings.Contains(calls, "pr create") || strings.Contains(calls, "pr edit") || strings.Contains(calls, "workflow run") {
		t.Errorf("a second night changed things:\n%s", calls)
	}
	if !strings.Contains(out, "#7 already proposes auth-v2.195.0-r1") {
		t.Errorf("output:\n%s", out)
	}

	// A newer release appeared: the same pull request is edited, not a second one opened.
	r.write("bumps.json", strings.Replace(bumpsJSON, "auth-v2.195.0-r1", "auth-v2.196.0-r0", 1))
	out, err = r.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	calls := r.ghCalls()
	if strings.Contains(calls, "pr create") || !strings.Contains(calls, "gh pr edit 7 --repo o/r --title Bump auth to auth-v2.196.0-r0") {
		t.Errorf("gh calls:\n%s", calls)
	}
	got, _ := r.remoteFile("bump/auth", "internal/versions/versions.yaml")
	if !strings.Contains(got, "auth-v2.196.0-r0") || strings.Contains(got, "auth-v2.195.0-r1") {
		t.Errorf("the branch was not moved to the newer release:\n%s", got)
	}
}

func TestProposeLeavesADeclinedVersionAlone(t *testing.T) {
	r := newProposeRig(t)
	r.write("bumps.json", bumpsJSON)
	r.closedPRs = `[{"title":"Bump auth to auth-v2.195.0-r1","mergedAt":null}]`
	out, err := r.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := r.remoteFile("bump/auth", "internal/versions/versions.yaml"); ok || strings.Contains(r.ghCalls(), "pr create") {
		t.Errorf("a closed proposal was proposed again:\n%s", out)
	}
	// A merged one for the same title does not count as declined (and the pin would no longer be older).
	r.closedPRs = `[{"title":"Bump auth to auth-v2.195.0-r1","mergedAt":"2026-10-01T00:00:00Z"}]`
	if _, err := r.run(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.remoteFile("bump/auth", "internal/versions/versions.yaml"); !ok {
		t.Error("a merged earlier pull request must not block a new proposal")
	}
}

func TestProposeDryRunChangesNothing(t *testing.T) {
	r := newProposeRig(t)
	r.write("bumps.json", bumpsJSON)
	out, err := r.run("DRY_RUN=1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := r.remoteFile("bump/auth", "internal/versions/versions.yaml"); ok {
		t.Error("a dry run pushed a branch")
	}
	calls := r.ghCalls()
	if strings.Contains(calls, "pr create") || strings.Contains(calls, "workflow run") {
		t.Errorf("a dry run wrote through gh:\n%s", calls)
	}
	if !strings.Contains(out, "DRY RUN: git push") || !strings.Contains(out, "DRY RUN: gh pr create") {
		t.Errorf("a dry run says what it would do:\n%s", out)
	}
}

func TestProposeRefusesUnexpectedNames(t *testing.T) {
	r := newProposeRig(t)
	r.write("bumps.json", `[{"name":"auth; touch pwned","pinned":"x","status":"ok","install":true,"newest":"auth-v2.195.0-r1","newer":true},
 {"name":"auth","pinned":"x","status":"ok","install":true,"newest":"auth-v2.195.0-r1 --force","newer":true}]`)
	out, err := r.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Count(out, "unexpected characters") != 2 || strings.Contains(r.ghCalls(), "pr create") {
		t.Errorf("output:\n%s\n%s", out, r.ghCalls())
	}
}
