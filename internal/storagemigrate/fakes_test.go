package storagemigrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
)

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
)

// memBucket is a Bucket in memory.
type memBucket struct {
	mu    sync.Mutex
	objs  map[string]memObj
	puts  []string
	dels  []string
	onPut func(key string)
	fail  func(op, key string) error
	lim   *limiter
}

// pace implements pacer.
func (b *memBucket) pace(l *limiter) { b.lim = l }

type memObj struct {
	data []byte
	meta FileMeta
	mod  time.Time
}

func newMemBucket() *memBucket { return &memBucket{objs: map[string]memObj{}} }

func (b *memBucket) Put(ctx context.Context, key string, src io.ReaderAt, size int64, meta FileMeta) error {
	if b.onPut != nil {
		b.onPut(key)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		if err := b.fail("put", key); err != nil {
			return err
		}
	}
	data := make([]byte, size)
	if _, err := src.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	b.objs[key] = memObj{data: data, meta: meta, mod: time.Now()}
	b.puts = append(b.puts, key)
	return nil
}

func (b *memBucket) List(ctx context.Context, prefix string, fn func(Entry) error) error {
	b.mu.Lock()
	var keys []string
	for k := range b.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	// Deliberately not sorted: the engine must not rely on the service's order.
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var ents []Entry
	for _, k := range keys {
		o := b.objs[k]
		ents = append(ents, Entry{Key: k, Size: int64(len(o.data)), ModTime: o.mod})
	}
	b.mu.Unlock()
	for _, en := range ents {
		if err := fn(en); err != nil {
			return err
		}
	}
	return nil
}

