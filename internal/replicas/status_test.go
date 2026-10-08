package replicas

import (
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// healthy replica in the registry: an active replica of refA on n2.
func (e *env) activeReplica() registry.Replica {
	e.t.Helper()
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		e.t.Fatal(err)
	}
	return e.settle(refA, "n2")
}

// The status mapping of design 2.7.7, one observation at a time.
func TestHealthMapping(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name       string
		mutate     func(o *peerapi.InstanceStatus)
		recvDown   time.Duration // the receiver has not been streaming for this long
		unreach    time.Duration // the node has not answered for this long
		from       string
		want       string
		wantReason string
	}{
		{"streaming and in step", nil, 0, 0, statusHealthy, statusHealthy, ""},
		{"unhealthy to healthy", nil, 0, 0, statusUnhealthy, statusHealthy, ""},
		{"lag just under the limit", func(o *peerapi.InstanceStatus) { o.LagSeconds = f(299) }, 0, 0, statusHealthy, statusHealthy, ""},
		{"lag over the limit", func(o *peerapi.InstanceStatus) { o.LagSeconds = f(301) }, 0, 0, statusHealthy, statusUnhealthy, "replication lag is 301 s, over the limit of 300 s"},
		{"postgrest down", func(o *peerapi.InstanceStatus) { o.PostgRESTReady = false }, 0, 0, statusHealthy, statusUnhealthy, "PostgREST does not answer"},
		{"postgres down", func(o *peerapi.InstanceStatus) { o.PostgresUp = false }, 0, 0, statusHealthy, statusUnhealthy, "Postgres is not running"},
		{"instance gone", func(o *peerapi.InstanceStatus) { o.Role = "absent"; o.PostgresUp = false }, 0, 0, statusHealthy, statusUnhealthy, "Postgres is not running"},
		{"receiver down for a minute", func(o *peerapi.InstanceStatus) { o.ReceiverStatus = "" }, time.Minute, 0, statusHealthy, statusHealthy, ""},
		{"receiver down for two minutes", func(o *peerapi.InstanceStatus) { o.ReceiverStatus = "" }, 2 * time.Minute, 0, statusHealthy, statusUnhealthy, "the WAL receiver has not been streaming for 2m0s"},
		{"node silent for a minute", nil, 0, time.Minute, statusHealthy, statusHealthy, ""},
		{"node silent for two minutes", nil, 0, 2 * time.Minute, statusHealthy, statusUnhealthy, "node n2 has not answered for 2m0s"},
		{"promoted: the failover owns it", func(o *peerapi.InstanceStatus) { o.Role = "primary"; o.InRecovery = false }, 0, 0, statusHealthy, statusHealthy, ""},
		{"restarting and not up yet", func(o *peerapi.InstanceStatus) { o.PostgresUp = false }, 0, 0, statusRestart, statusRestart, ""},
		{"restarting and back", nil, 0, 0, statusRestart, statusHealthy, ""},
		{"resizing and back", nil, 0, 0, statusResizing, statusHealthy, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			r := e.activeReplica()
			r.Status = tt.from
			obs := fakeStatus(r.Identifier, r.Ref)
			if tt.mutate != nil {
				tt.mutate(&obs)
			}
			e.clock.Advance(time.Second)
			e.ctrl.observed(r.Identifier, obs)
			e.ctrl.mu.Lock()
			s := e.ctrl.st(r.Identifier)
			if tt.recvDown > 0 {
				s.receiverDown = e.clock.Now().Add(-tt.recvDown)
			}
			if tt.unreach > 0 {
				s.unreachable = e.clock.Now().Add(-tt.unreach)
			}
			e.ctrl.mu.Unlock()
			got, reason := e.ctrl.health(e.ctx, &r, &obs)
			if got != tt.want || reason != tt.wantReason {
				t.Fatalf("health = %s, %q; want %s, %q", got, reason, tt.want, tt.wantReason)
			}
		})
	}
}

