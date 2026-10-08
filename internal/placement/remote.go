package placement

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// ErrRemoteSeed is returned by RemotePlane.Create for a seed: a seed is a function over the node's
// own disk and does not travel. A project that is homed elsewhere is created there, or restored
// there, by the node that is its home.
var ErrRemoteSeed = errors.New("placement: a project cannot be created from a seed on another node")

// RemotePlane is the lifecycle.Plane of the projects homed on one other node. Each method is a
// call on that node's agent over the peer API (peerapi.PathPlane). It is used by the leader.
type RemotePlane struct {
	// Node is the id of the node that runs the projects.
	Node string
	RPC  mesh.RPC
	// Epoch is the cluster epoch the calls are made under (peerapi.PlaneCall.Epoch); the node
	// refuses a call under an epoch older than its own. Nil sends 0.
	Epoch func() int64
}

var _ lifecycle.Plane = (*RemotePlane)(nil)

func (r *RemotePlane) epoch() int64 {
	if r.Epoch == nil {
		return 0
	}
	return r.Epoch()
}

// call runs method m for ref on the node. args is the JSON value of the method's arguments (nil for
// none); the node's result is decoded into out (nil to drop it).
func (r *RemotePlane) call(ctx context.Context, ref string, m peerapi.PlaneMethod, args, out any) error {
	call := peerapi.PlaneCall{Epoch: r.epoch()}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		call.Args = b
	}
	var res peerapi.PlaneResult
	if err := r.RPC.Call(ctx, r.Node, http.MethodPost, peerapi.PlanePath(ref, m), call, &res); err != nil {
		return wrapRemote(r.Node, err)
	}
	if out != nil && len(res.Result) > 0 {
		return json.Unmarshal(res.Result, out)
	}
	return nil
}

// Create implements lifecycle.DataPlane. A nil seed only.
func (r *RemotePlane) Create(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, seed lifecycle.DataSeeder) error {
	if seed != nil {
		return ErrRemoteSeed
	}
	return r.call(ctx, p.Ref, peerapi.PlaneCreate, projectArgs{Project: p, Keys: keys}, nil)
}

// Delete implements lifecycle.DataPlane.
func (r *RemotePlane) Delete(ctx context.Context, ref string) error {
	return r.call(ctx, ref, peerapi.PlaneDelete, nil, nil)
}

// Snapshot implements lifecycle.DataPlane.
func (r *RemotePlane) Snapshot(ctx context.Context, ref string) (*registry.Backup, error) {
	var b *registry.Backup
	if err := r.call(ctx, ref, peerapi.PlaneSnapshot, nil, &b); err != nil {
		return nil, err
	}
	return b, nil
}

// Route implements lifecycle.DataPlane.
func (r *RemotePlane) Route(ctx context.Context, ref string) (lifecycle.Upstreams, error) {
	var u lifecycle.Upstreams
	err := r.call(ctx, ref, peerapi.PlaneRoute, nil, &u)
	return u, err
}

// Usage implements lifecycle.DataPlane.
func (r *RemotePlane) Usage(ctx context.Context, ref string) (lifecycle.Usage, error) {
	var u lifecycle.Usage
	err := r.call(ctx, ref, peerapi.PlaneUsage, nil, &u)
	return u, err
}

// Start implements lifecycle.Runner.
func (r *RemotePlane) Start(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	return r.call(ctx, p.Ref, peerapi.PlaneStart, projectArgs{Project: p, Keys: keys}, nil)
}

// StartDatabase implements lifecycle.Runner.
func (r *RemotePlane) StartDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	return r.call(ctx, p.Ref, peerapi.PlaneStartDatabase, projectArgs{Project: p, Keys: keys}, nil)
}

// Stop implements lifecycle.Runner.
func (r *RemotePlane) Stop(ctx context.Context, ref string) error {
	return r.call(ctx, ref, peerapi.PlaneStop, nil, nil)
}

// Reconfigure implements lifecycle.Runner.
func (r *RemotePlane) Reconfigure(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	return r.call(ctx, p.Ref, peerapi.PlaneReconfigure, projectArgs{Project: p, Keys: keys}, nil)
}

// Health implements lifecycle.Runner. It has no error to return, so a node that cannot be reached
// answers as every service of the project unhealthy, with the cause.
func (r *RemotePlane) Health(ctx context.Context, p *registry.Project, _ *secrets.ProjectKeys) []lifecycle.ServiceHealth {
	var hs []lifecycle.ServiceHealth
	if err := r.call(ctx, p.Ref, peerapi.PlaneHealth, projectArgs{Project: p}, &hs); err != nil {
		names := []string{config.SvcPostgres, config.SvcGoTrue}
		if p.Ref != config.SystemRef {
			names = append(names, config.SvcPostgREST)
		}
		hs = hs[:0]
		for _, n := range names {
			hs = append(hs, lifecycle.ServiceHealth{Name: n, Status: "UNHEALTHY", Error: err.Error()})
		}
	}
	return hs
}

var _ CheckpointReader = (*RemotePlane)(nil)

// FinalCheckpoint implements CheckpointReader: the control file of ref's cluster on the node.
func (r *RemotePlane) FinalCheckpoint(ctx context.Context, ref string) (lifecycle.ControlInfo, error) {
	var ci lifecycle.ControlInfo
	err := r.call(ctx, ref, planeFinalCheckpoint, nil, &ci)
	return ci, err
}
