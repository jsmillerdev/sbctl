package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

var _ ReplicaSeeder = (*Service)(nil)

// replicationRole is the role a standby streams with (lifecycle.RoleReplication; the loopback
// pg_hba rule for it exists on every project cluster).
const replicationRole = "supabase_replication_admin"

// SeedReplica implements ReplicaSeeder: it extracts the base backup of plan.Ref into the empty
// plan.DataDir the way a restore does and turns the directory into a standby. A failed seed
// removes what it extracted, so the caller can retry on the same directory.
func (s *Service) SeedReplica(ctx context.Context, plan ReplicaSeedPlan) (err error) {
	if err := validRef(plan.Ref); err != nil {
		return err
	}
	if plan.DataDir == "" || !filepath.IsAbs(plan.DataDir) {
		return fmt.Errorf("backup: replica data directory %q must be an absolute path", plan.DataDir)
	}
	if plan.PrimaryPort != 0 {
		if plan.PrimaryPort < 1 || plan.PrimaryPort > 65535 {
			return fmt.Errorf("backup: replica primary port %d is out of range", plan.PrimaryPort)
		}
		if ref, _, _, ok := registry.ParseReplicaIdentifier(plan.Identifier); !ok || ref != plan.Ref {
			return fmt.Errorf("backup: %q is not a replica identifier of %s", plan.Identifier, plan.Ref)
		}
		if plan.ReplicationPassword == "" {
			return errors.New("backup: a streaming replica needs the replication password")
		}
	}
	// Start from a plan that finds a base backup on the archive's current timeline whose first
	// WAL file is still there; the newest one unless plan.BackupID names another.
	rp, err := s.PlanRestoreWith(ctx, plan.Ref, time.Time{}, RestoreOptions{Latest: true, BackupID: plan.BackupID})
	if err != nil {
		return err
	}
	if err := prepareDataDir(plan.DataDir); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			clearDir(plan.DataDir)
		}
	}()
	if err := s.unpackBase(ctx, &rp.Manifest, plan.DataDir); err != nil {
		return err
	}
	// A backup of a cluster that was itself a standby or in recovery could carry either signal.
	if err := os.Remove(filepath.Join(plan.DataDir, "recovery.signal")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := appendAutoConf(plan.DataDir, s.standbyConf(plan, &rp.Manifest)); err != nil {
		return err
	}
	// standby.signal last: it is what turns the start into a standby.
	if err := writeSyncFile(filepath.Join(plan.DataDir, "standby.signal"), nil, 0o600); err != nil {
		return err
	}
	return syncTree(plan.DataDir)
}

// standbyConf is the block appended to postgresql.auto.conf of a seeded standby. Later lines win,
// so it overrides what the source's own ALTER SYSTEM history left there; the data directory's
// postgresql.conf and wal-g.conf are the source's, and the latter ships hot_standby = off.
//
// archive_mode = on, never always: the standby archives nothing until it is promoted, and then
// it pushes under the project's prefix through the relay of this node, which checks promote.ok.
func (s *Service) standbyConf(plan ReplicaSeedPlan, m *Manifest) string {
	cfgPath := s.opt.ConfigPath
	c := s.opt.Config
	if c == nil {
		c = config.Default()
		c.Backup.WALRelay = "off"
	}
	var b strings.Builder
	if plan.PrimaryPort != 0 {
		fmt.Fprintf(&b, "\n# --- supavise standby %s of %s (backup %s) ---\n", plan.Identifier, plan.Ref, m.ID)
	} else {
		fmt.Fprintf(&b, "\n# --- supavise archive-only standby of %s (backup %s) ---\n", plan.Ref, m.ID)
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
