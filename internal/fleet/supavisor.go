package fleet

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// Supavisor pools every project's Postgres behind one pair of ports and routes on the
// user name suffix: postgres.<ref> is tenant <ref>. A tenant record says where the
// database is and how to authenticate clients. We use upstream's own scheme for that,
// the one hosted Supabase uses and the dockerless CLI provisions: no per-user rows
// (require_user false), and one manager user that runs the auth query
//
//	SELECT * FROM pgbouncer.get_auth($1)
//
// to fetch the SCRAM verifier of whoever logs in. The query function and the "pgbouncer"
// role ship in the supabase/postgres init scripts. The pgbouncer role has no password
// there, so EnsureTenant gives it one derived from the project's supabase_admin password;
// the pooler never holds a superuser credential, only the right to call get_auth.
const (
	supavisorManagerRole = "pgbouncer"
	supavisorAuthQuery   = "SELECT * FROM pgbouncer.get_auth($1)"
	// supavisorDefaultPool is Supavisor's own default pool size, sent explicitly because a
	// user row must carry one.
	supavisorDefaultPool = 15
	// supavisorDefaultMaxClients is Supavisor's default_max_clients default. It is sent
	// explicitly too: a tenant update that omits the field keeps the old value, so a limit that
	// was saved and then reset would otherwise stay in force.
	supavisorDefaultMaxClients = 1000
)

// managerPassword derives the pgbouncer role's password from the project's
// supabase_admin password, so it needs no storage of its own and stays the same across
// EnsureTenant calls and key rotations (which never change database passwords).
func managerPassword(adminPassword string) string {
	m := hmac.New(sha256.New, []byte(adminPassword))
	m.Write([]byte("supavise supavisor manager"))
	return hex.EncodeToString(m.Sum(nil))
}

// SupavisorTenant registers projects with Supavisor's HTTP API.
type supavisorTenant struct {
	followerGate
	cl     *apiClient
	store  tenantStore
	base   string // http://127.0.0.1:<port>
	secret string // API_JWT_SECRET
	now    func() time.Time
	// setManager gives the pgbouncer role of the project's database its password. Tests
	// replace it; the default talks to the project's Postgres over loopback TCP.
	setManager func(ctx context.Context, spec TenantSpec, password string) error
}

func (t *supavisorTenant) Service() string { return config.SvcSupavisor }

func (t *supavisorTenant) headers() (map[string]string, error) {
	tok, err := signToken(t.secret, t.now())
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + tok}, nil
}

func (t *supavisorTenant) tenantURL(ref string) string {
	return t.base + "/api/tenants/" + url.PathEscape(ref)
}

// body is PUT /api/tenants/<ref>: {"tenant": {...}} as TenantCreate in
// lib/supavisor_web/open_api_schemas.ex describes it. The external id comes from the path.
func supavisorBody(spec TenantSpec, managerPW string) map[string]any {
	pool := spec.PoolSize
	if pool <= 0 {
		pool = supavisorDefaultPool
	}
	maxClients := spec.MaxClients
	if maxClients <= 0 {
		maxClients = supavisorDefaultMaxClients
	}
	host := spec.DBHost
	if host == "" {
		host = "127.0.0.1"
	}
	db := spec.DBName
	if db == "" {
		db = "postgres"
	}
	tenant := map[string]any{
		"db_host":             host,
		"db_port":             spec.DBPort,
		"db_database":         db,
		"upstream_ssl":        false,
		"enforce_ssl":         false,
		"require_user":        false,
		"auth_query":          supavisorAuthQuery,
		"default_pool_size":   pool,
		"default_max_clients": maxClients,
		"users": []map[string]any{{
			"db_user":     supavisorManagerRole,
			"db_password": managerPW,
			"mode_type":   "transaction", // informational: the listening port picks the mode
			"pool_size":   pool,
			"is_manager":  true,
		}},
	}
	return map[string]any{"tenant": tenant}
}

