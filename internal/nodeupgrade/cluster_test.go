package nodeupgrade

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
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

// followerNode is testNode as a follower of a two-server cluster: the system project and the
// projects of the leader are not in its list (they are homed there), and projects are the ones
// homed here.
func followerNode(projects ...Project) *Node {
	n := testNode()
	n.Projects = projects
	n.Cluster = &ClusterView{Self: "n2", Leader: false, Elsewhere: []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"}}
	return n
}

func homed(ref, status string) Project {
	return Project{Ref: ref, Name: ref[:1], Status: status, LastBackup: t0.Add(-time.Hour),
		Versions: map[string]string{"gotrue": authOld, "postgrest": restOld, "postgres": pgOld}}
}

// A follower holds the registry read-only, so the rollout of projects, which writes upgrade rows,
// cannot run on it. A follower with no running project has none to roll out, and a new binary does
// not send it into one; the leader and a single server keep theirs.
func TestAFollowerWithoutProjectsHasNoRollout(t *testing.T) {
	for name, n := range map[string]*Node{
		"no project":     followerNode(),
		"a paused one":   followerNode(homed("cccccccccccccccccccc", "INACTIVE")),
		"a project left": followerNode(homed("cccccccccccccccccccc", "RESTORING")),
	} {
		p := BuildPlan(n, newInfo(), PlanOptions{})
		if p.Refusal != "" {
			t.Errorf("%s: refused: %s", name, p.Refusal)
		}
		if !p.BinaryChange || !p.NodeChanges() || p.Rollout() {
			t.Errorf("%s: binary change %v, node changes %v, rollout %v", name, p.BinaryChange, p.NodeChanges(), p.Rollout())
		}
		var out bytes.Buffer
		p.Render(&out)
		for _, want := range []string{"Projects restarted: none expected", "runs no project of its own", "No base backup is taken here"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: the plan lacks %q:\n%s", name, want, out.String())
			}
		}
		if strings.Contains(out.String(), "canary") {
			t.Errorf("%s: the plan promises a rollout:\n%s", name, out.String())
		}
	}
	if !BuildPlan(testNode(), newInfo(), PlanOptions{}).Rollout() || !BuildPlan(clusterNode("v1.1.0"), newInfo(), PlanOptions{}).Rollout() {
		t.Error("a single server and a leader run the rollout")
	}
}

// A follower with a running project of its own is refused before anything changes: the backup, the
// rollout and the restart of that project need a registry it can write. The refusal names the
// projects and the way out. A paused project does not count, and neither does a plan that changes
// nothing on the node.
func TestAFollowerThatRunsProjectsIsRefused(t *testing.T) {
	n := followerNode(homed("cccccccccccccccccccc", "ACTIVE_HEALTHY"), homed("dddddddddddddddddddd", "INACTIVE"))
	p := BuildPlan(n, newInfo(), PlanOptions{})
	for _, want := range []string{"follows the leader", "cannot write it", "1 project(s) run here: cccccccccccccccccccc", "supavise projects failover", "Nothing was changed"} {
		if !strings.Contains(p.Refusal, want) {
			t.Errorf("refusal lacks %q: %s", want, p.Refusal)
		}
	}
	if strings.Contains(p.Refusal, "dddddddddddddddddddd") {
		t.Errorf("the refusal names a paused project: %s", p.Refusal)
	}

	h := newFakeHost()
	h.node = n
	err := Run(context.Background(), h, runOpts(h))
	if code(t, err) != ExitRefused || !strings.Contains(err.Error(), "cccccccccccccccccccc") {
		t.Fatalf("err = %v", err)
	}
	for _, step := range []string{"prefetch", "backup", "install", "wait", "projects", "restore", "revert", "converge", "stack"} {
		if h.has(step) {
			t.Fatalf("%s ran after a refusal: %s", step, h.order())
		}
	}

	// The same server as the leader of its cluster upgrades its projects.
	leader := followerNode(homed("cccccccccccccccccccc", "ACTIVE_HEALTHY"))
	leader.Cluster.Leader = true
	if p := BuildPlan(leader, newInfo(), PlanOptions{}); p.Refusal != "" {
		t.Errorf("a leader: %s", p.Refusal)
	}

	// Nothing to change on the node: the plan has nothing to back up or restart either.
	same := followerNode(homed("cccccccccccccccccccc", "ACTIVE_HEALTHY"))
	same.Version, same.Pins = "v1.1.0", newPins()
	same.Projects[0].Versions = map[string]string{"gotrue": authNew, "postgrest": restNew, "postgres": pgOld}
	info := newInfo()
	if p := BuildPlan(same, info, PlanOptions{}); p.Refusal != "" || p.NodeChanges() {
		t.Errorf("a follower already on the release: refusal %q, node changes %v", p.Refusal, p.NodeChanges())
	}
}

// The upgrade of a follower without a project of its own goes through the sequence of any other
// node, without a backup (the leader takes the system project's) and without the rollout.
func TestUpgradeOfAFollowerWithoutProjects(t *testing.T) {
	h := newFakeHost()
	h.node = followerNode()
	if err := Run(context.Background(), h, runOpts(h)); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, h.out)
	}
	want := []string{
		"inspect", "resolve ", "stage",
		"prefetch",
		"install v1.0.0->v1.1.0 migrations 3/applied",
		"wait gotrue,realtime,storage,studio",
		"status",
		"end v1.1.0",
		"cleanup keep=3 current=v1.1.0",
		"discard",
	}
	if got := h.order(); got != strings.Join(want, " | ") {
		t.Fatalf("order:\n got %s\nwant %s", got, strings.Join(want, " | "))
	}
	if got := strings.Join(h.marks, " "); got != "preparing switching services verifying done" {
		t.Fatalf("marker phases = %s", got)
	}
	if strings.Contains(h.out.String(), "taking a base backup") || strings.Contains(h.out.String(), "upgrading 0 project") {
		t.Errorf("the run speaks of work it does not do:\n%s", h.out)
	}
}
