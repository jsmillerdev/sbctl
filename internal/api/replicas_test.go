package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/replicas"
)

// fakeReplicas is a replicas.Service over the registry: Setup writes the row a controller would
// (on the first active node of the region that is not the project's home), and the other methods
// read, delete or record. Tests set the errors, statuses and lag samples they need.
type fakeReplicas struct {
	reg registry.Registry

	mu       sync.Mutex
	n        int
	setupErr error
	listErr  error
	rmErr    error
	setups   []string // "ref region" per Setup
	removed  []string // identifiers
	restarts []string // identifiers
	statuses map[string]replicas.Status
	// unreported are replicas the controller has no status for yet.
	unreported map[string]bool
	lag        map[string][]replicas.LagPoint
	lagSince   []time.Time
}

var _ replicas.Service = (*fakeReplicas)(nil)

func (f *fakeReplicas) Setup(ctx context.Context, ref, region string) error {
	f.mu.Lock()
	f.setups = append(f.setups, ref+" "+region)
	err := f.setupErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	p, perr := f.reg.GetProject(ctx, ref)
	if perr != nil {
		return perr
	}
	nodes, _ := f.reg.ListNodes(ctx)
	for _, n := range nodes {
		if n.State != registry.NodeActive || n.Region != region || n.ID == p.NodeID {
			continue
		}
		f.mu.Lock()
		f.n++
		id6 := fmt.Sprintf("r%05d", f.n)
		f.mu.Unlock()
		return f.reg.CreateReplica(ctx, &registry.Replica{Identifier: registry.ReplicaIdentifier(ref, region, id6), Ref: ref, NodeID: n.ID})
	}
	return &replicas.UserError{Msg: "No Supavise server is joined in " + region + "."}
}

func (f *fakeReplicas) SetupOn(ctx context.Context, ref, nodeID string) error { return nil }

func (f *fakeReplicas) owns(ctx context.Context, ref, identifier string) bool {
	rs, _ := f.reg.ListReplicas(ctx, ref)
	return slices.ContainsFunc(rs, func(r registry.Replica) bool { return r.Identifier == identifier })
}

func (f *fakeReplicas) Remove(ctx context.Context, ref, identifier string) error {
	f.mu.Lock()
	err := f.rmErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if !f.owns(ctx, ref, identifier) {
		return replicas.ErrNotFound
	}
	f.mu.Lock()
	f.removed = append(f.removed, identifier)
	f.mu.Unlock()
	return f.reg.DeleteReplica(ctx, identifier)
}

func (f *fakeReplicas) Restart(ctx context.Context, ref, identifier string) error {
	if !f.owns(ctx, ref, identifier) {
		return replicas.ErrNotFound
	}
	f.mu.Lock()
	f.restarts = append(f.restarts, identifier)
	f.mu.Unlock()
	return nil
}

func (f *fakeReplicas) List(ctx context.Context, ref string) ([]replicas.Replica, error) {
	f.mu.Lock()
	err := f.listErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rs, err := f.reg.ListReplicas(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []replicas.Replica
	for _, r := range rs {
		n, err := f.reg.GetNode(ctx, r.NodeID)
		if err != nil {
			return nil, err
		}
		out = append(out, replicas.Replica{Replica: r, Region: n.Region, PublicHost: n.PublicHost})
	}
	return out, nil
}

func (f *fakeReplicas) Statuses(ctx context.Context, ref string) ([]replicas.Status, error) {
	rs, err := f.reg.ListReplicas(ctx, ref)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []replicas.Status
	for _, r := range rs {
		if f.unreported[r.Identifier] {
			continue
		}
		st, ok := f.statuses[r.Identifier]
		if !ok {
			st = replicas.Status{Identifier: r.Identifier, Status: r.Status, LagSeconds: -1}
		}
		out = append(out, st)
	}
	return out, nil
}

func (f *fakeReplicas) Lag(_ context.Context, identifier string, since time.Time) ([]replicas.LagPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lagSince = append(f.lagSince, since)
	var out []replicas.LagPoint
	for _, p := range f.lag[identifier] {
		if !p.At.Before(since) {
			out = append(out, p)
		}
	}
	return out, nil
}

// replicaFixture is a fixture whose server has a replica controller (the fake), a second node in
// eu-west-1, a Small project and a backup service (point-in-time recovery on), which is what
// Studio's Add read replica button needs.
type replicaFixture struct {
	*fixture
	svc *fakeReplicas
}

const euNode = "n2"

func newReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	f := newFixture(t)
	f.addNode(t, "eu", "eu-west-1", "eu.pooler.example.test", registry.NodeActive)
	svc := &fakeReplicas{reg: f.reg, statuses: map[string]replicas.Status{}, lag: map[string][]replicas.LagPoint{}}
	f.srv.replicas = svc
	f.srv.backups = &fakeBackupSource{}
	f.setClass(t, "small")
	return &replicaFixture{fixture: f, svc: svc}
}

