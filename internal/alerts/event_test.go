package alerts

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

// The kinds of the replica and failover work: the failover ones are announcements, the rest are
// conditions, and the checker raises none of them.
func TestClusterKinds(t *testing.T) {
	announcements := []string{KindFailoverStarted, KindFailoverCompleted, KindFailoverFailed}
	conditions := []string{
		KindReplicaUnhealthy, KindReplicaLag, KindReplicaCapacity, KindNodeUnreachable,
		KindNodeVersionSkew, KindFailoverAutoOff, KindFenced, KindInfraBehind, KindHostNotConverged, KindStandbyBehind,
	}
	seen := map[string]bool{}
	for _, k := range append(append([]string{}, announcements...), conditions...) {
		if k == "" || seen[k] {
			t.Errorf("kind %q is empty or listed twice", k)
		}
		seen[k] = true
		if owned(k) {
			t.Errorf("%s: the checker would resolve a condition it never raises", k)
		}
	}
	for _, k := range announcements {
		if !oneShot(k) {
			t.Errorf("%s should be an announcement", k)
		}
	}
	for _, k := range conditions {
		if oneShot(k) {
			t.Errorf("%s should be a condition", k)
		}
	}

	s := newSink(t)
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := n.Notify(ctx, Event{Kind: KindFailoverFailed, Severity: SeverityCritical, Title: "Failover failed"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := n.Notify(ctx, Event{Kind: KindReplicaUnhealthy, Ref: "aaaaaaaaaaaaaaaaaaaa", Title: "Replica is not healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 4 {
		t.Errorf("%d notifications: three failed failovers and one standing replica problem make 4", s.count())
	}
	if err := n.Notify(ctx, Event{Kind: KindReplicaUnhealthy, Ref: "aaaaaaaaaaaaaaaaaaaa", Title: "Replica is not healthy", Resolved: true}); err != nil || s.count() != 5 {
		t.Errorf("recovery: %d, %v", s.count(), err)
	}
}

// A kind that nothing raises is a promise the README makes and the node does not keep (replica_needs_rebuild
// and storage_not_s3 were such). Every kind the package defines is named in some other non-test file of the
// repository: the checker, or the code that owns the event.
func TestEveryKindIsRaisedSomewhere(t *testing.T) {
	fset := token.NewFileSet()
	defs, err := parser.ParseFile(fset, "event.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	ast.Inspect(defs, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok {
			for _, id := range vs.Names {
				if strings.HasPrefix(id.Name, "Kind") {
					kinds[id.Name] = false
				}
			}
		}
		return true
	})
	if len(kinds) < 20 {
		t.Fatalf("found only %d kinds in event.go", len(kinds))
	}
	root := "../.."
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "testdata" || (name != "." && strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "event.go" && strings.HasSuffix(filepath.Dir(path), "alerts") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil // a file that does not parse is the build's to report
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if _, defined := kinds[id.Name]; defined {
					kinds[id.Name] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, used := range kinds {
		if !used {
			t.Errorf("%s is defined and nothing raises it: implement it or remove it", k)
		}
	}
}
