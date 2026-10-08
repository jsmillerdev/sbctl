package placement

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// RouterOptions configure a Router.
type RouterOptions struct {
	// Local is the node's own plane, which serves the projects homed here.
	Local lifecycle.FullPlane
	// Resolver says where each project is homed.
	Resolver Resolver
	// Self is the id of this node.
	Self func() string
	// RPC reaches the other nodes' agents.
	RPC mesh.RPC
	// Epoch is the cluster epoch remote calls are made under.
	Epoch func() int64
}

// Router is the lifecycle.Plane the leader's Engine drives: each call goes to the plane of the
// project's home. For a project homed on this node that is the node's own plane; for any other it
// is a RemotePlane that runs the call on that node's agent. A project's home is the node_id of its
// registry row, so a project that has just moved is routed to its new home from the next call.
//
// The Engine also finds optional capabilities of its plane by type assertion (lifecycle.FullPlane
// lists them); a Router has them all, and runs each on the local plane. A project that is homed on
// another node answers lifecycle.ErrNotSupported for them: its settings are applied on its home.
type Router struct {
	opts    RouterOptions
	mu      sync.Mutex
	remotes map[string]*RemotePlane
}

var (
	_ PlaneRouter          = (*Router)(nil)
	_ lifecycle.FullPlane  = (*Router)(nil)
	_ lifecycle.HomeRouter = (*Router)(nil)
	_ CheckpointReader     = (*Router)(nil)
)

// NewRouter builds the router.
func NewRouter(o RouterOptions) *Router {
	return &Router{opts: o, remotes: map[string]*RemotePlane{}}
}

// RoutesByHome implements lifecycle.HomeRouter: an Engine that drives a Router may act on a project
// homed on another node.
func (r *Router) RoutesByHome() {}

// Local implements PlaneRouter.
func (r *Router) Local() lifecycle.Plane { return r.opts.Local }

// For implements PlaneRouter.
func (r *Router) For(ctx context.Context, ref string) (lifecycle.Plane, error) {
	home, err := r.opts.Resolver.HomeOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	return r.forNode(home), nil
}

// forNode is the plane of node: the local one for this node (or for no node at all, a project row
// from before the cluster migration), a remote one otherwise.
func (r *Router) forNode(node string) lifecycle.Plane {
	if node == "" || node == r.opts.Self() {
		return r.opts.Local
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rp, ok := r.remotes[node]
	if !ok {
		rp = &RemotePlane{Node: node, RPC: r.opts.RPC, Epoch: r.opts.Epoch}
		r.remotes[node] = rp
	}
	return rp
}

// forProject routes by the row the caller holds, which is the freshest account of the home.
func (r *Router) forProject(ctx context.Context, p *registry.Project) (lifecycle.Plane, error) {
	if p.NodeID != "" {
		return r.forNode(p.NodeID), nil
	}
	return r.For(ctx, p.Ref)
}

// forCleanup routes a call that must also work on a ref the registry no longer knows (a delete that
// is finishing, a stop after the row went): such leftovers can only be on this node.
func (r *Router) forCleanup(ctx context.Context, ref string) (lifecycle.Plane, error) {
	pl, err := r.For(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return r.opts.Local, nil
	}
	return pl, err
}

func (r *Router) Create(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, seed lifecycle.DataSeeder) error {
	pl, err := r.forProject(ctx, p)
	if err != nil {
		return err
	}
	return pl.Create(ctx, p, keys, seed)
}

func (r *Router) Delete(ctx context.Context, ref string) error {
	pl, err := r.forCleanup(ctx, ref)
	if err != nil {
		return err
	}
	return pl.Delete(ctx, ref)
}

func (r *Router) Snapshot(ctx context.Context, ref string) (*registry.Backup, error) {
	pl, err := r.For(ctx, ref)
	if err != nil {
		return nil, err
	}
	return pl.Snapshot(ctx, ref)
}

// Route answers from the local plane for every project: a project's canonical ports are the same on
// every node, the service itself on its home and a forwarder to it elsewhere (invariant I3).
func (r *Router) Route(ctx context.Context, ref string) (lifecycle.Upstreams, error) {
	return r.opts.Local.Route(ctx, ref)
}

func (r *Router) Usage(ctx context.Context, ref string) (lifecycle.Usage, error) {
	pl, err := r.For(ctx, ref)
	if err != nil {
		return lifecycle.Usage{}, err
	}
	return pl.Usage(ctx, ref)
}

func (r *Router) Start(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	pl, err := r.forProject(ctx, p)
	if err != nil {
		return err
	}
	return pl.Start(ctx, p, keys)
}

func (r *Router) StartDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	pl, err := r.forProject(ctx, p)
	if err != nil {
		return err
	}
	return pl.StartDatabase(ctx, p, keys)
}

