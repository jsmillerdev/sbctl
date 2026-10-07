package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFilesBackupOverS3Fake runs the Storage and function backup and restore over the S3
// backend against an in-process fake S3 server.
func TestFilesBackupOverS3Fake(t *testing.T) { runFilesOverS3(t, fakeS3(t)) }

// TestFilesBackupOverS3 is the same over a real S3-compatible service: CI only, skipped
// without SUPAVISE_TEST_S3_* (see store_s3_test.go).
func TestFilesBackupOverS3(t *testing.T) { runFilesOverS3(t, s3FromEnv(t)) }

func runFilesOverS3(t *testing.T, o S3Options) {
	ctx := context.Background()
	st, err := NewS3Store(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	cleanS3(t, st)
	e := newTestEnv(t)
	e.svc.opt.Store, e.store = st, nil
	fns := newFakeFunctions()
	e.svc.opt.Functions = fns
	old := e.now.Add(-time.Hour)

	// A multipart-sized object (the part size is 5 MiB), a small one with a content type, and a function.
	big := randBytes(t, 12<<20+99)
	e.putObject(t, testRef, "photos/big.bin/v1", big, old)
	small := e.putObject(t, testRef, "photos/small.txt/v1", []byte("hello"), old)
	attrsOK := setContentType(t, small, "text/plain")
	fns.deploy(testRef, fnRec("hello", 2, old), FunctionFile{Path: "index.ts", Content: []byte("export default 1")},
		FunctionFile{Path: ".supavise-bundle.ezbr", Content: randBytes(t, 6<<20)})
	fns.secrets[testRef] = map[string][]byte{"API_KEY": []byte("sealed")}
	want := readTree(t, e.objectsDir(testRef))

	res, err := e.svc.BackupFiles(ctx, testRef, FilesOptions{Reason: ReasonScheduled})
	if err != nil || res.Storage == nil || res.Functions == nil || res.Storage.NewFiles != 2 || res.Functions.NewFiles != 3 {
		t.Fatalf("backup = %+v, %v", res, err)
	}
	// The second run finds everything in the bucket.
	e.now = e.now.Add(time.Hour)
	res2, err := e.svc.BackupFiles(ctx, testRef, FilesOptions{Reason: ReasonScheduled})
	if err != nil || res2.Storage.NewFiles != 0 || res2.Functions.NewFiles != 0 {
		t.Fatalf("second backup = %+v, %v", res2, err)
	}

	// Lose the project's files, then restore in place.
	if err := os.RemoveAll(e.objectsDir(testRef)); err != nil {
		t.Fatal(err)
	}
	delete(fns.recs, testRef)
	delete(fns.files, testRef)
	delete(fns.secrets, testRef)
	e.now = e.now.Add(time.Hour)
	if _, err := e.svc.RestoreFiles(ctx, testRef, testRef, FilesRestoreOptions{Latest: true}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, readTree(t, e.objectsDir(testRef)), want)
	if cur, _ := fns.ListFunctions(ctx, testRef); len(cur) != 1 || cur[0].Version != 2 {
		t.Fatalf("functions after the in-place restore = %+v", cur)
	}

	// And as a new project.
	if _, err := e.svc.RestoreFiles(ctx, testRef, testRef2, FilesRestoreOptions{Latest: true}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, readTree(t, e.objectsDir(testRef2)), want)
	if cur, _ := fns.ListFunctions(ctx, testRef2); len(cur) != 1 {
		t.Fatalf("functions of the new project = %+v", cur)
	}
	if f, _ := fns.FunctionFiles(ctx, testRef2, "hello"); len(f) != 2 {
		t.Fatalf("function files of the new project = %d", len(f))
	}
	if attrsOK {
		got, err := readStorageAttrs(filepath.Join(e.objectsDir(testRef2), "photos", "small.txt", "v1"))
		if err != nil || string(got[contentTypeAttr()]) != "text/plain" {
			t.Errorf("content type = %q, %v", got[contentTypeAttr()], err)
		}
	}

	// The key escrow over the same backend: seal, find, open.
	cheapKDF(t)
	pass := []byte("a long enough passphrase")
	if _, err := PutKeyEscrow(ctx, st, pass, EscrowContents{MasterKey: testKey, ConfigTOML: "x = 1\n"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := GetKeyEscrow(ctx, st, pass, "")
	if err != nil || got.MasterKey != testKey {
		t.Fatalf("escrow over S3 = %+v, %v", got, err)
	}
	if all, err := ListKeyEscrows(ctx, st); err != nil || len(all) != 1 || all[0].KeyID != KeyID(testKey) {
		t.Fatalf("escrow list over S3 = %+v, %v", all, err)
	}
}

// A file whose mtime is within the racy window of the previous run is read again even when
// its size and mtime did not change: a write in the same clock tick could have gone unseen.
func TestBackupStorageRehashesRacyFiles(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	p := e.putObject(t, testRef, "b/o/v1", []byte("aaaa"), e.now) // modified "now"
	if _, err := e.svc.backupStorage(ctx, testRef, ReasonManual); err != nil {
		t.Fatal(err)
	}
	// Same size, same mtime, other content.
	if err := os.WriteFile(p, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, e.now, e.now); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Minute)
	s, err := e.svc.backupStorage(ctx, testRef, ReasonManual)
	if err != nil || s.NewFiles != 1 {
		t.Fatalf("second backup = %+v, %v (the changed file was trusted)", s, err)
	}
	res, err := e.svc.RestoreFiles(ctx, testRef, testRef2, FilesRestoreOptions{Latest: true})
	if err != nil || res.Storage == nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.objectsDir(testRef2), "b", "o", "v1")); string(b) != "bbbb" {
		t.Fatalf("restored %q", b)
	}
}
