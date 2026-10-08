package failover

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// mrig is a world for the monitor: a clock the test moves, a public probe it controls, and the
// monitor.
type mrig struct {
	*world
	now      time.Time
	probeErr error
	mon      *Monitor
	o        *Orchestrator
}

func (m *mrig) advance(d time.Duration) { m.now = m.now.Add(d) }

// newMonitorRig builds the cluster as n2 sees it with the leader dead in every way the automatic
// server mode checks: it does not answer the mesh, the public address does not answer, EC2 says
// stopped. Every test closes one gate or opens one.
func newMonitorRig(t *testing.T, mode string) *mrig {
	t.Helper()
	w := serverWorld(t)
	w.cfg.Failover = config.Failover{Mode: mode, Fencing: config.FencingAWS, MaxLagSeconds: 30, GraceSeconds: 90, ProjectGraceSeconds: 180, StopTimeoutSeconds: 120, CooldownMinutes: 60}
	w.down["n1"] = true
	w.provider.state = PeerState{State: "stopped"}
	m := &mrig{world: w, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), probeErr: errors.New("connection refused")}
	m.o = w.orch(func(d *Deps) {
		d.Now = func() time.Time { return m.now }
		d.PublicProbe = func(context.Context) error { return m.probeErr }
	})
	m.mon = NewMonitor(m.o)
	return m
}

// open runs the first look (which starts the silence) and moves past the grace period.
func (m *mrig) open() {
	m.t.Helper()
	if d := m.mon.Tick(m.ctx); d.Action != "none" {
		m.t.Fatalf("the first look must only start the clock: %+v", d)
	}
	m.advance(2 * time.Minute)
}

