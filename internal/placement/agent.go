package placement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

// The setup steps of a replica as the Management API shows them (design 2.7.2), and the failure each
// one reports. The agent runs steps 1 to 6; step 0 is the controller's row.
const (
	StepStarted    = "1_started"
	StepLaunched   = "2_launched_read_replica_instance"
	StepInitiated  = "3_initiated_read_replica_setup"
	StepDownloaded = "4_downloaded_base_backup"
	StepReplayed   = "5_replayed_wal_archives"
	StepCompleted  = registry.ReplicaStepDone
)

// stepFailures[n] is the error code of a setup that fails while it is at step n (1 to 5): the work
// that leads to the next step did not finish.
var stepFailures = map[string]string{
	StepStarted:    "1_read_replica_instance_launch_failed",
	StepLaunched:   "2_initiate_read_replica_setup_failed",
	StepInitiated:  "3_download_base_backup_failed",
	StepDownloaded: "4_replay_wal_archives_failed",
	StepReplayed:   "5_complete_read_replica_setup_failed",
}

// ReplicaPlane is what the node agent asks of the node's own plane: the replica operations of
// *lifecycle.PostgresPlane.
type ReplicaPlane interface {
	CreateReplica(ctx context.Context, t lifecycle.ReplicaTarget, o lifecycle.ReplicaCreateOptions) error
	StartReplica(ctx context.Context, t lifecycle.ReplicaTarget) error
	StartReplicaDatabase(ctx context.Context, t lifecycle.ReplicaTarget) error
	StartReplicaAPI(ctx context.Context, t lifecycle.ReplicaTarget) error
	StopReplica(ctx context.Context, ref string) error
	RemoveReplica(ctx context.Context, ref string) error
	ObserveReplica(ctx context.Context, t lifecycle.ReplicaTarget) lifecycle.ReplicaObservation
	PromoteReplica(ctx context.Context, t lifecycle.ReplicaTarget, o lifecycle.PromoteOptions) error
	DemoteToReplica(ctx context.Context, t lifecycle.ReplicaTarget) error
}

var _ ReplicaPlane = (*lifecycle.PostgresPlane)(nil)

// SeederFrom adapts the backup service's ReplicaSeeder to the plane's seeder function.
func SeederFrom(s backup.ReplicaSeeder) lifecycle.ReplicaSeeder {
	return func(ctx context.Context, p lifecycle.ReplicaSeedPlan) error {
		return s.SeedReplica(ctx, backup.ReplicaSeedPlan(p))
	}
}

// AgentOptions configure a NodeAgent.
type AgentOptions struct {
	Cfg      *config.Config
	Plane    ReplicaPlane
	Registry registry.Registry
	// Keys loads a project's credentials (Engine.Keys).
	Keys func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	// Members says who this node is, who leads and at which epoch. Nil skips the epoch checks (tests).
	Members cluster.Membership
	// Seeder builds the standby from a base backup (SeederFrom(backup.Service)).
	Seeder lifecycle.ReplicaSeeder
	// Admit checks that the node has room for the replica (Engine.AdmitReplica); nil admits.
	Admit func(ctx context.Context, p *registry.Project, identifier string, backupBytes int64) error
	// StallTimeout is how long a standby may make no replay progress and not stream before its setup
	// fails (default 15 minutes). Poll is how often the setup looks at it (default 2 seconds).
	StallTimeout time.Duration
	Poll         time.Duration
	// Concurrency is how many replicas StartLocal starts and ObserveAll observes at once (default 4).
	Concurrency int
	Log         *slog.Logger
	Now         func() time.Time
}

// NodeAgent runs the replica operations the leader asks of this node: it implements Agent. It keeps
// the setup of each replica in a small file in the project's directory (replica.json), so that a
// setup the daemon's restart interrupted resumes at the next request, and runs the long steps (the
// download of the base backup, the replay of the archive) in the background while the leader polls
// Observe.
type NodeAgent struct {
	o  AgentOptions
	mu sync.Mutex
	// insts are the replicas this process knows, by identifier; locks serialize the operations on a ref.
	insts map[string]*instance
	locks map[string]*sync.Mutex
	base  context.Context
}

