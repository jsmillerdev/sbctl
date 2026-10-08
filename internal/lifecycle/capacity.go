package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// NodeResources is what the machine offers the projects: its memory and its cores.
// A zero field is unknown, and the check that needs it is skipped.
type NodeResources struct {
	MemoryBytes int64
	CPUs        int
}

// DetectNode reads the node's memory and cores, with [compute] node_memory and node_cpus
// laid over what the machine reports.
func DetectNode(cfg *config.Config) NodeResources {
	n := NodeResources{MemoryBytes: machineMemory(), CPUs: runtime.NumCPU()}
	if cfg != nil {
		if v, err := config.ParseBytes(cfg.Compute.NodeMemory); err == nil && v > 0 {
			n.MemoryBytes = v
		}
		if cfg.Compute.NodeCPUs > 0 {
			n.CPUs = cfg.Compute.NodeCPUs
		}
	}
	return n
}

// machineMemory is the installed memory: MemTotal on Linux, hw.memsize on macOS (development),
// 0 when neither can be read.
func machineMemory() int64 {
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
				f := strings.Fields(rest)
				if len(f) >= 1 {
					if kb, err := strconv.ParseInt(f[0], 10, 64); err == nil {
						return kb * 1024
					}
				}
			}
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

// CapacityError is the refusal of a create or resize that the node cannot honor. The Management
// API answers it with 400 and Message, the CLI prints it.
type CapacityError struct {
	Message string
}

func (e *CapacityError) Error() string { return "lifecycle: " + e.Message }

// NoRoom is true: the node cannot hold what was asked of it. The replica controller reads it as a
// reason to wait and ask again, not as a failed setup (replicas.RoomError).
func (e *CapacityError) NoRoom() bool { return true }

// countsAgainstNode reports whether a project in status s holds, or is about to hold, its
// memory cap on the node. A paused project holds none, and a failed or removed one has no units.
func countsAgainstNode(s registry.Status) bool {
	switch s {
	case registry.StatusInactive, registry.StatusInitFailed, registry.StatusRemoved:
		return false
	}
	return true
}

// projectMemory is the memory cap a project holds: its limit when it has one, else its size's.
func projectMemory(p *registry.Project) int64 {
	if n, err := config.ParseBytes(p.Limits.MemoryMax); err == nil && n > 0 {
		return n
	}
	if c, err := ClassFor(p.Class); err == nil {
		return c.MemoryBytes
	}
	return 0
}

// Capacity is the node's room for project memory at one moment.
type Capacity struct {
	Node NodeResources
	// Overcommit is [compute] overcommit with its default applied.
	Overcommit float64
	// BudgetBytes is Node.MemoryBytes x Overcommit: the most the projects' caps may add up to
	// (0 when the node's memory is unknown, which means no limit).
	BudgetBytes int64
	// CommittedBytes is the sum of the memory caps of the projects that count (see countsAgainstNode).
	CommittedBytes int64
	// Projects is how many projects that is. The system project is not counted: it and the
	// shared services are part of what the overcommit ratio absorbs.
	Projects int
	// Replicas is how many replicas of projects homed elsewhere run here; each holds its project's
	// memory cap and is part of CommittedBytes and not of Projects.
	Replicas int
}

// holders names what the committed memory is promised to: "3 projects", or "3 projects and 1
// replica" on a node that holds replicas; with adj "other ", "3 other projects".
func (c Capacity) holders(adj string) string {
	s := fmt.Sprintf("%d %sprojects", c.Projects, adj)
	switch c.Replicas {
	case 0:
	case 1:
		s += " and 1 replica"
	default:
		s += fmt.Sprintf(" and %d replicas", c.Replicas)
	}
	return s
}

// FreeBytes is what is left of the budget (never negative; meaningless when BudgetBytes is 0).
func (c Capacity) FreeBytes() int64 { return max(c.BudgetBytes-c.CommittedBytes, 0) }

// Fits reports why a project cannot hold cl on top of the projects counted here, or nil when it
// can. Leave the project out of the sum (Engine.Capacity's exclude) when it exists already.
func (c Capacity) Fits(cl Class) error { return c.fits(cl, true, true) }

