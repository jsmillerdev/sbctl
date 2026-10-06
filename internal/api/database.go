package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	v1 "github.com/OWNER/sbctl/internal/api/gen/v1"
	"github.com/OWNER/sbctl/internal/secrets"
)

func (s *Server) routesDatabase(add func(string, handlerFunc)) {
	add("POST /v1/projects/{ref}/database/query", s.dbQuery(false))
	add("POST /v1/projects/{ref}/database/query/read-only", s.dbQuery(true))
	add("GET /v1/projects/{ref}/database/migrations", s.listMigrations)
	add("POST /v1/projects/{ref}/database/migrations", s.applyMigration)
	add("GET /v1/projects/{ref}/types/typescript", s.typescriptTypes)
	add("POST /v1/projects/{ref}/cli/login-role", s.createLoginRole)
	add("DELETE /v1/projects/{ref}/cli/login-role", s.deleteLoginRoles)
	add("GET /v1/projects/{ref}/advisors/security", s.advisors)
	add("GET /v1/projects/{ref}/advisors/performance", s.advisors)
	add("GET /v1/projects/{ref}/secrets", s.listSecrets)
	add("POST /v1/projects/{ref}/secrets", s.createSecrets)
	add("DELETE /v1/projects/{ref}/secrets", s.deleteSecrets)
}

