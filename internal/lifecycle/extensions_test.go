package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// fakePostgresArtifact lays out a Postgres artifact with the given control files, scripts
// (names without .sql) and libraries (names without suffix).
func fakePostgresArtifact(t *testing.T, controls map[string]string, scripts, libs []string) string {
	t.Helper()
	dir := t.TempDir()
	ext := filepath.Join(dir, "share/postgresql/extension")
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, ctl := range controls {
		write(filepath.Join(ext, name+".control"), ctl)
	}
	for _, s := range scripts {
		write(filepath.Join(ext, s+".sql"), "-- script\n")
	}
	for _, l := range libs {
		write(filepath.Join(dir, "lib", l+".so"), "")
	}
	return dir
}

var releaseControls = map[string]string{
	"pg_net":   "default_version = '0.20.4'\ncomment = 'Async HTTP'\nmodule_pathname = '$libdir/pg_net'\n",
	"pg_cron":  "default_version = '1.6.4'\nmodule_pathname = '$libdir/pg_cron'\n",
	"vector":   "default_version = '0.8.0'\nmodule_pathname = '$libdir/vector'\n",
	"wrappers": "default_version = '0.6.3'\n# commenting-out module_pathname results in versioned shared-object mode\n#module_pathname = '$libdir/wrappers'\n",
	"citext":   "default_version = '1.6'\nmodule_pathname = '$libdir/citext'\n",
	"plain":    "default_version = '1.0'\n",
}

func TestCheckExtensionFiles(t *testing.T) {
	art := fakePostgresArtifact(t, releaseControls,
		[]string{"pg_net--0.19.0--0.20.0", "pg_net--0.20.0--0.20.4", "pg_net--0.20.4", "vector--0.8.0", "vector--0.7.0--0.8.0", "wrappers--0.6.3", "citext--1.6", "plain--1.0"},
		// pg_cron's library is missing; wrappers is built in versioned mode
		[]string{"pg_net", "vector", "wrappers-0.6.3", "citext"})
	inst := func(db, name, ver string) InstalledExtension {
		return InstalledExtension{Database: db, Name: name, Version: ver}
	}
	got := CheckExtensionFiles(art, []InstalledExtension{
		inst("postgres", "pg_net", "0.19.0"),     // an update chain leads to the default
		inst("postgres", "pg_net", "0.20.4"),     // the default itself
		inst("postgres", "vector", "0.7.0"),      // one update step
		inst("postgres", "citext", "1.6"),        // fine
		inst("postgres", "plain", "1.0"),         // SQL only: no library to look for
		inst("postgres", "wrappers", "0.6.3"),    // versioned library present
		inst("postgres", "plpgsql", "1.0"),       // not in this fake release
		inst("postgres", "pg_cron", "1.6.4"),     // library gone
		inst("app", "pg_cron", "1.6.4"),          // same finding in another database
		inst("postgres", "wrappers", "0.5.0"),    // no script, no path, no library for 0.5.0
		inst("postgres", "pg_net", "0.5.0"),      // no path from 0.5.0
		inst("postgres", "pg_cron", "1.4.0"),     // another version of the same extension: a separate finding
		inst("postgres", "citext", "1.5-future"), // a version the release does not know
	})
	byKey := map[string]ExtensionProblem{}
	for _, p := range got {
		byKey[p.Name+" "+p.Version] = p
	}
	want := map[string]string{
		"plpgsql 1.0":       "does not ship this extension",
		"pg_cron 1.6.4":     "does not ship the library pg_cron",
		"pg_cron 1.4.0":     "no script for version 1.4.0",
		"wrappers 0.5.0":    "no script for version 0.5.0",
		"pg_net 0.5.0":      "no update path from it to 0.20.4",
		"citext 1.5-future": "no script for version 1.5-future",
	}
	if len(byKey) != len(want) {
		t.Fatalf("problems = %v, want %d", got, len(want))
	}
	for k, sub := range want {
		p, ok := byKey[k]
		if !ok || !strings.Contains(p.Reason, sub) {
			t.Errorf("%s: %+v, want a reason containing %q", k, p, sub)
		}
	}
	if p := byKey["pg_cron 1.6.4"]; strings.Join(p.Databases, ",") != "app,postgres" {
		t.Errorf("databases of pg_cron 1.6.4 = %v, want merged and sorted", p.Databases)
	}
	// A versioned-mode extension at its default version needs its versioned library.
	art2 := fakePostgresArtifact(t, releaseControls, []string{"wrappers--0.6.3"}, []string{"wrappers-0.7.0"})
	got = CheckExtensionFiles(art2, []InstalledExtension{inst("postgres", "wrappers", "0.6.3")})
	if len(got) != 1 || !strings.Contains(got[0].Reason, "library wrappers-0.6.3") {
		t.Fatalf("versioned library missing: %v", got)
	}
}

