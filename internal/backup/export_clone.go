package backup

import "github.com/jsmillerdev/supavise/internal/lifecycle"

// Exports for the branching package, which copies a running cluster's data directory
// copy-on-write instead of streaming it into a tar. It must leave out exactly what a base
// backup leaves out, so the rules live in one place (tardir.go).

// DataDirSkip reports whether the entry at rel (slash separated, relative to PGDATA) is
// omitted from a base backup: temporary files, the postmaster's pid file and so on.
func DataDirSkip(rel string, isDir bool) bool { return skipEntry(rel, isDir) }

// DataDirKeepsEmpty reports whether the top-level directory name is created empty in a
// copy of the data directory (its contents are runtime state). pg_wal is one of them:
// callers copy the WAL segments a backup needs themselves.
func DataDirKeepsEmpty(name string) bool { return excludeDirContents[name] }

// DataDirInitPending is the launcher's marker that PGDATA is mid-initialization.
const DataDirInitPending = initPendingFile

// WithManager returns a copy of the service whose restores create projects through m. The
// copy shares the backend, the registry and every other setting. Branching uses it to make
// the projects of one restore into branches without touching the Manager the shared
// service was given (SetManager would change it for concurrent restores).
func (s *Service) WithManager(m lifecycle.Manager) *Service {
	c := *s
	c.opt.Manager = m
	return &c
}
