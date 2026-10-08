package failover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

// serve sends a request to the handler of pattern as the peer node, and decodes the answer.
func serve(t *testing.T, o *Orchestrator, pattern, path string, peer string, body any, out any) *httptest.ResponseRecorder {
	t.Helper()
	return serveWith(t, context.Background(), o, pattern, path, peer, body, out)
}

// serveWith is serve for a request that carries ctx, which a caller that gives up cancels.
func serveWith(t *testing.T, ctx context.Context, o *Orchestrator, pattern, path string, peer string, body any, out any) *httptest.ResponseRecorder {
	t.Helper()
	mux := mesh.NewMux()
	for p, h := range o.PeerHandlers() {
		mux.Handle(p, h)
	}
	var rd bytes.Buffer
	if body != nil {
		must(t, json.NewEncoder(&rd).Encode(body))
	}
	method, _, _ := strings.Cut(pattern, " ")
	req := httptest.NewRequest(method, path, &rd).WithContext(ctx)
	if peer != "" {
		req = req.WithContext(mesh.WithPeer(req.Context(), mesh.Peer{Node: peer}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		must(t, json.Unmarshal(rec.Body.Bytes(), out))
	}
	return rec
}

func errorOf(rec *httptest.ResponseRecorder) peerapi.Error {
	var e peerapi.Error
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e
}

// launcher creates <ref>/postgres.run in the world's state directory, as a rendered cluster has.
func launcher(t *testing.T, w *world, ref string) string {
	t.Helper()
	run := units.FilesFor(w.cfg, units.Spec{Service: config.SvcPostgres, Ref: ref}).Run
	must(t, os.MkdirAll(filepath.Dir(run), 0o750))
	must(t, os.WriteFile(run, []byte("#!/bin/sh\n"), 0o750))
	return run
}

func exists1(p string) bool { _, err := os.Stat(p); return err == nil }

// The old leader, n1, is told by the survivor n2 that epoch 2 exists.
func TestCooperativeFenceOfANode(t *testing.T) {
	w := newWorld(t) // n1 leads at epoch 1
	runs := map[string]string{}
	for _, ref := range []string{config.SystemRef, refA, refB} {
		runs[ref] = launcher(t, w, ref)
	}
	o := w.orch()
	body := FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 2, Leader: "n2", Reason: "server failover to standby at epoch 2"}}

	// Nobody but the node that claims the leadership may ask.
	if rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "", body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no certificate: %d", rec.Code)
	}
	if rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n3", body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a node asking for another: %d %s", rec.Code, rec.Body)
	}
	if exists1(fencedPath(w)) || w.has("local.stop") {
		t.Fatal("a refused request changed something")
	}

	var resp peerapi.FenceResponse
	rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n2", body, &resp)
	if rec.Code != http.StatusOK || !resp.Fenced || resp.Epoch != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(resp.Stopped) != 3 {
		t.Fatalf("stopped: %v", resp.Stopped)
	}
	// The record is there, the launchers are gone (the unit cannot start a primary even at boot), and every primary was stopped.
	rc, err := fenced.Node(w.cfg.Paths())
	if err != nil || rc == nil || rc.Epoch != 2 || rc.Leader != "n2" {
		t.Fatalf("record: %+v, %v", rc, err)
	}
	for ref, run := range runs {
		if exists1(run) {
			t.Errorf("%s still has its launcher", ref)
		}
		if !w.has("local.stop " + ref) {
			t.Errorf("%s was not stopped", ref)
		}
	}
	// The record is written before anything is stopped, so nothing can start behind the handler's back.
	if w.index("local.stop") < 0 {
		t.Fatal("nothing stopped")
	}
	if k := w.alertKinds(); len(k) != 1 || k[0] != alerts.KindFenced || w.alerts[0].Severity != alerts.SeverityCritical {
		t.Fatalf("alerts: %v", w.alerts)
	}

	// The same request again changes nothing and says the node is fenced.
	resp = peerapi.FenceResponse{}
	serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n2", body, &resp)
	if !resp.Fenced {
		t.Fatalf("second answer: %+v", resp)
	}
}

