package branching

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// CreateInput is the body of POST /v1/projects/{ref}/branches, plus the lifetime.
type CreateInput struct {
	Name      string
	GitBranch string
	// Persistent branches never expire and get a final base backup when deleted.
	Persistent bool
	// WithData clones the parent's data (copy-on-write when the data disk allows it, else
	// from the parent's latest base backup plus WAL). Without it the branch gets the
	// parent's migrations and seed: schema only, like a hosted branch.
	WithData bool
	// DesiredInstanceSize selects the project class; empty uses [branching] default_class.
	DesiredInstanceSize string
	NotifyURL           string
	// TTL overrides [branching] default_ttl for a non-persistent branch; zero uses the default,
	// a negative value means the branch does not expire.
	TTL time.Duration
	// Seed is a SQL script applied after the migrations of a schema-only branch. Empty uses
	// the parent's stored seed (SetSeed), if any.
	Seed string
	// IsDefault is accepted so that the request body decodes, and refused when true.
	IsDefault bool
	// AllowEgress is the opt-out of the isolation a branch with data gets by default (sbctl
	// only: the Management API's create body has no such field, the API takes it as the query
	// parameter allow_egress=true). Without it, the branch's Postgres unit may reach loopback
	// only (systemd's IPAddressDeny, where the supervisor can enforce it) and the parent's
	// pg_cron jobs are paused. With it, the branch keeps the parent's outbound side effects:
	// its webhooks, pg_net calls, cron jobs and foreign servers act on the outside world as
	// the parent's do. Logical replication subscriptions are detached either way. Ignored
	// without WithData: a schema-only branch inherits no data.
	AllowEgress bool
}

// egressEnforced reports whether the supervisor can confine a unit's network traffic: the
// systemd backend can, the exec backend (development and tests) runs plain child processes.
func (s *Service) egressEnforced() bool { return s.cfg.Supervisor != config.SupervisorExec }

// egressPolicy is the registry.BranchInfo.Egress a branch with data starts with.
func (s *Service) egressPolicy(allow bool) string {
	switch {
	case allow:
		return registry.EgressAllowed
	case !s.egressEnforced():
		return registry.EgressUnenforced
	}
	return registry.EgressPending
}

// keepsCron reports whether the cron jobs of a branch with this policy stay active.
func (s *Service) keepsCron(egress string) bool {
	return s.cfg.Branching.KeepCronJobs || egress == registry.EgressAllowed
}

// egressDetail is the sentence a branch's detail carries about its outbound network, so that
// the state a client reads says what the branch can reach.
func (s *Service) egressDetail(egress string) string {
	cron := "pg_cron jobs paused (the ones that were active are in sbctl_branch.paused_cron_jobs)"
	if s.keepsCron(egress) {
		cron = "pg_cron jobs left active"
	}
	switch egress {
	case registry.EgressAllowed:
		return "egress NOT blocked (created with allow_egress): webhooks, pg_net, foreign servers and cron jobs act on the outside world as the parent's do; " + cron + "; " + withDataNotice
	case registry.EgressUnenforced:
		return "egress NOT blocked: the exec supervisor cannot confine a unit's network, so webhooks, pg_net and foreign servers of the parent's data can reach the outside world; " + cron + "; " + withDataNotice
	}
	return "egress denied (the branch's Postgres reaches loopback only); " + cron + "; " + withDataNotice
}

// withDataNotice is in the detail of every branch with data, the one place the Management API
// shows a client (the spec's create body has no field for it) what such a branch is.
const withDataNotice = "a with_data branch is a copy of production: users' sessions are removed, but credentials users stored in their own tables, function bodies or Vault entries (other than sbctl's keys) are not detected and loopback is open on every port, so give it to trusted users and agents only"