// A restart or resize does not end on an observation made before it, and fails open after ten minutes.
func TestTransientStatusesEndOnANewerObservation(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	obs := fakeStatus(r.Identifier, r.Ref)
	e.ctrl.observed(r.Identifier, obs)
	e.clock.Advance(time.Minute)
	r.Status = statusResizing
	if got, _ := e.ctrl.health(e.ctx, &r, &obs); got != statusResizing {
		t.Fatalf("a resize ended on an old observation: %s", got)
	}
	e.clock.Advance(time.Second)
	e.ctrl.observed(r.Identifier, obs)
	if got, _ := e.ctrl.health(e.ctx, &r, &obs); got != statusHealthy {
		t.Fatalf("the resize did not end on a newer one: %s", got)
	}
	// Ten minutes of a replica that does not come back.
	r.Status = statusRestart
	e.ctrl.mu.Lock()
	e.ctrl.st(r.Identifier).transient = e.clock.Now()
	e.ctrl.mu.Unlock()
	down := obs
	down.PostgresUp = false
	e.clock.Advance(9 * time.Minute)
	e.ctrl.observed(r.Identifier, down)
	if got, _ := e.ctrl.health(e.ctx, &r, &down); got != statusRestart {
		t.Fatalf("after 9 minutes: %s", got)
	}
	e.clock.Advance(2 * time.Minute)
	e.ctrl.observed(r.Identifier, down)
	if got, _ := e.ctrl.health(e.ctx, &r, &down); got != statusUnhealthy {
		t.Fatalf("after 11 minutes: %s", got)
	}
}

// The monitor writes a status only when it changes, and the alerts follow the edges.
func TestMonitorWritesTransitionsAndAlerts(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	set := func(f func(*fakeInstance)) {
		e.nodes.mu.Lock()
		f(e.nodes.inst["n2"][r.Identifier])
		e.nodes.mu.Unlock()
	}
	step := func() {
		e.clock.Advance(15 * time.Second)
		e.tick(1)
	}

	// A lag of 90 s is over the warning line but within the limit: healthy, with a replica_lag alert.
	set(func(in *fakeInstance) { in.lag = 90 })
	step()
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("status %s", got)
	}
	if e.alerts.count(alerts.KindReplicaLag, false) != 1 || e.alerts.count(alerts.KindReplicaUnhealthy, false) != 0 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
	step() // a second look raises nothing more
	if e.alerts.count(alerts.KindReplicaLag, false) != 1 {
		t.Fatalf("lag alert repeated: %+v", e.alerts.evs)
	}

	// Over the limit: unhealthy, one alert, the lag alert closes.
	set(func(in *fakeInstance) { in.lag = 600 })
	step()
	if got := e.replica(refA, "n2").Status; got != statusUnhealthy {
		t.Fatalf("status %s", got)
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, false) != 1 || e.alerts.count(alerts.KindReplicaLag, true) != 1 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}

	// Caught up: healthy again, the alert resolves once.
	set(func(in *fakeInstance) { in.lag = 0 })
	step()
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("status %s", got)
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, true) != 1 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
	e.alerts.checkTitles(t)

	// PostgREST down is unhealthy at once.
	set(func(in *fakeInstance) { in.postgrest = false })
	step()
	if got := e.replica(refA, "n2").Status; got != statusUnhealthy {
		t.Fatalf("status %s", got)
	}
}

// A node that stops answering makes its replicas unhealthy after two minutes, not before.
func TestMonitorTreatsASilentNodeAsUnhealthy(t *testing.T) {
	e := newEnv(t)
	e.activeReplica()
	e.nodes.down["n2"] = true
	e.clock.Advance(15 * time.Second)
	e.tick(1)
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("after one miss: %s", got)
	}
	e.clock.Advance(121 * time.Second)
	e.tick(1)
	if got := e.replica(refA, "n2").Status; got != statusUnhealthy {
		t.Fatalf("after two minutes: %s", got)
	}
	e.nodes.down["n2"] = false
	e.clock.Advance(15 * time.Second)
	e.tick(1)
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("after the node came back: %s", got)
	}
}

