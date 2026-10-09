//go:build !linux && !darwin

package backup

import "os"

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
