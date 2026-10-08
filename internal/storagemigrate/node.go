package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// NodeTenants reads the projects and their databases through an open node.
func NodeTenants(n *lifecycle.Node) Tenants { return nodeTenants{n} }

type nodeTenants struct{ n *lifecycle.Node }

func (t nodeTenants) Projects(ctx context.Context) ([]Project, error) {
	ps, err := t.n.Registry.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []Project
	for _, p := range ps {
		if p.Ref == config.SystemRef {
			continue
		}
		out = append(out, Project{Ref: p.Ref, Online: p.Status == registry.StatusActiveHealthy || p.Status == registry.StatusActiveUnhealthy})
	}
	return out, nil
}

// rowsQuery lists the objects a project's Storage knows. A row without a version (the column is
// nullable, and an object that predates versions has none) comes back with an empty Version: the
// migration does not know under which key Storage keeps its file or its S3 object, so it does not
// verify that row, and counts it (Tenant.Unversioned). The copy goes by file path, so the file is
// sent all the same.
const rowsQuery = `select bucket_id, name, coalesce(version::text, ''), metadata->>'size' from storage.objects`

func (t nodeTenants) Rows(ctx context.Context, ref string, fn func(Row) error) error {
	dsn, err := t.dsn(ctx, ref)
	if err != nil {
		return err
	}
	return rowsAt(ctx, dsn, fn)
}

// rowsAt reads storage.objects of the database at dsn.
func rowsAt(ctx context.Context, dsn string, fn func(Row) error) error {
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		if databaseDown(err) {
			return ErrOffline
		}
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	rows, err := conn.Query(ctx, rowsQuery)
	if err != nil {
		return noStorageSchema(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Row
		var size *string
		if err := rows.Scan(&r.Bucket, &r.Name, &r.Version, &size); err != nil {
			return err
		}
		if size != nil {
			if n, err := strconv.ParseInt(*size, 10, 64); err == nil {
				r.Size, r.HasSize = n, true
			}
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return noStorageSchema(rows.Err())
}

// noStorageSchema turns "there is no storage.objects" into no rows: Storage never ran for the project.
func noStorageSchema(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "42P01" || pe.Code == "3F000") {
		return nil
	}
	return err
}

// databaseDown says whether err means that nothing listens: a project that is not running.
func databaseDown(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "57P03" { // starting up or shutting down
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// dsn connects as supabase_admin over the project's private unix socket, which needs no password.
// A project whose database runs on another node is reached through the forwarder on its canonical
// port, with the sealed password.
func (t nodeTenants) dsn(ctx context.Context, ref string) (string, error) {
	p, err := t.n.Registry.GetProject(ctx, ref)
	if err != nil {
		return "", err
	}
	cfg := t.n.Cfg
	port := cfg.PortsFor(ref, p.Seq).Postgres
	sock := filepath.Join(cfg.Paths().ProjectService(ref, config.SvcPostgres), "sock")
	if _, err := lstat(filepath.Join(sock, fmt.Sprintf(".s.PGSQL.%d", port))); err == nil {
		return fmt.Sprintf("host=%s port=%d user=%s dbname=postgres sslmode=disable connect_timeout=5 application_name=supavise-storage-migrate",
			kvQuote(sock), port, lifecycle.RoleAdmin), nil
	}
	sealed, err := t.n.Registry.GetSecrets(ctx, ref)
	if err != nil {
		return "", err
	}
	blob, ok := sealed[secrets.NameAdminPassword]
	if !ok {
		return "", ErrOffline
	}
	pw, err := t.n.Secrets.Open(blob)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("host=127.0.0.1 port=%d user=%s password=%s dbname=postgres sslmode=disable connect_timeout=5 application_name=supavise-storage-migrate",
		port, lifecycle.RoleAdmin, kvQuote(string(pw))), nil
}

func kvQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// NodeService drives supavise-storage through a fleet manager that touches no other shared
// service. configPath is the file Start reads again, so that it renders what the switch wrote.
func NodeService(n *lifecycle.Node, configPath string, log *slog.Logger) Service {
	return nodeService{n: n, path: configPath, log: log}
}

type nodeService struct {
	n    *lifecycle.Node
	path string
	log  *slog.Logger
}

func (s nodeService) manager() (*fleet.Manager, *config.Config, error) {
	cfg, err := config.Load(s.path)
	if err != nil {
		return nil, nil, err
	}
	m, err := s.managerFor(cfg)
	return m, cfg, err
}

// managerFor is a fleet manager over cfg that touches no service but supavise-storage.
func (s nodeService) managerFor(cfg *config.Config) (*fleet.Manager, error) {
	skip := []string{config.SvcEdgeRuntime}
	for _, svc := range fleet.Services {
		if svc != config.SvcStorage {
			skip = append(skip, svc)
		}
	}
	return fleet.NewManager(fleet.Deps{Cfg: cfg, Log: s.log, Registry: s.n.Registry, Secrets: s.n.Secrets,
		Supervisor: s.n.Supervisor, Artifacts: s.n.Artifacts, Skip: skip})
}

// Render implements Service: the fleet builds the unit specs without starting a unit, and fails
// for what Start would refuse (no bucket, no key under systemd).
func (s nodeService) Render(ctx context.Context, cfg *config.Config) error {
	m, err := s.managerFor(cfg)
	if err != nil {
		return err
	}
	_, err = m.Specs(ctx)
	return err
}

func (s nodeService) Healthy(ctx context.Context) error {
	m, _, err := s.manager()
	if err != nil {
		return err
	}
	for _, h := range m.Status(ctx) {
		if h.Service == config.SvcStorage {
			if h.Healthy {
				return nil
			}
			return fmt.Errorf("%s is %s: %s", h.Unit, h.Status, h.Error)
		}
	}
	return errors.New("supavise-storage is not one of the shared services of this node")
}

func (s nodeService) Stop(ctx context.Context) error {
	m, _, err := s.manager()
	if err != nil {
		return err
	}
	return m.Stop(ctx)
}

func (s nodeService) Start(ctx context.Context) error {
	m, _, err := s.manager()
	if err != nil {
		return err
	}
	return m.Start(ctx)
}

// NodeReader reads objects through the Storage service on its loopback port with the project's
// service_role key, as the edge proxy would pass a client's request.
func NodeReader(n *lifecycle.Node, configPath string) Reader {
	return nodeReader{n: n, path: configPath}
}

type nodeReader struct {
	n    *lifecycle.Node
	path string
}

func (r nodeReader) Read(ctx context.Context, ref string, row Row) (int64, error) {
	cfg, err := config.Load(r.path)
	if err != nil {
		return 0, err
	}
	sealed, err := r.n.Registry.GetSecrets(ctx, ref)
	if err != nil {
		return 0, err
	}
	m := map[string]string{}
	for name, blob := range sealed {
		plain, err := r.n.Secrets.Open(blob)
		if err != nil {
			return 0, err
		}
		m[name] = string(plain)
	}
	key := secrets.KeysFromMap(m).ServiceRoleKey
	if key == "" {
		return 0, errors.New("the project has no service_role key to read with")
	}
	segs := strings.Split(row.Name, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/object/authenticated/%s/%s", cfg.Ports.Storage, url.PathEscape(row.Bucket), strings.Join(segs, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Forwarded-Host", cfg.ProjectHost(ref))
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return io.Copy(io.Discard, resp.Body)
}
