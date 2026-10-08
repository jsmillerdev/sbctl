package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

// The peer endpoints of the failover work, and the clients that call them. peerapi holds the
// endpoints every workstream shares; these belong to this package and register themselves on the
// mesh with Orchestrator.PeerHandlers.
//
//	POST /peer/v1/fence                          any node to a node: the cooperative fence (peerapi.PathFence)
//	POST /peer/v1/failover/quiesce               the survivor to the leader: stop everything for a planned switchover
//	POST /peer/v1/failover/resume                the survivor to the leader: undo a quiesce
//	POST /peer/v1/failover/primary/{ref}/{op}    the leader to a project's home: stop, start, health or aside
//	POST|GET /peer/v1/failover/server            the leader to the node that takes over: run a switchover, and tell how it goes
const (
	PathQuiesce = "/peer/v1/failover/quiesce"
	PathResume  = "/peer/v1/failover/resume"
	PathPrimary = "/peer/v1/failover/primary/{ref}/{op}"
)

// Operations of PathPrimary.
const (
	OpStop   = "stop"
	OpStart  = "start"
	OpHealth = "health"
	OpAside  = "aside"
)

// PrimaryPath fills in PathPrimary.
func PrimaryPath(ref, op string) string {
	return "/peer/v1/failover/primary/" + url.PathEscape(ref) + "/" + url.PathEscape(op)
}

// FenceCall is the body of the cooperative fence: a peerapi.FenceRequest, and optionally the one
// project whose primary is to be fenced. Without a ref the node stops acting as a primary at all.
// A project fence runs in the cluster's current epoch (a project move does not change the epoch);
// a node fence needs a higher one.
type FenceCall struct {
	peerapi.FenceRequest
	Ref string `json:"ref,omitempty"`
}

