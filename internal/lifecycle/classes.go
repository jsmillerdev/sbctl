package lifecycle

import (
	"fmt"
	"sort"
	"strconv"
)

// Class is a Postgres sizing profile, chosen by registry.Project.Class. Defaults are
// small on purpose: a node runs dozens of clusters, and a developer machine must not
// run fat ones. shared_buffers counts against the unit's MemoryMax.
type Class struct {
	SharedBuffers      string
	EffectiveCacheSize string
	MaintenanceWorkMem string
	MaxWALSize         string
	MaxConnections     int
}

// ClassSystem is the profile of the system cluster: the registry plus the metadata
// databases of Supavisor, Realtime and Storage, which hold more connections.
const ClassSystem = "system"

// DefaultClass is used when CreateRequest.Class is empty.
const DefaultClass = "default"

var classes = map[string]Class{
	"micro":      {"16MB", "64MB", "16MB", "128MB", 30},
	DefaultClass: {"32MB", "128MB", "32MB", "256MB", 60},
	"small":      {"32MB", "128MB", "32MB", "256MB", 60},
	"medium":     {"128MB", "512MB", "64MB", "1GB", 120},
	"large":      {"512MB", "2GB", "128MB", "2GB", 200},
	ClassSystem:  {"64MB", "256MB", "32MB", "512MB", 100},
}

// ClassFor returns the profile named name ("" means DefaultClass).
func ClassFor(name string) (Class, error) {
	if name == "" {
		name = DefaultClass
	}
	c, ok := classes[name]
	if !ok {
		return Class{}, fmt.Errorf("lifecycle: unknown project class %q (have %v)", name, ClassNames())
	}
	return c, nil
}

// ClassNames lists the user-selectable classes.
func ClassNames() []string {
	var n []string
	for k := range classes {
		if k != ClassSystem {
			n = append(n, k)
		}
	}
	sort.Strings(n)
	return n
}

// Settings returns the "name=value" server settings the class implies.
func (c Class) Settings() []string {
	return []string{
		"shared_buffers=" + c.SharedBuffers,
		"effective_cache_size=" + c.EffectiveCacheSize,
		"maintenance_work_mem=" + c.MaintenanceWorkMem,
		"max_wal_size=" + c.MaxWALSize,
		"max_connections=" + strconv.Itoa(c.MaxConnections),
	}
}

// cronSettings make pg_cron run its jobs in background workers of the cluster itself.
// Hosted Supabase leaves pg_cron on its default, a libpq connection to localhost that its
// pg_hba.conf trusts. Our pg_hba.conf trusts only the private unix socket as supabase_admin
// (see hbaRules), so a libpq job would fail with "connection failed" for every role. Workers
// need no connection, no pg_hba rule and no network, so the jobs also run behind the
// systemd egress filter of a branch, and cron.job's nodename and nodeport play no part in
// where a job runs.
//
// max_worker_processes counts the workers: pg_cron's launcher and pg_net's worker take two,
// a running job takes one, and parallel queries take what is left. Hosted Supabase documents
// at most eight concurrent jobs, which is cron.max_running_jobs here; 16 workers leave room
// for them next to parallel queries and logical replication workers. A saved
// max_worker_processes goes after these (it is one of cmdlineSettings).
var cronSettings = []string{
	"cron.use_background_workers=on",
	"cron.database_name=postgres",
	"cron.max_running_jobs=8",
	"max_worker_processes=16",
}
