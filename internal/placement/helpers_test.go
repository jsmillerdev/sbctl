package placement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

const testRef = "abcdefghijklmnopqrst"

// muxRPC is a mesh.RPC whose far end is a mesh.Mux in this process: the request is served as the peer
// server would serve it, with the caller's node id in the context.
type muxRPC struct {
	mux    *mesh.Mux
	caller string
	mu     sync.Mutex
	calls  []string
	nodes  []string
}

func (m *muxRPC) Call(ctx context.Context, node, method, path string, in, out any) error {
	m.mu.Lock()
	m.calls = append(m.calls, method+" "+path)
	m.nodes = append(m.nodes, node)
	m.mu.Unlock()
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req := httptest.NewRequest(method, path, &body).WithContext(mesh.WithPeer(ctx, mesh.Peer{Node: m.caller}))
	rec := httptest.NewRecorder()
	m.mux.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code > 299 {
		var pe peerapi.Error
		_ = json.Unmarshal(rec.Body.Bytes(), &pe)
		return &mesh.RemoteError{Node: node, Status: rec.Code, Message: pe.Message, Code: pe.Code}
	}
	if out != nil && rec.Body.Len() > 0 {
		return json.Unmarshal(rec.Body.Bytes(), out)
	}
	return nil
}

func (m *muxRPC) callLog() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.calls, "\n")
}

// members is a cluster.Membership of two nodes: this one (n2) and the leader n1.
func members(self string, leader string, epoch int64) *cluster.Static {
	n1 := registry.Node{ID: "n1", Name: "primary", State: registry.NodeActive}
	n2 := registry.Node{ID: "n2", Name: "second", State: registry.NodeActive}
	nodes := []registry.Node{n1, n2}
	s := n1
	if self == "n2" {
		s = n2
	}
	return cluster.NewStatic(cluster.Snapshot{Self: s, Nodes: nodes, Leader: leader, Epoch: epoch, Role: cluster.RoleFollower})
}

// fakeLocal is a lifecycle.FullPlane that records its calls and answers from fields.
type fakeLocal struct {
	mu       sync.Mutex
	calls    []string
	projects map[string]*registry.Project
	keys     map[string]*secrets.ProjectKeys
	err      map[string]error // by method name
	health   []lifecycle.ServiceHealth
}

func newFakeLocal() *fakeLocal {
	return &fakeLocal{projects: map[string]*registry.Project{}, keys: map[string]*secrets.ProjectKeys{}, err: map[string]error{}}
}

func (f *fakeLocal) rec(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err[strings.SplitN(call, " ", 2)[0]]
}

func (f *fakeLocal) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fakeLocal) remember(p *registry.Project, k *secrets.ProjectKeys) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p != nil {
		cp := *p
		f.projects[p.Ref] = &cp
	}
	if k != nil {
		ck := *k
		f.keys[p.Ref] = &ck
	}
}

func (f *fakeLocal) Create(_ context.Context, p *registry.Project, k *secrets.ProjectKeys, seed lifecycle.DataSeeder) error {
	f.remember(p, k)
	return f.rec(fmt.Sprintf("Create %s seed=%v", p.Ref, seed != nil))
}
func (f *fakeLocal) Delete(_ context.Context, ref string) error { return f.rec("Delete " + ref) }
func (f *fakeLocal) Snapshot(_ context.Context, ref string) (*registry.Backup, error) {
	return &registry.Backup{ID: 7, Ref: ref, Kind: "base", Status: registry.BackupCompleted, Location: "file:///b/" + ref + "/base/x", Timeline: 3, SizeBytes: 99}, f.rec("Snapshot " + ref)
}
func (f *fakeLocal) Route(_ context.Context, ref string) (lifecycle.Upstreams, error) {
	return lifecycle.Upstreams{Postgres: "127.0.0.1:20006", GoTrue: "127.0.0.1:20007", PostgREST: "127.0.0.1:20008"}, f.rec("Route " + ref)
}
func (f *fakeLocal) Usage(_ context.Context, ref string) (lifecycle.Usage, error) {
	return lifecycle.Usage{DiskBytes: 1 << 20, MemoryBytes: 5 << 20}, f.rec("Usage " + ref)
}
func (f *fakeLocal) Start(_ context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	f.remember(p, k)
	return f.rec("Start " + p.Ref)
}
func (f *fakeLocal) StartDatabase(_ context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	f.remember(p, k)
	return f.rec("StartDatabase " + p.Ref)
}
func (f *fakeLocal) Stop(_ context.Context, ref string) error { return f.rec("Stop " + ref) }
func (f *fakeLocal) Reconfigure(_ context.Context, p *registry.Project, k *secrets.ProjectKeys) error {
	f.remember(p, k)
	return f.rec("Reconfigure " + p.Ref)
}
func (f *fakeLocal) Health(_ context.Context, p *registry.Project, k *secrets.ProjectKeys) []lifecycle.ServiceHealth {
	f.remember(p, k)
	_ = f.rec("Health " + p.Ref)
	return f.health
}

