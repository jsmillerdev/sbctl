package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// objectsDir is where the test node's Storage keeps ref's objects.
func (e *testEnv) objectsDir(ref string) string { return e.cfg.Paths().StorageObjects(ref) }

// putObject writes an object the way Storage lays it out: <bucket>/<name>/<version>.
func (e *testEnv) putObject(t *testing.T, ref, rel string, content []byte, mtime time.Time) string {
	t.Helper()
	p := writeFile(t, filepath.Join(e.objectsDir(ref), filepath.FromSlash(rel)), content)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func contentTypeAttr() string {
	if runtime.GOOS == "darwin" {
		return "com.apple.metadata.supabase.content-type"
	}
	return "user.supabase.content-type"
}

// setContentType sets the attribute Storage keeps a file's type in, and reports whether the
// test file system supports extended attributes.
func setContentType(t *testing.T, p, typ string) bool {
	t.Helper()
	ok, err := writeStorageAttrs(p, map[string][]byte{contentTypeAttr(): []byte(typ)})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// tree maps relative paths to contents.
func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, got, want map[string][]byte) {
	t.Helper()
	var gk, wk []string
	for k := range got {
		gk = append(gk, k)
	}
	for k := range want {
		wk = append(wk, k)
	}
	sort.Strings(gk)
	sort.Strings(wk)
	if strings.Join(gk, "\n") != strings.Join(wk, "\n") {
		t.Fatalf("files = %v, want %v", gk, wk)
	}
	for k, v := range want {
		if !bytes.Equal(got[k], v) {
			t.Errorf("%s differs (%d bytes, want %d)", k, len(got[k]), len(v))
		}
	}
}

func TestBackupStorageIsIncrementalAndRestoreMatchesTheSource(t *testing.T) {
	e := newTestEnv(t)
	e.addProject(t, testRef)
	ctx := context.Background()
	old := e.now.Add(-48 * time.Hour)

	big := randBytes(t, 3<<20+17) // above smallFile: hashed in one pass and streamed in a second
	files := map[string][]byte{
		"stub/avatars/me.png/v1":    randBytes(t, 2048),
		"stub/avatars/you.png/v1":   []byte("small"),
		"stub/docs/a/b/c/report/v7": big,
		"stub/docs/empty.txt/v1":    {},
		"stub/docs/dup.txt/v1":      []byte("small"), // same content as you.png: one blob
	}
	// Only the tenant's own directory is backed up: the layout is <objects>/stub/<ref>/<bucket>/....
	tree := map[string][]byte{}
	attrsOK := true
	for rel, b := range files {
		rel = strings.TrimPrefix(rel, "stub/")
		p := e.putObject(t, testRef, rel, b, old)
		tree[rel] = b
		if strings.HasPrefix(rel, "avatars/me.png") {
			attrsOK = setContentType(t, p, "image/png")
		}
	}
	// Another project's object is not touched.
	e.putObject(t, testRef2, "bucket/other/v1", []byte("not mine"), old)

	s1, err := e.svc.backupStorage(ctx, testRef, ReasonScheduled)
	if err != nil || s1 == nil {
		t.Fatalf("backup 1 = %v, %v", s1, err)
	}
	if s1.Files != len(tree) || s1.NewFiles != 4 { // five files, one duplicate content
		t.Fatalf("snapshot 1: files %d, new %d, want %d and 4", s1.Files, s1.NewFiles, len(tree))
	}
	var total int64
	for _, b := range tree {
		total += int64(len(b))
	}
	if s1.Bytes != total {
		t.Errorf("snapshot 1 bytes = %d, want %d", s1.Bytes, total)
	}

	// Unchanged files are not read or copied again.
	e.now = e.now.Add(time.Hour)
	s2, err := e.svc.backupStorage(ctx, testRef, ReasonScheduled)
	if err != nil || s2.Files != len(tree) || s2.NewFiles != 0 || s2.NewBytes != 0 {
		t.Fatalf("snapshot 2 = %+v, %v", s2, err)
	}

	// A changed file, a new file and a deleted file.
	changed := randBytes(t, 4096)
	e.now = e.now.Add(time.Hour)
	e.putObject(t, testRef, "avatars/me.png/v1", changed, e.now)
	tree["avatars/me.png/v1"] = changed
	e.putObject(t, testRef, "avatars/new.png/v1", []byte("brand new"), old)
	tree["avatars/new.png/v1"] = []byte("brand new")
	if err := os.Remove(filepath.Join(e.objectsDir(testRef), "docs", "empty.txt", "v1")); err != nil {
		t.Fatal(err)
	}
	delete(tree, "docs/empty.txt/v1")
	s3, err := e.svc.backupStorage(ctx, testRef, ReasonScheduled)
	if err != nil || s3.Files != len(tree) || s3.NewFiles != 2 {
		t.Fatalf("snapshot 3 = %+v, %v", s3, err)
	}

	all, err := e.svc.ListFilesSnapshots(ctx, testRef, KindStorage)
	if err != nil || len(all) != 3 {
		t.Fatalf("snapshots = %v, %v", all, err)
	}

	// Restoring into another project reproduces the newest snapshot: deletions included.
	res, err := e.svc.RestoreFiles(ctx, testRef, testRef3, FilesRestoreOptions{Latest: true})
	if err != nil || res.Storage == nil || res.Storage.ID != s3.ID {
		t.Fatalf("restore = %+v, %v", res, err)
	}
	sameTree(t, readTree(t, e.objectsDir(testRef3)), tree)
	p := filepath.Join(e.objectsDir(testRef3), "avatars", "new.png", "v1")
	if fi, err := os.Stat(p); err != nil || !fi.ModTime().Equal(old.Truncate(time.Nanosecond)) && fi.ModTime().Sub(old).Abs() > time.Second || fi.Mode().Perm() != 0o600 {
		t.Errorf("restored file: %v, %v", fi, err)
	}

	// At the time of snapshot 1 the deleted file is still there and the changed one is old.
	res, err = e.svc.RestoreFiles(ctx, testRef, testRef3, FilesRestoreOptions{At: s1.StopTime, SnapshotID: s1.ID})
	if err == nil || !strings.Contains(err.Error(), "already holds objects") {
		t.Fatalf("restoring over a project that has objects must be refused: %v", err)
	}
	// In place: the objects directory is replaced and the old one is kept.
	res, err = e.svc.RestoreFiles(ctx, testRef, testRef, FilesRestoreOptions{At: s1.StopTime})
	if err != nil || res.Aside == "" {
		t.Fatalf("in place = %+v, %v", res, err)
	}
	want1 := map[string][]byte{}
	for rel, b := range files {
		want1[strings.TrimPrefix(rel, "stub/")] = b
	}
	sameTree(t, readTree(t, e.objectsDir(testRef)), want1)
	if _, err := os.Stat(filepath.Join(res.Aside, "avatars", "new.png", "v1")); err != nil {
		t.Errorf("the replaced directory was not kept: %v", err)
	}

	if attrsOK {
		got, err := readStorageAttrs(filepath.Join(e.objectsDir(testRef), "avatars", "me.png", "v1"))
		if err != nil || string(got[contentTypeAttr()]) != "image/png" {
			t.Errorf("content type after restore = %q, %v", got[contentTypeAttr()], err)
		}
	}
}

func TestBackupStorageSkipsWhatItCannotReadSafely(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	old := e.now.Add(-time.Hour)
	// No directory and no earlier snapshot: nothing to do, nothing written.
	if s, err := e.svc.backupStorage(ctx, testRef, ReasonManual); s != nil || err != nil {
		t.Fatalf("no directory = %v, %v", s, err)
	}
	if ids, _ := e.store.ListDirs(ctx, testRef+"/"); len(ids) != 0 {
		t.Fatalf("a project without objects left %v in the backend", ids)
	}

	e.putObject(t, testRef, "b/keep/v1", []byte("keep"), old)
	outside := filepath.Join(e.root, "outside.txt")
	writeFile(t, outside, []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(e.objectsDir(testRef), "b", "link")); err != nil {
		t.Skip("no symlinks here")
	}
	s, err := e.svc.backupStorage(ctx, testRef, ReasonManual)
	if err != nil || s.Files != 1 {
		t.Fatalf("snapshot = %+v, %v", s, err)
	}

	// All objects deleted: a snapshot says so.
	if err := os.RemoveAll(e.objectsDir(testRef)); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Minute)
	s, err = e.svc.backupStorage(ctx, testRef, ReasonManual)
	if err != nil || s == nil || s.Files != 0 {
		t.Fatalf("empty snapshot = %+v, %v", s, err)
	}
}

