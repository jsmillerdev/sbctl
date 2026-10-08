package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

var (
	_ ReplicaSeeder     = (*Service)(nil)
	_ StandbyConfigurer = (*Service)(nil)
)

// replicationRole is the role a standby streams with (lifecycle.RoleReplication; the loopback
// pg_hba rule for it exists on every project cluster).
const replicationRole = "supabase_replication_admin"

// seedMarker is the file that tells a data directory is half seeded. SeedReplica writes it before
// the first byte of the base backup lands and removes it as its last step, so a directory that
// holds it was cut off (a kill, a power loss) and carries a backup_label without standby.signal.
const seedMarker = "supavise-seeding"

// SeedUnfinished reports whether dataDir holds a seed that did not finish. Nothing may start a
// cluster on such a directory (it would come up as a primary); SeedReplica clears it and starts
// over.
func SeedUnfinished(dataDir string) bool {
	_, err := os.Lstat(filepath.Join(dataDir, seedMarker))
	return err == nil
}

// SeedReplica implements ReplicaSeeder: it extracts the base backup of plan.Ref into the empty
// plan.DataDir the way a restore does and turns the directory into a standby (ConfigureStandby).
// A failed seed removes what it extracted, so the caller can retry on the same directory; so does
// the retry after a seed that was cut off.
func (s *Service) SeedReplica(ctx context.Context, plan ReplicaSeedPlan) (err error) {
	if err := checkStandbyPlan(plan); err != nil {
		return err
	}
	// Start from a plan that finds a base backup on the archive's current timeline whose first
	// WAL file is still there; the newest one unless plan.BackupID names another.
	rp, err := s.PlanRestoreWith(ctx, plan.Ref, time.Time{}, RestoreOptions{Latest: true, BackupID: plan.BackupID})
	if err != nil {
		return err
	}
	if SeedUnfinished(plan.DataDir) {
		clearDir(plan.DataDir)
	}
	if err := prepareDataDir(plan.DataDir); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			clearDir(plan.DataDir)
		}
	}()
	marker := filepath.Join(plan.DataDir, seedMarker)
	if err := writeSyncFile(marker, nil, 0o600); err != nil {
		return err
	}
	if err := s.unpackBase(ctx, &rp.Manifest, plan.DataDir); err != nil {
		return err
	}
	plan.BackupID = rp.Manifest.ID
	if err := s.ConfigureStandby(plan); err != nil {
		return err
	}
	if err := syncTree(plan.DataDir); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil {
		return err
	}
	return syncDir(plan.DataDir)
}

