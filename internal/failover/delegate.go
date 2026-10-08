package failover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/registry"
)

// A planned switchover can be started on the leader with --to: the leader cannot run it, because
// its own registry stops partway and a move needs the one that the survivor promotes. It asks the
// node that takes over to run the move (StartServer), then follows the steps that node records
// (ServerStatus) and reports them as its own, so the operator sees one run. The move itself, and
// failover.json, belong to the node that takes over; --resume goes there.

// PathServer is the delegation endpoint, leader to the node that takes over. POST starts the move
// (a serverReq in, 202 out, or 409 with the failed checks); GET answers a ServerStatus, with the
// steps from index ?from=N on.
const PathServer = "/peer/v1/failover/server"

// delegationPoll is how often the leader looks at the node that runs the move.
const delegationPoll = 2 * time.Second

// delegationPatience is how many looks in a row may fail before the leader stops following. The
// move goes on without it.
const delegationPatience = 15

// Remote starts a server move on another node and follows it. MeshPeers implements it.
type Remote interface {
	StartServer(ctx context.Context, node string, o ServerOptions) error
	ServerStatus(ctx context.Context, node string, from int) (ServerStatus, error)
}

// ServerStatus is the progress of the move a node runs for a delegating leader.
type ServerStatus struct {
	// State is "idle" (nothing was asked), "running", "done", "failed" or "aborted".
	State string `json:"state"`
	// Steps are the steps recorded since the index that was asked for; Next is the index to ask
	// for next.
	Steps []stepJSON `json:"steps,omitempty"`
	Next  int        `json:"next"`
	Error string     `json:"error,omitempty"`
	Move  *moveJSON  `json:"move,omitempty"`
}

// delegated is the run this node does for a leader.
type delegated struct {
	// by is the node that asked.
	by    string
	mu    sync.Mutex
	state string
	steps []stepJSON
	err   string
	move  *moveJSON
}

func (d *delegated) status(from int) ServerStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == "" {
		return ServerStatus{State: "idle"}
	}
	from = max(0, min(from, len(d.steps)))
	return ServerStatus{State: d.state, Steps: append([]stepJSON(nil), d.steps[from:]...), Next: len(d.steps), Error: d.err, Move: d.move}
}

func (m MeshPeers) StartServer(ctx context.Context, node string, o ServerOptions) error {
	return m.RPC.Call(ctx, node, http.MethodPost, PathServer, serverFromOptions(o), nil)
}

func (m MeshPeers) ServerStatus(ctx context.Context, node string, from int) (ServerStatus, error) {
	var s ServerStatus
	err := m.RPC.Call(ctx, node, http.MethodGet, PathServer+"?from="+strconv.Itoa(from), nil, &s)
	return s, err
}

