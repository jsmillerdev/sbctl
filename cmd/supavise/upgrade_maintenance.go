package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/registry"
)

var _ nodeupgrade.MaintenanceAnnouncer = (*nodeHost)(nil)

// maintenanceRenewEvery is how often a run renews its announcement; a variable so that a test can
// shorten it.
var maintenanceRenewEvery = nodeupgrade.MaintenanceTTL / 3

// maintenanceHold is the announcement a run keeps alive: the registry connection that renews it and the
// goroutine that does.
type maintenanceHold struct {
	reg          registry.Registry
	node, reason string
	stop         chan struct{}
	done         chan struct{}
	once         sync.Once
}

// maintenanceRegistry opens the registry the announcement is written to; the tests of the run replace it.
func (h *nodeHost) maintenanceRegistry(ctx context.Context) (registry.Registry, error) {
	if h.openRegistry != nil {
		return h.openRegistry(ctx)
	}
	return registry.OpenExisting(ctx, h.registryDSN(ctx)+" pool_max_conns=2")
}

// AnnounceMaintenance implements nodeupgrade.MaintenanceAnnouncer: it writes cluster.maintenance for node
// and renews it every third of its life until EndMaintenance, so that a run of any length holds it and a
// run that dies loses it within nodeupgrade.MaintenanceTTL. An announcement that a run of ours left is
// replaced; one that a planned failover made is not.
func (h *nodeHost) AnnounceMaintenance(ctx context.Context, node, reason string) error {
	reg, err := h.maintenanceRegistry(ctx)
	if err != nil {
		return err
	}
	cl, err := reg.GetCluster(ctx)
	if err != nil {
		reg.Close()
		return fmt.Errorf("cannot read the cluster: %w", err)
	}
	if m := cl.Maintenance; m.Active(time.Now()) && !strings.HasPrefix(m.Reason, nodeupgrade.MaintenanceReasonPrefix) {
		reg.Close()
		return fmt.Errorf("maintenance is already announced on %s until %s (%s): wait for it to end", m.Node, m.Until.Format(time.RFC3339), m.Reason)
	}
	hold := &maintenanceHold{reg: reg, node: node, reason: reason, stop: make(chan struct{}), done: make(chan struct{})}
	if err := hold.write(ctx); err != nil {
		reg.Close()
		return err
	}
	h.mu.Lock()
	old := h.maint
	h.maint = hold
	h.mu.Unlock()
	if old != nil {
		old.end(ctx, false)
	}
	go hold.renew(h)
	return nil
}

func (m *maintenanceHold) write(ctx context.Context) error {
	return m.reg.SetMaintenance(ctx, registry.Maintenance{Node: m.node, Until: time.Now().Add(nodeupgrade.MaintenanceTTL), Reason: m.reason})
}

// renew extends the announcement until the hold ends.
func (m *maintenanceHold) renew(h *nodeHost) {
	defer close(m.done)
	t := time.NewTicker(maintenanceRenewEvery)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if err := m.write(ctx); err != nil {
				fmt.Fprintf(h.errw, "warning: the maintenance announcement was not renewed: %v\n", err)
			}
			cancel()
		}
	}
}

// end stops the renewal and, with clear, removes the announcement when it is still this one's. It closes
// the registry.
func (m *maintenanceHold) end(ctx context.Context, clear bool) error {
	var err error
	m.once.Do(func() {
		close(m.stop)
		<-m.done
		if clear {
			var cl *registry.Cluster
			if cl, err = m.reg.GetCluster(ctx); err == nil && cl.Maintenance.Node == m.node && cl.Maintenance.Reason == m.reason {
				err = m.reg.SetMaintenance(ctx, registry.Maintenance{})
			}
		}
		m.reg.Close()
	})
	return err
}

// EndMaintenance implements nodeupgrade.MaintenanceAnnouncer.
func (h *nodeHost) EndMaintenance(ctx context.Context, _ string) error {
	h.mu.Lock()
	hold := h.maint
	h.maint = nil
	h.mu.Unlock()
	if hold == nil {
		return nil
	}
	return hold.end(ctx, true)
}
