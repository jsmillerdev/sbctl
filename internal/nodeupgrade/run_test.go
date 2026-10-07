package nodeupgrade

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func runOpts(h *fakeHost) Options {
	return Options{Yes: true, Canary: 1, Batch: 5, Keep: 3, Out: h.out, Now: func() time.Time { return t0 },
		VerifyTimeout: 50 * time.Millisecond, VerifyEvery: time.Millisecond}
}

func code(t *testing.T, err error) int {
	t.Helper()
	var f *Failure
	if err != nil && !errors.As(err, &f) {
		t.Fatalf("not a Failure: %v", err)
	}
	return ExitCode(err)
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("missing %q in:\n%s", sub, s)
	}
}

func TestUpgradeHappyPath(t *testing.T) {
	h := newFakeHost()
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, h.out)
	}
	want := []string{
		"inspect", "resolve ", "stage",
		"prefetch",
		"backup system,aaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbb x3",
		"install v1.0.0->v1.1.0 migrations 3/applied",
		"wait gotrue,realtime,storage,studio",
		"projects gotrue=v2.195.0-r1 postgres=?",
		"status",
		"cleanup keep=3 current=v1.1.0",
		"discard",
	}
	if got := h.order(); got != strings.Join(want, " | ") {
		t.Fatalf("order:\n got %s\nwant %s", got, strings.Join(want, " | "))
	}
	if got := strings.Join(h.marks, " "); got != "preparing preparing switching services projects verifying done" {
		t.Fatalf("marker phases = %s", got)
	}
	mustContain(t, h.out.String(), "Supavise v1.1.0 is running")
}

// The schema a kept release can run on comes from its binary when the binary says, and from the
// registry as it was when the release ran otherwise.
func TestUpgradeRecordsTheSchemaOfTheRelease(t *testing.T) {
	h := newFakeHost()
	h.node.BinaryInfo = &Info{Version: "v1.0.0", Pins: oldPins(), RegistryMigrations: migsV1[:2]}
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatal(err)
	}
	if !h.has("install v1.0.0->v1.1.0 migrations 2/binary") {
		t.Fatalf("calls: %s", h.order())
	}
}

func TestCheckAndPlanChangeNothing(t *testing.T) {
	h := newFakeHost()
	o := runOpts(h)
	o.Check = true
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "installed v1.0.0, newest v1.1.0: update available")
	if h.has("stage") || h.has("install") || h.has("backup") {
		t.Fatalf("--check did more than look: %s", h.order())
	}

	h = newFakeHost()
	h.tag = "v1.0.0"
	h.node.BinaryInfo = &Info{Version: "v1.0.0", Pins: oldPins(), RegistryMigrations: migsV1}
	o = runOpts(h)
	o.Check = true
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "up to date")

	h = newFakeHost()
	o = runOpts(h)
	o.Plan = true
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "Projects (")
	if !h.has("stage") || h.has("prefetch") || h.has("backup") || h.has("install") || h.has("confirm") || len(h.marks) != 0 {
		t.Fatalf("--plan changed something: %s, marks %v", h.order(), h.marks)
	}
}

func TestNothingToDo(t *testing.T) {
	h := newFakeHost()
	h.tag = "v1.0.0"
	h.node.BinaryInfo = &Info{Version: "v1.0.0", Pins: oldPins(), RegistryMigrations: migsV1}
	h.info = h.node.BinaryInfo
	h.node.Projects = h.node.Projects[:1]
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatal(err)
	}
	mustContain(t, h.out.String(), "nothing to change")
	if h.has("stage") || h.has("backup") || h.has("install") {
		t.Fatalf("a current node was touched: %s", h.order())
	}
}

