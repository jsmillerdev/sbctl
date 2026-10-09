package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// FunctionRecord is the deployment record of one Edge Function, as the Management API
// stores it in the registry.
type FunctionRecord struct {
	Slug           string    `json:"slug"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Version        int       `json:"version"`
	Status         string    `json:"status"`
	VerifyJWT      bool      `json:"verify_jwt"`
	EntrypointPath string    `json:"entrypoint_path,omitempty"`
	ImportMapPath  string    `json:"import_map_path,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// FunctionFile is one stored file of a deployment: a source file, or the bundle the node
// made from the sources.
type FunctionFile struct {
	Path    string
	Content []byte
}

// FunctionSecret is a function secret as the registry stores it, sealed with the node's
// master key (the same protection the sealed secrets of a base backup have).
type FunctionSecret struct {
	Name   string
	Sealed []byte
}

// Functions is the registry's store of Edge Function deployments and secrets. Backups read
// it and restores write it. The Management API owns the implementation and imports this
// package, so internal/functions adapts the API's store to it.
type Functions interface {
	ListFunctions(ctx context.Context, ref string) ([]FunctionRecord, error)
	// FunctionFiles returns ErrNoFunction for a slug that is gone.
	FunctionFiles(ctx context.Context, ref, slug string) ([]FunctionFile, error)
	ListFunctionSecrets(ctx context.Context, ref string) ([]FunctionSecret, error)

	// RestoreFunction stores f exactly as given (identifier, version and timestamps
	// included), replacing the deployment of that slug and its files.
	RestoreFunction(ctx context.Context, ref string, f FunctionRecord, files []FunctionFile) error
	DeleteFunction(ctx context.Context, ref, slug string) error
	RestoreFunctionSecrets(ctx context.Context, ref string, sealed map[string][]byte) error
	DeleteFunctionSecrets(ctx context.Context, ref string, names []string) error
}

// ErrNoFunction is what Functions.FunctionFiles returns for a function that was deleted
// after it was listed.
var ErrNoFunction = errors.New("backup: function not found")

// Entry paths of a functions snapshot: "f/<slug>/<file path>" for a deployment's files and
// "s/<NAME>" for a secret.
const (
	fnFilePrefix   = "f/"
	fnSecretPrefix = "s/"
)

// backupFunctions takes a snapshot of ref's function deployments and secrets. It reports
// nil, nil when the project has none and there is no earlier snapshot to supersede.
func (s *Service) backupFunctions(ctx context.Context, ref, reason string) (*FilesSnapshot, error) {
	if s.opt.Functions == nil {
		return nil, nil
	}
	recs, err := s.opt.Functions.ListFunctions(ctx, ref)
	if err != nil {
		return nil, err
	}
	secs, err := s.opt.Functions.ListFunctionSecrets(ctx, ref)
	if err != nil {
		return nil, err
	}
	prev, err := s.latestFilesSnapshot(ctx, ref, KindFunctions)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 && len(secs) == 0 && prev == nil {
		return nil, nil
	}

	// A deployment whose version and update time are the ones in the previous snapshot has
	// the same files (every store write bumps both), so its entries are carried over
	// without loading the files.
	prevBySlug := map[string][]fileEntry{}
	prevRec := map[string]FunctionRecord{}
	if prev != nil {
		for _, r := range prev.Functions {
			prevRec[r.Slug] = r
		}
		if err := s.readEntries(ctx, prev.Dir()+"/"+entriesName, func(e fileEntry) error {
			if slug, ok := fnSlug(e.Path); ok {
				prevBySlug[slug] = append(prevBySlug[slug], e)
			}
			return nil
		}); err != nil {
			s.opt.Log.Warn("previous function snapshot unreadable; copying everything", "ref", ref, "snapshot", prev.ID, "err", err)
			prevBySlug, prevRec = map[string][]fileEntry{}, map[string]FunctionRecord{}
		}
	}

	started := s.opt.Now().UTC()
	id := newFilesID(started)
	unmark, err := s.markRunning(ctx, ref, KindFunctions, id)
	if err != nil {
		return nil, err
	}
	defer unmark()
	bw, err := s.newBlobWriter(ref, KindFunctions)
	if err != nil {
		return nil, err
	}
	defer bw.close()

	snap := &FilesSnapshot{Version: filesVersion, Kind: KindFunctions, ID: id, Ref: ref, Reason: reason,
		StartTime: started, SupaviseVersion: s.opt.Version}
	var files int
	var bytesTotal int64
	_, err = s.writeEntries(ctx, snap.Dir()+"/"+entriesName, func(emit func(fileEntry) error) error {
		put := func(path string, content []byte) error {
			sum := sha256.Sum256(content)
			e := fileEntry{Path: path, Size: int64(len(content)), Hash: hex.EncodeToString(sum[:])}
			if err := bw.putBytes(ctx, e.Hash, content); err != nil {
				return err
			}
			files++
			bytesTotal += e.Size
			return emit(e)
		}
		for _, r := range recs {
			if old, ok := prevRec[r.Slug]; ok && old.Version == r.Version && old.UpdatedAt.Equal(r.UpdatedAt) {
				for _, e := range prevBySlug[r.Slug] {
					files++
					bytesTotal += e.Size
					if err := emit(e); err != nil {
						return err
					}
				}
				snap.Functions = append(snap.Functions, r)
				continue
			}
			fl, err := s.opt.Functions.FunctionFiles(ctx, ref, r.Slug)
			if errors.Is(err, ErrNoFunction) {
				continue // deleted since it was listed
			}
			if err != nil {
				return fmt.Errorf("function %s: %w", r.Slug, err)
			}
			for _, f := range fl {
				if err := put(fnFilePrefix+r.Slug+"/"+f.Path, f.Content); err != nil {
					return fmt.Errorf("function %s: %w", r.Slug, err)
				}
			}
			snap.Functions = append(snap.Functions, r)
		}
		for _, sec := range secs {
			if err := put(fnSecretPrefix+sec.Name, sec.Sealed); err != nil {
				return fmt.Errorf("secret %s: %w", sec.Name, err)
			}
		}
		return nil
	})
	if err != nil {
		s.dropSnapshot(ctx, snap)
		return nil, fmt.Errorf("backup: functions of %s: %w", ref, err)
	}
	snap.Files, snap.Bytes = files, bytesTotal
	snap.NewFiles, snap.NewBytes = int(bw.newFiles.Load()), bw.newBytes.Load()
	snap.StopTime = s.opt.Now().UTC()
	if err := s.writeSnapshot(ctx, snap); err != nil {
		s.dropSnapshot(ctx, snap)
		return nil, err
	}
	return snap, nil
}

func fnSlug(entryPath string) (string, bool) {
	rest, ok := strings.CutPrefix(entryPath, fnFilePrefix)
	if !ok {
		return "", false
	}
	slug, _, ok := strings.Cut(rest, "/")
	return slug, ok && slug != ""
}

// restoreFunctions writes snap's deployments and secrets into ref. With replace, the
// deployments and secrets that the snapshot does not hold are deleted, so ref ends up
// as the snapshot says.
func (s *Service) restoreFunctions(ctx context.Context, snap *FilesSnapshot, ref string, replace bool) error {
	if s.opt.Functions == nil {
		return errors.New("backup: no function store is configured for this operation")
	}
	// Function ids are unique across projects. A copy into another project (restore --as, or
	// --into) keeps everything of the deployment except its id, which would otherwise
	// collide with the source's for a client that keys on it.
	fresh := snap.Ref != ref
	recs := map[string]FunctionRecord{}
	for _, r := range snap.Functions {
		if fresh {
			r.ID = secrets.NewUUID()
		}
		recs[r.Slug] = r
	}
	sealed := map[string][]byte{}
	// The entries of one deployment are contiguous, so one deployment's files are in
	// memory at a time.
	var curSlug string
	var curFiles []FunctionFile
	done := map[string]bool{}
	flush := func() error {
		if curSlug == "" {
			return nil
		}
		slug, files := curSlug, curFiles
		curSlug, curFiles = "", nil
		done[slug] = true
		if err := s.opt.Functions.RestoreFunction(ctx, ref, recs[slug], files); err != nil {
			return fmt.Errorf("restore function %s: %w", slug, err)
		}
		return nil
	}
	err := s.readEntries(ctx, snap.Dir()+"/"+entriesName, func(e fileEntry) error {
		if !validHash(e.Hash) {
			return fmt.Errorf("snapshot %s has a bad hash for %s", snap.ID, e.Path)
		}
		if name, ok := strings.CutPrefix(e.Path, fnSecretPrefix); ok {
			b, err := s.readBlobBytes(ctx, snap, e)
			if err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			sealed[name] = b
			return nil
		}
		slug, ok := fnSlug(e.Path)
		if !ok {
			return fmt.Errorf("snapshot %s has the unknown entry %q", snap.ID, e.Path)
		}
		if _, ok := recs[slug]; !ok {
			return fmt.Errorf("snapshot %s has files of function %s but no record of it", snap.ID, slug)
		}
		if slug != curSlug {
			if done[slug] {
				return fmt.Errorf("snapshot %s lists the files of function %s in two places", snap.ID, slug)
			}
			if err := flush(); err != nil {
				return err
			}
			curSlug = slug
		}
		b, err := s.readBlobBytes(ctx, snap, e)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		curFiles = append(curFiles, FunctionFile{Path: strings.TrimPrefix(e.Path, fnFilePrefix+slug+"/"), Content: b})
		return nil
	})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, r := range snap.Functions {
		keep[r.Slug] = true
		if !done[r.Slug] { // a deployment without files
			if err := s.opt.Functions.RestoreFunction(ctx, ref, recs[r.Slug], nil); err != nil {
				return fmt.Errorf("restore function %s: %w", r.Slug, err)
			}
		}
	}
	if len(sealed) > 0 {
		if err := s.opt.Functions.RestoreFunctionSecrets(ctx, ref, sealed); err != nil {
			return fmt.Errorf("restore function secrets: %w", err)
		}
	}
	if !replace {
		return nil
	}
	cur, err := s.opt.Functions.ListFunctions(ctx, ref)
	if err != nil {
		return err
	}
	for _, r := range cur {
		if !keep[r.Slug] {
			if err := s.opt.Functions.DeleteFunction(ctx, ref, r.Slug); err != nil && !errors.Is(err, ErrNoFunction) {
				return fmt.Errorf("remove function %s: %w", r.Slug, err)
			}
		}
	}
	curSec, err := s.opt.Functions.ListFunctionSecrets(ctx, ref)
	if err != nil {
		return err
	}
	var stale []string
	for _, sec := range curSec {
		if _, ok := sealed[sec.Name]; !ok {
			stale = append(stale, sec.Name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		return s.opt.Functions.DeleteFunctionSecrets(ctx, ref, stale)
	}
	return nil
}

// readBlobBytes loads one blob into memory (function files are capped at tens of
// megabytes by the API) and checks it against e.
func (s *Service) readBlobBytes(ctx context.Context, snap *FilesSnapshot, e fileEntry) ([]byte, error) {
	rc, err := s.openBlob(ctx, snap.Ref, snap.Kind, e.Hash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(&buf, h), ctxReader{ctx, io.LimitReader(rc, maxFunctionFile+1)}); err != nil {
		return nil, err
	}
	if buf.Len() > maxFunctionFile {
		return nil, fmt.Errorf("stored file is larger than %d MiB", maxFunctionFile>>20)
	}
	if hex.EncodeToString(h.Sum(nil)) != e.Hash {
		return nil, errors.New("the stored copy does not match its checksum: the backup is damaged")
	}
	return buf.Bytes(), nil
}

// maxFunctionFile bounds one function file read back into memory. The API refuses bundles
// of more than 64 MiB and hosted functions are limited to about 20 MB.
const maxFunctionFile = 128 << 20
