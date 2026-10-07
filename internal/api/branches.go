package api

import (
	"errors"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	v1 "github.com/OWNER/sbctl/internal/api/gen/v1"
	"github.com/OWNER/sbctl/internal/branching"
	"github.com/OWNER/sbctl/internal/registry"
)

// routesBranches serves the branch endpoints of the Management API (v1). Studio calls them
// directly (there is no /platform twin of the branch routes in the spec); the Supabase CLI
// (`branches ...`) and the MCP server's branch tools use the same paths. Without a
// branching service (Deps.Branching nil) a project has only its default branch.
func (s *Server) routesBranches(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/branches", s.listBranches)
	add("POST /v1/projects/{ref}/branches", s.createBranch)
	add("DELETE /v1/projects/{ref}/branches", s.disableBranching)
	add("GET /v1/projects/{ref}/branches/{name}", s.getBranchByName)
	add("GET /v1/branches/{branch_id_or_ref}", s.getBranch)
	add("PATCH /v1/branches/{branch_id_or_ref}", s.updateBranch)
	add("DELETE /v1/branches/{branch_id_or_ref}", s.deleteBranch)
	add("POST /v1/branches/{branch_id_or_ref}/merge", s.branchAction("merge"))
	add("POST /v1/branches/{branch_id_or_ref}/reset", s.branchAction("reset"))
	add("POST /v1/branches/{branch_id_or_ref}/push", s.branchAction("push"))
	add("POST /v1/branches/{branch_id_or_ref}/restore", s.restoreBranch)
	add("GET /v1/branches/{branch_id_or_ref}/diff", s.diffBranch)
}

// mapBranchErr turns branching errors into API errors.
func mapBranchErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, branching.ErrNotFound), errors.Is(err, registry.ErrNotFound):
		return errf(http.StatusNotFound, "Branch not found")
	case errors.Is(err, branching.ErrInvalid), errors.Is(err, branching.ErrLimit):
		return errf(http.StatusBadRequest, "%s", trimSentinel(err))
	case errors.Is(err, branching.ErrConflict):
		return errf(http.StatusConflict, "%s", trimSentinel(err))
	}
	return mapErr(err)
}

// trimSentinel drops the package-level sentinel's text from an error message that wraps it.
func trimSentinel(err error) string {
	msg := err.Error()
	for _, s := range []error{branching.ErrInvalid, branching.ErrConflict, branching.ErrLimit} {
		if errors.Is(err, s) {
			if p := s.Error() + ": "; len(msg) > len(p) && msg[:len(p)] == p {
				return msg[len(p):]
			}
		}
	}
	return msg
}

func (s *Server) noBranching() error {
	return errf(http.StatusBadRequest, "Branching is not enabled on this node")
}

func branchJSON(b *branching.Branch) (*v1.BranchResponseOutput, error) {
	var id openapi_types.UUID
	if err := id.UnmarshalText([]byte(b.ID)); err != nil {
		return nil, err
	}
	out := &v1.BranchResponseOutput{
		Id: id, Name: b.Name, ProjectRef: b.Ref, ParentProjectRef: b.ParentRef, IsDefault: b.IsDefault,
		Persistent: b.Persistent, Status: v1.BranchResponseOutputStatus(b.State), CreatedAt: b.CreatedAt.UTC(), UpdatedAt: b.UpdatedAt.UTC(),
		WithData: b.WithData,
	}
	if b.GitBranch != "" {
		g := b.GitBranch
		out.GitBranch = &g
	}
	if b.NotifyURL != "" {
		n := b.NotifyURL
		out.NotifyUrl = &n
	}
	if b.ProjectStatus != "" {
		st := v1.BranchResponseOutputPreviewProjectStatus(b.ProjectStatus)
		out.PreviewProjectStatus = &st
	}
	if b.DeletionScheduledAt != nil {
		t := b.DeletionScheduledAt.UTC()
		out.DeletionScheduledAt = &t
	}
	if b.ReviewRequestedAt != nil {
		t := b.ReviewRequestedAt.UTC()
		out.ReviewRequestedAt = &t
	}
	return out, nil
}

