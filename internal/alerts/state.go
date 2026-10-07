package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ActiveAlert is a problem that was sent and has not been resolved.
type ActiveAlert struct {
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	Ref       string    `json:"ref,omitempty"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail,omitempty"`
	FirstSent time.Time `json:"first_sent"`
	LastSent  time.Time `json:"last_sent"`
}

// state is what the notifier remembers between runs and between processes: the problems it
// has told the operator about, and when it last sent, for the hourly cap. It lives in
// <state_dir>/system/alerts.json, a file and not the registry, because the alerts that matter
// most are about the system cluster that holds the registry.
type state struct {
	Active map[string]ActiveAlert `json:"active"`
	// Sent are the times of the notifications of the last hour.
	Sent []time.Time `json:"sent"`
	// Held are the keys of the alerts the hourly cap kept back since the last notification that
	// went out. A key is counted once, however often the checker asks again.
	Held []string `json:"held,omitempty"`
}

// store reads and writes state under a lock that holds across processes (the daemon and a
// `supavise upgrade` run), so two of them never both decide to send the same alert.
type store struct {
	path string
	mu   sync.Mutex
}

func newStore(stateDir string) *store {
	return &store{path: filepath.Join(stateDir, "system", "alerts.json")}
}

// update runs fn on the current state and saves it when fn returns nil.
func (s *store) update(fn func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := s.handOver(lock.Name()); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	st, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.write(st)
}

// snapshot returns a copy of the state without taking the cross-process lock (a read only needs
// a file that is never half written).
func (s *store) snapshot() (*state, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

// Replaced by tests.
var (
	geteuid = os.Geteuid
	lchown  = os.Lchown
)

// handOver gives path to the owner of <state_dir>/system when root made it. `sudo supavise
// upgrade` raises events as root, and a state or lock file that root creates and keeps would
// lock the daemon (the supavise user) out of the alert state for good: every later Notify would
// fail on permissions and no alert would be delivered. A run by the supavise user itself needs
// nothing.
func (s *store) handOver(path string) error {
	if geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(filepath.Dir(s.path))
	if err != nil {
		return nil // no directory to take an owner from
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	if err := lchown(path, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("alerts: cannot give %s to the owner of the state directory: %w", path, err)
	}
	return nil
}

func (s *store) read() (*state, error) {
	st := &state{Active: map[string]ActiveAlert{}}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		// A damaged file must not silence the alerts for good: start over.
		return &state{Active: map[string]ActiveAlert{}}, nil
	}
	if st.Active == nil {
		st.Active = map[string]ActiveAlert{}
	}
	return st, nil
}

func (s *store) write(st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".alerts.")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if err := s.handOver(tmp.Name()); err != nil {
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
	return os.Rename(tmp.Name(), s.path)
}
