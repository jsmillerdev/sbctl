package projectconfig

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/OWNER/sbctl/internal/fleet"
)

// Seconds-valued settings that GoTrue takes as a Go duration, and hours-valued ones.
var (
	authSeconds = map[string]bool{"smtp_max_frequency": true, "sms_max_frequency": true, "mfa_phone_max_frequency": true, "api_max_request_duration": true}
	authHours   = map[string]bool{"sessions_timebox": true, "sessions_inactivity_timeout": true}
)

// RenderAuth returns the environment GoTrue gets for the changed settings in set: a value
// to put in the unit's environment, or "" to remove the variable the unit would otherwise
// carry. version is appended to template URLs so a changed template is never served from
// GoTrue's template cache; templateBase is where the daemon serves them
// (<base>/<ref>/<name>); externalURL is the project's API_EXTERNAL_URL.
func RenderAuth(ref string, set Values, version int64, templateBase, externalURL string) map[string]string {
	env := map[string]string{}
	for i := range AuthSchema.Fields {
		f := &AuthSchema.Fields[i]
		v, ok := set[f.Name]
		if !ok || f.Env == "" {
			continue
		}
		switch {
		case strings.HasPrefix(f.Name, "mailer_templates_") && strings.HasSuffix(f.Name, "_content"):
			if s, _ := v.(string); s != "" && templateBase != "" {
				name := strings.TrimSuffix(strings.TrimPrefix(f.Name, "mailer_templates_"), "_content")
				env[f.Env] = fmt.Sprintf("%s/%s/%s?v=%d", strings.TrimRight(templateBase, "/"), ref, name, version)
			}
		case authSeconds[f.Name]:
			n, _ := set.Int(f.Name)
			env[f.Env] = strconv.FormatInt(n, 10) + "s"
		case authHours[f.Name]:
			h, _ := set.Float(f.Name)
			if h > 0 { // GoTrue refuses a zero timebox; hosted treats 0 as off
				env[f.Env] = strconv.FormatFloat(h, 'f', -1, 64) + "h"
			}
		case f.Name == "sms_test_otp":
			s, _ := v.(string)
			env[f.Env] = strings.ReplaceAll(strings.TrimRight(s, ","), "=", ":") // envconfig maps are key:value
		case f.Name == "db_max_pool_size":
			if set.Str("db_max_pool_size_unit") == "percent" {
				env["GOTRUE_DB_CONN_PERCENTAGE"] = scalar(v)
				env[f.Env] = ""
			} else {
				env[f.Env] = scalar(v)
			}
		case strings.HasPrefix(f.Name, "external_") && strings.HasSuffix(f.Name, "_client_id"):
			// GoTrue takes the client ids of a provider as one list; hosted keeps the extra
			// ones apart.
			id, _ := v.(string)
			if extra := set.Str(strings.TrimSuffix(f.Name, "client_id") + "additional_client_ids"); extra != "" && id != "" {
				id += "," + extra
			}
			env[f.Env] = id
		default:
			env[f.Env] = scalar(v)
		}
	}
	// Providers: the callback is GoTrue's default (<external url>/callback), stated
	// explicitly for every enabled provider so it shows in the unit's environment.
	for _, p := range authProviders {
		if set.Bool("external_"+p.name+"_enabled") && externalURL != "" {
			env["GOTRUE_EXTERNAL_"+strings.ToUpper(p.name)+"_REDIRECT_URI"] = strings.TrimRight(externalURL, "/") + "/callback"
		}
	}
	// With SMTP configured people can confirm their address, so confirmation turns on
	// unless the setting was changed explicitly.
	if _, explicit := set["mailer_autoconfirm"]; !explicit && set.Str("smtp_host") != "" {
		env["GOTRUE_MAILER_AUTOCONFIRM"] = "false"
	}
	return env
}

