package projectconfig

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/gobwas/glob"
)

// AuthSchema is the settings of a project's GoTrue, named as in the Management API
// (UpdateAuthConfigBody of the v1 spec; the /platform twins use the same names in upper
// case). Every setting has the environment variable GoTrue reads, verified against
// supabase/auth internal/conf at the pinned release (testdata/gotrue-env-names.txt,
// TestAuthEnvNamesExist). Settings with no Env are stored and reported but GoTrue has no
// matching variable in the self-hosted build (see the README).
var AuthSchema = buildAuthSchema()

// Names of the email templates and their GoTrue variable suffix. The template body is
// stored as the setting mailer_templates_<name>_content and served by the daemon; GoTrue
// is given its URL.
var authTemplates = []string{
	"invite", "confirmation", "recovery", "email_change", "magic_link", "reauthentication",
	"password_changed_notification", "email_changed_notification", "phone_changed_notification",
	"mfa_factor_enrolled_notification", "mfa_factor_unenrolled_notification",
	"identity_linked_notification", "identity_unlinked_notification",
}

// TemplateNames lists the template names (invite, confirmation, ...).
func TemplateNames() []string { return append([]string(nil), authTemplates...) }

var authNotifications = authTemplates[6:]

// Hook points of GoTrue's extensibility API.
var authHooks = []string{
	"mfa_verification_attempt", "password_verification_attempt", "custom_access_token",
	"send_sms", "send_email", "before_user_created", "after_user_created",
}

type provider struct {
	name string
	// extras are the optional settings of the provider besides enabled, client_id, secret.
	emailOptional bool
	url           bool
	additionalIDs bool
	skipNonce     bool
}

// The external providers of the Management API that GoTrue v2.195 supports.
var authProviders = []provider{
	{name: "apple", emailOptional: true, additionalIDs: true},
	{name: "azure", emailOptional: true, url: true},
	{name: "bitbucket", emailOptional: true},
	{name: "discord", emailOptional: true},
	{name: "facebook", emailOptional: true},
	{name: "figma", emailOptional: true},
	{name: "github", emailOptional: true},
	{name: "gitlab", emailOptional: true, url: true},
	{name: "google", emailOptional: true, additionalIDs: true, skipNonce: true},
	{name: "kakao", emailOptional: true},
	{name: "keycloak", emailOptional: true, url: true},
	{name: "linkedin_oidc", emailOptional: true},
	{name: "slack_oidc", emailOptional: true},
	{name: "notion", emailOptional: true},
	{name: "slack", emailOptional: true},
	{name: "spotify", emailOptional: true},
	{name: "twitch", emailOptional: true},
	{name: "twitter", emailOptional: true},
	{name: "x", emailOptional: true},
	{name: "workos", url: true},
	{name: "zoom", emailOptional: true},
}

// ProviderNames lists the external providers.
func ProviderNames() []string {
	out := make([]string, len(authProviders))
	for i, p := range authProviders {
		out[i] = p.name
	}
	return out
}

var (
	noComma      = regexp.MustCompile(`^[^,]+$`)
	emailPattern = regexp.MustCompile(`^[A-Za-z0-9_'+\-.]*[A-Za-z0-9_+\-]@([A-Za-z0-9][A-Za-z0-9\-]*\.)+[A-Za-z]{2,}$`)
	sessionTags  = regexp.MustCompile(`^\s*([a-zA-Z0-9_-]+(\s*,+\s*)?)*\s*$`)
	smsTestOTP   = regexp.MustCompile(`^([0-9]{1,15}=[0-9]+,?)*$`)
	// GoTrue's own formats (internal/conf/configuration.go).
	hookSecretSym  = regexp.MustCompile(`^v1,whsec_[A-Za-z0-9+/=]{32,88}`)
	hookSecretAsym = regexp.MustCompile(`^v1a,whpk_[A-Za-z0-9+/=]{44,}:whsk_[A-Za-z0-9+/=]{44,}$`)
	pgName         = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)
	rfc3339        = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:\d{2})$`)
	// smtp ports are strings in the API.
	portPattern = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// passwordRequired are the values the spec allows for password_required_characters.
var passwordRequired = []string{
	"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456789",
	"abcdefghijklmnopqrstuvwxyz:ABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456789",
	"abcdefghijklmnopqrstuvwxyz:ABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456789:!@#$%^&*()_+-=[]{};'\\\\:\"|<>?,./`~",
}

