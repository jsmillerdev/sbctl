package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
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

// A restore brings the project's files back from the newest backup at or before the target,
// as a new project and in place, unless SkipFiles says otherwise.
func TestRestoreBringsFilesBack(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	src := e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), src)
	fns := newFakeFunctions()
	e.svc.opt.Functions = fns
	fm := &fakeManager{e: e, dataDir: filepath.Join(t.TempDir(), "restored")}
	e.svc.opt.Manager = fm

	e.putObject(t, testRef, "b/o/v1", []byte("object"), e.now.Add(-2*time.Hour))
	fns.deploy(testRef, fnRec("hello", 1, e.now.Add(-2*time.Hour)), FunctionFile{Path: "index.ts", Content: []byte("x")})
	if _, err := e.svc.BackupFiles(ctx, testRef, FilesOptions{Reason: ReasonScheduled}); err != nil {
		t.Fatal(err)
	}
	// Changes after the backup are not in what the restore brings back.
	e.now = e.now.Add(time.Hour)
	e.putObject(t, testRef, "b/newer/v1", []byte("stored after the nightly copy"), e.now)
	target := e.now.Add(-time.Minute)

	var progress []string
	p, err := e.svc.RestoreWith(ctx, testRef, target, testRef2, RestoreOptions{Progress: func(m string) { progress = append(progress, m) }})
	if err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, e.objectsDir(p.Ref)); len(got) != 1 || string(got["b/o/v1"]) != "object" {
		t.Fatalf("restored objects = %v", got)
	}
	if cur, _ := fns.ListFunctions(ctx, p.Ref); len(cur) != 1 {
		t.Fatalf("restored functions = %+v", cur)
	}
	if len(progress) != 2 || !strings.Contains(progress[0], "objects: 1 files") || !strings.Contains(progress[1], "functions: 1 deployments") {
		t.Fatalf("progress = %q", progress)
	}
	evs, _ := e.reg.ListEvents(ctx, testRef, 10)
	var completed bool
	for _, ev := range evs {
		completed = completed || ev.Kind == "restore.completed"
	}
	if !completed {
		t.Fatalf("events = %+v", evs)
	}

	// SkipFiles leaves the new project without files.
	fm.dataDir = filepath.Join(t.TempDir(), "restored2")
	if _, err := e.svc.RestoreWith(ctx, testRef, target, testRef3, RestoreOptions{SkipFiles: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.objectsDir(testRef3)); err == nil {
		t.Error("SkipFiles restored objects")
	}
	if cur, _ := fns.ListFunctions(ctx, testRef3); len(cur) != 0 {
		t.Errorf("SkipFiles restored functions: %+v", cur)
	}
}

