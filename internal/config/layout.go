package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SystemRef is the reserved ref of the system project (registry, fleet metadata, dashboard auth).
const SystemRef = "system"

// Default ports (docs/development.md, "Conventions"). Everything except Supavisor and the public
// proxy listens on loopback only. All of them can be changed in [ports] so that dev
// machines and tests can run several nodes side by side.
const (
	PortSupavisorSession     = 5432 // public
	PortSupavisorTransaction = 6543 // public
	PortSystemPostgres       = 5433
	PortSystemGoTrue         = 9999
	PortRealtime             = 4000
	PortStorage              = 5000
	PortStorageAdmin         = 5001
	PortImgproxy             = 5002
	PortPGMeta               = 8080
	PortStudio               = 3000
	PortEdgeRuntime          = 9000
	PortAdmin                = 7000
	PortProjectBase          = 20000
)

// Ports is the [ports] config section.
type Ports struct {
	// ProjectBase: project n gets Postgres ProjectBase+3n, GoTrue +1, PostgREST +2.
	ProjectBase          int `toml:"project_base"`
	SupavisorSession     int `toml:"supavisor_session"`
	SupavisorTransaction int `toml:"supavisor_transaction"`
	SystemPostgres       int `toml:"system_postgres"`
	SystemGoTrue         int `toml:"system_gotrue"`
	Realtime             int `toml:"realtime"`
	Storage              int `toml:"storage"`
	StorageAdmin         int `toml:"storage_admin"`
	Imgproxy             int `toml:"imgproxy"`
	PGMeta               int `toml:"pgmeta"`
	Studio               int `toml:"studio"`
	EdgeRuntime          int `toml:"edge_runtime"`
}

// DefaultPorts returns the production port plan.
func DefaultPorts() Ports {
	return Ports{
		ProjectBase: PortProjectBase, SupavisorSession: PortSupavisorSession, SupavisorTransaction: PortSupavisorTransaction,
		SystemPostgres: PortSystemPostgres, SystemGoTrue: PortSystemGoTrue, Realtime: PortRealtime, Storage: PortStorage,
		StorageAdmin: PortStorageAdmin, Imgproxy: PortImgproxy, PGMeta: PortPGMeta, Studio: PortStudio, EdgeRuntime: PortEdgeRuntime,
	}
}

// ProjectPorts are the loopback ports of a project's own units. Zero means the
// project has no such unit.
type ProjectPorts struct {
	Postgres  int
	GoTrue    int
	PostgREST int
}

// MaxProjectSeq is the largest sequence whose ports fit below 65536.
func (c *Config) MaxProjectSeq() int { return (65535 - c.Ports.ProjectBase - 2) / 3 }

// PortsFor returns the ports of a project with registry sequence seq. User projects
// use seq >= 1; the system project has fixed ports and no PostgREST.
func (c *Config) PortsFor(ref string, seq int) ProjectPorts {
	if ref == SystemRef {
		return ProjectPorts{Postgres: c.Ports.SystemPostgres, GoTrue: c.Ports.SystemGoTrue}
	}
	b := c.Ports.ProjectBase + 3*seq
	return ProjectPorts{Postgres: b, GoTrue: b + 1, PostgREST: b + 2}
}

// Service names, as used in unit names, env file names, artifact directories and
// project subdirectories. The artifact (slim-services) name can differ: see ArtifactName.
const (
	SvcPostgres    = "postgres"
	SvcGoTrue      = "gotrue"
	SvcPostgREST   = "postgrest"
	SvcSupavisor   = "supavisor"
	SvcRealtime    = "realtime"
	SvcStorage     = "storage"
	SvcPGMeta      = "pgmeta"
	SvcStudio      = "studio"
	SvcImgproxy    = "imgproxy"
	SvcEdgeRuntime = "edge-runtime"
	// SvcEdgeBundle is the one-shot unit (supavise-edge-bundle@<ref>.service, one instance per
	// project, so each has its own module cache) that bundles uploaded Edge Function sources
	// inside a sandbox; it runs the edge-runtime artifact's "bundle" command.
	SvcEdgeBundle = "edge-bundle"
)

// ProjectServices are templated per project; the rest are fleet singletons.
var ProjectServices = []string{SvcPostgres, SvcGoTrue, SvcPostgREST}

// ArtifactName maps a service to its slim-services release name (internal/versions/versions.yaml key).
func ArtifactName(svc string) string {
	switch svc {
	case SvcGoTrue:
		return "auth"
	case SvcSupavisor:
		return "pooler"
	}
	return svc
}

// UnitName returns the systemd unit for a service. Project services are template
// instances (supavise-postgres@<ref>.service); fleet services are singletons (supavise-realtime.service).
// The bundler is a template instance per project too, but only for the projects it bundles
// for, so it is not among ProjectServices (a project's lifecycle does not start it).
func UnitName(svc, ref string) string {
	if svc == SvcEdgeBundle && ref != "" {
		return fmt.Sprintf("supavise-%s@%s.service", svc, ref)
	}
	for _, s := range ProjectServices {
		if s == svc {
			return fmt.Sprintf("supavise-%s@%s.service", svc, ref)
		}
	}
	return "supavise-" + svc + ".service"
}

// EdgeBundleCleanUnit is the one-shot unit (supavise-edge-bundle-clean@<ref>.service, run as root)
// that deletes the module cache of the project's bundler instance, which is private to that
// instance's dynamic uid. supavise starts it when it deletes the project.
func EdgeBundleCleanUnit(ref string) string {
	return fmt.Sprintf("supavise-%s-clean@%s.service", SvcEdgeBundle, ref)
}

