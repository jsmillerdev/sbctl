package cluster

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/units"
)

// A join that stopped after the certificate was issued continues without a token.
func TestJoinResumes(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	tok := l.token(t, TokenOptions{})
	calls := 0
	failing := func(context.Context, peerapi.SystemBootstrap) error {
		calls++
		return errors.New("disk full")
	}
	_, err := Join(ctx, j.joinOptions(tok, "second", failing))
	if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("a join whose seed fails: %v", err)
	}
	dir := config.ClusterDir(j.confPath)
	st, err := ReadJoinState(dir)
	if err != nil || st.NodeID != "n2" || st.System.Identifier == "" || st.Leader != l.cfg.PeerAddr() || st.System.ReplicationPassword != "replication-secret" {
		t.Fatalf("join state %+v, %v", st, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, JoinStateFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("join.json holds the replication password and must be 0600: %v, %v", fi, err)
	}
	if n, _ := l.reg.GetNode(ctx, "n2"); n.State != registry.NodeJoining {
		t.Fatalf("the node is %s", n.State)
	}
	// A second join with the token is refused: the server holds a certificate already.
	if _, err := Join(ctx, j.joinOptions(tok, "second", failing)); err == nil || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("joining again: %v", err)
	}

	var seeded []peerapi.SystemBootstrap
	o := j.joinOptions(Token{}, "", func(_ context.Context, b peerapi.SystemBootstrap) error { seeded = append(seeded, b); return nil })
	o.Resume = true
	res, err := Join(ctx, o)
	if err != nil || res.NodeID != "n2" || len(seeded) != 1 || seeded[0] != st.System {
		t.Fatalf("resume: %+v, %v, seeded %v", res, err, seeded)
	}
	if n, _ := l.reg.GetNode(ctx, "n2"); n.State != registry.NodeActive {
		t.Fatalf("after the resume the node is %s", n.State)
	}
	if _, err := os.Stat(filepath.Join(dir, JoinStateFile)); !os.IsNotExist(err) {
		t.Error("join.json is still there")
	}
	// Nothing to resume now.
	if _, err := Join(ctx, o); err == nil || !strings.Contains(err.Error(), "no join to resume") {
		t.Fatalf("resume with nothing to resume: %v", err)
	}
	if _, err := Join(ctx, JoinOptions{Cfg: j.cfg, ConfigPath: j.confPath}); err == nil {
		t.Fatal("a join without a way to seed")
	}
}

// At the authority: a challenge works once, a proof for another request fails and spends nothing.
func TestChallengesAndProofs(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	tok := l.token(t, TokenOptions{})
	secret, _ := tokenSecretBytes(tok)
	key, _ := NewKey()
	csr, _ := NewCSR(key, "second")
	build := func() peerapi.JoinRequest {
		ch := l.auth.Challenge()
		return peerapi.JoinRequest{TokenID: tok.ID, Nonce: ch.Nonce, Proof: Proof(secret, ch.Nonce, csr, "second"), CSR: csr, Name: "second", Region: "eu-west-1", PeerAddr: "127.0.0.1:7443", Version: "v0.2.0", ArtifactPins: testPins}
	}
	code := func(err error) string {
		var ce *Error
		if errors.As(err, &ce) {
			return ce.Code
		}
		return ""
	}

	// A nonce the leader never gave.
	req := build()
	req.Nonce = []byte("made up")
	if _, err := l.auth.Join(ctx, req); code(err) != "bad_nonce" {
		t.Fatalf("made-up nonce: %v", err)
	}
	// A proof over another name, another request.
	req = build()
	req.Name = "other"
	if _, err := l.auth.Join(ctx, req); code(err) != "bad_proof" {
		t.Fatalf("proof for another name: %v", err)
	}
	req = build()
	other, _ := NewCSR(key, "other")
	req.CSR = other
	if _, err := l.auth.Join(ctx, req); code(err) != "bad_proof" {
		t.Fatalf("proof for another request: %v", err)
	}
	// A nonce is spent by the attempt, good or not: the same request cannot be sent twice.
	good := build()
	if _, err := l.auth.Join(ctx, good); err != nil {
		t.Fatalf("a good request after refusals: %v", err)
	}
	if _, err := l.auth.Join(ctx, good); code(err) != "bad_nonce" {
		t.Fatalf("replayed request: %v", err)
	}
	// An expired nonce.
	req = build()
	l.clk.Advance(3 * time.Minute)
	if _, err := l.auth.Join(ctx, req); code(err) != "bad_nonce" {
		t.Fatalf("expired nonce: %v", err)
	}
	// The table of nonces stays bounded.
	for range 3000 {
		l.auth.Challenge()
	}
	l.auth.mu.Lock()
	n := len(l.auth.nonces)
	l.auth.mu.Unlock()
	if n > 1024 {
		t.Fatalf("%d nonces kept", n)
	}
}

