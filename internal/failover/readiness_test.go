package failover

import (
	"context"
	"errors"
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
