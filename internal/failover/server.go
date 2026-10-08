package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// A server move (design 2.10.4): the leadership of the whole cluster goes to another node, and
// with it every project homed on the old leader. It runs on the node that takes over. The log is
// failover.json until the system cluster is promoted, because the survivor's registry cannot be
// written before that, and a moves row afterwards. The steps, in order:
//
//	switchover   begin, quiesce, caught-up, marker, address, promote-system, leader,
//	             projects (p:<ref>:seed|promote|homed|started|tenant|done), demote:<ref>,
//	             base-backups, dns
//	failover     begin, fence, marker, address, promote-system, leader, projects, base-backups, dns
//
// Nothing is promoted before the old leader cannot write (a clean stop, or a fence that
// succeeded), and nothing is written to the backup store's marker before the old leader is
// beyond return, so that a switchover that fails before then can still be undone. The marker is
// also what decides between two survivors that act at once: the store accepts one of them, and
// the other stops before it has touched the service address.

// serverPlanRecord is what the first step of a server move writes down: the standby of the
// system cluster and what happens to each project, so that a resume works after the replica rows
// it chose are gone.
type serverPlanRecord struct {
	SystemReplica string          `json:"system_replica"`
	Projects      []projectRecord `json:"projects"`
}

type projectRecord struct {
	Ref     string `json:"ref"`
	Replica string `json:"replica,omitempty"`
	Origin  string `json:"origin,omitempty"`
	Node    string `json:"node"`
	Restore bool   `json:"restore,omitempty"`
	Paused  bool   `json:"paused,omitempty"`
}

// FailoverServer moves the leadership to this node. A switchover can be asked of the leader with
// --to: the leader then has the node that takes over run the move (delegate.go).
func (o *Orchestrator) FailoverServer(ctx context.Context, opts ServerOptions) (*registry.Move, error) {
	release, err := o.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	return o.failoverServer(ctx, opts)
}

