package backup

import (
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
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

// ArchiveCommandRelay is the archive_command of a cluster that archives through the
// daemon: it names the relay socket and no config file, because the cluster's unit cannot
// read the config (it holds the backend credentials).
func ArchiveCommandRelay(binPath, ref, socket string) string {
	return commandLine(binPath, "", "wal", "push", "--ref", ref, "--socket", socket, "%p")
}

// RestoreCommandRelay is the restore_command of a cluster that reads the archive of source
// through the relay of the project that runs it (socket).
func RestoreCommandRelay(binPath, source, socket string) string {
	return commandLine(binPath, "", "wal", "fetch", "--ref", source, "--socket", socket, "%f", "%p")
}

// ArchiveCommandFor is the archive_command of ref's cluster under c: through the relay
// when c.WALRelayEnabled, else `supavise wal push` reading the config file at configPath.
func ArchiveCommandFor(c *config.Config, ref, configPath string) string {
	if c.WALRelayEnabled() {
		return ArchiveCommandRelay(c.BinPath, ref, c.Paths().WALSocket(ref))
	}
	return ArchiveCommand(c.BinPath, ref, configPath)
}

// RestoreCommandFor is the restore_command of the cluster of project target that replays
// the archive of source.
func RestoreCommandFor(c *config.Config, source, target, configPath string) string {
	if c.WALRelayEnabled() {
		return RestoreCommandRelay(c.BinPath, source, c.Paths().WALSocket(target))
	}
	return RestoreCommand(c.BinPath, source, configPath)
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
		confString(ArchiveCommandFor(c, ref, configPath)), archiveTimeout(c))
}

// confString quotes s as a postgresql.conf string literal (the file reads backslash escapes).
func confString(s string) string { return lifecycle.ConfString(s) }
