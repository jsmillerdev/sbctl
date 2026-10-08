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
	"github.com/supavise/supavise/internal/lifecycle"
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
const seedMarker = lifecycle.SeedMarker

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
		if err := clearDir(plan.DataDir); err != nil {
			return fmt.Errorf("backup: clearing the seed %s that was cut off: %w", plan.DataDir, err)
		}
	}
	if err := prepareDataDir(plan.DataDir); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// The marker stays if the directory cannot be emptied, which is what the next call and
			// SeedUnfinished look for.
			_ = clearDir(plan.DataDir)
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
// a standby. It deletes the promote.ok of plan.Ref (config.Paths.PromoteOK of the Service's Config,
// which is the path the relay reads unless RelayOptions.PromoteOK names another: the two must agree),
// so the relay refuses the standby's pushes again, drops the standby block of an earlier configuration (ClearStandbyConf) and the
// recovery.signal a backup of a cluster in recovery could carry, appends the block to
// postgresql.auto.conf and writes standby.signal last, since it is what turns the start into a
// standby.
func (s *Service) ConfigureStandby(plan ReplicaSeedPlan) error {
	if err := checkStandbyPlan(plan); err != nil {
		return err
	}
	// The relay trusts promote.ok for as long as it exists: the standby must not leave one behind. A
	// Service without a Config cannot say where it is, so it cannot make a standby.
	if s.opt.Config == nil {
		return errors.New("backup: a service without a Config cannot delete promote.ok, so it cannot make a standby")
	}
	if err := os.Remove(s.opt.Config.Paths().PromoteOK(plan.Ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The block of an earlier configuration goes whole, its archive settings too: the block that follows
	// has its own, and each cycle of demotion and promotion would add another pair.
	if err := lifecycle.ClearStandbyBlock(plan.DataDir, true); err != nil {
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
	// The backup id goes into a comment of postgresql.auto.conf: a newline in it would start a line
	// of settings.
	if strings.IndexFunc(plan.BackupID, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return fmt.Errorf("backup: replica backup id %q has whitespace or control characters", plan.BackupID)
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
	fmt.Fprintf(&b, "archive_mode = on\narchive_command = %s\n", confString(ArchiveCommandFor(c, plan.Ref, cfgPath)))
	fmt.Fprintf(&b, "restore_command = %s\n", confString(RestoreCommandFor(c, plan.Ref, plan.Ref, cfgPath)))
	b.WriteString("recovery_target_timeline = 'latest'\nhot_standby = on\n")
	if plan.PrimaryPort != 0 {
		info := fmt.Sprintf("host=127.0.0.1 port=%d user=%s password=%s application_name=%s sslmode=disable",
			plan.PrimaryPort, replicationRole, connValue(plan.ReplicationPassword), plan.Identifier)
		fmt.Fprintf(&b, "primary_conninfo = %s\n", confString(info))
	}
	return b.String()
}

// StandbyGUCs are the settings the seeder adds that a primary does not keep. After a promotion,
// ALTER SYSTEM RESET each of them (or ClearStandbyConf before the cluster starts as a primary):
// primary_conninfo holds the replication password, and the base backups of the promoted cluster
// copy its data directory. archive_mode and archive_command stay: they are the primary's own.
var StandbyGUCs = []string{"primary_conninfo", "restore_command", "recovery_target_timeline", "hot_standby"}

// ClearStandbyConf removes the standby block of SeedReplica from the postgresql.auto.conf of the
// stopped cluster in dataDir: its header and every setting of a cluster in recovery (StandbyGUCs and
// the recovery_target_* family). The primary's own archive_mode and archive_command stay. The rest of
// the file is kept, and it is not rewritten when it has none of them. The shared implementation is
// lifecycle.ClearStandbyBlock, which the plane's promotion and demotion use as well.
func ClearStandbyConf(dataDir string) error {
	if err := lifecycle.ClearStandbyBlock(dataDir, false); err != nil {
		return err
	}
	return syncDir(dataDir)
}

// connValue quotes s as a libpq connection-string value when it needs it.
func connValue(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.") == "" {
		return s
	}
	return lifecycle.KVQuote(s)
}

// clearDir removes everything in dir and keeps dir. The seeding marker goes last, and only when
// everything else did: a directory that could not be emptied (a busy mount, an immutable file) still
// says it is half seeded, so that nothing starts a cluster on it (SeedUnfinished).
func clearDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var first error
	for _, e := range ents {
		if e.Name() == seedMarker {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return first
	}
	if err := os.Remove(filepath.Join(dir, seedMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