func TestServerModeGates(t *testing.T) {
	for name, tc := range map[string]struct {
		mode string
		mut  func(m *mrig)
		// want is "server" when the monitor acts, else the text its reason must contain.
		want string
	}{
		"every gate open": {want: "server"},
		"an impaired peer that EC2 says is running counts as down": {
			mut: func(m *mrig) { m.provider.state = PeerState{State: "running", Impaired: true} }, want: "server"},
		"EC2 says the peer runs fine": {
			mut: func(m *mrig) { m.provider.state = PeerState{State: "running", Detail: "system ok, instance ok"} }, want: "reports"},
		"EC2 says the peer is stopping": {
			mut: func(m *mrig) { m.provider.state = PeerState{State: "stopping"} }, want: "reports"},
		"EC2 cannot be asked": {
			mut: func(m *mrig) { m.world.fail("provider.peerstate", errors.New("throttled"), -1) }, want: "cloud was not asked"},
		"the public address still answers": {
			mut: func(m *mrig) { m.probeErr = nil }, want: "still answers"},
		"there is no public probe": {
			mut: func(m *mrig) { m.o.d.PublicProbe = nil }, want: "no public probe"},
		"the leader answers the mesh": {
			mut: func(m *mrig) { m.world.down["n1"] = false }, want: "answers"},
		"maintenance on the leader": {
			mut: func(m *mrig) {
				must(m.t, m.reg.SetMaintenance(m.ctx, registry.Maintenance{Node: "n1", Until: m.now.Add(time.Hour), Reason: "supavise upgrade"}))
				m.refreshMembers()
			}, want: "maintenance"},
		"maintenance that has expired": {
			mut: func(m *mrig) {
				must(m.t, m.reg.SetMaintenance(m.ctx, registry.Maintenance{Node: "n1", Until: m.now.Add(time.Minute), Reason: "supavise upgrade"}))
				m.refreshMembers()
			}, want: "server"},
		"a failover ran ten minutes ago": {
			mut: func(m *mrig) { m.move(registry.MoveFailover, registry.MoveDone, 8*time.Minute) }, want: "cooldown"},
		"a failed failover counts for the cooldown": {
			mut: func(m *mrig) { m.move(registry.MoveFailover, registry.MoveFailed, 8*time.Minute) }, want: "cooldown"},
		"a failover two hours ago": {
			mut: func(m *mrig) { m.move(registry.MoveFailover, registry.MoveDone, 2*time.Hour) }, want: "server"},
		"a planned switchover is not a failover": {
			mut: func(m *mrig) { m.move(registry.MoveSwitchover, registry.MoveDone, time.Minute) }, want: "server"},
		"the nodes run different releases": {
			mut: func(m *mrig) {
				n, _ := m.reg.GetNode(m.ctx, "n1")
				n.Version = "v0.1.9"
				must(m.t, m.reg.UpdateNode(m.ctx, n))
				m.refreshMembers()
			}, want: "different releases"},
		"the nodes are in different regions": {
			mut: func(m *mrig) {
				n, _ := m.reg.GetNode(m.ctx, "n1")
				n.Provider.AWS.Region = "us-east-1"
				must(m.t, m.reg.UpdateNode(m.ctx, n))
				n2, _ := m.reg.GetNode(m.ctx, "n2")
				n2.Provider.AWS.Region = "eu-west-1"
				must(m.t, m.reg.UpdateNode(m.ctx, n2))
				m.refreshMembers()
			}, want: "different regions"},
		"a replica's lag is unknown": {
			mut: func(m *mrig) { m.inst[idAN2].lag = nil }, want: "lag is unknown"},
		"a replica lags past the limit": {
			mut: func(m *mrig) { m.inst[idBN2].lag = f64(31) }, want: "above max_lag_seconds"},
		"the system standby lags past the limit": {
			mut: func(m *mrig) { m.inst[idSysN2].lag = f64(120) }, want: "above max_lag_seconds"},
		"the storage backend is files": {
			mut: func(m *mrig) { m.cfg.Fleet.StorageBackend = "file" }, want: "storage backend"},
		"the fencer is not aws": {
			mut: func(m *mrig) { m.provider.name = "command" }, want: "needs [failover] fencing"},
		"the fencer's probe fails": {
			mut: func(m *mrig) { m.world.fail("provider.probe", errors.New("UnauthorizedOperation"), -1) }, want: "UnauthorizedOperation"},
		"a move is running": {
			mut: func(m *mrig) { _, _ = m.o.acquire() }, want: "a move is running"},
		"the mode is manual":                       {mode: config.FailoverManual, want: "manual"},
		"project mode does not take over a leader": {mode: config.FailoverProject, want: "only the leader decides"},
		"a project without a replica is never restored from the archive by itself": {
			mut: func(m *mrig) {
				org, _ := m.reg.GetOrganization(m.ctx, "acme")
				must(m.t, m.reg.CreateProject(m.ctx, &registry.Project{Ref: refC, OrgID: org.ID, Name: "c", Status: registry.StatusActiveHealthy}))
				m.lsn[refC] = "0/9000060"
			}, want: "projects without a replica"},
	} {
		t.Run(name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = config.FailoverServer
			}
			m := newMonitorRig(t, mode)
			if tc.mut != nil {
				tc.mut(m)
			}
			m.open()
			d := m.mon.Tick(m.ctx)
			if tc.want == "server" {
				if d.Action != "server" || d.Err != nil {
					t.Fatalf("decision: %+v\n%v", d, m.snapshot())
				}
				if !m.has("provider.fence") || !m.has("promote n2/"+idSysN2) {
					t.Fatalf("the move did not run:\n%v", m.snapshot())
				}
				return
			}
			if d.Action != "none" || !strings.Contains(d.Reason, tc.want) {
				t.Fatalf("decision: %+v, want a reason with %q", d, tc.want)
			}
			// Nothing that fences or promotes happened.
			for _, e := range []string{"provider.fence", "provider.takeover", "promote", "marker", "fence "} {
				m.assertNever(e)
			}
		})
	}
}

// move records a move of the given kind and state that began now, and moves the clock on by age.
func (m *mrig) move(kind registry.MoveKind, state registry.MoveState, age time.Duration) {
	mv := registry.Move{Scope: registry.MoveServer, Kind: kind, FromNode: "n1", ToNode: "n2", Epoch: 1}
	must(m.t, m.reg.CreateMove(m.ctx, &mv))
	if state != registry.MoveRunning {
		must(m.t, m.reg.FinishMove(m.ctx, mv.ID, state, ""))
	}
	m.now = time.Now().Add(age)
}

// projectMove records a project failover of ref that ended age ago.
func (m *mrig) projectMove(ref string, kind registry.MoveKind, state registry.MoveState, age time.Duration) {
	mv := registry.Move{Scope: registry.MoveProject, Ref: ref, Kind: kind, FromNode: "n1", ToNode: "n2", Epoch: 1}
	must(m.t, m.reg.CreateMove(m.ctx, &mv))
	must(m.t, m.reg.FinishMove(m.ctx, mv.ID, state, ""))
	m.now = time.Now().Add(age)
}