// A node fence is asked in the epoch after the node's own, by an active node that holds a standby of
// the system cluster: the one that takes over. Anything else leaves the node as it is.
func TestANodeFenceNeedsTheNextEpochAndTheNodeThatTakesOver(t *testing.T) {
	ask := func(t *testing.T, w *world, peer string, epoch int64) (*httptest.ResponseRecorder, peerapi.FenceResponse) {
		t.Helper()
		var resp peerapi.FenceResponse
		rec := serve(t, w.orch(), "POST "+peerapi.PathFence, peerapi.PathFence, peer, FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: epoch, Leader: peer}}, &resp)
		return rec, resp
	}
	untouched := func(t *testing.T, w *world) {
		t.Helper()
		if exists1(fencedPath(w)) || w.has("local.stop") || len(w.alerts) != 0 {
			t.Fatalf("a refused fence changed something: %v", w.snapshot())
		}
	}
	t.Run("an epoch that is not the next one", func(t *testing.T) {
		w := newWorld(t) // n1 leads at epoch 1
		for _, epoch := range []int64{3, 9} {
			rec, resp := ask(t, w, "n2", epoch)
			if rec.Code != http.StatusOK || resp.Fenced || resp.Epoch != 1 {
				t.Fatalf("epoch %d: %d %+v", epoch, rec.Code, resp)
			}
		}
		untouched(t, w)
		if rec, resp := ask(t, w, "n2", 2); rec.Code != http.StatusOK || !resp.Fenced {
			t.Fatalf("the next epoch: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("a node that holds no standby of the system cluster", func(t *testing.T) {
		w := newWorld(t)
		w.addNode3() // active, and it holds nothing
		if rec, _ := ask(t, w, "n3", 2); rec.Code != http.StatusForbidden {
			t.Fatalf("n3: %d %s", rec.Code, rec.Body)
		}
		untouched(t, w)
	})
	t.Run("a node the registry does not know, or that is not active", func(t *testing.T) {
		w := newWorld(t)
		if rec, _ := ask(t, w, "n9", 2); rec.Code != http.StatusForbidden {
			t.Fatalf("unknown: %d %s", rec.Code, rec.Body)
		}
		must(t, w.reg.SetNodeState(w.ctx, "n2", registry.NodeLeft))
		if rec, _ := ask(t, w, "n2", 2); rec.Code != http.StatusForbidden {
			t.Fatalf("left: %d %s", rec.Code, rec.Body)
		}
		untouched(t, w)
	})
	t.Run("a registry that cannot say", func(t *testing.T) {
		w := newWorld(t)
		launcher(t, w, refA)
		o := w.orch(func(d *Deps) { d.Store = func() Store { return brokenStore{} } })
		var resp peerapi.FenceResponse
		rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n2", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 2, Leader: "n2"}}, &resp)
		if rec.Code != http.StatusOK || !resp.Fenced {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
}

// The record carries the peers' addresses (`supavise node rejoin` needs the leader's when its own
// registry is stopped), and the shared services stop with the primaries.
func TestANodeFenceRecordsThePeersAndStopsTheSharedServices(t *testing.T) {
	w := newWorld(t)
	n2, err := w.reg.GetNode(w.ctx, "n2")
	must(t, err)
	n2.PeerAddr = "10.0.1.7:7443"
	must(t, w.reg.UpdateNode(w.ctx, n2))
	w.refreshMembers()
	launcher(t, w, refA)
	o := w.orch()
	var resp peerapi.FenceResponse
	if rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n2", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 2, Leader: "n2"}}, &resp); rec.Code != http.StatusOK || !resp.Fenced {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rc, err := fenced.Node(w.cfg.Paths())
	if err != nil || rc == nil || rc.Peers["n2"] != "10.0.1.7:7443" {
		t.Fatalf("record: %+v, %v", rc, err)
	}
	// The membership layer reads the same file with its own type: both sides keep what the other wrote.
	mrec, err := cluster.ReadFenced(w.cfg)
	if err != nil || mrec == nil || mrec.Peers["n2"] != "10.0.1.7:7443" || mrec.Leader != "n2" || mrec.Epoch != 2 {
		t.Fatalf("the membership layer reads %+v, %v", mrec, err)
	}
	w.assertOrder("services.stop", "local.stop "+refA)
	// A project fence leaves the services alone.
	w2 := newWorld(t)
	o2 := w2.orch()
	serve(t, o2, "POST "+peerapi.PathFence, peerapi.PathFence, "n1", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 1, Leader: "n1"}, Ref: refA}, nil)
	w2.assertNever("services.stop")
}

func fencedPath(w *world) string { return fenced.NodePath(w.cfg.Paths()) }

func TestFenceRequestsAtAnEpochTheNodeAlreadyHasDoNothing(t *testing.T) {
	w := newWorld(t)
	w.setSelf("n1", true)
	o := w.orch()
	var resp peerapi.FenceResponse
	rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n2", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 1, Leader: "n2"}}, &resp)
	if rec.Code != http.StatusOK || resp.Fenced || resp.Epoch != 1 {
		t.Fatalf("a request at the node's own epoch: %d %+v", rec.Code, resp)
	}
	if w.has("local.stop") || exists1(fencedPath(w)) {
		t.Fatal("it acted on a request that does not name a higher epoch")
	}
}

func TestFenceReportsAPrimaryThatDoesNotStopAndStillRecordsTheFence(t *testing.T) {
	w := newWorld(t)
	launcher(t, w, refA)
	w.fail("local.stop "+refA, errors.New("timed out"), -1)
	o := w.orch()
	rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n2", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 2, Leader: "n2"}}, nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(errorOf(rec).Message, "timed out") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// The caller must not promote, and the node must not start it again.
	if r, _ := fenced.Node(w.cfg.Paths()); r == nil {
		t.Fatal("no record")
	}
	if !w.has("local.stop "+refB) || !w.has("local.stop system") {
		t.Fatalf("one failure stopped the rest from being tried:\n%v", w.snapshot())
	}
}

func TestProjectFenceStopsOneProjectAndNotTheNode(t *testing.T) {
	w := newWorld(t)
	runA, runB := launcher(t, w, refA), launcher(t, w, refB)
	o := w.orch()
	// The leader asks in the current epoch.
	body := FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 1, Leader: "n1", Reason: "project failover"}, Ref: refA}
	w.setSelf("n2", false) // this node is n2, a follower; n1 leads
	o = w.orch()
	var resp peerapi.FenceResponse
	rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n1", body, &resp)
	if rec.Code != http.StatusOK || !resp.Fenced || len(resp.Stopped) != 1 || resp.Stopped[0] != refA {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if exists1(runA) || !exists1(runB) {
		t.Fatalf("launchers: A %v, B %v", exists1(runA), exists1(runB))
	}
	if r, _ := fenced.Project(w.cfg.Paths(), refA); r == nil {
		t.Fatal("no project record")
	}
	if r, _ := fenced.Node(w.cfg.Paths()); r != nil {
		t.Fatalf("the node was fenced by a project fence: %+v", r)
	}
	if w.has("local.stop "+refB) || w.has("local.stop system") {
		t.Fatalf("other primaries were stopped:\n%v", w.snapshot())
	}
	if len(w.alerts) != 0 {
		t.Fatalf("a project fence is not the node being fenced: %v", w.alerts)
	}
	// Only the leader asks for a project fence, and not in a past epoch.
	other := FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 1, Leader: "n3"}, Ref: refB}
	if rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n3", other, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a node that does not lead: %d", rec.Code)
	}
	w.refreshMembers()
	old := FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 0, Leader: "n1"}, Ref: refB}
	var r2 peerapi.FenceResponse
	serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n1", old, &r2)
	if r2.Fenced {
		t.Fatalf("a request from a past epoch fenced: %+v", r2)
	}
}

