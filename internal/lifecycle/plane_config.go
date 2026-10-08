package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
	"github.com/jsmillerdev/supavise/internal/units"
)

// ReconfigureService re-renders the env files of p's API units from the saved settings and
// restarts only the unit of svc (config.SvcGoTrue or config.SvcPostgREST), waiting until it
// answers. The other unit keeps running on the file it started with; its file is rewritten
// too, so its next start is consistent. A unit that failed on earlier settings is started
// again.
func (pl *PostgresPlane) ReconfigureService(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, svc string) error {
	specs, err := pl.apiSpecs(ctx, p, keys)
	if err != nil {
		return err
	}
	var target *units.Spec
	for i := range specs {
		if err := pl.sup.Render(ctx, specs[i]); err != nil {
			return err
		}
		if specs[i].Service == svc {
			target = &specs[i]
		}
	}
	if target == nil {
		return fmt.Errorf("lifecycle: %s has no %s unit", p.Ref, svc)
	}
	if err := pl.sup.Stop(ctx, target.Unit()); err != nil {
		return err
	}
	if err := pl.sup.Start(ctx, target.Unit()); err != nil {
		return err
	}
	return pl.wait(ctx, target.Unit(), svc, pl.opts.ServiceReadyTimeout, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, svc) })
}

// SystemRefreshError says which part of RefreshSystem failed.
type SystemRefreshError struct {
	// Auth is true when the dashboard's sign-in service failed, false for the cluster.
	Auth bool
	Err  error
}

func (e *SystemRefreshError) Error() string { return e.Err.Error() }
func (e *SystemRefreshError) Unwrap() error { return e.Err }

// RefreshSystem brings the system project's cluster and the dashboard's sign-in service to the
// files the current settings render: the cluster restarts when its files changed (StartDatabase),
// then RefreshSystemAuth. A restart of the cluster stops the sign-in service with it (the unit
// requires the cluster, and systemd stops a unit that requires one that is stopped, without
// starting it again when the cluster comes back), so a sign-in service that was running when the
// refresh began and is not running after it is started: the dashboard must not be left without
// sign-in by a release that changed how the cluster is rendered. One that was not running is left
// alone, as in RefreshSystemAuth.
func (pl *PostgresPlane) RefreshSystem(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	return pl.refreshSystem(ctx, p, keys, func() error { return pl.StartDatabase(ctx, p, keys) })
}

// refreshSystem is RefreshSystem with the cluster's step given, so that a test can stand in for it.
func (pl *PostgresPlane) refreshSystem(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, startDatabase func() error) error {
	unit := config.UnitName(config.SvcGoTrue, config.SystemRef)
	was := pl.unitRunning(ctx, unit)
	if err := startDatabase(); err != nil {
		return &SystemRefreshError{Err: err}
	}
	authErr := pl.RefreshSystemAuth(ctx, p, keys)
	if authErr == nil && was && !pl.unitRunning(ctx, unit) {
		pl.log.Info("the dashboard's sign-in service was stopped by the restart of the system cluster; starting it", "unit", unit)
		authErr = pl.startAPI(ctx, p, keys)
	}
	if authErr != nil {
		return &SystemRefreshError{Auth: true, Err: authErr}
	}
	return nil
}

func (pl *PostgresPlane) unitRunning(ctx context.Context, unit string) bool {
	st, err := pl.sup.Status(ctx, unit)
	return err == nil && (st.State == units.StateActive || st.State == units.StateActivating)
}

// RefreshSystemAuth renders supavise-gotrue@system again and restarts it when what it renders
// changed: a node upgraded to a version that turns on dashboard SSO, or a changed [mail]
// section. A unit whose files are unchanged is left alone (the daemon calls this at every
// start). It does nothing for a unit that is not running: whoever starts it renders it first.
func (pl *PostgresPlane) RefreshSystemAuth(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	specs, err := pl.apiSpecs(ctx, p, keys)
	if err != nil {
		return err
	}
	cr, ok := pl.sup.(units.ChangeRenderer)
	for _, spec := range specs {
		changed := false
		if ok {
			if changed, err = cr.RenderChanged(ctx, spec); err != nil {
				return err
			}
		} else if err := pl.sup.Render(ctx, spec); err != nil {
			return err
		}
		if !changed {
			continue
		}
		st, err := pl.sup.Status(ctx, spec.Unit())
		if err != nil || (st.State != units.StateActive && st.State != units.StateActivating) {
			continue
		}
		pl.log.Info("the dashboard's sign-in service runs on older settings; restarting it", "unit", spec.Unit())
		if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
			return err
		}
		if err := pl.sup.Start(ctx, spec.Unit()); err != nil {
			return err
		}
		svc := spec.Service
		if err := pl.wait(ctx, spec.Unit(), svc, pl.opts.ServiceReadyTimeout, func(ctx context.Context) error { return pl.checkHTTP(ctx, p, svc) }); err != nil {
			return err
		}
	}
	return nil
}

