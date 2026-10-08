package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// FencedRecord is the local memory of being fenced. A node that restarts while it cannot reach its
// peers or the backup store reads it, so that fencing survives a restart; `node rejoin` removes it
// once the node is rebuilt.
type FencedRecord struct {
	// Epoch and Leader are the higher epoch and the node that holds it.
	Epoch  int64     `json:"epoch"`
	Leader string    `json:"leader"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// Peers are the peer addresses of the other nodes as this node's registry had them when it was
	// fenced, by node id: `node rejoin` needs the leader's address when the local registry is stopped.
	Peers map[string]string `json:"peers,omitempty"`
}

// FencedPath is where the record lives: /var/lib/supavise/fenced.json.
func FencedPath(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "fenced.json") }

// ReadFenced returns the record, or nil when the node is not fenced.
func ReadFenced(cfg *config.Config) (*FencedRecord, error) {
	b, err := os.ReadFile(FencedPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r FencedRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", FencedPath(cfg), err)
	}
	return &r, nil
}

// WriteFenced records that the node is fenced.
func WriteFenced(cfg *config.Config, r FencedRecord) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(FencedPath(cfg), append(b, '\n'), 0o644)
}

// ClearFenced removes the record; a node that has none is not an error.
func ClearFenced(cfg *config.Config) error {
	if err := os.Remove(FencedPath(cfg)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
