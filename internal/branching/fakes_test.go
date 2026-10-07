package branching

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

const parentRef = "pppppppppppppppppppp"

// fakeEngine stands in for lifecycle.Engine: it keeps the registry rows and the keys and
// records what the service asked of it.
type fakeEngine struct {
	reg registry.Registry
	sec secrets.Secrets

	mu      sync.Mutex
	keys    map[string]*secrets.ProjectKeys
	creates []lifecycle.CreateRequest
	deletes []string
	skipped map[string]bool // ref -> SkipFinalBackup of the last delete
	// keptRecord marks refs whose last delete kept the registry row.
	keptRecord map[string]bool
	rotated    []string
	paused     map[string]int

	createErr error
	// keysFailAfter, when > 0, makes Keys(parentRef) fail after that many successful calls.
	keysFailAfter int
	keyCalls      int
	// failBeforeRow makes Create fail before it writes or touches the registry row.
	failBeforeRow error
	hold          chan struct{} // when set, Create waits for it to close (after the row exists)
}

func newFakeEngine(reg registry.Registry, sec secrets.Secrets) *fakeEngine {
	return &fakeEngine{reg: reg, sec: sec, keys: map[string]*secrets.ProjectKeys{}, skipped: map[string]bool{}, keptRecord: map[string]bool{}}
}

