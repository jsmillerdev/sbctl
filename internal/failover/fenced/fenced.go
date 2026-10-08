// Package fenced is the record a node keeps once a peer has told it, or it has found out at boot,
// that it no longer leads (design 2.10.6 and 2.10.8). While the record exists no cluster of the
// node starts as a primary: the plane refuses to start the primary of a fenced project, the
// membership reports the node as fenced, and the proxy answers 503. `supavise node rejoin` removes it.
//
// The record is a file, not a registry row, because the registry of a node that lost the
// leadership is the diverged copy, and because the node may be unable to open it at all. A node
// record covers every project and the system cluster. A project record covers one project whose
// primary was fenced on its own (a project failover): the rest of the node carries on.
//
// The package imports only config, so that the lifecycle plane, the cluster membership and the
// failover orchestrator can all read it without a cycle.
package fenced

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// Record says who fenced the node or the project and why.
type Record struct {
	// Epoch is the cluster epoch that replaced this node or this project's primary.
	Epoch int64 `json:"epoch"`
	// Leader is the node that holds that epoch ("n2"); empty when the epoch was learned from the
	// backup store's leader marker without a name.
	Leader string `json:"leader,omitempty"`
	// Ref is the project of a project record; empty for a node record.
	Ref string `json:"ref,omitempty"`
	// Planned marks the hold of a planned stop: a switchover writes it before it stops the old primary,
	// so that nothing starts the primary again (a restart of the daemon, a reboot) while the project
	// moves, and the move clears it once the old home follows the new one or the move was undone
	// (ReleaseProject). A record without it comes from a fence, which only setting the data aside clears.
	Planned bool `json:"planned,omitempty"`
	// Reason is one sentence for the operator: what was seen, and where.
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// Removed and Peers belong to the membership layer, which writes the same file when it fences the
	// node itself (cluster.FencedRecord): Removed marks a node taken out of the cluster, and Peers are
	// the peer addresses by node id as this node's registry had them, which `supavise node rejoin`
	// needs to find the leader when the local registry is stopped. The fields are here so that a record
	// written by either side keeps what the other put in it.
	Removed bool              `json:"removed,omitempty"`
	Peers   map[string]string `json:"peers,omitempty"`
}

var refRe = regexp.MustCompile(`^(system|[a-z]{20})$`)

// ValidRef reports whether ref can name a project directory: the system project or a project ref.
// A ref that arrives from a peer is checked with it before it builds a path.
func ValidRef(ref string) bool { return refRe.MatchString(ref) }

// NodePath is the file of the node record: <state_dir>/fenced.json.
func NodePath(p config.Paths) string { return filepath.Join(p.Root, "fenced.json") }

// ProjectPath is the file of a project record: <state_dir>/projects/<ref>/fenced.json.
func ProjectPath(p config.Paths, ref string) string {
	return filepath.Join(p.Project(ref), "fenced.json")
}

// Node reads the node record; nil when the node is not fenced.
func Node(p config.Paths) (*Record, error) { return read(NodePath(p)) }

// Project reads the record of one project; nil when its primary is not fenced.
func Project(p config.Paths, ref string) (*Record, error) { return read(ProjectPath(p, ref)) }

// Blocks reports whether the primary of ref may not start on this node: the node is fenced, or
// ref is. A record that cannot be read blocks too, because the answer must fail closed.
func Blocks(p config.Paths, ref string) (*Record, bool) {
	for _, path := range []string{NodePath(p), ProjectPath(p, ref)} {
		r, err := read(path)
		if err != nil {
			return &Record{Ref: ref, Reason: err.Error()}, true
		}
		if r != nil {
			return r, true
		}
	}
	return nil, false
}

// WriteNode records that the node is fenced. A record that exists is kept when it holds a
// higher or equal epoch, so a late, stale request cannot lower it.
func WriteNode(p config.Paths, r Record) error { return write(NodePath(p), r) }

// WriteProject records that the primary of r.Ref is fenced on this node.
func WriteProject(p config.Paths, r Record) error {
	if !ValidRef(r.Ref) {
		return fmt.Errorf("fenced: %q is not a project ref", r.Ref)
	}
	return write(ProjectPath(p, r.Ref), r)
}

// ReleaseProject removes the hold of a planned stop (Planned) and leaves any other project record alone:
// the record of a fence says that the data diverged, and a planned move has no say in that.
func ReleaseProject(p config.Paths, ref string) error {
	if !ValidRef(ref) {
		return fmt.Errorf("fenced: %q is not a project ref", ref)
	}
	r, err := read(ProjectPath(p, ref))
	if err != nil {
		return err
	}
	if r == nil || !r.Planned {
		return nil
	}
	return remove(ProjectPath(p, ref))
}

// ClearNode removes the node record (`supavise node rejoin`).
func ClearNode(p config.Paths) error { return remove(NodePath(p)) }

// ClearProject removes a project record: the old primary's data was set aside, or the project was
// demoted in place into a replica of its new home.
func ClearProject(p config.Paths, ref string) error {
	if !ValidRef(ref) {
		return fmt.Errorf("fenced: %q is not a project ref", ref)
	}
	return remove(ProjectPath(p, ref))
}

func read(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fenced: %w", err)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("fenced: %s is not a record: %w", path, err)
	}
	return &r, nil
}

func write(path string, r Record) error {
	if old, err := read(path); err == nil && old != nil && old.Epoch >= r.Epoch {
		return nil
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("fenced: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fenced-*")
	if err != nil {
		return fmt.Errorf("fenced: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("fenced: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("fenced: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fenced: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fenced: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("fenced: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

func remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fenced: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil // the rename happened; durability is best effort
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}
