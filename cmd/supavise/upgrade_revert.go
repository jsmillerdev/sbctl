package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/supavise/supavise/internal/nodeupgrade"
	"github.com/supavise/supavise/internal/registry"
)

// revertAttempts is how many times a project's revert is run when it fails and leaves the project
// on the releases it had. A revert that fails for a reason that stays fails again and is reported
// with every error.
const revertAttempts = 2

// projectReverter puts one project back on the releases an upgrade moved it from.
type projectReverter struct {
	// run executes `supavise <args>` as the supavise user.
	run func(ctx context.Context, args []string) error
	// state returns what the project runs now: its recorded releases and its status.
	state func(ctx context.Context, ref string) (versions map[string]string, status string, err error)
	say   func(format string, args ...any)
}

// revert runs `projects upgrade <ref> --to ...` for the project alone. What decides whether the
// project went back is where it ends, not what the command reported: a command that failed, for
// instance because the services were restarted too soon after a neighbour's, may still have left
// the project on its old releases (counts as reverted), and one that failed and left it on the new
// releases is run once more.
func (r projectReverter) revert(ctx context.Context, m nodeupgrade.ProjectMove) error {
	targets := m.RevertTargets()
	args := append([]string{"projects", "upgrade", m.Ref, "--yes", "--no-gc", "--allow-older"}, toArgs(targets)...)
	var errs []error
	for attempt := 1; attempt <= revertAttempts; attempt++ {
		err := r.run(ctx, args)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
		versions, status, serr := r.state(ctx, m.Ref)
		switch {
		case serr != nil:
			r.say("%s: the revert failed (%v) and the project's releases could not be read: %v", m.Ref, err, serr)
		case runsReleases(versions, status, targets):
			r.say("%s: the revert reported an error (%v), but the project runs its old releases", m.Ref, err)
			return nil
		}
		if attempt < revertAttempts {
			r.say("%s: not back on its old releases; trying once more", m.Ref)
		}
	}
	return errors.Join(errs...)
}

// runsReleases reports whether a healthy project runs every release in targets.
func runsReleases(versions map[string]string, status string, targets map[string]string) bool {
	if status != string(registry.StatusActiveHealthy) || len(targets) == 0 {
		return false
	}
	for svc, tag := range targets {
		if versions[svc] != tag {
			return false
		}
	}
	return true
}

// RevertProjects implements nodeupgrade.Host: each project on its own, one after the other. It
// returns an error for the projects that did not end on their old releases.
func (h *nodeHost) RevertProjects(ctx context.Context, moves []nodeupgrade.ProjectMove) error {
	var mu sync.Mutex
	lw := &prefixWriter{w: h.out, mu: &mu}
	var errs []error
	for _, m := range moves {
		pw, notes := lw.with(m.Ref+": "), lw.with("")
		r := projectReverter{
			run: func(ctx context.Context, args []string) error { return h.asSupavise(ctx, pw, nil, h.binPath, args...) },
			state: func(ctx context.Context, ref string) (map[string]string, string, error) {
				var buf bytes.Buffer
				if err := h.asSupavise(ctx, &buf, nil, h.binPath, "projects", "versions", ref, "--json"); err != nil {
					return nil, "", err
				}
				var v versionsView
				if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
					return nil, "", fmt.Errorf("projects versions %s --json: %w", ref, err)
				}
				return maps.Clone(v.Versions), v.Status, nil
			},
			say: func(format string, args ...any) { fmt.Fprintf(notes, format+"\n", args...) },
		}
		if err := r.revert(ctx, m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Ref, err))
		}
	}
	return errors.Join(errs...)
}