func TestStorageBackendS3IsNotCopied(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.Fleet.StorageBackend = "s3"
	e.putObject(t, testRef, "b/o/v1", []byte("x"), e.now.Add(-time.Hour))
	res, err := e.svc.BackupFiles(context.Background(), testRef, FilesOptions{})
	if err != nil || res.Storage != nil || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "S3") {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestBlobUploadFailsWhenTheFileChangesUnderIt(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	bw, err := e.svc.newBlobWriter(testRef, KindStorage)
	if err != nil {
		t.Fatal(err)
	}
	defer bw.close()
	want := strings.Repeat("a", 5000)
	hash := fmt.Sprintf("%x", sum256([]byte(want)))
	err = bw.putStream(ctx, hash, strings.NewReader(strings.Repeat("b", 5000)))
	if !errors.Is(err, errChanged) {
		t.Fatalf("putStream of other content = %v, want errChanged", err)
	}
	if _, err := e.store.Stat(ctx, blobKey(testRef, KindStorage, hash)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a blob was stored under the wrong hash: %v", err)
	}
	if err := bw.putStream(ctx, hash, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	rc, err := e.svc.openBlob(ctx, testRef, KindStorage, hash)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != want {
		t.Fatal("blob content differs")
	}
}

func TestRestoreRefusesUnsafePathsAndDamagedBlobs(t *testing.T) {
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", "a//b", "a/./b", `a\b`, "a/\x00"} {
		if _, err := cleanRel(p); err == nil {
			t.Errorf("cleanRel(%q) accepted", p)
		}
	}
	if got, err := cleanRel("bucket/name/version"); err != nil || got != "bucket/name/version" {
		t.Errorf("cleanRel = %q, %v", got, err)
	}

	e := newTestEnv(t)
	ctx := context.Background()
	e.putObject(t, testRef, "b/o/v1", []byte("content"), e.now.Add(-time.Hour))
	snap, err := e.svc.backupStorage(ctx, testRef, ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the blob with a valid zstd stream of other content.
	var hash string
	if err := e.svc.readEntries(ctx, snap.Dir()+"/"+entriesName, func(en fileEntry) error { hash = en.Hash; return nil }); err != nil {
		t.Fatal(err)
	}
	bw, _ := e.svc.newBlobWriter(testRef, KindStorage)
	defer bw.close()
	z := bw.enc.EncodeAll([]byte("tampered"), nil)
	if err := e.store.Put(ctx, blobKey(testRef, KindStorage, hash), bytes.NewReader(z)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RestoreFiles(ctx, testRef, testRef3, FilesRestoreOptions{Latest: true}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("restore of a damaged blob = %v", err)
	}
	if _, err := os.Stat(e.objectsDir(testRef3)); err == nil {
		t.Error("a failed restore left a directory behind")
	}
	if ents, _ := os.ReadDir(filepath.Dir(e.objectsDir(testRef3))); len(ents) != 1 { // only testRef's own directory
		t.Errorf("a failed restore left staging files behind: %v", ents)
	}
}

func TestRestoreFilesWithoutSnapshotsOrBeforeTheFirst(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	res, err := e.svc.RestoreFiles(ctx, testRef, testRef2, FilesRestoreOptions{Latest: true})
	if err != nil || res.Storage != nil || len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "no backup") {
		t.Fatalf("no snapshots = %+v, %v", res, err)
	}
	e.putObject(t, testRef, "b/o/v1", []byte("x"), e.now.Add(-time.Hour))
	if _, err := e.svc.backupStorage(ctx, testRef, ReasonManual); err != nil {
		t.Fatal(err)
	}
	res, err = e.svc.RestoreFiles(ctx, testRef, testRef2, FilesRestoreOptions{At: e.now.Add(-24 * time.Hour)})
	if err != nil || res.Storage != nil || len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "after the target") {
		t.Fatalf("target before the first snapshot = %+v, %v", res, err)
	}
	if _, err := os.Stat(e.objectsDir(testRef2)); err == nil {
		t.Error("objects were restored from a snapshot newer than the target")
	}
}

func TestPreRestoreSnapshotsAreNotPickedByTime(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.putObject(t, testRef, "b/o/v1", []byte("x"), e.now.Add(-time.Hour))
	a, _ := e.svc.backupStorage(ctx, testRef, ReasonScheduled)
	e.now = e.now.Add(time.Hour)
	pre, _ := e.svc.backupStorage(ctx, testRef, ReasonPreRestore)
	all, _ := e.svc.ListFilesSnapshots(ctx, testRef, KindStorage)
	got, err := pickFilesSnapshot(all, e.now.Add(time.Hour), false, "")
	if err != nil || got.ID != a.ID {
		t.Fatalf("picked %v, want the scheduled snapshot %s", got, a.ID)
	}
	if got, err = pickFilesSnapshot(all, time.Time{}, true, pre.ID); err != nil || got.ID != pre.ID {
		t.Fatalf("by id: %v, %v", got, err)
	}
}

func TestPruneFilesAppliesRetentionAndSweepsBlobs(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 7
	base := time.Now().UTC()
	old := base.Add(-time.Hour)
	// Blob ModTimes are real time; the snapshots' clock runs ahead of it.
	snapAt := func(day int, name string, content string) *FilesSnapshot {
		e.now = base.Add(time.Duration(day) * 24 * time.Hour)
		os.RemoveAll(e.objectsDir(testRef))
		e.putObject(t, testRef, "b/"+name+"/v1", []byte(content), old)
		e.putObject(t, testRef, "b/shared/v1", []byte("shared"), old)
		s, err := e.svc.backupStorage(ctx, testRef, ReasonScheduled)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a := snapAt(0, "a", "only in a")
	b := snapAt(1, "b", "only in b")
	c := snapAt(10, "c", "only in c")
	if n := len(blobList(t, e, testRef, KindStorage)); n != 4 {
		t.Fatalf("blobs = %d, want 4", n)
	}
	// A marker younger than a day: a run may be in flight, so no blob is deleted.
	e.now = base.Add(10 * 24 * time.Hour)
	marker := runningDir(testRef, KindStorage) + "inflight"
	if err := e.store.Put(ctx, marker, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	young := e.now.Add(-time.Hour)
	if err := os.Chtimes(e.store.path(marker), young, young); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Prune(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	if res.DeletedFileSnapshots != 1 || res.DeletedBlobs != 0 {
		t.Fatalf("with a run in flight: %+v", res)
	}
	// The marker ages out (the run died), and the next prune sweeps.
	e.now = base.Add(12 * 24 * time.Hour)
	res, err = e.svc.Prune(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := e.svc.ListFilesSnapshots(ctx, testRef, KindStorage)
	ids := []string{}
	for _, m := range all {
		ids = append(ids, m.ID)
	}
	if len(all) != 2 || ids[0] != b.ID || ids[1] != c.ID {
		t.Fatalf("kept snapshots = %v, want [%s %s] (a is outside the window and b is its anchor)", ids, b.ID, c.ID)
	}
	_ = a
	if res.DeletedBlobs != 1 {
		t.Fatalf("deleted blobs = %d, want 1 (only a's own)", res.DeletedBlobs)
	}
	// What is left restores.
	if _, err := e.svc.RestoreFiles(ctx, testRef, testRef3, FilesRestoreOptions{At: b.StopTime}); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, e.objectsDir(testRef3)); string(got["b/b/v1"]) != "only in b" || string(got["b/shared/v1"]) != "shared" {
		t.Fatalf("restored %v", got)
	}
	if ms, _ := e.store.List(ctx, runningDir(testRef, KindStorage)); len(ms) != 0 {
		t.Errorf("stale marker left: %v", ms)
	}
}

func TestPruneKeepsBlobsWhenASnapshotIsUnreadable(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 7
	e.putObject(t, testRef, "b/o/v1", []byte("x"), e.now.Add(-time.Hour))
	s, err := e.svc.backupStorage(ctx, testRef, ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Put(ctx, s.Dir()+"/"+entriesName, strings.NewReader("not zstd")); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(3 * 24 * time.Hour)
	if _, err := e.svc.Prune(ctx, testRef); err == nil {
		t.Fatal("prune went on over an unreadable snapshot")
	}
	if n := len(blobList(t, e, testRef, KindStorage)); n != 1 {
		t.Fatalf("blobs = %d, want the one the damaged snapshot lists", n)
	}
}

func blobList(t *testing.T, e *testEnv, ref, kind string) []ObjectInfo {
	t.Helper()
	l, err := e.store.List(context.Background(), blobsDir(ref, kind))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func sum256(b []byte) [32]byte { return sha256.Sum256(b) }
