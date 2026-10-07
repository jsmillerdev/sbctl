package backup

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
