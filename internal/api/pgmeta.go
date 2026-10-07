package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmillerdev/supavise/internal/api/cryptojs"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// SecretPGMetaCryptoKey is the name of the system-project secret that holds the
// passphrase shared with supavise-pgmeta (its CRYPTO_KEY) when [api] pgmeta_crypto_key is
// not configured. The unit renderer must give supavise-pgmeta the same value.
const SecretPGMetaCryptoKey = "pgmeta_crypto_key"

// ensureKeyMu serializes EnsurePGMetaCryptoKey within a process.
var ensureKeyMu sync.Mutex

// EnsurePGMetaCryptoKey returns the passphrase shared with supavise-pgmeta (its CRYPTO_KEY),
// creating it when absent: a random value sealed in the registry as system project
// secret SecretPGMetaCryptoKey. Insert-if-absent, then read back, so concurrent
// callers (the unit renderer and the API, possibly in different processes) agree on
// one key. The unit renderer must call this before it renders supavise-pgmeta, and the
// API calls it on first use. It never falls back to an ephemeral key: when the
// registry cannot hold the key it returns an error.
func EnsurePGMetaCryptoKey(ctx context.Context, reg registry.Registry, sec secrets.Secrets) (string, error) {
	ensureKeyMu.Lock()
	defer ensureKeyMu.Unlock()
	open := func() (string, error) {
		sealed, err := reg.GetSecret(ctx, config.SystemRef, SecretPGMetaCryptoKey)
		if err != nil {
			return "", err
		}
		plain, err := sec.Open(sealed)
		if err != nil {
			return "", fmt.Errorf("api: open %s: %w", SecretPGMetaCryptoKey, err)
		}
		return string(plain), nil
	}
	key, err := open()
	if !errors.Is(err, registry.ErrNotFound) {
		return key, err
	}
	sealed, err := sec.Seal([]byte(secrets.NewPassword()))
	if err != nil {
		return "", err
	}
	if pg, ok := reg.(interface{ Pool() *pgxpool.Pool }); ok {
		_, err = pg.Pool().Exec(ctx, `insert into supavise.project_secrets (ref, name, ciphertext) values ($1, $2, $3) on conflict (ref, name) do nothing`,
			config.SystemRef, SecretPGMetaCryptoKey, sealed)
	} else {
		err = reg.PutSecret(ctx, config.SystemRef, SecretPGMetaCryptoKey, sealed)
	}
	if err != nil {
		return "", fmt.Errorf("api: store %s: %w", SecretPGMetaCryptoKey, err)
	}
	return open()
}

// pgmetaKey returns the passphrase of the x-connection-encrypted header: [api]
// pgmeta_crypto_key when set, else the registry's (see EnsurePGMetaCryptoKey).
func (s *Server) pgmetaKey(ctx context.Context) (string, error) {
	s.pgmetaKeyMu <- struct{}{}
	defer func() { <-s.pgmetaKeyMu }()
	if s.pgmetaKeyCache != "" {
		return s.pgmetaKeyCache, nil
	}
	if k := s.cfg.API.PGMetaCryptoKey; k != "" {
		s.pgmetaKeyCache = k
		return k, nil
	}
	key, err := EnsurePGMetaCryptoKey(ctx, s.reg, s.sec)
	if err != nil {
		return "", err
	}
	s.pgmetaKeyCache = key
	return key, nil
}

// pgmetaConn builds the x-connection-encrypted header value for ref's database as
// role (see Server.dsn for the roles and readOnly).
func (s *Server) pgmetaConn(ctx context.Context, ref, role string, readOnly bool) (string, error) {
	dsn, err := s.dsn(ctx, ref, role, readOnly)
	if err != nil {
		return "", err
	}
	key, err := s.pgmetaKey(ctx)
	if err != nil {
		return "", err
	}
	return cryptojs.Encrypt(dsn, key)
}

