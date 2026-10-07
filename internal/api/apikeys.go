package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	v1 "github.com/OWNER/sbctl/internal/api/gen/v1"
	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// API keys of a project (/v1/projects/{ref}/api-keys*). A project has the legacy keys (the
// anon and service_role JWTs), which can be switched off as a pair, and opaque keys: a
// publishable and a secret key named "default" from the start, any number more that the
// API creates, and the ability to revoke any of them. The proxy accepts exactly the active
// ones (internal/secrets/apikeys.go explains the storage); a revocation reaches it through
// the registry change that invalidates its key cache, so the key stops working at once.

func (s *Server) routesKeys(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/api-keys", s.listAPIKeys)
	add("POST /v1/projects/{ref}/api-keys", s.createAPIKey)
	add("GET /v1/projects/{ref}/api-keys/legacy", s.legacyKeys)
	add("PUT /v1/projects/{ref}/api-keys/legacy", s.setLegacyKeys)
	add("GET /v1/projects/{ref}/api-keys/{id}", s.getAPIKey)
	add("PATCH /v1/projects/{ref}/api-keys/{id}", s.updateAPIKey)
	add("DELETE /v1/projects/{ref}/api-keys/{id}", s.deleteAPIKey)
}

const (
	maskDots = "••••••••••••"
	// maxAPIKeys bounds the active opaque keys of a project: the proxy compares a request
	// against every one of them and loads them all when its cache misses.
	maxAPIKeys = 50
)

var keyNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// keyID is the stable uuid-shaped id of a legacy or default key of a project.
func keyID(ref, name, typ string) string { return secrets.KeyID(ref, name, typ) }

func newKeyID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// keyEntry is one key as the API lists it: legacy, or opaque with its value resolved.
type keyEntry struct {
	id, name, typ, desc, key string
	template                 map[string]any
	created, updated         time.Time
	legacy, isDefault        bool
}

func (e keyEntry) secret() bool {
	return e.typ == secrets.KeyTypeSecret || (e.legacy && e.name == "service_role")
}

// keyEntries lists the keys of p: the legacy pair first (unless switched off), then the
// active opaque keys.
func keyEntries(p *registry.Project, k *secrets.ProjectKeys) []keyEntry {
	at := p.CreatedAt.UTC()
	var out []keyEntry
	if !k.LegacyDisabled {
		out = append(out,
			keyEntry{id: keyID(p.Ref, "anon", "legacy"), name: "anon", typ: "legacy", desc: "Legacy anon API key", key: k.AnonKey, created: at, updated: at, legacy: true},
			keyEntry{id: keyID(p.Ref, "service_role", "legacy"), name: "service_role", typ: "legacy", desc: "Legacy service_role API key", key: k.ServiceRoleKey, created: at, updated: at, legacy: true})
	}
	for _, o := range k.OpaqueKeys(p.Ref) {
		e := keyEntry{id: o.ID, name: o.Name, typ: o.Type, desc: o.Description, key: o.Key, template: o.Template,
			created: o.CreatedAt.UTC(), updated: o.UpdatedAt.UTC(), isDefault: o.Default}
		if o.CreatedAt.IsZero() {
			e.created = at
		}
		if o.UpdatedAt.IsZero() {
			e.updated = e.created
		}
		out = append(out, e)
	}
	return out
}

// keyPrefixChars is how many characters of a key's random part its masked display shows.
const keyPrefixChars = 4

// keyPrefix is the type prefix of an opaque key ("sb_secret_") and the first characters of its
// random part, enough to tell keys apart and no more.
func keyPrefix(key string) string {
	n := strings.LastIndexByte(key, '_') + 1
	if n+keyPrefixChars < len(key) {
		return key[:n+keyPrefixChars]
	}
	return key
}

// keyView is the response shape of one key. Secret keys are shown in full only when reveal
// is set (a masked prefix otherwise); the legacy keys have no prefix.
func keyView(e keyEntry, reveal bool) v1.ApiKeyResponseOutput {
	id, typ := e.id, v1.ApiKeyResponseOutputType(e.typ)
	desc, ins, upd := e.desc, e.created, e.updated
	out := v1.ApiKeyResponseOutput{Id: &id, Name: e.name, Type: &typ, Description: &desc, InsertedAt: &ins, UpdatedAt: &upd}
	if !e.legacy {
		prefix := keyPrefix(e.key)
		out.Prefix = &prefix
	}
	switch {
	case !e.secret() || reveal:
		k := e.key
		out.ApiKey = &k
	case !e.legacy:
		masked := *out.Prefix + maskDots
		out.ApiKey = &masked
	}
	if e.typ == secrets.KeyTypeSecret {
		// The CLI treats a secret key as the service_role key when its template says so
		// (cli-go internal/utils/tenant/client.go isServiceRole); without it the CLI falls
		// back to the legacy service_role JWT.
		t := e.template
		if t == nil {
			t = map[string]any{"role": secrets.RoleServiceRole}
		}
		out.SecretJwtTemplate = &t
	}
	return out
}

