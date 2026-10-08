package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMemoryCluster runs the cluster checks against the in-memory registry.
func TestMemoryCluster(t *testing.T) { testCluster(t, NewMemory()) }

// TestPostgresCluster runs them against a real database (a fresh one, so the founder row and the
// counters are the ones migration 1300 leaves).
func TestPostgresCluster(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	r, err := Open(context.Background(), tempDatabase(t, dsn, "supavise_cluster"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	testCluster(t, r)
}

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
)

func wantErr(t *testing.T, what string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

func testCluster(t *testing.T, r Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	changes, err := r.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seenMu sync.Mutex
	seen := map[string]bool{}
	go func() {
		for c := range changes {
			seenMu.Lock()
			seen[c.Table] = true
			seenMu.Unlock()
		}
	}()

	// A new registry holds the founder and a cluster of one.
	nodes, err := r.ListNodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].ID != FounderNodeID || nodes[0].Name != "primary" || nodes[0].State != NodeActive {
		t.Fatalf("founder: %+v, %v", nodes, err)
	}
	cl, err := r.GetCluster(ctx)
	if err != nil || cl.Epoch != 1 || cl.Leader != FounderNodeID || cl.Maintenance.Active(time.Now()) {
		t.Fatalf("cluster: %+v, %v", cl, err)
	}
	seq0 := cl.ChangeSeq

	org, err := r.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Project{{Ref: "system", Name: "system", Status: StatusActiveHealthy}, {Ref: refA, OrgID: org.ID, Name: "a"}, {Ref: refB, OrgID: org.ID, Name: "b"}} {
		if err := r.CreateProject(ctx, p); err != nil || p.NodeID != FounderNodeID {
			t.Fatalf("create %s: %v, node %q", p.Ref, err, p.NodeID)
		}
	}
	if cl, _ = r.GetCluster(ctx); cl.ChangeSeq <= seq0 {
		t.Fatalf("change_seq did not move with the projects: %d -> %d", seq0, cl.ChangeSeq)
	}
	if err := r.CreateProject(ctx, &Project{Ref: "cccccccccccccccccccc", Name: "c", NodeID: "n99"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a project on a node that does not exist: %v", err)
	}

	// Nodes.
	n2 := &Node{Name: "replica-1", Region: "eu-west-1", PublicHost: "r1.example.test", PeerAddr: "203.0.113.2:7443"}
	if err := r.CreateNode(ctx, n2); err != nil || n2.ID != "n2" || n2.State != NodeJoining || n2.JoinedAt.IsZero() {
		t.Fatalf("create node: %v %+v", err, n2)
	}
	if err := r.CreateNode(ctx, &Node{Name: "replica-1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate node name: %v", err)
	}
	if err := r.CreateNode(ctx, &Node{Name: "Not Valid"}); err == nil {
		t.Fatal("a node name with a space and capitals")
	}
	if got, err := r.GetNodeByName(ctx, "replica-1"); err != nil || got.ID != "n2" || got.Region != "eu-west-1" {
		t.Fatalf("get by name: %v %+v", err, got)
	}
	if _, err := r.GetNode(ctx, "n77"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	seq1, _ := r.GetCluster(ctx)
	n2.Provider = NodeProvider{AWS: &NodeAWS{InstanceID: "i-0abc", Zone: "eu-west-1a", Region: "eu-west-1", AllocationID: "eipalloc-1"}}
	n2.Version = "v0.2.0"
	if err := r.UpdateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetNode(ctx, "n2"); !reflect.DeepEqual(got.Provider, n2.Provider) || got.Version != "v0.2.0" || got.State != NodeJoining {
		t.Fatalf("update node: %+v", got)
	}
	seq2, _ := r.GetCluster(ctx)
	if seq2.ChangeSeq <= seq1.ChangeSeq {
		t.Fatal("updating a node did not move change_seq")
	}
	if err := r.UpdateNode(ctx, n2); err != nil { // nothing changed: no write
		t.Fatal(err)
	}
	if seq3, _ := r.GetCluster(ctx); seq3.ChangeSeq != seq2.ChangeSeq {
		t.Fatal("an update that changed nothing was written")
	}
	if err := r.UpdateNode(ctx, &Node{ID: "n77", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update an unknown node: %v", err)
	}
	if err := r.UpdateNode(ctx, &Node{ID: FounderNodeID, Name: "replica-1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename onto another node's name: %v", err)
	}
	if err := r.SetNodeCert(ctx, "n2", "serial-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetNodeState(ctx, "n2", NodeState("sleeping")); err == nil {
		t.Fatal("an unknown node state")
	}
	if err := r.SetNodeState(ctx, "n77", NodeActive); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if got, _ := r.GetNode(ctx, "n2"); got.CertSerial != "serial-1" {
		t.Fatalf("cert serial: %+v", got)
	}
	// Ids go on past 9: "n10" sorts after "n9".
	for i := 3; i <= 10; i++ {
		if err := r.CreateNode(ctx, &Node{Name: fmt.Sprintf("filler-%d", i), State: NodeLeft}); err != nil {
			t.Fatal(err)
		}
	}
	nodes, _ = r.ListNodes(ctx)
	var ids []string
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	if strings.Join(ids, ",") != "n1,n2,n3,n4,n5,n6,n7,n8,n9,n10" {
		t.Fatalf("node order: %v", ids)
	}
	if err := r.DeleteNode(ctx, "n10"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteNode(ctx, "n10"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := r.DeleteNode(ctx, FounderNodeID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete the leader: %v", err)
	}

	// The cluster row.
	if err := r.SetClusterName(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetServiceAddress(ctx, ServiceAddress{IP: "198.51.100.7", AllocationID: "eipalloc-9"}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	if err := r.SetMaintenance(ctx, Maintenance{Node: "n1", Until: until, Reason: "upgrade"}); err != nil {
		t.Fatal(err)
	}
	cl, _ = r.GetCluster(ctx)
	if cl.Name != "prod" || cl.ServiceAddress.AllocationID != "eipalloc-9" || !cl.Maintenance.Active(time.Now()) || !cl.Maintenance.Until.Equal(until) || cl.Maintenance.Reason != "upgrade" {
		t.Fatalf("cluster after setters: %+v", cl)
	}
	if err := r.SetMaintenance(ctx, Maintenance{}); err != nil {
		t.Fatal(err)
	}
	if cl, _ = r.GetCluster(ctx); cl.Maintenance.Active(time.Now()) || cl.Maintenance.Node != "" {
		t.Fatalf("maintenance not cleared: %+v", cl.Maintenance)
	}

	// Moving a project needs the current epoch and an active node.
	wantErr(t, "unknown project", r.SetProjectNode(ctx, "zzzzzzzzzzzzzzzzzzzz", "n1", 1), ErrNotFound)
	wantErr(t, "wrong epoch", r.SetProjectNode(ctx, refA, "n1", 2), ErrConflict)
	wantErr(t, "unknown node", r.SetProjectNode(ctx, refA, "n77", 1), ErrNotFound)
	wantErr(t, "joining node", r.SetProjectNode(ctx, refA, "n2", 1), ErrConflict)
	if err := r.SetNodeState(ctx, "n2", NodeActive); err != nil {
		t.Fatal(err)
	}

	// Replicas.
	idA := ReplicaIdentifier(refA, "eu-west-1", "k3j9d2")
	if ref, region, id6, ok := ParseReplicaIdentifier(idA); !ok || ref != refA || region != "eu-west-1" || id6 != "k3j9d2" {
		t.Fatalf("parse: %q %q %q %v", ref, region, id6, ok)
	}
	for _, bad := range []string{"", "abc", refA + "-rr-eu-west-1", "SHORT-rr-eu-west-1-k3j9d2", refA + "-rr--k3j9d2x"} {
		if ValidReplicaIdentifier(bad) {
			t.Errorf("%q passed as an identifier", bad)
		}
	}
	wantErr(t, "a replica on the home node", r.CreateReplica(ctx, &Replica{Identifier: idA, Ref: refA, NodeID: "n1"}), ErrConflict)
	wantErr(t, "a replica of an unknown project", r.CreateReplica(ctx, &Replica{Identifier: ReplicaIdentifier("zzzzzzzzzzzzzzzzzzzz", "eu-west-1", "k3j9d2"), Ref: "zzzzzzzzzzzzzzzzzzzz", NodeID: "n2"}), ErrNotFound)
	wantErr(t, "a replica on an unknown node", r.CreateReplica(ctx, &Replica{Identifier: idA, Ref: refA, NodeID: "n77"}), ErrNotFound)
	if err := r.CreateReplica(ctx, &Replica{Identifier: "nope", Ref: refA, NodeID: "n2"}); err == nil {
		t.Fatal("a malformed identifier")
	}
	rep := &Replica{Identifier: idA, Ref: refA, NodeID: "n2"}
	if err := r.CreateReplica(ctx, rep); err != nil || rep.Origin != ReplicaManual || rep.Status != ReplicaInit || rep.InitStep != ReplicaStepRequested || rep.CreatedAt.IsZero() {
		t.Fatalf("create replica: %v %+v", err, rep)
	}
	wantErr(t, "the same identifier again", r.CreateReplica(ctx, &Replica{Identifier: idA, Ref: refA, NodeID: "n3"}), ErrConflict)
	wantErr(t, "a second replica on one node", r.CreateReplica(ctx, &Replica{Identifier: ReplicaIdentifier(refA, "eu-west-1", "zzzzzz"), Ref: refA, NodeID: "n2"}), ErrConflict)
	sys := &Replica{Identifier: ReplicaIdentifier("system", "eu-west-1", "abc123"), Ref: "system", NodeID: "n2", Origin: ReplicaSystem}
	if err := r.CreateReplica(ctx, sys); err != nil || sys.Origin != ReplicaSystem {
		t.Fatalf("system replica: %v %+v", err, sys)
	}
	if err := r.CreateNode(ctx, &Node{Name: "replica-2", State: NodeActive}); err != nil { // takes the id n10 freed
		t.Fatal(err)
	}
	if err := r.CreateReplica(ctx, &Replica{Identifier: ReplicaIdentifier(refB, "eu-west-1", "bbb222"), Ref: refB, NodeID: "n2", Origin: ReplicaDefault}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ListReplicas(ctx, refA); err != nil || len(got) != 1 || got[0].Identifier != idA {
		t.Fatalf("list by ref: %v %+v", err, got)
	}
	if got, _ := r.ListReplicas(ctx, ""); len(got) != 3 || got[0].Ref != refA || got[2].Ref != "system" {
		t.Fatalf("list all: %+v", got)
	}
	if got, _ := r.ListReplicasOn(ctx, "n2"); len(got) != 3 {
		t.Fatalf("list on a node: %+v", got)
	}
	if got, _ := r.ListReplicasOn(ctx, "n1"); len(got) != 0 {
		t.Fatalf("list on the founder: %+v", got)
	}
	if err := r.SetReplicaStatus(ctx, idA, "ACTIVE_HEALTHY", ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
	seq4, _ := r.GetCluster(ctx)
	if err := r.SetReplicaStatus(ctx, idA, "ACTIVE_HEALTHY", ReplicaStepDone, ""); err != nil {
		t.Fatal(err)
	}
	if seq5, _ := r.GetCluster(ctx); seq5.ChangeSeq != seq4.ChangeSeq {
		t.Fatal("a status report that changed nothing was written")
	}
	wantErr(t, "status of an unknown replica", r.SetReplicaStatus(ctx, "nope", "x", "y", "z"), ErrNotFound)
	if got, _ := r.GetReplica(ctx, idA); got.Status != "ACTIVE_HEALTHY" || got.InitStep != ReplicaStepDone || got.InitError != "" {
		t.Fatalf("replica status: %+v", got)
	}
	if err := r.SetReplicaStatus(ctx, idA, ReplicaInitError, "3_initiated_read_replica_setup", "3_read_replica_setup_failed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetReplica(ctx, idA); got.Status != ReplicaInitError || got.InitError != "3_read_replica_setup_failed" {
		t.Fatalf("replica failure: %+v", got)
	}
	wantErr(t, "delete a node with replicas", r.DeleteNode(ctx, "n2"), ErrConflict)

	// Opt-outs.
	for i := 0; i < 2; i++ { // putting one twice changes nothing
		if err := r.PutReplicaOptout(ctx, refB, "n3"); err != nil {
			t.Fatal(err)
		}
	}
	wantErr(t, "opt-out of an unknown project", r.PutReplicaOptout(ctx, "zzzzzzzzzzzzzzzzzzzz", "n3"), ErrNotFound)
	wantErr(t, "opt-out on an unknown node", r.PutReplicaOptout(ctx, refB, "n77"), ErrNotFound)
	if got, _ := r.ListReplicaOptouts(ctx); !reflect.DeepEqual(got, []ReplicaOptout{{Ref: refB, NodeID: "n3"}}) {
		t.Fatalf("opt-outs: %+v", got)
	}

	// Leadership: a compare-and-set on the epoch.
	wantErr(t, "leader on an unknown node", r.SetLeader(ctx, "n77", 2), ErrNotFound)
	wantErr(t, "stale epoch", r.SetLeader(ctx, "n2", 1), ErrConflict)
	if err := r.SetLeader(ctx, FounderNodeID, 1); err != nil { // the same leader at the same epoch: nothing changes
		t.Fatal(err)
	}
	if err := r.SetLeader(ctx, "n2", 2); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "another leader at the same epoch", r.SetLeader(ctx, FounderNodeID, 2), ErrConflict)
	if cl, _ = r.GetCluster(ctx); cl.Leader != "n2" || cl.Epoch != 2 {
		t.Fatalf("leader: %+v", cl)
	}
	wantErr(t, "project move at the old epoch", r.SetProjectNode(ctx, refA, "n2", 1), ErrConflict)
	// A project created now lives on the new leader.
	pc := &Project{Ref: "cccccccccccccccccccc", OrgID: org.ID, Name: "c"}
	if err := r.CreateProject(ctx, pc); err != nil || pc.NodeID != "n2" {
		t.Fatalf("project on the new leader: %v %q", err, pc.NodeID)
	}
	// Moving refA's home onto the node that holds its replica removes that replica row (I2).
	if err := r.SetProjectNode(ctx, refA, "n2", 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetProject(ctx, refA); got.NodeID != "n2" {
		t.Fatalf("home after the move: %q", got.NodeID)
	}
	wantErr(t, "the replica of the new home", func() error { _, err := r.GetReplica(ctx, idA); return err }(), ErrNotFound)
	if got, _ := r.ListReplicasOn(ctx, "n2"); len(got) != 2 {
		t.Fatalf("replicas on n2 after the move: %+v", got)
	}
	// The old home takes a replica row now, and deleting the project takes its rows with it.
	idOld := ReplicaIdentifier(refA, "us-east-1", "old111")
	if err := r.CreateReplica(ctx, &Replica{Identifier: idOld, Ref: refA, NodeID: "n1", Origin: ReplicaManual}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutReplicaOptout(ctx, refA, "n3"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteProject(ctx, refA); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "a replica of a deleted project", func() error { _, err := r.GetReplica(ctx, idOld); return err }(), ErrNotFound)
	if got, _ := r.ListReplicaOptouts(ctx); len(got) != 1 || got[0].Ref != refB {
		t.Fatalf("opt-outs after deleting a project: %+v", got)
	}
	if err := r.DeleteReplicaOptout(ctx, refB, "n3"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteReplicaOptout(ctx, refB, "n3"); err != nil {
		t.Fatalf("clearing an opt-out twice: %v", err)
	}
	if err := r.DeleteReplica(ctx, sys.Identifier); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "delete a replica twice", r.DeleteReplica(ctx, sys.Identifier), ErrNotFound)

	testJoinTokens(t, r)
	testMoves(t, r)

	deadline := time.Now().Add(5 * time.Second)
	for {
		seenMu.Lock()
		done := seen["nodes"] && seen["replicas"] && seen["projects"]
		got := fmt.Sprint(seen)
		seenMu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("change feed incomplete: %s", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testJoinTokens(t *testing.T, r Registry) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	tok := &JoinToken{ID: "tok1", SecretHash: []byte("hash"), NodeName: "replica-9", ExpiresAt: now.Add(time.Hour)}
	if err := r.CreateJoinToken(ctx, tok); err != nil || tok.CreatedAt.IsZero() || tok.UsedAt != nil {
		t.Fatalf("create token: %v %+v", err, tok)
	}
	wantErr(t, "a token id twice", r.CreateJoinToken(ctx, &JoinToken{ID: "tok1", SecretHash: []byte("x"), ExpiresAt: now.Add(time.Hour)}), ErrConflict)
	if got, err := r.GetJoinToken(ctx, "tok1"); err != nil || string(got.SecretHash) != "hash" || got.NodeName != "replica-9" {
		t.Fatalf("get token: %v %+v", err, got)
	}
	wantErr(t, "an unknown token", func() error { _, err := r.UseJoinToken(ctx, "nope", now); return err }(), ErrNotFound)
	if got, err := r.UseJoinToken(ctx, "tok1", now); err != nil || got.UsedAt == nil {
		t.Fatalf("use token: %v %+v", err, got)
	}
	_, err := r.UseJoinToken(ctx, "tok1", now)
	wantErr(t, "a second use", err, ErrTokenUsed)
	wantErr(t, "a second use is a conflict", err, ErrConflict)

	if err := r.CreateJoinToken(ctx, &JoinToken{ID: "old", SecretHash: []byte("h"), ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	_, err = r.UseJoinToken(ctx, "old", now)
	wantErr(t, "an expired token", err, ErrTokenExpired)
	if n, err := r.DeleteExpiredJoinTokens(ctx, now); err != nil || n != 1 {
		t.Fatalf("delete expired: %d %v", n, err)
	}
	if _, err := r.GetJoinToken(ctx, "tok1"); err != nil {
		t.Fatalf("a live token was deleted: %v", err)
	}
}

func testMoves(t *testing.T, r Registry) {
	t.Helper()
	ctx := context.Background()
	if err := r.CreateMove(ctx, &Move{Scope: "galaxy", Kind: MoveFailover}); err == nil {
		t.Fatal("an unknown move scope")
	}
	first := &Move{Scope: MoveProject, Kind: MoveSwitchover, Ref: refB, FromNode: "n1", ToNode: "n2", Epoch: 2}
	if err := r.CreateMove(ctx, first); err != nil || first.ID == 0 || first.State != MoveRunning || first.StartedAt.IsZero() || len(first.Steps) != 0 {
		t.Fatalf("create move: %v %+v", err, first)
	}
	second := &Move{Scope: MoveServer, Kind: MoveFailover, FromNode: "n2", ToNode: "n1", Epoch: 3}
	if err := r.CreateMove(ctx, second); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	for _, s := range []MoveStep{{Name: "quiesce", At: at}, {Name: "promote", At: at.Add(time.Second), Detail: "lsn 0/3000100"}} {
		if err := r.AppendMoveStep(ctx, first.ID, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.GetMove(ctx, first.ID)
	if err != nil || len(got.Steps) != 2 || got.Steps[1].Name != "promote" || got.Steps[1].Detail != "lsn 0/3000100" || !got.Steps[0].At.Equal(at) || got.Ref != refB {
		t.Fatalf("get move: %v %+v", err, got)
	}
	if err := r.FinishMove(ctx, first.ID, MoveRunning, ""); err == nil {
		t.Fatal("a move cannot finish as running")
	}
	if err := r.FinishMove(ctx, first.ID, MoveFailed, "promote timed out"); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetMove(ctx, first.ID)
	if got.State != MoveFailed || got.Error != "promote timed out" || got.EndedAt == nil {
		t.Fatalf("finished move: %+v", got)
	}
	wantErr(t, "finish an unknown move", r.FinishMove(ctx, 1<<40, MoveDone, ""), ErrNotFound)
	wantErr(t, "append to an unknown move", r.AppendMoveStep(ctx, 1<<40, MoveStep{Name: "x"}), ErrNotFound)
	all, _ := r.ListMoves(ctx, "", 0)
	if len(all) != 2 || all[0].ID != second.ID || all[0].Ref != "" {
		t.Fatalf("list moves: %+v", all)
	}
	if running, _ := r.ListMoves(ctx, MoveRunning, 10); len(running) != 1 || running[0].ID != second.ID {
		t.Fatalf("running moves: %+v", running)
	}
	if one, _ := r.ListMoves(ctx, "", 1); len(one) != 1 {
		t.Fatalf("limit: %+v", one)
	}
}

// TestPostgresReadOnly: a registry opened read-only reads, refuses writes with ErrReadOnly, and
// its Subscribe polls change_seq instead of listening.
func TestPostgresReadOnly(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := tempDatabase(t, dsn, "supavise_ro")
	rw, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if err := rw.CreateProject(ctx, &Project{Ref: "system", Name: "system", Status: StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	old := readOnlyPoll
	readOnlyPoll = 20 * time.Millisecond
	defer func() { readOnlyPoll = old }()
	ro, err := OpenReadOnly(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	if ps, err := ro.ListProjects(ctx); err != nil || len(ps) != 1 || ps[0].NodeID != FounderNodeID {
		t.Fatalf("read: %v %+v", err, ps)
	}
	if cl, err := ro.GetCluster(ctx); err != nil || cl.Leader != FounderNodeID {
		t.Fatalf("cluster: %v %+v", err, cl)
	}
	_, err = ro.CreateOrganization(ctx, "x", "X")
	wantErr(t, "a write through a read-only registry", err, ErrReadOnly)
	wantErr(t, "a cluster write through a read-only registry", ro.SetClusterName(ctx, "x"), ErrReadOnly)

	sub, err := ro.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.PutRoute(ctx, Route{Host: "x.example.test", Ref: "system"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for !(seen["projects"] && seen["routes"] && seen["nodes"] && seen["replicas"] && seen["project_secrets"]) {
		select {
		case ch, ok := <-sub:
			if !ok {
				t.Fatal("the change feed closed")
			}
			if ch.Op != "reload" || ch.Key != "" {
				t.Fatalf("change from a poll: %+v", ch)
			}
			seen[ch.Table] = true
		case <-timeout:
			t.Fatalf("poll delivered only %v", seen)
		}
	}
	cancel()
	for range sub { // the feed ends with the context
	}
}

// TestClusterMigrationsKeepAV011Registry applies the migrations a v0.1.1 node has (everything
// before 1300), fills the registry with the rows such a node holds, applies 1300 and 1301, and
// checks that nothing was lost and that every project is on the founder node. It then runs the
// statements the v0.1.1 binary executes, which know nothing of node_id, against the new schema:
// the binary one minor behind must keep working (invariant I6).
func TestClusterMigrationsKeepAV011Registry(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db := tempDatabase(t, dsn, "supavise_v011")
	pool, err := pgxpool.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool, "1300_cluster.sql"); err != nil {
		t.Fatal(err)
	}
	applied, err := AppliedMigrations(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, v011Migrations) {
		t.Fatalf("migrations before 1300:\n got %v\nwant %v (the set v0.1.1 ships)", applied, v011Migrations)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	count := func(table string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) from supavise.`+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec(`insert into supavise.organizations (slug, name) values ('acme', 'Acme')`)
	for i, ref := range []string{"system", refA, refB} {
		exec(`insert into supavise.projects (ref, org_id, seq, name, region, engine, class, status, versions, limits)
			values ($1, case when $1 = 'system' then null else (select id from supavise.organizations limit 1) end, $2, $1, 'us-east-1', 'postgres', 'micro', 'ACTIVE_HEALTHY', '{"postgres":"pg17"}', '{}')`, ref, i)
		exec(`insert into supavise.project_secrets (ref, name, ciphertext) values ($1, 'jwt_secret', $2)`, ref, []byte{1, 2, 3, byte(i)})
		exec(`insert into supavise.routes (host, ref, kind) values ($1, $2, 'api')`, ref+".api.example.test", ref)
		exec(`insert into supavise.backups (ref, status, location, size_bytes) values ($1, 'completed', 's3://b/'||$1, 4096)`, ref)
		exec(`insert into supavise.events (ref, kind) values ($1, 'created')`, ref)
	}
	exec(`insert into supavise.project_upgrades (tracking_id, ref) values ('11111111-1111-4111-8111-111111111111', $1)`, refA)
	before := map[string]int{}
	tables := []string{"organizations", "projects", "project_secrets", "routes", "backups", "events", "project_upgrades"}
	for _, tb := range tables {
		before[tb] = count(tb)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		if got := count(tb); got != before[tb] {
			t.Errorf("%s: %d rows after, %d before", tb, got, before[tb])
		}
	}
	var onFounder int
	if err := pool.QueryRow(ctx, `select count(*) from supavise.projects where node_id = 'n1'`).Scan(&onFounder); err != nil || onFounder != 3 {
		t.Fatalf("projects on the founder node: %d, %v", onFounder, err)
	}
	var secret []byte
	if err := pool.QueryRow(ctx, `select ciphertext from supavise.project_secrets where ref = $1 and name = 'jwt_secret'`, refB).Scan(&secret); err != nil || !reflect.DeepEqual(secret, []byte{1, 2, 3, 2}) {
		t.Fatalf("a sealed secret changed: %v %v", secret, err)
	}
	var nodes, clusters int
	_ = pool.QueryRow(ctx, `select count(*) from supavise.nodes where id = 'n1' and name = 'primary' and state = 'active'`).Scan(&nodes)
	_ = pool.QueryRow(ctx, `select count(*) from supavise.cluster where leader = 'n1' and epoch = 1 and change_seq = 0`).Scan(&clusters)
	if nodes != 1 || clusters != 1 {
		t.Fatalf("founder node rows %d, cluster rows %d", nodes, clusters)
	}
	if err := Migrate(ctx, pool); err != nil { // applying again changes nothing
		t.Fatal(err)
	}

	// The new binary reads the old rows.
	reg := NewPostgres(pool)
	ps, err := reg.ListProjects(ctx)
	if err != nil || len(ps) != 3 {
		t.Fatalf("list: %v %+v", err, ps)
	}
	for _, p := range ps {
		if p.NodeID != FounderNodeID || p.Versions["postgres"] != "pg17" {
			t.Errorf("project %s: %+v", p.Ref, p)
		}
	}

	// The v0.1.1 binary's writes: an insert that omits node_id, a status change, a secret, a route,
	// a delete. They run unchanged against the migrated schema.
	const refC = "cccccccccccccccccccc"
	exec(`insert into supavise.projects (ref, org_id, seq, name, region, engine, class, status, versions, limits,
			branch_id, parent_ref, branch_name, git_branch, persistent, with_data, expires_at, deletion_scheduled_at,
			notify_url, branch_state, branch_detail, clone_method, review_requested_at, branch_egress)
		values ($1, (select id from supavise.organizations limit 1), 3, 'c', 'us-east-1', 'postgres', 'micro', 'COMING_UP', '{}', '{}',
			null, null, null, null, false, false, null, null, null, null, null, null, null, null)`, refC)
	exec(`update supavise.projects set name = 'c2', region = 'us-east-1', class = 'small', status = 'ACTIVE_HEALTHY', versions = '{}', limits = '{}', updated_at = now() where ref = $1`, refC)
	exec(`insert into supavise.project_secrets (ref, name, ciphertext) values ($1, 'jwt_secret', $2)
		on conflict (ref, name) do update set ciphertext = excluded.ciphertext, created_at = now()`, refC, []byte{9})
	exec(`insert into supavise.routes (host, ref, kind) values ('c.example.test', $1, 'api') on conflict (host) do update set ref = excluded.ref, kind = excluded.kind`, refC)
	var home string
	if err := pool.QueryRow(ctx, `select node_id from supavise.projects where ref = $1`, refC).Scan(&home); err != nil || home != FounderNodeID {
		t.Fatalf("a project inserted by the old binary: node %q, %v", home, err)
	}
	var seq int64
	_ = pool.QueryRow(ctx, `select change_seq from supavise.cluster`).Scan(&seq)
	if seq < 4 {
		t.Fatalf("change_seq after four writes: %d", seq)
	}
	exec(`delete from supavise.projects where ref = $1`, refC)
	if n := count("routes"); n != before["routes"] {
		t.Fatalf("routes after the delete cascaded: %d, want %d", n, before["routes"])
	}
}

// v011Migrations is the registry schema v0.1.1 ships. A change to what precedes 1300 must be a
// deliberate edit here, because TestClusterMigrationsKeepAV011Registry stands for that release.
var v011Migrations = []string{
	"0001_init.sql", "0100_api.sql", "0101_api_login_failures.sql", "0600_claim_tokens.sql", "0610_removed_users.sql",
	"0700_branching.sql", "0701_branch_egress.sql", "0800_project_settings.sql", "0801_project_settings_pooler.sql",
	"0900_members.sql", "0901_claim_token_invitation.sql", "0902_content_updated_by.sql", "1000_sso.sql", "1001_sso_denied.sql",
	"1100_project_upgrades.sql", "1190_custom_domains.sql", "1250_compute_sizes.sql",
}

// `supavise upgrade` runs the new binary against a registry the old release left, before anything
// migrates it: the registry must read it, with every project on the founder node, and write the
// project rows it always wrote.
func TestPostgresReadsARegistryThatHasNotMigrated(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := tempDatabase(t, dsn, "supavise_unmigrated")
	pool, err := pgxpool.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool, "1300_cluster.sql"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into supavise.projects (ref, seq, name, status) values ('system', 0, 'system', 'ACTIVE_HEALTHY')`); err != nil {
		t.Fatal(err)
	}

	existing, err := OpenExisting(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	if ps, err := existing.ListProjects(ctx); err != nil || len(ps) != 1 || ps[0].NodeID != FounderNodeID {
		t.Fatalf("list: %v %+v", err, ps)
	}
	if err := existing.SetProjectStatus(ctx, "system", StatusRestarting); err != nil {
		t.Fatal(err)
	}
	p := &Project{Ref: refA, Name: "a"}
	if err := existing.CreateProject(ctx, p); err != nil || p.NodeID != FounderNodeID || p.Seq != 1 {
		t.Fatalf("create: %v %+v", err, p)
	}
	if err := existing.CreateProject(ctx, &Project{Ref: refB, Name: "b", NodeID: "n2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a project on a node that cannot exist yet: %v", err)
	}
	p.Name = "renamed"
	if err := existing.UpdateProject(ctx, p); err != nil || p.NodeID != FounderNodeID {
		t.Fatalf("update: %v %+v", err, p)
	}

	// A read-only registry reads it too, and refuses to subscribe until migration 1300 has run.
	ro, err := OpenReadOnly(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got, err := ro.GetProject(ctx, refA); err != nil || got.NodeID != FounderNodeID || got.Name != "renamed" {
		t.Fatalf("read-only get: %v %+v", err, got)
	}
	if _, err := ro.Subscribe(ctx); err == nil {
		t.Fatal("subscribed to a registry without a change counter")
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	sub, err := ro.Subscribe(ctx)
	if err != nil {
		t.Fatalf("subscribe after the migration: %v", err)
	}
	_ = sub
	if got, err := ro.GetProject(ctx, refA); err != nil || got.NodeID != FounderNodeID {
		t.Fatalf("read-only get after the migration: %v %+v", err, got)
	}
	if ro.legacy.Load() {
		t.Fatal("still reading the legacy columns after the migration")
	}
}