// ApplyPostgresSettings makes the saved Postgres settings of p take effect in a running
// cluster: settings that overlap the class's command-line sizing are rendered into the
// unit (they apply at the next restart), every other saved setting is applied with ALTER
// SYSTEM and a reload, and a setting that is no longer saved is reset. restart restarts the
// cluster afterwards (beforeRestart, when given, runs first, so that the shared services can
// let go of their connections). It returns true when something still waits for a restart.
func (pl *PostgresPlane) ApplyPostgresSettings(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, restart bool, beforeRestart func(context.Context)) (pending bool, err error) {
	if pl.opts.Settings == nil {
		return false, nil
	}
	saved, err := pl.opts.Settings.PostgresSettings(ctx, p.Ref)
	if err != nil {
		return false, err
	}
	cmdline, alter := SplitPostgresSettings(saved)
	spec, err := pl.postgresSpec(ctx, p, keys)
	if err != nil {
		return false, err
	}
	// Render without restarting: the files are what the next start reads.
	changedUnit := false
	if cr, ok := pl.sup.(units.ChangeRenderer); ok {
		if changedUnit, err = cr.RenderChanged(ctx, spec); err != nil {
			return false, err
		}
	} else if err := pl.sup.Render(ctx, spec); err != nil {
		return false, err
	}

	pp := pl.paths(p)
	c, err := connect(ctx, socketDSN(pp, "postgres"))
	if err != nil {
		return false, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	wanted := map[string]string{}
	for _, s := range alter {
		name, val, _ := strings.Cut(s, "=")
		wanted[name] = val
	}
	// What an earlier save left in postgresql.auto.conf and is not wanted any more.
	rows, err := c.Query(ctx, `select distinct name from pg_file_settings where sourcefile like '%postgresql.auto.conf' and applied is not null`)
	if err != nil {
		return false, fmt.Errorf("lifecycle: read postgresql.auto.conf: %w", err)
	}
	var stale []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return false, err
		}
		if _, ok := projectconfig.PostgresSchema.Field(n); ok {
			if _, keep := wanted[n]; !keep && !isCmdline(n) {
				stale = append(stale, n)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, n := range stale {
		var stmt string
		if err := c.QueryRow(ctx, `select format('alter system reset %I', $1::text)`, n).Scan(&stmt); err != nil {
			return false, err
		}
		if _, err := c.Exec(ctx, stmt); err != nil {
			return false, fmt.Errorf("lifecycle: reset %s: %w", n, err)
		}
	}
	for name, val := range wanted {
		var stmt string
		if err := c.QueryRow(ctx, `select format('alter system set %I = %L', $1::text, $2::text)`, name, val).Scan(&stmt); err != nil {
			return false, err
		}
		if _, err := c.Exec(ctx, stmt); err != nil {
			return false, fmt.Errorf("lifecycle: set %s = %s: %w", name, val, err)
		}
	}
	if _, err := c.Exec(ctx, `select pg_reload_conf()`); err != nil {
		return false, err
	}
	// The reload is asynchronous; pg_settings shows the outcome once the postmaster has
	// re-read the files.
	for i := 0; i < 20; i++ {
		var waiting bool
		if err := c.QueryRow(ctx, `select exists (select 1 from pg_file_settings where sourcefile like '%postgresql.auto.conf' and applied is false and error is null)`).Scan(&waiting); err != nil {
			return false, err
		}
		if !waiting {
			break
		}
		sleepCtx(ctx, 100)
	}
	// pg_settings.pending_restart is true for every command-line setting that the artifact's
	// postgresql.conf disagrees with, so only the settings that are not on the command line can
	// be asked; the others are compared with their saved values below.
	var managed []string
	for _, f := range projectconfig.PostgresSchema.Fields {
		if !isCmdline(f.Name) {
			managed = append(managed, f.Name)
		}
	}
	var restartPending bool
	if err := c.QueryRow(ctx, `select exists (select 1 from pg_settings where pending_restart and name = any($1))`, managed).Scan(&restartPending); err != nil {
		return false, err
	}
	differs, err := cmdlinePending(ctx, c, cmdline)
	if err != nil {
		return false, err
	}
	pending = restartPending || changedUnit || differs
	c.Close(context.WithoutCancel(ctx))
	if restart && pending {
		if beforeRestart != nil {
			beforeRestart(ctx)
		}
		if err := pl.restartDatabase(ctx, p, keys); err != nil {
			return true, err
		}
		return false, nil
	}
	return pending, nil
}

func isCmdline(name string) bool {
	for _, n := range cmdlineSettings {
		if n == name {
			return true
		}
	}
	return false
}

// cmdlinePending reports whether a saved command-line setting is not what the running
// cluster has. Sizes are compared in the unit pg_settings reports them in (shared_buffers
// counts 8 kB blocks).
func cmdlinePending(ctx context.Context, c *pgx.Conn, cmdline []string) (bool, error) {
	for _, s := range cmdline {
		name, val, _ := strings.Cut(s, "=")
		var setting string
		var unit *string
		if err := c.QueryRow(ctx, `select setting, unit from pg_settings where name = $1`, name).Scan(&setting, &unit); err != nil {
			return false, err
		}
		have, err := strconv.ParseFloat(setting, 64)
		if err != nil {
			continue
		}
		want, err := strconv.ParseFloat(val, 64)
		if err != nil {
			bytes, serr := projectconfig.ParseSize(val)
			if serr != nil || unit == nil {
				continue
			}
			mult := sizeUnitBytes(*unit)
			if mult == 0 {
				continue
			}
			want = bytes / mult
		}
		if want != have {
			return true, nil
		}
	}
	return false, nil
}

// sizeUnitBytes is the byte size of a pg_settings unit such as "8kB" or "MB" (0: not a size).
func sizeUnitBytes(u string) float64 {
	i := 0
	for i < len(u) && u[i] >= '0' && u[i] <= '9' {
		i++
	}
	n := 1.0
	if i > 0 {
		n, _ = strconv.ParseFloat(u[:i], 64)
	}
	switch u[i:] {
	case "B":
		return n
	case "kB":
		return n * 1024
	case "MB":
		return n * 1024 * 1024
	case "GB":
		return n * 1024 * 1024 * 1024
	}
	return 0
}

func sleepCtx(ctx context.Context, ms int) {
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// restartDatabase restarts the whole project like a pause and a resume does: GoTrue and
// PostgREST are bound to the cluster's unit, so they stop with it and start again after it.
func (pl *PostgresPlane) restartDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	if err := pl.Stop(ctx, p.Ref); err != nil {
		return err
	}
	return pl.Start(ctx, p, keys)
}

// RecoverPostgres brings p's cluster back after an apply of Postgres settings that left it
// down: a restart that failed on a value Postgres accepted in ALTER SYSTEM and cannot start
// with. A cluster that answers is left alone (ApplyPostgresSettings resets what is stale
// through SQL). A cluster that does not is stopped, every setting of the schema is removed
// from postgresql.auto.conf offline (the saved settings are the truth: command-line ones
// are rendered into the unit, the others are written again by the ApplyPostgresSettings that
// follows), and the project starts on the saved settings.
func (pl *PostgresPlane) RecoverPostgres(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	pp := pl.paths(p)
	if err := ping(ctx, pp); err == nil {
		return nil
	}
	pl.log.Warn("the cluster does not answer after a settings change; clearing the managed settings from postgresql.auto.conf and starting it on the saved ones", "ref", p.Ref)
	if err := pl.Stop(ctx, p.Ref); err != nil {
		return err
	}
	if err := clearManagedAutoConf(pp.Data); err != nil {
		return err
	}
	return pl.Start(ctx, p, keys)
}

// clearManagedAutoConf removes the settings of projectconfig.PostgresSchema from the
// postgresql.auto.conf of a stopped cluster, keeping every other line.
func clearManagedAutoConf(dataDir string) error {
	path := filepath.Join(dataDir, "postgresql.auto.conf")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	out, changed := stripManagedSettings(string(b))
	if !changed {
		return nil
	}
	_, err = writeFile(path, []byte(out), 0o600)
	return err
}

// stripManagedSettings drops the lines of the schema's settings from the body of a
// postgresql.auto.conf. Names compare case-insensitively, as Postgres reads them.
func stripManagedSettings(body string) (string, bool) {
	var kept []string
	changed := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			name, _, _ := strings.Cut(t, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			if _, managed := projectconfig.PostgresSchema.Field(name); managed {
				changed = true
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), changed
}

// CheckRender renders the units of svc from the saved settings without touching anything
// that runs: an environment the supervisor cannot write (a line break in a value) or server
// arguments it cannot quote fail here, at the save, and not when the project resumes.
func (pl *PostgresPlane) CheckRender(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, svc projectconfig.Service) error {
	switch svc {
	case projectconfig.Auth, projectconfig.PostgREST:
		specs, err := pl.apiSpecs(ctx, p, keys)
		if err != nil {
			return err
		}
		for _, sp := range specs {
			if _, err := units.FormatEnv(sp.Env); err != nil {
				return err
			}
		}
	case projectconfig.Postgres:
		spec, err := pl.postgresSpec(ctx, p, keys)
		if err != nil {
			return err
		}
		if _, err := units.FormatRun(spec); err != nil {
			return err
		}
	}
	return nil
}

// RestartDatabase restarts the project (PostgreSQL, then GoTrue and PostgREST) on the saved settings.
func (pl *PostgresPlane) RestartDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	return pl.restartDatabase(ctx, p, keys)
}

// SetRolePasswords sets the login passwords of all the service roles to the ones in keys.
func (pl *PostgresPlane) SetRolePasswords(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	return setRolePasswords(ctx, pl.paths(p), pl.rolePasswords(keys))
}

// SetRolePassword sets the login password of role in p's cluster (as a SCRAM verifier,
// with statement logging off), over the unix socket as supabase_admin.
func (pl *PostgresPlane) SetRolePassword(ctx context.Context, p *registry.Project, role, password string) error {
	return setRolePasswords(ctx, pl.paths(p), map[string]string{role: password})
}
