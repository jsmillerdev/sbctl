package notice

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/supavise/supavise/internal/config"
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

// Banner returns the notices Studio would show at now: the announced maintenance while its
// banner is active, and an upgrade that is running. BannerJSON does not serve them yet. It reads two small files and never fails: a notice
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

// IncidentsJSON renders the list Banner returns in the shape of Studio's /api/incident-banner.
// The proxy does not serve it yet: see BannerJSON.
func IncidentsJSON(p config.Paths, now time.Time) []byte {
	b, err := json.Marshal(struct {
		Incidents []Incident `json:"incidents"`
	}{Banner(p, now)})
	if err != nil { // unreachable: plain structs
		return []byte(emptyBanner)
	}
	return append(b, '\n')
}

const emptyBanner = `{"incidents":[]}`

// BannerJSON is the answer of /api/incident-banner: no incidents, always.
//
// This Studio build draws every incident with the same fixed words ("We are investigating a
// technical issue") and a link to Supabase's status page, which knows nothing about this node. A
// maintenance window the operator planned, or an upgrade it started, would read as an unexplained
// outage. Until Studio shows the operator's own title and message (a Studio patch, or the
// incident.io status page route behind the incidentIoStatusPage flag, which is how hosted shows
// maintenance), the notices reach the operator through `supavise status`, /healthz/detail and the
// alerts instead. IncidentsJSON is the answer to serve once Studio can show it.
func BannerJSON() []byte { return []byte(emptyBanner + "\n") }