func TestParseControl(t *testing.T) {
	got := parseControl([]byte("# comment\ndefault_version = '1.2'\n  module_pathname='$libdir/x'  \n#module_pathname = 'no'\nrelocatable = false\n"))
	if got["default_version"] != "1.2" || got["module_pathname"] != "$libdir/x" || got["relocatable"] != "false" || len(got) != 3 {
		t.Fatalf("control = %v", got)
	}
}

// pgUpgradeHarness is the upgrade harness with the node's Postgres pin moved to a new release
// whose artifact is a fake directory, and the project's databases holding extensions.
func pgUpgradeHarness(t *testing.T, art string, exts ...InstalledExtension) *upHarness {
	t.Helper()
	h := newUpHarness(t)
	h.arts.pin(config.SvcPostgres, "postgres-17.2.0-r1")
	h.arts.dirs = map[string]string{config.SvcPostgres + " postgres-17.2.0-r1": art}
	h.plane.exts = exts
	return h
}

// An installed extension that the new Postgres release cannot serve is a blocker at planning (its
// name goes to Studio's unsupported-extension list) and refuses the upgrade before anything is
// touched, however the release gets there.
func TestUpgradeRefusesAnExtensionTheNewPostgresCannotServe(t *testing.T) {
	ctx := context.Background()
	// The new release has pg_net and wrappers 0.6.3 only; the project uses wrappers 0.5.0.
	art := fakePostgresArtifact(t, releaseControls, []string{"pg_net--0.20.4", "wrappers--0.6.3"}, []string{"pg_net", "wrappers-0.6.3"})
	h := pgUpgradeHarness(t, art,
		InstalledExtension{Database: "postgres", Name: "pg_net", Version: "0.20.4"},
		InstalledExtension{Database: "postgres", Name: "wrappers", Version: "0.5.0"})
	el, err := h.e.UpgradeEligibility(ctx, h.ref)
	if err != nil {
		t.Fatal(err)
	}
	if el.Eligible || len(el.Extensions) != 1 || el.Extensions[0].Name != "wrappers" || len(el.Blockers) != 1 ||
		el.Blockers[0].Type != BlockerExtension || el.Blockers[0].Extension != "wrappers" || !strings.Contains(el.Blockers[0].Message, "ALTER EXTENSION wrappers UPDATE") {
		t.Fatalf("eligibility = %+v", el)
	}
	_, err = h.e.BeginUpgrade(ctx, h.ref, UpgradeRequest{})
	if !errors.Is(err, ErrUpgradeUnsupported) || !strings.Contains(err.Error(), "wrappers 0.5.0") {
		t.Fatalf("BeginUpgrade: %v", err)
	}
	if got := h.project(t).Status; got != registry.StatusActiveHealthy || h.plane.log() != "" || len(h.backup.calls) != 0 {
		t.Fatalf("a refused upgrade changed things: %s, steps %q, backups %v", got, h.plane.log(), h.backup.calls)
	}
	// The owner updates the extension on the running release; the upgrade is open then.
	h.plane.exts = []InstalledExtension{{Database: "postgres", Name: "pg_net", Version: "0.20.4"}, {Database: "postgres", Name: "wrappers", Version: "0.6.3"}}
	if el, _ := h.e.UpgradeEligibility(ctx, h.ref); !el.Eligible || len(el.Extensions) != 0 {
		t.Fatalf("eligibility after the update = %+v", el)
	}
}

