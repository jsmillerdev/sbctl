package mesh

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// Topology is what the mesh needs to know about the cluster: who the nodes are, which one leads,
// and which one this is. cluster.Membership answers it.
type Topology interface {
	Self() registry.Node
	Nodes() []registry.Node
	Leader() (n registry.Node, ok bool)
	IsLeader() bool
}

// Source is the part of the registry the mesh reads: where each project lives and where its
// replicas are. On a follower it is the replicated copy.
type Source interface {
	GetProject(ctx context.Context, ref string) (*registry.Project, error)
	ListProjects(ctx context.Context) ([]registry.Project, error)
	ListReplicas(ctx context.Context, ref string) ([]registry.Replica, error)
	Subscribe(ctx context.Context) (<-chan registry.Change, error)
}

// Refusal is why the far end of a forward stream was not given a port. The stream is closed
// without a byte whatever the reason; the reason goes to the log of the node that refused.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, a ...any) error { return &Refusal{Reason: fmt.Sprintf(format, a...)} }

// Authorizer decides, from this node's own copy of the registry, whether a forward stream may be
// opened and to which loopback port. It is the whole of the policy: there is no open relay. A
// stream is refused unless the caller is a cluster member in a state that may forward, the ref is
// a project this node knows, and the registry says the service runs here.
type Authorizer struct {
	Cfg      *config.Config
	Topology Topology
	Source   Source
	Now      func() time.Time

	cache  sync.Map // ref -> *placement, kept for placementTTL
	stored atomic.Int64
	flight singleflight.Group // the registry reads in progress, by ref
}

// placementTTL is how long a project's home and replicas are remembered. A forward stream costs
// a registry read otherwise, and a project's pooler or PostgREST opens one per connection. A move
// is seen after at most this long, which the retry of the client that was refused covers.
const placementTTL = time.Second

type placement struct {
	until    time.Time
	project  registry.Project
	replicas []registry.Replica
	// haveReplicas says that replicas was read: only the replica kinds need them, so a placement
	// made for another kind has none.
	haveReplicas bool
	err          error
}

func (a *Authorizer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// placeTimeout bounds the registry read for a placement. The read is shared by everyone who asks for
// the same ref while it runs, so it does not belong to the stream that started it.
const placeTimeout = 15 * time.Second

// place returns where ref lives. withReplicas asks for its replicas as well, which only the replica
// kinds need. Concurrent calls for a ref that is not remembered share one registry read.
func (a *Authorizer) place(ctx context.Context, ref string, withReplicas bool) (*placement, error) {
	if v, ok := a.cache.Load(ref); ok {
		if p := v.(*placement); a.now().Before(p.until) && (p.haveReplicas || !withReplicas || p.err != nil) {
			return p, p.err
		}
	}
	key := ref
	if withReplicas {
		key += "+replicas"
	}
	ch := a.flight.DoChan(key, func() (any, error) {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), placeTimeout)
		defer cancel()
		return a.load(lctx, ref, withReplicas)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		p := res.Val.(*placement)
		return p, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// load reads ref from the registry and remembers the answer. A registry error is returned and not
// remembered: a hiccup is not a verdict.
func (a *Authorizer) load(ctx context.Context, ref string, withReplicas bool) (*placement, error) {
	p := &placement{until: a.now().Add(placementTTL)}
	proj, err := a.Source.GetProject(ctx, ref)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		p.err = refuse("unknown project %s", ref)
	case err != nil:
		return nil, err
	default:
		p.project = *proj
		if withReplicas {
			if p.replicas, err = a.Source.ListReplicas(ctx, ref); err != nil {
				return nil, err
			}
			p.haveReplicas = true
		}
	}
	a.cache.Store(ref, p)
	if a.stored.Add(1)%sweepEvery == 0 {
		a.sweep()
	}
	return p, nil
}

// sweepEvery is how many stored placements pass between looks for expired ones: an admitted peer that
// sends streams for refs that do not exist would otherwise grow the cache without limit.
const sweepEvery = 256

// sweep drops the placements that have expired.
func (a *Authorizer) sweep() {
	now := a.now()
	a.cache.Range(func(k, v any) bool {
		if !now.Before(v.(*placement).until) {
			a.cache.Delete(k)
		}
		return true
	})
}

// Resolve returns the loopback port that a forward stream with header h, opened by peer, reaches
// on this node.
func (a *Authorizer) Resolve(ctx context.Context, peer Peer, h Header) (int, error) {
	if peer.Node == "" {
		return 0, refuse("the caller presented no certificate")
	}
	if err := h.Validate(); err != nil {
		return 0, refuse("%v", err)
	}
	// A joining node streams its first copy of the system cluster and nothing else; a fenced node
	// is not served at all.
	switch peer.State {
	case registry.NodeActive:
	case registry.NodeJoining:
		if h.Kind != KindPostgres || h.Ref != config.SystemRef {
			return 0, refuse("node %s is joining: it may stream the system cluster and nothing else", peer.Node)
		}
	default:
		return 0, refuse("node %s is %s", peer.Node, peer.State)
	}
	self := a.Topology.Self().ID
	seq := 0
	switch {
	case h.Kind.IsService():
		if !a.Topology.IsLeader() {
			return 0, refuse("%s runs on the leader, and node %s is not the leader", h.Kind, self)
		}
	default:
		p, err := a.place(ctx, h.Ref, h.Kind != KindPostgres && h.Kind != KindGoTrue && h.Kind != KindPostgREST)
		if err != nil {
			return 0, err
		}
		seq = p.project.Seq
		switch h.Kind {
		case KindPostgres, KindGoTrue, KindPostgREST:
			if p.project.NodeID != self {
				return 0, refuse("project %s is homed on %s, not on %s", h.Ref, p.project.NodeID, self)
			}
		default: // the replica kinds
			if !hasReplicaOn(p.replicas, self) {
				return 0, refuse("project %s has no replica on %s", h.Ref, self)
			}
		}
	}
	port, err := LocalPort(a.Cfg, h.Kind, h.Ref, seq)
	if err != nil {
		return 0, refuse("%v", err)
	}
	return port, nil
}

func hasReplicaOn(rs []registry.Replica, node string) bool {
	for _, r := range rs {
		if r.NodeID == node {
			return true
		}
	}
	return false
}

// rpcAllowed is the request policy of the peer server: which endpoints a caller may reach given
// its state. A caller with no certificate reaches the join endpoint; a joining node confirms its
// join (and asks again to rejoin, when an earlier rejoin stopped); a fenced node hears the ping, the
// fence and the rejoin; an active node reaches every endpoint. An endpoint that only the leader may
// call checks the caller in its handler (RequireLeader is the shared check). The endpoints other
// workstreams register are not listed because they are for active nodes.
func rpcAllowed(p Peer, path string) (ok bool, why string) {
	switch {
	case p.Node == "":
		if path == peerapi.PathJoin {
			return true, ""
		}
		return false, "the caller presented no certificate"
	case p.State == registry.NodeActive:
		return true, ""
	case p.State == registry.NodeJoining:
		if path == peerapi.PathPing || path == peerapi.PathJoinConfirm || path == peerapi.PathRejoin {
			return true, ""
		}
	case p.State == registry.NodeFenced:
		if path == peerapi.PathPing || path == peerapi.PathFence || path == peerapi.PathRejoin {
			return true, ""
		}
	}
	return false, fmt.Sprintf("node %s is %s and may not call %s", p.Node, p.State, path)
}
