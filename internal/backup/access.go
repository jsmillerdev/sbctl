package backup

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// registryAccess builds loopback DSNs from the registry and sealed secrets, for
// processes (the CLI, the nightly timer) that run without a lifecycle.Manager.
type registryAccess struct {
	cfg *config.Config
	reg registry.Registry
	sec secrets.Secrets
}

// AccessFromRegistry returns an Access that connects to a project's cluster on its
// loopback port as supabase_admin or postgres, using the passwords in the registry.
func AccessFromRegistry(cfg *config.Config, reg registry.Registry, sec secrets.Secrets) Access {
	return &registryAccess{cfg: cfg, reg: reg, sec: sec}
}

func (a *registryAccess) ConnString(ctx context.Context, ref, role string) (string, error) {
	p, err := a.reg.GetProject(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("backup: project %s: %w", ref, err)
	}
	secret := secrets.NameAdminPassword
	switch role {
	case roleAdmin:
	case "postgres":
		secret = secrets.NameDBPassword
	default:
		return "", fmt.Errorf("backup: unsupported role %q", role)
	}
	sealed, err := a.reg.GetSecret(ctx, ref, secret)
	if err != nil {
		return "", fmt.Errorf("backup: %s of %s: %w", secret, ref, err)
	}
	pw, err := a.sec.Open(sealed)
	if err != nil {
		return "", err
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, string(pw)),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(a.cfg.PortsFor(ref, p.Seq).Postgres)),
		Path:     "/postgres",
		RawQuery: "sslmode=disable",
	}
	return u.String(), nil
}
