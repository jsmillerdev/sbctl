package health

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Verdict is the one-word state of the node. `supavise status` exits 0, 1 or 2 for it.
type Verdict string

const (
	// Healthy: everything that should answer does, and nothing needs the operator.
	Healthy Verdict = "healthy"
	// Degraded: the node serves, but something is wrong: a project or a shared service is
	// down, a backup is stale, the disk or a certificate is running out.
	Degraded Verdict = "degraded"
	// Down: the node cannot serve: the daemon, the edge or the system cluster is not running.
	Down Verdict = "down"
)

// ExitCode is the exit status of `supavise status` for the verdict: 0, 1 or 2.
func (v Verdict) ExitCode() int {
	switch v {
	case Healthy:
		return 0
	case Degraded:
		return 1
	}
	return 2
}

// State is the outcome of one check.
type State string

const (
	// OK: the check passed.
	OK State = "ok"
	// Info: something the operator may want to know that does not affect the verdict (no
	// copy of the master key in the backups, a release is available, a maintenance window).
	Info State = "info"
	// Warn: not right yet, not broken (a backup is stale, the disk is getting full).
	Warn State = "warn"
	// Fail: broken.
	Fail State = "fail"
)

// rank orders states by severity.
func (s State) rank() int {
	switch s {
	case Fail:
		return 3
	case Warn:
		return 2
	case Info:
		return 1
	}
	return 0
}