// Create starts a new branch of the project ref belongs to and returns it as soon as its
// project exists (state CREATING_PROJECT). The work continues in the background; poll Get
// or call WaitIdle.
func (s *Service) Create(ctx context.Context, ref string, in CreateInput) (*Branch, error) {
	if in.IsDefault {
		return nil, invalid("creating a default branch is not supported: the project itself is the default branch")
	}
	if err := ValidName(in.Name); err != nil {
		return nil, err
	}
	if in.NotifyURL != "" {
		if err := validNotifyURL(in.NotifyURL); err != nil {
			return nil, err
		}
	}
	if ref == config.SystemRef {
		return nil, invalid("the system project has no branches")
	}
	p, err := s.project(ctx, ref)
	if err != nil {
		return nil, err
	}
	if p.Branch != nil {
		return nil, invalid("%s is itself a branch of %s; create branches from the parent project", ref, p.Branch.ParentRef)
	}
	if p.Status != registry.StatusActiveHealthy && p.Status != registry.StatusActiveUnhealthy {
		return nil, conflict("project %s is %s; it must be running to be branched", p.Ref, p.Status)
	}
	class, err := s.classFor(in.DesiredInstanceSize)
	if err != nil {
		return nil, err
	}
	if err := s.checkLimits(ctx, p.Ref, in.Name); err != nil {
		return nil, err
	}
	if in.WithData {
		// Before anything exists: a clone that fills the disk takes the whole node down.
		if err := s.checkDisk(p.Ref, 0); err != nil {
			return nil, err
		}
	}
	org := ""
	if p.OrgID != 0 {
		o, err := s.reg.GetOrganizationByID(ctx, p.OrgID)
		if err != nil {
			return nil, err
		}
		org = o.Slug
	}

	info := &registry.BranchInfo{
		ID: newUUID(), ParentRef: p.Ref, Name: in.Name, GitBranch: in.GitBranch, Persistent: in.Persistent, WithData: in.WithData,
		NotifyURL: in.NotifyURL, State: registry.BranchCreatingProject, Detail: "creating the project",
	}
	if in.WithData {
		info.Egress = s.egressPolicy(in.AllowEgress)
	}
	if !in.Persistent {
		info.ExpiresAt = s.expiry(in.TTL)
	}
	newRef := secrets.NewRef()
	r, err := s.begin(newRef, "create")
	if err != nil {
		return nil, err
	}
	job := &createJob{parent: p, ref: newRef, info: info, in: in, class: class, org: org}
	failed := make(chan error, 1)
	s.spawn(r, newRef, p.Ref, func(ctx context.Context) (string, error) {
		detail, err := s.doCreate(ctx, job)
		if err != nil {
			failed <- err
		}
		return detail, err
	})

	// Return when the row is visible, or when the work failed before it got that far.
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(s.createWait)
	for {
		select {
		case err := <-failed:
			if b, gerr := s.Resolve(ctx, newRef); gerr == nil && b.State != "" {
				return b, nil // the row exists with the failure recorded
			}
			return nil, err
		case <-tick.C:
			if np, err := s.reg.GetProject(ctx, newRef); err == nil && np.Branch != nil {
				return branchOf(np), nil
			}
		case <-timeout:
			return &Branch{ID: info.ID, Name: info.Name, Ref: newRef, ParentRef: p.Ref, GitBranch: in.GitBranch, Persistent: in.Persistent,
				WithData: in.WithData, State: registry.BranchCreatingProject, ProjectStatus: registry.StatusComingUp, CreatedAt: s.now(), UpdatedAt: s.now(),
				ExpiresAt: info.ExpiresAt, NotifyURL: in.NotifyURL}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *Service) expiry(ttl time.Duration) *time.Time {
	if ttl < 0 {
		return nil
	}
	if ttl == 0 {
		d, ok, _ := s.cfg.Branching.TTL()
		if !ok {
			return nil
		}
		ttl = d
	}
	t := s.now().Add(ttl).UTC()
	return &t
}

func (s *Service) checkLimits(ctx context.Context, parentRef, name string) error {
	all, err := s.reg.ListProjects(ctx)
	if err != nil {
		return err
	}
	perProject, total := s.cfg.Branching.Limits()
	var mine, every int
	for _, q := range all {
		if q.Branch == nil {
			continue
		}
		every++
		if q.Branch.ParentRef == parentRef {
			mine++
			if q.Branch.Name == name {
				return conflict("project %s already has a branch named %q", parentRef, name)
			}
		}
	}
	if mine >= perProject {
		return fmt.Errorf("%w: project %s already has %d branches (limit %d, [branching] max_per_project)", ErrLimit, parentRef, mine, perProject)
	}
	if every >= total {
		return fmt.Errorf("%w: this node already has %d branches (limit %d, [branching] max_total)", ErrLimit, every, total)
	}
	return nil
}

type createJob struct {
	parent *registry.Project
	ref    string
	info   *registry.BranchInfo
	in     CreateInput
	class  string
	org    string
	// keys, when set, are the credentials the new cluster gets (a reset of a schema-only
	// branch keeps the branch's own).
	keys *secrets.ProjectKeys
	// upTo limits the migrations replayed into a schema-only branch (reset to a version).
	upTo string
	// keepExpiry leaves expires_at alone when the branch is ready (a reset keeps the lifetime).
	keepExpiry bool
	// recreate builds the cluster over the branch's existing registry row, which a reset keeps
	// (lifecycle.DeleteOptions.KeepRecord), so that a failure leaves the branch registered.
	recreate bool
}

// doCreate is the body of a branch creation. It returns the detail recorded on success.
func (s *Service) doCreate(ctx context.Context, j *createJob) (string, error) {
	req := lifecycle.CreateRequest{
		Name: j.in.Name, OrgSlug: j.org, Region: j.parent.Region, Class: j.class, Ref: j.ref, Branch: j.info, Recreate: j.recreate,
	}
	method := MethodSchema
	var stats *CloneStats
	var reason string
	if j.in.WithData {
		method, _, reason = s.planData(j.parent.Ref, j.ref)
		if method == MethodSchema {
			// Fail before anything exists rather than hand over an empty database that was
			// asked to hold data.
			return "", invalid("with_data is not possible: %s", reason)
		}
	}
	started := s.now()
	switch method {
	case MethodSchema:
		req.Keys = j.keys
		if _, err := s.eng.Create(ctx, req); err != nil {
			return "", err
		}
	case MethodBackup:
		if err := s.createFromBackup(ctx, j); err != nil {
			return "", err
		}
	default:
		pk, err := s.eng.Keys(ctx, j.parent.Ref)
		if err != nil {
			return "", fmt.Errorf("read the parent's credentials: %w", err)
		}
		// The clone carries the parent's role passwords inside its data, so the services
		// start with them; rotateCredentials replaces them right after. Everything a client
		// can hold is new from the start.
		nk := *pk
		nk.JWTSecret = secrets.NewJWTSecret()
		nk.PublishableKey = secrets.NewPublishableKey()
		nk.SecretKey = secrets.NewSecretKey()
		if err := nk.ResignLegacy(j.ref, s.now()); err != nil {
			return "", err
		}
		req.Keys = &nk
		req.Seed = s.cloneSeeder(j.parent, method, &stats)
		if _, err := s.eng.Create(ctx, req); err != nil {
			return "", err
		}
		if err := s.detach(ctx, j.ref); err != nil {
			return "", err
		}
	}
	// Record how the data was made before the migrations run: a failure below still tells it.
	s.setState(ctx, j.ref, registry.BranchRunningMigration, "project created", func(b *registry.BranchInfo) { b.CloneMethod = method })

	detail := "schema only"
	switch {
	case method == MethodSchema:
		n, seeded, err := s.replay(ctx, j)
		if err != nil {
			return "", err
		}
		detail = fmt.Sprintf("schema only: %d migration(s) replayed", n)
		if seeded {
			detail += ", seed applied"
		}
	case method == MethodBackup:
		detail = "data restored from the parent's latest base backup and WAL"
	case stats == nil:
		detail = "data cloned by " + method
	default:
		detail = fmt.Sprintf("data cloned by %s in %d ms (%d files, %s apparent, %s of new disk, %d WAL segments)",
			method, stats.TotalMillis, stats.Files, humanBytes(stats.Bytes), humanBytes(stats.ExtraDiskByte), stats.WALSegments)
	}
	egress := ""
	if j.in.WithData && method != MethodSchema {
		if p, err := s.reg.GetProject(ctx, j.ref); err == nil && p.Branch != nil {
			egress = p.Branch.Egress
			detail += "; " + s.egressDetail(egress)
		}
	}
	if !j.in.Persistent && !j.keepExpiry {
		// The expiry clock starts when the branch is usable, not when the request came in.
		s.setState(ctx, j.ref, registry.BranchRunningMigration, detail, func(b *registry.BranchInfo) { b.ExpiresAt = s.expiry(j.in.TTL) })
	}
	payload := map[string]any{"branch": j.ref, "name": j.in.Name, "method": method, "with_data": j.in.WithData, "ms": s.now().Sub(started).Milliseconds()}
	if stats != nil {
		payload["clone"] = stats
	}
	if reason != "" {
		payload["fallback_reason"] = reason
	}
	if egress != "" {
		payload["egress"] = egress
	}
	s.event(ctx, j.parent.Ref, "branch.created", payload)
	s.event(ctx, j.ref, "branch.created", payload)
	return detail, nil
}

// planData decides how a with_data branch gets its data: copy-on-write when the parent's
// data directory and the new branch's directory share a filesystem that can clone files,
// else the parent's base backup. The reason explains a fallback.
func (s *Service) planData(parentRef, newRef string) (method, fsys, reason string) {
	srcData := filepath.Join(s.cfg.Paths().ProjectService(parentRef, config.SvcPostgres), "data")
	dstParent := s.cfg.Paths().ProjectService(newRef, config.SvcPostgres)
	if s.cfg.Branching.Clone == "backup" {
		reason = "[branching] clone = backup"
	} else {
		m, f, why := s.detect(srcData, dstParent)
		if m != "" {
			return m, f, ""
		}
		fsys, reason = f, why
	}
	if s.bk == nil {
		return MethodSchema, fsys, reason + "; no backup backend is configured for the base-backup fallback"
	}
	return MethodBackup, fsys, reason
}

// cloneSeeder is the lifecycle.DataSeeder of a copy-on-write branch.
func (s *Service) cloneSeeder(parent *registry.Project, method string, out **CloneStats) lifecycle.DataSeeder {
	return func(ctx context.Context, _ *registry.Project, dataDir string) error {
		st, err := s.clone(ctx, parent.Ref, method, dataDir)
		if err != nil {
			return err
		}
		*out = st
		// The clone carries the parent's postgresql.auto.conf. The branch's first postmaster must
		// start with the parent's integrations switched off, as the base-backup path does
		// (stampManager.Create); isolate runs only after the first start.
		if err := writeQuarantine(dataDir); err != nil {
			return fmt.Errorf("write the first-start settings of the branch: %w", err)
		}
		s.log.Info("cloned the parent's data directory", "parent", parent.Ref, "method", st.Method, "fs", st.FS,
			"files", st.Files, "cloned", st.Cloned, "copied", st.Copied, "bytes", st.Bytes, "extra_disk", st.ExtraDiskByte, "ms", st.TotalMillis)
		return nil
	}
}

// cloneParent clones the parent's running cluster into dstData.
func (s *Service) cloneParent(ctx context.Context, parentRef, method, dstData string) (*CloneStats, error) {
	dsn, err := s.eng.ConnString(ctx, parentRef, lifecycle.RoleAdmin)
	if err != nil {
		return nil, err
	}
	c := &cloner{method: method, log: s.log}
	return c.Clone(ctx, cloneSource{DSN: dsn}, dstData)
}

// createFromBackup is the fallback for with_data: restore the parent's latest base backup
// plus all archived WAL as a new project (internal/backup), then make it a branch.
func (s *Service) createFromBackup(ctx context.Context, j *createJob) error {
	// The restore creates its project through this Manager, which stamps it as a branch.
	bs := s.bk.WithManager(stampManager{Manager: s.eng, info: j.info, class: j.class, recreate: j.recreate})
	if _, err := bs.RestoreWith(ctx, j.parent.Ref, s.now(), j.ref, backup.RestoreOptions{Latest: true, IntoFailedRow: j.recreate}); err != nil {
		return fmt.Errorf("restore the parent's base backup: %w", err)
	}
	p, err := s.reg.GetProject(ctx, j.ref)
	if err != nil {
		return err
	}
	if p.Name != j.in.Name {
		p.Name = j.in.Name
		_ = s.reg.UpdateProject(ctx, p)
	}
	return s.detach(ctx, j.ref)
}

// detach makes a branch whose cluster came from the parent's data independent of the parent:
// its outbound integrations are neutralized (isolateBranch), then its credentials rotate, then
// the parent's credentials inside the data are replaced by the new ones (rewriteBranchCredentials).
// A failure stops the branch (quarantine): it would otherwise run with the parent's passwords,
// with the parent's subscriptions, foreign servers and jobs, or with a key to the parent in its data.
func (s *Service) detach(ctx context.Context, ref string) error {
	if err := s.isolate(ctx, ref); err != nil {
		return s.quarantine(ctx, ref, fmt.Errorf("isolate the branch from the parent's integrations: %w", err))
	}
	if err := s.rotate(ctx, ref); err != nil {
		return s.quarantine(ctx, ref, fmt.Errorf("rotate the credentials of the branch: %w", err))
	}
	// The branch has credentials of its own now: put them where the data holds the parent's.
	if err := s.rewrite(ctx, ref); err != nil {
		return s.quarantine(ctx, ref, fmt.Errorf("replace the parent's credentials in the branch's data: %w", err))
	}
	return nil
}

// quarantine stops a branch that still carries the parent's database passwords because the
// rotation failed, so that nobody holding the parent's credentials can open it, and returns
// err with that said. The branch stays registered (MIGRATIONS_FAILED) for inspection, reset or delete.
func (s *Service) quarantine(ctx context.Context, ref string, err error) error {
	perr := s.eng.Pause(context.WithoutCancel(ctx), ref)
	if perr != nil {
		// Already stopped (a restart that failed between its stop and its start) is what is wanted.
		if p, gerr := s.reg.GetProject(ctx, ref); gerr == nil && p.Status == registry.StatusInactive {
			perr = nil
		}
	}
	if perr != nil {
		s.log.Error("could not stop a branch that kept the parent's credentials", "ref", ref, "err", perr)
		return fmt.Errorf("%w (the branch could not be stopped either: %v; delete it)", err, perr)
	}
	return fmt.Errorf("%w (the branch was stopped; delete it or reset it)", err)
}

// stampManager makes the projects a backup restore creates into branches, sizes them, and has
// the restored cluster start with the parent's integrations switched off (quarantineSettings).
type stampManager struct {
	lifecycle.Manager
	info     *registry.BranchInfo
	class    string
	recreate bool
}

func (m stampManager) Create(ctx context.Context, req lifecycle.CreateRequest) (*registry.Project, error) {
	b := *m.info
	req.Branch = &b
	req.Recreate = m.recreate
	if m.class != "" {
		req.Class = m.class
	}
	if seed := req.Seed; seed != nil {
		req.Seed = func(ctx context.Context, p *registry.Project, dataDir string) error {
			if err := seed(ctx, p, dataDir); err != nil {
				return err
			}
			return writeQuarantine(dataDir)
		}
	}
	return m.Manager.Create(ctx, req)
}

// replay gives a schema-only branch the parent's migration history, then the seed.
func (s *Service) replay(ctx context.Context, j *createJob) (migrations int, seeded bool, err error) {
	parentMigs, err := s.db.Migrations(ctx, j.parent.Ref)
	if err != nil {
		return 0, false, fmt.Errorf("read the parent's migration history: %w", err)
	}
	if j.upTo != "" {
		cut := parentMigs[:0:0]
		found := false
		for _, m := range parentMigs {
			if m.Version <= j.upTo {
				cut = append(cut, m)
			}
			found = found || m.Version == j.upTo
		}
		if !found {
			return 0, false, invalid("migration_version %s is not in the parent's migration history", j.upTo)
		}
		parentMigs = cut
	}
	if len(parentMigs) > 0 {
		s.setState(ctx, j.ref, registry.BranchRunningMigration, fmt.Sprintf("replaying %d migration(s)", len(parentMigs)), nil)
		if _, err := s.db.Apply(ctx, j.ref, parentMigs, ApplyOptions{}); err != nil {
			return 0, false, fmt.Errorf("replay the parent's migrations: %w", err)
		}
	}
	seed := j.in.Seed
	if strings.TrimSpace(seed) == "" {
		if seed, err = s.GetSeed(ctx, j.parent.Ref); err != nil {
			return 0, false, err
		}
	}
	if strings.TrimSpace(seed) != "" {
		s.setState(ctx, j.ref, registry.BranchRunningMigration, "applying the seed", nil)
		if err := s.db.RunScript(ctx, j.ref, seed); err != nil {
			return len(parentMigs), false, fmt.Errorf("apply the seed: %w", err)
		}
		return len(parentMigs), true, nil
	}
	return len(parentMigs), false, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
