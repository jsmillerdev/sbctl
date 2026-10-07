package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

// scheme is the public URL scheme: https unless TLS is switched off (dev and tests).
func (pl *PostgresPlane) scheme() string {
	if pl.cfg.TLS.Mode == "off" {
		return "http"
	}
	return "https"
}

// authExternalURL is the URL GoTrue puts in email links and OAuth redirect URIs, the
// way hosted projects and the dockerless CLI do: the API host plus /auth/v1.
func (pl *PostgresPlane) authExternalURL(ref string) string {
	host := pl.cfg.ProjectHost(ref)
	if ref == config.SystemRef {
		host = pl.cfg.APIHost()
	}
	return pl.scheme() + "://" + host + "/auth/v1"
}

// siteURL is GoTrue's default redirect target. Studio is where the system project's
// users land; a project starts with hosted's default and the developer changes it.
func (pl *PostgresPlane) siteURL(ref string) string {
	if ref == config.SystemRef {
		return pl.scheme() + "://" + pl.cfg.StudioHost()
	}
	return "http://localhost:3000"
}

func (pl *PostgresPlane) archiveCommand(ref string) string {
	if pl.opts.ArchiveCommandFor != nil {
		if c := pl.opts.ArchiveCommandFor(ref); c != "" {
			return c
		}
	}
	if c := pl.opts.ArchiveCommand; c != "" {
		return c
	}
	// Fallback for callers that did not wire the backup package (lifecycle cannot import
	// it): the same forms and quoting rules as backup.ArchiveCommandFor, which the daemon and
	// the CLI install through PlaneOptions.ArchiveCommandFor.
	q := func(s string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "%", "%%"), "'", `'\''`) + "'"
	}
	if pl.cfg.WALRelayEnabled() {
		return fmt.Sprintf("%s wal push --ref %s --socket %s %%p", q(pl.cfg.BinPath), ref, q(pl.cfg.Paths().WALSocket(ref)))
	}
	return fmt.Sprintf("%s wal push --ref %s %%p", q(pl.cfg.BinPath), ref)
}

func (pl *PostgresPlane) archiveTimeout() int {
	if pl.opts.ArchiveTimeout > 0 {
		return pl.opts.ArchiveTimeout
	}
	return 900
}

// postgresSpec renders the cluster unit. The launcher (bin/supabase-postgres-start)
// initializes an empty PGDATA, runs the artifact's roles and migrations once, then execs
// postgres with the arguments below after its own pgsodium and vault getkey settings.
// Command-line settings beat the artifact's postgresql.conf and conf.d, so the sizing,
// listen address, authentication file and archiving here are what runs.
//
// wal_level stays "logical", the artifact's default and what Realtime's
// postgres_changes needs; it is a superset of "replica", so archiving and base backups
// work unchanged.
//
// pg_cron runs its jobs in background workers (see cronSettings), not over libpq.
func (pl *PostgresPlane) postgresSpec(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) (units.Spec, error) {
	art, err := pl.arts.Dir(config.SvcPostgres)
	if err != nil {
		return units.Spec{}, err
	}
	className := p.Class
	if p.Ref == config.SystemRef {
		className = ClassSystem
	}
	class, err := ClassFor(className)
	if err != nil {
		return units.Spec{}, err
	}
	pp := pl.paths(p)
	settings := append([]string{
		"listen_addresses=127.0.0.1",
		"unix_socket_directories=" + pp.Sock,
		"unix_socket_permissions=0700",
		"hba_file=" + pp.HBA,
		"wal_level=logical",
		"max_wal_senders=5",
		"max_replication_slots=5",
		"jit=off",
	}, cronSettings...)
	settings = append(settings, class.Settings()...)
	// The project's saved Postgres settings that a running cluster cannot take over from a
	// reload (they overlap the class's command-line settings) go last, so they win.
	if pl.opts.Settings != nil && p.Ref != config.SystemRef {
		saved, err := pl.opts.Settings.PostgresSettings(ctx, p.Ref)
		if err != nil {
			return units.Spec{}, fmt.Errorf("lifecycle: saved postgres settings of %s: %w", p.Ref, err)
		}
		cmdline, _ := SplitPostgresSettings(saved)
		settings = append(settings, cmdline...)
	}
	if pl.archiveCommand(p.Ref) == "off" {
		settings = append(settings, "archive_mode=off")
	} else {
		settings = append(settings, "archive_mode=on", "archive_timeout="+strconv.Itoa(pl.archiveTimeout()), "archive_command="+pl.archiveCommand(p.Ref))
	}
	args := []string{units.StandardExec(config.SvcPostgres)[0], "-p", strconv.Itoa(pp.Port)}
	for _, s := range settings {
		args = append(args, "-c", s)
	}
	env := map[string]string{
		"PGDATA":            pp.Data,
		"PGSODIUM_KEY_FILE": pp.RootKey,
		"POSTGRES_USER":     RoleAdmin,
		"POSTGRES_DB":       "postgres",
	}
	if bootstrapPending(pp.Data) {
		// Only read on the first boot, when the launcher creates the postgres and
		// supabase_admin roles; setRolePasswords replaces them with the stored ones.
		// Rendered only while the data directory is not initialized, so the superuser
		// password does not sit in the env file and in every postmaster's environment
		// for the life of the cluster.
		env["POSTGRES_PASSWORD"] = keys.AdminPassword
	}
	if pl.opts.ConfigPath != "" && !pl.cfg.WALRelayEnabled() {
		env["SBCTL_CONFIG"] = pl.opts.ConfigPath
	}
	return units.Spec{
		Service:     config.SvcPostgres,
		Ref:         p.Ref,
		ArtifactDir: art,
		WorkDir:     pp.Dir,
		Env:         env,
		Limits:      p.Limits,
		Exec:        args,
		// A branch cloned from its parent's data carries the parent's outbound integrations:
		// once its isolation is done (EgressDenied) its cluster reaches loopback only.
		DenyEgress: p.Branch != nil && p.Branch.Egress == registry.EgressDenied,
	}, nil
}

