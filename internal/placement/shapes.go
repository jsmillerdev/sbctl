package placement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// This file is the one place that says what each method of lifecycle.Plane carries over
// peerapi.PathPlane: the shapes of the arguments after the context and the ref, the shapes of the
// results, and how a Go error travels as a status. RemotePlane encodes them, the node agent's plane
// handler decodes them, and the round-trip test in this package runs every method through both.

// projectArgs are the arguments of the methods that take the project row and its keys. The seed of
// Create is a function and does not travel: a project is created from nothing on its home, and
// remote homes arise only after a switchover (design 2.5).
type projectArgs struct {
	Project *registry.Project    `json:"project"`
	Keys    *secrets.ProjectKeys `json:"keys,omitempty"`
}

// planeMethodName is the peerapi name of a method of lifecycle.Plane: "StartDatabase" is "start_database".
func planeMethodName(goName string) peerapi.PlaneMethod {
	var b strings.Builder
	for i, r := range goName {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return peerapi.PlaneMethod(b.String())
}

// planeFinalCheckpoint is a request on the plane path that is not a method of lifecycle.Plane: the
// state and latest checkpoint of a stopped cluster (lifecycle.ReadControl), which a switchover reads
// after the old primary's fast shutdown to learn the position the new primary must replay to.
const planeFinalCheckpoint peerapi.PlaneMethod = "final_checkpoint"

// The requests below are on the plane path as well, and are not methods of lifecycle.Plane either. They
// reach what the leader's Engine runs for a project that is homed on another node and that is not a plane
// call: the project's nightly base backup timer, which runs where its data is, and the memory and cores
// of the node, which a resume or a resize is judged against.
const (
	planeStartTimer    peerapi.PlaneMethod = "start_timer"
	planeStopTimer     peerapi.PlaneMethod = "stop_timer"
	planeNodeResources peerapi.PlaneMethod = "node_resources"
)

// CheckpointReader reads the control file of a project's cluster on its home node.
type CheckpointReader interface {
	FinalCheckpoint(ctx context.Context, ref string) (lifecycle.ControlInfo, error)
}

// planeCall is one method of lifecycle.Plane as the agent runs it on its local plane.
type planeCall func(ctx context.Context, local lifecycle.Plane, ref string, args json.RawMessage) (any, error)

func decodeProject(ref string, args json.RawMessage, needKeys bool) (*projectArgs, error) {
	var a projectArgs
	if len(args) == 0 {
		return nil, errors.New("the call needs the project")
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("decode the arguments: %w", err)
	}
	if a.Project == nil || a.Project.Ref != ref {
		return nil, fmt.Errorf("the project in the arguments is not %s", ref)
	}
	if needKeys && a.Keys == nil {
		return nil, errors.New("the call needs the project's keys")
	}
	return &a, nil
}

// planeCalls maps every peerapi.PlaneMethod to what the agent does for it. The reflection test
// fails when lifecycle.Plane gains a method that is not here.
var planeCalls = map[peerapi.PlaneMethod]planeCall{
	peerapi.PlaneCreate: func(ctx context.Context, l lifecycle.Plane, ref string, args json.RawMessage) (any, error) {
		a, err := decodeProject(ref, args, true)
		if err != nil {
			return nil, err
		}
		return nil, l.Create(ctx, a.Project, a.Keys, nil)
	},
	peerapi.PlaneDelete: func(ctx context.Context, l lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
		return nil, l.Delete(ctx, ref)
	},
	peerapi.PlaneSnapshot: func(ctx context.Context, l lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
		return l.Snapshot(ctx, ref)
	},
	peerapi.PlaneRoute: func(ctx context.Context, l lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
		return l.Route(ctx, ref)
	},
	peerapi.PlaneUsage: func(ctx context.Context, l lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
		return l.Usage(ctx, ref)
	},
	peerapi.PlaneStart: func(ctx context.Context, l lifecycle.Plane, ref string, args json.RawMessage) (any, error) {
		a, err := decodeProject(ref, args, true)
		if err != nil {
			return nil, err
		}
		return nil, l.Start(ctx, a.Project, a.Keys)
	},
	peerapi.PlaneStartDatabase: func(ctx context.Context, l lifecycle.Plane, ref string, args json.RawMessage) (any, error) {
		a, err := decodeProject(ref, args, true)
		if err != nil {
			return nil, err
		}
		return nil, l.StartDatabase(ctx, a.Project, a.Keys)
	},
	peerapi.PlaneStop: func(ctx context.Context, l lifecycle.Plane, ref string, _ json.RawMessage) (any, error) {
		return nil, l.Stop(ctx, ref)
	},
	peerapi.PlaneReconfigure: func(ctx context.Context, l lifecycle.Plane, ref string, args json.RawMessage) (any, error) {
		a, err := decodeProject(ref, args, true)
		if err != nil {
			return nil, err
		}
		return nil, l.Reconfigure(ctx, a.Project, a.Keys)
	},
	peerapi.PlaneHealth: func(ctx context.Context, l lifecycle.Plane, ref string, args json.RawMessage) (any, error) {
		a, err := decodeProject(ref, args, false)
		if err != nil {
			return nil, err
		}
		return l.Health(ctx, a.Project, a.Keys), nil
	},
}

// Errors cross the peer API as a status and a message (mesh.RemoteError carries both). These are the
// statuses of the errors a caller tells apart; anything else is a 500 whose message the caller shows.
//
//	403  the caller is not the cluster leader                 cluster.ErrNotLeader
//	404  an unknown project or replica                        registry.ErrNotFound
//	409  the caller acts under an epoch older than the node's ErrStaleEpoch
//	410  the project has no restorable state                  lifecycle.ErrNoRestorableState
//	412  the directory holds a cluster already                lifecycle.ErrClusterExists
//	421  the project is not homed on the node                 ErrNotHome
//	422  the operation is not allowed in the current state    lifecycle.ErrInvalidState; the code tells
//	     lifecycle.ErrFenced, ErrNotStandby and ErrNotCleanShutdown apart
//	501  the node has no backup engine                        lifecycle.ErrNoSnapshot
//	504  replay did not reach the position asked              lifecycle.ErrReplayBehind
//	507  the node has no room for the replica                 ErrNoRoom
const (
	statusNotLeader     = http.StatusForbidden
	statusStaleEpoch    = http.StatusConflict
	statusNoRestorable  = http.StatusGone
	statusClusterExists = http.StatusPreconditionFailed
	statusNotHome       = http.StatusMisdirectedRequest
	statusInvalidState  = http.StatusUnprocessableEntity
	statusNoSnapshot    = http.StatusNotImplemented
	statusReplayBehind  = http.StatusGatewayTimeout
	statusNoRoom        = http.StatusInsufficientStorage
)

// Errors the agent answers with and RemotePlane gives back.
var (
	// ErrStaleEpoch: the request carries an epoch lower than the node knows, so its sender is a
	// leader that has been replaced.
	ErrStaleEpoch = errors.New("placement: the request is from an older epoch than the node's")
	// ErrNotHome: the project is not homed on the node that was asked to run it.
	ErrNotHome = errors.New("placement: the project is not homed on this node")
	// ErrNoRoom: the node has no room for the replica, in its memory budget or on its disk. The
	// request is recorded nowhere, so asking again once there is room starts the replica.
	ErrNoRoom = errors.New("placement: the node has no room for the replica")
)

// NoRoomError is the answer of a node that has no room for a replica (status 507), as the leader
// gets it from the peer API. It matches ErrNoRoom, and its NoRoom method says it is a refusal for
// lack of room, which is what the replica controller looks for in an error from InstanceOps.Ensure
// (replicas.RoomError). A refusal that happens on the leader's own node carries the
// *lifecycle.CapacityError of the admission, or an error wrapping lifecycle.ErrReplicaDisk, and
// matches the same way.
type NoRoomError struct {
	Node    string
	Message string
}

func (e *NoRoomError) Error() string        { return "node " + e.Node + ": " + e.Message }
func (e *NoRoomError) Is(target error) bool { return target == ErrNoRoom }

// NoRoom is true: the node refused the replica for lack of room and recorded nothing.
func (e *NoRoomError) NoRoom() bool { return true }

// Codes of the peerapi.Error that tell apart the errors that share a status.
const (
	codeNotLeader        = "not_leader"
	codeStaleEpoch       = "stale_epoch"
	codeNotHome          = "not_home"
	codeNoCapacity       = "no_capacity"
	codeNotFound         = "not_found"
	codeClusterExists    = "cluster_exists"
	codeNoRestorable     = "no_restorable_state"
	codeNoSnapshot       = "no_snapshot"
	codeLagTooHigh       = "lag_too_high"
	codeInvalidState     = "invalid_state"
	codeFenced           = "fenced"
	codeNotStandby       = "not_standby"
	codeNotCleanShutdown = "not_clean_shutdown"
)

// statusOf is the HTTP status the agent answers for err, and the peerapi code of the ones a caller
// can act on.
func statusOf(err error) (int, string) {
	switch {
	case errors.Is(err, cluster.ErrNotLeader):
		return statusNotLeader, codeNotLeader
	case errors.Is(err, ErrStaleEpoch):
		return statusStaleEpoch, codeStaleEpoch
	case errors.Is(err, ErrNotHome):
		return statusNotHome, codeNotHome
	case errors.Is(err, ErrNoRoom):
		return statusNoRoom, codeNoCapacity
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound, codeNotFound
	case errors.Is(err, lifecycle.ErrClusterExists):
		return statusClusterExists, codeClusterExists
	case errors.Is(err, lifecycle.ErrNoRestorableState):
		return statusNoRestorable, codeNoRestorable
	case errors.Is(err, lifecycle.ErrNoSnapshot):
		return statusNoSnapshot, codeNoSnapshot
	case errors.Is(err, lifecycle.ErrReplayBehind):
		return statusReplayBehind, codeLagTooHigh
	case errors.Is(err, lifecycle.ErrFenced):
		return statusInvalidState, codeFenced
	case errors.Is(err, lifecycle.ErrNotStandby):
		return statusInvalidState, codeNotStandby
	case errors.Is(err, lifecycle.ErrNotCleanShutdown):
		return statusInvalidState, codeNotCleanShutdown
	case errors.Is(err, lifecycle.ErrInvalidState):
		return statusInvalidState, codeInvalidState
	}
	return http.StatusInternalServerError, ""
}

// remoteError is an error that came back from another node, carrying the sentinels that its status
// and code stand for so that errors.Is works on the leader as it did on the node.
type remoteError struct {
	sentinels []error
	node      string
	msg       string
}

func (e *remoteError) Error() string   { return "node " + e.node + ": " + e.msg }
func (e *remoteError) Unwrap() []error { return e.sentinels }

// wrapRemote turns what mesh.RPC.Call returned into the error the caller of lifecycle.Plane expects:
// a *mesh.RemoteError whose status stands for a sentinel is wrapped so that errors.Is finds it, and
// a node's refusal for lack of room becomes a *NoRoomError.
func wrapRemote(node string, err error) error {
	var re *mesh.RemoteError
	if !errors.As(err, &re) {
		return err
	}
	var sentinels []error
	switch re.Status {
	case statusNotLeader:
		sentinels = append(sentinels, cluster.ErrNotLeader)
	case statusStaleEpoch:
		sentinels = append(sentinels, ErrStaleEpoch)
	case statusNotHome:
		sentinels = append(sentinels, ErrNotHome)
	case http.StatusNotFound:
		sentinels = append(sentinels, registry.ErrNotFound)
	case statusClusterExists:
		sentinels = append(sentinels, lifecycle.ErrClusterExists)
	case statusNoRestorable:
		sentinels = append(sentinels, lifecycle.ErrNoRestorableState)
	case statusNoSnapshot:
		sentinels = append(sentinels, lifecycle.ErrNoSnapshot)
	case statusReplayBehind:
		sentinels = append(sentinels, lifecycle.ErrReplayBehind)
	case statusNoRoom:
		return &NoRoomError{Node: node, Message: re.Message}
	case statusInvalidState:
		// The code says which of the errors that share the status it was; an answer from a node that
		// sends none is the general one.
		// A fenced node and a cluster that is no standby also match ErrInvalidState, as they always did;
		// a cluster that did not shut down cleanly does not: the node stopped it before it looked.
		switch re.Code {
		case codeFenced:
			sentinels = append(sentinels, lifecycle.ErrFenced, lifecycle.ErrInvalidState)
		case codeNotStandby:
			sentinels = append(sentinels, lifecycle.ErrNotStandby, lifecycle.ErrInvalidState)
		case codeNotCleanShutdown:
			sentinels = append(sentinels, lifecycle.ErrNotCleanShutdown)
		default:
			sentinels = append(sentinels, lifecycle.ErrInvalidState)
		}
	default:
		return err
	}
	return &remoteError{sentinels: sentinels, node: node, msg: re.Message}
}

// refusals are the errors a node answers with when it refuses a request before it changes anything.
var refusals = []error{
	cluster.ErrNotLeader, ErrStaleEpoch, ErrNotHome, ErrNoRoom, registry.ErrNotFound,
	lifecycle.ErrInvalidState, lifecycle.ErrNotStandby, lifecycle.ErrFenced, lifecycle.ErrReplayBehind,
	lifecycle.ErrClusterExists, lifecycle.ErrNoRestorableState, lifecycle.ErrNoSnapshot,
}

// Refused reports whether err is a node's refusal of a request: the caller is not the leader, or acts
// under an older epoch; the project is not homed there, the replica is not there or the node is
// fenced; the state does not allow the action (the cluster is no standby, a standby has not replayed
// as far as asked); the node has no room, no backup engine or nothing to restore. The node refused
// before it changed anything, so the request can be repeated or given up on with the node as it was.
// An error that is not a refusal (a status 500, a timeout, a lost answer, a failure in the middle of
// the work, ErrNotCleanShutdown after the cluster was stopped) says nothing about what the node did:
// the failover orchestrator treats a promotion that ends in one as possibly done. It finds the
// refusal through the wrapping of the peer API (wrapRemote) and of the node's own agent.
func Refused(err error) bool {
	if err == nil {
		return false
	}
	var ce *lifecycle.CapacityError
	if errors.As(err, &ce) {
		return true
	}
	for _, target := range refusals {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