// pgmetaDo sends one request to supavise-pgmeta against ref's database.
func (s *Server) pgmetaDo(ctx context.Context, ref, role string, readOnly bool, method, path, rawQuery string, body io.Reader, hdr http.Header) (*http.Response, error) {
	conn, err := s.pgmetaConn(ctx, ref, role, readOnly)
	if err != nil {
		return nil, err
	}
	u := s.pgmetaURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for _, h := range []string{"Content-Type", "X-Pg-Application-Name", "Accept"} {
		if v := hdr.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("X-Connection-Encrypted", conn)
	resp, err := s.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.log.Error("pg-meta unreachable", "err", err)
		return nil, errf(http.StatusServiceUnavailable, "Database metadata service is unavailable")
	}
	return resp, nil
}

// pgmetaErrorMessage extracts the message of a pg-meta error body.
func pgmetaErrorMessage(b []byte, status int) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Error != "" {
			return e.Error
		}
		if e.Message != "" {
			return e.Message
		}
	}
	if m := strings.TrimSpace(string(b)); m != "" {
		return m
	}
	return http.StatusText(status)
}

// sqlRows runs query in ref's database through pg-meta and returns the result rows
// as a JSON array (the rows of the last statement).
func (s *Server) sqlRows(ctx context.Context, ref, role string, readOnly bool, query string) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]string{"query": query})
	resp, err := s.pgmetaDo(ctx, ref, role, readOnly, http.MethodPost, "/query", "", bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		status := resp.StatusCode
		if status >= 500 {
			// pg-meta reports SQL errors as 400; 5xx means pg-meta itself failed.
			// The body can echo the statement, so log a short prefix of the message only.
			s.log.Error("pg-meta error", "status", status, "message", truncate(pgmetaErrorMessage(b, status), 120))
			return nil, errf(http.StatusBadGateway, "Database metadata service failed: %s", pgmetaErrorMessage(b, status))
		}
		return nil, errf(status, "%s", pgmetaErrorMessage(b, status))
	}
	return b, nil
}

// pgmetaProxy forwards /platform/pg-meta/{ref}/<path> to supavise-pgmeta, replacing the
// connection header Studio sends with one built here, and relays the answer
// verbatim: Studio reads pg-meta's own error format.
//
// It connects as postgres, not supabase_admin like upstream's self-hosted Studio
// does: objects created in the SQL editor then belong to postgres, the role the CLI
// and migrations use, and can be altered or dropped by them. supabase_admin is used
// only where a superuser is needed (login roles, the read-only role).
func (s *Server) pgmetaProxy(w http.ResponseWriter, r *http.Request) error {
	ref := r.PathValue("ref")
	if _, err := s.running(r.Context(), ref); err != nil {
		return err
	}
	rest := strings.TrimPrefix(r.URL.Path, "/platform/pg-meta/"+ref)
	if rest == "" {
		rest = "/"
	}
	// A caller who may only query (the Read-only role) has every statement run as the
	// read-only database role, which cannot change anything whatever the SQL says.
	role, err := s.sqlRole(r, ref)
	if err != nil {
		return err
	}
	body := http.MaxBytesReader(w, r.Body, maxBody)
	resp, err := s.pgmetaDo(r.Context(), ref, role, false, r.Method, rest, r.URL.RawQuery, body, r.Header)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// sqlParams runs a parameterized query (the Management API's "parameters" array)
// over a direct connection with the extended protocol, which pg-meta cannot do.
// Rows are serialized by Postgres itself (json_agg), like the pg-meta path returns
// them. Statements that cannot sit in a CTE (DDL, INSERT without RETURNING, SHOW,
// EXPLAIN) run unwrapped; their rows, if any, are marshaled from the driver's values.
func (s *Server) sqlParams(ctx context.Context, ref, role string, readOnly bool, query string, params []any) (json.RawMessage, error) {
	dsn, err := s.dsn(ctx, ref, role, readOnly)
	if err != nil {
		return nil, err
	}
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("api: connection string: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		s.log.Error("project database unreachable", "ref", ref, "err", err)
		return nil, errf(http.StatusServiceUnavailable, "Project database is unavailable")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = pgArg(p)
	}
	var out string
	// A trailing semicolon or comment would end the CTE early (syntax error).
	wrapped := "with supavise_q as (" + trimStatementEnd(query) + "\n) select coalesce(json_agg(supavise_q), '[]'::json)::text from supavise_q"
	err = conn.QueryRow(ctx, wrapped, args...).Scan(&out)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42601" || pgErr.Code == "0A000") { // not a row-returning statement
		var res json.RawMessage
		if res, err = queryUnwrapped(ctx, conn, query, args); err == nil {
			return res, nil
		}
	}
	if err != nil {
		if errors.As(err, &pgErr) {
			return nil, errf(http.StatusBadRequest, "ERROR: %s: %s", pgErr.Code, pgErr.Message)
		}
		return nil, err
	}
	return json.RawMessage(out), nil
}