// apiSpecs returns the GoTrue spec and, for user projects, the PostgREST spec.
func (pl *PostgresPlane) apiSpecs(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) ([]units.Spec, error) {
	ports := pl.cfg.PortsFor(p.Ref, p.Seq)
	pgPort := ports.Postgres

	authArt, err := pl.arts.Dir(config.SvcGoTrue)
	if err != nil {
		return nil, err
	}
	system := p.Ref == config.SystemRef
	auth := units.Spec{
		Service:     config.SvcGoTrue,
		Ref:         p.Ref,
		ArtifactDir: authArt,
		WorkDir:     pl.cfg.Paths().ProjectService(p.Ref, config.SvcGoTrue),
		Limits:      p.Limits,
		// One-shot schema migration before every start; it is a no-op once applied.
		PreStart: [][]string{{"bin/auth", "migrate"}},
		Env: map[string]string{
			"GOTRUE_API_HOST":        "127.0.0.1",
			"GOTRUE_API_PORT":        strconv.Itoa(ports.GoTrue),
			"GOTRUE_DB_DRIVER":       "postgres",
			"GOTRUE_DB_DATABASE_URL": dsnURL(RoleAuthAdmin, keys.AuthAdminPassword, pgPort, "postgres"),
			"DATABASE_URL":           dsnURL(RoleAuthAdmin, keys.AuthAdminPassword, pgPort, "postgres"),
			// Two connections is the artifact's default; five absorbs bursts of sign-ins.
			"GOTRUE_DB_MAX_POOL_SIZE": "5",
			"API_EXTERNAL_URL":        pl.authExternalURL(p.Ref),
			"GOTRUE_SITE_URL":         pl.siteURL(p.Ref),
			// GoTrue resolves these paths against API_EXTERNAL_URL with
			// url.ResolveReference, so the default "/verify" would drop the /auth/v1
			// prefix. Absolute URLs keep every email link routable.
			"GOTRUE_MAILER_URLPATHS_INVITE":       pl.authExternalURL(p.Ref) + "/verify",
			"GOTRUE_MAILER_URLPATHS_CONFIRMATION": pl.authExternalURL(p.Ref) + "/verify",
			"GOTRUE_MAILER_URLPATHS_RECOVERY":     pl.authExternalURL(p.Ref) + "/verify",
			"GOTRUE_MAILER_URLPATHS_EMAIL_CHANGE": pl.authExternalURL(p.Ref) + "/verify",
			"GOTRUE_JWT_SECRET":                   keys.JWTSecret,
			"GOTRUE_JWT_ISSUER":                   pl.authExternalURL(p.Ref),
			"GOTRUE_JWT_AUD":                      "authenticated",
			"GOTRUE_JWT_DEFAULT_GROUP_NAME":       "authenticated",
			"GOTRUE_JWT_ADMIN_ROLES":              "service_role",
			"GOTRUE_JWT_EXP":                      "3600",
			"GOTRUE_EXTERNAL_EMAIL_ENABLED":       "true",
			// Without an SMTP server nobody can confirm an address, so projects start with
			// auto-confirm on; the Management API turns it off when SMTP is configured.
			"GOTRUE_MAILER_AUTOCONFIRM": "true",
			"GOTRUE_DISABLE_SIGNUP":     strconv.FormatBool(system),
			"GOTRUE_LOG_LEVEL":          "warn",
		},
	}
	if system {
		// Outgoing mail of the dashboard (organization invitations): [mail] in config.toml.
		mergeEnv(auth.Env, pl.cfg.Mail.GoTrueEnv())
		if pl.opts.SystemAuth != nil {
			sa, err := pl.opts.SystemAuth(ctx)
			if err != nil {
				return nil, fmt.Errorf("lifecycle: dashboard SSO configuration: %w", err)
			}
			mergeEnv(auth.Env, systemSSOEnv(sa))
		}
	}
	if pl.opts.Settings != nil && !system {
		over, err := pl.opts.Settings.AuthEnv(ctx, p.Ref, pl.authExternalURL(p.Ref))
		if err != nil {
			return nil, fmt.Errorf("lifecycle: saved auth settings of %s: %w", p.Ref, err)
		}
		mergeEnv(auth.Env, over)
	}
	specs := []units.Spec{auth}
	if !hasPostgREST(p.Ref) {
		return specs, nil
	}
	restArt, err := pl.arts.Dir(config.SvcPostgREST)
	if err != nil {
		return nil, err
	}
	rest := units.Spec{
		Service:     config.SvcPostgREST,
		Ref:         p.Ref,
		ArtifactDir: restArt,
		WorkDir:     pl.cfg.Paths().ProjectService(p.Ref, config.SvcPostgREST),
		Limits:      p.Limits,
		Env: map[string]string{
			"PGRST_SERVER_HOST":          "127.0.0.1",
			"PGRST_SERVER_PORT":          strconv.Itoa(ports.PostgREST),
			"PGRST_DB_URI":               dsnURL(RoleAuthn, keys.AuthenticatorPassword, pgPort, "postgres"),
			"PGRST_DB_SCHEMAS":           "public,graphql_public",
			"PGRST_DB_EXTRA_SEARCH_PATH": "public,extensions",
			"PGRST_DB_ANON_ROLE":         "anon",
			"PGRST_DB_USE_LEGACY_GUCS":   "false",
			"PGRST_DB_MAX_ROWS":          "1000",
			"PGRST_DB_POOL":              "5",
			"PGRST_JWT_SECRET":           keys.JWTSecret,
			// Read by SQL that calls current_setting('app.settings.jwt_secret'), as in
			// upstream's compose file.
			"PGRST_APP_SETTINGS_JWT_SECRET":  keys.JWTSecret,
			"PGRST_APP_SETTINGS_JWT_EXP":     "3600",
			"PGRST_LOG_LEVEL":                "warn",
			"PGRST_OPENAPI_SERVER_PROXY_URI": pl.scheme() + "://" + pl.cfg.ProjectHost(p.Ref) + "/rest/v1",
		},
	}
	if pl.opts.Settings != nil {
		over, err := pl.opts.Settings.PostgRESTEnv(ctx, p.Ref)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: saved postgrest settings of %s: %w", p.Ref, err)
		}
		mergeEnv(rest.Env, over)
	}
	return append(specs, rest), nil
}