func fBool(name, env string, def any) Field {
	return Field{Name: name, Kind: Bool, Env: env, Default: def}
}
func fStr(name, env string) Field { return Field{Name: name, Kind: String, Env: env} }
func fSecret(name, env string) Field {
	return Field{Name: name, Kind: String, Env: env, Secret: true}
}
func fInt(name, env string, def any, min, max float64) Field {
	return Field{Name: name, Kind: Int, Env: env, Default: def, Min: min, Max: max, HasRange: true}
}

const maxInt32 = 2147483647

func buildAuthSchema() *Schema {
	var f []Field
	add := func(fs ...Field) { f = append(f, fs...) }

	site := fStr("site_url", "GOTRUE_SITE_URL")
	site.Default, site.Pattern, site.Check = "http://localhost:3000", noComma, checkURL
	allow := fStr("uri_allow_list", "GOTRUE_URI_ALLOW_LIST")
	allow.MaxLen, allow.Check = 32768, checkAllowList
	add(site, allow,
		fBool("disable_signup", "GOTRUE_DISABLE_SIGNUP", false),
		fInt("jwt_exp", "GOTRUE_JWT_EXP", int64(3600), 0, 604800),
		fBool("external_anonymous_users_enabled", "GOTRUE_EXTERNAL_ANONYMOUS_USERS_ENABLED", false),
		fBool("external_email_enabled", "GOTRUE_EXTERNAL_EMAIL_ENABLED", true),
		fBool("external_phone_enabled", "GOTRUE_EXTERNAL_PHONE_ENABLED", false),
		fBool("mailer_allow_unverified_email_sign_ins", "GOTRUE_MAILER_ALLOW_UNVERIFIED_EMAIL_SIGN_INS", false),
		fBool("mailer_autoconfirm", "GOTRUE_MAILER_AUTOCONFIRM", true),
		fBool("mailer_secure_email_change_enabled", "GOTRUE_MAILER_SECURE_EMAIL_CHANGE_ENABLED", true),
		fInt("mailer_otp_exp", "GOTRUE_MAILER_OTP_EXP", int64(86400), 0, maxInt32),
		fInt("mailer_otp_length", "GOTRUE_MAILER_OTP_LENGTH", int64(6), 6, 10),
	)

	// SMTP.
	email := fStr("smtp_admin_email", "GOTRUE_SMTP_ADMIN_EMAIL")
	email.Pattern = emailPattern
	port := fStr("smtp_port", "GOTRUE_SMTP_PORT")
	port.Pattern, port.Check = portPattern, checkPort
	freq := fInt("smtp_max_frequency", "GOTRUE_SMTP_MAX_FREQUENCY", int64(60), 0, 32767)
	add(email, fStr("smtp_host", "GOTRUE_SMTP_HOST"), port, fStr("smtp_user", "GOTRUE_SMTP_USER"),
		fSecret("smtp_pass", "GOTRUE_SMTP_PASS"), freq, fStr("smtp_sender_name", "GOTRUE_SMTP_SENDER_NAME"))

	// Subjects, templates, notifications.
	for _, t := range authTemplates {
		add(fStr("mailer_subjects_"+t, "GOTRUE_MAILER_SUBJECTS_"+strings.ToUpper(t)))
	}
	for _, t := range authTemplates {
		c := fStr("mailer_templates_"+t+"_content", "GOTRUE_MAILER_TEMPLATES_"+strings.ToUpper(t))
		c.MaxLen = 1_000_000 // GoTrue's template_max_size default
		c.Multiline = true   // served by URL, not carried in the environment
		add(c)
	}
	for _, t := range authNotifications {
		add(fBool("mailer_notifications_"+strings.TrimSuffix(t, "_notification")+"_enabled", "GOTRUE_MAILER_NOTIFICATIONS_"+strings.ToUpper(strings.TrimSuffix(t, "_notification"))+"_ENABLED", false))
	}

	// MFA, passkeys.
	add(fInt("mfa_max_enrolled_factors", "GOTRUE_MFA_MAX_ENROLLED_FACTORS", int64(10), 0, maxInt32),
		fBool("mfa_totp_enroll_enabled", "GOTRUE_MFA_TOTP_ENROLL_ENABLED", true),
		fBool("mfa_totp_verify_enabled", "GOTRUE_MFA_TOTP_VERIFY_ENABLED", true),
		fBool("mfa_web_authn_enroll_enabled", "GOTRUE_MFA_WEB_AUTHN_ENROLL_ENABLED", false),
		fBool("mfa_web_authn_verify_enabled", "GOTRUE_MFA_WEB_AUTHN_VERIFY_ENABLED", false),
		fBool("mfa_phone_enroll_enabled", "GOTRUE_MFA_PHONE_ENROLL_ENABLED", false),
		fBool("mfa_phone_verify_enabled", "GOTRUE_MFA_PHONE_VERIFY_ENABLED", false),
		fInt("mfa_phone_max_frequency", "GOTRUE_MFA_PHONE_MAX_FREQUENCY", int64(60), 0, 32767),
		fInt("mfa_phone_otp_length", "GOTRUE_MFA_PHONE_OTP_LENGTH", int64(6), 0, 32767),
		fStr("mfa_phone_template", "GOTRUE_MFA_PHONE_TEMPLATE"),
		fBool("passkey_enabled", "GOTRUE_PASSKEY_ENABLED", false),
		fStr("webauthn_rp_display_name", "GOTRUE_WEBAUTHN_RP_DISPLAY_NAME"),
		fStr("webauthn_rp_id", "GOTRUE_WEBAUTHN_RP_ID"),
		fStr("webauthn_rp_origins", "GOTRUE_WEBAUTHN_RP_ORIGINS"),
	)

	// SAML is workstream L's: stored and reported, not rendered, because GoTrue refuses to
	// start with SAML enabled and no signing key.
	add(fBool("saml_enabled", "", false), Field{Name: "saml_external_url", Kind: String, Pattern: noComma})

	// Security, sessions, captcha.
	captcha := Field{Name: "security_captcha_provider", Kind: Enum, Enum: []string{"turnstile", "hcaptcha"}, Env: "GOTRUE_SECURITY_CAPTCHA_PROVIDER"}
	add(fBool("security_sb_forwarded_for_enabled", "GOTRUE_SECURITY_SB_FORWARDED_FOR_ENABLED", false),
		fBool("security_captcha_enabled", "GOTRUE_SECURITY_CAPTCHA_ENABLED", false), captcha,
		fSecret("security_captcha_secret", "GOTRUE_SECURITY_CAPTCHA_SECRET"),
		fBool("security_manual_linking_enabled", "GOTRUE_SECURITY_MANUAL_LINKING_ENABLED", false),
		fBool("security_update_password_require_reauthentication", "GOTRUE_SECURITY_UPDATE_PASSWORD_REQUIRE_REAUTHENTICATION", false),
		fBool("security_update_password_require_current_password", "GOTRUE_SECURITY_UPDATE_PASSWORD_REQUIRE_CURRENT_PASSWORD", false),
		fInt("security_refresh_token_reuse_interval", "GOTRUE_SECURITY_REFRESH_TOKEN_REUSE_INTERVAL", int64(10), 0, maxInt32),
		fBool("refresh_token_rotation_enabled", "GOTRUE_SECURITY_REFRESH_TOKEN_ROTATION_ENABLED", true),
	)
	for _, n := range []string{"sessions_timebox", "sessions_inactivity_timeout"} {
		add(Field{Name: n, Kind: Number, Env: "GOTRUE_" + strings.ToUpper(n), Min: 0, Max: 100000, HasRange: true})
	}
	tags := fStr("sessions_tags", "GOTRUE_SESSIONS_TAGS")
	tags.Pattern = sessionTags
	add(fBool("sessions_single_per_user", "GOTRUE_SESSIONS_SINGLE_PER_USER", false), tags)

	// Rate limits.
	for _, r := range []struct {
		n   string
		def int64
	}{{"anonymous_users", 30}, {"email_sent", 30}, {"sms_sent", 30}, {"verify", 30}, {"token_refresh", 150}, {"otp", 30}, {"web3", 30}} {
		add(fInt("rate_limit_"+r.n, "GOTRUE_RATE_LIMIT_"+strings.ToUpper(r.n), r.def, 1, maxInt32))
	}

	// Passwords.
	chars := Field{Name: "password_required_characters", Kind: Enum, Enum: passwordRequired, AllowEmpty: true, Env: "GOTRUE_PASSWORD_REQUIRED_CHARACTERS", Default: ""}
	add(fBool("password_hibp_enabled", "GOTRUE_PASSWORD_HIBP_ENABLED", false),
		fInt("password_min_length", "GOTRUE_PASSWORD_MIN_LENGTH", int64(6), 6, 32767), chars)

	// SMS.
	provider := Field{Name: "sms_provider", Kind: Enum, Enum: []string{"messagebird", "textlocal", "twilio", "twilio_verify", "vonage"}, Env: "GOTRUE_SMS_PROVIDER"}
	testOTP := fStr("sms_test_otp", "GOTRUE_SMS_TEST_OTP")
	testOTP.Pattern = smsTestOTP
	until := fStr("sms_test_otp_valid_until", "GOTRUE_SMS_TEST_OTP_VALID_UNTIL")
	until.Pattern = rfc3339
	add(fBool("sms_autoconfirm", "GOTRUE_SMS_AUTOCONFIRM", false),
		fInt("sms_max_frequency", "GOTRUE_SMS_MAX_FREQUENCY", int64(60), 0, 32767),
		fInt("sms_otp_exp", "GOTRUE_SMS_OTP_EXP", int64(60), 0, maxInt32),
		fInt("sms_otp_length", "GOTRUE_SMS_OTP_LENGTH", int64(6), 0, 32767),
		provider,
		fSecret("sms_messagebird_access_key", "GOTRUE_SMS_MESSAGEBIRD_ACCESS_KEY"), fStr("sms_messagebird_originator", "GOTRUE_SMS_MESSAGEBIRD_ORIGINATOR"),
		testOTP, until,
		fSecret("sms_textlocal_api_key", "GOTRUE_SMS_TEXTLOCAL_API_KEY"), fStr("sms_textlocal_sender", "GOTRUE_SMS_TEXTLOCAL_SENDER"),
		fStr("sms_twilio_account_sid", "GOTRUE_SMS_TWILIO_ACCOUNT_SID"), fSecret("sms_twilio_auth_token", "GOTRUE_SMS_TWILIO_AUTH_TOKEN"),
		fStr("sms_twilio_content_sid", "GOTRUE_SMS_TWILIO_CONTENT_SID"), fStr("sms_twilio_message_service_sid", "GOTRUE_SMS_TWILIO_MESSAGE_SERVICE_SID"),
		fStr("sms_twilio_verify_account_sid", "GOTRUE_SMS_TWILIO_VERIFY_ACCOUNT_SID"), fSecret("sms_twilio_verify_auth_token", "GOTRUE_SMS_TWILIO_VERIFY_AUTH_TOKEN"),
		fStr("sms_twilio_verify_message_service_sid", "GOTRUE_SMS_TWILIO_VERIFY_MESSAGE_SERVICE_SID"),
		fStr("sms_vonage_api_key", "GOTRUE_SMS_VONAGE_API_KEY"), fSecret("sms_vonage_api_secret", "GOTRUE_SMS_VONAGE_API_SECRET"), fStr("sms_vonage_from", "GOTRUE_SMS_VONAGE_FROM"),
		fStr("sms_template", "GOTRUE_SMS_TEMPLATE"),
	)

	// Auth hooks.
	for _, h := range authHooks {
		up := strings.ToUpper(h)
		uri := fStr("hook_"+h+"_uri", "GOTRUE_HOOK_"+up+"_URI")
		uri.Check = checkHookURI
		sec := fSecret("hook_"+h+"_secrets", "GOTRUE_HOOK_"+up+"_SECRETS")
		sec.Check = checkHookSecrets
		add(fBool("hook_"+h+"_enabled", "GOTRUE_HOOK_"+up+"_ENABLED", false), uri, sec)
	}

	// External providers.
	for _, p := range authProviders {
		up := strings.ToUpper(p.name)
		add(fBool("external_"+p.name+"_enabled", "GOTRUE_EXTERNAL_"+up+"_ENABLED", false),
			fStr("external_"+p.name+"_client_id", "GOTRUE_EXTERNAL_"+up+"_CLIENT_ID"),
			fSecret("external_"+p.name+"_secret", "GOTRUE_EXTERNAL_"+up+"_SECRET"))
		if p.emailOptional {
			add(fBool("external_"+p.name+"_email_optional", "GOTRUE_EXTERNAL_"+up+"_EMAIL_OPTIONAL", false))
		}
		if p.url {
			u := fStr("external_"+p.name+"_url", "GOTRUE_EXTERNAL_"+up+"_URL")
			u.Check = checkURL
			add(u)
		}
		if p.additionalIDs {
			add(fStr("external_"+p.name+"_additional_client_ids", ""))
		}
		if p.skipNonce {
			add(fBool("external_"+p.name+"_skip_nonce_check", "GOTRUE_EXTERNAL_"+up+"_SKIP_NONCE_CHECK", false))
		}
	}
	add(fBool("external_web3_solana_enabled", "GOTRUE_EXTERNAL_WEB3_SOLANA_ENABLED", false),
		fBool("external_web3_ethereum_enabled", "GOTRUE_EXTERNAL_WEB3_ETHEREUM_ENABLED", false))

	// Database pool, request duration, OAuth server.
	unit := Field{Name: "db_max_pool_size_unit", Kind: Enum, Enum: []string{"connections", "percent"}, Default: "connections"}
	add(Field{Name: "db_max_pool_size", Kind: Int, Env: "GOTRUE_DB_MAX_POOL_SIZE", Default: int64(5), Min: 1, Max: 100000, HasRange: true}, unit,
		Field{Name: "api_max_request_duration", Kind: Int, Env: "GOTRUE_API_MAX_REQUEST_DURATION", Default: int64(10), Min: 1, Max: 3600, HasRange: true},
		fBool("oauth_server_enabled", "GOTRUE_OAUTH_SERVER_ENABLED", false),
		fBool("oauth_server_allow_dynamic_registration", "GOTRUE_OAUTH_SERVER_ALLOW_DYNAMIC_REGISTRATION", false),
		fStr("oauth_server_authorization_path", "GOTRUE_OAUTH_SERVER_AUTHORIZATION_PATH"),
		fBool("custom_oauth_enabled", "GOTRUE_CUSTOM_OAUTH_ENABLED", true),
		// Hosted-only settings of the specs, kept so a client can save and read them back.
		fStr("nimbus_oauth_client_id", ""), fSecret("nimbus_oauth_client_secret", ""),
	)

	s := NewSchema(Auth, f)
	s.Fixup = authFixup
	s.Cross = authCross
	return s
}

