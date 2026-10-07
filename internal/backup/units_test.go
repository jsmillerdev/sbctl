package backup

import (
	"os"
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/config"
)

// The deploy/ files are the default renderings; regenerate them with
// UPDATE_DEPLOY=1 go test ./internal/backup -run TestDeployUnits.
func TestDeployUnits(t *testing.T) {
	d := config.Default()
	files := map[string]string{
		"../../deploy/systemd/sb-basebackup@.service":      RenderBackupService(d.BinPath),
		"../../deploy/systemd/sb-basebackup@.timer":        RenderBackupTimer(d.Backup.BaseBackupOnCalendar),
		"../../deploy/systemd/sb-basebackup-prune.service": RenderPruneService(d.BinPath),
		"../../deploy/systemd/sb-basebackup-prune.timer":   RenderPruneTimer(),
	}
	for path, want := range files {
		if os.Getenv("UPDATE_DEPLOY") != "" {
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s is out of date; run UPDATE_DEPLOY=1 go test ./internal/backup -run TestDeployUnits", path)
		}
	}
}

func TestRenderBackupServiceOrdersBackupBeforePrune(t *testing.T) {
	s := RenderBackupService("/opt/my sbctl/sbctl")
	create := strings.Index(s, `"/opt/my sbctl/sbctl" backups create %i`)
	prune := strings.Index(s, `"/opt/my sbctl/sbctl" backups prune %i`)
	if create < 0 || prune < 0 || create > prune {
		t.Fatalf("unexpected unit:\n%s", s)
	}
	if !strings.Contains(s, "Type=oneshot") || !strings.Contains(s, "User=sbctl") {
		t.Fatalf("unit missing oneshot/user:\n%s", s)
	}
}

func TestOnCalendarIsValidatedBeforeItReachesAUnit(t *testing.T) {
	for _, bad := range []string{"", "  ", "daily\n[Service]\nExecStart=/bin/evil", "a\rb", "x\x00"} {
		if ValidateOnCalendar(bad) == nil {
			t.Errorf("ValidateOnCalendar(%q) accepted it", bad)
		}
		if _, err := RenderBackupTimerChecked(bad); err == nil {
			t.Errorf("RenderBackupTimerChecked(%q) accepted it", bad)
		}
		if got := RenderBackupTimer(bad); !strings.Contains(got, "OnCalendar="+config.Default().Backup.BaseBackupOnCalendar+"\n") || strings.Contains(got, "evil") {
			t.Errorf("RenderBackupTimer(%q) did not fall back to the default:\n%s", bad, got)
		}
	}
	for _, ok := range []string{"daily", "*-*-* 03:00:00", "Mon,Thu *-*-* 02:30"} {
		if err := ValidateOnCalendar(ok); err != nil {
			t.Errorf("ValidateOnCalendar(%q) = %v", ok, err)
		}
	}
}

func TestBackupServiceDoesNotStarveOnIdleIO(t *testing.T) {
	for name, s := range map[string]string{"backup": RenderBackupService("/x/sbctl"), "prune": RenderPruneService("/x/sbctl")} {
		if strings.Contains(s, "IOSchedulingClass=idle") || !strings.Contains(s, "IOSchedulingClass=best-effort") || !strings.Contains(s, "IOSchedulingPriority=7") {
			t.Errorf("%s unit I/O class:\n%s", name, s)
		}
	}
	if p := RenderPruneService("/x/sbctl"); !strings.Contains(p, "ExecStart=/x/sbctl backups prune\n") {
		t.Errorf("prune unit must prune every ref (no argument):\n%s", p)
	}
}