// QuiesceRequest asks the leader to stop for a planned switchover to To, which will hold Epoch.
type QuiesceRequest struct {
	Epoch  int64  `json:"epoch"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

// QuiesceResult is what the leader stopped: the shutdown checkpoint of each cluster by ref
// ("system" included). The survivor's standbys replay to these before they promote.
type QuiesceResult struct {
	LSNs map[string]string `json:"lsns"`
}

// PrimaryCall is the body of PathPrimary.
type PrimaryCall struct {
	Epoch int64 `json:"epoch"`
}

// PrimaryResult is the answer of PathPrimary.
type PrimaryResult struct {
	LSN     string `json:"lsn,omitempty"`
	Healthy bool   `json:"healthy,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// maintenanceTTL is how long a planned switchover keeps automatic failover quiet. A move that
// dies leaves the announcement to expire.
const maintenanceTTL = time.Hour

// maintenanceReason is the text of the announcement a planned switchover to node makes.
func maintenanceReason(to string) string { return "switchover to " + to }

// quiesceParallel bounds how many project clusters a planned switchover stops at once.
const quiesceParallel = 8

// MeshPeers implements Peers and Leader over the mesh.
type MeshPeers struct{ RPC mesh.RPC }

var (
	_ Peers  = MeshPeers{}
	_ Leader = MeshPeers{}
)

func (m MeshPeers) Ping(ctx context.Context, node string) (peerapi.Ping, error) {
	var p peerapi.Ping
	err := m.RPC.Call(ctx, node, http.MethodGet, peerapi.PathPing, nil, &p)
	return p, err
}

func (m MeshPeers) Fence(ctx context.Context, node string, req FenceCall) (peerapi.FenceResponse, error) {
	var r peerapi.FenceResponse
	err := m.RPC.Call(ctx, node, http.MethodPost, peerapi.PathFence, req, &r)
	return r, err
}

func (m MeshPeers) Quiesce(ctx context.Context, node string, req QuiesceRequest) (QuiesceResult, error) {
	var r QuiesceResult
	err := m.RPC.Call(ctx, node, http.MethodPost, PathQuiesce, req, &r)
	return r, err
}

func (m MeshPeers) Resume(ctx context.Context, node string) error {
	return m.RPC.Call(ctx, node, http.MethodPost, PathResume, struct{}{}, nil)
}

// MeshPrimaries implements Primaries: on this node through Local, on another through the peer
// endpoint of that node.
type MeshPrimaries struct {
	Self  func() string
	Local LocalPrimaries
	RPC   mesh.RPC
	// Epoch is the cluster epoch the calls are made in.
	Epoch func() int64
}

var _ Primaries = MeshPrimaries{}

func (m MeshPrimaries) call(ctx context.Context, node, ref, op string) (PrimaryResult, error) {
	var r PrimaryResult
	err := m.RPC.Call(ctx, node, http.MethodPost, PrimaryPath(ref, op), PrimaryCall{Epoch: m.Epoch()}, &r)
	return r, err
}

func (m MeshPrimaries) Stop(ctx context.Context, node, ref string) (string, error) {
	if node == m.Self() {
		return m.Local.Stop(ctx, ref)
	}
	r, err := m.call(ctx, node, ref, OpStop)
	return r.LSN, err
}

func (m MeshPrimaries) Start(ctx context.Context, node, ref string) error {
	if node == m.Self() {
		return m.Local.Start(ctx, ref)
	}
	_, err := m.call(ctx, node, ref, OpStart)
	return err
}

func (m MeshPrimaries) Healthy(ctx context.Context, node, ref string) (bool, string, error) {
	if node == m.Self() {
		return m.Local.Healthy(ctx, ref)
	}
	r, err := m.call(ctx, node, ref, OpHealth)
	return r.Healthy, r.Detail, err
}

func (m MeshPrimaries) SetAside(ctx context.Context, node, ref string, epoch int64) error {
	if node == m.Self() {
		return m.Local.SetAside(ctx, ref, epoch)
	}
	var r PrimaryResult
	return m.RPC.Call(ctx, node, http.MethodPost, PrimaryPath(ref, OpAside), PrimaryCall{Epoch: epoch}, &r)
}

// PeerHandlers are the handlers of this package's peer endpoints, by net/http pattern. The
// daemon registers them with mesh.Handle before the peer server starts.
func (o *Orchestrator) PeerHandlers() map[string]mesh.HandlerFunc {
	return map[string]mesh.HandlerFunc{
		"POST " + peerapi.PathFence: o.handleFence,
		"POST " + PathQuiesce:       o.handleQuiesce,
		"POST " + PathResume:        o.handleResume,
		"POST " + PathPrimary:       o.handlePrimary,
		"POST " + PathServer:        o.handleServerStart,
		"GET " + PathServer:         o.handleServerStatus,
	}
}

func writePeerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writePeerError(w http.ResponseWriter, status int, code, msg string) {
	writePeerJSON(w, status, peerapi.Error{Message: msg, Code: code})
}

func decodePeer(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writePeerError(w, http.StatusBadRequest, "bad_request", "the request body is not valid: "+err.Error())
		return false
	}
	return true
}

// caller returns the authenticated peer, or answers 401.
func caller(w http.ResponseWriter, r *http.Request) (mesh.Peer, bool) {
	p, ok := mesh.PeerFrom(r.Context())
	if !ok || p.Node == "" {
		writePeerError(w, http.StatusUnauthorized, "unauthenticated", "the request carries no node certificate")
		return mesh.Peer{}, false
	}
	return p, true
}

// callerLeads answers 403 unless the peer is the node this one believes is the leader.
func (o *Orchestrator) callerLeads(w http.ResponseWriter, p mesh.Peer) bool {
	if l, ok := o.d.Members.Leader(); !ok || l.ID != p.Node {
		writePeerError(w, http.StatusForbidden, "not_leader", "only the leader may ask for this")
		return false
	}
	return true
}

