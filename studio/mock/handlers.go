package main

import (
	"net/http"
	"strconv"
	"strings"
)

// registerReal adds the hand-written handlers: everything on the P0 flows of
// research/08-studio-platform-calls.md and what project pages read on open.
func (s *server) registerReal() {
	s.handle("GET", "/platform/profile", func(w *respWriter, r *http.Request, c *reqCtx) { w.json(200, s.profile(c.user)) })
	s.handle("POST", "/platform/profile", func(w *respWriter, r *http.Request, c *reqCtx) { w.json(201, s.profile(c.user)) })
	s.handle("PATCH", "/platform/profile", func(w *respWriter, r *http.Request, c *reqCtx) { w.json(200, s.profile(c.user)) })
	s.handle("GET", "/platform/profile/permissions", func(w *respWriter, r *http.Request, c *reqCtx) { w.json(200, s.permissions()) })

	s.handle("GET", "/platform/organizations", func(w *respWriter, r *http.Request, c *reqCtx) {
		w.json(200, []map[string]any{s.organization()})
	})
	s.handle("GET", "/platform/organizations/{slug}", s.withOrg(func(w *respWriter, r *http.Request, c *reqCtx) {
		w.json(200, s.organizationDetail())
	}))
	s.handle("GET", "/platform/organizations/{slug}/entitlements", s.withOrg(func(w *respWriter, r *http.Request, c *reqCtx) {
		w.json(200, map[string]any{"entitlements": s.entitlements()})
	}))
	s.handle("GET", "/platform/organizations/{slug}/projects", s.withOrg(func(w *respWriter, r *http.Request, c *reqCtx) {
		s.projectList(w, r, false)
	}))
	s.handle("GET", "/platform/projects", func(w *respWriter, r *http.Request, c *reqCtx) { s.projectList(w, r, true) })

	s.handle("GET", "/platform/projects/{ref}", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		w.json(200, s.projectDetail(p, n))
	}))
	s.handle("GET", "/platform/projects/{ref}/status", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		w.json(200, map[string]any{"status": statusOK})
	}))
	s.handle("GET", "/platform/projects/{ref}/settings", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		w.json(200, s.projectSettings(p))
	}))
	s.handle("POST", "/platform/pg-meta/{ref}/query", s.pgMetaQuery)

	s.handle("POST", "/platform/projects/{ref}/api-keys/temporary", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		w.json(201, map[string]any{"api_key": s.legacyKey(p, "service_role")})
	}))
	s.handle("GET", "/v1/projects/{ref}/api-keys", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		w.json(200, s.apiKeys(p))
	}))
	s.handle("GET", "/v1/projects/{ref}/api-keys/legacy", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		w.json(200, map[string]any{"enabled": true})
	}))
	s.handle("GET", "/v1/projects/{ref}/branches", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		w.json(200, []any{})
	}))
	s.handle("GET", "/v1/projects/{ref}/health", s.withProject(func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int) {
		var out []map[string]any
		for _, name := range []string{"auth", "db", "pooler", "realtime", "rest", "storage"} {
			out = append(out, map[string]any{"name": name, "healthy": true, "status": "ACTIVE_HEALTHY"})
		}
		w.json(200, out)
	}))
}

func (s *server) withOrg(h handlerFunc) handlerFunc {
	return func(w *respWriter, r *http.Request, c *reqCtx) {
		if c.params["slug"] != orgSlug {
			w.message(http.StatusNotFound, "Organization not found")
			return
		}
		h(w, r, c)
	}
}

func (s *server) withProject(h func(w *respWriter, r *http.Request, c *reqCtx, p *Project, n int)) handlerFunc {
	return func(w *respWriter, r *http.Request, c *reqCtx) {
		p, n := s.project(c.params["ref"])
		if p == nil {
			w.message(http.StatusNotFound, "Project not found")
			return
		}
		h(w, r, c, p, n)
	}
}

// projectList serves both project lists with Studio's limit, offset and search parameters.
func (s *server) projectList(w *respWriter, r *http.Request, global bool) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 {
		limit = 100
	}
	search := strings.ToLower(q.Get("search"))
	var items []map[string]any
	for i := range s.cfg.Projects {
		p := &s.cfg.Projects[i]
		if search != "" && !strings.Contains(strings.ToLower(p.Name), search) {
			continue
		}
		items = append(items, s.projectItem(p, i+1, global))
	}
	total := len(items)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := items[offset:end]
	if page == nil {
		page = []map[string]any{}
	}
	w.json(200, map[string]any{
		"projects":   page,
		"pagination": map[string]any{"count": total, "limit": limit, "offset": offset},
	})
}
