package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres Store over the registry's pool (tables from
// registry/migrations/0100_api.sql).
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore returns a PGStore. The registry migrations must have been applied.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

const userCols = `id, user_id::text, email, username, first_name, last_name, created_at, last_seen_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.UserID, &u.Email, &u.Username, &u.FirstName, &u.LastName, &u.CreatedAt, &u.LastSeenAt); err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

func (s *PGStore) UpsertUser(ctx context.Context, u User) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		insert into sbctl.api_users (user_id, email, username, first_name, last_name)
		values ($1::uuid, $2, $3, $4, $5)
		on conflict (user_id) do update
		   set last_seen_at = now(), email = case when sbctl.api_users.email = '' then excluded.email else sbctl.api_users.email end
		returning `+userCols, u.UserID, u.Email, u.Username, u.FirstName, u.LastName))
}

func (s *PGStore) GetUser(ctx context.Context, userID string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `select `+userCols+` from sbctl.api_users where user_id = $1::uuid`, userID))
}

func (s *PGStore) GetUserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `select `+userCols+` from sbctl.api_users where id = $1`, id))
}

func (s *PGStore) UpdateUser(ctx context.Context, u *User) error {
	tag, err := s.pool.Exec(ctx, `update sbctl.api_users set username=$2, first_name=$3, last_name=$4 where user_id=$1::uuid`,
		u.UserID, u.Username, u.FirstName, u.LastName)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *PGStore) PutLoginSession(ctx context.Context, l LoginSession) error {
	_, err := s.pool.Exec(ctx, `
		insert into sbctl.api_cli_login_sessions (session_id, user_id, token_id, server_public_key, nonce, ciphertext, expires_at)
		values ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)
		on conflict (session_id) do update set user_id = excluded.user_id, token_id = excluded.token_id,
		  server_public_key = excluded.server_public_key, nonce = excluded.nonce, ciphertext = excluded.ciphertext,
		  expires_at = excluded.expires_at`,
		l.SessionID, l.UserID, l.TokenID, l.ServerPublicKey, l.Nonce, l.Ciphertext, l.ExpiresAt)
	if err == nil {
		// Opportunistic cleanup keeps the table small without a janitor goroutine.
		_, err = s.pool.Exec(ctx, `delete from sbctl.api_cli_login_sessions where expires_at < now() - interval '1 hour'`)
	}
	return err
}

func (s *PGStore) TakeLoginSession(ctx context.Context, id string) (*LoginSession, error) {
	var l LoginSession
	err := s.pool.QueryRow(ctx, `
		delete from sbctl.api_cli_login_sessions where session_id = $1::uuid
		returning session_id::text, user_id::text, coalesce(token_id, 0), server_public_key, nonce, ciphertext, expires_at`, id).
		Scan(&l.SessionID, &l.UserID, &l.TokenID, &l.ServerPublicKey, &l.Nonce, &l.Ciphertext, &l.ExpiresAt)
	if err != nil {
		return nil, notFound(err)
	}
	if !l.ExpiresAt.After(time.Now()) {
		return nil, ErrNotFound
	}
	return &l, nil
}

const fnCols = `ref, slug, id::text, name, version, status, verify_jwt, coalesce(entrypoint_path,''), coalesce(import_map_path,''), created_at, updated_at`