// Slice is the systemd slice every supavise unit runs in.
const Slice = "supavise.slice"

// Paths is the state-directory layout under StateDir:
//
//	artifacts/<service>/<version>/   unpacked artifacts (artifacts/.cache holds archives)
//	projects/<ref>/<svc>/            per-project service state (projects/system is the system project)
//	projects/<ref>/<svc>.env         unit environment files, 0600
//	system/<svc>/                    fleet service state (storage file backend, studio, ...)
//	certs/                           CertMagic storage
//	backups/                         local backup backend (file://)
type Paths struct{ Root string }

func (c *Config) Paths() Paths { return Paths{Root: c.StateDir} }

func (p Paths) Artifacts() string { return filepath.Join(p.Root, "artifacts") }
func (p Paths) Artifact(svc, version string) string {
	return filepath.Join(p.Root, "artifacts", ArtifactName(svc), version)
}
func (p Paths) Project(ref string) string { return filepath.Join(p.Root, "projects", ref) }
func (p Paths) ProjectService(ref, svc string) string {
	return filepath.Join(p.Root, "projects", ref, svc)
}

// PostgresData is a project's PGDATA: the data directory a restore replaces.
func (p Paths) PostgresData(ref string) string {
	return filepath.Join(p.ProjectService(ref, SvcPostgres), "data")
}
func (p Paths) EnvFile(ref, svc string) string {
	return filepath.Join(p.Root, "projects", ref, svc+".env")
}

// WALDir is the directory the daemon serves a project's WAL relay socket in. The
// project's Postgres unit gets this one directory (read-only) and nothing else of the
// backup path, so it is also the whole of that cluster's reach into the archive.
func (p Paths) WALDir(ref string) string { return filepath.Join(p.Root, "projects", ref, "wal") }

// WALSocket is the unix socket of ref's WAL relay (see Backup.WALRelay). Short on purpose:
// a unix socket path is limited to 107 bytes on Linux and 103 on macOS.
func (p Paths) WALSocket(ref string) string { return filepath.Join(p.WALDir(ref), "r.sock") }

// RestoreSources is the file naming the other projects whose WAL archive the project's
// recovery may read through its relay: the source of a restore to a new project. It sits
// in the project directory, outside every directory the project's own units can see.
func (p Paths) RestoreSources(ref string) string {
	return filepath.Join(p.Root, "projects", ref, "restore-sources")
}

func (p Paths) System(svc string) string { return filepath.Join(p.Root, "system", svc) }

// StorageFileBucket is the first path component under the Storage file backend's directory
// (STORAGE_S3_BUCKET). "stub" is what upstream's compose and the dockerless CLI use, so
// layouts stay interchangeable.
const StorageFileBucket = "stub"

// StorageObjects is the directory holding one project's Storage objects when the file
// backend is in use (<state>/system/storage/objects/stub/<ref>): Storage writes every
// object of its tenant ref below it as <bucket>/<name>/<version>, and keeps the content
// type and cache headers in extended attributes of those files.
func (p Paths) StorageObjects(ref string) string {
	return filepath.Join(p.System(SvcStorage), "objects", StorageFileBucket, ref)
}

func (p Paths) Certs() string   { return filepath.Join(p.Root, "certs") }
func (p Paths) Backups() string { return filepath.Join(p.Root, "backups") }

// BaseDomain is Domain, or "<public_ip>.sslip.io" when no domain is configured.
func (c *Config) BaseDomain() string {
	if c.Domain != "" {
		return strings.TrimSuffix(strings.ToLower(c.Domain), ".")
	}
	if c.PublicIP != "" {
		return strings.ReplaceAll(c.PublicIP, ":", "-") + ".sslip.io"
	}
	return ""
}

// Hostnames (docs/development.md, "Conventions").
func (c *Config) ProjectHost(ref string) string { return ref + ".api." + c.BaseDomain() }

// VanityHost is the host of a vanity subdomain: <name>.api.<base domain>, under the wildcard
// record and certificate of the project hosts.
func (c *Config) VanityHost(name string) string { return name + ".api." + c.BaseDomain() }
func (c *Config) StudioHost() string            { return "studio." + c.BaseDomain() }
func (c *Config) APIHost() string               { return "api." + c.BaseDomain() }
func (c *Config) PoolerHost() string            { return "pooler." + c.BaseDomain() }

// RefFromProjectHost returns the ref of "<ref>.api.<base>" or "" if host is not a project host.
func (c *Config) RefFromProjectHost(host string) string {
	host = strings.ToLower(host)
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	suffix := ".api." + c.BaseDomain()
	if c.BaseDomain() == "" || !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}

// RealtimeInternalHost is the Host header supavise sends to Realtime; Realtime resolves
// the tenant from the first label.
func RealtimeInternalHost(ref string) string { return ref + ".realtime.internal" }

// StorageClientHostHeader carries the Host a client used into Storage. Storage verifies an S3
// request's signature against the host the client signed, which with a custom hostname or a
// vanity subdomain is not the derived host Storage gets in x-forwarded-host to find its tenant;
// the fleet sets S3_PROTOCOL_NON_CANONICAL_HOST_HEADER to this name and the proxy fills it.
const StorageClientHostHeader = "X-Supavise-Client-Host"
