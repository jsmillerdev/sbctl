package command

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/registry"
)

func request() failover.Request {
	return failover.Request{
		Old:   registry.Node{ID: "n1", Name: "primary", PeerAddr: "10.0.0.1:7443"},
		New:   registry.Node{ID: "n2", Name: "standby", PeerAddr: "10.0.0.2:7443"},
		Epoch: 7, Planned: false,
	}
}

func TestEnvironmentNamesTheMove(t *testing.T) {
	env := strings.Join(Environment(request()), "\n")
	for _, want := range []string{"OLD_NODE=primary", "NEW_NODE=standby", "EPOCH=7", "PLANNED=0", "OLD_NODE_ID=n1", "NEW_NODE_ID=n2", "OLD_NODE_ADDR=10.0.0.1:7443", "NEW_NODE_ADDR=10.0.0.2:7443"} {
		if !strings.Contains(env, want) {
			t.Errorf("no %s in\n%s", want, env)
		}
	}
	r := request()
	r.Planned = true
	if !strings.Contains(strings.Join(Environment(r), "\n"), "PLANNED=1") {
		t.Fatal("a switchover is PLANNED=1")
	}
}

// The commands really run, through /bin/sh, with the move in the environment.
func TestFenceCommandRunsInAShellWithTheEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	p := &Provider{FenceCommand: `echo "$OLD_NODE $NEW_NODE $EPOCH $PLANNED" > ` + out}
	if err := p.Fence(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.TrimSpace(string(b)) != "primary standby 7 0" {
		t.Fatalf("the command saw %q", b)
	}
}

func TestAFailingFenceCommandBlocksThePromotionAndSaysWhy(t *testing.T) {
	p := &Provider{FenceCommand: `echo "no route to the BMC" >&2; exit 3`}
	err := p.Fence(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "fence_command failed") || !strings.Contains(err.Error(), "no route to the BMC") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error: %v", err)
	}
	if err := (&Provider{}).Fence(context.Background(), request()); err == nil {
		t.Fatal("an empty fence command fenced")
	}
}

func TestACommandThatHangsIsKilledWithWhatItStarted(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	p := &Provider{FenceCommand: `sleep 30 & echo $! > ` + pidfile + `; wait`, Timeout: 300 * time.Millisecond}
	start := time.Now()
	err := p.Fence(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("error: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	b, _ := os.ReadFile(pidfile)
	pid := strings.TrimSpace(string(b))
	if pid == "" {
		t.Fatal("the command never started its child")
	}
	// The child was in the process group that got the signal. (/proc is Linux's; elsewhere the
	// timeout above is all the test can see.)
	time.Sleep(200 * time.Millisecond)
	if alive(pid) {
		t.Fatalf("the child %s survived the timeout", pid)
	}
}

// A descendant that left the process group (setsid) and holds the command's output must not hold the
// fence: a timeout still ends it, and a command that exited 0 still counts as done.
func TestADescendantOutsideTheProcessGroupDoesNotHoldTheFence(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on this system")
	}
	old := waitDelay
	waitDelay = 200 * time.Millisecond
	t.Cleanup(func() { waitDelay = old })

	start := time.Now()
	p := &Provider{FenceCommand: `setsid sleep 8 & sleep 8`, Timeout: 300 * time.Millisecond}
	if err := p.Fence(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("a timeout: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the timeout took %s: the fence waited for the descendant", time.Since(start))
	}

	start = time.Now()
	p = &Provider{FenceCommand: `setsid sleep 8 & exit 0`, Timeout: 30 * time.Second}
	if err := p.Fence(context.Background(), request()); err != nil {
		t.Fatalf("a command that exited 0: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the fence took %s: it waited for the descendant", time.Since(start))
	}
}

// alive reports whether the process runs: a zombie, which waits for its parent to be reaped, does not.
func alive(pid string) bool {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
	return !strings.HasPrefix(strings.TrimSpace(rest), "Z")
}

func TestTakeoverCommandIsOptional(t *testing.T) {
	p := &Provider{FenceCommand: "true"}
	if err := p.TakeOver(context.Background(), request()); !errors.Is(err, failover.ErrNoTakeover) {
		t.Fatalf("without a command: %v", err)
	}
	var ran []string
	p.TakeoverCommand = "move-eip --to $NEW_NODE"
	p.Run = func(_ context.Context, command string, env []string) (string, error) {
		ran = append(ran, command+" | "+strings.Join(env, " "))
		return "", nil
	}
	if err := p.TakeOver(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || !strings.Contains(ran[0], "move-eip --to $NEW_NODE") || !strings.Contains(ran[0], "NEW_NODE=standby") {
		t.Fatalf("ran %v", ran)
	}
	p.Run = func(context.Context, string, []string) (string, error) {
		return "route 53 said no", errors.New("exit status 1")
	}
	if err := p.TakeOver(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "takeover_command failed") || !strings.Contains(err.Error(), "route 53 said no") {
		t.Fatalf("a failing takeover: %v", err)
	}
}

func TestProbeChecksTheProgramExists(t *testing.T) {
	ctx := context.Background()
	if err := (&Provider{FenceCommand: "/bin/sh -c true"}).Probe(ctx); err != nil {
		t.Fatalf("a program that exists: %v", err)
	}
	if err := (&Provider{FenceCommand: "/no/such/fencer --now"}).Probe(ctx); err == nil || !strings.Contains(err.Error(), "fence_command") {
		t.Fatalf("a program that does not: %v", err)
	}
	if err := (&Provider{}).Probe(ctx); err == nil {
		t.Fatal("no command")
	}
	// Shell syntax cannot be checked from outside and is left to the run.
	if err := (&Provider{FenceCommand: "(cd /; ./x) || true"}).Probe(ctx); err != nil {
		t.Fatalf("compound command: %v", err)
	}
	if (&Provider{}).Name() != "command" {
		t.Fatal("name")
	}
}

func TestOutputInErrorsIsBounded(t *testing.T) {
	long := strings.Repeat("x", 5000)
	p := &Provider{FenceCommand: "x", Run: func(context.Context, string, []string) (string, error) { return long, errors.New("exit status 1") }}
	err := p.Fence(context.Background(), request())
	if err == nil || len(err.Error()) > 1200 {
		n := 0
		if err != nil {
			n = len(err.Error())
		}
		t.Fatalf("error of %d bytes", n)
	}
}

// The daemon's environment may hold the master key; the operator's command gets a short list and the
// move.
func TestACommandGetsAShortListOfTheDaemonsEnvironmentAndNoSecret(t *testing.T) {
	t.Setenv("SUPAVISE_MASTER_KEY", "do-not-pass-this")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "nor-this")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("HOME", "/home/supavise")
	p := &Provider{FenceCommand: "env"}
	var got string
	p.Run = func(ctx context.Context, command string, env []string) (string, error) {
		out, err := shell(ctx, command, env)
		got = out
		return out, err
	}
	req := failover.Request{Old: registry.Node{ID: "n1", Name: "primary"}, New: registry.Node{ID: "n2", Name: "standby"}, Epoch: 2}
	if err := p.Fence(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OLD_NODE=primary", "NEW_NODE_ID=n2", "EPOCH=2", "PLANNED=0", "AWS_REGION=eu-west-1", "HOME=/home/supavise", "PATH="} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in the command's environment:\n%s", want, got)
		}
	}
	for _, secret := range []string{"SUPAVISE_MASTER_KEY", "do-not-pass-this", "AWS_SECRET_ACCESS_KEY", "nor-this"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q reached the command:\n%s", secret, got)
		}
	}
}
