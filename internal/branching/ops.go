package branching

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// ActionInput is the body of merge, reset and push.
type ActionInput struct {
	// MigrationVersion limits a merge to branch migrations up to and including it, and a
	// reset of a schema-only branch to the parent's migrations up to and including it.
	MigrationVersion string
	// Force merges or pushes despite divergence: migrations whose versions differ in content
	// are skipped, the rest are applied.
	Force bool
}

// mergeLockTimeout bounds how long a merge waits for a lock on the parent: DDL that queues
// behind traffic would block the traffic behind it.
const mergeLockTimeout = 30 * time.Second

// Merge applies the migrations the branch has and its parent lacks to the parent, in one
// transaction where the SQL allows it. It refuses when the histories diverged (the parent
// has migrations the branch lacks, a version differs in content, or a branch migration is
// older than the parent's latest) unless Force is set. It runs in the background and
// returns the run id.
func (s *Service) Merge(ctx context.Context, idOrRef string, in ActionInput) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "merge")
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(b); err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	r, err := s.begin(b.Ref, "merge")
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "merging into "+parent.Ref, nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) {
		branchMigs, err := s.db.Migrations(ctx, b.Ref)
		if err != nil {
			return "", fmt.Errorf("read the branch's migrations: %w", err)
		}
		parentMigs, err := s.db.Migrations(ctx, parent.Ref)
		if err != nil {
			return "", fmt.Errorf("read the parent's migrations: %w", err)
		}
		plan := Compare(branchMigs, parentMigs)
		if d := plan.Summary(); d != "" && !in.Force {
			return "", fmt.Errorf("%w: %s (use force to merge anyway)", ErrDiverged, d)
		}
		apply, err := upTo(plan.Apply, in.MigrationVersion)
		if err != nil {
			return "", err
		}
		if len(apply) == 0 {
			return "nothing to merge: the parent already has every migration of the branch", nil
		}
		res, err := s.db.Apply(ctx, parent.Ref, apply, ApplyOptions{Atomic: true, LockTimeout: mergeLockTimeout})
		if err != nil {
			return "", fmt.Errorf("apply to the parent (rolled back where the SQL allows): %w", err)
		}
		detail := fmt.Sprintf("merged %d migration(s) into %s: %s", res.Applied, parent.Ref, versions(apply))
		if res.NonAtomic {
			detail += " (not atomic: a statement cannot run inside a transaction)"
		}
		if in.Force && len(plan.Conflicts) > 0 {
			detail += fmt.Sprintf("; skipped %d version(s) with different content", len(plan.Conflicts))
		}
		return detail, nil
	})
	return r.id, nil
}

// upTo keeps the migrations with version <= v ("" keeps all); v must name one of them.
func upTo(ms []Migration, v string) ([]Migration, error) {
	if v == "" {
		return ms, nil
	}
	var out []Migration
	found := false
	for _, m := range ms {
		if m.Version <= v {
			out = append(out, m)
		}
		found = found || m.Version == v
	}
	if !found {
		return nil, invalid("migration_version %s is not a migration the target lacks", v)
	}
	return out, nil
}

// Push applies the parent's migrations the branch lacks to the branch (a rebase onto the
// parent). Migrations the branch added itself stay. It refuses when a version exists on
// both sides with different content, unless Force is set (that version is skipped).
func (s *Service) Push(ctx context.Context, idOrRef string, in ActionInput) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "push")
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(b); err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	r, err := s.begin(b.Ref, "push")
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchRunningMigration, "applying the parent's migrations", nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) {
		parentMigs, err := s.db.Migrations(ctx, parent.Ref)
		if err != nil {
			return "", fmt.Errorf("read the parent's migrations: %w", err)
		}
		branchMigs, err := s.db.Migrations(ctx, b.Ref)
		if err != nil {
			return "", fmt.Errorf("read the branch's migrations: %w", err)
		}
		plan := Compare(parentMigs, branchMigs)
		if len(plan.Conflicts) > 0 && !in.Force {
			var v []string
			for _, c := range plan.Conflicts {
				v = append(v, c.Version)
			}
			return "", fmt.Errorf("%w: versions with different content on the branch and the parent: %s (use force to skip them)", ErrDiverged, strings.Join(v, ", "))
		}
		apply, err := upTo(plan.Apply, in.MigrationVersion)
		if err != nil {
			return "", err
		}
		if len(apply) == 0 {
			return "nothing to push: the branch already has every migration of the parent", nil
		}
		res, err := s.db.Apply(ctx, b.Ref, apply, ApplyOptions{Atomic: true})
		if err != nil {
			return "", fmt.Errorf("apply to the branch: %w", err)
		}
		return fmt.Sprintf("applied %d migration(s) from %s: %s", res.Applied, parent.Ref, versions(apply)), nil
	})
	return r.id, nil
}

