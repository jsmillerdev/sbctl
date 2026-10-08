package replicas

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
)

// A replica goes through the seven steps, one per pass once the node reports them, and ends
// ACTIVE_HEALTHY with its Supavisor tenant; the leader asked for a base backup of the configured age.
func TestSetupReachesActiveHealthy(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	r := e.replica(refA, "n2")
	if r.Status != registry.ReplicaInit || r.InitStep != StepRequested || r.Origin != registry.ReplicaManual {
		t.Fatalf("new row: %+v", r)
	}
	if want := "aaaaaaaaaaaaaaaaaaaa-rr-eu-west-1-000001"; r.Identifier != want {
		t.Fatalf("identifier %q, want %q", r.Identifier, want)
	}

	// Pass 1 admits the replica and the same pass launches it; every later pass follows the node.
	var seen []string
	for range 6 {
		e.tick(1)
		seen = append(seen, e.replica(refA, "n2").InitStep)
	}
	want := []string{StepLaunched, StepInitiated, StepDownloaded, StepReplayed, StepDone, StepDone}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("steps by pass: %v, want %v", seen, want)
		}
	}
	r = e.replica(refA, "n2")
	if r.Status != statusHealthy || r.InitError != "" {
		t.Fatalf("final row: %+v", r)
	}
	if len(e.pool.ensured) != 1 || e.pool.ensured[0] != r.Identifier {
		t.Fatalf("supavisor tenants: %v", e.pool.ensured)
	}
	if len(e.bk.calls) != 1 || e.bk.calls[0] != 24*time.Hour || e.bk.refs[0] != refA {
		t.Fatalf("EnsureBase calls: %v %v", e.bk.calls, e.bk.refs)
	}
	if len(e.nodes.ensured) != 1 {
		t.Fatalf("ensure calls: %+v", e.nodes.ensured)
	}
	spec := e.nodes.ensured[0]
	if spec.Identifier != r.Identifier || spec.Ref != refA || spec.BackupID != "20261008T110000Z" || spec.Epoch != 7 || spec.NoUpstream {
		t.Fatalf("instance spec: %+v", spec)
	}
	st, _ := e.ctrl.Statuses(e.ctx, refA)
	if len(st) != 1 || st[0].Status != statusHealthy || st[0].Init == nil || st[0].Init.Status != "completed" {
		t.Fatalf("statuses: %+v", st)
	}
}

// The backup age comes from [replicas] bootstrap_max_backup_age.
func TestSetupAsksForABackupOfTheConfiguredAge(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Config.Replicas.BootstrapMaxBackupAge = "6h" })
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.settle(refA, "n2")
	if e.bk.calls[0] != 6*time.Hour {
		t.Fatalf("max age %v", e.bk.calls[0])
	}
}

// A level-triggered controller picks up where the row says: a new controller (a restarted daemon)
// continues a setup that was at step 3.
func TestSetupResumesFromTheRowsStep(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(2) // launched, then initiated
	if got := e.replica(refA, "n2").InitStep; got != StepInitiated {
		t.Fatalf("step %s", got)
	}
	e.ctrl = New(e.opts)
	e.settle(refA, "n2")
	if len(e.bk.calls) != 1 {
		t.Fatalf("the new controller took another base backup: %v", e.bk.calls)
	}
}

