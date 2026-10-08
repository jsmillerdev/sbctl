package nodeupgrade

import (
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// Node is what the upgrade learns about the machine before it changes anything.
type Node struct {
	// Version is the installed binary's release ("v1.2.0"); "dev" for a build that is not one.
	Version string
	// BinaryInfo is what the installed binary reports about itself, when it can (nil for a
	// binary built before `release-info` existed).
	BinaryInfo *Info
	// Pins are the releases the node's own units run: the shared services and the system
	// project, read from what the daemon rendered, completed with BinaryInfo's pins for the
	// services no unit shows (PostgREST). The projects' releases are in Projects.
	Pins map[string]string
	// AppliedMigrations are the registry migrations the registry database has applied.
	AppliedMigrations []string
	Platform          string
	Projects          []Project

	// Verdict is the answer of `supavise status`: healthy, degraded, down, or unknown when it
	// could not be asked. Summary is its first line.
	Verdict string
	Summary string
	Escrow  Escrow

	// DiskFree is the free space of the state volume (DiskPath). LocalBackups says the backup
	// backend is on that volume, so the base backups taken first use it.
	DiskFree uint64
	// DiskUnknown is true when the free space could not be read (DiskFree is then meaningless).
	DiskUnknown  bool
	DiskPath     string
	LocalBackups bool

	// Running is set while another upgrade is running.
	Running *Running
}

// Verdicts of `supavise status`.
const (
	VerdictHealthy  = "healthy"
	VerdictDegraded = "degraded"
	VerdictDown     = "down"
	VerdictUnknown  = "unknown"
)

// Escrow is whether the master key has an encrypted copy in the backup backend.
type Escrow struct {
	// Known is false when the backend could not be asked.
	Known   bool
	Covered bool
	Detail  string
}

// Running is another upgrade that is alive.
type Running struct {
	PID   int
	Phase string
	To    string
}

// Project is a project row as the upgrade needs it.
type Project struct {
	Ref, Name, Status string
	// Versions are the releases the project records (none for a service it records none of:
	// it then runs the node's pin).
	Versions  map[string]string
	DiskBytes int64
	// LastBackup is the end of the newest completed base backup; zero when there is none.
	LastBackup time.Time
}

// Effective returns the release tag the project runs for svc.
func (p Project) Effective(svc string, node map[string]string) string {
	if t := p.Versions[svc]; t != "" {
		return t
	}
	return node[svc]
}

// Active reports whether the project's services run (and so can be backed up and upgraded).
func (p Project) Active() bool {
	return p.Status == string(registry.StatusActiveHealthy) || p.Status == string(registry.StatusActiveUnhealthy)
}

// ProjectsOf converts registry rows, the system project included. newestBackup gives the end
// of the newest completed base backup of a ref (zero when there is none).
func ProjectsOf(ps []registry.Project, newestBackup func(ref string) time.Time) []Project {
	out := make([]Project, 0, len(ps))
	for _, p := range ps {
		v := make(map[string]string, len(p.Versions))
		for k, t := range p.Versions {
			v[k] = t
		}
		out = append(out, Project{Ref: p.Ref, Name: p.Name, Status: string(p.Status), Versions: v, LastBackup: newestBackup(p.Ref)})
	}
	return out
}

// UserProjects are the projects other than the system project.
func (n *Node) UserProjects() []Project {
	var out []Project
	for _, p := range n.Projects {
		if p.Ref != config.SystemRef {
			out = append(out, p)
		}
	}
	return out
}
