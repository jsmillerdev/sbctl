package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/health"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/nodeupgrade"
	"github.com/jsmillerdev/supavise/internal/notice"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/selfupdate"
	"github.com/jsmillerdev/supavise/internal/units"
)

// nodeHost is nodeupgrade.Host for a Linux node. `supavise upgrade` runs as root, because the
// binary and the unit files are root's, and runs everything that touches the node's data as the
// supavise user through runuser, the way the installer does; what root itself reads (the
// registry, the unit scripts, the disk) it only reads.
type nodeHost struct {
	cfg  *config.Config
	out  io.Writer
	errw io.Writer
	log  *slog.Logger
	in   io.Reader

	exe     string // the running binary (the driver)
	binPath string // the installed binary
	cfgPath string
	root    bool
	wait    time.Duration

	rel      nodeupgrade.Releases
	selfOpts selfupdate.Options

	// state of one run
	from, to  string
	started   time.Time
	swappedAt time.Time
	stageDir  string
	tmpStage  bool
}

func newNodeHost(cmd cobraIO, cfg *config.Config, wait time.Duration, so selfupdate.Options) (*nodeHost, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	h := &nodeHost{cfg: cfg, out: cmd.Out, errw: cmd.Err, in: cmd.In, log: newLogger(cfg), exe: exe, binPath: cfg.BinPath,
		cfgPath: effectiveConfigPath(), root: os.Geteuid() == 0, wait: wait, selfOpts: so}
	if h.cfgPath == "" {
		h.cfgPath = selfUpdateConfigPath()
	}
	h.rel = nodeupgrade.Releases{Dir: releasesDir(cfg.BinPath)}
	return h, nil
}

// cobraIO carries the streams of a command.
type cobraIO struct {
	Out, Err io.Writer
	In       io.Reader
}

// releasesDir is where the kept releases live: <prefix>/lib/supavise/releases for a binary in
// <prefix>/bin, which is root's for the usual /usr/local/bin and out of the supavise user's reach.
func releasesDir(binPath string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(binPath)), "lib", "supavise", "releases")
}

