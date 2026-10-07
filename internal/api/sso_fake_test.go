package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ssoFake is the part of GoTrue's admin API that manages SAML providers (supabase/auth
// internal/api/ssoadmin.go at v2.195.0), with the same validations and the same JSON.
type ssoFake struct {
	mu        sync.Mutex
	providers map[string]map[string]any // by id
	order     []string
	next      int
	// requests records "METHOD path" of every call.
	requests []string
	// failNext answers the next call with this status.
	failNext int
}

var entityIDRe = regexp.MustCompile(`entityID="([^"]+)"`)

// testIdPMetadata returns a metadata document for an identity provider with this entity id.
func testIdPMetadata(entityID string) string {
	return `<?xml version="1.0"?><md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="` + entityID + `">` +
		`<md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">` +
		`<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="` + entityID + `/sso"/>` +
		`</md:IDPSSODescriptor></md:EntityDescriptor>`
}

func (f *ssoFake) record(r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
}

func (f *ssoFake) fail(w http.ResponseWriter) bool {
	if f.failNext == 0 {
		return false
	}
	st := f.failNext
	f.failNext = 0
	w.WriteHeader(st)
	_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	return true
}

func gtError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "error_code": code, "msg": msg})
}

func (f *ssoFake) domainOwner(domain, except string) string {
	for id, p := range f.providers {
		if id == except {
			continue
		}
		for _, d := range p["domains"].([]map[string]any) {
			if d["domain"] == domain {
				return id
			}
		}
	}
	return ""
}