// addNode adds a node to the registry and returns its id.
func (f *fixture) addNode(t *testing.T, name, region, host string, state registry.NodeState) string {
	t.Helper()
	n := &registry.Node{Name: name, Region: region, PublicHost: host, State: state}
	if err := f.reg.CreateNode(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	return n.ID
}

// setClass gives the fixture's project a compute size.
func (f *fixture) setClass(t *testing.T, class string) {
	t.Helper()
	p := f.projectRow(t)
	p.Class = class
	if err := f.reg.UpdateProject(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

// addReplica puts a replica of the fixture's project on a node, with a status and a setup step,
// and returns its identifier. Identifiers are rr<region>-<id6> as the controller builds them.
func (f *fixture) addReplica(t *testing.T, node, region, id6, status, step, initErr string) string {
	t.Helper()
	id := registry.ReplicaIdentifier(testRef, region, id6)
	if err := f.reg.CreateReplica(t.Context(), &registry.Replica{Identifier: id, Ref: testRef, NodeID: node}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.SetReplicaStatus(t.Context(), id, status, step, initErr); err != nil {
		t.Fatal(err)
	}
	return id
}

// addHealthyReplica is addReplica for a replica that finished setting up.
func (f *fixture) addHealthyReplica(t *testing.T, node, region, id6 string) string {
	t.Helper()
	return f.addReplica(t, node, region, id6, "ACTIVE_HEALTHY", registry.ReplicaStepDone, "")
}

const (
	setupPath  = "/v1/projects/" + testRef + "/read-replicas/setup"
	removePath = "/v1/projects/" + testRef + "/read-replicas/remove"
)

func TestSetupReadReplica(t *testing.T) {
	rf := newReplicaFixture(t)
	rf.run(t, []step{{key: "POST /v1/projects/{ref}/read-replicas/setup", body: map[string]any{"read_replica_region": "eu-west-1"}}})
	if !slices.Equal(rf.svc.setups, []string{testRef + " eu-west-1"}) {
		t.Fatalf("setups = %v", rf.svc.setups)
	}
	rs, _ := rf.reg.ListReplicas(t.Context(), testRef)
	if len(rs) != 1 || rs[0].NodeID != euNode || rs[0].Status != registry.ReplicaInit || rs[0].InitStep != registry.ReplicaStepRequested {
		t.Fatalf("replica rows = %+v", rs)
	}
	if ref, region, _, ok := registry.ParseReplicaIdentifier(rs[0].Identifier); !ok || ref != testRef || region != "eu-west-1" {
		t.Fatalf("identifier %q", rs[0].Identifier)
	}
}

// A refusal the controller words for the person is shown as it is, as a 400; the API adds the
// ones that concern the project itself. Each message is the one Studio puts in its toast.
func TestSetupReadReplicaRefusals(t *testing.T) {
	rf := newReplicaFixture(t)
	body := map[string]any{"read_replica_region": "eu-west-1"}
	post := func(path string, b any, wantStatus int, wantMsg string) {
		t.Helper()
		rec := rf.do("POST", path, b)
		if rec.Code != wantStatus {
			t.Fatalf("POST %s %v: %d %s, want %d", path, b, rec.Code, rec.Body, wantStatus)
		}
		if wantMsg == "" {
			return
		}
		if got, _ := decodeBody(t, rec).(map[string]any)["message"].(string); !strings.Contains(got, wantMsg) {
			t.Fatalf("POST %s: message %q, want %q", path, got, wantMsg)
		}
	}

	post(setupPath, map[string]any{}, 400, "read_replica_region is required")
	post(setupPath, map[string]any{"read_replica_region": "us-west-2"}, 400, "No Supavise server is joined in us-west-2.")
	rf.svc.setupErr = &replicas.UserError{Msg: "Not enough capacity on eu."}
	post(setupPath, body, 400, "Not enough capacity on eu.")
	rf.svc.setupErr = nil
	post("/v1/projects/zzzzzzzzzzzzzzzzzzzz/read-replicas/setup", body, 404, "Project not found")

	for _, size := range []string{"nano", "micro"} {
		rf.setClass(t, size)
		post(setupPath, body, 400, "Read replicas need a compute size of small or larger.")
	}
	if len(rf.svc.setups) != 2 { // only the controller's own refusals reached it
		t.Fatalf("the controller was asked %d times: %v", len(rf.svc.setups), rf.svc.setups)
	}

	// Hosted's caps: four up to Large, five above. The controller does not see a refused request.
	rf.setClass(t, "small")
	for i, region := range []string{"us-east-1", "us-west-2", "ap-south-1", "sa-east-1"} {
		node := rf.addNode(t, fmt.Sprintf("n-%d", i), region, "", registry.NodeActive)
		rf.addHealthyReplica(t, node, region, fmt.Sprintf("r9000%d", i))
	}
	before := len(rf.svc.setups)
	post(setupPath, body, 400, "The project already has the maximum of 4 read replicas.")
	rf.setClass(t, "xlarge")
	post(setupPath, body, 204, "")
	if len(rf.svc.setups) != before+1 {
		t.Fatalf("setups = %v", rf.svc.setups)
	}
	rf.setClass(t, "large")
	post(setupPath, body, 400, "The project already has the maximum of 4 read replicas.")
}

func TestSetupReadReplicaStateRefusals(t *testing.T) {
	rf := newReplicaFixture(t)
	body := map[string]any{"read_replica_region": "eu-west-1"}

	p := rf.projectRow(t)
	p.Status = registry.StatusInactive
	_ = rf.reg.UpdateProject(t.Context(), p)
	if rec := rf.do("POST", setupPath, body); rec.Code != 409 {
		t.Fatalf("paused project: %d %s", rec.Code, rec.Body)
	}
	p.Status = registry.StatusActiveHealthy
	_ = rf.reg.UpdateProject(t.Context(), p)

	// A branch gets no replica.
	const branch = "branchrefaaaaaaaaaaa"
	if err := rf.reg.CreateProject(t.Context(), &registry.Project{Ref: branch, OrgID: rf.org.ID, Name: "b", Status: registry.StatusActiveHealthy, Class: "small",
		Engine: registry.EnginePostgres, Branch: &registry.BranchInfo{ID: "0f8fad5b-d9cb-469f-a165-70867728950e", ParentRef: testRef, Name: "b"}}); err != nil {
		t.Fatal(err)
	}
	rec := rf.do("POST", "/v1/projects/"+branch+"/read-replicas/setup", body)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not available for branches") {
		t.Fatalf("branch: %d %s", rec.Code, rec.Body)
	}

	// The replica ports end below project_base: a project whose sequence is past the last one with
	// room is refused, and so is a replica_base that leaves no room at all.
	rf.cfg.Ports.ReplicaBase = rf.cfg.Ports.ProjectBase - 6 // room for sequence 1 only
	if rec := rf.do("POST", setupPath, body); rec.Code != 204 {
		t.Fatalf("sequence 1: %d %s", rec.Code, rec.Body)
	}
	const second = "bcdefghijklmnopqrstu"
	two := rf.mgr.addProject(t, second, "Second", rf.org.ID, registry.StatusActiveHealthy)
	two.Class = "small"
	_ = rf.reg.UpdateProject(t.Context(), two)
	rec = rf.do("POST", "/v1/projects/"+second+"/read-replicas/setup", body)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "port sequence") {
		t.Fatalf("sequence %d: %d %s", two.Seq, rec.Code, rec.Body)
	}
	rf.cfg.Ports.ReplicaBase = rf.cfg.Ports.ProjectBase - 2
	rec = rf.do("POST", "/v1/projects/"+second+"/read-replicas/setup", body)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "replica_base") {
		t.Fatalf("no room for replica ports: %d %s", rec.Code, rec.Body)
	}
}

// Without a controller the node lists the primary alone and refuses to add or remove a replica.
func TestReadReplicasWithoutAController(t *testing.T) {
	f := newFixture(t)
	f.setClass(t, "small")
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{setupPath, map[string]any{"read_replica_region": "eu-west-1"}},
		{removePath, map[string]any{"database_identifier": "x"}},
	} {
		rec := f.do("POST", c.path, c.body)
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "not set up") {
			t.Fatalf("%s: %d %s", c.path, rec.Code, rec.Body)
		}
	}
	rec := f.do("POST", "/platform/projects/"+testRef+"/restart", map[string]any{"database_identifier": registry.ReplicaIdentifier(testRef, "eu-west-1", "abcdef")})
	if rec.Code != 503 {
		t.Fatalf("restart of a replica: %d %s", rec.Code, rec.Body)
	}
}

