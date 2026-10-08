package nodeupgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Record is what the node keeps about a release it ran: where its binary is and what it pinned.
// `supavise rollback` goes back to the newest record before the current one.
type Record struct {
	Version  string `json:"version"`
	Platform string `json:"platform,omitempty"`
	// Pins are the service releases the binary pinned (Info.Pins), or, for a binary too old to
	// report them, the releases the node's units ran while it was installed.
	Pins map[string]string `json:"pins"`
	// Migrations are the registry migrations the release knows: the ones its binary embeds, or,
	// for a binary too old to say, the ones applied while it ran. Migrations only go forward, so a
	// registry that holds one this list lacks cannot be served by this release.
	Migrations []string `json:"registry_migrations"`
	// MigrationsFrom says where Migrations came from: "binary" or "applied".
	MigrationsFrom string `json:"migrations_from,omitempty"`
	// SHA256 is the checksum of the kept binary.
	SHA256 string `json:"sha256"`
	// InstalledAt is when the release became the one the node runs; zero for a release that was
	// running before the first upgrade kept it.
	InstalledAt time.Time `json:"installed_at"`
	// UpgradeEndedAt is when the upgrade that installed the release finished. The projects that
	// upgrade moved are the ones whose upgrade started between InstalledAt and this time; a project
	// that an Owner upgraded later is not put back by a rollback. Zero while the upgrade has not
	// ended, and in records written before it was kept.
	UpgradeEndedAt time.Time `json:"upgrade_ended_at,omitzero"`
	// Withdrawn marks a release the node was rolled back from: `supavise rollback` does not go
	// back to it, so that a second rollback keeps stepping backwards. Installing the release
	// again writes a fresh record.
	Withdrawn bool `json:"withdrawn,omitempty"`
}

// Sources of Record.Migrations.
const (
	MigrationsFromBinary  = "binary"
	MigrationsFromApplied = "applied"
)

// Releases is the directory of kept releases, <dir>/<version>/{supavise,release.json}. It lives
// next to the installed binary and belongs to root: the binary a rollback installs is run as root
// (the installer swaps it in) and as the supavise user, so a copy the supavise user could replace
// would be a way to root.
type Releases struct {
	Dir string
}

// BinaryName is the file name of a kept binary.
const BinaryName = "supavise"

const recordName = "release.json"

var safeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func (r Releases) dir(version string) (string, error) {
	if !safeVersion.MatchString(version) || version == "." || version == ".." {
		return "", fmt.Errorf("releases: %q is not a version that can name a directory", version)
	}
	return filepath.Join(r.Dir, version), nil
}

// BinaryPath is where the kept binary of version is (or would be).
func (r Releases) BinaryPath(version string) (string, error) {
	d, err := r.dir(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, BinaryName), nil
}

// Keep stores a copy of the binary at src as rec.Version and writes its record. A copy of the
// same version replaces the earlier one (a development build is named "dev" each time). rec.SHA256
// is filled in from the file.
func (r Releases) Keep(rec Record, src string) (Record, error) {
	d, err := r.dir(rec.Version)
	if err != nil {
		return rec, err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return rec, err
	}
	in, err := os.Open(src)
	if err != nil {
		return rec, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(d, ".binary-*")
	if err != nil {
		return rec, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		tmp.Close()
		return rec, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return rec, err
	}
	if err := tmp.Close(); err != nil {
		return rec, err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(d, BinaryName)); err != nil {
		return rec, err
	}
	rec.SHA256 = hex.EncodeToString(h.Sum(nil))
	return rec, r.write(d, rec)
}

func (r Releases) write(d string, rec Record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d, ".record-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(d, recordName))
}

// Touch marks version as installed now, which makes it the newest record: a rollback to it
// makes it the release the node runs. It clears Withdrawn: a release the node runs is not one
// it was rolled back from.
func (r Releases) Touch(version string, at time.Time) error {
	rec, err := r.Get(version)
	if err != nil {
		return err
	}
	rec.InstalledAt, rec.Withdrawn = at.UTC(), false
	d, _ := r.dir(version)
	return r.write(d, *rec)
}

// EndUpgrade records when the upgrade to version ended.
func (r Releases) EndUpgrade(version string, at time.Time) error {
	rec, err := r.Get(version)
	if err != nil {
		return err
	}
	rec.UpgradeEndedAt = at.UTC()
	d, _ := r.dir(version)
	return r.write(d, *rec)
}

// Withdraw marks version as a release the node was rolled back from.
func (r Releases) Withdraw(version string) error {
	rec, err := r.Get(version)
	if err != nil {
		return err
	}
	rec.Withdrawn = true
	d, _ := r.dir(version)
	return r.write(d, *rec)
}

// Get reads the record of version. A record whose binary is missing or does not match its
// checksum is an error: it could not be rolled back to.
func (r Releases) Get(version string) (*Record, error) {
	d, err := r.dir(version)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(d, recordName))
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("releases: %s: %w", filepath.Join(d, recordName), err)
	}
	return &rec, nil
}

// Verify checks that the kept binary of rec is still the file that was kept.
func (r Releases) Verify(rec *Record) error {
	p, err := r.BinaryPath(rec.Version)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("the kept binary of %s is gone: %w", rec.Version, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != rec.SHA256 {
		return fmt.Errorf("the kept binary of %s does not match its record (sha256 %s, recorded %s): refusing to install it", rec.Version, got, rec.SHA256)
	}
	return nil
}

// List returns the records, newest installed first. A directory without a readable record is
// skipped.
func (r Releases) List() ([]Record, error) {
	ents, err := os.ReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if rec, err := r.Get(e.Name()); err == nil {
			out = append(out, *rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].InstalledAt.After(out[j].InstalledAt) })
	return out, nil
}

// Previous returns the newest record that is not current and was not rolled back from: the
// release a rollback goes back to. It is nil when there is none.
func (r Releases) Previous(current string) (*Record, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Version != current && !all[i].Withdrawn {
			return &all[i], nil
		}
	}
	return nil, nil
}

// GC removes the releases a rollback will not go to and all but the keep newest of the others (the
// current release counts), and never removes current. A release the node was rolled back from
// (Withdrawn) is no rollback target, so it takes no place from the ones that are. It returns the
// versions it removed.
func (r Releases) GC(keep int, current string) ([]string, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}
	keep = max(keep, 1)
	var removed []string
	kept := 0
	for _, rec := range all {
		if rec.Version == current || (!rec.Withdrawn && kept < keep) {
			if !rec.Withdrawn {
				kept++
			}
			continue
		}
		d, err := r.dir(rec.Version)
		if err != nil {
			return removed, err
		}
		if err := os.RemoveAll(d); err != nil {
			return removed, err
		}
		removed = append(removed, rec.Version)
	}
	return removed, nil
}
