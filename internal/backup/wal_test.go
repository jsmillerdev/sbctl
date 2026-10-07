package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPushFetchRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	seg := bytes.Repeat([]byte("wal-data-"), 1<<16) // compressible, ~576 KiB
	name := walName(1, 7)
	src := writeFile(t, filepath.Join(e.root, "pgdata", "pg_wal", name), seg)

	if err := e.svc.PushWAL(ctx, testRef, src); err != nil {
		t.Fatal(err)
	}
	info, err := e.store.Stat(ctx, walKey(testRef, name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size >= int64(len(seg)) {
		t.Errorf("stored %d bytes for %d bytes of compressible WAL: not compressed", info.Size, len(seg))
	}

	dest := filepath.Join(e.root, "restore", "RECOVERYXLOG")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.FetchWAL(ctx, testRef, name, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, seg) {
		t.Fatalf("fetched file differs: err=%v len=%d want %d", err, len(got), len(seg))
	}
	ents, _ := os.ReadDir(filepath.Dir(dest))
	if len(ents) != 1 {
		t.Errorf("fetch left temporary files behind: %v", ents)
	}
}

func TestPushIsIdempotentAndRefusesDifferentContent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	name := walName(1, 3)
	src := writeFile(t, filepath.Join(e.root, "a", name), []byte("segment contents"))

	if err := e.svc.PushWAL(ctx, testRef, src); err != nil {
		t.Fatal(err)
	}
	first, _ := e.store.Stat(ctx, walKey(testRef, name))

	// Identical re-push (Postgres retries after a crash between archive and status update).
	if err := e.svc.PushWAL(ctx, testRef, src); err != nil {
		t.Fatalf("identical re-push: %v", err)
	}
	if again, _ := e.store.Stat(ctx, walKey(testRef, name)); !again.ModTime.Equal(first.ModTime) {
		t.Error("identical re-push rewrote the archived object")
	}

	// Same name, different bytes (also: same length, different bytes, and a longer file).
	for _, body := range []string{"segment contentz", "segment contents plus", "seg"} {
		other := writeFile(t, filepath.Join(e.root, "b", name), []byte(body))
		err := e.svc.PushWAL(ctx, testRef, other)
		if !errors.Is(err, ErrWALConflict) {
			t.Fatalf("push of different content %q = %v, want ErrWALConflict", body, err)
		}
	}
	if got := decompressedWAL(t, e, testRef, name); string(got) != "segment contents" {
		t.Fatalf("archive was modified by a refused push: %q", got)
	}

	// Another project's archive is independent.
	if err := e.svc.PushWAL(ctx, testRef2, writeFile(t, filepath.Join(e.root, "c", name), []byte("other project"))); err != nil {
		t.Fatalf("same name under another ref: %v", err)
	}
}

