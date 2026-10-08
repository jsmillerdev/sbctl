package fenced

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
)

func paths(t *testing.T) config.Paths { return config.Paths{Root: t.TempDir()} }

func TestNodeRecordRoundTrip(t *testing.T) {
	p := paths(t)
	if r, err := Node(p); r != nil || err != nil {
		t.Fatalf("empty: %+v, %v", r, err)
	}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	want := Record{Epoch: 4, Leader: "n2", Reason: "peer n2 leads at epoch 4", At: at}
	if err := WriteNode(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := Node(p)
	if err != nil || got == nil || *got != want {
		t.Fatalf("read: %+v, %v", got, err)
	}
	fi, err := os.Stat(NodePath(p))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v, %v", fi, err)
	}
	if err := ClearNode(p); err != nil {
		t.Fatal(err)
	}
	if err := ClearNode(p); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
	if r, _ := Node(p); r != nil {
		t.Fatalf("after clear: %+v", r)
	}
}

func TestARecordIsNotLoweredByAStaleRequest(t *testing.T) {
	p := paths(t)
	if err := WriteNode(p, Record{Epoch: 5, Leader: "n2", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteNode(p, Record{Epoch: 3, Leader: "n3", Reason: "stale"}); err != nil {
		t.Fatal(err)
	}
	r, _ := Node(p)
	if r.Epoch != 5 || r.Leader != "n2" {
		t.Fatalf("record: %+v", r)
	}
	if err := WriteNode(p, Record{Epoch: 6, Leader: "n3", Reason: "newer"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := Node(p); r.Epoch != 6 || r.Leader != "n3" {
		t.Fatalf("record: %+v", r)
	}
}

func TestBlocks(t *testing.T) {
	p := paths(t)
	const ref = "abcdefghijklmnopqrst"
	if _, ok := Blocks(p, ref); ok {
		t.Fatal("blocked with no record")
	}
	if err := WriteProject(p, Record{Epoch: 2, Ref: ref, Reason: "failed over"}); err != nil {
		t.Fatal(err)
	}
	if r, ok := Blocks(p, ref); !ok || r.Ref != ref {
		t.Fatalf("project record: %+v, %v", r, ok)
	}
	if _, ok := Blocks(p, "bbbbbbbbbbbbbbbbbbbb"); ok {
		t.Fatal("another project is blocked by a project record")
	}
	if err := ClearProject(p, ref); err != nil {
		t.Fatal(err)
	}
	if _, ok := Blocks(p, ref); ok {
		t.Fatal("blocked after the record was cleared")
	}
	if err := WriteNode(p, Record{Epoch: 3, Leader: "n2", Reason: "replaced"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{ref, "bbbbbbbbbbbbbbbbbbbb", "system"} {
		if _, ok := Blocks(p, r); !ok {
			t.Errorf("%s is not blocked on a fenced node", r)
		}
	}
}

func TestABrokenRecordBlocks(t *testing.T) {
	p := paths(t)
	if err := os.WriteFile(NodePath(p), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Node(p); err == nil {
		t.Fatal("a broken record was read")
	}
	if r, ok := Blocks(p, "system"); !ok || r.Reason == "" {
		t.Fatalf("a broken record must fail closed: %+v, %v", r, ok)
	}
}

func TestProjectRecordNeedsARef(t *testing.T) {
	if err := WriteProject(paths(t), Record{Epoch: 1}); err == nil {
		t.Fatal("a project record without a ref was written")
	}
}

func TestOnlyAProjectRefNamesAProjectRecord(t *testing.T) {
	p := paths(t)
	for _, ref := range []string{"../x", "a/b", "ABCDEFGHIJKLMNOPQRST", "short", "abcdefghijklmnopqrstu"} {
		if ValidRef(ref) {
			t.Errorf("%q is a ref", ref)
		}
		if err := WriteProject(p, Record{Epoch: 1, Ref: ref}); err == nil {
			t.Errorf("a record was written for %q", ref)
		}
		if err := ClearProject(p, ref); err == nil {
			t.Errorf("a record was cleared for %q", ref)
		}
	}
	if !ValidRef("system") || !ValidRef("abcdefghijklmnopqrst") {
		t.Fatal("a real ref was refused")
	}
}

func TestWriteCreatesTheProjectDirectory(t *testing.T) {
	p := paths(t)
	if err := WriteProject(p, Record{Epoch: 1, Ref: "system", Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Root, "projects", "system", "fenced.json")); err != nil {
		t.Fatal(err)
	}
}
