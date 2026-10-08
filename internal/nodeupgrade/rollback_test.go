package nodeupgrade

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckRollback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		known    []string
		applied  []string
		wantErr  bool
		contains string
	}{
		{"registry holds what the release knows", migsV1, migsV1, false, ""},
		{"registry behind the release", migsV2, migsV1, false, ""},
		{"registry never migrated", migsV1, nil, false, ""},
		{"registry migrated past the release", migsV1, migsV2, true, "restore the system cluster from its pre-upgrade base backup"},
		// The newest name does not tell: a lane's migration can number below the newest one.
		{"a migration numbered below the newest", []string{"0001_init.sql", "1190_custom_domains.sql"}, migsV1, true, "1100_project_upgrades.sql"},
		{"a release that does not say", nil, migsV1, true, "does not say which migrations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRollback(&Record{Version: "v1.0.0", Migrations: tc.known}, tc.applied)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr && (!errors.Is(err, ErrRegistryNewer) || !strings.Contains(err.Error(), tc.contains)) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestProjectMoveRevertTargets(t *testing.T) {
	m := ProjectMove{Ref: "a", From: map[string]string{"gotrue": authOld, "postgrest": restOld, "postgres": pgOld}, To: map[string]string{"gotrue": authNew, "postgrest": restOld, "postgres": pgOld}}
	got := m.RevertTargets()
	if len(got) != 1 || got["gotrue"] != authOld {
		t.Fatalf("targets = %v: only the service that moved goes back", got)
	}
}

func keptRelease(version string, migs []string) *Record {
	p := oldPins()
	return &Record{Version: version, Pins: p, Migrations: migs, InstalledAt: t0.Add(-24 * time.Hour)}
}

// rollbackHost is a node on v1.1.0 (the new pins) that kept v1.0.0, with two projects the upgrade moved.
func rollbackHost() *fakeHost {
	h := newFakeHost()
	h.node.Version, h.node.Pins = "v1.1.0", newPins()
	h.node.Projects[1].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
	h.node.Projects[2].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
	h.prev = keptRelease("v1.0.0", migsV1)
	h.cur = &Record{Version: "v1.1.0", Pins: newPins(), Migrations: migsV1, InstalledAt: t0.Add(-time.Hour)}
	move := func(ref string) ProjectMove {
		return ProjectMove{Ref: ref, From: map[string]string{"gotrue": authOld, "postgrest": restOld, "postgres": pgOld}, To: map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}}
	}
	h.since = []ProjectMove{move("aaaaaaaaaaaaaaaaaaaa"), move("bbbbbbbbbbbbbbbbbbbb")}
	return h
}

func TestRollbackGoesBackToThePreviousRelease(t *testing.T) {
	h := rollbackHost()
	if err := Rollback(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	want := "inspect | revert a,b | restore v1.0.0 (from v1.1.0) | wait gotrue,realtime,storage,studio | status"
	if got := h.order(); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
	mustContain(t, h.out.String(), "Supavise v1.1.0 -> v1.0.0")
	mustContain(t, h.out.String(), "2 project(s) go back")
	if got := strings.Join(h.marks, " "); got != "rolling_back rolling_back rolled_back" {
		t.Fatalf("phases = %s", got)
	}
}

func TestRollbackRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(h *fakeHost, o *Options)
		want  string
	}{
		"no previous release":       {func(h *fakeHost, o *Options) { h.prev = nil }, "no previous release is kept"},
		"registry migrated past it": {func(h *fakeHost, o *Options) { h.applied = migsV2 }, "restore the system cluster from its pre-upgrade base backup"},
		"registry unreadable":       {func(h *fakeHost, o *Options) { h.appliedErr = errBoom }, "cannot read the registry's migrations"},
		"an upgrade is running":     {func(h *fakeHost, o *Options) { h.node.Running = &Running{PID: 9, Phase: "services"} }, "an upgrade is running"},
		"declined":                  {func(h *fakeHost, o *Options) { o.Yes, h.confirm = false, false }, "nothing was changed"},
	} {
		t.Run(name, func(t *testing.T) {
			h := rollbackHost()
			o := runOpts(h)
			tc.setup(h, &o)
			err := Rollback(context.Background(), h, o)
			if code(t, err) != ExitRefused || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want exit 2 with %q", err, tc.want)
			}
			if h.has("restore") || h.has("revert") {
				t.Fatalf("a refused rollback changed something: %s", h.order())
			}
		})
	}
}

