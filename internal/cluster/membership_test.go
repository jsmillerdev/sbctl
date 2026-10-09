package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	// The old leader was a founder, with no follower mark; the rejoin makes it a follower of the new one.
	dir := config.ClusterDir(j.confPath)
	if err := os.Remove(filepath.Join(dir, FollowerFile)); err != nil || IsFollower(dir) {
		t.Fatalf("setting up a founder: %v", err)
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
	if !IsFollower(dir) {
		t.Fatal("a node that rejoined is not marked as a follower")
	}
	// The cluster settings carry the backup and DNS credentials: a rejoin writes them for their owner only.
	cl := filepath.Join(config.ConfigDDir(j.confPath), config.ClusterConfigFile)
	if fi, err := os.Stat(cl); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("%s after a rejoin: %v, %v", cl, fi, err)
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

// A fenced node that rejoins has been a member for a long time, but the leader's patience with a node
// that is joining runs from the moment it went back to joining, not from the day it first joined.
func TestRejoiningNodeIsNotReapedForItsAge(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	l.clk.Advance(90 * 24 * time.Hour) // a quarter of a year a member
	if err := l.reg.SetNodeState(ctx, "n2", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	l.live.Refresh(ctx)
	req := peerapi.RejoinRequest{Version: "v0.2.0", ArtifactPins: testPins}
	first, err := l.auth.Rejoin(ctx, "n2", req)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := l.auth.ReapJoining(ctx); len(got) != 0 {
		t.Fatalf("a node that went back to joining a moment ago was reaped: %v", got)
	}
	l.clk.Advance(JoiningTimeout - 5*time.Minute)
	// Asking again, because the first attempt stopped on the node, gets the same answer.
	again, err := l.auth.Rejoin(ctx, "n2", req)
	if err != nil || again.System.Identifier != first.System.Identifier {
		t.Fatalf("a second rejoin of a joining node: %+v, %v", again, err)
	}
	if got, _ := l.auth.ReapJoining(ctx); len(got) != 0 {
		t.Fatalf("a rejoin inside the joining time was reaped: %v", got)
	}
	if n, _ := l.reg.GetNode(ctx, "n2"); n.State != registry.NodeJoining {
		t.Fatalf("node n2 is %s", n.State)
	}
	// A node that is not fenced or joining is refused.
	if _, err := l.auth.Rejoin(ctx, "n1", req); err == nil || !strings.Contains(err.Error(), "only a fenced node rejoins") {
		t.Fatalf("rejoin of the leader: %v", err)
	}
	// The joining time still runs out.
	l.clk.Advance(10 * time.Minute)
	if got, _ := l.auth.ReapJoining(ctx); len(got) != 1 || got[0] != "n2" {
		t.Fatalf("reaped %v after the joining time", got)
	}
}

// A rejoin that stops on the node (a unit that will not stop) can be run again, and a leader the
// operator names by address needs no node id from the record, which may be an older leader's.
func TestRejoinRepeatsAfterAFailureOnTheNode(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	if err := l.reg.SetNodeState(ctx, "n2", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	l.live.Refresh(ctx)
	if err := os.MkdirAll(j.cfg.Paths().PostgresData("system"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The record names a leader that no longer leads, with no address.
	if err := WriteFenced(j.cfg, FencedRecord{Epoch: 2, Leader: "n9", Reason: "replaced", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	o := RejoinOptions{Cfg: j.cfg, ConfigPath: j.confPath, Leader: l.cfg.PeerAddr(), Version: "v0.2.0", Pins: testPins, Seed: seed, Log: quiet(),
		StopLocal: func(context.Context) error { return errors.New("a unit would not stop") },
		Streaming: func(context.Context) (string, error) { return "0/4000000", nil }}
	if _, err := Rejoin(ctx, o); err == nil || !strings.Contains(err.Error(), "would not stop") {
		t.Fatalf("a rejoin whose units do not stop: %v", err)
	}
	if n, _ := l.reg.GetNode(ctx, "n2"); n.State != registry.NodeJoining {
		t.Fatalf("the leader holds node n2 as %s", n.State)
	}
	if rec, _ := ReadFenced(j.cfg); rec == nil {
		t.Fatal("the node forgot that it is fenced")
	}
	if _, err := os.Stat(j.cfg.Paths().PostgresData("system")); err != nil {
		t.Fatalf("data was moved although nothing stopped: %v", err)
	}

	o.StopLocal = func(context.Context) error { return nil }
	if _, err := Rejoin(ctx, o); err != nil {
		t.Fatalf("the rejoin run again: %v", err)
	}
	if n, _ := l.reg.GetNode(ctx, "n2"); n.State != registry.NodeActive {
		t.Fatalf("node n2 is %s", n.State)
	}
	if rec, _ := ReadFenced(j.cfg); rec != nil {
		t.Fatal("the fenced record is still there")
	}
}

// The preflight of a join or a rejoin runs before anything is spent, asked or moved.
func TestPreflightRefusesBeforeAnythingChanges(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	tok := l.token(t, TokenOptions{})
	no := errors.New("the data directory is not empty")
	o := j.joinOptions(tok, "second", seed)
	o.Preflight = func(context.Context) error { return no }
	if _, err := Join(ctx, o); !errors.Is(err, no) {
		t.Fatalf("join with a refusing preflight: %v", err)
	}
	if ns, _ := l.reg.ListNodes(ctx); len(ns) != 1 {
		t.Fatalf("the leader created a node: %v", ns)
	}
	if Joined(config.ClusterDir(j.confPath)) {
		t.Fatal("the server has an identity")
	}
	o.Preflight = nil
	if _, err := Join(ctx, o); err != nil { // the token was not spent
		t.Fatalf("join after the preflight passed: %v", err)
	}

	if err := l.reg.SetNodeState(ctx, "n2", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	l.live.Refresh(ctx)
	if err := os.MkdirAll(j.cfg.Paths().PostgresData("system"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFenced(j.cfg, FencedRecord{Epoch: 2, Leader: "n1", At: time.Now(), Peers: map[string]string{"n1": l.cfg.PeerAddr()}}); err != nil {
		t.Fatal(err)
	}
	ro := RejoinOptions{Cfg: j.cfg, ConfigPath: j.confPath, Version: "v0.2.0", Pins: testPins, Seed: seed, Log: quiet(), Preflight: func(context.Context) error { return no },
		StopLocal: func(context.Context) error { t.Error("something was stopped"); return nil }}
	if _, err := Rejoin(ctx, ro); !errors.Is(err, no) {
		t.Fatalf("rejoin with a refusing preflight: %v", err)
	}
	if n, _ := l.reg.GetNode(ctx, "n2"); n.State != registry.NodeFenced {
		t.Fatalf("the leader moved node n2 to %s", n.State)
	}
	if _, err := os.Stat(j.cfg.Paths().PostgresData("system")); err != nil {
		t.Fatalf("data was moved: %v", err)
	}
}

// A server that holds the identity of a join the leader gave up on starts over with Reset; without
// it the join says how.
func TestJoinResetStartsOverFromAnUnfinishedJoin(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	stuck := func(context.Context, peerapi.SystemBootstrap) error { return errors.New("stuck") }
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", stuck)); err == nil {
		t.Fatal("expected the seed to fail")
	}
	if err := os.MkdirAll(j.cfg.Paths().PostgresData("system"), 0o700); err != nil { // the half-seeded standby
		t.Fatal(err)
	}
	if err := l.reg.SetNodeState(ctx, "n2", registry.NodeLeft); err != nil { // what the reaper does to a join that took an hour
		t.Fatal(err)
	}
	l.live.Refresh(ctx)

	seed, boot := okSeed(t)
	o := j.joinOptions(l.token(t, TokenOptions{}), "second-b", seed)
	if _, err := Join(ctx, o); err == nil || !strings.Contains(err.Error(), "--reset") {
		t.Fatalf("a join on a server with an identity: %v", err)
	}
	o.Reset = true
	if _, err := Join(ctx, o); err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("a reset without a way to stop what runs: %v", err)
	}
	stopped := 0
	o.StopLocal = func(context.Context) error { stopped++; return nil }
	res, err := Join(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if stopped != 1 || res.NodeID == "n2" || len(*boot) != 1 {
		t.Fatalf("stopped %d, node %s, seeded %d", stopped, res.NodeID, len(*boot))
	}
	if creds, err := LoadCredentials(config.ClusterDir(j.confPath)); err != nil || creds.NodeID != res.NodeID {
		t.Fatalf("the identity on disk: %+v, %v", creds, err)
	}
	if _, err := os.Stat(j.cfg.Paths().PostgresData("system")); !os.IsNotExist(err) {
		t.Fatal("the half-seeded data is still in place")
	}
	if rec, _ := ReadFenced(j.cfg); rec != nil {
		t.Fatalf("the record of the reset is still there: %+v", rec)
	}
	if n, _ := l.reg.GetNode(ctx, res.NodeID); n.State != registry.NodeActive || n.Name != "second-b" {
		t.Fatalf("the new node: %+v", n)
	}
}

// A node that was removed while the daemon could not delete its identity keeps the record of the removal
// and the files, says what stayed, and the next join clears them without a flag.
func TestRetireRecordsTheRemovalAndReportsWhatStayed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes files in a directory it cannot write")
	}
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	dir := config.ClusterDir(j.confPath)
	if err := os.Chmod(dir, 0o500); err != nil { // the unit does not let the daemon write here
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	moved, err := Retire(ctx, j.cfg, j.confPath, nil, time.Now())
	if err == nil || !strings.Contains(err.Error(), "not deleted completely") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("retire in a directory it cannot write: %v (moved %v)", err, moved)
	}
	if rec, _ := ReadFenced(j.cfg); rec == nil || !rec.Removed {
		t.Fatalf("no record of the removal: %+v", rec)
	}
	if d, err := DecideBoot(ctx, BootEnv{Cfg: j.cfg, ConfigPath: j.confPath}); err != nil || d.Role != RoleFenced {
		t.Fatalf("the boot of a node whose identity stayed: %+v, %v", d, err)
	}
	if !Joined(dir) {
		t.Fatal("the identity went although the directory cannot be written")
	}

	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := l.reg.SetNodeState(ctx, "n2", registry.NodeLeft); err != nil {
		t.Fatal(err)
	}
	res, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second-b", seed))
	if err != nil {
		t.Fatalf("join after a removal that left the identity: %v", err)
	}
	if creds, err := LoadCredentials(dir); err != nil || creds.NodeID != res.NodeID || res.NodeID == "n2" {
		t.Fatalf("the identity on disk: %+v, %v", creds, err)
	}
	if rec, _ := ReadFenced(j.cfg); rec != nil {
		t.Fatalf("the record of the removal is still there: %+v", rec)
	}
}

// A certificate that the node could not keep would lock it out at its next restart, so the renewal
// waits while the cluster directory cannot be written and says so; it goes ahead when it can.
func TestRenewalWaitsWhileTheDaemonCannotKeepTheCertificate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory that is not writable")
	}
	l := newLeader(t)
	ctx := context.Background()
	key := l.store.Creds().Cert.PrivateKey.(ed25519.PrivateKey)
	short, err := l.ca.Issue(key.Public().(ed25519.PublicKey), "n1", time.Now(), 5*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.reg.SetNodeCert(ctx, "n1", short.Serial)
	if err := l.store.Replace(short.DER); err != nil {
		t.Fatal(err)
	}
	l.live.Refresh(ctx)
	var told []error
	r := &Renewer{Store: l.store, Self: l.live.Self, IsLeader: func() bool { return true }, Authority: l.auth, Grace: time.Millisecond, Wait: time.Second,
		Blocked: func(err error) { told = append(told, err) }}

	dir := config.ClusterDir(l.confPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	if done, err := r.Once(ctx); done || !errors.Is(err, ErrIdentityReadOnly) {
		t.Fatalf("a renewal in a directory the daemon cannot write: %v, %v", done, err)
	}
	if n, _ := l.reg.GetNode(ctx, "n1"); n.CertSerial != short.Serial {
		t.Fatal("the leader issued a certificate the node could not keep")
	}
	if len(told) != 1 || !errors.Is(told[0], ErrIdentityReadOnly) {
		t.Fatalf("Blocked was told %v", told)
	}

	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if done, err := r.Once(ctx); err != nil || !done {
		t.Fatalf("the renewal once the directory can be written: %v, %v", done, err)
	}
	if len(told) != 2 || told[1] != nil {
		t.Fatalf("Blocked was told %v", told)
	}
	if left, _ := os.ReadDir(dir); len(left) != 3 {
		t.Fatalf("the write check left files in %s: %v", dir, left)
	}
}

// The joiner's own claims about itself are stored and acted on later, so each has the shape the cloud
// gives it; a refusal spends nothing.
func TestJoinRefusesIdentitiesThatAreNotTheClouds(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	tok := l.token(t, TokenOptions{})
	secret, _ := tokenSecretBytes(tok)
	key, _ := NewKey()
	csr, _ := NewCSR(key, "second")
	build := func(edit func(*peerapi.JoinRequest)) peerapi.JoinRequest {
		ch := l.auth.Challenge()
		req := peerapi.JoinRequest{TokenID: tok.ID, Nonce: ch.Nonce, Proof: Proof(secret, ch.Nonce, csr, "second"), CSR: csr, Name: "second",
			Region: "eu-west-1", PeerAddr: "127.0.0.1:7443", Version: "v0.2.0", ArtifactPins: testPins}
		edit(&req)
		return req
	}
	code := func(err error) string {
		var ce *Error
		if errors.As(err, &ce) {
			return ce.Code
		}
		return ""
	}
	good := &registry.NodeAWS{InstanceID: "i-0123456789abcdef0", Zone: "eu-west-1a", Region: "eu-west-1", AllocationID: "eipalloc-0abc1234ef567890a"}
	for name, c := range map[string]struct {
		edit func(*peerapi.JoinRequest)
		want string
	}{
		"an instance id with a path": {func(r *peerapi.JoinRequest) { r.Provider.AWS = &registry.NodeAWS{InstanceID: "i-../../x"} }, "bad_provider"},
		"a short instance id":        {func(r *peerapi.JoinRequest) { r.Provider.AWS = &registry.NodeAWS{InstanceID: "i-0abc"} }, "bad_provider"},
		"a region with spaces": {func(r *peerapi.JoinRequest) {
			a := *good
			a.Region = "eu west 1"
			r.Provider.AWS = &a
		}, "bad_provider"},
		"a zone that is a sentence": {func(r *peerapi.JoinRequest) {
			a := *good
			a.Zone = "any zone you like"
			r.Provider.AWS = &a
		}, "bad_provider"},
		"an allocation that is not": {func(r *peerapi.JoinRequest) {
			a := *good
			a.AllocationID = "eipalloc-zzz"
			r.Provider.AWS = &a
		}, "bad_provider"},
		"a release name of a book":      {func(r *peerapi.JoinRequest) { r.Version = strings.Repeat("v", 200) }, "bad_version"},
		"a release name with a newline": {func(r *peerapi.JoinRequest) { r.Version = "v0.2.0\nadmin" }, "bad_version"},
	} {
		if _, err := l.auth.Join(ctx, build(c.edit)); code(err) != c.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ns, _ := l.reg.ListNodes(ctx); len(ns) != 1 {
		t.Fatalf("a refused join created a node: %v", ns)
	}
	resp, err := l.auth.Join(ctx, build(func(r *peerapi.JoinRequest) { r.Provider.AWS = good }))
	if err != nil {
		t.Fatalf("a well-formed identity: %v", err)
	}
	if n, _ := l.reg.GetNode(ctx, resp.NodeID); n.Provider.AWS == nil || n.Provider.AWS.InstanceID != good.InstanceID {
		t.Fatalf("the node row: %+v", n)
	}
}

// Challenges are rationed per address, and the peer API tells the address of a caller with no
// certificate to the authority.
func TestChallengesAreRationedPerAddress(t *testing.T) {
	l := newLeader(t)
	tooMany := func(err error) bool {
		var ce *Error
		return errors.As(err, &ce) && ce.Status == http.StatusTooManyRequests && ce.Code == "too_many_requests"
	}
	for i := 0; i < challengeBurst; i++ {
		if _, err := l.auth.ChallengeFrom("192.0.2.1"); err != nil {
			t.Fatalf("challenge %d: %v", i, err)
		}
	}
	if _, err := l.auth.ChallengeFrom("192.0.2.1"); !tooMany(err) {
		t.Fatalf("the challenge beyond the burst: %v", err)
	}
	if _, err := l.auth.ChallengeFrom("192.0.2.2"); err != nil {
		t.Fatalf("another address was held up: %v", err)
	}
	l.clk.Advance(challengeEvery)
	if _, err := l.auth.ChallengeFrom("192.0.2.1"); err != nil {
		t.Fatalf("a challenge after the wait: %v", err)
	}
	if _, err := l.auth.ChallengeFrom("192.0.2.1"); !tooMany(err) {
		t.Fatalf("a second challenge after one wait: %v", err)
	}
	for i := 0; i < 100; i++ { // no address, no ration (the caller is not remote)
		if _, err := l.auth.ChallengeFrom(""); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2*maxBuckets; i++ {
		_, _ = l.auth.ChallengeFrom(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	l.auth.mu.Lock()
	n := len(l.auth.buckets)
	l.auth.mu.Unlock()
	if n > maxBuckets {
		t.Fatalf("%d addresses remembered", n)
	}

	// Over the wire: the address is the one the connection came from.
	ctx := context.Background()
	c, err := mesh.DialClient(ctx, l.cfg.PeerAddr(), "leader", mesh.PinnedTLS(l.ca.Fingerprint(), nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ok := 0
	var last error
	for i := 0; i < challengeBurst+5; i++ {
		var ch peerapi.JoinChallenge
		if last = c.Call(ctx, "GET", peerapi.PathJoin, nil, &ch); last == nil {
			ok++
		}
	}
	var re *mesh.RemoteError
	if ok < challengeBurst || ok > challengeBurst+2 || !errors.As(last, &re) || re.Status != http.StatusTooManyRequests || re.Code != "too_many_requests" {
		t.Fatalf("%d challenges served; the last: %v", ok, last)
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
	// The client asks the region this node runs in, so a peer in another region is not looked up; a
	// resolver that does not know its own region tries.
	other := peer
	other.Provider = registry.NodeProvider{AWS: &registry.NodeAWS{InstanceID: "i-0peer", Region: "us-east-1"}}
	calls := len(f.Calls())
	r2 := &AWSResolver{EC2: ec2, Self: func() registry.Node { return self }}
	if got, err = r2.Addrs(context.Background(), other); err != nil || len(got) != 0 || len(f.Calls()) != calls {
		t.Fatalf("another region: %v, %v, %d calls", got, err, len(f.Calls())-calls)
	}
	unaware := &AWSResolver{EC2: ec2}
	if got, _ = unaware.Addrs(context.Background(), other); strings.Join(got, ",") != "203.0.113.9:7443,10.77.0.5:7443" {
		t.Fatalf("a resolver without a region of its own: %v", got)
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

// The certificate store is the proxy's endpoint. If the membership endpoints took the pattern too, the
// daemon of any cluster node would panic at start (http.ServeMux refuses a pattern registered twice).
func TestPeerAPILeavesTheCertificateStoreToTheProxy(t *testing.T) {
	l := newLeader(t)
	mux := mesh.NewMux()
	(&PeerAPI{Authority: l.auth, Topology: l.live, Cfg: l.cfg, Reports: l.rep}).Register(mux)
	certs := "GET " + peerapi.PathCerts
	for _, p := range mux.Patterns() {
		if p == certs {
			t.Fatalf("the membership endpoints register %q", p)
		}
	}
	mux.Handle(certs, func(http.ResponseWriter, *http.Request) {}) // the proxy's registration must not clash
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

// followerNearExpiry joins node n2, runs it, and gives it a certificate with ten days left, which is
// the leader's record of it. The renewer of its store is the one under test.
func followerNearExpiry(t *testing.T) (*leaderFixture, *site) {
	t.Helper()
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	j.start(cctx, l.reg, RoleFollower, true)
	eventually(t, "a session", func() bool { return j.mgr.Connected("n1") })
	pub := j.store.Creds().Cert.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
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
	return l, j
}

func (j *site) renewer(wait time.Duration) *Renewer {
	return &Renewer{Store: j.store, Self: j.live.Self, Leader: func() (string, bool) { return "n1", true }, IsLeader: func() bool { return false },
		RPC: j.mgr, Log: quiet(), Grace: 10 * time.Millisecond, Wait: wait}
}

// The leader has recorded the new serial by the time the certificate arrives, so it is a node's
// own copy of the registry being late, not the certificate being wrong, when the serial does not
// show: the certificate that was issued is the one to use, because the old one is about to be refused.
func TestRenewalUsesTheNewCertificateWhenTheRegistryCopyLags(t *testing.T) {
	l, j := followerNearExpiry(t)
	ctx := context.Background()
	before := j.store.Creds().Serial
	r := j.renewer(200 * time.Millisecond)
	r.Self = func() registry.Node { return registry.Node{ID: "n2", CertSerial: before} } // the copy that never catches up
	done, err := r.Once(ctx)
	if err != nil || !done {
		t.Fatalf("renewal with a lagging copy: %v, %v", done, err)
	}
	n2, _ := l.reg.GetNode(ctx, "n2")
	if got := j.store.Creds().Serial; got == before || got != n2.CertSerial {
		t.Fatalf("in use: %s, leader's record %s, before %s", got, n2.CertSerial, before)
	}
	if onDisk, err := LoadCredentials(config.ClusterDir(j.confPath)); err != nil || onDisk.Serial != n2.CertSerial {
		t.Fatalf("on disk: %+v, %v", onDisk, err)
	}
}

// A daemon that stops while a renewal waits for its copy of the registry has the new certificate on
// disk already: the next start loads it, and the old file is not what the peers will admit.
func TestRenewalIsOnDiskBeforeItWaits(t *testing.T) {
	l, j := followerNearExpiry(t)
	ctx, cancel := context.WithCancel(context.Background())
	before := j.store.Creds().Serial
	r := j.renewer(time.Minute)
	r.Self = func() registry.Node { return registry.Node{ID: "n2", CertSerial: before} }
	go func() { time.Sleep(300 * time.Millisecond); cancel() }() // the daemon stops
	if done, err := r.Once(ctx); err == nil || done {
		t.Fatalf("renewal that was interrupted: %v, %v", done, err)
	}
	n2, _ := l.reg.GetNode(context.Background(), "n2")
	onDisk, err := LoadCredentials(config.ClusterDir(j.confPath))
	if err != nil || onDisk.Serial != n2.CertSerial || onDisk.Serial == before {
		t.Fatalf("on disk after the interruption: %+v, %v (leader's record %s)", onDisk, err, n2.CertSerial)
	}
	if j.store.Creds().Serial != before {
		t.Fatal("the new certificate was used before its serial showed")
	}
}

// A file that cannot be written after the leader recorded the serial raises the alert, the new
// certificate is used in memory, and the renewer writes the file when it can.
func TestRenewalThatCannotWriteTheFileIsToldAndWrittenLater(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory that is not writable")
	}
	l, j := followerNearExpiry(t)
	dir := config.ClusterDir(j.confPath)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	var told []error
	r := j.renewer(time.Second)
	r.Blocked = func(err error) {
		told = append(told, err)
		if err == nil && len(told) == 1 {
			_ = os.Chmod(dir, 0o500) // the check passed; now the disk goes away
		}
	}
	done, err := r.Once(context.Background())
	if err != nil || !done {
		t.Fatalf("renewal whose file cannot be written: %v, %v", done, err)
	}
	if len(told) != 2 || told[0] != nil || !errors.Is(told[1], ErrNotKept) {
		t.Fatalf("Blocked was told %v", told)
	}
	n2, _ := l.reg.GetNode(context.Background(), "n2")
	if j.store.Creds().Serial != n2.CertSerial {
		t.Fatal("the new certificate is not in use")
	}
	if onDisk, _ := LoadCredentials(dir); onDisk == nil || onDisk.Serial == n2.CertSerial {
		t.Fatal("the file was written although the directory could not be")
	}
	if wrote, err := j.store.Resync(); wrote || err == nil {
		t.Fatalf("a resync into a directory that cannot be written: %v, %v", wrote, err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if wrote, err := j.store.Resync(); err != nil || !wrote {
		t.Fatalf("resync: %v, %v", wrote, err)
	}
	if onDisk, err := LoadCredentials(dir); err != nil || onDisk.Serial != n2.CertSerial {
		t.Fatalf("on disk after the resync: %+v, %v", onDisk, err)
	}
	if wrote, err := j.store.Resync(); wrote || err != nil {
		t.Fatalf("a resync with nothing to write: %v, %v", wrote, err)
	}
}

// The removal is recorded before any data is touched: a node whose data cannot be set aside still
// comes up down with the reason, and keeps its identity until the data is dealt with.
func TestRetireRecordsTheRemovalBeforeItMovesData(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames into a directory that is not writable")
	}
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	conf := filepath.Join(t.TempDir(), "etc", "config.toml")
	ca, _ := NewCA(newSecrets(t))
	key, _ := NewKey()
	iss, _ := ca.Issue(key.Public().(ed25519.PublicKey), "n2", time.Now(), time.Hour)
	if err := SaveIdentity(config.ClusterDir(conf), key, iss.DER, ca.DER()); err != nil {
		t.Fatal(err)
	}
	data := cfg.Paths().PostgresData("system")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(data)
	if err := os.Chmod(parent, 0o500); err != nil { // the data cannot be renamed out of here
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	_, err := Retire(context.Background(), cfg, conf, nil, time.Now())
	if err == nil || !strings.Contains(err.Error(), "recorded as removed") {
		t.Fatalf("retire with data that cannot be moved: %v", err)
	}
	if rec, _ := ReadFenced(cfg); rec == nil || !rec.Removed {
		t.Fatalf("no record of the removal: %+v", rec)
	}
	if !Joined(config.ClusterDir(conf)) {
		t.Fatal("the identity went before the data was set aside")
	}
	if d, err := DecideBoot(context.Background(), BootEnv{Cfg: cfg, ConfigPath: conf}); err != nil || d.Role != RoleFenced || d.Reason != RemovedReason {
		t.Fatalf("the boot of that node: %+v, %v", d, err)
	}
}

// A reset on a node that is still a member looks at the token and the key before it stops or moves
// anything: a token that has expired, or a key that is not the cluster's, has retired nobody.
func TestJoinResetChecksItsInputsBeforeItRetiresTheNode(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(j.cfg.Paths().PostgresData("system"), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := func(context.Context) error { t.Error("something was stopped"); return nil }

	expired := l.token(t, TokenOptions{TTL: time.Minute})
	o := j.joinOptions(expired, "second-b", seed)
	o.Reset, o.StopLocal = true, stop
	o.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := Join(ctx, o); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("reset with an expired token: %v", err)
	}

	other := make([]byte, 32)
	if _, err := crand.Read(other); err != nil {
		t.Fatal(err)
	}
	o = j.joinOptions(l.token(t, TokenOptions{}), "second-b", seed)
	o.Reset, o.StopLocal, o.MasterKey = true, stop, []byte(hex.EncodeToString(other)+"\n")
	if _, err := Join(ctx, o); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Fatalf("reset with another cluster's key: %v", err)
	}
	if err := o.CheckInputs(); err == nil {
		t.Fatal("CheckInputs accepted another cluster's key")
	}

	if !Joined(config.ClusterDir(j.confPath)) {
		t.Fatal("the identity was deleted")
	}
	if rec, _ := ReadFenced(j.cfg); rec != nil {
		t.Fatalf("the node was recorded as removed: %+v", rec)
	}
	if _, err := os.Stat(j.cfg.Paths().PostgresData("system")); err != nil {
		t.Fatalf("data was moved: %v", err)
	}
}

// A resumed join has begun: the preflight ran at its start, and what it looks at (room, the store)
// is not what a half-seeded server offers.
func TestResumeDoesNotRunThePreflight(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	stuck := func(context.Context, peerapi.SystemBootstrap) error { return errors.New("stuck") }
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", stuck)); err == nil {
		t.Fatal("expected the seed to fail")
	}
	seed, boot := okSeed(t)
	o := j.joinOptions(Token{}, "", seed)
	o.Resume = true
	o.Preflight = func(context.Context) error { return errors.New("the data directory is not empty") }
	if _, err := Join(ctx, o); err != nil || len(*boot) != 1 {
		t.Fatalf("resume: %v, seeded %d", err, len(*boot))
	}
}

// What a joiner is sent besides its certificate is read before the node exists: a master key that
// cannot be read spends no token and leaves no row, so the same token joins once the key can be read.
func TestJoinReadsTheMasterKeyBeforeItSpendsTheToken(t *testing.T) {
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
	read := l.auth.MasterKey
	l.auth.MasterKey = func() ([]byte, error) { return nil, errors.New("the key file cannot be read") }
	if _, err := l.auth.Join(ctx, build()); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("join with a key that cannot be read: %v", err)
	}
	if ns, _ := l.reg.ListNodes(ctx); len(ns) != 1 {
		t.Fatalf("a node was created: %v", ns)
	}
	if got, err := l.reg.GetJoinToken(ctx, tok.ID); err != nil || got.UsedAt != nil {
		t.Fatalf("the token was spent: %+v, %v", got, err)
	}
	l.auth.MasterKey = read
	if _, err := l.auth.Join(ctx, build()); err != nil {
		t.Fatalf("the same token once the key can be read: %v", err)
	}
}

type failingTokens struct{ registry.Registry }

func (failingTokens) GetJoinToken(context.Context, string) (*registry.JoinToken, error) {
	return nil, errors.New("dial tcp 10.0.0.9:5432: connection refused")
}

// A caller with no certificate gets no detail of an internal failure: the text of a database error
// can carry a host name. A node does.
func TestPeerAPIKeepsInternalDetailFromCallersWithNoCertificate(t *testing.T) {
	l := newLeader(t)
	auth := &Authority{Reg: failingTokens{l.reg}, CA: l.ca, Secrets: l.sec, Cfg: l.cfg, Topology: l.live, Log: quiet(), Version: "v0.2.0", Pins: testPins, Now: l.clk.Now}
	mux := mesh.NewMux()
	(&PeerAPI{Authority: auth, Topology: l.live, Cfg: l.cfg, Reports: l.rep}).Register(mux)
	post := func(peer mesh.Peer) (int, string) {
		ch := auth.Challenge()
		body, _ := json.Marshal(peerapi.JoinRequest{TokenID: "x", Nonce: ch.Nonce})
		req := httptest.NewRequest("POST", peerapi.PathJoin, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req.WithContext(mesh.WithPeer(req.Context(), peer)))
		return rec.Code, rec.Body.String()
	}
	if code, body := post(mesh.Peer{Remote: "198.51.100.9"}); code != http.StatusInternalServerError || strings.Contains(body, "10.0.0.9") || strings.Contains(body, "refused") {
		t.Fatalf("a caller with no certificate got %d %s", code, body)
	}
	if code, body := post(mesh.Peer{Node: "n2", Remote: "198.51.100.9"}); code != http.StatusInternalServerError || !strings.Contains(body, "connection refused") {
		t.Fatalf("a node got %d %s", code, body)
	}
}

// A reset retires the server before the exchange, so the leader is looked at first: one that cannot be
// reached, or does not take joins, costs nothing, where it would have left the server down with its
// data set aside.
func TestJoinResetLooksAtTheLeaderBeforeItRetiresTheNode(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(j.cfg.Paths().PostgresData("system"), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := func(context.Context) error { t.Error("something was stopped"); return nil }
	unchanged := func(what string) {
		t.Helper()
		if !Joined(config.ClusterDir(j.confPath)) {
			t.Fatalf("%s: the identity was deleted", what)
		}
		if rec, _ := ReadFenced(j.cfg); rec != nil {
			t.Fatalf("%s: the node was recorded as removed: %+v", what, rec)
		}
		if _, err := os.Stat(j.cfg.Paths().PostgresData("system")); err != nil {
			t.Fatalf("%s: data was moved: %v", what, err)
		}
	}

	// A leader at an address nothing listens on.
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := gone.Addr().String()
	gone.Close()
	tok := l.token(t, TokenOptions{})
	tok.Leader = dead
	o := j.joinOptions(tok, "second-b", seed)
	o.Reset, o.StopLocal = true, stop
	if _, err := Join(ctx, o); err == nil || !strings.Contains(err.Error(), "cannot reach the leader") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("reset with an unreachable leader: %v", err)
	}
	unchanged("unreachable leader")

	// A node that answers but is not the leader.
	l.auth.Topology = cluster2Follower{l.auth.Topology}
	o = j.joinOptions(l.token2(t), "second-b", seed)
	o.Reset, o.StopLocal = true, stop
	if _, err := Join(ctx, o); err == nil || !strings.Contains(err.Error(), "does not take joins") {
		t.Fatalf("reset against a node that is not the leader: %v", err)
	}
	unchanged("not the leader")
}

// A server with a master key of its own finds out before the exchange that it is not the cluster's:
// the token is not spent and the leader has no row for the node.
func TestJoinRefusesAMasterKeyOfItsOwnBeforeAskingTheLeader(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	seed, _ := okSeed(t)
	tok := l.token(t, TokenOptions{})
	j := l.joiner(t, "n2")
	own := make([]byte, 32)
	if _, err := crand.Read(own); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(j.cfg.KeyPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.cfg.KeyPath, []byte(hex.EncodeToString(own)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Join(ctx, j.joinOptions(tok, "second", seed))
	if err == nil || !strings.Contains(err.Error(), "holds another key than the cluster's") || !strings.Contains(err.Error(), "--master-key-file") {
		t.Fatalf("join with a key of its own: %v", err)
	}
	if ns, _ := l.reg.ListNodes(ctx); len(ns) != 1 {
		t.Fatalf("the leader created a node: %v", ns)
	}
	if got, err := l.reg.GetJoinToken(ctx, tok.ID); err != nil || got.UsedAt != nil {
		t.Fatalf("the token was spent: %+v, %v", got, err)
	}
	// A key file that is not a key at all is no better.
	if err := os.WriteFile(j.cfg.KeyPath, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Join(ctx, j.joinOptions(tok, "second", seed)); err == nil || !strings.Contains(err.Error(), "holds another key") {
		t.Fatalf("join with a damaged key file: %v", err)
	}
	// The cluster's own key in that place is fine, and the same token joins.
	body, err := os.ReadFile(l.cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.cfg.KeyPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Join(ctx, j.joinOptions(tok, "second", seed)); err != nil {
		t.Fatalf("join with the cluster's key already in place: %v", err)
	}
}

// lostAnswers is an RPC whose first answers to the renewal never arrive: the leader handled the
// request, as a lost response looks from the node.
type lostAnswers struct {
	mesh.RPC
	mu    sync.Mutex
	lose  int
	calls int
}

func (l *lostAnswers) Call(ctx context.Context, node, method, path string, in, out any) error {
	err := l.RPC.Call(ctx, node, method, path, in, out)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if err == nil && path == peerapi.PathCertsRenew && l.lose > 0 {
		l.lose--
		return fmt.Errorf("%w: the answer was lost", mesh.ErrNoSession)
	}
	return err
}

// The leader records the serial of a new certificate before the node has it. When the answer is lost
// the node retries within the retry wait and ends with a certificate the leader's record names, not
// at the next look six hours on.
func TestRenewalIsTriedAgainSoonAfterALostAnswer(t *testing.T) {
	l, j := followerNearExpiry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := j.store.Creds().Serial
	lossy := &lostAnswers{RPC: j.mgr, lose: 1}
	r := j.renewer(time.Second)
	r.RPC, r.Every, r.Retry = lossy, time.Hour, 50*time.Millisecond
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	eventually(t, "the renewed certificate", func() bool {
		n2, _ := l.reg.GetNode(ctx, "n2")
		got := j.store.Creds().Serial
		return got != before && got == n2.CertSerial
	})
	cancel()
	<-done
	lossy.mu.Lock()
	defer lossy.mu.Unlock()
	if lossy.lose != 0 {
		t.Fatal("the answer was never lost")
	}
}

func TestRenewalRetryBacksOffToABound(t *testing.T) {
	r := &Renewer{}
	var got []time.Duration
	var last time.Duration
	for range 8 {
		last = r.nextRetry(last)
		got = append(got, last)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits %v, want %v", got, want)
		}
	}
	if r2 := (&Renewer{Retry: time.Second}); r2.nextRetry(0) != time.Second || r2.nextRetry(time.Second) != 2*time.Second {
		t.Fatal("Retry does not set the first wait")
	}
}

// A server that joined a cluster is marked a follower, which a founder, with its certificate, is not;
// the mark goes with the identity.
func TestFollowerMarkTellsAMemberFromTheFounder(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	if IsFollower(config.ClusterDir(l.confPath)) {
		t.Fatal("the founder is marked a follower")
	}
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	dir := config.ClusterDir(j.confPath)
	if !IsFollower(dir) {
		t.Fatal("a server that joined is not marked a follower")
	}
	info, err := os.Stat(filepath.Join(dir, FollowerFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v, %v", FollowerFile, info, err)
	}
	if _, err := Retire(ctx, j.cfg, j.confPath, func(context.Context) error { return nil }, time.Now()); err != nil {
		t.Fatal(err)
	}
	if IsFollower(dir) || Joined(dir) {
		t.Fatal("the identity or its mark survived a retirement")
	}
	// A mark without a certificate is not a member.
	if err := markFollower(dir, "n2", "x", "n1", time.Now()); err != nil || IsFollower(dir) {
		t.Fatalf("a mark with no certificate: %v", err)
	}
}

// The converge step that refreshes config.d/10-cluster.toml fetches the leader's cluster settings with
// the node's certificate, reports what it would change without writing, writes the file 0600 when the
// text differs, and leaves a server that leads, that never joined, or whose leader does not answer alone.
func TestConfigSyncRefreshesTheClusterSettingsFromTheLeader(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.ConfigDDir(j.confPath), config.ClusterConfigFile)
	joined, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sync := &ConfigSync{Cfg: j.cfg, ConfigPath: j.confPath, Log: quiet(),
		Where: func(context.Context) (string, string, error) { return "n1", l.cfg.PeerAddr(), nil }}

	if got, err := sync.Sync(ctx, true); err != nil || len(got) != 0 {
		t.Fatalf("settings that match: %v, %v", got, err)
	}
	l.cfg.Domain = "changed.example.test" // a cluster-scoped key
	got, err := sync.Sync(ctx, true)
	if err != nil || len(got) != 1 || got[0] != path {
		t.Fatalf("a changed setting, dry run: %v, %v", got, err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, joined) {
		t.Fatal("a dry run wrote the file")
	}
	if got, err := sync.Sync(ctx, false); err != nil || len(got) != 1 {
		t.Fatalf("a changed setting: %v, %v", got, err)
	}
	now, err := os.ReadFile(path)
	if err != nil || bytes.Equal(now, joined) || !strings.Contains(string(now), "changed.example.test") {
		t.Fatalf("the file after the sync: %q, %v", now, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if got, err := sync.Sync(ctx, false); err != nil || len(got) != 0 {
		t.Fatalf("the second run: %v, %v", got, err)
	}

	// A leader that does not answer is no failure of the convergence, and the file stays.
	gone, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := gone.Addr().String()
	gone.Close()
	l.cfg.Domain = "again.example.test"
	down := &ConfigSync{Cfg: j.cfg, ConfigPath: j.confPath, Log: quiet(),
		Where: func(context.Context) (string, string, error) { return "n1", dead, nil }}
	if got, err := down.Sync(ctx, false); err != nil || len(got) != 0 {
		t.Fatalf("an unreachable leader: %v, %v", got, err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, now) {
		t.Fatal("the file changed although the leader did not answer")
	}
	// The leader and a server that never joined have nothing to fetch.
	lead := &ConfigSync{Cfg: l.cfg, ConfigPath: l.confPath, Log: quiet(), Where: sync.Where}
	if got, err := lead.Sync(ctx, false); err != nil || len(got) != 0 {
		t.Fatalf("the founder: %v, %v", got, err)
	}
	none := &ConfigSync{Cfg: l.cfg, ConfigPath: filepath.Join(t.TempDir(), "etc", "config.toml"), Log: quiet(), Where: sync.Where}
	if got, err := none.Sync(ctx, false); err != nil || len(got) != 0 {
		t.Fatalf("a server that never joined: %v, %v", got, err)
	}
}

// With the registry out of reach the converge step falls back on the leader the node joined, and it holds
// the leader to the node id the join recorded: a mark written by an older release has none, and then any
// node of the cluster may answer.
func TestConfigSyncFallbackNamesTheLeaderTheNodeJoined(t *testing.T) {
	l := newLeader(t)
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(context.Background(), j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	dir := config.ClusterDir(j.confPath)
	ended, cancel := context.WithCancel(context.Background()) // the system cluster is not asked for long
	cancel()
	sync := &ConfigSync{Cfg: j.cfg, ConfigPath: j.confPath, Log: quiet(), DSNs: []string{"host=/nonexistent port=1 user=x connect_timeout=1"}}
	id, addr, err := sync.where(ended, dir, "n2")
	if err != nil || id != "n1" || addr != l.cfg.PeerAddr() {
		t.Fatalf("the leader the join recorded: %q %q %v", id, addr, err)
	}
	old := filepath.Join(dir, FollowerFile)
	if err := os.WriteFile(old, []byte(`{"node_id":"n2","leader":"203.0.113.9:7443"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, addr, err := sync.where(ended, dir, "n2"); err != nil || id != "" || addr != "203.0.113.9:7443" {
		t.Fatalf("a mark with no leader id: %q %q %v", id, addr, err)
	}
}

// The report intake names the reporting node from the certificate of the connection, never from the
// body: a node cannot report for another, and an empty name in the body is the caller's own.
func TestReportIntakeTakesTheNodeFromTheCertificate(t *testing.T) {
	l := newLeader(t)
	mux := mesh.NewMux()
	(&PeerAPI{Authority: l.auth, Topology: l.live, Cfg: l.cfg, Reports: l.rep}).Register(mux)
	var seen []string
	l.rep.Subscribe(func(rep peerapi.Report) { seen = append(seen, rep.Node) })
	post := func(body string) int {
		req := httptest.NewRequest("POST", peerapi.PathReport, strings.NewReader(body))
		req = req.WithContext(mesh.WithPeer(req.Context(), mesh.Peer{Node: "n2", State: registry.NodeActive}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(`{"node":"n3","epoch":1}`); code != http.StatusForbidden {
		t.Fatalf("a report for another node: %d", code)
	}
	if len(seen) != 0 {
		t.Fatalf("the subscriber heard of a refused report: %v", seen)
	}
	for _, body := range []string{`{"epoch":1}`, `{"node":"n2","epoch":2}`} {
		if code := post(body); code != http.StatusNoContent {
			t.Fatalf("%s: %d", body, code)
		}
	}
	if got := strings.Join(seen, ","); got != "n2,n2" {
		t.Fatalf("the subscriber saw %q, want the peer's name twice", got)
	}
	if _, ok := l.rep.Latest("n3"); ok {
		t.Fatal("a report was kept for n3")
	}
}
