package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jsmillerdev/supavise/internal/config"
)

// installedBinary is the path of the installed supavise binary that `supavise upgrade` and
// `supavise rollback` run, copy and replace as root. It does not come from the config: the
// supavise user owns /etc/supavise/config.toml, so a bin_path read from it would let that user
// name any file for root to run. The installer puts the binary at config.DefaultBinPath, which is
// the only path trusted here, and the path has to pass requireRootOwned: a regular file that
// root owns, in directories that root owns, none of them writable by anyone else.
func installedBinary(cfg *config.Config) (string, error) {
	if cfg.BinPath != config.DefaultBinPath {
		return "", fmt.Errorf("bin_path in the config is %q, but the installer puts the binary at %s and an upgrade runs only that one as root; remove bin_path from the config", cfg.BinPath, config.DefaultBinPath)
	}
	if err := requireRootOwned(config.DefaultBinPath, 0, "/"); err != nil {
		return "", err
	}
	return config.DefaultBinPath, nil
}

// requireRootOwned checks that no account but owner can change which file path names: path is a
// regular file owned by owner and not writable by its group or by others, and so is every directory
// above it up to and including top (a symlink on the way must be owner's too, and where it leads
// is checked the same way). owner is 0 on a node; tests pass their own uid and a directory they
// made as top.
func requireRootOwned(path string, owner uint32, top string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("the installed binary: %w", err)
	}
	chains := []string{path}
	if resolved != path {
		chains = append(chains, resolved)
	}
	for _, p := range chains {
		if err := requireOwnedChain(p, owner, top); err != nil {
			return err
		}
	}
	return nil
}

func requireOwnedChain(path string, owner uint32, top string) error {
	if err := requireOwnedEntry(path, owner, true); err != nil {
		return err
	}
	resolvedTop, err := filepath.EvalSymlinks(top)
	if err != nil {
		resolvedTop = top
	}
	for d := filepath.Dir(path); ; d = filepath.Dir(d) {
		if err := requireOwnedEntry(d, owner, false); err != nil {
			return err
		}
		if d == top || d == resolvedTop || d == filepath.Dir(d) {
			return nil
		}
	}
}

// requireOwnedEntry checks one path. file says it has to be a regular file, else it has to be a
// directory; a symlink has only its owner checked, because its mode means nothing.
func requireOwnedEntry(path string, owner uint32, file bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("the installed binary: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read the owner of %s", path)
	}
	if st.Uid != owner {
		return fmt.Errorf("%s belongs to uid %d, not to uid %d: refusing to run it as root", path, st.Uid, owner)
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return nil
	case file && !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file: refusing to run it as root", path)
	case !file && !fi.IsDir():
		return fmt.Errorf("%s is not a directory: refusing to run a file under it as root", path)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s can be changed by its group or by others (mode %o): refusing to run what is under it as root", path, fi.Mode().Perm())
	}
	return nil
}
