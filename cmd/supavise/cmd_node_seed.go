package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/artifacts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/placement"
)

// standbySeeder builds this server's standby of the system cluster for a join or a rejoin
// (cluster.JoinOptions.Seed and Preflight, RejoinOptions.Seed and Preflight). Close ends what the seeding
// kept running for it, and the command calls it when the join or the rejoin has returned.
type standbySeeder interface {
	Preflight(ctx context.Context) error
	Seed(ctx context.Context, b peerapi.SystemBootstrap) error
	Close()
}

// openStandby makes the seeder of a command: joining for `node join`, which runs before this server
// holds the leader's settings, and not for `node rejoin`. The tests of the commands replace it.
var openStandby = func(log *slog.Logger, joining bool) standbySeeder { return newSystemStandby(log, joining) }

// runAsSupavise is what the commands that seed a cluster call once they have read the inputs only root
// can read (the token file, the key and passphrase files): a command that sudo started as root becomes
// the supavise user, so that the data directory, the artifacts and the relay's sockets it creates belong
// to the user whose units use them. Run as anyone else it does nothing. The tests of the commands
// replace it.
var runAsSupavise = func() error {
	if os.Geteuid() != 0 {
		return nil
	}
	return becomeSupavise()
}

// systemStandby is the standbySeeder over the plane, the backup service and the artifact store of this
// server. The join writes the leader's settings (config.d/10-cluster.toml: the backup backend and its
// keys) after the command loaded its own, so Seed reads the configuration again.
//
// What a joining server lacks is built here, not by the install: no artifact is on disk before the
// join (nothing downloads them for a server that does not create a system project), and the standby
// replays WAL through restore_command, which asks the WAL relay of this node for the system cluster
// while no daemon runs.
type systemStandby struct {
	log     *slog.Logger
	joining bool
	// euid is os.Geteuid; the tests replace it.
	euid func() int
	// options changes the lifecycle options (the tests give it a supervisor and an artifact store).
	options func(*lifecycle.OpenOptions)
	// openPlane builds the plane that seeds; the tests replace it.
	openPlane func(cfg *config.Config, lo lifecycle.OpenOptions) (placement.StandbyPlane, func(), error)
	// seeder builds the plane's seeder over the backup store; the tests replace it.
	seeder func(ctx context.Context, cfg *config.Config) (lifecycle.ReplicaSeeder, error)

	mu     sync.Mutex
	closes []func()
}

func newSystemStandby(log *slog.Logger, joining bool) *systemStandby {
	s := &systemStandby{log: log, joining: joining, euid: os.Geteuid}
	s.openPlane = func(cfg *config.Config, lo lifecycle.OpenOptions) (placement.StandbyPlane, func(), error) {
		return lifecycle.OpenStandbyPlane(cfg, lo)
	}
	s.seeder = func(ctx context.Context, cfg *config.Config) (lifecycle.ReplicaSeeder, error) {
		store, err := backup.OpenStore(ctx, cfg.Backup)
		if err != nil {
			return nil, fmt.Errorf("the backup store named in [backup] does not open: %w", err)
		}
		svc, err := backup.New(backup.Options{Config: cfg, Store: store, ConfigPath: effectiveConfigPath(), Version: version, Log: s.log})
		if err != nil {
			return nil, err
		}
		return placement.SeederFrom(svc), nil
	}
	return s
}

// errStandbyAsRoot is returned when the seeding would run as root: it would leave the standby's data
// directory to root, and Postgres refuses to start on a directory it does not own.
var errStandbyAsRoot = errors.New("the system standby's data directory must belong to the supavise user, and this process is root (run the command as the supavise user: sudo -u supavise supavise ...)")

func (s *systemStandby) notRoot() error {
	if s.euid() == 0 {
		return errStandbyAsRoot
	}
	return nil
}

func (s *systemStandby) onClose(f func()) {
	s.mu.Lock()
	s.closes = append(s.closes, f)
	s.mu.Unlock()
}

// Close stops the relay and lets go of the supervisor, last started first.
func (s *systemStandby) Close() {
	s.mu.Lock()
	closes := s.closes
	s.closes = nil
	s.mu.Unlock()
	for i := len(closes) - 1; i >= 0; i-- {
		closes[i]()
	}
}

