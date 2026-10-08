package cluster

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// freeWindow finds size consecutive loopback ports nothing listens on, the lowest between lo and hi.
func freeWindow(t testing.TB, lo, hi, size int) int {
	t.Helper()
	for range 200 {
		base := lo + rand.IntN(hi-lo)
		var lns []net.Listener
		ok := true
		for p := base; p < base+size; p++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				ok = false
				break
			}
			lns = append(lns, ln)
		}
		for _, ln := range lns {
			ln.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatalf("no window of %d free ports", size)
	return 0
}

// site is one server of a test cluster: its own directories, its own ports, a mesh manager on a
// loopback port. The sites share one in-memory registry, which stands for the replicated copy each
// node reads.
type site struct {
	t        *testing.T
	id       string
	cfg      *config.Config
	confPath string
	ln       net.Listener
	live     *Live
	mgr      *mesh.Manager
	fwd      *mesh.Forwarders
	store    *Store
	mux      *mesh.Mux
}

func newSite(t *testing.T, reg registry.Registry, id string) *site {
	t.Helper()
	root := t.TempDir()
	s := &site{t: t, id: id, cfg: config.Default(), confPath: filepath.Join(root, "etc", "config.toml"), mux: mesh.NewMux()}
	s.cfg.StateDir = filepath.Join(root, "state")
	s.cfg.KeyPath = filepath.Join(root, "etc", "master.key")
	s.cfg.Domain = "example.test"
	s.cfg.Backup.Backend = "s3://bucket/prefix"
	s.cfg.Ports.ReplicaBase = freeWindow(t, 20000, 25000, 8) - 3
	s.cfg.Ports.ProjectBase = freeWindow(t, 26000, 31000, 8) - 3
	s.cfg.Ports.SystemPostgres = freeWindow(t, 31000, 32000, 1)
	s.cfg.Ports.Studio = freeWindow(t, 32000, 32500, 1)
	s.cfg.Ports.PGMeta = freeWindow(t, 32500, 33000, 1)
	s.cfg.Ports.Realtime = freeWindow(t, 33000, 33500, 1)
	s.cfg.Ports.Storage = freeWindow(t, 33500, 34000, 1)
	s.cfg.Ports.Imgproxy = freeWindow(t, 34000, 34500, 1)
	s.cfg.Ports.EdgeRuntime = freeWindow(t, 34500, 35000, 1)
	s.cfg.Listen.Admin = fmt.Sprintf("127.0.0.1:%d", freeWindow(t, 35000, 35500, 1))
	var err error
	if s.ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.ln.Close() })
	s.cfg.Node.PeerAddress = s.ln.Addr().String()
	return s
}

// start builds the node's membership and mesh from the credentials in its cluster directory and
// runs them. recovery is what the node's system cluster says.
func (s *site) start(ctx context.Context, reg registry.Registry, role Role, inRecovery bool) {
	s.t.Helper()
	var err error
	if s.store, err = OpenStore(config.ClusterDir(s.confPath)); err != nil {
		s.t.Fatal(err)
	}
	s.live = NewLive(LiveOptions{Cfg: s.cfg, Reg: reg, SelfID: s.id, Boot: BootDecision{Role: role, SelfID: s.id}, Poll: 20 * time.Millisecond,
		InRecovery: func(context.Context) (bool, error) { return inRecovery, nil }, Log: quiet()})
	s.live.Refresh(ctx)
	s.mgr = mesh.New(mesh.Options{
		Topology: s.live, Creds: s.store.Creds, Mux: s.mux, Log: quiet(),
		Authz:     &mesh.Authorizer{Cfg: s.cfg, Topology: s.live, Source: reg},
		PingEvery: 100 * time.Millisecond, Tick: 50 * time.Millisecond, DialDelay: 150 * time.Millisecond,
		RevokeGrace: 200 * time.Millisecond,
		OnPing:      func(node string, p peerapi.Ping, _ time.Duration) { s.live.ObserveEpoch(node, p.Epoch, p.Leader) },
		PingInfo: func() peerapi.Ping {
			return peerapi.Ping{Node: s.id, Epoch: s.live.Epoch(), Version: "v0.2.0"}
		},
	})
	s.fwd = &mesh.Forwarders{Cfg: s.cfg, Topology: s.live, Source: reg, Dialer: s.mgr, Log: quiet(), Interval: 50 * time.Millisecond,
		Fenced: func() bool { return s.live.Role() == RoleFenced }}
	go s.mgr.Serve(ctx, s.ln)
	go s.mgr.Run(ctx)
	go s.fwd.Run(ctx)
	go s.live.Run(ctx)
}

