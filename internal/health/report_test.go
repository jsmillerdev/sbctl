package health

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestVerdictAndExitCodes(t *testing.T) {
	ok := Component{Name: "daemon", State: OK, Critical: true}
	cases := []struct {
		name       string
		components []Component
		projects   []ProjectResult
		want       Verdict
		exit       int
	}{
		{"empty node", nil, nil, Healthy, 0},
		{"all ok", []Component{ok, {Name: "disk", State: OK}}, []ProjectResult{{Ref: "a", State: OK, Probed: true}}, Healthy, 0},
		{"info does not change the verdict", []Component{ok, {Name: "key escrow", State: Info, Detail: "not backed up"}}, nil, Healthy, 0},
		{"a warning degrades", []Component{ok, {Name: "disk", State: Warn, Detail: "8% free"}}, nil, Degraded, 1},
		{"a non-critical failure degrades", []Component{ok, {Name: "realtime", State: Fail, Detail: "unit is failed"}}, nil, Degraded, 1},
		{"a critical failure is down", []Component{{Name: "system postgres", State: Fail, Critical: true}}, nil, Down, 2},
		{"critical wins over warnings", []Component{{Name: "disk", State: Warn}, {Name: "daemon", State: Fail, Critical: true}}, nil, Down, 2},
		{"a failed project degrades", []Component{ok}, []ProjectResult{{Ref: "a", State: Fail}}, Degraded, 1},
		{"a project warning degrades", []Component{ok}, []ProjectResult{{Ref: "a", State: Warn}}, Degraded, 1},
		{"a busy project does not", []Component{ok}, []ProjectResult{{Ref: "a", State: Info, Status: "COMING_UP"}}, Healthy, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Report{Components: c.components, Projects: c.projects}
			r.Finish()
			if r.Verdict != c.want || r.Verdict.ExitCode() != c.exit {
				t.Errorf("verdict %q (exit %d), want %q (exit %d); summary %q", r.Verdict, r.Verdict.ExitCode(), c.want, c.exit, r.Summary)
			}
			if !strings.HasPrefix(r.Summary, string(c.want)+":") {
				t.Errorf("summary %q does not lead with the verdict", r.Summary)
			}
			if strings.Contains(r.Summary, "\n") {
				t.Errorf("summary is not one line: %q", r.Summary)
			}
		})
	}
}

func TestSummaryNamesTheWorstFirstAndCaps(t *testing.T) {
	r := &Report{
		Components: []Component{
			{Name: "disk", State: Warn, Detail: "8% free"},
			{Name: "system postgres", State: Fail, Critical: true, Detail: "unit is failed"},
			{Name: "realtime", State: Fail, Detail: "unit is failed"},
			{Name: "certificates", State: Warn, Detail: "expires in 3d"},
		},
		Projects: []ProjectResult{{Ref: "abc", State: Fail}},
	}
	r.Finish()
	if r.Verdict != Down || !strings.HasPrefix(r.Summary, "down: system postgres: unit is failed") {
		t.Errorf("summary %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "and 2 more") {
		t.Errorf("a long list is not capped: %q", r.Summary)
	}
}

func TestHealthySummaryCountsProjects(t *testing.T) {
	r := &Report{Projects: []ProjectResult{
		{Ref: "a", State: OK, Probed: true}, {Ref: "b", State: OK, Probed: true}, {Ref: "c", State: OK, Status: "INACTIVE"},
	}}
	r.Finish()
	if r.Summary != "healthy: 2 projects answering, 1 paused" {
		t.Errorf("summary %q", r.Summary)
	}
	r = &Report{Projects: []ProjectResult{{Ref: "a", State: OK, Probed: true}}}
	r.Finish()
	if r.Summary != "healthy: 1 project answering" {
		t.Errorf("summary %q", r.Summary)
	}
}

// The detailed view of an Administrator of one organization must not name another's project,
// and must not look healthier than the node is.
func TestFilterProjectsHidesWhatTheReaderMaySeeNothingOf(t *testing.T) {
	r := &Report{
		CheckedAt:  time.Now(),
		Components: []Component{{Name: "daemon", State: OK, Critical: true}},
		Projects: []ProjectResult{
			{Ref: "mineaaaaaaaaaaaaaaaa", Name: "mine", OrgID: 1, State: OK, Probed: true},
			{Ref: "secretbbbbbbbbbbbbbb", Name: "payroll", OrgID: 2, State: Fail, Probed: true, Detail: "postgres down"},
		},
	}
	r.Finish()
	if r.Verdict != Degraded {
		t.Fatalf("verdict %q", r.Verdict)
	}
	mine := r.FilterProjects(func(p ProjectResult) bool { return p.OrgID == 1 })
	b, _ := json.Marshal(mine)
	for _, leak := range []string{"payroll", "secretbbbbbbbbbbbbbb", "postgres down"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("filtered report leaks %q: %s", leak, b)
		}
	}
	if mine.Verdict != Degraded {
		t.Errorf("verdict improved to %q because a project was hidden", mine.Verdict)
	}
	if !strings.HasPrefix(mine.Summary, "degraded:") {
		t.Errorf("summary %q", mine.Summary)
	}
	if len(r.Projects) != 2 {
		t.Error("FilterProjects changed the original")
	}
}

func TestPublicShapeIsOnlyTheVerdict(t *testing.T) {
	b, _ := json.Marshal(Public{Status: Degraded})
	if string(b) != `{"status":"degraded"}` {
		t.Errorf("public body %s", b)
	}
}

func TestRenderIsShort(t *testing.T) {
	r := &Report{
		Components: []Component{{Name: "daemon", State: OK, Critical: true, Detail: "this process"}, {Name: "disk", State: Warn, Detail: "8% free"}},
		Projects: []ProjectResult{
			{Ref: "okokokokokokokokokok", Status: "ACTIVE_HEALTHY", State: OK, Probed: true},
			{Ref: "badbadbadbadbadbadba", Status: "ACTIVE_UNHEALTHY", State: Fail, Probed: true, Detail: "postgrest connection refused"},
		},
	}
	r.Finish()
	var b strings.Builder
	Render(&b, r, false)
	out := b.String()
	if !strings.HasPrefix(out, "degraded: ") {
		t.Errorf("the first line is not the verdict:\n%s", out)
	}
	if strings.Contains(out, "okokokokokokokokokok") {
		t.Errorf("a healthy project is listed without -v:\n%s", out)
	}
	if !strings.Contains(out, "badbadbadbadbadbadba") || !strings.Contains(out, "postgrest connection refused") {
		t.Errorf("the failing project is missing:\n%s", out)
	}
	b.Reset()
	Render(&b, r, true)
	if !strings.Contains(b.String(), "okokokokokokokokokok") {
		t.Errorf("-v does not list every project:\n%s", b.String())
	}
}
