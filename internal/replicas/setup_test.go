package replicas

import (
	"errors"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/registry"
)

// The refusals of design 2.7.2, word for word: Studio shows them as they are.
func TestSetupRefusals(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(e *env)
		ref     string
		region  string
		want    string
	}{
		{"no server in the region", nil, refA, "sa-east-1", "No Supavise server is joined in sa-east-1."},
		{"a region that is no region", nil, refA, "mars-1", "No Supavise server is joined in mars-1."},
		{"the only server there is the home", nil, refA, "us-east-1", "Read replicas on the same server as the primary are not offered."},
		{"a server that is still joining does not count", func(e *env) {
			n := &registry.Node{ID: "n4", Name: "new", Region: "sa-east-1", State: registry.NodeJoining}
			if err := e.reg.CreateNode(e.ctx, n); err != nil {
				e.t.Fatal(err)
			}
		}, refA, "sa-east-1", "No Supavise server is joined in sa-east-1."},
		{"a replica is there already", func(e *env) {
			if err := e.ctrl.Setup(e.ctx, refA, "eu-west-1"); err != nil {
				e.t.Fatal(err)
			}
		}, refA, "eu-west-1", "This project already has a replica on eu."},
		{"a size below small", func(e *env) { e.addProject(refC, "micro") }, refC, "eu-west-1", "Read replicas need a compute size of small or larger."},
		{"the smallest size", func(e *env) { e.addProject(refC, "nano") }, refC, "eu-west-1", "Read replicas need a compute size of small or larger."},
		{"no room on the server", func(e *env) { e.admit.full["n2"] = true }, refA, "eu-west-1", "Not enough capacity on eu."},
		{"a file backend", func(e *env) { e.cfg.Backup.Backend = "file:///var/lib/supavise/backups" }, refA, "eu-west-1", "Read replicas need S3-compatible backup storage."},
		{"a branch", func(e *env) {
			b := &registry.Project{Ref: refC, Name: "b", Class: "small", Status: registry.StatusActiveHealthy,
				Branch: &registry.BranchInfo{ID: "b1", ParentRef: refA, Name: "feature"}}
			if err := e.reg.CreateProject(e.ctx, b); err != nil {
				e.t.Fatal(err)
			}
		}, refC, "eu-west-1", "Read replicas are not offered for branches."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.prepare != nil {
				tt.prepare(e)
			}
			before := len(mustList(e, tt.ref))
			if got := e.user(e.ctrl.Setup(e.ctx, tt.ref, tt.region)); got != tt.want {
				t.Fatalf("refused with %q, want %q", got, tt.want)
			}
			if after := len(mustList(e, tt.ref)); after != before {
				t.Fatalf("a refused request changed the rows from %d to %d", before, after)
			}
		})
	}
}

// The port ranges: the host's replica_base must leave room, and a project whose port number is
// past the last that fits gets no replica (config.CheckReplicaPorts, config.MaxReplicaSeq).
func TestSetupRefusesPortsThatDoNotFit(t *testing.T) {
	e := newEnv(t)
	e.cfg.Ports.ReplicaBase = 80
	if got := e.user(e.ctrl.Setup(e.ctx, refA, "eu-west-1")); !strings.Contains(got, "ports.replica_base 80 out of range") {
		t.Fatalf("refused with %q", got)
	}
	e = newEnv(t)
	e.cfg.Ports.ReplicaBase = e.cfg.Ports.ProjectBase - 3*2 - 3 // room for the replicas of two projects
	e.addProject(refC, "small")                                 // the third project
	if err := e.ctrl.Setup(e.ctx, refB, "eu-west-1"); err != nil {
		t.Fatalf("project 2 fits: %v", err)
	}
	got := e.user(e.ctrl.Setup(e.ctx, refC, "eu-west-1"))
	if !strings.Contains(got, "its port number 3 is above 2") {
		t.Fatalf("refused with %q", got)
	}
	// The default does not make rows for it either.
	e.cfg.Replicas.Default = "all"
	e.tick(1)
	if e.hasReplica(refC, "n2") {
		t.Fatal("the default made a replica of a project past the replica range")
	}
}

func mustList(e *env, ref string) []registry.Replica {
	e.t.Helper()
	rs, err := e.reg.ListReplicas(e.ctx, ref)
	if err != nil {
		e.t.Fatal(err)
	}
	return rs
}

