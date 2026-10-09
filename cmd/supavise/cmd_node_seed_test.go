package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
)

// standbyArts is an artifact store that records what was fetched; Fetch is what the seeder calls.
type standbyArts struct {
	fetched *[]string
	err     error
}

func (a standbyArts) Dir(svc string) (string, error) { return "/artifacts/" + svc, nil }
func (a standbyArts) Tag(svc string) (string, error) { return "v1", nil }
func (a standbyArts) Fetch(_ context.Context, svc string) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	*a.fetched = append(*a.fetched, svc)
	return "/artifacts/" + svc, nil
}

// standbyPlane records what the seeder asks of the plane.
type standbyPlane struct {
	calls  *[]string
	plan   *lifecycle.SystemStandbyPlan
	seeder *lifecycle.ReplicaSeeder
	err    error
}

func (p standbyPlane) SeedSystemStandby(_ context.Context, plan lifecycle.SystemStandbyPlan, seeder lifecycle.ReplicaSeeder) error {
	*p.calls = append(*p.calls, "seed")
	*p.plan, *p.seeder = plan, seeder
	return p.err
}

func (p standbyPlane) SystemStandbyPreflight(context.Context) error {
	*p.calls = append(*p.calls, "preflight")
	return p.err
}

func (p standbyPlane) SystemStandbyJoinPreflight(context.Context) error {
	*p.calls = append(*p.calls, "join-preflight")
	return p.err
}

// standbyRig is a systemStandby over a config file in a temporary directory, with the artifact store,
// the plane and the seeder replaced by recorders.
type standbyRig struct {
	s       *systemStandby
	cfgPath string
	fetched []string
	calls   []string
	plan    lifecycle.SystemStandbyPlan
	seeder  lifecycle.ReplicaSeeder
	planeErr,
	artsErr error
	closed []string
	seeded int // the seeders built
}

func newStandbyRig(t *testing.T, joining bool, body string) *standbyRig {
	t.Helper()
	return newStandbyRigIn(t, t.TempDir(), joining, body)
}

// newStandbyRigIn is newStandbyRig with the server's directories under dir.
func newStandbyRigIn(t *testing.T, dir string, joining bool, body string) *standbyRig {
	t.Helper()
	r := &standbyRig{cfgPath: filepath.Join(dir, "etc", "config.toml")}
	if err := os.MkdirAll(filepath.Dir(r.cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		body = "[backup]\nbackend = \"s3://bucket/prefix\"\nwal_relay = \"off\"\n"
	}
	// The exec supervisor, because the systemd one cannot go with a relay that is off.
	body = "supervisor = \"exec\"\nstate_dir = \"" + filepath.Join(dir, "state") + "\"\nkey_path = \"" + filepath.Join(dir, "etc", "master.key") + "\"\n" + body
	if err := os.WriteFile(r.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := configPath
	configPath = r.cfgPath
	t.Cleanup(func() { configPath = old })

	r.s = newSystemStandby(slog.New(slog.DiscardHandler), joining)
	r.s.euid = func() int { return 1000 }
	r.s.options = func(lo *lifecycle.OpenOptions) { lo.Artifacts = standbyArts{fetched: &r.fetched, err: r.artsErr} }
	r.s.openPlane = func(*config.Config, lifecycle.OpenOptions) (placement.StandbyPlane, func(), error) {
		return standbyPlane{calls: &r.calls, plan: &r.plan, seeder: &r.seeder, err: r.planeErr}, func() { r.closed = append(r.closed, "units") }, nil
	}
	r.s.seeder = func(_ context.Context, cfg *config.Config) (lifecycle.ReplicaSeeder, error) {
		r.seeded++
		r.calls = append(r.calls, "backend "+cfg.Backup.Backend)
		return func(context.Context, lifecycle.ReplicaSeedPlan) error { return nil }, nil
	}
	return r
}

func TestStandbyServicesAreTheOnesSystemInitAndFleetStartFetch(t *testing.T) {
	cfg := config.Default()
	base := []string{"postgres", "gotrue", "postgrest", "pgmeta", "supavisor", "realtime", "storage"}
	if got := standbyServices(cfg); !reflect.DeepEqual(got, base) {
		t.Fatalf("services = %v, want %v", got, base)
	}
	cfg.Functions.Enabled = true
	cfg.Studio.ArtifactURL = "https://example.test/studio.tar.zst"
	got := standbyServices(cfg)
	want := append(append([]string{}, base...), "edge-runtime", "studio")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("with functions and a Studio build: %v, want %v", got, want)
	}
}

// A joining server has no artifact on disk and no say yet over the backend: the preflight downloads
// Postgres, then checks the disk and the release, and leaves the backend to the leader's settings.
func TestStandbyPreflightOfAJoinFetchesPostgresAndLeavesTheBackendAlone(t *testing.T) {
	r := newStandbyRig(t, true, "")
	if err := r.s.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.fetched, []string{"postgres"}) {
		t.Fatalf("fetched %v before the join; only Postgres is needed to check the host", r.fetched)
	}
	if !reflect.DeepEqual(r.calls, []string{"join-preflight"}) {
		t.Fatalf("plane calls = %v", r.calls)
	}
	if !reflect.DeepEqual(r.closed, []string{"units"}) {
		t.Fatalf("closed = %v: the preflight lets go of the supervisor", r.closed)
	}
}

func TestStandbyPreflightOfARejoinChecksTheBackendToo(t *testing.T) {
	r := newStandbyRig(t, false, "")
	if err := r.s.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls, []string{"preflight"}) {
		t.Fatalf("plane calls = %v", r.calls)
	}
}

func TestStandbyPreflightStopsOnADownloadOrAPlaneRefusal(t *testing.T) {
	r := newStandbyRig(t, true, "")
	r.artsErr = errors.New("release server down")
	if err := r.s.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "release server down") || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("a failed download = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("the plane was asked after a failed download: %v", r.calls)
	}
	r = newStandbyRig(t, true, "")
	r.planeErr = errors.New("2 GiB wanted")
	if err := r.s.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "2 GiB wanted") {
		t.Fatalf("a refusal of the plane = %v", err)
	}
}

// The seeding of a standby as root would leave a data directory Postgres refuses to start on.
func TestStandbyRefusesToRunAsRoot(t *testing.T) {
	r := newStandbyRig(t, true, "")
	r.s.euid = func() int { return 0 }
	if err := r.s.Preflight(context.Background()); !errors.Is(err, errStandbyAsRoot) {
		t.Fatalf("preflight as root = %v", err)
	}
	if err := r.s.Seed(context.Background(), peerapi.SystemBootstrap{}); !errors.Is(err, errStandbyAsRoot) {
		t.Fatalf("seed as root = %v", err)
	}
	if len(r.fetched) != 0 || len(r.calls) != 0 {
		t.Fatalf("something ran as root: fetched %v, calls %v", r.fetched, r.calls)
	}
}