// asSupavise runs bin with args as the supavise user (directly when this process already is).
func (h *nodeHost) asSupavise(ctx context.Context, stdout io.Writer, env []string, bin string, args ...string) error {
	full := append([]string{bin, "--config", h.cfgPath}, args...)
	var c *exec.Cmd
	if h.root {
		pre := append([]string{"-u", installUser, "--", "env", "HOME=" + h.cfg.StateDir}, env...)
		c = exec.CommandContext(ctx, "runuser", append(pre, full...)...)
	} else {
		c = exec.CommandContext(ctx, full[0], full[1:]...)
		c.Env = append(os.Environ(), env...)
	}
	if stdout == nil {
		stdout = h.out
	}
	c.Stdout, c.Stderr = stdout, h.errw
	// A cancelled upgrade asks the worker to stop; a project upgrade mid-way settles itself.
	c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
	c.WaitDelay = 2 * time.Minute
	if err := c.Run(); err != nil {
		return fmt.Errorf("supavise %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// version of the installed binary: the last word of `supavise --version`.
func binaryVersion(ctx context.Context, path string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", path, err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", fmt.Errorf("%s --version printed nothing", path)
	}
	return f[len(f)-1], nil
}

// Inspect implements nodeupgrade.Host.
func (h *nodeHost) Inspect(ctx context.Context) (*nodeupgrade.Node, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("supavise upgrade works on a Linux server install")
	}
	n := &nodeupgrade.Node{Platform: "linux-" + runtime.GOARCH}
	ver, err := binaryVersion(ctx, h.binPath)
	if err != nil {
		return nil, fmt.Errorf("no installed supavise at %s: %w", h.binPath, err)
	}
	n.Version, h.from = ver, ver
	if info, err := nodeupgrade.ProbeInfo(ctx, h.binPath); err == nil {
		n.BinaryInfo = info
	}
	if u := notice.ReadUpgrade(h.cfg.Paths()); u != nil && u.Running(time.Now()) && u.PID != os.Getpid() && (u.PID == 0 || pidAlive(u.PID)) {
		n.Running = &nodeupgrade.Running{PID: u.PID, Phase: u.Phase, To: u.To}
	}

	dsn := lifecycle.SystemSocketDSN(h.cfg, "supavise")
	if n.AppliedSchema, err = registry.AppliedSchema(ctx, dsn); err != nil {
		return nil, fmt.Errorf("cannot reach the registry (is supavise-postgres@system running? run as root or as the supavise user): %w", err)
	}
	reg, err := registry.OpenExisting(ctx, dsn+" pool_max_conns=2")
	if err != nil {
		return nil, fmt.Errorf("cannot reach the registry: %w", err)
	}
	defer reg.Close()
	rows, err := reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var listErr error
	n.Projects = nodeupgrade.ProjectsOf(rows, func(ref string) time.Time {
		bs, err := reg.ListBackups(ctx, ref)
		if err != nil {
			listErr = err
			return time.Time{}
		}
		for _, b := range bs { // newest first
			if b.Status == registry.BackupCompleted && b.FinishedAt != nil {
				return *b.FinishedAt
			}
		}
		return time.Time{}
	})
	if listErr != nil {
		return nil, listErr
	}
	h.nodePins(n)

	n.Verdict, n.Summary = h.status(ctx)
	if e, err := health.EscrowCheck(h.cfg, time.Minute, false)(ctx); err == nil && e != nil {
		n.Escrow = nodeupgrade.Escrow{Known: true, Covered: e.Covered, Detail: e.Detail}
	}
	n.DiskPath = h.cfg.StateDir
	n.DiskFree = freeBytes(h.cfg.StateDir)
	n.LocalBackups = strings.HasPrefix(h.cfg.Backup.Backend, "file://")
	if n.LocalBackups {
		for i := range n.Projects {
			n.Projects[i].DiskBytes = dirBytes(h.cfg.Paths().Project(n.Projects[i].Ref))
		}
	}
	return n, nil
}

// nodePins fills n.Pins: the release each of the node's own units is set to run, from the scripts
// the daemon rendered; for what no unit shows (PostgREST, a service not rendered) the installed
// binary's pins, else the release most projects run.
func (h *nodeHost) nodePins(n *nodeupgrade.Node) {
	n.Pins = map[string]string{}
	svcs := append([]string{config.SvcPostgres, config.SvcGoTrue}, fleet.ServicesFor(h.cfg)...)
	for _, svc := range svcs {
		if tag, err := fleet.RenderedTag(h.cfg, svc); err == nil {
			n.Pins[svc] = tag
		}
	}
	if n.BinaryInfo != nil {
		for svc, tag := range n.BinaryInfo.Pins {
			if n.Pins[svc] == "" {
				n.Pins[svc] = tag
			}
		}
	}
	for _, svc := range config.ProjectServices {
		if n.Pins[svc] != "" {
			continue
		}
		count := map[string]int{}
		for _, p := range n.UserProjects() {
			if t := p.Versions[svc]; t != "" {
				count[t]++
			}
		}
		best := ""
		for t, c := range count {
			if c > count[best] || (c == count[best] && t > best) {
				best = t
			}
		}
		n.Pins[svc] = best
	}
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func freeBytes(path string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize)
}

func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// status runs `supavise status --json` as the supavise user on the installed binary and returns the
// verdict and the first line of the summary.
func (h *nodeHost) status(ctx context.Context) (verdict, summary string) {
	var buf bytes.Buffer
	// Exit status 1 and 2 are verdicts; the JSON is on stdout either way.
	_ = h.asSupavise(ctx, &buf, nil, h.binPath, "status", "--json")
	var rep struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil || rep.Status == "" {
		return nodeupgrade.VerdictUnknown, "status printed no verdict"
	}
	return rep.Status, rep.Summary
}

// Status implements nodeupgrade.Host.
func (h *nodeHost) Status(ctx context.Context) (string, string, error) {
	v, s := h.status(ctx)
	if v == nodeupgrade.VerdictUnknown {
		return v, s, errors.New(s)
	}
	return v, s, nil
}

// resolved is what Resolve hands to Stage.
type resolved struct {
	ver  *selfupdate.Verified
	opts selfupdate.Options
}

// Resolve implements nodeupgrade.Host.
func (h *nodeHost) Resolve(ctx context.Context, tag string, n *nodeupgrade.Node) (*nodeupgrade.Candidate, error) {
	o := h.selfOpts
	o.Tag, o.Current, o.Platform, o.ExecPath = tag, n.Version, n.Platform, h.binPath
	ver, err := selfupdate.Fetch(ctx, o)
	if err != nil {
		return nil, err
	}
	// The release states the oldest version it upgrades from; a jump over a release it needs is
	// refused before anything else is downloaded. Going back is not an upgrade and is not held to it.
	if selfupdate.Newer(ver.Release.Tag, n.Version) {
		if err := ver.Manifest.CheckUpgradeFrom(n.Version); err != nil {
			return nil, err
		}
	}
	h.to = ver.Release.Tag
	return &nodeupgrade.Candidate{Tag: ver.Release.Tag, Data: &resolved{ver: ver, opts: o}}, nil
}

// staged is what Stage hands to the other methods.
type staged struct {
	st     *selfupdate.Staged
	studio struct{ name, url, sha string }
}

// Stage implements nodeupgrade.Host.
func (h *nodeHost) Stage(ctx context.Context, c *nodeupgrade.Candidate) (*nodeupgrade.Staged, error) {
	r := c.Data.(*resolved)
	o := r.opts
	o.Out = h.out
	// Next to the installed binary, so that installing it is one rename. Without root the plan
	// still downloads and runs the binary to read its pins, in a directory of the caller's own.
	o.StageDir = filepath.Dir(h.binPath)
	if !h.root {
		d, err := os.MkdirTemp("", "supavise-upgrade-")
		if err != nil {
			return nil, err
		}
		o.StageDir, h.stageDir, h.tmpStage = d, d, true
	}
	st, err := r.ver.Stage(ctx, o)
	if err != nil {
		return nil, err
	}
	info, err := nodeupgrade.ProbeInfo(ctx, st.Path)
	if err != nil {
		st.Discard()
		return nil, fmt.Errorf("the release binary cannot describe itself: %w", err)
	}
	if info.Version != c.Tag {
		st.Discard()
		return nil, fmt.Errorf("release %s ships a binary that describes itself as %s", c.Tag, info.Version)
	}
	// The signed manifest lists the pins of the release as the release tool read them from
	// versions.yaml; the binary reports the ones built into it. They are the same file, so a
	// difference means the binary is not the one the manifest describes.
	if err := nodeupgrade.CheckManifestPins(info, r.ver.Manifest.Artifacts, r.ver.Manifest.Studio); err != nil {
		st.Discard()
		return nil, err
	}
	s := &staged{st: st}
	s.studio.name, s.studio.url, s.studio.sha, _ = r.ver.Studio(o.Platform)
	return &nodeupgrade.Staged{Info: info, Data: s}, nil
}

// Discard implements nodeupgrade.Host.
func (h *nodeHost) Discard(s *nodeupgrade.Staged) {
	if sd, ok := s.Data.(*staged); ok && sd != nil {
		sd.st.Discard()
	}
	if h.tmpStage {
		_ = os.RemoveAll(h.stageDir)
	}
}

// Prefetch implements nodeupgrade.Host: the staged binary fetches the artifacts its release pins
// that the node does not have, as the supavise user. It reads no registry and so migrates nothing.
func (h *nodeHost) Prefetch(ctx context.Context, s *nodeupgrade.Staged, n *nodeupgrade.Node, p *nodeupgrade.Plan) error {
	bin, env := h.binPath, []string(nil)
	studio := false
	if sd, ok := s.Data.(*staged); ok && sd != nil {
		bin = sd.st.Path
		if _, moves := moved(p.Shared, config.SvcStudio); moves && sd.studio.url != "" {
			studio = true
			env = []string{"SUPAVISE_STUDIO_ARTIFACT_URL=" + sd.studio.url, "SUPAVISE_STUDIO_ARTIFACT_SHA256=" + sd.studio.sha}
		}
	}
	args := []string{"artifacts", "fetch"}
	for _, svc := range artifactServices(p) {
		args = append(args, svc)
	}
	if studio {
		args = append(args, "--studio")
	}
	if len(args) == 2 {
		return nil
	}
	return h.asSupavise(ctx, h.out, env, bin, args...)
}

func moved(ms []nodeupgrade.ServiceMove, svc string) (nodeupgrade.ServiceMove, bool) {
	for _, m := range ms {
		if m.Service == svc {
			return m, true
		}
	}
	return nodeupgrade.ServiceMove{}, false
}

// artifactServices are the services whose artifact the upgrade needs on disk before anything
// stops: the shared services and the system project that move, and what the projects move to.
func artifactServices(p *nodeupgrade.Plan) []string {
	set := map[string]bool{}
	for _, m := range append(append([]nodeupgrade.ServiceMove{}, p.System...), p.Shared...) {
		if m.Service != config.SvcStudio {
			set[m.Service] = true
		}
	}
	if len(p.Upgrade) > 0 {
		for svc := range p.ProjectTarget {
			set[svc] = true
		}
	}
	var out []string
	for svc := range set {
		out = append(out, svc)
	}
	sort.Strings(out)
	return out
}

// Backup implements nodeupgrade.Host: one `backups create` per ref on the installed binary, a few at
// a time. The first failure stops the rest from starting and fails the upgrade before anything
// changed.
func (h *nodeHost) Backup(ctx context.Context, refs []string, parallel int) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(parallel, 1))
	var mu sync.Mutex
	lw := &prefixWriter{w: h.out, mu: &mu}
	for _, ref := range refs {
		g.Go(func() error {
			if err := h.asSupavise(gctx, lw.with(ref+": "), nil, h.binPath, "backups", "create", ref, "--reason", backup.ReasonUpgrade); err != nil {
				return fmt.Errorf("%s: %w", ref, err)
			}
			h.Mark(nodeupgrade.PhasePreparing, "backing up")
			return nil
		})
	}
	return g.Wait()
}