func TestSetupOnRefusals(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(e *env)
		node    string
		want    string
	}{
		{"unknown node", nil, "n9", `There is no Supavise server "n9" in this cluster.`},
		{"the home", nil, "n1", "Read replicas on the same server as the primary are not offered."},
		{"the home by name", nil, "primary", "Read replicas on the same server as the primary are not offered."},
		{"a replica is there already", func(e *env) {
			if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
				e.t.Fatal(err)
			}
		}, "eu", "This project already has a replica on eu."},
		{"a server that is not active", func(e *env) {
			if err := e.reg.SetNodeState(e.ctx, "n3", registry.NodeFenced); err != nil {
				e.t.Fatal(err)
			}
		}, "n3", "The server ap has not finished joining the cluster."},
		{"no room", func(e *env) { e.admit.full["n3"] = true }, "n3", "Not enough capacity on ap."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.prepare != nil {
				tt.prepare(e)
			}
			if got := e.user(e.ctrl.SetupOn(e.ctx, refA, tt.node)); got != tt.want {
				t.Fatalf("refused with %q, want %q", got, tt.want)
			}
		})
	}
	e := newEnv(t)
	if err := e.ctrl.SetupOn(e.ctx, refA, "eu"); err != nil { // by name
		t.Fatal(err)
	}
	if r := e.replica(refA, "n2"); r.NodeID != "n2" {
		t.Fatalf("row %+v", r)
	}
	if err := e.ctrl.Setup(e.ctx, "zzzzzzzzzzzzzzzzzzzz", "eu-west-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// Hosted allows none below small, four up to large and five above; a project also cannot have
// more replicas than there are other servers.
func TestMaxReplicas(t *testing.T) {
	for class, want := range map[string]int{"nano": 0, "micro": 0, "pico": 0, "small": 4, "medium": 4, "large": 4, "xlarge": 5, "XL": 5, "2xlarge": 5, "16xlarge": 5, "bogus": 0} {
		if got := MaxReplicas(class); got != want {
			t.Errorf("MaxReplicas(%q) = %d, want %d", class, got, want)
		}
	}
	e := newEnv(t)
	regions := []string{"eu-west-2", "eu-west-3", "eu-central-1", "ca-central-1"}
	for i, r := range regions {
		e.addNode("n"+string(rune('4'+i)), "x"+string(rune('4'+i)), r)
	}
	e.addProject(refC, "xlarge")
	// refA is small: four replicas, the fifth refused. n2 and n3 plus four more nodes are non-home.
	for _, r := range append([]string{"eu-west-1", "ap-south-1"}, regions[:2]...) {
		if err := e.ctrl.Setup(e.ctx, refA, r); err != nil {
			t.Fatalf("%s: %v", r, err)
		}
	}
	if got := e.user(e.ctrl.Setup(e.ctx, refA, regions[2])); got != "The project already has the maximum of 4 read replicas." {
		t.Fatalf("fifth replica of a small project: %q", got)
	}
	// The xlarge project takes five.
	for _, r := range append([]string{"eu-west-1", "ap-south-1"}, regions[:3]...) {
		if err := e.ctrl.Setup(e.ctx, refC, r); err != nil {
			t.Fatalf("%s: %v", r, err)
		}
	}
	// A replica that is going down does not count against the limit.
	if err := e.ctrl.Remove(e.ctx, refA, e.replica(refA, "n2").Identifier); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.Setup(e.ctx, refA, regions[2]); err != nil {
		t.Fatalf("after a removal: %v", err)
	}
}

// A project cannot have more replicas than there are other servers: replicas on servers that
// are no longer active still count.
func TestMaxReplicasIsCappedByTheServers(t *testing.T) {
	e := newEnv(t)
	e.addNode("n4", "gone", "eu-west-2")
	if err := e.ctrl.Setup(e.ctx, refA, "eu-west-2"); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.SetNodeState(e.ctx, "n4", registry.NodeFenced); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.Setup(e.ctx, refA, "eu-west-1"); err != nil {
		t.Fatalf("one replica, two other active servers: %v", err)
	}
	if got := e.user(e.ctrl.Setup(e.ctx, refA, "ap-south-1")); got != "The project already has the maximum of 2 read replicas." {
		t.Fatalf("refused with %q", got)
	}
}

// The least loaded of several servers in a region gets the replica; a tie goes to the lower id.
func TestSetupPicksTheLeastLoadedServer(t *testing.T) {
	e := newEnv(t)
	e.addNode("n4", "eu-b", "eu-west-1")
	if err := e.ctrl.Setup(e.ctx, refA, "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	if !e.hasReplica(refA, "n2") {
		t.Fatal("a tie must go to n2")
	}
	if err := e.ctrl.Setup(e.ctx, refB, "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	if !e.hasReplica(refB, "n4") {
		t.Fatalf("n2 holds a replica already, n4 is the lighter: %+v", mustList(e, refB))
	}
}

// A replica that was removed as a default one is wanted again when someone asks for it.
func TestSetupClearsTheOptout(t *testing.T) {
	e := newEnv(t)
	if err := e.reg.PutReplicaOptout(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.Setup(e.ctx, refA, "eu-west-1"); err != nil {
		t.Fatal(err)
	}
	if os, _ := e.reg.ListReplicaOptouts(e.ctx); len(os) != 0 {
		t.Fatalf("opt-outs: %+v", os)
	}
}

// A request for a node's region uses the region's code in the identifier.
func TestSetupWritesTheRowAndKicksTheController(t *testing.T) {
	e := newEnv(t)
	if err := e.ctrl.Setup(e.ctx, refB, "ap-south-1"); err != nil {
		t.Fatal(err)
	}
	r := e.replica(refB, "n3")
	if !registry.ValidReplicaIdentifier(r.Identifier) || !strings.Contains(r.Identifier, "-rr-ap-south-1-") {
		t.Fatalf("identifier %q", r.Identifier)
	}
	l, err := e.ctrl.List(e.ctx, refB)
	if err != nil || len(l) != 1 || l[0].Region != "ap-south-1" || l[0].PublicHost != "ap.example.com" {
		t.Fatalf("list: %+v %v", l, err)
	}
	select {
	case <-e.ctrl.wake:
	default:
		t.Fatal("the request did not wake the controller")
	}
}
