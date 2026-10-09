package cluster

import (
	"path/filepath"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/fsutil"
)

// FencedRecord is the local memory of being fenced. A node that restarts while it cannot reach its
// peers or the backup store reads it, so that fencing survives a restart; `node rejoin` removes it
// once the node is rebuilt. It is the record of the failover/fenced package, which the failover
// procedure writes to the same file.
type FencedRecord = fenced.Record

// FencedPath is where the record lives: /var/lib/supavise/fenced.json.
func FencedPath(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "fenced.json") }

// ReadFenced returns the record, or nil when the node is not fenced.
func ReadFenced(cfg *config.Config) (*FencedRecord, error) { return fenced.Node(cfg.Paths()) }

// WriteFenced records that the node is fenced. It overwrites a record that exists, whatever its
// epoch (fenced.WriteNode keeps the higher one), and the file is readable by every user.
func WriteFenced(cfg *config.Config, r FencedRecord) error {
	return fsutil.WriteJSON(FencedPath(cfg), r, 0o644, writeOpts)
}

// ClearFenced removes the record; a node that has none is not an error.
func ClearFenced(cfg *config.Config) error { return fenced.ClearNode(cfg.Paths()) }