// queryUnwrapped runs query as is and returns its rows (if the command produces
// any) as a JSON array of objects.
func queryUnwrapped(ctx context.Context, conn *pgx.Conn, query string, args []any) (json.RawMessage, error) {
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	list := []map[string]any{}
	for rows.Next() {
		if len(fds) == 0 {
			continue
		}
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(fds))
		for i, fd := range fds {
			row[fd.Name] = vals[i]
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(list)
}

// trimStatementEnd removes trailing whitespace, comments and semicolons from a
// single SQL statement so it can be embedded in a CTE. It scans string literals
// (including E'..' and dollar-quoted ones), quoted identifiers and comments, so a
// semicolon or "--" inside them is left alone.
func trimStatementEnd(q string) string {
	for {
		end, ok := lastSignificant(q)
		if !ok { // ends inside a string or comment: leave it for Postgres to reject
			return q
		}
		if end > 0 && q[end-1] == ';' {
			q = q[:end-1]
			continue
		}
		return q[:end]
	}
}

// lastSignificant returns the offset just past the last byte of q that is neither
// whitespace nor inside a comment. ok is false when q ends inside an unterminated
// string, quoted identifier, dollar quote or block comment.
func lastSignificant(q string) (end int, ok bool) {
	last := 0
	i, n := 0, len(q)
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
	}
	for i < n {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && i+1 < n && q[i+1] == '-':
			for i < n && q[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && q[i+1] == '*':
			depth := 0
			for i < n {
				switch {
				case q[i] == '/' && i+1 < n && q[i+1] == '*':
					depth++
					i += 2
				case q[i] == '*' && i+1 < n && q[i+1] == '/':
					depth--
					i += 2
				default:
					i++
				}
				if depth == 0 {
					break
				}
			}
			if depth != 0 {
				return last, false
			}
		case c == '\'':
			esc := i > 0 && (q[i-1] == 'e' || q[i-1] == 'E') && (i < 2 || !isIdent(q[i-2]))
			i++
			closed := false
			for i < n {
				if esc && q[i] == '\\' {
					i += 2
					continue
				}
				if q[i] == '\'' {
					if i+1 < n && q[i+1] == '\'' {
						i += 2
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return last, false
			}
			i++
			last = i
		case c == '"':
			i++
			closed := false
			for i < n {
				if q[i] == '"' {
					if i+1 < n && q[i+1] == '"' {
						i += 2
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return last, false
			}
			i++
			last = i
		case c == '$':
			// Dollar quote: $tag$ ... $tag$ (tag may be empty). Otherwise a parameter or operator char.
			j := i + 1
			for j < n && (isIdent(q[j]) && q[j] != '$') {
				j++
			}
			if j < n && q[j] == '$' && (j == i+1 || !(q[i+1] >= '0' && q[i+1] <= '9')) {
				tag := q[i : j+1]
				if k := strings.Index(q[j+1:], tag); k >= 0 {
					i = j + 1 + k + len(tag)
				} else {
					return last, false
				}
				last = i
			} else {
				i++
				last = i
			}
		default:
			i++
			last = i
		}
	}
	return last, true
}

// pgArg converts a decoded JSON value to a driver argument: integral numbers become
// int64 so integer parameters bind, objects and arrays become JSON text.
func pgArg(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return v
}
