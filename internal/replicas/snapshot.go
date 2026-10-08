package replicas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

// The leader keeps lag and receiver state in memory (I5). A command that runs in another process,
// `supavise replicas ls`, reads a snapshot file the controller rewrites on every pass. It holds
// what that command shows (each replica's receiver status and lag), no LSNs and no secrets, and is
// readable by the daemon's user only, like the registry connection `ls` needs anyway. It is a view
// of the last pass, not state: nothing reads it back into the controller.

// SnapshotName is the file in the state directory.
const SnapshotName = "replicas-status.json"

// snapshotMaxAge is how old a snapshot may be before the reader ignores it: the controller writes
// one every pass, and a stopped daemon leaves the last one behind.
const snapshotMaxAge = 2 * time.Minute

// Snapshot is what the controller observed at one pass.
type Snapshot struct {
	At       time.Time       `json:"at"`
	Replicas []SnapshotEntry `json:"replicas"`
}

// SnapshotEntry is one replica as observed.
type SnapshotEntry struct {
	Identifier string    `json:"identifier"`
	Receiver   string    `json:"receiver,omitempty"`
	LagSeconds *float64  `json:"lag_seconds,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// Entry returns the entry of identifier.
func (s *Snapshot) Entry(identifier string) (SnapshotEntry, bool) {
	if s == nil {
		return SnapshotEntry{}, false
	}
	for _, e := range s.Replicas {
		if e.Identifier == identifier {
			return e, true
		}
	}
	return SnapshotEntry{}, false
}

// ReadSnapshot reads the snapshot at path. It returns nil when there is none, it is unreadable, or
// it is older than two minutes at now: the daemon is not running, and its numbers are not current.
func ReadSnapshot(path string, now time.Time) *Snapshot {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s Snapshot
	if json.Unmarshal(b, &s) != nil || now.Sub(s.At) > snapshotMaxAge {
		return nil
	}
	return &s
}

// writeSnapshot rewrites the snapshot file from the observations. Failure to write is logged
// at debug level: the file is a convenience.
func (c *Controller) writeSnapshot(rows []registry.Replica) {
	path := c.o.SnapshotPath
	if path == "" {
		return
	}
	snap := Snapshot{At: c.now().UTC(), Replicas: []SnapshotEntry{}}
	c.mu.Lock()
	for _, r := range rows {
		s := c.state[r.Identifier]
		if s == nil || s.obs == nil {
			continue
		}
		snap.Replicas = append(snap.Replicas, SnapshotEntry{
			Identifier: r.Identifier, Receiver: s.obs.ReceiverStatus, LagSeconds: s.obs.LagSeconds, ObservedAt: s.obsAt.UTC(),
		})
	}
	c.mu.Unlock()
	b, err := json.Marshal(snap)
	if err == nil {
		err = writeFileAtomic(path, b)
	}
	if err != nil {
		c.log.Debug("replicas: write the status snapshot", "path", path, "error", err)
	}
}

// writeFileAtomic replaces path with b, mode 0600, so a reader never sees half a file.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