// When the registry cannot be read (the system cluster is down) the node fences every cluster it has a launcher for.
func TestNodeFenceWithoutARegistryFencesWhatHasALauncher(t *testing.T) {
	w := newWorld(t)
	launcher(t, w, refA)
	launcher(t, w, "system")
	o := w.orch(func(d *Deps) { d.Store = func() Store { return brokenStore{} } })
	var resp peerapi.FenceResponse
	rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n2", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 2, Leader: "n2"}}, &resp)
	if rec.Code != http.StatusOK || !resp.Fenced || len(resp.Stopped) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// deadStore is the registry of a node whose system cluster has stopped: every question fails.
type deadStore struct{ Store }

var errRegistryDown = errors.New("connection refused")

func (deadStore) GetCluster(context.Context) (*registry.Cluster, error) { return nil, errRegistryDown }
func (deadStore) ListProjects(context.Context) ([]registry.Project, error) {
	return nil, errRegistryDown
}
func (deadStore) ListNodes(context.Context) ([]registry.Node, error) { return nil, errRegistryDown }
func (deadStore) SetMaintenance(context.Context, registry.Maintenance) error {
	return errRegistryDown
}

// registryGoneWithTheSystemCluster makes the world's registry fail from the moment the system
// cluster stops until it starts again, which is what the leader's own registry does.
func registryGoneWithTheSystemCluster(w *world) func(d *Deps) {
	var down atomic.Bool
	w.afterEvent("local.stop system", func() { down.Store(true) })
	w.afterEvent("local.start system", func() { down.Store(false) })
	return func(d *Deps) {
		d.Store = func() Store {
			if down.Load() {
				return deadStore{}
			}
			return &gatedStore{w: w, Registry: w.reg}
		}
	}
}

type brokenStore struct{ Store }

func (brokenStore) ListProjects(context.Context) ([]registry.Project, error) {
	return nil, errors.New("connection refused")
}
func (brokenStore) GetNode(context.Context, string) (*registry.Node, error) {
	return nil, errors.New("connection refused")
}
func (brokenStore) ListReplicas(context.Context, string) ([]registry.Replica, error) {
	return nil, errors.New("connection refused")
}

func TestQuiesceStopsEverythingInOrderAndReportsWhereEachStopped(t *testing.T) {
	w := newWorld(t) // n1 leads
	o := w.orch()
	var res QuiesceResult
	rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, &res)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, ref := range []string{config.SystemRef, refA, refB} {
		if res.LSNs[ref] != w.lsn[ref] {
			t.Errorf("%s stopped at %q, want %q", ref, res.LSNs[ref], w.lsn[ref])
		}
	}
	// Maintenance first; the projects, each after Realtime and the pooler let go; the shared
	// services; the system cluster last.
	w.assertOrder("registry.SetMaintenance n1", "fleet.quiesce "+refA, "local.stop "+refA, "services.stop", "local.stop system")
	w.assertOrder("registry.SetMaintenance n1", "fleet.quiesce "+refB, "local.stop "+refB, "services.stop", "local.stop system")
	if i, j := w.index("fleet.quiesce "+refA), w.index("local.stop "+refA); i > j {
		t.Fatal("Realtime was not quiesced before the stop")
	}
	cl, _ := w.reg.GetCluster(w.ctx)
	if !cl.Maintenance.Active(w.now()) || cl.Maintenance.Node != "n1" || cl.Maintenance.Reason != "switchover to n2" {
		t.Fatalf("maintenance: %+v", cl.Maintenance)
	}
}

