package lifecycle

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// replicaPoll is how often the waits of PromoteReplica look at the standby. A variable so that
// tests can shorten it.
var replicaPoll = 250 * time.Millisecond

// PromoteOptions tune PromoteReplica.
type PromoteOptions struct {
	// Epoch is the cluster epoch the promotion is authorized under. It goes into promote.ok, which
	// lets the WAL relay accept the first push of the new primary.
	Epoch int64
	// WaitLSN, when set, makes the promotion wait until replay reaches it and everything received:
	// a planned switchover passes the old primary's final checkpoint LSN.
	WaitLSN string
	// DrainArchive lets restore_command drain the WAL archive before the promotion: it waits until
	// replay has stood still for one retry interval of the standby, bounded by Timeout and 10
	// seconds. An unplanned failover sets it; the dead primary's last segments may be in the archive
	// only.
	DrainArchive bool
	// Timeout bounds the waits; zero means two minutes.
	Timeout time.Duration
}

func (o PromoteOptions) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 2 * time.Minute
}

// writePromoteOK records that this node may promote ref's standby at epoch (see
// config.Paths.PromoteOK). The content is the epoch in decimal and a newline, which is what
// backup.FormatPromoteOK writes and backup.ParsePromoteOK reads.
func (pl *PostgresPlane) writePromoteOK(ref string, epoch int64) error {
	path := pl.cfg.Paths().PromoteOK(ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	_, err := writeFile(path, []byte(strconv.FormatInt(epoch, 10)+"\n"), 0o600)
	return err
}

// PromoteReplica turns the standby of t into the project's primary on its canonical port, as step 4
// of a project switchover (design 2.10.3): it writes promote.ok(epoch), waits for the replay the
// options ask for, runs pg_promote and a checkpoint, then restarts the cluster from the primary's
// spec on the canonical port, with the standby's recovery settings removed from
// postgresql.auto.conf. GoTrue and PostgREST are not started: they belong to the home and start
// when the registry names this node the home (Start).
//
// It can be repeated after a failure at any point: a cluster that was promoted but not restarted
// is restarted as a primary, and one that already runs as a primary on the canonical port is left
// alone. ErrNotStandby when the cluster here is neither.
func (pl *PostgresPlane) PromoteReplica(ctx context.Context, t ReplicaTarget, o PromoteOptions) error {
	if err := t.check(pl.cfg); err != nil {
		return err
	}
	if o.Epoch < 1 {
		return errors.New("lifecycle: PromoteReplica needs the cluster epoch")
	}
	p := t.Project
	rp, cp := pl.replicaPaths(p), pl.paths(p)
	ra, ca := addrOf(rp), addrOf(cp)
	sql := pl.sql()
	timeout := o.timeout()

	if st, err := sql.Status(ctx, ca); err == nil && !st.InRecovery && pl.promoteOKAtLeast(p.Ref, o.Epoch) {
		return nil // promoted and restarted earlier
	}
	if err := pl.writePromoteOK(p.Ref, o.Epoch); err != nil {
		return err
	}
	standby := fileExists(filepath.Join(rp.Data, "standby.signal"))
	st, err := sql.Status(ctx, ra)
	if err != nil && standby {
		// A standby that is down replays the archive when it starts; start it to promote it.
		if err := pl.StartReplicaDatabase(ctx, t); err != nil {
			return err
		}
		st, err = sql.Status(ctx, ra)
	}
	switch {
	case err == nil && st.InRecovery:
		if o.WaitLSN != "" {
			if err := pl.waitReplay(ctx, ra, o.WaitLSN, timeout); err != nil {
				return err
			}
		}
		if o.DrainArchive {
			pl.drainArchive(ctx, ra, min(timeout, 10*time.Second))
		}
		pl.log.Info("promoting the replica", "ref", p.Ref, "replica", t.Identifier, "epoch", o.Epoch)
		if err := sql.Promote(ctx, ra, timeout); err != nil {
			return err
		}
		if err := sql.Checkpoint(ctx, ra); err != nil {
			return fmt.Errorf("lifecycle: checkpoint after the promotion of %s: %w", t.Identifier, err)
		}
	case err == nil:
		// Promoted by an earlier try that died before the restart.
	case !standby && fileExists(filepath.Join(rp.Data, "PG_VERSION")):
		// Promoted and then stopped.
	default:
		return fmt.Errorf("%w: %s: %v", ErrNotStandby, rp.Data, err)
	}

	// The cluster runs on the replica port with the standby's unit; restart it from the primary's.
	if hasPostgREST(p.Ref) {
		if err := pl.sup.Stop(ctx, config.UnitName(config.SvcPostgREST, p.Ref)); err != nil {
			return err
		}
	}
	if err := pl.sup.Stop(ctx, config.UnitName(config.SvcPostgres, p.Ref)); err != nil {
		return err
	}
	if err := dropRecoverySettings(rp.Data); err != nil {
		return err
	}
	return pl.StartDatabase(ctx, p, t.Keys)
}

func (pl *PostgresPlane) promoteOKAtLeast(ref string, epoch int64) bool {
	b, err := os.ReadFile(pl.cfg.Paths().PromoteOK(ref))
	if err != nil {
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return err == nil && n >= epoch
}

// waitReplay waits until replay on a reaches lsn and everything received.
func (pl *PostgresPlane) waitReplay(ctx context.Context, a ClusterAddr, lsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := pl.sql().ReplayedTo(ctx, a, lsn)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("%w: waiting for %s: %v", ErrReplayBehind, lsn, err)
			}
			return fmt.Errorf("%w: %s not reached within %s", ErrReplayBehind, lsn, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(replicaPoll):
		}
	}
}

