package backup

import "github.com/supavise/supavise/internal/storageattr"

// readStorageAttrs returns the Storage extended attributes of the regular file at path.
func readStorageAttrs(path string) (map[string][]byte, error) { return storageattr.ReadAll(path) }

// writeStorageAttrs sets attrs on path. It reports false, not an error, when the file
// system has no extended attributes.
func writeStorageAttrs(path string, attrs map[string][]byte) (bool, error) {
	for name, val := range attrs {
		if !storageattr.IsStorageAttr(name) {
			continue // a tampered snapshot does not get to set other attributes
		}
		if ok, err := storageattr.Set(path, name, val); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}
