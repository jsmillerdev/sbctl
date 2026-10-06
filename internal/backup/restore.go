package backup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// ErrForceRequired is returned when a restore would replace a project's own data and
// RestoreOptions.Force is not set.
var ErrForceRequired = errors.New("backup: restoring in place replaces the project's data with an older copy; pass Force to confirm")

// RestoreOptions tunes RestoreWith.
type RestoreOptions struct {
	// Force allows restoring over the source project itself (newRef empty or equal to ref).
	// The old data directory is kept next to it as <dir>.pre-restore-<time>.
	Force bool
	// BackupID pins the base backup instead of choosing the newest one before the target.
	BackupID string
}

// RestorePlan is the outcome of choosing a base backup for a target time.
type RestorePlan struct {
	Source    string // ref whose archive is replayed
	TargetRef string // ref the restored cluster archives to (Source for in-place)
	Target    time.Time
	Manifest  Manifest
}

// PlanRestore picks the newest base backup of ref that finished at or before target
// (or backupID) and checks that the WAL it starts from is in the archive.
func (s *Service) PlanRestore(ctx context.Context, ref string, target time.Time, backupID string) (*RestorePlan, error) {
	all, err := s.ListBackups(ctx, ref)
	if err != nil {
		return nil, err
	}
	m, err := pickBackup(all, target, backupID)
	if err != nil {
		return nil, err
	}
	if _, err := s.opt.Store.Stat(ctx, walKey(ref, m.StartWAL)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("backup: WAL file %s that backup %s starts from is not in the archive", m.StartWAL, m.ID)
		}
		return nil, err
	}
	return &RestorePlan{Source: ref, TargetRef: ref, Target: target, Manifest: *m}, nil
}

// Restore implements Backup. With newRef set it creates a new project; with newRef
// empty it refuses, because restoring in place needs RestoreWith and Force.
func (s *Service) Restore(ctx context.Context, ref string, target time.Time, newRef string) (*registry.Project, error) {
	return s.RestoreWith(ctx, ref, target, newRef, RestoreOptions{})
}

// RestoreWith restores ref to target.
//
// New project (newRef set and different from ref): lifecycle.Manager.Create builds
// project newRef with a DataSeeder that unpacks the base backup instead of running
// initdb and writes the recovery settings. The restored cluster already contains the
// source's role passwords and Vault data, so the source's database passwords and
// pgsodium root key are reused. Everything an API client can hold is new: a fresh
// JWT secret, legacy keys signed for newRef, and new publishable and secret keys.
//
// In place (newRef empty or equal to ref, Force set): the project is paused, its
// data directory moved aside, the seeder fills a fresh one, and the project resumes.
func (s *Service) RestoreWith(ctx context.Context, ref string, target time.Time, newRef string, opts RestoreOptions) (*registry.Project, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	for _, c := range []struct {
		what string
		ok   bool
	}{{"registry", s.opt.Registry != nil}, {"lifecycle manager", s.opt.Manager != nil}, {"secrets", s.opt.Secrets != nil}, {"config", s.opt.Config != nil}} {
		if err := s.need(c.what, c.ok); err != nil {
			return nil, err
		}
	}
	inPlace := newRef == "" || newRef == ref
	if inPlace && !opts.Force {
		return nil, ErrForceRequired
	}
	if !inPlace {
		if !secrets.ValidRef(newRef) {
			return nil, fmt.Errorf("backup: invalid new project ref %q", newRef)
		}
		if _, err := s.opt.Registry.GetProject(ctx, newRef); err == nil {
			return nil, fmt.Errorf("backup: project %s already exists", newRef)
		} else if !errors.Is(err, registry.ErrNotFound) {
			return nil, err
		}
	}

	plan, err := s.PlanRestore(ctx, ref, target, opts.BackupID)
	if err != nil {
		return nil, err
	}
	keys, err := s.sourceKeys(ctx, &plan.Manifest)
	if err != nil && !inPlace {
		return nil, err
	}

	var p *registry.Project
	if inPlace {
		p, err = s.restoreInPlace(ctx, plan)
	} else {
		plan.TargetRef = newRef
		p, err = s.restoreAsNew(ctx, plan, keys)
	}
	if err != nil {
		_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.failed", map[string]any{"target": target, "as": newRef, "error": err.Error()})
		return nil, err
	}
	_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.completed", map[string]any{"target": target, "as": p.Ref, "backup": plan.Manifest.ID})
	return p, nil
}

