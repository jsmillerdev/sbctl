package health

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/notice"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

const (
	refA = "aaaaaaaaaaaaaaaaaaaa"
	refB = "bbbbbbbbbbbbbbbbbbbb"
	refC = "cccccccccccccccccccc"
)

type fakePlane struct {
	mu    sync.Mutex
	out   map[string][]lifecycle.ServiceHealth
	calls map[string]int
}

func healthy3() []lifecycle.ServiceHealth {
	return []lifecycle.ServiceHealth{
		{Name: "postgres", Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: "gotrue", Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: "postgrest", Healthy: true, Status: "ACTIVE_HEALTHY"},
	}
}

func (f *fakePlane) Health(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) []lifecycle.ServiceHealth {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[p.Ref]++
	if hs, ok := f.out[p.Ref]; ok {
		return hs
	}
	return healthy3()
}

type fakeTenant struct {
	svc     string
	missing map[string]bool
	err     error
}

func (f *fakeTenant) Service() string                                      { return f.svc }
func (f *fakeTenant) EnsureTenant(context.Context, fleet.TenantSpec) error { return nil }
func (f *fakeTenant) RemoveTenant(context.Context, string) error           { return nil }
func (f *fakeTenant) HasTenant(_ context.Context, ref string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return !f.missing[ref], nil
}

type env struct {
	t     *testing.T
	cfg   *config.Config
	reg   *registry.Memory
	plane *fakePlane
	deps  Deps
	now   time.Time
}

// newEnv is a healthy node: the system project and three user projects (a and b running, c
// paused), every unit answering, every tenant present, a fresh backup for each running project.
func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.TLS.Mode = "off"
	e := &env{t: t, cfg: cfg, reg: registry.NewMemory(), plane: &fakePlane{out: map[string][]lifecycle.ServiceHealth{}}}
	e.now = time.Now()
	ctx := context.Background()
	for _, p := range []*registry.Project{
		{Ref: config.SystemRef, Name: "system", Status: registry.StatusActiveHealthy},
		{Ref: refA, Name: "alpha", OrgID: 1, Status: registry.StatusActiveHealthy},
		{Ref: refB, Name: "beta", OrgID: 2, Status: registry.StatusActiveHealthy},
		{Ref: refC, Name: "gamma", OrgID: 1, Status: registry.StatusInactive},
	} {
		if err := e.reg.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{config.SystemRef, refA, refB} {
		e.backup(ref, registry.BackupCompleted, e.now.Add(-3*time.Hour), "")
	}
	e.deps = Deps{
		Cfg: cfg, Version: "v1.2.3", Now: func() time.Time { return e.now },
		Registry: e.reg, Plane: e.plane,
		System: func(context.Context) []lifecycle.ServiceHealth { return healthy3()[:2] },
		Services: func(context.Context) []fleet.Health {
			return []fleet.Health{
				{Service: "supavisor", Healthy: true, Status: "ACTIVE_HEALTHY"},
				{Service: "realtime", Healthy: true, Status: "ACTIVE_HEALTHY"},
				{Service: "storage", Healthy: true, Status: "ACTIVE_HEALTHY"},
			}
		},
		Tenants: fleet.Fleet{&fakeTenant{svc: "realtime"}, &fakeTenant{svc: "storage"}},
		Disk:    func(string) (uint64, uint64, error) { return 60 << 30, 100 << 30, nil },
		Node:    func() lifecycle.NodeResources { return lifecycle.NodeResources{MemoryBytes: 16 << 30, CPUs: 4} },
	}
	return e
}

