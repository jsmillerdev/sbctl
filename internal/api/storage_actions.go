package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// Storage dashboard actions that are not a plain pass-through to Storage's tenant API (those
// are in proxies.go: storageMap): public URLs, built here, and the S3 access keys, which
// Storage serves on its admin port.

func (s *Server) routesStorageActions(add func(string, handlerFunc)) {
	add("POST /platform/storage/{ref}/buckets/{id}/objects/public-url", s.publicObjectURL)
	add("GET /platform/storage/{ref}/credentials", s.listS3Credentials)
	add("POST /platform/storage/{ref}/credentials", s.createS3Credential)
	add("DELETE /platform/storage/{ref}/credentials/{id}", s.deleteS3Credential)
}

// storageCall sends a request to Storage on behalf of project p with its service_role key
// and returns the status and body.
func (s *Server) storageCall(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.upstream(p, upStorage)+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+keys.ServiceRoleKey)
	req.Header.Set("apikey", keys.ServiceRoleKey)
	req.Header.Set("X-Forwarded-Host", s.cfg.ProjectHost(p.Ref))
	resp, err := s.hc.Do(req)
	if err != nil {
		s.log.Error("storage unreachable", "ref", p.Ref, "err", err)
		return 0, nil, errf(http.StatusBadGateway, "storage is unavailable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, b, err
}

// publicObjectURL builds the URL an object of a public bucket is served at. The bucket is
// checked first: a URL for a private bucket would only ever answer 400.
func (s *Server) publicObjectURL(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	keys, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	bucket := r.PathValue("id")
	if bucket == "" || strings.Contains(bucket, "/") || bucket == "." || bucket == ".." {
		return errf(http.StatusBadRequest, "Invalid id")
	}
	var in struct {
		Path    string         `json:"path"`
		Options map[string]any `json:"options"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	obj := strings.TrimPrefix(in.Path, "/")
	if obj == "" || hasDotSegment(obj) {
		return errf(http.StatusBadRequest, "Invalid path")
	}
	status, body, err := s.storageCall(r.Context(), p, keys, http.MethodGet, "/bucket/"+url.PathEscape(bucket), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errf(http.StatusNotFound, "Bucket not found")
	}
	if status >= 300 {
		return errf(http.StatusBadGateway, "storage answered %d", status)
	}
	var b struct {
		Public bool `json:"public"`
	}
	if json.Unmarshal(body, &b) != nil || !b.Public {
		return errf(http.StatusBadRequest, "The bucket is not public")
	}
	u := s.projectURL(p.Ref) + "/storage/v1/object/public/" + url.PathEscape(bucket) + "/" + escapePath(obj)
	// options.download: true downloads under the object's name, a string under that name.
	switch d := in.Options["download"].(type) {
	case string:
		u += "?download=" + url.QueryEscape(d)
	case bool:
		if d {
			u += "?download="
		}
	}
	resp := base("POST /platform/storage/{ref}/buckets/{id}/objects/public-url")
	set(resp, "publicUrl", u)
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

// storageAdmin calls Storage's admin API (the port with the S3 credential routes) with the
// fleet's admin key.
func (s *Server) storageAdmin(ctx context.Context, method, path string, body any) (int, []byte, error) {
	sealed, err := s.reg.GetSecret(ctx, config.SystemRef, "fleet_storage_admin_api_key")
	if err != nil {
		return 0, nil, errf(http.StatusServiceUnavailable, "Storage is not set up on this node")
	}
	key, err := s.sec.Open(sealed)
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.upstream(&registry.Project{Ref: config.SystemRef}, upStorageAdmin)+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("apikey", string(key))
	resp, err := s.hc.Do(req)
	if err != nil {
		s.log.Error("storage admin unreachable", "err", err)
		return 0, nil, errf(http.StatusBadGateway, "storage is unavailable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, b, err
}

func credentialsPath(ref string) string { return "/s3/" + url.PathEscape(ref) + "/credentials" }

// listS3Credentials lists the project's S3 access keys (never their secrets).
func (s *Server) listS3Credentials(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	status, body, err := s.storageAdmin(r.Context(), http.MethodGet, credentialsPath(p.Ref), nil)
	if err != nil {
		return err
	}
	if status >= 300 {
		return errf(http.StatusBadGateway, "storage answered %d", status)
	}
	var list []struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return errf(http.StatusBadGateway, "storage answered an unexpected body")
	}
	data := make([]map[string]any, 0, len(list))
	for _, c := range list {
		data = append(data, map[string]any{"id": c.ID, "description": c.Description, "created_at": c.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
	return nil
}

// createS3Credential creates an S3 access key for the project. The key acts as service_role
// (Storage's claims), as the dashboard's keys do; the secret is shown once, in this answer.
func (s *Server) createS3Credential(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in struct {
		Description string `json:"description"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if n := len(strings.TrimSpace(in.Description)); n < 3 || n > 2000 {
		return errf(http.StatusBadRequest, "The description must have 3 to 2000 characters")
	}
	status, body, err := s.storageAdmin(r.Context(), http.MethodPost, credentialsPath(p.Ref),
		map[string]any{"description": in.Description, "claims": map[string]any{"role": secrets.RoleServiceRole}})
	if err != nil {
		return err
	}
	if status >= 300 {
		return errf(http.StatusBadGateway, "storage answered %d", status)
	}
	writeRaw(w, http.StatusCreated, "application/json", body)
	return nil
}

func (s *Server) deleteS3Credential(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	status, _, err := s.storageAdmin(r.Context(), http.MethodDelete, credentialsPath(p.Ref), map[string]any{"id": r.PathValue("id")})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusBadRequest:
		return errf(http.StatusBadRequest, "Invalid id")
	case status >= 300:
		return errf(http.StatusBadGateway, "storage answered %d", status)
	}
	writeNoContent(w)
	return nil
}
