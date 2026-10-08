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
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jsmillerdev/supavise/internal/backup"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/fleet"
	"github.com/jsmillerdev/supavise/internal/lifecycle"
	"github.com/jsmillerdev/supavise/internal/nodeupgrade"
	"github.com/jsmillerdev/supavise/internal/notice"
	"github.com/jsmillerdev/supavise/internal/registry"
	"github.com/jsmillerdev/supavise/internal/selfupdate"
	"github.com/jsmillerdev/supavise/internal/units"
)

// nodeHost is nodeupgrade.Host for a Linux node. `supavise upgrade` runs as root, because the
// binary and the unit files are root's, and runs everything that touches the node's data as the
// supavise user (asSupavise); what root itself reads (the registry, the unit scripts, the disk) it
// only reads. The one binary root runs is the installed one at config.DefaultBinPath, checked by
// installedBinary, never a path from the config.
type nodeHost struct {
	cfg  *config.Config
	out  io.Writer
	errw io.Writer
	log  *slog.Logger
	in   io.Reader

	binPath string // the installed binary
	cfgPath string
	root    bool
	wait    time.Duration

	rel      nodeupgrade.Releases
	selfOpts selfupdate.Options

	// state of one run
	mu          sync.Mutex
	last        notice.Upgrade
	hbStop      chan struct{}
	knowsReason bool
	// halted is the project the last rollout stopped at (HaltedProject).
	halted string
	from, to    string
	started     time.Time
	swappedAt   time.Time
	stageDir    string
	tmpStage    bool
	// tagsAtSwap are the releases the node's units were set to run just before the binary was
	// swapped (by Install or by Restore), by service.
	tagsAtSwap map[string]string
}

