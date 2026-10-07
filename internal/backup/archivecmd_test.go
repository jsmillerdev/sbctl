package backup

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
)

func TestArchiveAndRestoreCommand(t *testing.T) {
	if got, want := ArchiveCommand("/usr/local/bin/supavise", testRef, ""), "/usr/local/bin/supavise wal push --ref "+testRef+" %p"; got != want {
		t.Errorf("ArchiveCommand = %q, want %q", got, want)
	}
	if got, want := RestoreCommand("/usr/local/bin/supavise", testRef, ""), "/usr/local/bin/supavise wal fetch --ref "+testRef+" %f %p"; got != want {
		t.Errorf("RestoreCommand = %q, want %q", got, want)
	}
	if got := ArchiveCommand("/usr/local/bin/supavise", testRef, config.DefaultPath); strings.Contains(got, "--config") {
		t.Errorf("default config path must not be embedded: %q", got)
	}
	got := ArchiveCommand("/opt/my supavise/supavise", testRef, "/tmp/it's here/c.toml")
	want := `'/opt/my supavise/supavise' --config '/tmp/it'\''s here/c.toml' wal push --ref ` + testRef + ` %p`
	if got != want {
		t.Errorf("ArchiveCommand with quoting = %q, want %q", got, want)
	}
	// A literal % in a path is doubled, because Postgres expands %-sequences before sh runs.
	if got := ArchiveCommand("/opt/100%/supavise", testRef, ""); !strings.HasPrefix(got, "'/opt/100%%/supavise'") {
		t.Errorf("percent not doubled: %q", got)
	}
}

// The command Postgres hands to sh must parse back into the intended arguments.
func TestArchiveCommandSurvivesShell(t *testing.T) {
	cmd := ArchiveCommand("/bin/echo", testRef, "/tmp/it's a path.toml")
	// Emulate Postgres: replace %p, then %% -> %.
	cmd = strings.ReplaceAll(cmd, "%p", "pg_wal/000000010000000000000001")
	cmd = strings.ReplaceAll(cmd, "%%", "%")
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(out)), "--config /tmp/it's a path.toml wal push --ref "+testRef+" pg_wal/000000010000000000000001"; got != want {
		t.Errorf("shell saw %q, want %q", got, want)
	}
}

func TestArchiveSettings(t *testing.T) {
	c := config.Default()
	c.BinPath = "/usr/local/bin/supavise"
	c.Backup.WALRelay = "off"
	got := ArchiveSettings(c, testRef, "")
	for _, want := range []string{
		"archive_mode = on\n",
		"archive_command = '/usr/local/bin/supavise wal push --ref " + testRef + " %p'\n",
		"archive_timeout = 300s\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ArchiveSettings missing %q in:\n%s", want, got)
		}
	}
	c.Backup.ArchiveTimeoutSeconds = 60
	if got := ArchiveSettings(c, testRef, ""); !strings.Contains(got, "archive_timeout = 60s") {
		t.Errorf("archive_timeout not configurable:\n%s", got)
	}
	c.Backup.WALRelay = "on"
	if got := ArchiveSettings(c, testRef, ""); !strings.Contains(got, "archive_command = '/usr/local/bin/supavise wal push --ref "+testRef+" --socket "+c.Paths().WALSocket(testRef)+" %p'\n") {
		t.Errorf("relay form missing:\n%s", got)
	}
	if got := confString("it's"); got != "'it''s'" {
		t.Errorf("confString = %q", got)
	}
}