func (f *fakeEngine) Create(ctx context.Context, req lifecycle.CreateRequest) (*registry.Project, error) {
	f.mu.Lock()
	f.creates = append(f.creates, req)
	f.mu.Unlock()
	if f.failBeforeRow != nil {
		return nil, f.failBeforeRow
	}
	p := &registry.Project{Ref: req.Ref, Name: req.Name, Class: req.Class, Status: registry.StatusComingUp}
	if req.Branch != nil {
		b := *req.Branch
		p.Branch = &b
	}
	if req.Recreate {
		cur, err := f.reg.GetProject(ctx, req.Ref)
		if err != nil {
			return nil, err
		}
		if cur.Status != registry.StatusInitFailed {
			return nil, lifecycle.ErrInvalidState
		}
		if err := f.reg.SetProjectStatus(ctx, req.Ref, registry.StatusComingUp); err != nil {
			return nil, err
		}
	} else if err := f.reg.CreateProject(ctx, p); err != nil {
		return nil, err
	}
	if f.hold != nil {
		<-f.hold
	}
	if f.createErr != nil {
		_ = f.reg.SetProjectStatus(ctx, req.Ref, registry.StatusInitFailed)
		return nil, f.createErr
	}
	k := req.Keys
	if k == nil {
		var err error
		if k, err = secrets.NewProjectKeys(req.Ref, time.Now()); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.keys[req.Ref] = k
	f.mu.Unlock()
	if err := f.reg.SetProjectStatus(ctx, req.Ref, registry.StatusActiveHealthy); err != nil {
		return nil, err
	}
	return f.reg.GetProject(ctx, req.Ref)
}

func (f *fakeEngine) DeleteWith(ctx context.Context, ref string, o lifecycle.DeleteOptions) error {
	f.mu.Lock()
	f.deletes = append(f.deletes, ref)
	f.skipped[ref] = o.SkipFinalBackup
	f.keptRecord[ref] = o.KeepRecord
	f.mu.Unlock()
	if o.KeepRecord {
		return f.reg.SetProjectStatus(ctx, ref, registry.StatusInitFailed)
	}
	return f.reg.DeleteProject(ctx, ref)
}

func (f *fakeEngine) Delete(ctx context.Context, ref string) error {
	return f.DeleteWith(ctx, ref, lifecycle.DeleteOptions{})
}
func (f *fakeEngine) Pause(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paused == nil {
		f.paused = map[string]int{}
	}
	f.paused[ref]++
	return nil
}
func (f *fakeEngine) Resume(context.Context, string) error { return nil }
func (f *fakeEngine) RotateKeys(_ context.Context, ref string) (*secrets.ProjectKeys, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotated = append(f.rotated, ref)
	return f.keys[ref], nil
}
func (f *fakeEngine) Keys(_ context.Context, ref string) (*secrets.ProjectKeys, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == parentRef && f.keysFailAfter > 0 {
		if f.keyCalls++; f.keyCalls > f.keysFailAfter {
			return nil, errors.New("secret store unavailable")
		}
	}
	k, ok := f.keys[ref]
	if !ok {
		return nil, registry.ErrNotFound
	}
	c := *k
	return &c, nil
}
func (f *fakeEngine) Health(context.Context, string) ([]lifecycle.ServiceHealth, error) {
	return nil, nil
}
func (f *fakeEngine) ConnString(_ context.Context, ref, role string) (string, error) {
	return "postgres://" + role + "@127.0.0.1/" + ref, nil
}

// fakeDB keeps a migration history and the scripts run per project.
type fakeDB struct {
	mu       sync.Mutex
	migs     map[string][]Migration
	applied  map[string][]string // ref -> versions in apply order
	scripts  map[string][]string
	applyErr map[string]error
	atomic   map[string]bool
	readErr  map[string]error
	// raceOnApply, when set, runs at the start of Apply, standing for another operation that
	// lands while this one waits for the apply lock.
	raceOnApply func(d *fakeDB, ref string)
}

func newFakeDB() *fakeDB {
	return &fakeDB{migs: map[string][]Migration{}, applied: map[string][]string{}, scripts: map[string][]string{},
		applyErr: map[string]error{}, atomic: map[string]bool{}, readErr: map[string]error{}}
}

func (d *fakeDB) Migrations(_ context.Context, ref string) ([]Migration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.readErr[ref]; err != nil {
		return nil, err
	}
	return append([]Migration(nil), d.migs[ref]...), nil
}

func (d *fakeDB) Apply(_ context.Context, ref string, ms []Migration, o ApplyOptions) (ApplyResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.applyErr[ref]; err != nil {
		return ApplyResult{}, err
	}
	if err := d.applyErr["*"]; err != nil {
		return ApplyResult{}, err
	}
	if d.raceOnApply != nil {
		d.raceOnApply(d, ref)
	}
	if o.Atomic {
		if err := recheck(ref, ms, d.migs[ref], o.Verify); err != nil {
			return ApplyResult{}, err
		}
	}
	d.atomic[ref] = o.Atomic
	for _, m := range ms {
		d.migs[ref] = append(d.migs[ref], m)
		d.applied[ref] = append(d.applied[ref], m.Version)
	}
	return ApplyResult{Applied: len(ms)}, nil
}

func (d *fakeDB) RunScript(_ context.Context, ref, script string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if script == "FAIL" {
		return errors.New("seed failed")
	}
	d.scripts[ref] = append(d.scripts[ref], script)
	return nil
}

func mig(v, name string, stmts ...string) Migration {
	return Migration{Version: v, Name: name, Statements: stmts}
}

type harness struct {
	t   *testing.T
	svc *Service
	reg *registry.Memory
	eng *fakeEngine
	db  *fakeDB
	cfg *config.Config
	now time.Time
	mu  sync.Mutex
	// isolated lists the branches the service isolated from the parent's integrations.
	isolated   []string
	isolateErr error
	// rewritten lists the branches whose data had the parent's credentials replaced.
	rewritten  []string
	rewriteErr error
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// newHarness builds a service over a memory registry with an active parent project that
// has three migrations.
func newHarness(t *testing.T, mut func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "supavise.test"
	if mut != nil {
		mut(cfg)
	}
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.NewMemory()
	h := &harness{t: t, reg: reg, cfg: cfg, now: time.Now().UTC().Truncate(time.Second)}
	h.eng = newFakeEngine(reg, sec)
	h.db = newFakeDB()
	org, _ := reg.CreateOrganization(context.Background(), "default", "Default")
	if err := reg.CreateProject(context.Background(), &registry.Project{Ref: parentRef, OrgID: org.ID, Name: "parent", Class: "default", Status: registry.StatusActiveHealthy}); err != nil {
		t.Fatal(err)
	}
	pk, _ := secrets.NewProjectKeys(parentRef, time.Now())
	h.eng.keys[parentRef] = pk
	h.db.migs[parentRef] = []Migration{
		mig("20260101000000", "init", "create table t (id int)"),
		mig("20260102000000", "add_name", "alter table t add column name text"),
		mig("20260103000000", "index", "create index on t (name)"),
	}
	svc, err := New(Deps{Cfg: cfg, Registry: reg, Secrets: sec, Engine: h.eng, DB: h.db, Now: h.clock, CreateWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	svc.rotate = func(_ context.Context, ref string) error {
		_, err := h.eng.RotateKeys(context.Background(), ref)
		return err
	}
	// The test disk is never the limit unless a test says so.
	svc.freeBytes = func(string) int64 { return 1 << 50 }
	svc.isolate = func(_ context.Context, ref string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.isolated = append(h.isolated, ref)
		return h.isolateErr
	}
	svc.rewrite = func(_ context.Context, ref string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.rewritten = append(h.rewritten, ref)
		return h.rewriteErr
	}
	h.svc = svc
	t.Cleanup(func() { svc.Drain(context.Background()) })
	return h
}

// create makes a schema-only branch and waits for it.
func (h *harness) create(name string, mut func(*CreateInput)) *Branch {
	h.t.Helper()
	in := CreateInput{Name: name}
	if mut != nil {
		mut(&in)
	}
	b, err := h.svc.Create(context.Background(), parentRef, in)
	if err != nil {
		h.t.Fatalf("create %s: %v", name, err)
	}
	return h.wait(b.Ref)
}

func (h *harness) wait(ref string) *Branch {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := h.svc.WaitIdle(ctx, ref)
	if err != nil {
		h.t.Fatalf("wait %s: %v", ref, err)
	}
	return b
}

func (h *harness) mustState(b *Branch, want registry.BranchState) {
	h.t.Helper()
	if b.State != want {
		h.t.Fatalf("branch %s state = %s (%s), want %s", b.Name, b.State, b.Detail, want)
	}
}

var _ = fmt.Sprint

func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }
