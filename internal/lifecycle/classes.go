package lifecycle

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/supavise/supavise/internal/config"
)

// Class is a project's compute size: the hosted names (Nano, Micro, Small, Medium, Large, XL,
// 2XL ... 16XL) with their memory, CPU, connection limits and the Postgres sizing that follows
// from them. registry.Project.Class holds Name. The memory and CPU become the project's systemd
// MemoryMax and CPUQuota (Limits), the Postgres fields become server arguments (Settings), and
// PoolSize and PoolerMaxClients are the project's Supavisor tenant defaults.
//
// Hosted publishes the memory, the vCPU count, the connection limits, the replication limits and
// the pooler client limit of each size (https://supabase.com/docs/guides/platform/compute-and-disk).
// It generates the Postgres settings per size with a closed-source tool (supabase-admin-api
// optimize db) and documents none of them, so the rest is ours, derived from those numbers by the
// rules at newSize: PostgreSQL's usual guidance of 25% of memory for shared_buffers and 75% for
// effective_cache_size, and work_mem from what is left of the memory divided across the
// connections. Saved Postgres settings (the dashboard's Database settings) still win over all
// of it, as on hosted.
type Class struct {
	// Name is the registry value and the name the CLI takes: Studio's infra_compute_size
	// ("nano", "micro", ..., "xlarge", "2xlarge").
	Name string
	// Variant is the billing add-on variant Studio sends and reads ("ci_micro").
	Variant string
	// Title is the name hosted shows ("Micro", "XL", "2XL").
	Title string
	// MemoryBytes is the memory cap (systemd MemoryMax).
	MemoryBytes int64
	// CPUQuotaPercent is systemd CPUQuota: 100 per core.
	CPUQuotaPercent int
	// Shared is true for the sizes hosted runs on shared CPU (up to Medium).
	Shared bool

	SharedBuffers      string
	EffectiveCacheSize string
	WorkMem            string
	MaintenanceWorkMem string
	MaxWALSize         string

	MaxConnections      int
	MaxWorkerProcesses  int
	MaxWALSenders       int
	MaxReplicationSlots int

	// PoolSize is the Supavisor tenant's default_pool_size: 40% of max_connections, the share
	// hosted advises when PostgREST is in heavy use, rounded down to 5.
	PoolSize int
	// PoolerMaxClients is the tenant's max_client_conn, the "Connection Pooler Max Clients"
	// column of hosted's table.
	PoolerMaxClients int
}

// ClassSystem is the profile of the system cluster: the registry plus the metadata
// databases of Supavisor, Realtime and Storage, which hold more connections.
const ClassSystem = "system"

// DefaultClass is used when CreateRequest.Class is empty: Micro, as on hosted.
const DefaultClass = "micro"

// MinWorkerProcesses is the floor of max_worker_processes: pg_cron runs its jobs in background
// workers of the cluster (cronSettings), and its launcher and pg_net's worker hold two of them.
const MinWorkerProcesses = 16

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// newSize derives a Class from the columns hosted publishes.
//
//	shared_buffers       25% of memory
//	effective_cache_size 75% of memory
//	work_mem             (memory - shared_buffers) / (3 x max_connections), 4 MB to 64 MB
//	maintenance_work_mem memory / 16, 32 MB to 2 GB
//	max_wal_size         shared_buffers, 128 MB to 8 GB
//	max_worker_processes 2 x vCPUs + 8, at least MinWorkerProcesses
//	default_pool_size    40% of max_connections, rounded down to 5
func newSize(name, variant, title string, memMB int, quotaPct int, shared bool, conns, slots, clients int) Class {
	mem := int64(memMB) * mib
	sb := mem / 4
	vcpus := (quotaPct + 99) / 100
	return Class{
		Name: name, Variant: variant, Title: title, MemoryBytes: mem, CPUQuotaPercent: quotaPct, Shared: shared,
		SharedBuffers:      pgSize(sb),
		EffectiveCacheSize: pgSize(mem * 3 / 4),
		WorkMem:            pgSize(clamp((mem-sb)/int64(3*conns), 4*mib, 64*mib)),
		MaintenanceWorkMem: pgSize(clamp(mem/16, 32*mib, 2*gib)),
		MaxWALSize:         pgSize(clamp(sb, 128*mib, 8*gib)),
		MaxConnections:     conns, MaxWorkerProcesses: max(MinWorkerProcesses, 2*vcpus+8),
		MaxWALSenders: slots, MaxReplicationSlots: slots,
		PoolSize: conns * 2 / 5 / 5 * 5, PoolerMaxClients: clients,
	}
}

