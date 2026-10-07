package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/projectconfig"
)

func (s *Server) routesConfig(add func(string, handlerFunc)) {
	s.routesConfigAuth(add)
	add("GET /v2/projects/{ref}/config", s.v2Config)
	s.routesDatabasePassword(add)
	s.routesStorageActions(add)

	// PostgREST: the CLI's [api] section and Studio's API settings page.
	add("GET /v1/projects/{ref}/postgrest", s.getPostgREST("GET /v1/projects/{ref}/postgrest", false))
	add("PATCH /v1/projects/{ref}/postgrest", s.patchPostgREST("PATCH /v1/projects/{ref}/postgrest"))
	add("GET /platform/projects/{ref}/config/postgrest", s.getPostgREST("GET /platform/projects/{ref}/config/postgrest", true))
	add("PATCH /platform/projects/{ref}/config/postgrest", s.patchPostgREST("PATCH /platform/projects/{ref}/config/postgrest"))

	// Realtime.
	add("GET /v1/projects/{ref}/config/realtime", s.getRealtime("GET /v1/projects/{ref}/config/realtime"))
	add("PATCH /v1/projects/{ref}/config/realtime", s.patchRealtime("PATCH /v1/projects/{ref}/config/realtime", "GET /v1/projects/{ref}/config/realtime"))
	add("GET /platform/projects/{ref}/config/realtime", s.getRealtime("GET /platform/projects/{ref}/config/realtime"))
	add("PATCH /platform/projects/{ref}/config/realtime", s.patchRealtime("PATCH /platform/projects/{ref}/config/realtime", "GET /platform/projects/{ref}/config/realtime"))

	// Storage.
	add("GET /v1/projects/{ref}/config/storage", s.getStorage("GET /v1/projects/{ref}/config/storage"))
	add("PATCH /v1/projects/{ref}/config/storage", s.patchStorage("PATCH /v1/projects/{ref}/config/storage", "GET /v1/projects/{ref}/config/storage"))
	add("GET /platform/projects/{ref}/config/storage", s.getStorage("GET /platform/projects/{ref}/config/storage"))
	add("PATCH /platform/projects/{ref}/config/storage", s.patchStorage("PATCH /platform/projects/{ref}/config/storage", "GET /platform/projects/{ref}/config/storage"))

	// Postgres.
	add("GET /v1/projects/{ref}/config/database/postgres", s.getPostgres)
	add("PUT /v1/projects/{ref}/config/database/postgres", s.putPostgres)
}

// ---- PostgREST -------------------------------------------------------------------

func (s *Server) postgrestView(key string, st *projectconfig.State, jwtSecret string, platform bool) map[string]any {
	resp := base(key)
	fillView(resp, st, projectconfig.PostgRESTSchema)
	if jwtSecret != "" {
		resp["jwt_secret"] = jwtSecret
	}
	if platform {
		resp["db_anon_role"], resp["role_claim_key"] = "anon", ".role"
	}
	return resp
}

func (s *Server) getPostgREST(key string, platform bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		keys, err := s.mgr.Keys(r.Context(), p.Ref)
		if err != nil {
			return err
		}
		st, err := s.settings.Get(r.Context(), p.Ref, projectconfig.PostgREST)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, s.postgrestView(key, st, keys.JWTSecret, platform))
		return nil
	}
}

func (s *Server) patchPostgREST(key string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.settingsProject(r)
		if err != nil {
			return err
		}
		in, err := patchBody(r)
		if err != nil {
			return err
		}
		ch, _, err := s.saveSettings(r.Context(), p, projectconfig.PostgREST, in, lifecycle.ApplyOptions{})
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, s.postgrestView(key, ch.State, "", false))
		return nil
	}
}

// ---- Realtime --------------------------------------------------------------------

func realtimeView(key string, st *projectconfig.State) map[string]any {
	resp := base(key)
	fillView(resp, st, projectconfig.RealtimeSchema)
	if _, ok := resp["admin_suspended_at"]; ok {
		resp["admin_suspended_at"] = nil
	}
	return resp
}

func (s *Server) getRealtime(key string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		st, err := s.settings.Get(r.Context(), p.Ref, projectconfig.Realtime)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, realtimeView(key, st))
		return nil
	}
}

func (s *Server) patchRealtime(key, viewKey string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.settingsProject(r)
		if err != nil {
			return err
		}
		in, err := patchBody(r)
		if err != nil {
			return err
		}
		ch, _, err := s.saveSettings(r.Context(), p, projectconfig.Realtime, in, lifecycle.ApplyOptions{})
		if err != nil {
			return err
		}
		writeStatusFor(w, key, realtimeView(viewKey, ch.State))
		return nil
	}
}

