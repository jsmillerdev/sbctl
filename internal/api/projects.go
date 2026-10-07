package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	v1 "github.com/OWNER/sbctl/internal/api/gen/v1"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// mapErr turns registry and lifecycle errors into API errors.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, ErrNotFound):
		return errf(http.StatusNotFound, "Not found")
	case errors.Is(err, registry.ErrConflict):
		return errf(http.StatusConflict, "Already exists")
	case errors.Is(err, lifecycle.ErrInvalidState):
		return errf(http.StatusConflict, "%v", err)
	}
	return err
}

func (s *Server) routesProjects(add func(string, handlerFunc)) {
	add("GET /v1/projects", s.v1ListProjects)
	add("POST /v1/projects", s.v1CreateProject)
	add("GET /v1/projects/{ref}", s.v1GetProject)
	add("PATCH /v1/projects/{ref}", s.v1UpdateProject)
	add("DELETE /v1/projects/{ref}", s.v1DeleteProject)
	add("POST /v1/projects/{ref}/pause", s.pauseProject(http.StatusOK))
	add("POST /v1/projects/{ref}/restore", s.restoreProject(http.StatusOK))
	add("POST /v1/projects/{ref}/restart", s.restartProject(http.StatusOK))
	add("GET /v1/projects/{ref}/health", s.v1Health)
	add("GET /v1/projects/{ref}/branches", s.noBranches)
	add("GET /v1/projects/{ref}/branches/{name}", s.branchNotFound)
	add("GET /v1/branches/{branch_id_or_ref}", s.branchNotFound)
	add("GET /v1/projects/{ref}/config/database/pooler", s.v1Pooler)

	add("GET /platform/projects", s.platformListProjects)
	add("POST /platform/projects", s.platformCreateProject)
	add("GET /platform/projects/{ref}", s.platformGetProject)
	add("PATCH /platform/projects/{ref}", s.platformUpdateProject)
	add("DELETE /platform/projects/{ref}", s.platformDeleteProject)
	add("POST /platform/projects/{ref}/pause", s.pauseProject(http.StatusCreated))
	add("POST /platform/projects/{ref}/restore", s.restoreProject(http.StatusOK))
	add("POST /platform/projects/{ref}/restart", s.restartProject(http.StatusCreated))
	add("POST /platform/projects/{ref}/restart-services", s.restartProject(http.StatusCreated))
	add("GET /platform/projects/{ref}/status", s.platformStatus)
	add("GET /platform/organizations/{slug}/projects", s.orgProjects)
}

// v1Project fills the project schema shared by list and get.
func (s *Server) v1Project(ctx context.Context, p *registry.Project) (*v1.V1ProjectWithDatabaseResponseOutput, error) {
	org, err := s.orgOf(ctx, p)
	if err != nil {
		return nil, err
	}
	out := &v1.V1ProjectWithDatabaseResponseOutput{
		CreatedAt: ts(p.CreatedAt), Id: p.Ref, Name: p.Name, OrganizationId: org.Slug,
		OrganizationSlug: org.Slug, Ref: p.Ref, Region: s.regionOf(p),
		Status: v1.V1ProjectWithDatabaseResponseOutputStatus(p.Status),
	}
	out.Database.Host = s.dbHost(p.Ref)
	out.Database.PostgresEngine = fmt.Sprint(pgMajor(p))
	out.Database.ReleaseChannel = "ga"
	out.Database.Version = pgVersion(p)
	return out, nil
}

// regionOf is the region shown for p: a real AWS region code, never a free-form label.
func (s *Server) regionOf(p *registry.Project) string { return s.cfg.ProjectRegion(p.Region) }

