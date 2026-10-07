// Package branching gives AI agents disposable databases: a branch is a project with a
// parent, so it has its own cluster, keys, host and fleet tenants (DESIGN.md section 9a).
//
// The Management API's branch endpoints (internal/api) are thin wrappers over Service.
// Long operations (create, merge, reset, push) run detached from the request that started
// them; their progress is the branch's State and Detail, visible through Get and List.
package branching

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// Engine is the part of lifecycle.Engine the service drives. *lifecycle.Engine satisfies it.
type Engine interface {
	lifecycle.Manager
	DeleteWith(ctx context.Context, ref string, o lifecycle.DeleteOptions) error
}

// Deps are the collaborators of Service.
type Deps struct {
	Cfg      *config.Config
	Registry registry.Registry
	Secrets  secrets.Secrets
	Engine   Engine
	// Backup configures the base-backup path of with_data branches and the cleanup of an
	// ephemeral branch's archive. Manager is filled in by the service. Nil: neither exists,
	// and with_data needs a filesystem that supports copy-on-write clones.
	Backup *backup.Options
	// DB runs SQL in project databases. Nil connects with pgx through Engine.ConnString.
	DB         Database
	Log        *slog.Logger
	Now        func() time.Time
	HTTPClient *http.Client // notify_url calls; nil builds a client that refuses private addresses
	// CreateWait bounds how long Create waits for the branch's row to exist before it
	// returns the branch in CREATING_PROJECT (default 10 seconds).
	CreateWait time.Duration
	// OpTimeout bounds a detached operation (default 1 hour).
	OpTimeout time.Duration
	// StaleAfter is how long a branch may sit in a busy state with no process working on it
	// before the sweeper marks the operation failed (default 30 minutes).
	StaleAfter time.Duration
}

// Service implements branching.
type Service struct {
	cfg  *config.Config
	reg  registry.Registry
	sec  secrets.Secrets
	eng  Engine
	bk   *backup.Options
	db   Database
	log  *slog.Logger
	now  func() time.Time
	http *http.Client

	createWait, opTimeout, staleAfter time.Duration

	// Seams for tests.
	detect func(srcData, dstParent string) (method, fsys, reason string)
	zfs    zfsRunner
	rotate func(ctx context.Context, ref string) error
	clone  func(ctx context.Context, parentRef, method, dstData string) (*CloneStats, error)

	mu   sync.Mutex
	runs map[string]*run // by branch ref
	wg   sync.WaitGroup
	base context.Context
	stop context.CancelFunc
}

// run is an operation in progress in this process.
type run struct {
	id, op string
	done   chan struct{}
}

// New builds the service. It connects to nothing.
func New(d Deps) (*Service, error) {
	if d.Cfg == nil || d.Registry == nil || d.Secrets == nil || d.Engine == nil {
		return nil, errors.New("branching: Deps needs Cfg, Registry, Secrets and Engine")
	}
	if _, _, err := d.Cfg.Branching.TTL(); err != nil {
		return nil, err
	}
	s := &Service{
		cfg: d.Cfg, reg: d.Registry, sec: d.Secrets, eng: d.Engine, bk: d.Backup, db: d.DB, log: d.Log, now: d.Now,
		http: d.HTTPClient, createWait: d.CreateWait, opTimeout: d.OpTimeout, staleAfter: d.StaleAfter,
		detect: nil, zfs: execZFS{}, runs: map[string]*run{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.createWait <= 0 {
		s.createWait = 10 * time.Second
	}
	if s.opTimeout <= 0 {
		s.opTimeout = time.Hour
	}
	if s.staleAfter <= 0 {
		s.staleAfter = 30 * time.Minute
	}
	if s.db == nil {
		s.db = &pgDatabase{dsn: func(ctx context.Context, ref string) (string, error) {
			return s.eng.ConnString(ctx, ref, lifecycle.RolePostgres)
		}}
	}
	if s.http == nil {
		s.http = newNotifyClient(d.Cfg.Branching.AllowPrivateNotifyURLs)
	}
	s.detect = func(srcData, dstParent string) (string, string, string) {
		return detectClone(srcData, dstParent, s.zfs)
	}
	s.base, s.stop = context.WithCancel(context.Background())
	return s, nil
}

// Drain waits for detached operations to finish (a daemon calls it on shutdown, after it
// stops accepting requests). When ctx ends first the operations are cancelled.
func (s *Service) Drain(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.stop()
		<-done
	}
}

// ---- reading -------------------------------------------------------------------------

func (s *Service) project(ctx context.Context, ref string) (*registry.Project, error) {
	p, err := s.reg.GetProject(ctx, ref)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, fmt.Errorf("%w: project %s", ErrNotFound, ref)
	}
	return p, err
}