func (e *env) backup(ref string, st registry.BackupStatus, finished time.Time, errMsg string) {
	e.t.Helper()
	b := &registry.Backup{Ref: ref, Kind: "base", Status: st, StartedAt: finished.Add(-5 * time.Minute), Error: errMsg}
	if st == registry.BackupCompleted {
		b.FinishedAt = &finished
	}
	if err := e.reg.CreateBackup(context.Background(), b); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) check() *Report {
	e.t.Helper()
	r, err := CheckNode(context.Background(), e.deps)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestHealthyNode(t *testing.T) {
	e := newEnv(t)
	r := e.check()
	if r.Verdict != Healthy {
		t.Fatalf("verdict %q: %s\n%+v", r.Verdict, r.Summary, r.Components)
	}
	if r.Summary != "healthy: 2 projects answering, 1 paused" {
		t.Errorf("summary %q", r.Summary)
	}
	if r.Version != "v1.2.3" {
		t.Errorf("version %q", r.Version)
	}
	for _, name := range []string{"daemon", "edge", "system postgres", "system gotrue", "supavisor", "realtime", "storage", "disk", "certificates", "update", "system backup"} {
		if _, ok := r.Component(name); !ok {
			t.Errorf("component %q is missing from %+v", name, r.Components)
		}
	}
	a, _ := r.Project(refA)
	if !a.Probed || a.State != OK || len(a.Services) != 3 || len(a.Tenants) != 2 || a.Backup == nil || a.Backup.Stale {
		t.Errorf("project a: %+v backup %+v", a, a.Backup)
	}
	c, _ := r.Project(refC)
	if c.Probed || c.Detail != "paused" || e.plane.calls[refC] != 0 {
		t.Errorf("a paused project is probed or not reported as paused: %+v (probes: %d)", c, e.plane.calls[refC])
	}
	if _, ok := r.Project(config.SystemRef); ok {
		t.Error("the system project is listed among the projects")
	}
}

func TestStoppedPostgRESTDegradesTheNode(t *testing.T) {
	e := newEnv(t)
	e.plane.out[refA] = []lifecycle.ServiceHealth{
		{Name: "postgres", Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: "gotrue", Healthy: true, Status: "ACTIVE_HEALTHY"},
		{Name: "postgrest", Status: "UNHEALTHY", Error: "unit is inactive/dead"},
	}
	r := e.check()
	if r.Verdict != Degraded || r.Verdict.ExitCode() != 1 {
		t.Fatalf("verdict %q", r.Verdict)
	}
	a, _ := r.Project(refA)
	if a.State != Fail || !strings.Contains(a.Detail, "postgrest") {
		t.Errorf("project a: %+v", a)
	}
	b, _ := r.Project(refB)
	if b.State != OK {
		t.Errorf("the other project is blamed: %+v", b)
	}
	if !strings.Contains(r.Summary, refA) {
		t.Errorf("summary does not name the project: %q", r.Summary)
	}
}

func TestMissingTenantAndUnreachableService(t *testing.T) {
	e := newEnv(t)
	e.deps.Tenants = fleet.Fleet{&fakeTenant{svc: "realtime", missing: map[string]bool{refA: true}}, &fakeTenant{svc: "storage", err: errors.New("connection refused")}}
	r := e.check()
	a, _ := r.Project(refA)
	if a.State != Fail || !strings.Contains(a.Detail, "realtime has no tenant") {
		t.Errorf("a missing tenant: %+v", a)
	}
	b, _ := r.Project(refB)
	if b.State != Warn || !strings.Contains(b.Detail, "storage tenant unknown") {
		t.Errorf("an unreachable service is not a warning on the project: %+v", b)
	}
	if r.Verdict != Degraded {
		t.Errorf("verdict %q", r.Verdict)
	}
}

func TestBackupFreshness(t *testing.T) {
	e := newEnv(t)
	// A: its only completed backup is older than the limit (36 hours by default).
	ctx := context.Background()
	rows, _ := e.reg.ListBackups(ctx, refA)
	for _, b := range rows {
		_ = e.reg.DeleteBackup(ctx, b.ID)
	}
	e.backup(refA, registry.BackupCompleted, e.now.Add(-40*time.Hour), "")
	// B: the newest backup failed after a completed one.
	e.backup(refB, registry.BackupFailed, e.now.Add(-1*time.Hour), "S3 said AccessDenied")
	r := e.check()
	a, _ := r.Project(refA)
	if !a.Backup.Stale || a.State != Warn {
		t.Errorf("stale backup: %+v %+v", a, a.Backup)
	}
	b, _ := r.Project(refB)
	if b.Backup.LastFailed != "S3 said AccessDenied" || b.State != Warn || b.Backup.Stale {
		t.Errorf("failed backup: %+v %+v", b, b.Backup)
	}
	if r.Verdict != Degraded || !strings.Contains(r.Summary, "without a fresh backup") {
		t.Errorf("verdict %q summary %q", r.Verdict, r.Summary)
	}
}

func TestFreshnessCases(t *testing.T) {
	now := time.Now()
	done := func(ago time.Duration) registry.Backup {
		f := now.Add(-ago)
		return registry.Backup{Status: registry.BackupCompleted, FinishedAt: &f, StartedAt: f.Add(-time.Minute)}
	}
	stale := 36 * time.Hour
	if b := freshness(nil, now.Add(-time.Hour), now, stale); b.Stale || b.Note != "no backup yet" {
		t.Errorf("a new project without a backup: %+v", b)
	}
	if b := freshness(nil, now.Add(-5*24*time.Hour), now, stale); !b.Stale {
		t.Errorf("an old project without a backup: %+v", b)
	}
	if b := freshness([]registry.Backup{done(20 * time.Hour)}, now.Add(-9*24*time.Hour), now, stale); b.Stale || b.LastCompleted == nil || b.AgeSeconds < 19*3600 {
		t.Errorf("a fresh backup: %+v", b)
	}
	// A running backup is neither fresh nor a failure.
	running := registry.Backup{Status: registry.BackupRunning, StartedAt: now}
	if b := freshness([]registry.Backup{running, done(2 * time.Hour)}, now.Add(-9*24*time.Hour), now, stale); b.Stale || b.LastFailed != "" {
		t.Errorf("a backup in progress: %+v", b)
	}
	// A failure older than the last success is history.
	old := registry.Backup{Status: registry.BackupFailed, StartedAt: now.Add(-30 * time.Hour), Error: "x"}
	if b := freshness([]registry.Backup{done(2 * time.Hour), old}, now.Add(-9*24*time.Hour), now, stale); b.LastFailed != "" {
		t.Errorf("an old failure is reported: %+v", b)
	}
}

func TestSystemClusterDownIsDown(t *testing.T) {
	e := newEnv(t)
	e.deps.System = func(context.Context) []lifecycle.ServiceHealth {
		return []lifecycle.ServiceHealth{
			{Name: "postgres", Status: "UNHEALTHY", Error: "unit is failed/failed"},
			{Name: "gotrue", Healthy: true, Status: "ACTIVE_HEALTHY"},
		}
	}
	r := e.check()
	if r.Verdict != Down || r.Verdict.ExitCode() != 2 {
		t.Fatalf("verdict %q", r.Verdict)
	}
	if c, _ := r.Component("system postgres"); c.State != Fail || !c.Critical {
		t.Errorf("%+v", c)
	}
}

func TestSystemGoTrueDownOnlyDegrades(t *testing.T) {
	e := newEnv(t)
	e.deps.System = func(context.Context) []lifecycle.ServiceHealth {
		return []lifecycle.ServiceHealth{
			{Name: "postgres", Healthy: true, Status: "ACTIVE_HEALTHY"},
			{Name: "gotrue", Status: "UNHEALTHY", Error: "connection refused"},
		}
	}
	if r := e.check(); r.Verdict != Degraded {
		t.Errorf("verdict %q", r.Verdict)
	}
}

func TestSharedServiceDown(t *testing.T) {
	e := newEnv(t)
	e.deps.Services = func(context.Context) []fleet.Health {
		return []fleet.Health{
			{Service: "supavisor", Status: "STOPPED", Error: "unit is inactive/dead"},
			{Service: "studio", Status: "STOPPED", Optional: true, Error: "never started on this node"},
			{Service: "realtime", Status: "COMING_UP", Error: "unit is activating/start"},
		}
	}
	r := e.check()
	if c, _ := r.Component("supavisor"); c.State != Fail || c.Critical {
		t.Errorf("supavisor: %+v", c)
	}
	if c, _ := r.Component("studio"); c.State != Info {
		t.Errorf("an optional service that never started must not fail the node: %+v", c)
	}
	if c, _ := r.Component("realtime"); c.State != Warn {
		t.Errorf("a service that is starting is a warning: %+v", c)
	}
	if r.Verdict != Degraded {
		t.Errorf("verdict %q", r.Verdict)
	}
}

func TestDaemonAndEdgeProbes(t *testing.T) {
	e := newEnv(t)
	e.deps.Daemon = func(context.Context) error {
		return errors.New("the daemon's admin listener does not answer (127.0.0.1:7000)")
	}
	e.deps.Edge = func(context.Context) error { return nil }
	r := e.check()
	if r.Verdict != Down {
		t.Fatalf("verdict %q", r.Verdict)
	}
	if c, _ := r.Component("edge"); c.State != OK {
		t.Errorf("edge: %+v", c)
	}
	if !strings.Contains(r.Summary, "admin listener") {
		t.Errorf("summary %q", r.Summary)
	}
}

func TestRegistryUnreachableIsDownAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	e.deps.Registry = nil
	e.deps.RegistryErr = errors.New("registry: cannot reach the system cluster")
	r := e.check()
	if r.Verdict != Down {
		t.Fatalf("verdict %q", r.Verdict)
	}
	if c, ok := r.Component("registry"); !ok || c.State != Fail || !strings.Contains(c.Detail, "cannot reach the system cluster") {
		t.Errorf("registry component: %+v", c)
	}
	if len(r.Projects) != 0 {
		t.Errorf("projects listed without a registry: %+v", r.Projects)
	}
}