func checkURL(v any) error {
	s, _ := v.(string)
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || (u.Host == "" && u.Opaque == "") {
		return fmt.Errorf("must be an absolute URL")
	}
	return nil
}

func checkPort(v any) error {
	var p int
	if _, err := fmt.Sscanf(v.(string), "%d", &p); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("must be a port between 1 and 65535")
	}
	return nil
}

// checkAllowList rejects entries GoTrue's glob compiler would refuse at start and entries
// that are neither a URL nor a pattern.
func checkAllowList(v any) error {
	for _, e := range strings.Split(v.(string), ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		// GoTrue compiles each entry at start with glob.MustCompile(uri, '.', '/') and
		// panics on a bad one, so an entry GoTrue would refuse must never be saved.
		if _, err := glob.Compile(e, '.', '/'); err != nil {
			return fmt.Errorf("redirect pattern %q: %v", e, err)
		}
		if !strings.ContainsAny(e, "*?[{") {
			if err := checkURL(e); err != nil {
				return fmt.Errorf("redirect URL %q: %v", e, err)
			}
		}
	}
	return nil
}

// checkHookURI is GoTrue's ValidateExtensibilityPoint: pg-functions://<db>/<schema>/<function>,
// https URLs, and http only for loopback.
func checkHookURI(v any) error {
	s, _ := v.(string)
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	switch strings.ToLower(u.Scheme) {
	case "pg-functions":
		parts := strings.Split(u.Path, "/")
		if len(parts) < 3 || !pgName.MatchString(parts[1]) || !pgName.MatchString(parts[2]) {
			return fmt.Errorf("must look like pg-functions://postgres/<schema>/<function>")
		}
		return nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
		return fmt.Errorf("http is only supported for localhost, 127.0.0.1 and ::1")
	case "https":
		return nil
	}
	return fmt.Errorf("only postgres functions (pg-functions://) and HTTPS URLs are supported")
}

