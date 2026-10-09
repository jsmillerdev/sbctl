//go:build linux || darwin

package storageattr

import (
	"errors"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// WritePrefix is the prefix this platform writes Storage's attributes under.
func WritePrefix() string {
	if runtime.GOOS == "darwin" {
		return Prefixes[1]
	}
	return Prefixes[0]
}

// unsupported reports an error that means "this file system has no extended attributes here",
// which is a state, not a failure.
func unsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, errNoAttr)
}

// Get reads the attribute name of the file at path. It reports false, not an error, when the file
// has none or the file system has no extended attributes.
func Get(path, name string) ([]byte, bool, error) {
	return get(name, func(attr string, dest []byte) (int, error) { return unix.Getxattr(path, attr, dest) })
}

// GetFd is Get for an open file, which is the file whose bytes were read and not whatever the path
// names by the time the attributes are asked for.
func GetFd(fd int, name string) ([]byte, bool, error) {
	return get(name, func(attr string, dest []byte) (int, error) { return unix.Fgetxattr(fd, attr, dest) })
}

func get(name string, read func(attr string, dest []byte) (int, error)) ([]byte, bool, error) {
	for {
		n, err := read(name, nil)
		if err != nil {
			if unsupported(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		buf := make([]byte, n)
		if n, err = read(name, buf); err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue // grew since the size query
			}
			if unsupported(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return buf[:n], true, nil
	}
}

// Set sets the attribute name of the file at path. It reports false, not an error, when the file
// system has no extended attributes.
func Set(path, name string, value []byte) (bool, error) {
	if err := unix.Setxattr(path, name, value, 0); err != nil {
		if unsupported(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReadAll returns Storage's extended attributes of the regular file at path, by their full names;
// nil when the file has none or the file system has no extended attributes.
func ReadAll(path string) (map[string][]byte, error) {
	var buf []byte
	for {
		n, err := unix.Listxattr(path, nil)
		if err != nil {
			if unsupported(err) {
				return nil, nil
			}
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		buf = make([]byte, n)
		if n, err = unix.Listxattr(path, buf); err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue // grew since the size query
			}
			return nil, err
		}
		buf = buf[:n]
		break
	}
	var out map[string][]byte
	for _, name := range strings.Split(string(buf), "\x00") {
		if !IsStorageAttr(name) {
			continue
		}
		val, ok, err := Get(path, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // removed meanwhile
		}
		if out == nil {
			out = map[string][]byte{}
		}
		out[name] = val
	}
	return out, nil
}
