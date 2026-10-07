package api

import (
	"net/http"
	"strings"

	"github.com/jsmillerdev/supavise/internal/projectconfig"
)

// v2Config is GET /v2/projects/{ref}/config, the one document of every service's settings that
// the Supabase CLI reads to diff its config.toml (`supabase config diff|push`; the writes go to
// the per-service /v1 routes). The auth block is GET /v1/.../config/auth without its nulls.
func (s *Server) v2Config(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	get := func(svc projectconfig.Service) (*projectconfig.State, error) {
		return s.settings.Get(r.Context(), p.Ref, svc)
	}
	authSt, err := get(projectconfig.Auth)
	if err != nil {
		return err
	}
	restSt, err := get(projectconfig.PostgREST)
	if err != nil {
		return err
	}
	rtSt, err := get(projectconfig.Realtime)
	if err != nil {
		return err
	}
	stSt, err := get(projectconfig.Storage)
	if err != nil {
		return err
	}
	pgSt, err := get(projectconfig.Postgres)
	if err != nil {
		return err
	}
	poolSt, err := get(projectconfig.Pooler)
	if err != nil {
		return err
	}
	poolSize, poolMax := poolerValues(poolSt)

	resp := base("GET /v2/projects/{ref}/config")
	set(resp, "data.id", p.Ref)
	// The spec's auth block is a free-form object of the settings that have a value (it cannot
	// say null), so unset ones are left out.
	auth := authView("GET /v1/projects/{ref}/config/auth", authSt, false)
	for k, v := range auth {
		if v == nil {
			delete(auth, k)
		}
	}
	set(resp, "data.attributes.auth", auth)
	api := map[string]any{}
	fillView(api, restSt, projectconfig.PostgRESTSchema)
	set(resp, "data.attributes.api", api)
	rt := map[string]any{}
	fillView(rt, rtSt, projectconfig.RealtimeSchema)
	set(resp, "data.attributes.realtime", rt)

	n := s.storageFileSizeLimit(stSt)
	feats := stSt.StorageFeatures()
	snake := map[string]any{}
	for k, v := range feats {
		snake[camelToSnake(k)] = snakeKeys(v)
	}
	upstream, _ := stSt.StorageExternal()["upstreamTarget"].(string)
	set(resp, "data.attributes.storage", map[string]any{
		"file_size_limit": n, "features": snake, "capabilities": storageCapabilities,
		"upstream_target": upstream, "migration_version": "",
	})

	pgView, err := s.pgConfigView(r.Context(), p.Ref, pgSt)
	if err != nil {
		return err
	}
	settings := map[string]any{}
	for k, v := range pgView {
		settings[strings.ReplaceAll(k, ".", "_")] = v
	}
	setAll(resp, map[string]any{
		"data.attributes.database.major_version":           pgMajor(p),
		"data.attributes.database.ssl_enforced":            false,
		"data.attributes.database.postgres_settings":       settings,
		"data.attributes.pooler.default_pool_size":         poolSize,
		"data.attributes.pooler.max_client_conn":           poolMax,
		"data.attributes.pooler.pool_mode":                 poolerMode,
		"data.attributes.pooler.ignore_startup_parameters": poolerIgnoredParams,
	})
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// snakeKeys renames the keys of a nested settings object from camelCase.
func snakeKeys(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, x := range m {
		out[camelToSnake(k)] = snakeKeys(x)
	}
	return out
}