// FitsChange is Fits for a project going from one size to another: going down never needs room,
// so a node that is over its budget (it was resized, or the overcommit lowered) can still shrink a
// project, and only a dimension that grows is judged.
func (c Capacity) FitsChange(from, to Class) error {
	return c.fits(to, to.MemoryBytes > from.MemoryBytes, to.VCPUs() > from.VCPUs())
}

func (c Capacity) fits(cl Class, memory, cpu bool) error {
	if cpu && c.Node.CPUs > 0 && cl.VCPUs() > c.Node.CPUs {
		return &CapacityError{fmt.Sprintf("this node cannot run a %s project: it needs %d vCPUs and the node has %d cores", cl.Title, cl.VCPUs(), c.Node.CPUs)}
	}
	if memory && c.BudgetBytes > 0 && c.CommittedBytes+cl.MemoryBytes > c.BudgetBytes {
		return &CapacityError{fmt.Sprintf(
			"this node cannot run a %s project: it needs a memory cap of %s, and the node's budget of %s (%s of memory x %g overcommit) has %s left after %s; delete or shrink a project, pause one that is idle, or raise [compute] overcommit",
			cl.Title, sizeText(cl.MemoryBytes), sizeText(c.BudgetBytes), sizeText(c.Node.MemoryBytes), c.Overcommit, sizeText(c.FreeBytes()), c.holders("other "))}
	}
	return nil
}

// sizeText formats a size for a message: "512 MB", "7.5 GB", "32 GB".
func sizeText(b int64) string {
	if b >= gib {
		return strings.TrimSuffix(strconv.FormatFloat(float64(b)/float64(gib), 'f', 1, 64), ".0") + " GB"
	}
	return strconv.FormatInt(b/mib, 10) + " MB"
}

// Offer is one size as the node can offer it to a project.
type Offer struct {
	Class Class
	// Current is true for the size the project has now (or the default for a new project).
	Current bool
	// Fits is false when the node cannot honor the size; Reason then says why.
	Fits   bool
	Reason string
}

// capacityState carries the Engine's view of the node. Its mutex orders the capacity check and
// the registry write that makes it true, so two creates or resizes cannot both take the last room.
type capacityState struct {
	mu     sync.Mutex
	node   func() NodeResources
	remote NodeResourcer
}

// SetNode sets how the Engine reads the node's resources. Without it (nil) no capacity check is
// made: tests and callers that do not know the node run unconstrained. internal/app sets DetectNode.
func (e *Engine) SetNode(node func() NodeResources) { e.capacity.node = node }

// NodeResourcer reads the resources of the node a project is homed on when that is not this one
// (internal/placement implements it over the peer API): a resume or a resize is judged against the
// machine that runs the project, not the leader's.
type NodeResourcer interface {
	NodeResources(ctx context.Context, p *registry.Project) (NodeResources, error)
}

// SetRemoteNodes sets how the Engine reads the resources of another node. Without it a project homed
// elsewhere is not judged against its home. internal/app sets it in a cluster, before the Engine serves
// a request.
func (e *Engine) SetRemoteNodes(r NodeResourcer) { e.capacity.remote = r }

// Capacity computes the node's room for project memory, leaving out the project exclude (a
// resize judges the new size against everything else). ok is false when the Engine has no node.
func (e *Engine) Capacity(ctx context.Context, exclude string) (Capacity, bool, error) {
	if e.capacity.node == nil {
		return Capacity{}, false, nil
	}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		return Capacity{}, true, err
	}
	var rs []registry.Replica
	if e.opts.NodeID != "" {
		if rs, err = e.reg.ListReplicasOn(ctx, e.opts.NodeID); err != nil {
			return Capacity{}, true, err
		}
	}
	return ComputeNodeCapacity(e.cfg, e.capacity.node(), e.opts.NodeID, ps, rs, exclude), true, nil
}

