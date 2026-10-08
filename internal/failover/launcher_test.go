package failover

import (
	"strings"
	"testing"

	"github.com/supavise/supavise/deploy/systemd"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/units"
)

// The hard stop of a fence is a missing file: the unit of a project's Postgres starts only while
// <ref>/postgres.run exists, and removeLauncher deletes the file at the path the engine computes. If
// the unit and the code disagreed about that path, a fenced node would restart its primary at boot.
// This runs on every machine, as the systemd-level check of the same thing cannot.
func TestTheUnitsConditionPathIsTheLauncherTheFenceRemoves(t *testing.T) {
	b, err := systemd.Read("supavise-postgres@.service")
	if err != nil {
		t.Fatal(err)
	}
	var condition, exec string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "ConditionPathExists="); ok {
			condition = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "ExecStart="); ok {
			exec = strings.TrimSpace(v)
		}
	}
	if condition == "" || !strings.Contains(condition, "%i") {
		t.Fatalf("the unit has no ConditionPathExists on the launcher: %q", condition)
	}
	if exec != condition {
		t.Fatalf("the unit starts %q but needs %q to exist", exec, condition)
	}
	cfg := config.Default() // the state directory the unit file hardcodes
	for _, ref := range []string{config.SystemRef, refA} {
		want := strings.ReplaceAll(condition, "%i", ref)
		if got := units.FilesFor(cfg, units.Spec{Service: config.SvcPostgres, Ref: ref}).Run; got != want {
			t.Errorf("%s: the engine renders the launcher at %s, the unit needs %s", ref, got, want)
		}
	}
}