// handleFence is the cooperative fence (design 2.10.6). A node that is told, by the node that
// will lead, that a higher epoch exists stops acting as a primary: it records the fence first (so
// that nothing restarts a primary behind its back), removes the launcher of each primary, which
// the unit's ConditionPathExists turns into a hard stop that survives a reboot, and stops them.
func (o *Orchestrator) handleFence(w http.ResponseWriter, r *http.Request) {
	var req FenceCall
	if !decodePeer(w, r, &req) {
		return
	}
	peer, ok := caller(w, r)
	if !ok {
		return
	}
	if req.Leader == "" || req.Leader != peer.Node {
		writePeerError(w, http.StatusForbidden, "forbidden", "a node asks for a fence only in its own name")
		return
	}
	if o.d.LocalPrimaries == nil {
		writePeerError(w, http.StatusNotImplemented, "unsupported", "this node cannot stop a primary")
		return
	}
	local := o.d.Members.Epoch()
	paths := o.d.Cfg.Paths()
	// The stops go on when the caller gives up: the fence is recorded and the launchers are gone by then.
	ctx := context.WithoutCancel(r.Context())
	if req.Ref != "" {
		if !fenced.ValidRef(req.Ref) {
			writePeerError(w, http.StatusBadRequest, "bad_request", "not a project ref: "+req.Ref)
			return
		}
		// A project fence runs in the current epoch, and only the leader asks for it.
		if req.Epoch < local {
			writePeerJSON(w, http.StatusOK, peerapi.FenceResponse{Epoch: local})
			return
		}
		if !o.callerLeads(w, peer) {
			return
		}
		resp, err := o.fenceProjectHere(ctx, req)
		if err != nil {
			writePeerError(w, http.StatusInternalServerError, "fence_failed", err.Error())
			return
		}
		writePeerJSON(w, http.StatusOK, resp)
		return
	}
	if req.Epoch <= local {
		already := false
		if rec, err := fenced.Node(paths); err == nil && rec != nil && rec.Epoch >= req.Epoch {
			already = true
		}
		writePeerJSON(w, http.StatusOK, peerapi.FenceResponse{Epoch: local, Fenced: already})
		return
	}
	refs := o.primaryRefs(ctx)
	stopped, err := o.fencePrimaries(ctx, refs, func() error {
		return fenced.WriteNode(paths, fenced.Record{Epoch: req.Epoch, Leader: req.Leader, Reason: reasonOf(req), At: o.d.Now().UTC()})
	})
	if err != nil {
		writePeerError(w, http.StatusInternalServerError, "fence_failed", err.Error())
		return
	}
	o.alert(ctx, alerts.Event{
		Kind: alerts.KindFenced, Severity: alerts.SeverityCritical, Title: "This node was fenced",
		Detail: fmt.Sprintf("Node %s leads at epoch %d and asked this node to stop acting as a primary. No cluster starts as a primary here until `supavise node rejoin` rebuilds it as a follower.", req.Leader, req.Epoch),
		Key:    "fenced",
	})
	writePeerJSON(w, http.StatusOK, peerapi.FenceResponse{Epoch: req.Epoch, Fenced: true, Stopped: stopped})
}

// fenceProjectHere records the fence of one project's primary on this node, then removes its
// launcher and stops it. The leader calls it for a project that is homed on the leader itself; a
// node cannot ask itself over the mesh.
func (o *Orchestrator) fenceProjectHere(ctx context.Context, req FenceCall) (peerapi.FenceResponse, error) {
	if o.d.LocalPrimaries == nil {
		return peerapi.FenceResponse{}, errors.New("this node cannot stop a primary")
	}
	stopped, err := o.fencePrimaries(ctx, []string{req.Ref}, func() error {
		return fenced.WriteProject(o.d.Cfg.Paths(), fenced.Record{Epoch: req.Epoch, Leader: req.Leader, Ref: req.Ref, Reason: reasonOf(req), At: o.d.Now().UTC()})
	})
	if err != nil {
		return peerapi.FenceResponse{}, err
	}
	return peerapi.FenceResponse{Epoch: o.d.Members.Epoch(), Fenced: true, Stopped: stopped}, nil
}

func reasonOf(req FenceCall) string {
	if req.Reason != "" {
		return req.Reason
	}
	return fmt.Sprintf("node %s leads at epoch %d", req.Leader, req.Epoch)
}

// fencePrimaries records the fence, then for each ref removes the launcher and stops the
// primary. It stops at nothing: every ref is tried, and the first error is returned after.
func (o *Orchestrator) fencePrimaries(ctx context.Context, refs []string, record func() error) ([]string, error) {
	if err := record(); err != nil {
		return nil, fmt.Errorf("recording the fence: %w", err)
	}
	var stopped []string
	var first error
	for _, ref := range refs {
		if err := removeLauncher(o.d.Cfg, ref); err != nil && first == nil {
			first = err
		}
		if _, err := o.d.LocalPrimaries.Stop(ctx, ref); err != nil {
			if first == nil {
				first = fmt.Errorf("stopping %s: %w", ref, err)
			}
			continue
		}
		stopped = append(stopped, ref)
	}
	return stopped, first
}

