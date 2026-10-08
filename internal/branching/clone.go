package branching

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/backup"
)

// errWALGone means a WAL segment the backup needs was recycled before it could be copied.
var errWALGone = errors.New("branching: a WAL segment of the backup was recycled before it could be copied")

// cloneSource names the parent cluster a clone is taken from.
type cloneSource struct {
	// DSN connects as supabase_admin (superuser).
	DSN string
}

// cloner copies a running cluster's data directory with copy-on-write file clones, the
// low-level base backup procedure of the PostgreSQL manual (section "Making a Base Backup
// Using the Low Level API"):
//
//  1. pg_backup_start on a superuser session that stays open
//  2. clone every file of PGDATA (pg_control last; what a base backup omits is omitted)
//  3. pg_backup_stop, which returns the backup_label
//  4. write backup_label into the clone
//  5. clone the WAL segments from the one the backup started in to the one it ended in,
//     into the clone's own pg_wal
//
// The clone starts as a crash recovery from the label's checkpoint over its own pg_wal:
// no archive is needed, so it works on a node without a backup backend. Nothing in the
// clone points at the parent's archive; the plane gives the clone its own archive_command.
type cloner struct {
	method string // MethodClonefile or MethodReflink
	log    *slog.Logger
	// attempts bounds retries after a recycled WAL segment (default 3).
	attempts int
}

// Clone fills dstData (which must not exist yet or be empty) with a consistent copy of
// the parent's data directory.
func (c *cloner) Clone(ctx context.Context, src cloneSource, dstData string) (*CloneStats, error) {
	attempts := c.attempts
	if attempts <= 0 {
		attempts = 3
	}
	var last error
	for i := 1; i <= attempts; i++ {
		st, err := c.once(ctx, src, dstData)
		if err == nil {
			st.Attempts = i
			return st, nil
		}
		_ = os.RemoveAll(dstData)
		last = err
		if !errors.Is(err, errWALGone) {
			return nil, err
		}
		c.log.Warn("clone: WAL recycled during the copy; retrying", "attempt", i, "err", err)
	}
	return nil, last
}

type clusterFacts struct {
	DataDir        string
	VersionNum     int
	FullPageWrites string
	Tablespaces    int
	WALSegmentSize int64
	InRecovery     bool
}

func (c *cloner) once(ctx context.Context, src cloneSource, dstData string) (*CloneStats, error) {
	began := time.Now()
	conn, err := pgx.Connect(ctx, src.DSN)
	if err != nil {
		return nil, fmt.Errorf("connect to the parent: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = conn.Close(cctx) // ends an unfinished backup session, which aborts the backup
	}()
	var f clusterFacts
	if err := conn.QueryRow(ctx, `
		select current_setting('data_directory'), current_setting('server_version_num')::int, current_setting('full_page_writes'),
		       (select count(*) from pg_tablespace where spcname not in ('pg_default', 'pg_global')),
		       (select bytes_per_wal_segment from pg_control_init()), pg_is_in_recovery()`).
		Scan(&f.DataDir, &f.VersionNum, &f.FullPageWrites, &f.Tablespaces, &f.WALSegmentSize, &f.InRecovery); err != nil {
		return nil, fmt.Errorf("inspect the parent: %w", err)
	}
	switch {
	case f.InRecovery:
		return nil, errors.New("the parent is in recovery; clone a primary")
	case f.VersionNum < 150000:
		return nil, fmt.Errorf("PostgreSQL %d is too old (pg_backup_start needs 15)", f.VersionNum)
	case f.FullPageWrites != "on":
		return nil, errors.New("full_page_writes is off, which makes file-level copies unsafe")
	case f.Tablespaces > 0:
		return nil, backup.ErrTablespaces
	}
	dstParent := filepath.Dir(dstData)
	if err := os.MkdirAll(dstData, 0o700); err != nil {
		return nil, err
	}
	st := &CloneStats{Method: c.method, FS: fsName(f.DataDir)}
	free0 := freeBytes(dstParent)

	var startLSN string
	if err := conn.QueryRow(ctx, `select pg_backup_start($1, true)::text`, "supavise branch "+filepath.Base(filepath.Dir(filepath.Dir(dstData)))).Scan(&startLSN); err != nil {
		return nil, fmt.Errorf("pg_backup_start: %w", err)
	}
	st.StartLSN = startLSN
	var startWAL string
	if err := conn.QueryRow(ctx, `select pg_walfile_name($1::pg_lsn)`, startLSN).Scan(&startWAL); err != nil {
		return nil, err
	}

	cp := &copier{ctx: ctx, clone: cloneFile, byteCopy: true, stats: st}
	copyStart := time.Now()
	if err := cp.copyDataDir(f.DataDir, dstData); err != nil {
		return nil, err
	}
	st.CopyMillis = time.Since(copyStart).Milliseconds()

	var stopLSN, labelFile, tablespaceMap, lastWAL string
	if err := conn.QueryRow(ctx, `select lsn::text, labelfile, spcmapfile, pg_walfile_name(lsn - 1) from pg_backup_stop(false)`).
		Scan(&stopLSN, &labelFile, &tablespaceMap, &lastWAL); err != nil {
		return nil, fmt.Errorf("pg_backup_stop: %w", err)
	}
	st.StopLSN = stopLSN
	if tablespaceMap != "" {
		return nil, backup.ErrTablespaces
	}
	// backup_label last of the data, written, not cloned: it comes from the stop call.
	if err := writeFileSync(filepath.Join(dstData, "backup_label"), []byte(labelFile), 0o600); err != nil {
		return nil, err
	}
	// The segments the backup needs: from the checkpoint it started at to the one holding
	// the end-of-backup record. They are still in the parent's pg_wal unless a checkpoint
	// recycled them, which a fast copy makes unlikely and a retry covers.
	names, err := walRange(startWAL, lastWAL, f.WALSegmentSize)
	if err != nil {
		return nil, err
	}
	walSrc, walDst := filepath.Join(f.DataDir, "pg_wal"), filepath.Join(dstData, "pg_wal")
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(walSrc, n)); err != nil {
			return nil, fmt.Errorf("%w: %s (%v)", errWALGone, n, err)
		}
		before := st.Files
		if err := cp.file(filepath.Join(walSrc, n), filepath.Join(walDst, n), "pg_wal/"+n); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s", errWALGone, n)
			}
			return nil, err
		}
		if st.Files == before { // vanished between the stat and the copy
			return nil, fmt.Errorf("%w: %s", errWALGone, n)
		}
		st.WALSegments++
	}
	if err := syncDirs(dstData); err != nil {
		return nil, err
	}
	st.TotalMillis = time.Since(began).Milliseconds()
	if free1 := freeBytes(dstParent); free0 >= 0 && free1 >= 0 {
		st.ExtraDiskByte = max(free0-free1, 0)
	}
	return st, nil
}

