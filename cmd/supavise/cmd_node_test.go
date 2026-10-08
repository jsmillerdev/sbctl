package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// nodeTestEnv is a leader over an in-memory registry with a cluster of three nodes in it.
func nodeTestEnv(t *testing.T) (*nodeEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "example.test"
	cfg.Node.Name, cfg.Node.PeerAddress = "main", "203.0.113.5:7443"
	cfg.Backup.Backend = "s3://bucket/prefix"
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	return &nodeEnv{cfg: cfg, configPath: filepath.Join(t.TempDir(), "etc", "config.toml"), reg: registry.NewMemory(), sec: sec,
		now: time.Now, out: &out, errw: &errw, in: strings.NewReader("")}, &out, &errw
}

func TestNodeTokenPrintsTheTokenAndGivesTheFounderItsIdentity(t *testing.T) {
	env, out, errw := nodeTestEnv(t)
	ctx := context.Background()
	if err := runNodeToken(ctx, env, cluster.TokenOptions{Name: "second", Region: "eu-west-1", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	tok, err := cluster.ParseToken(out.String())
	if err != nil {
		t.Fatalf("stdout is not just the token: %q: %v", out.String(), err)
	}
	if tok.Leader != "203.0.113.5:7443" || tok.Name != "second" || tok.Region != "eu-west-1" {
		t.Fatalf("token %+v", tok)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout holds more than the token: %q", out.String())
	}
	for _, want := range []string{"cluster identity", "node join", "peer port"} {
		if !strings.Contains(errw.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errw.String())
		}
	}
	if !cluster.Joined(config.ClusterDir(env.configPath)) {
		t.Fatal("the founder has no identity")
	}
	if n, _ := env.reg.GetNode(context.Background(), "n1"); n.Name != "main" || n.CertSerial == "" || n.PeerAddr != "203.0.113.5:7443" {
		t.Fatalf("founder row %+v", n)
	}
	// A second token: no new identity, no restart notice.
	out.Reset()
	errw.Reset()
	if err := runNodeToken(ctx, env, cluster.TokenOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errw.String(), "cluster identity") {
		t.Errorf("the second token said the identity was made again:\n%s", errw.String())
	}
	// Refusals reach the operator in words.
	env.cfg.Backup.Backend = "file:///var/lib/supavise/backups"
	if err := runNodeToken(ctx, env, cluster.TokenOptions{}); err == nil || !strings.Contains(err.Error(), "S3-compatible") {
		t.Fatalf("file backend: %v", err)
	}
}

// A refused token leaves a server that never had a cluster identity without one: the daemon restarts
// into cluster mode when the identity appears, and it must not for a token that was not made.
func TestNodeTokenRefusalLeavesNoIdentity(t *testing.T) {
	env, out, _ := nodeTestEnv(t)
	env.cfg.Backup.Backend = "file:///var/lib/supavise/backups"
	if err := runNodeToken(context.Background(), env, cluster.TokenOptions{}); err == nil {
		t.Fatal("a token on a file backend")
	}
	if cluster.Joined(config.ClusterDir(env.configPath)) || out.Len() != 0 {
		t.Fatalf("identity %v, stdout %q", cluster.Joined(config.ClusterDir(env.configPath)), out.String())
	}
}

func seedCluster(t *testing.T, env *nodeEnv) {
	t.Helper()
	ctx := context.Background()
	reg := env.reg
	for _, n := range []registry.Node{
		{Name: "second", Region: "eu-west-1", Version: "v0.2.0", PeerAddr: "198.51.100.7:7443", PublicHost: "replica.example.test", State: registry.NodeActive},
		{Name: "third", Region: "us-west-2", State: registry.NodeJoining},
	} {
		if err := reg.CreateNode(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.UpdateNode(ctx, &registry.Node{ID: "n1", Name: "main", Region: "us-east-1", Version: "v0.2.0", PeerAddr: "203.0.113.5:7443"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []registry.Project{{Ref: "system", Name: "system"}, {Ref: "aaaaaaaaaaaaaaaaaaaa", Name: "a"}} {
		if err := reg.CreateProject(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.CreateReplica(ctx, &registry.Replica{Identifier: "aaaaaaaaaaaaaaaaaaaa-rr-eu-west-1-abc123", Ref: "aaaaaaaaaaaaaaaaaaaa", NodeID: "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetClusterName(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeLsAgainstAFakeRegistry(t *testing.T) {
	env, out, _ := nodeTestEnv(t)
	seedCluster(t, env)
	env.status = &cluster.Status{At: time.Now(), Node: "n1", Role: cluster.RoleLeader, Epoch: 1, Leader: "n1",
		Peers: []cluster.PeerStatus{{Node: "n2", Connected: true, RTTMillis: 12.4}, {Node: "n3", Connected: false}}}
	if err := runNodeLs(context.Background(), env, false, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"ID", "SESSION", "n1", "main (this node)", "leader", "second", "follower", "198.51.100.7:7443", "up 12 ms", "joining", "down"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	line := func(id string) string {
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, id+" ") {
				return l
			}
		}
		return ""
	}
	// Projects and replicas: n1 homes the two projects, n2 holds the replica.
	if !regexp.MustCompile(`\s2\s+0\s+this node$`).MatchString(line("n1")) {
		t.Errorf("n1 line: %q", line("n1"))
	}
	if !regexp.MustCompile(`\s0\s+1\s+up 12 ms$`).MatchString(line("n2")) {
		t.Errorf("n2 line: %q", line("n2"))
	}

	// A status that is old shows no sessions.
	env.status.At = time.Now().Add(-time.Hour)
	out.Reset()
	if err := runNodeLs(context.Background(), env, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "up 12 ms") || strings.Contains(out.String(), "this node)") {
		t.Errorf("a stale status was shown:\n%s", out.String())
	}

	out.Reset()
	if err := runNodeLs(context.Background(), env, false, true); err != nil {
		t.Fatal(err)
	}
	var j struct {
		Cluster string    `json:"cluster"`
		Epoch   int64     `json:"epoch"`
		Leader  string    `json:"leader"`
		Nodes   []nodeRow `json:"nodes"`
	}
	if err := json.Unmarshal(out.Bytes(), &j); err != nil || j.Cluster != "prod" || j.Leader != "n1" || len(j.Nodes) != 3 || j.Nodes[1].Replicas != 1 || j.Nodes[0].Role != "leader" || j.Nodes[2].Role != "-" {
		t.Fatalf("json %s: %+v, %v", out.String(), j, err)
	}

	out.Reset()
	if err := runNodeLs(context.Background(), env, true, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api.example.test", "studio.example.test", "pooler.example.test", "*.api.example.test", "203.0.113.5", "replica.example.test", "198.51.100.7"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--dns lacks %q:\n%s", want, out.String())
		}
	}
}

func TestNodeRm(t *testing.T) {
	env, out, errw := nodeTestEnv(t)
	seedCluster(t, env)
	ctx := context.Background()

	// An answer other than yes leaves the node.
	env.in = strings.NewReader("no\n")
	if err := runNodeRm(ctx, env, "second", false, cluster.RemoveOptions{Wait: 50 * time.Millisecond, Poll: 5 * time.Millisecond}); err == nil || !strings.Contains(err.Error(), "not removed") {
		t.Fatalf("declined: %v", err)
	}
	if n, _ := env.reg.GetNode(ctx, "n2"); n.State != registry.NodeActive {
		t.Fatal("the node was removed without a yes")
	}
	if !strings.Contains(errw.String(), "Remove node n2 (second)") {
		t.Fatalf("prompt: %q", errw.String())
	}
	// The replica stays: the removal times out and says what to do. The node has left by then.
	if err := runNodeRm(ctx, env, "n2", true, cluster.RemoveOptions{Wait: 50 * time.Millisecond, Poll: 5 * time.Millisecond}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("stuck replica: %v", err)
	}
	if n, _ := env.reg.GetNode(ctx, "n2"); n.State != registry.NodeLeft {
		t.Fatalf("the node is %s after its removal was asked for", n.State)
	}
	if err := runNodeRm(ctx, env, "nobody", true, cluster.RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "no node") {
		t.Fatalf("unknown node: %v", err)
	}
	if err := runNodeRm(ctx, env, "main", true, cluster.RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "leader") {
		t.Fatalf("the leader: %v", err)
	}
	// By name, with --force.
	env.in = strings.NewReader("y\n")
	if err := runNodeRm(ctx, env, "second", false, cluster.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if n, _ := env.reg.GetNode(ctx, "n2"); n.State != registry.NodeLeft {
		t.Fatalf("state %s", n.State)
	}
	if !strings.Contains(out.String(), "node n2 (second) was removed") {
		t.Fatalf("stdout: %q", out.String())
	}
}

func TestJoinInputs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("svj1.abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tokenInput([]string{"svj1.arg"}, "", nil); err != nil || got != "svj1.arg" {
		t.Fatalf("argument: %q %v", got, err)
	}
	if got, err := tokenInput(nil, file, nil); err != nil || strings.TrimSpace(got) != "svj1.abc" {
		t.Fatalf("file: %q %v", got, err)
	}
	if got, err := tokenInput(nil, "-", strings.NewReader("svj1.stdin\n")); err != nil || strings.TrimSpace(got) != "svj1.stdin" {
		t.Fatalf("stdin: %q %v", got, err)
	}
	if got, err := tokenInput([]string{"-"}, "", strings.NewReader("svj1.dash\n")); err != nil || strings.TrimSpace(got) != "svj1.dash" {
		t.Fatalf("- argument: %q %v", got, err)
	}
	for name, args := range map[string]struct {
		args []string
		file string
	}{"both": {[]string{"x"}, file}, "neither": {nil, ""}, "missing file": {nil, filepath.Join(dir, "none")}} {
		if _, err := tokenInput(args.args, args.file, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	cfg := config.Default()
	keyFile := filepath.Join(dir, "master.key")
	good := strings.Repeat("ab", 32) + "\n"
	if err := os.WriteFile(keyFile, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := masterKeyInput(context.Background(), cfg, keyFile, false, "", nil); err != nil || string(k) != good {
		t.Fatalf("key file: %q %v", k, err)
	}
	if k, err := masterKeyInput(context.Background(), cfg, "", false, "", nil); err != nil || k != nil {
		t.Fatalf("no key given: %q %v", k, err)
	}
	if err := os.WriteFile(keyFile, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := masterKeyInput(context.Background(), cfg, keyFile, false, "", nil); err == nil {
		t.Error("a key that is not a key")
	}
	if _, err := masterKeyInput(context.Background(), cfg, keyFile, true, "", nil); err == nil {
		t.Error("both key sources")
	}
	if _, err := masterKeyInput(context.Background(), cfg, "", true, "", nil); err == nil || !strings.Contains(err.Error(), "--passphrase-file") {
		t.Errorf("escrow without a passphrase file: %v", err)
	}
}

// `node join --reset` says what it will set aside and asks; a server that leads its cluster is not reset
// without --yes, whatever is typed.
func TestConfirmReset(t *testing.T) {
	data := []string{"/var/lib/supavise/projects/system/postgres/data", "/var/lib/supavise/projects/abc/postgres/data"}
	keep := 3 * 24 * time.Hour
	for _, tc := range []struct {
		name  string
		lead  cluster.Leadership
		yes   bool
		input string
		ok    bool
	}{
		{"a member that is told yes", cluster.LeadsNo, false, "y\n", true},
		{"a member that is told yes in words", cluster.LeadsNo, false, "YES\n", true},
		{"a member that is told no", cluster.LeadsNo, false, "n\n", false},
		{"a member and no answer", cluster.LeadsNo, false, "", false},
		{"a member with --yes", cluster.LeadsNo, true, "", true},
		{"the leader and a typed yes", cluster.LeadsYes, false, "y\n", false},
		{"the leader with --yes", cluster.LeadsYes, true, "", true},
		{"a server that cannot tell and a typed yes", cluster.LeadsUnknown, false, "y\n", false},
		{"a server that cannot tell with --yes", cluster.LeadsUnknown, true, "", true},
	} {
		var out bytes.Buffer
		err := confirmReset(&out, strings.NewReader(tc.input), tc.lead, data, keep, tc.yes)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
		text := out.String()
		for _, d := range data {
			if !strings.Contains(text, d) {
				t.Errorf("%s: the output does not list %s:\n%s", tc.name, d, text)
			}
		}
		if !strings.Contains(text, "kept 3 days") || strings.Contains(text, "LEADER") != tc.lead.MayLead() {
			t.Errorf("%s: the output:\n%s", tc.name, text)
		}
		if tc.lead == cluster.LeadsUnknown && !tc.yes && (err == nil || !strings.Contains(err.Error(), "could not be checked")) {
			t.Errorf("%s: the refusal does not say why: %v", tc.name, err)
		}
	}
	var out bytes.Buffer
	if err := confirmReset(&out, strings.NewReader("y\n"), cluster.LeadsNo, nil, keep, false); err != nil || !strings.Contains(out.String(), "no project data") {
		t.Errorf("a server with no data: %v\n%s", err, out.String())
	}
}

func TestClusterBlockRendersNodesReplicasAndFencing(t *testing.T) {
	lag := 0.8
	b := &clusterBlock{Name: "prod", Epoch: 3, Leader: "n1", Node: "n1", Role: "leader", Live: true,
		Nodes: []nodeRow{{ID: "n1", Name: "main", Role: "leader", State: "active", Region: "us-east-1", Version: "v0.2.0", Self: true},
			{ID: "n2", Name: "second", Role: "follower", State: "active", Version: "v0.2.0", Connected: ptr(true), RTTMillis: 12}},
		Replicas: []clusterReplicaRow{{Identifier: "a-rr-eu-west-1-abc123", Node: "n2", Status: "ACTIVE_HEALTHY", LagSeconds: &lag},
			{Identifier: "b-rr-eu-west-1-def456", Node: "n2", Status: "INIT_READ_REPLICA", Step: "3_initiated_read_replica_setup"}},
		Maintenance: &registry.Maintenance{Node: "n2", Until: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Reason: "upgrade"}}
	var w bytes.Buffer
	b.render(&w)
	for _, want := range []string{"cluster   prod: epoch 3, leader n1 (this node: n1, leader)", "session up, 12 ms", "lag 0.8 s", "(3_initiated_read_replica_setup)", "maintenance on n2 until 2026-10-08T12:00:00Z: upgrade"} {
		if !strings.Contains(w.String(), want) {
			t.Errorf("missing %q in:\n%s", want, w.String())
		}
	}
	if strings.Contains(w.String(), "not current") {
		t.Error("said the live view was stale")
	}
	b.Live = false
	w.Reset()
	b.render(&w)
	if !strings.Contains(w.String(), "live view is not current") {
		t.Errorf("a stale view says nothing:\n%s", w.String())
	}
	f := &clusterBlock{Fenced: &cluster.FencedRecord{Epoch: 4, Leader: "n2", Reason: "node n2 says node n2 leads at epoch 4"}}
	w.Reset()
	f.render(&w)
	if !strings.Contains(w.String(), "FENCED: node n2 says") || !strings.Contains(w.String(), "supavise node rejoin") {
		t.Errorf("fenced block:\n%s", w.String())
	}
}

func ptr[T any](v T) *T { return &v }

// A server that is not part of a cluster has no cluster block, and reading it touches no database.
func TestClusterStatusIsAbsentOnASingleServer(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	old := configPath
	configPath = filepath.Join(t.TempDir(), "config.toml")
	defer func() { configPath = old }()
	if b, err := clusterStatus(context.Background(), cfg); b != nil || err != nil {
		t.Fatalf("%+v, %v", b, err)
	}
	// A fenced node shows the record, whose database is stopped.
	if err := cluster.WriteFenced(cfg, cluster.FencedRecord{Epoch: 4, Leader: "n2", Reason: "replaced", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b, err := clusterStatus(context.Background(), cfg)
	if err != nil || b == nil || b.Fenced == nil || b.Epoch != 4 {
		t.Fatalf("%+v, %v", b, err)
	}
}

// ---- the commands, driven through the root command ----

// joinedHost is a server with a config file in a temporary directory and a cluster identity: the
// files that make cluster.Joined true. leads and the units are the test's: nothing real is asked.
type joinedHost struct {
	cfgPath string
	cfg     *config.Config
	data    string // a data directory a reset would set aside
	leads   bool
	unknown bool // leads cannot be told: the system cluster does not answer
	stopped int
	asked   int // how often the units were wanted

	dropped int      // how often the command became the supavise user
	opened  []bool   // the seeders the commands opened: joining or not
	events  []string // the order of the drop, the preflight, the seed and the close
	seedErr error    // what the seed answers
	boot    []peerapi.SystemBootstrap
}

// fakeStandby is the seeder the commands get: it records what they ask of it.
type fakeStandby struct{ h *joinedHost }

func (f fakeStandby) Preflight(context.Context) error {
	f.h.events = append(f.h.events, "preflight")
	return nil
}

func (f fakeStandby) Seed(_ context.Context, b peerapi.SystemBootstrap) error {
	f.h.events = append(f.h.events, "seed")
	f.h.boot = append(f.h.boot, b)
	return f.h.seedErr
}

func (f fakeStandby) Close() { f.h.events = append(f.h.events, "close") }

func newJoinedHost(t *testing.T) *joinedHost {
	t.Helper()
	dir := t.TempDir()
	h := &joinedHost{cfgPath: filepath.Join(dir, "etc", "config.toml")}
	if err := os.MkdirAll(filepath.Dir(h.cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "state_dir = \"" + filepath.Join(dir, "state") + "\"\nkey_path = \"" + filepath.Join(dir, "etc", "master.key") + "\"\n"
	if err := os.WriteFile(h.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	cdir := config.ClusterDir(h.cfgPath)
	if err := os.MkdirAll(cdir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, config.NodeCertFile), []byte("certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.data = cfg.Paths().PostgresData("system")
	if err := os.MkdirAll(h.data, 0o700); err != nil {
		t.Fatal(err)
	}
	// A host that converged: the commands that turn cluster features on wait for it.
	if err := hostsetup.WriteMarker(cfg.StateDir, hostsetup.Marker{Revision: hostsetup.Revision, Version: "test", At: time.Now()}); err != nil {
		t.Fatal(err)
	}

	oldPath, oldStop, oldLeads, oldOpen, oldDrop := configPath, stopLocalFn, leadsHere, openStandby, runAsSupavise
	configPath = h.cfgPath
	stopLocalFn = func(*config.Config, *slog.Logger) (func(context.Context) error, func(), error) {
		h.asked++
		return func(context.Context) error { h.stopped++; return nil }, func() {}, nil
	}
	leadsHere = func(context.Context, *config.Config, string, []string) cluster.Leadership {
		switch {
		case h.unknown:
			return cluster.LeadsUnknown
		case h.leads:
			return cluster.LeadsYes
		}
		return cluster.LeadsNo
	}
	openStandby = func(_ *slog.Logger, joining bool) standbySeeder {
		h.opened = append(h.opened, joining)
		return fakeStandby{h}
	}
	runAsSupavise = func() error {
		h.dropped++
		h.events = append(h.events, "drop")
		return nil
	}
	t.Cleanup(func() {
		configPath, stopLocalFn, leadsHere, openStandby, runAsSupavise = oldPath, oldStop, oldLeads, oldOpen, oldDrop
		rootCmd.SetIn(nil)
	})
	return h
}

// token is a join token that parses, has not expired and names a leader at leaderAddr.
func (h *joinedHost) token(t *testing.T, leaderAddr string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	tok := cluster.Token{V: 1, ID: "tokenid", Leader: leaderAddr, CAFpr: strings.Repeat("0", 64),
		Secret: base64.RawURLEncoding.EncodeToString([]byte("secret")), Exp: time.Now().Add(time.Hour).Unix()}
	if err := os.WriteFile(file, []byte(tok.Encode()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// deadAddr is an address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// unchanged fails when the server was touched: the units wanted, the identity or the data moved.
func (h *joinedHost) unchanged(t *testing.T, what string) {
	t.Helper()
	if h.asked != 0 || h.stopped != 0 {
		t.Errorf("%s: the units were wanted %d times and stopped %d times", what, h.asked, h.stopped)
	}
	if !cluster.Joined(config.ClusterDir(h.cfgPath)) {
		t.Errorf("%s: the identity was deleted", what)
	}
	if _, err := os.Stat(h.data); err != nil {
		t.Errorf("%s: the data was moved: %v", what, err)
	}
	if rec, _ := cluster.ReadFenced(h.cfg); rec != nil {
		t.Errorf("%s: the server was recorded as fenced or removed: %+v", what, rec)
	}
}

// `node join --reset` on a server that leads its cluster is refused without --yes, before anything is
// stopped or moved; on another server it lists the data and asks, and only a yes goes on.
func TestNodeJoinResetAsksBeforeItStopsAnything(t *testing.T) {
	h := newJoinedHost(t)
	file := h.token(t, deadAddr(t))

	// The leader, without --yes.
	h.leads = true
	out, err := run(t, "node", "join", "--reset", "--token-file", file)
	if err == nil || !strings.Contains(err.Error(), "leads its cluster") {
		t.Fatalf("a leader without --yes: %v\n%s", err, out)
	}
	for _, want := range []string{"--reset gives up this server's place", h.data, "LEADER"} {
		if !strings.Contains(out, want) {
			t.Errorf("the confirmation lacks %q:\n%s", want, out)
		}
	}
	h.unchanged(t, "leader without --yes")

	// Not the leader: asked, and any answer but yes leaves everything.
	h.leads = false
	rootCmd.SetIn(strings.NewReader("no\n"))
	out, err = run(t, "node", "join", "--reset", "--token-file", file)
	if err == nil || !strings.Contains(err.Error(), "not reset") || !strings.Contains(out, "Go ahead? [y/N]") {
		t.Fatalf("a no: %v\n%s", err, out)
	}
	h.unchanged(t, "an answer of no")

	// The token read from standard input leaves no answer to give, and the refusal says so.
	rootCmd.SetIn(strings.NewReader(mustRead(t, file)))
	if _, err := run(t, "node", "join", "--reset", "--token-file", "-"); err == nil || !strings.Contains(err.Error(), "pass --yes") {
		t.Fatalf("a token from standard input: %v", err)
	}
	h.unchanged(t, "token from stdin")

	// An expired token is refused before the question is asked.
	expired := filepath.Join(t.TempDir(), "expired")
	old := cluster.Token{V: 1, ID: "t", Leader: deadAddr(t), CAFpr: strings.Repeat("0", 64), Secret: base64.RawURLEncoding.EncodeToString([]byte("s")), Exp: time.Now().Add(-time.Hour).Unix()}
	if err := os.WriteFile(expired, []byte(old.Encode()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "node", "join", "--reset", "--token-file", expired, "--yes")
	if err == nil || !strings.Contains(err.Error(), "expired") || strings.Contains(out, "gives up") {
		t.Fatalf("an expired token: %v\n%s", err, out)
	}
	h.unchanged(t, "expired token")

	// A yes goes on: the leader named in the token is looked at before the server is stopped, finds
	// nobody, and the server is as it was.
	rootCmd.SetIn(strings.NewReader("yes\n"))
	out, err = run(t, "node", "join", "--reset", "--token-file", file)
	if err == nil || !strings.Contains(err.Error(), "cannot reach the leader") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("a yes with no leader: %v\n%s", err, out)
	}
	if h.asked != 1 || h.stopped != 0 {
		t.Errorf("after a yes: the units were wanted %d times and stopped %d times, want wanted once and not stopped", h.asked, h.stopped)
	}
	if _, err := os.Stat(h.data); err != nil || !cluster.Joined(config.ClusterDir(h.cfgPath)) {
		t.Errorf("a join that could not reach the leader changed the server: %v", err)
	}

	// --yes on the leader goes on the same way.
	h.leads, h.asked = true, 0
	if _, err := run(t, "node", "join", "--reset", "--token-file", file, "--yes"); err == nil || !strings.Contains(err.Error(), "cannot reach the leader") {
		t.Fatalf("--yes on a leader: %v", err)
	}
	if h.asked != 1 {
		t.Errorf("--yes on a leader: the units were wanted %d times", h.asked)
	}
}

// A server whose database does not answer may still lead its cluster: the reset is refused without --yes
// and says that leadership could not be checked. A fenced node does not lead, and its stopped database
// is not held against it.
func TestNodeJoinResetWhenLeadershipCannotBeChecked(t *testing.T) {
	h := newJoinedHost(t)
	file := h.token(t, deadAddr(t))
	h.unknown = true
	rootCmd.SetIn(strings.NewReader("y\n"))
	out, err := run(t, "node", "join", "--reset", "--token-file", file)
	if err == nil || !strings.Contains(err.Error(), "could not be checked") || !strings.Contains(out, "LEADER") || strings.Contains(out, "Go ahead?") {
		t.Fatalf("leadership unknown, a typed yes: %v\n%s", err, out)
	}
	h.unchanged(t, "leadership unknown")

	rec := cluster.FencedRecord{Epoch: 2, Leader: "n2", Reason: "replaced as leader", At: time.Now()}
	if err := cluster.WriteFenced(h.cfg, rec); err != nil {
		t.Fatal(err)
	}
	rootCmd.SetIn(strings.NewReader("no\n"))
	out, err = run(t, "node", "join", "--reset", "--token-file", file)
	if err == nil || !strings.Contains(err.Error(), "not reset") || !strings.Contains(out, "Go ahead? [y/N]") || strings.Contains(out, "LEADER") {
		t.Fatalf("a fenced node: %v\n%s", err, out)
	}
	if h.asked != 0 || h.stopped != 0 {
		t.Errorf("a fenced node that answered no: the units were wanted %d times and stopped %d times", h.asked, h.stopped)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A server that holds the identity of a node that was removed has nothing to confirm and no units to
// stop: the identity is revoked, and the join deletes it.
func TestNodeJoinResetOfARemovedNodeAsksNothing(t *testing.T) {
	h := newJoinedHost(t)
	if err := cluster.WriteFenced(h.cfg, cluster.FencedRecord{Reason: cluster.RemovedReason, Removed: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.leads = true // would be refused, if it were asked
	out, err := run(t, "node", "join", "--reset", "--token-file", h.token(t, deadAddr(t)))
	if err == nil || strings.Contains(out, "gives up") || strings.Contains(err.Error(), "leads its cluster") {
		t.Fatalf("a removed node: %v\n%s", err, out)
	}
	if h.asked != 0 || h.stopped != 0 {
		t.Errorf("units were wanted for a removed node: %d, %d", h.asked, h.stopped)
	}
	if cluster.Joined(config.ClusterDir(h.cfgPath)) {
		t.Error("the revoked identity is still there")
	}
}

// Without --reset a join refuses a server that holds an identity, and says what to run instead.
func TestNodeJoinWithoutResetRefusesAJoinedServer(t *testing.T) {
	h := newJoinedHost(t)
	_, err := run(t, "node", "join", "--token-file", h.token(t, deadAddr(t)))
	if err == nil || !strings.Contains(err.Error(), "already joined") || !strings.Contains(err.Error(), "--reset") {
		t.Fatalf("join on a joined server: %v", err)
	}
	h.unchanged(t, "join without --reset")
}

// `node rejoin` on a server that is not fenced stops nothing and changes nothing; its units are asked
// for only to hand to the rejoin.
func TestNodeRejoinOfAServerThatIsNotFenced(t *testing.T) {
	h := newJoinedHost(t)
	_, err := run(t, "node", "rejoin")
	if err == nil || !strings.Contains(err.Error(), "not fenced") {
		t.Fatalf("rejoin of a server that is not fenced: %v", err)
	}
	if h.stopped != 0 {
		t.Error("rejoin stopped the units of a server that is not fenced")
	}
	if !cluster.Joined(config.ClusterDir(h.cfgPath)) {
		t.Error("the identity was deleted")
	}
}

// The commands that turn the cluster features on wait for the host to be converged.
func TestNodeCommandsRefuseWhileTheHostIsBehind(t *testing.T) {
	h := newJoinedHost(t)
	if err := hostsetup.WriteMarker(h.cfg.StateDir, hostsetup.Marker{Revision: hostsetup.Revision - 1, Version: "old", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"node", "token"}, {"node", "join", "--token-file", h.token(t, deadAddr(t))}, {"node", "rejoin"}} {
		_, err := run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "system converge") {
			t.Errorf("supavise %s with the host behind: %v", strings.Join(args, " "), err)
		}
	}
	h.unchanged(t, "host behind")
}

// A command that sudo started becomes the supavise user once the inputs only root could read are
// read, and before it asks the seeder for anything or stops anything. The seeder is closed whatever the
// join did.
func TestNodeJoinBecomesTheSupaviseUserBeforeItSeedsOrStopsAnything(t *testing.T) {
	h := newJoinedHost(t)
	file := h.token(t, deadAddr(t))

	// A token file that cannot be read stops the command before it becomes anyone else.
	if _, err := run(t, "node", "join", "--token-file", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing token file was accepted")
	}
	if h.dropped != 0 {
		t.Fatalf("the command dropped its rights %d times before it had read its inputs", h.dropped)
	}

	// A reset of a server whose leader cannot be reached: the drop comes first, then the preflight (the
	// join asks it before it touches the server), and nothing is stopped.
	h.events = nil
	if _, err := run(t, "node", "join", "--reset", "--yes", "--token-file", file); err == nil || !strings.Contains(err.Error(), "cannot reach the leader") {
		t.Fatalf("join --reset against a dead leader: %v", err)
	}
	if got, want := strings.Join(h.events, " "), "drop preflight close"; got != want {
		t.Fatalf("order of events = %q, want %q", got, want)
	}
	if len(h.opened) != 2 || !h.opened[0] || !h.opened[1] {
		t.Fatalf("seeders opened = %v: a join opens a joining seeder", h.opened)
	}
	if h.stopped != 0 || !cluster.Joined(config.ClusterDir(h.cfgPath)) {
		t.Fatalf("the server was touched: stopped %d, identity kept %v", h.stopped, cluster.Joined(config.ClusterDir(h.cfgPath)))
	}
	if _, err := os.Stat(h.data); err != nil {
		t.Fatalf("the data was moved: %v", err)
	}
}

// A resumed join needs no token and no preflight: the seeder is opened, asked nothing and closed when
// there is no join to resume.
func TestNodeJoinResumeWithoutAJoinAsksTheSeederNothing(t *testing.T) {
	h := newJoinedHost(t)
	if _, err := run(t, "node", "join", "--resume"); err == nil || !strings.Contains(err.Error(), "no join to resume") {
		t.Fatalf("join --resume: %v", err)
	}
	if got, want := strings.Join(h.events, " "), "drop close"; got != want {
		t.Fatalf("order of events = %q, want %q", got, want)
	}
}

// A rejoin becomes the supavise user before it stops the units, opens a seeder that checks the backend
// (a rejoining server holds the cluster's settings already) and closes it.
func TestNodeRejoinBecomesTheSupaviseUserAndPreflightsBeforeAnythingChanges(t *testing.T) {
	h := newJoinedHost(t)
	if _, err := run(t, "node", "rejoin"); err == nil || !strings.Contains(err.Error(), "not fenced") {
		t.Fatalf("rejoin of a server that is not fenced: %v", err)
	}
	if got, want := strings.Join(h.events, " "), "drop preflight close"; got != want {
		t.Fatalf("order of events = %q, want %q", got, want)
	}
	if len(h.opened) != 1 || h.opened[0] {
		t.Fatalf("seeders opened = %v: a rejoin does not open a joining seeder", h.opened)
	}
	if h.stopped != 0 {
		t.Error("rejoin stopped the units of a server that is not fenced")
	}
}