func tokenSecretBytes(t Token) ([]byte, error) { return base64.RawURLEncoding.DecodeString(t.Secret) }

// A fenced node asks to come back, is put to joining, rebuilds, confirms, and ends active with
// its old data set aside and its fenced record gone.
func TestRejoin(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	// The old leader n2 is fenced by a failover: its data is stale.
	if err := l.reg.SetNodeState(ctx, "n2", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	l.live.Refresh(ctx)
	for _, ref := range []string{"system", "aaaaaaaaaaaaaaaaaaaa"} {
		d := j.cfg.Paths().PostgresData(ref)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rejoin := RejoinOptions{Cfg: j.cfg, ConfigPath: j.confPath, Version: "v0.2.0", Pins: testPins,
		StopLocal: func(context.Context) error { return nil },
		Streaming: func(context.Context) (string, error) { return "0/4000000", nil }, Log: quiet()}
	rejoin.Seed = seed

	// A node that is not fenced has nothing to rejoin.
	if _, err := Rejoin(ctx, rejoin); err == nil || !strings.Contains(err.Error(), "not fenced") {
		t.Fatalf("rejoin without a fenced record: %v", err)
	}
	if err := WriteFenced(j.cfg, FencedRecord{Epoch: 2, Leader: "n1", Reason: "replaced", At: time.Now(), Peers: map[string]string{"n1": l.cfg.PeerAddr()}}); err != nil {
		t.Fatal(err)
	}
	stopped := 0
	rejoin.StopLocal = func(context.Context) error { stopped++; return nil }
	var boot []peerapi.SystemBootstrap
	rejoin.Seed = func(_ context.Context, b peerapi.SystemBootstrap) error { boot = append(boot, b); return nil }
	res, err := Rejoin(ctx, rejoin)
	if err != nil {
		t.Fatal(err)
	}
	if res.NodeID != "n2" || stopped != 1 || len(boot) != 1 || len(res.Diverged) != 2 {
		t.Fatalf("result %+v, stopped %d, seeded %v", res, stopped, boot)
	}
	for _, d := range res.Diverged {
		if _, err := os.Stat(d); err != nil || !strings.Contains(d, ".diverged-") {
			t.Errorf("diverged %s: %v", d, err)
		}
	}
	if _, err := os.Stat(j.cfg.Paths().PostgresData("system")); !os.IsNotExist(err) {
		t.Error("the stale system data is still in place")
	}
	if n, _ := l.reg.GetNode(ctx, "n2"); n.State != registry.NodeActive {
		t.Fatalf("node n2 is %s", n.State)
	}
	if rec, _ := ReadFenced(j.cfg); rec != nil {
		t.Fatal("the fenced record is still there")
	}
	if rs, _ := l.reg.ListReplicasOn(ctx, "n2"); len(rs) != 1 || rs[0].Ref != "system" || boot[0].Identifier != rs[0].Identifier {
		t.Fatalf("system replica rows %+v for bootstrap %+v", rs, boot)
	}

	// A node that was removed does not rejoin: it joins again with a token.
	if err := WriteFenced(j.cfg, FencedRecord{Reason: RemovedReason, Removed: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Rejoin(ctx, rejoin); err == nil || !strings.Contains(err.Error(), "removed from the cluster") {
		t.Fatalf("rejoin of a removed node: %v", err)
	}
	_ = ClearFenced(j.cfg)

	// A node that is active is not let back in as if fenced.
	if err := WriteFenced(j.cfg, FencedRecord{Epoch: 3, Leader: "n1", At: time.Now(), Peers: map[string]string{"n1": l.cfg.PeerAddr()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Rejoin(ctx, rejoin); err == nil || !strings.Contains(err.Error(), "only a fenced node rejoins") {
		t.Fatalf("rejoin of an active node: %v", err)
	}
	// With no address for the leader, and none given, it says so.
	if err := WriteFenced(j.cfg, FencedRecord{Epoch: 3, Leader: "n1", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Rejoin(ctx, rejoin); err == nil || !strings.Contains(err.Error(), "--leader") {
		t.Fatalf("rejoin without an address: %v", err)
	}
}

// A certificate near its end is replaced: the new serial is recorded by the leader, the follower waits
// to see it, and then uses the new certificate and writes it.
func TestRenewal(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	j.start(cctx, l.reg, RoleFollower, true)
	eventually(t, "a session", func() bool { return j.mgr.Connected("n1") })

	r := &Renewer{Store: j.store, Self: j.live.Self, Leader: func() (string, bool) { return "n1", true }, IsLeader: func() bool { return false },
		RPC: j.mgr, Log: quiet(), Grace: 10 * time.Millisecond, Wait: 5 * time.Second}
	// Plenty of life left: nothing happens.
	if done, err := r.Once(ctx); err != nil || done {
		t.Fatalf("renewal of a fresh certificate: %v, %v", done, err)
	}
	// Replace the certificate with one that has ten days left, as the leader's record.
	old := j.store.Creds()
	pub := old.Cert.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	short, err := l.ca.Issue(pub, "n2", time.Now(), 10*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.reg.SetNodeCert(ctx, "n2", short.Serial); err != nil {
		t.Fatal(err)
	}
	if err := j.store.Replace(short.DER); err != nil {
		t.Fatal(err)
	}
	j.live.Refresh(ctx)
	if got := j.store.Creds().Serial; got != short.Serial {
		t.Fatalf("serial %s", got)
	}

	done, err := r.Once(ctx)
	if err != nil || !done {
		t.Fatalf("renewal: %v, %v", done, err)
	}
	now := j.store.Creds()
	n2, _ := l.reg.GetNode(ctx, "n2")
	if now.Serial == short.Serial || now.Serial != n2.CertSerial || now.NotAfter.Sub(time.Now()) < 300*24*time.Hour {
		t.Fatalf("after the renewal: serial %s (registry %s), until %s", now.Serial, n2.CertSerial, now.NotAfter)
	}
	onDisk, err := LoadCredentials(config.ClusterDir(j.confPath))
	if err != nil || onDisk.Serial != now.Serial {
		t.Fatalf("on disk: %+v, %v", onDisk, err)
	}
	// The renewed node still reaches the leader, over a new connection with the new certificate.
	var ping peerapi.Ping
	if err := j.mgr.Call(ctx, "n1", "GET", peerapi.PathPing, nil, &ping); err != nil || ping.Node != "n1" {
		t.Fatalf("ping after the renewal: %+v, %v", ping, err)
	}

	// The leader renews its own certificate through the authority.
	lr := &Renewer{Store: l.store, Self: l.live.Self, IsLeader: func() bool { return true }, Authority: l.auth, Grace: time.Millisecond, Wait: time.Second}
	l.clk.Advance(0)
	if done, err := lr.Once(ctx); err != nil || done {
		t.Fatalf("the leader's fresh certificate: %v, %v", done, err)
	}
	lshort, _ := l.ca.Issue(l.store.Creds().Cert.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey), "n1", time.Now(), 5*24*time.Hour)
	_ = l.reg.SetNodeCert(ctx, "n1", lshort.Serial)
	if err := l.store.Replace(lshort.DER); err != nil {
		t.Fatal(err)
	}
	l.live.Refresh(ctx)
	if done, err := lr.Once(ctx); err != nil || !done {
		t.Fatalf("the leader renews itself: %v, %v", done, err)
	}
	if l.store.Creds().Serial == lshort.Serial {
		t.Fatal("the leader kept its short certificate")
	}

	// A renewal for a different node's certificate is refused by the store.
	other, _ := l.ca.Issue(pub, "n5", time.Now(), time.Hour)
	if err := j.store.Replace(other.DER); err == nil {
		t.Fatal("the store took a certificate for another node")
	}
}

func TestReapJoiningRemovesStuckNodes(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	failing := func(context.Context, peerapi.SystemBootstrap) error { return errors.New("stuck") }
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", failing)); err == nil {
		t.Fatal("expected the seed to fail")
	}
	if got, _ := l.auth.ReapJoining(ctx); len(got) != 0 {
		t.Fatalf("reaped a fresh joiner: %v", got)
	}
	l.clk.Advance(JoiningTimeout + time.Minute)
	got, err := l.auth.ReapJoining(ctx)
	if err != nil || len(got) != 1 || got[0] != "n2" {
		t.Fatalf("reaped %v, %v", got, err)
	}
	n, _ := l.reg.GetNode(ctx, "n2")
	if n.State != registry.NodeLeft {
		t.Fatalf("state %s", n.State)
	}
	if rs, _ := l.reg.ListReplicasOn(ctx, "n2"); len(rs) != 0 {
		t.Fatalf("replica rows kept: %v", rs)
	}
	// Its certificate is no longer admitted anywhere.
	if _, err := mesh.AdmitFromNodes(func() []registry.Node { ns, _ := l.reg.ListNodes(ctx); return ns })("n2", n.CertSerial); !errors.Is(err, mesh.ErrNotAdmitted) {
		t.Fatalf("admission of a reaped node: %v", err)
	}
}

func TestEnsureFounderFillsTheRowOnce(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	cfg := config.Default()
	cfg.Node.Name, cfg.Node.Region, cfg.Node.PeerAddress, cfg.PublicIP = "main", "us-east-1", "203.0.113.5:7443", "203.0.113.5"
	conf := filepath.Join(t.TempDir(), "etc", "config.toml")
	ca, _ := NewCA(newSecrets(t))
	made, err := EnsureFounder(ctx, reg, ca, cfg, conf, "v0.2.0", time.Now())
	if err != nil || !made {
		t.Fatalf("first: %v %v", made, err)
	}
	n, _ := reg.GetNode(ctx, "n1")
	if n.Name != "main" || n.Region != "us-east-1" || n.PeerAddr != "203.0.113.5:7443" || n.PublicHost != "203.0.113.5" || n.Version != "v0.2.0" || n.State != registry.NodeActive {
		t.Fatalf("row %+v", n)
	}
	creds, err := LoadCredentials(config.ClusterDir(conf))
	if err != nil || creds.NodeID != "n1" || creds.Serial != n.CertSerial {
		t.Fatalf("creds %+v, %v", creds, err)
	}
	made, err = EnsureFounder(ctx, reg, ca, cfg, conf, "v0.2.0", time.Now())
	if err != nil || made {
		t.Fatalf("second: %v %v", made, err)
	}
	if again, _ := reg.GetNode(ctx, "n1"); again.CertSerial != n.CertSerial {
		t.Fatal("the certificate was replaced")
	}
}

func TestIssueTokenRules(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	fail := func(what string, o TokenOptions, part string) {
		t.Helper()
		if _, err := l.auth.IssueToken(ctx, o); err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("%s: %v, want %q", what, err, part)
		}
	}
	fail("ttl too short", TokenOptions{TTL: time.Second}, "--ttl")
	fail("ttl too long", TokenOptions{TTL: 30 * 24 * time.Hour}, "--ttl")
	fail("bad name", TokenOptions{Name: "Bad Name"}, "not a node name")
	fail("a taken name", TokenOptions{Name: l.cfg.NodeName()}, "already in the cluster")
	fail("a region that does not exist", TokenOptions{Region: "mars-1"}, "not a region")
	l.cfg.Backup.Backend = "file:///var/lib/supavise/backups"
	fail("file backend", TokenOptions{}, "S3-compatible")
	l.cfg.Backup.Backend = "s3://bucket/prefix"
	addr := l.cfg.Node.PeerAddress
	l.cfg.Node.PeerAddress, l.cfg.PublicIP = "", ""
	fail("no address", TokenOptions{}, "address")
	l.cfg.Node.PeerAddress = addr
	cl, _ := l.reg.GetCluster(ctx)
	if cl.Name != "" {
		t.Fatalf("the cluster is named before any token: %q", cl.Name)
	}
	tok := l.token(t, TokenOptions{Name: "later", Region: "eu-west-1"})
	if cl, _ = l.reg.GetCluster(ctx); cl.Name != "example.test" {
		t.Fatalf("cluster name %q", cl.Name)
	}
	if tok.Leader != addr || tok.CAFpr != l.ca.Fingerprint() || tok.Name != "later" || tok.Region != "eu-west-1" {
		t.Fatalf("token %+v", tok)
	}
	row, err := l.reg.GetJoinToken(ctx, tok.ID)
	if err != nil || row.NodeName != "later" || len(row.SecretHash) != 32 || row.UsedAt != nil {
		t.Fatalf("token row %+v, %v", row, err)
	}
	// The registry keeps a hash, not the secret, and a leak of the row does not give the proof key.
	secret, _ := tokenSecretBytes(tok)
	if strings.Contains(string(row.SecretHash), string(secret)) || string(row.SecretHash) == string(secret) {
		t.Fatal("the registry holds the secret")
	}
	// A node that is not the leader does not issue.
	l.auth.Topology = cluster2Follower{l.auth.Topology}
	fail("not the leader", TokenOptions{}, "not the leader")
}

// ---- fencing the local clusters ----

type fakeSup struct {
	mu      sync.Mutex
	running map[string]bool
	stopped []string
	failOn  string
}

func (f *fakeSup) Render(context.Context, units.Spec) error { return nil }
func (f *fakeSup) Start(context.Context, string) error      { return nil }
func (f *fakeSup) Remove(context.Context, string) error     { return nil }
func (f *fakeSup) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if unit == f.failOn {
		return errors.New("stop refused")
	}
	f.stopped = append(f.stopped, unit)
	delete(f.running, unit)
	return nil
}
func (f *fakeSup) Status(_ context.Context, unit string) (units.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[unit] {
		return units.Status{Unit: unit, State: units.StateActive}, nil
	}
	return units.Status{Unit: unit, State: units.StateInactive}, nil
}

func TestFenceLocalStopsPrimariesAndRemovesRunScripts(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	sup := &fakeSup{running: map[string]bool{}}
	for _, ref := range []string{"system", "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"} {
		for _, svc := range []string{"postgres", "gotrue", "postgrest"} {
			p := filepath.Join(cfg.Paths().Project(ref), svc+".run")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if ref != "bbbbbbbbbbbbbbbbbbbb" { // project b is not running
				sup.running[config.UnitName(svc, ref)] = true
			}
		}
	}
	sup.running[config.UnitName("studio", "")] = true
	sup.running[config.UnitName("realtime", "")] = true

	stopped, err := FenceLocal(context.Background(), cfg, sup, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(stopped, ",") != "aaaaaaaaaaaaaaaaaaaa,system" {
		t.Fatalf("stopped %v", stopped)
	}
	if len(sup.running) != 0 {
		t.Fatalf("still running: %v", sup.running)
	}
	for _, ref := range []string{"system", "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"} {
		for _, svc := range []string{"postgres", "gotrue", "postgrest"} {
			if _, err := os.Stat(filepath.Join(cfg.Paths().Project(ref), svc+".run")); !os.IsNotExist(err) {
				t.Errorf("%s/%s.run is still there", ref, svc)
			}
		}
	}
	// Twice is fine; a unit that will not stop is reported after the rest were tried.
	if _, err := FenceLocal(context.Background(), cfg, sup, quiet()); err != nil {
		t.Fatal(err)
	}
	sup.running[config.UnitName("postgres", "aaaaaaaaaaaaaaaaaaaa")] = true
	sup.running[config.UnitName("studio", "")] = true
	sup.failOn = config.UnitName("postgres", "aaaaaaaaaaaaaaaaaaaa")
	sup.stopped = nil
	if _, err := FenceLocal(context.Background(), cfg, sup, quiet()); err == nil || !strings.Contains(err.Error(), "stop refused") {
		t.Fatalf("a unit that does not stop: %v", err)
	}
	if len(sup.stopped) != 1 || sup.stopped[0] != config.UnitName("studio", "") {
		t.Fatalf("the rest was not tried: %v", sup.stopped)
	}
}

func TestRetireClearsTheNodeFromTheCluster(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	conf := filepath.Join(t.TempDir(), "etc", "config.toml")
	ca, _ := NewCA(newSecrets(t))
	key, _ := NewKey()
	iss, _ := ca.Issue(key.Public().(ed25519.PublicKey), "n2", time.Now(), time.Hour)
	if err := SaveIdentity(config.ClusterDir(conf), key, iss.DER, ca.DER()); err != nil {
		t.Fatal(err)
	}
	cl := filepath.Join(config.ConfigDDir(conf), config.ClusterConfigFile)
	if err := writeFile(cl, []byte("domain='x'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Paths().PostgresData("system"), 0o700); err != nil {
		t.Fatal(err)
	}
	stopped := false
	moved, err := Retire(context.Background(), cfg, conf, func(context.Context) error { stopped = true; return nil }, time.Now())
	if err != nil || !stopped || len(moved) != 1 {
		t.Fatalf("retire: %v %v %v", moved, stopped, err)
	}
	if Joined(config.ClusterDir(conf)) {
		t.Fatal("the node still looks joined")
	}
	if _, err := os.Stat(cl); !os.IsNotExist(err) {
		t.Fatal("the cluster settings are still there")
	}
	rec, err := ReadFenced(cfg)
	if err != nil || rec == nil || !rec.Removed || rec.Reason != RemovedReason {
		t.Fatalf("the record of the removal: %+v, %v", rec, err)
	}
	// The boot decision keeps a removed node down, with the reason.
	d, err := DecideBoot(context.Background(), BootEnv{Cfg: cfg, ConfigPath: conf})
	if err != nil || d.Role != RoleFenced || d.Reason != RemovedReason {
		t.Fatalf("boot of a removed node: %+v, %v", d, err)
	}
}

// ---- the resolver ----

func TestAWSResolverUsesTheCurrentAddressesOfThePeer(t *testing.T) {
	f := awsfake.New(t)
	f.AddInstance(awsfake.Instance{ID: "i-0peer", PrivateIP: "10.77.0.5", PublicIP: "203.0.113.9"})
	f.AddInstance(awsfake.Instance{ID: "i-0stopped", PrivateIP: "10.77.0.6"})
	if _, err := f.Client().EC2.StopInstances(context.Background(), awsapi.StopInstancesInput{InstanceIDs: []string{"i-0stopped"}, Force: true}); err != nil {
		t.Fatal(err)
	}
	_ = f.Order()
	ec2 := f.Client().EC2
	self := registry.Node{ID: "n1", Provider: registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-0self", Region: "eu-west-1"}}}
	r := &AWSResolver{EC2: ec2, Self: func() registry.Node { return self }}
	peer := registry.Node{ID: "n2", PeerAddr: "198.51.100.1:7443", Provider: registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-0peer", Region: "eu-west-1"}}}

	got, err := r.Addrs(context.Background(), peer)
	if err != nil || strings.Join(got, ",") != "10.77.0.5:7443,203.0.113.9:7443" {
		t.Fatalf("same region: %v, %v", got, err)
	}
	peer.Provider.AWS.Region = "us-east-1"
	r2 := &AWSResolver{EC2: ec2, Self: func() registry.Node { return self }}
	if got, _ = r2.Addrs(context.Background(), peer); strings.Join(got, ",") != "203.0.113.9:7443,10.77.0.5:7443" {
		t.Fatalf("another region: %v", got)
	}
	// Answers are cached, so a dial that retries does not call EC2 each time.
	before := len(f.Calls())
	if _, err := r.Addrs(context.Background(), peer); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls()) != before {
		t.Fatal("the resolver asked EC2 again within its TTL")
	}
	// No AWS identity, no address; a stopped instance, no address; an API error is returned.
	if got, err := r.Addrs(context.Background(), registry.Node{ID: "n3"}); err != nil || got != nil {
		t.Fatalf("no identity: %v, %v", got, err)
	}
	if got, err := r.Addrs(context.Background(), registry.Node{ID: "n4", Provider: registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-0stopped"}}}); err != nil || len(got) != 0 {
		t.Fatalf("stopped: %v, %v", got, err)
	}
	f.Deny("ec2", "DescribeInstances")
	r3 := &AWSResolver{EC2: ec2}
	if _, err := r3.Addrs(context.Background(), peer); err == nil {
		t.Fatal("a denied call returned no error")
	}
}

// The mesh falls back to the resolver when the registry's address does not answer.
func TestManagerFallsBackToTheResolver(t *testing.T) {
	l := newLeader(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	o := j.joinOptions(l.token(t, TokenOptions{}), "second", seed)
	o.Address = "127.0.0.1:1" // a stale hint: nothing listens there
	if _, err := Join(ctx, o); err != nil {
		t.Fatal(err)
	}
	// The leader dials n2 at its recorded address (wrong); the resolver supplies the right one.
	l.store = mustStore(t, l.confPath)
	res := fixedResolver{"n2": j.ln.Addr().String()}
	mgr := mesh.New(mesh.Options{Topology: l.live, Creds: l.store.Creds, Mux: mesh.NewMux(), Resolver: res, Log: quiet()})
	j.start(ctx, l.reg, RoleFollower, true)
	var ping peerapi.Ping
	cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
	defer ccancel()
	if err := mgr.Call(cctx, "n2", "GET", peerapi.PathPing, nil, &ping); err != nil {
		var re *mesh.RemoteError
		if !errors.As(err, &re) { // the ping handler is not registered on the joiner's mux; reaching it is the point
			t.Fatalf("call through the resolver: %v", err)
		}
	}
	if !mgr.Connected("n2") {
		t.Fatal("no session through the resolver's address")
	}
}

type fixedResolver map[string]string

func (f fixedResolver) Addrs(_ context.Context, n registry.Node) ([]string, error) {
	if a, ok := f[n.ID]; ok {
		return []string{a}, nil
	}
	return nil, nil
}

func mustStore(t *testing.T, conf string) *Store {
	t.Helper()
	s, err := OpenStore(config.ClusterDir(conf))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---- reports, the status file, the certificate snapshot ----

func TestReportsKeepTheLatestOfEachNode(t *testing.T) {
	r := NewReports()
	var seen []string
	r.Subscribe(func(rep peerapi.Report) { seen = append(seen, rep.Node) })
	at := time.Now()
	r.Put("n2", peerapi.Report{Epoch: 1, Instances: []peerapi.InstanceStatus{{Identifier: "a-rr-x-abc123", Step: "3"}}}, at)
	r.Put("n2", peerapi.Report{Epoch: 1, Instances: []peerapi.InstanceStatus{{Identifier: "a-rr-x-abc123", Step: "4"}}}, at.Add(time.Second))
	r.Put("n3", peerapi.Report{Epoch: 1}, at)
	if in, when, ok := r.Instance("a-rr-x-abc123"); !ok || in.Step != "4" || !when.Equal(at.Add(time.Second)) {
		t.Fatalf("instance %+v %v %v", in, when, ok)
	}
	if _, _, ok := r.Instance("nope"); ok {
		t.Fatal("found an instance nobody reported")
	}
	if all := r.All(); len(all) != 2 || all[0].Node != "n2" || all[1].Node != "n3" {
		t.Fatalf("all %+v", all)
	}
	if got := strings.Join(seen, ","); got != "n2,n2,n3" {
		t.Fatalf("subscriber saw %s", got)
	}
}

type recordingRPC struct {
	calls []string
	body  peerapi.Report
	err   error
}

func (r *recordingRPC) Call(_ context.Context, node, method, path string, in, out any) error {
	r.calls = append(r.calls, node+" "+method+" "+path)
	r.body = in.(peerapi.Report)
	return r.err
}

func TestReporterSendsToTheLeaderAndKeepsTheLeadersOwn(t *testing.T) {
	reports := NewReports()
	rpc := &recordingRPC{}
	leader := false
	rp := &Reporter{Self: func() string { return "n2" }, Epoch: func() int64 { return 4 }, Leader: func() (string, bool) { return "n1", true },
		IsLeader: func() bool { return leader }, RPC: rpc, Reports: reports}
	rp.Add(func(context.Context) ([]peerapi.InstanceStatus, []peerapi.ProjectHealth) {
		return []peerapi.InstanceStatus{{Identifier: "x-rr-y-abc123"}}, []peerapi.ProjectHealth{{Ref: "aaaaaaaaaaaaaaaaaaaa", Healthy: true}}
	})
	rp.Add(func(context.Context) ([]peerapi.InstanceStatus, []peerapi.ProjectHealth) {
		return nil, []peerapi.ProjectHealth{{Ref: "bbbbbbbbbbbbbbbbbbbb"}}
	})
	if err := rp.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rpc.calls) != 1 || rpc.calls[0] != "n1 POST /peer/v1/report" || rpc.body.Node != "n2" || rpc.body.Epoch != 4 || len(rpc.body.Instances) != 1 || len(rpc.body.Projects) != 2 {
		t.Fatalf("calls %v, body %+v", rpc.calls, rpc.body)
	}
	if _, ok := reports.Latest("n2"); ok {
		t.Fatal("a follower stored its own report")
	}
	leader = true
	if err := rp.Once(context.Background()); err != nil || len(rpc.calls) != 1 {
		t.Fatalf("the leader called: %v, %v", rpc.calls, err)
	}
	if got, ok := reports.Latest("n2"); !ok || len(got.Instances) != 1 {
		t.Fatalf("the leader's own report: %+v %v", got, ok)
	}
	leader = false
	rp.Leader = func() (string, bool) { return "", false }
	if err := rp.Once(context.Background()); !errors.Is(err, mesh.ErrNoSession) {
		t.Fatalf("no leader: %v", err)
	}
}

func TestStatusFileRoundTrip(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	if s, err := ReadStatus(cfg); s != nil || err != nil {
		t.Fatalf("no file: %+v, %v", s, err)
	}
	lag := 0.4
	want := Status{At: time.Now().UTC().Truncate(time.Second), Node: "n1", Role: RoleLeader, Epoch: 3, Leader: "n1",
		Peers:    []PeerStatus{{Node: "n2", Connected: true, RTTMillis: 1.5, Version: "v0.2.0", Epoch: 3, Leader: "n1", Health: "healthy"}},
		Replicas: []ReplicaStatus{{Identifier: "a-rr-x-abc123", Ref: "a", Node: "n2", ReceiverStatus: "streaming", LagSeconds: &lag, ReportedAt: time.Now().UTC().Truncate(time.Second)}}}
	if err := WriteStatus(cfg, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("round trip:\n%s\n%s", a, b)
	}
}

func TestCertificateSnapshotAndTheEndpoint(t *testing.T) {
	root := t.TempDir()
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("certificates/acme/example.test/example.test.crt", "CERT", 0o644)
	write("certificates/acme/example.test/example.test.key", "KEY", 0o600)
	write("locks/issue_cert_example.test.lock", "busy", 0o644)
	write(".hidden", "x", 0o644)

	snap, etag, err := SnapshotCerts(root)
	if err != nil || len(snap.Files) != 2 || snap.Files[0].Path != "certificates/acme/example.test/example.test.crt" || snap.Files[1].Mode != 0o600 {
		t.Fatalf("snapshot %+v, %v", snap, err)
	}
	if _, again, _ := SnapshotCerts(root); again != etag {
		t.Fatal("the ETag of an unchanged store changed")
	}
	write("certificates/acme/example.test/example.test.crt", "CERT2", 0o644)
	if _, changed, _ := SnapshotCerts(root); changed == etag {
		t.Fatal("the ETag did not change with the content")
	}
	if empty, e, err := SnapshotCerts(filepath.Join(root, "missing")); err != nil || len(empty.Files) != 0 || e == "" {
		t.Fatalf("a store that does not exist: %+v %q %v", empty, e, err)
	}

	l := newLeader(t)
	l.cfg.StateDir = filepath.Dir(root)
	_ = os.Rename(root, l.cfg.Paths().Certs())
	api := &PeerAPI{Authority: l.auth, Topology: l.live, Cfg: l.cfg, Reports: l.rep}
	mux := mesh.NewMux()
	api.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", peerapi.PathCerts, nil))
	if rec.Code != 200 || rec.Header().Get("ETag") == "" {
		t.Fatalf("certs: %d %v", rec.Code, rec.Header())
	}
	var got peerapi.CertSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Files) != 2 {
		t.Fatalf("body %v, %v", got, err)
	}
	req := httptest.NewRequest("GET", peerapi.PathCerts, nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Fatalf("conditional get: %d %q", rec2.Code, rec2.Body.String())
	}
	// A node that is not the leader holds no authoritative store.
	l.auth.Topology = cluster2Follower{l.auth.Topology}
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", peerapi.PathCerts, nil))
	if rec3.Code != http.StatusServiceUnavailable {
		t.Fatalf("certs on a follower: %d", rec3.Code)
	}
}

func TestDNSRecords(t *testing.T) {
	cfg := config.Default()
	cfg.Domain = "example.test"
	nodes := []registry.Node{
		{ID: "n1", PeerAddr: "203.0.113.5:7443", State: registry.NodeActive},
		{ID: "n2", PeerAddr: "198.51.100.7:7443", PublicHost: "replica.example.test", State: registry.NodeActive},
		{ID: "n3", PublicHost: "gone.example.test", State: registry.NodeLeft},
		{ID: "n4", PublicHost: "192.0.2.1", State: registry.NodeActive},
	}
	recs := DNSRecords(cfg, &registry.Cluster{Leader: "n1"}, nodes)
	var lines []string
	for _, r := range recs {
		lines = append(lines, r.Name+" "+r.Type+" "+r.Value)
	}
	want := []string{"api.example.test A 203.0.113.5", "studio.example.test A 203.0.113.5", "pooler.example.test A 203.0.113.5",
		"*.api.example.test A 203.0.113.5", "replica.example.test A 198.51.100.7"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("records:\n%s", strings.Join(lines, "\n"))
	}
	svc := DNSRecords(cfg, &registry.Cluster{Leader: "n1", ServiceAddress: registry.ServiceAddress{IP: "2001:db8::1"}}, nodes[:1])
	if svc[0].Type != "AAAA" || svc[0].Value != "2001:db8::1" {
		t.Fatalf("service address: %+v", svc[0])
	}
}