// EnsureTenant implements Tenant. A follower leaves it to the leader.
func (t *supavisorTenant) EnsureTenant(ctx context.Context, spec TenantSpec) error {
	if skip, err := t.skip(ctx); skip || err != nil {
		return err
	}
	if err := validTenantRef(spec.Ref); err != nil {
		return err
	}
	if spec.DBPort <= 0 || spec.DBPassword == "" {
		return fmt.Errorf("fleet: supavisor tenant %s needs the database port and the supabase_admin password", spec.Ref)
	}
	pw := managerPassword(spec.DBPassword)
	body := supavisorBody(spec, pw)
	fp, err := fingerprintOf(body)
	if err != nil {
		return err
	}
	h, err := t.headers()
	if err != nil {
		return err
	}
	got, err := t.cl.do(ctx, "GET", t.tenantURL(spec.Ref), h, nil)
	if err != nil {
		return err
	}
	switch {
	case got.ok():
		if t.store.get(ctx, spec.Ref, config.SvcSupavisor) == fp {
			return nil // already registered exactly like this; an update would drop its pools
		}
	case got.Status == 404:
	default:
		return t.cl.apiError("get tenant "+spec.Ref, got)
	}
	if err := t.setManager(ctx, spec, pw); err != nil {
		return err
	}
	if h, err = t.headers(); err != nil {
		return err
	}
	put, err := t.cl.do(ctx, "PUT", t.tenantURL(spec.Ref), h, body)
	if err != nil {
		return err
	}
	if !put.ok() {
		return t.cl.apiError("put tenant "+spec.Ref, put)
	}
	if err := t.store.put(ctx, spec.Ref, config.SvcSupavisor, fp); err != nil {
		t.cl.log.Warn("fleet: could not record the supavisor tenant fingerprint", "ref", spec.Ref, "error", err)
	}
	return nil
}

// RemoveTenant implements Tenant: stop the tenant's pools (best effort, once), then delete
// the record. A tenant that is not there counts as removed.
func (t *supavisorTenant) RemoveTenant(ctx context.Context, ref string) error {
	if skip, err := t.skip(ctx); skip || err != nil {
		return err
	}
	if err := validTenantRef(ref); err != nil {
		return err
	}
	return t.remove(ctx, ref, config.SvcSupavisor, ref)
}

// remove stops the pools of the tenant with external id id and deletes it; the fingerprint it
// kept under project ref and name is cleared.
func (t *supavisorTenant) remove(ctx context.Context, ref, name, id string) error {
	h, err := t.headers()
	if err != nil {
		return err
	}
	once := *t.cl
	once.retry = Retry{Attempts: 1}
	_, _ = once.do(ctx, "GET", t.tenantURL(id)+"/terminate", h, nil)
	del, err := t.cl.do(ctx, "DELETE", t.tenantURL(id), h, nil)
	if err != nil {
		return err
	}
	if !del.ok() && del.Status != 404 {
		return t.cl.apiError("delete tenant "+id, del)
	}
	t.store.forget(ctx, ref, name)
	return nil
}

// supavisorReplicaService is the name a replica's tenant fingerprint is kept under, next to the
// project's own (config.SvcSupavisor).
func supavisorReplicaService(identifier string) string { return config.SvcSupavisor + "-" + identifier }

// EnsureReplicaTenant implements ReplicaTenanter: a tenant with the replica's identifier as its
// external id, pointing at the replica's port, so that postgres.<identifier> logs in to the
// standby (design 2.7.6). Supavisor keeps the row in _supavisor, which replicates to every
// follower's Supavisor. Everything else is the project's own tenant: the manager login is the
// project's pgbouncer role, whose password the project's EnsureTenant set and the standby
// replicated (a standby cannot be written, so this call sets nothing), so that tenant is
// ensured first. A follower leaves the call to the leader.
func (t *supavisorTenant) EnsureReplicaTenant(ctx context.Context, spec TenantSpec) error {
	if skip, err := t.skip(ctx); skip || err != nil {
		return err
	}
	if err := validTenantRef(spec.Ref); err != nil {
		return err
	}
	if ref, _, _, ok := registry.ParseReplicaIdentifier(spec.ReplicaID); !ok || ref != spec.Ref {
		return fmt.Errorf("fleet: %q is not a replica identifier of project %s", spec.ReplicaID, spec.Ref)
	}
	if spec.DBPort <= 0 || spec.DBPassword == "" {
		return fmt.Errorf("fleet: supavisor tenant %s needs the replica's port and the supabase_admin password", spec.ReplicaID)
	}
	body := supavisorBody(spec, managerPassword(spec.DBPassword))
	fp, err := fingerprintOf(body)
	if err != nil {
		return err
	}
	h, err := t.headers()
	if err != nil {
		return err
	}
	name := supavisorReplicaService(spec.ReplicaID)
	got, err := t.cl.do(ctx, "GET", t.tenantURL(spec.ReplicaID), h, nil)
	if err != nil {
		return err
	}
	switch {
	case got.ok():
		if t.store.get(ctx, spec.Ref, name) == fp {
			return nil
		}
	case got.Status == 404:
	default:
		return t.cl.apiError("get tenant "+spec.ReplicaID, got)
	}
	if h, err = t.headers(); err != nil {
		return err
	}
	put, err := t.cl.do(ctx, "PUT", t.tenantURL(spec.ReplicaID), h, body)
	if err != nil {
		return err
	}
	if !put.ok() {
		return t.cl.apiError("put tenant "+spec.ReplicaID, put)
	}
	if err := t.store.put(ctx, spec.Ref, name, fp); err != nil {
		t.cl.log.Warn("fleet: could not record the supavisor replica tenant fingerprint", "replica", spec.ReplicaID, "error", err)
	}
	return nil
}

