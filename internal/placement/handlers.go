package placement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// LocalBackups is what the backup handlers ask of the node's backup service for a project homed here.
type LocalBackups interface {
	// BaseBackupWith takes a base backup of ref's running cluster (backup.Service).
	BaseBackupWith(ctx context.Context, ref string, bo backup.BackupOptions) (*registry.Backup, error)
	// RestoreInPlace restores ref's database over itself (backup.Service).
	RestoreInPlace(ctx context.Context, ref string, req lifecycle.RestoreRequest) error
}

// HandlerDeps are what the peer API handlers of this package need.
type HandlerDeps struct {
	Agent Agent
	Plane lifecycle.Plane // the node's own plane, for PathPlane
	// Checkpoints reads the control file of a cluster of this node (*lifecycle.PostgresPlane); nil
	// answers 501 to the final_checkpoint request.
	Checkpoints interface {
		FinalCheckpoint(ref string) (lifecycle.ControlInfo, error)
	}
	// Resolver and Members are required: the handlers admit the leader only, check the epoch and that
	// the project is homed here against them.
	Resolver Resolver
	Members  cluster.Membership
	Backups  LocalBackups
	// Timers starts and stops the backup timer of a project homed here; nil does nothing (the exec
	// backend has none).
	Timers lifecycle.Timers
	// Resources reads the memory and cores of this node; nil answers that they are unknown.
	Resources func() lifecycle.NodeResources
}

// Register registers the instance, plane and backup endpoints of the peer API with handle, which is
// mesh.Handle in the daemon and a mesh.Mux's Handle in tests. It refuses a HandlerDeps with no Agent,
// Members or Resolver: handlers that cannot tell the leader from a peer, or the home from another
// node, would serve every request.
func Register(handle func(pattern string, fn mesh.HandlerFunc), d HandlerDeps) error {
	if d.Agent == nil || d.Members == nil || d.Resolver == nil {
		return errors.New("placement: the peer API handlers need the agent, the cluster membership and the resolver")
	}
	h := &handlers{d}
	// The endpoints change what runs on the node: they admit the cluster leader only, before they read the request.
	handle("PUT "+peerapi.PathInstance, h.leaderOnly(h.ensure))
	handle("GET "+peerapi.PathInstance, h.leaderOnly(h.observe))
	handle("DELETE "+peerapi.PathInstance, h.leaderOnly(h.remove))
	handle("POST "+peerapi.PathInstanceAction, h.leaderOnly(h.action))
	handle("POST "+peerapi.PathPlane, h.leaderOnly(h.plane))
	handle("POST "+peerapi.PathBackup, h.leaderOnly(h.backup))
	return nil
}

// queryEpoch is the query parameter that carries the cluster epoch of a request that has no body
// (DELETE of an instance).
const queryEpoch = "epoch"

type handlers struct{ d HandlerDeps }

// maxBody bounds a request body.
const maxBody = 4 << 20

// writeErr answers err with the status that stands for it.
func writeErr(w http.ResponseWriter, err error) {
	status, code := statusOf(err)
	mesh.RespondError(w, status, code, err.Error())
}

func badRequest(w http.ResponseWriter, format string, args ...any) {
	mesh.RespondError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...))
}

// decode reads the JSON body of a request into v. An empty body leaves v as it is: it is not an error
// here, where mesh.DecodeBodyMax answers 400 for it.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		badRequest(w, "read the request: %v", err)
		return false
	}
	if len(b) == 0 {
		return true
	}
	if err := json.Unmarshal(b, v); err != nil {
		badRequest(w, "decode the request: %v", err)
		return false
	}
	return true
}

// authorize admits the cluster leader only: the endpoints here change what runs on the node. The
// refusal names who asked and who leads, which the leader's log and the remote error carry.
func (h *handlers) authorize(r *http.Request) error {
	peer, ok := mesh.PeerFrom(r.Context())
	if !ok || peer.Node == "" {
		return fmt.Errorf("%w: the request carries no node certificate", cluster.ErrNotLeader)
	}
	leader, ok := h.d.Members.Leader()
	if !ok || leader.ID != peer.Node {
		return fmt.Errorf("%w: %s asked, the leader is %q", cluster.ErrNotLeader, peer.Node, leader.ID)
	}
	return nil
}

