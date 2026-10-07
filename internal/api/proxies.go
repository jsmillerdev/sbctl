package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/OWNER/sbctl/internal/registry"
)

// Upstream names accepted by Server.upstream.
const (
	upGoTrue  = "gotrue"
	upStorage = "storage"
	// upStorageAdmin is Storage's admin API (the port with the tenant and S3 credential routes).
	upStorageAdmin = "storage-admin"
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
	case upStorageAdmin:
		return "http://127.0.0.1:" + itoa(int64(s.cfg.Ports.StorageAdmin))
	}
	return ""
}

// hook rewrites a dashboard request body for the upstream: in is the decoded JSON
// body ({} when empty), id the {id} path value. It returns the upstream path and body.
type hook func(r *http.Request, id string, in map[string]any) (path string, body map[string]any)

// proxyRoute maps a platform operation to a call of a project service.
type proxyRoute struct {
	method, path string
	// req rewrites the request body; nil forwards the body untouched.
	req hook
	// resp rewrites a 2xx response body; nil forwards it untouched.
	resp func(s *Server, p *registry.Project, body []byte) []byte
}

// authMap maps platform auth operations to GoTrue's admin API.
var authMap = map[string]proxyRoute{
	"POST /platform/auth/{ref}/users":       {method: http.MethodPost, path: "/admin/users"},
	"GET /platform/auth/{ref}/users":        {method: http.MethodGet, path: "/admin/users"},
	"PATCH /platform/auth/{ref}/users/{id}": {method: http.MethodPut, path: "/admin/users/{id}"},
	"POST /platform/auth/{ref}/invite":      {method: http.MethodPost, path: "/invite"},
	"POST /platform/auth/{ref}/magiclink":   {method: http.MethodPost, path: "/magiclink"},
	"POST /platform/auth/{ref}/otp":         {method: http.MethodPost, path: "/otp"},
	"POST /platform/auth/{ref}/recover":     {method: http.MethodPost, path: "/recover"},
	"DELETE /platform/auth/{ref}/users/{id}": {method: http.MethodDelete, path: "/admin/users/{id}", req: func(r *http.Request, id string, _ map[string]any) (string, map[string]any) {
		return "/admin/users/" + url.PathEscape(id), map[string]any{"should_soft_delete": r.URL.Query().Get("soft_delete") == "true"}
	}},
}

// storageMap maps the dashboard's storage routes to Storage's own API ({id} is the
// bucket id). The request shapes are the platform spec's; Storage's are in
// supabase/storage src/http/routes.
var storageMap = map[string]proxyRoute{
	"GET /platform/storage/{ref}/buckets": {method: http.MethodGet, path: "/bucket"},
	"POST /platform/storage/{ref}/buckets": {method: http.MethodPost, path: "/bucket", req: func(_ *http.Request, _ string, in map[string]any) (string, map[string]any) {
		in["name"] = in["id"] // Storage wants a name; the dashboard sends only the id
		return "/bucket", in
	}},
	"GET /platform/storage/{ref}/buckets/{id}":        {method: http.MethodGet, path: "/bucket/{id}"},
	"PATCH /platform/storage/{ref}/buckets/{id}":      {method: http.MethodPut, path: "/bucket/{id}"},
	"DELETE /platform/storage/{ref}/buckets/{id}":     {method: http.MethodDelete, path: "/bucket/{id}"},
	"POST /platform/storage/{ref}/buckets/{id}/empty": {method: http.MethodPost, path: "/bucket/{id}/empty"},
	"POST /platform/storage/{ref}/buckets/{id}/objects/list": {method: http.MethodPost, path: "/object/list/{id}", req: func(_ *http.Request, id string, in map[string]any) (string, map[string]any) {
		out := map[string]any{"prefix": in["path"]}
		if opts, ok := in["options"].(map[string]any); ok {
			for k, v := range opts {
				out[k] = v
			}
		}
		return "/object/list/" + url.PathEscape(id), out
	}},
	"POST /platform/storage/{ref}/buckets/{id}/objects/move": {method: http.MethodPost, path: "/object/move", req: moveCopy},
	"POST /platform/storage/{ref}/buckets/{id}/objects/copy": {method: http.MethodPost, path: "/object/copy", req: moveCopy},
	"DELETE /platform/storage/{ref}/buckets/{id}/objects": {method: http.MethodDelete, path: "/object/{id}", req: func(_ *http.Request, id string, in map[string]any) (string, map[string]any) {
		var prefixes []string
		paths, _ := in["paths"].([]any)
		for _, p := range paths {
			switch x := p.(type) {
			case string:
				prefixes = append(prefixes, x)
			case map[string]any:
				prefixes = append(prefixes, str(x, "path"))
			}
		}
		return "/object/" + url.PathEscape(id), map[string]any{"prefixes": prefixes}
	}},
	"POST /platform/storage/{ref}/buckets/{id}/objects/sign": {method: http.MethodPost, path: "/object/sign/{id}", req: func(_ *http.Request, id string, in map[string]any) (string, map[string]any) {
		out := map[string]any{"expiresIn": in["expiresIn"]}
		if opts, ok := in["options"].(map[string]any); ok && opts["transform"] != nil {
			out["transform"] = opts["transform"]
		}
		return "/object/sign/" + url.PathEscape(id) + "/" + escapePath(strings.TrimPrefix(str(in, "path"), "/")), out
	}, resp: func(s *Server, p *registry.Project, body []byte) []byte {
		// Storage answers {signedURL: "/object/sign/..."}; the dashboard wants the full URL.
		var in struct {
			SignedURL string `json:"signedURL"`
		}
		if json.Unmarshal(body, &in) != nil || in.SignedURL == "" {
			return body
		}
		out, _ := json.Marshal(map[string]string{"signedUrl": s.projectURL(p.Ref) + "/storage/v1" + in.SignedURL})
		return out
	}},
	// Studio's explorer lists with the v2 route (capabilities.list_v2): the body is Storage's own
	// (prefix, cursor, limit, with_delimiter, sortBy, ...), and so is the answer.
	"POST /platform/storage/{ref}/buckets/{id}/objects/list-v2": {method: http.MethodPost, path: "/object/list-v2/{id}"},
	// Signing several objects: {path: [...], expiresIn} becomes Storage's {paths, expiresIn} and
	// each {signedURL: "/object/sign/..."} the full URL.
	"POST /platform/storage/{ref}/buckets/{id}/objects/sign-multi": {method: http.MethodPost, path: "/object/sign/{id}", req: func(_ *http.Request, id string, in map[string]any) (string, map[string]any) {
		var paths []string
		if list, ok := in["path"].([]any); ok {
			for _, p := range list {
				if s, ok := p.(string); ok {
					paths = append(paths, strings.TrimPrefix(s, "/"))
				}
			}
		}
		return "/object/sign/" + url.PathEscape(id), map[string]any{"expiresIn": in["expiresIn"], "paths": paths}
	}, resp: func(s *Server, p *registry.Project, body []byte) []byte {
		var in []struct {
			Error     *string `json:"error"`
			Path      string  `json:"path"`
			SignedURL *string `json:"signedURL"`
		}
		if json.Unmarshal(body, &in) != nil {
			return body
		}
		out := make([]map[string]any, 0, len(in))
		for _, e := range in {
			item := map[string]any{"path": e.Path, "error": e.Error, "signedUrl": nil}
			if e.SignedURL != nil {
				item["signedUrl"] = s.projectURL(p.Ref) + "/storage/v1" + *e.SignedURL
			}
			out = append(out, item)
		}
		b, _ := json.Marshal(out)
		return b
	}},
}