func clamp(v, lo, hi int64) int64 { return min(max(v, lo), hi) }

// pgSize formats bytes for a PostgreSQL memory setting: whole GB when exact, else MB.
func pgSize(b int64) string {
	if b%gib == 0 {
		return strconv.FormatInt(b/gib, 10) + "GB"
	}
	return strconv.FormatInt(b/mib, 10) + "MB"
}

// sizes is the table, smallest first. Columns: memory in MB, CPU quota in percent (100 per
// core; hosted publishes only "shared" up to Medium and 2 to 64 dedicated vCPUs from Large),
// shared CPU, max_connections, replication slots and WAL senders, pooler max clients.
var sizes = []Class{
	newSize("nano", "ci_nano", "Nano", 512, 100, true, 60, 5, 200),
	newSize("micro", "ci_micro", "Micro", 1024, 100, true, 60, 5, 200),
	newSize("small", "ci_small", "Small", 2048, 100, true, 90, 5, 400),
	newSize("medium", "ci_medium", "Medium", 4096, 200, true, 120, 5, 600),
	newSize("large", "ci_large", "Large", 8192, 200, false, 160, 8, 800),
	newSize("xlarge", "ci_xlarge", "XL", 16384, 400, false, 240, 24, 1000),
	newSize("2xlarge", "ci_2xlarge", "2XL", 32768, 800, false, 380, 80, 1500),
	newSize("4xlarge", "ci_4xlarge", "4XL", 65536, 1600, false, 480, 80, 3000),
	newSize("8xlarge", "ci_8xlarge", "8XL", 131072, 3200, false, 490, 80, 6000),
	newSize("12xlarge", "ci_12xlarge", "12XL", 196608, 4800, false, 500, 80, 9000),
	newSize("16xlarge", "ci_16xlarge", "16XL", 262144, 6400, false, 500, 80, 12000),
}

// systemClass is the profile of the system cluster. It is not a size a project can have.
var systemClass = Class{
	Name: ClassSystem, Title: "System", MemoryBytes: gib, CPUQuotaPercent: 100,
	SharedBuffers: "64MB", EffectiveCacheSize: "256MB", WorkMem: "4MB", MaintenanceWorkMem: "32MB", MaxWALSize: "512MB",
	MaxConnections: 100, MaxWorkerProcesses: MinWorkerProcesses, MaxWALSenders: 5, MaxReplicationSlots: 5,
}

// legacyClasses maps the class names of earlier versions onto sizes: what was "default" is
// Micro, as the 1 GB cap it always had says. (The old "micro" was a smaller cluster than that:
// registry migration 1250 renames those rows to Nano; a "micro" that still arrives from a backup
// manifest of an earlier version becomes the Micro of today, one size up.)
var legacyClasses = map[string]string{"default": "micro", "pico": "nano"}

// ParseSize returns the canonical name of a size from what a person or Studio writes: the
// registry name ("2xlarge"), hosted's title ("2XL", "xl"), the add-on variant ("ci_2xlarge") or
// a name of an earlier version ("default"). ok is false for an unknown size.
func ParseSize(s string) (name string, ok bool) {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.TrimPrefix(n, "ci_")
	if to, legacy := legacyClasses[n]; legacy {
		n = to
	}
	if strings.HasSuffix(n, "xl") && n != "xl" && !strings.HasSuffix(n, "xlarge") {
		n += "arge"
	} else if n == "xl" {
		n = "xlarge"
	}
	for _, c := range sizes {
		if c.Name == n {
			return n, true
		}
	}
	return "", false
}

