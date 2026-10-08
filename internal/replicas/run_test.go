package replicas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The whole thing in one process: a leader with [replicas] default = "all", a second node whose
// agent is the fake, the real Run loop on a real clock. Every project gets a replica on the second
// node, each goes through the seven steps to ACTIVE_HEALTHY, lag is sampled and the snapshot file
// is written; removing a replica takes the row away, and the loop stops with its context.
func TestTwoNodeRunReachesActiveHealthy(t *testing.T) {
	snap := filepath.Join(t.TempDir(), SnapshotName)
	e := newEnv(t, func(o *Options) {
		o.Config.Replicas.Default = "all"
		o.Now = nil
		o.Interval = 20 * time.Millisecond
		o.MinGap = time.Millisecond
		o.SnapshotPath = snap
	})
	if err := e.reg.DeleteNode(e.ctx, "n3"); err != nil { // a cluster of two nodes
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.ctrl.Run(ctx) }()

	waitFor(t, "both replicas ACTIVE_HEALTHY", func() bool {
		rs, _ := e.reg.ListReplicas(e.ctx, "")
		healthy := 0
		for _, r := range rs {
			if r.NodeID == "n2" && r.Status == statusHealthy && r.InitStep == StepDone {
				healthy++
			}
		}
		return healthy == 2
	})
	for _, ref := range []string{refA, refB} {
		r := e.replica(ref, "n2")
		if r.Origin != registry.ReplicaDefault || r.InitError != "" {
			t.Fatalf("%s: %+v", ref, r)
		}
		st, _ := e.ctrl.Statuses(e.ctx, ref)
		if len(st) != 1 || st[0].Init.Status != "completed" || st[0].LagSeconds != 0 {
			t.Fatalf("%s statuses: %+v %+v", ref, st, st[0].Init)
		}
		if pts, _ := e.ctrl.Lag(e.ctx, r.Identifier, time.Now().Add(-time.Hour)); len(pts) == 0 {
			t.Fatalf("%s: no lag samples", ref)
		}
	}
	if len(e.pool.ensured) != 2 {
		t.Fatalf("supavisor tenants: %v", e.pool.ensured)
	}
	waitFor(t, "the snapshot file", func() bool {
		s := ReadSnapshot(snap, time.Now())
		_, ok := s.Entry(e.replica(refA, "n2").Identifier)
		return ok
	})
	fi, err := os.Stat(snap)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("snapshot file: %v %v", fi, err)
	}

	id := e.replica(refB, "n2").Identifier
	if err := e.ctrl.Remove(e.ctx, refB, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the removal", func() bool { return !e.hasReplica(refB, "n2") })
	if !e.hasReplica(refA, "n2") {
		t.Fatal("removed the wrong replica")
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// A request made while the loop sleeps wakes it: the row moves on without waiting for the interval.
func TestRunWakesOnANewReplica(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Now = nil
		o.Interval = time.Hour
		o.MinGap = time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.ctrl.Run(ctx)
	time.Sleep(50 * time.Millisecond) // the first pass: nothing to do
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the replica to be healthy", func() bool { return e.replica(refA, "n2").Status == statusHealthy })
}

// A node that is not in a cluster (one node, no rows) is left alone: no registry reads beyond the first.
func TestPassIsIdleOnASingleNode(t *testing.T) {
	reg := registry.NewMemory()
	e := newEnv(t)
	c := New(Options{Registry: reg, Config: e.cfg, Ops: e.nodes, Backups: e.bk})
	if !c.pass(context.Background()) {
		t.Fatal("a single node with no replicas must report itself idle")
	}
	if c.o.Admit != nil || len(e.nodes.calls) != 0 {
		t.Fatalf("calls: %v", e.nodes.calls)
	}
}

// Without the node operations the controller answers from the registry and never runs.
func TestControllerWithoutOpsOnlyServesTheRegistry(t *testing.T) {
	e := newEnv(t)
	c := New(Options{Registry: e.reg, Config: e.cfg, Log: e.opts.Log, NewID: e.opts.NewID})
	if err := c.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	c.Tick(e.ctx)
	if r := e.replica(refA, "n2"); r.InitStep != StepRequested {
		t.Fatalf("a controller without ops moved the row: %+v", r)
	}
	if st, err := c.Statuses(e.ctx, refA); err != nil || len(st) != 1 || st[0].LagSeconds != -1 {
		t.Fatalf("statuses: %+v %v", st, err)
	}
}

// The snapshot is ignored once it is old, and a missing or damaged file reads as none.
func TestReadSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SnapshotName)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if ReadSnapshot(path, now) != nil {
		t.Fatal("a missing file")
	}
	if err := os.WriteFile(path, []byte("{nonsense"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ReadSnapshot(path, now) != nil {
		t.Fatal("a damaged file")
	}
	lag := 4.0
	b := `{"at":"2026-10-08T11:59:00Z","replicas":[{"identifier":"x-rr-eu-west-1-abcdef","lag_seconds":4,"receiver":"streaming","postgrest_ready":true,"observed_at":"2026-10-08T11:59:00Z"}]}`
	if err := os.WriteFile(path, []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	s := ReadSnapshot(path, now)
	e, ok := s.Entry("x-rr-eu-west-1-abcdef")
	if !ok || e.LagSeconds == nil || *e.LagSeconds != lag || e.Receiver != "streaming" {
		t.Fatalf("snapshot: %+v %+v", s, e)
	}
	if _, ok := s.Entry("other"); ok {
		t.Fatal("an entry that is not there")
	}
	if ReadSnapshot(path, now.Add(3*time.Minute)) != nil {
		t.Fatal("a snapshot three minutes old")
	}
	var none *Snapshot
	if _, ok := none.Entry("x"); ok {
		t.Fatal("entry of a nil snapshot")
	}
}

// A pass starts at most as many workers as there are tokens; the rows it could not serve go first
// in the next pass.
func TestPassBoundsItsWorkers(t *testing.T) {
	e := newEnv(t)
	e.addProject(refC, "small")
	refs := []string{refA, refB, refC}
	ids := map[string]string{}
	for _, ref := range refs {
		if err := e.ctrl.SetupOn(e.ctx, ref, "n2"); err != nil {
			t.Fatal(err)
		}
		ids[ref] = e.settle(ref, "n2").Identifier
	}
	e.ctrl.workers = make(chan struct{}, 2)
	// observed runs a pass whose workers wait at the gate until the pass is over, and returns the
	// replicas they observed.
	observed := func() map[string]bool {
		e.clock.Advance(time.Minute)
		before := len(e.nodes.calls)
		e.nodes.gate = make(chan struct{})
		e.ctrl.pass(e.ctx)
		close(e.nodes.gate)
		e.ctrl.wg.Wait()
		got := map[string]bool{}
		for _, c := range e.nodes.calls[before:] {
			for ref, id := range ids {
				if strings.HasSuffix(c, id) && strings.HasPrefix(c, "observe") {
					got[ref] = true
				}
			}
		}
		return got
	}
	if got := observed(); len(got) != 2 || !got[refA] || !got[refB] || e.ctrl.resume != 2 {
		t.Fatalf("first pass observed %v, resume %d", got, e.ctrl.resume)
	}
	// The row left over goes first, then the ones from the start of the list.
	if got := observed(); len(got) != 2 || !got[refC] || !got[refA] || e.ctrl.resume != 1 {
		t.Fatalf("second pass observed %v, resume %d", got, e.ctrl.resume)
	}
	if got := observed(); len(got) != 2 || !got[refB] || !got[refC] || e.ctrl.resume != 0 {
		t.Fatalf("third pass observed %v, resume %d", got, e.ctrl.resume)
	}
	if len(e.ctrl.workers) != 0 {
		t.Fatalf("%d tokens still held", len(e.ctrl.workers))
	}
}

// Restart after Run has wound down starts nothing.
func TestRestartAfterTheControllerStopped(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- e.ctrl.Run(ctx) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if err := e.ctrl.Restart(e.ctx, refA, r.Identifier); !errors.Is(err, errStopping) {
		t.Fatalf("Restart = %v", err)
	}
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("status %s", got)
	}
}