// Every failure code, from the node and from the leader's own work.
func TestSetupFailureCodes(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(e *env)
		wantStep string
		wantCode string
	}{
		{"node reports a failed launch", func(e *env) {
			e.nodes.script = func(in *fakeInstance) { in.failAt, in.errCode = StepLaunched, FailLaunch }
		}, StepStarted, FailLaunch},
		{"node fails to initiate", func(e *env) {
			e.nodes.script = func(in *fakeInstance) { in.failAt, in.errCode = StepInitiated, FailInitiate }
		}, StepLaunched, FailInitiate},
		{"node fails the download", func(e *env) {
			e.nodes.script = func(in *fakeInstance) { in.failAt, in.errCode = StepInitiated, FailDownload }
		}, StepInitiated, FailDownload},
		{"node fails the replay", func(e *env) {
			e.nodes.script = func(in *fakeInstance) { in.failAt, in.errCode = StepDownloaded, FailReplay }
		}, StepDownloaded, FailReplay},
		{"node fails to complete", func(e *env) {
			e.nodes.script = func(in *fakeInstance) { in.failAt, in.errCode = StepReplayed, FailComplete }
		}, StepReplayed, FailComplete},
		{"node reports an error that is not a spec value", func(e *env) {
			e.nodes.script = func(in *fakeInstance) { in.failAt, in.errCode = StepInitiated, "disk full" }
		}, StepLaunched, FailInitiate},
		{"the node refuses the instance for good", func(e *env) {
			e.nodes.ensureErr = &mesh.RemoteError{Node: "n2", Status: 400, Message: "no room"}
		}, StepStarted, FailLaunch},
		{"the pooler tenant cannot be made", func(e *env) {
			e.pool.err = errors.New("supavisor is down")
		}, StepReplayed, FailComplete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.prepare(e)
			if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
				t.Fatal(err)
			}
			for range 12 {
				e.tick(1)
				e.clock.Advance(3 * time.Minute) // past the retry pauses
				if e.replica(refA, "n2").Status == registry.ReplicaInitError {
					break
				}
			}
			r := e.replica(refA, "n2")
			if r.Status != registry.ReplicaInitError || r.InitError != tt.wantCode || r.InitStep != tt.wantStep {
				t.Fatalf("row: status %s step %s error %q, want FAILED at %s with %s", r.Status, r.InitStep, r.InitError, tt.wantStep, tt.wantCode)
			}
			st, _ := e.ctrl.Statuses(e.ctx, refA)
			if st[0].Status != registry.ReplicaInitError || st[0].Init.Status != "failed" || st[0].Init.Error != tt.wantCode {
				t.Fatalf("status: %+v %+v", st[0], st[0].Init)
			}
			if e.alerts.count(alerts.KindReplicaUnhealthy, false) != 1 {
				t.Fatalf("alerts: %+v", e.alerts.evs)
			}
			// A failed setup is left alone until it is removed.
			calls := len(e.nodes.calls)
			e.tick(2)
			if len(e.nodes.calls) != calls {
				t.Fatalf("the controller kept calling the node: %v", e.nodes.calls[calls:])
			}
		})
	}
}

// A call that fails is tried again with a pause, and the setup fails only when the retry window is over.
func TestSetupRetriesBeforeFailing(t *testing.T) {
	e := newEnv(t)
	e.bk.err = errors.New("the backup store is unreachable")
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(1) // admitted, first try fails
	if r := e.replica(refA, "n2"); r.Status != registry.ReplicaInit || r.InitStep != StepStarted {
		t.Fatalf("after one failure: %+v", r)
	}
	n := len(e.bk.calls)
	e.tick(1) // inside the pause: no new try
	if len(e.bk.calls) != n {
		t.Fatalf("tried again at once: %d calls", len(e.bk.calls))
	}
	e.clock.Advance(30 * time.Second)
	e.tick(1)
	if len(e.bk.calls) != n+1 {
		t.Fatalf("did not try after the pause: %d calls", len(e.bk.calls))
	}
	// The store comes back: the setup goes on from the same step.
	e.bk.err = nil
	e.clock.Advance(3 * time.Minute)
	e.settle(refA, "n2")
}

// The pause between tries grows to two minutes and the window closes after Timeouts.Retry.
func TestSetupFailsWhenTheRetryWindowEnds(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Timeouts.Retry = 5 * time.Minute })
	e.bk.err = errors.New("store down")
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	for range 40 {
		e.tick(1)
		e.clock.Advance(30 * time.Second)
		if e.replica(refA, "n2").Status == registry.ReplicaInitError {
			break
		}
	}
	r := e.replica(refA, "n2")
	if r.InitError != FailLaunch || r.InitStep != StepStarted {
		t.Fatalf("row: %+v", r)
	}
	if n := len(e.bk.calls); n < 3 || n > 9 {
		t.Fatalf("%d tries in the window", n)
	}
}

