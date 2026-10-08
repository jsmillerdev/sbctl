package alerts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
//
// <state_dir>/system belongs to the supavise account, so the root CLI must not trust what is in
// it: every file is opened through an os.Root on that directory (a link that leaves it is
// refused) with O_NOFOLLOW (a link inside it is refused too), and must be a regular file. As
// root the directory itself must be a real directory, not a link, owned by root or by the owner
// of the state directory.
type store struct {
	path string
	mu   sync.Mutex
}

func newStore(stateDir string) *store {
	return &store{path: filepath.Join(stateDir, "system", "alerts.json")}
}

// openDir opens <state_dir>/system, creating it when create is set. It returns nil, nil when the
// directory does not exist and create is not set.
func (s *store) openDir(create bool) (*os.Root, error) {
	dir := filepath.Dir(s.path)
	if create {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	root := geteuid() == 0
	var linfo os.FileInfo
	if root {
		var err error
		if linfo, err = os.Lstat(dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) && !create {
				return nil, nil
			}
			return nil, err
		}
		if err := s.trustedDir(dir, linfo); err != nil {
			return nil, err
		}
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !create {
			return nil, nil
		}
		return nil, err
	}
	if root {
		// The path may have been swapped for a link between the check and the open.
		oinfo, err := r.Stat(".")
		if err != nil || !os.SameFile(linfo, oinfo) {
			r.Close()
			return nil, fmt.Errorf("alerts: %s changed while it was opened: refusing to use it", dir)
		}
	}
	return r, nil
}

// trustedDir is the check root makes before it writes into dir.
func (s *store) trustedDir(dir string, fi os.FileInfo) error {
	if !fi.IsDir() { // a link is not a directory in an Lstat
		return fmt.Errorf("alerts: %s is not a plain directory (a link?): refusing to write the alert state there as root", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if st.Uid == 0 {
		return nil
	}
	parent, err := os.Stat(filepath.Dir(dir))
	if err != nil {
		return err
	}
	if ps, ok := parent.Sys().(*syscall.Stat_t); ok && ps.Uid == st.Uid {
		return nil
	}
	return fmt.Errorf("alerts: %s is owned by uid %d, neither root nor the owner of the state directory: refusing to write the alert state there as root", dir, st.Uid)
}

// openFile opens name in dir without following a link, and refuses anything but a regular file.
func openFile(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := dir.OpenFile(name, flag|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("alerts: %s is not a regular file: refusing to use it", name)
	}
	return f, nil
}

// update runs fn on the current state and saves it when fn returns nil.
func (s *store) update(fn func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.openDir(true)
	if err != nil {
		return err
	}
	defer dir.Close()
	lock, err := openFile(dir, filepath.Base(s.path)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := s.handOver(lock); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	st, err := s.read(dir)
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.write(dir, st)
}

// snapshot returns a copy of the state without taking the cross-process lock (a read only needs
// a file that is never half written).
func (s *store) snapshot() (*state, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.openDir(false)
	if err != nil {
		return nil, err
	}
	if dir == nil {
		return &state{Active: map[string]ActiveAlert{}}, nil
	}
	defer dir.Close()
	return s.read(dir)
}

// Replaced by tests.
var (
	geteuid = os.Geteuid
	fchown  = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
)

// handOver gives f to the owner of <state_dir>/system when root made it. `sudo supavise
// upgrade` raises events as root, and a state or lock file that root creates and keeps would
// lock the daemon (the supavise user) out of the alert state for good: every later Notify would
// fail on permissions and no alert would be delivered. A run by the supavise user itself needs
// nothing. It changes the open file, so a link swapped in for the name cannot redirect it.
func (s *store) handOver(f *os.File) error {
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
	if err := fchown(f, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("alerts: cannot give %s to the owner of the state directory: %w", f.Name(), err)
	}
	return nil
}

func (s *store) read(dir *os.Root) (*state, error) {
	st := &state{Active: map[string]ActiveAlert{}}
	f, err := openFile(dir, filepath.Base(s.path), os.O_RDONLY, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	b, err := io.ReadAll(f)
	f.Close()
	if err != nil {
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

func (s *store) write(dir *os.Root, st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmpName := ".alerts." + hex.EncodeToString(suffix[:])
	tmp, err := openFile(dir, tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer dir.Remove(tmpName) //nolint:errcheck
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if err := s.handOver(tmp); err != nil {
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
	return dir.Rename(tmpName, filepath.Base(s.path))
}