func TestRemoveReadReplica(t *testing.T) {
	rf := newReplicaFixture(t)
	id := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")

	rf.run(t, []step{{key: "POST /v1/projects/{ref}/read-replicas/remove", body: map[string]any{"database_identifier": id}}})
	if !slices.Equal(rf.svc.removed, []string{id}) {
		t.Fatalf("removed = %v", rf.svc.removed)
	}
	if rs, _ := rf.reg.ListReplicas(t.Context(), testRef); len(rs) != 0 {
		t.Fatalf("rows left: %+v", rs)
	}

	for name, ident := range map[string]string{"gone": id, "primary": testRef, "foreign": registry.ReplicaIdentifier("bcdefghijklmnopqrstu", "eu-west-1", "abcdef")} {
		rec := rf.do("POST", removePath, map[string]any{"database_identifier": ident})
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Read replica not found") {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := rf.do("POST", removePath, map[string]any{}); rec.Code != 400 {
		t.Fatalf("no identifier: %d %s", rec.Code, rec.Body)
	}
	rf.svc.rmErr = errors.New("node unreachable: dial tcp 10.0.0.9: i/o timeout")
	rec := rf.do("POST", removePath, map[string]any{"database_identifier": id})
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Fatalf("a controller failure is a generic 500: %d %s", rec.Code, rec.Body)
	}
}