// ---- Storage ---------------------------------------------------------------------

// storageCapabilities are what the fleet's Storage serves: the v2 object listing yes, Iceberg
// catalogs (they need a service sbctl does not run) and object versioning no.
var storageCapabilities = map[string]any{"iceberg_catalog": false, "list_v2": true, "object_versioning": false}

func storageView(key string, st *projectconfig.State) map[string]any {
	resp := base(key)
	n, _ := st.Effective.Int("fileSizeLimit")
	setAll(resp, map[string]any{
		"fileSizeLimit": n, "features": st.StorageFeatures(), "external": st.StorageExternal(),
		"capabilities": storageCapabilities, "migrationVersion": nil,
	})
	return resp
}

func (s *Server) getStorage(key string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		st, err := s.settings.Get(r.Context(), p.Ref, projectconfig.Storage)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, storageView(key, st))
		return nil
	}
}

func (s *Server) patchStorage(key, viewKey string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.settingsProject(r)
		if err != nil {
			return err
		}
		in, err := patchBody(r)
		if err != nil {
			return err
		}
		ch, _, err := s.saveSettings(r.Context(), p, projectconfig.Storage, in, lifecycle.ApplyOptions{})
		if err != nil {
			return err
		}
		// The v1 spec answers an empty 200; Studio's platform route the new config.
		if op := operationByKey(key); op != nil && !op.JSON {
			w.WriteHeader(http.StatusOK)
			return nil
		}
		writeJSON(w, http.StatusOK, storageView(viewKey, ch.State))
		return nil
	}
}

// ---- Postgres --------------------------------------------------------------------

// pgConfigView reports, for every setting hosted lets a project change, the saved value, else
// the value the running cluster has. restart_database is a request flag, never reported.
func (s *Server) pgConfigView(ctx context.Context, ref string, st *projectconfig.State) (map[string]any, error) {
	resp := base("GET /v1/projects/{ref}/config/database/postgres")
	live := s.livePostgresSettings(ctx, ref)
	for i := range projectconfig.PostgresSchema.Fields {
		f := &projectconfig.PostgresSchema.Fields[i]
		if v, ok := st.Set[f.Name]; ok {
			resp[f.Name] = v
		} else if v, ok := live[f.Name]; ok {
			resp[f.Name] = v
		}
	}
	return resp, nil
}

// livePostgresSettings reads the settings from the running cluster with the postgres role; an
// unreachable cluster (paused project) simply has none to report.
func (s *Server) livePostgresSettings(ctx context.Context, ref string) map[string]any {
	out := map[string]any{}
	dsn, err := s.mgr.ConnString(ctx, ref, "postgres")
	if err != nil {
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, 5e9)
	defer cancel()
	conn, err := pgx.Connect(cctx, dsn)
	if err != nil {
		return out
	}
	defer conn.Close(context.WithoutCancel(ctx))
	names := make([]string, len(projectconfig.PostgresSchema.Fields))
	for i, f := range projectconfig.PostgresSchema.Fields {
		names[i] = f.Name
	}
	rows, err := conn.Query(cctx, `select n, current_setting(n, true) from unnest($1::text[]) as n`, names)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var val *string
		if rows.Scan(&name, &val) != nil || val == nil {
			continue
		}
		f, _ := projectconfig.PostgresSchema.Field(name)
		switch f.Kind {
		case projectconfig.Bool:
			out[name] = *val == "on" || *val == "true"
		case projectconfig.Int:
			if n, err := strconv.ParseInt(*val, 10, 64); err == nil {
				out[name] = n
			}
		default:
			out[name] = strings.ReplaceAll(*val, " ", "")
		}
	}
	return out
}

func (s *Server) getPostgres(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	st, err := s.settings.Get(r.Context(), p.Ref, projectconfig.Postgres)
	if err != nil {
		return err
	}
	resp, err := s.pgConfigView(r.Context(), p.Ref, st)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) putPostgres(w http.ResponseWriter, r *http.Request) error {
	p, err := s.settingsProject(r)
	if err != nil {
		return err
	}
	in, err := patchBody(r)
	if err != nil {
		return err
	}
	restart, _ := in["restart_database"].(bool)
	delete(in, "restart_database")
	ch, res, err := s.saveSettings(r.Context(), p, projectconfig.Postgres, in, lifecycle.ApplyOptions{RestartDatabase: restart})
	if err != nil {
		return err
	}
	if res.PendingRestart {
		s.log.Info("postgres settings saved; they take effect at the next restart of the database", "ref", p.Ref)
	}
	resp, err := s.pgConfigView(r.Context(), p.Ref, ch.State)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}
