package sso

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// HookPath is where the daemon answers supavise-gotrue@system's before-user-created hook: GoTrue asks
// it before it creates any user, and the daemon allows exactly the users the dashboard accepts
// (a user signing in through a registered SAML provider, a person an administrator invited by
// mail) and refuses every other sign-up. GoTrue's own switch for that (GOTRUE_DISABLE_SIGNUP)
// cannot be used: it also refuses the SSO user's first sign-in, which is how SSO users come to
// exist.
const HookPath = "/internal/hooks/before-user-created"

// HookURL is the address supavise-gotrue@system calls: the daemon's loopback admin listener.
func HookURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Listen.Admin)
	if err != nil {
		return "http://127.0.0.1:7000" + HookPath
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + HookPath
}

type deriver interface{ Derive(label string) []byte }

// ErrNoDerivation is returned by HookSecret for Secrets that cannot derive keys (test doubles).
var ErrNoDerivation = errors.New("sso: the secrets implementation cannot derive keys")

// HookSecret is the shared secret of the hook, in the form GOTRUE_HOOK_*_SECRETS takes
// ("v1,whsec_" and Base64 of the key): derived from the node's master key, so the daemon and
// every process that renders the unit agree on it without storing it anywhere.
func HookSecret(sec secrets.Secrets) (string, error) {
	d, ok := sec.(deriver)
	if !ok {
		return "", ErrNoDerivation
	}
	return "v1,whsec_" + base64.StdEncoding.EncodeToString(d.Derive("gotrue-before-user-created-hook")), nil
}

// WebhookTolerance is how far the timestamp of a signed hook call may be from the clock.
const WebhookTolerance = 5 * time.Minute

// VerifyWebhook checks the Standard Webhooks signature GoTrue puts on an HTTP hook call
// (webhook-id, webhook-timestamp, webhook-signature: "v1,<base64 HMAC-SHA256 of
// id.timestamp.body>", several separated by spaces) against secret.
func VerifyWebhook(secret string, h http.Header, body []byte, now time.Time) error {
	key, ok := strings.CutPrefix(secret, "v1,whsec_")
	if !ok {
		return errors.New("unsupported hook secret")
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return errors.New("malformed hook secret")
	}
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return errors.New("missing webhook headers")
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("malformed webhook timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > WebhookTolerance || d < -WebhookTolerance {
		return errors.New("webhook timestamp out of range")
	}
	m := hmac.New(sha256.New, raw)
	fmt.Fprintf(m, "%s.%s.", id, ts)
	m.Write(body)
	want := m.Sum(nil)
	for _, part := range strings.Fields(sigs) {
		v, ok := strings.CutPrefix(part, "v1,")
		if !ok {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(v)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errors.New("webhook signature does not match")
}

// SignWebhook is what GoTrue does: the three headers for a call with this body. Tests use it.
func SignWebhook(secret, id string, at time.Time, body []byte) (http.Header, error) {
	key, ok := strings.CutPrefix(secret, "v1,whsec_")
	if !ok {
		return nil, errors.New("unsupported hook secret")
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	m := hmac.New(sha256.New, raw)
	fmt.Fprintf(m, "%s.%s.", id, ts)
	m.Write(body)
	h := http.Header{}
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", ts)
	h.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	return h, nil
}

// HookEvent is the part of the before-user-created payload the decision needs.
type HookEvent struct {
	Metadata struct {
		IPAddress string `json:"ip_address"`
	} `json:"metadata"`
	User struct {
		Email       string         `json:"email"`
		AppMetadata map[string]any `json:"app_metadata"`
		UserMeta    map[string]any `json:"user_metadata"`
	} `json:"user"`
}

// Provider returns the SSO provider id a user is being created for ("sso:<id>" in the user's
// app_metadata.provider, which GoTrue sets and a client cannot), or "" for any other kind.
func (e *HookEvent) Provider() string {
	p, _ := e.User.AppMetadata["provider"].(string)
	if id, ok := strings.CutPrefix(p, "sso:"); ok {
		return strings.ToLower(id)
	}
	return ""
}

// GrantToken returns the one-time invitation token the daemon put in the user_metadata of the
// invite it sent to GoTrue ("" when there is none).
func (e *HookEvent) GrantToken() string {
	s, _ := e.User.UserMeta[GrantKey].(string)
	return s
}

// GrantKey is the user_metadata key that carries the one-time sign-up grant of an invitation.
const GrantKey = "supavise_grant"

// HookRefusal is the answer that stops GoTrue from creating the user.
func HookRefusal(message string) map[string]any {
	return map[string]any{"error": map[string]any{"http_code": http.StatusForbidden, "message": message}}
}