func (s *Server) v1ListProjects(w http.ResponseWriter, r *http.Request) error {
	ps, err := s.userProjects(r.Context())
	if err != nil {
		return err
	}
	out := make([]*v1.V1ProjectWithDatabaseResponseOutput, 0, len(ps))
	for i := range ps {
		v, err := s.v1Project(r.Context(), &ps[i])
		if err != nil {
			return err
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) v1GetProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	v, err := s.v1Project(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

// createInput are the fields of the create-project bodies (v1 and platform) we use.
type createInput struct {
	Name             string `json:"name"`
	OrganizationSlug string `json:"organization_slug"`
	// OrganizationID is accepted as the slug: the v1 API's organization ids are slugs.
	OrganizationID string `json:"organization_id"`
	Region         string `json:"region"`
	// DBRegion is what Studio's CreateProjectBody sends instead of region.
	DBRegion string `json:"db_region"`
	DBPass   string `json:"db_pass"`
}

// createProject provisions a project through the lifecycle manager and returns it
// as soon as it is visible in the registry (COMING_UP), or after createWait with
// the allocated ref as COMING_UP. Provisioning continues in the background; a
// failure after that answer is logged and recorded as an INIT_FAILED registry row,
// so the ref the client holds does not 404.
func (s *Server) createProject(ctx context.Context, in createInput) (*registry.Project, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, errf(http.StatusBadRequest, "name is required")
	}
	var org *registry.Organization
	var err error
	if in.OrganizationSlug == "" {
		in.OrganizationSlug = in.OrganizationID
	}
	if in.OrganizationSlug == "" {
		org, err = s.defaultOrg(ctx)
	} else {
		org, err = s.orgBySlug(ctx, in.OrganizationSlug)
	}
	if err != nil {
		return nil, err
	}
	if in.Region == "" {
		in.Region = in.DBRegion
	}
	ref := secrets.NewRef()
	req := lifecycle.CreateRequest{Name: strings.TrimSpace(in.Name), OrgSlug: org.Slug, Region: in.Region, Ref: ref, DBPassword: in.DBPass}
	type result struct {
		p   *registry.Project
		err error
	}
	done := make(chan result, 1)
	var answeredComingUp atomic.Bool
	endOp, err := s.beginOp()
	if err != nil {
		return nil, err
	}
	go func() {
		defer endOp()
		// The request may end before provisioning does.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), createTimeout)
		defer cancel()
		p, err := s.mgr.Create(bg, req)
		if err != nil {
			s.log.Error("project creation failed", "ref", ref, "err", err)
		}
		done <- result{p, err}
		if err != nil && answeredComingUp.Load() {
			s.recordInitFailed(bg, ref, org.ID, req)
		}
	}()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(s.createWait)
	for {
		select {
		case res := <-done:
			if res.err != nil {
				return nil, mapErr(res.err)
			}
			return res.p, nil
		case <-tick.C:
			if p, err := s.reg.GetProject(ctx, ref); err == nil {
				return p, nil
			}
		case <-timeout:
			// Provisioning keeps running, so the project may still appear: a 504 would
			// invite a retry that creates a second one. Answer with the allocated ref
			// as COMING_UP; clients poll the project by ref.
			s.log.Warn("project creation is slow; answering COMING_UP", "ref", ref)
			answeredComingUp.Store(true)
			select {
			case res := <-done: // it failed in the instant before the flag was set
				if res.err != nil {
					return nil, mapErr(res.err)
				}
				return res.p, nil
			default:
			}
			return &registry.Project{Ref: ref, OrgID: org.ID, Name: req.Name, Region: req.Region, Status: registry.StatusComingUp, CreatedAt: s.now()}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// recordInitFailed marks ref INIT_FAILED after a failed background Create, inserting
// the row when Create did not get as far as writing one.
func (s *Server) recordInitFailed(ctx context.Context, ref string, orgID int64, req lifecycle.CreateRequest) {
	err := s.reg.SetProjectStatus(ctx, ref, registry.StatusInitFailed)
	if errors.Is(err, registry.ErrNotFound) {
		err = s.reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: orgID, Name: req.Name, Region: req.Region, Status: registry.StatusInitFailed})
	}
	if err != nil {
		s.log.Error("recording INIT_FAILED failed", "ref", ref, "err", err)
	}
}

func (s *Server) v1CreateProject(w http.ResponseWriter, r *http.Request) error {
	var in createInput
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.createProject(r.Context(), in)
	if err != nil {
		return err
	}
	org, err := s.orgOf(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, &v1.V1ProjectResponseOutput{
		CreatedAt: ts(p.CreatedAt), Id: p.Ref, Name: p.Name, OrganizationId: org.Slug,
		OrganizationSlug: org.Slug, Ref: p.Ref, Region: s.regionOf(p), Status: v1.V1ProjectResponseOutputStatus(p.Status),
	})
	return nil
}

func (s *Server) v1UpdateProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.renameProject(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &v1.V1ProjectRefResponseOutput{Id: p.Seq, Name: p.Name, Ref: p.Ref})
	return nil
}

func (s *Server) renameProject(r *http.Request) (*registry.Project, error) {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return nil, err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if n := strings.TrimSpace(in.Name); n != "" && n != p.Name {
		p.Name = n
		if err := s.reg.UpdateProject(r.Context(), p); err != nil {
			return nil, mapErr(err)
		}
	}
	return p, nil
}

func (s *Server) v1DeleteProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.deleteProject(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &v1.V1ProjectRefResponseOutput{Id: p.Seq, Name: p.Name, Ref: p.Ref})
	return nil
}

func (s *Server) deleteProject(r *http.Request) (*registry.Project, error) {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return nil, err
	}
	ctx, cancel, err := s.detach(r, deleteTimeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err := s.mgr.Delete(ctx, p.Ref); err != nil {
		return nil, mapErr(err)
	}
	s.functionsGone(ctx, p.Ref)
	return p, nil
}

func (s *Server) pauseProject(status int) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		ctx, cancel, err := s.detach(r, lifecycleTimeout)
		if err != nil {
			return err
		}
		defer cancel()
		if err := s.mgr.Pause(ctx, p.Ref); err != nil {
			return mapErr(err)
		}
		s.functionsGone(ctx, p.Ref)
		w.WriteHeader(status)
		return nil
	}
}

