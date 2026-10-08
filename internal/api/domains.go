package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/supavise/supavise/internal/domains"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/registry"
)

// Custom hostnames and vanity subdomains (internal/domains), the endpoints of the Management
// API's "Domains" tag that Studio's Settings > Custom Domains page and the CLI's `supabase
// domains` and `vanity-subdomains` call. Changing either also changes the project's GoTrue
// (API_EXTERNAL_URL, see lifecycle.presentedAuthURL), which is restarted here.

const (
	keyHostnameGet = "GET /v1/projects/{ref}/custom-hostname"
	keyHostnameNew = "POST /v1/projects/{ref}/custom-hostname/initialize"
	keyHostnameVer = "POST /v1/projects/{ref}/custom-hostname/reverify"
	keyHostnameAct = "POST /v1/projects/{ref}/custom-hostname/activate"
)

func (s *Server) routesDomains(add func(string, handlerFunc)) {
	add(keyHostnameGet, s.getCustomHostname)
	add("DELETE /v1/projects/{ref}/custom-hostname", s.deleteCustomHostname)
	add(keyHostnameNew, s.initCustomHostname)
	add(keyHostnameVer, s.reverifyCustomHostname)
	add(keyHostnameAct, s.activateCustomHostname)
	add("GET /v1/projects/{ref}/vanity-subdomain", s.getVanity)
	add("DELETE /v1/projects/{ref}/vanity-subdomain", s.deleteVanity)
	add("POST /v1/projects/{ref}/vanity-subdomain/check-availability", s.checkVanity)
	add("POST /v1/projects/{ref}/vanity-subdomain/activate", s.activateVanity)
}

// domainSvc returns the domain service or the error of a node that cannot serve it.
func (s *Server) domainSvc() (*domains.Service, error) {
	if s.domains == nil {
		return nil, errf(http.StatusNotImplemented, "This server's registry does not support custom domains")
	}
	return s.domains, nil
}

// domainError turns a domains.Error into the API error of its kind. A rate-limit refusal is
// written here, with its Retry-After header, and reported as handled (nil).
func domainError(w http.ResponseWriter, err error) error {
	if errors.Is(err, registry.ErrNotFound) {
		return errNoProject
	}
	e, ok := domains.AsError(err)
	if !ok {
		return err
	}
	switch e.Kind {
	case domains.KindConflict, domains.KindState:
		return errf(http.StatusConflict, "%s", e.Msg)
	case domains.KindRateLimited:
		secs := int(e.RetryAfter/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		return errf(http.StatusTooManyRequests, "%s", e.Msg)
	}
	return errf(http.StatusBadRequest, "%s", e.Msg)
}

// hostnameView is the body of the four custom-hostname calls: hosted's Cloudflare-shaped
// result, filled with what Supavise knows. Studio shows the TXT record from ssl.txt_name and
// ssl.txt_value, and the CNAME row while verification_errors lists CNAMEMismatch.
func hostnameView(key string, st *domains.State) map[string]any {
	resp := base(key)
	resp["status"] = st.Status
	resp["custom_hostname"] = st.Hostname
	errs := make([]any, 0, len(st.Errors))
	for _, e := range st.Errors {
		errs = append(errs, e)
	}
	verified := st.Status == registry.HostnameOriginReady || st.Status == registry.HostnameActive
	sslStatus, resStatus := "pending_validation", "pending"
	if verified {
		sslStatus = "active"
	}
	if st.Status == registry.HostnameActive {
		resStatus = "active"
	}
	ssl := map[string]any{
		"id": st.Ref, "type": "dv", "method": "txt", "status": sslStatus, "wildcard": false,
		"bundle_method": "ubiquitous", "certificate_authority": "lets_encrypt",
		"settings": map[string]any{"http2": "on", "tls_1_3": "on", "min_tls_version": "1.0"},
	}
	if !verified {
		ssl["txt_name"], ssl["txt_value"] = st.TXTName, st.TXTValue
		ssl["validation_records"] = []any{map[string]any{"status": "pending", "txt_name": st.TXTName, "txt_value": st.TXTValue}}
	}
	result := map[string]any{
		"id": st.Ref, "hostname": st.Hostname, "status": resStatus, "ssl": ssl,
		"created_at": st.CreatedAt.UTC().Format(time.RFC3339), "custom_metadata": map[string]any{},
		"custom_origin_server": st.Target, "verification_errors": errs,
		"ownership_verification": map[string]any{"type": "txt", "name": st.TXTName, "value": st.TXTValue},
	}
	resp["data"] = map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result}
	return resp
}

func (s *Server) getCustomHostname(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	st, err := svc.Hostname(r.Context(), p.Ref)
	if err != nil {
		return domainError(w, err)
	}
	writeJSON(w, http.StatusOK, hostnameView(keyHostnameGet, st))
	return nil
}