// Files that cannot be restored do not undo the database restore, and the error says so.
func TestRestoreReportsFilesThatFailAfterTheDatabaseCameBack(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	src := e.addProject(t, testRef)
	e.storeBase(t, testRef, fakeDataDir(t), e.now.Add(-time.Hour), src)
	e.svc.opt.Manager = &fakeManager{e: e, dataDir: filepath.Join(t.TempDir(), "restored")}
	e.putObject(t, testRef, "b/o/v1", []byte("object"), e.now.Add(-2*time.Hour))
	snap, err := e.svc.backupStorage(ctx, testRef, ReasonScheduled)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the blob the snapshot needs.
	var hash string
	if err := e.svc.readEntries(ctx, snap.Dir()+"/"+entriesName, func(en fileEntry) error { hash = en.Hash; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(ctx, blobKey(testRef, KindStorage, hash)); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.RestoreWith(ctx, testRef, e.now, testRef2, RestoreOptions{})
	if err == nil || p == nil || !strings.Contains(err.Error(), "was restored as "+testRef2) || !strings.Contains(err.Error(), "restore-files") {
		t.Fatalf("restore = %v, %v", p, err)
	}
	evs, _ := e.reg.ListEvents(ctx, testRef, 10)
	var failed bool
	for _, ev := range evs {
		failed = failed || ev.Kind == "restore.files_failed"
	}
	if !failed {
		t.Fatalf("no restore.files_failed event: %+v", evs)
	}
}

// A nightly run copies the files first and takes the base backup after, so the snapshot a
// backup is paired with finished before the backup did. "restore --to backup" must pick that
// one: not the previous night's, and not none at all for the very first backup.
func TestRestoreToBackupBringsBackTheFilesOfThatRun(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	src := e.addProject(t, testRef)
	fns := newFakeFunctions()
	e.svc.opt.Functions = fns
	fm := &fakeManager{e: e}
	e.svc.opt.Manager = fm
	night := func(n int, content string) Manifest {
		e.putObject(t, testRef, "b/o/v1", []byte(content), e.now.Add(-time.Hour))
		fns.deploy(testRef, fnRec("hello", n, e.now), FunctionFile{Path: "index.ts", Content: []byte(content)})
		if _, err := e.svc.BackupFiles(ctx, testRef, FilesOptions{Reason: ReasonScheduled}); err != nil {
			t.Fatal(err)
		}
		e.now = e.now.Add(2 * time.Minute) // the base backup follows
		return e.storeBase(t, testRef, fakeDataDir(t), e.now, src)
	}
	restoreAs := func(as string, o RestoreOptions) {
		t.Helper()
		fm.dataDir = filepath.Join(t.TempDir(), "restored")
		o.ToBackup = true
		if _, err := e.svc.RestoreWith(ctx, testRef, time.Time{}, as, o); err != nil {
			t.Fatalf("restore --to backup --as %s: %v", as, err)
		}
	}
	wantFiles := func(ref, content string) {
		t.Helper()
		if got := readTree(t, e.objectsDir(ref)); len(got) != 1 || string(got["b/o/v1"]) != content {
			t.Errorf("%s: objects = %v, want %q", ref, got, content)
		}
		files, err := fns.FunctionFiles(ctx, ref, "hello")
		if err != nil || len(files) != 1 || string(files[0].Content) != content {
			t.Errorf("%s: function = %v, %v, want %q", ref, files, err, content)
		}
	}

	// The first backup ever.
	first := night(1, "night one")
	restoreAs(testRef2, RestoreOptions{})
	wantFiles(testRef2, "night one")

	// A second night: the newest backup gets the second night's files, the first backup
	// still gets the first night's.
	e.now = e.now.Add(24 * time.Hour)
	night(2, "night two")
	restoreAs(testRef3, RestoreOptions{})
	wantFiles(testRef3, "night two")
	const ref4 = "cdefghijklmnopqrstuv"
	restoreAs(ref4, RestoreOptions{BackupID: first.ID})
	wantFiles(ref4, "night one")

	// In place, the same pick applies.
	e.putObject(t, testRef, "b/o/v1", []byte("damaged"), e.now)
	writeFile(t, filepath.Join(e.svc.opt.DataDir(testRef), "PG_VERSION"), []byte("old"))
	restoreAs("", RestoreOptions{Force: true, BackupID: first.ID})
	wantFiles(testRef, "night one")
}

// Restoring files into an existing project that has functions or secrets would overwrite
// those with the same names, so it is refused and nothing is written.
func TestRestoreFilesRefusesAProjectThatHasFunctions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fns := newFakeFunctions()
	e.svc.opt.Functions = fns
	e.addProject(t, testRef)
	e.addProject(t, testRef2)
	e.putObject(t, testRef, "b/o/v1", []byte("object"), e.now.Add(-time.Hour))
	fns.deploy(testRef, fnRec("hello", 1, e.now), FunctionFile{Path: "index.ts", Content: []byte("source project")})
	if _, err := e.svc.BackupFiles(ctx, testRef, FilesOptions{Reason: ReasonScheduled}); err != nil {
		t.Fatal(err)
	}

	// The target has its own function of the same name, and a secret.
	fns.deploy(testRef2, fnRec("hello", 2, e.now), FunctionFile{Path: "index.ts", Content: []byte("target project")})
	fns.secrets[testRef2] = map[string][]byte{"STRIPE_KEY": []byte("sealed")}
	_, err := e.svc.RestoreFiles(ctx, testRef, testRef2, FilesRestoreOptions{Latest: true, RequireProject: true})
	if err == nil || !strings.Contains(err.Error(), "already has 1 Edge Functions and 1 function secrets") {
		t.Fatalf("restore into a populated project = %v", err)
	}
	files, _ := fns.FunctionFiles(ctx, testRef2, "hello")
	if len(files) != 1 || string(files[0].Content) != "target project" || string(fns.secrets[testRef2]["STRIPE_KEY"]) != "sealed" {
		t.Fatalf("the target was changed: %v %v", files, fns.secrets[testRef2])
	}
	if _, err := os.Stat(e.objectsDir(testRef2)); err == nil {
		t.Fatal("objects were restored although the functions were refused")
	}

	// A project with none takes the files and gets new function ids.
	e.addProject(t, testRef3)
	if _, err := e.svc.RestoreFiles(ctx, testRef, testRef3, FilesRestoreOptions{Latest: true, RequireProject: true}); err != nil {
		t.Fatal(err)
	}
	src, _ := fns.ListFunctions(ctx, testRef)
	dst, _ := fns.ListFunctions(ctx, testRef3)
	if len(dst) != 1 || dst[0].Slug != "hello" || dst[0].Version != src[0].Version || !dst[0].UpdatedAt.Equal(src[0].UpdatedAt) {
		t.Fatalf("restored functions = %+v, source %+v", dst, src)
	}
	if dst[0].ID == src[0].ID || !validUUID(dst[0].ID) {
		t.Fatalf("the copy keeps the source's id or has a bad one: %q vs %q", dst[0].ID, src[0].ID)
	}
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !strings.ContainsRune("0123456789abcdef", c):
			return false
		}
	}
	return s[14] == '4'
}

