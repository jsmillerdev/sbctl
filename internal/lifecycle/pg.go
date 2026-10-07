package lifecycle

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/config"
)

// Roles whose passwords sbctl sets after the cluster exists, keyed by the role name.
// They all exist after the artifact's init migrations.
const (
	RoleAdmin       = "supabase_admin"
	RolePostgres    = "postgres"
	RoleAuthn       = "authenticator"
	RoleAuthAdmin   = "supabase_auth_admin"
	RoleStorage     = "supabase_storage_admin"
	RoleReplication = "supabase_replication_admin"
)

// unixSocketMax is the longest unix socket path both Linux (107) and macOS (103) accept.
const unixSocketMax = 103

// pgPaths are a cluster's locations under its project directory.
type pgPaths struct {
	Dir      string // projects/<ref>/postgres
	Data     string // PGDATA
	Sock     string // unix_socket_directories
	HBA      string // pg_hba.conf, outside PGDATA so a restored data dir cannot weaken it
	RootKey  string // pgsodium/vault root key file
	Port     int
	SockFile string // the socket itself
}

// pathsFor returns the cluster layout of ref listening on port.
func pathsFor(cfg *config.Config, ref string, port int) pgPaths {
	dir := cfg.Paths().ProjectService(ref, config.SvcPostgres)
	p := pgPaths{
		Dir:     dir,
		Data:    dir + "/data",
		Sock:    dir + "/sock",
		HBA:     dir + "/pg_hba.conf",
		RootKey: dir + "/pgsodium_root.key",
		Port:    port,
	}
	p.SockFile = fmt.Sprintf("%s/.s.PGSQL.%d", p.Sock, port)
	return p
}

// kvQuote quotes a libpq key/value connection string value.
func kvQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// socketDSN is a libpq connection string to the cluster's unix socket as
// supabase_admin, which pg_hba trusts on the socket (the directory is private to the
// sbctl user). No password is needed, which is what lets sbctl read its own registry
// before it can decrypt any secret.
func socketDSN(p pgPaths, db string) string {
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable connect_timeout=5 application_name=sbctl",
		kvQuote(p.Sock), p.Port, RoleAdmin, db)
}

// RegistryDSN is the DSN of the "sbctl" registry database in the system cluster.
func RegistryDSN(cfg *config.Config) string {
	p := pathsFor(cfg, config.SystemRef, cfg.Ports.SystemPostgres)
	return socketDSN(p, "sbctl") + " pool_max_conns=6"
}

// SystemSocketDSN connects to database db of the system cluster as supabase_admin.
func SystemSocketDSN(cfg *config.Config, db string) string {
	return socketDSN(pathsFor(cfg, config.SystemRef, cfg.Ports.SystemPostgres), db)
}

func connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cc)
}

// ping runs "select 1" against the cluster.
func ping(ctx context.Context, p pgPaths) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := connect(ctx, socketDSN(p, "postgres"))
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var one int
	return c.QueryRow(ctx, "select 1").Scan(&one)
}

// scramIterations is Postgres' default scram_iterations.
const scramIterations = 4096

// ScramVerifier computes the SCRAM-SHA-256 verifier Postgres stores for password (RFC 5802,
// the format of pg_authid.rolpassword). Sending the verifier instead of the password means
// the plaintext never reaches the server, so statement logging cannot capture it. The
// password is used as-is (SASLprep is the identity for the ASCII passwords sbctl generates).
func ScramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return scramVerifierWithSalt(password, salt, scramIterations)
}

func scramVerifierWithSalt(password string, salt []byte, iter int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	server := mac(salted, "Server Key")
	b := base64.StdEncoding
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iter, b.EncodeToString(salt), b.EncodeToString(stored[:]), b.EncodeToString(server)), nil
}

// setPassword sets role's login password on c. It sends a SCRAM-SHA-256 verifier, never the
// plaintext, and also silences statement logging for the session as defense in depth: the
// artifact's postgresql.conf logs DDL (log_statement = 'ddl') to stderr, which ends in
// journald or the exec backend's log file. The statement text is built server side with
// format(%I, %L) so nothing is spliced into SQL here.
func setPassword(ctx context.Context, c *pgx.Conn, role, password string) error {
	if password == "" {
		return fmt.Errorf("lifecycle: empty password for role %s", role)
	}
	verifier, err := ScramVerifier(password)
	if err != nil {
		return err
	}
	var stmt string
	if err := c.QueryRow(ctx, `select format('alter role %I with password %L', $1::text, $2::text)`, role, verifier).Scan(&stmt); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("lifecycle: set password of %s: %w", role, err)
	}
	return nil
}

// quietSession turns off statement logging for the session of c (superuser only).
func quietSession(ctx context.Context, c *pgx.Conn) error {
	if _, err := c.Exec(ctx, `set log_statement = 'none'`); err != nil {
		return fmt.Errorf("lifecycle: silence statement logging: %w", err)
	}
	// pgaudit may not be installed; the setting is a placeholder GUC then, which is fine.
	if _, err := c.Exec(ctx, `set pgaudit.log = 'none'`); err != nil {
		return fmt.Errorf("lifecycle: silence pgaudit: %w", err)
	}
	return nil
}

// setRolePasswords sets the login passwords of the service roles.
func setRolePasswords(ctx context.Context, p pgPaths, passwords map[string]string) error {
	c, err := connect(ctx, socketDSN(p, "postgres"))
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if err := quietSession(ctx, c); err != nil {
		return err
	}
	for role, pw := range passwords {
		if err := setPassword(ctx, c, role, pw); err != nil {
			return err
		}
	}
	return nil
}

// dsnURL builds a postgres:// URL to the cluster on loopback TCP.
func dsnURL(user, password string, port int, db string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/" + db, RawQuery: "sslmode=disable"}
	return u.String()
}
