//go:build !linux && !darwin

package storagemigrate

import "os"

// No extended attributes outside Linux and macOS, the platforms Storage's file backend runs on.

func readAttr(string, string) (string, bool, error) { return "", false, nil }

func writeAttr(string, string, string) (bool, error) { return false, nil }

func openFileNoFollow(path string) (*os.File, error) { return os.Open(path) }
