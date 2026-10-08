package registry

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// additiveFrom is the first migration number bound by invariant I6: a binary one minor release
// behind must run against a registry migrated by this release, so from 1300 on a migration may
// only add things (a table, an index, a column with a default, seed rows, a function, a trigger).
// Earlier migrations moved data and renamed columns, and a node that has applied them is
// already past them.
const additiveFrom = 1300

// TestMigrationsFrom1300AreAdditive lints every embedded migration numbered additiveFrom or higher.
func TestMigrationsFrom1300AreAdditive(t *testing.T) {
	names := MigrationNames()
	checked := 0
	for _, name := range names {
		n, err := strconv.Atoi(name[:4])
		if err != nil || n < additiveFrom {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, problem := range lintAdditive(string(body)) {
			t.Errorf("%s: %s", name, problem)
		}
	}
	if checked < 2 {
		t.Fatalf("linted %d migrations numbered %d or higher, want at least 1300 and 1301", checked, additiveFrom)
	}
}

func TestMigrationLintAcceptsAdditiveStatements(t *testing.T) {
	for name, sql := range map[string]string{
		"table":                 `create table supavise.things (id text primary key, n int not null default 0);`,
		"table2":                `create table if not exists supavise.things (id text primary key)`,
		"seed":                  `insert into supavise.things (id) values ('a;b'), ('c'); -- a ';' inside a string is not a separator`,
		"column":                `alter table supavise.projects add column node_id text not null default 'n1' references supavise.nodes (id);`,
		"nullable":              `alter table supavise.projects add column if not exists note text, add column n int default 0;`,
		"index":                 `create index things_n on supavise.things (n);`,
		"index on an old table": `create index projects_node on supavise.projects (node_id);`,
		"trigger":               `create trigger t after insert on supavise.projects for each statement execute function supavise.f();`,
		"comment":               `comment on table supavise.things is 'x';`,
		// The body of a function may update and delete: it is one statement.
		"function": `create function supavise.f() returns trigger language plpgsql as $$
			begin
			  update supavise.cluster set change_seq = change_seq + 1;
			  delete from supavise.things where false;
			  return null;
			end $$;`,
		"tagged body":   `create function supavise.g() returns int language sql as $body$ select 1; update x set y = 1 $body$;`,
		"block comment": `/* update supavise.projects set x = 1; */ create table supavise.t (id int);`,
	} {
		if got := lintAdditive(sql); len(got) != 0 {
			t.Errorf("%s: unexpected findings %v", name, got)
		}
	}
	// A unique index on a table created in the same migration cannot reject an older binary's rows.
	if got := lintAdditive(`create table supavise.t (id int); create unique index t_u on supavise.t (id);`); len(got) != 0 {
		t.Errorf("unique index on a new table: %v", got)
	}
}

func TestMigrationLintRejectsStatementsThatChangeShape(t *testing.T) {
	for name, sql := range map[string]string{
		"drop table":           `drop table supavise.backups;`,
		"drop column":          `alter table supavise.projects drop column class;`,
		"rename column":        `alter table supavise.projects rename column class to size;`,
		"rename table":         `alter table supavise.projects rename to apps;`,
		"change a type":        `alter table supavise.projects alter column seq type bigint;`,
		"set not null":         `alter table supavise.projects alter column org_id set not null;`,
		"set a default":        `alter table supavise.projects alter column class set default 'micro';`,
		"add constraint":       `alter table supavise.projects add constraint c check (seq < 10);`,
		"not null, no default": `alter table supavise.projects add column zone text not null;`,
		"one bad action":       `alter table supavise.projects add column a text default '', drop column b;`,
		"update":               `update supavise.projects set class = 'nano' where class = 'micro';`,
		"delete":               `delete from supavise.routes;`,
		"truncate":             `truncate supavise.events;`,
		"replace a function":   `create or replace function supavise.notify_change() returns trigger language plpgsql as $$ begin return null; end $$;`,
		"drop a trigger":       `drop trigger projects_notify on supavise.projects;`,
		"unique index":         `create unique index projects_name on supavise.projects (name);`,
		"a type":               `create type supavise.mood as enum ('a');`,
		"a statement after":    `create table supavise.t (id int); update supavise.t set id = 1;`,
		"unqualified table":    `create table things (id int);`,
	} {
		got := lintAdditive(sql)
		if len(got) == 0 {
			t.Errorf("%s: %q was accepted", name, sql)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	sql := "select 'a;b'; -- tail; comment\nselect $$x;y$$ ; /* c; */ select $t$ ; $t$;\n  \n"
	var got []string
	for _, s := range splitStatements(sql) {
		got = append(got, s.skeleton)
	}
	want := []string{"select ''", "select $$", "select $$"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("statements = %q, want %q", got, want)
	}
}

// ---- the linter ----------------------------------------------------------------------------

type statement struct{ text, skeleton string }

// splitStatements cuts sql at the semicolons that end a statement. Comments are dropped; the
// skeleton is the statement in lower case with single spaces and with every string literal and
// dollar-quoted body replaced by a placeholder, which is what the rules look at.
func splitStatements(sql string) []statement {
	var out []statement
	var text, skel strings.Builder
	flush := func() {
		t, s := strings.TrimSpace(text.String()), strings.Join(strings.Fields(skel.String()), " ")
		if s != "" {
			out = append(out, statement{text: strings.Join(strings.Fields(t), " "), skeleton: strings.ToLower(s)})
		}
		text.Reset()
		skel.Reset()
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '-' && strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 4
			}
			text.WriteByte(' ')
			skel.WriteByte(' ')
		case c == '\'':
			j := i + 1
			for j < len(sql) {
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			text.WriteString(sql[i:min(j+1, len(sql))])
			skel.WriteString("''")
			i = j + 1
		case c == '$':
			tag := dollarTag(sql[i:])
			if tag == "" {
				text.WriteByte(c)
				skel.WriteByte(c)
				i++
				continue
			}
			end := strings.Index(sql[i+len(tag):], tag)
			if end < 0 {
				end = len(sql) - i - len(tag)
			}
			j := min(i+len(tag)+end+len(tag), len(sql))
			text.WriteString(sql[i:j])
			skel.WriteString("$$")
			i = j
		case c == ';':
			flush()
			i++
		default:
			text.WriteByte(c)
			skel.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

var reDollarTag = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)?\$`)

// dollarTag returns the opening dollar-quote at the start of s ("$$" or "$name$"), or "".
func dollarTag(s string) string { return reDollarTag.FindString(s) }

var (
	reCreateTable    = regexp.MustCompile(`^create table (?:if not exists )?supavise\.([a-z_0-9]+)`)
	reCreateIndex    = regexp.MustCompile(`^create (unique )?index (?:concurrently )?(?:if not exists )?[a-z_0-9]+ on supavise\.([a-z_0-9]+)`)
	reCreateFunction = regexp.MustCompile(`^create function supavise\.[a-z_0-9]+ ?\(`)
	reCreateTrigger  = regexp.MustCompile(`^create trigger [a-z_0-9]+ (?:before|after) .* on supavise\.[a-z_0-9]+ `)
	reInsert         = regexp.MustCompile(`^insert into supavise\.[a-z_0-9]+`)
	reComment        = regexp.MustCompile(`^comment on `)
	reAlterTable     = regexp.MustCompile(`^alter table (?:if exists )?supavise\.[a-z_0-9]+ (.*)$`)
	reAddColumn      = regexp.MustCompile(`^add column (?:if not exists )?[a-z_0-9]+ `)
)

// lintAdditive returns what in sql an older binary could not live with. A statement is allowed
// when it adds something an older binary does not know about: a table, an index (a unique one
// only on a table created in the same file), a column that is nullable or has a default, seed
// rows, a new function, a trigger or a comment. Everything else is reported: drops, renames,
// type and constraint changes, updates and deletes of existing rows, a replaced function.
func lintAdditive(sql string) []string {
	var problems []string
	created := map[string]bool{}
	for _, st := range splitStatements(sql) {
		if msg := lintStatement(st.skeleton, created); msg != "" {
			problems = append(problems, msg+": "+shorten(st.text))
		}
	}
	return problems
}

func lintStatement(s string, created map[string]bool) string {
	switch {
	case reCreateTable.MatchString(s):
		created[reCreateTable.FindStringSubmatch(s)[1]] = true
	case reCreateIndex.MatchString(s):
		m := reCreateIndex.FindStringSubmatch(s)
		if m[1] != "" && !created[m[2]] {
			return "a unique index on an existing table can reject rows an older binary writes"
		}
	case reCreateFunction.MatchString(s), reCreateTrigger.MatchString(s), reInsert.MatchString(s), reComment.MatchString(s):
	case reAlterTable.MatchString(s):
		for _, action := range splitTopLevel(reAlterTable.FindStringSubmatch(s)[1], ',') {
			action = strings.TrimSpace(action)
			if !reAddColumn.MatchString(action) {
				return "alter table may only add columns"
			}
			if strings.Contains(action, " not null") && !strings.Contains(action, " default ") && !strings.Contains(action, " generated ") {
				return "a new not null column needs a default, or an older binary's inserts fail"
			}
		}
	default:
		return "not an additive statement (only create table, create index, add column with a default, insert, create function, create trigger, comment)"
	}
	return ""
}

// splitTopLevel splits s at sep outside parentheses.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

func shorten(s string) string {
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}
