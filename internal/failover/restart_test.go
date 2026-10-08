package failover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// takeoverFunc is a Takeover written as a function.
type takeoverFunc func(ctx context.Context, epoch int64) error

func (f takeoverFunc) BecomeLeader(ctx context.Context, epoch int64) error { return f(ctx, epoch) }

// cutAtTheTakeover makes a server move end the way the real one does: the system cluster is
// promoted, the daemon notices that its node leads now and stops to start again in that role, and the
// wait for the role ends with its context.
func cutAtTheTakeover(w *world, cancel context.CancelFunc) takeoverFunc {
	return func(ctx context.Context, epoch int64) error {
		w.log("become-leader epoch=%d (the daemon stops)", epoch)
		cancel()
		<-ctx.Done()
		return errors.New("the node did not become the leader before the wait ended")
	}
}

// restarted is the survivor's daemon after the restart: its system cluster is a primary, the boot
// decision settled on the epoch and AssumeLeadership recorded it, and the membership says the node leads.
func (w *world) restarted(epoch int64) {
	must(w.t, w.reg.SetLeader(w.ctx, "n2", epoch))
	w.setSelf("n2", true)
}

func TestAServerMoveCutOffByTheRestartContinuesInTheDaemonThatStarts(t *testing.T) {
	for name, down := range map[string]bool{"switchover": false, "failover": true} {
		t.Run(name, func(t *testing.T) {
			w := serverWorld(t)
			w.down["n1"] = down
			ctx, cancel := context.WithCancel(w.ctx)
			defer cancel()
			o := w.orch(func(d *Deps) { d.Takeover = cutAtTheTakeover(w, cancel) })
			mv, err := o.FailoverServer(ctx, ServerOptions{})
			if !errors.Is(err, ErrRestarting) {
				t.Fatalf("error: %v", err)
			}
			if mv == nil || mv.State == registry.MoveFailed || mv.State == registry.MoveAborted {
				t.Fatalf("the move that the restart cut off is not over: %+v", mv)
			}
			// Nothing says it failed: the move is running in its log, and the daemon that starts picks it up.
			for _, k := range w.alertKinds() {
				if k == alerts.KindFailoverFailed {
					t.Fatalf("a restart that the move asked for raised an alert: %v", w.alerts)
				}
			}
			fs, err := readStateFile(w.cfg.Paths().FailoverState())
			if err != nil || fs == nil || (fs.State != "" && fs.State != registry.MoveRunning) || !recordedStep(fs.Steps, "promote-system") || recordedStep(fs.Steps, "leader") {
				t.Fatalf("failover.json: %+v, %v", fs, err)
			}

			// The daemon that starts. Until it leads, nothing is continued.
			follower := w.orch()
			if got, err := follower.ResumeInterrupted(w.ctx); got != nil || err != nil {
				t.Fatalf("a node that does not lead continued the move: %+v, %v", got, err)
			}
			w.assertNever("registry.CreateMove")

			w.restarted(2)
			o2 := w.orch()
			got, err := o2.ResumeInterrupted(w.ctx)
			if err != nil || got == nil || got.State != registry.MoveDone {
				t.Fatalf("the continued move: %+v, %v\n%v", got, err, w.snapshot())
			}
			if n := w.count("promote n2/" + idSysN2); n != 1 {
				t.Fatalf("the system cluster was promoted %d times", n)
			}
			if stateFileExists(w) {
				t.Fatal("failover.json outlives the move")
			}
			logged := serverMove(t, w)
			if logged == nil || logged.State != registry.MoveDone || !hasStep(logged, "promote-system") || !hasStep(logged, "leader") || !hasStep(logged, "dns") {
				t.Fatalf("logged move: %+v", logged)
			}
			if !down {
				w.assertOrder("become-leader epoch=2 (the daemon stops)", "become-leader epoch=2", "registry.SetProjectNode system n2 2", "demote n1/")
			}
			for _, ref := range []string{refA, refB} {
				if p := projectOf(t, w, ref); p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
					t.Fatalf("project %s: %+v", ref, p)
				}
			}
			if cl := clusterOf(t, w); cl.Leader != "n2" || cl.Epoch != 2 {
				t.Fatalf("cluster: %+v", cl)
			}
			// One alert per run says it started, one says it finished.
			if k := strings.Join(w.alertKinds(), ","); k != alerts.KindFailoverStarted+","+alerts.KindFailoverStarted+","+alerts.KindFailoverCompleted {
				t.Fatalf("alerts: %s", k)
			}

			// The CLI that lost its connection finds the continued move by asking the daemon.
			st := o2.Follow(w.ctx, 0, 2)
			if st.State != "done" || st.Move == nil || st.Move.State != "done" || len(st.Steps) == 0 {
				t.Fatalf("follow: %+v", st)
			}
			if tail := o2.Follow(w.ctx, st.Next, 2); len(tail.Steps) != 0 || tail.State != "done" {
				t.Fatalf("follow from the end: %+v", tail)
			}
			// A daemon that restarted without a run of its own, as the old leader's does when it comes back
			// as a follower, reads the move from the registry it replicates.
			fresh := w.orch().Follow(w.ctx, 0, 2)
			if fresh.State != "done" || fresh.Move == nil || fresh.Move.Epoch != 2 || fresh.Next == 0 || len(fresh.Steps) != fresh.Next {
				t.Fatalf("follow from the registry: %+v", fresh)
			}
			if other := w.orch().Follow(w.ctx, 0, 9); other.State != "idle" {
				t.Fatalf("a move of another epoch: %+v", other)
			}
			// A finished run answers only for its own epoch: another move's CLI is not shown it.
			if other := o2.Follow(w.ctx, 0, 9); other.State != "idle" {
				t.Fatalf("a run of epoch 2 shown for epoch 9: %+v", other)
			}
			// So does the old leader that asked for the switchover, which polls the delegation endpoint.
			var seen ServerStatus
			if rec := serve(t, o2, "GET "+PathServer, PathServer+"?from=0", "n1", nil, &seen); rec.Code != http.StatusOK || seen.State != "done" {
				t.Fatalf("status for the old leader: %d %+v", rec.Code, seen)
			}
		})
	}
}

