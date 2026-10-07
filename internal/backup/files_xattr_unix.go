//go:build linux || darwin

package backup

import (
	"errors"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Storage's file backend keeps a file's content type, cache control, content encoding and
// ETag in extended attributes (supabase/storage, src/storage/backend/file.ts): "user.supabase.*" on Linux, "com.apple.metadata.supabase.*" on macOS. Without them a restored object is served as application/octet-stream with no cache headers.
var storageAttrPrefixes = []string{"user.supabase.", "com.apple.metadata.supabase."}

func isStorageAttr(name string) bool {
	for _, p := range storageAttrPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// xattrUnsupported reports an error that means "this file system has no extended attributes
// here", which is a state, not a failure.
func xattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, errNoAttr)
}

// readStorageAttrs returns the Storage extended attributes of the regular file at path.
func readStorageAttrs(path string) (map[string][]byte, error) {
	var buf []byte
	for {
		n, err := unix.Listxattr(path, nil)
		if err != nil {
			if xattrUnsupported(err) {
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
		if !isStorageAttr(name) {
			continue
		}
		var val []byte
		for {
			n, err := unix.Getxattr(path, name, nil)
			if err != nil {
				if xattrUnsupported(err) {
					break // removed meanwhile
				}
				return nil, err
			}
			val = make([]byte, n)
			if n, err = unix.Getxattr(path, name, val); err != nil {
				if errors.Is(err, unix.ERANGE) {
					continue
				}
				if xattrUnsupported(err) {
					val = nil
					break
				}
				return nil, err
			}
			val = val[:n]
			break
		}
		if val == nil {
			continue
		}
		if out == nil {
			out = map[string][]byte{}
		}
		out[name] = val
	}
	return out, nil
}

// writeStorageAttrs sets attrs on path. It reports false, not an error, when the file
// system has no extended attributes.
func writeStorageAttrs(path string, attrs map[string][]byte) (bool, error) {
	for name, val := range attrs {
		if !isStorageAttr(name) {
			continue // a tampered snapshot does not get to set other attributes
		}
		if err := unix.Setxattr(path, name, val, 0); err != nil {
			if xattrUnsupported(err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// openNoFollow opens path for reading and refuses a symbolic link.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
