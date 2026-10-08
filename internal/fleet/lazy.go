package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
	"github.com/supavise/supavise/internal/units"
)

// Lazy is a Fleet whose tenants load their credentials on first use. It removes the
// chicken-and-egg of Setup: lifecycle.Open builds its Engine from OpenOptions.Fleet before
// the registry and the master key exist, while Setup needs both. A caller creates a Lazy
// from the static part of Deps, passes Fleet() in lifecycle.OpenOptions, and calls Bind
// with the opened Node's registry and secrets:
//
//	lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log})
//	oo.Fleet = lz.Fleet()
//	n, err := lifecycle.Open(ctx, cfg, oo)
//	lz.Bind(n.Registry, n.Secrets)
//
// The Engine then registers, re-keys and removes tenants like one built from Setup, in the
// CLI as well as in the daemon. A service whose unit this node never rendered (the node
// does not run the fleet: a development node with the system cluster only) is skipped, so
// a node without the fleet still creates projects. A service that was rendered and is down
// fails the call after the bounded retries, as with Setup. Calls before Bind fail.
type Lazy struct {
	mu    sync.Mutex
	d     Deps
	real  map[string]Tenant
	bound bool
}

// NewLazy returns a Lazy for d. Only Cfg, Log, TenantClient and Retry are used; Start and
// Skip are ignored (a Lazy never starts services).
func NewLazy(d Deps) *Lazy {
	d.Start, d.Skip = false, nil
	return &Lazy{d: d}
}

// Bind gives the Lazy the registry and secrets it loads credentials from.
func (l *Lazy) Bind(reg registry.Registry, sec secrets.Secrets) {
	l.mu.Lock()
	l.d.Registry, l.d.Secrets, l.bound = reg, sec, true
	l.real = nil
	l.mu.Unlock()
}

// Fleet returns the three tenants in the order Setup returns them.
func (l *Lazy) Fleet() Fleet {
	return Fleet{
		lazyTenant{l, config.SvcSupavisor},
		lazyTenant{l, config.SvcRealtime},
		lazyTenant{l, config.SvcStorage},
	}
}

// tenant returns the real tenant of svc, or nil when this node never rendered svc's unit.
func (l *Lazy) tenant(ctx context.Context, svc string) (Tenant, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.bound {
		return nil, errors.New("fleet: tenants are not bound to a registry yet (call Lazy.Bind after lifecycle.Open)")
	}
	if _, err := os.Stat(units.FilesFor(l.d.Cfg, units.Spec{Service: svc}).Run); err != nil {
		return nil, nil
	}
	if l.real == nil {
		f, err := Setup(ctx, l.d)
		if err != nil {
			return nil, err
		}
		l.real = make(map[string]Tenant, len(f))
		for _, t := range f {
			l.real[t.Service()] = t
		}
	}
	t, ok := l.real[svc]
	if !ok {
		return nil, fmt.Errorf("fleet: no tenant for %s", svc)
	}
	return t, nil
}

type lazyTenant struct {
	l   *Lazy
	svc string
}

func (t lazyTenant) Service() string { return t.svc }

func (t lazyTenant) EnsureTenant(ctx context.Context, spec TenantSpec) error {
	r, err := t.l.tenant(ctx, t.svc)
	if err != nil || r == nil {
		return err
	}
	return r.EnsureTenant(ctx, spec)
}

func (t lazyTenant) RemoveTenant(ctx context.Context, ref string) error {
	r, err := t.l.tenant(ctx, t.svc)
	if err != nil || r == nil {
		return err
	}
	return r.RemoveTenant(ctx, ref)
}

// RefreshTenant implements Refresher for the Supavisor entry; the other two tenants cache
// nothing about logins.
func (t lazyTenant) RefreshTenant(ctx context.Context, ref string) error {
	r, err := t.l.tenant(ctx, t.svc)
	if err != nil || r == nil {
		return err
	}
	if rf, ok := r.(Refresher); ok {
		return rf.RefreshTenant(ctx, ref)
	}
	return nil
}

// EnsureReplicaTenant implements ReplicaTenanter for the Supavisor entry; the other two tenants
// serve no replica.
func (t lazyTenant) EnsureReplicaTenant(ctx context.Context, spec TenantSpec) error {
	r, err := t.l.tenant(ctx, t.svc)
	if err != nil || r == nil {
		return err
	}
	if rt, ok := r.(ReplicaTenanter); ok {
		return rt.EnsureReplicaTenant(ctx, spec)
	}
	return nil
}

// RemoveReplicaTenant implements ReplicaTenanter for the Supavisor entry.
func (t lazyTenant) RemoveReplicaTenant(ctx context.Context, identifier string) error {
	r, err := t.l.tenant(ctx, t.svc)
	if err != nil || r == nil {
		return err
	}
	if rt, ok := r.(ReplicaTenanter); ok {
		return rt.RemoveReplicaTenant(ctx, identifier)
	}
	return nil
}

// QuiesceTenant implements Quiescer for the Realtime entry.
func (t lazyTenant) QuiesceTenant(ctx context.Context, ref string) error {
	r, err := t.l.tenant(ctx, t.svc)
	if err != nil || r == nil {
		return err
	}
	if q, ok := r.(Quiescer); ok {
		return q.QuiesceTenant(ctx, ref)
	}
	return nil
}