// Seed reads the configuration again, because the join wrote the leader's backup settings to
// config.d/10-cluster.toml after the command loaded its own; it downloads what a follower runs, builds
// the standby from the bootstrap and closes what it opened.
func TestStandbySeedUsesTheLeadersSettingsAndTheBootstrap(t *testing.T) {
	// The server's own file names the default file:// backend, as a joiner that passed no --s3 flags does.
	r := newStandbyRig(t, true, "[backup]\nwal_relay = \"off\"\n")
	cluster := filepath.Join(filepath.Dir(r.cfgPath), config.ConfigDName, config.ClusterConfigFile)
	if err := os.MkdirAll(filepath.Dir(cluster), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cluster, []byte("[backup]\nbackend = \"s3://leaders-bucket/p\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := peerapi.SystemBootstrap{Identifier: "system-rr-us-east-1-abc123", BackupID: "20261008T120000Z-abcdef", Leader: "n1", Epoch: 3, ReplicationPassword: "pw"}
	if err := r.s.Seed(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if want := []string{"backend s3://leaders-bucket/p", "seed"}; !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
	if want := (lifecycle.SystemStandbyPlan{Identifier: b.Identifier, BackupID: b.BackupID, ReplicationPassword: "pw"}); r.plan != want {
		t.Fatalf("plan = %+v, want %+v", r.plan, want)
	}
	if r.seeder == nil {
		t.Fatal("the plane got no seeder")
	}
	cfg := config.Default()
	if want := standbyServices(cfg); !reflect.DeepEqual(r.fetched, want) {
		t.Fatalf("fetched %v, want %v", r.fetched, want)
	}
	r.s.Close()
	if !reflect.DeepEqual(r.closed, []string{"units"}) {
		t.Fatalf("closed = %v", r.closed)
	}
}

// A server whose backend is a file:// one is refused before anything is downloaded or built.
func TestStandbySeedRefusesAFileBackend(t *testing.T) {
	r := newStandbyRig(t, true, "[backup]\nbackend = \"file:///var/lib/supavise/backups\"\nwal_relay = \"off\"\n")
	err := r.s.Seed(context.Background(), peerapi.SystemBootstrap{Identifier: "system-rr-us-east-1-abc123"})
	if err == nil || !strings.Contains(err.Error(), "file://") {
		t.Fatalf("seed with a file:// backend = %v", err)
	}
	if len(r.fetched) != 0 || len(r.calls) != 0 {
		t.Fatalf("something ran: fetched %v, calls %v", r.fetched, r.calls)
	}
}

func TestStandbySeedReportsWhatTheDownloadOrThePlaneRefused(t *testing.T) {
	r := newStandbyRig(t, true, "")
	r.artsErr = errors.New("no route to host")
	if err := r.s.Seed(context.Background(), peerapi.SystemBootstrap{}); err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("download failure = %v", err)
	}
	r = newStandbyRig(t, true, "")
	r.planeErr = errors.New("no base backup")
	if err := r.s.Seed(context.Background(), peerapi.SystemBootstrap{}); err == nil || !strings.Contains(err.Error(), "no base backup") {
		t.Fatalf("seed failure = %v", err)
	}
	r.s.Close()
	if !reflect.DeepEqual(r.closed, []string{"units"}) {
		t.Fatalf("closed = %v: a failed seed still lets go of the supervisor", r.closed)
	}
}

// The standby replays the archive through the node's WAL relay before it streams, and no daemon runs
// during a join: the directories of the system cluster make the relay serve its socket at once.
func TestStandbyArchiveReadyReachesTheRelay(t *testing.T) {
	r := newStandbyRig(t, true, "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	lo, err := r.s.lifecycleOptions(cfg, func(ref string) { refs = append(refs, ref) })
	if err != nil {
		t.Fatal(err)
	}
	if lo.ArchiveReady == nil || lo.ArchiveCommandFor == nil {
		t.Fatalf("options lack the archive hooks: %+v", lo)
	}
	lo.ArchiveReady(config.SystemRef)
	if !reflect.DeepEqual(refs, []string{config.SystemRef}) {
		t.Fatalf("ready was told %v", refs)
	}
}

// readyPlane is standbyPlane that runs ready when it is asked to seed: the real plane calls ArchiveReady
// once the system cluster's directories exist, before it starts the standby.
type readyPlane struct {
	standbyPlane
	seeding func()
}

func (p readyPlane) SeedSystemStandby(ctx context.Context, plan lifecycle.SystemStandbyPlan, seeder lifecycle.ReplicaSeeder) error {
	p.seeding()
	return p.standbyPlane.SeedSystemStandby(ctx, plan, seeder)
}

// With the relay on, Seed serves the system cluster's WAL socket from this process, from the moment the
// plane says the cluster's directories exist until Close: the standby replays the archive through it before
// it streams, and no daemon runs during a join. This is the wiring from Seed through StartWALRelayAt and the
// plane's ArchiveReady to Relay.Ensure; the tests above stop at the closure the plane is handed.
func TestStandbySeedServesTheSystemSocketUntilClose(t *testing.T) {
	// A unix socket path is limited to 103 bytes on macOS, which t.TempDir() can exceed.
	dir, err := os.MkdirTemp("/tmp", "sbs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := newStandbyRigIn(t, dir, true, "[backup]\nbackend = \"s3://bucket/prefix\"\nwal_relay = \"on\"\n")

	var sock string
	var duringSeed error
	r.s.openPlane = func(cfg *config.Config, lo lifecycle.OpenOptions) (placement.StandbyPlane, func(), error) {
		plane := readyPlane{standbyPlane: standbyPlane{calls: &r.calls, plan: &r.plan, seeder: &r.seeder}}
		plane.seeding = func() {
			sock = cfg.Paths().WALSocket(config.SystemRef)
			if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
				t.Error(err)
				return
			}
			if lo.ArchiveReady == nil {
				t.Error("the plane was given no ArchiveReady")
				return
			}
			lo.ArchiveReady(config.SystemRef)
			duringSeed = backup.RelayPing(context.Background(), sock)
		}
		return plane, func() {}, nil
	}

	if err := r.s.Seed(context.Background(), peerapi.SystemBootstrap{Identifier: "system-rr-us-east-1-abc123"}); err != nil {
		t.Fatal(err)
	}
	if duringSeed != nil {
		t.Fatalf("the system socket %s did not answer once the plane said its directories exist: %v", sock, duringSeed)
	}
	if err := backup.RelayPing(context.Background(), sock); err != nil {
		t.Fatalf("the relay stopped when Seed returned, and the standby still replays through it: %v", err)
	}
	r.s.Close()
	if err := backup.RelayPing(context.Background(), sock); err == nil {
		t.Fatal("the relay still answers after Close")
	}
}