var _ Agent = (*NodeAgent)(nil)

type instance struct {
	spec   peerapi.InstanceSpec
	step   string
	errc   string
	detail string
	// cancel stops a setup that runs in the background; done closes when it has ended. Both are
	// nil when none runs.
	cancel context.CancelFunc
	done   chan struct{}
}

// instanceFile is replica.json.
type instanceFile struct {
	Spec   peerapi.InstanceSpec `json:"spec"`
	Step   string               `json:"step"`
	Error  string               `json:"error,omitempty"`
	Detail string               `json:"detail,omitempty"`
}

// NewNodeAgent builds the agent. Start gives it the context its background setups run under.
func NewNodeAgent(o AgentOptions) *NodeAgent {
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = 15 * time.Minute
	}
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	return &NodeAgent{o: o, insts: map[string]*instance{}, locks: map[string]*sync.Mutex{}, base: context.Background()}
}

// Start sets the context the agent's background setups run under; they stop when it ends.
func (a *NodeAgent) Start(ctx context.Context) { a.base = ctx }

func (a *NodeAgent) lockRef(ref string) func() {
	a.mu.Lock()
	m, ok := a.locks[ref]
	if !ok {
		m = &sync.Mutex{}
		a.locks[ref] = m
	}
	a.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (a *NodeAgent) self() string {
	if a.o.Members == nil {
		return ""
	}
	return a.o.Members.Self().ID
}

// checkEpoch refuses a request made under an epoch older than the node's: its sender is a leader
// that has been replaced.
func (a *NodeAgent) checkEpoch(epoch int64) error {
	if a.o.Members == nil {
		return nil
	}
	if cur := a.o.Members.Epoch(); epoch < cur {
		return fmt.Errorf("%w: the request is under epoch %d and this node is at %d", ErrStaleEpoch, epoch, cur)
	}
	return nil
}

func refOf(identifier string) (string, error) {
	ref, _, _, ok := registry.ParseReplicaIdentifier(identifier)
	if !ok {
		return "", fmt.Errorf("%w: %q is not a replica identifier", lifecycle.ErrInvalidState, identifier)
	}
	return ref, nil
}

func (a *NodeAgent) filePath(ref string) string {
	return filepath.Join(a.o.Cfg.Paths().Project(ref), "replica.json")
}

func (a *NodeAgent) persist(in *instance) {
	f := instanceFile{Spec: in.spec, Step: in.step, Error: in.errc, Detail: in.detail}
	b, err := json.Marshal(f)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(a.filePath(in.spec.Ref)), 0o750)
	}
	if err == nil {
		err = writeAtomic(a.filePath(in.spec.Ref), b)
	}
	if err != nil {
		a.o.Log.Warn("replica: could not record the setup state", "replica", in.spec.Identifier, "error", err)
	}
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// get returns the replica this process knows, or reads it from replica.json; nil when there is none.
func (a *NodeAgent) get(identifier string) *instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	if in, ok := a.insts[identifier]; ok {
		return in
	}
	ref, err := refOf(identifier)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(a.filePath(ref))
	if err != nil {
		return nil
	}
	var f instanceFile
	if json.Unmarshal(b, &f) != nil || f.Spec.Identifier != identifier {
		return nil
	}
	in := &instance{spec: f.Spec, step: f.Step, errc: f.Error, detail: f.Detail}
	a.insts[identifier] = in
	return in
}

func (a *NodeAgent) forget(identifier string) {
	a.mu.Lock()
	delete(a.insts, identifier)
	a.mu.Unlock()
}

// snapshot copies the fields of in that setup changes.
func (a *NodeAgent) snapshot(in *instance) (step, errc, detail string, running bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return in.step, in.errc, in.detail, in.cancel != nil
}

