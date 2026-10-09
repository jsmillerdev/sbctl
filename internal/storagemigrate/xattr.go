package storagemigrate

import "github.com/supavise/supavise/internal/storageattr"

// readAttr returns Storage's attribute name of the file at path, and false when it has none.
func readAttr(path, name string) (string, bool, error) {
	return readAttrWith(name, func(attr string) ([]byte, bool, error) { return storageattr.Get(path, attr) })
}

// readAttrFd is readAttr for an open file, which is the file whose bytes were read and not whatever
// the path names by the time the attributes are asked for.
func readAttrFd(fd int, name string) (string, bool, error) {
	return readAttrWith(name, func(attr string) ([]byte, bool, error) { return storageattr.GetFd(fd, attr) })
}

func readAttrWith(name string, get func(attr string) ([]byte, bool, error)) (string, bool, error) {
	for _, p := range storageattr.Prefixes {
		v, ok, err := get(p + name)
		if err != nil || ok {
			return string(v), ok, err
		}
	}
	return "", false, nil
}

// writeAttr sets Storage's attribute name; false when the file system has no extended attributes.
func writeAttr(path, name, value string) (bool, error) {
	return storageattr.Set(path, storageattr.WritePrefix()+name, []byte(value))
}
