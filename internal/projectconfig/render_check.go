package projectconfig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// checkRenders refuses a save whose settings cannot be written into the unit of the
// service: the supervisor refuses an environment value with a line break or NUL, which
// would otherwise surface only when the project restarts or resumes, with GoTrue or
// PostgREST down. Coerce rejects the same characters per setting, with a better message;
// this is the net under it (a secret that was stored before, or a derived value).
func (m *Manager) checkRenders(ref string, svc Service, set Values, version int64) error {
	var env map[string]string
	switch svc {
	case Auth:
		env = RenderAuth(ref, set, version, m.opts.TemplateBaseURL, "", nil)
	case PostgREST:
		env = RenderPostgREST(set, 0)
	default:
		return nil
	}
	for name, v := range env {
		if strings.ContainsAny(v, "\n\r\x00") {
			return invalid("a setting renders to %s with a line break or NUL byte, which the %s unit cannot carry", name, svc)
		}
	}
	return nil
}

// deriver is what a Secrets implementation offers to sign values with the node's master key
// (secrets.AESGCM).
type deriver interface{ Derive(label string) []byte }

// TemplateToken is the access token in the URL of one of ref's email templates: an HMAC
// under a key derived from the node's master key, so only the project whose GoTrue was given
// the URL can read the template (the route is on the loopback interface, which any process
// of the node can reach). It is "" when the secrets cannot derive keys; the route then
// serves without a token (TemplateTokenOK).
func (m *Manager) TemplateToken(ref, name string) string {
	d, ok := m.sec.(deriver)
	if !ok {
		return ""
	}
	h := hmac.New(sha256.New, d.Derive("email-template-url"))
	fmt.Fprintf(h, "%s/%s", ref, name)
	return hex.EncodeToString(h.Sum(nil))
}

// TemplateTokenOK reports whether got is the token of the template, in constant time. Without
// a derivable key every request passes.
func (m *Manager) TemplateTokenOK(ref, name, got string) bool {
	want := m.TemplateToken(ref, name)
	if want == "" {
		return true
	}
	return hmac.Equal([]byte(want), []byte(got))
}