func (f *fakeLocal) ReconfigureService(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys, svc string) error {
	return f.rec("ReconfigureService " + p.Ref + " " + svc)
}
func (f *fakeLocal) ApplyPostgresSettings(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys, restart bool, before func(context.Context)) (bool, error) {
	return true, f.rec(fmt.Sprintf("ApplyPostgresSettings %s restart=%v", p.Ref, restart))
}
func (f *fakeLocal) SetRolePassword(_ context.Context, p *registry.Project, role, _ string) error {
	return f.rec("SetRolePassword " + p.Ref + " " + role)
}
func (f *fakeLocal) SetRolePasswords(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) error {
	return f.rec("SetRolePasswords " + p.Ref)
}
func (f *fakeLocal) RecoverPostgres(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) error {
	return f.rec("RecoverPostgres " + p.Ref)
}
func (f *fakeLocal) CheckRender(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys, svc projectconfig.Service) error {
	return f.rec("CheckRender " + p.Ref)
}
func (f *fakeLocal) Extensions(_ context.Context, p *registry.Project) ([]lifecycle.InstalledExtension, error) {
	return nil, f.rec("Extensions " + p.Ref)
}
func (f *fakeLocal) VerifyExtensions(_ context.Context, p *registry.Project) error {
	return f.rec("VerifyExtensions " + p.Ref)
}
func (f *fakeLocal) PendingRestart(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) (bool, error) {
	return true, f.rec("PendingRestart " + p.Ref)
}
func (f *fakeLocal) RestartPending(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) (bool, error) {
	return true, f.rec("RestartPending " + p.Ref)
}

var _ lifecycle.FullPlane = (*fakeLocal)(nil)

// testProject is a project of the registry's shape that survives JSON.
func testProject(node string) *registry.Project {
	return &registry.Project{
		Ref: testRef, Seq: 7, Name: "demo", Region: "us-east-1", Engine: registry.EnginePostgres, Class: "micro",
		Status: registry.StatusActiveHealthy, NodeID: node,
		Versions: map[string]string{"postgres": "17.6.1.001", "gotrue": "v2.180.0"},
	}
}

func testKeys() *secrets.ProjectKeys {
	return &secrets.ProjectKeys{JWTSecret: "jwt", AnonKey: "anon", ServiceRoleKey: "svc", DBPassword: "db", AdminPassword: "admin",
		AuthenticatorPassword: "authn", AuthAdminPassword: "authadmin", StorageAdminPassword: "storage", ReplicationPassword: "repl", PGSodiumRootKey: "root"}
}

func mustCreate(t *testing.T, reg *registry.Memory, nodes ...string) {
	t.Helper()
	ctx := context.Background()
	for _, n := range nodes {
		if _, err := reg.GetNodeByName(ctx, n); err == nil {
			continue
		}
		if err := reg.CreateNode(ctx, &registry.Node{Name: n, State: registry.NodeActive}); err != nil {
			t.Fatal(err)
		}
	}
}

// recordingAgent is an Agent that records its calls.
type recordingAgent struct {
	mu    sync.Mutex
	calls []string
	err   error
	// last holds the arguments of the latest call, for assertions.
	spec peerapi.InstanceSpec
	req  peerapi.InstanceAction
}

func (a *recordingAgent) rec(s string) {
	a.mu.Lock()
	a.calls = append(a.calls, s)
	a.mu.Unlock()
}
func (a *recordingAgent) all() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.calls, ",")
}
func (a *recordingAgent) st(id string) peerapi.InstanceStatus {
	ref, _, _, _ := registry.ParseReplicaIdentifier(id)
	return peerapi.InstanceStatus{Identifier: id, Ref: ref, Role: "replica", Step: StepCompleted, PostgresUp: true, PostgRESTReady: true, InRecovery: true}
}
func (a *recordingAgent) Ensure(_ context.Context, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error) {
	a.rec("ensure " + spec.Identifier)
	a.spec = spec
	return a.st(spec.Identifier), a.err
}
func (a *recordingAgent) Observe(_ context.Context, id string) (peerapi.InstanceStatus, error) {
	a.rec("observe " + id)
	return a.st(id), a.err
}
func (a *recordingAgent) Remove(_ context.Context, id string) error {
	a.rec("remove " + id)
	return a.err
}
func (a *recordingAgent) Do(_ context.Context, id string, act peerapi.Action, req peerapi.InstanceAction) (peerapi.InstanceStatus, error) {
	a.rec(string(act) + " " + id)
	a.req = req
	return a.st(id), a.err
}

// fakeBackups is a LocalBackups with canned answers.
type fakeBackups struct {
	mu       sync.Mutex
	reasons  []string
	noRecord []bool
	restored []lifecycle.RestoreRequest
	files    []string
	recorded []backup.RemoteBase
	err      error
}

func (f *fakeBackups) BaseBackupWith(_ context.Context, ref string, bo backup.BackupOptions) (*registry.Backup, error) {
	f.mu.Lock()
	f.reasons = append(f.reasons, bo.Reason)
	f.noRecord = append(f.noRecord, bo.NoRecord)
	f.mu.Unlock()
	return &registry.Backup{Ref: ref, Location: "file:///b/" + ref + "/base/20261001T000000Z-ab12/", Timeline: 2, StartLSN: "0/3000028", StopLSN: "0/3000100", SizeBytes: 1234}, f.err
}
func (f *fakeBackups) BackupFiles(_ context.Context, ref string, fo backup.FilesOptions) (*backup.FilesResult, error) {
	f.mu.Lock()
	f.files = append(f.files, ref+" "+fo.Reason)
	f.mu.Unlock()
	return &backup.FilesResult{}, f.err
}
func (f *fakeBackups) RecordBase(_ context.Context, ref string, b backup.RemoteBase) (*registry.Backup, error) {
	f.mu.Lock()
	f.recorded = append(f.recorded, b)
	f.mu.Unlock()
	return &registry.Backup{ID: 31, Ref: ref, Status: registry.BackupCompleted, Location: "file:///b/" + ref + "/base/" + b.ID + "/", Timeline: b.Timeline}, nil
}
func (f *fakeBackups) RestoreInPlace(_ context.Context, ref string, req lifecycle.RestoreRequest) error {
	f.mu.Lock()
	f.restored = append(f.restored, req)
	f.mu.Unlock()
	return f.err
}
