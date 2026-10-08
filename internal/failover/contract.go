// Package failover moves a project, or the whole server, to another node: a planned switchover
// with the old primary alive (nothing is lost) and an unplanned failover with the old primary
// fenced first. This file is the contract the other packages are written against; the
// orchestrator, the fence providers (aws/ and command/) and the automatic monitor live in the
// other files.
//
// The order that keeps one writer: fence the old primary (a failed fence means no promotion),
// write the epoch marker to the backup store (the store lets one survivor through), take the
// service address, promote, then register the move in the registry. Every step is recorded in a
// registry.Move, or in failover.json until the system cluster is promoted, so that a crash resumes.
package failover

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/supavise/supavise/internal/registry"
)

// Request is what a fence provider and an address taker are told about a move.
type Request struct {
	// Old is the node that must stop writing; New is the survivor, where the provider runs.
	Old, New registry.Node
	// Epoch is the cluster epoch of the move, the one after the leader's current epoch for a
	// server move.
	Epoch int64
	// Planned is true for a switchover: the old primary was stopped cleanly, so a provider may
	// skip the hard stop. The takeover still happens.
	Planned bool
	// ServiceAddress is the cluster's service address (registry.Cluster.ServiceAddress).
	ServiceAddress registry.ServiceAddress
}

// Fencer makes sure the old primary cannot write.
type Fencer interface {
	// Name is "aws", "command" or "manual".
	Name() string
	// Fence returns nil only when the node can no longer write; any other result means no
	// promotion. On AWS: describe the instance, StopInstances, wait for stopped (force after
	// [failover] stop_timeout_seconds), in that order. A running peer that merely misses pings is
	// never stopped by the caller: the orchestrator gates automatic fences on the checks in 2.10.7.
	Fence(ctx context.Context, req Request) error
	// Probe checks without side effects that Fence would be permitted (on AWS, DryRun calls that
	// answer DryRunOperation). The daemon runs it at start for an automatic mode and the readiness
	// report shows its result; a failure downgrades the node to manual and raises an alert.
	Probe(ctx context.Context) error
}

// AddressTaker moves the cluster's service address to the survivor, before the promotion.
type AddressTaker interface {
	// TakeOver associates the service address with req.New (on AWS: AssociateAddress with
	// reassociation allowed), or runs [failover] takeover_command, or does nothing and lets the
	// caller print the DNS guidance.
	TakeOver(ctx context.Context, req Request) error
}

// Provider is the pair a failover uses; the AWS provider and the command provider are both.
type Provider interface {
	Fencer
	AddressTaker
}

// ProjectOptions are the flags of `supavise projects failover <ref>`.
type ProjectOptions struct {
	Ref string
	// To is the node to move the project to; empty picks the healthiest replica.
	To    string
	Force bool
	// DryRun changes nothing: the plan is made and FailoverProject returns a nil move.
	DryRun bool
	Resume bool
	// ExpectKind is the kind of move the operator confirmed in the plan, "switchover" or "failover".
	// When it is set and the plan made at the start of the run says the other, the run is
	// refused with ErrPlanChanged: a project that stopped answering between the plan and the run
	// is not fenced on the strength of a confirmation for a clean stop.
	ExpectKind string
	// Yes is the CLI's flag to skip the typed confirmation of a project failover; the daemon never reads it.
	Yes bool
}

// ServerOptions are the flags of `supavise failover`.
type ServerOptions struct {
	To    string
	Force bool
	// DryRun changes nothing: the plan is made and FailoverServer returns a nil move.
	DryRun           bool
	Resume           bool
	RestoreMissing   bool
	OldPrimaryIsDown bool
	Yes              bool
	// Abort discards the unfinished server move of this node, when it stopped before anything that
	// cannot be undone: the leader marker is not written, and an old leader that was stopped for a
	// switchover is started again. It replaces the run; no other option applies.
	Abort bool
	// ExpectKind and ExpectEpoch are what the operator confirmed in the plan. When ExpectKind is
	// set and the plan made at the start of the run differs in kind or epoch, the run is refused
	// with ErrPlanChanged.
	ExpectKind  string
	ExpectEpoch int64
}

// Check is one precondition and its verdict.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Blocking: the move is refused while it fails, unless Force overrides it.
	Blocking bool `json:"blocking"`
	// Hard: Force does not override it either. It marks the checks whose failure would lose
	// data or leave two writers (Storage objects that exist only on the old node, a fence that
	// cannot be made).
	Hard bool `json:"hard,omitempty"`
}