func (s *Server) initCustomHostname(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in struct {
		Hostname string `json:"custom_hostname"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	st, err := svc.Initialize(r.Context(), p.Ref, in.Hostname)
	if err != nil {
		return domainError(w, err)
	}
	writeJSON(w, http.StatusCreated, hostnameView(keyHostnameNew, st))
	return nil
}

func (s *Server) reverifyCustomHostname(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	st, err := svc.Reverify(r.Context(), p.Ref)
	if err != nil {
		return domainError(w, err)
	}
	writeJSON(w, http.StatusCreated, hostnameView(keyHostnameVer, st))
	return nil
}

func (s *Server) activateCustomHostname(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	st, _, err := svc.Activate(r.Context(), p.Ref)
	if err != nil {
		return domainError(w, err)
	}
	// Also when the hostname was active already: a first attempt may have failed here, and
	// activating again is how the user retries it.
	if err := s.applyDomainChange(r, p.Ref); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, hostnameView(keyHostnameAct, st))
	return nil
}

func (s *Server) deleteCustomHostname(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	_, err = svc.Delete(r.Context(), p.Ref)
	if err != nil {
		// Also when there is nothing left to delete: a first attempt may have failed at Auth, and
		// deleting again is how the user retries it. The refusal is still answered.
		if e, ok := domains.AsError(err); ok && e.Kind == domains.KindNotConfigured {
			if aerr := s.applyDomainChange(r, p.Ref); aerr != nil {
				return aerr
			}
		}
		return domainError(w, err)
	}
	if err := s.applyDomainChange(r, p.Ref); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// applyDomainChange restarts the project's GoTrue on the external URL its domains now give
// (lifecycle renders it from the registry). A paused project picks it up when it resumes.
func (s *Server) applyDomainChange(r *http.Request, ref string) error {
	rc, ok := s.mgr.(lifecycle.Reconfigurer)
	if !ok {
		return nil
	}
	ctx, cancel := detached(r, applyTimeout)
	defer cancel()
	if _, err := rc.ApplyConfig(ctx, ref, projectconfig.Auth, lifecycle.ApplyOptions{}); err != nil {
		if errors.Is(err, lifecycle.ErrInvalidState) {
			return errf(http.StatusConflict, "%v; the domain change is saved, activate again once the project is running to apply it to Auth", err)
		}
		s.log.Error("could not apply a domain change to Auth", "ref", ref, "err", err)
		return errf(http.StatusInternalServerError, "The domain change is saved but Auth could not be restarted on it; repeat the request to try again")
	}
	return nil
}

type vanityBody struct {
	Name string `json:"vanity_subdomain"`
}

func (s *Server) getVanity(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	v, err := svc.Vanity(r.Context(), p.Ref)
	if err != nil {
		return domainError(w, err)
	}
	resp := base("GET /v1/projects/{ref}/vanity-subdomain")
	resp["status"] = v.Status
	if v.CustomDomain != "" {
		resp["custom_domain"] = v.CustomDomain
	} else {
		delete(resp, "custom_domain")
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) checkVanity(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in vanityBody
	if err := decode(r, &in); err != nil {
		return err
	}
	ok, err := svc.CheckVanity(r.Context(), p.Ref, in.Name)
	if err != nil {
		return domainError(w, err)
	}
	resp := base("POST /v1/projects/{ref}/vanity-subdomain/check-availability")
	resp["available"] = ok
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

func (s *Server) activateVanity(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in vanityBody
	if err := decode(r, &in); err != nil {
		return err
	}
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	host, err := svc.ActivateVanity(r.Context(), p.Ref, in.Name)
	if err != nil {
		return domainError(w, err)
	}
	// Always, as for a custom hostname: activating the name the project has already is how a
	// caller retries an Auth restart that failed after the change was stored.
	if err := s.applyDomainChange(r, p.Ref); err != nil {
		return err
	}
	resp := base("POST /v1/projects/{ref}/vanity-subdomain/activate")
	resp["custom_domain"] = host
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

func (s *Server) deleteVanity(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.domainSvc()
	if err != nil {
		return err
	}
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	if err := svc.DeleteVanity(r.Context(), p.Ref); err != nil {
		// Also when there is nothing left to delete, so that a delete whose Auth restart failed can
		// be repeated; the refusal is still answered.
		if e, ok := domains.AsError(err); ok && e.Kind == domains.KindNotConfigured {
			if aerr := s.applyDomainChange(r, p.Ref); aerr != nil {
				return aerr
			}
		}
		return domainError(w, err)
	}
	if err := s.applyDomainChange(r, p.Ref); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
