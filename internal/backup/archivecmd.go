package backup

import (
	"fmt"
	"strings"

	"github.com/OWNER/sbctl/internal/config"
)

// ArchiveCommand is the archive_command for ref's cluster. Postgres runs it from the
// data directory, so %p is a relative path.
func ArchiveCommand(binPath, ref, configPath string) string {
	return commandLine(binPath, configPath, "wal", "push", "--ref", ref, "%p")
}

// RestoreCommand is the restore_command that replays ref's archive.
func RestoreCommand(binPath, ref, configPath string) string {
	return commandLine(binPath, configPath, "wal", "fetch", "--ref", ref, "%f", "%p")
}

func commandLine(bin, configPath string, args ...string) string {
	parts := []string{shellQuote(bin)}
	if configPath != "" && configPath != config.DefaultPath {
		parts = append(parts, "--config", shellQuote(configPath))
	}
	for _, a := range args {
		if a == "%p" || a == "%f" {
			parts = append(parts, a)
			continue
		}
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes s for sh only when needed, and doubles '%' because Postgres
// expands %-sequences in archive_command and restore_command before the shell sees it.
func shellQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=,+@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ArchiveSettings returns the postgresql.conf lines lifecycle must give every project
// cluster so WAL reaches the archive: archive_mode, archive_command and archive_timeout.
// The cluster's wal_level must be replica or higher (the Supabase artifact uses logical).
func ArchiveSettings(c *config.Config, ref, configPath string) string {
	return fmt.Sprintf("archive_mode = on\narchive_command = %s\narchive_timeout = %ds\n",
		confString(ArchiveCommand(c.BinPath, ref, configPath)), archiveTimeout(c))
}

// confString quotes s as a postgresql.conf string literal.
func confString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