func scanFn(row pgx.Row) (*Function, error) {
	var f Function
	if err := row.Scan(&f.Ref, &f.Slug, &f.ID, &f.Name, &f.Version, &f.Status, &f.VerifyJWT, &f.EntrypointPath, &f.ImportMapPath, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &f, nil
}

func (s *PGStore) UpsertFunction(ctx context.Context, f *Function, files []FunctionFile) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		got, err := scanFn(tx.QueryRow(ctx, `
			insert into sbctl.api_functions (ref, slug, name, status, verify_jwt, entrypoint_path, import_map_path)
			values ($1, $2, $3, $4, $5, nullif($6,''), nullif($7,''))
			on conflict (ref, slug) do update set name = excluded.name, status = excluded.status,
			  verify_jwt = excluded.verify_jwt, entrypoint_path = excluded.entrypoint_path,
			  import_map_path = excluded.import_map_path, version = sbctl.api_functions.version + 1, updated_at = now()
			returning `+fnCols, f.Ref, f.Slug, f.Name, f.Status, f.VerifyJWT, f.EntrypointPath, f.ImportMapPath))
		if err != nil {
			return err
		}
		*f = *got
		if files == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `delete from sbctl.api_function_files where ref = $1 and slug = $2`, f.Ref, f.Slug); err != nil {
			return err
		}
		for _, file := range files {
			if _, err := tx.Exec(ctx, `insert into sbctl.api_function_files (ref, slug, path, content) values ($1,$2,$3,$4)`,
				f.Ref, f.Slug, file.Path, file.Content); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PGStore) ListFunctions(ctx context.Context, ref string) ([]Function, error) {
	rows, err := s.pool.Query(ctx, `select `+fnCols+` from sbctl.api_functions where ref = $1 order by slug`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Function
	for rows.Next() {
		f, err := scanFn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *PGStore) GetFunction(ctx context.Context, ref, slug string) (*Function, error) {
	return scanFn(s.pool.QueryRow(ctx, `select `+fnCols+` from sbctl.api_functions where ref = $1 and slug = $2`, ref, slug))
}

func (s *PGStore) FunctionFiles(ctx context.Context, ref, slug string) ([]FunctionFile, error) {
	if _, err := s.GetFunction(ctx, ref, slug); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `select path, content from sbctl.api_function_files where ref = $1 and slug = $2 order by path`, ref, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FunctionFile
	for rows.Next() {
		var f FunctionFile
		if err := rows.Scan(&f.Path, &f.Content); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *PGStore) DeleteFunction(ctx context.Context, ref, slug string) error {
	tag, err := s.pool.Exec(ctx, `delete from sbctl.api_functions where ref = $1 and slug = $2`, ref, slug)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *PGStore) PutFunctionSecrets(ctx context.Context, ref string, sealed map[string][]byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for n, b := range sealed {
			if _, err := tx.Exec(ctx, `
				insert into sbctl.api_function_secrets (ref, name, ciphertext) values ($1,$2,$3)
				on conflict (ref, name) do update set ciphertext = excluded.ciphertext, updated_at = now()`, ref, n, b); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PGStore) ListFunctionSecrets(ctx context.Context, ref string) ([]FunctionSecret, error) {
	rows, err := s.pool.Query(ctx, `select name, ciphertext, updated_at from sbctl.api_function_secrets where ref = $1 order by name`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FunctionSecret
	for rows.Next() {
		var f FunctionSecret
		if err := rows.Scan(&f.Name, &f.Sealed, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *PGStore) DeleteFunctionSecrets(ctx context.Context, ref string, names []string) error {
	_, err := s.pool.Exec(ctx, `delete from sbctl.api_function_secrets where ref = $1 and name = any($2)`, ref, names)
	return err
}

const contentCols = `id::text, ref, folder_id::text, owner_id, type, name, description, visibility, favorite, content::text, inserted_at, updated_at`

func scanContent(row pgx.Row) (*Content, error) {
	var c Content
	var body string
	if err := row.Scan(&c.ID, &c.Ref, &c.FolderID, &c.OwnerID, &c.Type, &c.Name, &c.Description, &c.Visibility, &c.Favorite, &body, &c.InsertedAt, &c.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	c.Body = []byte(body)
	return &c, nil
}

func (s *PGStore) ListContent(ctx context.Context, ref string, q ContentQuery) ([]Content, error) {
	sql := `select ` + contentCols + ` from sbctl.api_content where ref = $1
	  and ($2 = '' or type = $2) and ($3 = '' or visibility = $3) and ($4::bigint = 0 or owner_id = $4)
	  and (not $5 or favorite) and ($6 = '' or name ilike '%' || $6 || '%')
	  and (not $7 or folder_id is null) and ($8::uuid is null or folder_id = $8::uuid)
	  order by updated_at desc`
	if q.Limit > 0 {
		sql += fmt.Sprintf(" limit %d", q.Limit)
	}
	rows, err := s.pool.Query(ctx, sql, ref, q.Type, q.Visibility, q.OwnerID, q.Favorite, q.Name, q.RootOnly, q.FolderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Content
	for rows.Next() {
		c, err := scanContent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *PGStore) GetContent(ctx context.Context, ref, id string) (*Content, error) {
	c, err := scanContent(s.pool.QueryRow(ctx, `select `+contentCols+` from sbctl.api_content where ref = $1 and id = $2::uuid`, ref, id))
	return c, err
}

func (s *PGStore) UpsertContent(ctx context.Context, c *Content) error {
	body := string(c.Body)
	if body == "" {
		body = "{}"
	}
	var id any
	if c.ID != "" {
		id = c.ID
	}
	got, err := scanContent(s.pool.QueryRow(ctx, `
		insert into sbctl.api_content (id, ref, folder_id, owner_id, type, name, description, visibility, favorite, content)
		values (coalesce($1::uuid, gen_random_uuid()), $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10::jsonb)
		on conflict (id) do update set folder_id = excluded.folder_id, name = excluded.name, description = excluded.description,
		  visibility = excluded.visibility, favorite = excluded.favorite, content = excluded.content, type = excluded.type, updated_at = now()
		  where sbctl.api_content.ref = excluded.ref
		returning `+contentCols, id, c.Ref, c.FolderID, c.OwnerID, c.Type, c.Name, c.Description, c.Visibility, c.Favorite, body))
	if err != nil {
		return err
	}
	*c = *got
	return nil
}

func (s *PGStore) DeleteContent(ctx context.Context, ref string, ids []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `delete from sbctl.api_content where ref = $1 and id = any($2::uuid[]) returning id::text`, ref, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *PGStore) CountContent(ctx context.Context, ref string, ownerID int64) (ContentCount, error) {
	var n ContentCount
	err := s.pool.QueryRow(ctx, `
		select count(*) filter (where owner_id = $2 and visibility = 'user'),
		       count(*) filter (where visibility <> 'user'),
		       count(*) filter (where owner_id = $2 and favorite)
		  from sbctl.api_content where ref = $1`, ref, ownerID).Scan(&n.Private, &n.Shared, &n.Favorites)
	return n, err
}

func scanFolder(row pgx.Row) (*ContentFolder, error) {
	var f ContentFolder
	if err := row.Scan(&f.ID, &f.Ref, &f.ParentID, &f.OwnerID, &f.Name, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &f, nil
}

const folderCols = `id::text, ref, parent_id::text, owner_id, name, created_at, updated_at`

func (s *PGStore) ListFolders(ctx context.Context, ref string, parentID *string) ([]ContentFolder, error) {
	rows, err := s.pool.Query(ctx, `select `+folderCols+` from sbctl.api_content_folders
		where ref = $1 and parent_id is not distinct from $2::uuid order by name`, ref, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentFolder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *PGStore) CreateFolder(ctx context.Context, f *ContentFolder) error {
	got, err := scanFolder(s.pool.QueryRow(ctx, `
		insert into sbctl.api_content_folders (ref, parent_id, owner_id, name) values ($1, $2::uuid, $3, $4)
		returning `+folderCols, f.Ref, f.ParentID, f.OwnerID, f.Name))
	if err != nil {
		return err
	}
	*f = *got
	return nil
}

func (s *PGStore) RenameFolder(ctx context.Context, ref, id, name string) error {
	tag, err := s.pool.Exec(ctx, `update sbctl.api_content_folders set name = $3, updated_at = now() where ref = $1 and id = $2::uuid`, ref, id, name)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *PGStore) DeleteFolders(ctx context.Context, ref string, ids []string) error {
	_, err := s.pool.Exec(ctx, `delete from sbctl.api_content_folders where ref = $1 and id = any($2::uuid[])`, ref, ids)
	return err
}
