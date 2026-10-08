package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
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
	// The replica stays: the removal times out and says what to do.
	if err := runNodeRm(ctx, env, "n2", true, cluster.RemoveOptions{Wait: 50 * time.Millisecond, Poll: 5 * time.Millisecond}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("stuck replica: %v", err)
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
		leads bool
		yes   bool
		input string
		ok    bool
	}{
		{"a member that is told yes", false, false, "y\n", true},
		{"a member that is told yes in words", false, false, "YES\n", true},
		{"a member that is told no", false, false, "n\n", false},
		{"a member and no answer", false, false, "", false},
		{"a member with --yes", false, true, "", true},
		{"the leader and a typed yes", true, false, "y\n", false},
		{"the leader with --yes", true, true, "", true},
	} {
		var out bytes.Buffer
		err := confirmReset(&out, strings.NewReader(tc.input), tc.leads, data, keep, tc.yes)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
		text := out.String()
		for _, d := range data {
			if !strings.Contains(text, d) {
				t.Errorf("%s: the output does not list %s:\n%s", tc.name, d, text)
			}
		}
		if !strings.Contains(text, "kept 3 days") || strings.Contains(text, "LEADER") != tc.leads {
			t.Errorf("%s: the output:\n%s", tc.name, text)
		}
	}
	var out bytes.Buffer
	if err := confirmReset(&out, strings.NewReader("y\n"), false, nil, keep, false); err != nil || !strings.Contains(out.String(), "no project data") {
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