// The cooldown is kept per what moves: a project that failed over holds back itself, not the others,
// and not the replacement of a leader that died after.
func TestTheCooldownIsPerProjectInProjectModeAndForTheServerInServerMode(t *testing.T) {
	due := func(m *mrig) {
		m.world.prim["n1/"+refA].healthy = false
		m.setStatus(refA, registry.StatusActiveUnhealthy)
		m.mon.Tick(m.ctx)
		m.advance(4 * time.Minute)
	}
	t.Run("another project failed over a minute ago", func(t *testing.T) {
		m := newProjectRig(t, config.FailoverProject)
		m.projectMove(refB, registry.MoveFailover, registry.MoveDone, time.Minute)
		due(m)
		if d := m.mon.Tick(m.ctx); d.Action != "project" || d.Ref != refA || d.Err != nil {
			t.Fatalf("decision: %+v\n%v", d, m.snapshot())
		}
	})
	t.Run("this project failed over a minute ago", func(t *testing.T) {
		m := newProjectRig(t, config.FailoverProject)
		m.projectMove(refA, registry.MoveFailover, registry.MoveDone, time.Minute)
		due(m)
		d := m.mon.Tick(m.ctx)
		if d.Action != "none" || !strings.Contains(d.Reason, "a failover of this project") || !strings.Contains(d.Reason, "cooldown") {
			t.Fatalf("decision: %+v", d)
		}
	})
	t.Run("a switchover of this project is no failover", func(t *testing.T) {
		m := newProjectRig(t, config.FailoverProject)
		m.projectMove(refA, registry.MoveSwitchover, registry.MoveDone, time.Minute)
		due(m)
		if d := m.mon.Tick(m.ctx); d.Action != "project" {
			t.Fatalf("decision: %+v", d)
		}
	})
	t.Run("a failover of the server moved every project", func(t *testing.T) {
		m := newProjectRig(t, config.FailoverProject)
		m.move(registry.MoveFailover, registry.MoveDone, time.Minute)
		due(m)
		if d := m.mon.Tick(m.ctx); d.Action != "none" || !strings.Contains(d.Reason, "a failover of the server") {
			t.Fatalf("decision: %+v", d)
		}
	})
	t.Run("the leader dies after a project failed over", func(t *testing.T) {
		m := newMonitorRig(t, config.FailoverServer)
		m.projectMove(refA, registry.MoveFailover, registry.MoveDone, time.Minute)
		m.open()
		if d := m.mon.Tick(m.ctx); d.Action != "server" || d.Err != nil {
			t.Fatalf("decision: %+v\n%v", d, m.snapshot())
		}
	})
}