func (s *Server) writeBranch(w http.ResponseWriter, status int, b *branching.Branch) error {
	out, err := branchJSON(b)
	if err != nil {
		return err
	}
	writeJSON(w, status, out)
	return nil
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, []any{})
		return nil
	}
	list, err := s.branches.List(r.Context(), r.PathValue("ref"))
	if err != nil {
		return s.branchProjectErr(err)
	}
	out := make([]*v1.BranchResponseOutput, 0, len(list))
	for i := range list {
		j, err := branchJSON(&list[i])
		if err != nil {
			return err
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// branchProjectErr is mapBranchErr for routes whose path names a project: a missing
// project is the usual project 404.
func (s *Server) branchProjectErr(err error) error {
	if errors.Is(err, branching.ErrNotFound) {
		return errNoProject
	}
	return mapBranchErr(err)
}

func (s *Server) createBranch(w http.ResponseWriter, r *http.Request) error {
	var in v1.CreateBranchBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if s.branches == nil {
		return s.noBranching()
	}
	ci := branching.CreateInput{Name: in.BranchName}
	if in.GitBranch != nil {
		ci.GitBranch = *in.GitBranch
	}
	if in.IsDefault != nil {
		ci.IsDefault = *in.IsDefault
	}
	if in.Persistent != nil {
		ci.Persistent = *in.Persistent
	}
	if in.WithData != nil {
		ci.WithData = *in.WithData
	}
	if in.NotifyUrl != nil {
		ci.NotifyURL = *in.NotifyUrl
	}
	if in.DesiredInstanceSize != nil {
		ci.DesiredInstanceSize = string(*in.DesiredInstanceSize)
	}
	b, err := s.branches.Create(r.Context(), r.PathValue("ref"), ci)
	if err != nil {
		return s.branchProjectErr(err)
	}
	return s.writeBranch(w, http.StatusCreated, b)
}

func (s *Server) disableBranching(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return s.noBranching()
	}
	if err := s.branches.DisableBranching(r.Context(), r.PathValue("ref")); err != nil {
		return s.branchProjectErr(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getBranchByName(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	b, err := s.branches.Get(r.Context(), r.PathValue("ref"), r.PathValue("name"))
	if err != nil {
		if errors.Is(err, branching.ErrNotFound) {
			// An unknown project and an unknown branch name are both "branch not found" here.
			return errf(http.StatusNotFound, "Branch not found")
		}
		return mapBranchErr(err)
	}
	return s.writeBranch(w, http.StatusOK, b)
}

// getBranch returns the connection details of a branch (BranchDetailResponse).
func (s *Server) getBranch(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	b, err := s.branches.Resolve(r.Context(), r.PathValue("branch_id_or_ref"))
	if err != nil {
		return mapBranchErr(err)
	}
	p, err := s.loadProject(r.Context(), b.Ref)
	if err != nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	keys, err := s.mgr.Keys(r.Context(), b.Ref)
	if err != nil {
		return err
	}
	user := "postgres." + b.Ref
	out := &v1.BranchDetailResponseOutput{
		Ref: b.Ref, PostgresVersion: pgVersion(p), PostgresEngine: pgEngine(p), ReleaseChannel: "ga",
		Status: v1.BranchDetailResponseOutputStatus(p.Status),
		// The pooler is the one public Postgres endpoint; it routes on the user name.
		DbHost: s.cfg.PoolerHost(), DbPort: s.cfg.Ports.SupavisorSession, DbUser: &user, DbPass: &keys.DBPassword, JwtSecret: &keys.JWTSecret,
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func pgEngine(p *registry.Project) string { return itoa(int64(pgMajor(p))) }

func (s *Server) updateBranch(w http.ResponseWriter, r *http.Request) error {
	var in v1.UpdateBranchBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	b, err := s.branches.Update(r.Context(), r.PathValue("branch_id_or_ref"), branching.UpdateInput{
		Name: in.BranchName, GitBranch: in.GitBranch, Persistent: in.Persistent, NotifyURL: in.NotifyUrl, RequestReview: in.RequestReview,
	})
	if err != nil {
		return mapBranchErr(err)
	}
	return s.writeBranch(w, http.StatusOK, b)
}

func (s *Server) deleteBranch(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	// force=false asks for the soft delete with a grace period; anything else is immediate.
	_, err := s.branches.Delete(r.Context(), r.PathValue("branch_id_or_ref"), branching.DeleteOptions{Schedule: r.URL.Query().Get("force") == "false"})
	if err != nil {
		return mapBranchErr(err)
	}
	writeJSON(w, http.StatusOK, &v1.BranchDeleteResponseOutput{Message: "ok"})
	return nil
}

// branchAction serves merge, reset and push: the work runs in the background and the
// answer is the run id; the branch's status (GET) follows it.
func (s *Server) branchAction(op string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var in v1.BranchActionBody
		if err := decode(r, &in); err != nil {
			return err
		}
		if s.branches == nil {
			return errf(http.StatusNotFound, "Branch not found")
		}
		ai := branching.ActionInput{}
		if in.MigrationVersion != nil {
			ai.MigrationVersion = *in.MigrationVersion
		}
		// The Management API has no force flag; the query parameter is ours.
		ai.Force = r.URL.Query().Get("force") == "true"
		id := r.PathValue("branch_id_or_ref")
		var run string
		var err error
		switch op {
		case "merge":
			run, err = s.branches.Merge(r.Context(), id, ai)
		case "reset":
			run, err = s.branches.Reset(r.Context(), id, ai)
		default:
			run, err = s.branches.Push(r.Context(), id, ai)
		}
		if err != nil {
			return mapBranchErr(err)
		}
		writeJSON(w, http.StatusCreated, &v1.BranchUpdateResponseOutput{Message: "ok", WorkflowRunId: run})
		return nil
	}
}

func (s *Server) restoreBranch(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	if _, err := s.branches.Restore(r.Context(), r.PathValue("branch_id_or_ref")); err != nil {
		return mapBranchErr(err)
	}
	writeJSON(w, http.StatusCreated, &v1.BranchRestoreResponseOutput{Message: "Branch restoration initiated"})
	return nil
}

func (s *Server) diffBranch(w http.ResponseWriter, r *http.Request) error {
	if s.branches == nil {
		return errf(http.StatusNotFound, "Branch not found")
	}
	diff, err := s.branches.Diff(r.Context(), r.PathValue("branch_id_or_ref"))
	if err != nil {
		return mapBranchErr(err)
	}
	writeRaw(w, http.StatusOK, "text/plain; charset=utf-8", []byte(diff))
	return nil
}

// branchRefs returns the refs of the branches of a project, for Studio's project cards.
func (s *Server) branchRefs(ps []registry.Project, ref string) []string {
	out := []string{}
	for _, q := range ps {
		if q.Branch != nil && q.Branch.ParentRef == ref {
			out = append(out, q.Ref)
		}
	}
	return out
}