// RemoveReplicaTenant implements ReplicaTenanter: stop the replica tenant's pools and delete it.
// One that is not there counts as removed.
func (t *supavisorTenant) RemoveReplicaTenant(ctx context.Context, identifier string) error {
	if skip, err := t.skip(ctx); skip || err != nil {
		return err
	}
	ref, _, _, ok := registry.ParseReplicaIdentifier(identifier)
	if !ok {
		return fmt.Errorf("fleet: %q is not a replica identifier", identifier)
	}
	return t.remove(ctx, ref, supavisorReplicaService(identifier), identifier)
}

// RefreshTenant implements Refresher: GET /api/tenants/<ref>/terminate stops the tenant's
// pools and drops its cached credentials, so the next login asks the project's database
// again (auth_query) and a changed role password takes effect at once. The tenant record
// stays. Pooled client sessions end; clients reconnect. ref may be a replica identifier. A
// follower's Supavisor keeps the target of a tenant row until this call, so the leader asks for
// it on every node that runs Supavisor once the node's standby has replayed the change
// (PeerRefresher); it is the one tenant call a follower makes.
func (t *supavisorTenant) RefreshTenant(ctx context.Context, ref string) error {
	if err := validTenantID(ref); err != nil {
		return err
	}
	h, err := t.headers()
	if err != nil {
		return err
	}
	res, err := t.cl.do(ctx, "GET", t.tenantURL(ref)+"/terminate", h, nil)
	if err != nil {
		return err
	}
	if !res.ok() && res.Status != 404 {
		return t.cl.apiError("terminate tenant "+ref, res)
	}
	return nil
}

// setManagerPassword is the default setManager: connect to the project's database as
// supabase_admin and give pgbouncer its password as a SCRAM verifier (never plaintext, so
// statement logging cannot capture it).
func setManagerPassword(ctx context.Context, spec TenantSpec, password string) error {
	host := spec.DBHost
	if host == "" {
		host = "127.0.0.1"
	}
	db := spec.DBName
	if db == "" {
		db = "postgres"
	}
	user := spec.DBUser
	if user == "" {
		user = "supabase_admin"
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, spec.DBPassword), Host: host + ":" + strconv.Itoa(spec.DBPort), Path: "/" + db, RawQuery: "sslmode=disable&connect_timeout=10"}
	cc, err := pgx.ParseConfig(u.String())
	if err != nil {
		return err
	}
	c, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return fmt.Errorf("fleet: connect to project %s to set the %s role: %w", spec.Ref, supavisorManagerRole, err)
	}
	defer c.Close(context.WithoutCancel(ctx))
	if _, err := c.Exec(ctx, `set log_statement = 'none'`); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, `set pgaudit.log = 'none'`); err != nil { // a placeholder GUC when pgaudit is absent
		return err
	}
	verifier, err := scramVerifier(password)
	if err != nil {
		return err
	}
	var stmt string
	if err := c.QueryRow(ctx, `select format('alter role %I with login password %L', $1::text, $2::text)`, supavisorManagerRole, verifier).Scan(&stmt); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("fleet: set the %s role's password in project %s: %w", supavisorManagerRole, spec.Ref, err)
	}
	return nil
}

// scramVerifier computes the SCRAM-SHA-256 verifier Postgres stores for password (RFC 5802,
// the format of pg_authid.rolpassword), 4096 iterations like Postgres' default. The same
// computation as lifecycle's ScramVerifier, which this package cannot import. The password
// is used as-is: SASLprep is the identity for the hex passwords generated here.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return scramVerifierWithSalt(password, salt, 4096)
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
