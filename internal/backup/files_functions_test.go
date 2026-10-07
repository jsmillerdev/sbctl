package backup

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFunctions is an in-memory Functions that counts file loads.
type fakeFunctions struct {
	mu      sync.Mutex
	recs    map[string]map[string]FunctionRecord // ref -> slug
	files   map[string]map[string][]FunctionFile
	secrets map[string]map[string][]byte
	loads   int
}

func newFakeFunctions() *fakeFunctions {
	return &fakeFunctions{recs: map[string]map[string]FunctionRecord{}, files: map[string]map[string][]FunctionFile{}, secrets: map[string]map[string][]byte{}}
}

func (f *fakeFunctions) deploy(ref string, r FunctionRecord, files ...FunctionFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recs[ref] == nil {
		f.recs[ref], f.files[ref] = map[string]FunctionRecord{}, map[string][]FunctionFile{}
	}
	f.recs[ref][r.Slug] = r
	f.files[ref][r.Slug] = files
}

func (f *fakeFunctions) ListFunctions(_ context.Context, ref string) ([]FunctionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FunctionRecord
	for _, r := range f.recs[ref] {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (f *fakeFunctions) FunctionFiles(_ context.Context, ref, slug string) ([]FunctionFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.recs[ref][slug]; !ok {
		return nil, ErrNoFunction
	}
	f.loads++
	return append([]FunctionFile(nil), f.files[ref][slug]...), nil
}

func (f *fakeFunctions) ListFunctionSecrets(_ context.Context, ref string) ([]FunctionSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FunctionSecret
	for n, b := range f.secrets[ref] {
		out = append(out, FunctionSecret{Name: n, Sealed: b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeFunctions) RestoreFunction(ctx context.Context, ref string, r FunctionRecord, files []FunctionFile) error {
	f.deploy(ref, r, files...)
	return nil
}

func (f *fakeFunctions) DeleteFunction(_ context.Context, ref, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.recs[ref][slug]; !ok {
		return ErrNoFunction
	}
	delete(f.recs[ref], slug)
	delete(f.files[ref], slug)
	return nil
}

func (f *fakeFunctions) RestoreFunctionSecrets(_ context.Context, ref string, sealed map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.secrets[ref] == nil {
		f.secrets[ref] = map[string][]byte{}
	}
	for n, b := range sealed {
		f.secrets[ref][n] = b
	}
	return nil
}

func (f *fakeFunctions) DeleteFunctionSecrets(_ context.Context, ref string, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		delete(f.secrets[ref], n)
	}
	return nil
}

func fnRec(slug string, version int, at time.Time) FunctionRecord {
	return FunctionRecord{Slug: slug, ID: "00000000-0000-4000-8000-00000000000" + string(rune('0'+version)), Name: slug, Version: version,
		Status: "ACTIVE", VerifyJWT: true, EntrypointPath: "index.ts", CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
}

func TestFunctionsBackupIsIncrementalAndRestoresExactly(t *testing.T) {
	e := newTestEnv(t)
	fns := newFakeFunctions()
	e.svc.opt.Functions = fns
	ctx := context.Background()
	t0 := e.now

	fns.deploy(testRef, fnRec("hello", 3, t0), FunctionFile{Path: "index.ts", Content: []byte("v3")}, FunctionFile{Path: "lib/a.ts", Content: []byte{0, 1, 2}})
	fns.deploy(testRef, fnRec("bundled", 1, t0), FunctionFile{Path: ".supavise-bundle.ezbr", Content: randBytes(t, 3<<20)})
	fns.deploy(testRef, fnRec("nofiles", 1, t0))
	fns.secrets[testRef] = map[string][]byte{"API_KEY": []byte("sealed-1"), "OTHER": []byte("sealed-2")}

	s1, err := e.svc.backupFunctions(ctx, testRef, ReasonScheduled)
	if err != nil || s1 == nil || len(s1.Functions) != 3 || s1.Files != 5 {
		t.Fatalf("snapshot 1 = %+v, %v", s1, err)
	}
	if fns.loads != 3 {
		t.Fatalf("file loads = %d, want 3", fns.loads)
	}

	// Nothing changed: no function is loaded and nothing is uploaded.
	e.now = e.now.Add(time.Hour)
	s2, err := e.svc.backupFunctions(ctx, testRef, ReasonScheduled)
	if err != nil || s2.NewFiles != 0 || s2.Files != 5 || fns.loads != 3 {
		t.Fatalf("snapshot 2 = %+v (loads %d), %v", s2, fns.loads, err)
	}

	// A new version of one function.
	e.now = e.now.Add(time.Hour)
	fns.deploy(testRef, fnRec("hello", 4, e.now), FunctionFile{Path: "index.ts", Content: []byte("v4")}, FunctionFile{Path: "lib/a.ts", Content: []byte{0, 1, 2}})
	s3, err := e.svc.backupFunctions(ctx, testRef, ReasonScheduled)
	if err != nil || s3.NewFiles != 1 || fns.loads != 4 {
		t.Fatalf("snapshot 3 = %+v (loads %d), %v", s3, fns.loads, err)
	}

	// Restore snapshot 2 (the one before v4) as a new project: records come back exactly,
	// with their versions, ids and timestamps.
	res, err := e.svc.RestoreFiles(ctx, testRef, testRef2, FilesRestoreOptions{SnapshotID: s2.ID})
	if err != nil || res.Functions == nil {
		t.Fatalf("restore = %+v, %v", res, err)
	}
	got, _ := fns.ListFunctions(ctx, testRef2)
	if len(got) != 3 || got[1].Slug != "hello" || got[1].Version != 3 || !got[1].UpdatedAt.Equal(t0) || got[1].ID != fnRec("hello", 3, t0).ID {
		t.Fatalf("restored records = %+v", got)
	}
	files, _ := fns.FunctionFiles(ctx, testRef2, "hello")
	if len(files) != 2 || string(files[0].Content) != "v3" {
		t.Fatalf("restored files = %+v", files)
	}
	if b, _ := fns.FunctionFiles(ctx, testRef2, "bundled"); len(b) != 1 || len(b[0].Content) != 3<<20 {
		t.Fatalf("bundle not restored")
	}
	if s, _ := fns.ListFunctionSecrets(ctx, testRef2); len(s) != 2 || string(s[0].Sealed) != "sealed-1" {
		t.Fatalf("restored secrets = %+v", s)
	}

	// In place to the first snapshot: the function and the secret it does not know are gone,
	// and what was there is kept in a pre-restore snapshot.
	fns.deploy(testRef, fnRec("extra", 1, e.now), FunctionFile{Path: "index.ts", Content: []byte("x")})
	fns.secrets[testRef]["NEW"] = []byte("sealed-3")
	e.now = e.now.Add(time.Hour)
	res, err = e.svc.RestoreFiles(ctx, testRef, testRef, FilesRestoreOptions{SnapshotID: s1.ID})
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := fns.ListFunctions(ctx, testRef)
	if len(cur) != 3 || cur[1].Slug != "hello" || cur[1].Version != 3 {
		t.Fatalf("after in-place restore = %+v", cur)
	}
	if sec, _ := fns.ListFunctionSecrets(ctx, testRef); len(sec) != 2 {
		t.Fatalf("secrets after in-place restore = %+v", sec)
	}
	all, _ := e.svc.ListFilesSnapshots(ctx, testRef, KindFunctions)
	var pre *FilesSnapshot
	for i := range all {
		if all[i].Reason == ReasonPreRestore {
			pre = &all[i]
		}
	}
	if pre == nil || len(pre.Functions) != 4 {
		t.Fatalf("no pre-restore snapshot of the 4 deployments: %+v", all)
	}

	// Restoring a pre-restore snapshot brings the replaced state back.
	if _, err := e.svc.RestoreFiles(ctx, testRef, testRef, FilesRestoreOptions{SnapshotID: pre.ID}); err != nil {
		t.Fatal(err)
	}
	if cur, _ = fns.ListFunctions(ctx, testRef); len(cur) != 4 {
		t.Fatalf("after restoring the pre-restore snapshot = %+v", cur)
	}
}

func TestFunctionsNothingToBackUp(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if s, err := e.svc.backupFunctions(ctx, testRef, ReasonManual); s != nil || err != nil {
		t.Fatalf("without a store = %v, %v", s, err)
	}
	e.svc.opt.Functions = newFakeFunctions()
	if s, err := e.svc.backupFunctions(ctx, testRef, ReasonManual); s != nil || err != nil {
		t.Fatalf("project without functions = %v, %v", s, err)
	}
	if _, err := e.svc.RestoreFiles(ctx, testRef, testRef2, FilesRestoreOptions{Latest: true}); err != nil {
		t.Fatal(err)
	}
}

func TestFunctionsRestoreRejectsAForgedSnapshot(t *testing.T) {
	e := newTestEnv(t)
	fns := newFakeFunctions()
	e.svc.opt.Functions = fns
	ctx := context.Background()
	fns.deploy(testRef, fnRec("hello", 1, e.now), FunctionFile{Path: "index.ts", Content: []byte("x")})
	s, err := e.svc.backupFunctions(ctx, testRef, ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	s.Functions = nil // entries now name a function the summary does not know
	if err := e.svc.restoreFunctions(ctx, s, testRef2, false); err == nil || !strings.Contains(err.Error(), "no record") {
		t.Fatalf("restore = %v", err)
	}
	if err := e.svc.restoreFunctions(ctx, &FilesSnapshot{Ref: testRef, Kind: KindFunctions, ID: "nope"}, testRef2, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore of a missing snapshot = %v", err)
	}
}
