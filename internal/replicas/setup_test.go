package replicas

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
		{"the system project", nil, "system", "eu-west-1", msgSystem},
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
		}, "n3", "The server ap is fenced and cannot take a replica."},
		{"a server that is joining", func(e *env) {
			if err := e.reg.SetNodeState(e.ctx, "n3", registry.NodeJoining); err != nil {
				e.t.Fatal(err)
			}
		}, "n3", "The server ap has not finished joining the cluster."},
		{"a server that left", func(e *env) {
			if err := e.reg.SetNodeState(e.ctx, "n3", registry.NodeLeft); err != nil {
				e.t.Fatal(err)
			}
		}, "n3", "The server ap has left the cluster."},
		{"the replica there is going down", func(e *env) {
			if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
				e.t.Fatal(err)
			}
			if err := e.ctrl.Remove(e.ctx, refA, e.replica(refA, "n2").Identifier); err != nil {
				e.t.Fatal(err)
			}
		}, "eu", "The replica on eu is being removed; try again when it is gone."},
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

// Studio's recovery drops a failed replica and adds it again. While the old row is going down the
// answer says so, by region as by node; once it is gone the new one is accepted.
func TestSetupWhileTheOldReplicaGoesDown(t *testing.T) {
	e := newEnv(t)
	e.nodes.down["n2"] = true // the removal cannot finish
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.Remove(e.ctx, refA, e.replica(refA, "n2").Identifier); err != nil {
		t.Fatal(err)
	}
	// Only n2 is in eu-west-1.
	if got := e.user(e.ctrl.Setup(e.ctx, refA, "eu-west-1")); got != "The replica on eu is being removed; try again when it is gone." {
		t.Fatalf("by region: %q", got)
	}
	e.nodes.down["n2"] = false
	e.tick(1)
	e.clock.Advance(3 * time.Minute)
	e.tick(1)
	if err := e.ctrl.Setup(e.ctx, refA, "eu-west-1"); err != nil {
		t.Fatalf("after the removal: %v", err)
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

// Requests that arrive together, as Studio's double click and the API beside `supavise replicas add`
// do, take turns: a small project may have four replicas, and of eight requests four are accepted.
func TestConcurrentSetupsRespectTheCap(t *testing.T) {
	e := newEnv(t)
	targets := []string{"n2", "n3"}
	for _, id := range []string{"n4", "n5", "n6", "n7", "n8", "n9"} {
		e.addNode(id, "x"+id, "region-"+id)
		targets = append(targets, id)
	}
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, n := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e.ctrl.SetupOn(e.ctx, refA, n)
		}()
	}
	wg.Wait()
	accepted := 0
	for _, err := range errs {
		if err == nil {
			accepted++
		} else if got := e.user(err); got != "The project already has the maximum of 4 read replicas." {
			t.Fatalf("refused with %q", got)
		}
	}
	if rows := mustList(e, refA); accepted != 4 || len(rows) != 4 {
		t.Fatalf("%d requests accepted, %d rows", accepted, len(rows))
	}
}

// Two processes share the registry and not the controller's lock. When both pass the count and
// both insert, whoever looks after the other's row has gone in sees the excess and withdraws
// its own: the project is never over its cap. Both may withdraw, and a retry then succeeds.
func TestSetupsOfTwoProcessesNeverExceedTheCap(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Registry = &hookRegistry{Registry: o.Registry} })
	h := e.opts.Registry.(*hookRegistry)
	other := New(e.opts) // the second process
	for _, id := range []string{"n4", "n5", "n6"} {
		e.addNode(id, "x"+id, "region-"+id)
	}
	for _, n := range []string{"n2", "n3", "n4"} { // three of four places taken
		if err := e.ctrl.SetupOn(e.ctx, refA, n); err != nil {
			t.Fatal(err)
		}
	}
	// Each request waits until the other has counted, then until the other has inserted its row.
	counted, inserted := barrier(2), barrier(2)
	h.beforeAdd, h.afterAdd = counted.wait, inserted.wait
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = e.ctrl.SetupOn(e.ctx, refA, "n5") }()
	go func() { defer wg.Done(); errs[1] = other.SetupOn(e.ctx, refA, "n6") }()
	wg.Wait()
	if counted.arrived() != 2 || inserted.arrived() != 2 {
		t.Fatalf("%d requests counted and %d inserted, the test needs both", counted.arrived(), inserted.arrived())
	}
	refused := 0
	for i, err := range errs {
		if err == nil {
			continue
		}
		refused++
		if got := e.user(err); got != "The project already has the maximum of 4 read replicas." {
			t.Fatalf("request %d refused with %q", i, got)
		}
	}
	rows := mustList(e, refA)
	if refused == 0 || len(rows) > 4 || len(rows) != 5-refused {
		t.Fatalf("%d requests refused, %d rows: %+v", refused, len(rows), rows)
	}
}

// A failover that moves the project's home onto the chosen server after the checks is caught by
// the registry when it inserts the row (I2), and the request is refused in the words for it.
func TestSetupRefusesWhenTheProjectMovedOntoTheChosenServer(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Registry = &hookRegistry{Registry: o.Registry} })
	h := e.opts.Registry.(*hookRegistry)
	cl, err := e.reg.GetCluster(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.beforeAdd = func() {
		if err := e.reg.SetProjectNode(e.ctx, refA, "n2", cl.Epoch); err != nil {
			t.Error(err)
		}
	}
	if got := e.user(e.ctrl.Setup(e.ctx, refA, "eu-west-1")); got != msgSameServer {
		t.Fatalf("refused with %q", got)
	}
	if rows := mustList(e, refA); len(rows) != 0 {
		t.Fatalf("rows: %+v", rows)
	}
}

// gate lets n callers of wait through together, or none after five seconds.
type gate struct {
	mu   sync.Mutex
	n    int
	open chan struct{}
	seen int
}

func barrier(n int) *gate { return &gate{n: n, open: make(chan struct{})} }

func (g *gate) wait() {
	g.mu.Lock()
	if g.seen++; g.seen == g.n {
		close(g.open)
	}
	g.mu.Unlock()
	select {
	case <-g.open:
	case <-time.After(5 * time.Second):
	}
}

func (g *gate) arrived() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen
}