// capacityOf is Capacity for the node p is homed on (a project that does not exist yet, p nil, is this
// node's): this one, or another whose resources the Engine asks for (SetRemoteNodes), with the
// registry's projects and replicas counted the same way. ok is false when the Engine cannot know them,
// and no check is made.
func (e *Engine) capacityOf(ctx context.Context, p *registry.Project, exclude string) (Capacity, bool, error) {
	if p == nil || e.homedHere(p) {
		return e.Capacity(ctx, exclude)
	}
	if e.capacity.node == nil || e.capacity.remote == nil {
		return Capacity{}, false, nil
	}
	res, err := e.capacity.remote.NodeResources(ctx, p)
	if err != nil {
		return Capacity{}, true, fmt.Errorf("lifecycle: read the resources of node %s: %w", p.NodeID, err)
	}
	ps, err := e.reg.ListProjects(ctx)
	if err != nil {
		return Capacity{}, true, err
	}
	rs, err := e.reg.ListReplicasOn(ctx, p.NodeID)
	if err != nil {
		return Capacity{}, true, err
	}
	return ComputeNodeCapacity(e.cfg, res, p.NodeID, ps, rs, exclude), true, nil
}

// ComputeCapacity works out the node's room from its resources and its projects, leaving out the
// project exclude. `supavise status` uses it without an Engine. Every project in ps counts: use
// ComputeNodeCapacity on a node of a cluster.
func ComputeCapacity(cfg *config.Config, n NodeResources, ps []registry.Project, exclude string) Capacity {
	return ComputeNodeCapacity(cfg, n, "", ps, nil, exclude)
}

// replicaCounts reports whether a replica in status s holds its project's memory cap on its node:
// one that failed to set up or is being removed has no units.
func replicaCounts(s string) bool {
	return s != registry.ReplicaInitError && s != string(registry.StatusGoingDown)
}

// ComputeNodeCapacity is ComputeCapacity for the node with id node (empty: every project is
// this node's): the projects homed on another node do not count, and each replica in rs (the
// replicas on this node) counts with the memory cap of its project, which is the size its spec is
// rendered from. A replica of the project exclude is left out with the project, and so is the
// system cluster's standby. A replica of a paused project counts: pausing a project stops its own
// units and not its replicas, which keep their memory.
func ComputeNodeCapacity(cfg *config.Config, n NodeResources, node string, ps []registry.Project, rs []registry.Replica, exclude string) Capacity {
	c := Capacity{Node: n, Overcommit: cfg.Compute.OvercommitRatio()}
	c.BudgetBytes = int64(float64(n.MemoryBytes) * c.Overcommit)
	byRef := make(map[string]*registry.Project, len(ps))
	for i := range ps {
		p := &ps[i]
		byRef[p.Ref] = p
		if p.Ref == config.SystemRef || p.Ref == exclude || !countsAgainstNode(p.Status) {
			continue
		}
		if node != "" && p.NodeID != "" && p.NodeID != node {
			continue
		}
		c.CommittedBytes += projectMemory(p)
		c.Projects++
	}
	for _, r := range rs {
		p := byRef[r.Ref]
		if p == nil || p.Ref == config.SystemRef || p.Ref == exclude || p.Status == registry.StatusRemoved || !replicaCounts(r.Status) {
			continue
		}
		c.CommittedBytes += projectMemory(p)
		c.Replicas++
	}
	return c
}

// Summary is the one-line account of the node's room, as `supavise status` shows it.
func (c Capacity) Summary() string {
	if c.BudgetBytes == 0 {
		return fmt.Sprintf("%s of project memory caps promised to %s; the node's memory is unknown", sizeText(c.CommittedBytes), c.holders(""))
	}
	return fmt.Sprintf("%s of %s project memory caps promised to %s (%s of memory x %g overcommit); %d cores",
		sizeText(c.CommittedBytes), sizeText(c.BudgetBytes), c.holders(""), sizeText(c.Node.MemoryBytes), c.Overcommit, c.Node.CPUs)
}

// Over reports whether the caps already promised exceed the budget (sizes were raised by
// hand, the overcommit ratio was lowered, or the machine shrank).
func (c Capacity) Over() bool { return c.BudgetBytes > 0 && c.CommittedBytes > c.BudgetBytes }