// checkHookSecrets validates the "|"-separated secrets GoTrue signs hook requests with.
func checkHookSecrets(v any) error {
	for _, sec := range strings.Split(v.(string), "|") {
		if sec == "" {
			continue
		}
		if !hookSecretSym.MatchString(sec) && !hookSecretAsym.MatchString(sec) {
			return fmt.Errorf("a hook secret must look like v1,whsec_<base64>")
		}
	}
	return nil
}

// authFixup: switching SMTP off (smtp_host set to "") resets every smtp_* setting, as the
// hosted API does.
func authFixup(set Values, touched map[string]bool) {
	if touched["smtp_host"] {
		if h, ok := set["smtp_host"].(string); ok && h == "" {
			for k := range set {
				if strings.HasPrefix(k, "smtp_") {
					delete(set, k)
				}
			}
		}
	}
}

// authCross validates combinations GoTrue refuses at start or that would lock users out.
func authCross(eff, set Values, _ CrossContext) error {
	if eff.Bool("security_captcha_enabled") {
		if eff.Str("security_captcha_provider") == "" {
			return invalid("security_captcha_provider is required when captcha is enabled")
		}
		if strings.TrimSpace(eff.Str("security_captcha_secret")) == "" {
			return invalid("security_captcha_secret is required when captcha is enabled")
		}
	}
	if eff.Str("smtp_host") != "" {
		if eff.Str("smtp_admin_email") == "" {
			return invalid("smtp_admin_email is required when smtp_host is set")
		}
	}
	if eff.Bool("passkey_enabled") || eff.Bool("mfa_web_authn_enroll_enabled") || eff.Bool("mfa_web_authn_verify_enabled") {
		for _, n := range []string{"webauthn_rp_id", "webauthn_rp_display_name", "webauthn_rp_origins"} {
			if eff.Str(n) == "" {
				return invalid("%s is required when passkeys or WebAuthn MFA are enabled", n)
			}
		}
	}
	for _, h := range authHooks {
		if !eff.Bool("hook_" + h + "_enabled") {
			continue
		}
		uri := eff.Str("hook_" + h + "_uri")
		if uri == "" {
			return invalid("hook_%s_uri is required when the hook is enabled", h)
		}
		if strings.HasPrefix(strings.ToLower(uri), "http") && eff.Str("hook_"+h+"_secrets") == "" {
			return invalid("hook_%s_secrets is required for an HTTP hook", h)
		}
	}
	for _, p := range authProviders {
		if !eff.Bool("external_" + p.name + "_enabled") {
			continue
		}
		if eff.Str("external_"+p.name+"_client_id") == "" {
			return invalid("external_%s_client_id is required when the provider is enabled", p.name)
		}
		if eff.Str("external_"+p.name+"_secret") == "" && p.name != "apple" {
			return invalid("external_%s_secret is required when the provider is enabled", p.name)
		}
	}
	if autoconfirm(eff, set) && eff.Bool("mailer_allow_unverified_email_sign_ins") {
		return invalid("mailer_allow_unverified_email_sign_ins cannot be enabled while mailer_autoconfirm is on: turn auto-confirm off first")
	}
	if eff.Str("db_max_pool_size_unit") == "percent" {
		if n, _ := eff.Int("db_max_pool_size"); n > 100 {
			return invalid("db_max_pool_size is a percentage and must be at most 100")
		}
	}
	return nil
}

// autoconfirm is the mailer_autoconfirm GoTrue will run with: the saved value, else off
// once SMTP is configured (users can then confirm their address) and on until then
// (without SMTP nobody could).
func autoconfirm(eff, set Values) bool {
	if v, ok := set["mailer_autoconfirm"].(bool); ok {
		return v
	}
	return eff.Str("smtp_host") == ""
}

// AuthAutoconfirm is the mailer_autoconfirm of an auth State as GoTrue runs it.
func (st *State) AuthAutoconfirm() bool { return autoconfirm(st.Effective, st.Set) }
