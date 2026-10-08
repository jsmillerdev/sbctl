package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// A Postgres upgrade swaps the binaries under an existing data directory, and the extensions
// created in the project's databases stay as they are: their catalog rows name a version, and
// their functions name a shared library. A release that no longer ships that library, or drops
// the SQL of that version, leaves the extension broken while the cluster itself starts and
// answers. Three checks stand in the way:
//
//   - planning (eligibility) and again before anything is touched: every installed extension
//     must exist in the target release with its library and a path to its default version
//     (CheckExtensionFiles);
//   - after the new cluster starts and before the upgrade is recorded: the library of every
//     function an extension owns must load and export that function (VerifyExtensions);
//     a failure rolls the upgrade back.
//
// An upgrade never runs ALTER EXTENSION UPDATE: an extension keeps its version, and the owner
// updates it afterwards, once the project is on the new release. The base backup taken first is
// the way back.

// InstalledExtension is one extension created in one database of a project's cluster.
type InstalledExtension struct {
	Database string
	Name     string
	Version  string
}

// ExtensionProblem is an installed extension that a Postgres release cannot serve.
type ExtensionProblem struct {
	Name      string
	Version   string
	Databases []string
	Reason    string
}

func (p ExtensionProblem) String() string {
	return fmt.Sprintf("%s %s (%s): %s", p.Name, p.Version, strings.Join(p.Databases, ", "), p.Reason)
}

// ExtensionInspector is the optional Plane capability behind the extension checks of an upgrade
// that changes the Postgres release (PostgresPlane has it; a plane without it skips them).
type ExtensionInspector interface {
	// Extensions lists the extensions created in every database of p's running cluster.
	Extensions(ctx context.Context, p *registry.Project) ([]InstalledExtension, error)
	// VerifyExtensions loads the library of every function the installed extensions own and
	// returns an error naming those that fail.
	VerifyExtensions(ctx context.Context, p *registry.Project) error
}

// extensionDirs and libDirs are where a Postgres artifact keeps control and script files, and
// shared libraries.
var (
	extensionDirs = []string{"share/postgresql/extension", "share/extension"}
	libDirs       = []string{"lib", "lib/postgresql"}
	libSuffixes   = []string{".so", ".dylib"}
)