// The membership shows a promotion before the registry does (it reads the epoch from promote.ok), so
// the wait for the takeover can end while the daemon is still the standby's, with a registry handle
// that refuses writes. That is the same cut as a stopping daemon, and not a failure of the move.
func TestAServerMoveThatFindsItsRegistryReadOnlyAfterThePromotionWaitsForTheRestart(t *testing.T) {
	w := serverWorld(t)
	w.frozen = true
	o := w.orch()
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if !errors.Is(err, ErrRestarting) || !errors.Is(err, registry.ErrReadOnly) {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if mv == nil || mv.State == registry.MoveFailed {
		t.Fatalf("the move: %+v", mv)
	}
	for _, k := range w.alertKinds() {
		if k == alerts.KindFailoverFailed {
			t.Fatalf("alerts: %v", w.alerts)
		}
	}
	fs, err := readStateFile(w.cfg.Paths().FailoverState())
	if err != nil || fs == nil || (fs.State != "" && fs.State != registry.MoveRunning) || !recordedStep(fs.Steps, "promote-system") {
		t.Fatalf("failover.json: %+v, %v", fs, err)
	}
	if w.has("registry.CreateMove") || w.has("registry.SetLeader") {
		t.Fatalf("the read-only registry was written:\n%v", w.snapshot())
	}
	// The daemon that starts writes.
	w.frozen = false
	w.restarted(2)
	got, err := w.orch().ResumeInterrupted(w.ctx)
	if err != nil || got == nil || got.State != registry.MoveDone {
		t.Fatalf("the continued move: %+v, %v", got, err)
	}
	if n := w.count("promote n2/" + idSysN2); n != 1 {
		t.Fatalf("the system cluster was promoted %d times", n)
	}
}

func TestOnlyAMoveThatIsStillRunningIsContinuedByTheDaemon(t *testing.T) {
	t.Run("a move that ended failed is the operator's", func(t *testing.T) {
		w := serverWorld(t)
		w.fail("promote n2/"+idAN2, errors.New("injected"), -1)
		o := w.orch()
		mv, err := o.FailoverServer(w.ctx, ServerOptions{})
		if err == nil || mv.State != registry.MoveFailed {
			t.Fatalf("move %+v, error %v", mv, err)
		}
		w.clearFailures()
		w.restarted(2)
		events := len(w.snapshot())
		if got, err := w.orch().ResumeInterrupted(w.ctx); got != nil || err != nil {
			t.Fatalf("continued: %+v, %v", got, err)
		}
		if len(w.snapshot()) != events {
			t.Fatalf("a failed move was started again by itself:\n%v", w.snapshot()[events:])
		}
	})
	t.Run("a move to another node is not this node's", func(t *testing.T) {
		w := serverWorld(t)
		ctx, cancel := context.WithCancel(w.ctx)
		defer cancel()
		o := w.orch(func(d *Deps) { d.Takeover = cutAtTheTakeover(w, cancel) })
		if _, err := o.FailoverServer(ctx, ServerOptions{}); !errors.Is(err, ErrRestarting) {
			t.Fatal(err)
		}
		w.addNode3()
		w.setSelf("n3", true)
		if got, err := w.orch().ResumeInterrupted(w.ctx); got != nil || err != nil {
			t.Fatalf("continued: %+v, %v", got, err)
		}
	})
	t.Run("nothing unfinished", func(t *testing.T) {
		w := newWorld(t)
		if got, err := w.orch().ResumeInterrupted(w.ctx); got != nil || err != nil {
			t.Fatalf("continued: %+v, %v", got, err)
		}
	})
}

// A cancelled context before the leader marker is a failure like any other; the exception starts
// where the move cannot be undone.
func TestACancelledContextIsARestartOnlyAfterTheLeaderMarker(t *testing.T) {
	w := serverWorld(t)
	o := w.orch()
	ctx, cancel := context.WithCancel(w.ctx)
	cancel()
	j := o.fileJournal(registry.Move{Scope: registry.MoveServer, Kind: registry.MoveSwitchover, FromNode: "n1", ToNode: "n2", Epoch: 2}, serverFlags{}, nil)
	someError := errors.New("a step failed")
	if interrupted(ctx, j, someError) || interrupted(w.ctx, j, registry.ErrReadOnly) {
		t.Fatal("a move that has not written its marker counts as cut off by a restart")
	}
	must(t, j.record(w.ctx, "marker", "epoch 2"))
	if !interrupted(ctx, j, someError) || interrupted(w.ctx, j, someError) {
		t.Fatal("a cancelled context after the marker, and only it, is a restart")
	}
	if !interrupted(w.ctx, j, fmt.Errorf("creating the move: %w", registry.ErrReadOnly)) {
		t.Fatal("a registry that still refuses writes after the marker is a daemon that has not restarted yet")
	}
	pj := o.journalFor(registry.Move{Scope: registry.MoveProject, Steps: []registry.MoveStep{{Name: "marker"}}})
	if interrupted(ctx, pj, someError) || interrupted(w.ctx, pj, registry.ErrReadOnly) {
		t.Fatal("a project move is never cut off by a role change")
	}
}

// The leader that asked for the switchover polls the survivor, whose daemon restarts in the middle.
func TestTheLeaderWaitsOutTheRestartOfTheSurvivorsDaemon(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	done := ServerStatus{State: "done", Next: 5, Steps: []stepJSON{{Name: "dns", At: at}}, Move: &moveJSON{ID: 5, Scope: "server", Kind: "switchover", From: "n1", To: "n2", Epoch: 2, State: "done"}}
	rem := &scriptedRemote{}
	w := newWorld(t)
	script := []struct {
		st  ServerStatus
		err error
	}{
		{st: ServerStatus{State: "running", Next: 3}},
		{err: errors.New("the daemon is down")}, // the restart
		{err: errors.New("the daemon is down")},
		{st: ServerStatus{State: "idle"}}, // up again, and it has not picked the move up yet
		{st: ServerStatus{State: "idle"}},
		{st: ServerStatus{State: "running", Next: 1}},
		{st: done},
	}
	i := 0
	o := w.orch(func(d *Deps) {
		rem.Peers = d.Peers
		d.Peers = scriptedStatus{scriptedRemote: rem, next: func() (ServerStatus, error) {
			r := script[min(i, len(script)-1)]
			i++
			return r.st, r.err
		}}
	})
	mv, err := o.delegateServer(w.ctx, ServerOptions{To: "n2"}, &serverRun{to: registry.Node{ID: "n2", Name: "standby"}, planned: true})
	if err != nil || mv == nil || mv.State != registry.MoveDone {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if i != len(script) {
		t.Fatalf("looked %d times, want %d", i, len(script))
	}
}

// scriptedStatus answers the delegation's looks from a function.
type scriptedStatus struct {
	*scriptedRemote
	next func() (ServerStatus, error)
}

func (s scriptedStatus) ServerStatus(context.Context, string, int) (ServerStatus, error) {
	return s.next()
}

// The old leader's other clusters are demoted when it answers again with the new epoch: its daemon
// restarts once its system cluster is a standby, and its copy of the registry trails.
func TestTheOldLeadersOtherClustersWaitForItToLearnTheNewLeader(t *testing.T) {
	w := serverWorld(t)
	st := &staleOldLeader{Peers: (*worldPeers)(w), w: w, left: 3}
	var leftAtDemote = -1
	w.afterEvent("demote n1/"+refA, func() { leftAtDemote = st.left })
	o := w.orch(func(d *Deps) { d.Peers = st })
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if err != nil || mv.State != registry.MoveDone {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if leftAtDemote != 0 {
		t.Fatalf("a project was demoted while the old leader still reported the old epoch (%d stale answers left)", leftAtDemote)
	}
	w.assertOrder("demote n1/"+idSystemOn("n1", w), "demote n1/"+refA)
}

// idSystemOn is the identifier of the system replica row on node.
func idSystemOn(node string, w *world) string {
	reps, _ := w.reg.ListReplicas(w.ctx, config.SystemRef)
	for _, r := range reps {
		if r.NodeID == node {
			return r.Identifier
		}
	}
	return ""
}

// staleOldLeader answers pings for n1 with the old epoch for a while once its system cluster is demoted.
type staleOldLeader struct {
	Peers
	w    *world
	left int
}

func (s *staleOldLeader) Ping(ctx context.Context, node string) (peerapi.Ping, error) {
	p, err := s.Peers.Ping(ctx, node)
	if err == nil && node == "n1" && s.w.has("demote n1/") && s.left > 0 {
		s.left--
		p.Epoch, p.Leader = 1, "n1"
	}
	return p, err
}

func TestTheOldLeaderThatNeverAnswersFailsTheDemotionAndTheMoveResumes(t *testing.T) {
	w := serverWorld(t)
	nodeWaitWas := nodeWait
	nodeWait = 0
	defer func() { nodeWait = nodeWaitWas }()
	st := &staleOldLeader{Peers: (*worldPeers)(w), w: w, left: 1 << 30}
	o := w.orch(func(d *Deps) { d.Peers = st })
	mv, err := o.FailoverServer(w.ctx, ServerOptions{})
	if err == nil || mv.State != registry.MoveFailed || !strings.Contains(err.Error(), "does not answer as a follower") {
		t.Fatalf("move %+v, error %v", mv, err)
	}
	if p := projectOf(t, w, refA); p.NodeID != "n2" || p.Status != registry.StatusActiveHealthy {
		t.Fatalf("the projects run on the new leader although the old one could not be demoted: %+v", p)
	}
	st.left = 0
	w.restarted(2)
	mv, err = w.orch().FailoverServer(w.ctx, ServerOptions{Resume: true})
	if err != nil || mv.State != registry.MoveDone {
		t.Fatalf("resume: %+v, %v", mv, err)
	}
}

// The monitor is what the wiring starts in every daemon; it picks the cut-off move up first.
func TestTheMonitorContinuesTheMoveTheRestartCutOffBeforeItLooks(t *testing.T) {
	w := serverWorld(t)
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	o := w.orch(func(d *Deps) { d.Takeover = cutAtTheTakeover(w, cancel) })
	if _, err := o.FailoverServer(ctx, ServerOptions{}); !errors.Is(err, ErrRestarting) {
		t.Fatal(err)
	}
	w.restarted(2)
	NewMonitor(w.orch()).Run(w.ctx) // manual mode: it returns once the move is continued
	if mv := serverMove(t, w); mv == nil || mv.State != registry.MoveDone {
		t.Fatalf("move: %+v\n%v", mv, w.snapshot())
	}
}

// The wiring of a daemon continues the move itself once the shared services are up, with the plain
// FailoverServer call. Nothing in memory says so, and the CLI that lost its connection, and the old
// leader that waits for the survivor, still follow it: they read the move from its log.
func TestTheMoveIsFollowedFromItsLogWhoeverContinuesIt(t *testing.T) {
	w := serverWorld(t)
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	o := w.orch(func(d *Deps) { d.Takeover = cutAtTheTakeover(w, cancel) })
	if _, err := o.FailoverServer(ctx, ServerOptions{}); !errors.Is(err, ErrRestarting) {
		t.Fatal(err)
	}
	w.restarted(2)
	fresh := w.orch() // the daemon that started: nothing in memory

	// Before anything continues it, the log shows a move that is running, with the steps it holds.
	st := fresh.Follow(w.ctx, 0, 2)
	if st.State != "running" || len(st.Steps) == 0 || st.Move == nil || st.Move.Epoch != 2 || st.Steps[len(st.Steps)-1].Name != "promote-system" {
		t.Fatalf("follow: %+v", st)
	}
	var seen ServerStatus
	if rec := serve(t, fresh, "GET "+PathServer, PathServer+"?from=0", "n1", nil, &seen); rec.Code != http.StatusOK || seen.State != "running" || seen.Next != st.Next {
		t.Fatalf("status for the old leader: %d %+v", rec.Code, seen)
	}
	if rec := serve(t, fresh, "GET "+PathServer, PathServer+"?from=0", "n3", nil, &seen); rec.Code != http.StatusOK || seen.State != "idle" {
		t.Fatalf("a node the move is not for: %d %+v", rec.Code, seen)
	}

	// The wiring continues it with the plain call.
	mv, err := fresh.FailoverServer(w.ctx, ServerOptions{Resume: true})
	if err != nil || mv.State != registry.MoveDone {
		t.Fatalf("resume: %+v, %v", mv, err)
	}
	later := w.orch() // and the CLI asks a daemon that did not run it, or one that restarted again
	st = later.Follow(w.ctx, st.Next, 2)
	if st.State != "done" || st.Move == nil || st.Move.State != "done" || len(st.Steps) == 0 {
		t.Fatalf("follow after: %+v", st)
	}
	if rec := serve(t, later, "GET "+PathServer, PathServer+"?from=0", "n1", nil, &seen); rec.Code != http.StatusOK || seen.State != "done" {
		t.Fatalf("status for the old leader after: %d %+v", rec.Code, seen)
	}
}

// What the wiring finished in the time the daemon waited is not started again by the daemon.
func TestTheDaemonDoesNotStartAMoveThatTheWiringFinishedMeanwhile(t *testing.T) {
	w := serverWorld(t)
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	o := w.orch(func(d *Deps) { d.Takeover = cutAtTheTakeover(w, cancel) })
	if _, err := o.FailoverServer(ctx, ServerOptions{}); !errors.Is(err, ErrRestarting) {
		t.Fatal(err)
	}
	w.restarted(2)
	var wiring *Orchestrator
	d := w.deps()
	d.Sleep = func(c context.Context, _ time.Duration) error {
		if wiring != nil { // the wait of ResumeInterrupted: the wiring's resume runs in it
			if mv, err := wiring.FailoverServer(c, ServerOptions{Resume: true}); err != nil || mv.State != registry.MoveDone {
				t.Errorf("the wiring's resume: %+v, %v", mv, err)
			}
			wiring = nil
		}
		return c.Err()
	}
	daemon, err := New(d)
	must(t, err)
	wiring = w.orch()
	if got, err := daemon.ResumeInterrupted(w.ctx); got != nil || err != nil {
		t.Fatalf("the daemon started it again: %+v, %v", got, err)
	}
	if w.count("registry.CreateMove") != 1 || w.count("promote n2/"+idAN2) != 1 {
		t.Fatalf("the move ran twice:\n%v", w.snapshot())
	}
}