func TestAutoModeDowngradesToManualWhenTheProbeFailsAndSaysSo(t *testing.T) {
	m := newMonitorRig(t, config.FailoverServer)
	m.world.fail("provider.probe", errors.New("the instance role may not call ec2:StopInstances (UnauthorizedOperation)"), -1)
	for i := 0; i < 3; i++ {
		d := m.mon.Tick(m.ctx)
		if d.Action != "none" || !strings.Contains(d.Reason, "UnauthorizedOperation") {
			t.Fatalf("look %d: %+v", i, d)
		}
		m.advance(2 * time.Minute)
	}
	// One alert, however many looks: the state flipped once.
	n := 0
	for _, a := range m.alerts {
		if a.Kind == alerts.KindFailoverAutoOff && a.Title == "Automatic failover is off" && !a.Resolved {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d alerts about the downgrade, want 1: %v", n, m.alertKinds())
	}
	if reason := m.o.autoOff(); !strings.Contains(reason, "UnauthorizedOperation") {
		t.Fatalf("readiness would not say why: %q", reason)
	}
	// The permission is fixed: the node arms itself again at the next look after the cache expires.
	m.world.clearFailures()
	m.advance(2 * time.Minute)
	m.mon.Tick(m.ctx)
	if reason := m.o.autoOff(); reason != "" {
		t.Fatalf("still off: %q", reason)
	}
	// It says so: the condition has a recovery message, and it is not a announcement of a move.
	last := m.alerts[len(m.alerts)-1]
	if last.Kind != alerts.KindFailoverAutoOff || !last.Resolved || last.Key != "failover/auto-off" {
		t.Fatalf("the last alert: %+v", last)
	}
}

func TestSilenceStartsOverWhenTheLeaderAnswers(t *testing.T) {
	m := newMonitorRig(t, config.FailoverServer)
	m.mon.Tick(m.ctx) // silent since now
	m.advance(80 * time.Second)
	m.world.down["n1"] = false
	if d := m.mon.Tick(m.ctx); !strings.Contains(d.Reason, "answers") {
		t.Fatalf("decision: %+v", d)
	}
	m.world.down["n1"] = true
	m.advance(80 * time.Second) // 160 s after the first look, but only 0 s of the new silence
	if d := m.mon.Tick(m.ctx); d.Action != "none" || !strings.Contains(d.Reason, "silent for") {
		t.Fatalf("decision: %+v", d)
	}
}

// ---- project mode: the leader decides about one project ----

func newProjectRig(t *testing.T, mode string) *mrig {
	t.Helper()
	w := newWorld(t) // n1 leads
	w.cfg.Failover = config.Failover{Mode: mode, Fencing: config.FencingAWS, MaxLagSeconds: 30, GraceSeconds: 90, ProjectGraceSeconds: 180, StopTimeoutSeconds: 120, CooldownMinutes: 60}
	m := &mrig{world: w, now: time.Now()}
	m.o = w.orch(func(d *Deps) { d.Now = func() time.Time { return m.now } })
	m.mon = NewMonitor(m.o)
	return m
}

func (m *mrig) setStatus(ref string, s registry.Status) {
	must(m.t, m.reg.SetProjectStatus(m.ctx, ref, s))
}

func TestProjectModeWaitsForTheGracePeriodAndThenFailsTheProjectOver(t *testing.T) {
	m := newProjectRig(t, config.FailoverProject)
	m.world.prim["n1/"+refA].healthy = false
	m.setStatus(refA, registry.StatusActiveUnhealthy)
	if d := m.mon.Tick(m.ctx); d.Action != "none" {
		t.Fatalf("first look: %+v", d)
	}
	m.advance(120 * time.Second)
	if d := m.mon.Tick(m.ctx); d.Action != "none" || !strings.Contains(d.Reason, "180") && !strings.Contains(d.Reason, "3m0s") {
		t.Fatalf("inside the grace period: %+v", d)
	}
	m.advance(70 * time.Second)
	d := m.mon.Tick(m.ctx)
	if d.Action != "project" || d.Ref != refA || d.Err != nil {
		t.Fatalf("decision: %+v\n%v", d, m.snapshot())
	}
	// Failed over, not switched over: the old primary was fenced, nothing was stopped cleanly.
	m.assertOrder("local.stop "+refA, "promote n2/"+idAN2, "registry.SetProjectNode "+refA+" n2 1")
	m.assertNever("stop n1/")
	if p := projectOf(t, m.world, refA); p.NodeID != "n2" {
		t.Fatalf("project: %+v", p)
	}
	// Project B was never touched.
	if p := projectOf(t, m.world, refB); p.NodeID != "n1" {
		t.Fatalf("B: %+v", p)
	}
}

func TestProjectModeForgetsAProjectThatRecovers(t *testing.T) {
	m := newProjectRig(t, config.FailoverProject)
	m.setStatus(refA, registry.StatusActiveUnhealthy)
	m.mon.Tick(m.ctx)
	m.advance(150 * time.Second)
	m.setStatus(refA, registry.StatusActiveHealthy) // the health recovery worked
	m.mon.Tick(m.ctx)
	m.advance(150 * time.Second)
	m.setStatus(refA, registry.StatusActiveUnhealthy)
	if d := m.mon.Tick(m.ctx); d.Action != "none" {
		t.Fatalf("the grace period must start over: %+v", d)
	}
	if m.has("promote") {
		t.Fatal("promoted")
	}
}

func TestProjectModeGates(t *testing.T) {
	for name, tc := range map[string]struct {
		mode string
		mut  func(m *mrig)
		want string
	}{
		"nothing wrong": {want: "server-or-project"},
		"the project's primary answers again": {
			mut: func(m *mrig) { m.world.prim["n1/"+refA].healthy = true }, want: "answers now"},
		"the home node does not answer": {
			mut: func(m *mrig) { m.world.down["n1"] = true }, want: "node failure"},
		"the replica lags past the limit": {
			mut: func(m *mrig) { m.inst[idAN2].lag = f64(60) }, want: "above max_lag_seconds"},
		"the replica's lag is unknown": {
			mut: func(m *mrig) { m.inst[idAN2].lag = nil }, want: "unknown"},
		"maintenance": {
			mut: func(m *mrig) {
				must(m.t, m.reg.SetMaintenance(m.ctx, registry.Maintenance{Node: "n1", Until: m.now.Add(time.Hour), Reason: "supavise upgrade"}))
				m.refreshMembers()
			}, want: "maintenance"},
		"a failover ran 5 minutes ago": {
			mut: func(m *mrig) { m.move(registry.MoveFailover, registry.MoveDone, 2*time.Minute) }, want: "cooldown"},
		"the nodes run different releases": {
			mut: func(m *mrig) {
				n, _ := m.reg.GetNode(m.ctx, "n2")
				n.Version = "v0.3.0"
				must(m.t, m.reg.UpdateNode(m.ctx, n))
				m.refreshMembers()
			}, want: "upgrade the older node first"},
		"the mode is manual": {mode: config.FailoverManual, want: "manual"},
		"the project has no replica": {
			mut: func(m *mrig) { must(m.t, m.reg.DeleteReplica(m.ctx, idAN2)) }, want: "no replica"},
		"a move is running": {
			mut: func(m *mrig) { _, _ = m.o.acquire() }, want: "a move is running"},
		"the system project is never failed over by itself": {
			mut: func(m *mrig) {
				m.setStatus(refA, registry.StatusActiveHealthy)
				m.setStatus(config.SystemRef, registry.StatusActiveUnhealthy)
			}, want: "no project has been unhealthy"},
	} {
		t.Run(name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = config.FailoverProject
			}
			m := newProjectRig(t, mode)
			m.world.prim["n1/"+refA].healthy = false
			m.setStatus(refA, registry.StatusActiveUnhealthy)
			if tc.mut != nil {
				tc.mut(m)
			}
			m.mon.Tick(m.ctx)
			m.advance(4 * time.Minute)
			d := m.mon.Tick(m.ctx)
			if tc.want == "server-or-project" {
				if d.Action != "project" || d.Err != nil {
					t.Fatalf("decision: %+v", d)
				}
				return
			}
			if d.Action == "project" || !strings.Contains(d.Reason, tc.want) {
				t.Fatalf("decision: %+v, want a reason with %q", d, tc.want)
			}
			m.assertNever("fence ")
			m.assertNever("promote")
		})
	}
}