// truthy parses the spec's "Boolean string": true, 1, yes, on, y and enabled
// (case-insensitively) are true, everything else is false.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on", "y", "enabled":
		return true
	}
	return false
}

func (s *Server) projectKeys(r *http.Request) (*registry.Project, *secrets.ProjectKeys, error) {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return nil, nil, err
	}
	// Secret keys are listed masked; revealing them needs the permission to read them.
	if truthy(r.URL.Query().Get("reveal")) {
		if ok, err := s.canReadSecrets(r, p); err != nil {
			return nil, nil, err
		} else if !ok {
			return nil, nil, forbidden(members.ActRead, members.ResServiceKeys)
		}
	}
	k, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return nil, nil, err
	}
	return p, k, nil
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) error {
	p, k, err := s.projectKeys(r)
	if err != nil {
		return err
	}
	reveal := truthy(r.URL.Query().Get("reveal"))
	out := []v1.ApiKeyResponseOutput{}
	for _, e := range keyEntries(p, k) {
		out = append(out, keyView(e, reveal))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getAPIKey(w http.ResponseWriter, r *http.Request) error {
	p, k, err := s.projectKeys(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	for _, e := range keyEntries(p, k) {
		if e.id == id {
			writeJSON(w, http.StatusOK, keyView(e, truthy(r.URL.Query().Get("reveal"))))
			return nil
		}
	}
	return errf(http.StatusNotFound, "API key not found")
}

// putRecord stores a key record sealed; the registry change invalidates the proxy's cache.
func (s *Server) putRecord(ctx context.Context, ref string, rec secrets.APIKeyRecord) error {
	b, err := secrets.MarshalRecord(rec)
	if err != nil {
		return err
	}
	sealed, err := s.sec.Seal(b)
	if err != nil {
		return err
	}
	return s.reg.PutSecret(ctx, ref, secrets.RecordSecretName(rec.ID), sealed)
}

// keyLock serializes the key mutations of one project (read, check, write).
func (s *Server) keyLock(ref string) func() {
	return s.cfgLock(ref, "api-keys")
}

// validateName checks the name of a new or renamed key against the active ones.
func validateKeyName(name, selfID string, entries []keyEntry) error {
	if !keyNamePattern.MatchString(name) {
		return errf(http.StatusBadRequest, "A key name can only contain lowercase letters, numbers and underscores, and must not start with a number (at most 63 characters)")
	}
	for _, e := range entries {
		if e.name == name && e.id != selfID {
			return errf(http.StatusConflict, "A key named %q already exists", name)
		}
	}
	return nil
}

func validateTemplate(t *map[string]any) (map[string]any, error) {
	if t == nil || *t == nil {
		return nil, nil
	}
	for k, v := range *t {
		if k != "role" || v != secrets.RoleServiceRole {
			return nil, errf(http.StatusBadRequest, "secret_jwt_template supports only {\"role\": \"service_role\"}")
		}
	}
	return *t, nil
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in v1.CreateApiKeyBody
	if err := decode(r, &in); err != nil {
		return err
	}
	typ := string(in.Type)
	if typ != secrets.KeyTypePublishable && typ != secrets.KeyTypeSecret {
		return errf(http.StatusBadRequest, "type must be publishable or secret")
	}
	tmpl, err := validateTemplate(in.SecretJwtTemplate)
	if err != nil {
		return err
	}
	if tmpl != nil && typ != secrets.KeyTypeSecret {
		return errf(http.StatusBadRequest, "only a secret key has a secret_jwt_template")
	}
	defer s.keyLock(p.Ref)()
	k, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	entries := keyEntries(p, k)
	if err := validateKeyName(in.Name, "", entries); err != nil {
		return err
	}
	if n := len(k.OpaqueKeys(p.Ref)); n >= maxAPIKeys {
		return errf(http.StatusBadRequest, "A project can have at most %d API keys; delete one first", maxAPIKeys)
	}
	now := s.now().UTC()
	rec := secrets.APIKeyRecord{ID: newKeyID(), Name: in.Name, Type: typ, Template: tmpl, CreatedAt: now, UpdatedAt: now}
	if in.Description != nil {
		rec.Description = *in.Description
	}
	if typ == secrets.KeyTypePublishable {
		rec.Key = secrets.NewPublishableKey()
	} else {
		rec.Key = secrets.NewSecretKey()
	}
	if err := s.putRecord(r.Context(), p.Ref, rec); err != nil {
		return err
	}
	e := keyEntry{id: rec.ID, name: rec.Name, typ: rec.Type, desc: rec.Description, key: rec.Key, template: rec.Template, created: now, updated: now}
	writeJSON(w, http.StatusCreated, keyView(e, true)) // the only time a secret key is shown unasked
	return nil
}

// recordFor returns the stored record of an opaque key entry, or a fresh one for a default
// key that was never modified.
func recordFor(k *secrets.ProjectKeys, e keyEntry) secrets.APIKeyRecord {
	if rec, ok := k.Record(e.id); ok {
		return rec
	}
	return secrets.APIKeyRecord{ID: e.id, Name: e.name, Type: e.typ, Default: e.isDefault, CreatedAt: e.created, Description: e.desc}
}

func (s *Server) updateAPIKey(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in v1.UpdateApiKeyBody
	if err := decode(r, &in); err != nil {
		return err
	}
	tmpl, err := validateTemplate(in.SecretJwtTemplate)
	if err != nil {
		return err
	}
	defer s.keyLock(p.Ref)()
	k, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	entries := keyEntries(p, k)
	var target *keyEntry
	for i := range entries {
		if entries[i].id == r.PathValue("id") {
			target = &entries[i]
		}
	}
	if target == nil {
		return errf(http.StatusNotFound, "API key not found")
	}
	if target.legacy {
		return errf(http.StatusBadRequest, "The legacy keys cannot be edited")
	}
	rec := recordFor(k, *target)
	if in.Name != nil && *in.Name != rec.Name {
		if err := validateKeyName(*in.Name, target.id, entries); err != nil {
			return err
		}
		rec.Name = *in.Name
	}
	if in.Description != nil {
		rec.Description = *in.Description
	}
	if tmpl != nil {
		if rec.Type != secrets.KeyTypeSecret {
			return errf(http.StatusBadRequest, "only a secret key has a secret_jwt_template")
		}
		rec.Template = tmpl
	}
	rec.UpdatedAt = s.now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = target.created
	}
	if err := s.putRecord(r.Context(), p.Ref, rec); err != nil {
		return err
	}
	e := *target
	e.name, e.desc, e.updated = rec.Name, rec.Description, rec.UpdatedAt
	if rec.Template != nil {
		e.template = rec.Template
	}
	writeJSON(w, http.StatusOK, keyView(e, truthy(r.URL.Query().Get("reveal"))))
	return nil
}

// deleteAPIKey revokes a key: its record keeps the name and type and loses the value, so a
// revoked key can never be read back, and the proxy refuses it from the next request on.
func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	defer s.keyLock(p.Ref)()
	k, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	var target *keyEntry
	entries := keyEntries(p, k)
	for i := range entries {
		if entries[i].id == r.PathValue("id") {
			target = &entries[i]
		}
	}
	if target == nil {
		return errf(http.StatusNotFound, "API key not found")
	}
	if target.legacy {
		return errf(http.StatusBadRequest, "The legacy keys cannot be deleted; disable them with PUT /api-keys/legacy?enabled=false")
	}
	rec := recordFor(k, *target)
	rec.Revoked, rec.Key, rec.UpdatedAt = true, "", s.now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = target.created
	}
	if err := s.putRecord(r.Context(), p.Ref, rec); err != nil {
		return err
	}
	s.log.Info("api key revoked", "ref", p.Ref, "name", target.name, "type", target.typ, "reason", truncate(r.URL.Query().Get("reason"), 200))
	writeJSON(w, http.StatusOK, keyView(*target, truthy(r.URL.Query().Get("reveal"))))
	return nil
}