// POST restart with a database_identifier restarts that replica; without one, or with the
// project's own identifier, it restarts the project (the primary) as before.
func TestRestartReplica(t *testing.T) {
	rf := newReplicaFixture(t)
	id := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	const path = "/platform/projects/" + testRef + "/restart"

	rec := rf.do("POST", path, map[string]any{"database_identifier": id})
	if rec.Code != 201 || !slices.Equal(rf.svc.restarts, []string{id}) {
		t.Fatalf("restart replica: %d %s %v", rec.Code, rec.Body, rf.svc.restarts)
	}
	if len(rf.mgr.paused) != 0 {
		t.Fatalf("restarting a replica paused the project: %v", rf.mgr.paused)
	}

	rec = rf.do("POST", path, map[string]any{"database_identifier": registry.ReplicaIdentifier(testRef, "eu-west-1", "zzzzzz")})
	if rec.Code != 404 {
		t.Fatalf("unknown replica: %d %s", rec.Code, rec.Body)
	}

	for _, body := range []any{nil, map[string]any{}, map[string]any{"database_identifier": testRef}} {
		rf.mgr.paused, rf.mgr.resumed = nil, nil
		if rec := rf.do("POST", path, body); rec.Code != 201 {
			t.Fatalf("restart project with %v: %d %s", body, rec.Code, rec.Body)
		}
		if len(rf.mgr.paused) != 1 || len(rf.mgr.resumed) != 1 {
			t.Fatalf("restart project with %v: paused %v resumed %v", body, rf.mgr.paused, rf.mgr.resumed)
		}
	}
	if len(rf.svc.restarts) != 1 {
		t.Fatalf("replica restarts = %v", rf.svc.restarts)
	}

	// A body that is not JSON restarts nothing where replicas are managed: a client that meant a
	// replica must not take the primary down by sending it badly.
	rf.mgr.paused, rf.mgr.resumed = nil, nil
	for _, body := range []any{"not json", `{"database_identifier":`} {
		if rec := rf.do("POST", path, body); rec.Code != 400 {
			t.Fatalf("restart with %v: %d %s", body, rec.Code, rec.Body)
		}
	}
	if len(rf.mgr.paused) != 0 || len(rf.svc.restarts) != 1 {
		t.Fatalf("a bad body restarted something: paused %v, replica restarts %v", rf.mgr.paused, rf.svc.restarts)
	}
	// A node without a controller never read the body.
	rf.srv.replicas = nil
	if rec := rf.do("POST", path, "not json"); rec.Code != 201 || len(rf.mgr.paused) != 1 {
		t.Fatalf("restart without a controller: %d %s, paused %v", rec.Code, rec.Body, rf.mgr.paused)
	}
}

