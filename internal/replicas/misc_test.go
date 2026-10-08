package replicas

import (
	"errors"
	"fmt"
	"testing"

	"github.com/supavise/supavise/internal/mesh"
)

// A node's refusal is final when it understood the request and said no; a busy node, a conflict
// with another change and a lost session are tried again.
func TestTerminal(t *testing.T) {
	re := func(status int) error { return fmt.Errorf("call: %w", &mesh.RemoteError{Node: "n2", Status: status}) }
	for err, want := range map[error]bool{
		re(400): true, re(404): true, re(422): true,
		re(408): false, re(409): false, re(425): false, re(429): false, re(500): false, re(503): false,
		fmt.Errorf("x: %w", mesh.ErrNoSession): false, errors.New("boom"): false,
	} {
		if got := terminal(err); got != want {
			t.Errorf("terminal(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestStepHelpers(t *testing.T) {
	for i, s := range steps {
		if stepIndex(s) != i {
			t.Errorf("stepIndex(%q) = %d", s, stepIndex(s))
		}
	}
	if stepIndex("nonsense") != -1 || laterStep(StepLaunched, "nonsense") != StepLaunched || laterStep(StepLaunched, StepReplayed) != StepReplayed {
		t.Error("laterStep")
	}
	// Each code belongs to the step the row is at when it fails, and every step maps back to one.
	for i, code := range failureCodes {
		if failureStep(code) != steps[i+1] || failureFor(steps[i+1]) != code {
			t.Errorf("code %q: step %q, back %q", code, failureStep(code), failureFor(steps[i+1]))
		}
	}
	if failureFor(StepRequested) != FailLaunch || failureFor(StepDone) != FailComplete || failureFor("") != FailLaunch {
		t.Error("failureFor at the ends")
	}
	if got := (&PendingError{Identifiers: []string{"a", "b"}}).Error(); got != "replicas: removal pending for a, b" {
		t.Errorf("PendingError: %q", got)
	}
}

// Two replicas that draw the same six characters get different identifiers.
func TestIdentifierCollisionIsRetried(t *testing.T) {
	ids := []string{"aaaaaa", "aaaaaa", "bbbbbb"}
	e := newEnv(t, func(o *Options) { o.NewID = func() string { id := ids[0]; ids = ids[1:]; return id } })
	e.addNode("n4", "eu-b", "eu-west-1")
	if err := e.ctrl.SetupOn(e.ctx, refA, "n2"); err != nil {
		t.Fatal(err)
	}
	if err := e.ctrl.SetupOn(e.ctx, refA, "n4"); err != nil {
		t.Fatal(err)
	}
	if got := e.replica(refA, "n4").Identifier; got != refA+"-rr-eu-west-1-bbbbbb" {
		t.Fatalf("identifier %q", got)
	}
}