// A step that never finishes fails with its code once its allowance is over.
func TestSetupStepTimeouts(t *testing.T) {
	tests := []struct {
		stuckAt  string
		wantCode string
		after    time.Duration
	}{
		{StepLaunched, FailInitiate, 5 * time.Minute},
		{StepInitiated, FailDownload, 30 * time.Minute},
		{StepDownloaded, FailReplay, 2 * time.Hour},
		{StepReplayed, FailComplete, 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.stuckAt, func(t *testing.T) {
			e := newEnv(t)
			e.nodes.script = func(in *fakeInstance) { in.stopAt = tt.stuckAt }
			if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
				t.Fatal(err)
			}
			e.tick(5)
			if r := e.replica(refA, "n2"); r.InitStep != tt.stuckAt || r.Status != registry.ReplicaInit {
				t.Fatalf("expected the setup stuck at %s: %+v", tt.stuckAt, r)
			}
			// Just inside the allowance (the estimate for 2 GB is 20 seconds, so 4x is far below it).
			e.clock.Advance(tt.after - time.Minute)
			e.tick(1)
			if r := e.replica(refA, "n2"); r.Status != registry.ReplicaInit {
				t.Fatalf("failed too early: %+v", r)
			}
			e.clock.Advance(2 * time.Minute)
			e.tick(1)
			r := e.replica(refA, "n2")
			if r.Status != registry.ReplicaInitError || r.InitError != tt.wantCode || r.InitStep != tt.stuckAt {
				t.Fatalf("row: %+v, want %s at %s", r, tt.wantCode, tt.stuckAt)
			}
		})
	}
}

// A node that lost the instance (it was wiped) is asked for it again, and the setup goes on.
func TestSetupRecreatesAnInstanceTheNodeLost(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(2)
	id := e.replica(refA, "n2").Identifier
	e.nodes.mu.Lock()
	delete(e.nodes.inst["n2"], id)
	e.nodes.mu.Unlock()
	e.clock.Advance(2 * time.Minute)
	e.settle(refA, "n2")
	if got := e.nodes.callsMatching("ensure n2 " + id); got != 2 {
		t.Fatalf("ensure calls: %d", got)
	}
}

// Concurrency: only [replicas] concurrency setups run at once, oldest first, the rest wait at 0_requested.
func TestSetupConcurrencyLimit(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Config.Replicas.Concurrency = 1 })
	e.addProject(refC, "small")
	for _, ref := range []string{refA, refB, refC} {
		if err := e.ctrl.SetupOn(e.ctx, ref, "n2"); err != nil {
			t.Fatal(err)
		}
	}
	e.tick(1)
	started := 0
	for _, ref := range []string{refA, refB, refC} {
		if e.replica(ref, "n2").InitStep != StepRequested {
			started++
		}
	}
	if started != 1 || e.replica(refA, "n2").InitStep == StepRequested {
		t.Fatalf("started %d, first %s", started, e.replica(refA, "n2").InitStep)
	}
	e.settle(refA, "n2")
	// The slot is free again; the next replica is admitted in the pass after.
	e.tick(1)
	if e.replica(refB, "n2").InitStep == StepRequested || e.replica(refC, "n2").InitStep != StepRequested {
		t.Fatalf("second round: B %s, C %s", e.replica(refB, "n2").InitStep, e.replica(refC, "n2").InitStep)
	}
	e.settle(refB, "n2")
	e.settle(refC, "n2")
}

// A project that is not running holds its replicas at 0_requested; they go on when it runs.
func TestSetupWaitsForARunningProject(t *testing.T) {
	e := newEnv(t)
	e.setProject(refA, registry.StatusInactive)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(3)
	if r := e.replica(refA, "n2"); r.InitStep != StepRequested {
		t.Fatalf("started for a paused project: %+v", r)
	}
	e.setProject(refA, registry.StatusActiveHealthy)
	e.settle(refA, "n2")
}