// ProjectPlan is what a server move does with one project.
type ProjectPlan struct {
	Ref string `json:"ref"`
	// Replica is the replica that would be promoted, "" when the project has none.
	Replica string `json:"replica,omitempty"`
	Node    string `json:"node,omitempty"`
	// LagSeconds is nil when unknown.
	LagSeconds *float64 `json:"lag_seconds,omitempty"`
	// RestoreFromArchive: no replica; --restore-missing seeds a standby from the archive (RPO up to archive_timeout).
	RestoreFromArchive bool `json:"restore_from_archive,omitempty"`
}

// Plan is what --dry-run prints.
type Plan struct {
	// Kind is "switchover" (the old primary is alive and stops cleanly) or "failover" (it is fenced
	// first). From and To are the node ids the move goes between, Ref is the project of a project
	// move and Epoch the cluster epoch the move runs in.
	Kind  string `json:"kind,omitempty"`
	Ref   string `json:"ref,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Epoch int64  `json:"epoch,omitempty"`
	// FromName and ToName are the nodes' names ([node] name), which is what the operator types.
	FromName string `json:"from_name,omitempty"`
	ToName   string `json:"to_name,omitempty"`

	Checks   []Check       `json:"checks"`
	Projects []ProjectPlan `json:"projects,omitempty"`
	// Notes say what else the move does or leaves to the operator ("DNS: point api. at ...").
	Notes []string `json:"notes,omitempty"`
}

// Blocked lists the failed blocking checks.
func (p Plan) Blocked() []Check {
	var out []Check
	for _, c := range p.Checks {
		if c.Blocking && !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Refused lists the failed blocking checks that stop a move that was started with force: all
// of them without it, and only the Hard ones with it.
func (p Plan) Refused(force bool) []Check {
	var out []Check
	for _, c := range p.Blocked() {
		if !force || c.Hard {
			out = append(out, c)
		}
	}
	return out
}

// Service is the failover orchestrator. The CLI and the API call it; it runs on the node that
// holds the leader's engine lock (a project move) or on the survivor (a server move).
type Service interface {
	// PlanProject checks the preconditions of a project switchover (2.10.2) without changing anything.
	PlanProject(ctx context.Context, o ProjectOptions) (*Plan, error)
	// FailoverProject switches the project over, or fails it over when its home is unreachable,
	// and returns the registry's record of the move. With Resume it continues the unfinished one.
	FailoverProject(ctx context.Context, o ProjectOptions) (*registry.Move, error)
	PlanServer(ctx context.Context, o ServerOptions) (*Plan, error)
	FailoverServer(ctx context.Context, o ServerOptions) (*registry.Move, error)
	// Readiness reports whether a server failover would be accepted now.
	Readiness(ctx context.Context) (Readiness, error)
}

// Readiness is the failover-readiness block of `supavise status` and the answer of
// GET /supavise/v1/failover/readiness.
type Readiness struct {
	// Ready is true when a server failover would pass its preconditions.
	Ready bool `json:"ready"`
	// Mode is [failover] mode.
	Mode string `json:"mode"`
	// Blockers are the reasons a server failover would be refused, one sentence each.
	Blockers []string `json:"blockers,omitempty"`
	// Notes are facts worth knowing that block nothing ("2 projects have no replica ...").
	Notes []string `json:"notes,omitempty"`
	// ProjectsWithoutReplica are the refs `supavise failover` would restore from the archive.
	ProjectsWithoutReplica []string `json:"projects_without_replica,omitempty"`
	// Fencer is "aws", "command" or "none"; FencerStatus is the probe's verdict ("DryRun OK").
	Fencer       string `json:"fencer"`
	FencerStatus string `json:"fencer_status,omitempty"`
	// EpochMarker says whether the backup store, where the leader marker lives, is reachable.
	EpochMarker string `json:"epoch_marker"`
}

// Render writes the block the way `supavise status` shows it:
//
//	failover  NOT READY: storage backend is file (objects exist only on this node)
//	          2 projects have no replica (--restore-missing: RPO up to archive_timeout)
//	          fencer: aws (DryRun OK)   epoch marker: store reachable
func (r Readiness) Render(w io.Writer) {
	const label, indent = "failover  ", "          "
	lines := append([]string{}, r.Blockers...)
	if len(lines) > 0 {
		lines[0] = "NOT READY: " + lines[0]
	} else if r.Ready {
		lines = []string{"READY"}
	} else {
		lines = []string{"NOT READY"}
	}
	lines = append(lines, r.Notes...)
	fence := "fencer: " + orNone(r.Fencer)
	if r.FencerStatus != "" {
		fence += " (" + r.FencerStatus + ")"
	}
	if r.EpochMarker != "" {
		fence += "   epoch marker: " + r.EpochMarker
	}
	lines = append(lines, fence)
	for i, l := range lines {
		pre := indent
		if i == 0 {
			pre = label
		}
		fmt.Fprintln(w, strings.TrimRight(pre+l, " "))
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
