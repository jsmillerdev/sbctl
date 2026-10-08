package cluster

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/registry"
)

// The rule of 2.10.8: what a node whose system cluster is a primary does at boot, from what it can
// see.
func TestDecide(t *testing.T) {
	self := registry.Node{ID: "n1", State: registry.NodeActive}
	ev := func(f func(*Evidence)) Evidence {
		e := Evidence{SelfID: "n1", Self: self, Epoch: 3, Leader: "n1"}
		if f != nil {
			f(&e)
		}
		return e
	}
	marker := func(epoch int64, leader string) *backup.LeaderMarker {
		return &backup.LeaderMarker{Epoch: epoch, Leader: leader, At: time.Now()}
	}
	for _, tc := range []struct {
		name   string
		ev     Evidence
		role   Role
		epoch  int64
		reason string // a word the reason holds
	}{
		{"alone: nobody to ask", ev(nil), RoleLeader, 3, ""},
		{"peers agree", ev(func(e *Evidence) { e.Peers = []PeerView{{"n2", 3, "n1"}} }), RoleLeader, 3, ""},
		{"peers that know no leader say nothing", ev(func(e *Evidence) { e.Peers = []PeerView{{"n2", 9, ""}} }), RoleLeader, 3, ""},
		{"a peer is behind", ev(func(e *Evidence) { e.Peers = []PeerView{{"n2", 2, "n3"}} }), RoleLeader, 3, ""},
		{"the marker agrees", ev(func(e *Evidence) { e.Marker = marker(3, "n1") }), RoleLeader, 3, ""},
		{"an old marker of another leader", ev(func(e *Evidence) { e.Marker = marker(2, "n2") }), RoleLeader, 3, ""},
		{"a peer holds a higher epoch", ev(func(e *Evidence) { e.Peers = []PeerView{{"n2", 4, "n2"}} }), RoleFenced, 4, "node n2"},
		{"the marker holds a higher epoch", ev(func(e *Evidence) { e.Marker = marker(4, "n2") }), RoleFenced, 4, "marker"},
		{"another leader at this epoch", ev(func(e *Evidence) { e.Peers = []PeerView{{"n2", 3, "n2"}} }), RoleFenced, 3, "node n2"},
		{"demoted in the local record", ev(func(e *Evidence) { e.Self.State = registry.NodeFenced }), RoleFenced, 3, "fenced"},
		{"removed in the local record", ev(func(e *Evidence) { e.Self.State = registry.NodeLeft }), RoleFenced, 3, "left"},
		{"the registry names another leader and nothing names this node", ev(func(e *Evidence) { e.Leader = "n2" }), RoleFenced, 3, "names node n2"},
		{"promoted: the marker names this node at a higher epoch",
			ev(func(e *Evidence) { e.Leader = "n2"; e.Marker = marker(4, "n1") }), RoleLeader, 4, ""},
		{"promoted: the node's own promote.ok names it, with no marker", ev(func(e *Evidence) { e.Leader = "n2"; e.Promoted = 4 }), RoleLeader, 4, ""},
		{"an old promote.ok changes nothing", ev(func(e *Evidence) { e.Leader = "n2"; e.Promoted = 3 }), RoleFenced, 3, "names node n2"},
		{"a peer at a higher epoch beats the node's promote.ok", ev(func(e *Evidence) {
			e.Leader = "n2"
			e.Promoted = 4
			e.Peers = []PeerView{{"n3", 5, "n3"}}
		}), RoleFenced, 5, "node n3"},
		{"promoted: the old leader still claims the old epoch",
			ev(func(e *Evidence) {
				e.Leader = "n2"
				e.Marker = marker(4, "n1")
				e.Peers = []PeerView{{"n2", 3, "n2"}}
			}), RoleLeader, 4, ""},
		{"promoted: the old leader already knows the new epoch",
			ev(func(e *Evidence) {
				e.Leader = "n2"
				e.Peers = []PeerView{{"n2", 4, "n1"}}
			}), RoleLeader, 4, ""},
		{"a marker that names another leader at the promotion epoch wins over a peer that names this node",
			ev(func(e *Evidence) {
				e.Leader = "n2"
				e.Peers = []PeerView{{"n3", 4, "n1"}}
				e.Marker = marker(4, "n2")
			}), RoleFenced, 4, "marker"},
	} {
		got := Decide(tc.ev)
		if got.Role != tc.role || got.Epoch != tc.epoch || !got.Joined || got.SelfID != "n1" {
			t.Errorf("%s: %+v, want %s at epoch %d", tc.name, got, tc.role, tc.epoch)
		}
		if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("%s: reason %q lacks %q", tc.name, got.Reason, tc.reason)
		}
		if got.Role == RoleLeader && got.Reason != "" {
			t.Errorf("%s: a leader with a reason: %q", tc.name, got.Reason)
		}
	}
}