func scalar(v any) string {
	switch x := v.(type) {
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// RenderPostgREST returns PostgREST's environment for the changed settings. jwtExp is the
// JWT lifetime of the project's auth settings (0: not set), which PostgREST exposes to SQL
// as app.settings.jwt_exp.
func RenderPostgREST(set Values, jwtExp int64) map[string]string {
	env := map[string]string{}
	for i := range PostgRESTSchema.Fields {
		f := &PostgRESTSchema.Fields[i]
		v, ok := set[f.Name]
		if !ok {
			continue
		}
		switch f.Name {
		case "db_pool", "db_pool_acquisition_timeout":
			if n, _ := set.Int(f.Name); n == 0 {
				continue // 0 means "default" in the API; PostgREST needs at least 1
			}
		}
		env[f.Env] = scalar(v)
	}
	if jwtExp > 0 {
		env["PGRST_APP_SETTINGS_JWT_EXP"] = strconv.FormatInt(jwtExp, 10)
	}
	return env
}

// RenderPostgres returns the server arguments ("name=value") for the changed settings, in
// schema order. Booleans are on and off, sizes and durations without spaces.
func RenderPostgres(set Values) []string {
	var out []string
	for i := range PostgresSchema.Fields {
		f := &PostgresSchema.Fields[i]
		v, ok := set[f.Name]
		if !ok {
			continue
		}
		if b, isBool := v.(bool); isBool {
			if b {
				out = append(out, f.Name+"=on")
			} else {
				out = append(out, f.Name+"=off")
			}
			continue
		}
		out = append(out, f.Name+"="+scalar(v))
	}
	return out
}

// RenderStorage returns the Storage tenant settings.
func RenderStorage(set Values) fleet.StorageSettings {
	var out fleet.StorageSettings
	if n, ok := set.Int("fileSizeLimit"); ok {
		out.FileSizeLimit = n
	}
	if f, ok := set["features"].(map[string]any); ok {
		out.Features = f
	}
	return out
}

// RenderRealtime returns the Realtime tenant settings: only what differs from the
// server's defaults is sent.
func RenderRealtime(set Values) fleet.RealtimeSettings {
	out := fleet.RealtimeSettings{Tenant: map[string]any{}, Extension: map[string]any{}}
	names := set.Keys()
	sort.Strings(names)
	for _, name := range names {
		switch name {
		case "connection_pool":
			out.Extension["db_pool"] = set[name]
		case "postgres_changes_pool":
			out.Extension["postgres_changes_pool"] = set[name]
		default:
			out.Tenant[name] = set[name]
		}
	}
	return out
}

// The methods below are what the lifecycle engine reads when it renders units and tenants.

// AuthEnv implements lifecycle's settings source: GoTrue's saved settings as environment.
func (m *Manager) AuthEnv(ctx context.Context, ref, externalURL string) (map[string]string, error) {
	st, err := m.Get(ctx, ref, Auth)
	if err != nil {
		return nil, err
	}
	return RenderAuth(ref, st.Set, st.Version, m.opts.TemplateBaseURL, externalURL), nil
}

// PostgRESTEnv is PostgREST's saved settings as environment.
func (m *Manager) PostgRESTEnv(ctx context.Context, ref string) (map[string]string, error) {
	st, err := m.Get(ctx, ref, PostgREST)
	if err != nil {
		return nil, err
	}
	au, err := m.Get(ctx, ref, Auth)
	if err != nil {
		return nil, err
	}
	exp, _ := au.Set.Int("jwt_exp")
	return RenderPostgREST(st.Set, exp), nil
}

// PostgresSettings is the saved server arguments of the project's cluster.
func (m *Manager) PostgresSettings(ctx context.Context, ref string) ([]string, error) {
	st, err := m.Get(ctx, ref, Postgres)
	if err != nil {
		return nil, err
	}
	return RenderPostgres(st.Set), nil
}

// StorageSettings is the saved Storage tenant settings.
func (m *Manager) StorageSettings(ctx context.Context, ref string) (fleet.StorageSettings, error) {
	st, err := m.Get(ctx, ref, Storage)
	if err != nil {
		return fleet.StorageSettings{}, err
	}
	return RenderStorage(st.Set), nil
}

// RealtimeSettings is the saved Realtime tenant settings.
func (m *Manager) RealtimeSettings(ctx context.Context, ref string) (fleet.RealtimeSettings, error) {
	st, err := m.Get(ctx, ref, Realtime)
	if err != nil {
		return fleet.RealtimeSettings{}, err
	}
	return RenderRealtime(st.Set), nil
}

// Template returns the saved body of an email template ("invite", "magic_link", ...) and
// whether one is saved.
func (m *Manager) Template(ctx context.Context, ref, name string) (string, bool, error) {
	if _, ok := AuthSchema.Field("mailer_templates_" + name + "_content"); !ok {
		return "", false, ErrNotFound
	}
	st, err := m.Get(ctx, ref, Auth)
	if err != nil {
		return "", false, err
	}
	s := st.Set.Str("mailer_templates_" + name + "_content")
	return s, s != "", nil
}
