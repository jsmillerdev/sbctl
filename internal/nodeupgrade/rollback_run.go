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
	applied, err := h.AppliedSchema(ctx)
	if err != nil {
		return refused("cannot read the registry schema: %v", err)
	}
	if err := CheckRollback(prev, applied); err != nil {
		return refused("%v", err)
	}

	var moves []ProjectMove
	if cur != nil {
		all, err := h.MovesSince(ctx, cur.InstalledAt)
		if err != nil {
			return refused("cannot read which projects the upgrade moved: %v", err)
		}
		moves = revertable(all, node)
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
// upgrade moved it to (a project upgraded again since is left alone), and the move changed
// something. The result is in ref order.
func revertable(all []ProjectMove, node *Node) []ProjectMove {
	now := map[string]Project{}
	for _, p := range node.Projects {
		now[p.Ref] = p
	}
	var out []ProjectMove
	for _, m := range all {
		p, ok := now[m.Ref]
		if !ok || !p.Active() {
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
