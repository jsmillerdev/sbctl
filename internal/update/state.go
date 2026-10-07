package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is what `supavise update run` remembers between runs, as JSON in StatePath.
// It is the node's own bookkeeping, not configuration: deleting the file loses the memory of
// which window already ran and any pause after a failed upgrade, and nothing else.
type State struct {
	// Window is the opening time of the maintenance window occurrence in which an unattended
	// upgrade last ran to an end (upgraded, nothing to do, rolled back or failed) or was skipped
	// because it would repeat a rolled-back release: the window gets one such attempt. A refusal (exit 2) leaves it unset, so the next tick of the window retries.
	Window time.Time `json:"window,omitempty"`
	// InProgress is set (and saved) before an unattended upgrade starts and cleared when the
	// upgrade command returns. A run that finds it set means the upgrade was cut short with no
	// word of how it ended (a crash, an OOM kill, a power cut, a kill of the service): Run then
	// pauses automatic upgrades as needing the operator, as for exit 4, instead of starting the
	// upgrade again.
	InProgress *Progress `json:"in_progress,omitempty"`
	// Result is the outcome of the last unattended upgrade.
	Result *Result `json:"result,omitempty"`
	// Blocked is why automatic upgrades stopped: the last one failed in a way that needs a person
	// (exit 4), or was cut short with no result (InProgress). `supavise update resume` clears it.
	Blocked string `json:"blocked,omitempty"`
	// RolledBack is the release the last unattended upgrade rolled back (exit 3). Automatic
	// upgrades skip it until a newer release exists, so that a release that cannot upgrade this
	// node does not cost it an upgrade-and-rollback outage every week. `supavise update resume`
	// clears it, and so does a successful upgrade.
	RolledBack string `json:"rolled_back,omitempty"`
	// RebootWindow is the opening time of the window occurrence in which the node last rebooted
	// itself for an OS patch: one reboot per window, whatever the marker file says afterwards.
	RebootWindow time.Time `json:"reboot_window,omitempty"`
}

// Progress is the record of an unattended upgrade that has started.
type Progress struct {
	Window  time.Time `json:"window"`
	Started time.Time `json:"started"`
	Version string    `json:"version,omitempty"`
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

// StatePath is where the state of a node with the given state directory lives: a directory of its
// own next to it, <state_dir>-upgrade (/var/lib/supavise-upgrade, the unit's StateDirectory). The
// state decides whether a root-run service upgrades and reboots the node, so it must not sit in
// the state directory, which every supavise unit (a project's Postgres among them) can write.
func StatePath(stateDir string) string {
	return filepath.Join(filepath.Clean(stateDir)+"-upgrade", "state.json")
}

// checkDir refuses a state directory that someone else could have changed: a symlink, a directory
// that another user owns, or one that group or others can write. The service runs as root and
// creates and renames files in it.
func (s Store) checkDir() error {
	dir := filepath.Dir(s.Path)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() { // Lstat: a symlink to a directory is not a directory here
		return fmt.Errorf("%s is not a plain directory (a symlink?); remove it", dir)
	}
	if owner, ok := ownerOf(fi); ok && owner != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not by uid %d that runs the update: refusing to trust it", dir, owner, os.Geteuid())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (%s): chmod 755 it", dir, fi.Mode().Perm())
	}
	return nil
}

// Load returns the saved state, or an empty one when there is none. A file that does not parse
// is an error, not silently replaced: it would make the node announce the same release again
// and forget a block.
func (s Store) Load() (State, error) {
	var st State
	if os.Geteuid() == 0 { // root acts on what it reads; anyone else only displays it
		if err := s.checkDir(); err != nil {
			return st, err
		}
	}
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
	if err := s.checkDir(); err != nil {
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

// ErrBusy means another `supavise update run` holds the lock.
var ErrBusy = errors.New("another `supavise update run` is in progress")

// Lock takes the lock that keeps two passes from running at once (the timer's service and a
// run by hand): the second one would otherwise read the first one's in-progress record as a
// crashed upgrade. The lock is a flock, so a crash releases it. It sits next to the state file.
func (s Store) Lock() (release func(), err error) {
	release, held, err := tryLock(filepath.Join(filepath.Dir(s.Path), "run.lock"))
	switch {
	case err != nil:
		return nil, err
	case held:
		return nil, ErrBusy
	}
	return release, nil
}
