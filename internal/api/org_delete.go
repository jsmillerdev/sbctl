package api

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

	"github.com/jsmillerdev/supavise/internal/branching"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/members"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// Deleting an organization (DELETE /platform/organizations/{slug}, `supavise orgs delete`).
//
// Hosted Supabase deletes the organization's projects with it (Studio's dialog says so and makes
// the Owner acknowledge each project), so this does too, through the lifecycle engine: every
// project takes its final base backup as a normal project delete does. The steps run in an order
// that keeps a failed run harmless and a second run able to finish it:
//
//  1. The organization's SSO providers are removed (DashboardSSO.RemoveForOrgDelete): GoTrue
//     drops the provider, its users lose their seats and tokens, and the provider's record, its
//     users and its refusals go. This comes first because it can refuse (a user of the provider
//     holds a role in an organization the caller does not control), before any data is touched.
//  2. The projects are deleted, branches before their parents.
//  3. The organization's row is deleted. Every table that points at the organization cascades from
//     it (members, project-scoped roles, invitations and the invite tokens bound to them, the MFA
//     setting, default-role rules), so nothing can be left behind; it fails, and changes nothing,
//     when a project was created meanwhile. The explicit clean-up that follows is for stores
//     without foreign keys (the in-memory ones of the tests).
//
// The node's last organization is never deleted. Studio copes with none, but the API gives a node
// with no organization a "Default" one owned by whoever opens the dashboard first (defaultOrg,
// allOrgs): deleting the last organization would make an arbitrary member the Owner of a new one.

// OrgDeleter deletes organizations. The daemon builds one for the HTTP route and the CLI for
// `supavise orgs delete`.
type OrgDeleter struct {
	Reg     registry.Registry
	Members *members.Service
	// SSO removes the organization's identity providers; nil when the node has no SSO store.
	SSO *DashboardSSO
	// Claims holds the invite tokens bound to the organization's invitations.
	Claims ClaimStore
	// DeleteProject deletes one project, a branch included, through the lifecycle engine.
	DeleteProject func(ctx context.Context, p registry.Project) error
	Log           *slog.Logger

	mu sync.Mutex // one deletion at a time, so two cannot both see another organization left
}

// OrgDeletion is what a deletion removed.
type OrgDeletion struct {
	Slug      string
	Projects  []string // refs, in the order they were deleted
	Providers []string // SSO provider ids
}

// OrgDeleteError says which project stopped a deletion. Nothing else of the organization has
// changed except what is listed as removed; the deletion can be run again.
type OrgDeleteError struct {
	Ref string
	Err error
}

func (e *OrgDeleteError) Error() string {
	return fmt.Sprintf("project %s was not deleted, so the organization was kept (run the deletion again): %v", e.Ref, e.Err)
}
func (e *OrgDeleteError) Unwrap() error { return e.Err }

var errLastOrganization = errf(http.StatusConflict, "This is the last organization on this node. Create another organization first.")

func (d *OrgDeleter) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Projects returns the projects of the organization in the order they are deleted: branches
// first (a parent with branches cannot be deleted), then the rest by sequence number.
func (d *OrgDeleter) Projects(ctx context.Context, org *registry.Organization) ([]registry.Project, error) {
	all, err := d.Reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []registry.Project
	for _, p := range all {
		if p.OrgID == org.ID && p.Ref != config.SystemRef {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Branch != nil && out[j].Branch == nil })
	return out, nil
}