// systemSSOEnv is what turns on single sign-on in sb-gotrue@system: SAML with the node's own
// signing key, and a sign-up that is open to GoTrue but closed to everyone the hook does not
// vouch for (see SystemAuth). GOTRUE_DISABLE_SIGNUP cannot stay on: it also stops the first
// sign-in of an SSO user, whose account is created by that sign-in.
func systemSSOEnv(sa *SystemAuth) map[string]string {
	return map[string]string{
		"GOTRUE_DISABLE_SIGNUP":                   "false",
		"GOTRUE_SAML_ENABLED":                     "true",
		"GOTRUE_SAML_PRIVATE_KEY":                 sa.SigningKey,
		"GOTRUE_HOOK_BEFORE_USER_CREATED_ENABLED": "true",
		"GOTRUE_HOOK_BEFORE_USER_CREATED_URI":     sa.HookURL,
		"GOTRUE_HOOK_BEFORE_USER_CREATED_SECRETS": sa.HookSecret,
	}
}

// mergeEnv lays the saved settings over a unit's base environment; an empty value removes
// the variable.
func mergeEnv(base, over map[string]string) {
	for k, v := range over {
		if v == "" {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
}

// bootstrapPending reports whether the launcher still has first-boot work to do in
// dataDir: the cluster does not exist yet, or an earlier initialization was cut short.
func bootstrapPending(dataDir string) bool {
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err != nil {
		return true
	}
	_, err := os.Stat(filepath.Join(dataDir, initPendingWitness))
	return err == nil
}
