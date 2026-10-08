package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The in-memory registry hands out and keeps its own copy of a node's AWS identity: a caller that
// edits what it passed in or what it got back does not edit the registry.
func TestMemoryNodeProviderIsNotShared(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	aws := &NodeAWS{InstanceID: "i-0abc", Zone: "eu-west-1a"}
	n := &Node{Name: "second", Provider: NodeProvider{AWS: aws}}
	if err := m.CreateNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	aws.InstanceID = "i-changed-by-the-caller"
	n.Provider.AWS.Zone = "also changed"

	got, err := m.GetNode(ctx, n.ID)
	if err != nil || got.Provider.AWS.InstanceID != "i-0abc" || got.Provider.AWS.Zone != "eu-west-1a" {
		t.Fatalf("create stored the caller's pointer: %+v, %v", got.Provider.AWS, err)
	}
	got.Provider.AWS.InstanceID = "i-changed-after-get"
	byName, _ := m.GetNodeByName(ctx, "second")
	list, _ := m.ListNodes(ctx)
	byName.Provider.AWS.Zone = "changed after get by name"
	list[1].Provider.AWS.Region = "changed after list"
	again, _ := m.GetNode(ctx, n.ID)
	if *again.Provider.AWS != (NodeAWS{InstanceID: "i-0abc", Zone: "eu-west-1a"}) {
		t.Fatalf("a reader changed the registry's copy: %+v", again.Provider.AWS)
	}

	upd := &Node{ID: n.ID, Name: "second", Provider: NodeProvider{AWS: &NodeAWS{InstanceID: "i-1", Zone: "z"}}}
	if err := m.UpdateNode(ctx, upd); err != nil {
		t.Fatal(err)
	}
	upd.Provider.AWS.InstanceID = "i-changed-after-update"
	if got, _ := m.GetNode(ctx, n.ID); got.Provider.AWS.InstanceID != "i-1" {
		t.Fatalf("update kept the caller's pointer: %+v", got.Provider.AWS)
	}
}

// A project move and a leader change hold the project row and the cluster row in the same order
// as every other writer, so moves that run together, and next to ordinary project updates, finish
// instead of deadlocking.
func TestPostgresMovesDoNotDeadlock(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, tempDatabase(t, dsn, "supavise_moves"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.CreateNode(ctx, &Node{Name: "second", State: NodeActive}); err != nil {
		t.Fatal(err)
	}
	var refs []string
	for i := range 8 {
		ref := strings.Repeat(string(rune('a'+i)), 20)
		refs = append(refs, ref)
		if err := r.CreateProject(ctx, &Project{Ref: ref, Name: ref[:3]}); err != nil {
			t.Fatal(err)
		}
	}
	for round := range 5 {
		to, epoch := "n2", int64(1)
		if round%2 == 1 {
			to = "n1"
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2*len(refs))
		for _, ref := range refs {
			wg.Add(2)
			go func() {
				defer wg.Done()
				errs <- r.SetProjectNode(ctx, ref, to, epoch)
			}()
			go func() { // an ordinary writer: the project row, then the cluster row
				defer wg.Done()
				p, err := r.GetProject(ctx, ref)
				if err == nil {
					p.Name = fmt.Sprintf("%s-%d", ref[:3], round)
					err = r.UpdateProject(ctx, p)
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		for _, ref := range refs {
			if p, _ := r.GetProject(ctx, ref); p.NodeID != to {
				t.Fatalf("round %d: %s is on %s, want %s", round, ref, p.NodeID, to)
			}
		}
	}

	// A move under an epoch that the leader change has passed is refused, whichever commits first.
	if err := r.SetLeader(ctx, "n2", 2); err != nil {
		t.Fatal(err)
	}
	if err := r.SetProjectNode(ctx, refs[0], "n1", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("a move under the old epoch: %v", err)
	}
	if err := r.SetProjectNode(ctx, refs[0], "n2", 2); err != nil {
		t.Fatalf("a move under the new epoch: %v", err)
	}
}

// A registry handle opened before migration 1300 notices that the migration ran: the next read
// reports each project's real node, not the founder.
func TestPostgresLegacyHandleSeesTheMigration(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	old := legacyReprobe
	legacyReprobe = 0
	defer func() { legacyReprobe = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := tempDatabase(t, dsn, "supavise_reprobe")
	r, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.CreateNode(ctx, &Node{Name: "second", State: NodeActive}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateProject(ctx, &Project{Ref: refA, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetProjectNode(ctx, refA, "n2", 1); err != nil {
		t.Fatal(err)
	}
	// A handle that believes the registry is unmigrated, as one opened before the upgrade does.
	stale, err := OpenExisting(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	stale.legacy.Store(true)
	if got, err := stale.GetProject(ctx, refA); err != nil || got.NodeID != "n2" {
		t.Fatalf("the stale handle reports node %q (%v), want n2", got.NodeID, err)
	}
	if stale.legacy.Load() {
		t.Fatal("the handle still reads the legacy columns")
	}
}

// SetProjectNode reads the target node's state under a share lock: a `node rm` that is committing
// holds the row, the move waits for it and then sees the node left, instead of reading the old
// state and putting the project on a node that no longer exists.
func TestPostgresMoveWaitsForANodeThatIsLeaving(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Open(ctx, tempDatabase(t, dsn, "supavise_leaving"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.CreateNode(ctx, &Node{Name: "second", State: NodeActive}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateProject(ctx, &Project{Ref: refA, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update supavise.nodes set state = 'left' where id = 'n2'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.SetProjectNode(ctx, refA, "n2", 1) }()
	select {
	case err := <-done:
		t.Fatalf("the move did not wait for the node row (it returned %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("a move to a node that left: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the move never finished")
	}
	if p, _ := r.GetProject(ctx, refA); p.NodeID != "n1" {
		t.Fatalf("the project moved to %s", p.NodeID)
	}
}
