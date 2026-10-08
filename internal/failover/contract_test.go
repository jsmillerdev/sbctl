package failover

import (
	"strings"
	"testing"
)

func TestReadinessRender(t *testing.T) {
	var b strings.Builder
	Readiness{
		Blockers: []string{"storage backend is file (objects exist only on this node)"},
		Notes:    []string{"2 projects have no replica (--restore-missing: RPO up to archive_timeout)"},
		Fencer:   "aws", FencerStatus: "DryRun OK", EpochMarker: "store reachable",
	}.Render(&b)
	want := `failover  NOT READY: storage backend is file (objects exist only on this node)
          2 projects have no replica (--restore-missing: RPO up to archive_timeout)
          fencer: aws (DryRun OK)   epoch marker: store reachable
`
	if b.String() != want {
		t.Errorf("render:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	Readiness{Ready: true}.Render(&b)
	if b.String() != "failover  READY\n          fencer: none\n" {
		t.Errorf("ready, no fencer:\n%s", b.String())
	}
}

func TestPlanBlocked(t *testing.T) {
	p := Plan{Checks: []Check{
		{Name: "replica healthy", OK: true, Blocking: true},
		{Name: "lag", OK: false, Blocking: true, Detail: "45s"},
		{Name: "advice", OK: false},
	}}
	if got := p.Blocked(); len(got) != 1 || got[0].Name != "lag" {
		t.Errorf("blocked: %+v", got)
	}
}
