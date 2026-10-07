package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/health"
)

// fakeHealth is a HealthSource with a fixed report.
type fakeHealth struct {
	rep *health.Report
	err error
}

func (f fakeHealth) Report(context.Context) (*health.Report, error) { return f.rep, f.err }

func report(comps []health.Component, projects ...health.ProjectResult) *health.Report {
	r := &health.Report{CheckedAt: time.Now(), Version: "v9.9.9-secret", Components: comps, Projects: projects}
	r.Finish()
	return r
}

func healthzRequest(f *fixture, method string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/healthz", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	calledRoutes.Store("GET /healthz", true)
	return rec
}

func TestHealthzIsPublicAndTiny(t *testing.T) {
	f := newFixture(t)
	secretProject := health.ProjectResult{Ref: "payrollpayrollpayroll", Name: "payroll-db", State: health.Fail, Probed: true, Detail: "postgres refused"}
	cases := []struct {
		name string
		src  HealthSource
		code int
		body string
	}{
		{"healthy", fakeHealth{rep: report([]health.Component{{Name: "daemon", State: health.OK, Critical: true}})}, 200, `{"status":"healthy"}`},
		// A degraded node answers 200: a load balancer must not pull it for one project's PostgREST.
		{"degraded", fakeHealth{rep: report([]health.Component{{Name: "disk", State: health.Warn, Detail: "8% free"}}, secretProject)}, 200, `{"status":"degraded"}`},
		{"down", fakeHealth{rep: report([]health.Component{{Name: "system postgres", State: health.Fail, Critical: true, Detail: "unit failed"}}, secretProject)}, 503, `{"status":"down"}`},
		{"the check itself failed", fakeHealth{err: errors.New("registry hung: password=hunter2")}, 503, `{"status":"down"}`},
		{"no health source: liveness", nil, 200, `{"status":"healthy"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.srv.healthSrc = c.src
			rec := healthzRequest(f, "GET")
			if rec.Code != c.code || strings.TrimSpace(rec.Body.String()) != c.body {
				t.Errorf("%d %q, want %d %s", rec.Code, rec.Body.String(), c.code, c.body)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control %q", got)
			}
			if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
				t.Errorf("Content-Type %q", rec.Header().Get("Content-Type"))
			}
			// Nothing about the node leaks: no project, component, version or error text.
			for _, leak := range []string{"payroll", "hunter2", "v9.9.9", "postgres", "disk", "8%", "registry", "components", "projects"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("/healthz leaks %q: %s", leak, rec.Body)
				}
			}
		})
	}
	f.srv.healthSrc = fakeHealth{rep: report(nil)}
	if rec := healthzRequest(f, "HEAD"); rec.Code != 200 { // net/http drops the body of a HEAD answer
		t.Errorf("HEAD: %d", rec.Code)
	}
	// Writes are not served.
	req := httptest.NewRequest("POST", "/healthz", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Errorf("POST /healthz answered 200")
	}
}

func TestHealthzNeedsNoCredentialsButDetailDoes(t *testing.T) {
	f := newFixture(t)
	f.srv.healthSrc = fakeHealth{rep: report(nil)}
	if rec := f.doAs("", "GET", "/healthz/detail", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("detail without credentials: %d", rec.Code)
	}
	if rec := f.doAs("garbage", "GET", "/healthz/detail", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("detail with a bad token: %d", rec.Code)
	}
	if rec := f.do("GET", "/healthz/detail", nil); rec.Code != 200 {
		t.Errorf("detail as the owner: %d %s", rec.Code, rec.Body)
	}
}

func TestHealthDetailIsForOwnersAndAdministrators(t *testing.T) {
	rf := newRolesFixture(t)
	rf.srv.healthSrc = fakeHealth{rep: report([]health.Component{{Name: "daemon", State: health.OK, Critical: true}})}
	want := map[string]int{"owner": 200, "admin": 200, "dev": 403, "ro": 403, "scoped": 403, "stranger": 403}
	for role, code := range want {
		if rec := rf.as(role, "GET", "/healthz/detail", nil); rec.Code != code {
			t.Errorf("%s: %d, want %d (%s)", role, rec.Code, code, rec.Body)
		}
	}
	if rec := rf.as("dev", "GET", "/healthz/detail", nil); !strings.Contains(rec.Body.String(), "Owner or Administrator") {
		t.Errorf("the refusal does not say who may: %s", rec.Body)
	}
}

func TestHealthDetailShowsOnlyTheCallersOrganizations(t *testing.T) {
	rf := newRolesFixture(t)
	other, err := rf.reg.CreateOrganization(context.Background(), "finance", "Finance")
	if err != nil {
		t.Fatal(err)
	}
	mine := health.ProjectResult{Ref: testRef, Name: "First project", OrgID: rf.org.ID, Status: "ACTIVE_HEALTHY", State: health.OK, Probed: true}
	theirs := health.ProjectResult{Ref: "payrollpayrollpayroll", Name: "payroll-db", OrgID: other.ID, Status: "ACTIVE_UNHEALTHY", State: health.Fail, Probed: true, Detail: "postgres refused"}
	rf.srv.healthSrc = fakeHealth{rep: report([]health.Component{{Name: "daemon", State: health.OK, Critical: true}, {Name: "disk", State: health.Warn, Detail: "8% free"}}, mine, theirs)}
	for _, role := range []string{"owner", "admin"} {
		rec := rf.as(role, "GET", "/healthz/detail", nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", role, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		for _, leak := range []string{"payroll", "postgres refused"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s sees another organization's project (%q): %s", role, leak, body)
			}
		}
		if !strings.Contains(body, testRef) || !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, "8% free") {
			t.Errorf("%s: the report lost what the caller may see: %s", role, body)
		}
		if strings.Contains(body, "org_id") || strings.Contains(body, "OrgID") {
			t.Errorf("the organization id leaks: %s", body)
		}
	}
}

func TestHealthDetailWithoutASourceIsUnavailable(t *testing.T) {
	f := newFixture(t)
	if rec := f.do("GET", "/healthz/detail", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	f.srv.healthSrc = fakeHealth{err: errors.New("boom")}
	if rec := f.do("GET", "/healthz/detail", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}
