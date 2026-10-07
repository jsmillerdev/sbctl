package lifecycle

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/projectconfig"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
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

// ApplyPostgresSettings makes the saved Postgres settings of p take effect in a running
// cluster: settings that overlap the class's command-line sizing are rendered into the
// unit (they apply at the next restart), every other saved setting is applied with ALTER
// SYSTEM and a reload, and a setting that is no longer saved is reset. restart restarts the
// cluster afterwards. It returns true when something still waits for a restart.
func (pl *PostgresPlane) ApplyPostgresSettings(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, restart bool) (pending bool, err error) {
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
	var restartPending bool
	if err := c.QueryRow(ctx, `select exists (select 1 from pg_settings where pending_restart)`).Scan(&restartPending); err != nil {
		return false, err
	}
	differs, err := cmdlinePending(ctx, c, cmdline)
	if err != nil {
		return false, err
	}
	pending = restartPending || changedUnit || differs
	c.Close(context.WithoutCancel(ctx))
	if restart && pending {
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

// restartDatabase restarts the cluster and waits until it answers.
func (pl *PostgresPlane) restartDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	spec, err := pl.postgresSpec(ctx, p, keys)
	if err != nil {
		return err
	}
	if err := pl.sup.Stop(ctx, spec.Unit()); err != nil {
		return err
	}
	return pl.StartDatabase(ctx, p, keys)
}

// RestartDatabase restarts the project's PostgreSQL on the saved settings.
func (pl *PostgresPlane) RestartDatabase(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) error {
	return pl.restartDatabase(ctx, p, keys)
}

// SetRolePassword sets the login password of role in p's cluster (as a SCRAM verifier,
// with statement logging off), over the unix socket as supabase_admin.
func (pl *PostgresPlane) SetRolePassword(ctx context.Context, p *registry.Project, role, password string) error {
	return setRolePasswords(ctx, pl.paths(p), map[string]string{role: password})
}
