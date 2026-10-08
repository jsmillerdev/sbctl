package storagemigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// rowPath is where the file of a row's object lies below the project's directory, which is also its
// key below <ref>/ in the bucket (spike S5).
func rowPath(r Row) string { return r.Bucket + "/" + r.Name + "/" + r.Version }

// VerifyError is the reason a project's objects are not all in the bucket. The run stops at it: a
// switch would make those objects disappear.
type VerifyError struct {
	Ref string
	// Missing are the rows whose object is not in the files, MismatchedSize those whose size differs
	// from what the database recorded, NotInBucket the files the bucket does not hold.
	Missing, MismatchedSize, NotInBucket []string
	Rows                                 int
}

func (v *VerifyError) Error() string {
	var parts []string
	add := func(n []string, what string) {
		if len(n) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s (%s)", len(n), what, strings.Join(first(n, 3), ", ")))
		}
	}
	add(v.Missing, "objects the database lists have no file")
	add(v.MismatchedSize, "objects differ in size from the database")
	add(v.NotInBucket, "files are not in the bucket")
	return fmt.Sprintf("verification of project %s failed: %s; nothing was switched", v.Ref, strings.Join(parts, "; "))
}

func first[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// checkRows compares the rows of ref's database with inv. Rows whose object is in inv at the size
// the row records are counted; the others come back in pending.
func (e *Engine) checkRows(ctx context.Context, ref string, inv inventory) (ok Counts, pending []Row, err error) {
	err = e.d.Tenants.Rows(ctx, ref, func(r Row) error {
		if f, found := inv.find(rowPath(r)); found && (!r.HasSize || r.Size == f.Size) {
			ok.Files++
			ok.Bytes += f.Size
			return nil
		}
		pending = append(pending, r)
		return nil
	})
	return ok, pending, err
}

// verifyTenant checks that every object the project's database lists is in the bucket at the size it
// records, and that the bucket holds every file of the project. The order matters while Storage
// takes writes: the rows are read first, the files are copied after, and only then are the rows that
// had no file yet looked up again. A row that was committed before it was read has its file, so
// every object that is really missing is still missing at the end, and an upload made during the
// check does not count against the project.
func (e *Engine) verifyTenant(ctx context.Context, b Bucket, ref string, p Project) (Tenant, error) {
	if e.inv[ref] == nil {
		if err := e.syncOne(ctx, b, ref); err != nil {
			return Tenant{}, err
		}
	}
	var ok Counts
	var pending []Row
	offline := !p.Online
	if p.Online {
		var err error
		ok, pending, err = e.checkRows(ctx, ref, e.inv[ref])
		switch {
		case errors.Is(err, ErrOffline):
			offline, ok, pending = true, Counts{}, nil
		case err != nil:
			return Tenant{}, fmt.Errorf("%s: read storage.objects: %w", ref, err)
		}
	}
	if err := e.syncOne(ctx, b, ref); err != nil {
		return Tenant{}, err
	}
	inv := e.inv[ref]
	verr := &VerifyError{Ref: ref}
	for _, r := range pending {
		f, found := inv.find(rowPath(r))
		switch {
		case !found:
			verr.Missing = append(verr.Missing, quote(rowPath(r)))
		case r.HasSize && r.Size != f.Size:
			verr.MismatchedSize = append(verr.MismatchedSize, quote(rowPath(r)))
		default:
			ok.Files++
			ok.Bytes += f.Size
		}
	}
	missing, extra, err := e.compareBucket(ctx, b, ref, inv)
	if err != nil {
		return Tenant{}, err
	}
	verr.NotInBucket = missing
	if len(verr.Missing)+len(verr.MismatchedSize)+len(verr.NotInBucket) > 0 {
		verr.Rows = ok.Files + len(verr.Missing) + len(verr.MismatchedSize)
		return Tenant{}, verr
	}
	return Tenant{Ref: ref, Rows: ok.Files, Bytes: ok.Bytes, Files: len(inv), Orphans: max(0, len(inv)-ok.Files), Extra: extra, Offline: offline}, nil
}

// compareBucket lists the project's keys in the bucket and compares them with inv: missing are
// files the bucket lacks or holds at another size, extra counts the keys without a file. Those are
// objects deleted while an earlier part of the run was stopped, or objects of another origin; they
// are not touched.
func (e *Engine) compareBucket(ctx context.Context, b Bucket, ref string, inv inventory) (missing []string, extra int, err error) {
	listed, err := e.listTenant(ctx, b, ref)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", ref, err)
	}
	prefix := ref + "/"
	i, j := 0, 0
	for i < len(listed) || j < len(inv) {
		var key string
		if i < len(listed) {
			key = listed[i].Key[len(prefix):]
		}
		switch {
		case j == len(inv) || (i < len(listed) && key < inv[j].Path):
			extra++
			i++
		case i == len(listed) || inv[j].Path < key:
			missing = append(missing, quote(prefix+inv[j].Path))
			j++
		default:
			if listed[i].Size != inv[j].Size {
				missing = append(missing, quote(prefix+inv[j].Path))
			}
			i++
			j++
		}
	}
	return missing, extra, nil
}

// errStop ends a Rows scan early.
var errStop = errors.New("stop")

// samples reads up to n rows from the databases, from different projects when it can, to read back
// through Storage once it runs on the new backend.
func (e *Engine) samples(ctx context.Context, refs []string, n int) map[string]Row {
	out := map[string]Row{}
	projects, err := e.d.Tenants.Projects(ctx)
	if err != nil {
		return out
	}
	online := map[string]bool{}
	for _, p := range projects {
		online[p.Ref] = p.Online
	}
	for _, ref := range refs {
		if len(out) >= n {
			break
		}
		if !online[ref] {
			continue
		}
		_ = e.d.Tenants.Rows(ctx, ref, func(r Row) error {
			if r.HasSize {
				out[ref] = r
				return errStop
			}
			return nil
		})
	}
	return out
}
