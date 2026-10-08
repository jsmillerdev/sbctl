package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// pgStoreWithProject opens the registry database CI provides (SUPAVISE_TEST_DATABASE_URL), applies the
// migrations and creates a project of its own, which the function tables reference. The project
// (and with it every row of the test) is removed when the test ends.
func pgStoreWithProject(t *testing.T) (*PGStore, string) {
	t.Helper()
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := registry.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	if err := registry.Migrate(ctx, r.Pool()); err != nil {
		t.Fatal(err)
	}
	org, err := r.CreateOrganization(ctx, fmt.Sprintf("pgstore-%d", time.Now().UnixNano()), "PG store")
	if err != nil {
		t.Fatal(err)
	}
	letters := make([]byte, 20) // a ref is 20 lowercase letters
	if _, err := rand.Read(letters); err != nil {
		t.Fatal(err)
	}
	for i := range letters {
		letters[i] = 'a' + letters[i]%26
	}
	ref := string(letters)
	if err := r.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: org.ID, Name: "pgstore"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.DeleteProject(context.Background(), ref) })
	return NewPGStore(r.Pool()), ref
}

// TestPGStoreRestoreFunction checks RestoreFunction, which backups use to put a function's
// deployment back, against a real Postgres: it stores the row exactly as given (identifier, version
// and timestamps included), replaces what the slug had, files too, and does nothing by halves.
func TestPGStoreRestoreFunction(t *testing.T) {
	s, ref := pgStoreWithProject(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fn := Function{Ref: ref, Slug: "restored", ID: "c0ffee00-0000-4000-8000-000000000001", Name: "Restored", Version: 7, Status: "ACTIVE",
		VerifyJWT: false, EntrypointPath: "index.ts", ImportMapPath: "deno.json", CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
	files := []FunctionFile{{Path: "index.ts", Content: []byte("r1")}, {Path: "deno.json", Content: []byte("{}")}, {Path: "bin.dat", Content: []byte{0, 1, 2, 0xff}}}

	// A slug that does not exist yet is created as given.
	if err := s.RestoreFunction(ctx, fn, files); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := s.GetFunction(ctx, ref, "restored")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != fn.ID || got.Name != "Restored" || got.Version != 7 || got.Status != "ACTIVE" || got.VerifyJWT ||
		got.EntrypointPath != "index.ts" || got.ImportMapPath != "deno.json" ||
		!got.CreatedAt.Equal(fn.CreatedAt) || !got.UpdatedAt.Equal(fn.UpdatedAt) {
		t.Fatalf("restored function = %+v, want %+v", got, fn)
	}
	stored, err := s.FunctionFiles(ctx, ref, "restored")
	if err != nil || len(stored) != 3 {
		t.Fatalf("restored files = %+v, %v", stored, err)
	}
	for _, want := range files {
		found := false
		for _, f := range stored {
			if f.Path == want.Path {
				found = true
				if !bytes.Equal(f.Content, want.Content) {
					t.Errorf("%s = %x, want %x", f.Path, f.Content, want.Content)
				}
			}
		}
		if !found {
			t.Errorf("file %s was not restored", want.Path)
		}
	}
	if list, err := s.ListFunctions(ctx, ref); err != nil || len(list) != 1 || list[0].Slug != "restored" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// A deployment that was changed after the backup is replaced, not merged: a new id and version, the
	// import map gone, and the files of the backup only. The optional paths come back empty, not "".
	fn.ID, fn.Version, fn.ImportMapPath, fn.VerifyJWT, fn.Name = "c0ffee00-0000-4000-8000-000000000002", 8, "", true, "Again"
	if err := s.RestoreFunction(ctx, fn, []FunctionFile{{Path: "index.ts", Content: []byte("r2")}}); err != nil {
		t.Fatalf("restore over an existing function: %v", err)
	}
	if got, err = s.GetFunction(ctx, ref, "restored"); err != nil || got.ID != fn.ID || got.Version != 8 || got.ImportMapPath != "" || !got.VerifyJWT || got.Name != "Again" {
		t.Fatalf("replaced function = %+v, %v", got, err)
	}
	if stored, _ = s.FunctionFiles(ctx, ref, "restored"); len(stored) != 1 || string(stored[0].Content) != "r2" {
		t.Fatalf("restore must replace the files: %+v", stored)
	}

	// A restore that fails half way changes nothing: two files with one path break the primary key
	// after the function row and the first file were written.
	broken := fn
	broken.Version, broken.Name = 99, "Broken"
	err = s.RestoreFunction(ctx, broken, []FunctionFile{{Path: "a.ts", Content: []byte("x")}, {Path: "a.ts", Content: []byte("y")}})
	if err == nil {
		t.Fatal("a restore with a duplicate path succeeded")
	}
	if got, err = s.GetFunction(ctx, ref, "restored"); err != nil || got.Version != 8 || got.Name != "Again" {
		t.Fatalf("the function changed although the restore failed: %+v, %v", got, err)
	}
	if stored, _ = s.FunctionFiles(ctx, ref, "restored"); len(stored) != 1 || string(stored[0].Content) != "r2" {
		t.Fatalf("the files changed although the restore failed: %+v", stored)
	}

	// An ordinary deployment of the slug continues from the restored version.
	up := &Function{Ref: ref, Slug: "restored", Name: "Again", Status: "ACTIVE", VerifyJWT: true}
	if err := s.UpsertFunction(ctx, up, nil); err != nil || up.Version != 9 {
		t.Fatalf("the next deployment after a restore = %+v, %v (want version 9)", up, err)
	}

	// No files: the slug ends up with none.
	if err := s.RestoreFunction(ctx, fn, nil); err != nil {
		t.Fatal(err)
	}
	if stored, _ = s.FunctionFiles(ctx, ref, "restored"); len(stored) != 0 {
		t.Fatalf("a restore without files leaves none: %+v", stored)
	}

	// A project that does not exist is refused, and so is a malformed identifier.
	other := fn
	other.Ref = "zzzzzzzzzzzzzzzzzzzz"
	if err := s.RestoreFunction(ctx, other, nil); err == nil {
		t.Error("a function of an unknown project was restored")
	}
	bad := fn
	bad.Slug, bad.ID = "badid", "not-a-uuid"
	if err := s.RestoreFunction(ctx, bad, nil); err == nil {
		t.Error("a function with a malformed id was restored")
	}
	if _, err := s.GetFunction(ctx, ref, "badid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refused restore left a function behind: %v", err)
	}

	// Deleting the function takes its files with it, and the restore can be repeated.
	if err := s.DeleteFunction(ctx, ref, "restored"); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFunction(ctx, fn, files); err != nil {
		t.Fatalf("restore after a delete: %v", err)
	}
}