// A server that never joined a cluster does nothing at boot: no probe, no registry, no peers.
func TestDecideBootForASingleServer(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	env := BootEnv{Cfg: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.toml"), DSNs: []string{"host=/nonexistent port=1 user=x"}, Wait: time.Millisecond}
	d, err := DecideBoot(context.Background(), env)
	if err != nil || d.Role != RoleLeader || d.Joined || d.DSN != "" {
		t.Fatalf("single server: %+v, %v", d, err)
	}
}

// A fenced record decides at once, even with a certificate and a stopped database: the node stays
// fenced across a restart that cannot reach anybody.
func TestDecideBootStaysFencedFromItsRecord(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	conf := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteFenced(cfg, FencedRecord{Epoch: 5, Leader: "n2", Reason: "replaced", At: time.Now(), Peers: map[string]string{"n2": "10.0.0.2:7443"}}); err != nil {
		t.Fatal(err)
	}
	env := BootEnv{Cfg: cfg, ConfigPath: conf, DSNs: []string{"host=/nonexistent port=1 user=x"}, Wait: time.Millisecond}
	d, err := DecideBoot(context.Background(), env)
	if err != nil || d.Role != RoleFenced || d.Epoch != 5 || d.Leader != "n2" || d.Reason != "replaced" {
		t.Fatalf("fenced: %+v, %v", d, err)
	}
	rec, _ := ReadFenced(cfg)
	if rec == nil || rec.Peers["n2"] != "10.0.0.2:7443" {
		t.Fatalf("the record: %+v", rec)
	}
	if err := ClearFenced(cfg); err != nil {
		t.Fatal(err)
	}
	if rec, _ := ReadFenced(cfg); rec != nil {
		t.Fatal("the record was not cleared")
	}
	if err := ClearFenced(cfg); err != nil {
		t.Fatalf("clearing a record that is not there: %v", err)
	}
}

// A joined node whose database never answers fails after the wait; it does not guess.
func TestDecideBootFailsWhenTheSystemClusterNeverAnswers(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	conf := filepath.Join(t.TempDir(), "config.toml")
	ca, _ := NewCA(newSecrets(t))
	key, _ := NewKey()
	iss, err := ca.Issue(key.Public().(ed25519.PublicKey), "n1", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveIdentity(config.ClusterDir(conf), key, iss.DER, ca.DER()); err != nil {
		t.Fatal(err)
	}
	env := BootEnv{Cfg: cfg, ConfigPath: conf, DSNs: []string{"host=/nonexistent port=1 user=x connect_timeout=1"}, Wait: 50 * time.Millisecond}
	if _, err := DecideBoot(context.Background(), env); !errors.Is(err, ErrRegistryUnreachable) {
		t.Fatalf("DecideBoot = %v", err)
	}
	if !Joined(config.ClusterDir(conf)) {
		t.Fatal("Joined")
	}
	creds, err := LoadCredentials(config.ClusterDir(conf))
	if err != nil || creds.NodeID != "n1" || creds.Serial != iss.Serial {
		t.Fatalf("credentials: %+v, %v", creds, err)
	}
	info, _ := os.Stat(filepath.Join(config.ClusterDir(conf), config.NodeKeyFile))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("node.key is %v", info.Mode().Perm())
	}
}

// ---- Live ----

type probe struct {
	mu  sync.Mutex
	rec bool
	err error
}

func (p *probe) set(rec bool, err error) { p.mu.Lock(); p.rec, p.err = rec, err; p.mu.Unlock() }
func (p *probe) InRecovery(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rec, p.err
}

func newLive(t *testing.T, boot BootDecision, p *probe) (*Live, *registry.Memory, *config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	reg := registry.NewMemory()
	if err := reg.CreateNode(context.Background(), &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
		t.Fatal(err)
	}
	boot.Joined, boot.Epoch = true, 1
	l := NewLive(LiveOptions{Cfg: cfg, Reg: reg, SelfID: boot.SelfID, Boot: boot, InRecovery: p.InRecovery, Poll: 10 * time.Millisecond})
	return l, reg, cfg
}

func TestLiveFollowsTheRegistryAndTheProbe(t *testing.T) {
	p := &probe{}
	l, reg, _ := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, p)
	l.Refresh(context.Background())
	snap := <-l.Watch(context.Background())
	if !l.IsLeader() || l.Role() != RoleLeader || l.Self().ID != "n1" || len(l.Nodes()) != 2 || l.Epoch() != 1 || snap.Leader != "n1" {
		t.Fatalf("leader: %+v", snap)
	}
	if lead, ok := l.Leader(); !ok || lead.ID != "n1" {
		t.Fatalf("Leader() = %+v %v", lead, ok)
	}
	select {
	case <-l.Changed():
		t.Fatal("changed with nothing to change")
	default:
	}

	// A node row change reaches a watcher.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := l.Watch(ctx)
	<-ch // the first snapshot
	if err := reg.SetNodeState(ctx, "n2", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	l.Refresh(ctx)
	select {
	case s := <-ch:
		if s.Nodes[1].State != registry.NodeFenced {
			t.Fatalf("snapshot %+v", s.Nodes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher heard nothing")
	}

	// A failed probe leaves the role alone; one poll of a different role is not yet a change; two are.
	p.set(false, errors.New("socket gone"))
	l.Refresh(ctx)
	if l.Role() != RoleLeader {
		t.Fatal("a failed probe changed the role")
	}
	p.set(true, nil) // demoted in place
	l.Refresh(ctx)
	if l.Role() != RoleFollower || l.IsLeader() {
		t.Fatalf("role %s", l.Role())
	}
	select {
	case <-l.Changed():
		t.Fatal("one poll was taken for a change")
	default:
	}
	p.set(false, nil)
	l.Refresh(ctx) // back to the boot role: the count starts over
	p.set(true, nil)
	l.Refresh(ctx)
	l.Refresh(ctx)
	select {
	case <-l.Changed():
		if !strings.Contains(l.Why(), "started as leader and is now follower") {
			t.Fatalf("why: %q", l.Why())
		}
	default:
		t.Fatal("two polls of another role did not close Changed")
	}
}

// A follower whose system cluster is promoted becomes the leader at once (I1), whatever the
// registry still says, and the daemon is told to restart in that role.
func TestLiveSeesAPromotion(t *testing.T) {
	p := &probe{rec: true}
	l, _, _ := newLive(t, BootDecision{Role: RoleFollower, SelfID: "n2"}, p)
	ctx := context.Background()
	l.Refresh(ctx)
	if l.IsLeader() {
		t.Fatal("a standby is not the leader")
	}
	if lead, ok := l.Leader(); !ok || lead.ID != "n1" {
		t.Fatalf("leader %+v %v", lead, ok)
	}
	p.set(false, nil) // pg_promote
	l.Refresh(ctx)
	if !l.IsLeader() {
		t.Fatal("a primary system cluster is the leader")
	}
	if lead, ok := l.Leader(); !ok || lead.ID != "n2" {
		t.Fatalf("after the promotion the leader is %+v (the registry still names n1)", lead)
	}
	l.Refresh(ctx)
	select {
	case <-l.Changed():
	default:
		t.Fatal("no change after a promotion")
	}
}

func TestLiveFencesALeaderThatHearsOfAHigherEpoch(t *testing.T) {
	p := &probe{}
	l, reg, cfg := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, p)
	var got []FencedRecord
	l.o.OnFenced = func(r FencedRecord) { got = append(got, r) }
	ctx := context.Background()
	n2, err := reg.GetNode(ctx, "n2")
	if err != nil {
		t.Fatal(err)
	}
	n2.PeerAddr = "10.0.0.2:7443"
	if err := reg.UpdateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	l.Refresh(ctx)

	for _, tc := range []struct {
		name          string
		epoch         int64
		leader        string
		fencesAtEpoch bool
	}{
		{"a lower epoch", 0, "n2", false}, {"no leader named", 9, "", false}, {"this node is the leader", 9, "n1", false},
	} {
		l.ObserveEpoch("n2", tc.epoch, tc.leader)
		if l.Role() != RoleLeader || len(got) != 0 {
			t.Fatalf("%s fenced the leader", tc.name)
		}
	}
	l.ObserveEpoch("n2", 1, "n2") // the same epoch under another leader is a split brain
	if l.Role() != RoleFenced || l.IsLeader() || len(got) != 1 {
		t.Fatalf("role %s, calls %d", l.Role(), len(got))
	}
	rec := l.Fenced()
	if rec == nil || rec.Leader != "n2" || rec.Epoch != 1 || !strings.Contains(rec.Reason, "node n2 says node n2 leads") {
		t.Fatalf("record %+v", rec)
	}
	onDisk, err := ReadFenced(cfg)
	if err != nil || onDisk == nil || onDisk.Leader != "n2" {
		t.Fatalf("fenced.json: %+v, %v", onDisk, err)
	}
	// `node rejoin` needs the peers' addresses, and the record written at run time is the one most
	// fenced nodes keep.
	if onDisk.Peers["n2"] != "10.0.0.2:7443" || len(onDisk.Peers) != 1 {
		t.Fatalf("the record lost the peers: %+v", onDisk.Peers)
	}
	select {
	case <-l.Changed():
	default:
		t.Fatal("a fenced leader must restart into fenced mode")
	}
	l.ObserveEpoch("n3", 7, "n3") // fenced already: once
	if len(got) != 1 {
		t.Fatal("fenced twice")
	}
	l.Refresh(ctx)
	if l.Role() != RoleFenced {
		t.Fatal("a refresh un-fenced the node")
	}

	// A leader whose database is stopped writes nothing: a planned switchover looks like this to the new
	// leader until the old one is demoted in place. It is not fenced by the new leader's pings.
	g, _, _ := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
	gp := &probe{}
	g.o.InRecovery = gp.InRecovery
	gp.set(false, errors.New("the system cluster is stopped"))
	g.Refresh(ctx)
	g.ObserveEpoch("n2", 2, "n2")
	if g.Role() != RoleLeader || g.Fenced() != nil {
		t.Fatal("a leader with its database down was fenced")
	}
	gp.set(false, nil) // the database is back as a primary: now it is a zombie
	g.Refresh(ctx)
	g.ObserveEpoch("n2", 2, "n2")
	if g.Role() != RoleFenced {
		t.Fatal("a primary that hears of a higher epoch was not fenced")
	}

	// A follower does not fence itself on anyone's word.
	f, _, _ := newLive(t, BootDecision{Role: RoleFollower, SelfID: "n2"}, &probe{rec: true})
	f.Refresh(ctx)
	f.ObserveEpoch("n1", 99, "n3")
	if f.Role() != RoleFollower {
		t.Fatal("a follower fenced itself")
	}
}

func TestLiveStartsFencedWhenBootedFenced(t *testing.T) {
	l, _, _ := newLive(t, BootDecision{Role: RoleFenced, SelfID: "n1", Epoch: 5, Leader: "n2", Reason: "x"}, &probe{})
	l.Refresh(context.Background())
	if l.Role() != RoleFenced || l.IsLeader() || l.Fenced() == nil || l.Fenced().Leader != "n2" {
		t.Fatalf("role %s, record %+v", l.Role(), l.Fenced())
	}
}

// ---- removal ----

func TestRemoveNode(t *testing.T) {
	ctx := context.Background()
	newReg := func() *registry.Memory {
		reg := registry.NewMemory()
		for _, n := range []registry.Node{{Name: "two", State: registry.NodeActive}, {Name: "three", State: registry.NodeActive}} {
			if err := reg.CreateNode(ctx, &n); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range []registry.Project{{Ref: "system", Name: "system"}, {Ref: "aaaaaaaaaaaaaaaaaaaa", Name: "a"}, {Ref: "bbbbbbbbbbbbbbbbbbbb", Name: "b"}} {
			if err := reg.CreateProject(ctx, &p); err != nil {
				t.Fatal(err)
			}
		}
		for _, r := range []registry.Replica{
			{Identifier: "system-rr-local-sys001", Ref: "system", NodeID: "n2", Origin: registry.ReplicaSystem},
			{Identifier: "aaaaaaaaaaaaaaaaaaaa-rr-local-rep001", Ref: "aaaaaaaaaaaaaaaaaaaa", NodeID: "n2"},
		} {
			if err := reg.CreateReplica(ctx, &r); err != nil {
				t.Fatal(err)
			}
		}
		return reg
	}

	reg := newReg()
	if err := RemoveNode(ctx, reg, "n1", RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "leader") {
		t.Fatalf("removing the leader: %v", err)
	}
	if err := RemoveNode(ctx, reg, "n9", RemoveOptions{}); err == nil {
		t.Fatal("removing a node that does not exist")
	}
	if err := reg.SetNodeState(ctx, "n3", registry.NodeActive); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetProjectNode(ctx, "bbbbbbbbbbbbbbbbbbbb", "n3", 1); err != nil {
		t.Fatal(err)
	}
	if err := RemoveNode(ctx, reg, "n3", RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "bbbbbbbbbbbbbbbbbbbb") {
		t.Fatalf("removing a node that homes a project: %v", err)
	}

	// The replica controller removes the user replica while the removal waits; the system replica row goes at once.
	// The node has left by the time the replica is marked: the default reconciler would make it again
	// on a node that is active.
	leftWhenMarked := make(chan bool, 1)
	go func() {
		for range 200 {
			time.Sleep(10 * time.Millisecond)
			if r, err := reg.GetReplica(ctx, "aaaaaaaaaaaaaaaaaaaa-rr-local-rep001"); err == nil && r.Status == string(registry.StatusGoingDown) {
				n, _ := reg.GetNode(ctx, "n2")
				leftWhenMarked <- n.State == registry.NodeLeft
				_ = reg.DeleteReplica(ctx, r.Identifier)
				return
			}
		}
		leftWhenMarked <- false
	}()
	var waited []string
	err := RemoveNode(ctx, reg, "n2", RemoveOptions{Wait: 5 * time.Second, Poll: 10 * time.Millisecond, Log: func(f string, a ...any) { waited = append(waited, f) }})
	if err != nil {
		t.Fatal(err)
	}
	if !<-leftWhenMarked {
		t.Error("the replica was marked going down before the node left")
	}
	if n, _ := reg.GetNode(ctx, "n2"); n.State != registry.NodeLeft {
		t.Fatalf("state %s", n.State)
	}
	if rs, _ := reg.ListReplicasOn(ctx, "n2"); len(rs) != 0 {
		t.Fatalf("replicas left: %v", rs)
	}
	if len(waited) == 0 {
		t.Error("the removal did not say it was waiting")
	}
	if err := RemoveNode(ctx, reg, "n2", RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "removed already") {
		t.Fatalf("removing twice: %v", err)
	}

	// Nobody removes the replica: the wait ends with an error that names it and says the node is gone
	// already. The same command, run again, finishes with --force.
	reg = newReg()
	err = RemoveNode(ctx, reg, "n2", RemoveOptions{Wait: 50 * time.Millisecond, Poll: 10 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "aaaaaaaaaaaaaaaaaaaa-rr-local-rep001") || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "is removed") {
		t.Fatalf("a replica that stays: %v", err)
	}
	if n, _ := reg.GetNode(ctx, "n2"); n.State != registry.NodeLeft {
		t.Fatalf("the node is %s although its removal was asked for", n.State)
	}
	if r, _ := reg.GetReplica(ctx, "aaaaaaaaaaaaaaaaaaaa-rr-local-rep001"); r == nil || r.Status != string(registry.StatusGoingDown) {
		t.Fatalf("the replica is %+v", r)
	}
	if err := RemoveNode(ctx, reg, "n2", RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if rs, _ := reg.ListReplicasOn(ctx, "n2"); len(rs) != 0 {
		t.Fatalf("replicas left after --force: %v", rs)
	}
	if err := RemoveNode(ctx, reg, "n2", RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "removed already") {
		t.Fatalf("removing after --force: %v", err)
	}

	// --force on a node that is active does not wait.
	reg = newReg()
	if err := RemoveNode(ctx, reg, "n2", RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if n, _ := reg.GetNode(ctx, "n2"); n.State != registry.NodeLeft {
		t.Fatalf("state %s", n.State)
	}

	// Inside the daemon the controller removes the replicas: it is asked after the node left, and the
	// rows it leaves GOING_DOWN are waited for.
	reg = newReg()
	rm := &fakeRemover{reg: reg}
	if err := RemoveNode(ctx, reg, "n2", RemoveOptions{Remover: rm, Wait: time.Second, Poll: 10 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if rm.calls != 1 || rm.node != "n2" || rm.state != registry.NodeLeft {
		t.Fatalf("remover: %+v", rm)
	}
	if rs, _ := reg.ListReplicasOn(ctx, "n2"); len(rs) != 0 {
		t.Fatalf("replicas left: %v", rs)
	}
}

// fakeRemover removes the replicas on a node as the replica controller does, and notes the state the
// node was in when it was asked.
type fakeRemover struct {
	reg   registry.Registry
	calls int
	node  string
	state registry.NodeState
}

func (f *fakeRemover) RemoveOn(ctx context.Context, node string) error {
	f.calls, f.node = f.calls+1, node
	n, err := f.reg.GetNode(ctx, node)
	if err != nil {
		return err
	}
	f.state = n.State
	rs, _ := f.reg.ListReplicasOn(ctx, node)
	for _, r := range rs {
		_ = f.reg.DeleteReplica(ctx, r.Identifier)
	}
	return nil
}

// ---- diverged data, fencing the local clusters ----

func TestMoveDivergedSetsDataAsideAndPrunes(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	mk := func(ref string) string {
		d := cfg.Paths().PostgresData(ref)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "PG_VERSION"), []byte("17"), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	sys, a := mk("system"), mk("aaaaaaaaaaaaaaaaaaaa")
	if err := os.MkdirAll(cfg.Paths().Project("empty"), 0o700); err != nil { // a project directory with no data
		t.Fatal(err)
	}
	old := filepath.Join(filepath.Dir(sys), "data.diverged-2")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(old, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}
	moved, err := MoveDiverged(cfg, 4, time.Now())
	if err != nil || len(moved) != 2 {
		t.Fatalf("moved %v, %v", moved, err)
	}
	for _, d := range []string{sys, a} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s is still there", d)
		}
		if b, err := os.ReadFile(filepath.Join(d+".diverged-4", "PG_VERSION")); err != nil || string(b) != "17" {
			t.Errorf("%s: %q, %v", d, b, err)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a diverged directory older than keep_diverged_days was kept")
	}
	// The name is taken the next time at the same epoch: a number is added.
	if err := os.MkdirAll(sys, 0o700); err != nil {
		t.Fatal(err)
	}
	moved, err = MoveDiverged(cfg, 4, time.Now())
	if err != nil || len(moved) != 1 || !strings.HasSuffix(moved[0], "data.diverged-4-2") {
		t.Fatalf("second move %v, %v", moved, err)
	}
}

// A rename keeps a directory's own time, which for a fenced primary is the day it stopped. The
// retention of what was set aside runs from the move: a directory whose time is days old survives
// the call that moved it, and goes only once it has been set aside for longer than the retention.
func TestMoveDivergedRetentionStartsAtTheMove(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	data := cfg.Paths().PostgresData("system")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "PG_VERSION"), []byte("17"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := time.Now().Add(-20 * 24 * time.Hour) // the primary was fenced three weeks ago
	if err := os.Chtimes(data, stopped, stopped); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	moved, err := MoveDiverged(cfg, 4, now)
	if err != nil || len(moved) != 1 {
		t.Fatalf("moved %v, %v", moved, err)
	}
	if b, err := os.ReadFile(filepath.Join(moved[0], "PG_VERSION")); err != nil || string(b) != "17" {
		t.Fatalf("the data set aside by the call that moved it: %q, %v", b, err)
	}
	if fi, err := os.Stat(moved[0]); err != nil || now.Sub(fi.ModTime()) > time.Minute {
		t.Fatalf("the moved directory was not stamped: %v, %v", fi, err)
	}
	// Later calls keep it for the retention, and only then remove it.
	keep := cfg.Failover.KeepDiverged()
	for _, tc := range []struct {
		after time.Duration
		kept  bool
	}{{time.Hour, true}, {keep - time.Hour, true}, {keep + time.Hour, false}} {
		if _, err := MoveDiverged(cfg, 5, now.Add(tc.after)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(moved[0]); (err == nil) != tc.kept {
			t.Fatalf("%s after the move the directory is kept=%v, want %v", tc.after, err == nil, tc.kept)
		}
	}
}

// The ping of the boot decision is authenticated by the chain to the cluster CA. The copy of the
// registry that a returning node reads may be old: a peer that renewed its certificate while the node
// was down has a serial that copy does not show, and its answer is wanted all the same.
func TestBootPingDoesNotCheckSerialsAgainstAStaleRegistryCopy(t *testing.T) {
	l := newLeader(t)
	ctx := context.Background()
	j := l.joiner(t, "n2")
	seed, _ := okSeed(t)
	if _, err := Join(ctx, j.joinOptions(l.token(t, TokenOptions{}), "second", seed)); err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCredentials(config.ClusterDir(j.confPath))
	if err != nil {
		t.Fatal(err)
	}
	env := BootEnv{Cfg: j.cfg, ConfigPath: j.confPath, Log: quiet(), PeerTimeout: 5 * time.Second,
		openRegistry: func(context.Context, string) (registry.Registry, error) { return oldSerials{l.reg}, nil }}
	ev, err := env.gather(ctx, "unused", creds)
	if err != nil || len(ev.Peers) != 1 || ev.Peers[0].Node != "n1" || ev.Peers[0].Leader != "n1" {
		t.Fatalf("evidence %+v, %v", ev, err)
	}
}

// oldSerials is a registry whose copy shows no node's current certificate serial.
type oldSerials struct{ registry.Registry }

func (o oldSerials) ListNodes(ctx context.Context) ([]registry.Node, error) {
	ns, err := o.Registry.ListNodes(ctx)
	for i := range ns {
		ns[i].CertSerial = "00"
	}
	return ns, err
}

func TestAssumeLeadership(t *testing.T) {
	ctx := context.Background()
	mk := func() *registry.Memory {
		reg := registry.NewMemory()
		if err := reg.CreateNode(ctx, &registry.Node{Name: "second", State: registry.NodeActive}); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	now := time.Now()

	// The node that has led all along changes nothing.
	reg := mk()
	if planned, err := AssumeLeadership(ctx, reg, "n1", 1, now); err != nil || planned {
		t.Fatalf("steady state: %v %v", planned, err)
	}
	if cl, _ := reg.GetCluster(ctx); cl.Epoch != 1 || cl.Leader != "n1" {
		t.Fatalf("%+v", cl)
	}

	// An unplanned promotion: n2 leads at epoch 2 and n1 is fenced.
	if planned, err := AssumeLeadership(ctx, reg, "n2", 2, now); err != nil || planned {
		t.Fatalf("unplanned: %v %v", planned, err)
	}
	cl, _ := reg.GetCluster(ctx)
	n1, _ := reg.GetNode(ctx, "n1")
	if cl.Leader != "n2" || cl.Epoch != 2 || n1.State != registry.NodeFenced {
		t.Fatalf("leader %s epoch %d, n1 %s", cl.Leader, cl.Epoch, n1.State)
	}
	// Asked again it does nothing, and a lower epoch is refused by the registry.
	if _, err := AssumeLeadership(ctx, reg, "n2", 2, now); err != nil {
		t.Fatal(err)
	}
	if _, err := AssumeLeadership(ctx, reg, "n1", 2, now); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("a lower claim: %v", err)
	}

	// A planned switchover: maintenance names the old leader, so it stays active.
	reg = mk()
	if err := reg.SetMaintenance(ctx, registry.Maintenance{Node: "n1", Until: now.Add(time.Hour), Reason: "switchover"}); err != nil {
		t.Fatal(err)
	}
	if planned, err := AssumeLeadership(ctx, reg, "n2", 2, now); err != nil || !planned {
		t.Fatalf("planned: %v %v", planned, err)
	}
	if n1, _ := reg.GetNode(ctx, "n1"); n1.State != registry.NodeActive {
		t.Fatalf("the old leader of a planned switchover is %s", n1.State)
	}
	// An announcement about another node, or an expired one, does not make it planned.
	reg = mk()
	if err := reg.SetMaintenance(ctx, registry.Maintenance{Node: "n1", Until: now.Add(-time.Minute), Reason: "old"}); err != nil {
		t.Fatal(err)
	}
	if planned, _ := AssumeLeadership(ctx, reg, "n2", 2, now); planned {
		t.Fatal("an expired announcement counted")
	}
}

func TestPlainDSNDropsThePoolParameters(t *testing.T) {
	for in, want := range map[string]string{
		"host='/var/lib/supavise/projects/system/postgres/sock' port=5433 user=supabase_admin dbname=supavise sslmode=disable connect_timeout=5 application_name=supavise pool_max_conns=6": "host='/var/lib/supavise/projects/system/postgres/sock' port=5433 user=supabase_admin dbname=supavise sslmode=disable connect_timeout=5 application_name=supavise",
		"host=/s port=1 pool_max_conns=6 pool_min_conns=1 dbname=x": "host=/s port=1 dbname=x",
		"host=/s port=1": "host=/s port=1",
		"postgres://u:p@127.0.0.1:5432/db?sslmode=disable&pool_max_conns=6": "postgres://u:p@127.0.0.1:5432/db?sslmode=disable",
	} {
		if got := PlainDSN(in); got != want {
			t.Errorf("PlainDSN(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- Live: the failover procedure's side of the membership ----

// The cooperative fence of the failover procedure writes fenced.json from outside the daemon's own
// observation. The membership reports the node fenced as soon as it reads the record, tells the daemon
// to restart in that role, and does so when the registry cannot be read either: the fence stops the
// system cluster too.
func TestLiveReportsFencedWhenTheRecordAppears(t *testing.T) {
	ctx := context.Background()
	l, reg, cfg := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
	l.Refresh(ctx)
	if l.Role() != RoleLeader {
		t.Fatalf("role %s", l.Role())
	}
	if err := fenced.WriteNode(cfg.Paths(), fenced.Record{Epoch: 2, Leader: "n2", Reason: "a survivor fenced this node", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	l.Refresh(ctx)
	if l.Role() != RoleFenced || l.IsLeader() {
		t.Fatalf("role %s after the record appeared", l.Role())
	}
	if rec := l.Fenced(); rec == nil || rec.Leader != "n2" || rec.Epoch != 2 {
		t.Fatalf("record %+v", rec)
	}
	select {
	case <-l.Changed():
	default:
		t.Fatal("the daemon was not told to restart as a fenced node")
	}

	// The registry is down: the node is fenced all the same.
	g, _, gcfg := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
	g.Refresh(ctx)
	g.o.Reg = noCluster{reg}
	if err := fenced.WriteNode(gcfg.Paths(), fenced.Record{Epoch: 2, Leader: "n2", Reason: "fenced", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	g.Refresh(ctx)
	if g.Role() != RoleFenced {
		t.Fatalf("role %s with the registry down", g.Role())
	}

	// A record that cannot be read does not make a leader a fenced node by itself.
	h, _, hcfg := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
	if err := os.WriteFile(fenced.NodePath(hcfg.Paths()), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Refresh(ctx)
	if h.Role() != RoleLeader {
		t.Fatalf("role %s with a damaged record", h.Role())
	}
}

type noCluster struct{ registry.Registry }

func (noCluster) GetCluster(context.Context) (*registry.Cluster, error) {
	return nil, errors.New("the system cluster is stopped")
}

// The orchestrator's FenceOnHigherEpoch is asked while the membership still reports the leader, and its
// verdict decides: a node it will not fence keeps leading, one it fences is fenced and the daemon told.
func TestLiveAsksTheFencerBeforeItFencesALeader(t *testing.T) {
	ctx := context.Background()
	l, reg, cfg := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
	n2, err := reg.GetNode(ctx, "n2")
	if err != nil {
		t.Fatal(err)
	}
	n2.PeerAddr = "10.0.0.2:7443"
	if err := reg.UpdateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	var calls []string
	var verdict bool
	var ferr error
	l.o.Fence = func(_ context.Context, source string, epoch int64, leader string) (bool, error) {
		calls = append(calls, fmt.Sprintf("%s|%d|%s|leader=%v", source, epoch, leader, l.IsLeader()))
		return verdict, ferr
	}
	var told []FencedRecord
	l.o.OnFenced = func(r FencedRecord) { told = append(told, r) }
	l.Refresh(ctx)

	l.ObserveEpoch("n2", 5, "n2") // the fencer says no
	if l.Role() != RoleLeader || len(told) != 0 || len(calls) != 1 {
		t.Fatalf("role %s, told %d, calls %v", l.Role(), len(told), calls)
	}
	if calls[0] != "node n2|5|n2|leader=true" {
		t.Fatalf("the fencer was asked %q", calls[0])
	}
	verdict, ferr = true, errors.New("a primary did not stop")
	l.ObserveEpoch("n2", 5, "n2")
	if l.Role() != RoleFenced || len(told) != 1 || len(calls) != 2 {
		t.Fatalf("role %s, told %d, calls %v", l.Role(), len(told), calls)
	}
	if rec, err := ReadFenced(cfg); err != nil || rec == nil || rec.Leader != "n2" || len(rec.Peers) != 1 {
		t.Fatalf("fenced.json: %+v, %v", rec, err)
	}
	l.ObserveEpoch("n3", 6, "n3")
	if len(calls) != 2 || len(told) != 1 {
		t.Fatal("fenced twice")
	}

	// The record the fencer wrote and a refresh adopted does not stop the daemon from being told once.
	m, _, mcfg := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
	var mtold int
	m.o.OnFenced = func(FencedRecord) { mtold++ }
	m.o.Fence = func(context.Context, string, int64, string) (bool, error) {
		if err := fenced.WriteNode(mcfg.Paths(), fenced.Record{Epoch: 5, Leader: "n2", Reason: "fenced", At: time.Now()}); err != nil {
			t.Error(err)
		}
		m.Refresh(ctx) // the membership polls while the primaries stop
		return true, nil
	}
	m.Refresh(ctx)
	m.ObserveEpoch("n2", 5, "n2")
	if m.Role() != RoleFenced || mtold != 1 {
		t.Fatalf("role %s, told %d", m.Role(), mtold)
	}
}

// A leader that no peer reaches learns from the backup store's leader marker that another node was
// promoted; a store that does not answer, a follower and a cluster of one node leave things as they are.
func TestLiveReadsTheLeaderMarker(t *testing.T) {
	ctx := context.Background()
	newLeader := func(m fakeMarker) *Live {
		l, _, _ := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
		l.o.Marker = m
		l.Refresh(ctx)
		return l
	}
	l := newLeader(fakeMarker{err: errors.New("the store does not answer")})
	l.CheckMarker(ctx)
	if l.Role() != RoleLeader {
		t.Fatal("an unreadable marker fenced the leader")
	}
	l = newLeader(fakeMarker{m: &backup.LeaderMarker{Epoch: 1, Leader: "n1"}})
	l.CheckMarker(ctx)
	if l.Role() != RoleLeader {
		t.Fatal("a marker that names this node fenced it")
	}
	l = newLeader(fakeMarker{m: &backup.LeaderMarker{Epoch: 3, Leader: "n2"}})
	var called string
	l.o.Fence = func(_ context.Context, source string, epoch int64, leader string) (bool, error) {
		called = fmt.Sprintf("%s|%d|%s", source, epoch, leader)
		return true, nil
	}
	l.CheckMarker(ctx)
	if l.Role() != RoleFenced || called != "the leader marker in the backup store|3|n2" {
		t.Fatalf("role %s, fencer asked %q", l.Role(), called)
	}
	if rec := l.Fenced(); rec == nil || !strings.Contains(rec.Reason, "leader marker") {
		t.Fatalf("record %+v", rec)
	}

	f, _, _ := newLive(t, BootDecision{Role: RoleFollower, SelfID: "n2"}, &probe{rec: true})
	f.o.Marker = fakeMarker{m: &backup.LeaderMarker{Epoch: 9, Leader: "n3"}}
	f.Refresh(ctx)
	f.CheckMarker(ctx)
	if f.Role() != RoleFollower {
		t.Fatal("a follower fenced itself on the marker")
	}

	// Run reads the marker on its own timer.
	r := newLeader(fakeMarker{m: &backup.LeaderMarker{Epoch: 3, Leader: "n2"}})
	r.o.MarkerEvery = 10 * time.Millisecond
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.Run(rctx)
	eventually(t, "Run to read the marker", func() bool { return r.Role() == RoleFenced })
}

// A node that was just promoted leads at the epoch it was promoted for before the registry says so:
// the procedure that promoted it waits for the membership to show it. An older promote.ok shows nothing.
func TestLiveShowsThePromotedEpoch(t *testing.T) {
	ctx := context.Background()
	p := &probe{rec: true}
	l, _, cfg := newLive(t, BootDecision{Role: RoleFollower, SelfID: "n2"}, p)
	l.Refresh(ctx)
	path := cfg.Paths().PromoteOK(config.SystemRef)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, backup.FormatPromoteOK(4), 0o600); err != nil {
		t.Fatal(err)
	}
	l.Refresh(ctx) // still a standby: promote.ok is written before pg_promote, and says nothing yet
	if l.IsLeader() || l.Epoch() != 1 {
		t.Fatalf("a standby: leader %v, epoch %d", l.IsLeader(), l.Epoch())
	}
	p.set(false, nil)
	l.Refresh(ctx)
	if !l.IsLeader() || l.Epoch() != 4 {
		t.Fatalf("after the promotion: leader %v, epoch %d, want epoch 4", l.IsLeader(), l.Epoch())
	}
	if lead, ok := l.Leader(); !ok || lead.ID != "n2" {
		t.Fatalf("leader %+v", lead)
	}
	// A promote.ok from an epoch the registry has passed changes nothing.
	if err := os.WriteFile(path, backup.FormatPromoteOK(1), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _, mcfg := newLive(t, BootDecision{Role: RoleLeader, SelfID: "n1"}, &probe{})
	mp := mcfg.Paths().PromoteOK(config.SystemRef)
	_ = os.MkdirAll(filepath.Dir(mp), 0o750)
	if err := os.WriteFile(mp, backup.FormatPromoteOK(1), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Refresh(ctx)
	if m.Epoch() != 1 {
		t.Fatalf("epoch %d", m.Epoch())
	}
}
