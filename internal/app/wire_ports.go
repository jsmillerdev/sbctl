package app

import (
	"sort"
	"strings"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/placement"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/replicas"
)

// The parts of the cluster work meet at ports: interfaces one package declares, another implements,
// and this package connects with Provide and Get. A port that nobody provides does not fail to compile
// and does not fail at run time either: the code that wants it reads it with Get and goes on without,
// so a forgotten Provide is a feature that silently does nothing. The rule for a node that belongs to
// a cluster is therefore that every port in clusterPorts is either provided or switched off on
// purpose, with the reason, by Wire.Off. Wire.run checks it after the hooks ran and names each port that
// is neither in the log, and TestEveryClusterPortIsProvidedOrOff fails for it.

// What the backup service must be able to do for the cluster work is checked when the package builds,
// not guessed at run time: a release whose service lacks one of these does not compile.
var (
	_ backup.EpochMarkerStore  = (*backup.Service)(nil) // the leader marker (failover, the boot decision)
	_ backup.ReplicaSeeder     = (*backup.Service)(nil) // the base backup a replica starts from
	_ backup.BaseBackupEnsurer = (*backup.Service)(nil) // the replica controller's base backup
)

// A clusterPort is one such seam.
type clusterPort struct {
	// Name is the port as the code names it ("failover.Locker"); Wire.Off takes the same name.
	Name string
	// Has reports whether an earlier hook provided it.
	Has func(w *Wire) bool
}

// hasForwarders reports whether the mesh hook provided its forwarders, under whatever type it chose.
func hasForwarders(w *Wire) bool { _, ok := providedAs[portHolder](w); return ok }

func provided[T any](name string) clusterPort {
	return clusterPort{Name: name, Has: func(w *Wire) bool { _, ok := Get[T](w); return ok }}
}

// clusterPorts are the ports a node with a cluster identity must have connected. Each hook that
// provides one is named in its entry.
var clusterPorts = []clusterPort{
	provided[mesh.Mesh]("mesh.Mesh"),                             // wireMesh
	provided[cluster.Membership]("cluster.Membership"),           // wireMesh
	provided[*cluster.Reporter]("cluster.Reporter"),              // wireMesh; wirePlacement adds the node's observations
	provided[*cluster.Reports]("cluster.Reports"),                // wireMesh; wireReplicas subscribes to it
	{Name: "mesh.Forwarders", Has: hasForwarders},                // wireMesh; a promotion takes the project's ports from it (wirePlacement)
	provided[placement.Resolver]("placement.Resolver"),           // wirePlacement
	provided[placement.PlaneRouter]("placement.PlaneRouter"),     // wirePlacement
	provided[placement.InstanceOps]("placement.InstanceOps"),     // wirePlacement
	provided[placement.BackupOps]("placement.BackupOps"),         // wirePlacement
	provided[placement.Contribution]("placement.Contribution"),   // wirePlacement
	provided[lifecycle.Timers]("lifecycle.Timers"),               // wirePlacement: the node's own backup timers
	provided[fleet.Fleet]("fleet.Fleet"),                         // wireFleet
	provided[fleet.PeerRefresher]("fleet.PeerRefresher"),         // wireFleet
	provided[replicas.Pooler]("replicas.Pooler"),                 // wireFleet
	provided[replicas.Service]("replicas.Service"),               // wireReplicas
	provided[replicas.Remover]("replicas.Remover"),               // wireReplicas
	provided[replicas.ReportSink]("replicas.ReportSink"),         // wireReplicas
	provided[*proxy.CertRole]("proxy.CertRole"),                  // wireProxy
	provided[failover.LocalPrimaries]("failover.LocalPrimaries"), // wireFailover
	provided[failover.Fleet]("failover.Fleet"),                   // wireFailover
	provided[failover.LocalServices]("failover.LocalServices"),   // wireFailover
	provided[failover.Locker]("failover.Locker"),                 // wireFailover
	provided[failover.ExtraChecks]("failover.ExtraChecks"),       // wireFailover
	provided[failover.Takeover]("failover.Takeover"),             // wireFailover
	provided[failover.Service]("failover.Service"),               // wireFailover
}

// apiPorts are the fields of api.Deps that a node with a cluster identity must set, by the
// name the log uses.
var apiPorts = []struct {
	Name string
	Set  func(w *Wire) bool
}{
	{"api.Deps.Replicas", func(w *Wire) bool { return w.API.Replicas != nil }},
	{"api.Deps.Placement", func(w *Wire) bool { return w.API.Placement != nil }},
	{"api.Deps.LoadBalancers", func(w *Wire) bool { return w.API.LoadBalancers }},
	{"api.Deps.Failover", func(w *Wire) bool { return w.API.Failover != nil }},
}

// Off records that a feature of the cluster work is not running on this node, and why, and says so in
// the log. It is how a hook declines a port without hiding it: the reason is one that a person reading
// the log can act on, such as "the shared services run under systemd". Call it only for what does not run.
func (w *Wire) Off(feature, reason string) {
	if w.off == nil {
		w.off = map[string]string{}
	}
	w.off[feature] = reason
	w.Log.Warn("cluster feature off", "feature", feature, "reason", reason)
}

// offReason returns why feature was switched off, if it was.
func (w *Wire) offReason(feature string) (string, bool) {
	r, ok := w.off[feature]
	return r, ok
}

// clustered reports whether this node has a cluster: a hook provided the mesh.
func (w *Wire) clustered() bool {
	_, ok := Get[mesh.Mesh](w)
	return ok
}

// unaccounted lists the ports of a node with a cluster that are neither provided nor switched off with
// a reason. Nothing on a node that has no cluster.
func (w *Wire) unaccounted() []string {
	if !w.clustered() {
		return nil
	}
	var missing []string
	for _, p := range clusterPorts {
		if p.Has(w) {
			continue
		}
		if r, off := w.offReason(p.Name); off && strings.TrimSpace(r) != "" {
			continue
		}
		missing = append(missing, p.Name)
	}
	for _, p := range apiPorts {
		if p.Set(w) {
			continue
		}
		if r, off := w.offReason(p.Name); off && strings.TrimSpace(r) != "" {
			continue
		}
		missing = append(missing, p.Name)
	}
	sort.Strings(missing)
	return missing
}

// verifyPorts logs every port that unaccounted names. The daemon goes on: a node that lacks a feature
// still serves its projects, and the line tells the operator which feature that is.
func (w *Wire) verifyPorts() {
	for _, name := range w.unaccounted() {
		w.Log.Error("cluster port neither provided nor switched off: this is a defect in the daemon's wiring", "port", name)
	}
}