// A paused project's replicas are stopped with it and say nothing.
func TestMonitorIgnoresReplicasOfAPausedProject(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	e.setProject(refA, registry.StatusInactive)
	e.nodes.mu.Lock()
	e.nodes.inst["n2"][r.Identifier].postgrest = false
	e.nodes.mu.Unlock()
	e.clock.Advance(15 * time.Second)
	e.tick(2)
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("status %s", got)
	}
	if len(e.alerts.evs) != 0 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
}

// A node that lost an active replica's instance gets it asked for again, not more often than every
// five minutes, and the replica is unhealthy meanwhile.
func TestMonitorRecreatesALostInstance(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	e.nodes.mu.Lock()
	delete(e.nodes.inst["n2"], r.Identifier)
	e.nodes.mu.Unlock()
	before := e.nodes.callsMatching("ensure n2")
	e.clock.Advance(15 * time.Second)
	e.tick(1)
	if got := e.nodes.callsMatching("ensure n2"); got != before+1 {
		t.Fatalf("ensure calls %d -> %d", before, got)
	}
	e.nodes.mu.Lock()
	delete(e.nodes.inst["n2"], r.Identifier)
	e.nodes.mu.Unlock()
	e.clock.Advance(15 * time.Second)
	e.tick(1)
	if got := e.nodes.callsMatching("ensure n2"); got != before+1 {
		t.Fatalf("asked again within five minutes: %d", got)
	}
}

// Statuses gives each replica's step and estimations while it sets up, and the failure after it failed.
func TestStatusesTellTheSetupProgress(t *testing.T) {
	e := newEnv(t)
	e.bk.size = 4 << 30
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	get := func() Status {
		t.Helper()
		st, err := e.ctrl.Statuses(e.ctx, refA)
		if err != nil || len(st) != 1 {
			t.Fatalf("statuses: %+v %v", st, err)
		}
		return st[0]
	}
	st := get()
	if st.Status != registry.ReplicaInit || st.Init.Status != "in_progress" || st.Init.Progress != StepRequested || st.LagSeconds != -1 {
		t.Fatalf("requested: %+v %+v", st, st.Init)
	}
	e.tick(1) // launched
	st = get()
	if st.Init.Progress != StepLaunched {
		t.Fatalf("launched: %+v", st.Init)
	}
	// 4 GiB over the assumed 100 MB/s.
	if want := 43; st.Init.BaseBackupDownloadEstimateSeconds != want {
		t.Fatalf("download estimate %d s, want %d", st.Init.BaseBackupDownloadEstimateSeconds, want)
	}
	e.tick(2) // downloaded
	st = get()
	if st.Init.Progress != StepDownloaded || st.Init.BaseBackupDownloadEstimateSeconds != 0 {
		t.Fatalf("downloaded: %+v", st.Init)
	}
	e.settle(refA, "n2")
	st = get()
	if st.Init.Status != "completed" || st.Init.BaseBackupDownloadEstimateSeconds != 0 || st.Init.WALArchiveReplayEstimateSeconds != 0 {
		t.Fatalf("completed: %+v", st.Init)
	}
	if st.LagSeconds != 0 {
		t.Fatalf("lag %v", st.LagSeconds)
	}
}