// removeLauncher deletes <ref>/postgres.run, which supavise-postgres@<ref> needs to exist
// (ConditionPathExists): without it systemd will not start the cluster, not even at boot.
func removeLauncher(cfg *config.Config, ref string) error {
	run := units.FilesFor(cfg, units.Spec{Service: config.SvcPostgres, Ref: ref}).Run
	if err := os.Remove(run); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", run, err)
	}
	return nil
}

// primaryRefs are the refs whose primary may run on this node: the projects the registry homes
// here, and the system project when this node leads. When the registry cannot be read (the
// system cluster is down, which is the state of a node that was fenced from outside) every
// directory with a launcher counts, replicas included: a fenced node serves nothing.
func (o *Orchestrator) primaryRefs(ctx context.Context) []string {
	self := o.self().ID
	var refs []string
	if ps, err := o.store().ListProjects(ctx); err == nil {
		for _, p := range ps {
			if p.NodeID == self || (p.Ref == config.SystemRef && o.d.Members.IsLeader()) {
				refs = append(refs, p.Ref)
			}
		}
		sort.Strings(refs)
		return refs
	}
	entries, err := os.ReadDir(filepath.Join(o.d.Cfg.Paths().Root, "projects"))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		run := units.FilesFor(o.d.Cfg, units.Spec{Service: config.SvcPostgres, Ref: e.Name()}).Run
		if _, err := os.Stat(run); err == nil {
			refs = append(refs, e.Name())
		}
	}
	return refs
}

// handleQuiesce stops the leader for a planned switchover (design 2.10.4 step 2).
func (o *Orchestrator) handleQuiesce(w http.ResponseWriter, r *http.Request) {
	var req QuiesceRequest
	if !decodePeer(w, r, &req) {
		return
	}
	peer, ok := caller(w, r)
	if !ok {
		return
	}
	if !o.d.Members.IsLeader() {
		writePeerError(w, http.StatusConflict, "not_leader", "this node is not the leader")
		return
	}
	if peer.Node != req.To {
		writePeerError(w, http.StatusForbidden, "forbidden", "only the node that takes over may ask for this")
		return
	}
	if want := o.d.Members.Epoch() + 1; req.Epoch != want {
		writePeerError(w, http.StatusConflict, "stale_epoch", fmt.Sprintf("a switchover from epoch %d runs at epoch %d, not %d", want-1, want, req.Epoch))
		return
	}
	if o.d.LocalPrimaries == nil {
		writePeerError(w, http.StatusNotImplemented, "unsupported", "this node cannot stop a primary")
		return
	}
	// One quiesce at a time, and not beside a move of this node's own: a second request waits for
	// the first and answers from its record, and a project move that started during the stops would
	// stop or start the same clusters. Only the quiesce a switchover is waiting for holds the slot;
	// the leader refuses to start another move until it has finished. A switchover that this node
	// started with --to holds the slot itself while the survivor runs it, and the survivor's
	// quiesce is that move continuing: it passes (acquireForDelegate).
	o.quiesceMu.Lock()
	defer o.quiesceMu.Unlock()
	release, err := o.acquireForDelegate(peer.Node)
	if err != nil {
		writePeerError(w, http.StatusConflict, "busy", "a move is running on the leader")
		return
	}
	defer release()
	// A quiesce is not undone by a caller that gives up: the record is what a retry and the undo go by.
	rec, err := o.currentQuiesce()
	switch {
	case err != nil:
		writePeerError(w, http.StatusInternalServerError, "quiesce_failed", err.Error())
		return
	case rec != nil && !rec.matches(req):
		writePeerError(w, http.StatusConflict, "quiesce_pending", fmt.Sprintf("a switchover to %s at epoch %d is waiting; its survivor continues or undoes it", rec.To, rec.Epoch))
		return
	}
	res, err := o.quiesceLocal(context.WithoutCancel(r.Context()), req, rec)
	if err != nil {
		writePeerError(w, http.StatusInternalServerError, "quiesce_failed", err.Error())
		return
	}
	writePeerJSON(w, http.StatusOK, res)
}