// lifecycleOptions is what the plane is built from: the archive and restore commands of the config,
// the artifact store, and ready, called when a cluster's directories exist and before it starts.
func (s *systemStandby) lifecycleOptions(cfg *config.Config, ready func(ref string)) (lifecycle.OpenOptions, error) {
	lo := app.LifecycleOptions(cfg, app.Options{Log: s.log, ConfigPath: effectiveConfigPath(), Version: version, ArchiveReady: ready})
	if s.options != nil {
		s.options(&lo)
	}
	if lo.Artifacts == nil {
		store, err := artifacts.New(cfg, artifacts.WithLogger(s.log))
		if err != nil {
			return lo, err
		}
		lo.Artifacts = store
	}
	return lo, nil
}

// fetchArtifacts downloads the artifacts of svcs that are not on disk.
func fetchArtifacts(ctx context.Context, arts lifecycle.Artifacts, svcs ...string) error {
	f, ok := arts.(interface {
		Fetch(context.Context, string) (string, error)
	})
	if !ok {
		return errors.New("the artifact store cannot download")
	}
	for _, svc := range svcs {
		if _, err := f.Fetch(ctx, svc); err != nil {
			return fmt.Errorf("fetching the %s artifact: %w", svc, err)
		}
	}
	return nil
}

// standbyServices are the artifacts a follower needs on disk: Postgres, GoTrue and PostgREST for the
// system standby and the replicas, and the shared services, which a follower runs cold (Supavisor
// runs) so that a promotion has them. It is the set `system init` and `fleet start` fetch.
func standbyServices(cfg *config.Config) []string {
	svcs := append([]string{}, config.ProjectServices...)
	for _, svc := range fleet.ServicesFor(cfg) {
		if svc == config.SvcStudio && cfg.Studio.ArtifactURL == "" {
			continue // fleet start skips Studio without a build to fetch, too
		}
		svcs = append(svcs, svc)
	}
	return svcs
}

// Preflight checks, before the join spends its token or the rejoin moves data aside, what the seeding
// needs: the Postgres release (fetched here, because a server that has not joined has none), room on the
// disk, and for a rejoin a backend a second server can read (the join takes the backend from the leader).
func (s *systemStandby) Preflight(ctx context.Context) error {
	if err := s.notRoot(); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	lo, err := s.lifecycleOptions(cfg, nil)
	if err != nil {
		return err
	}
	if err := fetchArtifacts(ctx, lo.Artifacts, config.SvcPostgres); err != nil {
		return err
	}
	pl, closeUnits, err := s.openPlane(cfg, lo)
	if err != nil {
		return err
	}
	defer closeUnits()
	return placement.SystemStandby{Plane: pl, Joining: s.joining}.Preflight(ctx)
}

// Seed builds the standby from the base backup b names and starts it. The configuration is read again
// first: the join has written the leader's settings since the command loaded its own. The WAL relay of
// the system cluster runs in this process until Close, because the standby replays the archive through
// it before it streams, and the daemon is not up yet. It can be repeated (see
// lifecycle.PostgresPlane.SeedSystemStandby).
func (s *systemStandby) Seed(ctx context.Context, b peerapi.SystemBootstrap) error {
	if err := s.notRoot(); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.FileBackup() {
		return fmt.Errorf("[backup] names %s, a file:// backend a second server cannot read; point it at S3-compatible storage on the leader first", cfg.Backup.Backend)
	}
	var relay *backup.Relay
	ready := func(ref string) {
		if relay != nil {
			_ = relay.Ensure(ref)
		}
	}
	relay, stopRelay := app.StartWALRelayAt(ctx, cfg, config.ResolvePath(configPath), s.log, true, config.SystemRef)
	s.onClose(stopRelay)
	lo, err := s.lifecycleOptions(cfg, ready)
	if err != nil {
		return err
	}
	if err := fetchArtifacts(ctx, lo.Artifacts, standbyServices(cfg)...); err != nil {
		return err
	}
	pl, closeUnits, err := s.openPlane(cfg, lo)
	if err != nil {
		return err
	}
	s.onClose(closeUnits)
	seeder, err := s.seeder(ctx, cfg)
	if err != nil {
		return err
	}
	return placement.SystemStandby{Plane: pl, Seeder: seeder}.Seed(ctx, b)
}
