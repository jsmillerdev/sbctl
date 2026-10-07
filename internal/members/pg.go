package members

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG is the Postgres Store over the registry's pool (tables from registry migration
// 0900_members.sql, which must have been applied).
type PG struct {
	pool *pgxpool.Pool
	pgOps
}

// NewPG returns a PG store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool, pgOps: pgOps{q: pool}} }

var _ Store = (*PG)(nil)

// querier is what a pool and a transaction share.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Update implements Store: the transaction first locks the organization's row, which also
// makes a missing organization an ErrNotFound instead of a foreign-key failure later.
func (s *PG) Update(ctx context.Context, org int64, fn func(Ops) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `select 1 from supavise.organizations where id = $1 for update`, org).Scan(&one); err != nil {
			return mapErr(err)
		}
		return fn(&pgOps{q: tx})
	})
}

type pgOps struct{ q querier }

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" { // foreign_key_violation
		return errors.Join(ErrInvalid, errors.New("a referenced project or organization does not exist"))
	}
	return err
}

func roleOrNil(id int) any {
	if id == 0 {
		return nil
	}
	return int16(id)
}

func (o *pgOps) GetMember(ctx context.Context, org int64, user string) (*Member, error) {
	var m Member
	var role *int16
	err := o.q.QueryRow(ctx, `select org_id, user_id::text, role_id, created_at from supavise.org_members where org_id = $1 and user_id = $2::uuid`,
		org, user).Scan(&m.OrgID, &m.UserID, &role, &m.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if role != nil {
		m.RoleID = int(*role)
	}
	return &m, nil
}

func scanMembers(rows pgx.Rows) ([]Member, error) {
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var role *int16
		if err := rows.Scan(&m.OrgID, &m.UserID, &role, &m.CreatedAt); err != nil {
			return nil, err
		}
		if role != nil {
			m.RoleID = int(*role)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (o *pgOps) ListMembers(ctx context.Context, org int64) ([]Member, error) {
	rows, err := o.q.Query(ctx, `select org_id, user_id::text, role_id, created_at from supavise.org_members where org_id = $1 order by created_at, user_id`, org)
	if err != nil {
		return nil, err
	}
	return scanMembers(rows)
}

func (o *pgOps) MembershipsOf(ctx context.Context, user string) ([]Member, error) {
	rows, err := o.q.Query(ctx, `select org_id, user_id::text, role_id, created_at from supavise.org_members where user_id = $1::uuid order by org_id`, user)
	if err != nil {
		return nil, err
	}
	return scanMembers(rows)
}

func (o *pgOps) PutMember(ctx context.Context, m Member) error {
	_, err := o.q.Exec(ctx, `
		insert into supavise.org_members (org_id, user_id, role_id) values ($1, $2::uuid, $3)
		on conflict (org_id, user_id) do update set role_id = excluded.role_id`, m.OrgID, m.UserID, roleOrNil(m.RoleID))
	if err != nil {
		return mapErr(err)
	}
	// Whoever holds a membership is settled: removing it later must not hand the account to the
	// legacy rule, which would make an account that predates roles Owner of everything.
	return o.MarkLegacyChecked(ctx, m.UserID)
}

func (o *pgOps) DeleteMember(ctx context.Context, org int64, user string) error {
	tag, err := o.q.Exec(ctx, `delete from supavise.org_members where org_id = $1 and user_id = $2::uuid`, org, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (o *pgOps) CountOwners(ctx context.Context, org int64) (int, error) {
	var n int
	err := o.q.QueryRow(ctx, `select count(*) from supavise.org_members where org_id = $1 and role_id = 1`, org).Scan(&n)
	return n, err
}

const projectRoleSelect = `
	select r.id, r.org_id, r.user_id::text, r.base_role_id,
	       coalesce(array_agg(f.ref order by f.ref) filter (where f.ref is not null), '{}')
	  from supavise.org_project_roles r
	  left join supavise.org_project_role_refs f on f.role_id = r.id `

func scanProjectRoles(rows pgx.Rows) ([]ProjectRole, error) {
	defer rows.Close()
	var out []ProjectRole
	for rows.Next() {
		var r ProjectRole
		var base int16
		if err := rows.Scan(&r.ID, &r.OrgID, &r.UserID, &base, &r.Refs); err != nil {
			return nil, err
		}
		r.BaseRoleID = int(base)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (o *pgOps) ProjectRoles(ctx context.Context, org int64) ([]ProjectRole, error) {
	rows, err := o.q.Query(ctx, projectRoleSelect+` where r.org_id = $1 group by r.id having count(f.ref) > 0 order by r.id`, org)
	if err != nil {
		return nil, err
	}
	return scanProjectRoles(rows)
}

func (o *pgOps) ProjectRolesOf(ctx context.Context, user string) ([]ProjectRole, error) {
	rows, err := o.q.Query(ctx, projectRoleSelect+` where r.user_id = $1::uuid group by r.id having count(f.ref) > 0 order by r.id`, user)
	if err != nil {
		return nil, err
	}
	return scanProjectRoles(rows)
}

func (o *pgOps) GetProjectRole(ctx context.Context, org, id int64) (*ProjectRole, error) {
	rows, err := o.q.Query(ctx, projectRoleSelect+` where r.org_id = $1 and r.id = $2 group by r.id`, org, id)
	if err != nil {
		return nil, err
	}
	rs, err := scanProjectRoles(rows)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, ErrNotFound
	}
	return &rs[0], nil
}

func (o *pgOps) PutProjectRole(ctx context.Context, org int64, user string, base int, refs []string) (*ProjectRole, error) {
	refs = dedupe(refs)
	if len(refs) == 0 {
		_, err := o.q.Exec(ctx, `delete from supavise.org_project_roles where org_id = $1 and user_id = $2::uuid and base_role_id = $3`, org, user, int16(base))
		return nil, err
	}
	var id int64
	err := o.q.QueryRow(ctx, `
		insert into supavise.org_project_roles (org_id, user_id, base_role_id) values ($1, $2::uuid, $3)
		on conflict (org_id, user_id, base_role_id) do update set base_role_id = excluded.base_role_id
		returning id`, org, user, int16(base)).Scan(&id)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23503" {
			return nil, ErrNotFound // not a member
		}
		return nil, mapErr(err)
	}
	if _, err := o.q.Exec(ctx, `delete from supavise.org_project_role_refs where role_id = $1 and ref <> all($2)`, id, refs); err != nil {
		return nil, err
	}
	if _, err := o.q.Exec(ctx, `insert into supavise.org_project_role_refs (role_id, ref) select $1, unnest($2::text[]) on conflict do nothing`, id, refs); err != nil {
		return nil, mapErr(err)
	}
	return &ProjectRole{ID: id, OrgID: org, UserID: user, BaseRoleID: base, Refs: refs}, nil
}

func (o *pgOps) DeleteProjectRole(ctx context.Context, org, id int64) error {
	tag, err := o.q.Exec(ctx, `delete from supavise.org_project_roles where org_id = $1 and id = $2`, org, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (o *pgOps) OwnerScopedRoleIDs(ctx context.Context, orgs []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	rows, err := o.q.Query(ctx, `select org_id, id from supavise.org_project_roles where base_role_id = 1 and org_id = any($1)`, orgs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var org, id int64
		if err := rows.Scan(&org, &id); err != nil {
			rows.Close()
			return nil, err
		}
		out[org] = append(out[org], id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = o.q.Query(ctx, `select org_id, id from supavise.org_invitations
		where role_id = 1 and accepted_at is null and cardinality(project_refs) > 0 and org_id = any($1)`, orgs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var org, id int64
		if err := rows.Scan(&org, &id); err != nil {
			return nil, err
		}
		out[org] = append(out[org], InvitationRoleIDBase+id)
	}
	return out, rows.Err()
}

const invCols = `id, org_id, email, role_id, project_refs, coalesce(invited_by::text, ''), created_at, expires_at, accepted_at, coalesce(accepted_by::text, '')`

func scanInv(row pgx.Row) (*Invitation, error) {
	var i Invitation
	var role int16
	if err := row.Scan(&i.ID, &i.OrgID, &i.Email, &role, &i.ProjectRefs, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt, &i.AcceptedAt, &i.AcceptedBy); err != nil {
		return nil, mapErr(err)
	}
	i.RoleID = int(role)
	return &i, nil
}

func scanInvs(rows pgx.Rows) ([]Invitation, error) {
	defer rows.Close()
	var out []Invitation
	for rows.Next() {
		i, err := scanInv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

func (o *pgOps) CreateInvitation(ctx context.Context, inv Invitation, hash []byte) (*Invitation, error) {
	if _, err := o.q.Exec(ctx, `delete from supavise.org_invitations where org_id = $1 and email = $2 and accepted_at is null`, inv.OrgID, inv.Email); err != nil {
		return nil, err
	}
	var by any
	if inv.InvitedBy != "" {
		by = inv.InvitedBy
	}
	refs := inv.ProjectRefs
	if refs == nil {
		refs = []string{}
	}
	return scanInv(o.q.QueryRow(ctx, `
		insert into supavise.org_invitations (org_id, email, role_id, project_refs, token_hash, invited_by, expires_at)
		values ($1, $2, $3, $4, $5, $6::uuid, $7) returning `+invCols,
		inv.OrgID, inv.Email, int16(inv.RoleID), refs, hash, by, inv.ExpiresAt))
}

func (o *pgOps) ListInvitations(ctx context.Context, org int64) ([]Invitation, error) {
	rows, err := o.q.Query(ctx, `select `+invCols+` from supavise.org_invitations where org_id = $1 and accepted_at is null order by id desc`, org)
	if err != nil {
		return nil, err
	}
	return scanInvs(rows)
}

func (o *pgOps) GetInvitation(ctx context.Context, org, id int64) (*Invitation, error) {
	return scanInv(o.q.QueryRow(ctx, `select `+invCols+` from supavise.org_invitations where org_id = $1 and id = $2`, org, id))
}

func (o *pgOps) InvitationByHash(ctx context.Context, hash []byte) (*Invitation, error) {
	return scanInv(o.q.QueryRow(ctx, `select `+invCols+` from supavise.org_invitations where token_hash = $1`, hash))
}

func (o *pgOps) PendingInvitationsFor(ctx context.Context, email string, now time.Time) ([]Invitation, error) {
	rows, err := o.q.Query(ctx, `select `+invCols+` from supavise.org_invitations where email = $1 and accepted_at is null and expires_at > $2 order by id`, email, now)
	if err != nil {
		return nil, err
	}
	return scanInvs(rows)
}

func (o *pgOps) DeleteInvitation(ctx context.Context, org, id int64) error {
	tag, err := o.q.Exec(ctx, `delete from supavise.org_invitations where org_id = $1 and id = $2 and accepted_at is null`, org, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (o *pgOps) MarkInvitationAccepted(ctx context.Context, id int64, user string, now time.Time) error {
	tag, err := o.q.Exec(ctx, `update supavise.org_invitations set accepted_at = $3, accepted_by = $2::uuid
		where id = $1 and accepted_at is null and expires_at > $3`, id, user, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (o *pgOps) MFAEnforced(ctx context.Context, org int64) (bool, error) {
	var v bool
	err := o.q.QueryRow(ctx, `select coalesce((select enforced from supavise.org_mfa where org_id = $1), false)`, org).Scan(&v)
	return v, err
}

func (o *pgOps) SetMFAEnforced(ctx context.Context, org int64, v bool) error {
	_, err := o.q.Exec(ctx, `insert into supavise.org_mfa (org_id, enforced) values ($1, $2)
		on conflict (org_id) do update set enforced = excluded.enforced, updated_at = now()`, org, v)
	return mapErr(err)
}

func (o *pgOps) GetDomainDefault(ctx context.Context, domain string) (*DomainDefault, error) {
	var d DomainDefault
	var role int16
	err := o.q.QueryRow(ctx, `select domain, org_id, role_id, created_at from supavise.sso_default_roles where domain = $1`, domain).
		Scan(&d.Domain, &d.OrgID, &role, &d.Created)
	if err != nil {
		return nil, mapErr(err)
	}
	d.RoleID = int(role)
	return &d, nil
}

func (o *pgOps) ListDomainDefaults(ctx context.Context) ([]DomainDefault, error) {
	rows, err := o.q.Query(ctx, `select domain, org_id, role_id, created_at from supavise.sso_default_roles order by domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainDefault
	for rows.Next() {
		var d DomainDefault
		var role int16
		if err := rows.Scan(&d.Domain, &d.OrgID, &role, &d.Created); err != nil {
			return nil, err
		}
		d.RoleID = int(role)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (o *pgOps) SetDomainDefault(ctx context.Context, d DomainDefault) error {
	_, err := o.q.Exec(ctx, `insert into supavise.sso_default_roles (domain, org_id, role_id) values ($1, $2, $3)
		on conflict (domain) do update set org_id = excluded.org_id, role_id = excluded.role_id`, d.Domain, d.OrgID, int16(d.RoleID))
	return mapErr(err)
}

func (o *pgOps) DeleteDomainDefault(ctx context.Context, domain string) error {
	tag, err := o.q.Exec(ctx, `delete from supavise.sso_default_roles where domain = $1`, domain)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (o *pgOps) LegacyCutoff(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := o.q.QueryRow(ctx, `select at from supavise.member_meta where key = 'legacy_cutoff'`).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

func (o *pgOps) LegacyChecked(ctx context.Context, user string) (bool, error) {
	var v bool
	err := o.q.QueryRow(ctx, `select exists (select 1 from supavise.member_legacy_checked where user_id = $1::uuid)`, user).Scan(&v)
	return v, err
}

func (o *pgOps) MarkLegacyChecked(ctx context.Context, user string) error {
	_, err := o.q.Exec(ctx, `insert into supavise.member_legacy_checked (user_id) values ($1::uuid) on conflict do nothing`, user)
	return err
}