func (a *NodeAgent) setStep(in *instance, step string) {
	a.mu.Lock()
	in.step = step
	a.mu.Unlock()
	a.persist(in)
}

// fail records that the setup failed at the step it had reached. A setup that was cancelled (the
// daemon is stopping, or the replica is being removed) did not fail: it keeps the step it reached
// and resumes at the next request.
func (a *NodeAgent) fail(ctx context.Context, in *instance, err error) {
	if ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	in.errc = stepFailures[in.step]
	if in.errc == "" {
		in.errc = stepFailures[StepStarted]
	}
	in.detail = err.Error()
	a.mu.Unlock()
	a.persist(in)
	a.o.Log.Warn("replica setup failed", "replica", in.spec.Identifier, "step", in.step, "error", err)
}

// target loads what the plane needs to render the replica: the project row (with class, when the
// leader says which size to render) and, with keys, its credentials.
func (a *NodeAgent) target(ctx context.Context, identifier, class string, withKeys bool) (lifecycle.ReplicaTarget, error) {
	ref, err := refOf(identifier)
	if err != nil {
		return lifecycle.ReplicaTarget{}, err
	}
	p, err := a.o.Registry.GetProject(ctx, ref)
	if err != nil {
		return lifecycle.ReplicaTarget{}, err
	}
	if class != "" && class != p.Class {
		c, err := lifecycle.ClassFor(class)
		if err != nil {
			return lifecycle.ReplicaTarget{}, err
		}
		cp := *p
		cp.Class, cp.Limits = c.Name, c.Limits()
		p = &cp
	}
	t := lifecycle.ReplicaTarget{Identifier: identifier, Project: p}
	if withKeys {
		if t.Keys, err = a.o.Keys(ctx, ref); err != nil {
			return t, err
		}
	}
	return t, nil
}

// backupBytes is the size of the base backup a replica of spec would be seeded from, 0 when unknown.
func (a *NodeAgent) backupBytes(ctx context.Context, spec peerapi.InstanceSpec) int64 {
	bs, err := a.o.Registry.ListBackups(ctx, spec.Ref)
	if err != nil {
		return 0
	}
	for _, b := range bs { // newest first
		if b.Status != registry.BackupCompleted {
			continue
		}
		if spec.BackupID == "" || path.Base(strings.TrimRight(b.Location, "/")) == spec.BackupID {
			return b.SizeBytes
		}
	}
	return 0
}

