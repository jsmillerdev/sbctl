package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"

	v1 "github.com/OWNER/sbctl/internal/api/gen/v1"
)

func (s *Server) routesKeys(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/api-keys", s.listAPIKeys)
	add("GET /v1/projects/{ref}/api-keys/{id}", s.getAPIKey)
	add("GET /v1/projects/{ref}/api-keys/legacy", s.legacyKeys)
	add("PUT /v1/projects/{ref}/api-keys/legacy", s.legacyKeys)
}

// keyID is a stable uuid-shaped id for a key of a project.
func keyID(ref, name, typ string) string {
	h := sha256.Sum256([]byte(ref + "/" + typ + "/" + name))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

const maskDots = "••••••••••••"

// apiKeys lists the keys of a project. Secrets (service_role, sb_secret_*) are
// shown in full only when reveal is true.
func (s *Server) apiKeys(r *http.Request, reveal bool) ([]v1.ApiKeyResponseOutput, error) {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return nil, err
	}
	k, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return nil, err
	}
	at := p.CreatedAt.UTC()
	mk := func(name, typ, key, desc string, secret bool) v1.ApiKeyResponseOutput {
		id, t := keyID(p.Ref, name, typ), v1.ApiKeyResponseOutputType(typ)
		out := v1.ApiKeyResponseOutput{Id: &id, Name: name, Type: &t, Description: &desc, InsertedAt: &at, UpdatedAt: &at}
		prefix := key
		if typ != "legacy" && len(prefix) > 20 {
			prefix = prefix[:20]
		} else if typ == "legacy" {
			prefix = ""
		}
		if typ != "legacy" {
			out.Prefix = &prefix
		}
		switch {
		case !secret || reveal:
			out.ApiKey = &key
		case typ != "legacy":
			masked := prefix + maskDots
			out.ApiKey = &masked
		}
		return out
	}
	return []v1.ApiKeyResponseOutput{
		mk("anon", "legacy", k.AnonKey, "Legacy anon API key", false),
		mk("service_role", "legacy", k.ServiceRoleKey, "Legacy service_role API key", true),
		mk("default", "publishable", k.PublishableKey, "", false),
		mk("default", "secret", k.SecretKey, "", true),
	}, nil
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) error {
	keys, err := s.apiKeys(r, r.URL.Query().Get("reveal") == "true")
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, keys)
	return nil
}

func (s *Server) getAPIKey(w http.ResponseWriter, r *http.Request) error {
	keys, err := s.apiKeys(r, r.URL.Query().Get("reveal") == "true")
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	for _, k := range keys {
		if k.Id != nil && *k.Id == id {
			writeJSON(w, http.StatusOK, k)
			return nil
		}
	}
	return errf(http.StatusNotFound, "API key not found")
}

// legacyKeys reports the legacy anon and service_role keys as always enabled: they
// are the keys the Postgres services themselves verify.
func (s *Server) legacyKeys(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &v1.LegacyApiKeysResponseOutput{Enabled: true})
	return nil
}
