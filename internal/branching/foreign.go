package branching

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A copy of the parent's data carries its foreign servers and the user mappings that hold
// their passwords. A branch with denied egress cannot reach a remote server, but a server whose
// host is loopback reaches another project on this node (or the parent itself), as whoever the
// stored password says, and the loopback allow cannot tell a port from a port. So on a branch that
// does not have the egress opt-out, every foreign server that speaks the Postgres protocol is made
// unusable and every stored password is dropped, loopback or not (a remote target would be
// blocked anyway, and which is which is not worth a mistake):
//
//   - postgres_fdw and dblink_fdw servers, and any other server that carries a libpq connection
//     option (host, hostaddr, port, dbname, service), which dblink can use by name: host is set
//     to a unix socket path that does not exist (no network, no DNS: a hostname would be resolved
//     through the stub resolver on loopback), hostaddr and service are removed;
//   - every user mapping with a password option (password, sslpassword, passwd) loses it, for
//     every foreign data wrapper;
//   - connection strings written as literals in the commands of pg_cron jobs are replaced by a
//     disabled one (cron jobs may also be inactive: the jobs are neutralized either way).
//
// What was done is recorded in supavise_branch.paused_foreign_servers: names, the host, port and
// plain database name the server had, and the roles whose mapping lost a password. The
// passwords themselves are NOT recorded anywhere: they are credentials, and the branch's
// owner, who can read the table, may be an untrusted agent. Turning a server back on means
// setting its options again and re-entering the password in the user mapping. Original cron
// commands are not recorded either (they may hold a password); the jobs whose command was
// changed are listed in supavise_branch.neutralized_cron_commands.

// disabledHost is a libpq host that cannot connect: a unix socket directory that does not exist.
const disabledHost = "/nonexistent/supavise-branch-disabled"

// PausedForeignTable lists the foreign servers that were disabled for the branch.
const PausedForeignTable = "supavise_branch.paused_foreign_servers"

// NeutralizedCronTable lists the pg_cron jobs whose command had a connection string replaced.
const NeutralizedCronTable = "supavise_branch.neutralized_cron_commands"

var passwordOptions = map[string]bool{"password": true, "sslpassword": true, "passwd": true}

var libpqOptions = map[string]bool{"host": true, "hostaddr": true, "port": true, "dbname": true, "service": true}

// foreignOptions parses a text[] of "name=value" options.
func foreignOptions(opts []string) map[string]string {
	m := map[string]string{}
	for _, o := range opts {
		k, v, _ := strings.Cut(o, "=")
		m[strings.ToLower(k)] = v
	}
	return m
}

// speaksPostgres reports whether a foreign server could open a Postgres connection: it is a
// postgres_fdw or dblink_fdw server, or it carries libpq connection options (dblink_connect
// accepts any server by name and reads those).
func speaksPostgres(fdw string, opts map[string]string) bool {
	if fdw == "postgres_fdw" || fdw == "dblink_fdw" {
		return true
	}
	for k := range opts {
		if libpqOptions[k] {
			return true
		}
	}
	return false
}

// plainConnValue is the value to record for a connection option, "" when it could be a connection
// string with credentials in it (libpq reads a dbname with an = sign or a URI as one).
func plainConnValue(v string) string {
	if strings.ContainsAny(v, "=") || strings.Contains(v, "://") {
		return ""
	}
	return v
}

type foreignServer struct {
	name, fdw string
	opts      map[string]string
}

type userMapping struct {
	server, user string
	public       bool
	opts         []string // name=value, as stored
}