// failoverServer is FailoverServer for a caller that holds the node's move already.
func (o *Orchestrator) failoverServer(ctx context.Context, opts ServerOptions) (*registry.Move, error) {
	pl, run, err := o.planServer(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.DryRun {
		return nil, nil
	}
	if refused := pl.Refused(opts.Force || opts.Resume); len(refused) > 0 {
		return nil, &RefusedError{Checks: refused, Force: opts.Force}
	}
	if opts.ExpectKind != "" && !opts.Resume && (pl.Kind != opts.ExpectKind || pl.Epoch != opts.ExpectEpoch) {
		return nil, fmt.Errorf("%w: it was a %s at epoch %d and is a %s at epoch %d now", ErrPlanChanged, opts.ExpectKind, opts.ExpectEpoch, pl.Kind, pl.Epoch)
	}
	if run.to.ID != o.self().ID {
		return o.delegateServer(ctx, opts, run)
	}

	var j *journal
	var rec serverPlanRecord
	if opts.Resume {
		if j, rec, err = o.resumeServer(ctx, run); err != nil {
			return nil, err
		}
	} else {
		if j, rec, err = o.beginServer(ctx, run); err != nil {
			return nil, err
		}
	}
	detail := fmt.Sprintf("Moving the leadership from %s to %s (epoch %d).", run.from.Name, run.to.Name, run.epoch)
	if opts.Resume {
		detail = fmt.Sprintf("Continuing the move of the leadership from %s to %s (epoch %d) after it stopped.", run.from.Name, run.to.Name, run.epoch)
	}
	if restored := restoredRefs(rec); len(restored) > 0 {
		detail += fmt.Sprintf(" No replica exists for %s: their standbys are built from the archive, with data loss up to archive_timeout.", listRefs(restored))
	}
	o.announce(ctx, alerts.KindFailoverStarted, alerts.SeverityInfo, j.snapshot(), detail)
	return o.endMove(ctx, j, "", o.serverSteps(ctx, j, run, rec))
}

// restoredRefs are the projects of the plan that get a standby from the archive.
func restoredRefs(rec serverPlanRecord) []string {
	var refs []string
	for _, p := range rec.Projects {
		if p.Restore {
			refs = append(refs, p.Ref)
		}
	}
	return refs
}

// beginServer opens the log in failover.json with the plan as its first step.
func (o *Orchestrator) beginServer(ctx context.Context, run *serverRun) (*journal, serverPlanRecord, error) {
	kind := registry.MoveSwitchover
	if !run.planned {
		kind = registry.MoveFailover
	}
	mv := registry.Move{Scope: registry.MoveServer, Kind: kind, FromNode: run.from.ID, ToNode: run.to.ID, Epoch: run.epoch}
	j := o.fileJournal(mv, run.flags, nil)
	rec := serverPlanRecord{SystemReplica: run.systemReplica}
	for _, c := range run.projects {
		rec.Projects = append(rec.Projects, c.record())
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, rec, err
	}
	if err := j.record(ctx, "begin", string(b)); err != nil {
		return nil, rec, err
	}
	return j, rec, nil
}

// resumeServer loads the log of the unfinished move: the moves row when the system cluster was
// promoted and the move adopted into it, failover.json before.
func (o *Orchestrator) resumeServer(ctx context.Context, run *serverRun) (*journal, serverPlanRecord, error) {
	var rec serverPlanRecord
	fs, err := readStateFile(o.d.Cfg.Paths().FailoverState())
	if err != nil {
		return nil, rec, err
	}
	var j *journal
	if mv, err := unfinishedServerMove(ctx, o.store()); err != nil {
		return nil, rec, err
	} else if mv != nil {
		j = o.journalFor(*mv)
		if fs != nil { // a crash between the moves row and the removal of the file
			fj := o.fileJournal(*mv, fs.Flags, fs.Steps)
			if aerr := fj.adopt(ctx); aerr != nil {
				o.d.Log.Warn("could not finish adopting failover.json", "error", aerr)
			}
		}
	} else if fs != nil {
		mv := registry.Move{Scope: registry.MoveServer, Kind: fs.Kind, FromNode: fs.From, ToNode: fs.To, Epoch: fs.Epoch, StartedAt: fs.StartedAt}
		j = o.fileJournal(mv, fs.Flags, fs.Steps)
	} else {
		return nil, rec, ErrNothingToResume
	}
	if err := json.Unmarshal([]byte(j.detail("begin")), &rec); err != nil {
		return nil, rec, fmt.Errorf("failover: the first step of the move is unreadable: %w", err)
	}
	run.systemReplica = rec.SystemReplica
	return j, rec, nil
}

// serverSteps runs the steps of a server move that are not in the log yet.
func (o *Orchestrator) serverSteps(ctx context.Context, j *journal, run *serverRun, rec serverPlanRecord) error {
	st := o.store()
	cl, err := st.GetCluster(ctx)
	if err != nil {
		return fmt.Errorf("reading the cluster: %w", err)
	}
	req := Request{Old: run.from, New: run.to, Epoch: run.epoch, Planned: run.planned, ServiceAddress: cl.ServiceAddress}
	timeout := timeoutSeconds(o.conf().StopTimeout())

	if run.planned {
		if err := o.stopLeader(ctx, j, run); err != nil {
			return err
		}
	} else if err := j.step(ctx, "fence", func() (string, error) { return o.fenceLeader(ctx, run, req) }); err != nil {
		return err
	}

	if err := j.step(ctx, "marker", func() (string, error) { return o.writeMarker(ctx, run) }); err != nil {
		return err
	}

	if err := j.step(ctx, "address", func() (string, error) {
		err := o.d.Provider.TakeOver(ctx, req)
		switch {
		case err == nil:
			return "the service address moved to " + run.to.Name, nil
		case errors.Is(err, ErrNoTakeover):
			return "not moved: " + strings.TrimPrefix(err.Error(), ErrNoTakeover.Error()+": "), nil
		}
		return "", fmt.Errorf("taking the service address over: %w", err)
	}); err != nil {
		return err
	}

	if err := j.step(ctx, "promote-system", func() (string, error) {
		a := promoteArgs{Epoch: run.epoch, Timeout: timeout, Drain: !run.planned}
		if run.planned {
			a.WaitLSN = j.detail("stopped:system")
		}
		d, err := o.promoteReplica(ctx, run.to, rec.SystemReplica, a)
		var abort *abortError
		if errors.As(err, &abort) { // the marker is written: this can no longer be undone
			err = abort.cause
		}
		return d, err
	}); err != nil {
		return err
	}

	if err := j.step(ctx, "leader", func() (string, error) { return o.becomeLeader(ctx, j, run) }); err != nil {
		return err
	}

	if run.planned {
		if err := j.step(ctx, "system-homed", func() (string, error) {
			return o.addReplicaRow(ctx, "system", run.from, registry.ReplicaSystem)
		}); err != nil {
			return err
		}
	}

	failed := o.moveProjects(ctx, j, run, rec, timeout)
	if run.planned {
		failed = append(failed, o.demoteOldLeader(ctx, j, run, rec, timeout)...)
	}
	o.baseBackups(ctx, j, run, rec)

	if len(failed) > 0 {
		return fmt.Errorf("%d step(s) did not finish: %s; the cluster runs on %s, run supavise failover --resume to finish", len(failed), strings.Join(failed, "; "), run.to.Name)
	}
	return j.step(ctx, "dns", func() (string, error) {
		moved := strings.HasPrefix(j.detail("address"), "the service address moved")
		return o.guidance(cl.ServiceAddress, run.to, moved), nil
	})
}

// stopLeader runs the first half of a switchover: the leader stops everything and tells the final
// position of each cluster, and the survivor waits until its standby of the system cluster has
// replayed to it. Both can be undone while nothing irreversible has happened: the leader is told
// to start again.
func (o *Orchestrator) stopLeader(ctx context.Context, j *journal, run *serverRun) error {
	if o.d.Leader == nil {
		return errors.New("no way to reach the leader")
	}
	if !j.has("quiesce") {
		res, err := o.d.Leader.Quiesce(ctx, run.from.ID, QuiesceRequest{Epoch: run.epoch, To: run.to.ID, Reason: "switchover to " + run.to.Name})
		if err != nil {
			o.resumeLeader(ctx, run)
			return &abortError{cause: fmt.Errorf("stopping %s: %w", run.from.Name, err)}
		}
		for ref, lsn := range res.LSNs {
			if err := j.record(ctx, "stopped:"+ref, lsn); err != nil {
				return err
			}
		}
		if err := j.record(ctx, "quiesce", fmt.Sprintf("%d cluster(s) stopped", len(res.LSNs))); err != nil {
			return err
		}
	}
	if err := j.step(ctx, "caught-up", func() (string, error) {
		lsn := j.detail("stopped:system")
		if lsn == "" {
			return "", errors.New("the leader reported no final position for the system cluster")
		}
		if err := o.waitReplayed(ctx, run.to.ID, run.systemReplica, lsn); err != nil {
			return "", err
		}
		return "system standby at " + lsn, nil
	}); err != nil {
		o.resumeLeader(ctx, run)
		return &abortError{cause: err}
	}
	return nil
}

// resumeLeader tells the leader to start again; a failure is logged, because the caller is
// already reporting the reason it gave up.
func (o *Orchestrator) resumeLeader(ctx context.Context, run *serverRun) {
	if err := o.d.Leader.Resume(context.WithoutCancel(ctx), run.from.ID); err != nil {
		o.d.Log.Error("could not tell the leader to start again; restart supavise on it", "node", run.from.ID, "error", err)
	}
}

// waitReplayed waits until the standby has replayed up to lsn, for as long as the stop timeout.
func (o *Orchestrator) waitReplayed(ctx context.Context, node, identifier, lsn string) error {
	deadline := o.d.Now().Add(o.conf().StopTimeout())
	var last string
	for {
		obs, err := o.d.Instances.Observe(ctx, node, identifier)
		if err == nil {
			last = obs.ReplayLSN
			if lsnPast(obs.ReplayLSN, lsn) {
				return nil
			}
		} else {
			last = err.Error()
		}
		if !o.d.Now().Before(deadline) {
			return fmt.Errorf("%w: %s replayed to %s, the old leader stopped at %s", ErrReplayBehind, identifier, last, lsn)
		}
		if err := o.wait(ctx, time.Second); err != nil {
			return err
		}
	}
}

// fenceLeader makes sure the old leader cannot write before anything is promoted (design
// 2.10.6). The cooperative fence goes first: a leader that can still hear stops cleanly and
// records why. The provider's fence is the one that counts when there is one; without a
// provider the operator asserted that the node is down, and that is what the move rests on.
func (o *Orchestrator) fenceLeader(ctx context.Context, run *serverRun, req Request) (string, error) {
	var parts []string
	if o.d.Peers != nil {
		timeout := o.conf().StopTimeout()
		if o.d.Provider.Name() != "manual" {
			timeout = 20 * time.Second // the hard fence follows
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := o.d.Peers.Fence(cctx, run.from.ID, FenceCall{FenceRequest: peerapi.FenceRequest{
			Epoch: run.epoch, Leader: run.to.ID, Reason: fmt.Sprintf("server failover to %s at epoch %d", run.to.Name, run.epoch),
		}})
		cancel()
		switch {
		case err != nil:
			parts = append(parts, "cooperative fence: no answer from "+run.from.Name)
		case resp.Fenced:
			parts = append(parts, fmt.Sprintf("cooperative fence: %s stopped %d cluster(s)", run.from.Name, len(resp.Stopped)))
		default:
			parts = append(parts, fmt.Sprintf("cooperative fence: %s refused (epoch %d)", run.from.Name, resp.Epoch))
		}
	}
	if o.d.Provider.Name() == "manual" {
		if !run.flags.OldPrimaryIsDown {
			return "", fmt.Errorf("%w: no fencing method is configured and --old-primary-is-down was not given", ErrFence)
		}
		parts = append(parts, "no fencing method: asserted down by the operator")
		return strings.Join(parts, "; "), nil
	}
	if err := o.d.Provider.Fence(ctx, req); err != nil {
		return "", fmt.Errorf("%w: %s fencer: %v", ErrFence, o.d.Provider.Name(), err)
	}
	parts = append(parts, o.d.Provider.Name()+" fence: "+run.from.Name+" cannot write")
	return strings.Join(parts, "; "), nil
}

// writeMarker records the new leader in the backup store, where a node that cannot reach its peers
// learns at boot that it was replaced. A store that already holds a higher epoch means another
// node was promoted first: the move ends there, aborted, and nothing is promoted.
func (o *Orchestrator) writeMarker(ctx context.Context, run *serverRun) (string, error) {
	if o.d.Marker == nil {
		return "no backup store marker", nil
	}
	lost := &abortError{cause: fmt.Errorf("%w: the backup store's leader marker holds a higher epoch than %d, or this epoch under another leader: another node was promoted, and this node must not be (do not start %s again either)", ErrEpochLost, run.epoch, run.from.Name)}
	// A store that cannot refuse the same epoch under another leader is asked first.
	if m, rerr := o.d.Marker.ReadLeaderMarker(ctx); rerr == nil && m != nil && (m.Epoch > run.epoch || m.Epoch == run.epoch && m.Leader != run.to.ID) {
		return "", lost
	}
	err := o.d.Marker.WriteLeaderMarker(ctx, backup.LeaderMarker{Epoch: run.epoch, Leader: run.to.ID, At: o.d.Now().UTC()})
	switch {
	case err == nil:
		// A store that ignores conditional writes (Garage does) lets two survivors both write; the
		// one whose marker was overwritten sees the other's when it reads back.
		if m, rerr := o.d.Marker.ReadLeaderMarker(ctx); rerr == nil && m != nil && (m.Epoch > run.epoch || m.Epoch == run.epoch && m.Leader != run.to.ID) {
			return "", lost
		}
		return fmt.Sprintf("epoch %d, leader %s", run.epoch, run.to.ID), nil
	case errors.Is(err, backup.ErrMarkerNewer):
		return "", lost
	case run.flags.Force && o.recordsLeadership():
		return "warning: the leader marker was not written: " + err.Error(), nil
	}
	// The daemon of the promoted node restarts and decides its role from its cluster row, its peers and
	// this marker. With none of them naming it, it starts fenced: there would be no leader.
	return "", fmt.Errorf("writing the leader marker: %w (without it the promoted node's daemon starts fenced, so --force does not go on without it)", err)
}

// becomeLeader runs when the system cluster has been promoted: the daemon notices and becomes the
// leader (the Takeover port waits for that), the log moves into the registry, and the cluster
// row says who leads. The old leader is marked fenced when it was fenced.
func (o *Orchestrator) becomeLeader(ctx context.Context, j *journal, run *serverRun) (string, error) {
	if o.d.Takeover != nil {
		if err := o.d.Takeover.BecomeLeader(ctx, run.epoch); err != nil {
			// The daemon of this node takes up its new role by restarting, which ends this wait with the
			// context. The move is not over: the daemon that starts continues it at this step.
			return "", fmt.Errorf("waiting for %s to run as the leader: %w", run.to.Name, err)
		}
	}
	if err := j.adopt(ctx); err != nil {
		return "", err
	}
	st := o.store()
	if err := st.SetLeader(ctx, run.to.ID, run.epoch); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return "", fmt.Errorf("%w: the cluster is at a higher epoch than %d", ErrEpochLost, run.epoch)
		}
		return "", fmt.Errorf("recording the new leader: %w", err)
	}
	if !run.planned {
		if err := st.SetNodeState(ctx, run.from.ID, registry.NodeFenced); err != nil {
			return "", fmt.Errorf("marking %s fenced: %w", run.from.Name, err)
		}
	}
	if err := st.SetProjectNode(ctx, "system", run.to.ID, run.epoch); err != nil {
		return "", fmt.Errorf("moving the system project to %s: %w", run.to.Name, err)
	}
	// The new leader is serving: the announcement a switchover made is over.
	if err := st.SetMaintenance(ctx, registry.Maintenance{}); err != nil {
		o.d.Log.Warn("could not clear the maintenance announcement", "error", err)
	}
	return fmt.Sprintf("%s leads at epoch %d", run.to.Name, run.epoch), nil
}

// guidance says what the operator does about the service address.
func (o *Orchestrator) guidance(addr registry.ServiceAddress, to registry.Node, moved bool) string {
	if moved {
		return fmt.Sprintf("The service address %s now belongs to %s; DNS needs no change.", addrText(addr), to.Name)
	}
	host := to.PublicHost
	if host == "" {
		host = o.d.Cfg.PublicIP
	}
	if host == "" {
		host = "the public address of " + to.Name
	}
	d := o.d.Cfg.BaseDomain()
	return fmt.Sprintf("Point DNS at %s: *.api.%s, api.%s, studio.%s and pooler.%s (the service address was not moved).", host, d, d, d, d)
}

func addrText(a registry.ServiceAddress) string {
	if a.IP != "" {
		return a.IP
	}
	return a.AllocationID
}
