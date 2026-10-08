package lifecycle

import (
	"context"
	"slices"
	"strings"

	"github.com/supavise/supavise/internal/projectconfig"
)

// Settings is the project's saved configuration as the lifecycle renders it: the
// environment of GoTrue and PostgREST, the server settings of the cluster, and the Storage
// and Realtime tenant settings. *projectconfig.Manager implements it. Without Settings
// every project runs on the defaults.
type Settings interface {
	// AuthEnv returns the GoTrue variables to set ("" removes one); externalURL is the
	// project's API_EXTERNAL_URL.
	AuthEnv(ctx context.Context, ref, externalURL string) (map[string]string, error)
	PostgRESTEnv(ctx context.Context, ref string) (map[string]string, error)
	// PostgresSettings returns "name=value" server settings in a stable order.
	PostgresSettings(ctx context.Context, ref string) ([]string, error)
	StorageSettings(ctx context.Context, ref string) (projectconfig.StorageSettings, error)
	RealtimeSettings(ctx context.Context, ref string) (projectconfig.RealtimeSettings, error)
	// PoolerSettings returns the saved pool size and client limit of the project's Supavisor
	// tenant (zero: the service's default).
	PoolerSettings(ctx context.Context, ref string) (projectconfig.PoolerSettings, error)
}

// cmdlineSettings are the settings the class puts on the postmaster's command line (and
// the replication limits and the worker count supavise fixes there). A command-line value beats
// postgresql.auto.conf and a reload cannot change it, so a saved value for one of these is
// rendered into the unit after the class's and takes effect at the next restart. Every other
// saved setting goes through ALTER SYSTEM.
var cmdlineSettings = []string{
	"shared_buffers", "effective_cache_size", "work_mem", "maintenance_work_mem", "max_wal_size",
	"max_connections", "max_wal_senders", "max_replication_slots", "max_worker_processes",
}

// SplitPostgresSettings divides "name=value" settings into those rendered as server
// arguments (cmdline) and those applied with ALTER SYSTEM.
func SplitPostgresSettings(all []string) (cmdline, alter []string) {
	for _, s := range all {
		name, _, _ := strings.Cut(s, "=")
		if slices.Contains(cmdlineSettings, name) {
			cmdline = append(cmdline, s)
		} else {
			alter = append(alter, s)
		}
	}
	return cmdline, alter
}

// ApplyOptions tune Reconfigurer.ApplyConfig.
type ApplyOptions struct {
	// RestartDatabase restarts PostgreSQL when a saved Postgres setting needs it.
	RestartDatabase bool
	// Recover applies the settings of a rollback: an earlier apply may have left the Postgres
	// cluster down (a restart that failed on the new values), so a cluster that does not answer
	// is started again on the saved settings before they are applied.
	Recover bool
}

// ApplyResult is the outcome of ApplyConfig.
type ApplyResult struct {
	// Applied is false when nothing was running to apply to (a paused project picks the
	// settings up when it resumes).
	Applied bool
	// PendingRestart is true when a Postgres setting waits for a restart of the cluster.
	PendingRestart bool
}

// Reconfigurer is the optional Manager capability that makes saved settings take effect
// and rotates the database password. *Engine implements it; the Management API uses it when
// its Manager does.
type Reconfigurer interface {
	// ApplyConfig applies the saved settings of svc (a projectconfig.Service name) of ref
	// to what is running, touching only what the service owns: GoTrue's or PostgREST's
	// unit is re-rendered and restarted, the Realtime and Storage tenants are updated, the
	// Postgres settings are applied with ALTER SYSTEM and a reload.
	ApplyConfig(ctx context.Context, ref string, svc projectconfig.Service, opts ApplyOptions) (ApplyResult, error)
	// SetDatabasePassword changes the password of the project's postgres role and tells the
	// pooler.
	SetDatabasePassword(ctx context.Context, ref, password string) error
}