func (f *ssoFake) register(mux *http.ServeMux) {
	f.providers = map[string]map[string]any{}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	domainsOf := func(in []any) []map[string]any {
		out := []map[string]any{}
		for _, d := range in {
			out = append(out, map[string]any{"domain": d, "created_at": now(), "updated_at": now()})
		}
		return out
	}
	mux.HandleFunc("GET /admin/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record(r)
		if f.fail(w) {
			return
		}
		items := []map[string]any{}
		for _, id := range f.order {
			p := f.providers[id]
			cp := map[string]any{}
			for k, v := range p {
				cp[k] = v
			}
			saml := map[string]any{}
			for k, v := range p["saml"].(map[string]any) {
				if k != "metadata_xml" {
					saml[k] = v
				}
			}
			cp["saml"] = saml
			items = append(items, cp)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("POST /admin/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record(r)
		if f.fail(w) {
			return
		}
		var in struct {
			Type             string         `json:"type"`
			MetadataURL      string         `json:"metadata_url"`
			MetadataXML      string         `json:"metadata_xml"`
			Domains          []any          `json:"domains"`
			AttributeMapping map[string]any `json:"attribute_mapping"`
			NameIDFormat     string         `json:"name_id_format"`
			Disabled         *bool          `json:"disabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch {
		case in.Type != "saml":
			gtError(w, 400, "validation_failed", "Only 'saml' supported for SSO provider type")
			return
		case in.MetadataURL != "" && in.MetadataXML != "":
			gtError(w, 400, "validation_failed", "Only one of metadata_xml or metadata_url needs to be set")
			return
		case in.MetadataURL == "" && in.MetadataXML == "":
			gtError(w, 400, "validation_failed", "Either metadata_xml or metadata_url must be set")
			return
		case in.MetadataURL != "" && !strings.HasPrefix(in.MetadataURL, "https://"):
			gtError(w, 400, "validation_failed", "metadata_url is not a HTTPS URL")
			return
		}
		xml := in.MetadataXML
		if in.MetadataURL != "" {
			xml = testIdPMetadata(strings.TrimSuffix(in.MetadataURL, "/metadata.xml"))
		}
		m := entityIDRe.FindStringSubmatch(xml)
		if m == nil {
			gtError(w, 400, "validation_failed", "SAML Metadata does not contain an EntityID")
			return
		}
		for _, p := range f.providers {
			if p["saml"].(map[string]any)["entity_id"] == m[1] {
				gtError(w, 422, "saml_idp_already_exists", "SAML Identity Provider with this EntityID ("+m[1]+") already exists")
				return
			}
		}
		for _, d := range in.Domains {
			if id := f.domainOwner(fmt.Sprint(d), ""); id != "" {
				gtError(w, 400, "sso_domain_already_exists", fmt.Sprintf("SSO Domain '%v' is already assigned to an SSO identity provider (%s)", d, id))
				return
			}
		}
		f.next++
		id := fmt.Sprintf("a0000000-0000-4000-8000-%012d", f.next)
		saml := map[string]any{"entity_id": m[1], "metadata_xml": xml, "attribute_mapping": map[string]any{}}
		if in.MetadataURL != "" {
			saml["metadata_url"] = in.MetadataURL
		}
		if in.AttributeMapping != nil {
			saml["attribute_mapping"] = in.AttributeMapping
		}
		if in.NameIDFormat != "" {
			saml["name_id_format"] = in.NameIDFormat
		}
		p := map[string]any{"id": id, "saml": saml, "domains": domainsOf(in.Domains), "disabled": in.Disabled, "created_at": now(), "updated_at": now()}
		f.providers[id] = p
		f.order = append(f.order, id)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)
	})
	load := func(w http.ResponseWriter, r *http.Request) map[string]any {
		f.record(r)
		p, ok := f.providers[r.PathValue("id")]
		if !ok {
			gtError(w, 404, "sso_provider_not_found", "SSO Identity Provider not found")
			return nil
		}
		return p
	}
	mux.HandleFunc("GET /admin/sso/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p := load(w, r); p != nil {
			_ = json.NewEncoder(w).Encode(p)
		}
	})
	mux.HandleFunc("PUT /admin/sso/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := load(w, r)
		if p == nil {
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		saml := p["saml"].(map[string]any)
		xml, _ := in["metadata_xml"].(string)
		if u, _ := in["metadata_url"].(string); u != "" {
			xml = testIdPMetadata(strings.TrimSuffix(u, "/metadata.xml"))
			saml["metadata_url"] = u
		}
		if xml != "" {
			m := entityIDRe.FindStringSubmatch(xml)
			if m == nil || m[1] != saml["entity_id"] {
				gtError(w, 400, "saml_entity_id_mismatch", "SAML Metadata can be updated only if the EntityID matches for the provider")
				return
			}
			saml["metadata_xml"] = xml
		}
		if ds, ok := in["domains"]; ok && ds != nil {
			list, _ := ds.([]any)
			for _, d := range list {
				if id := f.domainOwner(fmt.Sprint(d), r.PathValue("id")); id != "" {
					gtError(w, 400, "sso_domain_already_exists", fmt.Sprintf("SSO domain '%v' already assigned to another provider (%s)", d, id))
					return
				}
			}
			p["domains"] = domainsOf(list)
		}
		if am, ok := in["attribute_mapping"]; ok && am != nil {
			saml["attribute_mapping"] = am
		}
		if nf, ok := in["name_id_format"].(string); ok {
			if nf == "" {
				delete(saml, "name_id_format")
			} else {
				saml["name_id_format"] = nf
			}
		}
		if d, ok := in["disabled"].(bool); ok {
			p["disabled"] = &d
		}
		p["updated_at"] = now()
		_ = json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("DELETE /admin/sso/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := load(w, r)
		if p == nil {
			return
		}
		delete(f.providers, r.PathValue("id"))
		for i, id := range f.order {
			if id == r.PathValue("id") {
				f.order = append(f.order[:i], f.order[i+1:]...)
				break
			}
		}
		_ = json.NewEncoder(w).Encode(p)
	})
}

func (f *ssoFake) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.order...)
	sort.Strings(out)
	return out
}

func (f *ssoFake) called(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.requests {
		if c == call {
			n++
		}
	}
	return n
}

// add puts a provider in the fake behind sbctl's back (one that exists in GoTrue only).
func (f *ssoFake) add(entityID string, domains ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("a0000000-0000-4000-8000-%012d", f.next)
	ds := []map[string]any{}
	for _, d := range domains {
		ds = append(ds, map[string]any{"domain": d})
	}
	f.providers[id] = map[string]any{"id": id, "domains": ds, "saml": map[string]any{"entity_id": entityID, "metadata_xml": testIdPMetadata(entityID), "attribute_mapping": map[string]any{}}}
	f.order = append(f.order, id)
	return id
}
