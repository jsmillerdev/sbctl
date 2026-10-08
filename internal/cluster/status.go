package cluster

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// Status is the daemon's live view of the cluster, written to the state directory every few
// seconds for `supavise status` and `supavise node ls`, which run in other processes and cannot see
// the daemon's memory. It holds node ids, versions and lag, no secrets.
type Status struct {
	At     time.Time     `json:"at"`
	Node   string        `json:"node"`
	Role   Role          `json:"role"`
	Epoch  int64         `json:"epoch"`
	Leader string        `json:"leader"`
	Fenced *FencedRecord `json:"fenced,omitempty"`
	// Peers are the nodes this one has a session with or tried, by id.
	Peers []PeerStatus `json:"peers,omitempty"`
	// Replicas are the observations the nodes last reported; only the leader has them.
	Replicas []ReplicaStatus `json:"replicas,omitempty"`
}

// PeerStatus is what this node knows of one peer.
type PeerStatus struct {
	Node      string    `json:"node"`
	Connected bool      `json:"connected"`
	RTTMillis float64   `json:"rtt_ms,omitempty"`
	LastPing  time.Time `json:"last_ping,omitzero"`
	Version   string    `json:"version,omitempty"`
	Epoch     int64     `json:"epoch,omitempty"`
	Leader    string    `json:"leader,omitempty"`
	Health    string    `json:"health,omitempty"`
}

// ReplicaStatus is the last observation of a replica instance.
type ReplicaStatus struct {
	Identifier     string    `json:"identifier"`
	Ref            string    `json:"ref"`
	Node           string    `json:"node"`
	Step           string    `json:"step,omitempty"`
	Error          string    `json:"error,omitempty"`
	ReceiverStatus string    `json:"receiver_status,omitempty"`
	LagSeconds     *float64  `json:"lag_seconds,omitempty"`
	ReportedAt     time.Time `json:"reported_at"`
}

// StatusPath is /var/lib/supavise/cluster-status.json.
func StatusPath(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "cluster-status.json") }

// WriteStatus replaces the status file.
func WriteStatus(cfg *config.Config, s Status) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFile(StatusPath(cfg), b, 0o644)
}

// ReadStatus returns the daemon's last status, or nil when it wrote none.
func ReadStatus(cfg *config.Config) (*Status, error) {
	b, err := os.ReadFile(StatusPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// StatusStale is how old a status may be before a reader says the daemon is not updating it.
const StatusStale = time.Minute