// Parent resolves a project ref to the project whose branches are meant: the project
// itself, or the parent when ref is itself a branch (a client linked to a branch asks
// that branch's project for its branches and gets the whole family).
func (s *Service) Parent(ctx context.Context, ref string) (*registry.Project, error) {
	if ref == config.SystemRef {
		return nil, invalid("the system project has no branches")
	}
	p, err := s.project(ctx, ref)
	if err != nil {
		return nil, err
	}
	if p.Ref == config.SystemRef {
		return nil, invalid("the system project has no branches")
	}
	if p.Branch != nil {
		return s.project(ctx, p.Branch.ParentRef)
	}
	return p, nil
}

// List returns the branches of the project ref belongs to: the default branch (the
// project itself) first, then the others by creation time.
func (s *Service) List(ctx context.Context, ref string) ([]Branch, error) {
	parent, err := s.Parent(ctx, ref)
	if err != nil {
		return nil, err
	}
	kids, err := s.children(ctx, parent.Ref)
	if err != nil {
		return nil, err
	}
	out := []Branch{*defaultBranchOf(parent)}
	for i := range kids {
		out = append(out, *branchOf(&kids[i]))
	}
	return out, nil
}

func (s *Service) children(ctx context.Context, parentRef string) ([]registry.Project, error) {
	all, err := s.reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var kids []registry.Project
	for _, p := range all {
		if p.Branch != nil && p.Branch.ParentRef == parentRef {
			kids = append(kids, p)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].CreatedAt.Before(kids[j].CreatedAt) })
	return kids, nil
}

// Children returns the refs of the branches of parentRef (not the default branch).
func (s *Service) Children(ctx context.Context, parentRef string) ([]string, error) {
	kids, err := s.children(ctx, parentRef)
	if err != nil {
		return nil, err
	}
	refs := make([]string, len(kids))
	for i, k := range kids {
		refs[i] = k.Ref
	}
	return refs, nil
}

// Get returns the branch of ref's family called name.
func (s *Service) Get(ctx context.Context, ref, name string) (*Branch, error) {
	parent, err := s.Parent(ctx, ref)
	if err != nil {
		return nil, err
	}
	if name == DefaultBranchName {
		return defaultBranchOf(parent), nil
	}
	kids, err := s.children(ctx, parent.Ref)
	if err != nil {
		return nil, err
	}
	for i := range kids {
		if kids[i].Branch.Name == name {
			return branchOf(&kids[i]), nil
		}
	}
	return nil, fmt.Errorf("%w: %q in project %s", ErrNotFound, name, parent.Ref)
}

// Resolve finds a branch by its UUID or by its project ref. A ref of a project that is
// not a branch resolves to that project's default branch.
func (s *Service) Resolve(ctx context.Context, idOrRef string) (*Branch, error) {
	if IsUUID(idOrRef) {
		all, err := s.reg.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		for i := range all {
			if all[i].Branch != nil && strings.EqualFold(all[i].Branch.ID, idOrRef) {
				return branchOf(&all[i]), nil
			}
			if all[i].Branch == nil && all[i].Ref != config.SystemRef && strings.EqualFold(defaultBranchID(all[i].Ref), idOrRef) {
				return defaultBranchOf(&all[i]), nil
			}
		}
		return nil, fmt.Errorf("%w: id %s", ErrNotFound, idOrRef)
	}
	p, err := s.project(ctx, idOrRef)
	if err != nil {
		return nil, err
	}
	if p.Ref == config.SystemRef {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, idOrRef)
	}
	if p.Branch == nil {
		return defaultBranchOf(p), nil
	}
	return branchOf(p), nil
}

// resolveBranch is Resolve for operations that need a real branch, not the default one.
func (s *Service) resolveBranch(ctx context.Context, idOrRef, op string) (*Branch, error) {
	b, err := s.Resolve(ctx, idOrRef)
	if err != nil {
		return nil, err
	}
	if b.IsDefault {
		return nil, invalid("cannot %s the default branch of project %s", op, b.Ref)
	}
	return b, nil
}

func busy(st registry.BranchState) bool {
	return st == registry.BranchCreatingProject || st == registry.BranchRunningMigration
}

// ---- operation tracking ----------------------------------------------------------------

// begin registers an operation on ref; it fails when another runs here.
func (s *Service) begin(ref, op string) (*run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[ref]; ok {
		return nil, conflict("branch %s is busy with %s", ref, r.op)
	}
	r := &run{id: newUUID(), op: op, done: make(chan struct{})}
	s.runs[ref] = r
	return r, nil
}

func (s *Service) end(ref string, r *run) {
	s.mu.Lock()
	if s.runs[ref] == r {
		delete(s.runs, ref)
	}
	s.mu.Unlock()
	close(r.done)
}

// running reports the operation in progress on ref in this process.
func (s *Service) running(ref string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[ref]; ok {
		return r.op, true
	}
	return "", false
}

