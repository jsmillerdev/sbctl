package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/units"
)

// touchRunFile marks svc's unit as rendered on this node.
func touchRunFile(t *testing.T, cfg *config.Config, svc string) {
	t.Helper()
	p := units.FilesFor(cfg, units.Spec{Service: svc}).Run
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
}

func startingDeps(r *managerRig) Deps {
	d := r.n.deps()
	d.Supervisor, d.Artifacts, d.ReadyTimeout, d.Start = r.sup, allArtifacts(), 5*time.Second, true
	return d
}

func tenantServices(f Fleet) string {
	var s []string
	for _, t := range f {
		s = append(s, t.Service())
	}
	return strings.Join(s, ",")
}

// A Studio that cannot start (its unit needs a template change on some nodes) must not
// leave the daemon without tenants.
func TestSetupReturnsTheTenantsWhenStudioFails(t *testing.T) {
	r := newManagerRig(t, nil)
	r.sup.failOn["start sb-studio.service"] = errors.New("read-only file system")
	f, err := Setup(context.Background(), startingDeps(r))
	if err != nil {
		t.Fatalf("Setup failed because of Studio: %v", err)
	}
	if got := tenantServices(f); got != "supavisor,realtime,storage" {
		t.Fatalf("tenants = %q", got)
	}
}

// When a core service fails, Setup reports it and still returns the usable Fleet.
func TestSetupReturnsTheTenantsWithTheStartError(t *testing.T) {
	r := newManagerRig(t, nil)
	r.sup.failOn["start sb-realtime.service"] = errors.New("exec format error")
	f, err := Setup(context.Background(), startingDeps(r))
	if err == nil || !strings.Contains(err.Error(), "realtime") {
		t.Fatalf("err = %v", err)
	}
	if got := tenantServices(f); got != "supavisor,realtime,storage" {
		t.Fatalf("the Fleet must be usable when a service did not start; tenants = %q", got)
	}
	if !strings.Contains(r.sup.log(), "start sb-storage.service") {
		t.Fatal("the services after the failed one must still start")
	}
}

func TestSetupWithoutTheSystemProjectReturnsNoFleet(t *testing.T) {
	n := newTestNode(t)
	d := n.deps()
	d.Registry = registry.NewMemory()
	f, err := Setup(context.Background(), d)
	if err == nil || f != nil {
		t.Fatalf("fleet = %v, err = %v", f, err)
	}
}

func TestLazyIsBoundOnlyAfterBind(t *testing.T) {
	n := newTestNode(t)
	lz := NewLazy(Deps{Cfg: n.cfg})
	f := lz.Fleet()
	if got := tenantServices(f); got != "supavisor,realtime,storage" {
		t.Fatalf("tenants = %q", got)
	}
	// Nothing rendered: a node that does not run the fleet skips every tenant call, bound or not.
	lz.Bind(n.reg, n.sec)
	if err := f.EnsureTenant(context.Background(), TenantSpec{Ref: "abcdefghijklmnopqrst"}); err != nil {
		t.Fatalf("a node without the fleet must skip the tenant calls: %v", err)
	}
	if err := f.RemoveTenant(context.Background(), "abcdefghijklmnopqrst"); err != nil {
		t.Fatal(err)
	}
}

func TestLazyUnboundFailsOnceTheFleetRuns(t *testing.T) {
	r := newManagerRig(t, nil)
	// The fake supervisor renders nothing to disk, so mark the units rendered by hand.
	for _, svc := range Services {
		touchRunFile(t, r.n.cfg, svc)
	}
	lz := NewLazy(Deps{Cfg: r.n.cfg, Retry: Retry{Attempts: 1}})
	err := lz.Fleet().EnsureTenant(context.Background(), TenantSpec{Ref: "abcdefghijklmnopqrst"})
	if err == nil || !strings.Contains(err.Error(), "Bind") {
		t.Fatalf("err = %v", err)
	}
	// Bound, it resolves its credentials and reaches the service (which is not Supavisor here).
	lz.Bind(r.n.reg, r.n.sec)
	err = lz.Fleet().EnsureTenant(context.Background(), TenantSpec{Ref: "abcdefghijklmnopqrst"})
	if err == nil || strings.Contains(err.Error(), "Bind") || !strings.Contains(err.Error(), config.SvcSupavisor) {
		t.Fatalf("a bound Lazy must call the service: %v", err)
	}
}
