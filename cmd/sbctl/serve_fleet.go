package main

import (
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
)

// newFleet returns the tenant fleet every command that creates or deletes projects
// registers them with. It is empty until the fleet workstream's fleet.Setup is wired in
// here: an empty fleet.Fleet skips the tenant calls, so projects work without Supavisor,
// Realtime and Storage. Both `sbctl serve` and the project commands use it, so the daemon
// and the CLI register tenants the same way.
func newFleet(_ *config.Config) fleet.Fleet { return fleet.Fleet{} }
