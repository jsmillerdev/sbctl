package branching

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/OWNER/sbctl/internal/config"
)

// ErrInsufficientDisk is returned by Create and Reset when the state disk cannot hold a
// branch with data. The request is refused (409) before anything is created or removed.
var ErrInsufficientDisk = fmt.Errorf("%w: not enough free disk", ErrConflict)

// cloneSpaceFactor is how much of the parent's data a clone is assumed to need, in tenths: the
// data itself plus 20% for what the branch writes while it is set up (WAL, the rotation, the
// first migrations). A reflink or clonefile copy shares blocks with the parent and needs
// little at first, but it diverges as either side writes, so the check does not tell the
// copy-on-write methods apart: it is conservative on purpose.
const cloneSpaceFactor = 12

// dataSize is the apparent size of a PGDATA directory (pg_wal included). A directory that
// does not exist, or a file that vanishes during the walk, counts as zero.
func dataSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// requiredDisk is the free space a with_data branch of a parent holding size bytes needs:
// 1.2 times the data plus the configured reserve ([branching] disk_reserve_mb, default 2 GB).
func (s *Service) requiredDisk(size int64) int64 {
	return size/10*cloneSpaceFactor + s.cfg.Branching.DiskReserve()
}

// checkDisk refuses a with_data create or reset when the free space of the disk the
// branch's data would land on is below requiredDisk for the parent's data. credit is space
// the operation itself gives back before it needs the room (the old copy a reset removes).
// It runs before anything is created or removed. When the free space cannot be read the check
// is skipped with a warning rather than blocking every create on a filesystem that does not
// report it.
func (s *Service) checkDisk(parentRef string, credit int64) error {
	dir := s.cfg.Paths().ProjectService(parentRef, config.SvcPostgres)
	size := dataSize(filepath.Join(dir, "data"))
	// The parent's directory exists on a running node; the nearest ancestor that does is on
	// the same filesystem.
	free := s.freeBytes(dir)
	for d := dir; free < 0 && filepath.Dir(d) != d; {
		d = filepath.Dir(d)
		free = s.freeBytes(d)
	}
	if free < 0 {
		s.log.Warn("branch disk check skipped: the free space could not be read", "dir", dir)
		return nil
	}
	need := s.requiredDisk(size)
	if free+credit >= need {
		return nil
	}
	return fmt.Errorf("%w: the disk of the state directory has %s free and cloning project %s needs about %s "+
		"(its data is %s, times 1.2, plus a reserve of %s, [branching] disk_reserve_mb). "+
		"Free some disk space, delete branches you do not need, or lower the reserve, then try again",
		ErrInsufficientDisk, humanBytes(free+credit), parentRef, humanBytes(need), humanBytes(size), humanBytes(s.cfg.Branching.DiskReserve()))
}