// legacyKeys reports whether the legacy anon and service_role keys are accepted.
func (s *Server) legacyKeys(w http.ResponseWriter, r *http.Request) error {
	_, k, err := s.projectKeys(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &v1.LegacyApiKeysResponseOutput{Enabled: !k.LegacyDisabled})
	return nil
}

// setLegacyKeys switches the legacy keys on or off (PUT ...?enabled=). Switching them off
// needs a publishable and a secret key to fall back on.
func (s *Server) setLegacyKeys(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	raw := r.URL.Query().Get("enabled")
	if raw == "" {
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if in.Enabled == nil {
			return errf(http.StatusBadRequest, "enabled is required")
		}
		raw = fmt.Sprint(*in.Enabled)
	}
	enabled := truthy(raw)
	defer s.keyLock(p.Ref)()
	k, err := s.mgr.Keys(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	if !enabled {
		var pub, sec bool
		for _, o := range k.OpaqueKeys(p.Ref) {
			pub, sec = pub || o.Type == secrets.KeyTypePublishable, sec || o.Type == secrets.KeyTypeSecret
		}
		if !pub || !sec {
			return errf(http.StatusBadRequest, "Create a publishable and a secret API key before disabling the legacy keys")
		}
	}
	if k.LegacyDisabled == !enabled {
		writeJSON(w, http.StatusOK, &v1.LegacyApiKeysResponseOutput{Enabled: enabled})
		return nil
	}
	sealed, err := s.sec.Seal(secrets.MarshalLegacyState(enabled))
	if err != nil {
		return err
	}
	if err := s.reg.PutSecret(r.Context(), p.Ref, secrets.NameLegacyKeys, sealed); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return errNoProject
		}
		return err
	}
	s.log.Info("legacy api keys switched", "ref", p.Ref, "enabled", enabled)
	writeJSON(w, http.StatusOK, &v1.LegacyApiKeysResponseOutput{Enabled: enabled})
	return nil
}