func (r *Router) Stop(ctx context.Context, ref string) error {
	pl, err := r.forCleanup(ctx, ref)
	if err != nil {
		return err
	}
	return pl.Stop(ctx, ref)
}

func (r *Router) Reconfigure(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	pl, err := r.forProject(ctx, p)
	if err != nil {
		return err
	}
	return pl.Reconfigure(ctx, p, keys)
}

func (r *Router) Health(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) []lifecycle.ServiceHealth {
	pl, err := r.forProject(ctx, p)
	if err != nil {
		return []lifecycle.ServiceHealth{{Name: "placement", Status: "UNHEALTHY", Error: err.Error()}}
	}
	return pl.Health(ctx, p, keys)
}

// FinalCheckpoint implements CheckpointReader: the control file of ref's cluster, read on its home.
func (r *Router) FinalCheckpoint(ctx context.Context, ref string) (lifecycle.ControlInfo, error) {
	pl, err := r.For(ctx, ref)
	if err != nil {
		return lifecycle.ControlInfo{}, err
	}
	if cr, ok := pl.(CheckpointReader); ok {
		return cr.FinalCheckpoint(ctx, ref)
	}
	if lr, ok := pl.(interface {
		FinalCheckpoint(ref string) (lifecycle.ControlInfo, error)
	}); ok {
		return lr.FinalCheckpoint(ref)
	}
	return lifecycle.ControlInfo{}, fmt.Errorf("%w: the plane cannot read a control file", lifecycle.ErrNotSupported)
}

// local returns the node's own plane when p is homed here, and otherwise the error an optional
// capability answers for a project that runs elsewhere.
func (r *Router) local(ctx context.Context, p *registry.Project, what string) (lifecycle.FullPlane, error) {
	node := p.NodeID
	if node == "" {
		var err error
		if node, err = r.opts.Resolver.HomeOf(ctx, p.Ref); err != nil {
			return nil, err
		}
	}
	if node != "" && node != r.opts.Self() {
		return nil, fmt.Errorf("%w: %s of %s is applied on node %s, its home", lifecycle.ErrNotSupported, what, p.Ref, node)
	}
	return r.opts.Local, nil
}

func (r *Router) ReconfigureService(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, svc string) error {
	l, err := r.local(ctx, p, "reconfiguring "+svc)
	if err != nil {
		return err
	}
	return l.ReconfigureService(ctx, p, keys, svc)
}

func (r *Router) ApplyPostgresSettings(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, restart bool, beforeRestart func(context.Context)) (bool, error) {
	l, err := r.local(ctx, p, "applying Postgres settings")
	if err != nil {
		return false, err
	}
	return l.ApplyPostgresSettings(ctx, p, keys, restart, beforeRestart)
}

func (r *Router) SetRolePassword(ctx context.Context, p *registry.Project, role, password string) error {
	l, err := r.local(ctx, p, "setting a role password")
	if err != nil {
		return err
	}
	return l.SetRolePassword(ctx, p, role, password)
}

func (r *Router) SetRolePasswords(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	l, err := r.local(ctx, p, "setting the role passwords")
	if err != nil {
		return err
	}
	return l.SetRolePasswords(ctx, p, keys)
}

func (r *Router) RecoverPostgres(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	l, err := r.local(ctx, p, "recovering Postgres")
	if err != nil {
		return err
	}
	return l.RecoverPostgres(ctx, p, keys)
}

func (r *Router) CheckRender(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, svc projectconfig.Service) error {
	l, err := r.local(ctx, p, "checking a render")
	if err != nil {
		return err
	}
	return l.CheckRender(ctx, p, keys, svc)
}

func (r *Router) Extensions(ctx context.Context, p *registry.Project) ([]lifecycle.InstalledExtension, error) {
	l, err := r.local(ctx, p, "listing extensions")
	if err != nil {
		return nil, err
	}
	return l.Extensions(ctx, p)
}

func (r *Router) VerifyExtensions(ctx context.Context, p *registry.Project) error {
	l, err := r.local(ctx, p, "verifying extensions")
	if err != nil {
		return err
	}
	return l.VerifyExtensions(ctx, p)
}

func (r *Router) PendingRestart(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (bool, error) {
	l, err := r.local(ctx, p, "looking for a held-back restart")
	if err != nil {
		return false, err
	}
	return l.PendingRestart(ctx, p, keys)
}

func (r *Router) RestartPending(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (bool, error) {
	l, err := r.local(ctx, p, "restarting a held-back unit")
	if err != nil {
		return false, err
	}
	return l.RestartPending(ctx, p, keys)
}