// The download estimate follows the rate measured on earlier replicas, and the replay estimate is
// the WAL between the standby and the furthest position reported, at 50 MB/s.
func TestEstimations(t *testing.T) {
	e := newEnv(t)
	e.bk.size = 6e9
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	r := e.replica(refA, "n2")
	e.tick(2) // launched, initiated: the clock for the download starts
	e.clock.Advance(60 * time.Second)
	e.tick(1) // downloaded: 6 GB in 60 s is 100 MB/s... measure something else:
	if rate := e.ctrl.rate; rate < 99e6 || rate > 101e6 {
		t.Fatalf("measured rate %v", rate)
	}
	// A second replica: 6 GB at the measured rate.
	if err := e.ctrl.SetupOn(e.ctx, refB, "n2"); err != nil {
		t.Fatal(err)
	}
	e.ctrl.mu.Lock()
	e.ctrl.rate = 200e6
	e.ctrl.mu.Unlock()
	e.tick(1)
	rb := e.replica(refB, "n2")
	if got := e.ctrl.estimate(e.ctx, &rb).BaseBackupDownloadEstimateSeconds; got != 30 {
		t.Fatalf("download estimate %d s at 200 MB/s", got)
	}
	// Replay: the replica reported replay 0/5000000, another instance of the project reported
	// 0/5000000 + 100 MB.
	r = e.replica(refA, "n2")
	r.InitStep = StepInitiated
	head := peerapi.InstanceStatus{Identifier: "x", Ref: refA, ReceiveLSN: "0/B6C5A00"} // 0x5000000 + 100,000,000
	e.ctrl.observed("other", head)
	e.ctrl.mu.Lock()
	e.ctrl.st(r.Identifier).obs = &peerapi.InstanceStatus{Ref: refA, ReplayLSN: "0/5000000"}
	e.ctrl.mu.Unlock()
	if got := e.ctrl.estimate(e.ctx, &r).WALArchiveReplayEstimateSeconds; got != 2 {
		t.Fatalf("replay estimate %d s", got)
	}
	// Nothing known: zero.
	r2 := registry.Replica{Identifier: "none", Ref: refC, InitStep: StepInitiated}
	if got := e.ctrl.estimate(e.ctx, &r2); got.WALArchiveReplayEstimateSeconds != 0 {
		t.Fatalf("unknown backlog: %+v", got)
	}
}

