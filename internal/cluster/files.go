package cluster

import (
	"os"
	"path/filepath"
)

// writeFile writes data to path through a temporary file in the same directory, so that a reader
// never sees half of it, and gives it the owner of the directory it lands in: the commands that
// write these files run as root during an install and the daemon reads them as the supavise user.
func writeFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := mkdirAllOwned(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	chownLike(name, dir)
	return os.Rename(name, path)
}

// mkdirAllOwned creates dir and the parents that are missing with mode 0750, each owned like the
// directory it was created in.
func mkdirAllOwned(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return &os.PathError{Op: "mkdir", Path: dir, Err: os.ErrExist}
		}
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirAllOwned(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o750); err != nil && !os.IsExist(err) {
		return err
	}
	chownLike(dir, parent)
	return nil
}
