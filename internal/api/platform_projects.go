package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	plat "github.com/OWNER/sbctl/internal/api/gen/platform"
	"github.com/OWNER/sbctl/internal/registry"
)

// elem returns a minimal valid element of the array at property prop of the object
// response of "METHOD /path" (prop "" means the response is itself the array).
func elem(key, prop string) map[string]any {
	op := operationByKey(key)
	if op == nil || op.Response == nil || op.Response.Value == nil {
		panic("api: no response schema for " + key)
	}
	items := op.Response.Value.Items
	if prop != "" {
		p := op.Response.Value.Properties[prop]
		if p == nil || p.Value == nil {
			panic("api: " + key + " has no property " + prop)
		}
		items = p.Value.Items
	}
	if items == nil {
		panic("api: " + key + " has no array at " + prop)
	}
	m, _ := MinimalValue(items).(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// connectionPlaceholder is the connectionString Studio requires to be non-empty
// before it will call pg-meta. It carries no password: the pg-meta proxy ignores the
// header Studio sends back and builds its own from the lifecycle manager.
func (s *Server) connectionPlaceholder(p *registry.Project) string {
	return fmt.Sprintf("postgresql://postgres:[YOUR-PASSWORD]@%s:%d/postgres", s.dbHost(p.Ref), s.cfg.Ports.SupavisorSession)
}

func (s *Server) platformProject(p *registry.Project, org *registry.Organization) plat.ProjectDetailResponseOutput {
	conn := s.connectionPlaceholder(p)
	ver := pgVersion(p)
	return plat.ProjectDetailResponseOutput{
		CloudProvider: "AWS", ConnectionString: &conn, DbVersion: &ver, DbHost: s.dbHost(p.Ref),
		Id: projectNumID(p), InsertedAt: ts(p.CreatedAt), UpdatedAt: ts(p.UpdatedAt),
		Name: p.Name, OrganizationId: float32(org.ID), Ref: p.Ref, Region: regionOf(p),
		RestUrl: s.projectURL(p.Ref) + "/rest/v1/", Status: plat.ProjectDetailResponseOutputStatus(p.Status),
		SubscriptionId: "sbctl",
	}
}

func (s *Server) platformGetProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	org, err := s.orgOf(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.platformProject(p, org))
	return nil
}

func (s *Server) platformUpdateProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.renameProject(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &plat.ProjectRefResponseOutput{Id: projectNumID(p), Name: p.Name, Ref: p.Ref})
	return nil
}

func (s *Server) platformDeleteProject(w http.ResponseWriter, r *http.Request) error {
	p, err := s.deleteProject(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &plat.RemoveProjectResponseOutput{Id: projectNumID(p), Name: p.Name, Ref: p.Ref})
	return nil
}

func (s *Server) platformStatus(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(p.Status)})
	return nil
}

