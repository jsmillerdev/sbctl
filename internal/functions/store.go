package functions

import (
	"github.com/jsmillerdev/supavise/internal/api"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// NewStore returns the API store of reg: the registry's own database for a Postgres
// registry, memory otherwise. It is the store api.Deps.Store takes, so the API server and
// the Syncer can share one.
func NewStore(reg registry.Registry) api.Store {
	if pg, ok := reg.(*registry.Postgres); ok {
		return api.NewPGStore(pg.Pool())
	}
	return api.NewMemoryStore()
}