func (s *Server) restoreProject(status int) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		ctx, cancel, err := s.detach(r, lifecycleTimeout)
		if err != nil {
			return err
		}
		defer cancel()
		if err := s.mgr.Resume(ctx, p.Ref); err != nil {
			return mapErr(err)
		}
		s.functionsGone(ctx, p.Ref)
		w.WriteHeader(status)
		return nil
	}
}

// restartProject pauses and resumes: there is no in-place restart in Manager. Both
// calls run as one detached unit, so a client that leaves between them cannot strand the
// project paused.
func (s *Server) restartProject(status int) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		ctx, cancel, err := s.detach(r, 2*lifecycleTimeout)
		if err != nil {
			return err
		}
		defer cancel()
		// The intent is recorded so that a daemon stop between the two calls does not
		// leave the project paused: Engine.Recover resumes it (lifecycle.EventRestart*).
		s.recordEvent(ctx, p.Ref, lifecycle.EventRestartRequested, nil)
		defer func() { s.recordEvent(ctx, p.Ref, lifecycle.EventRestartFinished, nil) }()
		if err := s.mgr.Pause(ctx, p.Ref); err != nil {
			return mapErr(err)
		}
		if err := s.mgr.Resume(ctx, p.Ref); err != nil {
			return mapErr(err)
		}
		s.functionsGone(ctx, p.Ref)
		w.WriteHeader(status)
		return nil
	}
}

// Bounds for lifecycle operations that outlive their HTTP request. Delete includes a
// final base backup, so it gets the longest.
const (
	createTimeout    = 20 * time.Minute
	lifecycleTimeout = 10 * time.Minute
	deleteTimeout    = 30 * time.Minute
)

