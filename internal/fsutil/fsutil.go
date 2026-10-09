// Package fsutil holds the file helpers the daemon and the CLI share: writing a file so that a
// reader never sees half of it, and giving a file the owner of the directory it lands in.
package fsutil

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Options says how WriteFile writes. The zero value writes and renames, with no fsync, into a
// directory that must exist.
type Options struct {
	// Sync fsyncs the file before the rename, so that a crash leaves the old or the new contents.
	Sync bool
	// SyncDir fsyncs the directory after the rename (best effort), so that the rename itself survives
	// a crash.
	SyncDir bool
	// MkdirMode, when non-zero, creates the missing parent directories with this mode.
	MkdirMode os.FileMode
	// OwnerLike, when set and the process runs as root, gives the file (and the directories MkdirMode
	// creates) the owner and group of this path; "dir" names the file's own directory. The commands
	// that write as root during an install leave files the daemon reads as the supavise user.
	OwnerLike string
}

// Dir is the Options.OwnerLike value that names the written file's own directory.
const Dir = "dir"

// WriteFile writes data to path through a temporary file in the same directory and renames it into
// place, with mode set before any data is written.
func WriteFile(path string, data []byte, mode os.FileMode, o Options) error {
	dir := filepath.Dir(path)
	owner := o.OwnerLike
	if owner == Dir {
		owner = dir
	}
	if o.MkdirMode != 0 {
		var err error
		if owner != "" {
			err = MkdirAllOwned(dir, o.MkdirMode)
		} else {
			err = os.MkdirAll(dir, o.MkdirMode)
		}
		if err != nil {
			return err
		}
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
	if o.Sync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if owner != "" {
		ChownLike(name, owner)
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if o.SyncDir {
		syncDir(dir)
	}
	return nil
}

// WriteJSON writes v as indented JSON with a trailing newline, through WriteFile.
func WriteJSON(path string, v any, mode os.FileMode, o Options) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(b, '\n'), mode, o)
}

// MkdirAllOwned creates dir and the parents that are missing with mode, each owned like the
// directory it was created in.
func MkdirAllOwned(dir string, mode os.FileMode) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return &os.PathError{Op: "mkdir", Path: dir, Err: os.ErrExist}
		}
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := MkdirAllOwned(parent, mode); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, mode); err != nil && !os.IsExist(err) {
		return err
	}
	ChownLike(dir, parent)
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return // the rename happened; durability of the directory entry is best effort
	}
	defer d.Close()
	_ = d.Sync()
}
