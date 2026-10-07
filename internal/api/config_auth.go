package api

import (
	"net/http"
	"strings"

	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
)

// The auth settings of a project are served three ways: the public Management API
// (GET and PATCH /v1/projects/{ref}/config/auth, lower-case names, what the CLI and the MCP
// server use), and Studio's /platform twins (/platform/auth/{ref}/config and .../hooks, the
// same names in upper case). All three read and write projectconfig.AuthSchema.

func (s *Server) routesConfigAuth(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/config/auth", s.getAuthConfig("GET /v1/projects/{ref}/config/auth", false))
	add("PATCH /v1/projects/{ref}/config/auth", s.patchAuthConfig("GET /v1/projects/{ref}/config/auth", false, false))
	add("GET /platform/auth/{ref}/config", s.getAuthConfig("GET /platform/auth/{ref}/config", true))
	add("PATCH /platform/auth/{ref}/config", s.patchAuthConfig("GET /platform/auth/{ref}/config", true, false))
	add("PATCH /platform/auth/{ref}/config/hooks", s.patchAuthConfig("GET /platform/auth/{ref}/config", true, true))
}

// authView is the response of the config GETs: every setting with its effective value,
// secrets as their redaction, on top of the spec's minimal instance so that fields this
// server does not model stay valid.
func authView(key string, st *projectconfig.State, upper bool) map[string]any {
	resp := base(key)
	name := func(n string) string {
		if upper {
			return strings.ToUpper(n)
		}
		return n
	}
	for i := range projectconfig.AuthSchema.Fields {
		f := &projectconfig.AuthSchema.Fields[i]
		k := name(f.Name)
		if _, known := resp[k]; !known {
			continue // the platform spec leaves a few settings out
		}
		v, ok := st.Effective[f.Name]
		if !ok {
			// The public API reports an unset setting as null; Studio's twin has no nullable
			// fields (its forms read an empty string, false or 0 as "not set").
			resp[k] = nil
			if upper {
				resp[k] = zeroValue(f)
			}
			continue
		}
		resp[k] = secretView(f, v)
	}
	resp[name("mailer_autoconfirm")] = st.AuthAutoconfirm()
	if _, ok := resp[name("custom_oauth_max_providers")]; ok {
		resp[name("custom_oauth_max_providers")] = 0
	}
	return resp
}

func (s *Server) getAuthConfig(key string, upper bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.loadProject(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		st, err := s.settings.Get(r.Context(), p.Ref, projectconfig.Auth)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, authView(key, st, upper))
		return nil
	}
}

func (s *Server) patchAuthConfig(key string, upper, hooksOnly bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.settingsProject(r)
		if err != nil {
			return err
		}
		in, err := patchBody(r)
		if err != nil {
			return err
		}
		patch := make(map[string]any, len(in))
		for k, v := range in {
			if upper {
				k = strings.ToLower(k)
			}
			if hooksOnly && !strings.HasPrefix(k, "hook_") {
				continue
			}
			patch[k] = v
		}
		ch, _, err := s.saveSettings(r.Context(), p, projectconfig.Auth, patch, lifecycle.ApplyOptions{})
		if err != nil {
			return err
		}
		for _, c := range ch.Changed {
			if c == "jwt_exp" {
				// PostgREST exposes the token lifetime to SQL (app.settings.jwt_exp).
				s.applyBestEffort(r, p.Ref, projectconfig.PostgREST)
				break
			}
		}
		writeJSON(w, http.StatusOK, authView(key, ch.State, upper))
		return nil
	}
}

// applyBestEffort re-applies svc of ref without failing the request: the change that
// triggered it is already saved and applied.
func (s *Server) applyBestEffort(r *http.Request, ref string, svc projectconfig.Service) {
	rc, ok := s.mgr.(lifecycle.Reconfigurer)
	if !ok {
		return
	}
	ctx, cancel := detached(r, applyTimeout)
	defer cancel()
	if _, err := rc.ApplyConfig(ctx, ref, svc, lifecycle.ApplyOptions{}); err != nil {
		s.log.Warn("could not apply a dependent setting", "ref", ref, "service", svc, "err", err)
	}
}

// zeroValue is the "not set" value of a setting for a response that cannot say null.
func zeroValue(f *projectconfig.Field) any {
	switch f.Kind {
	case projectconfig.Bool:
		return false
	case projectconfig.Int:
		return 0
	case projectconfig.Number:
		return 0.0
	}
	return ""
}