// CheckExtensionFiles checks installed against the Postgres artifact at art, from its files. For
// each extension it needs the control file, a library for the version (module_pathname, or the
// versioned name <extension>-<version> that extensions built without one use), and the version's
// own script or a chain of update scripts from it to the release's default version. Problems are
// merged across databases and sorted by extension.
func CheckExtensionFiles(art string, installed []InstalledExtension) []ExtensionProblem {
	extDir := firstDir(art, extensionDirs)
	libs := libNames(art)
	scripts := scanScripts(extDir)
	found := map[string]*ExtensionProblem{}
	for _, x := range installed {
		reason := extensionReason(extDir, art, libs, scripts, x)
		if reason == "" {
			continue
		}
		key := x.Name + "\x00" + x.Version
		pr := found[key]
		if pr == nil {
			pr = &ExtensionProblem{Name: x.Name, Version: x.Version, Reason: reason}
			found[key] = pr
		}
		pr.Databases = append(pr.Databases, x.Database)
	}
	out := make([]ExtensionProblem, 0, len(found))
	for _, pr := range found {
		sort.Strings(pr.Databases)
		out = append(out, *pr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

func firstDir(art string, rel []string) string {
	for _, r := range rel {
		if fi, err := os.Stat(filepath.Join(art, r)); err == nil && fi.IsDir() {
			return filepath.Join(art, r)
		}
	}
	return filepath.Join(art, rel[0])
}

// libNames returns the shared libraries of the artifact by name without the suffix.
func libNames(art string) map[string]bool {
	out := map[string]bool{}
	for _, d := range libDirs {
		es, err := os.ReadDir(filepath.Join(art, d))
		if err != nil {
			continue
		}
		for _, e := range es {
			for _, suf := range libSuffixes {
				if strings.HasSuffix(e.Name(), suf) {
					out[strings.TrimSuffix(e.Name(), suf)] = true
				}
			}
		}
	}
	return out
}

// extScripts are the SQL scripts of one extension: full[v] is an install script of version v,
// edges[a] the versions an update script moves a to.
type extScripts struct {
	full  map[string]bool
	edges map[string][]string
}

func scanScripts(dir string) map[string]*extScripts {
	out := map[string]*extScripts{}
	es, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range es {
		base, ok := strings.CutSuffix(e.Name(), ".sql")
		if !ok {
			continue
		}
		parts := strings.Split(base, "--")
		if len(parts) < 2 || len(parts) > 3 {
			continue
		}
		s := out[parts[0]]
		if s == nil {
			s = &extScripts{full: map[string]bool{}, edges: map[string][]string{}}
			out[parts[0]] = s
		}
		if len(parts) == 2 {
			s.full[parts[1]] = true
		} else {
			s.edges[parts[1]] = append(s.edges[parts[1]], parts[2])
		}
	}
	return out
}

// reaches reports whether a chain of update scripts leads from version from to version to.
func (s *extScripts) reaches(from, to string) bool {
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if v == to {
			return true
		}
		for _, next := range s.edges[v] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// parseControl reads the key = 'value' lines of an extension control file.
func parseControl(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "'\"")
	}
	return out
}

func extensionReason(extDir, art string, libs map[string]bool, scripts map[string]*extScripts, x InstalledExtension) string {
	raw, err := os.ReadFile(filepath.Join(extDir, x.Name+".control"))
	if err != nil {
		return "the target release does not ship this extension"
	}
	ctl := parseControl(raw)
	def := ctl["default_version"]
	if s := scripts[x.Name]; x.Version != def && !(s != nil && (s.full[x.Version] || s.reaches(x.Version, def))) {
		return fmt.Sprintf("the target release has no script for version %s and no update path from it to %s; run ALTER EXTENSION %s UPDATE while the project runs its current release", x.Version, def, x.Name)
	}
	if mp := ctl["module_pathname"]; mp != "" {
		if filepath.IsAbs(mp) {
			return ""
		}
		lib := strings.TrimPrefix(mp, "$libdir/")
		if !libs[lib] {
			return fmt.Sprintf("the target release does not ship the library %s", lib)
		}
		return ""
	}
	// Without module_pathname an extension is either SQL only or built in "versioned shared-object
	// mode", where its functions name a library with the version in its file name.
	versioned := false
	for l := range libs {
		if strings.HasPrefix(l, x.Name+"-") {
			versioned = true
			break
		}
	}
	if versioned && !libs[x.Name+"-"+x.Version] {
		return fmt.Sprintf("the target release does not ship the library %s-%s that this version uses", x.Name, x.Version)
	}
	return ""
}

// postgresArtifactDir finds the Postgres artifact of tag: the node's pin, or by tag.
func (e *Engine) postgresArtifactDir(tag string) (string, error) {
	if pin, err := e.arts.Tag(config.SvcPostgres); err == nil && pin == tag {
		return e.arts.Dir(config.SvcPostgres)
	}
	if ta, ok := e.arts.(TagArtifacts); ok {
		return ta.DirFor(config.SvcPostgres, tag)
	}
	return "", fmt.Errorf("the artifact store cannot find %s by tag", tag)
}

// extensionProblems checks the extensions installed in p's databases against the Postgres release
// in to. Nothing to check (the release stays, or the plane cannot list extensions) returns nil.
// An error means the check could not be made: planning notes it, and the upgrade itself fails
// closed on it.
func (e *Engine) extensionProblems(ctx context.Context, p *registry.Project, to map[string]string) ([]ExtensionProblem, error) {
	cur, err := e.EffectiveVersions(p)
	if err != nil {
		return nil, err
	}
	tag := to[config.SvcPostgres]
	if tag == "" || tag == cur[config.SvcPostgres] {
		return nil, nil
	}
	insp, ok := e.plane.(ExtensionInspector)
	if !ok {
		return nil, nil
	}
	art, err := e.postgresArtifactDir(tag)
	if err != nil {
		return nil, fmt.Errorf("the PostgreSQL release %s is not on disk yet: %w", ShortVersion(config.SvcPostgres, tag), err)
	}
	installed, err := insp.Extensions(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("list the installed extensions: %w", err)
	}
	return CheckExtensionFiles(art, installed), nil
}

func describeProblems(ps []ExtensionProblem) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return strings.Join(parts, "; ")
}

// Extensions implements ExtensionInspector.
func (pl *PostgresPlane) Extensions(ctx context.Context, p *registry.Project) ([]InstalledExtension, error) {
	var out []InstalledExtension
	err := pl.eachDatabase(ctx, p, func(ctx context.Context, db string, c *pgx.Conn) error {
		rows, err := c.Query(ctx, `select extname, extversion from pg_extension order by 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			x := InstalledExtension{Database: db}
			if err := rows.Scan(&x.Name, &x.Version); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

// VerifyExtensions implements ExtensionInspector. fmgr_c_validator is the validator Postgres
// runs when a C function is created: it loads the function's library and looks up its symbol, so
// a library the release dropped, one built for another server version and a function the new
// library no longer exports all fail here. (LOAD would not do: pg_cron and pg_net refuse to be
// loaded outside shared_preload_libraries, and a loaded library says nothing about its symbols.)
func (pl *PostgresPlane) VerifyExtensions(ctx context.Context, p *registry.Project) error {
	var bad []string
	err := pl.eachDatabase(ctx, p, func(ctx context.Context, db string, c *pgx.Conn) error {
		rows, err := c.Query(ctx, `select e.extname, p.proname, p.oid::int8
			from pg_proc p
			join pg_language l on l.oid = p.prolang and l.lanname = 'c'
			join pg_depend d on d.classid = 'pg_proc'::regclass and d.objid = p.oid and d.deptype = 'e'
			join pg_extension e on e.oid = d.refobjid
			order by 1, 2`)
		if err != nil {
			return err
		}
		type fn struct {
			ext, name string
			oid       int64
		}
		var fns []fn
		for rows.Next() {
			var f fn
			if err := rows.Scan(&f.ext, &f.name, &f.oid); err != nil {
				rows.Close()
				return err
			}
			fns = append(fns, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		failed := map[string]bool{}
		for _, f := range fns {
			if failed[f.ext] {
				continue
			}
			if _, err := c.Exec(ctx, `select pg_catalog.fmgr_c_validator($1::int8::oid)`, f.oid); err != nil {
				failed[f.ext] = true
				bad = append(bad, fmt.Sprintf("%s in %s: %s: %v", f.ext, db, f.name, err))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("extension code does not load on the new release: %s", strings.Join(bad, "; "))
	}
	return nil
}

// eachDatabase runs f on a connection to every database of p's cluster that accepts
// connections, as the superuser the cluster trusts on its socket.
func (pl *PostgresPlane) eachDatabase(ctx context.Context, p *registry.Project, f func(ctx context.Context, db string, c *pgx.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pp := pl.paths(p)
	c, err := connect(ctx, socketDSN(pp, "postgres"))
	if err != nil {
		return err
	}
	rows, err := c.Query(ctx, `select datname from pg_database where datallowconn and not datistemplate order by 1`)
	if err != nil {
		_ = c.Close(context.WithoutCancel(ctx))
		return err
	}
	var dbs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			_ = c.Close(context.WithoutCancel(ctx))
			return err
		}
		dbs = append(dbs, d)
	}
	rows.Close()
	_ = c.Close(context.WithoutCancel(ctx))
	if err := rows.Err(); err != nil {
		return err
	}
	for _, db := range dbs {
		dc, err := connect(ctx, socketDSN(pp, db))
		if err != nil {
			return fmt.Errorf("database %s: %w", db, err)
		}
		err = f(ctx, db, dc)
		_ = dc.Close(context.WithoutCancel(ctx))
		if err != nil {
			return fmt.Errorf("database %s: %w", db, err)
		}
	}
	return nil
}
