package functions

import (
	"context"
	"testing"

	"github.com/jsmillerdev/supavise/internal/api"
	"github.com/jsmillerdev/supavise/internal/backup"
)

// Deployments made through the API store survive a backup and a restore into another
// project with their version, id and files, and the restore replaces what the target holds.
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

	// The target holds a function the snapshot does not know: restoring into it replaces.
	other := &api.Function{Ref: ref2, Slug: "stale", Name: "stale", Status: "ACTIVE"}
	if err := st.UpsertFunction(ctx, other, []api.FunctionFile{{Path: "index.ts", Content: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreFiles(ctx, ref, ref2, backup.FilesRestoreOptions{Latest: true}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListFunctions(ctx, ref2)
	if err != nil || len(got) != 2 {
		t.Fatalf("functions of the target = %+v, %v (a restore into another project adds)", got, err)
	}
	g, err := st.GetFunction(ctx, ref2, "hello")
	if err != nil || g.Version != 2 || g.ID != f.ID || g.ImportMapPath != "deno.json" || !g.UpdatedAt.Equal(f.UpdatedAt) || !g.VerifyJWT {
		t.Fatalf("restored = %+v, %v; want version 2, id %s", g, err, f.ID)
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
	if len(cur) != 1 || cur[0].Slug != "hello" || cur[0].Version != 2 {
		t.Fatalf("in place = %+v", cur)
	}
}