// A release that is not on disk at planning cannot be checked: planning says so and stays open,
// and the upgrade checks after fetching it, before the backup and before anything stops.
func TestUpgradeChecksExtensionsAfterFetchingTheRelease(t *testing.T) {
	ctx := context.Background()
	art := fakePostgresArtifact(t, map[string]string{"pg_net": releaseControls["pg_net"]}, []string{"pg_net--0.20.4"}, []string{"pg_net"})
	h := pgUpgradeHarness(t, art, InstalledExtension{Database: "postgres", Name: "wrappers", Version: "0.6.3"})
	h.arts.hidden = map[string]bool{config.SvcPostgres + " postgres-17.2.0-r1": true}
	el, err := h.e.UpgradeEligibility(ctx, h.ref)
	if err != nil || !el.Eligible || !strings.Contains(strings.Join(el.Notes, "|"), "could not be checked") {
		t.Fatalf("eligibility = %+v, %v", el, err)
	}
	_, err = h.e.UpgradeProject(ctx, h.ref, nil)
	if err == nil || !strings.Contains(err.Error(), "before any service was touched") || !strings.Contains(err.Error(), "does not ship this extension") {
		t.Fatalf("err = %v", err)
	}
	if h.plane.log() != "" || len(h.backup.calls) != 0 {
		t.Fatalf("the backup or a service ran: steps %q, backups %v", h.plane.log(), h.backup.calls)
	}
	if p := h.project(t); p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcPostgres] != oldPG {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
	if st := h.latest(t); st.Status != registry.UpgradeFailed || st.Error != UpgradeErrArtifacts {
		t.Fatalf("status row = %+v", st)
	}
}

// After the new cluster starts, extension code that does not load rolls the upgrade back.
func TestUpgradeRollsBackWhenAnExtensionDoesNotLoad(t *testing.T) {
	ctx := context.Background()
	art := fakePostgresArtifact(t, releaseControls, []string{"wrappers--0.6.3"}, []string{"wrappers-0.6.3"})
	h := pgUpgradeHarness(t, art, InstalledExtension{Database: "postgres", Name: "wrappers", Version: "0.6.3"})
	h.plane.verifyBad = "postgres-17.2.0-r1"
	_, err := h.e.UpgradeProject(ctx, h.ref, nil)
	if err == nil || !strings.Contains(err.Error(), "previous versions are running again") || !strings.Contains(err.Error(), "wrappers_handler") {
		t.Fatalf("err = %v", err)
	}
	if got, want := h.plane.log(), "Stop, StartDatabase v2.195.0-r1, Start v2.195.0-r1, VerifyExtensions v2.195.0-r1, Stop, StartDatabase v2.100.0-r1, Start v2.100.0-r1"; got != want {
		t.Fatalf("plane steps = %s, want %s", got, want)
	}
	if p := h.project(t); p.Status != registry.StatusActiveHealthy || p.Versions[config.SvcPostgres] != oldPG || p.Versions[config.SvcGoTrue] != oldAuth {
		t.Fatalf("project = %s %v", p.Status, p.Versions)
	}
	if st := h.latest(t); st.Status != registry.UpgradeFailed || st.Error != UpgradeErrHealth || !strings.Contains(st.Detail, "rolled back") {
		t.Fatalf("status row = %+v", st)
	}
}

// The extension checks are for a changed Postgres release only: an upgrade of GoTrue and
// PostgREST never asks the databases for their extensions.
func TestAPIOnlyUpgradeSkipsTheExtensionChecks(t *testing.T) {
	h := newUpHarness(t)
	h.plane.exts = []InstalledExtension{{Database: "postgres", Name: "wrappers", Version: "0.1.0"}}
	if _, err := h.e.UpgradeProject(context.Background(), h.ref, nil); err != nil {
		t.Fatal(err)
	}
	if h.plane.extCalls != 0 || strings.Contains(h.plane.log(), "VerifyExtensions") {
		t.Fatalf("extensions were inspected: %d calls, steps %s", h.plane.extCalls, h.plane.log())
	}
}

// A rollback that fails on an API unit still leaves PostgreSQL running, and the project's
// scheduled backups with it.
func TestRollbackFailureKeepsBackupsRunningWhileThePostgresRuns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pgHealthy bool
		wantStart bool
	}{{"cluster up", true, true}, {"cluster down", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpHarness(t)
			ft := &fakeTimers{}
			h.e.opts.Timers = ft
			h.plane.bad, h.plane.failRollback, h.plane.pgHealthy = newAuth, true, tc.pgHealthy
			_, err := h.e.UpgradeProject(context.Background(), h.ref, nil)
			if err == nil || !strings.Contains(err.Error(), "rollback to the previous versions failed too") {
				t.Fatalf("err = %v", err)
			}
			started := strings.Contains(strings.Join(ft.calls, ","), "start "+h.ref)
			if started != tc.wantStart {
				t.Fatalf("timer calls = %v, want a start: %v", ft.calls, tc.wantStart)
			}
			if st := h.latest(t); strings.Contains(st.Detail, "scheduled backups stay paused") == tc.wantStart {
				t.Fatalf("detail = %q", st.Detail)
			}
		})
	}
}