// The leader's registry is the system cluster that the quiesce stops. A survivor that asks again
// (it crashed before it recorded the answer) is answered from the record the leader keeps in a file,
// and the undo is authorized from the same file.
func TestAQuiesceIsRepeatableAndUndoableAfterTheRegistryStopped(t *testing.T) {
	w := newWorld(t)
	o := w.orch(registryGoneWithTheSystemCluster(w))
	var first, again QuiesceResult
	if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, &first); rec.Code != http.StatusOK || len(first.LSNs) != 3 {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	if _, err := o.store().GetCluster(w.ctx); err == nil {
		t.Fatal("the registry should be down: the system cluster stopped")
	}

	// Asked again, it answers from the record and stops nothing a second time.
	if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, &again); rec.Code != http.StatusOK {
		t.Fatalf("again: %d %s", rec.Code, rec.Body)
	}
	for ref, lsn := range first.LSNs {
		if again.LSNs[ref] != lsn {
			t.Errorf("%s: %q, then %q", ref, lsn, again.LSNs[ref])
		}
	}
	if n := w.count("local.stop system"); n != 1 {
		t.Fatalf("the system cluster was stopped %d times", n)
	}
	// Another switchover is turned away while this one waits.
	if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n3", QuiesceRequest{Epoch: 2, To: "n3"}, nil); rec.Code != http.StatusConflict || errorOf(rec).Code != "quiesce_pending" {
		t.Fatalf("another node: %d %s", rec.Code, rec.Body)
	}

	// The undo works with the registry down: it is the node the quiesce was for, by the record.
	if rec := serve(t, o, "POST "+PathResume, PathResume, "n3", struct{}{}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("resume by another node: %d", rec.Code)
	}
	if rec := serve(t, o, "POST "+PathResume, PathResume, "n2", struct{}{}, nil); rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	w.assertOrder("local.start system", "services.start", "local.start "+refA, "local.start "+refB)
	if cl, _ := w.reg.GetCluster(w.ctx); cl.Maintenance.Node != "" {
		t.Fatalf("maintenance is still announced: %+v", cl.Maintenance)
	}
	if exists1(quiescePath(w.cfg.Paths())) {
		t.Fatal("the record outlived the undo")
	}
	// With the record gone there is nothing to undo, and a new switchover is accepted.
	if rec := serve(t, o, "POST "+PathResume, PathResume, "n2", struct{}{}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("resume of nothing: %d", rec.Code)
	}
	if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n3", QuiesceRequest{Epoch: 2, To: "n3"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("a new switchover: %d %s", rec.Code, rec.Body)
	}
}

// A quiesce that stopped on a project is tried again with the list of projects it made the first
// time, and maintenance is announced once.
func TestARetriedQuiesceKeepsItsListAndAnnouncesMaintenanceOnce(t *testing.T) {
	w := newWorld(t)
	w.fail("local.stop "+refB, errors.New("shutdown timed out"), 1)
	o := w.orch()
	if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("first: %d", rec.Code)
	}
	var res QuiesceResult
	if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, &res); rec.Code != http.StatusOK || len(res.LSNs) != 3 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if n := w.count("registry.SetMaintenance"); n != 1 {
		t.Fatalf("maintenance announced %d times", n)
	}
}