// The capacity line shows the headroom for creates and resizes, and warns when the sizes already
// promise more than the budget.
func TestCapacity(t *testing.T) {
	e := newEnv(t) // two running Micro projects (1 GB each); the paused one holds nothing
	comp, ok := e.check().Component("capacity")
	if !ok || comp.State != OK || !strings.Contains(comp.Detail, "2 GB of 96 GB project memory caps promised to 2 projects (16 GB of memory x 6 overcommit); 4 cores") {
		t.Fatalf("capacity = %+v %v", comp, ok)
	}
	e.deps.Node = func() lifecycle.NodeResources { return lifecycle.NodeResources{MemoryBytes: 256 << 20, CPUs: 1} }
	comp, _ = e.check().Component("capacity")
	if comp.State != Warn || !strings.Contains(comp.Detail, "over the budget") {
		t.Fatalf("over the budget = %+v", comp)
	}
}

func TestDisk(t *testing.T) {
	cases := []struct {
		name        string
		free, total uint64
		err         error
		want        State
	}{
		{"plenty", 60 << 30, 100 << 30, nil, OK},
		{"under the percentage", 9 << 30, 100 << 30, nil, Warn},
		{"far under the percentage", 4 << 30, 100 << 30, nil, Fail},
		{"a big disk at 8 percent", 80 << 30, 1000 << 30, nil, Warn},
		{"a big disk almost full", 4 << 30, 1000 << 30, nil, Fail},
		{"percentage fine, gigabytes low on a small disk", 3 << 30, 10 << 30, nil, Warn},
		{"unreadable", 0, 0, errors.New("statfs: no such file"), Warn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.deps.Disk = func(string) (uint64, uint64, error) { return c.free, c.total, c.err }
			comp, _ := e.check().Component("disk")
			if comp.State != c.want {
				t.Errorf("state %q (%s), want %q", comp.State, comp.Detail, c.want)
			}
		})
	}
	// A disk failure degrades the node; it is not "down" while everything still answers.
	e := newEnv(t)
	e.deps.Disk = func(string) (uint64, uint64, error) { return 1 << 30, 100 << 30, nil }
	if r := e.check(); r.Verdict != Degraded {
		t.Errorf("verdict %q", r.Verdict)
	}
	// The thresholds come from [health].
	e = newEnv(t)
	e.cfg.Health.DiskLowPercent = 70
	if c, _ := e.check().Component("disk"); c.State != Warn {
		t.Errorf("disk_low_percent = 70 with 60%% free: %+v", c)
	}
}

