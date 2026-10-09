//go:build !linux && !darwin

package storageattr

// WritePrefix is the prefix of the Linux attributes, which nothing writes here.
func WritePrefix() string { return Prefixes[0] }

// Get finds no attribute.
func Get(string, string) ([]byte, bool, error) { return nil, false, nil }

// GetFd finds no attribute.
func GetFd(int, string) ([]byte, bool, error) { return nil, false, nil }

// Set reports that the file system has no extended attributes.
func Set(string, string, []byte) (bool, error) { return false, nil }

// ReadAll finds no attribute.
func ReadAll(string) (map[string][]byte, error) { return nil, nil }
