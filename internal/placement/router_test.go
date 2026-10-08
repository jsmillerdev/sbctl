package placement

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
)

// recordRPC records the calls made through it and answers 200 with no body.
type recordRPC struct {
	mu    sync.Mutex
	calls []string
	nodes []string
}

func (r *recordRPC) Call(_ context.Context, node, method, path string, _, _ any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, method+" "+path)
	r.nodes = append(r.nodes, node)
	return nil
}

type routerEnv struct {
	local *fakeLocal
	rpc   *recordRPC
	reg   *registry.Memory
	r     *Router
	a, b  string // refs homed on n1 and on n2
}

func newRouterEnv(t *testing.T) *routerEnv {
	t.Helper()
	ctx := context.Background()
	reg := registry.NewMemory()
	mustCreate(t, reg, "second")
	e := &routerEnv{local: newFakeLocal(), rpc: &recordRPC{}, reg: reg, a: testRef, b: "bcdefghijklmnopqrstu"}
	for _, ref := range []string{e.a, e.b} {
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.SetProjectNode(ctx, e.b, "n2", 1); err != nil {
		t.Fatal(err)
	}
	e.r = NewRouter(RouterOptions{Local: e.local, Resolver: RegistryResolver{Reg: reg}, Self: func() string { return "n1" }, RPC: e.rpc, Epoch: func() int64 { return 1 }})
	return e
}

func TestRouterSendsEachProjectToItsHome(t *testing.T) {
	ctx := context.Background()
	e := newRouterEnv(t)
	pl, err := e.r.For(ctx, e.a)
	if err != nil || pl != lifecycle.Plane(e.local) || e.r.Local() != lifecycle.Plane(e.local) {
		t.Fatalf("For(a) = %T, %v", pl, err)
	}
	pb, err := e.r.For(ctx, e.b)
	rp, ok := pb.(*RemotePlane)
	if err != nil || !ok || rp.Node != "n2" {
		t.Fatalf("For(b) = %T %+v, %v", pb, pb, err)
	}
	if again, _ := e.r.For(ctx, e.b); again != pb {
		t.Fatal("a remote plane is made again for each call")
	}
	if _, err := e.r.For(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown ref: %v", err)
	}

	pa := testProject("n1")
	pa.Ref = e.a
	pbRow := testProject("n2")
	pbRow.Ref = e.b
	k := testKeys()
	if err := e.r.Start(ctx, pa, k); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Start(ctx, pbRow, k); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Stop(ctx, e.b); err != nil {
		t.Fatal(err)
	}
	if e.local.has("Start "+e.b) || !e.local.has("Start "+e.a) || len(e.rpc.calls) != 2 {
		t.Fatalf("local %v, remote %v", e.local.calls, e.rpc.calls)
	}
	if e.rpc.nodes[0] != "n2" || e.rpc.calls[0] != "POST /peer/v1/projects/"+e.b+"/plane/start" || e.rpc.calls[1] != "POST /peer/v1/projects/"+e.b+"/plane/stop" {
		t.Fatalf("remote calls %v on %v", e.rpc.calls, e.rpc.nodes)
	}
}

func TestRouterRoutesByTheRowItIsGiven(t *testing.T) {
	ctx := context.Background()
	e := newRouterEnv(t)
	// The row says where the project is going to live, whatever the registry says at the moment.
	p := testProject("n2")
	if err := e.r.Create(ctx, p, testKeys(), nil); err != nil {
		t.Fatal(err)
	}
	if len(e.rpc.calls) != 1 || !strings.HasSuffix(e.rpc.calls[0], "/plane/create") || len(e.local.calls) != 0 {
		t.Fatalf("remote %v, local %v", e.rpc.calls, e.local.calls)
	}
	// A row from before the cluster migration has no node and is this node's.
	old := testProject("")
	if err := e.r.Start(ctx, old, testKeys()); err != nil || !e.local.has("Start "+old.Ref) {
		t.Fatalf("a row with no node: %v %v", err, e.local.calls)
	}
}

func TestRouterRouteAndCleanupStayLocal(t *testing.T) {
	ctx := context.Background()
	e := newRouterEnv(t)
	// Route is the same on every node (I3): the router never asks the home.
	if _, err := e.r.Route(ctx, e.b); err != nil || len(e.rpc.calls) != 0 || !e.local.has("Route "+e.b) {
		t.Fatalf("Route: %v remote %v local %v", err, e.rpc.calls, e.local.calls)
	}
	// A ref the registry no longer has can only have leftovers here.
	if err := e.r.Delete(ctx, "zzzzzzzzzzzzzzzzzzzz"); err != nil || !e.local.has("Delete zzzzzzzzzzzzzzzzzzzz") {
		t.Fatalf("Delete of an unknown ref: %v %v", err, e.local.calls)
	}
	if err := e.r.Stop(ctx, "zzzzzzzzzzzzzzzzzzzz"); err != nil || !e.local.has("Stop zzzzzzzzzzzzzzzzzzzz") {
		t.Fatalf("Stop of an unknown ref: %v %v", err, e.local.calls)
	}
	// The others need the row.
	if _, err := e.r.Usage(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Usage: %v", err)
	}
	if _, err := e.r.Snapshot(ctx, "zzzzzzzzzzzzzzzzzzzz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Snapshot: %v", err)
	}
	// Health cannot return an error: a ref nobody knows is unhealthy with the reason.
	p := testProject("")
	p.Ref = "zzzzzzzzzzzzzzzzzzzz"
	p.NodeID = ""
	hs := e.r.Health(ctx, p, nil)
	if len(hs) != 1 || hs[0].Healthy || !strings.Contains(hs[0].Error, "not found") {
		t.Fatalf("Health of an unknown project: %+v", hs)
	}
}