// escapePath escapes each segment of an object path and keeps the slashes.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// hasDotSegment reports whether an (escaped) URL path has a "." or ".." segment.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// moveCopy maps the dashboard's {from, to} to Storage's bucket-qualified keys.
func moveCopy(_ *http.Request, id string, in map[string]any) (string, map[string]any) {
	out := map[string]any{"bucketId": id, "sourceKey": in["from"], "destinationKey": in["to"]}
	if v, ok := in["sourceVersionId"]; ok {
		out["sourceVersionId"] = v
	}
	return "", out
}

func (s *Server) routesProxies(add func(string, handlerFunc)) {
	ops, _ := Operations()
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "/platform/pg-meta/{ref}/") {
			add(op.Key(), s.pgmetaProxy)
		}
	}
	for key, m := range authMap {
		add(key, s.adminProxy(upGoTrue, m))
	}
	for key, m := range storageMap {
		add(key, s.adminProxy(upStorage, m))
	}
}

// adminProxy forwards the request to a project service with the project's
// service_role key.
func (s *Server) adminProxy(svc string, m proxyRoute) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.running(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		keys, err := s.mgr.Keys(r.Context(), p.Ref)
		if err != nil {
			return err
		}
		// PathValue decodes %2F, so an id could climb out of its place in the upstream
		// path. Ids are one segment: reject separators and dot segments, escape the rest.
		id := r.PathValue("id")
		if strings.Contains(id, "/") || id == "." || id == ".." {
			return errf(http.StatusBadRequest, "Invalid id")
		}
		target := strings.ReplaceAll(m.path, "{id}", url.PathEscape(id))
		body := io.Reader(http.MaxBytesReader(w, r.Body, maxBody))
		if m.req != nil {
			in := map[string]any{}
			if err := decode(r, &in); err != nil {
				return err
			}
			path, out := m.req(r, id, in)
			if path != "" {
				target = path
			}
			if hasDotSegment(target) {
				return errf(http.StatusBadRequest, "Invalid path")
			}
			b, _ := json.Marshal(out)
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(r.Context(), m.method, s.upstream(p, svc)+target, body)
		if err != nil {
			return err
		}
		req.URL.RawQuery = r.URL.RawQuery
		if m.req != nil {
			req.Header.Set("Content-Type", "application/json")
		} else if ct := r.Header.Get("Content-Type"); ct != "" {
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
		if m.resp != nil && resp.StatusCode < 300 {
			b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
			if err != nil {
				return err
			}
			writeRaw(w, resp.StatusCode, resp.Header.Get("Content-Type"), m.resp(s, p, b))
			return nil
		}
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