// Restoring the system project's pre-upgrade backup brings the registry back to the old schema, and
// the same command then goes through.
func TestRollbackAfterTheBackupWasRestored(t *testing.T) {
	h := rollbackHost()
	h.applied = migsV2
	if err := Rollback(context.Background(), h, runOpts(h)); code(t, err) != ExitRefused {
		t.Fatalf("before the restore: %v", err)
	}
	h = rollbackHost()
	h.applied = migsV1
	if err := Rollback(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("after the restore: %v", err)
	}
}

func TestRollbackFailureNeedsTheOperator(t *testing.T) {
	h := rollbackHost()
	h.restoreErr = errors.New("the units did not render")
	err := Rollback(context.Background(), h, runOpts(h))
	if code(t, err) != ExitNeedsOperator {
		t.Fatalf("err = %v", err)
	}
	h = rollbackHost()
	h.revertErr = errors.New("project a failed")
	if err := Rollback(context.Background(), h, runOpts(h)); code(t, err) != ExitNeedsOperator {
		t.Fatalf("a project that did not go back: %v", err)
	}
}

// Only the projects that still run what the upgrade moved them to go back: one that was upgraded
// again since, or paused, or never moved, stays.
func TestRevertableFiltersTheMoves(t *testing.T) {
	n := rollbackHost().node
	n.Projects[2].Versions["gotrue"] = "auth-v2.300.0-r0" // upgraded again since
	n.Projects[1].Status = "ACTIVE_HEALTHY"
	all := []ProjectMove{
		{Ref: "aaaaaaaaaaaaaaaaaaaa", From: map[string]string{"gotrue": authOld, "postgrest": restOld}, To: map[string]string{"gotrue": authNew, "postgrest": restNew}},
		{Ref: "bbbbbbbbbbbbbbbbbbbb", From: map[string]string{"gotrue": authOld, "postgrest": restOld}, To: map[string]string{"gotrue": authNew, "postgrest": restNew}},
		{Ref: "cccccccccccccccccccc", From: map[string]string{"gotrue": authOld}, To: map[string]string{"gotrue": authNew}},
		{Ref: "zzzzzzzzzzzzzzzzzzzz", From: map[string]string{"gotrue": authOld}, To: map[string]string{"gotrue": authNew}},
	}
	got := revertable(all, n, time.Time{})
	if len(got) != 2 || got[0].Ref != "aaaaaaaaaaaaaaaaaaaa" || got[1].Ref != "bbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("revertable = %+v", got)
	}
	if got[0].From["gotrue"] != authOld || got[0].From["postgrest"] != restOld {
		t.Fatalf("a: %+v", got[0])
	}
	if _, ok := got[1].From["gotrue"]; ok || got[1].From["postgrest"] != restOld {
		t.Fatalf("b was upgraded again, so only PostgREST goes back: %+v", got[1])
	}
}

// A second rollback keeps stepping backwards: the release the first one left is not a target.
func TestRollbackStepsBackwards(t *testing.T) {
	r := Releases{Dir: t.TempDir()}
	keepRelease(t, r, "v1.0.0", t0)
	keepRelease(t, r, "v1.1.0", t0.Add(time.Hour))
	keepRelease(t, r, "v1.2.0", t0.Add(2*time.Hour))
	prev, _ := r.Previous("v1.2.0")
	if prev == nil || prev.Version != "v1.1.0" {
		t.Fatalf("previous of v1.2.0 = %+v", prev)
	}
	// The upgrade to v1.1.0 began at t0+30m and ended at t0+1h.
	rec, _ := r.Get("v1.1.0")
	rec.UpgradeStartedAt, rec.UpgradeEndedAt = t0.Add(30*time.Minute), t0.Add(time.Hour)
	if _, err := r.Keep(*rec, filepath.Join(r.Dir, "v1.1.0", BinaryName)); err != nil {
		t.Fatal(err)
	}
	// Rolled back from v1.2.0 to v1.1.0, which becomes the newest again.
	if err := r.Touch("v1.1.0", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get("v1.1.0"); !got.InstalledAt.Equal(t0.Add(3*time.Hour)) || !got.UpgradeStartedAt.Equal(t0.Add(30*time.Minute)) || !got.UpgradeEndedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("a rollback moved the window of the upgrade that installed v1.1.0: %+v", got)
	}
	if err := r.Withdraw("v1.2.0"); err != nil {
		t.Fatal(err)
	}
	if prev, _ = r.Previous("v1.1.0"); prev == nil || prev.Version != "v1.0.0" {
		t.Fatalf("after the rollback the previous of v1.1.0 = %+v, want v1.0.0 (not the release rolled back from)", prev)
	}
	// Installing it again makes it a target once more.
	keepRelease(t, r, "v1.2.0", t0.Add(4*time.Hour))
	if prev, _ = r.Previous("v1.1.0"); prev == nil || prev.Version != "v1.2.0" {
		t.Fatalf("a reinstalled release is still withdrawn: %+v", prev)
	}
}

