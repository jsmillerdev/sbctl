package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/OWNER/sbctl/internal/registry"
)

// Upstream names accepted by Server.upstream.
const (
	upGoTrue  = "gotrue"
	upStorage = "storage"
)

// upstream returns the loopback base URL of a service of p (GoTrue is per project;
// Storage is the shared fleet instance).
func (s *Server) upstream(p *registry.Project, svc string) string {
	if s.upstreamOverride != nil {
		if u := s.upstreamOverride(p, svc); u != "" {
			return u
		}
	}
	switch svc {
	case upGoTrue:
		return "http://127.0.0.1:" + itoa(int64(s.cfg.PortsFor(p.Ref, p.Seq).GoTrue))
	case upStorage:
		return "http://127.0.0.1:" + itoa(int64(s.cfg.Ports.Storage))
	}
	return ""
}

// authMap maps platform auth operations to GoTrue's admin API: method of the
// upstream request, and its path with {id} substituted.
var authMap = map[string]struct{ method, path string }{
	"POST /platform/auth/{ref}/users":        {http.MethodPost, "/admin/users"},
	"GET /platform/auth/{ref}/users":         {http.MethodGet, "/admin/users"},
	"PATCH /platform/auth/{ref}/users/{id}":  {http.MethodPut, "/admin/users/{id}"},
	"DELETE /platform/auth/{ref}/users/{id}": {http.MethodDelete, "/admin/users/{id}"},
	"POST /platform/auth/{ref}/invite":       {http.MethodPost, "/invite"},
	"POST /platform/auth/{ref}/magiclink":    {http.MethodPost, "/magiclink"},
	"POST /platform/auth/{ref}/otp":          {http.MethodPost, "/otp"},
	"POST /platform/auth/{ref}/recover":      {http.MethodPost, "/recover"},
}

// storageMap maps the dashboard's storage routes (not in the platform spec) to
// Storage's own API. {id} is the bucket id.
var storageMap = map[string]struct{ method, path string }{
	"GET /platform/storage/{ref}/buckets":                      {http.MethodGet, "/bucket"},
	"POST /platform/storage/{ref}/buckets":                     {http.MethodPost, "/bucket"},
	"GET /platform/storage/{ref}/buckets/{id}":                 {http.MethodGet, "/bucket/{id}"},
	"PATCH /platform/storage/{ref}/buckets/{id}":               {http.MethodPut, "/bucket/{id}"},
	"DELETE /platform/storage/{ref}/buckets/{id}":              {http.MethodDelete, "/bucket/{id}"},
	"POST /platform/storage/{ref}/buckets/{id}/empty":          {http.MethodPost, "/bucket/{id}/empty"},
	"POST /platform/storage/{ref}/buckets/{id}/objects/list":   {http.MethodPost, "/object/list/{id}"},
	"POST /platform/storage/{ref}/buckets/{id}/objects/move":   {http.MethodPost, "/object/move"},
	"POST /platform/storage/{ref}/buckets/{id}/objects/copy":   {http.MethodPost, "/object/copy"},
	"POST /platform/storage/{ref}/buckets/{id}/objects/sign":   {http.MethodPost, "/object/sign/{id}"},
	"POST /platform/storage/{ref}/buckets/{id}/objects/delete": {http.MethodDelete, "/object/{id}"},
}

func (s *Server) routesProxies(add func(string, handlerFunc)) {
	ops, _ := Operations()
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "/platform/pg-meta/{ref}/") {
			add(op.Key(), s.pgmetaProxy)
		}
	}
	for key, m := range authMap {
		add(key, s.adminProxy(upGoTrue, m.method, m.path, false))
	}
	for key, m := range storageMap {
		add(key, s.adminProxy(upStorage, m.method, m.path, strings.HasSuffix(key, "/objects/delete")))
	}
}

// adminProxy forwards the request to a project service with the project's
// service_role key. path may contain {id}. deleteObjects rewrites the dashboard's
// {paths} body into Storage's {prefixes}.
func (s *Server) adminProxy(svc, method, path string, deleteObjects bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.running(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		keys, err := s.mgr.Keys(r.Context(), p.Ref)
		if err != nil {
			return err
		}
		target := strings.ReplaceAll(path, "{id}", r.PathValue("id"))
		body := io.Reader(http.MaxBytesReader(w, r.Body, maxBody))
		if deleteObjects {
			var in struct {
				Paths []string `json:"paths"`
			}
			if err := decode(r, &in); err != nil {
				return err
			}
			b, _ := json.Marshal(map[string]any{"prefixes": in.Paths})
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(r.Context(), method, s.upstream(p, svc)+target, body)
		if err != nil {
			return err
		}
		req.URL.RawQuery = r.URL.RawQuery
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Authorization", "Bearer "+keys.ServiceRoleKey)
		req.Header.Set("apikey", keys.ServiceRoleKey)
		if svc == upStorage {
			req.Header.Set("X-Forwarded-Host", s.cfg.ProjectHost(p.Ref))
		}
		resp, err := s.hc.Do(req)
		if err != nil {
			s.log.Error("upstream unreachable", "svc", svc, "ref", p.Ref, "err", err)
			return errf(http.StatusBadGateway, "%s is unavailable", svc)
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return nil
	}
}
