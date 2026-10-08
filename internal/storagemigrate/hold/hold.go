// Package hold is the marker with which `supavise storage migrate` asks the edge proxy to answer
// Storage's writes with 503 while it switches Storage between backends. It is a package of its own
// so that the proxy can read it without importing the migration.
//
// The marker is a small JSON file in Storage's directory under the state directory. It expires, and
// the run that wrote it renews it, so a run that dies cannot block writes for longer than the
// expiry.
package hold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// File is the name of the marker.
const File = "write-hold.json"

// Marker is the content of the file.
type Marker struct {
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

// Path is where the marker is.
func Path(p config.Paths) string { return filepath.Join(p.System(config.SvcStorage), File) }

// Held reports whether Storage's writes are held at now: the marker exists and has not expired. The
// edge proxy answers a Storage request that changes data (any method but GET, HEAD and OPTIONS) with
// 503 and `Retry-After: 5` while it is true; reads go on.
func Held(p config.Paths, now time.Time) bool {
	m, ok := read(p)
	return ok && now.Before(m.Until)
}

func read(p config.Paths) (Marker, bool) {
	b, err := os.ReadFile(Path(p))
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	return m, json.Unmarshal(b, &m) == nil
}

// Write replaces the marker. The file is not secret, and a reader never sees half of it.
func Write(p config.Paths, m Marker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	dir := filepath.Dir(Path(p))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hold.")
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
	return os.Rename(tmp.Name(), Path(p))
}

// Remove deletes the marker; a marker that is not there is not an error.
func Remove(p config.Paths) error {
	if err := os.Remove(Path(p)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearStale removes a marker that has expired and reports whether it did.
func ClearStale(p config.Paths, now time.Time) bool {
	m, ok := read(p)
	if !ok || now.Before(m.Until) {
		return false
	}
	return os.Remove(Path(p)) == nil
}