// A rollback asks for the moves of the upgrade that installed the release it leaves, by that
// upgrade's own window; a release the node reached by an earlier rollback was installed again
// later, and its moves lie before that.
func TestRollbackAsksForTheWindowOfTheUpgrade(t *testing.T) {
	h := rollbackHost()
	start, end := t0.Add(-3*time.Hour), t0.Add(-2*time.Hour)
	h.cur = &Record{Version: "v1.1.0", Pins: newPins(), Migrations: migsV1, InstalledAt: t0.Add(-time.Minute), UpgradeStartedAt: start, UpgradeEndedAt: end}
	if err := Rollback(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !h.window[0].Equal(start) || !h.window[1].Equal(end) {
		t.Fatalf("the moves were read for %v to %v, want %v to %v", h.window[0], h.window[1], start, end)
	}
	// A release that no upgrade installed has no window: its moves start when it became current.
	h = rollbackHost()
	h.cur.UpgradeStartedAt = time.Time{}
	if err := Rollback(context.Background(), h, runOpts(h)); err != nil {
		t.Fatal(err)
	}
	if !h.window[0].Equal(h.cur.InstalledAt) {
		t.Fatalf("window start = %v, want %v", h.window[0], h.cur.InstalledAt)
	}
}

// A project an Owner upgraded after the node's upgrade ended is not put back by a rollback; with no
// end recorded the limit is off.
func TestRevertableStopsAtTheEndOfTheUpgrade(t *testing.T) {
	n := rollbackHost().node
	end := t0.Add(-30 * time.Minute)
	all := []ProjectMove{
		{Ref: "aaaaaaaaaaaaaaaaaaaa", At: end.Add(-5 * time.Minute), From: map[string]string{"gotrue": authOld}, To: map[string]string{"gotrue": authNew}},
		{Ref: "bbbbbbbbbbbbbbbbbbbb", At: end.Add(24 * time.Hour), From: map[string]string{"gotrue": authOld}, To: map[string]string{"gotrue": authNew}},
	}
	if got := revertable(all, n, end); len(got) != 1 || got[0].Ref != "aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("with an end: %+v", got)
	}
	if got := revertable(all, n, time.Time{}); len(got) != 2 {
		t.Fatalf("without an end: %+v", got)
	}
}

func TestEndOfTheUpgradeIsKeptInTheRecord(t *testing.T) {
	r := Releases{Dir: t.TempDir()}
	keepRelease(t, r, "v1.1.0", t0)
	if rec, _ := r.Get("v1.1.0"); !rec.UpgradeEndedAt.IsZero() {
		t.Fatalf("a new record has an end: %+v", rec)
	}
	if err := r.EndUpgrade("v1.1.0", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec, _ := r.Get("v1.1.0"); !rec.UpgradeEndedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("record = %+v", rec)
	}
}

// A kept record that has the start of its upgrade and no end cannot tell the upgrade's moves from
// an Owner's later ones: a rollback puts none of them back, and does not even read them.
func TestRollbackWithoutTheEndOfTheUpgradeLeavesTheProjects(t *testing.T) {
	h := rollbackHost()
	h.cur.UpgradeStartedAt = t0.Add(-3 * time.Hour)
	if err := Rollback(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if h.has("revert") {
		t.Fatalf("projects were reverted: %s", h.order())
	}
	if !h.window[0].IsZero() {
		t.Fatalf("the moves were read for %v", h.window)
	}
	mustContain(t, h.out.String(), "does not say when its upgrade ended")
	mustContain(t, h.out.String(), "rolled back: Supavise v1.0.0 is running")
}