// ConfigureStandby implements StandbyConfigurer: it turns the stopped cluster in plan.DataDir into
// a standby. It deletes the promote.ok of plan.Ref, so the relay refuses the standby's pushes
// again, drops the standby block of an earlier configuration (ClearStandbyConf) and the
// recovery.signal a backup of a cluster in recovery could carry, appends the block to
// postgresql.auto.conf and writes standby.signal last, since it is what turns the start into a
// standby.
func (s *Service) ConfigureStandby(plan ReplicaSeedPlan) error {
	if err := checkStandbyPlan(plan); err != nil {
		return err
	}
	if s.opt.Config != nil {
		if err := os.Remove(s.opt.Config.Paths().PromoteOK(plan.Ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := ClearStandbyConf(plan.DataDir); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(plan.DataDir, "recovery.signal")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := appendAutoConf(plan.DataDir, s.standbyConf(plan)); err != nil {
		return err
	}
	if err := writeSyncFile(filepath.Join(plan.DataDir, "standby.signal"), nil, 0o600); err != nil {
		return err
	}
	return syncDir(plan.DataDir)
}

// checkStandbyPlan refuses a plan that SeedReplica and ConfigureStandby cannot carry out.
func checkStandbyPlan(plan ReplicaSeedPlan) error {
	if err := validRef(plan.Ref); err != nil {
		return err
	}
	if plan.DataDir == "" || !filepath.IsAbs(plan.DataDir) {
		return fmt.Errorf("backup: replica data directory %q must be an absolute path", plan.DataDir)
	}
	if plan.PrimaryPort == 0 {
		return nil
	}
	if plan.PrimaryPort < 1 || plan.PrimaryPort > 65535 {
		return fmt.Errorf("backup: replica primary port %d is out of range", plan.PrimaryPort)
	}
	if ref, _, _, ok := registry.ParseReplicaIdentifier(plan.Identifier); !ok || ref != plan.Ref {
		return fmt.Errorf("backup: %q is not a replica identifier of %s", plan.Identifier, plan.Ref)
	}
	if plan.ReplicationPassword == "" {
		return errors.New("backup: a streaming replica needs the replication password")
	}
	return nil
}

// standbyConf is the block appended to postgresql.auto.conf of a standby. Later lines win, so it
// overrides what the source's own ALTER SYSTEM history left there; the data directory's
// postgresql.conf and wal-g.conf are the source's, and the latter ships hot_standby = off.
//
// archive_mode = on, never always: the standby archives nothing until it is promoted, and then
// it pushes under the project's prefix through the relay of this node, which checks promote.ok.
func (s *Service) standbyConf(plan ReplicaSeedPlan) string {
	cfgPath := s.opt.ConfigPath
	c := s.opt.Config
	if c == nil {
		c = config.Default()
		c.Backup.WALRelay = "off"
	}
	from := ""
	if plan.BackupID != "" {
		from = " (backup " + plan.BackupID + ")"
	}
	var b strings.Builder
	if plan.PrimaryPort != 0 {
		fmt.Fprintf(&b, "\n# --- supavise standby %s of %s%s ---\n", plan.Identifier, plan.Ref, from)
	} else {
		fmt.Fprintf(&b, "\n# --- supavise archive-only standby of %s%s ---\n", plan.Ref, from)
	}
	fmt.Fprintf(&b, "archive_mode = on\narchive_command = %s\n", confQuote(ArchiveCommandFor(c, plan.Ref, cfgPath)))
	fmt.Fprintf(&b, "restore_command = %s\n", confQuote(RestoreCommandFor(c, plan.Ref, plan.Ref, cfgPath)))
	b.WriteString("recovery_target_timeline = 'latest'\nhot_standby = on\n")
	if plan.PrimaryPort != 0 {
		info := fmt.Sprintf("host=127.0.0.1 port=%d user=%s password=%s application_name=%s sslmode=disable",
			plan.PrimaryPort, replicationRole, connValue(plan.ReplicationPassword), plan.Identifier)
		fmt.Fprintf(&b, "primary_conninfo = %s\n", confQuote(info))
	}
	return b.String()
}

// StandbyGUCs are the settings the seeder adds that a primary does not keep. After a promotion,
// ALTER SYSTEM RESET each of them (or ClearStandbyConf before the cluster starts as a primary):
// primary_conninfo holds the replication password, and the base backups of the promoted cluster
// copy its data directory. archive_mode and archive_command stay: they are the primary's own.
var StandbyGUCs = []string{"primary_conninfo", "restore_command", "recovery_target_timeline", "hot_standby"}

// ClearStandbyConf removes the standby block of SeedReplica from the postgresql.auto.conf of the
// stopped cluster in dataDir: the lines of StandbyGUCs and the header comment. The rest of the
// file is kept. It does nothing when the file has none of them.
func ClearStandbyConf(dataDir string) error {
	p := filepath.Join(dataDir, "postgresql.auto.conf")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	kept := lines[:0:0]
	for _, l := range lines {
		if !isStandbyLine(l) {
			kept = append(kept, l)
		}
	}
	if len(kept) == len(lines) {
		return nil
	}
	tmp := p + ".tmp"
	if err := writeSyncFile(tmp, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dataDir)
}

func isStandbyLine(line string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "# --- supavise standby ") || strings.HasPrefix(t, "# --- supavise archive-only standby ") {
		return true
	}
	for _, k := range StandbyGUCs {
		if rest, ok := strings.CutPrefix(t, k); ok && strings.HasPrefix(strings.TrimSpace(rest), "=") {
			return true
		}
	}
	return false
}

// connValue quotes s as a libpq connection-string value when it needs it.
func connValue(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.") == "" {
		return s
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// confQuote quotes s as a postgresql.conf string literal; the file reads backslash escapes.
func confQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// clearDir removes everything in dir and keeps dir.
func clearDir(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