// quiesceLocal enters maintenance, then stops the project clusters (in parallel, Realtime and the
// pooler let go first), the shared services and last the system cluster. The daemon, and the WAL
// relay in it, keeps running: a cluster that cannot archive its last segments cannot finish
// its shutdown, and the survivor replays the archive up to these positions.
//
// rec is the record of an earlier try of the same request, nil at the first one. The stops are safe
// to repeat (a stopped cluster answers with its checkpoint), and a try that finds the record
// complete answers from it, because the registry it would list the projects from is stopped.
func (o *Orchestrator) quiesceLocal(ctx context.Context, req QuiesceRequest, rec *quiesceRecord) (QuiesceResult, error) {
	var err error
	switch {
	case rec == nil:
		if rec, err = o.startQuiesce(ctx, req); err != nil {
			return QuiesceResult{}, err
		}
	case rec.LSNs != nil:
		return QuiesceResult{LSNs: rec.LSNs}, nil
	case !rec.Announced:
		m := registry.Maintenance{Node: o.self().ID, Until: o.d.Now().Add(maintenanceTTL), Reason: rec.Reason}
		if err := o.store().SetMaintenance(ctx, m); err != nil {
			return QuiesceResult{}, fmt.Errorf("announcing maintenance: %w", err)
		}
	}
	res := QuiesceResult{LSNs: map[string]string{}}
	var mu sync.Mutex
	var g errgroup.Group
	g.SetLimit(quiesceParallel)
	for _, ref := range rec.Refs {
		g.Go(func() error {
			o.quiesceTenant(ctx, ref)
			lsn, err := o.d.LocalPrimaries.Stop(ctx, ref)
			if err != nil {
				return fmt.Errorf("stopping %s: %w", ref, err)
			}
			mu.Lock()
			res.LSNs[ref] = lsn
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return QuiesceResult{}, err
	}
	if o.d.LocalServices != nil {
		if err := o.d.LocalServices.Stop(ctx); err != nil {
			return QuiesceResult{}, fmt.Errorf("stopping the shared services: %w", err)
		}
	}
	lsn, err := o.d.LocalPrimaries.Stop(ctx, config.SystemRef)
	if err != nil {
		return QuiesceResult{}, fmt.Errorf("stopping the system cluster: %w", err)
	}
	res.LSNs[config.SystemRef] = lsn
	rec.LSNs = res.LSNs
	if err := o.saveQuiesce(rec); err != nil {
		return QuiesceResult{}, fmt.Errorf("recording where each cluster stopped: %w", err)
	}
	return res, nil
}

// stopPrimary stops one project's primary the way a planned stop needs: Realtime and the pooler
// let go of the database first (without that Realtime's logical walsender holds the shutdown
// until it times out), then the project's units stop and the cluster shuts down. It returns the
// shutdown checkpoint. The daemon, and the WAL relay in it, is not touched: the cluster archives
// its last segments through the relay while it shuts down.
func (o *Orchestrator) stopPrimary(ctx context.Context, node, ref string) (string, error) {
	o.quiesceTenant(ctx, ref)
	return o.d.Primaries.Stop(ctx, node, ref)
}

// quiesceTenant makes the shared services let go of the project's database. A failure is
// logged: the stop may then be slow, and it still works.
func (o *Orchestrator) quiesceTenant(ctx context.Context, ref string) {
	if o.d.Fleet == nil {
		return
	}
	if err := o.d.Fleet.QuiesceTenant(ctx, ref); err != nil {
		o.d.Log.Warn("could not quiesce the shared services before the stop", "ref", ref, "error", err)
	}
}

// handleResume undoes a quiesce: the survivor aborted before it fenced or promoted anything.
func (o *Orchestrator) handleResume(w http.ResponseWriter, r *http.Request) {
	peer, ok := caller(w, r)
	if !ok {
		return
	}
	if o.d.LocalPrimaries == nil {
		writePeerError(w, http.StatusNotImplemented, "unsupported", "this node cannot start a primary")
		return
	}
	// A quiesce that is still stopping clusters finishes first; its record is what this undo reads.
	o.quiesceMu.Lock()
	defer o.quiesceMu.Unlock()
	// Only the node a quiesce was made for may undo it. The record is a file: the registry is the
	// system cluster this very quiesce stopped.
	rec, err := o.currentQuiesce()
	if err != nil {
		writePeerError(w, http.StatusInternalServerError, "resume_failed", err.Error())
		return
	}
	if rec == nil || rec.To != peer.Node {
		writePeerError(w, http.StatusConflict, "no_quiesce", "no switchover to that node is waiting")
		return
	}
	if err := o.resumeLocal(context.WithoutCancel(r.Context())); err != nil {
		writePeerError(w, http.StatusInternalServerError, "resume_failed", err.Error())
		return
	}
	o.clearQuiesce()
	writePeerJSON(w, http.StatusOK, nil)
}

// resumeLocal starts the system cluster, then the shared services, then the projects that run, in
// the order Quiesce stopped them backwards, and ends the maintenance announcement.
func (o *Orchestrator) resumeLocal(ctx context.Context) error {
	if err := o.d.LocalPrimaries.Start(ctx, config.SystemRef); err != nil {
		return fmt.Errorf("starting the system cluster: %w", err)
	}
	st := o.store()
	var ps []registry.Project
	var err error
	for range 30 { // the registry answers when the system cluster has started
		if ps, err = st.ListProjects(ctx); err == nil {
			break
		}
		if werr := o.wait(ctx, 2*time.Second); werr != nil {
			return werr
		}
	}
	if err != nil {
		return fmt.Errorf("reading the registry after the system cluster started: %w", err)
	}
	if o.d.LocalServices != nil {
		if err := o.d.LocalServices.Start(ctx); err != nil {
			return fmt.Errorf("starting the shared services: %w", err)
		}
	}
	self := o.self().ID
	var firstErr error
	for _, p := range ps {
		if p.Ref == config.SystemRef || p.NodeID != self || p.Status == registry.StatusInactive {
			continue
		}
		if err := o.d.LocalPrimaries.Start(ctx, p.Ref); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("starting %s: %w", p.Ref, err)
			continue
		}
		if o.d.Fleet != nil {
			if err := o.d.Fleet.EnsureTenant(ctx, p.Ref); err != nil {
				o.d.Log.Warn("could not register the project with the shared services again", "ref", p.Ref, "error", err)
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return st.SetMaintenance(ctx, registry.Maintenance{})
}

// handlePrimary serves the operations on one project's primary that the leader asks of its home.
func (o *Orchestrator) handlePrimary(w http.ResponseWriter, r *http.Request) {
	ref, op := r.PathValue("ref"), r.PathValue("op")
	var req PrimaryCall
	if !decodePeer(w, r, &req) {
		return
	}
	peer, ok := caller(w, r)
	if !ok {
		return
	}
	if !o.callerLeads(w, peer) {
		return
	}
	if !fenced.ValidRef(ref) {
		writePeerError(w, http.StatusBadRequest, "bad_request", "not a project ref: "+ref)
		return
	}
	if local := o.d.Members.Epoch(); req.Epoch < local {
		writePeerError(w, http.StatusConflict, "stale_epoch", fmt.Sprintf("the node is at epoch %d, the request at %d", local, req.Epoch))
		return
	}
	if o.d.LocalPrimaries == nil {
		writePeerError(w, http.StatusNotImplemented, "unsupported", "this node cannot run a primary")
		return
	}
	ctx := r.Context()
	var res PrimaryResult
	var err error
	switch op {
	case OpStop:
		res.LSN, err = o.d.LocalPrimaries.Stop(ctx, ref)
	case OpStart:
		err = o.d.LocalPrimaries.Start(ctx, ref)
	case OpHealth:
		res.Healthy, res.Detail, err = o.d.LocalPrimaries.Healthy(ctx, ref)
	case OpAside:
		// Data moves aside only for a project the registry homes elsewhere: the leader homes the
		// project on the replica's node before it asks, and one this node still homes is live.
		if p, gerr := o.store().GetProject(ctx, ref); gerr == nil && p.NodeID == o.self().ID {
			writePeerError(w, http.StatusConflict, "homed_here", "the registry homes "+ref+" on this node: its data is not set aside")
			return
		}
		err = o.d.LocalPrimaries.SetAside(ctx, ref, req.Epoch)
	default:
		writePeerError(w, http.StatusNotFound, "unknown_op", "no such operation: "+op)
		return
	}
	if err != nil {
		writePeerError(w, http.StatusInternalServerError, "primary_failed", err.Error())
		return
	}
	writePeerJSON(w, http.StatusOK, res)
}