// Ensure implements Agent. A replica this node does not know is checked (it must not be on the
// project's home, it must fit the node) and set up in the background; one it knows is reported
// as it is, so a repeated request changes nothing. A setup that failed stays failed until the
// replica is removed and added again. A replica the node has no room for is refused with ErrNoRoom
// and recorded nowhere, so that the leader can ask again when there is room.
func (a *NodeAgent) Ensure(ctx context.Context, spec peerapi.InstanceSpec) (peerapi.InstanceStatus, error) {
	ref, err := refOf(spec.Identifier)
	if err != nil {
		return peerapi.InstanceStatus{}, err
	}
	if ref != spec.Ref {
		return peerapi.InstanceStatus{}, fmt.Errorf("%w: %s is not a replica of %s", lifecycle.ErrInvalidState, spec.Identifier, spec.Ref)
	}
	if err := a.checkEpoch(spec.Epoch); err != nil {
		return peerapi.InstanceStatus{}, err
	}
	defer a.lockRef(ref)()

	if in := a.get(spec.Identifier); in != nil {
		if step, errc, _, running := a.snapshot(in); !running && errc == "" && step != StepCompleted {
			// The resume of a setup that had not got far deletes the project's directory here. The
			// registry may name this node the home by now (a switchover moved the project onto the
			// replica's node while its setup was cut short): the directory is the home's then.
			if home, err := a.isHome(ctx, ref); err != nil {
				return peerapi.InstanceStatus{}, err
			} else if home {
				return peerapi.InstanceStatus{}, fmt.Errorf("%w: %s is the home of %s now, and a replica is never on the home", lifecycle.ErrInvalidState, a.self(), ref)
			}
			a.resume(in) // interrupted by a restart of the daemon
		}
		return a.status(ctx, in), nil
	}
	p, err := a.o.Registry.GetProject(ctx, ref)
	if err != nil {
		return peerapi.InstanceStatus{}, err
	}
	if self := a.self(); self != "" && p.NodeID == self {
		return peerapi.InstanceStatus{}, fmt.Errorf("%w: %s is the home of %s, and a replica is never on the home", lifecycle.ErrInvalidState, self, ref)
	}
	if ref != config.SystemRef && p.Seq > a.o.Cfg.MaxReplicaSeq() {
		return peerapi.InstanceStatus{}, fmt.Errorf("%w: %s", lifecycle.ErrReplicaSeq, ref)
	}
	if err := a.o.Cfg.CheckReplicaPorts(); err != nil {
		return peerapi.InstanceStatus{}, err
	}
	if held := a.recorded(ref); held != "" {
		return peerapi.InstanceStatus{}, fmt.Errorf("%w: this node holds the replica %s of %s already", lifecycle.ErrInvalidState, held, ref)
	}
	if a.o.Admit != nil {
		if err := a.o.Admit(ctx, p, spec.Identifier, a.backupBytes(ctx, spec)); err != nil {
			var ce *lifecycle.CapacityError
			if errors.As(err, &ce) || errors.Is(err, lifecycle.ErrReplicaDisk) {
				return peerapi.InstanceStatus{}, fmt.Errorf("%w: %w", ErrNoRoom, err)
			}
			return peerapi.InstanceStatus{}, err
		}
	}
	in := &instance{spec: spec, step: StepStarted}
	a.mu.Lock()
	a.insts[spec.Identifier] = in
	a.mu.Unlock()
	a.persist(in)
	a.run(in, false)
	return a.status(ctx, in), nil
}

// recorded returns the identifier of the replica of ref that this node holds a record of: one it
// knows in memory, or the one replica.json names. It is empty when there is none. A node holds one
// replica of a project at most.
func (a *NodeAgent) recorded(ref string) string {
	a.mu.Lock()
	for id, in := range a.insts {
		if in.spec.Ref == ref {
			a.mu.Unlock()
			return id
		}
	}
	a.mu.Unlock()
	b, err := os.ReadFile(a.filePath(ref))
	if err != nil {
		return ""
	}
	var f instanceFile
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	return f.Spec.Identifier
}

func (a *NodeAgent) known() map[string]*instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]*instance, len(a.insts))
	for k, v := range a.insts {
		out[k] = v
	}
	return out
}

// resume restarts the setup of an instance read from replica.json.
func (a *NodeAgent) resume(in *instance) { a.run(in, true) }

// run starts the setup of in in the background.
func (a *NodeAgent) run(in *instance, resume bool) {
	ctx, cancel := context.WithCancel(a.base)
	done := make(chan struct{})
	a.mu.Lock()
	in.cancel, in.done = cancel, done
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			in.cancel, in.done = nil, nil
			a.mu.Unlock()
			cancel()
			close(done)
		}()
		a.setup(ctx, in, resume)
	}()
}

