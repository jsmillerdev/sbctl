//go:build !linux && !darwin

package backup

import "os"

// No extended attributes outside Linux and macOS (the platforms Storage's file backend runs on).

func readStorageAttrs(string) (map[string][]byte, error) { return nil, nil }

func writeStorageAttrs(string, map[string][]byte) (bool, error) { return false, nil }

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
