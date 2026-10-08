// Package hostsetup is the host layer of a Supavise upgrade: `supavise system converge`, a list of
// idempotent root steps that bring a server's unit files, directories, mount protection, firewall
// and packages to what a release expects, and `supavise install --aws-first-boot`, which prepares
// an EC2 instance's data volume before the installer runs.
//
// A release carries a converge revision (Revision). Converge writes the revision it completed to
// <state_dir>/converged; the daemon compares the two and raises host_not_converged while the file is
// behind (Monitor). Every step does its own check before it changes anything, so running converge
// twice changes nothing the second time, and `--check` runs the same checks without the changes.
package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Revision is the converge revision of this release. It is raised when the step list gains a
// step or a step changes what it does, so that a node whose marker is behind is known to need a
// converge. The release manifest names the same number (host.converge_revision).
const Revision = 1

// Runner runs the commands converge and the first-boot code need (systemctl, ufw, lsblk, mount,
// apt-get). Tests replace it. The output is stdout and stderr together.
type Runner interface {
	Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
}

// ExecRunner runs the commands for real. A command that exits with a status other than 0 returns
// an *ExitError.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		c.Env = append(os.Environ(), env...)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		e := &ExitError{Command: name + " " + strings.Join(args, " "), Code: -1, Output: strings.TrimSpace(string(out)), Err: err}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			e.Code = ee.ExitCode()
		}
		return out, e
	}
	return out, nil
}

// ExitError is a command that did not succeed. Code is its exit status, -1 when it did not run.
type ExitError struct {
	Command string
	Code    int
	Output  string
	Err     error
}

func (e *ExitError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("%s: %v", e.Command, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Command, e.Err, e.Output)
}

func (e *ExitError) Unwrap() error { return e.Err }

// exitCode is the exit status err carries: 0 for nil, -1 for an error that is not an *ExitError.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

// Pending is the answer of a step's check.
type Pending struct {
	// Pending is true when applying the step would change something.
	Pending bool
	// Unknown is true when the check could not tell, because what it compares with did not answer
	// (the leader of a cluster). Pending is false then, since applying changes nothing that is
	// known, but the step has not been seen to be in order.
	Unknown bool
	// Detail says what, or why the step does not apply here.
	Detail string
}

// Outcome is what a step did.
type Outcome struct {
	// Changed describes each change, one line each ("installed /etc/systemd/system/x.service").
	Changed []string
	// Files are the paths written, for the caller that reacts to a particular file.
	Files []string
	// Reload is true when systemd has to re-read its unit files.
	Reload bool
	// Warnings are things the step could not do and went on without; Run prints each. They are not
	// errors: the step counts as done.
	Warnings []string
}

// Step is one idempotent change of the host.
type Step interface {
	ID() string
	Title() string
	// NeedsRoot says that Apply needs root. Check never does, though a check that cannot see
	// something without root says so in its detail and does not report it pending.
	NeedsRoot() bool
	Check(ctx context.Context) (Pending, error)
	Apply(ctx context.Context) (Outcome, error)
}

// funcStep is a Step made of two functions.
type funcStep struct {
	id, title string
	root      bool
	check     func(ctx context.Context) (Pending, error)
	apply     func(ctx context.Context) (Outcome, error)
	// same is printed when Apply changed nothing ("units are up to date").
	same string
}

func (s *funcStep) ID() string                                 { return s.id }
func (s *funcStep) Title() string                              { return s.title }
func (s *funcStep) NeedsRoot() bool                            { return s.root }
func (s *funcStep) Check(ctx context.Context) (Pending, error) { return s.check(ctx) }
func (s *funcStep) Apply(ctx context.Context) (Outcome, error) { return s.apply(ctx) }
func (s *funcStep) unchanged() string                          { return s.same }