// prefixWriter puts a prefix before each line a worker prints, so that parallel workers stay readable.
type prefixWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

type prefixed struct {
	p      *prefixWriter
	prefix string
	buf    []byte
}

func (p *prefixWriter) with(prefix string) io.Writer { return &prefixed{p: p, prefix: prefix} }

func (w *prefixed) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.p.mu.Lock()
		fmt.Fprintf(w.p.w, "%s%s\n", w.prefix, w.buf[:i])
		w.p.mu.Unlock()
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

// Mark implements nodeupgrade.Host.
func (h *nodeHost) Mark(phase, detail string) {
	if h.started.IsZero() {
		h.started = time.Now().UTC()
	}
	u := notice.Upgrade{Phase: phase, From: h.from, To: h.to, StartedAt: h.started, PID: os.Getpid(), Detail: detail}
	if err := notice.WriteUpgrade(h.cfg.Paths(), u); err != nil {
		h.log.Warn("could not write the upgrade marker", "error", err)
		return
	}
	handOverToStateOwner(h.cfg, filepath.Join(h.cfg.Paths().Root, "system", "upgrade.json"))
}

// handOverToStateOwner gives a file root wrote in the state directory to the directory's owner.
func handOverToStateOwner(cfg *config.Config, path string) {
	if os.Geteuid() != 0 {
		return
	}
	if fi, err := os.Stat(cfg.StateDir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(path, int(st.Uid), int(st.Gid))
		}
	}
}

