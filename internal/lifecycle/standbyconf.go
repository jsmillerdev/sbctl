package lifecycle

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The recovery settings of a standby live in postgresql.auto.conf of its data directory. Three
// writers touch them: DemoteToReplica (a primary turned into a standby in place), PromoteReplica (a
// standby turned into a primary) and the backup service's seeder (a standby built from a base backup,
// and ConfigureStandby, which does the same to a stopped primary). They share the quoting and the
// list of what belongs to a standby here; internal/backup imports this package and uses both.

// ConfString quotes s as a postgresql.conf string literal, where a backslash inside quotes starts an
// escape.
func ConfString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''") + "'"
}

// KVQuote quotes s as the value of a libpq key/value connection string.
func KVQuote(s string) string { return kvQuote(s) }

// recoverySettingNames are the settings of a cluster in recovery that a primary must not carry.
// hot_standby is the one the backup service's seeder writes and a primary ignores; the replica's units
// pass it on the command line.
var recoverySettingNames = map[string]bool{
	"primary_conninfo": true, "primary_slot_name": true, "restore_command": true, "hot_standby": true,
	"recovery_target_timeline": true, "recovery_target": true, "recovery_target_name": true,
	"recovery_target_time": true, "recovery_target_xid": true, "recovery_target_lsn": true,
	"recovery_target_inclusive": true, "recovery_target_action": true, "archive_cleanup_command": true,
	"recovery_end_command": true, "recovery_min_apply_delay": true,
}

// dropRecoverySettings removes the recovery settings from the postgresql.auto.conf of dataDir and the
// signal files that start a recovery, so that the cluster starts as a primary.
func dropRecoverySettings(dataDir string) error {
	for _, f := range []string{"standby.signal", "recovery.signal"} {
		if err := os.Remove(filepath.Join(dataDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return rewriteAutoConf(dataDir, nil, false)
}

// ClearStandbyBlock removes every setting of a cluster in recovery from the postgresql.auto.conf of the
// stopped cluster in dataDir, and the header of the block the backup service's seeder appends with
// them. The rest of the file stays. archive also removes the archive_mode and archive_command lines
// that follow the header: they are the primary's own and stay after a promotion, and the seeder
// writes them again each time it makes a standby (the units pass both on the command line, so the
// file's copies only add up with each cycle of demotion and promotion). It leaves the signal files alone.
func ClearStandbyBlock(dataDir string, archive bool) error {
	return rewriteAutoConf(dataDir, nil, archive)
}

// setRecoverySettings replaces the recovery settings of dataDir's postgresql.auto.conf by lines.
func setRecoverySettings(dataDir string, lines []string) error {
	return rewriteAutoConf(dataDir, lines, true)
}

// recoverySettingOf returns the name of the recovery setting that a line of postgresql.auto.conf assigns.
func recoverySettingOf(line string) (string, bool) {
	name := strings.TrimSpace(line)
	if i := strings.IndexAny(name, "= \t"); i > 0 && !strings.HasPrefix(name, "#") {
		if n := strings.ToLower(name[:i]); recoverySettingNames[n] {
			return n, true
		}
	}
	return "", false
}

// standbyBlockHeaders open the block of settings the backup service's seeder appends to a standby's
// postgresql.auto.conf (backup.SeedReplica); the settings go with the block, so its header does too.
var standbyBlockHeaders = []string{"# --- supavise standby ", "# --- supavise archive-only standby "}

func isStandbyBlockHeader(line string) bool {
	for _, h := range standbyBlockHeaders {
		if strings.HasPrefix(strings.TrimSpace(line), h) {
			return true
		}
	}
	return false
}

// blockOwnLine reports whether a line inside the seeder's block is one of the archive settings it
// writes next to the recovery settings.
func blockOwnLine(line string) bool {
	name := strings.TrimSpace(line)
	if i := strings.IndexAny(name, "= \t"); i > 0 {
		switch strings.ToLower(name[:i]) {
		case "archive_mode", "archive_command":
			return true
		}
	}
	return false
}

// rewriteAutoConf drops every assignment of a recovery setting and the header of the seeder's block of
// them, and with archive the archive settings inside that block as well, keeps the rest as it is and
// appends add.
func rewriteAutoConf(dataDir string, add []string, archive bool) error {
	path := filepath.Join(dataDir, "postgresql.auto.conf")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	inBlock := false // between the seeder's header and the first line that is not its own
	for sc.Scan() {
		line := sc.Text()
		if isStandbyBlockHeader(line) {
			inBlock = true
			continue
		}
		if _, ok := recoverySettingOf(line); ok {
			continue
		}
		if inBlock {
			if archive && blockOwnLine(line) {
				continue
			}
			if !blockOwnLine(line) {
				inBlock = false
			}
		}
		out.WriteString(line + "\n")
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for _, l := range add {
		out.WriteString(l + "\n")
	}
	_, err = writeFile(path, out.Bytes(), 0o600)
	return err
}
