package cluster

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// tempDatabase creates a database next to the one in dsn and returns the DSN of the new one.
func tempDatabase(t *testing.T, dsn, prefix string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `create database `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), `drop database if exists `+name+` with (force)`)
			a.Close()
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

type fakeMarker struct {
	m   *backup.LeaderMarker
	err error
}

func (f fakeMarker) ReadLeaderMarker(context.Context) (*backup.LeaderMarker, error) {
	return f.m, f.err
}
func (f fakeMarker) WriteLeaderMarker(context.Context, backup.LeaderMarker) error { return nil }

type pgTopo struct {
	reg  registry.Registry
	self string
}

func (p pgTopo) Self() registry.Node {
	n, err := p.reg.GetNode(context.Background(), p.self)
	if err != nil {
		return registry.Node{ID: p.self}
	}
	return *n
}
func (p pgTopo) Nodes() []registry.Node { ns, _ := p.reg.ListNodes(context.Background()); return ns }
func (p pgTopo) Leader() (registry.Node, bool) {
	c, err := p.reg.GetCluster(context.Background())
	if err != nil {
		return registry.Node{}, false
	}
	n, err := p.reg.GetNode(context.Background(), c.Leader)
	if err != nil {
		return registry.Node{}, false
	}
	return *n, true
}
func (p pgTopo) IsLeader() bool { l, ok := p.Leader(); return ok && l.ID == p.self }

// The boot decision against a real database: the recovery probe, the read-only read of the local
// record, the ping of a peer over mutual TLS and the leader marker.
func TestDecideBootAgainstPostgres(t *testing.T) {
	base := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	dsn := tempDatabase(t, base, "supavise_boot")
	reg, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if rec, err := InRecovery(ctx, dsn); err != nil || rec {
		t.Fatalf("a primary reports recovery=%v, %v", rec, err)
	}
	if rec, err := InRecovery(ctx, dsn+"&pool_max_conns=6"); err != nil || rec {
		t.Fatalf("a DSN with a pool parameter, as the daemon's has: recovery=%v, %v", rec, err)
	}
	bootScenario(ctx, t, reg, dsn, []string{"host=/nonexistent port=1 user=x connect_timeout=1", dsn}, func(*BootEnv) {})
}

// The same decisions over an in-memory registry, with the database calls replaced: they run on a
// development machine, where the Postgres test is skipped.
func TestDecideBootOverAMemoryRegistry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reg := registry.NewMemory()
	bootScenario(ctx, t, reg, "memory", []string{"memory"}, func(e *BootEnv) {
		e.probe = func(context.Context, []string, time.Duration) (string, bool, error) { return "memory", false, nil }
		e.openRegistry = func(context.Context, string) (registry.Registry, error) { return reg, nil }
	})
}

