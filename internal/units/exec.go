//go:build unix

package units

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// Exec is the development and test Supervisor: it runs each unit's launcher as a
// detached child process (own session and process group), logs to
// <state_dir>/logs/<unit>.log and keeps a pid file in <state_dir>/run/ so that a later
// supavise invocation can find, query and stop what an earlier one started. It does not
// enforce resource limits, restart crashed units or start anything at boot; use the
// systemd backend on servers.
type Exec struct {
	cfg *config.Config
	log *slog.Logger

	// Stop timeouts; tests shorten them.
	PostgresStopTimeout time.Duration // SIGINT (fast shutdown) before SIGQUIT
	StopTimeout         time.Duration // SIGTERM before SIGKILL for everything else
	// StartGrace is how long Start watches for a launcher that dies immediately.
	StartGrace time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var _ Supervisor = (*Exec)(nil)

// Sandboxed implements Sandboxer: child processes are not confined.
func (*Exec) Sandboxed() bool { return false }

// NewExec returns the exec backend for cfg.
func NewExec(cfg *config.Config, log *slog.Logger) *Exec {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Exec{cfg: cfg, log: log, PostgresStopTimeout: 90 * time.Second, StopTimeout: 15 * time.Second, StartGrace: 500 * time.Millisecond, locks: map[string]*sync.Mutex{}}
}

type pidRecord struct {
	PID     int       `json:"pid"`
	Started string    `json:"started"` // ps lstart of the process, guards against pid reuse
	At      time.Time `json:"at"`
}

func (e *Exec) runDir() string { return filepath.Join(e.cfg.StateDir, "run") }
func (e *Exec) pidPath(unit string) string {
	return filepath.Join(e.runDir(), unit+".pid")
}
func (e *Exec) logPath(unit string) string {
	return filepath.Join(e.cfg.StateDir, "logs", unit+".log")
}

// LogPath is where unit's stdout and stderr go.
func (e *Exec) LogPath(unit string) string { return e.logPath(unit) }

func (e *Exec) lock(unit string) func() {
	e.mu.Lock()
	m := e.locks[unit]
	if m == nil {
		m = &sync.Mutex{}
		e.locks[unit] = m
	}
	e.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Render implements Supervisor. Limits are not enforced by this backend.
func (e *Exec) Render(ctx context.Context, spec Spec) error {
	_, err := e.RenderChanged(ctx, spec)
	return err
}

// RenderChanged implements ChangeRenderer.
func (e *Exec) RenderChanged(_ context.Context, spec Spec) (bool, error) {
	return renderFiles(e.cfg, spec)
}

// Start implements Supervisor. It returns once the launcher has been exec'd; a launcher
// that dies within a moment is reported with the tail of its log.
func (e *Exec) Start(ctx context.Context, unit string) error {
	defer e.lock(unit)()
	files, _, _, err := runFilesFor(e.cfg, unit)
	if err != nil {
		return err
	}
	if rec, alive := e.running(unit); alive {
		e.log.Debug("unit already running", "unit", unit, "pid", rec.PID)
		return nil
	}
	envBytes, err := os.ReadFile(files.Env)
	if err != nil {
		return fmt.Errorf("units: %s was not rendered: %w", unit, err)
	}
	env, err := ParseEnv(envBytes)
	if err != nil {
		return err
	}
	if _, err := os.Stat(files.Run); err != nil {
		return fmt.Errorf("units: %s was not rendered: %w", unit, err)
	}
	if err := os.MkdirAll(e.runDir(), 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.logPath(unit)), 0o750); err != nil {
		return err
	}
	logf, err := os.OpenFile(e.logPath(unit), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fmt.Fprintf(logf, "--- supavise exec backend: starting %s at %s\n", unit, time.Now().Format(time.RFC3339))

	cmd := exec.Command(files.Run)
	cmd.Env = append(baseEnv(), flatten(env)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("units: start %s: %w", unit, err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); logf.Close() }()

	rec := pidRecord{PID: pid, Started: processStart(pid), At: time.Now()}
	b, _ := json.Marshal(rec)
	if _, err := writeIfChanged(e.pidPath(unit), b, 0o640); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return err
	}
	select {
	case werr := <-exited:
		_ = os.Remove(e.pidPath(unit))
		return fmt.Errorf("units: %s exited right after start (%v)\n%s", unit, werr, e.tail(unit, 20))
	case <-time.After(e.StartGrace):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Stop implements Supervisor: Postgres gets SIGINT (fast shutdown) on its main
// process and SIGQUIT if that takes too long; everything else gets SIGTERM on the
// process group. A process that is still there after the timeout is killed with its
// whole group.
func (e *Exec) Stop(ctx context.Context, unit string) error {
	defer e.lock(unit)()
	return e.stop(ctx, unit)
}

func (e *Exec) stop(ctx context.Context, unit string) error {
	svc, _, err := ParseUnit(unit)
	if err != nil {
		return err
	}
	rec, alive := e.running(unit)
	if rec == nil {
		return nil
	}
	defer os.Remove(e.pidPath(unit))
	if !alive {
		e.reapGroup(rec.PID)
		return nil
	}
	if svc == config.SvcPostgres {
		_ = syscall.Kill(rec.PID, syscall.SIGINT)
		if e.waitGone(ctx, rec.PID, e.PostgresStopTimeout) {
			e.reapGroup(rec.PID)
			return nil
		}
		e.log.Warn("postgres did not stop on SIGINT; sending SIGQUIT", "unit", unit)
		_ = syscall.Kill(rec.PID, syscall.SIGQUIT)
		if e.waitGone(ctx, rec.PID, 15*time.Second) {
			e.reapGroup(rec.PID)
			return nil
		}
	} else {
		_ = syscall.Kill(-rec.PID, syscall.SIGTERM)
		if e.waitGone(ctx, rec.PID, e.StopTimeout) {
			e.reapGroup(rec.PID)
			return nil
		}
	}
	e.log.Warn("killing unit", "unit", unit)
	_ = syscall.Kill(-rec.PID, syscall.SIGKILL)
	if !e.waitGone(ctx, rec.PID, 5*time.Second) {
		return fmt.Errorf("units: %s (pid %d) did not exit", unit, rec.PID)
	}
	return nil
}

// reapGroup kills stragglers left in the process group after the main process is gone.
func (e *Exec) reapGroup(pgid int) {
	if syscall.Kill(-pgid, 0) != nil {
		return
	}
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(-pgid, 0) == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

func (e *Exec) waitGone(ctx context.Context, pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return !pidAlive(pid)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return !pidAlive(pid)
}

// Status implements Supervisor. A pid file whose process has gone means the unit
// died on its own (StateFailed); Stop removes the pid file (StateInactive).
func (e *Exec) Status(ctx context.Context, unit string) (Status, error) {
	if _, _, err := ParseUnit(unit); err != nil {
		return Status{}, err
	}
	st := Status{Unit: unit, State: StateInactive, SubState: "dead"}
	rec, alive := e.running(unit)
	switch {
	case rec == nil:
	case !alive:
		st.State, st.SubState = StateFailed, "failed"
	default:
		st.State, st.SubState, st.MainPID, st.Since = StateActive, "running", rec.PID, rec.At
		st.MemoryBytes = groupRSS(rec.PID)
	}
	return st, nil
}

// Remove implements Supervisor.
func (e *Exec) Remove(ctx context.Context, unit string) error {
	defer e.lock(unit)()
	files, _, _, err := runFilesFor(e.cfg, unit)
	if err != nil {
		return err
	}
	if err := e.stop(ctx, unit); err != nil {
		return err
	}
	_ = os.Remove(e.logPath(unit))
	return removeFiles(files)
}

// running returns the unit's pid record (nil if none) and whether that process is
// still the one we started.
func (e *Exec) running(unit string) (*pidRecord, bool) {
	b, err := os.ReadFile(e.pidPath(unit))
	if err != nil {
		return nil, false
	}
	var rec pidRecord
	if json.Unmarshal(b, &rec) != nil || rec.PID <= 0 {
		return nil, false
	}
	if !pidAlive(rec.PID) {
		return &rec, false
	}
	if rec.Started != "" && processStart(rec.PID) != rec.Started {
		return &rec, false // pid was reused by an unrelated process
	}
	return &rec, true
}

// Tail returns the last n lines of unit's log.
func (e *Exec) Tail(unit string, n int) string { return e.tail(unit, n) }

func (e *Exec) tail(unit string, n int) string {
	b, err := os.ReadFile(e.logPath(unit))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processStart returns ps's start time string of pid, "" if it cannot be read.
func processStart(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// groupRSS sums the resident set size, in bytes, of every process in process group pgid.
func groupRSS(pgid int) uint64 {
	out, err := exec.Command("ps", "-axo", "pgid=,rss=").Output()
	if err != nil {
		return 0
	}
	var total uint64
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) != 2 {
			continue
		}
		if g, err := strconv.Atoi(f[0]); err != nil || g != pgid {
			continue
		}
		if kb, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			total += kb << 10
		}
	}
	return total
}

// baseEnv is the minimal environment every unit gets, mirroring systemd's.
func baseEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "USER", "LOGNAME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	return env
}

func flatten(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
