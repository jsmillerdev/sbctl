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
	t.Cleanup(func() { SetFailoverSource(nil) })

	SetFailoverSource(nil)
	if rec := f.do("GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not part of a cluster") {
		t.Fatalf("without an orchestrator (a single server): %d %s", rec.Code, rec.Body)
	}

	SetFailoverSource(fakeFailover{r: failover.Readiness{
		Ready: false, Mode: "manual", Blockers: []string{"storage backend is file (objects exist only on this node)"},
		Notes: []string{"2 projects have no replica"}, ProjectsWithoutReplica: []string{"abcdefghijklmnopqrst"},
		Fencer: "aws", FencerStatus: "DryRun OK", EpochMarker: "store reachable",
	}})
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

	SetFailoverSource(fakeFailover{err: failover.ErrNoCluster})
	if rec := f.do("GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusNotFound {
		t.Errorf("single server: %d %s", rec.Code, rec.Body)
	}
	SetFailoverSource(fakeFailover{err: errors.New("the backup store timed out")})
	if rec := f.do("GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "timed out") {
		t.Errorf("error: %d %s", rec.Code, rec.Body)
	}
}

func TestFailoverReadinessIsForOwnersAndAdministrators(t *testing.T) {
	rf := newRolesFixture(t)
	t.Cleanup(func() { SetFailoverSource(nil) })
	SetFailoverSource(fakeFailover{r: failover.Readiness{Mode: "manual"}})
	want := map[string]int{"owner": 200, "admin": 200, "dev": 403, "ro": 403, "scoped": 403, "stranger": 403}
	for role, code := range want {
		if rec := rf.as(role, "GET", "/supavise/v1/failover/readiness", nil); rec.Code != code {
			t.Errorf("%s: %d, want %d (%s)", role, rec.Code, code, rec.Body)
		}
	}
}

func TestFailoverReadinessNeedsCredentials(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { SetFailoverSource(nil) })
	SetFailoverSource(fakeFailover{r: failover.Readiness{Mode: "manual"}})
	for _, token := range []string{"", "garbage"} {
		if rec := f.doAs(token, "GET", "/supavise/v1/failover/readiness", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", token, rec.Code)
		}
	}
}
