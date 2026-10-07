package registry

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var (
	_ DomainStore = (*Postgres)(nil)
	_ DomainStore = (*Memory)(nil)
)

const hostnameCols = `ref, hostname, status, token, cname_ok, txt_ok, created_at, updated_at, verified_at, activated_at`

func scanHostname(row pgx.Row) (*CustomHostname, error) {
	var h CustomHostname
	if err := row.Scan(&h.Ref, &h.Hostname, &h.Status, &h.Token, &h.CNAMEOK, &h.TXTOK, &h.CreatedAt, &h.UpdatedAt, &h.VerifiedAt, &h.ActivatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &h, nil
}

// inTx runs fn in a transaction.
func (r *Postgres) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Postgres) GetCustomHostname(ctx context.Context, ref string) (*CustomHostname, error) {
	return scanHostname(r.pool.QueryRow(ctx, `select `+hostnameCols+` from supavise.custom_hostnames where ref = $1`, ref))
}

func (r *Postgres) PutCustomHostname(ctx context.Context, h *CustomHostname) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		var held bool
		if err := tx.QueryRow(ctx, `
			select exists (select 1 from supavise.custom_hostnames
			  where hostname = $1 and ref <> $2 and status in ($3, $4))`,
			h.Hostname, h.Ref, HostnameOriginReady, HostnameActive).Scan(&held); err != nil {
			return err
		}
		if held {
			return fmt.Errorf("%w: custom hostname held by another project", ErrConflict)
		}
		tag, err := tx.Exec(ctx, `
			insert into supavise.custom_hostnames (ref, hostname, status, token)
			values ($1, $2, $3, $4)
			on conflict (ref) do update set hostname = excluded.hostname, status = excluded.status,
			  token = excluded.token, cname_ok = false, txt_ok = false, created_at = now(),
			  updated_at = now(), verified_at = null, activated_at = null
			where supavise.custom_hostnames.status <> $5`,
			h.Ref, h.Hostname, h.Status, h.Token, HostnameActive)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: the project's custom hostname is active", ErrConflict)
		}
		return nil
	})
}

func (r *Postgres) UpdateCustomHostname(ctx context.Context, h *CustomHostname) error {
	tag, err := r.pool.Exec(ctx, `
		update supavise.custom_hostnames
		   set status = $3, cname_ok = $4, txt_ok = $5, verified_at = $6, updated_at = now()
		 where ref = $1 and hostname = $2 and status <> $7`,
		h.Ref, h.Hostname, h.Status, h.CNAMEOK, h.TXTOK, h.VerifiedAt, HostnameActive)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.GetCustomHostname(ctx, h.Ref); err != nil {
		return err
	}
	return fmt.Errorf("%w: the custom hostname changed meanwhile", ErrConflict)
}

func (r *Postgres) ActivateCustomHostname(ctx context.Context, ref string) (*CustomHostname, error) {
	var out *CustomHostname
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		h, err := scanHostname(tx.QueryRow(ctx, `select `+hostnameCols+` from supavise.custom_hostnames where ref = $1 for update`, ref))
		if err != nil {
			return err
		}
		if h.Status != HostnameOriginReady {
			return fmt.Errorf("%w: the custom hostname is %s", ErrConflict, h.Status)
		}
		if _, err := tx.Exec(ctx, `insert into supavise.routes (host, ref, kind) values ($1, $2, $3)`, h.Hostname, ref, RouteCustom); err != nil {
			return mapErr(err)
		}
		out, err = scanHostname(tx.QueryRow(ctx, `
			update supavise.custom_hostnames set status = $2, activated_at = now(), updated_at = now()
			 where ref = $1 returning `+hostnameCols, ref, HostnameActive))
		return err
	})
	return out, err
}

func (r *Postgres) DeleteCustomHostname(ctx context.Context, ref string) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		var host, status string
		if err := tx.QueryRow(ctx, `delete from supavise.custom_hostnames where ref = $1 returning hostname, status`, ref).Scan(&host, &status); err != nil {
			return mapErr(err)
		}
		if status == HostnameActive {
			_, err := tx.Exec(ctx, `delete from supavise.routes where host = $1 and ref = $2 and kind = $3`, host, ref, RouteCustom)
			return err
		}
		return nil
	})
}

func (r *Postgres) ListCustomHostnames(ctx context.Context) ([]CustomHostname, error) {
	rows, err := r.pool.Query(ctx, `select `+hostnameCols+` from supavise.custom_hostnames order by hostname, ref`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (CustomHostname, error) {
		h, err := scanHostname(row)
		if err != nil {
			return CustomHostname{}, err
		}
		return *h, nil
	})
}

func (r *Postgres) GetVanitySubdomain(ctx context.Context, ref string) (*VanitySubdomain, error) {
	var v VanitySubdomain
	err := r.pool.QueryRow(ctx, `select ref, name, created_at from supavise.vanity_subdomains where ref = $1`, ref).Scan(&v.Ref, &v.Name, &v.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &v, nil
}

func (r *Postgres) VanitySubdomainOwner(ctx context.Context, name string) (string, error) {
	var ref string
	err := r.pool.QueryRow(ctx, `select ref from supavise.vanity_subdomains where name = $1`, name).Scan(&ref)
	return ref, mapErr(err)
}

func (r *Postgres) PutVanitySubdomain(ctx context.Context, ref, name, host string) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `delete from supavise.routes where ref = $1 and kind = $2`, ref, RouteVanity); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into supavise.vanity_subdomains (ref, name) values ($1, $2)
			on conflict (ref) do update set name = excluded.name, created_at = now()`, ref, name); err != nil {
			return mapErr(err)
		}
		_, err := tx.Exec(ctx, `insert into supavise.routes (host, ref, kind) values ($1, $2, $3)`, host, ref, RouteVanity)
		return mapErr(err)
	})
}

func (r *Postgres) DeleteVanitySubdomain(ctx context.Context, ref string) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `delete from supavise.vanity_subdomains where ref = $1`, ref)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `delete from supavise.routes where ref = $1 and kind = $2`, ref, RouteVanity)
		return err
	})
}
