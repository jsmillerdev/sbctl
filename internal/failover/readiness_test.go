package failover

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

func TestReadinessOfAHealthyCluster(t *testing.T) {
	w := serverWorld(t)
	r, err := w.orch().Readiness(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Ready || len(r.Blockers) != 0 || r.Mode != "manual" || r.Fencer != "aws" || r.FencerStatus != "DryRun OK" || r.EpochMarker != "store reachable" {
		t.Fatalf("readiness: %+v", r)
	}
	var b strings.Builder
	r.Render(&b)
	if !strings.HasPrefix(b.String(), "failover  READY\n") || !strings.Contains(b.String(), "fencer: aws (DryRun OK)   epoch marker: store reachable") {
		t.Fatalf("rendered:\n%s", b.String())
	}
}

func TestReadinessSaysWhatBlocksAndWhatIsOnlyWorthKnowing(t *testing.T) {
	w := serverWorld(t)
	w.cfg.Fleet.StorageBackend = "file"
	org, _ := w.reg.GetOrganization(w.ctx, "acme")
	for _, ref := range []string{refC, "dddddddddddddddddddd"} {
		must(t, w.reg.CreateProject(w.ctx, &registry.Project{Ref: ref, OrgID: org.ID, Name: ref[:1], Status: registry.StatusActiveHealthy}))
	}
	w.provider.name = "command"
	r, err := w.orch().Readiness(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ready || len(r.Blockers) != 1 || !strings.Contains(r.Blockers[0], "storage_backend is file") {
		t.Fatalf("blockers: %+v", r.Blockers)
	}
	if len(r.ProjectsWithoutReplica) != 2 || !strings.Contains(strings.Join(r.Notes, "\n"), "2 project(s) have no replica (--restore-missing: RPO up to archive_timeout)") {
		t.Fatalf("notes %v, without %v", r.Notes, r.ProjectsWithoutReplica)
	}
	var b strings.Builder
	r.Render(&b)
	out := b.String()
	if !strings.HasPrefix(out, "failover  NOT READY: [fleet] storage_backend is file") || !strings.Contains(out, "\n          2 project(s) have no replica") || !strings.Contains(out, "fencer: command (ok)") {
		t.Fatalf("rendered:\n%s", out)
	}
}

func TestReadinessWithoutAFencerOrAMarkerStore(t *testing.T) {
	w := serverWorld(t)
	o := w.orch(func(d *Deps) { d.Provider = Manual{}; d.Marker = nil })
	r, err := o.Readiness(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Fencer != "none" || r.FencerStatus != "not configured" || !strings.HasPrefix(r.EpochMarker, "none") {
		t.Fatalf("readiness: %+v", r)
	}
	if r.Ready || !strings.Contains(strings.Join(r.Blockers, "\n"), "leader marker cannot be written") {
		t.Fatalf("blockers: %v", r.Blockers)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "--old-primary-is-down") {
		t.Fatalf("notes: %v", r.Notes)
	}
	w.markErr = errors.New("connection refused")
	r, _ = w.orch().Readiness(w.ctx)
	if !strings.HasPrefix(r.EpochMarker, "store not reachable") || r.Ready {
		t.Fatalf("an unreachable store: %+v", r)
	}
}

func TestReadinessOnAServerWithoutACluster(t *testing.T) {
	w := newWorld(t)
	for _, id := range []string{"n2"} {
		must(t, w.reg.DeleteReplica(w.ctx, idAN2))
		must(t, w.reg.DeleteReplica(w.ctx, idBN2))
		must(t, w.reg.DeleteReplica(w.ctx, idSysN2))
		must(t, w.reg.SetNodeState(w.ctx, id, registry.NodeLeft))
	}
	if _, err := w.orch().Readiness(w.ctx); !errors.Is(err, ErrNoCluster) {
		t.Fatalf("error: %v", err)
	}
}

func TestReadinessFromTheLeaderLooksAtTheBestStandby(t *testing.T) {
	w := newWorld(t) // n1 leads
	r, err := w.orch().Readiness(w.ctx)
	if err != nil || !r.Ready {
		t.Fatalf("%+v, %v", r, err)
	}
	// With the system standby lagging, the leader's view blocks.
	w.inst[idSysN2].lag = f64(500)
	r, _ = w.orch().Readiness(w.ctx)
	if r.Ready || !strings.Contains(strings.Join(r.Blockers, "\n"), "max_lag_seconds") {
		t.Fatalf("readiness: %+v", r)
	}
	// With no standby of the system cluster anywhere there is nobody to fail over to.
	must(t, w.reg.DeleteReplica(w.ctx, idSysN2))
	r, _ = w.orch().Readiness(w.ctx)
	if r.Ready || len(r.Blockers) != 1 || !strings.Contains(r.Blockers[0], "no other server holds a standby") {
		t.Fatalf("readiness: %+v", r)
	}
}

func TestTheFencerProbeIsNotRepeatedEveryTime(t *testing.T) {
	w := serverWorld(t)
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	o := w.orch(func(d *Deps) { d.Now = func() time.Time { return clock } })
	for i := 0; i < 4; i++ {
		if _, err := o.Readiness(w.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.count("provider.probe"); n != 1 {
		t.Fatalf("%d probes for 4 readings inside a minute", n)
	}
	clock = clock.Add(2 * time.Minute)
	_, _ = o.Readiness(w.ctx)
	if n := w.count("provider.probe"); n != 2 {
		t.Fatalf("%d probes after the cache expired", n)
	}
}

func TestReadinessShowsWhyAutomaticModeIsOff(t *testing.T) {
	m := newMonitorRig(t, config.FailoverServer)
	m.world.fail("provider.probe", errors.New("UnauthorizedOperation"), -1)
	m.mon.Tick(context.Background())
	r, _ := m.o.Readiness(m.ctx)
	if !strings.Contains(strings.Join(r.Notes, "\n"), "automatic failover is off: UnauthorizedOperation") {
		t.Fatalf("notes: %v", r.Notes)
	}
}

// The readiness is asked on the leader (the API serves it there). The leader is this node and cannot
// ping itself over the mesh; it answers, so a failover would be a switchover, which needs no fencer.
func TestReadinessOnTheLeaderOfAClusterWithoutAFencerIsReady(t *testing.T) {
	w := newWorld(t) // n1 leads and is this node
	o := w.orch(func(d *Deps) { d.Provider = Manual{} })
	r, err := o.Readiness(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Ready || len(r.Blockers) != 0 {
		t.Fatalf("readiness: %+v", r)
	}
	pl, err := o.PlanServer(w.ctx, ServerOptions{To: "n2"})
	must(t, err)
	if pl.Kind != "switchover" {
		t.Fatalf("plan: %+v", pl)
	}
}

// A fencer that fails its probe makes the block NOT READY even when the leader answers and the
// plan is a switchover that never asks the fencer: a failover would be refused.
func TestReadinessIsNotReadyWhileTheFencerFailsItsProbe(t *testing.T) {
	w := newWorld(t) // n1 leads and is this node, so the plan is a switchover
	w.fail("provider.probe", errors.New("UnauthorizedOperation: ec2:StopInstances"), -1)
	r, err := w.orch().Readiness(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ready || len(r.Blockers) != 1 || !strings.Contains(r.Blockers[0], "fencing") || !strings.Contains(r.Blockers[0], "UnauthorizedOperation") {
		t.Fatalf("readiness: %+v", r)
	}
	var b strings.Builder
	r.Render(&b)
	if !strings.HasPrefix(b.String(), "failover  NOT READY: fencing") {
		t.Fatalf("rendered:\n%s", b.String())
	}
	// A cluster that never configured a fencer has none to fail, and --old-primary-is-down is open to it.
	r, _ = w.orch(func(d *Deps) { d.Provider = Manual{} }).Readiness(w.ctx)
	if !r.Ready {
		t.Fatalf("without a fencer: %+v", r)
	}
}

func TestReadinessSaysAutomaticServerModeWaitsForProjectsWithoutAReplica(t *testing.T) {
	m := newMonitorRig(t, config.FailoverServer)
	org, _ := m.reg.GetOrganization(m.ctx, "acme")
	must(t, m.reg.CreateProject(m.ctx, &registry.Project{Ref: refC, OrgID: org.ID, Name: "c", Status: registry.StatusActiveHealthy}))
	r, err := m.o.Readiness(m.ctx)
	if err != nil || !strings.Contains(strings.Join(r.Notes, "\n"), "does not run while a project has no replica") {
		t.Fatalf("notes %v, %v", r.Notes, err)
	}
}

// A daemon whose wiring lacks an adapter runs a move that leaves Realtime and the shared services as
// they were. That is said where it is built and where it is read, not discovered after a switchover.
func TestAPortThatIsNotWiredIsLoggedAndShownInReadiness(t *testing.T) {
	w := serverWorld(t)
	var logged strings.Builder
	o := w.orch(func(d *Deps) {
		d.Fleet, d.LocalServices, d.Locks, d.Extra, d.Backups, d.Replicas = nil, nil, nil, nil, nil, nil
		d.Log = slog.New(slog.NewTextHandler(&logged, nil))
	})
	for _, port := range []string{"Fleet", "LocalServices", "Locker", "ExtraChecks", "BaseBackups", "ReplicaSetup"} {
		if !strings.Contains(logged.String(), "port="+port) {
			t.Errorf("the missing %s is not logged:\n%s", port, logged.String())
		}
	}
	if strings.Contains(logged.String(), "port=LocalPrimaries") || strings.Contains(logged.String(), "port=Takeover") {
		t.Errorf("a port that is wired is logged as missing:\n%s", logged.String())
	}
	r, err := o.Readiness(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(r.Notes, "\n")
	if !strings.Contains(notes, "Realtime and the pooler will not be re-registered") || !strings.Contains(notes, "shared services will keep running through a planned switchover") {
		t.Fatalf("notes: %v", r.Notes)
	}
	if !r.Ready {
		t.Fatalf("the move works without them, so readiness is not blocked: %v", r.Blockers)
	}
	// With them wired the notes are gone.
	if r, _ = w.orch().Readiness(w.ctx); strings.Contains(strings.Join(r.Notes, "\n"), "re-registered") {
		t.Fatalf("notes: %v", r.Notes)
	}
	var ports []string
	for _, g := range w.orch().Gaps() {
		ports = append(ports, g.Port)
	}
	if strings.Join(ports, ",") != "Locker,ExtraChecks" { // the test world wires the rest
		t.Fatalf("gaps of the test world: %v", ports)
	}
}

// ctxProvider is a fencer whose probe fails with the caller's context when it ends and passes otherwise.
type ctxProvider struct {
	*fakeProvider
	calls int
}

func (p *ctxProvider) Probe(ctx context.Context) error {
	p.calls++
	return ctx.Err()
}

// An interrupted `supavise status`, or a request that timed out, ends the probe's context. That says
// nothing about the fencer, and the next caller must not be told for a minute that the probe failed.
func TestAProbeThatTheCallerCutShortIsNotRemembered(t *testing.T) {
	w := serverWorld(t)
	cp := &ctxProvider{fakeProvider: w.provider}
	o := w.orch(func(d *Deps) { d.Provider = cp })
	gone, cancel := context.WithCancel(w.ctx)
	cancel()
	if err := o.probe(gone); err == nil {
		t.Fatal("a probe with a cancelled context passed")
	}
	if err := o.probe(w.ctx); err != nil || cp.calls != 2 {
		t.Fatalf("the next caller: %v after %d probe(s)", err, cp.calls)
	}
	// What the fencer says is remembered, so that every status does not cost EC2 calls.
	if err := o.probe(w.ctx); err != nil || cp.calls != 2 {
		t.Fatalf("a repeated probe: %v after %d probe(s)", err, cp.calls)
	}
	r, err := o.Readiness(w.ctx)
	if err != nil || strings.Contains(strings.Join(r.Blockers, "\n"), "fencer fails its probe") {
		t.Fatalf("readiness after it: %+v, %v", r, err)
	}
}

// The monitor's own context ends when the daemon stops: no verdict on the fencer, no alert.
func TestTheMonitorDoesNotTurnAutomaticModeOffBecauseItsOwnContextEnded(t *testing.T) {
	m := newMonitorRig(t, "server")
	cp := &ctxProvider{fakeProvider: m.provider}
	m.o.d.Provider = cp
	gone, cancel := context.WithCancel(m.ctx)
	cancel()
	if reason := m.mon.arm(gone); reason != "" {
		t.Fatalf("armed with a cancelled context: %q", reason)
	}
	for _, k := range m.alertKinds() {
		if k == "failover_auto_off" {
			t.Fatalf("alerts: %v", m.alerts)
		}
	}
	if reason := m.mon.arm(m.ctx); reason != "" {
		t.Fatalf("arm: %q", reason)
	}
}

// After a failover the new leader has the old one beside it, fenced until it rejoins. That is a
// cluster with no standby, and the readiness block says so instead of hiding.
func TestReadinessOfALeaderWhoseOnlyOtherNodeIsFenced(t *testing.T) {
	w := newWorld(t)
	must(t, w.reg.SetNodeState(w.ctx, "n2", registry.NodeFenced))
	r, err := w.orch().Readiness(w.ctx)
	if err != nil {
		t.Fatalf("a fenced node is still a node of the cluster: %v", err)
	}
	if r.Ready || !strings.Contains(strings.Join(r.Blockers, "\n"), "no other server holds a standby of the system cluster") {
		t.Fatalf("readiness: %+v", r)
	}
}