// delegateServer asks the node that takes over to run the move and follows it.
func (o *Orchestrator) delegateServer(ctx context.Context, opts ServerOptions, run *serverRun) (*registry.Move, error) {
	switch {
	case opts.Resume:
		return nil, fmt.Errorf("failover: a move that stopped continues on the node that runs it: run supavise failover --resume on %s", run.to.Name)
	case !o.d.Members.IsLeader():
		return nil, fmt.Errorf("failover: a server move runs on the node that takes over: run it on %s", run.to.Name)
	case !run.planned:
		return nil, fmt.Errorf("failover: a failover runs on the node that takes over: run it on %s", run.to.Name)
	}
	remote, ok := o.d.Peers.(Remote)
	if !ok {
		return nil, fmt.Errorf("failover: this node cannot ask %s to take over: run it on %s", run.to.Name, run.to.Name)
	}
	if err := remote.StartServer(ctx, run.to.ID, opts); err != nil {
		var re *mesh.RemoteError
		// The refusal is a 409 whose text is the RefusedError (the peer API's code does not
		// reach this release's RemoteError).
		if errors.As(err, &re) && re.Status == http.StatusConflict && strings.HasPrefix(re.Message, "failover: refused") {
			return nil, fmt.Errorf("%w by %s: %s", ErrRefused, run.to.Name, re.Message)
		}
		return nil, fmt.Errorf("failover: asking %s to take over: %w", run.to.Name, err)
	}
	reportStep(ctx, registry.MoveStep{Name: "delegated", At: o.d.Now().UTC(), Detail: run.to.Name + " runs the switchover"})

	next, misses := 0, 0
	for {
		st, err := remote.ServerStatus(ctx, run.to.ID, next)
		if err != nil {
			if misses++; misses >= delegationPatience {
				return nil, fmt.Errorf("failover: lost contact with %s, which goes on with the switchover; follow it there with supavise failover --resume or in the moves log: %w", run.to.Name, err)
			}
		} else {
			misses = 0
			for _, s := range st.Steps {
				reportStep(ctx, registry.MoveStep{Name: s.Name, At: s.At, Detail: s.Detail})
			}
			next = st.Next
			switch st.State {
			case "done":
				return st.Move.move(), nil
			case "failed", "aborted":
				return st.Move.move(), errors.New(st.Error)
			case "idle":
				return nil, fmt.Errorf("failover: %s does not run the switchover (it restarted?); run it there", run.to.Name)
			}
		}
		if err := o.wait(ctx, delegationPoll); err != nil {
			return nil, err
		}
	}
}

// handleServerStart runs a switchover for the leader that asked: the preconditions are checked
// here, where the move runs, and a refusal is answered at once; the move then runs on a goroutine
// of the daemon, and handleServerStatus tells how it goes.
func (o *Orchestrator) handleServerStart(w http.ResponseWriter, r *http.Request) {
	var req serverReq
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
	opts := req.options()
	opts.To, opts.DryRun, opts.Resume = o.self().ID, false, false
	if o.Busy() {
		writePeerError(w, http.StatusConflict, "busy", ErrBusy.Error())
		return
	}
	pl, err := o.PlanServer(r.Context(), opts)
	if err != nil {
		writePeerError(w, http.StatusInternalServerError, "plan_failed", err.Error())
		return
	}
	if refused := pl.Refused(opts.Force); len(refused) > 0 {
		writePeerError(w, http.StatusConflict, "refused", (&RefusedError{Checks: refused}).Error())
		return
	}
	if pl.Kind != string(registry.MoveSwitchover) {
		writePeerError(w, http.StatusConflict, "not_planned", "the old leader does not answer here: a failover is run on this node, not asked of it")
		return
	}
	run := &delegated{state: "running", by: peer.Node}
	o.delegMu.Lock()
	o.deleg = run
	o.delegMu.Unlock()
	go func() {
		ctx := WithProgress(context.Background(), func(s registry.MoveStep) {
			run.mu.Lock()
			run.steps = append(run.steps, stepJSON{Name: s.Name, At: s.At, Detail: s.Detail})
			run.mu.Unlock()
		})
		mv, err := o.FailoverServer(ctx, opts)
		run.mu.Lock()
		defer run.mu.Unlock()
		run.move = toMoveJSON(mv)
		switch {
		case err == nil:
			run.state = "done"
		case mv != nil && mv.State == registry.MoveAborted:
			run.state, run.err = "aborted", err.Error()
		default:
			run.state, run.err = "failed", err.Error()
		}
	}()
	writePeerJSON(w, http.StatusAccepted, nil)
}

// handleServerStatus tells the leader that asked how the move goes.
func (o *Orchestrator) handleServerStatus(w http.ResponseWriter, r *http.Request) {
	peer, ok := caller(w, r)
	if !ok {
		return
	}
	o.delegMu.Lock()
	run := o.deleg
	o.delegMu.Unlock()
	if run == nil {
		writePeerJSON(w, http.StatusOK, ServerStatus{State: "idle"})
		return
	}
	if run.by != peer.Node {
		writePeerError(w, http.StatusForbidden, "forbidden", "the move runs for another node")
		return
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	writePeerJSON(w, http.StatusOK, run.status(from))
}
