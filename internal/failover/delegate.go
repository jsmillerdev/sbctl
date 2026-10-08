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

// delegationPatience is how many looks in a row may fail before the leader stops following; the
// move goes on without it. The node that runs the move restarts its daemon when its system cluster
// is promoted and takes a while to answer again, so the patience covers a few minutes.
const delegationPatience = 90

// delegationStall is how many looks in a row may find the move running with no step recorded since the
// one before, about half an hour, before the leader stops following. A move that nothing continues (the
// daemon that was to continue it did not start, or found the node not leading) stays running in its log,
// and a leader that waited for it would never end.
const delegationStall = 900

// Remote starts a server move on another node and follows it. MeshPeers implements it.
type Remote interface {
	StartServer(ctx context.Context, node string, o ServerOptions) error
	// ServerStatus asks for the move of epoch (any when it is 0) that the node runs for the caller.
	ServerStatus(ctx context.Context, node string, from int, epoch int64) (ServerStatus, error)
}

// stateUnknown is the State of a ServerStatus from a node that cannot read the log of the move yet (the
// registry copy of a daemon that has just restarted as a follower): it does not say there is no move.
const stateUnknown = "unknown"

// ServerStatus is the progress of the move a node runs for a delegating leader.
type ServerStatus struct {
	// State is "idle" (nothing was asked), "running", "done", "failed", "aborted" or "unknown" (the log
	// of the move cannot be read yet).
	State string `json:"state"`
	// Steps are the steps recorded since the index that was asked for; Next is the index to ask
	// for next.
	Steps []stepJSON `json:"steps,omitempty"`
	Next  int        `json:"next"`
	Error string     `json:"error,omitempty"`
	Move  *moveJSON  `json:"move,omitempty"`
}

