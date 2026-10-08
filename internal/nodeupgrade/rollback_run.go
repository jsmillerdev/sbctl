package nodeupgrade

import (
	"context"
	"sort"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// Rollback is `supavise rollback`: it puts the node back on the previous kept release, binary and
// shared-service pins, and the projects that the current release's upgrade moved back on the
// releases they ran. It refuses (exit status 2) when there is no previous release, when its kept
// binary is not the one that was kept, and when the registry has been migrated past what that
// release expects (CheckRollback). A failure after it started is exit status 4: there is no
// release further back to go to.
func Rollback(ctx context.Context, h Host, o Options) error {
	node, err := h.Inspect(ctx)
	if err != nil {
		return refused("cannot read the node: %v", err)
	}
	if node.Running != nil {
		return refused("an upgrade is running (process %d, phase %s)", node.Running.PID, node.Running.Phase)
	}
	prev, cur, err := h.PreviousRelease(ctx, node.Version)
	if err != nil {
		return refused("%v", err)
	}
	if prev == nil {
		return refused("no previous release is kept: %s is the oldest this node can go back to", node.Version)
	}
	applied, err := h.AppliedMigrations(ctx)
	if err != nil {
		return refused("cannot read the registry's migrations: %v", err)
	}
	if err := CheckRollback(prev, applied); err != nil {
		return refused("%v", err)
	}

	var moves []ProjectMove
	if cur != nil {
		// The window of the upgrade that installed the release, not the time the release last became
		// current: a release the node reached by a rollback was installed again at that moment,
		// and its own moves lie before it.
		since := cur.UpgradeStartedAt
		if since.IsZero() {
			since = cur.InstalledAt
		}
		if !cur.UpgradeStartedAt.IsZero() && cur.UpgradeEndedAt.IsZero() {
			// Without the end, a project an Owner upgraded after the node's upgrade would look like
			// one of the upgrade's moves, so none is put back.
			o.say("note: the kept record of %s does not say when its upgrade ended, so the projects it upgraded are left on their releases (`supavise projects versions` lists them)", node.Version)
		} else {
			all, err := h.MovesBetween(ctx, since, cur.UpgradeEndedAt)
			if err != nil {
				return refused("cannot read which projects the upgrade moved: %v", err)
			}
			moves = revertable(all, node, cur.UpgradeEndedAt)
		}
	} else {
		o.say("note: the kept record of %s is gone, so the projects it upgraded are left on their releases (`supavise projects versions` lists them)", node.Version)
	}

	moveList := DiffPins(node.Pins, prev.Pins)
	o.say("Supavise %s -> %s (the previous kept release, installed %s)", node.Version, prev.Version, prev.InstalledAt.Format(time.RFC3339))
	for _, m := range moveList {
		if m.Service != config.SvcPostgREST {
			o.say("  %-13s %s -> %s", m.Service, short(m.Service, m.From), short(m.Service, m.To))
		}
	}
	if len(moves) > 0 {
		refs := make([]string, len(moves))
		for i, m := range moves {
			refs[i] = m.Ref
		}
		o.say("%d project(s) go back to the releases they ran before the upgrade: %s", len(moves), refList(refs))
		o.say("note: GoTrue's database migrations only go forward; the older GoTrue runs on the schema the newer one left")
	}
	o.say("note: when the old daemon starts it restarts, one after another and outside the canary and batches, every project whose PostgreSQL, GoTrue or PostgREST files %s and %s render differently and that runs the newer files; each such restart drops the project's database connections. A project that still runs the files of %s keeps running. A release built without the held-back-restart marks restarts every project whose files differ.", node.Version, prev.Version, prev.Version)
	if !o.Yes {
		ok, err := h.Confirm("Roll back now?")
		if err != nil {
			return refused("%v", err)
		}
		if !ok {
			return refused("nothing was changed")
		}
	}

	r := &run{h: h, o: o, node: node, started: o.now()}
	r.plan = &Plan{From: node.Version, To: node.Version, Target: &Info{Version: node.Version, Pins: node.Pins}}
	r.mark(PhaseRollingBack, "going back to "+prev.Version)
	o.log().Info("rollback_started", "from", node.Version, "to", prev.Version, "projects", len(moves))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Minute)
	defer cancel()
	if err := rollBackTo(ctx, h, o, rollbackArgs{From: node.Version, FromPins: node.Pins, To: prev, Moves: moves, Verdict: node.Verdict, Mark: r.mark}); err != nil {
		r.mark(PhaseFailed, err.Error())
		o.log().Error("rollback_failed", "error", err.Error())
		return &Failure{Code: ExitNeedsOperator, Err: err}
	}
	r.mark(PhaseRolledBack, "")
	o.log().Info("rollback_succeeded", "to", prev.Version)
	o.say("rolled back: Supavise %s is running", prev.Version)
	return nil
}

// revertable keeps the moves a rollback should undo: the project still runs the releases the
// upgrade moved it to (a project upgraded again since is left alone), the move changed something,
// and it began before the upgrade ended (end zero: no limit), so that an upgrade an Owner started
// later is not undone. The result is in ref order.
func revertable(all []ProjectMove, node *Node, end time.Time) []ProjectMove {
	now := map[string]Project{}
	for _, p := range node.Projects {
		now[p.Ref] = p
	}
	var out []ProjectMove
	for _, m := range all {
		p, ok := now[m.Ref]
		if !ok || !p.Active() || (!end.IsZero() && m.At.After(end)) {
			continue
		}
		back := m.RevertTargets()
		for svc := range back {
			if p.Effective(svc, node.Pins) != m.To[svc] {
				delete(back, svc)
			}
		}
		if len(back) == 0 {
			continue
		}
		// What is reverted is exactly the services still on the upgrade's release.
		keep := ProjectMove{Ref: m.Ref, At: m.At, From: map[string]string{}, To: map[string]string{}}
		for svc, from := range back {
			keep.From[svc], keep.To[svc] = from, m.To[svc]
		}
		out = append(out, keep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}