func writeFileSync(p string, b []byte, perm os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// walRange lists the segment file names from first to last inclusive (24 hex digits:
// timeline, log, segment). A backup never crosses a timeline.
func walRange(first, last string, segSize int64) ([]string, error) {
	if len(first) != 24 || len(last) != 24 || first[:8] != last[:8] {
		return nil, fmt.Errorf("branching: WAL range %s..%s is not on one timeline", first, last)
	}
	if segSize <= 0 {
		return nil, errors.New("branching: unknown WAL segment size")
	}
	perLog := uint64(0x100000000) / uint64(segSize)
	parse := func(s string) (uint64, uint64, error) {
		l, err := strconv.ParseUint(s[8:16], 16, 64)
		if err != nil {
			return 0, 0, err
		}
		g, err := strconv.ParseUint(s[16:24], 16, 64)
		return l, g, err
	}
	l, g, err := parse(first)
	if err != nil {
		return nil, err
	}
	el, eg, err := parse(last)
	if err != nil {
		return nil, err
	}
	var out []string
	for n := 0; ; n++ {
		out = append(out, fmt.Sprintf("%s%08X%08X", first[:8], l, g))
		if l == el && g == eg {
			return out, nil
		}
		if l > el || (l == el && g > eg) || n > 100000 {
			return nil, fmt.Errorf("branching: WAL range %s..%s is not ascending", first, last)
		}
		if g++; g == perLog {
			g, l = 0, l+1
		}
	}
}

// detectClone decides how a clone of the data directory srcData into a directory under
// dstParent can be made. method is "" when copy-on-write is not available, with reason
// saying why (it ends up in the branch's detail).
func detectClone(srcData, dstParent string) (method, fsys, reason string) {
	fsys = fsName(srcData)
	if err := os.MkdirAll(dstParent, 0o700); err != nil {
		return "", fsys, err.Error()
	}
	if !sameDevice(srcData, dstParent) {
		return "", fsys, fmt.Sprintf("the parent's data directory and %s are on different filesystems", dstParent)
	}
	probe := filepath.Join(dstParent, fmt.Sprintf(".supavise-clone-probe-%d", time.Now().UnixNano()))
	err := cloneFile(filepath.Join(srcData, "PG_VERSION"), probe)
	_ = os.Remove(probe)
	if err == nil {
		return cloneMethodName, fsys, ""
	}
	reason = fmt.Sprintf("%s does not support file cloning", fsys)
	if !errors.Is(err, errNoClone) {
		reason = fmt.Sprintf("file clone probe on %s failed: %v", fsys, err)
	}
	if fsys == "zfs" {
		// A snapshot of the dataset would still be copied byte by byte, and a zfs clone needs one
		// dataset per project, which the state directory does not have. Only block cloning (the
		// FICLONE probe above) gives a copy that does not grow with the database.
		reason += " (OpenZFS block cloning needs release 2.2 or later with the block_cloning feature active and cloning enabled; a snapshot copy would not be copy-on-write)"
	}
	return "", fsys, reason
}
