package nodeupgrade

import (
	"bytes"
	"strings"
	"testing"
)

// clusterNode is testNode as the leader of a two-server cluster: n2 holds a standby of the system
// cluster and of project a, and runs peerVersion.
func clusterNode(peerVersion string) *Node {
	n := testNode()
	n.Cluster = &ClusterView{Self: "n1", Leader: true, Elsewhere: []string{"dddddddddddddddddddd"},
		Standbys: []Standby{{Node: "n2", Name: "second", Version: peerVersion, Refs: []string{"aaaaaaaaaaaaaaaaaaaa", "system"}}}}
	return n
}

func walInfo() *Info {
	i := newInfo()
	i.Pins["postgres"] = pgNew
	i.WALIncompatible = true
	return i
}

// A release that keeps no WAL compatibility waits for the servers that hold standbys of this
// server's databases: they run it first.
func TestPlanWaitsForTheStandbysWhenThePostgresReleaseKeepsNoWALCompatibility(t *testing.T) {
	p := BuildPlan(clusterNode("v1.0.0"), walInfo(), PlanOptions{})
	for _, want := range []string{"wal_compat: false", "n2 (second) runs v1.0.0 and holds standbys of aaaaaaaaaaaaaaaaaaaa, system", "sudo supavise upgrade", "supavise projects failover"} {
		if !strings.Contains(p.Refusal, want) {
			t.Errorf("refusal lacks %q: %s", want, p.Refusal)
		}
	}

	// The standby is on the release: this server goes next.
	if p := BuildPlan(clusterNode("v1.1.0"), walInfo(), PlanOptions{}); p.Refusal != "" {
		t.Errorf("the standbys are on the release already: %s", p.Refusal)
	}
	// Ahead of it is no reason to wait either.
	if p := BuildPlan(clusterNode("v1.2.0"), walInfo(), PlanOptions{}); p.Refusal != "" {
		t.Errorf("a standby ahead of the release: %s", p.Refusal)
	}
	// A node that never said which release it runs may be behind; a build that is not a release
	// cannot be judged.
	if p := BuildPlan(clusterNode(""), walInfo(), PlanOptions{}); !strings.Contains(p.Refusal, "an unknown release") {
		t.Errorf("a standby of unknown version: %q", p.Refusal)
	}
	if p := BuildPlan(clusterNode("dev"), walInfo(), PlanOptions{}); p.Refusal != "" {
		t.Errorf("a standby on a dev build: %s", p.Refusal)
	}
}

// Nothing waits when the release keeps the WAL format, when PostgreSQL does not move, or on a
// server that is not in a cluster; and a project's PostgreSQL counts only when it moves too.
func TestPlanDoesNotWaitWhenNothingNeedsTheOrder(t *testing.T) {
	compat := walInfo()
	compat.WALIncompatible = false
	if p := BuildPlan(clusterNode("v1.0.0"), compat, PlanOptions{}); p.Refusal != "" {
		t.Errorf("a WAL compatible release: %s", p.Refusal)
	}
	noPG := newInfo() // the same PostgreSQL as the node's pins
	noPG.Pins["postgres"] = pgOld
	noPG.WALIncompatible = true
	if p := BuildPlan(clusterNode("v1.0.0"), noPG, PlanOptions{}); p.Refusal != "" {
		t.Errorf("PostgreSQL does not move: %s", p.Refusal)
	}
	single := testNode()
	if p := BuildPlan(single, walInfo(), PlanOptions{}); p.Refusal != "" {
		t.Errorf("a single server: %s", p.Refusal)
	}

	// The projects' PostgreSQL moves only with --include-postgres; the system cluster's always does,
	// so a node whose system PostgreSQL is already on the release waits for the projects' alone.
	n := clusterNode("v1.0.0")
	n.Pins["postgres"] = pgNew
	for i := range n.Projects {
		if n.Projects[i].Ref != "system" {
			n.Projects[i].Versions["postgres"] = pgOld
		}
	}
	if p := BuildPlan(n, walInfo(), PlanOptions{}); p.Refusal != "" {
		t.Errorf("only the projects' PostgreSQL differs and it is held back: %s", p.Refusal)
	}
	if p := BuildPlan(n, walInfo(), PlanOptions{IncludePostgres: true}); !strings.Contains(p.Refusal, "wal_compat: false") {
		t.Errorf("the projects' PostgreSQL moves: %q", p.Refusal)
	}
}

// A follower leaves the system project's backup to the leader, and the plan names the projects
// that other servers upgrade.
func TestPlanInAClusterBacksUpWhatRunsHere(t *testing.T) {
	leader := clusterNode("v1.1.0")
	if got := strings.Join(BackupRefs(leader), ","); got != "system,aaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("leader refs = %s", got)
	}
	follower := clusterNode("v1.1.0")
	follower.Cluster.Leader = false
	if got := strings.Join(BackupRefs(follower), ","); got != "aaaaaaaaaaaaaaaaaaaa,bbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("follower refs = %s (a standby of the system cluster has no backup of its own)", got)
	}
	p := BuildPlan(leader, newInfo(), PlanOptions{})
	var out bytes.Buffer
	p.Render(&out)
	if !strings.Contains(out.String(), "1 project(s) are homed on other servers of the cluster (dddddddddddddddddddd)") {
		t.Errorf("the plan does not name the projects of other servers:\n%s", out.String())
	}
	var single bytes.Buffer
	BuildPlan(testNode(), newInfo(), PlanOptions{}).Render(&single)
	if strings.Contains(single.String(), "other servers of the cluster") {
		t.Errorf("a single server's plan mentions a cluster:\n%s", single.String())
	}
}