func TestParseLSN(t *testing.T) {
	for in, want := range map[string]uint64{"0/3000100": 0x3000100, "1/0": 1 << 32, "A/FFFFFFFF": 0xA<<32 | 0xFFFFFFFF} {
		if got, ok := parseLSN(in); !ok || got != want {
			t.Errorf("parseLSN(%q) = %#x %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0", "x/y", "0/", "/1"} {
		if _, ok := parseLSN(in); ok {
			t.Errorf("parseLSN(%q) accepted", in)
		}
	}
}

// The ring keeps one point per minute (the mean of its samples) for 24 hours.
func TestLagRing(t *testing.T) {
	var r lagRing
	t0 := time.Date(2026, 10, 8, 12, 0, 20, 0, time.UTC)
	r.add(t0, 2)
	r.add(t0.Add(30*time.Second), 4) // 12:00:50, same minute as t0
	r.add(t0.Add(time.Minute), 10)
	r.add(t0.Add(3*time.Minute), 7)
	pts := r.since(time.Time{}, t0.Add(5*time.Minute))
	if len(pts) != 3 {
		t.Fatalf("points: %+v", pts)
	}
	if pts[0].Seconds != 3 || !pts[0].At.Equal(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("first point %+v: the mean of 2 and 4 at the start of the minute", pts[0])
	}
	if pts[1].Seconds != 10 || pts[2].Seconds != 7 {
		t.Fatalf("points: %+v", pts)
	}
	if got := r.since(t0.Add(2*time.Minute), t0.Add(5*time.Minute)); len(got) != 1 || got[0].Seconds != 7 {
		t.Fatalf("since: %+v", got)
	}
	if got := r.since(t0.Add(time.Hour), t0.Add(5*time.Minute)); len(got) != 0 {
		t.Fatalf("future since: %+v", got)
	}
	// Wrap: the points from the first two minutes are older than 24 hours and one minute at
	// 12:01 the next day; the slot of the second is reused by the new sample.
	later := t0.Add(24*time.Hour + time.Minute)
	r.add(later, 1)
	pts = r.since(time.Time{}, later)
	if len(pts) != 2 || pts[0].Seconds != 7 || pts[1].Seconds != 1 {
		t.Fatalf("after 24 hours: %+v", pts)
	}
	if !pts[0].At.After(later.Add(-lagWindow - time.Minute)) {
		t.Fatalf("a point older than the window: %+v", pts[0])
	}
}

// Lag returns what the controller sampled, oldest first, and an empty list for a replica it knows nothing of.
func TestLagComesFromObservations(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	for i, lag := range []float64{1, 3, 5} {
		e.nodes.mu.Lock()
		e.nodes.inst["n2"][r.Identifier].lag = lag
		e.nodes.mu.Unlock()
		e.clock.Advance(time.Minute)
		e.tick(1)
		_ = i
	}
	pts, err := e.ctrl.Lag(e.ctx, r.Identifier, time.Time{})
	if err != nil || len(pts) < 3 {
		t.Fatalf("lag: %+v %v", pts, err)
	}
	for i := 1; i < len(pts); i++ {
		if !pts[i].At.After(pts[i-1].At) {
			t.Fatalf("not oldest first: %+v", pts)
		}
	}
	if last := pts[len(pts)-1]; last.Seconds != 5 {
		t.Fatalf("last point %+v", last)
	}
	pts, err = e.ctrl.Lag(e.ctx, "nope-rr-eu-west-1-aaaaaa", time.Time{})
	if err != nil || pts == nil || len(pts) != 0 {
		t.Fatalf("unknown replica: %#v %v", pts, err)
	}
}

// A replica that is going down keeps the outcome of its setup in replicaInitializationStatus.
func TestInitStatusOfAReplicaGoingDown(t *testing.T) {
	e := newEnv(t)
	e.nodes.down["n2"] = true // the removals cannot finish
	init := func(ref string) *InitStatus {
		st, err := e.ctrl.Statuses(e.ctx, ref)
		if err != nil || len(st) != 1 || st[0].Status != statusGoingDown {
			t.Fatalf("statuses: %+v %v", st, err)
		}
		return st[0].Init
	}
	// Finished its setup.
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	e.nodes.down["n2"] = false
	e.settle(refA, "n2")
	e.nodes.down["n2"] = true
	if err := e.ctrl.Remove(e.ctx, refA, e.replica(refA, "n2").Identifier); err != nil {
		t.Fatal(err)
	}
	if is := init(refA); is.Status != "completed" || is.Progress != StepDone {
		t.Fatalf("completed then removed: %+v", is)
	}
	// Never finished it.
	if err := e.ctrl.SetupOn(e.ctx, refB, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.SetReplicaStatus(e.ctx, e.replica(refB, "n2").Identifier, registry.ReplicaInit, StepInitiated, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.Remove(e.ctx, refB, e.replica(refB, "n2").Identifier); err != nil {
		t.Fatal(err)
	}
	if is := init(refB); is.Status != "in_progress" || is.Progress != StepInitiated {
		t.Fatalf("removed mid-setup: %+v", is)
	}
	// Failed.
	e.addProject(refC, "small")
	if err := e.ctrl.SetupOn(e.ctx, refC, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.SetReplicaStatus(e.ctx, e.replica(refC, "n2").Identifier, registry.ReplicaInitError, StepDownloaded, FailDownload); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.Remove(e.ctx, refC, e.replica(refC, "n2").Identifier); err != nil {
		t.Fatal(err)
	}
	if is := init(refC); is.Status != "failed" || is.Error != FailDownload || is.Progress != StepDownloaded {
		t.Fatalf("failed then removed: %+v", is)
	}
}

// A leader that restarts while a replica is ACTIVE_UNHEALTHY in the registry has lost the memory
// of its alert; when the replica is well it closes the alert all the same.
func TestRecoveryAfterARestartClosesTheAlert(t *testing.T) {
	e := newEnv(t)
	r := e.activeReplica()
	if err := e.reg.SetReplicaStatus(e.ctx, r.Identifier, statusUnhealthy, StepDone, ""); err != nil {
		t.Fatal(err)
	}
	e.ctrl = New(e.opts)
	e.clock.Advance(15 * time.Second)
	e.tick(1)
	if got := e.replica(refA, "n2").Status; got != statusHealthy {
		t.Fatalf("status %s", got)
	}
	if e.alerts.count(alerts.KindReplicaUnhealthy, true) != 1 || e.alerts.count(alerts.KindReplicaUnhealthy, false) != 0 {
		t.Fatalf("alerts: %+v", e.alerts.evs)
	}
	if ev := e.alerts.evs[0]; ev.Title != titleUnhealthy || ev.Key != "replica_unhealthy/"+r.Identifier {
		t.Fatalf("recovery: %+v", ev)
	}
}