// setup carries a replica from its row to a standby that streams and serves PostgREST: the steps of
// design 2.7.2. A resumed setup that had not extracted the base backup starts from scratch (the
// partly written directory goes); one that had starts the standby.
func (a *NodeAgent) setup(ctx context.Context, in *instance, resume bool) {
	spec := in.spec
	t, err := a.target(ctx, spec.Identifier, "", true)
	if err != nil {
		a.fail(ctx, in, err)
		return
	}
	if resume && (in.step == StepDownloaded || in.step == StepReplayed) {
		err = a.o.Plane.StartReplicaDatabase(ctx, t)
	} else {
		if resume {
			if err := a.o.Plane.RemoveReplica(ctx, spec.Ref); err != nil {
				a.fail(ctx, in, err)
				return
			}
			a.setStep(in, StepStarted)
		}
		err = a.o.Plane.CreateReplica(ctx, t, lifecycle.ReplicaCreateOptions{
			Seeder: a.o.Seeder, BackupID: spec.BackupID, NoUpstream: spec.NoUpstream,
			Progress: func(s lifecycle.ReplicaStage) {
				switch s {
				case lifecycle.StageLaunched:
					a.setStep(in, StepLaunched)
				case lifecycle.StageSeeding:
					a.setStep(in, StepInitiated)
				case lifecycle.StageSeeded:
					a.setStep(in, StepDownloaded)
				}
			},
		})
	}
	if err != nil {
		a.fail(ctx, in, err)
		return
	}
	if spec.NoUpstream {
		// Nothing to stream from and no API to serve: the standby replays the archive and waits
		// to be promoted.
		a.setStep(in, StepReplayed)
		a.setStep(in, StepCompleted)
		return
	}
	if err := a.waitStreaming(ctx, t); err != nil {
		a.fail(ctx, in, err)
		return
	}
	a.setStep(in, StepReplayed)
	if err := a.o.Plane.StartReplicaAPI(ctx, t); err != nil {
		a.fail(ctx, in, err)
		return
	}
	a.setStep(in, StepCompleted)
}

// waitStreaming waits until the standby's receiver streams, which it does once replay has reached
// the end of the archive. A standby that neither streams nor replays for the stall timeout fails.
func (a *NodeAgent) waitStreaming(ctx context.Context, t lifecycle.ReplicaTarget) error {
	last, since := "", a.o.Now()
	for {
		obs := a.o.Plane.ObserveReplica(ctx, t)
		if obs.PostgresUp && obs.InRecovery {
			if obs.ReceiverStatus == "streaming" {
				return nil
			}
			if obs.ReplayLSN != last {
				last, since = obs.ReplayLSN, a.o.Now()
			}
		}
		if a.o.Now().Sub(since) > a.o.StallTimeout {
			return fmt.Errorf("the standby made no progress for %s and its receiver is %q (replayed %s)", a.o.StallTimeout, obs.ReceiverStatus, obs.ReplayLSN)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.o.Poll):
		}
	}
}

// status is what the node sees of in: the setup's step and failure, and the standby's state.
func (a *NodeAgent) status(ctx context.Context, in *instance) peerapi.InstanceStatus {
	step, errc, detail, _ := a.snapshot(in)
	st := peerapi.InstanceStatus{Identifier: in.spec.Identifier, Ref: in.spec.Ref, Role: lifecycle.ReplicaRoleAbsent,
		Step: step, Error: errc, Detail: detail, At: a.o.Now()}
	a.observe(ctx, &st)
	return st
}

// observe fills the live fields of st from the standby.
func (a *NodeAgent) observe(ctx context.Context, st *peerapi.InstanceStatus) {
	t, err := a.target(ctx, st.Identifier, "", false)
	if err != nil {
		return
	}
	obs := a.o.Plane.ObserveReplica(ctx, t)
	st.Role = obs.Role
	st.PostgresUp, st.PostgRESTReady, st.InRecovery = obs.PostgresUp, obs.PostgRESTReady, obs.InRecovery
	st.ReceiverStatus, st.ReceiveLSN, st.ReplayLSN, st.LagSeconds = obs.ReceiverStatus, obs.ReceiveLSN, obs.ReplayLSN, obs.LagSeconds
}

// Observe implements Agent.
func (a *NodeAgent) Observe(ctx context.Context, identifier string) (peerapi.InstanceStatus, error) {
	ref, err := refOf(identifier)
	if err != nil {
		return peerapi.InstanceStatus{}, err
	}
	if in := a.get(identifier); in != nil {
		return a.status(ctx, in), nil
	}
	// A replica this node holds no record of (the file was lost): report what the cluster says.
	st := peerapi.InstanceStatus{Identifier: identifier, Ref: ref, Role: lifecycle.ReplicaRoleAbsent, Step: registry.ReplicaStepRequested, At: a.o.Now()}
	a.observe(ctx, &st)
	if st.Role != lifecycle.ReplicaRoleAbsent {
		st.Step = StepCompleted
	}
	return st, nil
}

