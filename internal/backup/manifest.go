package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

const (
	manifestVersion = 1
	manifestName    = "backup.json"
	dataName        = "data.tar.zst"
	secretsName     = "secrets.json"
	idLayout        = "20060102T150405Z"
)

// Backup reasons recorded in the manifest.
const (
	ReasonManual    = "manual"
	ReasonScheduled = "scheduled"
	ReasonFinal     = "final"        // taken by project delete
	ReasonRestore   = "post-restore" // taken right after an in-place restore, so the new timeline has a base
)

// Manifest describes one complete base backup. It lives next to the data and is the
// source of truth for restore and prune; registry rows are an index over it.
type Manifest struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Ref     string `json:"ref"`
	Reason  string `json:"reason"`

	Timeline int    `json:"timeline"`
	StartLSN string `json:"start_lsn"`
	StopLSN  string `json:"stop_lsn"`
	// StartWAL is the first WAL file recovery needs; StopWAL the one holding the end-of-backup record.
	StartWAL string `json:"start_wal"`
	StopWAL  string `json:"stop_wal"`
	// StartTime is when the backup began; StopTime is when pg_backup_stop returned. A
	// point-in-time restore target must be at or after StopTime.
	StartTime time.Time `json:"start_time"`
	StopTime  time.Time `json:"stop_time"`

	PGVersionNum   int   `json:"pg_version_num"`
	WALSegmentSize int64 `json:"wal_segment_size"`

	Data        string `json:"data"`    // object name of the tar.zst, relative to the backup dir
	Secrets     string `json:"secrets"` // object name of the sealed secrets, empty if none
	SizeBytes   int64  `json:"size_bytes"`
	StoredBytes int64  `json:"stored_bytes"`
	Files       int    `json:"files"`

	Project         *ManifestProject `json:"project,omitempty"`
	SupaviseVersion string           `json:"supavise_version,omitempty"`
}

// ManifestProject is the project metadata a restore needs when the registry row is gone.
type ManifestProject struct {
	Name     string            `json:"name"`
	OrgSlug  string            `json:"org_slug,omitempty"`
	Region   string            `json:"region,omitempty"`
	Class    string            `json:"class,omitempty"`
	Engine   string            `json:"engine,omitempty"`
	Versions map[string]string `json:"versions,omitempty"`
	Limits   config.Limits     `json:"limits"`
}

// Dir is the backup's key prefix, without trailing slash.
func (m *Manifest) Dir() string { return baseDir(m.Ref) + m.ID }

func backupID(t time.Time) string { return t.UTC().Format(idLayout) }

// newBackupID is backupID plus a random suffix. Two runs that start in the same second
// (the timer racing a manual backup or a final backup) must never share a directory:
// the loser's cleanup would delete the winner's objects.
func newBackupID(t time.Time) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return backupID(t) + "-" + hex.EncodeToString(b[:])
}

// ListBackups returns ref's complete base backups from the store, oldest first.
// Directories without a manifest (uploads in flight or abandoned) are not listed.
func (s *Service) ListBackups(ctx context.Context, ref string) ([]Manifest, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	ids, err := s.opt.Store.ListDirs(ctx, baseDir(ref))
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, id := range ids {
		m, err := s.readManifest(ctx, ref, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("backup: manifest of %s/%s: %w", ref, id, err)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime.Before(out[j].StartTime) })
	return out, nil
}

func (s *Service) readManifest(ctx context.Context, ref, id string) (*Manifest, error) {
	rc, err := s.opt.Store.Get(ctx, baseDir(ref)+id+"/"+manifestName)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if m.Ref != ref || m.ID != id {
		return nil, fmt.Errorf("manifest says %s/%s but is stored at %s/%s", m.Ref, m.ID, ref, id)
	}
	return &m, nil
}

func (s *Service) writeManifest(ctx context.Context, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return s.opt.Store.Put(ctx, m.Dir()+"/"+manifestName, strings.NewReader(string(b)+"\n"))
}

// pickBackup returns the newest backup whose StopTime is at or before target, or the
// one named by id.
func pickBackup(all []Manifest, target time.Time, id string) (*Manifest, error) {
	if len(all) == 0 {
		return nil, errors.New("backup: no base backups exist for this project")
	}
	if id != "" {
		for i := range all {
			if all[i].ID == id {
				if all[i].StopTime.After(target) {
					return nil, fmt.Errorf("backup: backup %s finished at %s, after the restore target %s",
						id, all[i].StopTime.UTC().Format(time.RFC3339), target.UTC().Format(time.RFC3339))
				}
				return &all[i], nil
			}
		}
		return nil, fmt.Errorf("backup: no base backup %q", id)
	}
	var best *Manifest
	for i := range all {
		if !all[i].StopTime.After(target) && (best == nil || all[i].StopTime.After(best.StopTime)) {
			best = &all[i]
		}
	}
	if best == nil {
		first := all[0]
		for _, m := range all {
			if m.StopTime.Before(first.StopTime) {
				first = m
			}
		}
		return nil, fmt.Errorf("backup: the earliest base backup finished at %s, after the restore target %s",
			first.StopTime.UTC().Format(time.RFC3339), target.UTC().Format(time.RFC3339))
	}
	return best, nil
}
