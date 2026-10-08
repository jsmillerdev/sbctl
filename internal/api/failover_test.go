package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/failover"
)

type fakeFailover struct {
	r   failover.Readiness
	err error
}

func (f fakeFailover) Readiness(context.Context) (failover.Readiness, error) { return f.r, f.err }

func TestFailoverReadinessRoute(t *testing.T) {
	f := newFixture(t)

	// A server that is not in a cluster has no orchestrator: nothing to fail over to.
	if rec := f.do("GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("without an orchestrator: %d %s", rec.Code, rec.Body)
	}

	f.srv.failover = fakeFailover{r: failover.Readiness{
		Ready: false, Mode: "manual", Blockers: []string{"storage backend is file (objects exist only on this node)"},
		Notes: []string{"2 projects have no replica"}, ProjectsWithoutReplica: []string{"abcdefghijklmnopqrst"},
		Fencer: "aws", FencerStatus: "DryRun OK", EpochMarker: "store reachable",
	}}
	rec := f.do("GET", "/supavise/v1/failover/readiness", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`"ready":false`, `"mode":"manual"`, `"blockers":["storage backend is file`, `"fencer":"aws"`, `"fencer_status":"DryRun OK"`, `"epoch_marker":"store reachable"`, `"projects_without_replica":["abcdefghijklmnopqrst"]`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body lacks %s: %s", want, rec.Body)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", rec.Header().Get("Cache-Control"))
	}

	f.srv.failover = fakeFailover{err: failover.ErrNoCluster}
	if rec := f.do("GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusNotFound {
		t.Errorf("single server: %d %s", rec.Code, rec.Body)
	}
	f.srv.failover = fakeFailover{err: errors.New("the backup store timed out")}
	if rec := f.do("GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "timed out") {
		t.Errorf("error: %d %s", rec.Code, rec.Body)
	}
}

func TestFailoverReadinessIsForOwnersAndAdministrators(t *testing.T) {
	rf := newRolesFixture(t)
	rf.srv.failover = fakeFailover{r: failover.Readiness{Mode: "manual"}}
	want := map[string]int{"owner": 200, "admin": 200, "dev": 403, "ro": 403, "scoped": 403, "stranger": 403}
	for role, code := range want {
		if rec := rf.as(role, "GET", "/supavise/v1/failover/readiness", nil); rec.Code != code {
			t.Errorf("%s: %d, want %d (%s)", role, rec.Code, code, rec.Body)
		}
	}
}

func TestFailoverReadinessNeedsCredentials(t *testing.T) {
	f := newFixture(t)
	f.srv.failover = fakeFailover{r: failover.Readiness{Mode: "manual"}}
	for _, token := range []string{"", "garbage"} {
		if rec := f.doAs(token, "GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", token, rec.Code)
		}
	}
}

// The daemon hands the cluster's collaborators to the server through Deps, not through
// package-level settings: two servers in one process do not share them, and a server built
// without them answers as a single server does.
func TestNewServerTakesTheClusterDeps(t *testing.T) {
	f := newFixture(t)
	d := Deps{Registry: f.reg, Secrets: f.srv.sec, Manager: f.mgr, Config: f.cfg, Store: NewMemoryStore(),
		Replicas: &fakeReplicas{reg: f.reg}, LoadBalancers: true, Failover: fakeFailover{r: failover.Readiness{Mode: "manual"}}}
	s, err := NewServer(d)
	if err != nil {
		t.Fatal(err)
	}
	if s.replicas == nil || !s.lbOn || s.failover == nil || s.placement == nil {
		t.Fatalf("deps not taken: replicas %v, load balancers %v, failover %v, placement %v", s.replicas, s.lbOn, s.failover, s.placement)
	}
	bare, err := NewServer(Deps{Registry: f.reg, Secrets: f.srv.sec, Manager: f.mgr, Config: f.cfg, Store: NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	if bare.replicas != nil || bare.lbOn || bare.failover != nil {
		t.Fatalf("a server with no cluster deps has replicas %v, load balancers %v, failover %v", bare.replicas, bare.lbOn, bare.failover)
	}
}