// A project that cannot move does not hold back the due projects after it: the first of two
// unhealthy projects has no replica, and the second is failed over.
func TestProjectModeWalksPastAProjectThatCannotMove(t *testing.T) {
	m := newProjectRig(t, config.FailoverProject)
	must(t, m.reg.DeleteReplica(m.ctx, idAN2))
	for _, ref := range []string{refA, refB} {
		m.world.prim["n1/"+ref].healthy = false
		m.setStatus(ref, registry.StatusActiveUnhealthy)
	}
	m.mon.Tick(m.ctx)
	m.advance(4 * time.Minute)
	d := m.mon.Tick(m.ctx)
	if d.Action != "project" || d.Ref != refB || d.Err != nil {
		t.Fatalf("decision: %+v\n%v", d, m.snapshot())
	}
	if p := projectOf(t, m.world, refB); p.NodeID != "n2" {
		t.Fatalf("B: %+v", p)
	}
	if p := projectOf(t, m.world, refA); p.NodeID != "n1" {
		t.Fatalf("A has no replica and stays: %+v", p)
	}
}

// When every due project is refused the look says why for each, and starts nothing.
func TestProjectModeNamesEveryProjectItRefused(t *testing.T) {
	m := newProjectRig(t, config.FailoverProject)
	must(t, m.reg.DeleteReplica(m.ctx, idAN2))
	must(t, m.reg.DeleteReplica(m.ctx, idBN2))
	for _, ref := range []string{refA, refB} {
		m.world.prim["n1/"+ref].healthy = false
		m.setStatus(ref, registry.StatusActiveUnhealthy)
	}
	m.mon.Tick(m.ctx)
	m.advance(4 * time.Minute)
	d := m.mon.Tick(m.ctx)
	if d.Action != "none" || !strings.Contains(d.Reason, refA) || !strings.Contains(d.Reason, refB) {
		t.Fatalf("decision: %+v", d)
	}
	m.assertNever("promote")
}

func TestServerModeAlsoWatchesTheProjectsOfTheLeader(t *testing.T) {
	m := newProjectRig(t, config.FailoverServer)
	m.world.prim["n1/"+refB].healthy = false
	m.setStatus(refB, registry.StatusActiveUnhealthy)
	m.mon.Tick(m.ctx)
	m.advance(4 * time.Minute)
	if d := m.mon.Tick(m.ctx); d.Action != "project" || d.Ref != refB {
		t.Fatalf("decision: %+v", d)
	}
}

