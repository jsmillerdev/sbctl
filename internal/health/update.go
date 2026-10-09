package health

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/selfupdate"
)

// UpdateRecord is what the daily update check found, kept in <state_dir>/system/update.json.
// The check only looks; it installs nothing. `supavise status` and the alerts read the record,
// and the dashboard never shows it: its users cannot act on a release.
type UpdateRecord struct {
	CheckedAt time.Time `json:"checked_at"`
	// Installed is the running version when the check ran; Latest the newest published release.
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	// Available: Latest is a later release than Installed. Never set for a development build.
	Available bool `json:"available"`
	// NotifiedVersion is the release the operator was last told about, so that each release
	// raises update_available once.
	NotifiedVersion string `json:"notified_version,omitempty"`
	// Error is why the last check failed; the rest of the record is then the last good one.
	Error string `json:"error,omitempty"`
}

func updateFile(p config.Paths) string { return filepath.Join(p.Root, "system", "update.json") }

// ReadUpdate returns the record, or nil when no check has run.
func ReadUpdate(p config.Paths) (*UpdateRecord, error) {
	b, err := os.ReadFile(updateFile(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r UpdateRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", updateFile(p), err)
	}
	return &r, nil
}

// WriteUpdate replaces the record atomically.
func WriteUpdate(p config.Paths, r *UpdateRecord) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := updateFile(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".update.")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

var releaseVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+`)

// IsRelease reports whether v names a release ("v1.2.3", also "v1.2.3-4-gabcdef"). A
// development build ("dev") is not one, and is never told that an update is available.
func IsRelease(v string) bool { return releaseVersion.MatchString(strings.TrimSpace(v)) }

// UpdateSettings are the parts of the [update] section the check reads. The section belongs to
// the release tooling (config.toml: mode, window, channel, check_interval, ...); this reads it
// on its own with defaults, so the check works whether or not that section exists, and ignores
// what it does not need. The interval's grammar is config.ParseCheckInterval, the one the
// config validator uses.
type UpdateSettings struct {
	// CheckInterval is the pause between checks: 24 hours unless [update] check_interval says
	// otherwise, and never under an hour (an unauthenticated client may ask GitHub 60 times an hour).
	CheckInterval time.Duration
	// Off is set by check_interval = "off" (or "never", or 0): a node with no route to GitHub
	// asks nobody, and `supavise status` says no check has run.
	Off bool
}

// EnvUpdateInterval overrides [update] check_interval, like every config key.
const EnvUpdateInterval = config.EnvPrefix + "UPDATE_CHECK_INTERVAL"

// ReadUpdateSettings reads [update] from the config file at path (empty: none) and the
// environment. A missing file, a missing section and a value it cannot read all give the
// default; the check never fails the daemon for a setting (the config validator is what
// reports a bad value). "off" turns the check off.
func ReadUpdateSettings(path string) UpdateSettings {
	s := UpdateSettings{CheckInterval: config.DefaultCheckInterval}
	var raw string
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var doc struct {
				Update struct {
					CheckInterval any `toml:"check_interval"`
				} `toml:"update"`
			}
			if toml.Unmarshal(b, &doc) == nil {
				switch v := doc.Update.CheckInterval.(type) {
				case string:
					raw = v
				case int64: // a bare number is seconds, and 0 is off
					raw = strconv.FormatInt(v, 10)
				}
			}
		}
	}
	if v := os.Getenv(EnvUpdateInterval); v != "" {
		raw = v
	}
	d, on, err := config.ParseCheckInterval(raw)
	switch {
	case err != nil:
	case !on:
		s.Off = true
	default:
		s.CheckInterval = d
	}
	return s
}

// UpdateSource says where releases are published; the zero value is the project's GitHub
// repository. Tests point APIBase at a fake.
type UpdateSource struct {
	Repo    string
	APIBase string
}

// CheckUpdate asks for the newest release and records what it found, keeping NotifiedVersion
// from the earlier record. A failed check records its error next to the last good answer and
// returns it.
func CheckUpdate(ctx context.Context, cfg *config.Config, installed string, src UpdateSource, now time.Time) (*UpdateRecord, error) {
	paths := cfg.Paths()
	prev, _ := ReadUpdate(paths)
	rec := UpdateRecord{CheckedAt: now.UTC(), Installed: installed}
	if prev != nil {
		rec = *prev
		rec.CheckedAt, rec.Installed, rec.Error = now.UTC(), installed, ""
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rel, err := selfupdate.Latest(cctx, selfupdate.Options{Repo: src.Repo, APIBase: src.APIBase, Current: installed})
	if err != nil {
		rec.Error = err.Error()
		if werr := WriteUpdate(paths, &rec); werr != nil {
			return &rec, errors.Join(err, werr)
		}
		return &rec, err
	}
	rec.Latest = rel.Tag
	rec.Available = IsRelease(installed) && selfupdate.Newer(rel.Tag, installed)
	if err := WriteUpdate(paths, &rec); err != nil {
		return &rec, err
	}
	return &rec, nil
}

// MarkNotified records that the operator was told about version.
func MarkNotified(cfg *config.Config, version string) error {
	paths := cfg.Paths()
	rec, err := ReadUpdate(paths)
	if err != nil || rec == nil {
		return err
	}
	rec.NotifiedVersion = version
	return WriteUpdate(paths, rec)
}

func (d *Deps) checkUpdate() Component {
	c := Component{Name: "update", State: OK}
	rec, err := ReadUpdate(d.Cfg.Paths())
	switch {
	case err != nil:
		c.State, c.Detail = Info, "cannot read the update record: "+shorten(err.Error())
	case rec == nil:
		c.Detail = "no check has run yet"
	case rec.Available:
		c.State = Info
		c.Detail = fmt.Sprintf("%s is available (running %s); see `supavise upgrade --check`", rec.Latest, cmp.Or(rec.Installed, d.Version))
	case rec.Error != "":
		c.State, c.Detail = Info, "the last check failed: "+shorten(rec.Error)
	default:
		c.Detail = fmt.Sprintf("%s is the latest release (checked %s ago)", cmp.Or(rec.Latest, "this version"), humanAge(d.now().Sub(rec.CheckedAt)))
	}
	return c
}