// Result is what `converge --json` and `converge --check --json` print for one step.
type Result struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Pending   bool   `json:"pending"`
	NeedsRoot bool   `json:"needs_root"`
	// Unknown is set on a check that could not tell (see Pending.Unknown); it is left out otherwise.
	Unknown bool `json:"unknown,omitempty"`
	// Detail says what is pending, or why the step does not apply on this host.
	Detail string `json:"detail,omitempty"`
	// Changed lists what Run changed.
	Changed []string `json:"changed,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// MarkerID is the id of the last entry of a converge result: the converged marker.
const MarkerID = "marker"

// Converger runs a list of steps and records the revision it completed.
type Converger struct {
	Steps []Step
	// StateDir holds the marker (<StateDir>/converged).
	StateDir string
	// Revision is the revision this list completes; zero means Revision.
	Revision int
	// Version is the release, recorded in the marker for people.
	Version string
	// NoMarker leaves the marker alone: a run that did not take every step of the host (the unit
	// files of a test directory) has not converged it.
	NoMarker bool
	// Out receives what each step changed, one line each. Nil discards it.
	Out io.Writer
	// Activate runs once after the steps and gets what they did together: it re-reads systemd's
	// unit files when Reload is set and enables what boot needs. It runs when every step
	// succeeded (enabling is idempotent and repairs a unit that was disabled), and after a
	// failure only if some step changed something already.
	Activate func(ctx context.Context, o Outcome) error
	Now      func() time.Time
}

func (c *Converger) revision() int {
	if c.Revision > 0 {
		return c.Revision
	}
	return Revision
}

func (c *Converger) say(format string, a ...any) {
	if c.Out != nil {
		fmt.Fprintf(c.Out, format+"\n", a...)
	}
}

// Check reports, for every step and the marker, whether Run would change something. It changes
// nothing and needs no root. A step whose check fails is reported pending with the error as its
// detail: not knowing is not the same as having nothing to do.
func (c *Converger) Check(ctx context.Context) []Result {
	var out []Result
	for _, s := range c.Steps {
		r := Result{ID: s.ID(), Title: s.Title(), NeedsRoot: s.NeedsRoot()}
		p, err := s.Check(ctx)
		if err != nil {
			r.Pending, r.Detail = true, err.Error()
		} else {
			r.Pending, r.Unknown, r.Detail = p.Pending, p.Unknown, p.Detail
		}
		out = append(out, r)
	}
	return append(out, c.markerResult())
}

func (c *Converger) markerResult() Result {
	r := Result{ID: MarkerID, Title: "Record the converge revision", NeedsRoot: true}
	m, err := ReadMarker(c.StateDir)
	switch {
	case err != nil:
		r.Pending, r.Detail = true, err.Error()
	case m.Revision < c.revision():
		r.Pending, r.Detail = true, fmt.Sprintf("revision %d of %d", m.Revision, c.revision())
	}
	return r
}

// Run applies every step, whether its check found something or not (each step checks for itself),
// then records the revision. Every step runs even if an earlier one failed, so that one broken
// step does not hold back the rest; the marker is written only when none failed (and NoMarker is
// not set). The returned error joins the steps' errors.
func (c *Converger) Run(ctx context.Context) ([]Result, error) {
	var (
		results []Result
		errs    []error
		all     Outcome
	)
	for _, s := range c.Steps {
		r := Result{ID: s.ID(), Title: s.Title(), NeedsRoot: s.NeedsRoot()}
		if p, err := s.Check(ctx); err == nil {
			r.Pending, r.Unknown, r.Detail = p.Pending, p.Unknown, p.Detail
		}
		o, err := s.Apply(ctx)
		r.Changed = o.Changed
		for _, line := range o.Changed {
			c.say("%s", line)
		}
		for _, line := range o.Warnings {
			c.say("warning: %s", line)
		}
		if len(o.Changed) == 0 && len(o.Warnings) == 0 && err == nil {
			if u, ok := s.(interface{ unchanged() string }); ok && u.unchanged() != "" {
				c.say("%s", u.unchanged())
			}
		}
		all.Changed = append(all.Changed, o.Changed...)
		all.Files = append(all.Files, o.Files...)
		all.Reload = all.Reload || o.Reload
		if err != nil {
			r.Error = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", s.Title(), err))
		}
		results = append(results, r)
	}
	if c.Activate != nil && (all.Reload || len(all.Changed) > 0 || len(errs) == 0) {
		if err := c.Activate(ctx, all); err != nil {
			errs = append(errs, err)
		}
	}
	mr := c.markerResult()
	if len(errs) == 0 && mr.Pending && !c.NoMarker {
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		if err := WriteMarker(c.StateDir, Marker{Revision: c.revision(), Version: c.Version, At: now().UTC()}); err != nil {
			errs = append(errs, fmt.Errorf("recording the converge revision: %w", err))
			mr.Error = err.Error()
		} else {
			mr.Changed = []string{fmt.Sprintf("recorded converge revision %d", c.revision())}
			c.say("%s", mr.Changed[0])
		}
	}
	results = append(results, mr)
	return results, errors.Join(errs...)
}

// AnyPending reports whether any result is pending.
func AnyPending(rs []Result) bool {
	for _, r := range rs {
		if r.Pending {
			return true
		}
	}
	return false
}