// ObserveAll observes every replica this node holds, for the report to the leader, a few at a time
// (AgentOptions.Concurrency): each observation runs SQL and HTTP probes of its own. The result is in
// the order of the identifiers.
func (a *NodeAgent) ObserveAll(ctx context.Context) []peerapi.InstanceStatus {
	ids := map[string]bool{}
	for id := range a.known() {
		ids[id] = true
	}
	if self := a.self(); self != "" {
		if rs, err := a.o.Registry.ListReplicasOn(ctx, self); err == nil {
			for _, r := range rs {
				ids[r.Identifier] = true
			}
		}
	}
	order := make([]string, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	slices.Sort(order)
	type observed struct {
		id string
		st *peerapi.InstanceStatus
	}
	got := make([]*observed, len(order))
	for i, id := range order {
		got[i] = &observed{id: id}
	}
	eachLimit(ctx, a.o.Concurrency, got, func(o *observed) {
		if st, err := a.Observe(ctx, o.id); err == nil {
			o.st = &st
		}
	})
	var out []peerapi.InstanceStatus
	for _, o := range got {
		if o.st != nil {
			out = append(out, *o.st)
		}
	}
	return out
}

// Remove implements Agent. It stops the setup if one runs, stops the units and deletes the
// replica's directory. It removes only the replica this node holds: another identifier of the
// project is refused, and so is a directory whose cluster is a primary (a promoted replica, or a
// home) and the project's home itself. Removing a replica must never remove a home.
func (a *NodeAgent) Remove(ctx context.Context, identifier string) error {
	ref, err := refOf(identifier)
	if err != nil {
		return err
	}
	// The system cluster's standby is this node's own registry, which the daemon reads. A node leaves
	// the cluster through its retirement (`supavise node rm` on the leader, `node join --reset` here),
	// never by a request for one replica.
	if ref == config.SystemRef {
		return fmt.Errorf("%w: %s is the standby of the system cluster, this node's own registry; it goes with the node when the node leaves", lifecycle.ErrInvalidState, identifier)
	}
	defer a.lockRef(ref)()
	if held := a.recorded(ref); held != "" && held != identifier {
		return fmt.Errorf("%w: this node holds the replica %s of %s, not %s", lifecycle.ErrInvalidState, held, ref, identifier)
	}
	in := a.get(identifier)
	if in != nil {
		a.mu.Lock()
		cancel, done := in.cancel, in.done
		a.mu.Unlock()
		if cancel != nil {
			cancel()
			<-done
		}
	}
	if t, err := a.target(ctx, identifier, "", false); err == nil {
		if self := a.self(); self != "" && t.Project.NodeID == self {
			return fmt.Errorf("%w: %s is the home of %s; a replica is removed from a node that is not", lifecycle.ErrInvalidState, self, ref)
		}
		if obs := a.o.Plane.ObserveReplica(ctx, t); obs.Role == lifecycle.ReplicaRolePrimary {
			return fmt.Errorf("%w: the cluster of %s here is a primary now", lifecycle.ErrInvalidState, ref)
		}
	} else if !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	// The cluster may not answer (a promoted primary that was stopped): what is on disk decides. A
	// setup that has not finished leaves a directory with no standby.signal yet, and may go.
	if in == nil || in.step == StepCompleted {
		if err := a.checkStandbyData(ref, identifier); err != nil {
			return err
		}
	}
	if err := a.o.Plane.RemoveReplica(ctx, ref); err != nil {
		return err
	}
	if err := os.Remove(a.filePath(ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	a.forget(identifier)
	return nil
}

// checkStandbyData refuses to remove ref's data directory on this node when it is not a standby of
// identifier: a cluster without standby.signal is a primary, and a standby that was set up for
// another replica is not this one's to delete. A directory with no cluster in it is left to the plane.
func (a *NodeAgent) checkStandbyData(ref, identifier string) error {
	dir := a.o.Cfg.Paths().PostgresData(ref)
	if _, err := os.Stat(filepath.Join(dir, "PG_VERSION")); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "standby.signal")); err != nil {
		return fmt.Errorf("%w: the cluster of %s here has no standby.signal, so it is a primary", lifecycle.ErrInvalidState, ref)
	}
	if name := standbyName(dir); name != "" && name != identifier {
		return fmt.Errorf("%w: the standby of %s here follows as %s, not %s", lifecycle.ErrInvalidState, ref, name, identifier)
	}
	return nil
}

// standbyName is the application_name in the primary_conninfo of dataDir's postgresql.auto.conf,
// which the replica's identifier is; empty when the file or the setting is not there.
func standbyName(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "postgresql.auto.conf"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "primary_conninfo") {
			continue
		}
		_, rest, ok := strings.Cut(line, "application_name=")
		if !ok {
			return ""
		}
		rest = strings.TrimLeft(rest, `'\`)
		if i := strings.IndexAny(rest, `'\ `); i >= 0 {
			rest = rest[:i]
		}
		return rest
	}
	return ""
}