// A record of an epoch the cluster has reached is over: it does not block the next switchover.
func TestAStaleQuiesceRecordIsIgnored(t *testing.T) {
	w := newWorld(t)
	o := w.orch()
	must(t, o.saveQuiesce(&quiesceRecord{To: "n3", Epoch: 1, Reason: "switchover to n3", At: w.now(), LSNs: map[string]string{"system": "0/1"}}))
	if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestQuiesceRefusals(t *testing.T) {
	w := newWorld(t)
	o := w.orch()
	for name, tc := range map[string]struct {
		peer string
		req  QuiesceRequest
		code int
	}{
		"no certificate":     {"", QuiesceRequest{Epoch: 2, To: "n2"}, http.StatusUnauthorized},
		"a node for another": {"n3", QuiesceRequest{Epoch: 2, To: "n2"}, http.StatusForbidden},
		"the wrong epoch":    {"n2", QuiesceRequest{Epoch: 5, To: "n2"}, http.StatusConflict},
		"the epoch of today": {"n2", QuiesceRequest{Epoch: 1, To: "n2"}, http.StatusConflict},
	} {
		if rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, tc.peer, tc.req, nil); rec.Code != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	w.setSelf("n2", false)
	if rec := serve(t, w.orch(), "POST "+PathQuiesce, PathQuiesce, "n1", QuiesceRequest{Epoch: 2, To: "n1"}, nil); rec.Code != http.StatusConflict || errorOf(rec).Code != "not_leader" {
		t.Errorf("a follower: %d %s", rec.Code, rec.Body)
	}
	if w.has("local.stop") || w.has("services.stop") {
		t.Fatalf("a refused quiesce stopped something:\n%v", w.snapshot())
	}
}

func TestAQuiesceThatFailsHalfwayReportsItAndResumeStartsEverythingAgain(t *testing.T) {
	w := newWorld(t)
	w.fail("local.stop "+refB, errors.New("shutdown timed out"), 1)
	o := w.orch()
	rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(errorOf(rec).Message, refB) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if w.has("local.stop system") {
		t.Fatal("the system cluster was stopped although a project did not stop")
	}
	// The survivor gives up and says so; only the node the quiesce was for may undo it.
	if rec := serve(t, o, "POST "+PathResume, PathResume, "n3", struct{}{}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("resume by another node: %d", rec.Code)
	}
	rec = serve(t, o, "POST "+PathResume, PathResume, "n2", struct{}{}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	w.assertOrder("local.start system", "services.start", "local.start "+refA, "fleet.ensure "+refA)
	w.assertOrder("local.start system", "services.start", "local.start "+refB, "fleet.ensure "+refB)
	cl, _ := w.reg.GetCluster(w.ctx)
	if cl.Maintenance.Node != "" {
		t.Fatalf("maintenance is still announced: %+v", cl.Maintenance)
	}
	// With nothing announced there is nothing to undo.
	if rec := serve(t, o, "POST "+PathResume, PathResume, "n2", struct{}{}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("resume of nothing: %d", rec.Code)
	}
}

func TestPrimaryEndpointsServeOnlyTheLeader(t *testing.T) {
	w := newWorld(t)
	w.setSelf("n2", false) // n2 is the home; n1 leads
	o := w.orch()
	path := func(ref, op string) string { return PrimaryPath(ref, op) }
	var res PrimaryResult
	for _, op := range []string{OpStop, OpStart, OpHealth, OpAside} {
		res = PrimaryResult{}
		rec := serve(t, o, "POST "+PathPrimary, path(refA, op), "n1", PrimaryCall{Epoch: 1}, &res)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", op, rec.Code, rec.Body)
		}
		if op == OpStop && res.LSN != w.lsn[refA] {
			t.Fatalf("stop answered %q", res.LSN)
		}
	}
	for _, e := range []string{"local.stop " + refA, "local.start " + refA, "local.aside " + refA + " epoch=1"} {
		if !w.has(e) {
			t.Errorf("no %q:\n%v", e, w.snapshot())
		}
	}
	if rec := serve(t, o, "POST "+PathPrimary, path(refA, OpStop), "n3", PrimaryCall{Epoch: 1}, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a node that does not lead: %d", rec.Code)
	}
	if rec := serve(t, o, "POST "+PathPrimary, path(refA, OpStop), "n1", PrimaryCall{Epoch: 0}, nil); rec.Code != http.StatusConflict {
		t.Errorf("a past epoch: %d", rec.Code)
	}
	if rec := serve(t, o, "POST "+PathPrimary, path(refA, "drop"), "n1", PrimaryCall{Epoch: 1}, nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown operation: %d", rec.Code)
	}
	w.fail("local.stop "+refB, errors.New("boom"), -1)
	if rec := serve(t, o, "POST "+PathPrimary, path(refB, OpStop), "n1", PrimaryCall{Epoch: 1}, nil); rec.Code != http.StatusInternalServerError || !strings.Contains(errorOf(rec).Message, "boom") {
		t.Errorf("an error: %d %s", rec.Code, rec.Body)
	}
}

// The data of a live primary is not moved aside on a peer's say-so: the registry must home the
// project on another node first, as the leader does before it asks.
func TestAsideRefusesAProjectTheRegistryHomesOnThisNode(t *testing.T) {
	w := newWorld(t)
	w.setSelf("n2", false)
	rehomeOn(t, w, refA, "n2", "n1")
	o := w.orch()
	rec := serve(t, o, "POST "+PathPrimary, PrimaryPath(refA, OpAside), "n1", PrimaryCall{Epoch: 1}, nil)
	if rec.Code != http.StatusConflict || errorOf(rec).Code != "homed_here" || w.has("local.aside") {
		t.Fatalf("%d %s\n%v", rec.Code, rec.Body, w.snapshot())
	}
	// A project homed on the leader is the old primary of a move: its data on n2 may go.
	if rec := serve(t, o, "POST "+PathPrimary, PrimaryPath(refB, OpAside), "n1", PrimaryCall{Epoch: 1}, nil); rec.Code != http.StatusOK || !w.has("local.aside "+refB) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// A ref from a peer builds paths (the launcher, the fence record, the set-aside data): only a real ref passes.
func TestARefThatIsNotAProjectRefIsRefusedBeforeItBuildsAPath(t *testing.T) {
	w := newWorld(t)
	w.setSelf("n2", false)
	o := w.orch()
	for _, ref := range []string{"../etc", "UPPERCASEUPPERCASEUP", "short", "aaaaaaaaaaaaaaaaaaaaa"} {
		rec := serve(t, o, "POST "+peerapi.PathFence, peerapi.PathFence, "n1", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 1, Leader: "n1"}, Ref: ref}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("fence of %q: %d %s", ref, rec.Code, rec.Body)
		}
	}
	for _, ref := range []string{"UPPERCASEUPPERCASEUP", "short"} {
		rec := serve(t, o, "POST "+PathPrimary, PrimaryPath(ref, OpStop), "n1", PrimaryCall{Epoch: 1}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("stop of %q: %d %s", ref, rec.Code, rec.Body)
		}
	}
	if w.has("local.") {
		t.Fatalf("a bad ref reached the plane:\n%v", w.snapshot())
	}
}

// The caller of a fence may give up after its 20 seconds; the launchers are gone and the record is
// written by then, so the stops go on.
func TestTheStopsOfAFenceOutliveACallerThatGaveUp(t *testing.T) {
	w := newWorld(t)
	launcher(t, w, refA)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var resp peerapi.FenceResponse
	rec := serveWith(t, ctx, w.orch(), "POST "+peerapi.PathFence, peerapi.PathFence, "n2",
		FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 2, Leader: "n2"}}, &resp)
	if rec.Code != http.StatusOK || !resp.Fenced || !w.has("local.stop "+refA) {
		t.Fatalf("%d %s\n%v", rec.Code, rec.Body, w.snapshot())
	}
}

func TestEveryPeerHandlerIsRegisteredOnceAndThePathsAreTheDocumentedOnes(t *testing.T) {
	o := newWorld(t).orch()
	m := mesh.NewMux()
	for p, h := range o.PeerHandlers() {
		m.Handle(p, h) // panics on a clash
	}
	want := []string{"POST /peer/v1/fence", "POST /peer/v1/failover/quiesce", "POST /peer/v1/failover/resume", "POST /peer/v1/failover/primary/{ref}/{op}",
		"POST /peer/v1/failover/server", "GET /peer/v1/failover/server"}
	got := m.Patterns()
	if len(got) != len(want) {
		t.Fatalf("patterns: %v", got)
	}
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	for _, p := range want {
		if !have[p] {
			t.Errorf("no handler for %s", p)
		}
	}
	if PrimaryPath("abc", "stop") != "/peer/v1/failover/primary/abc/stop" {
		t.Fatalf("PrimaryPath: %s", PrimaryPath("abc", "stop"))
	}
}