// drainArchive waits until replay has stood still for one retry interval of the standby (plus a
// second), which means its restore_command asked the archive for the next segment and found
// nothing; it gives up after bound. It never fails: the promotion goes on with what was replayed.
func (pl *PostgresPlane) drainArchive(ctx context.Context, a ClusterAddr, bound time.Duration) {
	sql := pl.sql()
	still := 6 * time.Second
	if d, err := sql.RetryInterval(ctx, a); err == nil {
		still = d + time.Second
	}
	still = min(still, bound)
	start := time.Now()
	last, _ := sql.ReplayLSN(ctx, a)
	since := start
	for time.Since(start) < bound && time.Since(since) < still {
		select {
		case <-ctx.Done():
			return
		case <-time.After(replicaPoll):
		}
		if cur, err := sql.ReplayLSN(ctx, a); err == nil && cur != last {
			last, since = cur, time.Now()
		}
	}
}

// DemoteToReplica turns the cluster of a project that was cleanly shut down into a standby of the
// project's new home, in place, as step 7 of a project switchover (design 2.10.3): its units are
// stopped, promote.ok is removed, standby.signal and the recovery settings (primary_conninfo to the
// canonical port, which is a forwarder to the new home here; restore_command; the latest timeline) are
// written, and the cluster starts from the replica's spec on the replica port, with the replica's
// PostgREST. The old timeline ends at the shutdown checkpoint, where the new primary's begins, so no
// base backup is needed. ErrNotCleanShutdown when the cluster crashed or never shut down: it would
// have to be rebuilt.
//
// It can be repeated: a cluster that already runs as the replica is left running.
func (pl *PostgresPlane) DemoteToReplica(ctx context.Context, t ReplicaTarget) error {
	if err := t.check(pl.cfg); err != nil {
		return err
	}
	p := t.Project
	rp, cp := pl.replicaPaths(p), pl.paths(p)
	if fileExists(filepath.Join(rp.Data, "standby.signal")) {
		if st, err := pl.sql().Status(ctx, addrOf(rp)); err == nil && st.InRecovery {
			return pl.StartReplicaAPI(ctx, t)
		}
	}
	if pl.opts.RestoreCommandFor == nil {
		return errors.New("lifecycle: no restore_command is configured, so a demoted cluster could not catch up from the archive")
	}
	if err := pl.Stop(ctx, p.Ref); err != nil {
		return err
	}
	if st, err := ReadControl(cp.Data); err != nil {
		return err
	} else if !st.ShutDown() {
		return fmt.Errorf("%w: %s is %q", ErrNotCleanShutdown, cp.Data, st.State)
	}
	if err := os.Remove(pl.cfg.Paths().PromoteOK(p.Ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// GoTrue is the home's; the old home keeps none.
	if err := pl.sup.Remove(ctx, config.UnitName(config.SvcGoTrue, p.Ref)); err != nil {
		pl.log.Warn("demote: removing the GoTrue unit", "ref", p.Ref, "error", err)
	}
	conf := []string{
		"primary_conninfo = " + confString(primaryConninfo(pl.cfg.PortsFor(p.Ref, p.Seq).Postgres, t.Keys.ReplicationPassword, t.Identifier)),
		"restore_command = " + confString(pl.opts.RestoreCommandFor(p.Ref)),
		"recovery_target_timeline = 'latest'",
	}
	if err := setRecoverySettings(rp.Data, conf); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rp.Data, "standby.signal"), nil, 0o600); err != nil {
		return err
	}
	pl.log.Info("demoting the cluster to a replica", "ref", p.Ref, "replica", t.Identifier)
	return pl.StartReplica(ctx, t)
}