// Install implements nodeupgrade.Host.
func (h *nodeHost) Install(ctx context.Context, s *nodeupgrade.Staged, prev, next nodeupgrade.Record) (bool, error) {
	sd := s.Data.(*staged)
	if !h.root {
		return false, errors.New("run as root: sudo supavise upgrade")
	}
	if _, err := h.rel.Keep(prev, h.binPath); err != nil {
		return false, fmt.Errorf("keeping the running release: %w", err)
	}
	if _, err := h.rel.Keep(next, sd.st.Path); err != nil {
		return false, fmt.Errorf("keeping the new release: %w", err)
	}
	if _, err := sd.st.Install(); err != nil {
		return false, err
	}
	h.swappedAt = time.Now()
	return true, h.activate(ctx)
}

// activate renders the units with the binary at BinPath, restarts the daemon and waits until it
// answers.
func (h *nodeHost) activate(ctx context.Context) error {
	c := exec.CommandContext(ctx, h.binPath, "system", "install-units")
	c.Stdout, c.Stderr = h.out, h.errw
	if err := c.Run(); err != nil {
		fmt.Fprintf(h.errw, "warning: supavise system install-units failed: %v\n", err)
	}
	_ = exec.CommandContext(ctx, "systemctl", "reset-failed", "supavise.service").Run()
	if err := restartAndWait(ctx, h.cfg, h.wait); err != nil {
		return err
	}
	h.swappedAt = time.Now()
	return nil
}