// dbQuery runs SQL as the project's postgres role through pg-meta and answers with
// the rows of the last statement, as the hosted Management API does.
func (s *Server) dbQuery(readOnlyRoute bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Query      string `json:"query"`
			Parameters []any  `json:"parameters"`
			ReadOnly   bool   `json:"read_only"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if strings.TrimSpace(in.Query) == "" {
			return errf(http.StatusBadRequest, "query is required")
		}
		p, err := s.running(r.Context(), r.PathValue("ref"))
		if err != nil {
			return err
		}
		var rows json.RawMessage
		if len(in.Parameters) > 0 {
			rows, err = s.sqlParams(r.Context(), p.Ref, "postgres", readOnlyRoute || in.ReadOnly, in.Query, in.Parameters)
		} else {
			rows, err = s.sqlRows(r.Context(), p.Ref, "postgres", readOnlyRoute || in.ReadOnly, in.Query)
		}
		if err != nil {
			return err
		}
		writeRaw(w, http.StatusCreated, "application/json", rows)
		return nil
	}
}

func (s *Server) listMigrations(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	exists, err := s.sqlRows(r.Context(), p.Ref, "postgres", true, `select to_regclass('supabase_migrations.schema_migrations') is not null as e`)
	if err != nil {
		return err
	}
	var e []struct {
		E bool `json:"e"`
	}
	if json.Unmarshal(exists, &e) != nil || len(e) != 1 || !e[0].E {
		writeJSON(w, http.StatusOK, []any{})
		return nil
	}
	rows, err := s.sqlRows(r.Context(), p.Ref, "postgres", true,
		`select version, name from supabase_migrations.schema_migrations order by version`)
	if err != nil {
		return err
	}
	// Rows hold exactly the schema's fields; drop null names so the strict client
	// sees an optional field instead of null.
	var list []map[string]any
	if err := json.Unmarshal(rows, &list); err != nil {
		return err
	}
	res := make([]map[string]any, 0, len(list))
	for _, m := range list {
		item := map[string]any{"version": fmt.Sprint(m["version"])}
		if n, ok := m["name"].(string); ok && n != "" {
			item["name"] = n
		}
		res = append(res, item)
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) applyMigration(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Query    string `json:"query"`
		Name     string `json:"name"`
		Rollback string `json:"rollback"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Query) == "" {
		return errf(http.StatusBadRequest, "query is required")
	}
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	const tbl = "supabase_migrations.schema_migrations"
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		// A repeated key is a retry of a migration that already ran.
		hit, err := s.sqlRows(r.Context(), p.Ref, "postgres", true, `select to_regclass('`+tbl+`') is not null and exists (select 1 from `+tbl+` where idempotency_key = `+sqlLiteral(key)+`) as hit`)
		if err == nil {
			var h []struct {
				Hit bool `json:"hit"`
			}
			if json.Unmarshal(hit, &h) == nil && len(h) == 1 && h[0].Hit {
				w.WriteHeader(http.StatusOK)
				return nil
			}
		}
	}
	version := s.now().UTC().Format("20060102150405")
	var b strings.Builder
	b.WriteString(`create schema if not exists supabase_migrations;
create table if not exists ` + tbl + ` (version text not null primary key, statements text[], name text);
alter table ` + tbl + ` add column if not exists created_by text;
alter table ` + tbl + ` add column if not exists idempotency_key text unique;
alter table ` + tbl + ` add column if not exists rollback text[];
`)
	b.WriteString(in.Query)
	b.WriteString("\n;\n")
	b.WriteString(`insert into ` + tbl + ` (version, name, statements, rollback, idempotency_key) values (` +
		sqlLiteral(version) + `, ` + nullable(in.Name) + `, array[` + sqlLiteral(in.Query) + `], ` + nullableArray(in.Rollback) + `, ` + nullable(key) + `);`)
	if _, err := s.sqlRows(r.Context(), p.Ref, "postgres", false, b.String()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func nullable(s string) string {
	if s == "" {
		return "null"
	}
	return sqlLiteral(s)
}

func nullableArray(s string) string {
	if s == "" {
		return "null"
	}
	return "array[" + sqlLiteral(s) + "]"
}

var schemaListRe = regexp.MustCompile(`^[A-Za-z0-9_,$ -]*$`)

func (s *Server) typescriptTypes(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	schemas := r.URL.Query().Get("included_schemas")
	if schemas == "" {
		schemas = "public"
	}
	if !schemaListRe.MatchString(schemas) {
		return errf(http.StatusBadRequest, "invalid included_schemas")
	}
	resp, err := s.pgmetaDo(r.Context(), p.Ref, "supabase_admin", false, http.MethodGet, "/generators/typescript",
		"included_schemas="+strings.ReplaceAll(schemas, " ", ""), nil, http.Header{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return errf(http.StatusBadGateway, "Type generation failed: %s", pgmetaErrorMessage(b, resp.StatusCode))
	}
	writeJSON(w, http.StatusOK, &v1.TypescriptResponseOutput{Types: string(b)})
	return nil
}

// loginRoleTTL is how long a temporary CLI login role stays valid.
const loginRoleTTL = time.Hour

// createLoginRole makes a short-lived login role for `supabase db push|pull|dump`.
// The role's objects belong to postgres (ALTER ROLE ... SET role), so dropping the
// role later never fails on ownership.
func (s *Server) createLoginRole(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ReadOnly bool `json:"read_only"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	role := "cli_login_" + strings.ToLower(secrets.RandomString(8, "abcdefghijklmnopqrstuvwxyz"))
	pw := secrets.NewPassword()
	until := s.now().Add(loginRoleTTL).UTC().Format("2006-01-02 15:04:05+00")
	var b strings.Builder
	b.WriteString(dropExpiredLoginRoles)
	b.WriteString(fmt.Sprintf("create role %s login password %s valid until %s", sqlIdent(role), sqlLiteral(pw), sqlLiteral(until)))
	if in.ReadOnly {
		b.WriteString(" in role pg_read_all_data;\n")
		b.WriteString(fmt.Sprintf("alter role %s set default_transaction_read_only = on;", sqlIdent(role)))
	} else {
		b.WriteString(" in role postgres;\n")
		b.WriteString(fmt.Sprintf("alter role %s set role = postgres;", sqlIdent(role)))
	}
	if _, err := s.sqlRows(r.Context(), p.Ref, "supabase_admin", false, b.String()); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, &v1.CreateRoleResponseOutput{Password: pw, Role: role, TtlSeconds: int64(loginRoleTTL.Seconds())})
	return nil
}

const dropExpiredLoginRoles = `do $$ declare r record; begin
  for r in select rolname from pg_roles where rolname like 'cli\_login\_%' and rolvaliduntil < now() loop
    execute format('drop role %I', r.rolname);
  end loop;
end $$;
`

func (s *Server) deleteLoginRoles(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	q := `do $$ declare r record; begin
  for r in select rolname from pg_roles where rolname like 'cli\_login\_%' loop
    execute format('drop role %I', r.rolname);
  end loop;
end $$;`
	if _, err := s.sqlRows(r.Context(), p.Ref, "supabase_admin", false, q); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, &v1.DeleteRolesResponseOutput{Message: v1.DeleteRolesResponseOutputMessageOk})
	return nil
}

// advisors reports no lints: the security and performance advisors of the hosted
// platform run Supabase's splinter queries, which sbctl does not run yet.
func (s *Server) advisors(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.loadProject(r.Context(), r.PathValue("ref")); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"lints": []any{}})
	return nil
}

var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func digest(plain []byte) string {
	sum := sha256.Sum256(plain)
	return hex.EncodeToString(sum[:])
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	list, err := s.store.ListFunctionSecrets(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	out := make([]v1.SecretResponseOutput, 0, len(list))
	for _, sec := range list {
		plain, err := s.sec.Open(sec.Sealed)
		if err != nil {
			return err
		}
		at := ts(sec.UpdatedAt)
		out = append(out, v1.SecretResponseOutput{Name: sec.Name, UpdatedAt: &at, Value: digest(plain)})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) createSecrets(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	sealed := map[string][]byte{}
	for _, e := range in {
		if !secretNameRe.MatchString(e.Name) || strings.HasPrefix(strings.ToUpper(e.Name), "SUPABASE_") {
			return errf(http.StatusBadRequest, "Invalid secret name %q: letters, digits and underscores, not starting with SUPABASE_", e.Name)
		}
		b, err := s.sec.Seal([]byte(e.Value))
		if err != nil {
			return err
		}
		sealed[e.Name] = b
	}
	if err := s.store.PutFunctionSecrets(r.Context(), p.Ref, sealed); err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (s *Server) deleteSecrets(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var names []string
	if err := decode(r, &names); err != nil {
		return err
	}
	if err := s.store.DeleteFunctionSecrets(r.Context(), p.Ref, names); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