// SetWALKeepSize makes the primary of p keep size of WAL for its standbys (wal_keep_size, for example
// "2GB"; empty removes the setting) with ALTER SYSTEM and a reload. Standbys use no replication slots
// (the archive covers any gap), so this is an option for a deployment whose standbys fall behind the
// archive's reach; it is never part of the unit's arguments, which keeps a primary's units as they
// were. The setting survives restarts in postgresql.auto.conf. A settings save through the
// Management API does not touch it.
func (pl *PostgresPlane) SetWALKeepSize(ctx context.Context, p *registry.Project, size string) error {
	return pl.sql().AlterSystem(ctx, addrOf(pl.paths(p)), "wal_keep_size", size)
}

// primaryConninfo is the primary_conninfo of a standby that streams through the loopback port of the
// project's primary: the mesh carries it to the home and is already TLS, so sslmode is disable.
func primaryConninfo(port int, password, applicationName string) string {
	return fmt.Sprintf("host=127.0.0.1 port=%d user=%s password=%s application_name=%s sslmode=disable",
		port, RoleReplication, kvQuote(password), kvQuote(applicationName))
}

// confString quotes a value for postgresql.conf.
func confString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// recoverySettingNames are the settings of a cluster in recovery that a primary must not carry.
var recoverySettingNames = map[string]bool{
	"primary_conninfo": true, "primary_slot_name": true, "restore_command": true,
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
	return rewriteAutoConf(dataDir, nil)
}

// setRecoverySettings replaces the recovery settings of dataDir's postgresql.auto.conf by lines.
func setRecoverySettings(dataDir string, lines []string) error {
	return rewriteAutoConf(dataDir, lines)
}

// rewriteAutoConf drops every assignment of a recovery setting from postgresql.auto.conf, keeps the
// rest as it is and appends add.
func rewriteAutoConf(dataDir string, add []string) error {
	path := filepath.Join(dataDir, "postgresql.auto.conf")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		name := strings.TrimSpace(line)
		if i := strings.IndexAny(name, "= \t"); i > 0 && !strings.HasPrefix(name, "#") && recoverySettingNames[strings.ToLower(name[:i])] {
			continue
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

// ControlInfo is what a cluster's pg_control file says.
type ControlInfo struct {
	// State is the cluster state as pg_controldata prints it: "shut down", "shut down in recovery",
	// "in production", "in archive recovery", "in crash recovery", "starting up", "shutting down".
	State string
	// Checkpoint is the LSN of the latest checkpoint record ("0/3000060"). After a clean shutdown it
	// is the shutdown checkpoint, the last record of the cluster's WAL.
	Checkpoint string
}

// ShutDown reports whether the cluster was shut down cleanly.
func (c ControlInfo) ShutDown() bool {
	return c.State == "shut down" || c.State == "shut down in recovery"
}

var controlStates = []string{"starting up", "shut down", "shut down in recovery", "shutting down", "in crash recovery", "in archive recovery", "in production"}

// ReadControl reads the state and the latest checkpoint from dataDir's global/pg_control. The layout
// of the first fields is the same from Postgres 9.5 to 17 (system identifier, control and catalog
// versions, the state, the time of the last update, the checkpoint LSN), and reading the file needs
// no binary of the artifact and no running cluster.
func ReadControl(dataDir string) (ControlInfo, error) {
	f, err := os.Open(filepath.Join(dataDir, "global", "pg_control"))
	if err != nil {
		return ControlInfo{}, err
	}
	defer f.Close()
	var head [40]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return ControlInfo{}, fmt.Errorf("lifecycle: read pg_control: %w", err)
	}
	var order binary.ByteOrder = binary.LittleEndian
	if v := order.Uint32(head[8:12]); v < 900 || v > 2000 {
		order = binary.BigEndian
		if v := order.Uint32(head[8:12]); v < 900 || v > 2000 {
			return ControlInfo{}, errors.New("lifecycle: pg_control is not a control file of a Postgres this code reads")
		}
	}
	state := int(order.Uint32(head[16:20]))
	if state < 0 || state >= len(controlStates) {
		return ControlInfo{}, fmt.Errorf("lifecycle: pg_control holds an unknown cluster state %d", state)
	}
	lsn := order.Uint64(head[32:40])
	return ControlInfo{State: controlStates[state], Checkpoint: fmt.Sprintf("%X/%X", lsn>>32, uint32(lsn))}, nil
}

// FinalCheckpoint reads the control file of ref's cluster on this node. The failover orchestrator
// reads it after a fast shutdown of the old primary, to learn the position the new primary's replay
// must reach (PromoteOptions.WaitLSN).
func (pl *PostgresPlane) FinalCheckpoint(ref string) (ControlInfo, error) {
	return ReadControl(pl.cfg.Paths().PostgresData(ref))
}