// passwordMappingOptions returns the names of the password options of a mapping.
func passwordMappingOptions(opts []string) []string {
	var names []string
	for k := range foreignOptions(opts) {
		if passwordOptions[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

// neutralizeForeign disables the foreign servers and user mappings of the database c is connected
// to, in one transaction, and records them. Running it again changes and records nothing new.
func neutralizeForeign(ctx context.Context, c *pgx.Conn, res *IsolateResult) error {
	rows, err := c.Query(ctx, `select s.srvname, w.fdwname, coalesce(s.srvoptions, '{}'::text[])
		from pg_foreign_server s join pg_foreign_data_wrapper w on w.oid = s.srvfdw order by s.srvname`)
	if err != nil {
		return err
	}
	var servers []foreignServer
	for rows.Next() {
		var fs foreignServer
		var opts []string
		if err := rows.Scan(&fs.name, &fs.fdw, &opts); err != nil {
			rows.Close()
			return err
		}
		fs.opts = foreignOptions(opts)
		servers = append(servers, fs)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(servers) == 0 {
		return nil
	}
	mrows, err := c.Query(ctx, `select srvname, usename, umuser = 0, coalesce(umoptions, '{}'::text[])
		from pg_user_mappings order by srvname, usename`)
	if err != nil {
		return err
	}
	byServer := map[string][]userMapping{}
	for mrows.Next() {
		var m userMapping
		if err := mrows.Scan(&m.server, &m.user, &m.public, &m.opts); err != nil {
			mrows.Close()
			return err
		}
		byServer[m.server] = append(byServer[m.server], m)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return err
	}

	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tableReady := false
	for _, s := range servers {
		connects := speaksPostgres(s.fdw, s.opts)
		hostOff := s.opts["host"] == disabledHost && s.opts["hostaddr"] == "" && s.opts["service"] == ""
		var withPassword []userMapping
		for _, m := range byServer[s.name] {
			if len(passwordMappingOptions(m.opts)) > 0 {
				withPassword = append(withPassword, m)
			}
		}
		needServer := connects && !hostOff
		if !needServer && len(withPassword) == 0 {
			continue
		}
		// A server of postgres_fdw or dblink_fdw must be neutralized or the branch is not isolated;
		// another wrapper's validator may refuse the change, which is recorded, not fatal.
		hard := s.fdw == "postgres_fdw" || s.fdw == "dblink_fdw"
		var users []string
		var note []string
		change := func(sp pgx.Tx) error {
			if needServer {
				verb := "add"
				if _, ok := s.opts["host"]; ok {
					verb = "set"
				}
				clauses := []string{fmt.Sprintf("%s host '%s'", verb, disabledHost)}
				for _, k := range []string{"hostaddr", "service"} {
					if _, ok := s.opts[k]; ok {
						clauses = append(clauses, "drop "+k)
					}
				}
				var stmt string
				if err := sp.QueryRow(ctx, `select format('alter server %I options (%s)', $1::text, $2::text)`, s.name, strings.Join(clauses, ", ")).Scan(&stmt); err != nil {
					return err
				}
				if _, err := sp.Exec(ctx, stmt); err != nil {
					return fmt.Errorf("alter server %s: %w", s.name, err)
				}
			}
			for _, m := range withPassword {
				var drops []string
				for _, k := range passwordMappingOptions(m.opts) {
					drops = append(drops, "drop "+k)
				}
				var stmt string
				if m.public {
					err := sp.QueryRow(ctx, `select format('alter user mapping for public server %I options (%s)', $1::text, $2::text)`, s.name, strings.Join(drops, ", ")).Scan(&stmt)
					if err != nil {
						return err
					}
				} else if err := sp.QueryRow(ctx, `select format('alter user mapping for %I server %I options (%s)', $1::text, $2::text, $3::text)`, m.user, s.name, strings.Join(drops, ", ")).Scan(&stmt); err != nil {
					return err
				}
				if _, err := sp.Exec(ctx, stmt); err != nil {
					return fmt.Errorf("drop the password of the user mapping %s on %s: %w", m.user, s.name, err)
				}
			}
			return nil
		}
		for _, m := range withPassword {
			users = append(users, m.user)
		}
		if !tableReady {
			for _, stmt := range []string{
				`create schema if not exists supavise_branch`,
				`create table if not exists ` + PausedForeignTable + ` (
					server_name text primary key,
					fdw_name text not null,
					original_host text,
					original_hostaddr text,
					original_port text,
					original_dbname text,
					passwords_dropped_for text[] not null default '{}',
					note text,
					paused_at timestamptz not null default now())`,
			} {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return err
				}
			}
			tableReady = true
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := change(sp); err != nil {
			_ = sp.Rollback(ctx)
			if hard {
				return err
			}
			note = append(note, "NOT neutralized: "+err.Error())
			res.ForeignNotNeutralized = append(res.ForeignNotNeutralized, s.name)
		} else if err := sp.Commit(ctx); err != nil {
			return err
		} else {
			if needServer {
				res.ForeignServers++
			}
			res.MappingPasswords += len(withPassword)
		}
		if len(note) == 0 && needServer && !hard {
			note = append(note, "not a postgres_fdw or dblink_fdw server; disabled because it carries libpq connection options")
		}
		if users == nil {
			users = []string{}
		}
		if _, err := tx.Exec(ctx, `insert into `+PausedForeignTable+`
			(server_name, fdw_name, original_host, original_hostaddr, original_port, original_dbname, passwords_dropped_for, note)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			on conflict (server_name) do update set
				passwords_dropped_for = (select coalesce(array_agg(distinct u order by u), '{}') from unnest(`+PausedForeignTable+`.passwords_dropped_for || excluded.passwords_dropped_for) u)`,
			s.name, s.fdw, nullIfEmpty(plainConnValue(s.opts["host"])), nullIfEmpty(s.opts["hostaddr"]),
			nullIfEmpty(s.opts["port"]), nullIfEmpty(plainConnValue(s.opts["dbname"])), users, nullIfEmpty(strings.Join(note, "; "))); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// neutralizeCronCommands replaces the connection strings written as literals in the commands of
// pg_cron jobs (dblink('host=... password=...', ...)), active or not, and records the jobs it
// changed. The originals are not kept: they may hold a password.
func neutralizeCronCommands(ctx context.Context, c *pgx.Conn) (int, error) {
	rows, err := c.Query(ctx, `select jobid, coalesce(jobname, ''), command from cron.job order by jobid`)
	if err != nil {
		return 0, err
	}
	type job struct {
		id        int64
		name, cmd string
	}
	var changed []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.name, &j.cmd); err != nil {
			rows.Close()
			return 0, err
		}
		if nc, n := neutralizeConnStrings(j.cmd); n > 0 {
			j.cmd = nc
			changed = append(changed, j)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(changed) == 0 {
		return 0, nil
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		`create schema if not exists supavise_branch`,
		`create table if not exists ` + NeutralizedCronTable + ` (
			jobid bigint primary key,
			jobname text,
			neutralized_at timestamptz not null default now())`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return 0, err
		}
	}
	for _, j := range changed {
		if _, err := tx.Exec(ctx, `update cron.job set command = $2 where jobid = $1`, j.id, j.cmd); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `insert into `+NeutralizedCronTable+` (jobid, jobname) values ($1, nullif($2, '')) on conflict (jobid) do nothing`, j.id, j.name); err != nil {
			return 0, err
		}
	}
	return len(changed), tx.Commit(ctx)
}

var connStringRE = regexp.MustCompile(`(?i)(?:^|[\s,;])(?:host|hostaddr|dbname|password|service)\s*=|^\s*postgres(?:ql)?://`)

// disabledConnString is what a neutralized connection string literal becomes.
const disabledConnString = "host=" + disabledHost

// neutralizeConnStrings replaces every string literal of a SQL command that looks like a libpq
// connection string (key=value pairs with host, hostaddr, dbname, password or service, or a
// postgres:// URI) with a disabled one, and returns the new command and how many literals it
// replaced. Single-quoted (with doubled quotes and, after E, backslash escapes) and dollar-quoted literals are
// understood; comments and quoted identifiers are skipped.
func neutralizeConnStrings(cmd string) (string, int) {
	var out strings.Builder
	n := 0
	i := 0
	for i < len(cmd) {
		ch := cmd[i]
		switch {
		case ch == '-' && strings.HasPrefix(cmd[i:], "--"):
			j := strings.IndexByte(cmd[i:], '\n')
			if j < 0 {
				j = len(cmd) - i
			}
			out.WriteString(cmd[i : i+j])
			i += j
		case ch == '/' && strings.HasPrefix(cmd[i:], "/*"):
			j := strings.Index(cmd[i+2:], "*/")
			if j < 0 {
				out.WriteString(cmd[i:])
				i = len(cmd)
			} else {
				out.WriteString(cmd[i : i+2+j+2])
				i += 2 + j + 2
			}
		case ch == '"':
			j := i + 1
			for j < len(cmd) {
				if cmd[j] == '"' {
					if j+1 < len(cmd) && cmd[j+1] == '"' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(cmd))
			out.WriteString(cmd[i:end])
			i = end
		case ch == '\'':
			escape := i > 0 && (cmd[i-1] == 'E' || cmd[i-1] == 'e') && (i < 2 || !isIdentByte(cmd[i-2]))
			j := i + 1
			for j < len(cmd) {
				if escape && cmd[j] == '\\' {
					j += 2
					continue
				}
				if cmd[j] == '\'' {
					if j+1 < len(cmd) && cmd[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(cmd))
			body := cmd[i+1 : min(j, len(cmd))]
			if j < len(cmd) && connStringRE.MatchString(body) {
				out.WriteString("'" + disabledConnString + "'")
				n++
			} else {
				out.WriteString(cmd[i:end])
			}
			i = end
		case ch == '$':
			tag, ok := dollarTag(cmd[i:])
			if !ok {
				out.WriteByte(ch)
				i++
				break
			}
			bodyStart := i + len(tag)
			j := strings.Index(cmd[bodyStart:], tag)
			if j < 0 {
				out.WriteString(cmd[i:])
				i = len(cmd)
				break
			}
			body := cmd[bodyStart : bodyStart+j]
			end := bodyStart + j + len(tag)
			if connStringRE.MatchString(body) {
				out.WriteString(tag + disabledConnString + tag)
				n++
			} else {
				out.WriteString(cmd[i:end])
			}
			i = end
		default:
			out.WriteByte(ch)
			i++
		}
	}
	return out.String(), n
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// dollarTag returns the opening tag ($$ or $name$) at the start of s.
func dollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	if !(s[1] == '_' || s[1] >= 'a' && s[1] <= 'z' || s[1] >= 'A' && s[1] <= 'Z' || s[1] >= 0x80) {
		return "", false // $1 is a parameter
	}
	for j := 2; j < len(s); j++ {
		if s[j] == '$' {
			return s[:j+1], true
		}
		if !isIdentByte(s[j]) {
			return "", false
		}
	}
	return "", false
}