// Diff returns, as SQL text, what Merge would apply: the branch's migrations the parent lacks.
func (s *Service) Diff(ctx context.Context, idOrRef string) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "diff")
	if err != nil {
		return "", err
	}
	if err := s.activeBranch(ctx, b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	branchMigs, err := s.db.Migrations(ctx, b.Ref)
	if err != nil {
		return "", err
	}
	parentMigs, err := s.db.Migrations(ctx, parent.Ref)
	if err != nil {
		return "", err
	}
	plan := Compare(branchMigs, parentMigs)
	var sb strings.Builder
	fmt.Fprintf(&sb, "-- migrations of branch %s that %s lacks\n", b.Name, parent.Ref)
	if d := plan.Summary(); d != "" {
		fmt.Fprintf(&sb, "-- divergence: %s\n", d)
	}
	for _, m := range plan.Apply {
		fmt.Fprintf(&sb, "\n-- migration %s %s\n", m.Version, m.Name)
		for _, st := range m.Statements {
			sb.WriteString(strings.TrimRight(st, "; \n\t") + ";\n")
		}
	}
	if len(plan.Apply) == 0 {
		sb.WriteString("-- nothing to merge\n")
	}
	return sb.String(), nil
}

// Reset recreates the branch from its parent: a schema-only branch gets the parent's
// migrations (up to MigrationVersion when set) and seed, a with_data branch a fresh copy of
// the parent's data. The branch keeps its id, name, ref and settings. A schema-only branch
// also keeps its credentials; a with_data branch gets new ones, as at creation. Anything
// written to the branch is gone.
func (s *Service) Reset(ctx context.Context, idOrRef string, in ActionInput) (string, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "reset")
	if err != nil {
		return "", err
	}
	if err := s.checkIdle(b); err != nil {
		return "", err
	}
	parent, err := s.activeParent(ctx, b)
	if err != nil {
		return "", err
	}
	if in.MigrationVersion != "" && b.WithData {
		return "", invalid("migration_version only applies to a schema-only branch")
	}
	old, err := s.reg.GetProject(ctx, b.Ref)
	if err != nil {
		return "", err
	}
	org := ""
	if old.OrgID != 0 {
		o, err := s.reg.GetOrganizationByID(ctx, old.OrgID)
		if err != nil {
			return "", err
		}
		org = o.Slug
	}
	var keys *secrets.ProjectKeys
	if !b.WithData {
		if keys, err = s.eng.Keys(ctx, b.Ref); err != nil {
			return "", err
		}
	}
	r, err := s.begin(b.Ref, "reset")
	if err != nil {
		return "", err
	}
	s.setState(ctx, b.Ref, registry.BranchCreatingProject, "resetting from "+parent.Ref, nil)
	s.spawn(r, b.Ref, parent.Ref, func(ctx context.Context) (string, error) {
		if err := s.removeBranch(ctx, b, false); err != nil {
			return "", fmt.Errorf("remove the old cluster: %w", err)
		}
		info := *old.Branch
		info.State, info.Detail, info.CloneMethod = registry.BranchCreatingProject, "recreating the project", ""
		if !info.Persistent {
			info.ExpiresAt = s.expiry(0)
		}
		info.DeletionScheduledAt = nil
		j := &createJob{
			parent: parent, ref: b.Ref, info: &info, class: old.Class, org: org,
			in: CreateInput{Name: b.Name, GitBranch: b.GitBranch, Persistent: b.Persistent, WithData: b.WithData, NotifyURL: b.NotifyURL},
		}
		if keys != nil {
			j.keys = keys
		}
		j.upTo = in.MigrationVersion
		return s.doCreate(ctx, j)
	})
	return r.id, nil
}

// Update is PATCH /v1/branches/{id}.
type UpdateInput struct {
	Name          *string
	GitBranch     *string
	Persistent    *bool
	NotifyURL     *string
	RequestReview *bool
}

// Update changes the settings of a branch. Making a branch persistent clears its expiry;
// making it non-persistent starts the default lifetime.
func (s *Service) Update(ctx context.Context, idOrRef string, in UpdateInput) (*Branch, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "update")
	if err != nil {
		return nil, err
	}
	p, err := s.project(ctx, b.Ref)
	if err != nil {
		return nil, err
	}
	nb := *p.Branch
	if in.Name != nil && *in.Name != nb.Name {
		if err := ValidName(*in.Name); err != nil {
			return nil, err
		}
		nb.Name = *in.Name
	}
	if in.GitBranch != nil {
		nb.GitBranch = *in.GitBranch
	}
	if in.NotifyURL != nil {
		if *in.NotifyURL != "" {
			if err := validNotifyURL(*in.NotifyURL); err != nil {
				return nil, err
			}
		}
		nb.NotifyURL = *in.NotifyURL
	}
	if in.Persistent != nil && *in.Persistent != nb.Persistent {
		nb.Persistent = *in.Persistent
		if nb.Persistent {
			nb.ExpiresAt = nil
		} else {
			nb.ExpiresAt = s.expiry(0)
		}
	}
	if in.RequestReview != nil && *in.RequestReview {
		now := s.now().UTC()
		nb.ReviewRequestedAt = &now
	}
	if err := s.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return nil, conflict("project %s already has a branch named %q", b.ParentRef, nb.Name)
		}
		return nil, err
	}
	if nb.Name != p.Name {
		if q, err := s.reg.GetProject(ctx, b.Ref); err == nil && q.Name != nb.Name {
			q.Name = nb.Name
			_ = s.reg.UpdateProject(ctx, q)
		}
	}
	return s.Resolve(ctx, b.Ref)
}