// A file that has the same content as another file being stored must not be recorded as
// stored until that upload has succeeded: the other file may change under its upload and
// retry under a new hash, leaving this one's blob unwritten.
func TestHasWaitsForTheUploadOfTheSameContent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	bw, err := e.svc.newBlobWriter(testRef, KindStorage)
	if err != nil {
		t.Fatal(err)
	}
	defer bw.close()
	hash := fmt.Sprintf("%x", sum256([]byte("content")))
	started, release := make(chan struct{}), make(chan struct{})
	uploadErr := make(chan error, 1)
	go func() {
		uploadErr <- bw.ensure(ctx, hash, func() (int64, error) {
			close(started)
			<-release
			return 0, errChanged // the first file changed under its upload
		})
	}()
	<-started
	got := make(chan bool, 1)
	go func() {
		ok, err := bw.has(ctx, hash)
		if err != nil {
			t.Error(err)
		}
		got <- ok
	}()
	select {
	case ok := <-got:
		t.Fatalf("has answered %v while the upload was still running", ok)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-uploadErr; !errors.Is(err, errChanged) {
		t.Fatalf("upload = %v", err)
	}
	if ok := <-got; ok {
		t.Fatal("has reported a blob as stored after its upload failed")
	}
}

// A pre-restore snapshot is never picked by a restore to a time, so it must not become the
// retention anchor and push out the older snapshot that a restore to the start of the window
// needs. It is kept for the window like any other and then dropped.
func TestPruneAnchorIgnoresPreRestoreSnapshots(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.cfg.Backup.RetentionDays = 7
	base := time.Now().UTC()
	snap := func(day int, reason, content string) *FilesSnapshot {
		e.now = base.Add(time.Duration(day) * 24 * time.Hour)
		e.putObject(t, testRef, "b/o/v1", []byte(content), base.Add(-time.Duration(day+1)*time.Hour))
		s, err := e.svc.backupStorage(ctx, testRef, reason)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a := snap(0, ReasonScheduled, "day zero")
	preOld := snap(1, ReasonPreRestore, "before a restore")
	snap(2, ReasonScheduled, "day two") // still before the window at day 10: day 3 on
	c := snap(8, ReasonScheduled, "day eight")
	preNew := snap(9, ReasonPreRestore, "before another restore")

	e.now = base.Add(10 * 24 * time.Hour)
	if _, err := e.svc.Prune(ctx, testRef); err != nil {
		t.Fatal(err)
	}
	all, _ := e.svc.ListFilesSnapshots(ctx, testRef, KindStorage)
	var got []string
	for _, m := range all {
		got = append(got, m.ID)
	}
	// Anchor: the newest normal snapshot before the window (day 2); a and preOld are out.
	if len(all) != 3 || got[1] != c.ID || got[2] != preNew.ID {
		t.Fatalf("kept %v; a=%s preOld=%s c=%s preNew=%s", got, a.ID, preOld.ID, c.ID, preNew.ID)
	}
	if all[0].Reason != ReasonScheduled || !all[0].StopTime.Before(base.Add(3*24*time.Hour)) {
		t.Fatalf("the anchor is %+v, want the day-two snapshot", all[0])
	}
}

// storeHook runs a function before every List, so a test can start "a backup" at the moment
// the sweep is about to delete.
type storeHook struct {
	Store
	onList func(prefix string)
}

func (h *storeHook) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if h.onList != nil {
		h.onList(prefix)
	}
	return h.Store.List(ctx, prefix)
}

