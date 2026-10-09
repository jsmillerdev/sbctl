// Package storageattr reads and writes the extended attributes Storage's file backend keeps an
// object's metadata in. The Storage migration and the file backup share it.
//
// Storage keeps a file's content type, cache control, content encoding and ETag in extended
// attributes (supabase/storage, src/storage/backend/file.ts): "user.supabase.*" on Linux,
// "com.apple.metadata.supabase.*" on macOS. Without them a restored object is served as
// application/octet-stream with no cache headers.
//
// Outside Linux and macOS, the platforms Storage's file backend runs on, files have no such
// attributes: every read finds none and every write reports that the file system has none.
package storageattr

import "strings"

// Prefixes are the attribute name prefixes of Storage: the Linux one first, then the macOS one.
var Prefixes = []string{"user.supabase.", "com.apple.metadata.supabase."}

// IsStorageAttr reports whether name is one of Storage's attributes.
func IsStorageAttr(name string) bool {
	for _, p := range Prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
