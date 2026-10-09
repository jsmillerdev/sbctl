package storagemigrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fsutil"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/storagemigrate/hold"
)

// The record of a run, its lock and the write hold live in the migration's own directory,
// <state>/system/storage-migrate/, which supavise-storage cannot write to (its unit may write
// below system/storage only). The objects and the directory that keeps them after the switch are
// Storage's, in <state>/system/storage/.
const (
	stateFile = "migrate.json"
	lockFile  = "migrate.lock"
	// retainedPrefix starts the name of the directory that keeps the files after the switch.
	retainedPrefix = "objects.migrated-"
)

func serviceDir(p config.Paths) string { return p.System(config.SvcStorage) }

// runDir is where the record of the run, its lock and the write hold are.
func runDir(p config.Paths) string { return hold.Dir(p) }

// RunDir is runDir for the command that makes the scratch directories of a preview in it: the
// state directory is the daemon's user's, not the world's like $TMPDIR.
func RunDir(p config.Paths) string { return runDir(p) }

// objectsDir is the directory Storage's file backend writes to (STORAGE_FILE_BACKEND_PATH).
func objectsDir(p config.Paths) string { return filepath.Join(serviceDir(p), "objects") }

// ReadState returns the record of the last run, or nil when there is none.
func ReadState(p config.Paths) (*State, error) {
	b, err := os.ReadFile(filepath.Join(runDir(p), stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(runDir(p), stateFile), err)
	}
	return &st, nil
}

func saveState(p config.Paths, st *State, now time.Time) error {
	st.UpdatedAt = now.UTC()
	st.PID = os.Getpid()
	return writeJSON(filepath.Join(runDir(p), stateFile), st)
}

// writeJSON replaces path with v atomically. The file is not secret; the daemon and the CLI can
// run as different users. The directory is created owned like its parent when this process runs as
// root: the daemon, which runs as the supavise user, removes an expired write hold from it.
func writeJSON(path string, v any) error {
	if err := fsutil.MkdirAllOwned(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return fsutil.WriteJSON(path, v, 0o644, fsutil.Options{})
}

// lock takes the run lock; a second run on the node (or a rollback during a migration) finds it
// held. The kernel drops it when the process dies, so a crashed run never blocks --resume.
func lock(p config.Paths) (release func(), err error) {
	path := filepath.Join(runDir(p), lockFile)
	if err := fsutil.MkdirAllOwned(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		if uid, gid, ok := fsutil.OwnerOf(filepath.Dir(path)); ok {
			_ = f.Chown(uid, gid)
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another supavise storage migrate is running on this node")
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Describe is the text of `supavise storage migrate --status`.
func Describe(st *State, now time.Time) string {
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%-12s %s\n", k+":", v) }
	phase := string(st.Phase)
	if st.Step != "" {
		phase += " (" + st.Step + ")"
	}
	line("phase", phase)
	line("bucket", st.Dest.Bucket)
	if st.Dest.Endpoint != "" {
		line("endpoint", st.Dest.Endpoint)
	}
	line("started", st.StartedAt.Format(time.RFC3339))
	if st.Passes > 0 {
		line("copied", fmt.Sprintf("%d objects, %s, in %d passes", st.Uploaded.Files, lifecycle.HumanBytes(st.Uploaded.Bytes), st.Passes))
	}
	if len(st.Tenants) > 0 {
		var rows, files, offline, unversioned int
		var bytes int64
		for _, t := range st.Tenants {
			rows += t.Rows
			files += t.Files
			bytes += t.Bytes
			unversioned += t.Unversioned
			if t.Offline {
				offline++
			}
		}
		line("verified", fmt.Sprintf("%d objects (%s) in %d projects; %d files in the directories", rows, lifecycle.HumanBytes(bytes), len(st.Tenants)-offline, files))
		if offline > 0 {
			line("", fmt.Sprintf("%d projects had no running database; only their copy was checked", offline))
		}
		if unversioned > 0 {
			line("", fmt.Sprintf("%d rows have no version, so their files could not be matched; the files were copied but not verified", unversioned))
		}
	}
	if st.SkippedTotal > 0 {
		line("skipped", fmt.Sprintf("%d files cannot be S3 keys and were not copied", st.SkippedTotal))
		for _, s := range st.Skipped {
			line("", fmt.Sprintf("%s: %s", s.Path, s.Reason))
		}
	}
	if st.Phase == PhaseRolledBack || st.Downloaded.Files > 0 || st.Removed > 0 {
		line("rolled back", fmt.Sprintf("%d objects (%s) copied back, %d removed", st.Downloaded.Files, lifecycle.HumanBytes(st.Downloaded.Bytes), st.Removed))
	}
	if st.RefusedKeys > 0 {
		line("refused", fmt.Sprintf("%d keys in the bucket cannot be files below the project's directory and were left there", st.RefusedKeys))
	}
	switch {
	case st.Cleaned:
		line("files", "deleted")
	case st.Retained != "":
		l := "kept as " + st.Retained
		if !st.RetainUntil.IsZero() {
			if now.Before(st.RetainUntil) {
				l += ", advised until " + st.RetainUntil.Format("2006-01-02")
			} else {
				l += " (the 14 days are over: --cleanup deletes them)"
			}
		}
		line("files", l)
	}
	if st.Error != "" {
		line("last error", st.Error)
	}
	if !st.Phase.Terminal() {
		line("next", "supavise storage migrate --resume")
		if st.Phase == PhaseFlipping && reached(flipSteps, st.Step, stepLive) {
			line("", "Storage runs on the bucket and may have taken writes: supavise storage migrate --rollback copies them back to the files")
		}
	}
	return b.String()
}