func decompressedWAL(t *testing.T, e *testEnv, ref, name string) []byte {
	t.Helper()
	dest := filepath.Join(t.TempDir(), name)
	if err := e.svc.FetchWAL(context.Background(), ref, name, dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetchMissingIsErrNoWAL(t *testing.T) {
	e := newTestEnv(t)
	dest := filepath.Join(e.root, "out")
	err := e.svc.FetchWAL(context.Background(), testRef, walName(1, 99), dest)
	if !errors.Is(err, ErrNoWAL) {
		t.Fatalf("FetchWAL missing = %v, want ErrNoWAL", err)
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Error("fetch of a missing file created the destination")
	}
	// The usual first probe of a timeline switch during recovery.
	if err := e.svc.FetchWAL(context.Background(), testRef, "00000002.history", dest); !errors.Is(err, ErrNoWAL) {
		t.Fatalf("FetchWAL missing history = %v, want ErrNoWAL", err)
	}
}

func TestPushFetchSpecialNames(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	for _, name := range []string{
		"00000002.history",
		walName(2, 5) + ".partial",
		walName(1, 4) + ".00000028.backup",
		walName(1, 4),
	} {
		body := []byte("content of " + name)
		if err := e.svc.PushWAL(ctx, testRef, writeFile(t, filepath.Join(e.root, "src", name), body)); err != nil {
			t.Fatalf("push %s: %v", name, err)
		}
		if got := decompressedWAL(t, e, testRef, name); !bytes.Equal(got, body) {
			t.Fatalf("%s round trip = %q", name, got)
		}
	}
	// .partial and the full segment are different archive entries.
	if _, err := e.store.Stat(ctx, walKey(testRef, walName(2, 5))); !errors.Is(err, ErrNotFound) {
		t.Errorf(".partial must not shadow the full segment name: %v", err)
	}
}

func TestPushFetchRejectNonWALNames(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	for _, name := range []string{"base", "000000010000000000000001.zst", "../00000001.history", "RECOVERYXLOG", "00000001.HISTORY", "0000000100000000000000G1", ""} {
		if err := e.svc.FetchWAL(ctx, testRef, name, filepath.Join(e.root, "x")); err == nil || errors.Is(err, ErrNoWAL) {
			t.Errorf("FetchWAL(%q) = %v, want a name error", name, err)
		}
		if name == "" {
			continue
		}
		p := writeFile(t, filepath.Join(e.root, "n", filepath.Base(name)+"_"), []byte("x"))
		if err := e.svc.PushWAL(ctx, testRef, p); err == nil {
			t.Errorf("PushWAL(%q) succeeded", p)
		}
	}
	for _, ref := range []string{"", "short", "../../etc", "UPPERCASEUPPERCASEUP"} {
		if err := e.svc.PushWAL(ctx, ref, "x"); err == nil {
			t.Errorf("PushWAL with ref %q succeeded", ref)
		}
	}
	if err := e.svc.FetchWAL(ctx, "system", walName(1, 1), filepath.Join(e.root, "x")); !errors.Is(err, ErrNoWAL) {
		t.Errorf("the system ref is valid: %v", err)
	}
}

func TestPushResolvesRelativePathAgainstDataDir(t *testing.T) {
	e := newTestEnv(t)
	dd := e.svc.opt.DataDir(testRef)
	name := walName(1, 2)
	writeFile(t, filepath.Join(dd, "pg_wal", name), []byte("rel"))
	if err := e.svc.PushWAL(context.Background(), testRef, "pg_wal/"+name); err != nil {
		t.Fatal(err)
	}
	if got := decompressedWAL(t, e, testRef, name); string(got) != "rel" {
		t.Fatalf("got %q", got)
	}
}

func TestPushMissingSourceFailsAndStoresNothing(t *testing.T) {
	e := newTestEnv(t)
	name := walName(1, 8)
	err := e.svc.PushWAL(context.Background(), testRef, filepath.Join(e.root, "nope", name))
	if err == nil {
		t.Fatal("push of a missing file succeeded")
	}
	if objs, _ := e.store.List(context.Background(), testRef+"/"); len(objs) != 0 {
		t.Fatalf("failed push left objects: %v", objs)
	}
}

func TestPushDetectsCorruptArchivedObject(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	name := walName(1, 6)
	src := writeFile(t, filepath.Join(e.root, "s", name), []byte("good"))
	// Not valid zstd.
	if err := e.store.Put(ctx, walKey(testRef, name), bytes.NewReader([]byte("garbage garbage garbage"))); err != nil {
		t.Fatal(err)
	}
	err := e.svc.PushWAL(ctx, testRef, src)
	if err == nil || errors.Is(err, ErrWALConflict) {
		t.Fatalf("push over a corrupt object = %v, want a read error that is not a plain conflict", err)
	}
	// Fetch of a corrupt object is an error but not "end of archive".
	if err := e.svc.FetchWAL(ctx, testRef, name, filepath.Join(e.root, "o")); err == nil || errors.Is(err, ErrNoWAL) {
		t.Fatalf("fetch of a corrupt object = %v", err)
	}
	if _, serr := os.Stat(filepath.Join(e.root, "o")); !os.IsNotExist(serr) {
		t.Error("fetch of a corrupt object left a destination file")
	}
}

func TestParseWALName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want walFileInfo
	}{
		{"000000020000000A0000001F", walFileInfo{Timeline: 2, Seg: 0xA<<32 | 0x1F, Kind: "segment"}},
		{"000000020000000A0000001F.partial", walFileInfo{Timeline: 2, Seg: 0xA<<32 | 0x1F, Kind: "partial"}},
		{"000000020000000A0000001F.00000028.backup", walFileInfo{Timeline: 2, Seg: 0xA<<32 | 0x1F, Kind: "backup"}},
		{"0000000B.history", walFileInfo{Timeline: 0xB, Kind: "history"}},
	} {
		got, ok := parseWALName(tc.name)
		if !ok || got != tc.want {
			t.Errorf("parseWALName(%q) = %+v, %v, want %+v", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := parseWALName("nonsense"); ok {
		t.Error("parseWALName accepted nonsense")
	}
}

// failPutStore fails every Put at once, without reading.
type failPutStore struct{ Store }

func (failPutStore) Put(context.Context, string, io.Reader) error { return errors.New("put failed") }

// slowReader blocks in Read until released, and records whether a Read is in progress.
type slowReader struct {
	in          atomic.Bool
	release     chan struct{}
	interrupted atomic.Bool
	once        sync.Once
}

func (r *slowReader) Read(p []byte) (int, error) {
	r.in.Store(true)
	defer r.in.Store(false)
	<-r.release
	return 0, io.ErrUnexpectedEOF
}

func (r *slowReader) Interrupt() {
	r.interrupted.Store(true)
	r.once.Do(func() { close(r.release) })
}

// PushWALReader returns only when nothing reads its reader any more, and it interrupts a
// reader that can be stuck in a Read (the relay's request body).
func TestPushWALReaderStopsReadingBeforeItReturns(t *testing.T) {
	e := newTestEnv(t)
	svc, err := New(Options{Config: e.cfg, Registry: e.reg, Store: failPutStore{e.store}, Secrets: e.sec})
	if err != nil {
		t.Fatal(err)
	}
	r := &slowReader{release: make(chan struct{})}
	if err := svc.PushWALReader(context.Background(), testRef, walA, r); err == nil {
		t.Fatal("push succeeded although the store failed")
	}
	if r.in.Load() {
		t.Fatal("PushWALReader returned while its reader was still being read")
	}
	if !r.interrupted.Load() {
		t.Fatal("a blocked reader was not interrupted")
	}
}