// Do implements Agent. A start, restart, stop or promotion of a replica of a project that is homed on
// this node is refused: the units are the home's own, and the replica spec would be rendered over the
// primary. A promotion repeated after the registry moved the home here is the exception, which finds
// the cluster a primary and has nothing left to do.
func (a *NodeAgent) Do(ctx context.Context, identifier string, act peerapi.Action, req peerapi.InstanceAction) (peerapi.InstanceStatus, error) {
	ref, err := refOf(identifier)
	if err != nil {
		return peerapi.InstanceStatus{}, err
	}
	if err := a.checkEpoch(req.Epoch); err != nil {
		return peerapi.InstanceStatus{}, err
	}
	defer a.lockRef(ref)()
	in := a.get(identifier)
	if in != nil {
		if _, _, _, running := a.snapshot(in); running {
			return peerapi.InstanceStatus{}, fmt.Errorf("%w: %s is still being set up", lifecycle.ErrInvalidState, identifier)
		}
	}
	switch act {
	case peerapi.ActionStop:
		if err = a.notHome(ctx, ref, act); err != nil {
			break
		}
		err = a.o.Plane.StopReplica(ctx, ref)
	case peerapi.ActionStart, peerapi.ActionRestart:
		if err = a.notHome(ctx, ref, act); err != nil {
			break
		}
		// A restart that arrives after the promotion and before the registry names the new home would
		// render the standby's spec (the replica port, hot_standby) for a cluster that is a writable
		// primary now: only a standby of this replica is started from the replica's spec.
		if err = a.checkStandbyData(ref, identifier); err != nil {
			break
		}
		var t lifecycle.ReplicaTarget
		if t, err = a.target(ctx, identifier, req.Class, true); err != nil {
			break
		}
		if act == peerapi.ActionRestart {
			if err = a.o.Plane.StopReplica(ctx, ref); err != nil {
				break
			}
		}
		err = a.o.Plane.StartReplica(ctx, t)
	case peerapi.ActionPromote:
		var t lifecycle.ReplicaTarget
		if t, err = a.target(ctx, identifier, "", true); err != nil {
			break
		}
		if self := a.self(); self != "" && t.Project.NodeID == self {
			if a.o.Plane.ObserveReplica(ctx, t).Role != lifecycle.ReplicaRolePrimary {
				err = fmt.Errorf("%w: %s is the home of %s and its cluster is no promoted replica", lifecycle.ErrInvalidState, self, ref)
			}
		} else {
			err = a.o.Plane.PromoteReplica(ctx, t, lifecycle.PromoteOptions{
				Epoch: req.Epoch, WaitLSN: req.WaitLSN, DrainArchive: req.DrainArchive,
				Timeout: time.Duration(req.TimeoutSeconds) * time.Second,
			})
		}
		if err == nil {
			// The cluster is the project's primary now: it is no replica instance any more.
			if in != nil {
				a.forget(identifier)
			}
			_ = os.Remove(a.filePath(ref))
		}
	case peerapi.ActionDemote:
		// The registry names the new home before the old one is demoted (design 2.10.3, steps 5 and 7):
		// a demotion of the node the registry still names the home would stop its primary.
		var home bool
		if home, err = a.isHome(ctx, ref); err != nil {
			break
		}
		if home {
			err = fmt.Errorf("%w: cannot demote the cluster of %s: %s is still its home in the registry; move the home first", lifecycle.ErrInvalidState, ref, a.self())
			break
		}
		var t lifecycle.ReplicaTarget
		if t, err = a.target(ctx, identifier, "", true); err != nil {
			break
		}
		if err = a.o.Plane.DemoteToReplica(ctx, t); err == nil {
			a.adopt(identifier, ref)
		}
	default:
		err = fmt.Errorf("%w: unknown action %q", lifecycle.ErrInvalidState, act)
	}
	if err != nil {
		return peerapi.InstanceStatus{}, err
	}
	return a.Observe(ctx, identifier)
}

