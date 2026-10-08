//go:build !linux && !darwin

package storagemigrate

// No extended attributes outside Linux and macOS, the platforms Storage's file backend runs on.

func readAttr(string, string) (string, bool, error) { return "", false, nil }

func readAttrFd(int, string) (string, bool, error) { return "", false, nil }

func writeAttr(string, string, string) (bool, error) { return false, nil }
