package config

import "strings"

// API is the [api] config section: settings of the Management API server
// (internal/api). Everything has a working default.
type API struct {
	// AllowedOrigins is a comma-separated list of browser origins allowed to call the
	// API with credentials (CORS), in addition to https://<studio host>. "*" is not
	// accepted: Studio sends Authorization headers, so origins are listed explicitly.
	AllowedOrigins string `toml:"allowed_origins"`
	// PGMetaCryptoKey is the passphrase shared with supavise-pgmeta (its CRYPTO_KEY
	// variable) for the x-connection-encrypted header. Empty means a random key kept
	// sealed in the registry (system project secret "pgmeta_crypto_key").
	PGMetaCryptoKey string `toml:"pgmeta_crypto_key"`
	// PublicURL is the externally visible origin of the API ("https://api.<domain>").
	// Empty derives it from the base domain, with https unless tls.mode is "off".
	PublicURL string `toml:"public_url"`
	// DashboardURL is the externally visible Studio origin. Empty derives it from
	// the studio host like PublicURL.
	DashboardURL string `toml:"dashboard_url"`
	// DisableDeviceLogin turns off the `supabase login` browser flow (PATs created in
	// the dashboard keep working).
	DisableDeviceLogin bool `toml:"disable_device_login"`
	// AdminEmails is a comma-separated allowlist of dashboard users (matched on the
	// session's email, case-insensitively) who may use the API even without the
	// app_metadata.supavise_admin claim that supavise sets on the users it creates.
	AdminEmails string `toml:"admin_emails"`
}

// Admins returns AdminEmails split, trimmed and lower-cased, empty entries removed.
func (a API) Admins() []string {
	var out []string
	for _, e := range strings.Split(a.AdminEmails, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Origins returns AllowedOrigins split and trimmed, empty entries removed.
func (a API) Origins() []string {
	var out []string
	for _, o := range strings.Split(a.AllowedOrigins, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// scheme is the URL scheme supavise's public listeners speak.
func (c *Config) scheme() string {
	if c.TLS.Mode == "off" {
		return "http"
	}
	return "https"
}

// APIURL is the public origin of the Management API, no trailing slash.
func (c *Config) APIURL() string {
	if c.API.PublicURL != "" {
		return strings.TrimRight(c.API.PublicURL, "/")
	}
	return c.scheme() + "://" + c.APIHost()
}

// DashboardURL is the public origin of Studio, no trailing slash.
func (c *Config) DashboardURL() string {
	if c.API.DashboardURL != "" {
		return strings.TrimRight(c.API.DashboardURL, "/")
	}
	return c.scheme() + "://" + c.StudioHost()
}