// Not enough room: the replica stays at 0_requested, the node gets one alert, and the alert
// closes when the replica fits.
func TestSetupWaitsForCapacity(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.SetupOn(e.ctx, refB, "n2"); err != nil {
		t.Fatal(err)
	}
	e.admit.full["n2"] = true
	e.tick(3)
	for _, ref := range []string{refA, refB} {
		if r := e.replica(ref, "n2"); r.InitStep != StepRequested || r.Status != registry.ReplicaInit {
			t.Fatalf("%s: %+v", ref, r)
		}
	}
	if e.alerts.count(alerts.KindReplicaCapacity, false) != 1 {
		t.Fatalf("capacity alerts: %+v", e.alerts.evs)
	}
	e.admit.full["n2"] = false
	e.settle(refA, "n2")
	e.settle(refB, "n2")
	if e.alerts.count(alerts.KindReplicaCapacity, true) != 1 {
		t.Fatalf("the capacity alert did not close: %+v", e.alerts.evs)
	}
	e.alerts.checkTitles(t)
}

// The room a replica needs counts the projects homed on the node and the replicas past admission,
// not the ones still waiting.
func TestAdmitSeesWhatTheNodeHolds(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.SetupOn(e.ctx, refB, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(1)
	if len(e.admit.reqs) < 2 {
		t.Fatalf("admit calls: %d", len(e.admit.reqs))
	}
	first, second := e.admit.reqs[len(e.admit.reqs)-2], e.admit.reqs[len(e.admit.reqs)-1]
	if len(first.Hosted) != 0 {
		t.Fatalf("n2 holds nothing yet, hosted %+v", first.Hosted)
	}
	if len(second.Hosted) != 1 || second.Hosted[0].Ref != e.replica(refA, "n2").Identifier {
		t.Fatalf("the second admission must count the first replica: %+v", second.Hosted)
	}
	if second.SeedBytes != 0 {
		t.Fatalf("seed bytes %d with no base backup row", second.SeedBytes)
	}
}

// What counts as a node's refusal for lack of room: the capacity error the node's admission makes,
// and anything that says NoRoom, through any wrapping; nothing else does.
func TestNoRoom(t *testing.T) {
	capacity := &lifecycle.CapacityError{Message: "node n2 has 1.0 GB of memory left and a small project needs 2.0 GB"}
	for name, tt := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                        {nil, false},
		"a capacity error":           {capacity, true},
		"a wrapped capacity error":   {fmt.Errorf("node n2: %w", capacity), true},
		"a NoRoom error":             {fmt.Errorf("call: %w", fakeNoRoom{"disk"}), true},
		"a NoRoom that says no":      {noRoomNo{}, false},
		"a failure of another kind":  {errors.New("boom"), false},
		"a remote error without one": {&mesh.RemoteError{Node: "n2", Status: 507}, false},
	} {
		if got := noRoom(tt.err); got != tt.want {
			t.Errorf("%s: noRoom = %v, want %v", name, got, tt.want)
		}
	}
}

type noRoomNo struct{}

func (noRoomNo) Error() string { return "no" }
func (noRoomNo) NoRoom() bool  { return false }

// A node the leader cannot judge refuses the replica itself when it is asked to create it. That
// is a wait, not a failure: the row goes back to 0_requested, the node gets one replica_capacity
// alert, the node is asked again every minute however long it takes, and the setup goes on when it fits.
func TestNodeRefusalForRoomWaits(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	id := e.replica(refA, "n2").Identifier
	e.nodes.ensureErr = fakeNoRoom{"not enough memory"}
	e.tick(2)
	if r := e.replica(refA, "n2"); r.Status != registry.ReplicaInit || r.InitStep != StepRequested || r.InitError != "" {
		t.Fatalf("after the refusal: %+v", r)
	}
	if got := e.nodes.callsMatching("ensure n2 " + id); got != 1 {
		t.Fatalf("ensure calls: %d", got)
	}
	if e.alerts.count(alerts.KindReplicaCapacity, false) != 1 || !strings.Contains(e.alerts.evs[0].Detail, "not enough memory") {
		t.Fatalf("capacity alerts: %+v", e.alerts.evs)
	}
	// Not asked again within the minute.
	e.clock.Advance(30 * time.Second)
	e.tick(2)
	if got := e.nodes.callsMatching("ensure n2 " + id); got != 1 {
		t.Fatalf("asked again within a minute: %d", got)
	}
	// Refused for longer than the window of failed calls: still waiting, one alert, no failure.
	for range 14 {
		e.clock.Advance(time.Minute)
		e.tick(2)
	}
	if r := e.replica(refA, "n2"); r.Status != registry.ReplicaInit || r.InitError != "" {
		t.Fatalf("after 14 minutes of refusals: %+v", r)
	}
	if got := e.nodes.callsMatching("ensure n2 " + id); got < 10 {
		t.Fatalf("ensure calls: %d", got)
	}
	if e.alerts.count(alerts.KindReplicaCapacity, false) != 1 || e.alerts.count(alerts.KindReplicaCapacity, true) != 0 {
		t.Fatalf("capacity alerts flapped: %+v", e.alerts.evs)
	}
	// Room is made: the next ask is taken and the setup runs to the end.
	e.nodes.ensureErr = nil
	e.clock.Advance(time.Minute)
	e.settle(refA, "n2")
	if e.alerts.count(alerts.KindReplicaCapacity, true) != 1 {
		t.Fatalf("the capacity alert did not close: %+v", e.alerts.evs)
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, false) != 0 {
		t.Fatalf("a refusal raised a failure: %+v", e.alerts.evs)
	}
}