// Offers lists every size with whether the node can give it to ref now (ref "" is a project
// that does not exist yet). Sizes below the current one always fit.
func (e *Engine) Offers(ctx context.Context, ref string) ([]Offer, error) {
	have := ""
	var p *registry.Project
	if ref != "" {
		var err error
		if p, err = e.reg.GetProject(ctx, ref); err != nil {
			return nil, err
		}
		have = p.Class
		if c, err := ClassFor(have); err == nil {
			have = c.Name
		}
	}
	cp, known, err := e.capacityOf(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	var out []Offer
	for _, cl := range sizes {
		o := Offer{Class: cl, Current: cl.Name == have || (have == "" && cl.Name == DefaultClass), Fits: true}
		if known && !o.Current {
			// The project's own cap is left out of the sum, so cl is judged on top of everyone else.
			if err := cp.Fits(cl); err != nil {
				var ce *CapacityError
				if errors.As(err, &ce) {
					o.Fits, o.Reason = false, ce.Message
				}
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// lockCapacity takes the lock that orders every capacity check with the registry write that makes
// it true: the Engine's mutex, and with the Postgres registry a node-wide advisory lock as well,
// so that `supavise projects resize` and the daemon (two processes, each with an Engine) cannot
// both take the last room. It is always taken after a project's own lock and held only for the
// check and one registry write.
func (e *Engine) lockCapacity(ctx context.Context) (func(), error) {
	e.capacity.mu.Lock()
	ap, ok := e.reg.(advisoryPool)
	if !ok || ap.Pool() == nil {
		return e.capacity.mu.Unlock, nil
	}
	conn, err := pgx.ConnectConfig(ctx, ap.Pool().Config().ConnConfig)
	if err != nil {
		e.capacity.mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock the node's capacity: %w", err)
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext('supavise:capacity'))`); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		e.capacity.mu.Unlock()
		return nil, fmt.Errorf("lifecycle: lock the node's capacity: %w", err)
	}
	return func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(cctx) // ends the session, which drops the advisory lock
		e.capacity.mu.Unlock()
	}, nil
}

// holdCapacity judges a create that names its size (CreateRequest.Class is not empty) against
// the node, and returns the release of the lock that keeps another create or resize from taking
// the room before the new row counts. Without a node (SetNode) or an explicit size it checks nothing:
// a plain create takes the default size and is never refused. Branches and restores as a new
// project always name their size (the branch's, the original's), so the node judges them.
func (e *Engine) holdCapacity(ctx context.Context, req CreateRequest, size Class) (release func(), err error) {
	noop := func() {}
	if req.Class == "" || e.capacity.node == nil {
		return noop, nil
	}
	unlock, err := e.lockCapacity(ctx)
	if err != nil {
		return noop, err
	}
	var once sync.Once
	release = func() { once.Do(unlock) }
	cp, known, err := e.Capacity(ctx, "")
	if err == nil && known {
		err = cp.Fits(size)
	}
	if err != nil {
		release()
		return noop, err
	}
	return release, nil
}

// holdResume judges a paused project coming back against the node it is homed on, as an upsize is
// judged: its cap must fit on top of the projects that run. A paused project holds no room, so without this
// a resize could take the room of a paused project and the resume would put the node over its
// budget. The returned release keeps the room until the status that counts it is written.
func (e *Engine) holdResume(ctx context.Context, p *registry.Project) (release func(), err error) {
	noop := func() {}
	if e.capacity.node == nil {
		return noop, nil
	}
	unlock, err := e.lockCapacity(ctx)
	if err != nil {
		return noop, err
	}
	var once sync.Once
	release = func() { once.Do(unlock) }
	cp, known, err := e.capacityOf(ctx, p, p.Ref)
	if err == nil && known {
		cl, cerr := ClassFor(p.Class)
		if cerr != nil {
			cl = Class{Title: p.Class} // a class no migration renamed: judge the memory it holds
		}
		if n := projectMemory(p); n > 0 {
			cl.MemoryBytes = n // limits set by hand count as they are
		}
		err = cp.Fits(cl)
	}
	if err != nil {
		release()
		return noop, err
	}
	return release, nil
}