func writeCert(t *testing.T, dir, host string, notAfter time.Time) {
	t.Helper()
	writeCertFrom(t, dir, "acme-v02.api.letsencrypt.org-directory", host, notAfter)
}

// writeCertFrom stores the certificate under the issuer's directory, as CertMagic does.
func writeCertFrom(t *testing.T, dir, issuer, host string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(dir, "certificates", issuer, host)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, host+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCertificates(t *testing.T) {
	e := newEnv(t)
	e.cfg.TLS.Mode = "auto"
	e.cfg.Domain = "example.com"
	if c, _ := e.check().Component("certificates"); c.State != Info || !strings.Contains(c.Detail, "none issued") {
		t.Errorf("no certificates yet: %+v", c)
	}
	certs := e.cfg.Paths().Certs()
	writeCert(t, certs, "api.example.com", e.now.Add(60*24*time.Hour))
	writeCert(t, certs, "studio.example.com", e.now.Add(40*24*time.Hour))
	if c, _ := e.check().Component("certificates"); c.State != OK || !strings.Contains(c.Detail, "studio.example.com") {
		t.Errorf("healthy certificates: %+v", c)
	}
	// The wildcard is renewed in the background, so a short one means renewal is failing.
	writeCert(t, certs, "*.api.example.com", e.now.Add(5*24*time.Hour))
	r := e.check()
	if c, _ := r.Component("certificates"); c.State != Warn || !strings.Contains(c.Detail, "*.api.example.com") {
		t.Errorf("a managed certificate near its end: %+v", c)
	}
	if r.Verdict != Degraded {
		t.Errorf("verdict %q", r.Verdict)
	}
	writeCert(t, certs, "studio.example.com", e.now.Add(-48*time.Hour))
	if c, _ := e.check().Component("certificates"); c.State != Fail || !strings.Contains(c.Detail, "expired") {
		t.Errorf("an expired managed certificate: %+v", c)
	}
	e.cfg.TLS.Mode = "off"
	if c, _ := e.check().Component("certificates"); c.State != OK || c.Detail != "TLS is off" {
		t.Errorf("TLS off: %+v", c)
	}
}

// CertMagic keeps a copy per issuer. After [tls] ca changes, the old issuer's copy is never
// renewed; the copy being served decides, not the oldest file.
func TestCertificatesJudgeTheNewestCopyOfAName(t *testing.T) {
	e := newEnv(t)
	e.cfg.TLS.Mode = "auto"
	e.cfg.Domain = "example.com"
	certs := e.cfg.Paths().Certs()
	for _, host := range []string{"api.example.com", "studio.example.com", "*.api.example.com"} {
		writeCertFrom(t, certs, "acme-v02.api.letsencrypt.org-directory", host, e.now.Add(60*24*time.Hour))
	}
	// The staging copy of the API certificate expired long ago and nothing renews it.
	writeCertFrom(t, certs, "acme-staging-v02.api.letsencrypt.org-directory", "api.example.com", e.now.Add(-30*24*time.Hour))
	r := e.check()
	c, _ := r.Component("certificates")
	if c.State != OK || !strings.Contains(c.Detail, "3 issued") {
		t.Fatalf("an old issuer's expired copy changed the verdict: %+v", c)
	}
	// When the newest copy of a name is the one that expired, that is still a failure.
	writeCertFrom(t, certs, "acme-v02.api.letsencrypt.org-directory", "studio.example.com", e.now.Add(-time.Hour))
	if c, _ := e.check().Component("certificates"); c.State != Fail || !strings.Contains(c.Detail, "studio.example.com") {
		t.Errorf("the newest copy expired: %+v", c)
	}
}

// HTTP-01 certificates for project hosts are issued and renewed on demand. One for a host the
// node no longer serves is never renewed, and neither it nor a live one that nobody opened lately
// may degrade the node (it would hold back `supavise upgrade --unattended` for good).
func TestOnDemandCertificatesDoNotJudgeTheNode(t *testing.T) {
	e := newEnv(t)
	e.cfg.TLS.Mode = "http01"
	e.cfg.Domain = "example.com"
	certs := e.cfg.Paths().Certs()
	writeCert(t, certs, "api.example.com", e.now.Add(60*24*time.Hour))
	writeCert(t, certs, "studio.example.com", e.now.Add(60*24*time.Hour))
	// A removed project's host (expired), a deleted custom hostname, and a live project's host
	// nobody opened since the last restart.
	writeCert(t, certs, "removed00000000000000.api.example.com", e.now.Add(-72*time.Hour))
	writeCert(t, certs, "data.gone.example.org", e.now.Add(3*24*time.Hour))
	writeCert(t, certs, e.cfg.ProjectHost(refA), e.now.Add(4*24*time.Hour))
	// A custom hostname of a live project.
	if err := e.reg.PutRoute(context.Background(), registry.Route{Host: "db.alpha.example.org", Ref: refA, Kind: "custom"}); err != nil {
		t.Fatal(err)
	}
	writeCert(t, certs, "db.alpha.example.org", e.now.Add(2*24*time.Hour))

	r := e.check()
	c, _ := r.Component("certificates")
	if c.State != OK || r.Verdict != Healthy {
		t.Fatalf("on-demand certificates changed the verdict to %q: %+v", r.Verdict, c)
	}
	for _, want := range []string{"2 issued", "2 project host certificate(s) are near their end", "2 certificate(s) for hosts this node no longer serves are ignored"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q lacks %q", c.Detail, want)
		}
	}
	// Without a registry nothing is known to be live, so every on-demand certificate is ignored.
	e.deps.Registry = nil
	if c, _ := e.check().Component("certificates"); c.State != OK || !strings.Contains(c.Detail, "4 certificate(s)") {
		t.Errorf("no registry: %+v", c)
	}
}