// isHome reports whether the registry names this node the home of ref; a node with no id answers false.
func (a *NodeAgent) isHome(ctx context.Context, ref string) (bool, error) {
	self := a.self()
	if self == "" {
		return false, nil
	}
	p, err := a.o.Registry.GetProject(ctx, ref)
	if err != nil {
		return false, err
	}
	return p.NodeID == self, nil
}

// notHome refuses act on the replica of ref when the registry names this node the project's home.
func (a *NodeAgent) notHome(ctx context.Context, ref string, act peerapi.Action) error {
	home, err := a.isHome(ctx, ref)
	if err != nil {
		return err
	}
	if home {
		return fmt.Errorf("%w: cannot %s a replica of %s: %s is its home", lifecycle.ErrInvalidState, act, ref, a.self())
	}
	return nil
}

// adopt records a cluster that was demoted in place as a complete replica.
func (a *NodeAgent) adopt(identifier, ref string) {
	in := &instance{spec: peerapi.InstanceSpec{Identifier: identifier, Ref: ref}, step: StepCompleted}
	a.mu.Lock()
	a.insts[identifier] = in
	a.mu.Unlock()
	a.persist(in)
}

// StartLocal starts the replicas of this node whose setup is complete, after a restart of the
// machine or of the daemon: their units are not enabled for boot (the system cluster's standby is,
// and is left to systemd). A replica whose setup was
// interrupted resumes when the leader asks for it again (Ensure). The errors are logged.
func (a *NodeAgent) StartLocal(ctx context.Context) {
	self := a.self()
	if self == "" {
		return
	}
	rs, err := a.o.Registry.ListReplicasOn(ctx, self)
	if err != nil {
		a.o.Log.Warn("replica start: listing the replicas of this node", "error", err)
		return
	}
	var todo []registry.Replica
	for _, r := range rs {
		// The system cluster's standby is the daemon's own registry: systemd starts it before the
		// daemon, and the daemon does not restart it.
		if r.InitStep == StepCompleted && r.Ref != config.SystemRef {
			todo = append(todo, r)
		}
	}
	eachLimit(ctx, a.o.Concurrency, todo, func(r registry.Replica) {
		unlock := a.lockRef(r.Ref)
		// A cluster that is no standby of this replica was promoted, and the registry has not caught up
		// (the daemon or the machine restarted between the promotion and the move of the home): starting
		// it from the replica's spec would run a writable cluster on the replica port beside the real one.
		err := a.checkStandbyData(r.Ref, r.Identifier)
		var t lifecycle.ReplicaTarget
		if err == nil {
			t, err = a.target(ctx, r.Identifier, "", true)
		}
		if err == nil {
			err = a.o.Plane.StartReplica(ctx, t)
		}
		unlock()
		if err != nil {
			a.o.Log.Warn("replica did not start", "replica", r.Identifier, "error", err)
		}
	})
}
