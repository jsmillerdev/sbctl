package lifecycle

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// notices records what Options.UpgradeNotify is told.
type notices struct {
	mu  sync.Mutex
	got []UpgradeNotice
}

func (n *notices) add(_ context.Context, u UpgradeNotice) {
	n.mu.Lock()
	n.got = append(n.got, u)
	n.mu.Unlock()
}

func (n *notices) events() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var ev []string
	for _, u := range n.got {
		ev = append(ev, u.Event)
	}
	return strings.Join(ev, ",")
}

func (n *notices) last() UpgradeNotice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.got[len(n.got)-1]
}

func TestUpgradeNotifiesStartedAndSucceeded(t *testing.T) {
	h := newUpHarness(t)
	var ns notices
	h.e.opts.UpgradeNotify = ns.add
	up, err := h.e.UpgradeProject(context.Background(), h.ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ns.events(); got != EventUpgradeStarted+","+EventUpgradeSucceeded {
		t.Fatalf("events = %s", got)
	}
	ns.mu.Lock()
	started, done := ns.got[0], ns.got[1]
	ns.mu.Unlock()
	if started.Ref != h.ref || started.TrackingID != up.TrackingID || !strings.Contains(started.Changes, "gotrue") || !strings.Contains(started.Changes, "postgrest") {
		t.Fatalf("started = %+v", started)
	}
	if done.TrackingID != up.TrackingID || done.BackupID != 42 || !strings.Contains(done.Changes, "gotrue") || done.Settled || done.Seconds < 0 {
		t.Fatalf("succeeded = %+v", done)
	}
}

func TestUpgradeNotifiesAFailureWithItsOutcome(t *testing.T) {
	t.Run("rolled back", func(t *testing.T) {
		h := newUpHarness(t)
		var ns notices
		h.e.opts.UpgradeNotify = ns.add
		h.plane.bad = newAuth
		if _, err := h.e.UpgradeProject(context.Background(), h.ref, nil); err == nil {
			t.Fatal("the upgrade should have failed")
		}
		if got := ns.events(); got != EventUpgradeStarted+","+EventUpgradeFailed {
			t.Fatalf("events = %s", got)
		}
		f := ns.last()
		if f.ErrorCode != UpgradeErrStart || f.Outcome != "rolled back to the previous versions" || f.BackupID != 42 || f.Unresolved() || !strings.Contains(f.Cause, "gotrue did not become ready") {
			t.Fatalf("failed = %+v", f)
		}
	})
	t.Run("the rollback failed too", func(t *testing.T) {
		h := newUpHarness(t)
		var ns notices
		h.e.opts.UpgradeNotify = ns.add
		h.plane.bad, h.plane.failRollback = newAuth, true
		if _, err := h.e.UpgradeProject(context.Background(), h.ref, nil); err == nil {
			t.Fatal("the upgrade should have failed")
		}
		if f := ns.last(); !f.Unresolved() || !strings.HasPrefix(f.Outcome, "the rollback failed too") {
			t.Fatalf("failed = %+v", f)
		}
	})
	t.Run("nothing was changed", func(t *testing.T) {
		h := newUpHarness(t)
		var ns notices
		h.e.opts.UpgradeNotify = ns.add
		h.arts.fetchErr = context.DeadlineExceeded
		if _, err := h.e.UpgradeProject(context.Background(), h.ref, nil); err == nil {
			t.Fatal("the upgrade should have failed")
		}
		if f := ns.last(); f.Event != EventUpgradeFailed || f.Outcome != "nothing was changed" || f.ErrorCode != UpgradeErrArtifacts || f.Unresolved() {
			t.Fatalf("failed = %+v", f)
		}
	})
}

// The daemon closes the record of an upgrade whose process died, and says so once.
func TestRecoverNotifiesAnInterruptedUpgrade(t *testing.T) {
	h := newUpHarness(t)
	ctx := context.Background()
	run, err := h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	daemon := h.daemonOf()
	var ns notices
	daemon.opts.UpgradeNotify = ns.add
	h.setProgress(t, ProgressStopping)
	run.(*upgradeRun).release()
	h.e.upgrading.Delete(h.ref)
	if rec := daemon.Recover(ctx); len(rec) != 1 {
		t.Fatalf("recovered = %+v", rec)
	}
	if got := ns.events(); got != EventUpgradeFailed {
		t.Fatalf("events = %s", got)
	}
	if f := ns.last(); f.Outcome != "interrupted" || !f.Settled || f.Ref != h.ref || f.TrackingID != run.Upgrade().TrackingID {
		t.Fatalf("failed = %+v", f)
	}
	// A second pass finds no running record and says nothing more.
	daemon.SettleUpgrades(ctx)
	if got := ns.events(); got != EventUpgradeFailed {
		t.Fatalf("events after a second pass = %s", got)
	}
}