// The project detail carries what Studio's Add read replica button reads: dbVersion in hosted's
// shape and is_physical_backups_enabled from the node's PITR.
func TestProjectDetailForReplicaEligibility(t *testing.T) {
	rf := newReplicaFixture(t)
	const path = "/platform/projects/" + testRef
	got := rf.body(t, "GET", path, nil, 200)
	if got["dbVersion"] != "supabase-postgres-17.11.0.004" || got["is_physical_backups_enabled"] != true {
		t.Fatalf("detail: dbVersion %v, physical backups %v", got["dbVersion"], got["is_physical_backups_enabled"])
	}
	list := rf.body(t, "GET", "/platform/projects", nil, 200)
	row := list["projects"].([]any)[0].(map[string]any)
	if row["is_physical_backups_enabled"] != true {
		t.Fatalf("list row: %v", row)
	}
	// The v1 project keeps the bare version its schema names.
	if v := rf.body(t, "GET", "/v1/projects/"+testRef, nil, 200); v["database"].(map[string]any)["version"] != "17.11.0.004" {
		t.Fatalf("v1 version: %v", v["database"])
	}

	rf.srv.backups = nil
	if got := rf.body(t, "GET", path, nil, 200); got["is_physical_backups_enabled"] != false {
		t.Fatalf("without a backup service: %v", got["is_physical_backups_enabled"])
	}
	list = rf.body(t, "GET", "/platform/projects", nil, 200)
	if list["projects"].([]any)[0].(map[string]any)["is_physical_backups_enabled"] != false {
		t.Fatalf("list row without a backup service: %v", list)
	}
}

// Studio's replica screens stay hidden (infrastructure:read_replicas in disabled_features) until a
// controller is wired and a second server has joined: a node on its own looks as it did.
func TestProfileOffersReplicasWithASecondServer(t *testing.T) {
	disabled := func(f *fixture) bool {
		t.Helper()
		rec := f.do("GET", "/platform/profile", nil)
		if rec.Code != 200 {
			t.Fatalf("profile: %d %s", rec.Code, rec.Body)
		}
		var list []any
		if m, ok := decodeBody(t, rec).(map[string]any); ok {
			list, _ = m["disabled_features"].([]any)
		}
		return slices.Contains(list, any("infrastructure:read_replicas"))
	}
	f := newFixture(t)
	if !disabled(f) {
		t.Fatal("a single server offers read replicas")
	}
	f.addNode(t, "eu", "eu-west-1", "", registry.NodeJoining)
	f.srv.replicas = &fakeReplicas{reg: f.reg}
	if !disabled(f) {
		t.Fatal("a node that has not confirmed its join counts")
	}
	if err := f.reg.SetNodeState(t.Context(), euNode, registry.NodeActive); err != nil {
		t.Fatal(err)
	}
	if disabled(f) {
		t.Fatal("two servers and a controller still hide read replicas")
	}
	f.srv.replicas = nil
	if !disabled(f) {
		t.Fatal("no controller, yet read replicas are offered")
	}
	if !slices.Contains(disabledFeatures, "infrastructure:read_replicas") || !slices.Contains(disabledFeatures, "billing:all") {
		t.Fatal("the default list lost an entry")
	}
}
