// Package command is the failover provider for any platform: the operator's own commands fence the
// old primary and move the service address (design 2.10.6). [failover] fence_command and
// takeover_command run on the survivor, through /bin/sh, with the move in the environment:
//
//	OLD_NODE, NEW_NODE         the nodes' names ([node] name); OLD_NODE_ID and NEW_NODE_ID are "n1", "n2"
//	OLD_NODE_ADDR, NEW_NODE_ADDR  the peer address hint of each (host:port), when the registry has one
//	EPOCH                       the cluster epoch of the move
//	PLANNED                     1 for a switchover, 0 for a failover
//
// A fence command must exit 0 only when the old node can no longer write; anything else means no
// promotion. The environment is a short list of the daemon's own (PATH, HOME, LANG, LC_ALL, TZ,
// TMPDIR, AWS_REGION and AWS_DEFAULT_REGION) plus these variables. The daemon's environment may hold
// secrets, such as the master key, and a command is the operator's script, not the daemon: nothing
// else is passed, and a command that needs a secret reads it from a file it owns.
package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/supavise/supavise/internal/failover"
)

// Runner runs one shell command with extra environment variables and returns its combined output.
type Runner func(ctx context.Context, command string, env []string) (output string, err error)

// Provider runs the configured commands.
type Provider struct {
	FenceCommand    string
	TakeoverCommand string
	// Timeout bounds each command. Zero is 2 minutes ([failover] stop_timeout_seconds).
	Timeout time.Duration
	// Run replaces the shell, for tests. Nil runs /bin/sh -c.
	Run Runner
}

var _ failover.Provider = (*Provider)(nil)

func (p *Provider) Name() string { return "command" }

func (p *Provider) timeout() time.Duration {
	if p.Timeout <= 0 {
		return 2 * time.Minute
	}
	return p.Timeout
}

func (p *Provider) run(ctx context.Context, command string, env []string) (string, error) {
	if p.Run != nil {
		return p.Run(ctx, command, env)
	}
	return shell(ctx, command, env)
}

// Environment is the variables a command gets for a move.
func Environment(req failover.Request) []string {
	planned := "0"
	if req.Planned {
		planned = "1"
	}
	return []string{
		"OLD_NODE=" + req.Old.Name, "OLD_NODE_ID=" + req.Old.ID, "OLD_NODE_ADDR=" + req.Old.PeerAddr,
		"NEW_NODE=" + req.New.Name, "NEW_NODE_ID=" + req.New.ID, "NEW_NODE_ADDR=" + req.New.PeerAddr,
		"EPOCH=" + strconv.FormatInt(req.Epoch, 10), "PLANNED=" + planned,
	}
}

// Fence runs fence_command. Without one it fails: a "command" provider is configured with one
// (config validation requires it).
func (p *Provider) Fence(ctx context.Context, req failover.Request) error {
	if strings.TrimSpace(p.FenceCommand) == "" {
		return errors.New("command: [failover] fence_command is empty")
	}
	return p.exec(ctx, "fence_command", p.FenceCommand, req)
}

// TakeOver runs takeover_command, when there is one. Without it nothing moves the address and the
// caller prints the DNS guidance.
func (p *Provider) TakeOver(ctx context.Context, req failover.Request) error {
	if strings.TrimSpace(p.TakeoverCommand) == "" {
		return failover.ErrNoTakeover
	}
	return p.exec(ctx, "takeover_command", p.TakeoverCommand, req)
}

// Probe checks, without running anything, that the commands are there: the fence command's
// program exists and may be run.
func (p *Provider) Probe(context.Context) error {
	if strings.TrimSpace(p.FenceCommand) == "" {
		return errors.New("[failover] fence_command is empty")
	}
	return checkProgram("fence_command", p.FenceCommand)
}

func checkProgram(key, command string) error {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return fmt.Errorf("%s is empty", key)
	}
	// A shell builtin, a variable or a compound command cannot be checked from outside.
	prog := fields[0]
	if strings.ContainsAny(prog, "$`(){}<>|&;'\"=") {
		return nil
	}
	if _, err := exec.LookPath(prog); err != nil {
		return fmt.Errorf("%s: %q: %w", key, prog, err)
	}
	return nil
}

func (p *Provider) exec(ctx context.Context, key, command string, req failover.Request) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	out, err := p.run(ctx, command, Environment(req))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("did not finish in %s", p.timeout())
		}
		return fmt.Errorf("%s failed: %w%s", key, err, tail(out))
	}
	return nil
}

// tail is the last of a command's output, for the error text.
func tail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	const max = 1024
	if len(out) > max {
		out = "..." + out[len(out)-max:]
	}
	return ": " + out
}

// waitDelay is how long shell waits for a killed command's output to end. A descendant that left the
// process group (setsid, nohup) and still holds the output would keep Wait blocked for ever.
var waitDelay = 5 * time.Second

// shell runs command through /bin/sh in its own process group, so that a timeout stops what the
// command started too.
func shell(ctx context.Context, command string, env []string) (string, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = append(Inherited(os.Environ()), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if errors.Is(err, exec.ErrWaitDelay) { // the command succeeded; only a descendant still holds its output
			err = nil
		}
		return out.String(), err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return out.String(), ctx.Err()
	}
}

// inherited are the variables of the daemon's environment a command gets: where to find programs and
// the home directory, the locale, and the region the cloud tools ask about.
var inherited = map[string]bool{
	"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true,
	"AWS_REGION": true, "AWS_DEFAULT_REGION": true,
}

// Inherited picks the variables of environ that a command gets, in their order.
func Inherited(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if name, _, ok := strings.Cut(kv, "="); ok && inherited[name] {
			out = append(out, kv)
		}
	}
	return out
}
