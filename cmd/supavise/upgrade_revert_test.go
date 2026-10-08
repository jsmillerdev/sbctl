package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/nodeupgrade"
)

var gotrueMove = nodeupgrade.ProjectMove{Ref: "aaaa",
	From: map[string]string{"gotrue": "auth-v2.195.0-r1", "postgrest": "postgrest-v16.4-r0"},
	To:   map[string]string{"gotrue": "auth-v2.196.0-r0", "postgrest": "postgrest-v16.4-r0"}}

type fakeRevert struct {
	runs    [][]string
	runErrs []error // the error of each run, in order; nil beyond the end
	// after each run, the state the project is in
	states []fakeState
	notes  []string
}

type fakeState struct {
	versions map[string]string
	status   string
	err      error
}

func (f *fakeRevert) reverter() projectReverter {
	return projectReverter{
		run: func(_ context.Context, args []string) error {
			f.runs = append(f.runs, args)
			if len(f.runs) <= len(f.runErrs) {
				return f.runErrs[len(f.runs)-1]
			}
			return nil
		},
		state: func(context.Context, string) (map[string]string, string, error) {
			i := min(len(f.runs), len(f.states)) - 1
			if i < 0 {
				return nil, "", errors.New("no state")
			}
			s := f.states[i]
			return s.versions, s.status, s.err
		},
		say: func(format string, args ...any) { f.notes = append(f.notes, fmt.Sprintf(format, args...)) },
	}
}

var (
	onOld = fakeState{versions: map[string]string{"gotrue": "auth-v2.195.0-r1", "postgrest": "postgrest-v16.4-r0"}, status: "ACTIVE_HEALTHY"}
	onNew = fakeState{versions: map[string]string{"gotrue": "auth-v2.196.0-r0", "postgrest": "postgrest-v16.4-r0"}, status: "ACTIVE_HEALTHY"}
)

// The revert names its project and only the services that moved.
func TestRevertRunsOneProjectWithItsTargets(t *testing.T) {
	f := &fakeRevert{}
	if err := f.reverter().revert(context.Background(), gotrueMove); err != nil {
		t.Fatal(err)
	}
	want := []string{"projects", "upgrade", "aaaa", "--yes", "--no-gc", "--allow-older", "--to", "gotrue=auth-v2.195.0-r1"}
	if len(f.runs) != 1 || !slices.Equal(f.runs[0], want) {
		t.Fatalf("runs = %v, want one run of %v", f.runs, want)
	}
}

// A command that failed but left the project on its old releases did revert it.
func TestRevertCountsTheFinalState(t *testing.T) {
	f := &fakeRevert{runErrs: []error{errors.New("exit status 1")}, states: []fakeState{onOld}}
	if err := f.reverter().revert(context.Background(), gotrueMove); err != nil {
		t.Fatalf("a project on its old releases is reverted: %v", err)
	}
	if len(f.runs) != 1 || len(f.notes) != 1 || !strings.Contains(f.notes[0], "runs its old releases") {
		t.Fatalf("runs = %d, notes = %q", len(f.runs), f.notes)
	}
}

// A project the first run left on the new releases is run once more, and the second run decides.
func TestRevertTriesOnceMore(t *testing.T) {
	f := &fakeRevert{runErrs: []error{errors.New("exit status 1")}, states: []fakeState{onNew, onOld}}
	if err := f.reverter().revert(context.Background(), gotrueMove); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(f.runs))
	}
}

// A failure that stays is reported, with the error of every run, after revertAttempts runs.
func TestRevertReportsAFailureThatStays(t *testing.T) {
	f := &fakeRevert{runErrs: []error{errors.New("first"), errors.New("second")}, states: []fakeState{onNew}}
	err := f.reverter().revert(context.Background(), gotrueMove)
	if err == nil || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") || len(f.runs) != revertAttempts {
		t.Fatalf("err = %v after %d runs", err, len(f.runs))
	}
}

// Old releases on an unhealthy project, or a state that cannot be read, are not a revert.
func TestRevertNeedsAHealthyProjectOnItsOldReleases(t *testing.T) {
	unhealthy := onOld
	unhealthy.status = "ACTIVE_UNHEALTHY"
	for name, st := range map[string]fakeState{
		"unhealthy":        unhealthy,
		"unreadable":       {err: errors.New("boom")},
		"one service back": {versions: map[string]string{"gotrue": "auth-v2.196.0-r0"}, status: "ACTIVE_HEALTHY"},
	} {
		f := &fakeRevert{runErrs: []error{errors.New("e1"), errors.New("e2")}, states: []fakeState{st}}
		if err := f.reverter().revert(context.Background(), gotrueMove); err == nil {
			t.Errorf("%s: reported as reverted", name)
		}
	}
}

// A cancelled context ends the attempts.
func TestRevertStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRevert{runErrs: []error{errors.New("killed")}, states: []fakeState{onNew}}
	r := f.reverter()
	run := r.run
	r.run = func(ctx context.Context, args []string) error { cancel(); return run(ctx, args) }
	if err := r.revert(ctx, gotrueMove); err == nil || len(f.runs) != 1 {
		t.Fatalf("err = %v after %d runs", err, len(f.runs))
	}
}