// leaderFixture is the founder with its authority, serving the peer API on a loopback port.
type leaderFixture struct {
	*site
	reg  *registry.Memory
	sec  *secrets.AESGCM
	ca   *CA
	auth *Authority
	rep  *Reports
	clk  *fakeClock
}

type fakeClock struct {
	mu sync.Mutex
	d  time.Duration
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().Add(c.d) }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.d += d
	c.mu.Unlock()
}

var testPins = map[string]string{"postgres": "postgres-17.11.0.004-r1"}

func newLeader(t *testing.T) *leaderFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := registry.NewMemory()
	s := newSite(t, reg, "n1")
	// The master key file is hex, as `system init` writes it.
	raw := make([]byte, 32)
	if _, err := crand.Read(raw); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.KeyPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfg.KeyPath, []byte(hex.EncodeToString(raw)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ca, err := NewCA(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CreateProject(ctx, &registry.Project{Ref: "system", Name: "system", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{}
	if _, err := EnsureFounder(ctx, reg, ca, s.cfg, s.confPath, "v0.2.0", clk.Now()); err != nil {
		t.Fatal(err)
	}
	s.start(ctx, reg, RoleLeader, false)
	rep := NewReports()
	auth := &Authority{Reg: reg, CA: ca, Secrets: sec, Cfg: s.cfg, Topology: s.live, Log: quiet(), Version: "v0.2.0", Pins: testPins, Now: clk.Now,
		MasterKey: func() ([]byte, error) { return os.ReadFile(s.cfg.KeyPath) }}
	auth.Changed = func(ctx context.Context) { s.live.Refresh(ctx) }
	api := &PeerAPI{Authority: auth, Topology: s.live, Cfg: s.cfg, Reports: rep, Now: clk.Now,
		Ping: func() peerapi.Ping {
			return peerapi.Ping{Node: "n1", Epoch: s.live.Epoch(), Leader: "n1", Version: "v0.2.0"}
		}}
	api.Register(s.mux)
	return &leaderFixture{site: s, reg: reg, sec: sec, ca: ca, auth: auth, rep: rep, clk: clk}
}

// join runs a joiner against the leader and returns its site; seed is called in place of the
// real seeding.
func (l *leaderFixture) joiner(t *testing.T, id string) *site {
	t.Helper()
	return newSite(t, l.reg, id)
}

func (l *leaderFixture) token(t *testing.T, o TokenOptions) Token {
	t.Helper()
	enc, err := l.auth.IssueToken(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ParseToken(enc)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (j *site) joinOptions(tok Token, name string, seed SeedFunc) JoinOptions {
	return JoinOptions{
		Cfg: j.cfg, ConfigPath: j.confPath, Token: tok, Name: name, Region: "eu-west-1", Address: j.ln.Addr().String(),
		PublicHost: "second.example.test", Version: "v0.2.0", Pins: testPins, Seed: seed, Log: quiet(),
		Streaming: func(context.Context) (string, error) { return "0/3000100", nil },
	}
}

func okSeed(*testing.T) (SeedFunc, *[]peerapi.SystemBootstrap) {
	var got []peerapi.SystemBootstrap
	return func(_ context.Context, b peerapi.SystemBootstrap) error { got = append(got, b); return nil }, &got
}

// The whole join over real TLS between two in-process nodes: the token, the pin, the proof, the
// certificate, the master key, the cluster settings, the forwarder that carries the standby's
// connection while it seeds, and the confirmation. Then the two nodes forward a Postgres
// connection both ways and refuse what the registry does not allow.
func TestJoinAndForwardBothWays(t *testing.T) {
	l := newLeader(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := l.joiner(t, "n2")
	tok := l.token(t, TokenOptions{Region: "eu-west-1", TTL: time.Hour})
	echoSystem(t, l.cfg.PortsFor("system", 0).Postgres, "system@leader")

	var seeded []peerapi.SystemBootstrap
	var viaForwarder string
	seed := func(_ context.Context, b peerapi.SystemBootstrap) error {
		seeded = append(seeded, b)
		// While the standby is seeded, its connection to the canonical system port reaches the leader.
		got, err := roundTrip(j.cfg.PortsFor("system", 0).Postgres, "hello")
		if err != nil {
			return err
		}
		viaForwarder = got
		return nil
	}
	res, err := Join(ctx, j.joinOptions(tok, "second", seed))
	if err != nil {
		t.Fatal(err)
	}
	if res.NodeID != "n2" || len(seeded) != 1 || seeded[0].Identifier == "" || seeded[0].Leader != "n1" || seeded[0].Epoch != 1 {
		t.Fatalf("result %+v, seeded %+v", res, seeded)
	}
	if viaForwarder != "system@leader:hello" {
		t.Fatalf("the standby's connection during the seed: %q", viaForwarder)
	}
	if !registryHasIdentifier(seeded[0].Identifier) {
		t.Fatalf("system replica identifier %q", seeded[0].Identifier)
	}

	// The registry: the node is active with the serial of the certificate the joiner holds, the
	// system replica row exists and is done, the token is spent.
	n2, err := l.reg.GetNode(ctx, "n2")
	if err != nil || n2.State != registry.NodeActive || n2.Name != "second" || n2.Region != "eu-west-1" || n2.Version != "v0.2.0" || n2.PeerAddr != j.ln.Addr().String() {
		t.Fatalf("node row %+v, %v", n2, err)
	}
	creds, err := LoadCredentials(config.ClusterDir(j.confPath))
	if err != nil || creds.NodeID != "n2" || creds.Serial != n2.CertSerial {
		t.Fatalf("credentials %+v, %v", creds, err)
	}
	if rs, _ := l.reg.ListReplicasOn(ctx, "n2"); len(rs) != 1 || rs[0].Ref != "system" || rs[0].Origin != registry.ReplicaSystem || rs[0].InitStep != registry.ReplicaStepDone {
		t.Fatalf("replicas of n2: %+v", rs)
	}
	if _, err := l.reg.UseJoinToken(ctx, tok.ID, time.Now()); !errors.Is(err, registry.ErrTokenUsed) {
		t.Fatalf("the token after the join: %v", err)
	}

	// On the joiner's disk: the key, the identity, the cluster settings; the state file is gone.
	wantKey, _ := os.ReadFile(l.cfg.KeyPath)
	if got, err := os.ReadFile(j.cfg.KeyPath); err != nil || string(got) != string(wantKey) {
		t.Fatalf("master key %q, %v", got, err)
	}
	for _, f := range []struct {
		path string
		mode os.FileMode
	}{{j.cfg.KeyPath, 0o600}, {filepath.Join(config.ClusterDir(j.confPath), config.NodeKeyFile), 0o600},
		{filepath.Join(config.ConfigDDir(j.confPath), config.ClusterConfigFile), 0o600}} {
		if fi, err := os.Stat(f.path); err != nil || fi.Mode().Perm() != f.mode {
			t.Errorf("%s: %v, %v", f.path, fi, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(config.ConfigDDir(j.confPath), config.ClusterConfigFile)); !strings.Contains(string(b), "[backup]") {
		t.Errorf("10-cluster.toml: %q", b)
	}
	if _, err := os.Stat(filepath.Join(config.ClusterDir(j.confPath), JoinStateFile)); !os.IsNotExist(err) {
		t.Error("join.json is still there")
	}
	// The cluster file loads as configuration on the joiner: the settings are the leader's.
	merged, err := config.Load(j.confPath)
	if err != nil || merged.Backup.Backend != "s3://bucket/prefix" {
		t.Fatalf("merged config: %v, %v", merged, err)
	}

	// Now the two nodes talk. n1 leads; n2 follows.
	l.reg.SetNodeCert(ctx, "n1", l.store.Creds().Serial)
	j.start(ctx, l.reg, RoleFollower, true)
	eventually(t, "a session between the nodes", func() bool { return l.mgr.Connected("n2") && j.mgr.Connected("n1") })

	l.project(t, "aaaaaaaaaaaaaaaaaaaa", 1, "n1")
	l.project(t, "bbbbbbbbbbbbbbbbbbbb", 2, "n1")
	echoSystem(t, l.cfg.PortsFor("aaaaaaaaaaaaaaaaaaaa", 1).Postgres, "A@n1")
	echoSystem(t, j.cfg.PortsFor("bbbbbbbbbbbbbbbbbbbb", 2).Postgres, "B@n2")
	if err := l.reg.SetProjectNode(ctx, "bbbbbbbbbbbbbbbbbbbb", "n2", 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "A's port on n2 to reach n1", func() bool {
		got, err := roundTrip(j.cfg.PortsFor("aaaaaaaaaaaaaaaaaaaa", 1).Postgres, "q")
		return err == nil && got == "A@n1:q"
	})
	eventually(t, "B's port on n1 to reach n2", func() bool {
		got, err := roundTrip(l.cfg.PortsFor("bbbbbbbbbbbbbbbbbbbb", 2).Postgres, "q")
		return err == nil && got == "B@n2:q"
	})
	// The shared services answer on the follower through the leader.
	echoSystem(t, l.cfg.Ports.Studio, "studio@n1")
	eventually(t, "Studio on n2 to reach n1", func() bool {
		got, err := roundTrip(j.cfg.Ports.Studio, "s")
		return err == nil && got == "studio@n1:s"
	})

	// What the registry does not allow is refused: a follower is asked for a shared service, the
	// leader is asked for a project that lives on the follower.
	for _, tc := range []struct {
		name string
		from *site
		to   string
		h    mesh.Header
	}{
		{"a service on the follower", l.site, "n2", mesh.Header{T: mesh.StreamForward, Kind: mesh.KindStudio}},
		{"a project homed on the follower, asked of the leader", j, "n1", mesh.Header{T: mesh.StreamForward, Kind: mesh.KindPostgres, Ref: "bbbbbbbbbbbbbbbbbbbb"}},
		{"a project homed on the leader, asked of the follower", l.site, "n2", mesh.Header{T: mesh.StreamForward, Kind: mesh.KindPostgres, Ref: "aaaaaaaaaaaaaaaaaaaa"}},
		{"an unknown project", j, "n1", mesh.Header{T: mesh.StreamForward, Kind: mesh.KindPostgREST, Ref: "zzzzzzzzzzzzzzzzzzzz"}},
	} {
		st, err := tc.from.mgr.Dial(ctx, tc.to, tc.h)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_ = st.SetReadDeadline(time.Now().Add(3 * time.Second))
		if n, err := st.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Errorf("%s: got %d bytes, %v", tc.name, n, err)
		}
		st.Close()
	}

	// Peer API calls both ways, and the leader's reports intake.
	var ping peerapi.Ping
	if err := j.mgr.Call(ctx, "n1", "GET", peerapi.PathPing, nil, &ping); err != nil || ping.Node != "n1" || ping.Leader != "n1" {
		t.Fatalf("ping n1: %+v, %v", ping, err)
	}
	var cc peerapi.ClusterConfig
	if err := j.mgr.Call(ctx, "n1", "GET", peerapi.PathConfig, nil, &cc); err != nil || !strings.Contains(cc.TOML, "[backup]") || cc.Revision == "" {
		t.Fatalf("config: %+v, %v", cc, err)
	}
	if err := j.mgr.Call(ctx, "n1", "POST", peerapi.PathReport, peerapi.Report{Node: "n2", Epoch: 1, Instances: []peerapi.InstanceStatus{{Identifier: "x-rr-eu-west-1-abc123", Ref: "x"}}}, nil); err != nil {
		t.Fatalf("report: %v", err)
	}
	if in, _, ok := l.rep.Instance("x-rr-eu-west-1-abc123"); !ok || in.Ref != "x" {
		t.Fatalf("report not stored: %+v %v", in, ok)
	}
	var re *mesh.RemoteError
	if err := j.mgr.Call(ctx, "n1", "POST", peerapi.PathReport, peerapi.Report{Node: "n1"}, nil); !errors.As(err, &re) || re.Status != 403 {
		t.Fatalf("a report in another node's name: %v", err)
	}
	if err := l.mgr.Call(ctx, "n2", "POST", peerapi.PathReport, peerapi.Report{Node: "n1"}, nil); err == nil {
		t.Fatal("a follower accepted reports")
	}

	// Removing the node cuts it off.
	if err := l.reg.SetNodeState(ctx, "n2", registry.NodeLeft); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the removed node to lose its session", func() bool { return !l.mgr.Connected("n2") })
}

func registryHasIdentifier(id string) bool { return registry.ValidReplicaIdentifier(id) }

func echoSystem(t *testing.T, port int, prefix string) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				_, _ = c.Write(append([]byte(prefix+":"), buf[:n]...))
			}()
		}
	}()
}

func roundTrip(port int, msg string) (string, error) {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	out, err := io.ReadAll(c)
	return string(out), err
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

func (l *leaderFixture) project(t *testing.T, ref string, seq int, home string) {
	t.Helper()
	if err := l.reg.CreateProject(context.Background(), &registry.Project{Ref: ref, Name: ref[:4], Seq: seq, NodeID: home}); err != nil {
		t.Fatal(err)
	}
}

// Every way a join can be refused, and that a refusal costs the token nothing unless the token is
// the problem.
func TestJoinRefusals(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	seed, _ := okSeed(t)

	join := func(j *site, tok Token, name string) error {
		_, err := Join(ctx, j.joinOptions(tok, name, seed))
		return err
	}
	wantErr := func(what string, err error, parts ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: joined", what)
		}
		for _, p := range parts {
			if !strings.Contains(err.Error(), p) {
				t.Errorf("%s: %q lacks %q", what, err, p)
			}
		}
	}
	nodes := func() int { ns, _ := l.reg.ListNodes(ctx); return len(ns) }

	// A wrong pin: the leader is not the one the token names. The leader is never sent anything.
	tok := l.token(t, TokenOptions{})
	bad := tok
	bad.CAFpr = strings.Repeat("0", 64)
	wantErr("wrong pin", join(l.joiner(t, "n2"), bad, "second"), "pins")
	if nodes() != 1 {
		t.Fatal("a node was created for a wrong pin")
	}

	// A token the leader never made, and a token with another secret.
	forged := tok
	forged.ID = "doesnotexist1"
	wantErr("unknown token", join(l.joiner(t, "n2"), forged, "second"), "not valid")
	wrongSecret := tok
	wrongSecret.Secret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	wantErr("bad HMAC", join(l.joiner(t, "n2"), wrongSecret, "second"), "not valid")
	if nodes() != 1 {
		t.Fatal("a node was created for a bad proof")
	}

	// Name rules: invalid, taken, and not the one the token admits.
	wantErr("invalid name", join(l.joiner(t, "n2"), tok, "Not A Name"), "not a node name")
	wantErr("the name of the founder", join(l.joiner(t, "n2"), tok, l.cfg.NodeName()), "already in the cluster")
	named := l.token(t, TokenOptions{Name: "only-this"})
	wantErr("another name than the token's", join(l.joiner(t, "n2"), named, "something-else"), "admits the node")
	if nodes() != 1 {
		t.Fatal("a node was created for a bad name")
	}

	// Version skew: refused, token unspent.
	skewed := l.joiner(t, "n2")
	o := skewed.joinOptions(tok, "second", seed)
	o.Version = "v0.9.0"
	_, err := Join(ctx, o)
	wantErr("version skew", err, "version skew")
	o = skewed.joinOptions(tok, "second", seed)
	o.Pins = map[string]string{"postgres": "postgres-18.0.0-r0"}
	_, err = Join(ctx, o)
	wantErr("postgres major", err, "Postgres")
	if nodes() != 1 {
		t.Fatal("a node was created for a version the cluster cannot take")
	}

	// A master key that is not the cluster's is caught before anything is sent.
	o = skewed.joinOptions(tok, "second", seed)
	o.MasterKey = []byte(strings.Repeat("ab", 32))
	_, err = Join(ctx, o)
	wantErr("another master key", err, "not the one this cluster was made with")

	// None of that spent the token: a good joiner uses it.
	if _, err := Join(ctx, l.joiner(t, "n2").joinOptions(tok, "second", seed)); err != nil {
		t.Fatalf("the token was spent by refusals: %v", err)
	}
	// Used: refused, and nothing is created.
	wantErr("a used token", join(l.joiner(t, "n3"), tok, "third"), "used already")
	if nodes() != 2 {
		t.Fatalf("%d nodes", nodes())
	}

	// Expired: the leader's clock is past the token's expiry.
	short := l.token(t, TokenOptions{TTL: time.Minute})
	l.clk.Advance(2 * time.Minute)
	wantErr("an expired token", join(l.joiner(t, "n3"), short, "third"), "expired")
	if nodes() != 2 {
		t.Fatalf("%d nodes", nodes())
	}
	// And the joiner's own check of the expiry in the token, before it connects.
	o = l.joiner(t, "n3").joinOptions(short, "third", seed)
	o.Now = func() time.Time { return time.Now().Add(time.Hour) }
	_, err = Join(ctx, o)
	wantErr("expired in the joiner's view", err, "expired")

	// A follower is not asked: not the leader.
	l.auth.Topology = cluster2Follower{l.auth.Topology}
	wantErr("not the leader", join(l.joiner(t, "n3"), l.token2(t), "third"), "not the leader")
}

// token2 makes a token without the leader check (IssueToken refuses on a follower).
func (l *leaderFixture) token2(t *testing.T) Token {
	t.Helper()
	id := newTokenID()
	secret := tokenSecret(l.sec, id)
	if err := l.reg.CreateJoinToken(context.Background(), &registry.JoinToken{ID: id, SecretHash: SecretHash(secret), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return Token{V: 1, ID: id, Leader: l.cfg.PeerAddr(), CAFpr: l.ca.Fingerprint(), Secret: base64.RawURLEncoding.EncodeToString(secret), Exp: time.Now().Add(time.Hour).Unix()}
}

type cluster2Follower struct{ mesh.Topology }

func (cluster2Follower) IsLeader() bool { return false }
