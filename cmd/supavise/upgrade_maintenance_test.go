package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/registry"
)

func maintenanceHost(reg registry.Registry) (*nodeHost, *bytes.Buffer) {
	var errw bytes.Buffer
	return &nodeHost{errw: &errw, openRegistry: func(context.Context) (registry.Registry, error) { return reg, nil }}, &errw
}

// A leader announces maintenance with a time to live, renews it while the run lives, and clears it when
// the run ends.
func TestAnnounceMaintenanceHoldsRenewsAndClears(t *testing.T) {
	old := maintenanceRenewEvery
	maintenanceRenewEvery = 10 * time.Millisecond
	t.Cleanup(func() { maintenanceRenewEvery = old })
	reg := registry.NewMemory()
	h, _ := maintenanceHost(reg)
	ctx := context.Background()
	reason := nodeupgrade.MaintenanceReason("v0.2.0")

	if err := h.AnnounceMaintenance(ctx, "n1", reason); err != nil {
		t.Fatal(err)
	}
	cl, _ := reg.GetCluster(ctx)
	first := cl.Maintenance
	if !first.Active(time.Now()) || first.Node != "n1" || first.Reason != reason {
		t.Fatalf("maintenance = %+v", first)
	}
	if first.Until.After(time.Now().Add(nodeupgrade.MaintenanceTTL + time.Second)) {
		t.Fatalf("until %s is beyond the time to live", first.Until)
	}
	// Renewed: the end moves forward while the run lives.
	deadline := time.Now().Add(5 * time.Second)
	for {
		cl, _ = reg.GetCluster(ctx)
		if cl.Maintenance.Until.After(first.Until) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the announcement was not renewed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.EndMaintenance(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	cl, _ = reg.GetCluster(ctx)
	if cl.Maintenance.Active(time.Now()) || cl.Maintenance.Node != "" {
		t.Fatalf("maintenance after the run: %+v", cl.Maintenance)
	}
	// Ending twice, or without an announcement, is fine.
	if err := h.EndMaintenance(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
}

// Another reason is not ours to override or to clear: a planned failover that is under way keeps its
// announcement, and so does one that replaced ours while the run lived.
func TestAnnounceMaintenanceLeavesOthersAlone(t *testing.T) {
	reg := registry.NewMemory()
	h, _ := maintenanceHost(reg)
	ctx := context.Background()
	other := registry.Maintenance{Node: "n2", Until: time.Now().Add(30 * time.Minute), Reason: "planned switchover to n1"}
	if err := reg.SetMaintenance(ctx, other); err != nil {
		t.Fatal(err)
	}
	err := h.AnnounceMaintenance(ctx, "n1", nodeupgrade.MaintenanceReason("v0.2.0"))
	if err == nil || !strings.Contains(err.Error(), "already announced") || !strings.Contains(err.Error(), "planned switchover") {
		t.Fatalf("announce over a failover's = %v", err)
	}
	if cl, _ := reg.GetCluster(ctx); cl.Maintenance != other {
		t.Fatalf("the failover's announcement changed: %+v", cl.Maintenance)
	}

	// A leftover of a run that was killed is replaced.
	if err := reg.SetMaintenance(ctx, registry.Maintenance{Node: "n1", Until: time.Now().Add(10 * time.Minute), Reason: nodeupgrade.MaintenanceReason("v0.1.9")}); err != nil {
		t.Fatal(err)
	}
	reason := nodeupgrade.MaintenanceReason("v0.2.0")
	if err := h.AnnounceMaintenance(ctx, "n1", reason); err != nil {
		t.Fatalf("over an earlier upgrade's: %v", err)
	}
	// Something else announced meanwhile: the run's end clears nothing.
	if err := reg.SetMaintenance(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := h.EndMaintenance(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if cl, _ := reg.GetCluster(ctx); cl.Maintenance != other {
		t.Fatalf("the run cleared an announcement that was not its own: %+v", cl.Maintenance)
	}
}
