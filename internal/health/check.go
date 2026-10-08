package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/notice"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// ProjectProber probes a project's units with real requests: lifecycle.PostgresPlane.
type ProjectProber interface {
	Health(ctx context.Context, p *registry.Project, keys *secrets.ProjectKeys) []lifecycle.ServiceHealth
}

// Escrow is whether the node's master key has an encrypted copy in the backup backend.
type Escrow struct {
	Covered bool
	// Detail says what was found, for the report.
	Detail string
}

// Deps is what CheckNode looks at. Every part is optional: a part that is nil is skipped (a
// development node with no fleet has nothing to say about it), and a Registry that could not
// be opened (RegistryErr) makes the project checks report that instead of failing the run.
// `supavise status` fills it from an opened node; the daemon fills it from its own.
type Deps struct {
	Cfg     *config.Config
	Version string
	Log     *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	Registry    registry.Registry
	RegistryErr error
	// Plane probes the units of each project.
	Plane ProjectProber
	// System probes the system project (supavise-postgres@system and supavise-gotrue@system).
	System func(ctx context.Context) []lifecycle.ServiceHealth
	// Services reports the shared services (fleet.Manager.Status).
	Services func(ctx context.Context) []fleet.Health
	// Tenants asks the shared services whether they hold each project's tenant.
	Tenants fleet.Fleet
	// Daemon and Edge probe the daemon's admin listener and the public listener. Nil means the
	// check runs inside the daemon, which is then evidently up.
	Daemon func(ctx context.Context) error
	Edge   func(ctx context.Context) error
	// Escrow looks for the master key's copy in the backup backend. Nil skips the check.
	Escrow func(ctx context.Context) (*Escrow, error)
	// Disk reads the free and total bytes of the filesystem holding path; nil uses statfs.
	Disk func(path string) (free, total uint64, err error)
	// Node reads the machine's memory and cores for the capacity line; nil detects them.
	Node func() lifecycle.NodeResources

	// Parallel bounds the projects probed at once (0 means 16). ProjectTimeout bounds one
	// project's probes (0 means 10 seconds).
	Parallel       int
	ProjectTimeout time.Duration
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// CheckNode looks at the whole node: the daemon, the edge, the system cluster, each shared
// service, every project, the disk, the certificates, the backups, the master key's copy and
// the update state, and returns what it found with the verdict. It reports problems in the
// report and returns an error only when ctx ends first or Deps lacks the config.
func CheckNode(ctx context.Context, d Deps) (*Report, error) {
	if d.Cfg == nil {
		return nil, errors.New("health: Deps needs Cfg")
	}
	r := &Report{CheckedAt: d.now().UTC(), Version: d.Version}

	var (
		wg       sync.WaitGroup
		nodeComp []Component
		projects []ProjectResult
		sysBk    *Component
	)
	wg.Add(2)
	go func() { defer wg.Done(); nodeComp = d.checkNode(ctx) }()
	go func() { defer wg.Done(); projects, sysBk = d.checkProjects(ctx) }()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.Components = nodeComp
	if sysBk != nil {
		r.Components = append(r.Components, *sysBk)
	}
	r.Components = append(r.Components, d.checkLocal(ctx)...)
	r.Projects = projects
	r.Finish()
	return r, nil
}

// checkNode probes the control plane and the shared services.
func (d *Deps) checkNode(ctx context.Context) []Component {
	var out []Component
	probe := func(name string, critical bool, f func(context.Context) error, inProcess string) {
		c := Component{Name: name, Critical: critical, State: OK, Detail: inProcess}
		if f != nil {
			c.Detail = ""
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := f(pctx); err != nil {
				c.State, c.Detail = Fail, err.Error()
			}
		}
		out = append(out, c)
	}
	probe("daemon", true, d.Daemon, "this process")
	probe("edge", true, d.Edge, "this process")

	if d.System != nil {
		got := map[string]lifecycle.ServiceHealth{}
		for _, h := range d.System(ctx) {
			got[h.Name] = h
		}
		for _, svc := range []string{config.SvcPostgres, config.SvcGoTrue} {
			h, ok := got[svc]
			c := Component{Name: "system " + svc, Critical: svc == config.SvcPostgres, State: OK}
			switch {
			case !ok:
				c.State, c.Detail = Fail, "not checked"
			case !h.Healthy:
				c.State, c.Detail = serviceState(h), serviceDetail(h)
			}
			out = append(out, c)
		}
	}
	if d.Services != nil {
		for _, h := range d.Services(ctx) {
			c := Component{Name: h.Service, State: OK}
			switch {
			case h.Healthy:
			case h.Optional:
				c.State, c.Detail = Info, firstNonEmpty(h.Error, strings.ToLower(h.Status))
			case h.Status == "COMING_UP":
				c.State, c.Detail = Warn, firstNonEmpty(h.Error, "starting")
			default:
				c.State, c.Detail = Fail, firstNonEmpty(h.Error, strings.ToLower(h.Status))
			}
			out = append(out, c)
		}
	}
	return out
}

func serviceState(h lifecycle.ServiceHealth) State {
	if h.Status == "COMING_UP" {
		return Warn
	}
	return Fail
}

func serviceDetail(h lifecycle.ServiceHealth) string {
	return firstNonEmpty(h.Error, strings.ToLower(h.Status))
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// checkProjects probes every project the registry lists and the system cluster's backups.
func (d *Deps) checkProjects(ctx context.Context) ([]ProjectResult, *Component) {
	if d.Registry == nil {
		if d.RegistryErr != nil {
			return nil, &Component{Name: "registry", State: Fail, Critical: true, Detail: "cannot open the node: " + shorten(d.RegistryErr.Error())}
		}
		return nil, nil
	}
	ps, err := d.Registry.ListProjects(ctx)
	if err != nil {
		return nil, &Component{Name: "registry", State: Fail, Critical: true, Detail: "cannot list projects: " + err.Error()}
	}
	par := d.Parallel
	if par <= 0 {
		par = 16
	}
	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, par)
		mu      sync.Mutex
		results []ProjectResult
		sysBk   *Component
	)
	for _, p := range ps {
		p := p
		if p.Status == registry.StatusRemoved {
			continue
		}
		if p.Ref == config.SystemRef {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := d.systemBackup(ctx, p)
				mu.Lock()
				sysBk = c
				mu.Unlock()
			}()
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pr := d.checkProject(ctx, &p)
			mu.Lock()
			results = append(results, pr)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results, sysBk
}

// checkProject probes one project according to its registry status.
func (d *Deps) checkProject(ctx context.Context, p *registry.Project) ProjectResult {
	pr := ProjectResult{Ref: p.Ref, Name: p.Name, OrgID: p.OrgID, Status: string(p.Status), State: OK}
	switch p.Status {
	case registry.StatusActiveHealthy, registry.StatusActiveUnhealthy:
	case registry.StatusInactive:
		pr.Detail = "paused"
		return pr
	case registry.StatusInitFailed:
		pr.State, pr.Detail = Info, "creation failed; delete the project or create it again"
		return pr
	case registry.StatusRestoreFailed:
		pr.State, pr.Detail = Warn, "the last restore failed"
		return pr
	case registry.StatusUnknown:
		pr.State, pr.Detail = Warn, "status unknown"
		return pr
	default: // being created, paused, restarted, restored, upgraded or deleted
		pr.State, pr.Detail = Info, "busy: "+strings.ToLower(strings.ReplaceAll(string(p.Status), "_", " "))
		return pr
	}

	timeout := d.ProjectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pr.Probed = true
	var problems []string
	if d.Plane != nil {
		for _, h := range d.Plane.Health(pctx, p, nil) {
			pr.Services = append(pr.Services, ServiceResult{Name: h.Name, OK: h.Healthy, Status: h.Status, Error: h.Error})
			if h.Healthy {
				continue
			}
			s := serviceState(h)
			if s.rank() > pr.State.rank() {
				pr.State = s
			}
			problems = append(problems, h.Name+" "+firstNonEmpty(h.Error, strings.ToLower(h.Status)))
		}
	}
	for _, t := range d.Tenants.TenantPresence(pctx, p.Ref) {
		tr := TenantResult{Service: t.Service, Present: t.Present}
		switch {
		case t.Err != nil:
			tr.Error = t.Err.Error()
			if pr.State.rank() < Warn.rank() {
				pr.State = Warn
			}
			problems = append(problems, t.Service+" tenant unknown: "+shorten(tr.Error))
		case !t.Present:
			pr.State = Fail
			problems = append(problems, t.Service+" has no tenant for this project")
		}
		pr.Tenants = append(pr.Tenants, tr)
	}
	if d.Registry != nil {
		pr.Backup = d.backupFreshness(pctx, p)
		if pr.Backup != nil && (pr.Backup.Stale || pr.Backup.LastFailed != "") {
			if pr.State.rank() < Warn.rank() {
				pr.State = Warn
			}
			if pr.Backup.LastFailed != "" {
				problems = append(problems, "last backup failed: "+shorten(pr.Backup.LastFailed))
			} else {
				problems = append(problems, "backup is stale")
			}
		}
	}
	pr.Detail = strings.Join(problems, "; ")
	return pr
}

func shorten(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}

// backupFreshness reads the project's backup rows from the registry.
func (d *Deps) backupFreshness(ctx context.Context, p *registry.Project) *BackupResult {
	rows, err := d.Registry.ListBackups(ctx, p.Ref)
	if err != nil {
		return &BackupResult{Note: "could not read the backup list: " + shorten(err.Error())}
	}
	return freshness(rows, p.CreatedAt, d.now(), d.Cfg.BackupStale())
}

// freshness judges rows (newest first) at now.
func freshness(rows []registry.Backup, created, now time.Time, stale time.Duration) *BackupResult {
	b := &BackupResult{}
	var last *registry.Backup
	for i := range rows {
		if rows[i].Status == registry.BackupCompleted && rows[i].FinishedAt != nil {
			last = &rows[i]
			break
		}
	}
	if len(rows) > 0 && rows[0].Status == registry.BackupFailed && (last == nil || rows[0].StartedAt.After(*last.FinishedAt)) {
		b.LastFailed = firstNonEmpty(rows[0].Error, "failed")
	}
	if last == nil {
		if created.IsZero() || now.Sub(created) < stale {
			b.Note = "no backup yet"
		} else {
			b.Stale = true
			b.Note = "no completed backup"
		}
		return b
	}
	at := last.FinishedAt.UTC()
	b.LastCompleted = &at
	age := now.Sub(at)
	b.AgeSeconds = int64(age.Seconds())
	b.Stale = age > stale
	return b
}

// systemBackup is the freshness of the system cluster's backups, which hold the registry.
func (d *Deps) systemBackup(ctx context.Context, p registry.Project) *Component {
	c := &Component{Name: "system backup", State: OK}
	b := d.backupFreshness(ctx, &p)
	switch {
	case b.LastFailed != "":
		c.State, c.Detail = Warn, "last backup failed: "+shorten(b.LastFailed)
	case b.Stale:
		c.State, c.Detail = Warn, "backup is stale"
	case b.LastCompleted != nil:
		c.Detail = "last backup " + humanAge(time.Duration(b.AgeSeconds)*time.Second) + " ago"
	case b.Note != "":
		c.Detail = b.Note
	}
	return c
}

// checkLocal looks at this machine and the node's own files: disk, certificates, the
// master key's copy, the update record and the notices.
func (d *Deps) checkLocal(ctx context.Context) []Component {
	out := []Component{d.checkDisk(), d.checkCertificates(ctx)}
	if c := d.checkCapacity(ctx); c != nil {
		out = append(out, *c)
	}
	if c := d.checkEscrow(ctx); c != nil {
		out = append(out, *c)
	}
	out = append(out, d.checkUpdate())
	out = append(out, d.checkNotices()...)
	return out
}

func (d *Deps) checkEscrow(ctx context.Context) *Component {
	if d.Escrow == nil {
		return nil
	}
	ectx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	e, err := d.Escrow(ectx)
	c := &Component{Name: "key escrow", State: OK}
	switch {
	case errors.Is(err, errEscrowPending):
		c.Detail = "not checked yet"
	case err != nil:
		c.State, c.Detail = Info, "could not check the backup backend: "+shorten(err.Error())
	case !e.Covered:
		c.State, c.Detail = Info, firstNonEmpty(e.Detail, "the master key is not in the backups; see `supavise backups status`")
	default:
		c.Detail = firstNonEmpty(e.Detail, "an encrypted copy is in the backup backend")
	}
	return c
}

func (d *Deps) checkNotices() []Component {
	var out []Component
	paths := d.Cfg.Paths()
	now := d.now()
	if u, ok := notice.UpgradeRunning(paths, now); ok {
		detail := "phase " + u.Phase
		if u.To != "" {
			detail = fmt.Sprintf("%s to %s", detail, u.To)
		}
		out = append(out, Component{Name: "upgrade", State: Info, Detail: detail})
	}
	if m, err := notice.ReadMaintenance(paths); err == nil && m != nil && now.Before(m.EndsAt) {
		detail := fmt.Sprintf("%q from %s to %s", m.Message, m.StartsAt.Format(time.RFC3339), m.EndsAt.Format(time.RFC3339))
		out = append(out, Component{Name: "maintenance", State: Info, Detail: detail})
	}
	return out
}

// humanAge renders d as "5m", "3h" or "2d".
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

// ProbeEdge returns a probe that opens a TCP connection to the node's public listener on
// loopback (HTTPS, or HTTP when TLS is off).
func ProbeEdge(cfg *config.Config) func(context.Context) error {
	addr := cfg.Listen.HTTPS
	if cfg.TLS.Mode == "off" {
		addr = cfg.Listen.HTTP
	}
	return dialProbe("the edge listener", addr)
}

// ProbeAdmin returns a probe that opens a TCP connection to the daemon's admin listener.
func ProbeAdmin(cfg *config.Config) func(context.Context) error {
	return dialProbe("the daemon's admin listener", cfg.Listen.Admin)
}

func dialProbe(what, addr string) func(context.Context) error {
	return func(ctx context.Context) error {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("%s: bad address %q", what, addr)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		var dl net.Dialer
		c, err := dl.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			return fmt.Errorf("%s does not answer (%s)", what, net.JoinHostPort(host, port))
		}
		return c.Close()
	}
}
