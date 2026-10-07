// Package notice holds what the operator tells the dashboard's users: a maintenance window
// announced with `supavise maintenance announce`, and the marker an upgrade writes while it
// runs. `supavise status` and /healthz/detail show them to the operator. They are not sent to
// Studio's /api/incident-banner yet (see BannerJSON).
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

// staleUpgrade is how long an upgrade marker is believed after its last sign of life (started_at,
// or the file's modification time, whichever is later). A marker that outlives it was left by a
// process that died. While it is believed the alerts stay quiet about restarted services, so it
// is kept short: a crashed upgrade is when projects are most likely down. The upgrade rewrites
// the marker at each phase change, which keeps it alive.
const staleUpgrade = 2 * time.Hour

// MaxWindow is the longest maintenance window an announcement grants without Extended. The
// window quiets alerts for its whole length, so a typo such as --duration 720h must not silence
// them for a month.
const MaxWindow = 24 * time.Hour

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
	// Extended is the operator's explicit permission (--allow-long) for a window longer than MaxWindow.
	Extended bool `json:"extended,omitempty"`
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
	case !m.Extended && m.EndsAt.Sub(m.StartsAt) > MaxWindow:
		return fmt.Errorf("the maintenance window is longer than %s, and alerts stay quiet for all of it; pass --allow-long if you mean it", MaxWindow)
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

// Quiet reports whether alerts about restarted services are held back at now: the window is
// open and, unless the operator allowed a long one, no more than MaxWindow old. The second
// condition guards a file edited by hand; Validate already refuses such an announcement.
func (m Maintenance) Quiet(now time.Time) bool {
	return m.InProgress(now) && (m.Extended || now.Sub(m.StartsAt) < MaxWindow)
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
// in, the versions it moves between and when it began. The upgrade leaves a terminal Phase in the
// file when it ends ("done", "rolled_back", "failed"), so that `supavise status` can say how the
// last one went.
type Upgrade struct {
	Phase     string    `json:"phase"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"started_at"`
	// PID is the process that runs the upgrade; another `supavise upgrade` refuses to start while
	// it is alive. Zero when unknown.
	PID int `json:"pid,omitempty"`
	// Detail says what the phase is doing now ("Realtime"), for `supavise status`.
	Detail string `json:"detail,omitempty"`
	// Heartbeat is the file's modification time, set by ReadUpgrade and not stored in the file.
	// A marker without started_at is dated by it.
	Heartbeat time.Time `json:"-"`
}

// terminal are the phases that mean the upgrade is over, whichever way it ended.
var terminal = map[string]bool{
	"": true, "done": true, "complete": true, "completed": true, "succeeded": true, "success": true,
	"failed": true, "rolled_back": true, "rolled-back": true, "rollback": true, "aborted": true, "refused": true, "idle": true,
}

// Running reports whether the marker describes an upgrade that is still going at now. It fails
// closed: a marker with neither started_at nor a modification time is not believed, and neither
// is one whose last sign of life is older than staleUpgrade.
func (u Upgrade) Running(now time.Time) bool {
	if terminal[strings.ToLower(strings.TrimSpace(u.Phase))] {
		return false
	}
	alive := u.StartedAt
	if u.Heartbeat.After(alive) {
		alive = u.Heartbeat
	}
	return !alive.IsZero() && now.Sub(alive) < staleUpgrade
}

// WriteUpgrade replaces the upgrade marker. The upgrade calls it at each phase change, which also
// keeps the marker alive (Running counts the file's modification time).
func WriteUpgrade(p config.Paths, u Upgrade) error {
	return writeJSON(file(p, upgradeFile), u)
}

// ClearUpgrade removes the upgrade marker; it reports whether there was one.
func ClearUpgrade(p config.Paths) (bool, error) {
	err := os.Remove(file(p, upgradeFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// ReadUpgrade returns the upgrade marker, or nil when there is none. It is tolerant: the file
// belongs to the upgrade, and an unreadable one is "no upgrade running", not an error.
func ReadUpgrade(p config.Paths) *Upgrade {
	var u Upgrade
	if ok, err := readJSON(file(p, upgradeFile), &u); err != nil || !ok {
		return nil
	}
	if fi, err := os.Stat(file(p, upgradeFile)); err == nil {
		u.Heartbeat = fi.ModTime()
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