// A backup that starts after the sweep looked at the running markers may be about to
// reference a blob the sweep has chosen to delete. The sweep looks again before it deletes.
func TestPruneSweepLooksAgainBeforeDeletingBlobs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(t *testing.T, e *testEnv)
	}{
		{"a run that left its marker", func(t *testing.T, e *testEnv) {
			marker := runningDir(testRef, KindStorage) + "late"
			if err := e.store.Put(context.Background(), marker, strings.NewReader("x")); err != nil {
				t.Fatal(err)
			}
			young := e.now.Add(-time.Hour)
			if err := os.Chtimes(e.store.path(marker), young, young); err != nil {
				t.Fatal(err)
			}
		}},
		{"a run that already finished", func(t *testing.T, e *testEnv) {
			key := snapshotsDir(testRef, KindStorage) + "late-run/" + summaryName
			if err := e.store.Put(context.Background(), key, strings.NewReader("{}")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			ctx := context.Background()
			e.cfg.Backup.RetentionDays = 7
			base := time.Now().UTC()
			for i, content := range []string{"only in a", "only in b", "only in c"} {
				e.now = base.Add([]time.Duration{0, 24 * time.Hour, 10 * 24 * time.Hour}[i])
				os.RemoveAll(e.objectsDir(testRef))
				e.putObject(t, testRef, "b/o/v1", []byte(content), base.Add(-time.Duration(i+1)*time.Hour))
				if _, err := e.svc.backupStorage(ctx, testRef, ReasonScheduled); err != nil {
					t.Fatal(err)
				}
			}
			e.now = base.Add(12 * 24 * time.Hour)
			lists := 0
			hs := &storeHook{Store: e.store}
			hs.onList = func(prefix string) {
				if prefix == runningDir(testRef, KindStorage) {
					if lists++; lists == 2 { // the look right before the delete
						tc.start(t, e)
					}
				}
			}
			e.svc.opt.Store = hs
			res, err := e.svc.Prune(ctx, testRef)
			if err != nil {
				t.Fatal(err)
			}
			if res.DeletedBlobs != 0 || len(blobList(t, e, testRef, KindStorage)) != 3 {
				t.Fatalf("blobs were deleted although a run had started: %+v, %d blobs", res, len(blobList(t, e, testRef, KindStorage)))
			}
			// With nothing new, the next prune sweeps a's blob.
			e.svc.opt.Store = e.store
			e.store.Delete(ctx, runningDir(testRef, KindStorage)+"late", snapshotsDir(testRef, KindStorage)+"late-run/"+summaryName)
			if res, err = e.svc.Prune(ctx, testRef); err != nil || res.DeletedBlobs != 1 {
				t.Fatalf("later prune = %+v, %v", res, err)
			}
		})
	}
}

// storage_backend is read from the config file at each run: a daemon that started before `supavise
// storage migrate` changed it must not back up the directory the objects left, nor the other way round.
func TestStorageBackendIsReadFromTheConfigFileAtEachRun(t *testing.T) {
	e := newTestEnv(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	e.svc.opt.ConfigPath = cfgPath
	if e.cfg.Fleet.StorageBackend == "s3" {
		t.Fatal("the test config starts on the file backend")
	}
	// No file: the Service's own configuration decides (it is on the file backend).
	if e.svc.storageIsS3() || e.svc.storageDir(testRef) == "" {
		t.Fatal("without a config file the Service's copy decides")
	}
	// The operator migrated Storage to S3 after the daemon started.
	if err := os.WriteFile(cfgPath, []byte("[fleet]\nstorage_backend = \"s3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !e.svc.storageIsS3() || e.svc.storageDir(testRef) != "" {
		t.Fatal("a daemon that predates the migration still looks at the old directory")
	}
	// And rolled it back.
	if err := os.WriteFile(cfgPath, []byte("[fleet]\nstorage_backend = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.svc.storageIsS3() || e.svc.storageDir(testRef) == "" {
		t.Fatal("a daemon that predates the rollback still skips the objects")
	}
	// An unreadable file leaves the copy it started with, and says so once.
	if err := os.WriteFile(cfgPath, []byte("[fleet\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	e.svc.opt.Log = slog.New(slog.NewTextHandler(&logged, nil))
	for i := 0; i < 3; i++ {
		if e.svc.storageIsS3() {
			t.Fatal("a broken file switched the backend")
		}
	}
	if n := strings.Count(logged.String(), "cannot be loaded"); n != 1 {
		t.Fatalf("the broken file was reported %d times:\n%s", n, logged.String())
	}
}

// A Service that loaded no config file does not go looking for one: $SUPAVISE_CONFIG and the default
// path belong to whatever else runs on the host.
func TestStorageBackendIgnoresAFileTheServiceWasNotGiven(t *testing.T) {
	e := newTestEnv(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[fleet]\nstorage_backend = \"s3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigPath, cfgPath)
	e.svc.opt.ConfigPath = ""
	if e.svc.storageIsS3() || e.svc.storageDir(testRef) == "" {
		t.Fatal("a file named only by the environment steered the Service")
	}
}
