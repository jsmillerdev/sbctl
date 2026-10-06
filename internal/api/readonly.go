package api

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
	"strings"
	"time"
)

// roleReadOnly is the login role behind read-only SQL (POST .../database/query
// with read_only, and .../database/query/read-only). Its only privileges are those
// of pg_read_all_data, and it is not a member of postgres, so no statement the
// client sends, `begin read write` and `reset role` included, can change data.
// default_transaction_read_only is set on top as a second layer, which a client may
// override but which grants nothing.
const roleReadOnly = "sbctl_read_only"

// readOnlyEnsureTTL is how long a successful ensureReadOnlyRole is trusted. The role
// lives in the project's database, so a restore or a rebuilt data directory can
// remove it; the next call after the TTL recreates it.
const readOnlyEnsureTTL = 30 * time.Second

type readOnlyEnsured struct {
	password string
	at       time.Time
}

// readOnlyPassword derives the role's password from the project's admin password:
// no extra secret to store, and it follows a rotation of that password.
func (s *Server) readOnlyPassword(ctx context.Context, ref string) (string, error) {
	k, err := s.mgr.Keys(ctx, ref)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(k.AdminPassword))
	mac.Write([]byte("sbctl-api-read-only-role"))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// ensureReadOnlyRole creates roleReadOnly in ref's database if it is missing and
// sets its password, then returns the password.
func (s *Server) ensureReadOnlyRole(ctx context.Context, ref string) (string, error) {
	pw, err := s.readOnlyPassword(ctx, ref)
	if err != nil {
		return "", err
	}
	s.roMu.Lock()
	defer s.roMu.Unlock()
	if e, ok := s.roEnsured[ref]; ok && e.password == pw && s.now().Sub(e.at) < readOnlyEnsureTTL {
		return pw, nil
	}
	verifier, err := scramVerifier(pw)
	if err != nil {
		return "", err
	}
	q := `do $$ begin
  if not exists (select from pg_roles where rolname = ` + sqlLiteral(roleReadOnly) + `) then
    begin
      create role ` + sqlIdent(roleReadOnly) + ` login nosuperuser nocreatedb nocreaterole noreplication nobypassrls connection limit 20 in role pg_read_all_data;
    exception when duplicate_object then null;
    end;
  end if;
end $$;
alter role ` + sqlIdent(roleReadOnly) + ` login password ` + sqlLiteral(verifier) + `;
alter role ` + sqlIdent(roleReadOnly) + ` set default_transaction_read_only = on;`
	if _, err := s.sqlRows(ctx, ref, "supabase_admin", false, q); err != nil {
		return "", err
	}
	s.roEnsured[ref] = readOnlyEnsured{password: pw, at: s.now()}
	return pw, nil
}

// dsn returns the connection string for ref's database as role. role may be
// "postgres", "supabase_admin" (both from the Manager) or roleReadOnly. readOnly
// adds default_transaction_read_only=on to the session, a safety net for the
// server's own read queries; it is not a security boundary (use roleReadOnly).
func (s *Server) dsn(ctx context.Context, ref, role string, readOnly bool) (string, error) {
	managed := role
	if role == roleReadOnly {
		managed, readOnly = "postgres", true
	}
	dsn, err := s.mgr.ConnString(ctx, ref, managed)
	if err != nil {
		return "", err
	}
	var pw string
	if role == roleReadOnly {
		if pw, err = s.ensureReadOnlyRole(ctx, ref); err != nil {
			return "", err
		}
	}
	if !readOnly {
		return dsn, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("api: connection string: %w", err)
	}
	if role == roleReadOnly {
		u.User = url.UserPassword(roleReadOnly, pw)
	}
	q := u.Query()
	q.Set("options", "-c default_transaction_read_only=on")
	// %20, not "+": libpq-style URL parsers differ on decoding "+" in a query.
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String(), nil
}

// scramVerifier returns the SCRAM-SHA-256 verifier Postgres stores for password
// (RFC 5802 with Postgres's layout). Sending it instead of the cleartext keeps the
// password out of statement logs and error messages.
func scramVerifier(password string) (string, error) {
	const iterations = 4096
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return scramVerifierWithSalt(password, salt, iterations)
}

func scramVerifierWithSalt(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	hm := func(key []byte, msg string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(msg))
		return m.Sum(nil)
	}
	stored := sha256.Sum256(hm(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$" + strconv.Itoa(iterations) + ":" + b64(salt) + "$" + b64(stored[:]) + ":" + b64(hm(salted, "Server Key")), nil
}