// Restore implements nodeupgrade.Host.
func (h *nodeHost) Restore(ctx context.Context, rec nodeupgrade.Record) error {
	if !h.root {
		return errors.New("run as root: sudo supavise rollback")
	}
	if err := h.rel.Verify(&rec); err != nil {
		return err
	}
	src, err := h.rel.BinaryPath(rec.Version)
	if err != nil {
		return err
	}
	if err := replaceFile(src, h.binPath); err != nil {
		return fmt.Errorf("installing the kept %s: %w", rec.Version, err)
	}
	if err := h.activate(ctx); err != nil {
		return err
	}
	return h.rel.Touch(rec.Version, time.Now())
}

// replaceFile copies src next to dst and renames it over dst, so a reader sees one or the other.
func replaceFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// WaitShared implements nodeupgrade.Host: for each service in order, wait until its unit is set to
// run the new release, has restarted since the binary was swapped, and answers.
func (h *nodeHost) WaitShared(ctx context.Context, moves []nodeupgrade.ServiceMove) error {
	if len(moves) == 0 {
		return nil
	}
	sup, err := units.New(h.cfg, h.log)
	if err != nil {
		return err
	}
	mgr, err := fleet.NewManager(fleet.Deps{Cfg: h.cfg, Log: h.log, Supervisor: sup})
	if err != nil {
		return err
	}
	for _, m := range moves {
		if err := h.waitService(ctx, sup, mgr, m); err != nil {
			return fmt.Errorf("%s: %w", m.Service, err)
		}
		fmt.Fprintf(h.out, "  %s runs %s\n", m.Service, short(m.Service, m.To))
		h.Mark(h.currentPhase(), m.Service)
	}
	return nil
}

func short(svc, tag string) string { return lifecycle.ShortVersion(svc, tag) }

func (h *nodeHost) currentPhase() string {
	if u := notice.ReadUpgrade(h.cfg.Paths()); u != nil && u.Phase != "" {
		return u.Phase
	}
	return nodeupgrade.PhaseServices
}

// serviceWait bounds the wait for one service: Realtime and Supavisor run migrations first.
const serviceWait = 6 * time.Minute

