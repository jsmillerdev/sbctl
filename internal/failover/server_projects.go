package failover

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// The per-project half of a server move (design 2.10.4 steps 8 and 9). Projects are promoted
// four at a time, in the order of their sequence numbers. A project that fails does not stop the
// others: the move reports it and a resume runs only what is left.

const (
	// serverParallel is how many projects a server move promotes at once.
	serverParallel = 4
	// backupParallel is how many base backups on the new timeline run at once.
	backupParallel = 2
	// seedTimeout bounds the wait for a standby built from the archive.
	seedTimeout = 30 * time.Minute
)

// moveProjects promotes every project of the plan. It returns the failures, one line each.
func (o *Orchestrator) moveProjects(ctx context.Context, j *journal, run *serverRun, rec serverPlanRecord, timeout int) []string {
	var mu sync.Mutex
	var failed []string
	var g errgroup.Group
	g.SetLimit(serverParallel)
	for _, ch := range rec.Projects {
		g.Go(func() error {
			if err := o.serverProject(ctx, j, run, ch, timeout); err != nil {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("project %s: %v", ch.Ref, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	sort.Strings(failed)
	return failed
}

// serverProject moves one project to its node. The steps are recorded as p:<ref>:<step>, and
// p:<ref>:done ends the project.
func (o *Orchestrator) serverProject(ctx context.Context, j *journal, run *serverRun, ch projectRecord, timeout int) (err error) {
	ref, pfx := ch.Ref, "p:"+ch.Ref+":"
	if j.has(pfx + "done") {
		return nil
	}
	st := o.store()
	to, err := o.node(ctx, ch.Node)
	if err != nil {
		return err
	}
	if !ch.Paused {
		_ = st.SetProjectStatus(ctx, ref, registry.StatusComingUp)
	}
	defer func() {
		if err != nil {
			_ = st.SetProjectStatus(ctx, ref, registry.StatusActiveUnhealthy)
		}
	}()

	identifier := ch.Replica
	if ch.Restore {
		if err := j.step(ctx, pfx+"seed", func() (string, error) { return o.seedFromArchive(ctx, run, ch, to) }); err != nil {
			return err
		}
		identifier = j.detail(pfx + "seed")
	}
	if err := j.step(ctx, pfx+"promote", func() (string, error) {
		a := promoteArgs{Epoch: run.epoch, Timeout: timeout, Drain: !run.planned || ch.Restore}
		if run.planned && !ch.Restore {
			if a.WaitLSN = j.detail("stopped:" + ref); a.WaitLSN == "" {
				return "", errors.New("the old leader reported no final position for the project")
			}
		}
		d, err := o.promoteReplica(ctx, to, identifier, a)
		var abort *abortError
		if errors.As(err, &abort) { // the leadership has moved: there is no going back for one project
			err = abort.cause
		}
		return d, err
	}); err != nil {
		return err
	}
	if err := j.step(ctx, pfx+"homed", func() (string, error) {
		if err := st.SetProjectNode(ctx, ref, to.ID, run.epoch); err != nil {
			return "", fmt.Errorf("moving the project's home to %s: %w", to.Name, err)
		}
		if !run.planned || ch.Restore {
			// A project restored from the archive may end before the old primary did: the old
			// primary cannot follow the new timeline and is rebuilt instead (reseed).
			return "-", nil
		}
		return o.addReplicaRow(ctx, ref, run.from, ch.Origin)
	}); err != nil {
		return err
	}
	if ch.Paused {
		if err := j.step(ctx, pfx+"started", func() (string, error) {
			if _, err := o.d.Primaries.Stop(ctx, to.ID, ref); err != nil {
				return "", fmt.Errorf("pausing the project on %s: %w", to.Name, err)
			}
			return "paused", nil
		}); err != nil {
			return err
		}
	} else {
		if err := j.step(ctx, pfx+"started", func() (string, error) {
			if err := o.d.Primaries.Start(ctx, to.ID, ref); err != nil {
				return "", fmt.Errorf("starting the project on %s: %w", to.Name, err)
			}
			return "", nil
		}); err != nil {
			return err
		}
		if err := j.step(ctx, pfx+"tenant", func() (string, error) { return "", o.ensureTenant(ctx, ref) }); err != nil {
			return err
		}
	}
	status := registry.StatusActiveHealthy
	if ch.Paused {
		status = registry.StatusInactive
	}
	if err := st.SetProjectStatus(ctx, ref, status); err != nil {
		o.d.Log.Warn("could not set the project's status", "ref", ref, "error", err)
	}
	return j.record(ctx, pfx+"done", to.Name)
}

// seedFromArchive builds a standby of a project that had no replica from the WAL archive alone,
// with no upstream, and waits until it replays. It returns the new replica's identifier.
func (o *Orchestrator) seedFromArchive(ctx context.Context, run *serverRun, ch projectRecord, to registry.Node) (string, error) {
	id, err := o.addReplicaRow(ctx, ch.Ref, to, registry.ReplicaManual)
	if err != nil {
		return "", err
	}
	st, err := o.d.Instances.Ensure(ctx, to.ID, peerapi.InstanceSpec{Identifier: id, Ref: ch.Ref, NoUpstream: true, Epoch: run.epoch})
	deadline := o.d.Now().Add(seedTimeout)
	for {
		switch {
		case err != nil:
			return "", fmt.Errorf("seeding a standby of %s on %s from the archive: %w", ch.Ref, to.Name, err)
		case st.Error != "":
			return "", fmt.Errorf("seeding a standby of %s on %s failed at %s: %s", ch.Ref, to.Name, st.Error, st.Detail)
		case st.PostgresUp && st.InRecovery:
			return id, nil
		case !o.d.Now().Before(deadline):
			return "", fmt.Errorf("the standby of %s on %s was not ready in %s (step %s)", ch.Ref, to.Name, seedTimeout, st.Step)
		}
		if err := o.wait(ctx, 5*time.Second); err != nil {
			return "", err
		}
		st, err = o.d.Instances.Observe(ctx, to.ID, id)
	}
}

// demoteOldLeader turns the clusters the old leader stopped into replicas of their new homes, in
// place: the system cluster first, because the node needs its registry back to render the
// others. It returns the failures.
func (o *Orchestrator) demoteOldLeader(ctx context.Context, j *journal, run *serverRun, rec serverPlanRecord, timeout int) []string {
	var failed []string
	demote := func(ref, identifier string) {
		step := "demote:" + ref
		if j.has(step) {
			return
		}
		if err := j.step(ctx, step, func() (string, error) {
			return o.demoteOld(ctx, run.from, ref, identifier, run.epoch, timeout)
		}); err != nil {
			failed = append(failed, fmt.Sprintf("demote %s: %v", ref, err))
		}
	}
	demote(config.SystemRef, j.detail("system-homed"))
	if len(failed) > 0 { // the node's registry is not back: the rest would only wait for it
		return failed
	}
	for _, ch := range rec.Projects {
		if !j.has("p:" + ch.Ref + ":done") {
			continue
		}
		if ch.Restore {
			if step := "reseed:" + ch.Ref; !j.has(step) {
				_ = j.record(ctx, step, o.reseedOld(ctx, run.from, ch.Ref, run.epoch))
			}
			continue
		}
		demote(ch.Ref, j.detail("p:"+ch.Ref+":homed"))
	}
	return failed
}

// baseBackups takes a base backup of the system project and of each project that was promoted, on
// its new timeline. A failure is a warning in the log: the projects run, and their backup timers
// take the next one.
func (o *Orchestrator) baseBackups(ctx context.Context, j *journal, run *serverRun, rec serverPlanRecord) {
	if j.has("base-backups") || o.d.Backups == nil {
		return
	}
	type target struct{ ref, node string }
	targets := []target{{config.SystemRef, run.to.ID}}
	for _, ch := range rec.Projects {
		if ch.Paused || !j.has("p:"+ch.Ref+":done") {
			continue
		}
		targets = append(targets, target{ch.Ref, ch.Node})
	}
	var mu sync.Mutex
	var warned []string
	var g errgroup.Group
	g.SetLimit(backupParallel)
	for _, t := range targets {
		g.Go(func() error {
			if _, err := o.d.Backups.BaseBackup(ctx, t.node, t.ref, peerapi.BackupRequest{Epoch: run.epoch, Reason: "failover"}); err != nil {
				mu.Lock()
				warned = append(warned, fmt.Sprintf("%s (%v)", t.ref, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	sort.Strings(warned)
	detail := fmt.Sprintf("%d taken", len(targets)-len(warned))
	if len(warned) > 0 {
		detail = "warning: no base backup on the new timeline for " + strings.Join(warned, ", ") + "; their backup timers take the next one"
	}
	_ = j.record(ctx, "base-backups", detail)
}
