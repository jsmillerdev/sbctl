package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// walNameRE matches every file name Postgres passes to archive_command and
// restore_command: a segment ("000000010000000000000003"), a partial segment
// ("...partial"), a backup history file ("....00000028.backup") and a timeline
// history file ("00000002.history").
var walNameRE = regexp.MustCompile(`^(?:[0-9A-F]{24}(?:\.partial|\.[0-9A-F]{8}\.backup)?|[0-9A-F]{8}\.history)$`)

// WALExitFatal is the exit status of `sbctl wal fetch` when the archive could not be
// read. PostgreSQL treats a restore_command exit status above 125 (or a signal) as
// fatal and aborts recovery; every other non-zero status means "file not in the
// archive" and ends recovery at that point. A storage outage must not look like the
// end of the archive, or recovery would stop early and promote a cluster that is
// missing data.
const WALExitFatal = 126

// ErrWALConflict is returned by PushWAL when the archive already holds a different
// file under the same name. Postgres must never overwrite archived WAL.
var ErrWALConflict = errors.New("backup: archive already holds different content under this WAL file name")

func walKey(ref, name string) string { return walDir(ref) + name + ".zst" }

func checkWALName(name string) error {
	if !walNameRE.MatchString(name) {
		return fmt.Errorf("backup: %q is not a WAL file name", name)
	}
	return nil
}

// PushWAL implements Backup and the archive_command contract: it returns nil only
// after the compressed file is durable in the backend. Pushing a file identical to
// the archived one succeeds without rewriting it; a different file under the same
// name fails with ErrWALConflict and leaves the archive untouched.
func (s *Service) PushWAL(ctx context.Context, ref, path string) error {
	if err := validRef(ref); err != nil {
		return err
	}
	if !filepath.IsAbs(path) && s.opt.DataDir != nil {
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join(s.opt.DataDir(ref), path)
		}
	}
	name := filepath.Base(path)
	if err := checkWALName(name); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.PushWALReader(ctx, ref, name, f)
}

// PushWALReader is PushWAL for a file that arrives as a stream (the WAL relay): name is
// the WAL file name and r its whole content. A reader that fails before its end (a client
// that went away) makes the push fail and leaves nothing in the archive.
func (s *Service) PushWALReader(ctx context.Context, ref, name string, r io.Reader) error {
	if err := validRef(ref); err != nil {
		return err
	}
	if err := checkWALName(name); err != nil {
		return err
	}
	key := walKey(ref, name)
	st := s.opt.Store

	if _, err := st.Stat(ctx, key); err == nil {
		same, cerr := s.sameAsArchived(ctx, key, r)
		if cerr != nil {
			return cerr
		}
		if !same {
			return fmt.Errorf("%w: %s", ErrWALConflict, name)
		}
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	pr, pw := io.Pipe()
	go func() {
		enc, err := newEncoder(pw, 1)
		if err == nil {
			if _, err = io.Copy(enc, ctxReader{ctx, r}); err == nil {
				err = enc.Close()
			} else {
				enc.Close()
			}
		}
		pw.CloseWithError(err)
	}()
	err := st.Put(ctx, key, pr)
	pr.CloseWithError(err) // unblock the compressor if Put returned early
	return err
}

// sameAsArchived reports whether the object at key decompresses to exactly the bytes of r.
func (s *Service) sameAsArchived(ctx context.Context, key string, r io.Reader) (bool, error) {
	rc, err := s.opt.Store.Get(ctx, key)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	dec, err := newDecoder(rc)
	if err != nil {
		return false, err
	}
	defer dec.Close()
	return readersEqual(dec, ctxReader{ctx, r})
}

func readersEqual(a, b io.Reader) (bool, error) {
	const chunk = 1 << 20
	ba, bb := make([]byte, chunk), make([]byte, chunk)
	for {
		na, ea := io.ReadFull(a, ba)
		nb, eb := io.ReadFull(b, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			// A decode error on the archived side is corruption, not a difference.
			if ea != nil && !isEOF(ea) {
				return false, fmt.Errorf("backup: archived file is unreadable: %w", ea)
			}
			return false, nil
		}
		if ea != nil && !isEOF(ea) {
			return false, fmt.Errorf("backup: archived file is unreadable: %w", ea)
		}
		if eb != nil && !isEOF(eb) {
			return false, eb
		}
		if isEOF(ea) && isEOF(eb) {
			return true, nil
		}
	}
}

func isEOF(err error) bool { return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) }

// FetchWAL implements Backup and the restore_command contract. A file that is not
// in the archive yields an error wrapping ErrNoWAL (Postgres reads that as "end of
// archive"). Any other error means the archive could not be read, and the CLI turns
// it into an exit status that aborts recovery instead of ending it early.
// dest is written through a temporary file and renamed, so Postgres never sees a
// partial segment.
func (s *Service) FetchWAL(ctx context.Context, ref, name, dest string) (err error) {
	rc, err := s.OpenWAL(ctx, ref, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeFileAtomic(ctx, dest, rc, name)
}

// OpenWAL returns the decompressed content of an archived WAL file. A file that is not in
// the archive yields an error wrapping ErrNoWAL; any other error means the archive could
// not be read.
func (s *Service) OpenWAL(ctx context.Context, ref, name string) (io.ReadCloser, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if err := checkWALName(name); err != nil {
		return nil, err
	}
	rc, err := s.opt.Store.Get(ctx, walKey(ref, name))
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNoWAL, name)
	}
	if err != nil {
		return nil, err
	}
	dec, err := newDecoder(rc)
	if err != nil {
		rc.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{dec, closerFunc(func() error { dec.Close(); return rc.Close() })}, nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// writeFileAtomic writes r to dest through a temporary file in the same directory: it is
// synced and renamed, so Postgres never sees a partial segment.
func writeFileAtomic(ctx context.Context, dest string, r io.Reader, name string) (err error) {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+tmpMarker+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, ctxReader{ctx, r}); err != nil {
		return fmt.Errorf("backup: reading %s: %w", name, err)
	}
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	return syncDir(dir)
}

// walFileInfo is a parsed segment name.
type walFileInfo struct {
	Timeline uint32
	Seg      uint64 // log<<32 | seg, comparable within one timeline
	Kind     string // "segment", "partial", "backup", "history"
}

// parseWALName splits a name from checkWALName. History files have no segment.
func parseWALName(name string) (walFileInfo, bool) {
	if !walNameRE.MatchString(name) {
		return walFileInfo{}, false
	}
	var tli uint32
	if strings.HasSuffix(name, ".history") {
		fmt.Sscanf(name[:8], "%08X", &tli)
		return walFileInfo{Timeline: tli, Kind: "history"}, true
	}
	var log, seg uint32
	fmt.Sscanf(name[:8], "%08X", &tli)
	fmt.Sscanf(name[8:16], "%08X", &log)
	fmt.Sscanf(name[16:24], "%08X", &seg)
	kind := "segment"
	switch {
	case strings.HasSuffix(name, ".partial"):
		kind = "partial"
	case strings.HasSuffix(name, ".backup"):
		kind = "backup"
	}
	return walFileInfo{Timeline: tli, Seg: uint64(log)<<32 | uint64(seg), Kind: kind}, true
}
