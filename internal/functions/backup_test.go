package functions

import (
	"context"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/backup"
)

// Deployments made through the API store survive a backup and a restore: into another
// project (refused if it has functions) with their version and files and a new id, and in
// place with the same id.
func TestBackupStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	const ref, ref2 = "abcdefghijklmnopqrst", "tsrqponmlkjihgfedcba"
	st := api.NewMemoryStore()
	f := &api.Function{Ref: ref, Slug: "hello", Name: "hello", Status: "ACTIVE", VerifyJWT: true, EntrypointPath: "index.ts", ImportMapPath: "deno.json"}
	files := []api.FunctionFile{{Path: "index.ts", Content: []byte("v1")}, {Path: ".supavise-bundle.ezbr", Content: []byte{1, 2, 3}}}
	if err := st.UpsertFunction(ctx, f, files); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFunction(ctx, f, files); err != nil || f.Version != 2 {
		t.Fatalf("second deploy = %d, %v", f.Version, err)
	}
	if err := st.PutFunctionSecrets(ctx, ref, map[string][]byte{"API_KEY": []byte("sealed")}); err != nil {
		t.Fatal(err)
	}

	store, err := backup.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := backup.New(backup.Options{Store: store, Functions: BackupStore(st)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.BackupFiles(ctx, ref, backup.FilesOptions{})
	if err != nil || res.Functions == nil || res.Functions.Files != 3 {
		t.Fatalf("backup = %+v, %v", res, err)
	}

	// A target that holds a function is refused and left as it is: a restore writes
	// functions and secrets by name.
	other := &api.Function{Ref: ref2, Slug: "stale", Name: "stale", Status: "ACTIVE"}
	if err := st.UpsertFunction(ctx, other, []api.FunctionFile{{Path: "index.ts", Content: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreFiles(ctx, ref, ref2, backup.FilesRestoreOptions{Latest: true}); err == nil || !strings.Contains(err.Error(), "already has 1 Edge Functions") {
		t.Fatalf("restore into a project with a function = %v", err)
	}
	if got, err := st.ListFunctions(ctx, ref2); err != nil || len(got) != 1 || got[0].Slug != "stale" {
		t.Fatalf("the refused target was changed: %+v, %v", got, err)
	}
	if err := st.DeleteFunction(ctx, ref2, "stale"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreFiles(ctx, ref, ref2, backup.FilesRestoreOptions{Latest: true}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListFunctions(ctx, ref2)
	if err != nil || len(got) != 1 {
		t.Fatalf("functions of the target = %+v, %v", got, err)
	}
	g, err := st.GetFunction(ctx, ref2, "hello")
	if err != nil || g.Version != 2 || g.ID == f.ID || g.ID == "" || g.ImportMapPath != "deno.json" || !g.UpdatedAt.Equal(f.UpdatedAt) || !g.VerifyJWT {
		t.Fatalf("restored = %+v, %v; want version 2 and an id other than %s", g, err, f.ID)
	}
	fl, err := st.FunctionFiles(ctx, ref2, "hello")
	if err != nil || len(fl) != 2 {
		t.Fatalf("files = %+v, %v", fl, err)
	}
	if sec, _ := st.ListFunctionSecrets(ctx, ref2); len(sec) != 1 || string(sec[0].Sealed) != "sealed" {
		t.Fatalf("secrets = %+v", sec)
	}

	// In place: a function deployed after the backup is removed, the backed up one is back.
	if err := st.DeleteFunction(ctx, ref, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFunction(ctx, &api.Function{Ref: ref, Slug: "newer", Name: "newer", Status: "ACTIVE"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreFiles(ctx, ref, ref, backup.FilesRestoreOptions{SnapshotID: res.Functions.ID}); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.ListFunctions(ctx, ref)
	if len(cur) != 1 || cur[0].Slug != "hello" || cur[0].Version != 2 || cur[0].ID != f.ID {
		t.Fatalf("in place = %+v", cur)
	}
}