// DeleteOptions tune Delete.
type DeleteOptions struct {
	// Schedule is the soft delete of DELETE /v1/branches/{id}?force=false: the branch is
	// removed after [branching] soft_delete_grace_minutes, and Restore cancels it.
	Schedule bool
}

// Delete removes a branch: its fleet tenants, units, data and registry row. A branch that
// is not persistent gets no final backup and its WAL archive is removed; a persistent one
// keeps the final base backup the lifecycle takes (when a backup backend is configured).
// With Schedule it only marks the branch for deletion and returns it.
func (s *Service) Delete(ctx context.Context, idOrRef string, o DeleteOptions) (*Branch, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "delete")
	if err != nil {
		return nil, err
	}
	if err := s.checkIdle(b); err != nil {
		return nil, err
	}
	if o.Schedule {
		p, err := s.project(ctx, b.Ref)
		if err != nil {
			return nil, err
		}
		nb := *p.Branch
		at := s.now().Add(s.cfg.Branching.SoftDeleteGrace()).UTC()
		nb.DeletionScheduledAt = &at
		if err := s.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
			return nil, err
		}
		s.event(ctx, b.Ref, "branch.deletion_scheduled", map[string]any{"at": at})
		return s.Resolve(ctx, b.Ref)
	}
	// A request that gives up must not leave a half-deleted branch.
	return b, s.deleteNow(context.WithoutCancel(ctx), b, "api")
}

func (s *Service) deleteNow(ctx context.Context, b *Branch, by string) error {
	r, err := s.begin(b.Ref, "delete")
	if err != nil {
		return err
	}
	defer s.end(b.Ref, r)
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	if err := s.removeBranch(ctx, b, true); err != nil {
		return err
	}
	payload := map[string]any{"branch": b.Ref, "name": b.Name, "by": by}
	s.event(ctx, b.ParentRef, "branch.deleted", payload)
	return nil
}

// removeBranch deletes the branch's project. forget also removes the registry row's
// history of events; reset keeps nothing but the row is recreated by the caller.
func (s *Service) removeBranch(ctx context.Context, b *Branch, final bool) error {
	keepBackup := b.Persistent && final
	if err := s.eng.DeleteWith(ctx, b.Ref, lifecycle.DeleteOptions{SkipFinalBackup: !keepBackup}); err != nil {
		return err
	}
	if !keepBackup {
		// An ephemeral branch leaves nothing in the archive: its WAL and base backups are
		// worthless, cost storage, and a reset reuses the ref (its WAL would collide).
		if err := s.purgeArchive(ctx, b.Ref); err != nil {
			s.log.Warn("could not remove the branch's archive", "ref", b.Ref, "err", err)
		}
	}
	return nil
}

// Restore cancels a scheduled deletion.
func (s *Service) Restore(ctx context.Context, idOrRef string) (*Branch, error) {
	b, err := s.resolveBranch(ctx, idOrRef, "restore")
	if err != nil {
		return nil, err
	}
	if b.DeletionScheduledAt == nil {
		return nil, conflict("branch %s is not scheduled for deletion", b.Name)
	}
	p, err := s.project(ctx, b.Ref)
	if err != nil {
		return nil, err
	}
	nb := *p.Branch
	nb.DeletionScheduledAt = nil
	if err := s.reg.UpdateBranch(ctx, b.Ref, &nb); err != nil {
		return nil, err
	}
	s.event(ctx, b.Ref, "branch.deletion_cancelled", nil)
	return s.Resolve(ctx, b.Ref)
}

// DisableBranching deletes every branch of the project (DELETE /v1/projects/{ref}/branches).
func (s *Service) DisableBranching(ctx context.Context, ref string) error {
	parent, err := s.Parent(ctx, ref)
	if err != nil {
		return err
	}
	kids, err := s.children(ctx, parent.Ref)
	if err != nil {
		return err
	}
	var errs []error
	for i := range kids {
		b := branchOf(&kids[i])
		if err := s.deleteNow(context.WithoutCancel(ctx), b, "disable"); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.Name, err))
		}
	}
	return errors.Join(errs...)
}
