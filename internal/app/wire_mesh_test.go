package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// meshWire is a Wire over an in-memory registry whose node holds a cluster identity.
func meshWire(t *testing.T) (*Wire, *cluster.CA, string, *registry.Memory) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = filepath.Join(root, "state")
	cfg.KeyPath = filepath.Join(root, "etc", "master.key")
	cfg.Node.PeerListen = freeAddr(t)
	cfg.Node.PeerAddress = cfg.Node.PeerListen
	cfg.Backup.Backend = "s3://bucket/prefix"
	conf := filepath.Join(root, "etc", "config.toml")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.NewMemory()
	ca, err := cluster.NewCA(sec)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := reg.CreateProject(ctx, &registry.Project{Ref: "system", Name: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.EnsureFounder(ctx, reg, ca, cfg, conf, "v0.2.0", time.Now()); err != nil {
		t.Fatal(err)
	}
	node := &lifecycle.Node{Registry: reg, Secrets: sec, Cfg: cfg}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newWire(cfg, log, node, Options{ConfigPath: conf, Version: "v0.2.0"}, &api.Deps{}, &proxy.Options{})
	return w, ca, conf, reg
}

// A server with a cluster identity starts the peer server, the sessions and the forwarders, answers
// a peer's ping and an anonymous joiner's challenge, and provides what the other hooks read.
func TestWireMeshStartsTheClusterOfAJoinedNode(t *testing.T) {
	w, ca, _, reg := meshWire(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Provide(w, cluster.BootDecision{Role: cluster.RoleLeader, Joined: true, SelfID: "n1", Epoch: 1, Leader: "n1"})
	if err := wireMesh(ctx, w); err != nil {
		t.Fatal(err)
	}
	m, ok := Get[cluster.Membership](w)
	if !ok || !m.IsLeader() || m.Self().ID != "n1" {
		t.Fatalf("membership %v %v", m, ok)
	}
	if _, ok := Get[mesh.Mesh](w); !ok {
		t.Fatal("no mesh provided")
	}
	if _, ok := Get[*cluster.Reporter](w); !ok {
		t.Fatal("no reporter provided for the replica agent to add to")
	}
	patterns := strings.Join(mesh.DefaultMux.Patterns(), "\n")
	for _, want := range []string{peerapi.PathPing, peerapi.PathJoin, peerapi.PathJoinConfirm, peerapi.PathRejoin, peerapi.PathCertsRenew, peerapi.PathConfig, peerapi.PathCerts, peerapi.PathReport} {
		if !strings.Contains(patterns, want) {
			t.Errorf("the peer API lacks %s:\n%s", want, patterns)
		}
	}
	names := map[string]bool{}
	for _, r := range w.runners {
		names[r.name] = true
	}
	for _, want := range []string{"peer server", "mesh sessions", "forwarders", "membership", "reports", "certificate renewal", "leader upkeep", "cluster status", "role watch"} {
		if !names[want] {
			t.Errorf("no worker %q (have %v)", want, names)
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	w.start(g, gctx)

	// A second node, registered by the leader, reaches the leader with its certificate.
	key, _ := cluster.NewKey()
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	iss, err := ca.Issue(key.Public().(ed25519.PublicKey), n2.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetNodeCert(ctx, n2.ID, iss.Serial); err != nil {
		t.Fatal(err)
	}
	creds, err := mesh.NewCredentials(iss.DER, key, ca.Cert())
	if err != nil {
		t.Fatal(err)
	}
	live, _ := Get[*cluster.Live](w)
	eventually(t, "the leader to know the second node", func() bool { live.Refresh(ctx); return len(live.Nodes()) == 2 })
	admit := mesh.AdmitFromNodes(func() []registry.Node { ns, _ := reg.ListNodes(ctx); return ns })
	var ping peerapi.Ping
	eventually(t, "a ping to be answered", func() bool {
		c, err := mesh.DialClient(ctx, w.Cfg.PeerListen(), "n1", mesh.ClientTLS(func() *mesh.Credentials { return creds }, "n1", admit, time.Now))
		if err != nil {
			return false
		}
		defer c.Close()
		return c.Call(ctx, "GET", peerapi.PathPing, nil, &ping) == nil
	})
	if ping.Node != "n1" || ping.Leader != "n1" || ping.Epoch != 1 || ping.Version != "v0.2.0" || ping.Health == "" || ping.Time.IsZero() {
		t.Fatalf("ping %+v", ping)
	}
	var cc peerapi.ClusterConfig
	c, err := mesh.DialClient(ctx, w.Cfg.PeerListen(), "n1", mesh.ClientTLS(func() *mesh.Credentials { return creds }, "n1", admit, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "GET", peerapi.PathConfig, nil, &cc); err != nil || !strings.Contains(cc.TOML, "[backup]") {
		t.Fatalf("config %+v, %v", cc, err)
	}
	c.Close()
	// An anonymous caller gets the challenge and nothing else.
	pin := ca.Fingerprint()
	anon, err := mesh.DialClient(ctx, w.Cfg.PeerListen(), "n1", mesh.PinnedTLS(pin, nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	var ch peerapi.JoinChallenge
	if err := anon.Call(ctx, "GET", peerapi.PathJoin, nil, &ch); err != nil || len(ch.Nonce) == 0 {
		t.Fatalf("challenge %+v, %v", ch, err)
	}
	if err := anon.Call(ctx, "GET", peerapi.PathPing, nil, &ping); !errors.Is(err, mesh.ErrRefused) {
		t.Fatalf("an anonymous ping: %v", err)
	}
	anon.Close()

	// The leader's status file appears for `supavise status`.
	eventually(t, "the cluster status file", func() bool {
		s, _ := cluster.ReadStatus(w.Cfg)
		return s != nil && s.Role == cluster.RoleLeader && s.Node == "n1"
	})

	cancel()
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the workers ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the workers did not stop")
	}
}

// Without a cluster identity the hook starts nothing; the identity appearing stops the daemon so
// that it restarts as the leader of a cluster.
func TestWireMeshWatchesForAClusterIdentity(t *testing.T) {
	old := identityPoll
	identityPoll = 20 * time.Millisecond
	defer func() { identityPoll = old }()
	w := testWire(t)
	w.Options.ConfigPath = filepath.Join(t.TempDir(), "etc", "config.toml")
	if err := wireMesh(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if _, ok := Get[mesh.Mesh](w); ok {
		t.Fatal("a single server got a mesh")
	}
	if m, _ := Get[cluster.Membership](w); !m.IsLeader() {
		t.Fatal("a single server is not its own leader")
	}
	g, ctx := errgroup.WithContext(context.Background())
	w.start(g, ctx)
	time.Sleep(100 * time.Millisecond)
	if err := os.MkdirAll(config.ClusterDir(w.Options.ConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.ClusterDir(w.Options.ConfigPath), config.NodeCertFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRoleChanged) {
			t.Fatalf("the watcher ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not notice the identity")
	}
}

func TestDecideBootForASingleServerTouchesNothing(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := decideBoot(context.Background(), cfg, Options{ConfigPath: filepath.Join(t.TempDir(), "config.toml")}, log)
	if err != nil || d.Role != cluster.RoleLeader || d.Joined || d.DSN != "" {
		t.Fatalf("%+v, %v", d, err)
	}
	dsns := RegistryDSNs(cfg)
	if len(dsns) != 2 || dsns[0] == dsns[1] || !strings.Contains(dsns[0], fmt.Sprint(cfg.Ports.SystemPostgres)) || !strings.Contains(dsns[1], fmt.Sprint(cfg.ReplicaBase())) {
		t.Fatalf("registry DSNs %v", dsns)
	}
}

// A fenced node answers 503 with the reason, records that it is fenced and stops when the record goes.
func TestServeFencedAnswers503AndEndsWhenRejoined(t *testing.T) {
	old := fencedPoll
	fencedPoll = 20 * time.Millisecond
	defer func() { fencedPoll = old }()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Supervisor = config.SupervisorExec
	addr := freeAddr(t)
	cfg.Listen.HTTP = addr
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	boot := cluster.BootDecision{Role: cluster.RoleFenced, Joined: true, SelfID: "n1", Epoch: 4, Leader: "n2", Reason: "node n2 says node n2 leads at epoch 4"}
	go func() { done <- serveFenced(context.Background(), cfg, Options{}, log, boot) }()

	var body string
	eventually(t, "the 503 answer", func() bool {
		resp, err := http.Get("http://" + addr + "/rest/v1/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") != ""
	})
	if !strings.Contains(body, "fenced") || !strings.Contains(body, "node n2 says") {
		t.Fatalf("body %q", body)
	}
	rec, err := cluster.ReadFenced(cfg)
	if err != nil || rec == nil || rec.Epoch != 4 || rec.Leader != "n2" {
		t.Fatalf("record %+v, %v", rec, err)
	}
	if err := cluster.ClearFenced(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRoleChanged) {
			t.Fatalf("serveFenced ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveFenced did not end after the rejoin")
	}
}

func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type nopSupervisor struct{ units.Supervisor }

func (nopSupervisor) Status(_ context.Context, unit string) (units.Status, error) {
	return units.Status{Unit: unit, State: units.StateInactive}, nil
}

// A follower whose own row says left retires itself: it stops what runs, sets its data aside,
// deletes its identity, records that it was removed and ends the daemon so that it restarts down.
func TestRetireWhenRemoved(t *testing.T) {
	old := retirePoll
	retirePoll = 10 * time.Millisecond
	defer func() { retirePoll = old }()
	ctx := context.Background()
	w, ca, conf, reg := meshWire(t)
	w.Node.Supervisor = nopSupervisor{}
	n2 := &registry.Node{Name: "second", State: registry.NodeActive}
	if err := reg.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	_ = ca
	if err := os.MkdirAll(w.Cfg.Paths().PostgresData("system"), 0o700); err != nil {
		t.Fatal(err)
	}
	live := cluster.NewLive(cluster.LiveOptions{Cfg: w.Cfg, Reg: reg, SelfID: n2.ID, Boot: cluster.BootDecision{Role: cluster.RoleFollower, SelfID: n2.ID},
		InRecovery: func(context.Context) (bool, error) { return true, nil }})
	live.Refresh(ctx)
	done := make(chan error, 1)
	go func() { done <- retireWhenRemoved(ctx, w, live, w.Log) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("a node that is active retired: %v", err)
	default:
	}
	if err := reg.SetNodeState(ctx, n2.ID, registry.NodeLeft); err != nil {
		t.Fatal(err)
	}
	live.Refresh(ctx)
	select {
	case err := <-done:
		if !errors.Is(err, ErrRoleChanged) {
			t.Fatalf("retirement ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a removed node did not retire")
	}
	if cluster.Joined(config.ClusterDir(conf)) {
		t.Fatal("the node kept its cluster identity")
	}
	rec, err := cluster.ReadFenced(w.Cfg)
	if err != nil || rec == nil || !rec.Removed {
		t.Fatalf("record %+v, %v", rec, err)
	}
	if _, err := os.Stat(w.Cfg.Paths().PostgresData("system")); !os.IsNotExist(err) {
		t.Fatal("the data was not set aside")
	}
	// The leader does not retire, whatever its row says.
	lead := cluster.NewLive(cluster.LiveOptions{Cfg: w.Cfg, Reg: reg, SelfID: n2.ID, Boot: cluster.BootDecision{Role: cluster.RoleLeader, SelfID: n2.ID},
		InRecovery: func(context.Context) (bool, error) { return false, nil }})
	lead.Refresh(ctx)
	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := retireWhenRemoved(cctx, w, lead, w.Log); err != nil {
		t.Fatalf("a leader retired: %v", err)
	}
}