func (s *Service) restoreAsNew(ctx context.Context, plan *RestorePlan, src *secrets.ProjectKeys) (*registry.Project, error) {
	k := *src
	k.JWTSecret = secrets.NewJWTSecret()
	k.PublishableKey = secrets.NewPublishableKey()
	k.SecretKey = secrets.NewSecretKey()
	if err := k.ResignLegacy(plan.TargetRef, s.opt.Now()); err != nil {
		return nil, err
	}
	req := lifecycle.CreateRequest{
		Name: "restore of " + plan.Source, Region: "local", Ref: plan.TargetRef,
		DBPassword: k.DBPassword, Seed: s.Seeder(plan), Keys: &k,
	}
	if mp := plan.Manifest.Project; mp != nil {
		req.Name = mp.Name + " (restored)"
		req.OrgSlug, req.Region, req.Class = mp.OrgSlug, mp.Region, mp.Class
		lim := mp.Limits
		req.Limits = &lim
	}
	p, err := s.opt.Manager.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("backup: create restored project %s: %w", plan.TargetRef, err)
	}
	s.resetRecoverySettings(ctx, plan.TargetRef)
	return p, nil
}

func (s *Service) restoreInPlace(ctx context.Context, plan *RestorePlan) (*registry.Project, error) {
	ref := plan.Source
	proj, err := s.opt.Registry.GetProject(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("backup: project %s: %w", ref, err)
	}
	if s.opt.DataDir == nil {
		return nil, errors.New("backup: no data directory resolver configured")
	}
	dataDir := s.opt.DataDir(ref)
	if err := s.opt.Manager.Pause(ctx, ref); err != nil {
		return nil, fmt.Errorf("backup: stop %s: %w", ref, err)
	}
	aside := dataDir + ".pre-restore-" + s.opt.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(dataDir, aside); err != nil {
		_ = s.opt.Manager.Resume(ctx, ref)
		return nil, fmt.Errorf("backup: move data directory aside: %w", err)
	}
	if err := s.Seeder(plan)(ctx, proj, dataDir); err != nil {
		os.RemoveAll(dataDir)
		rerr := os.Rename(aside, dataDir)
		if rerr == nil {
			rerr = s.opt.Manager.Resume(ctx, ref)
		}
		if rerr != nil {
			return nil, fmt.Errorf("backup: seed failed (%v) and the original could not be put back (%v); original data is in %s", err, rerr, aside)
		}
		return nil, fmt.Errorf("backup: seed data directory (original restored): %w", err)
	}
	if err := s.opt.Manager.Resume(ctx, ref); err != nil {
		return nil, fmt.Errorf("backup: start %s after restore (original data kept in %s): %w", ref, aside, err)
	}
	s.resetRecoverySettings(ctx, ref)
	s.opt.Log.Info("restored in place", "ref", ref, "original_data", aside)
	return s.opt.Registry.GetProject(ctx, ref)
}

// resetRecoverySettings removes the recovery settings the seeder put in
// postgresql.auto.conf, so a later start never replays another project's archive.
// Failure is logged and recorded as an event, not returned: the project is restored.
func (s *Service) resetRecoverySettings(ctx context.Context, ref string) {
	err := func() error {
		if s.opt.Access == nil {
			return errors.New("no database access configured")
		}
		dsn, err := s.opt.Access.ConnString(ctx, ref, roleAdmin)
		if err != nil {
			return err
		}
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return err
		}
		defer conn.Close(context.WithoutCancel(ctx))
		for _, g := range recoveryGUCs {
			if _, err := conn.Exec(ctx, "alter system reset "+g); err != nil {
				return err
			}
		}
		_, err = conn.Exec(ctx, "select pg_reload_conf()")
		return err
	}()
	if err != nil {
		s.opt.Log.Warn("could not clear recovery settings after restore", "ref", ref, "err", err)
		_ = s.opt.Registry.AppendEvent(ctx, ref, "restore.cleanup_failed", map[string]any{"error": err.Error()})
	}
}

var recoveryGUCs = []string{"restore_command", "recovery_target_time", "recovery_target_action", "recovery_target_timeline"}

// sourceKeys opens the credentials the restored cluster was running with: the sealed
// copy stored in the backup, or, for backups without one, the source project's
// current registry secrets.
func (s *Service) sourceKeys(ctx context.Context, m *Manifest) (*secrets.ProjectKeys, error) {
	sealed := map[string][]byte{}
	if m.Secrets != "" {
		rc, err := s.opt.Store.Get(ctx, m.Dir()+"/"+m.Secrets)
		if err != nil {
			return nil, fmt.Errorf("backup: read secrets of backup %s: %w", m.ID, err)
		}
		defer rc.Close()
		if err := json.NewDecoder(rc).Decode(&sealed); err != nil {
			return nil, fmt.Errorf("backup: secrets of backup %s: %w", m.ID, err)
		}
	} else if sealedReg, err := s.opt.Registry.GetSecrets(ctx, m.Ref); err == nil {
		sealed = sealedReg
	}
	plain := make(map[string]string, len(sealed))
	for name, blob := range sealed {
		pt, err := s.opt.Secrets.Open(blob)
		if err != nil {
			return nil, fmt.Errorf("backup: open secret %s (is this node's master key the one that sealed the backup?): %w", name, err)
		}
		plain[name] = string(pt)
	}
	k := secrets.KeysFromMap(plain)
	if k.DBPassword == "" || k.AdminPassword == "" || k.PGSodiumRootKey == "" {
		return nil, fmt.Errorf("backup: no database credentials available for %s; they are needed to open the restored cluster", m.Ref)
	}
	return k, nil
}