// bootScenario walks DecideBoot through the cases of 2.10.8 against registry reg, which holds the
// founder (this node) and gets a second node that answers pings over mutual TLS.
func bootScenario(ctx context.Context, t *testing.T, reg registry.Registry, dsn string, dsns []string, tweak func(*BootEnv)) {
	t.Helper()
	if err := reg.CreateProject(ctx, &registry.Project{Ref: "system", Name: "system"}); err != nil {
		t.Fatal(err)
	}

	// This node is the founder, with an identity in a temporary cluster directory.
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	conf := filepath.Join(t.TempDir(), "etc", "config.toml")
	ca, _ := NewCA(newSecrets(t))
	if _, err := EnsureFounder(ctx, reg, ca, cfg, conf, "v0.2.0", time.Now()); err != nil {
		t.Fatal(err)
	}

	// The peer n2 answers pings with whatever its test says.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	n2 := &registry.Node{Name: "second", State: registry.NodeActive, PeerAddr: ln.Addr().String()}
	if err := reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	key, _ := NewKey()
	iss, _ := ca.Issue(key.Public().(ed25519.PublicKey), n2.ID, time.Now(), time.Hour)
	if err := reg.SetNodeCert(ctx, n2.ID, iss.Serial); err != nil {
		t.Fatal(err)
	}
	creds, err := mesh.NewCredentials(iss.DER, key, ca.Cert())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	answer := peerapi.Ping{Node: "n2", Epoch: 1, Leader: "n1"}
	set := func(p peerapi.Ping) { mu.Lock(); answer = p; mu.Unlock() }
	mux := mesh.NewMux()
	mux.Handle("GET "+peerapi.PathPing, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		p := answer
		mu.Unlock()
		mesh.RespondJSON(w, 200, p)
	})
	peer := mesh.New(mesh.Options{Topology: pgTopo{reg, "n2"}, Creds: func() *mesh.Credentials { return creds }, Mux: mux})
	go peer.Serve(ctx, ln)

	env := func(m backup.EpochMarkerStore) BootEnv {
		e := BootEnv{Cfg: cfg, ConfigPath: conf, DSNs: dsns, Wait: 10 * time.Second, Marker: m, Log: quiet(),
			PeerTimeout: 3 * time.Second, MarkerTimeout: time.Second}
		tweak(&e)
		return e
	}
	// decide runs one boot decision under a budget of its own, so that a stall names its step.
	decide := func(step string, m backup.EpochMarkerStore) BootDecision {
		t.Helper()
		start := time.Now()
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		d, err := DecideBoot(sctx, env(m))
		t.Logf("%s: %s in %s", step, d.Role, time.Since(start).Round(time.Millisecond))
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return d
	}

	if d := decide("peer agrees", nil); d.Role != RoleLeader || !d.Joined || d.SelfID != "n1" || d.Epoch != 1 || d.DSN != dsn {
		t.Fatalf("peer agrees: %+v", d)
	}
	set(peerapi.Ping{Node: "n2", Epoch: 5, Leader: "n2"})
	if d := decide("peer holds a higher epoch", nil); d.Role != RoleFenced || d.Epoch != 5 || d.Leader != "n2" {
		t.Fatalf("peer holds a higher epoch: %+v", d)
	}
	set(peerapi.Ping{Node: "n2", Epoch: 1, Leader: "n1"})
	if d := decide("marker holds a higher epoch", fakeMarker{m: &backup.LeaderMarker{Epoch: 3, Leader: "n2", At: time.Now()}}); d.Role != RoleFenced || d.Epoch != 3 {
		t.Fatalf("marker holds a higher epoch: %+v", d)
	}
	// A marker that cannot be read is not evidence.
	if d := decide("marker unreadable", fakeMarker{err: fmt.Errorf("store down")}); d.Role != RoleLeader {
		t.Fatalf("marker unreadable: %+v", d)
	}
	// Nobody answers: the node starts, its own record shows no demotion.
	ln.Close()
	peer.Close()
	if d := decide("no peer answers", nil); d.Role != RoleLeader {
		t.Fatalf("no peer answers: %+v", d)
	}

	// A promotion the registry has not caught up with: the registry names n2, the marker names this node.
	if err := reg.SetLeader(ctx, "n2", 2); err != nil {
		t.Fatal(err)
	}
	if d := decide("a primary that nothing names", nil); d.Role != RoleFenced {
		t.Fatalf("a primary that nothing names: %+v", d)
	}
	d := decide("promoted", fakeMarker{m: &backup.LeaderMarker{Epoch: 3, Leader: "n1", At: time.Now()}})
	if d.Role != RoleLeader || d.Epoch != 3 {
		t.Fatalf("promoted: %+v", d)
	}
	planned, err := AssumeLeadership(ctx, reg, "n1", d.Epoch, time.Now())
	if err != nil || planned {
		t.Fatalf("assume: %v %v", planned, err)
	}
	cl, _ := reg.GetCluster(ctx)
	old, _ := reg.GetNode(ctx, "n2")
	if cl.Leader != "n1" || cl.Epoch != 3 || old.State != registry.NodeFenced {
		t.Fatalf("after the promotion: leader %s epoch %d, n2 %s", cl.Leader, cl.Epoch, old.State)
	}
}

// The read-only registry on the same database reads what the boot decision needs and refuses writes.
func TestProbeSystemAgainstPostgres(t *testing.T) {
	base := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dsn, rec, err := probeSystem(ctx, []string{"host=/nonexistent port=1 user=x connect_timeout=1", base}, 5*time.Second)
	if err != nil || dsn != base || rec {
		t.Fatalf("%q %v %v", dsn, rec, err)
	}
	if _, _, err := probeSystem(ctx, []string{"host=/nonexistent port=1 user=x connect_timeout=1"}, 100*time.Millisecond); err == nil {
		t.Fatal("an unreachable cluster answered")
	}
}

// A server leads when its system cluster is a primary and the registry in it names this node, which is
// what `node join --reset` asks before it retires a node.
func TestLeadsHereAgainstPostgres(t *testing.T) {
	base := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	dsn := tempDatabase(t, base, "supavise_leads")
	reg, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if err := reg.CreateProject(ctx, &registry.Project{Ref: "system", Name: "system"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	conf := filepath.Join(t.TempDir(), "etc", "config.toml")
	ca, _ := NewCA(newSecrets(t))
	if _, err := EnsureFounder(ctx, reg, ca, cfg, conf, "v0.2.0", time.Now()); err != nil {
		t.Fatal(err)
	}
	dsns := []string{"host=/nonexistent port=1 user=x connect_timeout=1", dsn}
	if got := LeadsHere(ctx, cfg, conf, dsns); got != LeadsYes {
		t.Fatalf("the founder, a primary that the registry names: %v", got)
	}
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetLeader(ctx, n2.ID, 2); err != nil {
		t.Fatal(err)
	}
	if got := LeadsHere(ctx, cfg, conf, dsns); got != LeadsNo {
		t.Fatalf("a primary whose registry names another leader: %v", got)
	}
	if got := LeadsHere(ctx, cfg, filepath.Join(t.TempDir(), "etc", "config.toml"), dsns); got != LeadsNo {
		t.Fatalf("a server with no cluster identity: %v", got)
	}
	// A database that does not answer is not an answer: with no data directory it cannot be a primary.
	if got := LeadsHere(ctx, cfg, conf, []string{"host=/nonexistent port=1 user=x connect_timeout=1"}); got != LeadsNo {
		t.Fatalf("a server whose database does not answer and holds no data: %v", got)
	}
}
