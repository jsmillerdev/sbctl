package nodeupgrade

import (
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/infra"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/selfupdate"
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

	// ConvergeRevision is the host layer revision the node completed (its converged marker; 0 for
	// a node that never converged, which is every v0.1.x node). ConvergeKnown is false when the
	// marker could not be read, and the plan then claims nothing about the host.
	ConvergeRevision int
	ConvergeKnown    bool
	// Infra is what the node knows of its AWS stack (infra.Gap); nil on a host that is not on a
	// stack or when the question could not be answered.
	Infra *infra.Report

	// Cluster is what the node knows of the cluster it belongs to; nil on a single server, whose
	// every project is its own.
	Cluster *ClusterView

	// Running is set while another upgrade is running.
	Running *Running
}

// ClusterView is the node's place in a cluster, as far as an upgrade of this node needs it.
// Projects holds only the projects homed on this node: a project homed on another server is
// upgraded by that server (its files, its backups and its restarts are there), and Elsewhere names
// them.
type ClusterView struct {
	// Self is this node's id; Leader says its system cluster is the primary.
	Self   string
	Leader bool
	// Standbys are the other active nodes that hold a standby of a database whose primary runs
	// here: the system cluster on every node while this one leads, and the replicas of the projects
	// homed here.
	Standbys []Standby
	// Elsewhere are the refs of the projects homed on other nodes.
	Elsewhere []string
}

// Standby is a node that holds standbys of this node's databases.
type Standby struct {
	// Node and Name identify the node; Version is the release it runs ("" when it never said).
	Node, Name, Version string
	// Refs are the databases it holds a standby of: "system" and project refs.
	Refs []string
}

// Behind returns the standbys that do not run the release version yet: the nodes that must be
// upgraded before this one when the release changes PostgreSQL in a way a standby cannot follow. A
// node that never reported its version counts as behind, one on a build that is not a release
// ("dev") does not: it cannot be judged.
func (c *ClusterView) Behind(version string) []Standby {
	if c == nil {
		return nil
	}
	var out []Standby
	for _, s := range c.Standbys {
		if cmp, ok := selfupdate.Compare(s.Version, version); s.Version == "" || (ok && cmp < 0) {
			out = append(out, s)
		}
	}
	return out
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
	// HeldRestart is true when a restart of the project's PostgreSQL, GoTrue or PostgREST was
	// held back for a rollout that did not finish (lifecycle.HeldRestart).
	HeldRestart bool
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
	return registry.Status(p.Status).Running()
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