// Seeder returns the lifecycle.DataSeeder that turns an empty directory into the
// restored cluster: it unpacks the base backup, writes recovery.signal and the
// recovery settings, and points the cluster's own archiving at plan.TargetRef so a
// restored clone never writes into the source's archive.
func (s *Service) Seeder(plan *RestorePlan) lifecycle.DataSeeder {
	return func(ctx context.Context, _ *registry.Project, dataDir string) error {
		return s.seed(ctx, plan, dataDir)
	}
}

func (s *Service) seed(ctx context.Context, plan *RestorePlan, dataDir string) error {
	m := &plan.Manifest
	if ents, err := os.ReadDir(dataDir); err == nil && len(ents) > 0 {
		return fmt.Errorf("backup: refusing to restore into non-empty directory %s", dataDir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}
	rc, err := s.opt.Store.Get(ctx, m.Dir()+"/"+m.Data)
	if err != nil {
		return fmt.Errorf("backup: read base backup %s: %w", m.ID, err)
	}
	defer rc.Close()
	dec, err := newDecoder(rc)
	if err != nil {
		return err
	}
	defer dec.Close()
	if err := extractTar(ctx, tar.NewReader(dec), dataDir); err != nil {
		return fmt.Errorf("backup: unpack base backup %s: %w", m.ID, err)
	}

	// The Supabase launcher refuses an existing cluster without a non-empty
	// postmaster.opts, and pg_basebackup-style backups omit it. Postgres rewrites it.
	if err := writeSyncFile(filepath.Join(dataDir, "postmaster.opts"), []byte("# recreated by sbctl restore\n"), 0o600); err != nil {
		return err
	}
	conf, err := os.OpenFile(filepath.Join(dataDir, "postgresql.auto.conf"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := conf.WriteString(s.recoveryConf(plan)); err != nil {
		conf.Close()
		return err
	}
	if err := conf.Sync(); err != nil {
		conf.Close()
		return err
	}
	if err := conf.Close(); err != nil {
		return err
	}
	// recovery.signal last: it is what turns the start into a recovery.
	if err := writeSyncFile(filepath.Join(dataDir, "recovery.signal"), nil, 0o600); err != nil {
		return err
	}
	if err := chownLikeParent(dataDir); err != nil {
		return err
	}
	return syncTree(dataDir)
}

// recoveryConf is the block appended to postgresql.auto.conf. Later lines win, so it
// overrides anything the source's own ALTER SYSTEM history left there.
func (s *Service) recoveryConf(plan *RestorePlan) string {
	bin, cfgPath := config.DefaultBinPath, s.opt.ConfigPath
	c := s.opt.Config
	if c != nil && c.BinPath != "" {
		bin = c.BinPath
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n# --- sbctl restore of %s (backup %s) to %s ---\n", plan.Source, plan.Manifest.ID, plan.Target.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "archive_mode = on\narchive_command = %s\n", confString(ArchiveCommand(bin, plan.TargetRef, cfgPath)))
	fmt.Fprintf(&b, "restore_command = %s\n", confString(RestoreCommand(bin, plan.Source, cfgPath)))
	fmt.Fprintf(&b, "recovery_target_time = %s\n", confString(plan.Target.UTC().Format("2006-01-02 15:04:05.999999")+"+00"))
	b.WriteString("recovery_target_action = 'promote'\nrecovery_target_timeline = 'latest'\n")
	return b.String()
}

func writeSyncFile(p string, b []byte, perm os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// extractTar unpacks tr under root. Only directories and regular files are accepted,
// and every path must stay inside root. Files are fsynced as they are written.
func extractTar(ctx context.Context, tr *tar.Reader, root string) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q in archive", hdr.Name)
		}
		dst := filepath.Join(root, filepath.FromSlash(name))
		perm := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
			if err := os.Chmod(dst, perm); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, ctxReader{ctx, tr})
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		default:
			return fmt.Errorf("unsupported entry type %q for %s", hdr.Typeflag, name)
		}
	}
}

// chownLikeParent gives the tree under root the owner of root's parent directory when
// sbctl runs as root. An administrator who runs `sudo sbctl backups restore` must not
// leave root-owned files behind: Postgres runs as the sbctl user and refuses a data
// directory it does not own. As any other user it does nothing (files are already ours).
func chownLikeParent(root string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(filepath.Dir(root))
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, int(st.Uid), int(st.Gid))
	})
}

// syncTree fsyncs every directory under root, deepest first, so the new tree is durable.
func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return syncDir(filepath.Dir(root))
}