// ClassFor returns the profile named name ("" means DefaultClass). It takes every spelling
// ParseSize does, and ClassSystem.
func ClassFor(name string) (Class, error) {
	if name == "" {
		name = DefaultClass
	}
	if name == ClassSystem {
		return systemClass, nil
	}
	n, ok := ParseSize(name)
	if !ok {
		return Class{}, fmt.Errorf("lifecycle: unknown project size %q (have %s)", name, strings.Join(ClassNames(), ", "))
	}
	for _, c := range sizes {
		if c.Name == n {
			return c, nil
		}
	}
	panic("unreachable")
}

// ClassNames lists the user-selectable sizes, smallest first.
func ClassNames() []string {
	n := make([]string, len(sizes))
	for i, c := range sizes {
		n[i] = c.Name
	}
	return n
}

// Classes returns the user-selectable sizes, smallest first.
func Classes() []Class { return append([]Class(nil), sizes...) }

// ClassByVariant finds the size of a billing add-on variant ("ci_small").
func ClassByVariant(v string) (Class, bool) {
	for _, c := range sizes {
		if c.Variant == v {
			return c, true
		}
	}
	return Class{}, false
}

// Limits are the systemd limits the size gives its project's units.
func (c Class) Limits() config.Limits {
	return config.Limits{MemoryMax: limitString(c.MemoryBytes), CPUQuota: strconv.Itoa(c.CPUQuotaPercent) + "%"}
}

// StandardLimits reports whether l is what a project of the named size runs under unless
// someone set limits by hand: the size's own, nothing, or the 1 GB and 100% that every project
// had before sizes existed. A restore from a backup manifest keeps hand-set limits and lets
// the rest follow the size.
func StandardLimits(class string, l config.Limits) bool {
	cl, err := ClassFor(class)
	if err != nil {
		return false
	}
	return l == cl.Limits() || l == (config.Limits{}) || l == (config.Limits{MemoryMax: "1G", CPUQuota: "100%"})
}

// VCPUs is the number of whole cores the CPU quota asks for.
func (c Class) VCPUs() int { return (c.CPUQuotaPercent + 99) / 100 }

// MemoryGB is the memory in GB as hosted lists it (0.5, 1, 2, ...).
func (c Class) MemoryGB() float64 { return float64(c.MemoryBytes) / float64(gib) }

// limitString formats bytes as a systemd size: "512M", "1G", "32G".
func limitString(b int64) string {
	if b%gib == 0 {
		return strconv.FormatInt(b/gib, 10) + "G"
	}
	return strconv.FormatInt(b/mib, 10) + "M"
}

// Settings returns the "name=value" server settings the class implies.
func (c Class) Settings() []string {
	return []string{
		"shared_buffers=" + c.SharedBuffers,
		"effective_cache_size=" + c.EffectiveCacheSize,
		"work_mem=" + c.WorkMem,
		"maintenance_work_mem=" + c.MaintenanceWorkMem,
		"max_wal_size=" + c.MaxWALSize,
		"max_connections=" + strconv.Itoa(c.MaxConnections),
		"max_worker_processes=" + strconv.Itoa(max(c.MaxWorkerProcesses, MinWorkerProcesses)),
		"max_wal_senders=" + strconv.Itoa(c.MaxWALSenders),
		"max_replication_slots=" + strconv.Itoa(c.MaxReplicationSlots),
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
// at most eight concurrent jobs, which is cron.max_running_jobs here; 16 workers (the floor
// MinWorkerProcesses, which the sizes keep) leave room for them next to parallel queries and
// logical replication workers. A saved max_worker_processes goes after these (it is one of
// cmdlineSettings).
var cronSettings = []string{
	"cron.use_background_workers=on",
	"cron.database_name=postgres",
	"cron.max_running_jobs=8",
	"max_worker_processes=16",
}