// leaderOnly runs fn for the leader's requests and answers every other with writeErr, before fn reads
// the request. (mesh.RequireLeader answers with a generic message; the one here is what callers have
// always got.)
func (h *handlers) leaderOnly(fn mesh.HandlerFunc) mesh.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h.authorize(r); err != nil {
			writeErr(w, err)
			return
		}
		fn(w, r)
	}
}

func (h *handlers) epochOK(epoch int64) error {
	if cur := h.d.Members.Epoch(); epoch < cur {
		return fmt.Errorf("%w: the request is under epoch %d and this node is at %d", ErrStaleEpoch, epoch, cur)
	}
	return nil
}

func (h *handlers) ensure(w http.ResponseWriter, r *http.Request) {
	var spec peerapi.InstanceSpec
	if !decode(w, r, &spec) {
		return
	}
	if id := r.PathValue("identifier"); spec.Identifier == "" {
		spec.Identifier = id
	} else if spec.Identifier != id {
		badRequest(w, "the body names %s and the path %s", spec.Identifier, id)
		return
	}
	st, err := h.d.Agent.Ensure(r.Context(), spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	mesh.RespondJSON(w, http.StatusOK, st)
}

func (h *handlers) observe(w http.ResponseWriter, r *http.Request) {
	st, err := h.d.Agent.Observe(r.Context(), r.PathValue("identifier"))
	if err != nil {
		writeErr(w, err)
		return
	}
	mesh.RespondJSON(w, http.StatusOK, st)
}

func (h *handlers) remove(w http.ResponseWriter, r *http.Request) {
	// The removal deletes a directory: it is made under the epoch of the leader that asks, like every
	// other request that changes the node, so that a leader that was replaced and does not know it yet
	// cannot destroy what the new one keeps. (A request without an epoch is a leader of an older
	// release, which only the leader check covers.)
	if q := r.URL.Query().Get(queryEpoch); q != "" {
		epoch, err := strconv.ParseInt(q, 10, 64)
		if err != nil || epoch < 0 {
			badRequest(w, "the epoch %q is not a cluster epoch", q)
			return
		}
		if err := h.epochOK(epoch); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := h.d.Agent.Remove(r.Context(), r.PathValue("identifier")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) action(w http.ResponseWriter, r *http.Request) {
	var req peerapi.InstanceAction
	if !decode(w, r, &req) {
		return
	}
	st, err := h.d.Agent.Do(r.Context(), r.PathValue("identifier"), peerapi.Action(r.PathValue("action")), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	mesh.RespondJSON(w, http.StatusOK, st)
}

// home checks that the project is homed on this node: the plane and backup endpoints run what only
// the home can.
func (h *handlers) home(ctx context.Context, ref string) error {
	home, err := h.d.Resolver.HomeOf(ctx, ref)
	if err != nil {
		return err
	}
	if self := h.d.Members.Self().ID; home != self {
		return fmt.Errorf("%w: %s is homed on %s and this is %s", ErrNotHome, ref, home, self)
	}
	return nil
}

func (h *handlers) plane(w http.ResponseWriter, r *http.Request) {
	ref, method := r.PathValue("ref"), peerapi.PlaneMethod(r.PathValue("method"))
	call, ok := h.planeCall(method)
	if !ok {
		badRequest(w, "unknown plane method %q", method)
		return
	}
	var pc peerapi.PlaneCall
	if !decode(w, r, &pc) {
		return
	}
	if err := h.epochOK(pc.Epoch); err != nil {
		writeErr(w, err)
		return
	}
	// The node's copy of the registry can trail the leader's by a moment; a call that arrives before
	// the row that makes this node the home is refused, and the leader asks again.
	if err := h.home(r.Context(), ref); err != nil {
		writeErr(w, err)
		return
	}
	res, err := call(r.Context(), h.d.Plane, ref, pc.Args)
	if err != nil {
		writeErr(w, err)
		return
	}
	var out peerapi.PlaneResult
	if res != nil {
		b, err := json.Marshal(res)
		if err != nil {
			writeErr(w, err)
			return
		}
		out.Result = b
	}
	mesh.RespondJSON(w, http.StatusOK, out)
}

// planeCall is what the agent does for method: a method of lifecycle.Plane (planeCalls), or one of the
// requests of this package that share the path.
func (h *handlers) planeCall(method peerapi.PlaneMethod) (planeCall, bool) {
	switch method {
	case planeFinalCheckpoint:
		return h.finalCheckpoint, true
	case planeStartTimer:
		return func(ctx context.Context, _ lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
			if h.d.Timers == nil {
				return nil, nil
			}
			return nil, h.d.Timers.StartTimer(ctx, ref)
		}, true
	case planeStopTimer:
		return func(ctx context.Context, _ lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
			if h.d.Timers == nil {
				return nil, nil
			}
			return nil, h.d.Timers.StopTimer(ctx, ref)
		}, true
	case planeNodeResources:
		return func(context.Context, lifecycle.Plane, string, json.RawMessage) (any, error) {
			if h.d.Resources == nil {
				return lifecycle.NodeResources{}, nil
			}
			return h.d.Resources(), nil
		}, true
	}
	call, ok := planeCalls[method]
	return call, ok
}

// finalCheckpoint is the plane call that reads a stopped cluster's control file.
func (h *handlers) finalCheckpoint(_ context.Context, _ lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
	if h.d.Checkpoints == nil {
		return nil, lifecycle.ErrNoSnapshot
	}
	return h.d.Checkpoints.FinalCheckpoint(ref)
}

func (h *handlers) backup(w http.ResponseWriter, r *http.Request) {
	var req peerapi.BackupRequest
	if !decode(w, r, &req) {
		return
	}
	if err := h.epochOK(req.Epoch); err != nil {
		writeErr(w, err)
		return
	}
	ref := r.PathValue("ref")
	if err := h.home(r.Context(), ref); err != nil {
		writeErr(w, err)
		return
	}
	if h.d.Backups == nil {
		writeErr(w, lifecycle.ErrNoSnapshot)
		return
	}
	op := peerapi.BackupOp(r.PathValue("op"))
	if op != peerapi.BackupBase && op != peerapi.BackupRestore {
		badRequest(w, "unknown backup operation %q", op)
		return
	}
	// This node's registry is a copy the leader writes: the leader records the backup (RecordBase).
	res, err := runBackup(r.Context(), h.d.Backups, ref, op, req, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	mesh.RespondJSON(w, http.StatusOK, res)
}

// RunBackup performs the backup operation op for ref with the node's backup service: a base backup,
// or an in-place restore. It is the leader's own backup, which records itself in the registry.
func RunBackup(ctx context.Context, b LocalBackups, ref string, op peerapi.BackupOp, req peerapi.BackupRequest) (peerapi.BackupResult, error) {
	return runBackup(ctx, b, ref, op, req, true)
}

// runBackup is RunBackup; record says whether a base backup writes the registry. The backup a
// follower takes for the leader does not: its registry is a read-only copy.
func runBackup(ctx context.Context, b LocalBackups, ref string, op peerapi.BackupOp, req peerapi.BackupRequest, record bool) (peerapi.BackupResult, error) {
	switch op {
	case peerapi.BackupBase:
		bo := backup.BackupOptions{Reason: req.Reason, NoRecord: !record}
		rec, err := b.BaseBackupWith(ctx, ref, bo)
		if err != nil {
			return peerapi.BackupResult{}, err
		}
		return peerapi.BackupResult{ID: path.Base(strings.TrimRight(rec.Location, "/")), Timeline: rec.Timeline,
			StartLSN: rec.StartLSN, StopLSN: rec.StopLSN, SizeBytes: rec.SizeBytes}, nil
	case peerapi.BackupRestore:
		rr := lifecycle.RestoreRequest{BackupID: req.BackupID}
		if req.Target != nil {
			rr.Target = *req.Target
		}
		if err := b.RestoreInPlace(ctx, ref, rr); err != nil {
			return peerapi.BackupResult{}, err
		}
		return peerapi.BackupResult{ID: req.BackupID}, nil
	}
	return peerapi.BackupResult{}, fmt.Errorf("placement: unknown backup operation %q", op)
}