// fakeRPC records the calls of the mesh clients.
type fakeRPC struct {
	calls  []string
	bodies []string
	answer map[string]any
	err    error
}

func (f *fakeRPC) Call(_ context.Context, node, method, path string, in, out any) error {
	f.calls = append(f.calls, node+" "+method+" "+path)
	b, _ := json.Marshal(in)
	f.bodies = append(f.bodies, string(b))
	if f.err != nil {
		return f.err
	}
	if a, ok := f.answer[path]; ok && out != nil {
		raw, _ := json.Marshal(a)
		return json.Unmarshal(raw, out)
	}
	return nil
}

func TestMeshClientsSpeakThePeerAPI(t *testing.T) {
	rpc := &fakeRPC{answer: map[string]any{
		peerapi.PathPing:            peerapi.Ping{Node: "n1", Epoch: 3},
		peerapi.PathFence:           peerapi.FenceResponse{Epoch: 3, Fenced: true, Stopped: []string{"system"}},
		PathQuiesce:                 QuiesceResult{LSNs: map[string]string{"system": "0/3000060"}},
		PrimaryPath(refA, OpStop):   PrimaryResult{LSN: "0/5000060"},
		PrimaryPath(refA, OpHealth): PrimaryResult{Healthy: true, Detail: "ok"},
	}}
	ctx := context.Background()
	p := MeshPeers{RPC: rpc}
	if ping, err := p.Ping(ctx, "n1"); err != nil || ping.Epoch != 3 {
		t.Fatalf("ping: %+v, %v", ping, err)
	}
	fr, err := p.Fence(ctx, "n1", FenceCall{FenceRequest: peerapi.FenceRequest{Epoch: 3, Leader: "n2"}, Ref: refA})
	if err != nil || !fr.Fenced {
		t.Fatalf("fence: %+v, %v", fr, err)
	}
	// The body is a peerapi.FenceRequest with one more field: an older peer reads it as one.
	if !strings.Contains(rpc.bodies[len(rpc.bodies)-1], `"epoch":3`) || !strings.Contains(rpc.bodies[len(rpc.bodies)-1], `"leader":"n2"`) || !strings.Contains(rpc.bodies[len(rpc.bodies)-1], `"ref":"`+refA+`"`) {
		t.Fatalf("fence body: %s", rpc.bodies[len(rpc.bodies)-1])
	}
	if q, err := p.Quiesce(ctx, "n1", QuiesceRequest{Epoch: 3, To: "n2"}); err != nil || q.LSNs["system"] != "0/3000060" {
		t.Fatalf("quiesce: %+v, %v", q, err)
	}
	if err := p.Resume(ctx, "n1"); err != nil {
		t.Fatal(err)
	}

	local := &worldLocal{}
	w := newWorld(t)
	local = (*worldLocal)(w)
	pr := MeshPrimaries{Self: func() string { return "n2" }, Local: local, RPC: rpc, Epoch: func() int64 { return 3 }}
	if lsn, err := pr.Stop(ctx, "n1", refA); err != nil || lsn != "0/5000060" {
		t.Fatalf("remote stop: %q, %v", lsn, err)
	}
	if ok, d, err := pr.Healthy(ctx, "n1", refA); err != nil || !ok || d != "ok" {
		t.Fatalf("remote health: %v %q %v", ok, d, err)
	}
	if err := pr.Start(ctx, "n1", refA); err != nil {
		t.Fatal(err)
	}
	if err := pr.SetAside(ctx, "n1", refA, 3); err != nil {
		t.Fatal(err)
	}
	n := len(rpc.calls)
	w.self = "n2"
	if _, err := pr.Stop(ctx, "n2", refA); err != nil {
		t.Fatal(err)
	}
	if len(rpc.calls) != n || !w.has("local.stop "+refA) {
		t.Fatalf("a call to this node went over the mesh: %v", rpc.calls)
	}
	want := []string{
		"n1 GET /peer/v1/ping", "n1 POST /peer/v1/fence", "n1 POST /peer/v1/failover/quiesce", "n1 POST /peer/v1/failover/resume",
		"n1 POST /peer/v1/failover/primary/" + refA + "/stop", "n1 POST /peer/v1/failover/primary/" + refA + "/health",
		"n1 POST /peer/v1/failover/primary/" + refA + "/start", "n1 POST /peer/v1/failover/primary/" + refA + "/aside",
	}
	if strings.Join(rpc.calls[:len(want)], "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(rpc.calls, "\n"))
	}
	if !strings.Contains(rpc.bodies[len(want)-4], `"epoch":3`) {
		t.Fatalf("a primary call carries the epoch: %s", rpc.bodies[len(want)-4])
	}
	rpc.err = errors.New("no session")
	if _, err := p.Ping(ctx, "n1"); err == nil {
		t.Fatal("an error was lost")
	}
}

// ---- boot ----

func bootRig(t *testing.T) *world {
	t.Helper()
	w := newWorld(t) // n1 leads at epoch 1
	launcher(t, w, config.SystemRef)
	launcher(t, w, refA)
	launcher(t, w, refB)
	return w
}

func TestBootCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		mut    func(w *world)
		fenced bool
		why    string
	}{
		"nothing newer anywhere": {},
		"a peer at a higher epoch": {fenced: true, why: "peer standby says epoch 2",
			mut: func(w *world) { w.setPeerEpoch(2, "n2") }},
		"the marker at a higher epoch": {fenced: true, why: "leader marker says epoch 2",
			mut: func(w *world) { w.setMarkerEpoch(2, "n2"); w.down["n2"] = true }},
		"a peer at the same epoch under another leader": {fenced: true, why: "n2 leads at epoch 1",
			mut: func(w *world) { w.setPeerEpoch(1, "n2") }},
		"the marker at the same epoch under the same leader": {
			mut: func(w *world) { w.setMarkerEpoch(1, "n1") }},
		"a peer behind": {
			mut: func(w *world) { w.setPeerEpoch(0, "n1") }},
		"neither source reachable": {
			mut: func(w *world) { w.down["n2"] = true; w.markErr = errors.New("store down") }},
		"a follower that missed a leader change is not fenced for it": {
			mut: func(w *world) { w.setSelf("n2", false); w.setPeerEpoch(2, "n3") }},
		"an earlier record": {fenced: true, why: "peer n2 leads",
			mut: func(w *world) {
				must(w.t, fenced.WriteNode(w.cfg.Paths(), fenced.Record{Epoch: 3, Leader: "n2", Reason: "peer n2 leads at epoch 3"}))
			}},
	} {
		t.Run(name, func(t *testing.T) {
			w := bootRig(t)
			if tc.mut != nil {
				tc.mut(w)
			}
			res, err := w.orch().BootCheck(w.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if res.Fenced != tc.fenced || (tc.why != "" && !strings.Contains(res.Reason, tc.why)) {
				t.Fatalf("result: %+v", res)
			}
			rec, _ := fenced.Node(w.cfg.Paths())
			stopped := w.has("local.stop")
			if tc.fenced {
				if rec == nil {
					t.Fatal("no record")
				}
				if name != "an earlier record" {
					if !stopped {
						t.Fatal("the clusters that systemd started were not stopped")
					}
					for _, ref := range []string{config.SystemRef, refA, refB} {
						if exists1(units.FilesFor(w.cfg, units.Spec{Service: config.SvcPostgres, Ref: ref}).Run) {
							t.Errorf("%s keeps its launcher", ref)
						}
					}
					if k := w.alertKinds(); len(k) != 1 || k[0] != alerts.KindFenced {
						t.Fatalf("alerts: %v", k)
					}
				}
				return
			}
			if rec != nil || stopped || len(w.alerts) != 0 {
				t.Fatalf("a node that was not replaced changed: record %+v, stopped %v, alerts %v", rec, stopped, w.alerts)
			}
		})
	}
}

// The runtime half of the boot check: a leader that learns of a higher epoch from the membership
// layer fences itself the same way; a follower, or a leader that saw nothing newer, does not.
func TestFenceOnHigherEpochIsTheBootVerdictForARunningLeader(t *testing.T) {
	for name, tc := range map[string]struct {
		follower bool
		epoch    int64
		leader   string
		fenced   bool
	}{
		"a higher epoch":               {epoch: 2, leader: "n2", fenced: true},
		"another leader at this epoch": {epoch: 1, leader: "n2", fenced: true},
		"the same epoch and leader":    {epoch: 1, leader: "n1"},
		"a lower epoch":                {epoch: 0, leader: "n2"},
		"a follower is never replaced": {follower: true, epoch: 5, leader: "n3"},
		"a higher epoch of no leader":  {epoch: 2, fenced: true},
		"the same epoch of no leader":  {epoch: 1},
	} {
		t.Run(name, func(t *testing.T) {
			w := bootRig(t)
			if tc.follower {
				w.setSelf("n2", false)
			}
			done, err := w.orch().FenceOnHigherEpoch(w.ctx, "peer standby", tc.epoch, tc.leader)
			if err != nil || done != tc.fenced {
				t.Fatalf("fenced %v, error %v", done, err)
			}
			rec, _ := fenced.Node(w.cfg.Paths())
			if !tc.fenced {
				if rec != nil || w.has("local.stop") || len(w.alerts) != 0 {
					t.Fatalf("a node that was not replaced changed: %+v", rec)
				}
				return
			}
			if rec == nil || rec.Epoch != tc.epoch || !strings.Contains(rec.Reason, "peer standby says") {
				t.Fatalf("record: %+v", rec)
			}
			for _, ref := range []string{config.SystemRef, refA, refB} {
				if !w.has("local.stop "+ref) || exists1(units.FilesFor(w.cfg, units.Spec{Service: config.SvcPostgres, Ref: ref}).Run) {
					t.Errorf("%s still runs or keeps its launcher", ref)
				}
			}
			if k := w.alertKinds(); len(k) != 1 || k[0] != alerts.KindFenced || !strings.Contains(w.alerts[0].Detail, "While running") {
				t.Fatalf("alerts: %v", w.alerts)
			}
		})
	}
}