func (b *memBucket) Get(ctx context.Context, key string) (io.ReadCloser, FileMeta, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objs[key]
	if !ok {
		return nil, FileMeta{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), o.meta, nil
}

func (b *memBucket) Delete(ctx context.Context, keys ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range keys {
		delete(b.objs, k)
		b.dels = append(b.dels, k)
	}
	return nil
}

func (b *memBucket) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k := range b.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (b *memBucket) clear() { b.mu.Lock(); b.puts, b.dels = nil, nil; b.mu.Unlock() }

// fakeTenants serves rows from memory.
type fakeTenants struct {
	mu      sync.Mutex
	rows    map[string][]Row
	offline map[string]bool
	failOn  func(ref string) error
}

func (f *fakeTenants) Projects(ctx context.Context) ([]Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Project
	for ref := range f.rows {
		out = append(out, Project{Ref: ref, Online: !f.offline[ref]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (f *fakeTenants) Rows(ctx context.Context, ref string, fn func(Row) error) error {
	f.mu.Lock()
	rows := append([]Row(nil), f.rows[ref]...)
	off, fail := f.offline[ref], f.failOn
	f.mu.Unlock()
	if off {
		return ErrOffline
	}
	if fail != nil {
		if err := fail(ref); err != nil {
			return err
		}
	}
	for _, r := range rows {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// fakeService records what the switch does to supavise-storage.
type fakeService struct {
	mu       sync.Mutex
	calls    []string
	health   error
	stopErr  error
	startErr error
	onStop   func()
	onStart  func()

	renderErr error
	renders   int
}

func (s *fakeService) record(c string) { s.mu.Lock(); s.calls = append(s.calls, c); s.mu.Unlock() }

func (s *fakeService) Healthy(context.Context) error { return s.health }

func (s *fakeService) Stop(context.Context) error {
	s.record("stop")
	if s.onStop != nil {
		s.onStop()
	}
	return s.stopErr
}

func (s *fakeService) Start(context.Context) error {
	s.record("start")
	if s.onStart != nil {
		s.onStart()
	}
	if s.startErr != nil {
		err := s.startErr
		return err
	}
	return nil
}

func (s *fakeService) Render(_ context.Context, cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renders++
	return s.renderErr
}

func (s *fakeService) setRenderErr(err error) { s.mu.Lock(); s.renderErr = err; s.mu.Unlock() }

func (s *fakeService) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.calls, ",")
}

// fakeSettings keeps the backend the configuration names.
type fakeSettings struct {
	backend   string
	prev      string
	wrote     bool
	useBucket int
	useFiles  int
	err       error
	filesErr  error // what UseFiles returns
	dest      Destination
	creds     Credentials

	previewErr error
	previews   int
}

func (s *fakeSettings) Preview(_ context.Context, d Destination, c Credentials) (*config.Config, error) {
	s.previews++
	if s.previewErr != nil {
		return nil, s.previewErr
	}
	cfg := config.Default()
	cfg.Fleet.StorageBackend, cfg.Fleet.StorageS3Bucket = "s3", d.Bucket
	return cfg, nil
}

func (s *fakeSettings) UseBucket(_ context.Context, d Destination, c Credentials) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.useBucket++
	s.backend, s.dest, s.creds = "s3", d, c
	s.wrote = c.Source == CredFile || c.Source == CredRole
	return s.wrote, nil
}

func (s *fakeSettings) UseFiles(_ context.Context, previous string, remove bool) error {
	s.useFiles++
	if s.filesErr != nil {
		return s.filesErr
	}
	s.backend, s.prev = previous, previous
	if remove {
		s.wrote = false
	}
	return nil
}

// fakeReader returns the size the row records, unless told otherwise.
type fakeReader struct {
	mu    sync.Mutex
	reads []string
	err   error
	size  int64
}

func (r *fakeReader) Read(_ context.Context, ref string, row Row) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, ref+"/"+rowPath(row))
	if r.err != nil {
		return 0, r.err
	}
	if r.size != 0 {
		return r.size, nil
	}
	return row.Size, nil
}

// metaFiles is OSFiles with the extended attributes kept in memory, so that the tests do not depend
// on the file system under t.TempDir.
type metaFiles struct {
	OSFiles
	mu   sync.Mutex
	meta map[string]FileMeta
}

func (m *metaFiles) Meta(abs string) (FileMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.meta[abs], nil
}

func (m *metaFiles) MetaOf(f *os.File) (FileMeta, error) { return m.Meta(f.Name()) }

func (m *metaFiles) SetMeta(abs string, fm FileMeta) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meta[abs] = fm
	return true, nil
}

// env is a node: a state directory, a bucket, and the fakes around it.
type env struct {
	t       *testing.T
	cfg     *config.Config
	paths   config.Paths
	bucket  *memBucket
	tenants *fakeTenants
	svc     *fakeService
	set     *fakeSettings
	rd      *fakeReader
	files   Files
	meta    *metaFiles
	out     *bytes.Buffer
	opens   int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Fleet.StorageS3Bucket = "objects"
	cfg.Fleet.StorageS3Endpoint = "http://s3.test"
	e := &env{t: t, cfg: cfg, paths: cfg.Paths(), bucket: newMemBucket(),
		tenants: &fakeTenants{rows: map[string][]Row{}, offline: map[string]bool{}},
		svc:     &fakeService{}, set: &fakeSettings{backend: ""}, rd: &fakeReader{},
		out: &bytes.Buffer{}}
	e.meta = &metaFiles{meta: map[string]FileMeta{}}
	e.files = e.meta
	if err := os.MkdirAll(e.paths.System(config.SvcStorage), 0o750); err != nil {
		t.Fatal(err)
	}
	return e
}

// engine is a new Engine over the node, as a new process would build it.
func (e *env) engine() *Engine {
	return New(Deps{
		Cfg: e.cfg, Out: e.out, Files: e.files, Tenants: e.tenants, Storage: e.svc, Settings: e.set, Reader: e.rd,
		Open:    func(context.Context, Destination, Credentials) (Bucket, error) { e.opens++; return e.bucket, nil },
		Workers: 2, VerifyWait: time.Millisecond, HoldFor: 200 * time.Millisecond,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
}

func (e *env) req() Request {
	return Request{Credentials: Credentials{Source: CredFile, AccessKeyID: "AK", SecretAccessKey: "SK"}, RateMiB: -1}
}

// put writes an object the way Storage's file backend does and records its row.
func (e *env) put(ref, bucketID, name, version string, data []byte, ct string) {
	e.t.Helper()
	e.write(ref, bucketID+"/"+name+"/"+version, data, ct)
	e.tenants.mu.Lock()
	e.tenants.rows[ref] = append(e.tenants.rows[ref], Row{Bucket: bucketID, Name: name, Version: version, Size: int64(len(data)), HasSize: true})
	e.tenants.mu.Unlock()
}

// write creates a file below the project's directory without a row.
func (e *env) write(ref, rel string, data []byte, ct string) {
	e.t.Helper()
	abs := filepath.Join(e.paths.StorageObjects(ref), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(abs, data, 0o640); err != nil {
		e.t.Fatal(err)
	}
	// Written an hour ago, so that a copy made now is plainly newer than the file.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(abs, old, old); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.files.SetMeta(abs, FileMeta{ContentType: ct, CacheControl: "max-age=3600"}); err != nil {
		e.t.Fatal(err)
	}
}

// remove deletes the file and its row.
func (e *env) remove(ref, bucketID, name, version string) {
	e.t.Helper()
	abs := filepath.Join(e.paths.StorageObjects(ref), bucketID, filepath.FromSlash(name), version)
	if err := os.Remove(abs); err != nil {
		e.t.Fatal(err)
	}
	e.tenants.mu.Lock()
	defer e.tenants.mu.Unlock()
	rows := e.tenants.rows[ref][:0:0]
	for _, r := range e.tenants.rows[ref] {
		if !(r.Bucket == bucketID && r.Name == name && r.Version == version) {
			rows = append(rows, r)
		}
	}
	e.tenants.rows[ref] = rows
}

// populate stores a few objects of different sizes in two projects.
func (e *env) populate() {
	e.put(refA, "avatars", "top.txt", "v1", []byte("top level object\n"), "text/plain")
	e.put(refA, "avatars", "dir/sub/nested.bin", "v2", bytes.Repeat([]byte{7}, 300_000), "application/octet-stream")
	e.put(refA, "docs", "empty", "v3", nil, "")
	e.put(refB, "pics", "sp ace/ü-é.txt", "v4", []byte("space and unicode\n"), "text/plain")
	e.put(refB, "pics", "a+b=c.txt", "v5", []byte("plus and equals\n"), "text/plain")
}

// realAttrs switches the node to extended attributes on the real file system and reports whether
// it has them. Without them the in-memory ones stay, which cannot follow a rename.
func (e *env) realAttrs() bool {
	probe := filepath.Join(e.t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if ok, _ := (OSFiles{}).SetMeta(probe, FileMeta{ContentType: "x"}); !ok {
		return false
	}
	e.files = OSFiles{}
	return true
}

func (e *env) keyList() []string { return e.bucket.keys() }

func (e *env) state() *State {
	e.t.Helper()
	st, err := ReadState(e.paths)
	if err != nil || st == nil {
		e.t.Fatalf("state: %v, %v", st, err)
	}
	return st
}

// exists says whether rel is below Storage's own directory.
func (e *env) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(e.paths.System(config.SvcStorage), rel))
	return err == nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func mustf(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}
