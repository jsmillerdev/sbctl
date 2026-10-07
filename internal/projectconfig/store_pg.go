package projectconfig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Store over the registry's pool (table sbctl.project_settings, migration
// 0800_project_settings.sql).
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore returns a PGStore. The registry migrations must have been applied.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Get implements Store.
func (s *PGStore) Get(ctx context.Context, ref string, svc Service) (*Record, error) {
	var (
		version        int64
		values, sealed []byte
		rec            = &Record{Ref: ref, Service: svc, Values: map[string]any{}, Sealed: map[string][]byte{}}
	)
	err := s.pool.QueryRow(ctx, `select version, "values", sealed, updated_at from sbctl.project_settings where ref = $1 and service = $2`,
		ref, string(svc)).Scan(&version, &values, &sealed, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, nil
	}
	if err != nil {
		return nil, err
	}
	rec.Version = version
	if err := json.Unmarshal(values, &rec.Values); err != nil {
		return nil, fmt.Errorf("projectconfig: stored %s settings of %s: %w", svc, ref, err)
	}
	enc := map[string]string{}
	if err := json.Unmarshal(sealed, &enc); err != nil {
		return nil, fmt.Errorf("projectconfig: stored sealed %s settings of %s: %w", svc, ref, err)
	}
	for k, v := range enc {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("projectconfig: sealed setting %s of %s: %w", k, ref, err)
		}
		rec.Sealed[k] = b
	}
	return rec, nil
}

// Put implements Store: an insert when expected is 0 (a concurrent first writer makes it
// a conflict), otherwise an update guarded by the version.
func (s *PGStore) Put(ctx context.Context, rec *Record, expected int64) (*Record, error) {
	values, err := json.Marshal(rec.Values)
	if err != nil {
		return nil, err
	}
	enc := make(map[string]string, len(rec.Sealed))
	for k, v := range rec.Sealed {
		enc[k] = base64.StdEncoding.EncodeToString(v)
	}
	sealed, _ := json.Marshal(enc)
	out := *rec
	out.Values, out.Sealed = rec.Values, rec.Sealed
	if expected == 0 {
		err = s.pool.QueryRow(ctx, `
			insert into sbctl.project_settings (ref, service, version, "values", sealed)
			values ($1, $2, 1, $3, $4)
			returning version, updated_at`, rec.Ref, string(rec.Service), values, sealed).Scan(&out.Version, &out.UpdatedAt)
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "23505":
			return nil, ErrConflict
		case errors.As(err, &pe) && pe.Code == "23503":
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	err = s.pool.QueryRow(ctx, `
		update sbctl.project_settings
		   set version = version + 1, "values" = $3, sealed = $4, updated_at = now()
		 where ref = $1 and service = $2 and version = $5
		returning version, updated_at`, rec.Ref, string(rec.Service), values, sealed, expected).Scan(&out.Version, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}
