package api

import (
	"errors"
	"net"
	"net/http"

	"github.com/supavise/supavise/internal/projectconfig"
	"github.com/supavise/supavise/internal/secrets"
)

// serveTemplate serves the saved body of a project's email template to its GoTrue, which is
// configured with this URL (GOTRUE_MAILER_TEMPLATES_<NAME>, see projectconfig.RenderAuth) and
// fetches it over HTTP when it sends a mail. The route carries no credentials, so it answers
// only a client on the loopback interface that did not come through the edge proxy
// (X-Forwarded-For or Forwarded would be set); the templates are not secret, but nothing
// outside the node has a reason to read them either. The URL carries an HMAC token of the
// project and template (Manager.TemplateToken), so a process on the node that merely reaches
// the loopback port, such as user code in an Edge Function, cannot read another project's
// templates; a wrong or missing token is answered like an unknown template.
func (s *Server) serveTemplate(w http.ResponseWriter, r *http.Request) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() ||
		r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return errf(http.StatusNotFound, "Not Found")
	}
	ref := r.PathValue("ref")
	if !secrets.ValidRef(ref) {
		return errf(http.StatusNotFound, "Not Found")
	}
	if !s.settings.TemplateTokenOK(ref, r.PathValue("name"), r.URL.Query().Get("t")) {
		return errf(http.StatusNotFound, "Not Found")
	}
	body, ok, err := s.settings.Template(r.Context(), ref, r.PathValue("name"))
	if errors.Is(err, projectconfig.ErrNotFound) || (err == nil && !ok) {
		return errf(http.StatusNotFound, "Not Found")
	}
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	writeRaw(w, http.StatusOK, "text/html; charset=utf-8", []byte(body))
	return nil
}