func newNodeHost(cmd cobraIO, cfg *config.Config, wait time.Duration, so selfupdate.Options) (*nodeHost, error) {
	// Root runs, copies and replaces this file, so its path is not taken from the config (the
	// supavise user writes that) and nothing is executed before it has passed the check.
	binPath, err := installedBinary(cfg)
	if err != nil {
		return nil, err
	}
	h := &nodeHost{cfg: cfg, out: cmd.Out, errw: cmd.Err, in: cmd.In, log: newLogger(cfg), binPath: binPath,
		cfgPath: effectiveConfigPath(), root: os.Geteuid() == 0, wait: wait, selfOpts: so}
	if h.cfgPath == "" {
		h.cfgPath = selfUpdateConfigPath()
	}
	h.rel = nodeupgrade.Releases{Dir: releasesDir(binPath)}
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
// Root drops to that user in the child itself, not through runuser: runuser opens a PAM session
// and keeps a parent that, on SIGTERM, sends the worker SIGKILL two seconds later, which would cut
// a project upgrade off between its steps.
func (h *nodeHost) asSupavise(ctx context.Context, stdout io.Writer, env []string, bin string, args ...string) error {
	return h.asSupaviseTo(ctx, stdout, h.errw, env, bin, args...)
}

// asSupaviseTo is asSupavise with the worker's stderr sent to stderr.
func (h *nodeHost) asSupaviseTo(ctx context.Context, stdout, stderr io.Writer, env []string, bin string, args ...string) error {
	full := append([]string{bin, "--config", h.cfgPath}, args...)
	c := exec.CommandContext(ctx, full[0], full[1:]...)
	c.Env = append(os.Environ(), env...)
	if h.root {
		cred, err := h.superviseCredential()
		if err != nil {
			return err
		}
		c.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		c.Env = append(os.Environ(), append([]string{"HOME=" + h.cfg.StateDir, "USER=" + installUser, "LOGNAME=" + installUser}, env...)...)
	}
	if stdout == nil {
		stdout = h.out
	}
	c.Stdout, c.Stderr = stdout, stderr
	// A cancelled upgrade asks the worker to stop. It gets the signal itself: a project upgrade
	// that is cut off rolls its project back and settles it, and is given two minutes to do that.
	c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
	c.WaitDelay = 2 * time.Minute
	if err := c.Run(); err != nil {
		return fmt.Errorf("supavise %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// superviseCredential is the uid, gid and groups of the supavise user.
func (h *nodeHost) superviseCredential() (*syscall.Credential, error) {
	u, err := user.Lookup(installUser)
	if err != nil {
		return nil, fmt.Errorf("the %s user: %w", installUser, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("the %s user's uid %q: %w", installUser, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("the %s user's gid %q: %w", installUser, u.Gid, err)
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("the groups of the %s user: %w", installUser, err)
	}
	groups := make([]uint32, 0, len(ids))
	for _, g := range ids {
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("the %s user's group %q: %w", installUser, g, err)
		}
		groups = append(groups, uint32(n))
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, nil
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

// InspectVersion implements nodeupgrade.VersionInspector: what `upgrade --check` needs, which is
// the installed version and what the binary says about itself.
func (h *nodeHost) InspectVersion(ctx context.Context) (*nodeupgrade.Node, error) {
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
	return n, nil
}

// Inspect implements nodeupgrade.Host.
func (h *nodeHost) Inspect(ctx context.Context) (*nodeupgrade.Node, error) {
	n, err := h.InspectVersion(ctx)
	if err != nil {
		return nil, err
	}
	// A binary that reports its release is one that knows the reason "pre-upgrade" for a backup.
	h.knowsReason = n.BinaryInfo != nil
	if u := notice.ReadUpgrade(h.cfg.Paths()); u != nil && u.Running(time.Now()) && u.PID != os.Getpid() && (u.PID == 0 || pidAlive(u.PID)) {
		n.Running = &nodeupgrade.Running{PID: u.PID, Phase: u.Phase, To: u.To}
	}

	dsn := lifecycle.SystemSocketDSN(h.cfg, "supavise")
	if n.AppliedMigrations, err = registry.AppliedMigrations(ctx, dsn); err != nil {
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
	for i := range n.Projects {
		n.Projects[i].HeldRestart = lifecycle.HeldRestart(h.cfg, n.Projects[i].Ref)
	}
	h.nodePins(n)

	n.Verdict, n.Summary, n.Escrow = h.statusReport(ctx)
	n.DiskPath = h.cfg.StateDir
	n.DiskFree, n.DiskUnknown = freeBytes(h.cfg.StateDir)
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
	for _, svc := range config.ProjectServices {
		if n.Pins[svc] != "" {
			continue
		}
		if n.BinaryInfo != nil && n.BinaryInfo.Pins[svc] != "" {
			n.Pins[svc] = n.BinaryInfo.Pins[svc]
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

// freeBytes returns the free space of the volume holding path; unknown is true when it cannot be
// read.
func freeBytes(path string) (free uint64, unknown bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, true
	}
	return uint64(st.Bavail) * uint64(st.Bsize), false
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

// statusReport runs `supavise status --json` as the supavise user on the installed binary and
// returns the verdict, the first line of the summary and what the report says about the master
// key's copy in the backup backend. The report is the one place that asks the backend, and it asks
// as the user that owns the backups: root opening a file backend that does not exist yet would
// create it root's.
func (h *nodeHost) statusReport(ctx context.Context) (verdict, summary string, esc nodeupgrade.Escrow) {
	var buf bytes.Buffer
	// Exit status 1 and 2 are verdicts; the JSON is on stdout either way.
	_ = h.asSupavise(ctx, &buf, nil, h.binPath, "status", "--json")
	return parseStatusReport(buf.Bytes())
}

// parseStatusReport reads the JSON `supavise status --json` prints.
func parseStatusReport(b []byte) (verdict, summary string, esc nodeupgrade.Escrow) {
	var rep struct {
		Status     string `json:"status"`
		Summary    string `json:"summary"`
		Components []struct {
			Name   string `json:"name"`
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &rep); err != nil || rep.Status == "" {
		return nodeupgrade.VerdictUnknown, "status printed no verdict", esc
	}
	for _, c := range rep.Components {
		if c.Name != "key escrow" {
			continue
		}
		switch {
		// Covered needs the component to say ok and not to say that it has not looked yet; any
		// other state, or no component, leaves it not covered, so a reworded report cannot let an
		// unattended upgrade through. Known only tells the warning apart: the report named the
		// key as missing (and not, say, an unreachable backend).
		case c.State == "ok" && !strings.HasPrefix(c.Detail, "not checked"):
			esc = nodeupgrade.Escrow{Known: true, Covered: true, Detail: c.Detail}
		case strings.Contains(c.Detail, "not in the backups"):
			esc = nodeupgrade.Escrow{Known: true, Covered: false, Detail: c.Detail}
		default:
			esc = nodeupgrade.Escrow{Detail: c.Detail}
		}
	}
	return rep.Status, rep.Summary, esc
}

// Status implements nodeupgrade.Host.
func (h *nodeHost) Status(ctx context.Context) (string, string, error) {
	v, s, _ := h.statusReport(ctx)
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
			args := []string{"backups", "create", ref}
			if h.knowsReason {
				args = append(args, "--reason", backup.ReasonUpgrade)
			}
			if err := h.asSupavise(gctx, lw.with(ref+": "), nil, h.binPath, args...); err != nil {
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

// Mark implements nodeupgrade.Host. The marker is also written every ten minutes while the
// upgrade runs, so that a long rollout does not look like a process that died (the marker counts
// as stale two hours after its last write).
func (h *nodeHost) Mark(phase, detail string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started.IsZero() {
		h.started = time.Now().UTC()
	}
	h.last = notice.Upgrade{Phase: phase, From: h.from, To: h.to, StartedAt: h.started, PID: os.Getpid(), Detail: detail}
	h.writeMarker()
	switch {
	case nodeupgrade.Finished(phase):
		if h.hbStop != nil {
			close(h.hbStop)
			h.hbStop = nil
		}
	case h.hbStop == nil:
		h.hbStop = make(chan struct{})
		go h.heartbeat(h.hbStop)
	}
}

func (h *nodeHost) writeMarker() {
	uid, gid := stateOwner(h.cfg)
	if err := notice.WriteUpgradeAs(h.cfg.Paths(), h.last, uid, gid); err != nil {
		h.log.Warn("could not write the upgrade marker", "error", err)
	}
}

func (h *nodeHost) heartbeat(stop chan struct{}) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.mu.Lock()
			h.writeMarker()
			h.mu.Unlock()
		}
	}
}

// stateOwner is the owner of the state directory, whom a file that root writes there belongs to
// (-1, -1 when this process is not root and its files are its own already). The owner is given to
// the file while it is still a temporary one that only this process has open: the state directory
// belongs to the supavise user, whose services run user code, and a chown by path after the rename
// would follow whatever that user put at the path in between.
func stateOwner(cfg *config.Config) (uid, gid int) {
	if os.Geteuid() != 0 {
		return -1, -1
	}
	fi, err := os.Stat(cfg.StateDir)
	if err != nil {
		return -1, -1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

// Install implements nodeupgrade.Host.
func (h *nodeHost) Install(ctx context.Context, s *nodeupgrade.Staged, prev, next nodeupgrade.Record) (bool, error) {
	sd := s.Data.(*staged)
	if !h.root {
		return false, errors.New("run as root: sudo supavise upgrade")
	}
	if old, err := h.rel.Get(prev.Version); err == nil {
		// What the store knows of the running release stays: when it became current and the window
		// of the upgrade that installed it, which its own rollback needs.
		prev.InstalledAt, prev.UpgradeStartedAt, prev.UpgradeEndedAt = old.InstalledAt, old.UpgradeStartedAt, old.UpgradeEndedAt
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
	return true, h.activate(ctx)
}

// activate renders the units with the binary at BinPath, restarts the daemon and waits until it
// answers.
func (h *nodeHost) activate(ctx context.Context) error {
	// A service counts as moved when it restarted after this point: the daemon restarts them
	// while it starts, possibly before it answers on its admin listener.
	h.swappedAt = time.Now()
	h.tagsAtSwap = h.renderedTags()
	c := exec.CommandContext(ctx, h.binPath, "system", "install-units")
	c.Stdout, c.Stderr = h.out, h.errw
	if err := c.Run(); err != nil {
		fmt.Fprintf(h.errw, "warning: supavise system install-units failed: %v\n", err)
	}
	_ = exec.CommandContext(ctx, "systemctl", "reset-failed", "supavise.service").Run()
	return restartAndWait(ctx, h.cfg, h.wait)
}

// renderedTags are the releases the units of the node's services are set to run, by service.
func (h *nodeHost) renderedTags() map[string]string {
	tags := map[string]string{}
	for _, svc := range append([]string{config.SvcPostgres, config.SvcGoTrue}, fleet.ServicesFor(h.cfg)...) {
		if tag, err := fleet.RenderedTag(h.cfg, svc); err == nil {
			tags[svc] = tag
		}
	}
	return tags
}

// Restore implements nodeupgrade.Host.
func (h *nodeHost) Restore(ctx context.Context, from string, rec nodeupgrade.Record) error {
	if !h.root {
		return errors.New("run as root: sudo supavise rollback")
	}
	// The record on disk is the authority: root wrote it when it kept the binary, with the checksum
	// the binary has to match. The one the caller holds may be the plan's, made before it was kept.
	stored, err := h.rel.Get(rec.Version)
	if err != nil {
		return fmt.Errorf("the kept release %s has no record: %w", rec.Version, err)
	}
	if err := h.rel.Verify(stored); err != nil {
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
	if err := h.rel.Touch(rec.Version, time.Now()); err != nil {
		return err
	}
	if err := h.rel.Withdraw(from); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.Phase != "" {
		return h.last.Phase
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
		case mustHaveRestarted(st.Since, h.swappedAt, h.tagsAtSwap[m.Service], m.To):
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

// mustHaveRestarted says whether a service that is active since `since` is still the process of
// the release before the swap, and so has to restart before it counts as moved. A unit that was
// already set to run `to` when the binary was swapped has no reason to restart: the swap moved
// nothing for it. That is the case for a service that a daemon which died on its start never got
// to move, when a rollback puts the old release back.
func mustHaveRestarted(since, swappedAt time.Time, tagAtSwap, to string) bool {
	return tagAtSwap != to && since.Before(swappedAt)
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
func upgradeProjectsArgs(target map[string]string, since time.Time) []string {
	args := append([]string{"projects", "upgrade", "--all", "--yes", "--no-gc", "--restart-changed"}, toArgs(target)...)
	if !since.IsZero() {
		// The base backups this run took are the pre-upgrade backups: a project does not take a
		// second one, and the archived WAL carries a restore from them up to the moment of the upgrade.
		args = append(args, "--reuse-backup-since", since.UTC().Format(time.RFC3339))
	}
	return args
}

// UpgradeProjects implements nodeupgrade.Host.
func (h *nodeHost) UpgradeProjects(ctx context.Context, target map[string]string, since time.Time) ([]nodeupgrade.ProjectMove, error) {
	tee := &haltTee{w: h.errw}
	err := h.asSupaviseTo(ctx, h.out, tee, nil, h.binPath, upgradeProjectsArgs(target, since)...)
	h.mu.Lock()
	h.halted = tee.Ref()
	h.mu.Unlock()
	moves, merr := h.MovesBetween(context.WithoutCancel(ctx), since, time.Time{})
	if merr != nil {
		h.log.Warn("could not read which projects were upgraded", "error", merr)
	}
	return moves, err
}

// HaltedProject implements nodeupgrade.HaltReporter: the project the rollout stopped at.
func (h *nodeHost) HaltedProject() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.halted
}

// EndUpgrade implements nodeupgrade.Host.
func (h *nodeHost) EndUpgrade(_ context.Context, version string, at time.Time) error {
	return h.rel.EndUpgrade(version, at)
}

// MovesBetween implements nodeupgrade.Host.
func (h *nodeHost) MovesBetween(ctx context.Context, since, until time.Time) ([]nodeupgrade.ProjectMove, error) {
	reg, err := registry.OpenExisting(ctx, lifecycle.SystemSocketDSN(h.cfg, "supavise")+" pool_max_conns=2")
	if err != nil {
		return nil, err
	}
	defer reg.Close()
	store := registry.Upgrades(reg)
	if store == nil {
		return nil, errors.New("the registry keeps no upgrade history")
	}
	ups, err := store.UpgradesBetween(ctx, since, until)
	if err != nil {
		return nil, err
	}
	return netMoves(ups), nil
}

// netMoves folds the upgrades (oldest first) into one move per project: each service goes from
// the release it ran before its first upgrade in the list to the one it runs after its last. The
// system project is left out, and so is a service an upgrade left where it was.
func netMoves(ups []registry.Upgrade) []nodeupgrade.ProjectMove {
	byRef := map[string]*nodeupgrade.ProjectMove{}
	var order []string
	for _, u := range ups {
		if u.Ref == config.SystemRef {
			continue
		}
		m := byRef[u.Ref]
		if m == nil {
			m = &nodeupgrade.ProjectMove{Ref: u.Ref, From: map[string]string{}, To: map[string]string{}, At: u.InitiatedAt}
			byRef[u.Ref] = m
			order = append(order, u.Ref)
		}
		for svc, from := range u.From {
			if _, seen := m.From[svc]; !seen {
				m.From[svc] = from
			}
		}
		for svc, to := range u.To {
			m.To[svc] = to
		}
	}
	var out []nodeupgrade.ProjectMove
	for _, ref := range order {
		m := byRef[ref]
		if len(m.RevertTargets()) > 0 {
			out = append(out, *m)
		}
	}
	return out
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

// AppliedMigrations implements nodeupgrade.Host.
func (h *nodeHost) AppliedMigrations(ctx context.Context) ([]string, error) {
	return registry.AppliedMigrations(ctx, lifecycle.SystemSocketDSN(h.cfg, "supavise"))
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
