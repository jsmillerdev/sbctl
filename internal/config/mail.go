package config

import (
	"strconv"
	"strings"
)

// Mail is the [mail] config section: the SMTP server supavise-gotrue@system sends the
// dashboard's mail through (organization invitations). Without a host no mail leaves the
// node: an invitation then yields a link for the administrator to pass on.
type Mail struct {
	// SMTPHost and SMTPPort are the relay (for example "smtp.example.com" and 587).
	SMTPHost string `toml:"smtp_host"`
	SMTPPort int    `toml:"smtp_port"`
	// SMTPUser and SMTPPass authenticate to the relay; empty for an open relay. The password
	// sits in config.toml in plain text, like the backup keys: keep the file mode 0600.
	SMTPUser string `toml:"smtp_user"`
	SMTPPass string `toml:"smtp_pass"`
	// SMTPFrom is the sender address and SMTPName the sender name.
	SMTPFrom string `toml:"smtp_from"`
	SMTPName string `toml:"smtp_name"`
}

// Enabled reports whether outgoing mail is configured.
func (m Mail) Enabled() bool {
	return strings.TrimSpace(m.SMTPHost) != "" && strings.TrimSpace(m.SMTPFrom) != ""
}

// GoTrueEnv returns the GoTrue variables that make supavise-gotrue@system send mail through the
// relay; empty when mail is not configured.
func (m Mail) GoTrueEnv() map[string]string {
	if !m.Enabled() {
		return nil
	}
	port := m.SMTPPort
	if port == 0 {
		port = 587
	}
	name := m.SMTPName
	if name == "" {
		name = "supavise"
	}
	env := map[string]string{
		"GOTRUE_SMTP_HOST":        strings.TrimSpace(m.SMTPHost),
		"GOTRUE_SMTP_PORT":        strconv.Itoa(port),
		"GOTRUE_SMTP_ADMIN_EMAIL": strings.TrimSpace(m.SMTPFrom),
		"GOTRUE_SMTP_SENDER_NAME": name,
		// The invitation of a new user and the sign-in link of an existing one.
		"GOTRUE_MAILER_SUBJECTS_INVITE":     "You have been invited to the " + name + " dashboard",
		"GOTRUE_MAILER_SUBJECTS_MAGIC_LINK": "Your " + name + " sign-in link",
	}
	if m.SMTPUser != "" {
		env["GOTRUE_SMTP_USER"] = m.SMTPUser
		env["GOTRUE_SMTP_PASS"] = m.SMTPPass
	}
	return env
}