func (h *nodeHost) waitService(ctx context.Context, sup units.Supervisor, mgr *fleet.Manager, m nodeupgrade.ServiceMove) error {
	unit := config.UnitName(m.Service, "")
	system := m.Service == config.SvcPostgres || m.Service == config.SvcGoTrue
	if system {
		unit = config.UnitName(m.Service, config.SystemRef)
	}
	deadline := time.Now().Add(serviceWait)
	var last string
	for {
		tag, terr := fleet.RenderedTag(h.cfg, m.Service)
		st, serr := sup.Status(ctx, unit)
		switch {
		case terr != nil:
			last = terr.Error()
		case tag != m.To:
			last = fmt.Sprintf("set to run %s, want %s", tag, m.To)
		case serr != nil:
			last = serr.Error()
		case st.State == units.StateFailed:
			return fmt.Errorf("%s failed on %s", unit, short(m.Service, m.To))
		case st.State != units.StateActive:
			last = fmt.Sprintf("%s is %s/%s", unit, st.State, st.SubState)
		case st.Since.Before(h.swappedAt):
			last = "still the process that ran before the swap"
		case system:
			return nil
		default:
			healthy := false
			for _, hh := range mgr.Status(ctx) {
				if hh.Service == m.Service {
					healthy = hh.Healthy
					last = hh.Error
				}
			}
			if healthy {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not on %s and healthy after %s: %s", short(m.Service, m.To), serviceWait, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// toArgs turns release tags per service into repeated --to flags.
func toArgs(target map[string]string) []string {
	svcs := make([]string, 0, len(target))
	for svc := range target {
		svcs = append(svcs, svc)
	}
	sort.Strings(svcs)
	var out []string
	for _, svc := range svcs {
		out = append(out, "--to", svc+"="+target[svc])
	}
	return out
}

// upgradeProjectsArgs is the command line of the rollout: the new binary upgrades every
// eligible project to the named releases, canary first, and stops at the first failure.
func upgradeProjectsArgs(target map[string]string) []string {
	return append([]string{"projects", "upgrade", "--all", "--yes", "--no-gc"}, toArgs(target)...)
}

// UpgradeProjects implements nodeupgrade.Host.
func (h *nodeHost) UpgradeProjects(ctx context.Context, target map[string]string, since time.Time) ([]nodeupgrade.ProjectMove, error) {
	err := h.asSupavise(ctx, h.out, nil, h.binPath, upgradeProjectsArgs(target)...)
	moves, merr := h.MovesSince(context.WithoutCancel(ctx), since)
	if merr != nil {
		h.log.Warn("could not read which projects were upgraded", "error", merr)
	}
	return moves, err
}

// MovesSince implements nodeupgrade.Host.
func (h *nodeHost) MovesSince(ctx context.Context, since time.Time) ([]nodeupgrade.ProjectMove, error) {
	reg, err := registry.OpenExisting(ctx, lifecycle.SystemSocketDSN(h.cfg, "supavise")+" pool_max_conns=2")
	if err != nil {
		return nil, err
	}
	defer reg.Close()
	rows, err := reg.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []nodeupgrade.ProjectMove
	for _, p := range rows {
		if p.Ref == config.SystemRef {
			continue
		}
		u, err := reg.LatestUpgrade(ctx, p.Ref)
		if errors.Is(err, registry.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if u.Status == registry.UpgradeDone && !u.InitiatedAt.Before(since) {
			out = append(out, nodeupgrade.ProjectMove{Ref: p.Ref, From: u.From, To: u.To, At: u.InitiatedAt})
		}
	}
	return out, nil
}

// RevertProjects implements nodeupgrade.Host.
func (h *nodeHost) RevertProjects(ctx context.Context, moves []nodeupgrade.ProjectMove) error {
	var mu sync.Mutex
	lw := &prefixWriter{w: h.out, mu: &mu}
	var errs []error
	for _, m := range moves {
		args := append([]string{"projects", "upgrade", m.Ref, "--yes", "--no-gc", "--allow-older"}, toArgs(m.RevertTargets())...)
		if err := h.asSupavise(ctx, lw.with(m.Ref+": "), nil, h.binPath, args...); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Ref, err))
		}
	}
	return errors.Join(errs...)
}

// AppliedSchema implements nodeupgrade.Host.
func (h *nodeHost) AppliedSchema(ctx context.Context) (string, error) {
	return registry.AppliedSchema(ctx, lifecycle.SystemSocketDSN(h.cfg, "supavise"))
}

// PreviousRelease implements nodeupgrade.Host.
func (h *nodeHost) PreviousRelease(_ context.Context, current string) (prev, cur *nodeupgrade.Record, err error) {
	if prev, err = h.rel.Previous(current); err != nil {
		return nil, nil, err
	}
	if prev != nil {
		if err := h.rel.Verify(prev); err != nil {
			return nil, nil, err
		}
		h.to = prev.Version
	}
	cur, _ = h.rel.Get(current)
	return prev, cur, nil
}

// Cleanup implements nodeupgrade.Host: the artifacts no kept release or project needs, and the
// kept binaries beyond keep_releases.
func (h *nodeHost) Cleanup(ctx context.Context, keep int, current string) {
	if removed, err := h.rel.GC(keep, current); err != nil {
		fmt.Fprintf(h.errw, "warning: removing old kept releases: %v\n", err)
	} else if len(removed) > 0 {
		fmt.Fprintf(h.out, "removed the kept release(s) %s\n", strings.Join(removed, ", "))
	}
	if err := h.asSupavise(ctx, h.out, nil, h.binPath, "artifacts", "gc", "--keep", fmt.Sprint(keep)); err != nil {
		fmt.Fprintf(h.errw, "warning: removing unused artifacts: %v\n", err)
	}
}

// Confirm implements nodeupgrade.Host.
func (h *nodeHost) Confirm(question string) (bool, error) {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("nothing was changed; run it again with --yes to go ahead without being asked")
	}
	fmt.Fprintf(h.out, "%s [y/N] ", question)
	var line string
	fmt.Fscanln(h.in, &line)
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}
