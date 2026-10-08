package api

import (
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
)

// The pooler config of a project: the pool size and client limit of its Supavisor tenant. The
// Management API has two sets of routes for it. The public ones are GET and PATCH
// /v1/projects/{ref}/config/database/pooler (the CLI reads and diffs them as db.pooler.*), the
// dashboard's are GET /platform/projects/{ref}/config/supavisor and GET and PATCH
// /platform/projects/{ref}/config/pgbouncer (its database settings page; the platform spec
// still names it after PgBouncer). All of them read and write the same saved settings
// (projectconfig.Pooler); a save updates the tenant in the running Supavisor (EnsureTenant), which
// ends the tenant's pooled connections: clients reconnect.
//
// The shared Supavisor cannot honor every field the specs list. A field it cannot honor is
// refused with 400 and the usual {"message"} body, so a client never believes it got what it
// asked for; a field that already has the value it would get is accepted, because the dashboard
// saves every field it was shown.

// poolerMode is the only mode a project reports: transaction mode on the pooler's transaction
// port. Session mode is the other port of the same Supavisor, for every project, and a client
// chooses it by connecting there.
const poolerMode = "transaction"

// poolerIgnoredParams is what the pooler config reports as ignore_startup_parameters. It is not
// a setting of the shared pooler: it is accepted back unchanged (or empty) and nothing else.
const poolerIgnoredParams = "extra_float_digits"

// pgbouncerOnly are the PgBouncer settings of UpdatePgbouncerConfigBody. Supavisor has no
// counterpart for any of them.
var pgbouncerOnly = []string{"server_idle_timeout", "server_lifetime", "query_wait_timeout", "reserve_pool_size"}

// poolerPatch turns the body of a pooler PATCH (v1 or platform) into the settings to save, or
// refuses it. maxPool is the route's spec limit for default_pool_size.
func (s *Server) poolerPatch(body map[string]any, maxPool float64) (map[string]any, error) {
	patch := map[string]any{}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := body[k]
		switch k {
		case "default_pool_size", "max_client_conn":
			patch[k] = v
			if n, ok := v.(float64); ok && k == "default_pool_size" && n > maxPool {
				return nil, errf(http.StatusBadRequest, "default_pool_size must be between 1 and %d", int(maxPool))
			}
		case "pool_mode":
			if v != nil && v != poolerMode {
				return nil, errf(http.StatusBadRequest, "pool_mode %v cannot be set: the shared pooler serves transaction mode on port %d and session mode on port %d for every project, and a client picks one by the port it connects to",
					quoted(v), s.cfg.Ports.SupavisorTransaction, s.cfg.Ports.SupavisorSession)
			}
		case "ignore_startup_parameters":
			if str, ok := v.(string); v != nil && (!ok || (str != "" && str != poolerIgnoredParams)) {
				return nil, errf(http.StatusBadRequest, "ignore_startup_parameters cannot be changed: the shared pooler has no such setting")
			}
		case "pgbouncer_enabled":
			if v == false {
				return nil, errf(http.StatusBadRequest, "pgbouncer_enabled cannot be turned off: the shared pooler is the only way into the database")
			}
		default:
			if slices.Contains(pgbouncerOnly, k) && v != nil {
				return nil, errf(http.StatusBadRequest, "%s cannot be set: it is a PgBouncer setting and the shared pooler is Supavisor", k)
			}
		}
	}
	return patch, nil
}

func quoted(v any) string { return fmt.Sprintf("%q", fmt.Sprint(v)) }

// poolerValues reads the effective pool size and client limit of a saved state. A value nobody
// saved is the project's size's (lifecycle.Class.PoolSize and PoolerMaxClients, the latter held
// under the node's per-project ceiling), the same the project's pooler tenant runs with.
func (s *Server) poolerValues(p *registry.Project, st *projectconfig.State) (pool, maxClients int64) {
	pool, _ = st.Effective.Int("default_pool_size")
	maxClients, _ = st.Effective.Int("max_client_conn")
	size := sizeOf(p)
	if _, saved := st.Set.Int("default_pool_size"); !saved {
		pool = int64(size.PoolSize)
	}
	if _, saved := st.Set.Int("max_client_conn"); !saved {
		maxClients = int64(min(size.PoolerMaxClients, s.cfg.Fleet.PoolerMaxClients()))
	}
	return pool, maxClients
}

func (s *Server) patchPoolerV1(w http.ResponseWriter, r *http.Request) error {
	const key = "PATCH /v1/projects/{ref}/config/database/pooler"
	p, err := s.settingsProject(r)
	if err != nil {
		return err
	}
	body, err := patchBody(r)
	if err != nil {
		return err
	}
	patch, err := s.poolerPatch(body, 3000)
	if err != nil {
		return err
	}
	ch, _, err := s.saveSettings(r.Context(), p, projectconfig.Pooler, patch, lifecycle.ApplyOptions{})
	if err != nil {
		return err
	}
	pool, _ := s.poolerValues(p, ch.State)
	resp := base(key)
	setAll(resp, map[string]any{"default_pool_size": pool, "pool_mode": poolerMode})
	writeStatusFor(w, key, resp)
	return nil
}

func (s *Server) patchPgbouncer(w http.ResponseWriter, r *http.Request) error {
	const key = "PATCH /platform/projects/{ref}/config/pgbouncer"
	p, err := s.settingsProject(r)
	if err != nil {
		return err
	}
	body, err := patchBody(r)
	if err != nil {
		return err
	}
	patch, err := s.poolerPatch(body, 4950)
	if err != nil {
		return err
	}
	ch, _, err := s.saveSettings(r.Context(), p, projectconfig.Pooler, patch, lifecycle.ApplyOptions{})
	if err != nil {
		return err
	}
	pool, maxClients := s.poolerValues(p, ch.State)
	resp := base(key)
	setAll(resp, map[string]any{
		"pgbouncer_enabled": true, "pgbouncer_status": "ENABLED", "ignore_startup_parameters": poolerIgnoredParams,
		"pool_mode": poolerMode, "default_pool_size": pool, "max_client_conn": maxClients,
		"server_idle_timeout": nil, "server_lifetime": nil,
	})
	writeStatusFor(w, key, resp)
	return nil
}

// poolerState loads the saved pooler settings of p.
func (s *Server) poolerState(r *http.Request, p *registry.Project) (*projectconfig.State, error) {
	return s.settings.Get(r.Context(), p.Ref, projectconfig.Pooler)
}
