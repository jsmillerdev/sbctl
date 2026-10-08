package failover

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
)

// What a node does with the data of an old primary that another node replaced (design 2.10.8):
// the stale directory is kept for [failover] keep_diverged_days under the name data.diverged-<epoch>,
// so that writes the new primary never saw can be inspected, and a standby is built in its place
// from the current leader's archive. The janitor removes the directory when the time is up.

// divergedMarker is the file inside a set-aside directory that says when and why.
const divergedMarker = "DIVERGED.json"

// Diverged describes one set-aside directory.
type Diverged struct {
	Ref   string `json:"ref"`
	Epoch int64  `json:"epoch"`
	// At is when the directory was set aside.
	At time.Time `json:"at"`
	// ControlLSN is the old primary's last checkpoint and ForkLSN where the new timeline left the
	// old one; LostBytes is the WAL between them: what the old primary wrote that the new one never
	// received. Unknown (zero) when either position is not known.
	ControlLSN string `json:"control_lsn,omitempty"`
	ForkLSN    string `json:"fork_lsn,omitempty"`
	LostBytes  uint64 `json:"lost_bytes,omitempty"`
	// Path is the directory (not stored).
	Path string `json:"-"`
}

// SetAsideDiverged moves ref's data directory to data.diverged-<epoch> next to it, records why,
// and clears the project's fence record, so that a standby can be seeded where the primary was. A
// project with no data directory returns the zero Diverged and no error. controlLSN and forkLSN
// are optional; with both, the size of the lost tail is recorded.
func SetAsideDiverged(cfg *config.Config, ref string, epoch int64, controlLSN, forkLSN string, now time.Time) (Diverged, error) {
	paths := cfg.Paths()
	data := paths.PostgresData(ref)
	if _, err := os.Stat(data); errors.Is(err, fs.ErrNotExist) {
		return Diverged{}, nil
	} else if err != nil {
		return Diverged{}, fmt.Errorf("failover: %w", err)
	}
	dest := fmt.Sprintf("%s.diverged-%d", data, epoch)
	for i := 2; exists(dest); i++ {
		dest = fmt.Sprintf("%s.diverged-%d-%d", data, epoch, i)
	}
	if err := os.Rename(data, dest); err != nil {
		return Diverged{}, fmt.Errorf("failover: setting %s aside: %w", data, err)
	}
	d := Diverged{Ref: ref, Epoch: epoch, At: now.UTC(), ControlLSN: controlLSN, ForkLSN: forkLSN, Path: dest}
	if c, err := ParseLSN(controlLSN); err == nil {
		if f, err := ParseLSN(forkLSN); err == nil && c > f {
			d.LostBytes = c - f
		}
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	if err := os.WriteFile(filepath.Join(dest, divergedMarker), append(b, '\n'), 0o600); err != nil {
		return d, fmt.Errorf("failover: recording why %s was set aside: %w", dest, err)
	}
	if err := fenced.ClearProject(paths, ref); err != nil {
		return d, err
	}
	return d, nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// ListDiverged finds the set-aside directories of every project on the node.
func ListDiverged(cfg *config.Config) ([]Diverged, error) {
	pattern := filepath.Join(cfg.Paths().Root, "projects", "*", config.SvcPostgres, "data.diverged-*")
	dirs, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var out []Diverged
	for _, dir := range dirs {
		d := Diverged{Path: dir, Ref: filepath.Base(filepath.Dir(filepath.Dir(dir)))}
		if b, err := os.ReadFile(filepath.Join(dir, divergedMarker)); err == nil {
			_ = json.Unmarshal(b, &d)
			d.Path = dir
		} else if fi, err := os.Stat(dir); err == nil {
			d.At = fi.ModTime() // set aside by hand, or the marker was lost
		}
		out = append(out, d)
	}
	return out, nil
}

// SweepDiverged removes the set-aside directories older than keep and returns their paths. A
// directory with no readable time is kept.
func SweepDiverged(cfg *config.Config, keep time.Duration, now time.Time) ([]string, error) {
	all, err := ListDiverged(cfg)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, d := range all {
		if d.At.IsZero() || now.Sub(d.At) < keep {
			continue
		}
		// Only what this package made: the directory must still carry its name.
		if !strings.Contains(filepath.Base(d.Path), ".diverged-") {
			continue
		}
		if err := os.RemoveAll(d.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, d.Path)
	}
	return removed, errors.Join(errs...)
}
