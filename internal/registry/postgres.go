package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the production Registry.
type Postgres struct{ pool *pgxpool.Pool }

var (
	_ Registry = (*Postgres)(nil)
	_ Registry = (*Memory)(nil)
)

// Open connects to dsn (the "sbctl" database of the system cluster) and applies migrations.
func Open(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

// NewPostgres wraps an existing, already migrated pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Pool exposes the pool for packages that add their own tables (see migration ranges).
func (r *Postgres) Pool() *pgxpool.Pool { return r.pool }

func (r *Postgres) Close() { r.pool.Close() }

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "23505" || pe.Code == "23503") {
		// unique_violation, or foreign_key_violation (a project that still has branches)
		return fmt.Errorf("%w: %s", ErrConflict, pe.ConstraintName)
	}
	return err
}

func affected(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Organizations

const orgCols = `id, slug, name, created_at`

func scanOrg(row pgx.Row) (*Organization, error) {
	var o Organization
	if err := row.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

func (r *Postgres) CreateOrganization(ctx context.Context, slug, name string) (*Organization, error) {
	return scanOrg(r.pool.QueryRow(ctx,
		`insert into sbctl.organizations (slug, name) values ($1, $2) returning `+orgCols, slug, name))
}

func (r *Postgres) GetOrganization(ctx context.Context, slug string) (*Organization, error) {
	return scanOrg(r.pool.QueryRow(ctx, `select `+orgCols+` from sbctl.organizations where slug = $1`, slug))
}

func (r *Postgres) GetOrganizationByID(ctx context.Context, id int64) (*Organization, error) {
	return scanOrg(r.pool.QueryRow(ctx, `select `+orgCols+` from sbctl.organizations where id = $1`, id))
}

func (r *Postgres) ListOrganizations(ctx context.Context) ([]Organization, error) {
	rows, err := r.pool.Query(ctx, `select `+orgCols+` from sbctl.organizations order by id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Organization, error) {
		o, err := scanOrg(row)
		if err != nil {
			return Organization{}, err
		}
		return *o, nil
	})
}

func (r *Postgres) UpdateOrganization(ctx context.Context, o *Organization) error {
	return affected(r.pool.Exec(ctx, `update sbctl.organizations set slug = $2, name = $3 where id = $1`, o.ID, o.Slug, o.Name))
}

// Projects

const projectCols = `ref, coalesce(org_id, 0), seq, name, region, engine, class, status, versions, limits, created_at, updated_at,
	branch_id::text, parent_ref, branch_name, git_branch, persistent, with_data, expires_at, deletion_scheduled_at,
	notify_url, branch_state, branch_detail, clone_method, review_requested_at, branch_egress`

func scanProject(row pgx.Row) (*Project, error) {
	var p Project
	var versions, limits []byte
	var (
		bID, bParent, bName, bGit, bNotify, bState, bDetail, bMethod, bEgress *string
		bPersistent, bData                                                    bool
		bExpires, bDeletion, bReview                                          *time.Time
	)
	if err := row.Scan(&p.Ref, &p.OrgID, &p.Seq, &p.Name, &p.Region, &p.Engine, &p.Class, &p.Status,
		&versions, &limits, &p.CreatedAt, &p.UpdatedAt,
		&bID, &bParent, &bName, &bGit, &bPersistent, &bData, &bExpires, &bDeletion,
		&bNotify, &bState, &bDetail, &bMethod, &bReview, &bEgress); err != nil {
		return nil, mapErr(err)
	}
	if bParent != nil {
		str := func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		}
		p.Branch = &BranchInfo{
			ID: str(bID), ParentRef: *bParent, Name: str(bName), GitBranch: str(bGit), Persistent: bPersistent, WithData: bData,
			ExpiresAt: bExpires, DeletionScheduledAt: bDeletion, NotifyURL: str(bNotify), State: BranchState(str(bState)),
			Detail: str(bDetail), CloneMethod: str(bMethod), ReviewRequestedAt: bReview, Egress: str(bEgress),
		}
	}
	if err := json.Unmarshal(versions, &p.Versions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(limits, &p.Limits); err != nil {
		return nil, err
	}
	return &p, nil
}

func nullOrg(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (r *Postgres) CreateProject(ctx context.Context, p *Project) error {
	if p.Engine == "" {
		p.Engine = EnginePostgres
	}
	if p.Region == "" {
		p.Region = "local"
	}
	if p.Class == "" {
		p.Class = "default"
	}
	if p.Status == "" {
		p.Status = StatusComingUp
	}
	if p.Versions == nil {
		p.Versions = map[string]string{}
	}
	versions, _ := json.Marshal(p.Versions)
	limits, _ := json.Marshal(p.Limits)
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Serialize seq allocation.
		if _, err := tx.Exec(ctx, `lock table sbctl.projects in share row exclusive mode`); err != nil {
			return err
		}
		seq := p.Seq
		if seq == 0 && p.Ref != "system" {
			if err := tx.QueryRow(ctx, `
				select coalesce(min(s), 1) from generate_series(1, (select coalesce(max(seq), 0) + 1 from sbctl.projects)) s
				where s not in (select seq from sbctl.projects)`).Scan(&seq); err != nil {
				return err
			}
		}
		got, err := scanProject(tx.QueryRow(ctx, `
			insert into sbctl.projects (ref, org_id, seq, name, region, engine, class, status, versions, limits,
				branch_id, parent_ref, branch_name, git_branch, persistent, with_data, expires_at, deletion_scheduled_at,
				notify_url, branch_state, branch_detail, clone_method, review_requested_at, branch_egress)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
				$11::text::uuid, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24) returning `+projectCols,
			append([]any{p.Ref, nullOrg(p.OrgID), seq, p.Name, p.Region, p.Engine, p.Class, p.Status, versions, limits}, branchArgs(p.Branch)...)...))
		if err != nil {
			return err
		}
		*p = *got
		return nil
	})
}

// nullStr maps "" to SQL null.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// branchArgs are the 14 branch columns of an insert, in column order.
func branchArgs(b *BranchInfo) []any {
	if b == nil {
		a := make([]any, 14)
		a[4], a[5] = false, false // persistent, with_data
		return a
	}
	return []any{nullStr(b.ID), nullStr(b.ParentRef), nullStr(b.Name), nullStr(b.GitBranch), b.Persistent, b.WithData,
		b.ExpiresAt, b.DeletionScheduledAt, nullStr(b.NotifyURL), nullStr(string(b.State)), nullStr(b.Detail), nullStr(b.CloneMethod), b.ReviewRequestedAt, nullStr(b.Egress)}
}

// UpdateBranch implements Registry.
func (r *Postgres) UpdateBranch(ctx context.Context, ref string, b *BranchInfo) error {
	return affected(r.pool.Exec(ctx, `
		update sbctl.projects set branch_name = $2, git_branch = $3, persistent = $4, with_data = $5, expires_at = $6,
			deletion_scheduled_at = $7, notify_url = $8, branch_state = $9, branch_detail = $10, clone_method = $11,
			review_requested_at = $12, updated_at = now()
		where ref = $1 and parent_ref is not null`,
		ref, nullStr(b.Name), nullStr(b.GitBranch), b.Persistent, b.WithData, b.ExpiresAt, b.DeletionScheduledAt,
		nullStr(b.NotifyURL), nullStr(string(b.State)), nullStr(b.Detail), nullStr(b.CloneMethod), b.ReviewRequestedAt))
}

// SetBranchEgress implements Registry: a compare-and-set on branch_egress alone.
func (r *Postgres) SetBranchEgress(ctx context.Context, ref, from, to string) error {
	tag, err := r.pool.Exec(ctx, `
		update sbctl.projects set branch_egress = $3, updated_at = now()
		where ref = $1 and parent_ref is not null and coalesce(branch_egress, '') = $2`,
		ref, from, nullStr(to))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var isBranch bool
	if err := r.pool.QueryRow(ctx, `select parent_ref is not null from sbctl.projects where ref = $1`, ref).Scan(&isBranch); err != nil {
		return mapErr(err)
	}
	if !isBranch {
		return ErrNotFound
	}
	return fmt.Errorf("%w: the egress policy of %s is no longer %q", ErrConflict, ref, from)
}

func (r *Postgres) GetProject(ctx context.Context, ref string) (*Project, error) {
	return scanProject(r.pool.QueryRow(ctx, `select `+projectCols+` from sbctl.projects where ref = $1`, ref))
}

func (r *Postgres) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := r.pool.Query(ctx, `select `+projectCols+` from sbctl.projects order by seq`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Project, error) {
		p, err := scanProject(row)
		if err != nil {
			return Project{}, err
		}
		return *p, nil
	})
}

func (r *Postgres) UpdateProject(ctx context.Context, p *Project) error {
	versions, _ := json.Marshal(p.Versions)
	limits, _ := json.Marshal(p.Limits)
	got, err := scanProject(r.pool.QueryRow(ctx, `
		update sbctl.projects set name = $2, region = $3, class = $4, status = $5, versions = $6, limits = $7, updated_at = now()
		where ref = $1 returning `+projectCols, p.Ref, p.Name, p.Region, p.Class, p.Status, versions, limits))
	if err != nil {
		return err
	}
	*p = *got
	return nil
}

func (r *Postgres) SetProjectStatus(ctx context.Context, ref string, s Status) error {
	return affected(r.pool.Exec(ctx, `update sbctl.projects set status = $2, updated_at = now() where ref = $1`, ref, s))
}

func (r *Postgres) DeleteProject(ctx context.Context, ref string) error {
	return affected(r.pool.Exec(ctx, `delete from sbctl.projects where ref = $1`, ref))
}

// Secrets

func (r *Postgres) PutSecret(ctx context.Context, ref, name string, sealed []byte) error {
	_, err := r.pool.Exec(ctx, `
		insert into sbctl.project_secrets (ref, name, ciphertext) values ($1, $2, $3)
		on conflict (ref, name) do update set ciphertext = excluded.ciphertext, created_at = now()`, ref, name, sealed)
	return mapErr(err)
}

// PutSecretIfAbsent implements SecretCreator.
func (r *Postgres) PutSecretIfAbsent(ctx context.Context, ref, name string, sealed []byte) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		insert into sbctl.project_secrets (ref, name, ciphertext) values ($1, $2, $3)
		on conflict (ref, name) do nothing`, ref, name, sealed)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// HasDashboardSSO reports whether at least one SAML identity provider of the dashboard is
// registered (migration 1000_sso.sql): Studio shows "Continue with SSO" only then.
func (r *Postgres) HasDashboardSSO(ctx context.Context) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `select exists (select 1 from sbctl.sso_providers)`).Scan(&ok)
	return ok, mapErr(err)
}

