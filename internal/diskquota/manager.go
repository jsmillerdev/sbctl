package diskquota

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/units"
)

// ErrNotEnforceable is returned by Manager.Set when the data volume cannot enforce a per-project
// size: it is not XFS mounted with prjquota.
var ErrNotEnforceable = errors.New("diskquota: the data volume is not XFS mounted with prjquota, so a project's disk size cannot be enforced")

// Unit is the root one-shot unit that sets a project's quota: supavise-diskquota@<ref>.service.
func Unit(ref string) string { return "supavise-diskquota@" + ref + ".service" }

// Manager reads and sets the per-project disk limits of one node.
type Manager struct {
	Cfg *config.Config
	// Sup starts the root unit (units.Supervisor). Nil: Set works only when the process itself is root.
	Sup units.Supervisor
	// Run, Euid and InspectFn are replaced by tests.
	Run       Runner
	Euid      func() int
	InspectFn func(dir string) (Volume, error)
}

// New returns a Manager for cfg's state directory.
func New(cfg *config.Config, sup units.Supervisor) *Manager { return &Manager{Cfg: cfg, Sup: sup} }

func (m *Manager) inspect(dir string) (Volume, error) {
	if m.InspectFn != nil {
		return m.InspectFn(dir)
	}
	return Inspect(dir)
}

// Volume describes the filesystem that holds the projects.
func (m *Manager) Volume() (Volume, error) { return m.inspect(m.Cfg.StateDir + "/projects") }

// Used is the bytes under the project's directory.
func (m *Manager) Used(ref string) int64 { return Used(m.Cfg.Paths().Project(ref)) }

// Limit is the limit asked for the project in GB (ok false: none was set).
func (m *Manager) Limit(ref string) (gb int, ok bool) {
	s, ok, err := ReadSetting(SettingFile(m.Cfg.Paths().Project(ref)))
	if err != nil || !ok {
		return 0, false
	}
	return s.SizeGB, true
}

// Set limits the project's disk to gb GB and returns once the limit is in force. seq is the
// project's registry sequence number (it makes the XFS project id).
func (m *Manager) Set(ctx context.Context, ref string, seq, gb int) error {
	vol, err := m.Volume()
	if err != nil {
		return err
	}
	if !vol.Enforceable() {
		return ErrNotEnforceable
	}
	dir := m.Cfg.Paths().Project(ref)
	s := Setting{ProjectID: ProjectID(seq), SizeGB: gb}
	if err := WriteSetting(SettingFile(dir), s); err != nil {
		return err
	}
	euid := os.Geteuid
	if m.Euid != nil {
		euid = m.Euid
	}
	if euid() == 0 {
		run := m.Run
		if run == nil {
			run = Exec
		}
		return Apply(ctx, run, vol.Mount, dir, s)
	}
	if m.Sup == nil {
		return fmt.Errorf("diskquota: cannot set a quota as an unprivileged process without a supervisor")
	}
	return m.Sup.Start(ctx, Unit(ref))
}

// ApplyStored is what the root unit runs: it reads the project's Setting and applies it.
func (m *Manager) ApplyStored(ctx context.Context, ref string) error {
	dir := m.Cfg.Paths().Project(ref)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("diskquota: %s is not a project directory", dir)
	}
	// The directory must be the project's own, not a link to somewhere else: root is about to mark
	// the whole tree under it. (The state directory itself may sit behind a link.)
	root, rerr := filepath.EvalSymlinks(filepath.Join(m.Cfg.StateDir, "projects"))
	real, err := filepath.EvalSymlinks(dir)
	if rerr != nil || err != nil || real != filepath.Join(root, ref) {
		return fmt.Errorf("diskquota: %s is not the project directory (a symbolic link?)", dir)
	}
	s, ok, err := ReadSetting(SettingFile(dir))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("diskquota: project %s has no disk limit to apply", ref)
	}
	vol, err := m.inspect(dir)
	if err != nil {
		return err
	}
	if !vol.Enforceable() {
		return ErrNotEnforceable
	}
	run := m.Run
	if run == nil {
		run = Exec
	}
	return Apply(ctx, run, vol.Mount, dir, s)
}