// A node that cannot write its fence record (a full disk is likely on a node that is failing) still
// stops its primaries and takes their launchers away.
func TestFenceSelfStopsThePrimariesEvenWhenTheRecordCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	w := bootRig(t)
	root := w.cfg.Paths().Root
	must(t, os.Chmod(root, 0o500))
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	w.setPeerEpoch(2, "n2")
	res, err := w.orch().BootCheck(w.ctx)
	if err == nil || !res.Fenced || !strings.Contains(err.Error(), "recording that this node is fenced") {
		t.Fatalf("result %+v, error %v", res, err)
	}
	for _, ref := range []string{config.SystemRef, refA, refB} {
		if !w.has("local.stop " + ref) {
			t.Errorf("%s was not stopped", ref)
		}
		if exists1(units.FilesFor(w.cfg, units.Spec{Service: config.SvcPostgres, Ref: ref}).Run) {
			t.Errorf("%s keeps its launcher", ref)
		}
	}
	if k := w.alertKinds(); len(k) != 1 || k[0] != alerts.KindFenced {
		t.Fatalf("alerts: %v", k)
	}
}

// setPeerEpoch makes n2 answer pings with the given epoch and leader.
func (w *world) setPeerEpoch(epoch int64, leader string) {
	w.mu.Lock()
	w.pingAs = &peerapi.Ping{Epoch: epoch, Leader: leader, Health: "healthy"}
	w.mu.Unlock()
}

func (w *world) setMarkerEpoch(epoch int64, leader string) {
	w.mu.Lock()
	w.marker = &backupMarker{Epoch: epoch, Leader: leader}
	w.mu.Unlock()
}

// quiesceInFlight starts a quiesce for n2 whose stop of refA does not return until release is closed.
type quiesceAnswer struct {
	code int
	res  QuiesceResult
}

func quiesceInFlight(t *testing.T, w *world, o *Orchestrator) (first chan quiesceAnswer, release chan struct{}) {
	t.Helper()
	inStop := make(chan struct{})
	release = make(chan struct{})
	w.afterEvent("local.stop "+refA, func() { close(inStop); <-release })
	first = askPeer(t, o, PathQuiesce, QuiesceRequest{Epoch: 2, To: "n2"})
	<-inStop
	return first, release
}

func askPeer(t *testing.T, o *Orchestrator, path string, body any) chan quiesceAnswer {
	ch := make(chan quiesceAnswer, 1)
	go func() {
		var a quiesceAnswer
		defer func() { ch <- a }() // also when serve stops the goroutine with t.Fatal
		var out any
		if path == PathQuiesce { // the resume answers with no body
			out = &a.res
		}
		a.code = serve(t, o, "POST "+path, path, "n2", body, out).Code
	}()
	return ch
}

func assertHeldBack(t *testing.T, what string, ch chan quiesceAnswer) {
	t.Helper()
	select {
	case a := <-ch:
		t.Fatalf("%s answered while the quiesce was stopping clusters: %+v", what, a)
	case <-time.After(50 * time.Millisecond):
	}
}

// A quiesce that is stopping clusters holds the move slot: no move of the leader's own runs beside
// it, and a repeated request waits for it and answers from its record instead of stopping again.
func TestARepeatedQuiesceWaitsForTheOneInFlightAndAnswersFromItsRecord(t *testing.T) {
	w := newWorld(t)
	o := w.orch()
	first, release := quiesceInFlight(t, w, o)
	if !o.Busy() {
		t.Fatal("the quiesce does not hold the move slot")
	}
	if _, err := o.FailoverProject(w.ctx, ProjectOptions{Ref: refB, DryRun: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a move beside the quiesce: %v", err)
	}
	again := askPeer(t, o, PathQuiesce, QuiesceRequest{Epoch: 2, To: "n2"})
	assertHeldBack(t, "the repeated quiesce", again)
	close(release)
	if a := <-first; a.code != http.StatusOK || len(a.res.LSNs) != 3 {
		t.Fatalf("first: %+v", a)
	}
	if a := <-again; a.code != http.StatusOK || len(a.res.LSNs) != 3 {
		t.Fatalf("repeat: %+v", a)
	}
	if n := w.count("local.stop system"); n != 1 {
		t.Fatalf("the system cluster was stopped %d times", n)
	}
	if o.Busy() {
		t.Fatal("the slot was not released")
	}
}

// A resume that arrives while the quiesce it undoes is still stopping clusters waits for it: it
// would start clusters the quiesce goes on to stop.
func TestAResumeWaitsForTheQuiesceInFlight(t *testing.T) {
	w := newWorld(t)
	o := w.orch()
	first, release := quiesceInFlight(t, w, o)
	resume := askPeer(t, o, PathResume, struct{}{})
	assertHeldBack(t, "the resume", resume)
	if w.has("local.start") {
		t.Fatalf("the resume started a cluster during the quiesce:\n%v", w.snapshot())
	}
	close(release)
	if a := <-first; a.code != http.StatusOK {
		t.Fatalf("first: %+v", a)
	}
	if a := <-resume; a.code != http.StatusOK {
		t.Fatalf("resume: %+v", a)
	}
	w.assertOrder("local.stop system", "local.start system", "local.start "+refA)
}

// A move of the leader's own that is running turns the quiesce away; the survivor undoes and tries later.
func TestAQuiesceIsRefusedWhileALeaderMoveRuns(t *testing.T) {
	w := newWorld(t)
	o := w.orch()
	rel, err := o.acquire()
	must(t, err)
	defer rel()
	rec := serve(t, o, "POST "+PathQuiesce, PathQuiesce, "n2", QuiesceRequest{Epoch: 2, To: "n2"}, nil)
	if rec.Code != http.StatusConflict || errorOf(rec).Code != "busy" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if w.has("local.stop") || w.has("registry.SetMaintenance") {
		t.Fatalf("a refused quiesce did something:\n%v", w.snapshot())
	}
}
