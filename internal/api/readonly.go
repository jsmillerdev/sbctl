package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/lifecycle"
)

// roleReadOnly is the login role behind read-only SQL (POST .../database/query
// with read_only, and .../database/query/read-only). Its only privileges are those
// of pg_read_all_data plus BYPASSRLS (so RLS tables read the same as through
// postgres), and it is not a member of postgres, so no statement the
// client sends, `begin read write` and `reset role` included, can change data.
// default_transaction_read_only is set on top as a second layer, which a client may
// override but which grants nothing.
const roleReadOnly = "sbctl_read_only"

// readOnlyEnsureTTL is how long a successful ensureReadOnlyRole is trusted. The role
// lives in the project's database, so a restore or a rebuilt data directory can
// remove it; the next call after the TTL recreates it.
const readOnlyEnsureTTL = 2 * time.Minute

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

// roleLock returns the mutex that serializes role setup for ref, so a slow or
// unreachable project never blocks read-only queries on another.
func (s *Server) roleLock(ref string) *sync.Mutex {
	s.roMu.Lock()
	defer s.roMu.Unlock()
	if s.roLocks == nil {
		s.roLocks = map[string]*sync.Mutex{}
	}
	m, ok := s.roLocks[ref]
	if !ok {
		m = &sync.Mutex{}
		s.roLocks[ref] = m
	}
	return m
}

// readOnlySalt derives the SCRAM salt from the password, so the verifier for one
// password is always the same: a repeat ALTER ROLE is a no-op rewrite and the
// state check below can compare verifiers.
func readOnlySalt(pw string) []byte {
	mac := hmac.New(sha256.New, []byte(pw))
	mac.Write([]byte("sbctl-api-read-only-salt"))
	return mac.Sum(nil)[:16]
}

// ensureReadOnlyRole creates roleReadOnly in ref's database if it is missing and
// sets its password, then returns the password. The role is BYPASSRLS, like
// upstream's supabase_read_only_user: pg_read_all_data alone does not bypass row
// level security, so without it read-only queries would silently see no rows of any
// table that has RLS. BYPASSRLS grants no write privilege.
func (s *Server) ensureReadOnlyRole(ctx context.Context, ref string) (string, error) {
	pw, err := s.readOnlyPassword(ctx, ref)
	if err != nil {
		return "", err
	}
	mu := s.roleLock(ref)
	mu.Lock()
	defer mu.Unlock()
	s.roMu.Lock()
	e, ok := s.roEnsured[ref]
	s.roMu.Unlock()
	if ok && e.password == pw && s.now().Sub(e.at) < readOnlyEnsureTTL {
		return pw, nil
	}
	verifier, err := scramVerifierWithSalt(pw, readOnlySalt(pw), 4096)
	if err != nil {
		return "", err
	}
	// Cheap check first: when the role is already right, nothing is written.
	chk := `select exists (select from pg_authid where rolname = ` + sqlLiteral(roleReadOnly) +
		` and rolcanlogin and rolbypassrls and not rolsuper and rolpassword = ` + sqlLiteral(verifier) + `) as ok`
	if out, err := s.sqlRows(ctx, ref, "supabase_admin", false, chk); err == nil {
		var rows []struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(out, &rows) == nil && len(rows) == 1 && rows[0].OK {
			s.markReadOnlyEnsured(ref, pw)
			return pw, nil
		}
	}
	q := `do $$ begin
  if not exists (select from pg_roles where rolname = ` + sqlLiteral(roleReadOnly) + `) then
    begin
      create role ` + sqlIdent(roleReadOnly) + ` login nosuperuser nocreatedb nocreaterole noreplication bypassrls connection limit 20 in role pg_read_all_data;
    exception when duplicate_object then null;
    end;
  end if;
end $$;
alter role ` + sqlIdent(roleReadOnly) + ` login bypassrls password ` + sqlLiteral(verifier) + `;
alter role ` + sqlIdent(roleReadOnly) + ` set default_transaction_read_only = on;`
	if _, err := s.sqlRows(ctx, ref, "supabase_admin", false, q); err != nil {
		return "", err
	}
	s.markReadOnlyEnsured(ref, pw)
	return pw, nil
}

func (s *Server) markReadOnlyEnsured(ref, pw string) {
	s.roMu.Lock()
	s.roEnsured[ref] = readOnlyEnsured{password: pw, at: s.now()}
	s.roMu.Unlock()
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
	return lifecycle.ScramVerifierWithSalt(password, salt, iterations)
}
