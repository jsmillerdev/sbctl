package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	walA = "000000010000000000000003"
	walB = "000000010000000000000004"
)

// relayEnv is a Relay over a FileStore with short socket paths (a unix socket path is
// limited to 103 bytes on macOS, which t.TempDir() can exceed).
type relayEnv struct {
	*testEnv
	relay *Relay
	dir   string
	log   *bytes.Buffer
}

func newRelayEnv(t *testing.T, refs ...string) *relayEnv {
	t.Helper()
	e := newTestEnv(t)
	dir, err := os.MkdirTemp("/tmp", "sbr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	re := &relayEnv{testEnv: e, dir: dir, log: &bytes.Buffer{}}
	re.relay = NewRelay(RelayOptions{
		Config:  e.cfg,
		Service: func(context.Context) (*Service, error) { return e.svc, nil },
		Refs:    func() []string { return refs },
		Socket:  func(ref string) string { return filepath.Join(dir, ref[:6]+".sock") },
		Sources: func(ref string) []string { return readRestoreSources(e.cfg, ref) },
		Log:     slog.New(slog.NewTextHandler(re.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	t.Cleanup(re.relay.Close)
	re.relay.Reconcile()
	return re
}

func (re *relayEnv) sock(ref string) string { return filepath.Join(re.dir, ref[:6]+".sock") }

func writeWAL(t *testing.T, name string, size int, seed byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	b := make([]byte, size)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRelayPushAndFetchRoundTrip(t *testing.T) {
	re := newRelayEnv(t, testRef)
	ctx := context.Background()
	src := writeWAL(t, walA, 3<<20, 7)
	if err := RelayPush(ctx, re.sock(testRef), testRef, src); err != nil {
		t.Fatal(err)
	}
	// Durable in the backend when push returned: the same object `sbctl wal push` writes.
	if _, err := re.store.Stat(ctx, walKey(testRef, walA)); err != nil {
		t.Fatalf("pushed file is not in the backend: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "RECOVERYXLOG")
	if err := RelayFetch(ctx, re.sock(testRef), testRef, walA, dest); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(src)
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(want, got) {
		t.Fatal("fetched file differs from the pushed one")
	}
	// An identical re-push succeeds.
	if err := RelayPush(ctx, re.sock(testRef), testRef, src); err != nil {
		t.Fatalf("identical re-push: %v", err)
	}
	// The same name with other content is refused and the archive keeps the first file.
	other := writeWAL(t, walA, 3<<20, 99)
	if err := RelayPush(ctx, re.sock(testRef), testRef, other); !errors.Is(err, ErrWALConflict) {
		t.Fatalf("conflicting push = %v; want ErrWALConflict", err)
	}
	if err := RelayFetch(ctx, re.sock(testRef), testRef, walA, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(want, got) {
		t.Fatal("a refused conflicting push changed the archive")
	}
}

func TestRelayFetchOfAMissingFileIsErrNoWAL(t *testing.T) {
	re := newRelayEnv(t, testRef)
	dest := filepath.Join(t.TempDir(), "x")
	err := RelayFetch(context.Background(), re.sock(testRef), testRef, walB, dest)
	if !errors.Is(err, ErrNoWAL) {
		t.Fatalf("missing file = %v; want ErrNoWAL (exit 1: end of archive)", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a destination file was created for a missing WAL")
	}
}

func TestRelayRefusesBadNamesAndForeignRefs(t *testing.T) {
	re := newRelayEnv(t, testRef, testRef2)
	ctx := context.Background()
	src := writeWAL(t, walA, 1<<10, 1)
	// A push through project 1's socket for project 2 is refused: the socket decides.
	if err := RelayPush(ctx, re.sock(testRef), testRef2, src); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("push for another project = %v; want a 403", err)
	}
	if _, err := re.store.Stat(ctx, walKey(testRef2, walA)); err == nil {
		t.Fatal("a push through the wrong socket reached another project's archive")
	}
	// Project 2's archive holds a file; project 1's socket may not read it.
	if err := RelayPush(ctx, re.sock(testRef2), testRef2, src); err != nil {
		t.Fatal(err)
	}
	err := RelayFetch(ctx, re.sock(testRef), testRef2, walA, filepath.Join(t.TempDir(), "x"))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("fetch of another project's WAL = %v; want a 403", err)
	}
	// Names Postgres never sends are refused before any I/O.
	for _, bad := range []string{"../../etc/passwd", "wal.zst", "00000001000000000000000", "base/x"} {
		req, _ := http.NewRequest(http.MethodPost, "http://relay"+relayPushPath+"?name="+strings.ReplaceAll(bad, "/", "%2F"), strings.NewReader("x"))
		resp, err := relayDo(ctx, re.sock(testRef), req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("name %q answered %d; want 400", bad, resp.StatusCode)
		}
	}
}

// A clone's restore_command reads the source project's archive through the clone's own
// socket, and only while the daemon's restore-sources file names that source.
func TestRelayFetchFromARestoreSource(t *testing.T) {
	re := newRelayEnv(t, testRef, testRef2)
	ctx := context.Background()
	src := writeWAL(t, walA, 1<<10, 5)
	if err := RelayPush(ctx, re.sock(testRef), testRef, src); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "x")
	if err := RelayFetch(ctx, re.sock(testRef2), testRef, walA, dest); err == nil {
		t.Fatal("clone read the source's archive before the restore allowed it")
	}
	if err := re.svc.allowSource(&RestorePlan{Source: testRef, TargetRef: testRef2}); err != nil {
		t.Fatal(err)
	}
	if err := RelayFetch(ctx, re.sock(testRef2), testRef, walA, dest); err != nil {
		t.Fatalf("clone cannot read its restore source: %v", err)
	}
	re.svc.clearRestoreSources(testRef2)
	if err := RelayFetch(ctx, re.sock(testRef2), testRef, walA, dest); err == nil {
		t.Fatal("clone still reads the source's archive after recovery finished")
	}
}

// With the daemon down the socket does not answer: push fails (Postgres retries it) and
// nothing is buffered anywhere else.
func TestRelayDownFailsClosed(t *testing.T) {
	re := newRelayEnv(t, testRef)
	sock := re.sock(testRef)
	re.relay.Close()
	src := writeWAL(t, walA, 1<<10, 1)
	err := RelayPush(context.Background(), sock, testRef, src)
	if !errors.Is(err, ErrRelayDown) {
		t.Fatalf("push with no relay = %v; want ErrRelayDown", err)
	}
	if err := RelayFetch(context.Background(), sock, testRef, walA, filepath.Join(t.TempDir(), "x")); !errors.Is(err, ErrRelayDown) {
		t.Fatalf("fetch with no relay = %v; want ErrRelayDown (a fatal exit: not the end of the archive)", err)
	}
	if left, _ := re.store.ListDirs(context.Background(), testRef+"/"); len(left) > 0 {
		t.Fatalf("the failed push left %v in the archive", left)
	}
}

// A backend that cannot be read must never look like "not in the archive".
func TestRelayBackendFailureIsNotEndOfArchive(t *testing.T) {
	e := newTestEnv(t)
	dir, _ := os.MkdirTemp("/tmp", "sbr")
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := NewRelay(RelayOptions{
		Config:  e.cfg,
		Service: func(context.Context) (*Service, error) { return nil, errors.New("bucket unreachable") },
		Refs:    func() []string { return []string{testRef} },
		Socket:  func(string) string { return filepath.Join(dir, "a.sock") },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(r.Close)
	r.Reconcile()
	err := RelayFetch(context.Background(), filepath.Join(dir, "a.sock"), testRef, walA, filepath.Join(dir, "x"))
	if err == nil || errors.Is(err, ErrNoWAL) {
		t.Fatalf("unreadable backend = %v; want an error that is not ErrNoWAL", err)
	}
	if err := RelayPush(context.Background(), filepath.Join(dir, "a.sock"), testRef, writeWAL(t, walA, 100, 1)); err == nil {
		t.Fatal("push succeeded with an unreachable backend")
	}
}

// A client that stops halfway (the postmaster was killed) must not leave a short segment.
func TestRelayTruncatedPushLeavesNothing(t *testing.T) {
	re := newRelayEnv(t, testRef)
	conn, err := net.Dial("unix", re.sock(testRef))
	if err != nil {
		t.Fatal(err)
	}
	// Declares 1 MiB, sends 100 bytes and hangs up.
	io.WriteString(conn, "POST "+relayPushPath+"?name="+walA+" HTTP/1.1\r\nHost: relay\r\nContent-Length: 1048576\r\n\r\n")
	conn.Write(make([]byte, 100))
	conn.Close()
	time.Sleep(300 * time.Millisecond)
	if _, err := re.store.Stat(context.Background(), walKey(testRef, walA)); err == nil {
		t.Fatal("a truncated push produced an archived WAL file")
	}
}

func TestRelayServesManyPushesAtOnce(t *testing.T) {
	re := newRelayEnv(t, testRef)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		name := "0000000100000000000000" + strings.ToUpper(string("0123456789ABCDEF"[i])) + "0"
		p := writeWAL(t, name, 1<<20, byte(i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- RelayPush(context.Background(), re.sock(testRef), testRef, p)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRelayReconcileFollowsTheProjects(t *testing.T) {
	e := newTestEnv(t)
	dir, _ := os.MkdirTemp("/tmp", "sbr")
	t.Cleanup(func() { os.RemoveAll(dir) })
	var mu sync.Mutex
	refs := []string{testRef}
	r := NewRelay(RelayOptions{
		Config:  e.cfg,
		Service: func(context.Context) (*Service, error) { return e.svc, nil },
		Refs:    func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), refs...) },
		Socket:  func(ref string) string { return filepath.Join(dir, ref[:6]+".sock") },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(r.Close)
	r.Reconcile()
	if got := r.Served(); len(got) != 1 || got[0] != testRef {
		t.Fatalf("served = %v", got)
	}
	mu.Lock()
	refs = []string{testRef2}
	mu.Unlock()
	r.Reconcile()
	if got := r.Served(); len(got) != 1 || got[0] != testRef2 {
		t.Fatalf("served after the project list changed = %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, testRef[:6]+".sock")); err == nil {
		t.Fatal("the socket of a project that is gone is still there")
	}
	// A socket file that disappears (the directory was recreated) is served again.
	os.Remove(filepath.Join(dir, testRef2[:6]+".sock"))
	r.Reconcile()
	if err := RelayPing(context.Background(), filepath.Join(dir, testRef2[:6]+".sock")); err != nil {
		t.Fatalf("relay did not come back after its socket was removed: %v", err)
	}
}

func TestRelaySkipServedLeavesAnotherRelaysSocketAlone(t *testing.T) {
	re := newRelayEnv(t, testRef)
	other := NewRelay(RelayOptions{
		Config: re.cfg, SkipServed: true,
		Service: func(context.Context) (*Service, error) { return re.svc, nil },
		Refs:    func() []string { return []string{testRef} },
		Socket:  func(ref string) string { return re.sock(ref) },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(other.Close)
	other.Reconcile()
	if got := other.Served(); len(got) != 0 {
		t.Fatalf("a relay that skips served projects took over %v", got)
	}
	if err := RelayPing(context.Background(), re.sock(testRef)); err != nil {
		t.Fatalf("the first relay stopped answering: %v", err)
	}
}

func TestRelayCommandsCarrySocketAndNoConfig(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.Backup.WALRelay = "on"
	got := ArchiveCommandFor(e.cfg, testRef, "/etc/sbctl/other.toml")
	want := "/usr/local/bin/sbctl wal push --ref " + testRef + " --socket " + e.cfg.Paths().WALSocket(testRef) + " %p"
	if got != want {
		t.Fatalf("archive_command = %q\nwant %q", got, want)
	}
	if strings.Contains(got, "--config") {
		t.Fatal("the relay form must not name a config file: the unit cannot read it")
	}
	rc := RestoreCommandFor(e.cfg, testRef2, testRef, "")
	wantRC := "/usr/local/bin/sbctl wal fetch --ref " + testRef2 + " --socket " + e.cfg.Paths().WALSocket(testRef) + " %f %p"
	if rc != wantRC {
		t.Fatalf("restore_command = %q\nwant %q", rc, wantRC)
	}
	e.cfg.Backup.WALRelay = "off"
	if got := ArchiveCommandFor(e.cfg, testRef, "/etc/sbctl/other.toml"); !strings.Contains(got, "--config /etc/sbctl/other.toml") || strings.Contains(got, "--socket") {
		t.Fatalf("direct form = %q", got)
	}
}

func TestWALRelayAutoFollowsTheSupervisor(t *testing.T) {
	e := newTestEnv(t)
	e.cfg.Backup.WALRelay = ""
	e.cfg.Supervisor = "systemd"
	if !e.cfg.WALRelayEnabled() {
		t.Fatal("auto must be on under systemd")
	}
	e.cfg.Supervisor = "exec"
	if e.cfg.WALRelayEnabled() {
		t.Fatal("auto must be off under the exec backend")
	}
}
