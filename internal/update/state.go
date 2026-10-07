package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is what `supavise update run` remembers between runs, as JSON in <state_dir>/update/state.json.
// It is the node's own bookkeeping, not configuration: deleting the file loses the memory of
// which release was announced and which window already ran, and nothing else.
type State struct {
	// CheckedAt is when GitHub was last asked for the latest release, and Latest what it answered.
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Latest    string    `json:"latest,omitempty"`
	// Notified is the release the update_available event was logged for, so a release is
	// announced once, not at every check.
	Notified string `json:"notified,omitempty"`
	// Window is the opening time of the maintenance window occurrence in which an unattended
	// upgrade last ran to an end (upgraded, nothing to do, rolled back or failed): the window gets
	// one such attempt. A refusal (exit 2) leaves it unset, so the next tick of the window retries.
	Window time.Time `json:"window,omitempty"`
	// Result is the outcome of the last unattended upgrade.
	Result *Result `json:"result,omitempty"`
	// Blocked is why automatic upgrades stopped: the last one failed in a way that needs a person
	// (exit 4). `supavise update resume` clears it.
	Blocked string `json:"blocked,omitempty"`
	// RebootWindow is the opening time of the window occurrence in which the node last rebooted
	// itself for an OS patch: one reboot per window, whatever the marker file says afterwards.
	RebootWindow time.Time `json:"reboot_window,omitempty"`
}

// Result is the outcome of one `supavise upgrade --unattended` run.
type Result struct {
	At      time.Time `json:"at"`
	Exit    int       `json:"exit"`
	Meaning string    `json:"meaning"`
	// Version is the running Supavise version when the upgrade started.
	Version string `json:"version,omitempty"`
}

// Store reads and writes State.
type Store struct{ Path string }

// StatePath is where the state of a node with the given state directory lives.
func StatePath(stateDir string) string { return filepath.Join(stateDir, "update", "state.json") }

// Load returns the saved state, or an empty one when there is none. A file that does not parse
// is an error, not silently replaced: it would make the node announce the same release again
// and forget a block.
func (s Store) Load() (State, error) {
	var st State
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w (delete it to start over)", s.Path, err)
	}
	return st, nil
}

// Save writes st atomically.
func (s Store) Save(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}
