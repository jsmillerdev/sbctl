package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/projectconfig"
	"github.com/OWNER/sbctl/internal/registry"
)

// applyTimeout bounds applying a saved setting to the running service: restarting GoTrue or
// PostgREST and waiting for it to answer, or restarting PostgreSQL.
const applyTimeout = 5 * time.Minute

// cfgLock serializes the save, apply and undo of one service's settings of one project, so
// an undo after a failed apply cannot wipe out a concurrent save.
func (s *Server) cfgLock(ref string, svc projectconfig.Service) func() {
	m, _ := s.cfgLocks.LoadOrStore(ref+"/"+string(svc), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// memoryLimit parses the project's systemd MemoryMax ("1G", "512M", bytes); 0 when unset
// or not a plain size.
func memoryLimit(p *registry.Project) int64 {
	v := strings.TrimSpace(p.Limits.MemoryMax)
	if v == "" || v == "infinity" {
		return 0
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	}
	if mult != 1 {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * mult
}

// asValidation turns a settings validation error into the 400 of the API.
func asValidation(err error) error {
	var ve *projectconfig.ValidationError
	if errors.As(err, &ve) {
		return errf(http.StatusBadRequest, "%s", ve.Msg)
	}
	return err
}

// saveSettings validates and saves patch as the settings of svc and applies the change to
// the running project. When applying fails the previous settings are saved again and
// applied (best effort), so a rejected save leaves the project as it was. The apply runs on
// a context that outlives the request, like every lifecycle operation: a client that leaves
// partway must not strand a half-restarted service.
func (s *Server) saveSettings(ctx context.Context, p *registry.Project, svc projectconfig.Service, patch map[string]any, opts lifecycle.ApplyOptions) (*projectconfig.Change, lifecycle.ApplyResult, error) {
	// A save that changes nothing is applied only when a restart is asked for: a setting saved
	// earlier may still wait for one.
	applyAnyway := opts.RestartDatabase
	done, err := s.beginOp()
	if err != nil {
		return nil, lifecycle.ApplyResult{}, err
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	defer s.cfgLock(p.Ref, svc)()

	prior, err := s.settings.Get(ctx, p.Ref, svc)
	if err != nil {
		return nil, lifecycle.ApplyResult{}, err
	}
	ch, err := s.settings.Patch(ctx, p.Ref, svc, patch, projectconfig.CrossContext{MemoryLimit: memoryLimit(p)})
	if err != nil {
		return nil, lifecycle.ApplyResult{}, asValidation(err)
	}
	if len(ch.Changed) == 0 && !applyAnyway {
		return ch, lifecycle.ApplyResult{}, nil
	}
	rc, ok := s.mgr.(lifecycle.Reconfigurer)
	if !ok {
		s.log.Warn("settings saved but the lifecycle manager cannot apply them", "ref", p.Ref, "service", svc)
		return ch, lifecycle.ApplyResult{}, nil
	}
	res, err := rc.ApplyConfig(ctx, p.Ref, svc, opts)
	if err == nil {
		return ch, res, nil
	}
	s.log.Error("applying settings failed; restoring the previous ones", "ref", p.Ref, "service", svc, "err", err)
	if rerr := s.settings.Restore(ctx, p.Ref, svc, prior); rerr != nil {
		s.log.Error("could not restore the previous settings", "ref", p.Ref, "service", svc, "err", rerr)
	} else if _, aerr := rc.ApplyConfig(ctx, p.Ref, svc, lifecycle.ApplyOptions{}); aerr != nil {
		s.log.Error("could not apply the restored settings", "ref", p.Ref, "service", svc, "err", aerr)
	}
	var inv *Error
	if errors.As(err, &inv) {
		return nil, lifecycle.ApplyResult{}, inv
	}
	if errors.Is(err, lifecycle.ErrInvalidState) {
		return nil, lifecycle.ApplyResult{}, errf(http.StatusConflict, "The project is not in a state that accepts this change: %v", err)
	}
	return nil, lifecycle.ApplyResult{}, errf(http.StatusBadGateway, "The %s service did not accept the new settings, which were rolled back: %v", svc, err)
}

// settingsProject loads the project of the request and refuses states in which settings
// cannot be applied (a paused project accepts them: they apply when it resumes).
func (s *Server) settingsProject(r *http.Request) (*registry.Project, error) {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return nil, err
	}
	switch p.Status {
	case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy, registry.StatusInactive:
		return p, nil
	}
	return nil, errf(http.StatusServiceUnavailable, "Project is not ready (status %s)", p.Status)
}

// patchBody reads the JSON object of a request (an empty body is an empty object).
func patchBody(r *http.Request) (map[string]any, error) {
	m := map[string]any{}
	if err := decode(r, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// secretHash is how a GET reports a secret setting; see projectconfig.Redact.
func secretView(f *projectconfig.Field, v any) any {
	if f.Secret {
		if s, ok := v.(string); ok && s != "" {
			return projectconfig.Redact(s)
		}
	}
	return v
}

// detached returns a context that outlives the request, bounded by d.
func detached(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), d)
}

// fillView writes the effective value of every setting of the schema into resp.
func fillView(resp map[string]any, st *projectconfig.State, sch *projectconfig.Schema) {
	for i := range sch.Fields {
		f := &sch.Fields[i]
		if v, ok := st.Effective[f.Name]; ok {
			resp[f.Name] = secretView(f, v)
		} else {
			resp[f.Name] = nil
		}
	}
}

// writeStatusFor answers a successful write with the status the spec gives key, and the
// body unless that status carries none.
func writeStatusFor(w http.ResponseWriter, key string, body any) {
	status := http.StatusOK
	if op := operationByKey(key); op != nil && op.Status != 0 {
		status = op.Status
	}
	if status == http.StatusNoContent {
		writeNoContent(w)
		return
	}
	writeJSON(w, status, body)
}