// The Engine looks for the optional capabilities of its plane by type assertion; the router has
// them all, runs them on the local plane and refuses them for a project homed elsewhere.
func TestRouterOptionalCapabilities(t *testing.T) {
	ctx := context.Background()
	e := newRouterEnv(t)
	local, remote := testProject("n1"), testProject("n2")
	remote.Ref = e.b
	k := testKeys()

	type op struct {
		name string
		run  func(p *registry.Project) error
	}
	ops := []op{
		{"ReconfigureService", func(p *registry.Project) error { return e.r.ReconfigureService(ctx, p, k, "gotrue") }},
		{"ApplyPostgresSettings", func(p *registry.Project) error { _, err := e.r.ApplyPostgresSettings(ctx, p, k, true, nil); return err }},
		{"SetRolePassword", func(p *registry.Project) error { return e.r.SetRolePassword(ctx, p, "postgres", "x") }},
		{"SetRolePasswords", func(p *registry.Project) error { return e.r.SetRolePasswords(ctx, p, k) }},
		{"RecoverPostgres", func(p *registry.Project) error { return e.r.RecoverPostgres(ctx, p, k) }},
		{"CheckRender", func(p *registry.Project) error { return e.r.CheckRender(ctx, p, k, projectconfig.Service("auth")) }},
		{"Extensions", func(p *registry.Project) error { _, err := e.r.Extensions(ctx, p); return err }},
		{"VerifyExtensions", func(p *registry.Project) error { return e.r.VerifyExtensions(ctx, p) }},
		{"PendingRestart", func(p *registry.Project) error { _, err := e.r.PendingRestart(ctx, p, k); return err }},
		{"RestartPending", func(p *registry.Project) error { _, err := e.r.RestartPending(ctx, p, k); return err }},
	}
	for _, o := range ops {
		if err := o.run(local); err != nil || !e.local.has(o.name+" "+local.Ref+extra(o.name)) {
			t.Errorf("%s on the home: %v (calls %v)", o.name, err, e.local.calls)
		}
		before := len(e.local.calls)
		if err := o.run(remote); !errors.Is(err, lifecycle.ErrNotSupported) || len(e.local.calls) != before {
			t.Errorf("%s for a project homed elsewhere: %v", o.name, err)
		}
	}
	if len(e.rpc.calls) != 0 {
		t.Fatalf("an optional capability went over the wire: %v", e.rpc.calls)
	}
	// A failure of the local plane comes back as it is.
	boom := errors.New("boom")
	e.local.err["RecoverPostgres"] = boom
	if err := e.r.RecoverPostgres(ctx, local, k); !errors.Is(err, boom) {
		t.Fatalf("local error: %v", err)
	}
	// A row without a node is looked up.
	row := testProject("")
	row.Ref = e.b
	if err := e.r.RecoverPostgres(ctx, row, k); !errors.Is(err, lifecycle.ErrNotSupported) {
		t.Fatalf("a row without a node whose project is homed elsewhere: %v", err)
	}
}

func extra(name string) string {
	switch name {
	case "ReconfigureService":
		return " gotrue"
	case "ApplyPostgresSettings":
		return " restart=true"
	case "SetRolePassword":
		return " postgres"
	}
	return ""
}