func (r *Postgres) GetSecret(ctx context.Context, ref, name string) ([]byte, error) {
	var b []byte
	err := r.pool.QueryRow(ctx, `select ciphertext from sbctl.project_secrets where ref = $1 and name = $2`, ref, name).Scan(&b)
	return b, mapErr(err)
}

func (r *Postgres) GetSecrets(ctx context.Context, ref string) (map[string][]byte, error) {
	rows, err := r.pool.Query(ctx, `select name, ciphertext from sbctl.project_secrets where ref = $1`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var n string
		var b []byte
		if err := rows.Scan(&n, &b); err != nil {
			return nil, err
		}
		out[n] = b
	}
	return out, rows.Err()
}

// Access tokens

const tokenCols = `id, user_id::text, name, token_hash, token_prefix, created_at, last_used_at, expires_at`

func scanToken(row pgx.Row) (*AccessToken, error) {
	var t AccessToken
	if err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Hash, &t.Prefix, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (r *Postgres) CreateAccessToken(ctx context.Context, t *AccessToken) error {
	got, err := scanToken(r.pool.QueryRow(ctx, `
		insert into sbctl.access_tokens (user_id, name, token_hash, token_prefix, expires_at)
		values ($1, $2, $3, $4, $5) returning `+tokenCols, t.UserID, t.Name, t.Hash, t.Prefix, t.ExpiresAt))
	if err != nil {
		return err
	}
	*t = *got
	return nil
}

func (r *Postgres) GetAccessTokenByHash(ctx context.Context, hash []byte) (*AccessToken, error) {
	return scanToken(r.pool.QueryRow(ctx, `select `+tokenCols+` from sbctl.access_tokens where token_hash = $1`, hash))
}

func (r *Postgres) ListAccessTokens(ctx context.Context, userID string) ([]AccessToken, error) {
	rows, err := r.pool.Query(ctx, `select `+tokenCols+` from sbctl.access_tokens where user_id = $1 order by id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccessToken, error) {
		t, err := scanToken(row)
		if err != nil {
			return AccessToken{}, err
		}
		return *t, nil
	})
}

func (r *Postgres) TouchAccessToken(ctx context.Context, id int64, at time.Time) error {
	return affected(r.pool.Exec(ctx, `update sbctl.access_tokens set last_used_at = $2 where id = $1`, id, at))
}

func (r *Postgres) DeleteAccessToken(ctx context.Context, userID string, id int64) error {
	return affected(r.pool.Exec(ctx, `delete from sbctl.access_tokens where id = $1 and user_id = $2`, id, userID))
}

// Routes

func (r *Postgres) PutRoute(ctx context.Context, rt Route) error {
	if rt.Kind == "" {
		rt.Kind = "api"
	}
	_, err := r.pool.Exec(ctx, `
		insert into sbctl.routes (host, ref, kind) values ($1, $2, $3)
		on conflict (host) do update set ref = excluded.ref, kind = excluded.kind`, rt.Host, rt.Ref, rt.Kind)
	return mapErr(err)
}

func (r *Postgres) DeleteRoute(ctx context.Context, host string) error {
	return affected(r.pool.Exec(ctx, `delete from sbctl.routes where host = $1`, host))
}

func (r *Postgres) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := r.pool.Query(ctx, `select host, ref, kind, created_at from sbctl.routes order by host`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Route, error) {
		var rt Route
		err := row.Scan(&rt.Host, &rt.Ref, &rt.Kind, &rt.CreatedAt)
		return rt, err
	})
}

// Backups

const backupCols = `id, ref, kind, status, location, timeline, start_lsn, stop_lsn, size_bytes, error, started_at, finished_at`

func scanBackup(row pgx.Row) (*Backup, error) {
	var b Backup
	if err := row.Scan(&b.ID, &b.Ref, &b.Kind, &b.Status, &b.Location, &b.Timeline, &b.StartLSN, &b.StopLSN,
		&b.SizeBytes, &b.Error, &b.StartedAt, &b.FinishedAt); err != nil {
		return nil, mapErr(err)
	}
	return &b, nil
}

func (r *Postgres) CreateBackup(ctx context.Context, b *Backup) error {
	if b.Kind == "" {
		b.Kind = "base"
	}
	if b.Status == "" {
		b.Status = BackupRunning
	}
	if b.StartedAt.IsZero() {
		b.StartedAt = time.Now()
	}
	got, err := scanBackup(r.pool.QueryRow(ctx, `
		insert into sbctl.backups (ref, kind, status, location, timeline, start_lsn, stop_lsn, size_bytes, error, started_at, finished_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) returning `+backupCols,
		b.Ref, b.Kind, b.Status, b.Location, b.Timeline, b.StartLSN, b.StopLSN, b.SizeBytes, b.Error, b.StartedAt, b.FinishedAt))
	if err != nil {
		return err
	}
	*b = *got
	return nil
}

func (r *Postgres) UpdateBackup(ctx context.Context, b *Backup) error {
	return affected(r.pool.Exec(ctx, `
		update sbctl.backups set status = $2, location = $3, timeline = $4, start_lsn = $5, stop_lsn = $6,
		size_bytes = $7, error = $8, finished_at = $9 where id = $1`,
		b.ID, b.Status, b.Location, b.Timeline, b.StartLSN, b.StopLSN, b.SizeBytes, b.Error, b.FinishedAt))
}

func (r *Postgres) ListBackups(ctx context.Context, ref string) ([]Backup, error) {
	rows, err := r.pool.Query(ctx, `select `+backupCols+` from sbctl.backups where ref = $1 order by started_at desc, id desc`, ref)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Backup, error) {
		b, err := scanBackup(row)
		if err != nil {
			return Backup{}, err
		}
		return *b, nil
	})
}

func (r *Postgres) DeleteBackup(ctx context.Context, id int64) error {
	return affected(r.pool.Exec(ctx, `delete from sbctl.backups where id = $1`, id))
}

// Events

func (r *Postgres) AppendEvent(ctx context.Context, ref, kind string, payload any) error {
	b := []byte("{}")
	if payload != nil {
		var err error
		if b, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	var refArg any
	if ref != "" {
		refArg = ref
	}
	_, err := r.pool.Exec(ctx, `insert into sbctl.events (ref, kind, payload) values ($1, $2, $3)`, refArg, kind, b)
	return err
}

func (r *Postgres) ListEvents(ctx context.Context, ref string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		select id, coalesce(ref, ''), kind, payload, created_at from sbctl.events
		where $1 = '' or ref = $1 order by id desc limit $2`, ref, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var p []byte
		err := row.Scan(&e.ID, &e.Ref, &e.Kind, &p, &e.CreatedAt)
		e.Payload = p
		return e, err
	})
}

// Subscribe LISTENs on sbctl_changes with a dedicated connection. The channel closes
// when ctx ends or the connection drops; consumers then resubscribe and reload.
func (r *Postgres) Subscribe(ctx context.Context) (<-chan Change, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `listen sbctl_changes`); err != nil {
		conn.Release()
		return nil, err
	}
	ch := make(chan Change, 64)
	go func() {
		defer close(ch)
		defer conn.Release()
		defer conn.Exec(context.Background(), `unlisten sbctl_changes`) //nolint:errcheck
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				return
			}
			var c Change
			if json.Unmarshal([]byte(n.Payload), &c) != nil {
				continue
			}
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