// Everything that refuses leaves the node as it was, with exit status 2.
func TestRefusalsChangeNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(h *fakeHost, o *Options)
		want  string
	}{
		"another upgrade runs":       {func(h *fakeHost, o *Options) { h.node.Running = &Running{PID: 7, Phase: "projects"} }, "another upgrade is running (process 7"},
		"release cannot be resolved": {func(h *fakeHost, o *Options) { h.resolveErr = errors.New("signature does not verify") }, "signature does not verify"},
		"unsupported jump": {func(h *fakeHost, o *Options) {
			h.resolveErr = errors.New("this release cannot be installed over the running version")
		}, "cannot be installed over"},
		"older release":         {func(h *fakeHost, o *Options) { h.tag = "v0.9.0" }, "older than the installed v1.0.0"},
		"binary does not stage": {func(h *fakeHost, o *Options) { h.stageErr = errors.New("does not match its checksum") }, "checksum"},
		"node down":             {func(h *fakeHost, o *Options) { h.node.Verdict, h.node.Summary = VerdictDown, "down" }, "the node is down"},
		"postgres major":        {func(h *fakeHost, o *Options) { h.info.Pins["postgres"] = "postgres-18.0.0-r0" }, "17 to 18"},
		"unattended and degraded": {func(h *fakeHost, o *Options) {
			o.Unattended = true
			h.node.Verdict, h.node.Summary = VerdictDegraded, "degraded"
		}, "unattended upgrade needs a healthy node"},
		"declined":      {func(h *fakeHost, o *Options) { o.Yes, h.confirm = false, false }, "nothing was changed"},
		"nobody to ask": {func(h *fakeHost, o *Options) { o.Yes, h.confirmErr = false, errors.New("run it again with --yes") }, "--yes"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost()
			o := runOpts(h)
			tc.setup(h, &o)
			err := Run(context.Background(), h, o)
			if code(t, err) != ExitRefused || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want exit 2 with %q", err, tc.want)
			}
			for _, step := range []string{"prefetch", "backup", "install", "wait", "projects", "restore", "revert"} {
				if h.has(step) {
					t.Fatalf("%s ran after a refusal: %s", step, h.order())
				}
			}
		})
	}
}

func TestUnattendedAndYesDoNotAsk(t *testing.T) {
	h := newFakeHost()
	o := runOpts(h)
	o.Yes, o.Unattended = false, true
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatal(err)
	}
	if h.has("confirm") {
		t.Fatal("--unattended asked")
	}
	h = newFakeHost()
	o = runOpts(h)
	o.Yes = false
	if err := Run(context.Background(), h, o); err != nil || !h.has("confirm") {
		t.Fatalf("an attended upgrade without --yes must ask: %v %s", err, h.order())
	}
}

// Fetching and backing up come before anything is stopped; their failure refuses.
func TestPrepareFailsClosed(t *testing.T) {
	for name, set := range map[string]func(*fakeHost){
		"artifacts": func(h *fakeHost) { h.prefetchErr = errBoom },
		"backups":   func(h *fakeHost) { h.backupErr = errBoom },
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost()
			set(h)
			err := Run(context.Background(), h, runOpts(h))
			if code(t, err) != ExitRefused || !strings.Contains(err.Error(), "nothing was") {
				t.Fatalf("err = %v", err)
			}
			if h.has("install") || h.has("wait") || h.has("projects") || h.has("restore") {
				t.Fatalf("the node was touched: %s", h.order())
			}
			if got := h.marks[len(h.marks)-1]; got != PhaseRefused {
				t.Fatalf("last phase = %s", got)
			}
		})
	}
}

func TestInstallFailureBeforeTheSwapChangesNothing(t *testing.T) {
	h := newFakeHost()
	h.installErr = errors.New("cannot keep the running binary")
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitRefused || h.has("restore") {
		t.Fatalf("err = %v, calls %s", err, h.order())
	}
}

// A daemon that does not answer after the swap puts the previous binary back: exit status 3.
func TestDaemonThatDoesNotComeUpIsRolledBack(t *testing.T) {
	h := newFakeHost()
	h.installErr, h.installSwaps = errors.New("did not answer"), true
	h.sharedErr = nil
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitRolledBack {
		t.Fatalf("err = %v\n%s", err, h.order())
	}
	if !strings.Contains(h.order(), "restore v1.0.0 (from v1.1.0)") || h.has("projects") {
		t.Fatalf("calls: %s", h.order())
	}
	if got := h.marks[len(h.marks)-1]; got != PhaseRolledBack {
		t.Fatalf("last phase = %s", got)
	}
	mustContain(t, err.Error(), "back on v1.0.0")
}

func TestSharedServiceThatDoesNotComeUpIsRolledBack(t *testing.T) {
	h := newFakeHost()
	h.sharedErr, h.sharedErrOnce = errors.New("realtime is not healthy"), true
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitRolledBack {
		t.Fatalf("err = %v\n%s", err, h.order())
	}
	// Forward wait fails, then restore, then the way back waits for the old releases.
	order := h.order()
	if !(strings.Index(order, "wait gotrue,realtime") < strings.Index(order, "restore v1.0.0") && strings.LastIndex(order, "wait") > strings.Index(order, "restore")) {
		t.Fatalf("order: %s", order)
	}
	if h.has("projects") || h.has("revert") {
		t.Fatalf("projects were touched: %s", order)
	}
}