func TestAdvisoriesDoNotChangeTheVerdict(t *testing.T) {
	e := newEnv(t)
	e.deps.Escrow = func(context.Context) (*Escrow, error) { return &Escrow{Covered: false}, nil }
	if err := WriteUpdate(e.cfg.Paths(), &UpdateRecord{CheckedAt: e.now, Installed: "v1.2.3", Latest: "v1.3.0", Available: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := notice.WriteMaintenance(e.cfg.Paths(), notice.Maintenance{Message: "disk work", StartsAt: e.now.Add(time.Hour), EndsAt: e.now.Add(3 * time.Hour)}, e.now); err != nil {
		t.Fatal(err)
	}
	r := e.check()
	if r.Verdict != Healthy {
		t.Fatalf("an advisory changed the verdict to %q: %s", r.Verdict, r.Summary)
	}
	for name, want := range map[string]string{"key escrow": "master key", "update": "v1.3.0 is available", "maintenance": "disk work"} {
		c, ok := r.Component(name)
		if !ok || c.State != Info || !strings.Contains(c.Detail, want) {
			t.Errorf("%s: %+v", name, c)
		}
	}
	e.deps.Escrow = func(context.Context) (*Escrow, error) { return nil, errors.New("s3 unreachable") }
	if c, _ := e.check().Component("key escrow"); c.State != Info {
		t.Errorf("an escrow check that cannot run is not an advisory: %+v", c)
	}
}

func TestUpgradeInProgressIsReported(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(notice.Upgrade{Phase: "rollout", From: "v1.2.3", To: "v1.3.0", StartedAt: e.now.Add(-time.Minute)})
	if err := os.MkdirAll(filepath.Join(e.cfg.StateDir, "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "system", "upgrade.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	c, ok := e.check().Component("upgrade")
	if !ok || c.State != Info || !strings.Contains(c.Detail, "v1.3.0") {
		t.Errorf("%+v", c)
	}
}

func TestProjectStatusesAreHandled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mk := func(ref string, st registry.Status) {
		if err := e.reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref, Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	mk("dddddddddddddddddddd", registry.StatusComingUp)
	mk("eeeeeeeeeeeeeeeeeeee", registry.StatusInitFailed)
	mk("ffffffffffffffffffff", registry.StatusRestoreFailed)
	mk("gggggggggggggggggggg", registry.StatusRemoved)
	r := e.check()
	if p, _ := r.Project("dddddddddddddddddddd"); p.State != Info || p.Probed || !strings.Contains(p.Detail, "coming up") {
		t.Errorf("a project being created: %+v", p)
	}
	if p, _ := r.Project("eeeeeeeeeeeeeeeeeeee"); p.State != Info {
		t.Errorf("a failed creation: %+v", p)
	}
	if p, _ := r.Project("ffffffffffffffffffff"); p.State != Warn {
		t.Errorf("a failed restore: %+v", p)
	}
	if _, ok := r.Project("gggggggggggggggggggg"); ok {
		t.Error("a removed project is listed")
	}
	if r.Verdict != Degraded { // the failed restore
		t.Errorf("verdict %q", r.Verdict)
	}
	for _, ref := range []string{"dddddddddddddddddddd", "eeeeeeeeeeeeeeeeeeee", "ffffffffffffffffffff"} {
		if e.plane.calls[ref] != 0 {
			t.Errorf("%s was probed", ref)
		}
	}
}

func TestManyProjectsAreProbedInParallelWithinTheBound(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		ref := strings.Repeat(string(rune('h'+i%8)), 19) + string(rune('a'+i/8))
		if err := e.reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref, Status: registry.StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	cur, peak := 0, 0
	e.deps.Plane = probeFunc(func(p *registry.Project) []lifecycle.ServiceHealth {
		mu.Lock()
		cur++
		peak = max(peak, cur)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		return healthy3()
	})
	e.deps.Parallel = 4
	r := e.check()
	if len(r.Projects) != 43 || r.Verdict == Down {
		t.Fatalf("%d projects, verdict %q", len(r.Projects), r.Verdict)
	}
	if peak > 4 || peak < 2 {
		t.Errorf("peak concurrency %d, want between 2 and 4", peak)
	}
}

type probeFunc func(p *registry.Project) []lifecycle.ServiceHealth

func (f probeFunc) Health(_ context.Context, p *registry.Project, _ *secrets.ProjectKeys) []lifecycle.ServiceHealth {
	return f(p)
}

func TestCanceledContextReturnsAnError(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CheckNode(ctx, e.deps); err == nil {
		t.Error("a check under a canceled context returned a report")
	}
	if _, err := CheckNode(context.Background(), Deps{}); err == nil {
		t.Error("a check without Cfg did not fail")
	}
}

// The report is what `supavise status --json` prints and what an operator's tooling reads:
// the keys are part of the contract.
func TestReportJSONShape(t *testing.T) {
	e := newEnv(t)
	b, err := json.Marshal(e.check())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "checked_at", "summary", "components", "projects", "version"} {
		if _, ok := m[k]; !ok {
			t.Errorf("report has no %q: %s", k, b)
		}
	}
	if m["status"] != "healthy" {
		t.Errorf("status %v", m["status"])
	}
	if strings.Contains(string(b), `"OrgID"`) || strings.Contains(string(b), "org_id") {
		t.Errorf("the organization id leaks into the report: %s", b)
	}
}
