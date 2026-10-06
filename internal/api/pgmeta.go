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
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/OWNER/sbctl/internal/api/cryptojs"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// SecretPGMetaCryptoKey is the name of the system-project secret that holds the
// passphrase shared with sb-pgmeta (its CRYPTO_KEY) when [api] pgmeta_crypto_key is
// not configured. The unit renderer must give sb-pgmeta the same value.
const SecretPGMetaCryptoKey = "pgmeta_crypto_key"

// pgmetaKey returns the passphrase of the x-connection-encrypted header.
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
	sealed, err := s.reg.GetSecret(ctx, config.SystemRef, SecretPGMetaCryptoKey)
	switch {
	case err == nil:
		plain, err := s.sec.Open(sealed)
		if err != nil {
			return "", fmt.Errorf("api: open %s: %w", SecretPGMetaCryptoKey, err)
		}
		s.pgmetaKeyCache = string(plain)
	case errors.Is(err, registry.ErrNotFound):
		key := secrets.NewPassword()
		sealed, err := s.sec.Seal([]byte(key))
		if err != nil {
			return "", err
		}
		if err := s.reg.PutSecret(ctx, config.SystemRef, SecretPGMetaCryptoKey, sealed); err != nil {
			// No system project row yet (tests, first boot): keep a process-local key.
			s.log.Warn("pg-meta crypto key not persisted; set [api] pgmeta_crypto_key", "err", err)
		}
		s.pgmetaKeyCache = key
	default:
		return "", err
	}
	return s.pgmetaKeyCache, nil
}

// pgmetaConn builds the x-connection-encrypted header value for ref's database as
// role. readOnly makes every transaction of the connection read only.
func (s *Server) pgmetaConn(ctx context.Context, ref, role string, readOnly bool) (string, error) {
	dsn, err := s.mgr.ConnString(ctx, ref, role)
	if err != nil {
		return "", err
	}
	if readOnly {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("api: connection string: %w", err)
		}
		q := u.Query()
		q.Set("options", "-c default_transaction_read_only=on")
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	key, err := s.pgmetaKey(ctx)
	if err != nil {
		return "", err
	}
	return cryptojs.Encrypt(dsn, key)
}

// pgmetaDo sends one request to sb-pgmeta against ref's database.
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
			s.log.Error("pg-meta error", "status", status, "body", string(b))
			return nil, errf(http.StatusBadGateway, "Database metadata service failed: %s", pgmetaErrorMessage(b, status))
		}
		return nil, errf(status, "%s", pgmetaErrorMessage(b, status))
	}
	return b, nil
}

// pgmetaProxy forwards /platform/pg-meta/{ref}/<path> to sb-pgmeta, replacing the
// connection header Studio sends with one built here, and relays the answer
// verbatim: Studio reads pg-meta's own error format.
func (s *Server) pgmetaProxy(w http.ResponseWriter, r *http.Request) error {
	ref := r.PathValue("ref")
	if _, err := s.running(r.Context(), ref); err != nil {
		return err
	}
	rest := strings.TrimPrefix(r.URL.Path, "/platform/pg-meta/"+ref)
	if rest == "" {
		rest = "/"
	}
	body := http.MaxBytesReader(w, r.Body, maxBody)
	resp, err := s.pgmetaDo(r.Context(), ref, "supabase_admin", false, r.Method, rest, r.URL.RawQuery, body, r.Header)
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
// them. Statements that cannot sit in a CTE (DDL) run unwrapped and answer [].
func (s *Server) sqlParams(ctx context.Context, ref, role string, readOnly bool, query string, params []any) (json.RawMessage, error) {
	dsn, err := s.mgr.ConnString(ctx, ref, role)
	if err != nil {
		return nil, err
	}
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("api: connection string: %w", err)
	}
	if readOnly {
		cc.RuntimeParams["default_transaction_read_only"] = "on"
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
	wrapped := "with sbctl_q as (" + query + "\n) select coalesce(json_agg(sbctl_q), '[]'::json)::text from sbctl_q"
	err = conn.QueryRow(ctx, wrapped, args...).Scan(&out)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42601" || pgErr.Code == "0A000") { // not a row-returning statement
		if _, err2 := conn.Exec(ctx, query, args...); err2 == nil {
			return json.RawMessage("[]"), nil
		} else {
			err = err2
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
