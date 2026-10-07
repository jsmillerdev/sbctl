package notice

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// Incident is one entry of the list Studio's /api/incident-banner returns. The first three
// fields are the shape Studio reads (apps/studio/data/platform/incident-banner-query.ts at the
// pinned tag); the rest are ours, for anything that reads the answer besides Studio.
type Incident struct {
	// ID is what Studio stores when a user dismisses the banner, so a new announcement or a new
	// upgrade needs a new ID.
	ID string `json:"id"`
	// ShowBanner is "force": show it whatever the user's projects and regions.
	ShowBanner string   `json:"show_banner"`
	Metadata   Metadata `json:"metadata"`

	Kind     string     `json:"kind"` // "maintenance" or "upgrade"
	Title    string     `json:"title"`
	Message  string     `json:"message"`
	StartsAt *time.Time `json:"starts_at,omitempty"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
}

// Metadata is Studio's IncidentCache: force shows the banner to everyone, including users
// with no project, and no region restricts it.
type Metadata struct {
	AffectedRegions        []string `json:"affected_regions"`
	AffectsProjectCreation bool     `json:"affects_project_creation"`
	Force                  bool     `json:"force"`
}

func forced() Metadata { return Metadata{Force: true} }

// Banner returns the notices to show at now: the announced maintenance while its banner is
// active, and an upgrade that is running. It reads two small files and never fails: a notice
// that cannot be read is not shown. An update that is merely available is never here, because
// the dashboard's users cannot act on it; the operator is told through `supavise status` and
// the alerts.
func Banner(p config.Paths, now time.Time) []Incident {
	out := []Incident{}
	if u, ok := UpgradeRunning(p, now); ok {
		msg := "The node is being upgraded. Projects restart one at a time and are back in minutes."
		if u.To != "" {
			msg = fmt.Sprintf("The node is being upgraded to %s. Projects restart one at a time and are back in minutes.", u.To)
		}
		start := u.StartedAt
		inc := Incident{
			ID: fmt.Sprintf("upgrade-%s-%d", u.To, u.StartedAt.Unix()), ShowBanner: "force", Metadata: forced(),
			Kind: "upgrade", Title: "Upgrade in progress", Message: msg,
		}
		if !start.IsZero() {
			inc.StartsAt = &start
		}
		out = append(out, inc)
	}
	if m, err := ReadMaintenance(p); err == nil && m != nil && m.Active(now) {
		start, end := m.StartsAt, m.EndsAt
		title := "Scheduled maintenance"
		if m.InProgress(now) {
			title = "Maintenance in progress"
		}
		out = append(out, Incident{
			ID: "maintenance-" + m.ID, ShowBanner: "force", Metadata: forced(),
			Kind: "maintenance", Title: title, Message: m.Message, StartsAt: &start, EndsAt: &end,
		})
	}
	return out
}

// BannerJSON renders the answer of /api/incident-banner for the notices active at now.
func BannerJSON(p config.Paths, now time.Time) []byte {
	b, err := json.Marshal(struct {
		Incidents []Incident `json:"incidents"`
	}{Banner(p, now)})
	if err != nil { // unreachable: plain structs
		return []byte(`{"incidents":[]}`)
	}
	return append(b, '\n')
}
