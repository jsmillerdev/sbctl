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
//	422  the operation is not allowed in the current state    lifecycle.ErrInvalidState
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

// statusOf is the HTTP status the agent answers for err, and the peerapi code of the ones a caller
// can act on.
func statusOf(err error) (int, string) {
	switch {
	case errors.Is(err, cluster.ErrNotLeader):
		return statusNotLeader, "not_leader"
	case errors.Is(err, ErrStaleEpoch):
		return statusStaleEpoch, "stale_epoch"
	case errors.Is(err, ErrNotHome):
		return statusNotHome, "not_home"
	case errors.Is(err, ErrNoRoom):
		return statusNoRoom, "no_capacity"
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, lifecycle.ErrClusterExists):
		return statusClusterExists, "cluster_exists"
	case errors.Is(err, lifecycle.ErrNoRestorableState):
		return statusNoRestorable, "no_restorable_state"
	case errors.Is(err, lifecycle.ErrNoSnapshot):
		return statusNoSnapshot, "no_snapshot"
	case errors.Is(err, lifecycle.ErrReplayBehind):
		return statusReplayBehind, "lag_too_high"
	case errors.Is(err, lifecycle.ErrInvalidState), errors.Is(err, lifecycle.ErrNotStandby), errors.Is(err, lifecycle.ErrNotCleanShutdown):
		return statusInvalidState, "invalid_state"
	}
	return http.StatusInternalServerError, ""
}

// remoteError is an error that came back from another node, carrying the sentinel that its status
// stands for so that errors.Is works on the leader as it did on the node.
type remoteError struct {
	sentinel error
	node     string
	msg      string
}

func (e *remoteError) Error() string { return "node " + e.node + ": " + e.msg }
func (e *remoteError) Unwrap() error { return e.sentinel }

// wrapRemote turns what mesh.RPC.Call returned into the error the caller of lifecycle.Plane expects:
// a *mesh.RemoteError whose status stands for a sentinel is wrapped so that errors.Is finds it.
func wrapRemote(node string, err error) error {
	var re *mesh.RemoteError
	if !errors.As(err, &re) {
		return err
	}
	var sentinel error
	switch re.Status {
	case statusNotLeader:
		sentinel = cluster.ErrNotLeader
	case statusStaleEpoch:
		sentinel = ErrStaleEpoch
	case statusNotHome:
		sentinel = ErrNotHome
	case http.StatusNotFound:
		sentinel = registry.ErrNotFound
	case statusClusterExists:
		sentinel = lifecycle.ErrClusterExists
	case statusNoRestorable:
		sentinel = lifecycle.ErrNoRestorableState
	case statusNoSnapshot:
		sentinel = lifecycle.ErrNoSnapshot
	case statusReplayBehind:
		sentinel = lifecycle.ErrReplayBehind
	case statusNoRoom:
		sentinel = ErrNoRoom
	case statusInvalidState:
		sentinel = lifecycle.ErrInvalidState
	default:
		return err
	}
	return &remoteError{sentinel: sentinel, node: node, msg: re.Message}
}
