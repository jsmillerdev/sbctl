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
		"../../deploy/systemd/sb-basebackup@.service": RenderBackupService(d.BinPath),
		"../../deploy/systemd/sb-basebackup@.timer":   RenderBackupTimer(d.Backup.BaseBackupOnCalendar),
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
