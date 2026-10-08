//go:build linux || darwin

package storagemigrate

import (
	"errors"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// Storage's file backend keeps a file's content type and cache control in extended attributes
// (supabase/storage, src/storage/backend/file.ts): "user.supabase.*" on Linux,
// "com.apple.metadata.supabase.*" on macOS.
var attrPrefixes = []string{"user.supabase.", "com.apple.metadata.supabase."}

func writePrefix() string {
	if runtime.GOOS == "darwin" {
		return attrPrefixes[1]
	}
	return attrPrefixes[0]
}

func xattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, errNoAttr)
}

// readAttr returns Storage's attribute name of the file, and false when it has none.
func readAttr(path, name string) (string, bool, error) {
	for _, p := range attrPrefixes {
		v, ok, err := getxattr(path, p+name)
		if err != nil || ok {
			return v, ok, err
		}
	}
	return "", false, nil
}

func getxattr(path, name string) (string, bool, error) {
	for {
		n, err := unix.Getxattr(path, name, nil)
		if err != nil {
			if xattrUnsupported(err) {
				return "", false, nil
			}
			return "", false, err
		}
		buf := make([]byte, n)
		if n, err = unix.Getxattr(path, name, buf); err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue // grew since the size query
			}
			if xattrUnsupported(err) {
				return "", false, nil
			}
			return "", false, err
		}
		return string(buf[:n]), true, nil
	}
}

// writeAttr sets Storage's attribute name; false when the file system has no extended attributes.
func writeAttr(path, name, value string) (bool, error) {
	if err := unix.Setxattr(path, writePrefix()+name, []byte(value), 0); err != nil {
		if xattrUnsupported(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func openFileNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
