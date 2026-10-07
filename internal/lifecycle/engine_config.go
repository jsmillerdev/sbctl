package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/projectconfig"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// configPlane is what ApplyConfig needs of the data plane beyond Plane; PostgresPlane has it.
type configPlane interface {
	ReconfigureService(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, svc string) error
	ApplyPostgresSettings(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys, restart bool, beforeRestart func(context.Context)) (bool, error)
	SetRolePassword(ctx context.Context, p *registry.Project, role, password string) error
}

var _ Reconfigurer = (*Engine)(nil)

// ErrNotSupported is returned by ApplyConfig for a data plane that cannot reconfigure
// single services.
var ErrNotSupported = errors.New("lifecycle: this data plane cannot apply settings")

// ApplyConfig implements Reconfigurer. A paused project is not touched (the settings are
// rendered when it starts); a project in a transitional state is refused.
func (e *Engine) ApplyConfig(ctx context.Context, ref string, svc projectconfig.Service, opts ApplyOptions) (ApplyResult, error) {
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return ApplyResult{}, err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return ApplyResult{}, err
	}
	if p.Status == registry.StatusInactive {
		return ApplyResult{}, nil
	}
	if !active(p.Status) {
		return ApplyResult{}, invalidState(p, "apply settings to")
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return ApplyResult{}, err
	}
	cp, ok := e.plane.(configPlane)
	if !ok {
		return ApplyResult{}, ErrNotSupported
	}
	res := ApplyResult{Applied: true}
	switch svc {
	case projectconfig.Auth:
		err = cp.ReconfigureService(ctx, p, keys, config.SvcGoTrue)
	case projectconfig.PostgREST:
		err = cp.ReconfigureService(ctx, p, keys, config.SvcPostgREST)
	case projectconfig.Realtime, projectconfig.Storage:
		var spec fleet.TenantSpec
		if spec, err = e.tenantSpec(ctx, p, keys); err == nil && len(e.opts.Fleet) > 0 {
			err = e.opts.Fleet.EnsureTenant(ctx, spec)
		}
	case projectconfig.Postgres:
		res.PendingRestart, err = cp.ApplyPostgresSettings(ctx, p, keys, opts.RestartDatabase, func(ctx context.Context) { e.quiesce(ctx, ref) })
	default:
		return ApplyResult{}, fmt.Errorf("lifecycle: unknown settings service %q", svc)
	}
	if err != nil {
		return ApplyResult{}, fmt.Errorf("lifecycle: apply %s settings of %s: %w", svc, ref, err)
	}
	e.event(ctx, ref, "project.config_applied", map[string]any{"service": string(svc), "pending_restart": res.PendingRestart})
	return res, nil
}

// SetDatabasePassword implements Reconfigurer: the postgres role gets the new password in
// the cluster, the sealed secret follows, and Supavisor drops the pools and cached logins of
// the tenant, so the old password stops working through the pooler at once. Everything else
// keeps running: the services connect as their own roles. When the sealed secret cannot be
// stored the role gets its old password back, so the registry and the cluster never disagree.
func (e *Engine) SetDatabasePassword(ctx context.Context, ref, password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("lifecycle: empty password")
	}
	unlock, err := e.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := e.reg.GetProject(ctx, ref)
	if err != nil {
		return err
	}
	if !active(p.Status) {
		return invalidState(p, "change the database password of")
	}
	keys, err := e.loadKeys(ctx, ref)
	if err != nil {
		return err
	}
	cp, ok := e.plane.(configPlane)
	if !ok {
		return ErrNotSupported
	}
	old := keys.DBPassword
	if err := cp.SetRolePassword(ctx, p, RolePostgres, password); err != nil {
		return err
	}
	nk := *keys
	nk.DBPassword = password
	if err := e.storeKeys(ctx, ref, &nk); err != nil {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		if rerr := cp.SetRolePassword(cctx, p, RolePostgres, old); rerr != nil {
			e.log.Error("database password: could not restore the previous password; the registry and the cluster differ", "ref", ref, "error", rerr)
		}
		return fmt.Errorf("lifecycle: store the new database password: %w", err)
	}
	if len(e.opts.Fleet) > 0 {
		// The password itself needs no tenant update (the pooler asks the database), but the
		// pooler may hold the old verifier.
		if err := e.opts.Fleet.RefreshTenant(ctx, ref); err != nil {
			e.log.Warn("database password changed, but the pooler could not drop its cached logins; the old password may keep working through it for a while", "ref", ref, "error", err)
		}
	}
	e.event(ctx, ref, "project.db_password_changed", nil)
	return nil
}

// quiesce asks the shared services to let go of ref's database before its cluster stops: a
// logical replication connection of Realtime keeps a fast shutdown from finishing (systemd
// kills the cluster after its stop timeout, 90 seconds, and the next start recovers from the
// crash). Failures are logged: the stop goes ahead either way.
func (e *Engine) quiesce(ctx context.Context, ref string) {
	if len(e.opts.Fleet) == 0 || ref == config.SystemRef {
		return
	}
	if err := e.opts.Fleet.QuiesceTenant(ctx, ref); err != nil {
		e.log.Warn("could not ask the shared services to let go of the project's database; its shutdown may be slow", "ref", ref, "error", err)
	}
}