func (s *Server) platformListProjects(w http.ResponseWriter, r *http.Request) error {
	ps, err := s.userProjects(r.Context())
	if err != nil {
		return err
	}
	limit, offset := queryInt(r, "limit", 100), queryInt(r, "offset", 0)
	if search := r.URL.Query().Get("search"); search != "" {
		ps = filterProjects(ps, search)
	}
	total := len(ps)
	if offset > total {
		offset = total
	}
	ps = ps[offset:min(total, offset+limit)]
	rows := make([]any, 0, len(ps))
	for i := range ps {
		p := &ps[i]
		org, err := s.orgOf(r.Context(), p)
		if err != nil {
			return err
		}
		row := elem("GET /platform/projects", "projects")
		rows = append(rows, setAll(row, map[string]any{
			"cloud_provider": "AWS", "id": projectNumID(p), "inserted_at": ts(p.CreatedAt), "is_branch_enabled": false,
			"is_physical_backups_enabled": false, "name": p.Name, "organization_id": org.ID, "organization_slug": org.Slug,
			"preview_branch_refs": []string{}, "ref": p.Ref, "region": regionOf(p), "status": string(p.Status), "subscription_id": "sbctl",
		}))
	}
	resp := base("GET /platform/projects")
	set(resp, "projects", rows)
	set(resp, "pagination", map[string]any{"count": total, "limit": limit, "offset": offset})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func filterProjects(ps []registry.Project, q string) []registry.Project {
	out := ps[:0:0]
	for _, p := range ps {
		if containsFold(p.Name, q) || containsFold(p.Ref, q) {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) platformCreateProject(w http.ResponseWriter, r *http.Request) error {
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
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		// The project exists but its keys are not readable yet: answer without them
		// rather than failing a creation that succeeded.
		s.log.Warn("keys unavailable right after create", "ref", p.Ref, "err", err)
		keys = nil
	}
	resp := base("POST /platform/projects")
	setAll(resp, map[string]any{
		"cloud_provider": "AWS", "endpoint": s.projectURL(p.Ref), "id": projectNumID(p), "inserted_at": ts(p.CreatedAt),
		"is_branch_enabled": false, "is_physical_backups_enabled": false, "name": p.Name, "organization_id": org.ID,
		"organization_slug": org.Slug, "preview_branch_refs": []string{}, "ref": p.Ref, "region": regionOf(p),
		"status": string(p.Status), "subscription_id": "sbctl",
	})
	if keys != nil {
		set(resp, "anon_key", keys.AnonKey)
		set(resp, "service_key", keys.ServiceRoleKey)
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

func (s *Server) orgProjects(w http.ResponseWriter, r *http.Request) error {
	org, err := s.orgBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	ps, err := s.userProjects(r.Context())
	if err != nil {
		return err
	}
	def, err := s.defaultOrg(r.Context())
	if err != nil {
		return err
	}
	var mine []registry.Project
	for _, p := range ps {
		if p.OrgID == org.ID || (p.OrgID == 0 && def.ID == org.ID) {
			mine = append(mine, p)
		}
	}
	if search := r.URL.Query().Get("search"); search != "" {
		mine = filterProjects(mine, search)
	}
	total := len(mine)
	limit, offset := queryInt(r, "limit", 100), queryInt(r, "offset", 0)
	if offset > total {
		offset = total
	}
	mine = mine[offset:min(total, offset+limit)]
	rows := make([]any, 0, len(mine))
	const key = "GET /platform/organizations/{slug}/projects"
	for _, p := range mine {
		row := elem(key, "projects")
		db := map[string]any{}
		if dbs, ok := row["databases"].([]any); ok && len(dbs) > 0 {
			db, _ = dbs[0].(map[string]any)
		} else if ops := operationByKey(key); ops != nil {
			// databases has no minItems: build one element from the item schema.
			item := ops.Response.Value.Properties["projects"].Value.Items.Value.Properties["databases"].Value.Items
			db, _ = MinimalValue(item).(map[string]any)
		}
		setAll(db, map[string]any{"cloud_provider": "AWS", "identifier": p.Ref, "region": regionOf(&p), "status": string(p.Status), "type": "PRIMARY"})
		setAll(row, map[string]any{
			"cloud_provider": "AWS", "databases": []any{db}, "inserted_at": ts(p.CreatedAt), "integration_source": nil,
			"is_branch": false, "name": p.Name, "ref": p.Ref, "region": regionOf(&p), "status": string(p.Status),
		})
		rows = append(rows, row)
	}
	resp := base(key)
	set(resp, "projects", rows)
	set(resp, "pagination", map[string]any{"count": total, "limit": limit, "offset": offset})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) routesPlatformProject(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/settings", s.platformSettings)
	add("GET /platform/projects/{ref}/config/postgrest", s.platformPostgrestConfig)
	add("POST /platform/projects/{ref}/api-keys/temporary", s.temporaryKey)
	add("GET /v2/projects/{ref}/config", s.v2Config)
	add("GET /platform/database/{ref}/backups", s.platformBackups)
	add("GET /platform/projects/{ref}/config/storage", s.storageConfig("GET /platform/projects/{ref}/config/storage"))
	add("GET /v1/projects/{ref}/config/storage", s.storageConfig("GET /v1/projects/{ref}/config/storage"))
	add("GET /platform/projects/{ref}/config/pgbouncer", s.pgbouncerConfig)
	add("GET /platform/projects/{ref}/config/supavisor", s.v1Pooler)
}

// storageConfig reports Storage's defaults. Per-project storage settings are not
// stored yet, so PATCH keeps answering with a stub.
func (s *Server) storageConfig(key string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
			return err
		}
		resp := base(key)
		set(resp, "fileSizeLimit", 50*1024*1024) // Storage's FILE_SIZE_LIMIT default
		writeJSON(w, http.StatusOK, resp)
		return nil
	}
}

// pgbouncerConfig describes the shared pooler entry (Supavisor) in the shape the
// dashboard's database settings page reads.
func (s *Server) pgbouncerConfig(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	user := "postgres." + p.Ref
	resp := base("GET /platform/projects/{ref}/config/pgbouncer")
	setAll(resp, map[string]any{
		"connection_string": fmt.Sprintf("postgres://%s:[YOUR-PASSWORD]@%s:%d/postgres", user, s.cfg.PoolerHost(), s.cfg.Ports.SupavisorTransaction),
		"db_dns_name":       s.cfg.PoolerHost(), "db_host": s.cfg.PoolerHost(), "db_name": "postgres",
		"db_port": s.cfg.Ports.SupavisorTransaction, "db_user": user, "inserted_at": ts(p.CreatedAt),
		"pgbouncer_enabled": true, "pool_mode": "transaction", "ssl_enforced": false,
	})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) platformSettings(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	resp := base("GET /platform/projects/{ref}/settings")
	setAll(resp, map[string]any{
		"cloud_provider": "AWS", "db_dns_name": s.dbHost(p.Ref), "db_host": s.dbHost(p.Ref), "db_name": "postgres",
		"db_port": s.cfg.Ports.SupavisorSession, "db_user": "postgres", "inserted_at": ts(p.CreatedAt), "name": p.Name,
		"ref": p.Ref, "region": regionOf(p), "ssl_enforced": false, "status": string(p.Status), "is_sensitive": false,
		"app_config": map[string]any{
			"db_schema": "public", "endpoint": s.cfg.ProjectHost(p.Ref), "storage_endpoint": s.cfg.ProjectHost(p.Ref),
			"protocol": s.publicScheme(),
		},
	})
	if keys, err := s.mgr.Keys(r.Context(), p.Ref); err == nil {
		set(resp, "jwt_secret", keys.JWTSecret)
		set(resp, "service_api_keys", []map[string]string{
			{"name": "anon key", "tags": "anon", "api_key": keys.AnonKey},
			{"name": "service_role key", "tags": "service_role", "api_key": keys.ServiceRoleKey},
		})
	} else {
		s.log.Warn("keys unavailable for settings", "ref", p.Ref, "err", err)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) platformPostgrestConfig(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &plat.GetPostgrestConfigResponseOutput{
		DbAnonRole: "anon", DbExtraSearchPath: "public, extensions", DbSchema: "public, storage, graphql_public",
		JwtSecret: keys.JWTSecret, MaxRows: 1000, RoleClaimKey: ".role",
	})
	return nil
}

// temporaryKey issues a short-lived service_role JWT for the dashboard's own calls.
func (s *Server) temporaryKey(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	now := s.now()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "supabase", "ref": p.Ref, "role": "service_role", "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}).SignedString([]byte(keys.JWTSecret))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, &plat.TemporaryApiKeyResponseOutput{ApiKey: tok})
	return nil
}

func (s *Server) v2Config(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	resp := base("GET /v2/projects/{ref}/config")
	setAll(resp, map[string]any{
		"data.id":                       p.Ref,
		"data.attributes.api.db_schema": "public, storage, graphql_public",
		"data.attributes.api.db_extra_search_path":         "public, extensions",
		"data.attributes.api.max_rows":                     1000,
		"data.attributes.database.major_version":           pgMajor(p),
		"data.attributes.database.ssl_enforced":            false,
		"data.attributes.pooler.default_pool_size":         15,
		"data.attributes.pooler.max_client_conn":           200,
		"data.attributes.pooler.pool_mode":                 "transaction",
		"data.attributes.pooler.ignore_startup_parameters": "extra_float_digits",
	})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// platformBackups reports no backups: base backups are managed by sbctl itself and
// have no dashboard representation yet.
func (s *Server) platformBackups(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	resp := base("GET /platform/database/{ref}/backups")
	set(resp, "backups", []any{})
	set(resp, "pitr_enabled", false)
	set(resp, "walg_enabled", false)
	writeJSON(w, http.StatusOK, resp)
	return nil
}