// The node's report makes the leader's picture fresher, and a node can report only its own instances.
func TestHandleReportRecordsOnlyTheNodesOwnInstances(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.settle(refA, "n2")
	id := e.replica(refA, "n2").Identifier
	lag := 12.5
	st := fakeStatus(id, refA)
	st.LagSeconds = &lag
	e.ctrl.HandleReport(e.ctx, reportOf("n3", st)) // n3 does not hold it
	if got := e.ctrl.lagNow(id); got == 12.5 {
		t.Fatal("took a report from the wrong node")
	}
	e.ctrl.HandleReport(e.ctx, reportOf("n2", st))
	if got := e.ctrl.lagNow(id); got != 12.5 {
		t.Fatalf("lag %v after a report", got)
	}
	// A fresh report spares the poll.
	calls := e.nodes.callsMatching("observe n2 " + id)
	e.tick(1)
	if got := e.nodes.callsMatching("observe n2 " + id); got != calls {
		t.Fatalf("polled despite a fresh report: %d -> %d", calls, got)
	}
}

// A node that answers every request and still reports the instance absent does not hold the
// setup for ever: the step runs out of its allowance like any other, and the node is asked for the
// instance again at most once a minute.
func TestSetupFailsWhenTheNodeKeepsReportingTheInstanceAbsent(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(1)
	id := e.replica(refA, "n2").Identifier
	e.nodes.absent["n2"] = true
	for range 12 {
		e.clock.Advance(30 * time.Second)
		e.tick(1)
		if e.replica(refA, "n2").Status == registry.ReplicaInitError {
			break
		}
	}
	r := e.replica(refA, "n2")
	if r.Status != registry.ReplicaInitError || r.InitError != FailInitiate {
		t.Fatalf("row: %+v", r)
	}
	if got := e.nodes.callsMatching("ensure n2 " + id); got < 2 || got > 8 {
		t.Fatalf("ensure calls: %d", got)
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, false) != 1 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
}

// A setup that stops moving is nudged: the node is asked for the instance again, which resumes a
// setup a restart of its daemon interrupted, and not more often than every two minutes.
func TestSetupNudgesAStalledStep(t *testing.T) {
	e := newEnv(t)
	e.nodes.script = func(in *fakeInstance) { in.stopAt = StepInitiated }
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.tick(4)
	id := e.replica(refA, "n2").Identifier
	if got := e.nodes.callsMatching("ensure n2 " + id); got != 1 {
		t.Fatalf("ensure calls before the stall: %d", got)
	}
	e.clock.Advance(time.Minute)
	e.tick(1)
	if got := e.nodes.callsMatching("ensure n2 " + id); got != 1 {
		t.Fatalf("nudged after a minute: %d", got)
	}
	e.clock.Advance(90 * time.Second)
	e.tick(1)
	if got := e.nodes.callsMatching("ensure n2 " + id); got != 2 {
		t.Fatalf("not nudged after two minutes: %d", got)
	}
	e.clock.Advance(30 * time.Second)
	e.tick(1)
	if got := e.nodes.callsMatching("ensure n2 " + id); got != 2 {
		t.Fatalf("nudged twice within two minutes: %d", got)
	}
}