// The rollout halts at the first failed project; the projects it moved go back first, then the
// binary, and the exit status is 3.
func TestProjectFailureHaltsAndRollsBack(t *testing.T) {
	h := newFakeHost()
	h.projectsErr = errors.New("project bbbbbbbbbbbbbbbbbbbb failed")
	h.moves = []ProjectMove{{Ref: "aaaaaaaaaaaaaaaaaaaa", From: map[string]string{"gotrue": authOld}, To: map[string]string{"gotrue": authNew}}}
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitRolledBack {
		t.Fatalf("err = %v\n%s", err, h.order())
	}
	order := h.order()
	if !(strings.Index(order, "revert a") > strings.Index(order, "projects ") && strings.Index(order, "restore v1.0.0") > strings.Index(order, "revert a")) {
		t.Fatalf("projects go back before the binary: %s", order)
	}
	if h.has("cleanup") {
		t.Fatal("a failed upgrade collected garbage")
	}
	mustContain(t, h.out.String(), "putting 1 project(s) back")
}

// Migrations only go forward: a registry that the new release migrated cannot be served by the
// old binary. The node stays as it is and the operator is told what to restore.
func TestRollbackRefusedByTheRegistrySchema(t *testing.T) {
	h := newFakeHost()
	h.projectsErr = errBoom
	h.applied = migsV2 // the new release migrated the registry
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitNeedsOperator {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, err.Error(), "Restore the system project's pre-upgrade backup")
	mustContain(t, err.Error(), "the upgrade failed because")
	if h.has("restore") || h.has("revert") {
		t.Fatalf("the node was rolled back past the registry: %s", h.order())
	}
	if got := h.marks[len(h.marks)-1]; got != PhaseFailed {
		t.Fatalf("last phase = %s", got)
	}
}

func TestRollbackThatFailsNeedsTheOperator(t *testing.T) {
	for name, set := range map[string]func(*fakeHost){
		"restore fails":          func(h *fakeHost) { h.restoreErr = errors.New("units would not render") },
		"projects do not return": func(h *fakeHost) { h.revertErr = errors.New("project a failed") },
		"registry unreadable":    func(h *fakeHost) { h.appliedErr = errors.New("connection refused") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost()
			h.projectsErr = errBoom
			h.moves = []ProjectMove{{Ref: "aaaaaaaaaaaaaaaaaaaa", From: map[string]string{"gotrue": authOld}, To: map[string]string{"gotrue": authNew}}}
			set(h)
			err := Run(context.Background(), h, runOpts(h))
			if code(t, err) != ExitNeedsOperator {
				t.Fatalf("err = %v\n%s", err, h.order())
			}
		})
	}
}

// A node that is worse after the upgrade than before it is rolled back; one that was degraded
// before and still is stays upgraded, and a verdict that needs a moment to settle gets it.
func TestVerdictAfterTheUpgrade(t *testing.T) {
	h := newFakeHost()
	h.verdicts = []string{VerdictDegraded, VerdictDegraded, VerdictHealthy}
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("a node that settles: %v", err)
	}

	h = newFakeHost()
	h.verdicts = []string{VerdictDegraded}
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitRolledBack {
		t.Fatalf("a healthy node that ends degraded: %v", err)
	}
	mustContain(t, err.Error(), "not healthy after the upgrade")

	h = newFakeHost()
	h.node.Verdict = VerdictDegraded
	h.verdicts = []string{VerdictDegraded}
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("degraded before and after: %v", err)
	}
}

// The same binary with the services behind it converges the node without swapping anything.
func TestSameBinaryConvergesTheServices(t *testing.T) {
	h := newFakeHost()
	h.tag = "v1.1.0"
	h.node.Version = "v1.1.0"
	h.node.BinaryInfo = newInfo()
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if h.has("install") || h.has("stage") {
		t.Fatalf("a binary that is already installed was swapped or downloaded: %s", h.order())
	}
	if !h.has("prefetch") || !h.has("backup") || !h.has("wait") || !h.has("projects") {
		t.Fatalf("calls: %s", h.order())
	}
}

func TestIncludePostgresReachesTheRollout(t *testing.T) {
	h := newFakeHost()
	h.info.Pins["postgres"] = pgNew
	o := runOpts(h)
	o.IncludePostgres = true
	if err := Run(context.Background(), h, o); err != nil {
		t.Fatal(err)
	}
	if !h.has("projects gotrue=v2.195.0-r1 postgres=17.11.0.004-r1") {
		t.Fatalf("calls: %s", h.order())
	}
}

func TestExitCodes(t *testing.T) {
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 || ExitCode(refused("x")) != 2 || ExitCode(&Failure{Code: 3, Err: errBoom}) != 3 || ExitCode(&Failure{Code: 4, Err: errBoom}) != 4 {
		t.Fatal("exit codes")
	}
	if !errors.Is(&Failure{Code: 3, Err: errBoom}, errBoom) {
		t.Fatal("a Failure does not unwrap")
	}
}