// delegated is a run this node keeps for someone to follow: the switchover it does for a leader that
// asked, or the move it continued after its daemon restarted (restart.go).
type delegated struct {
	// by is the node that follows it.
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

func (m MeshPeers) ServerStatus(ctx context.Context, node string, from int, epoch int64) (ServerStatus, error) {
	var s ServerStatus
	err := m.RPC.Call(ctx, node, http.MethodGet, PathServer+"?from="+strconv.Itoa(from)+"&epoch="+strconv.FormatInt(epoch, 10), nil, &s)
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
	// The slot this move holds is lent to the node that runs the switchover: its quiesce of this node
	// (handleQuiesce) arrives while the move is waiting for it.
	defer o.delegate(run.to.ID)()
	if err := remote.StartServer(ctx, run.to.ID, opts); err != nil {
		var re *mesh.RemoteError
		// The refusal is a 409 with the code "refused" whose text is the RefusedError.
		if errors.As(err, &re) && re.Status == http.StatusConflict && (re.Code == "refused" || strings.HasPrefix(re.Message, "failover: refused")) {
			return nil, fmt.Errorf("%w by %s: %s", ErrRefused, run.to.Name, re.Message)
		}
		return nil, fmt.Errorf("failover: asking %s to take over: %w", run.to.Name, err)
	}
	reportStep(ctx, registry.MoveStep{Name: "delegated", At: o.d.Now().UTC(), Detail: run.to.Name + " runs the switchover"})

	// The node that runs the move restarts its daemon when it promotes its system cluster, and the
	// daemon that starts continues the move. While it is down, or has not picked the move up yet, the
	// looks fail, find nothing or cannot read the log; that is waited out, but only after the node was
	// seen running it.
	next, misses, stalled, seen := 0, 0, 0, false
	for {
		st, err := remote.ServerStatus(ctx, run.to.ID, next, run.epoch)
		switch {
		case err != nil || st.State == "idle" && seen || st.State == stateUnknown:
			if misses++; misses >= delegationPatience {
				cause := err
				switch {
				case cause != nil:
				case st.State == stateUnknown:
					cause = fmt.Errorf("it cannot read the log of the move: %s", st.Error)
				default:
					cause = errors.New("it reports no move")
				}
				return nil, fmt.Errorf("failover: lost contact with %s, which goes on with the switchover; follow it there with supavise failover --resume or in the moves log: %w", run.to.Name, cause)
			}
		default:
			misses, seen = 0, true
			for _, s := range st.Steps {
				reportStep(ctx, registry.MoveStep{Name: s.Name, At: s.At, Detail: s.Detail})
			}
			if len(st.Steps) > 0 {
				stalled = 0
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
			if stalled++; stalled >= delegationStall {
				return nil, fmt.Errorf("failover: %s runs the switchover and has recorded no step for a long time; it may have stopped. This command stops following it: supavise status shows where the cluster leads, and supavise failover --resume on %s continues a move that stopped", run.to.Name, run.to.Name)
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
	// The node's move is taken before the answer, so that two requests cannot both pass, and the
	// run below is the one that holds it.
	release, err := o.acquire()
	if err != nil {
		writePeerError(w, http.StatusConflict, "busy", ErrBusy.Error())
		return
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
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
	run := o.keepRun(peer.Node)
	started = true
	go func() {
		defer release()
		run.record(context.Background(), func(ctx context.Context) (*registry.Move, error) { return o.failoverServer(ctx, opts) })
	}()
	writePeerJSON(w, http.StatusAccepted, nil)
}

// keepRun starts the record of a run that node follows, in place of the one before it.
func (o *Orchestrator) keepRun(by string) *delegated {
	run := &delegated{state: "running", by: by}
	o.delegMu.Lock()
	o.deleg = run
	o.delegMu.Unlock()
	return run
}

// record runs do under base with each step it records, and its outcome, kept in the run. A move that
// the daemon's restart cut (ErrRestarting) has no outcome yet: it is running in its log, the daemon that
// starts continues it, and whoever follows it keeps waiting. Calling it failed would have the leader that
// asked for the switchover report an error for a move that goes on.
func (d *delegated) record(base context.Context, do func(ctx context.Context) (*registry.Move, error)) {
	ctx := WithProgress(base, func(s registry.MoveStep) {
		d.mu.Lock()
		d.steps = append(d.steps, stepJSON{Name: s.Name, At: s.At, Detail: s.Detail})
		d.mu.Unlock()
	})
	mv, err := do(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.move = toMoveJSON(mv)
	switch {
	case err == nil:
		d.state = "done"
	case errors.Is(err, ErrRestarting):
		d.state = "running"
	case mv != nil && mv.State == registry.MoveAborted:
		d.state, d.err = "aborted", err.Error()
	default:
		d.state, d.err = "failed", err.Error()
	}
}

// Follow reports a server move for someone who lost the connection that ran it: the run this node
// keeps (a switchover it runs for a leader, or the move it continued after its daemon restarted),
// else the move of that epoch in failover.json or the moves table, which any caller that continued it
// writes (the daemon's wiring continues one too) and which a node that restarted as a follower has
// from the new leader's log. A node whose own copy of the log cannot say (it cannot be read yet, or has
// not caught up) asks the node that leads, which keeps the move. It gives the steps from index from on.
// The control socket serves it to the CLI, whose daemon restarted in the middle of the move.
func (o *Orchestrator) Follow(ctx context.Context, from int, epoch int64) ServerStatus {
	o.delegMu.Lock()
	run := o.deleg
	o.delegMu.Unlock()
	if run != nil {
		// A run that ended is the one of its own epoch; a caller asking about another is not shown it.
		if st := run.status(from); epoch <= 0 || st.Move == nil || st.Move.Epoch == epoch {
			return st
		}
	}
	if epoch <= 0 {
		return ServerStatus{State: "idle"}
	}
	st, ok := o.loggedStatus(ctx, from, func(_, _ string, e int64) bool { return e == epoch })
	if ok && st.State != stateUnknown {
		return st
	}
	if led, found := o.leaderStatus(ctx, from, epoch); found {
		return led
	}
	if ok {
		return st
	}
	return ServerStatus{State: "idle"}
}

// leaderLook bounds the question to the node that leads; a node that does not answer is not waited for.
const leaderLook = 10 * time.Second

// leaderStatus asks the node that leads for the move of epoch. It is for a node that follows: the old
// leader of a switchover restarts as a follower, and the move is in the leader's memory and log before
// it is in the copy of the registry this node has.
func (o *Orchestrator) leaderStatus(ctx context.Context, from int, epoch int64) (ServerStatus, bool) {
	remote, ok := o.d.Peers.(Remote)
	if !ok || o.d.Members.IsLeader() {
		return ServerStatus{}, false
	}
	lead, ok := o.d.Members.Leader()
	if !ok || lead.ID == o.self().ID {
		return ServerStatus{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, leaderLook)
	defer cancel()
	st, err := remote.ServerStatus(ctx, lead.ID, from, epoch)
	if err != nil || st.State == "" || st.State == "idle" || st.State == stateUnknown {
		return ServerStatus{}, false
	}
	return st, true
}

// loggedStatus reports the newest server move that match accepts, from failover.json (the log of a
// move until its system cluster is promoted) or else the moves table. A moves table that cannot be read
// is stateUnknown, not the absence of a move.
func (o *Orchestrator) loggedStatus(ctx context.Context, from int, match func(fromNode, toNode string, epoch int64) bool) (ServerStatus, bool) {
	if fs, err := readStateFile(o.d.Cfg.Paths().FailoverState()); err == nil && fs != nil && match(fs.From, fs.To, fs.Epoch) {
		state := fs.State
		if state == "" {
			state = registry.MoveRunning
		}
		mv := registry.Move{Scope: registry.MoveServer, Kind: fs.Kind, FromNode: fs.From, ToNode: fs.To, Epoch: fs.Epoch, State: state, Error: fs.Error, Steps: fs.Steps}
		return statusOfMove(mv, from), true
	}
	ms, err := o.store().ListMoves(ctx, "", 50)
	if err != nil {
		return ServerStatus{State: stateUnknown, Error: err.Error()}, true
	}
	for _, m := range ms {
		if m.Scope == registry.MoveServer && match(m.FromNode, m.ToNode, m.Epoch) {
			return statusOfMove(m, from), true
		}
	}
	return ServerStatus{}, false
}

// statusOfMove is a logged move as a ServerStatus, with the steps from index from on.
func statusOfMove(m registry.Move, from int) ServerStatus {
	from = max(0, min(from, len(m.Steps)))
	st := ServerStatus{State: string(m.State), Next: len(m.Steps), Error: m.Error, Move: toMoveJSON(&m)}
	for _, s := range m.Steps[from:] {
		st.Steps = append(st.Steps, stepJSON{Name: s.Name, At: s.At, Detail: s.Detail})
	}
	if m.State == registry.MoveFailed && st.Error == "" {
		st.Error = "the move stopped"
	}
	return st
}

// handleServerStatus tells the leader that asked how the move goes. The epoch in the query is the one
// the leader started the move at, so that a finished move of the same two nodes from before is not
// taken for it.
func (o *Orchestrator) handleServerStatus(w http.ResponseWriter, r *http.Request) {
	peer, ok := caller(w, r)
	if !ok {
		return
	}
	o.delegMu.Lock()
	run := o.deleg
	o.delegMu.Unlock()
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	epoch, _ := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
	if run != nil && run.by != peer.Node && run.by != "" {
		writePeerError(w, http.StatusForbidden, "forbidden", "the move runs for another node")
		return
	}
	if run != nil {
		if st := run.status(from); epoch <= 0 || st.Move == nil || st.Move.Epoch == epoch {
			writePeerJSON(w, http.StatusOK, st)
			return
		}
	}
	// A daemon that restarted keeps no run in memory; the move it continues is in its log, and the node
	// that asked for it is the one it moves the leadership away from.
	self := o.self().ID
	match := func(fromNode, toNode string, e int64) bool {
		return fromNode == peer.Node && toNode == self && (epoch <= 0 || e == epoch)
	}
	if st, ok := o.loggedStatus(r.Context(), from, match); ok {
		writePeerJSON(w, http.StatusOK, st)
		return
	}
	writePeerJSON(w, http.StatusOK, ServerStatus{State: "idle"})
}
