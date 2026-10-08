package replicas

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// replicaState is what the controller remembers about one replica besides its row. All of it is
// in memory: the registry keeps the desired state and the status, never lag or receiver state (I5).
type replicaState struct {
	// obs is the latest observation of the instance and obsAt when the leader got it.
	obs   *peerapi.InstanceStatus
	obsAt time.Time
	lag   lagRing

	// stepSeen is the row's step as this process last saw it, and stepSince since when.
	stepSeen  string
	stepSince time.Time
	// calls keeps the retry bookkeeping of each kind of call that can fail, by key ("launch",
	// "observe", "pooler", "recreate", "remove").
	calls map[string]*callState
	// unreachable is since when the node has not answered an observation.
	unreachable time.Time
	// receiverDown is since when the receiver has not been streaming.
	receiverDown time.Time
	// transient is when the row was first seen RESTARTING or RESIZING; only an observation made
	// after it ends the status. restarting holds while Restart's call to the node is out.
	transient  time.Time
	restarting bool
	// reensured is when an instance the node lost was last asked for again.
	reensured time.Time
	// removeSince is when the removal of the replica first failed.
	removeSince time.Time

	// What the setup learned, for the estimates: the base backup's id, size and stop LSN, and when
	// the download began.
	backupID   string
	seedBytes  int64
	backupLSN  string
	downloadAt time.Time
	// alerted holds the alert keys that are open for this replica.
	alerted map[string]bool
}

// callState is the retry bookkeeping of one kind of call: since when it has been failing, how
// many times in a row, the earliest time for the next try and the last error's text.
type callState struct {
	since time.Time
	tries int
	next  time.Time
	err   string
}

// st returns id's state, creating it. The caller holds c.mu.
func (c *Controller) st(id string) *replicaState {
	s := c.state[id]
	if s == nil {
		s = &replicaState{alerted: map[string]bool{}, calls: map[string]*callState{}}
		c.state[id] = s
	}
	return s
}

// observed records what a node reports about an instance, as it is now.
func (c *Controller) observed(id string, o peerapi.InstanceStatus) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st(id)
	s.obs, s.obsAt = &o, now
	s.unreachable = time.Time{}
	if o.LagSeconds != nil {
		s.lag.add(now, *o.LagSeconds)
	}
	if o.ReceiverStatus == "streaming" || o.Role != "replica" {
		s.receiverDown = time.Time{}
	} else if s.receiverDown.IsZero() {
		s.receiverDown = now
	}
}

// unreached notes that an observation of id failed.
func (c *Controller) unreached(id string) time.Duration {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st(id)
	if s.unreachable.IsZero() {
		s.unreachable = now
	}
	return now.Sub(s.unreachable)
}

// fresh returns the latest observation when it is not older than max.
func (c *Controller) fresh(id string, max time.Duration) (peerapi.InstanceStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state[id]
	if s == nil || s.obs == nil || c.now().Sub(s.obsAt) > max {
		return peerapi.InstanceStatus{}, false
	}
	return *s.obs, true
}

// Retry bookkeeping. A call that fails is tried again after a pause that doubles from 10 seconds
// to 2 minutes; once calls of one kind have failed for Timeouts.Retry the setup gives up.

// due reports whether the call of that kind may be tried now.
func (c *Controller) due(id, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cs := c.st(id).calls[key]
	return cs == nil || !c.now().Before(cs.next)
}

// failedCall records a failed call and reports whether the retry window is over.
func (c *Controller) failedCall(id, key string, err error) (giveUp bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st(id)
	cs := s.calls[key]
	if cs == nil {
		cs = &callState{since: now}
		s.calls[key] = cs
	}
	cs.tries++
	cs.err = err.Error()
	cs.next = now.Add(min(10*time.Second<<min(cs.tries-1, 4), 2*time.Minute))
	return now.Sub(cs.since) >= c.to.Retry
}

// worked clears the bookkeeping of a kind of call that went through.
func (c *Controller) worked(id, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.st(id).calls, key)
}

// lastError is the text of the failure that is being retried.
func (c *Controller) lastError(id, key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cs := c.st(id).calls[key]; cs != nil {
		return cs.err
	}
	return ""
}

// stepClock returns how long the row has been at its current step, as this process saw it. The
// first sight of a step starts the clock, so a restart of the daemon gives every step a new allowance.
func (c *Controller) stepClock(id, step string) time.Duration {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st(id)
	if s.stepSeen != step {
		s.stepSeen, s.stepSince = step, now
		clear(s.calls)
	}
	return now.Sub(s.stepSince)
}

// setStatus writes a replica's status, step and failure code unless the row is being removed
// (GOING_DOWN is never overwritten by the setup or the health check) or is gone. It reports
// whether it wrote.
func (c *Controller) setStatus(ctx context.Context, id, status, step, code string) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	cur, err := c.reg.GetReplica(ctx, id)
	if err != nil {
		return false
	}
	if cur.Status == statusGoingDown && status != statusGoingDown {
		return false
	}
	if err := c.reg.SetReplicaStatus(ctx, id, status, step, code); err != nil {
		c.log.Warn("replicas: write status", "identifier", id, "status", status, "step", step, "error", err)
		return false
	}
	return true
}

// seedSize is the stored size of the newest completed base backup of ref: what a replica would download.
func (c *Controller) seedSize(ctx context.Context, ref string) int64 {
	bs, err := c.reg.ListBackups(ctx, ref)
	if err != nil {
		return 0
	}
	for i := range bs {
		if bs[i].Status == registry.BackupCompleted {
			return bs[i].SizeBytes
		}
	}
	return 0
}

// parseLSN reads the text form of a pg_lsn ("0/3000100").
func parseLSN(s string) (uint64, bool) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok {
		return 0, false
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return h<<32 | l, true
}
