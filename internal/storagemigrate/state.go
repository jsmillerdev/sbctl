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
)

// The files of a run live in Storage's own directory under the state directory, next to the
// objects: <state>/system/storage/.
const (
	stateFile = "migrate.json"
	lockFile  = "migrate.lock"
	// retainedPrefix starts the name of the directory that keeps the files after the switch.
	retainedPrefix = "objects.migrated-"
)

func serviceDir(p config.Paths) string { return p.System(config.SvcStorage) }

// objectsDir is the directory Storage's file backend writes to (STORAGE_FILE_BACKEND_PATH).
func objectsDir(p config.Paths) string { return filepath.Join(serviceDir(p), "objects") }

// ReadState returns the record of the last run, or nil when there is none.
func ReadState(p config.Paths) (*State, error) {
	b, err := os.ReadFile(filepath.Join(serviceDir(p), stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(serviceDir(p), stateFile), err)
	}
	return &st, nil
}

func saveState(p config.Paths, st *State, now time.Time) error {
	st.UpdatedAt = now.UTC()
	st.PID = os.Getpid()
	return writeJSON(filepath.Join(serviceDir(p), stateFile), st)
}

// writeJSON replaces path with v atomically. The file is not secret; the daemon and the CLI can
// run as different users.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".migrate.")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// lock takes the run lock; a second run on the node (or a rollback during a migration) finds it
// held. The kernel drops it when the process dies, so a crashed run never blocks --resume.
func lock(p config.Paths) (release func(), err error) {
	path := filepath.Join(serviceDir(p), lockFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
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
		line("copied", fmt.Sprintf("%d objects, %s, in %d passes", st.Uploaded.Files, bytesString(st.Uploaded.Bytes), st.Passes))
	}
	if len(st.Tenants) > 0 {
		var rows, files, offline int
		var bytes int64
		for _, t := range st.Tenants {
			rows += t.Rows
			files += t.Files
			bytes += t.Bytes
			if t.Offline {
				offline++
			}
		}
		line("verified", fmt.Sprintf("%d objects (%s) in %d projects; %d files in the directories", rows, bytesString(bytes), len(st.Tenants)-offline, files))
		if offline > 0 {
			line("", fmt.Sprintf("%d projects had no running database; only their copy was checked", offline))
		}
	}
	if st.SkippedTotal > 0 {
		line("skipped", fmt.Sprintf("%d files cannot be S3 keys and were not copied", st.SkippedTotal))
		for _, s := range st.Skipped {
			line("", fmt.Sprintf("%s: %s", s.Path, s.Reason))
		}
	}
	if st.Phase == PhaseRolledBack || st.Downloaded.Files > 0 || st.Removed > 0 {
		line("rolled back", fmt.Sprintf("%d objects (%s) copied back, %d removed", st.Downloaded.Files, bytesString(st.Downloaded.Bytes), st.Removed))
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
	}
	return b.String()
}

func bytesString(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
