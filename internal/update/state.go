package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
type Store struct {
	Path string
	// pinned marks a Store from PinnedStore: it creates no directory but DefaultStateDir.
	pinned bool
}

// DefaultStateDir is where `supavise update run` keeps its record: the StateDirectory of
// supavise-upgrade.service. It is a fixed path, not <state_dir>-upgrade or anything else the config
// names: the unit runs as root, `supavise` owns config.toml, and a record whose place the config
// decides is a record the supavise account can send root to read and write somewhere else.
const DefaultStateDir = "/var/lib/supavise-upgrade"

// stateDirEnv is the variable systemd sets for a unit with StateDirectory=: the absolute path of
// the directory it created for the unit (a colon-separated list when there are several).
const stateDirEnv = "STATE_DIRECTORY"

// PinnedStateDir returns the directory of the record: $STATE_DIRECTORY when the service was started
// by systemd, DefaultStateDir (the directory that variable names for the shipped unit) when a person
// runs the command by hand. Nothing in config.toml reaches it. A value that is not one clean
// absolute path is an error, so that a unit edited to list several directories, or an environment
// that names a relative one, stops the command instead of choosing for it.
func PinnedStateDir(getenv func(string) string) (string, error) {
	v := getenv(stateDirEnv)
	switch {
	case v == "":
		return DefaultStateDir, nil
	case strings.Contains(v, ":"):
		return "", fmt.Errorf("$%s lists several directories (%q): the update record has one place", stateDirEnv, v)
	case !filepath.IsAbs(v) || filepath.Clean(v) != v:
		return "", fmt.Errorf("$%s is %q, not a clean absolute path", stateDirEnv, v)
	}
	return v, nil
}

// PinnedStore returns the Store of the node's update record, at the pinned directory. Unlike a Store
// literal it refuses to create a directory: systemd makes the unit's StateDirectory before the
// service starts, and a missing one elsewhere means the unit is not what it should be. DefaultStateDir
// is the one directory it creates (a person running `sudo supavise update run` before the timer
// ever ran), with the mode checkDir demands.
func PinnedStore(getenv func(string) string) (Store, error) {
	dir, err := PinnedStateDir(getenv)
	if err != nil {
		return Store{}, err
	}
	return Store{Path: filepath.Join(dir, "state.json"), pinned: true}, nil
}

// ensureDir makes sure the directory of the record exists. A Store from a literal creates it with
// MkdirAll (tests, the dry run's scratch directory); a pinned one never creates a directory but
// DefaultStateDir.
func (s Store) ensureDir() error {
	dir := filepath.Dir(s.Path)
	if !s.pinned {
		return os.MkdirAll(dir, 0o755)
	}
	if _, err := os.Lstat(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if dir != DefaultStateDir {
		return fmt.Errorf("%s does not exist: systemd creates it for supavise-upgrade.service (StateDirectory=), and the update record is not created anywhere else", dir)
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return os.Chmod(dir, 0o755) // the umask may have opened it up; checkDir refuses a directory others can write
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
	if err := s.ensureDir(); err != nil {
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
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	release, held, err := tryLock(filepath.Join(filepath.Dir(s.Path), "run.lock"))
	switch {
	case err != nil:
		return nil, err
	case held:
		return nil, ErrBusy
	}
	return release, nil
}
