package lifecycle

import (
	"context"
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

// setRolePasswords sets the login passwords of the service roles. Statements are built
// server side with format(%I, %L) so no password is ever spliced into SQL here.
func setRolePasswords(ctx context.Context, p pgPaths, passwords map[string]string) error {
	c, err := connect(ctx, socketDSN(p, "postgres"))
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	for role, pw := range passwords {
		if pw == "" {
			return fmt.Errorf("lifecycle: empty password for role %s", role)
		}
		var stmt string
		if err := c.QueryRow(ctx, `select format('alter role %I with password %L', $1::text, $2::text)`, role, pw).Scan(&stmt); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("lifecycle: set password of %s: %w", role, err)
		}
	}
	return nil
}

// dsnURL builds a postgres:// URL to the cluster on loopback TCP.
func dsnURL(user, password string, port int, db string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/" + db, RawQuery: "sslmode=disable"}
	return u.String()
}