// The plan pings the leader once more. A leader that answers by then is not stopped by a monitor that
// saw it silent a moment ago: the plan would be a switchover, which quiesces and stops it cleanly.
func TestAFollowerThatSeesTheLeaderAnswerWhilePlanningDoesNotStopIt(t *testing.T) {
	m := newMonitorRig(t, config.FailoverServer)
	m.open()
	m.world.afterEvent("provider.peerstate", func() {
		m.world.mu.Lock()
		delete(m.world.down, "n1")
		m.world.mu.Unlock()
	})
	d := m.mon.Tick(m.ctx)
	if d.Action != "none" || !strings.Contains(d.Reason, "answers again") {
		t.Fatalf("decision: %+v\n%v", d, m.snapshot())
	}
	for _, e := range []string{"quiesce", "stop n1", "provider.fence", "marker", "promote"} {
		m.assertNever(e)
	}
}

// With three nodes every follower reaches the gates. The first in line (by node id) acts at the
// grace period; the next waits one more.
func TestFollowersTakeTurnsAndTheFirstInLineActsAtTheGracePeriod(t *testing.T) {
	m := newMonitorRig(t, config.FailoverServer)
	m.addNode3()
	m.open()
	if d := m.mon.Tick(m.ctx); d.Action != "server" || d.Err != nil {
		t.Fatalf("n2 is first in line: %+v\n%v", d, m.snapshot())
	}
}

func TestAFollowerLaterInLineWaitsAnotherGracePeriod(t *testing.T) {
	m := newMonitorRig(t, config.FailoverServer)
	m.addNode3()
	m.setSelf("n3", false)
	m.open() // two minutes of silence: n3's turn comes at twice the 90 seconds
	d := m.mon.Tick(m.ctx)
	if d.Action != "none" || !strings.Contains(d.Reason, "take their turn first") {
		t.Fatalf("inside the second grace period: %+v", d)
	}
	m.advance(70 * time.Second)
	if d := m.mon.Tick(m.ctx); strings.Contains(d.Reason, "turn") {
		// n3 holds no standby of the system cluster here, so the plan refuses it; the turn is no longer what stops it.
		t.Fatalf("after its turn: %+v", d)
	}
	for _, e := range []string{"provider.fence", "marker", "promote"} {
		m.assertNever(e)
	}
}

// Only a project that is still unhealthy and has no answering primary at the run is failed over.
func TestProjectModeDoesNotFenceAProjectThatRecoversBetweenThePlanAndTheRun(t *testing.T) {
	m := newProjectRig(t, config.FailoverProject)
	m.world.prim["n1/"+refA].healthy = false
	m.setStatus(refA, registry.StatusActiveUnhealthy)
	m.mon.Tick(m.ctx)
	m.advance(190 * time.Second)
	// The primary answers again after the monitor's plan and before the move's own.
	m.o.d.Primaries = &flipPrimaries{inner: m.o.d.Primaries, flipAfter: 1, onFlip: func() {
		m.world.mu.Lock()
		m.world.prim["n1/"+refA].healthy = true
		m.world.mu.Unlock()
	}}
	d := m.mon.Tick(m.ctx)
	if d.Action != "project" || !errors.Is(d.Err, ErrPlanChanged) {
		t.Fatalf("decision: %+v\n%v", d, m.snapshot())
	}
	m.assertNever("local.stop")
	m.assertNever("promote")
}

// flipPrimaries answers the first flipAfter health questions as the world does, then runs onFlip.
type flipPrimaries struct {
	inner     Primaries
	flipAfter int
	onFlip    func()
	mu        sync.Mutex
	asked     int
}

func (f *flipPrimaries) Healthy(ctx context.Context, node, ref string) (bool, string, error) {
	ok, detail, err := f.inner.Healthy(ctx, node, ref)
	f.mu.Lock()
	f.asked++
	flip := f.asked == f.flipAfter
	f.mu.Unlock()
	if flip {
		f.onFlip()
	}
	return ok, detail, err
}
func (f *flipPrimaries) Stop(ctx context.Context, node, ref string) (string, error) {
	return f.inner.Stop(ctx, node, ref)
}
func (f *flipPrimaries) Start(ctx context.Context, node, ref string) error {
	return f.inner.Start(ctx, node, ref)
}
func (f *flipPrimaries) SetAside(ctx context.Context, node, ref string, epoch int64) error {
	return f.inner.SetAside(ctx, node, ref, epoch)
}