// checkIdle refuses an operation on a branch that is busy, here or (by its registry
// state) in another process.
func (s *Service) checkIdle(b *Branch) error {
	if op, ok := s.running(b.Ref); ok {
		return conflict("branch %s is busy with %s", b.Name, op)
	}
	if busy(b.State) && s.now().Sub(b.UpdatedAt) < s.staleAfter {
		return conflict("branch %s is busy (%s)", b.Name, b.State)
	}
	return nil
}

// WaitIdle blocks until no operation runs on ref (here, or by registry state elsewhere).
func (s *Service) WaitIdle(ctx context.Context, ref string) (*Branch, error) {
	for {
		s.mu.Lock()
		r := s.runs[ref]
		s.mu.Unlock()
		if r != nil {
			select {
			case <-r.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		b, err := s.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !busy(b.State) || s.now().Sub(b.UpdatedAt) >= s.staleAfter {
			return b, nil
		}
		if r == nil {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
}

// setState records the state and detail of the branch project ref. A branch that is gone
// (deleted while the operation ran) is not an error.
func (s *Service) setState(ctx context.Context, ref string, st registry.BranchState, detail string, mutate func(*registry.BranchInfo)) {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil || p.Branch == nil {
		return
	}
	b := *p.Branch
	b.State, b.Detail = st, detail
	if mutate != nil {
		mutate(&b)
	}
	if err := s.reg.UpdateBranch(ctx, ref, &b); err != nil && !errors.Is(err, registry.ErrNotFound) {
		s.log.Warn("could not record the branch state", "ref", ref, "state", st, "err", err)
	}
}

func (s *Service) event(ctx context.Context, ref, kind string, payload any) {
	if err := s.reg.AppendEvent(ctx, ref, kind, payload); err != nil {
		s.log.Warn("could not record an event", "ref", ref, "kind", kind, "err", err)
	}
}

// spawn runs fn detached from the caller. It records started/succeeded/failed events on the
// branch and its parent, sets the branch's State and Detail from fn's outcome, and tells
// notify_url. It returns the run id (the API's workflow_run_id). A caller that already
// holds the run (Create) passes it in.
func (s *Service) spawn(r *run, ref, parentRef string, fn func(ctx context.Context) (string, error)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.end(ref, r)
		ctx, cancel := context.WithTimeout(s.base, s.opTimeout)
		defer cancel()
		started := s.now()
		payload := map[string]any{"run_id": r.id, "operation": r.op, "branch": ref, "parent": parentRef}
		s.event(ctx, ref, "branch."+r.op+".started", payload)
		detail, err := fn(ctx)
		// Recording the outcome must survive a cancelled operation context.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rcancel()
		payload["ms"] = s.now().Sub(started).Milliseconds()
		if err != nil {
			payload["error"] = err.Error()
			s.log.Error("branch operation failed", "op", r.op, "ref", ref, "run", r.id, "err", err)
			s.setState(rctx, ref, registry.BranchMigrationsFailed, err.Error(), nil)
			s.event(rctx, ref, "branch."+r.op+".failed", payload)
			s.event(rctx, parentRef, "branch."+r.op+".failed", payload)
			s.notify(rctx, ref, r.op, "MIGRATIONS_FAILED", err.Error())
			return
		}
		payload["detail"] = detail
		s.setState(rctx, ref, registry.BranchMigrationsPassed, detail, nil)
		s.event(rctx, ref, "branch."+r.op+".succeeded", payload)
		s.event(rctx, parentRef, "branch."+r.op+".succeeded", payload)
		s.notify(rctx, ref, r.op, "MIGRATIONS_PASSED", detail)
	}()
}

// ---- helpers shared by the operations ---------------------------------------------------

// activeParent loads the parent of b and checks it can take part in an operation.
func (s *Service) activeParent(ctx context.Context, b *Branch) (*registry.Project, error) {
	p, err := s.project(ctx, b.ParentRef)
	if err != nil {
		return nil, err
	}
	if p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy {
		return nil, conflict("project %s is %s; it must be running", p.Ref, p.Status)
	}
	return p, nil
}

func (s *Service) activeBranch(ctx context.Context, b *Branch) error {
	if b.ProjectStatus != registry.StatusActiveHealthy && b.ProjectStatus != registry.StatusActiveUnhealthy {
		return conflict("branch %s is %s; it must be running", b.Name, b.ProjectStatus)
	}
	return nil
}

func (s *Service) classFor(size string) (string, error) {
	switch size {
	case "":
		return s.cfg.Branching.Class(), nil
	case "pico", "nano", "micro":
		return "micro", nil
	case "small":
		return "small", nil
	case "medium":
		return "medium", nil
	}
	if strings.HasSuffix(size, "large") || strings.Contains(size, "xlarge") || strings.HasPrefix(size, "large") {
		return "large", nil
	}
	return "", invalid("unknown instance size %q", size)
}