// detach returns a context for a lifecycle mutation that keeps the request's values but
// not its cancellation: a client that disconnects (Ctrl-C on `supabase projects delete`,
// a closed Studio tab, a proxy idle timeout) must not stop the operation halfway and
// leave a project half deleted or stopped. The timeout bounds a stuck operation. The
// operation is registered for Drain, and the returned func ends it: a shutdown waits for
// it, and a server that is already draining refuses it with 503 before anything starts.
func (s *Server) detach(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	end, err := s.beginOp()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	return ctx, func() { cancel(); end() }, nil
}

func (s *Server) recordEvent(ctx context.Context, ref, kind string, payload any) {
	if err := s.reg.AppendEvent(ctx, ref, kind, payload); err != nil {
		s.log.Warn("could not record event", "ref", ref, "kind", kind, "err", err)
	}
}

// healthName maps service names of the lifecycle manager to the API's.
var healthName = map[string]string{
	config.SvcGoTrue: "auth", config.SvcPostgres: "db", config.SvcPostgREST: "rest",
	config.SvcSupavisor: "pooler", config.SvcRealtime: "realtime", config.SvcStorage: "storage",
}

// allHealthNames are the services a health request can name, in report order.
var allHealthNames = []string{"auth", "db", "rest", "realtime", "storage", "pooler"}

func (s *Server) v1Health(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, v := range r.URL.Query()["services"] {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				want[n] = true
			}
		}
	}
	reported := map[string]lifecycle.ServiceHealth{}
	if p.Status == registry.StatusActiveHealthy || p.Status == registry.StatusActiveUnhealthy || p.Status == registry.StatusComingUp {
		hs, err := s.mgr.Health(r.Context(), p.Ref)
		if err != nil {
			s.log.Warn("health check failed", "ref", p.Ref, "err", err)
		}
		for _, h := range hs {
			if n, ok := healthName[h.Name]; ok {
				reported[n] = h
			}
		}
	}
	out := make([]v1.V1ServiceHealthResponseOutput, 0, len(allHealthNames))
	for _, n := range allHealthNames {
		if len(want) > 0 && !want[n] {
			continue
		}
		e := v1.V1ServiceHealthResponseOutput{Name: v1.V1ServiceHealthResponseOutputName(n)}
		if h, ok := reported[n]; ok {
			e.Healthy, e.Status = h.Healthy, v1.V1ServiceHealthResponseOutputStatus(h.Status)
			if h.Error != "" {
				msg := h.Error
				e.Error = &msg
			}
		} else {
			// Fleet services are not tracked per project: they follow the project.
			e.Healthy = p.Status == registry.StatusActiveHealthy
			e.Status = v1.V1ServiceHealthResponseOutputStatus(map[bool]string{true: "ACTIVE_HEALTHY", false: "COMING_UP"}[e.Healthy])
		}
		if e.Status == "" {
			e.Status = "UNHEALTHY"
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) noBranches(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, []any{})
	return nil
}

func (s *Server) branchNotFound(w http.ResponseWriter, r *http.Request) error {
	return errf(http.StatusNotFound, "Branch not found")
}

// v1Pooler describes the shared Supavisor entry of a project. The password is a
// placeholder: it never leaves the secret store through this route.
func (s *Server) v1Pooler(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	user := "postgres." + p.Ref
	conn := fmt.Sprintf("postgres://%s:[YOUR-PASSWORD]@%s:%d/postgres", user, s.cfg.PoolerHost(), s.cfg.Ports.SupavisorTransaction)
	mode := v1.SupavisorConfigResponseOutputPoolMode("transaction")
	size, maxConn := 15, 200
	writeJSON(w, http.StatusOK, []v1.SupavisorConfigResponseOutput{{
		ConnectionString: conn, ConnectionStringSnake: conn, DatabaseType: "PRIMARY",
		DbHost: s.cfg.PoolerHost(), DbName: "postgres", DbPort: s.cfg.Ports.SupavisorTransaction, DbUser: user,
		DefaultPoolSize: &size, Identifier: p.Ref, IsUsingScramAuth: true, MaxClientConn: &maxConn, PoolMode: mode,
	}})
	return nil
}