// Delete deletes the organization with its projects and everything keyed by it. actor is the
// caller whose rights the SSO removal checks (nil: the operator); the caller has been checked
// for the right to delete the organization already.
func (d *OrgDeleter) Delete(ctx context.Context, actor *members.Access, org *registry.Organization) (*OrgDeletion, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	orgs, err := d.Reg.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	if len(orgs) <= 1 {
		return nil, errLastOrganization
	}
	res := &OrgDeletion{Slug: org.Slug}

	if d.SSO != nil {
		rows, err := d.SSO.Store.ListProviders(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.OrgID != org.ID {
				continue
			}
			if _, err := d.SSO.RemoveForOrgDelete(ctx, actor, row.ID, org.ID); err != nil {
				return res, err
			}
			res.Providers = append(res.Providers, row.ID)
		}
	}

	ps, err := d.Projects(ctx, org)
	if err != nil {
		return res, err
	}
	for _, p := range ps {
		if err := d.DeleteProject(ctx, p); err != nil {
			return res, &OrgDeleteError{Ref: p.Ref, Err: err}
		}
		res.Projects = append(res.Projects, p.Ref)
	}

	if err := d.Reg.DeleteOrganization(ctx, org.ID); err != nil {
		switch {
		case errors.Is(err, registry.ErrConflict):
			return res, errf(http.StatusConflict, "A project was created in the organization while it was being deleted. Try again.")
		case errors.Is(err, registry.ErrNotFound):
			return res, errf(http.StatusNotFound, "Organization not found")
		}
		return res, err
	}
	// The organization is gone. What follows cleans stores without foreign keys; a failure
	// leaves nothing behind in Postgres, so it is logged and the deletion stands.
	if d.Members != nil {
		ids, err := d.Members.DeleteOrganization(ctx, org.ID)
		if err != nil {
			d.log().Warn("organization deleted; its leftover memberships could not be cleared", "org", org.Slug, "error", err)
		} else if d.Claims != nil {
			if err := d.Claims.DeleteInviteTokens(ctx, ids); err != nil {
				d.log().Warn("organization deleted; its invite tokens could not be cleared", "org", org.Slug, "error", err)
			}
		}
	}
	payload := map[string]any{"org": org.Slug, "name": org.Name, "projects": res.Projects, "sso_providers": res.Providers}
	if err := d.Reg.AppendEvent(context.WithoutCancel(ctx), config.SystemRef, "org.deleted", payload); err != nil {
		d.log().Warn("organization deleted; the audit event was not recorded", "org", org.Slug, "error", err)
	}
	d.log().Info("organization deleted", "org", org.Slug, "projects", strings.Join(res.Projects, ","))
	return res, nil
}

// ---- the route ---------------------------------------------------------------------

// removeProject deletes a project of an organization being deleted, as the project routes do:
// a branch through the branching service (an ephemeral one takes no final backup and leaves no
// archive), anything else through the lifecycle manager (final base backup).
func (s *Server) removeProject(ctx context.Context, p registry.Project) error {
	if p.Branch != nil && s.branches != nil {
		if _, err := s.branches.Delete(ctx, p.Ref, branching.DeleteOptions{}); err != nil {
			return err
		}
	} else if err := s.mgr.Delete(ctx, p.Ref); err != nil {
		return err
	}
	s.functionsGone(ctx, p.Ref)
	return nil
}

// platformDeleteOrg is DELETE /platform/organizations/{slug}: Owners only (authz.go). It answers
// 200 with no body, as the spec says.
func (s *Server) platformDeleteOrg(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	actor, err := s.callerAccess(r)
	if err != nil {
		return err
	}
	ps, err := s.orgs.Projects(r.Context(), org)
	if err != nil {
		return err
	}
	// One delete bound per project: each takes a final base backup.
	ctx, cancel, err := s.detach(r, time.Duration(1+len(ps))*deleteTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	if _, err := s.orgs.Delete(ctx, actor, org); err != nil {
		var he *Error
		if errors.As(err, &he) {
			return he
		}
		var de *OrgDeleteError
		if errors.As(err, &de) {
			s.log.Error("organization deletion stopped", "org", org.Slug, "project", de.Ref, "error", de.Err)
			if errors.Is(de.Err, lifecycle.ErrInvalidState) {
				return errf(http.StatusConflict, "Project %s cannot be deleted now: %v", de.Ref, de.Err)
			}
			return errf(http.StatusInternalServerError, "Failed to delete project %s, so the organization was kept. See the Supavise log and try again.", de.Ref)
		}
		return mapErr(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
