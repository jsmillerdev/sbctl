// Package notice holds what the operator tells the dashboard's users: a maintenance window
// announced with `supavise maintenance announce`, and the marker an upgrade writes while it
// runs. The proxy turns the active ones into the answer of Studio's /api/incident-banner;
// `supavise status` shows them to the operator.
//
// Both live in small JSON files under <state_dir>/system/, so the answer needs neither the
// registry nor the daemon's memory, and a file written by the CLI is seen by the next request.
package notice

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

const (
	maintenanceFile = "maintenance.json"
	upgradeFile     = "upgrade.json"
)

// staleUpgrade is how long an upgrade marker is believed. A marker that outlives it was left
// by a process that died; showing "upgrade in progress" for days would be worse than showing
// nothing.
const staleUpgrade = 12 * time.Hour

func file(p config.Paths, name string) string { return filepath.Join(p.Root, "system", name) }

// Maintenance is an announced maintenance window.
type Maintenance struct {
	// ID identifies the announcement; a new announcement has a new ID, so a dashboard user who
	// dismissed the old banner sees the new one.
	ID      string `json:"id"`
	Message string `json:"message"`
	// StartsAt and EndsAt bound the window.
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	// LeadSeconds is how long before StartsAt the banner appears. 0 shows it only while the
	// window is open.
	LeadSeconds int       `json:"lead_seconds,omitempty"`
	AnnouncedAt time.Time `json:"announced_at"`
}

// Validate checks the announcement the CLI is about to write.
func (m Maintenance) Validate() error {
	switch {
	case strings.TrimSpace(m.Message) == "":
		return errors.New("a maintenance announcement needs a message")
	case len(m.Message) > 500:
		return errors.New("the maintenance message is longer than 500 characters")
	case m.StartsAt.IsZero() || m.EndsAt.IsZero():
		return errors.New("a maintenance announcement needs a start and an end")
	case !m.EndsAt.After(m.StartsAt):
		return errors.New("the maintenance window must end after it starts")
	case m.LeadSeconds < 0:
		return errors.New("the notice lead must not be negative")
	}
	return nil
}

// Active reports whether the banner is shown at now.
func (m Maintenance) Active(now time.Time) bool {
	return !now.Before(m.StartsAt.Add(-time.Duration(m.LeadSeconds)*time.Second)) && now.Before(m.EndsAt)
}

// InProgress reports whether the window is open at now.
func (m Maintenance) InProgress(now time.Time) bool {
	return !now.Before(m.StartsAt) && now.Before(m.EndsAt)
}

// ReadMaintenance returns the announced window, or nil when there is none. A window that has
// ended is returned too: `supavise maintenance show` reports it, and Banner ignores it.
func ReadMaintenance(p config.Paths) (*Maintenance, error) {
	var m Maintenance
	if ok, err := readJSON(file(p, maintenanceFile), &m); err != nil || !ok {
		return nil, err
	}
	return &m, nil
}

// WriteMaintenance announces m, replacing an earlier announcement. It fills ID and AnnouncedAt.
func WriteMaintenance(p config.Paths, m Maintenance, now time.Time) (*Maintenance, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.AnnouncedAt = now.UTC()
	m.StartsAt, m.EndsAt = m.StartsAt.UTC(), m.EndsAt.UTC()
	m.ID = fmt.Sprintf("m%d", now.UnixNano()/int64(time.Millisecond))
	if err := writeJSON(file(p, maintenanceFile), m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ClearMaintenance removes the announcement; it reports whether there was one.
func ClearMaintenance(p config.Paths) (bool, error) {
	err := os.Remove(file(p, maintenanceFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// Upgrade is the marker the upgrade writes to <state_dir>/system/upgrade.json: the phase it is
// in, the versions it moves between and when it began. The upgrade removes the file when it
// ends, or leaves a terminal Phase in it.
type Upgrade struct {
	Phase     string    `json:"phase"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"started_at"`
}

// terminal are the phases that mean the upgrade is over, whichever way it ended.
var terminal = map[string]bool{
	"": true, "done": true, "complete": true, "completed": true, "succeeded": true, "success": true,
	"failed": true, "rolled_back": true, "rolled-back": true, "rollback": true, "aborted": true, "refused": true, "idle": true,
}

// Running reports whether the marker describes an upgrade that is still going at now.
func (u Upgrade) Running(now time.Time) bool {
	if terminal[strings.ToLower(strings.TrimSpace(u.Phase))] {
		return false
	}
	return u.StartedAt.IsZero() || now.Sub(u.StartedAt) < staleUpgrade
}

// ReadUpgrade returns the upgrade marker, or nil when there is none. It is tolerant: the file
// belongs to the upgrade, and an unreadable one is "no upgrade running", not an error.
func ReadUpgrade(p config.Paths) *Upgrade {
	var u Upgrade
	if ok, err := readJSON(file(p, upgradeFile), &u); err != nil || !ok {
		return nil
	}
	return &u
}

// UpgradeRunning reports whether an upgrade is running at now.
func UpgradeRunning(p config.Paths, now time.Time) (*Upgrade, bool) {
	u := ReadUpgrade(p)
	if u == nil || !u.Running(now) {
		return nil, false
	}
	return u, true
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// writeJSON replaces path atomically (written aside, then renamed), so a reader never sees half a file.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".notice.")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil { // not secret; the proxy and the CLI may run as different users
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