// Component is one part of the node: the daemon, the edge, a service, the disk, the
// certificates.
type Component struct {
	Name  string `json:"name"`
	State State  `json:"state"`
	// Critical components take the node down when they fail; the others only degrade it.
	Critical bool   `json:"critical,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ServiceResult is one of a project's units, probed with a real request.
type ServiceResult struct {
	Name   string `json:"name"` // postgres, gotrue, postgrest
	OK     bool   `json:"ok"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// TenantResult says whether a shared service holds the project's tenant.
type TenantResult struct {
	Service string `json:"service"` // supavisor, realtime, storage
	Present bool   `json:"present"`
	Error   string `json:"error,omitempty"`
}

// BackupResult is the backup freshness of one project.
type BackupResult struct {
	// LastCompleted is when the newest completed base backup finished; nil when there is none.
	LastCompleted *time.Time `json:"last_completed,omitempty"`
	AgeSeconds    int64      `json:"age_seconds,omitempty"`
	// Stale: older than [health] backup_stale_hours, or none although the project is older
	// than that.
	Stale bool `json:"stale,omitempty"`
	// LastFailed is the error of the newest backup when it failed after the last completed one.
	LastFailed string `json:"last_failed,omitempty"`
	// Note explains a result that is neither fresh nor stale ("paused", "no backup yet").
	Note string `json:"note,omitempty"`
}

// ProjectResult is the health of one project.
type ProjectResult struct {
	Ref   string `json:"ref"`
	Name  string `json:"name,omitempty"`
	OrgID int64  `json:"-"`
	// Status is the registry's status for the project (ACTIVE_HEALTHY, INACTIVE, ...).
	Status string `json:"status"`
	State  State  `json:"state"`
	// Probed is false for a project that is not supposed to answer (paused, being created).
	Probed   bool            `json:"probed"`
	Services []ServiceResult `json:"services,omitempty"`
	Tenants  []TenantResult  `json:"tenants,omitempty"`
	Backup   *BackupResult   `json:"backup,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

// Report is the health of the node at one moment.
type Report struct {
	Verdict   Verdict   `json:"status"`
	CheckedAt time.Time `json:"checked_at"`
	// Version is the running binary's version.
	Version string `json:"version,omitempty"`
	// Summary is the one-line verdict `supavise status` prints.
	Summary    string          `json:"summary"`
	Components []Component     `json:"components"`
	Projects   []ProjectResult `json:"projects"`
	// Issues are the sentences behind Summary, worst first.
	Issues []string `json:"issues,omitempty"`
}

// Finish computes Verdict, Issues and Summary from the components and projects.
func (r *Report) Finish() {
	sort.SliceStable(r.Projects, func(i, j int) bool { return r.Projects[i].Ref < r.Projects[j].Ref })
	r.Verdict = Healthy
	type issue struct {
		rank int
		text string
	}
	var issues []issue
	for _, c := range r.Components {
		switch c.State {
		case Fail:
			if c.Critical {
				r.Verdict = Down
			} else if r.Verdict == Healthy {
				r.Verdict = Degraded
			}
			issues = append(issues, issue{rank: 4 + btoi(c.Critical), text: componentIssue(c)})
		case Warn:
			if r.Verdict == Healthy {
				r.Verdict = Degraded
			}
			issues = append(issues, issue{rank: 2, text: componentIssue(c)})
		}
	}
	var bad, stale, other []string
	for _, p := range r.Projects {
		switch {
		case p.State == Fail:
			bad = append(bad, p.Ref)
		case p.State == Warn && p.Backup != nil && (p.Backup.Stale || p.Backup.LastFailed != ""):
			stale = append(stale, p.Ref)
		case p.State == Warn:
			other = append(other, p.Ref)
		}
	}
	if len(bad) > 0 {
		issues = append(issues, issue{3, fmt.Sprintf("%s not answering: %s", plural(len(bad), "project"), refList(bad))})
	}
	if len(other) > 0 {
		issues = append(issues, issue{2, fmt.Sprintf("%s need attention: %s", plural(len(other), "project"), refList(other))})
	}
	if len(stale) > 0 {
		issues = append(issues, issue{2, fmt.Sprintf("%s without a fresh backup: %s", plural(len(stale), "project"), refList(stale))})
	}
	for _, p := range r.Projects {
		if (p.State == Fail || p.State == Warn) && r.Verdict == Healthy {
			r.Verdict = Degraded
		}
	}
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].rank > issues[j].rank })
	r.Issues = nil
	for _, i := range issues {
		r.Issues = append(r.Issues, i.text)
	}
	r.Summary = r.summary()
}

func (r *Report) summary() string {
	if r.Verdict == Healthy {
		running, paused := 0, 0
		for _, p := range r.Projects {
			if p.Probed {
				running++
			} else if p.Status == "INACTIVE" {
				paused++
			}
		}
		s := fmt.Sprintf("healthy: %s answering", plural(running, "project"))
		if paused > 0 {
			s += fmt.Sprintf(", %d paused", paused)
		}
		return s
	}
	shown := r.Issues
	more := 0
	if len(shown) > 3 {
		shown, more = shown[:3], len(shown)-3
	}
	s := string(r.Verdict) + ": " + strings.Join(shown, "; ")
	if more > 0 {
		s += fmt.Sprintf("; and %d more", more)
	}
	return s
}

func componentIssue(c Component) string {
	if c.Detail == "" {
		return c.Name + " is " + map[State]string{Fail: "down", Warn: "not right"}[c.State]
	}
	return c.Name + ": " + c.Detail
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func refList(refs []string) string {
	if len(refs) > 5 {
		return strings.Join(refs[:5], ", ") + fmt.Sprintf(" and %d more", len(refs)-5)
	}
	return strings.Join(refs, ", ")
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Component returns the named component and whether there is one.
func (r *Report) Component(name string) (Component, bool) {
	for _, c := range r.Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// Project returns the result of ref and whether there is one.
func (r *Report) Project(ref string) (ProjectResult, bool) {
	for _, p := range r.Projects {
		if p.Ref == ref {
			return p, true
		}
	}
	return ProjectResult{}, false
}

// Public is the answer of the unauthenticated /healthz: the verdict and nothing else. No
// project names, no versions, no component names.
type Public struct {
	Status Verdict `json:"status"`
}

// FilterProjects returns a copy of r that lists only the projects keep accepts (by result).
// The components and the verdict stay: they describe the node, not a tenant. Summary and Issues
// are recomputed, so they do not name a project the reader may not see.
func (r *Report) FilterProjects(keep func(ProjectResult) bool) *Report {
	out := *r
	out.Projects = nil
	for _, p := range r.Projects {
		if keep(p) {
			out.Projects = append(out.Projects, p)
		}
	}
	verdict := r.Verdict
	out.Finish()
	// Finish derives the verdict from what is left; the node's own verdict must not improve
	// because a reader cannot see the project that is down.
	if verdict.ExitCode() > out.Verdict.ExitCode() {
		out.Verdict = verdict
		out.Summary = out.summary()
		if len(out.Issues) == 0 {
			out.Summary = string(verdict) + ": something outside your organizations needs attention"
		}
	}
	return &out
}
