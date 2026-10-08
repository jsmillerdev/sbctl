package main

import (
	"log/slog"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// newFleet returns the tenant fleet that every process creating, re-keying or deleting
// projects registers them with (Supavisor, Realtime and Storage), and the function that
// binds it to the node's registry and master key once lifecycle.Open has them.
//
// The tenants are the ones fleet.Setup builds: `supavise serve` and the project commands go
// through the same code, so a project created through the Management API reaches the
// shared services exactly as one created with `supavise projects create`. lifecycle.Open builds
// its Engine before the registry and the master key exist, and Setup needs both, so the
// fleet is a fleet.Lazy: its tenants run Setup on first use and skip a service whose unit
// this node never rendered (a node that does not run the shared services still creates
// projects). `supavise serve` additionally starts the services themselves at boot
// (app.startFleet calls fleet.Setup with Start set), which is what the installer relies on.
func newFleet(cfg *config.Config, log *slog.Logger) (fleet.Fleet, func(reg registry.Registry, sec secrets.Secrets)) {
	lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log.With("component", "fleet")})
	return lz.Fleet(), lz.Bind
}
