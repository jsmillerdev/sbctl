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

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/registry"
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
			"this node cannot run a %s project: it needs a memory cap of %s, and the node's budget of %s (%s of memory x %g overcommit) has %s left after %d other projects; delete or shrink a project, pause one that is idle, or raise [compute] overcommit",
			cl.Title, sizeText(cl.MemoryBytes), sizeText(c.BudgetBytes), sizeText(c.Node.MemoryBytes), c.Overcommit, sizeText(c.FreeBytes()), c.Projects)}
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
	mu   sync.Mutex
	node func() NodeResources
}

// SetNode sets how the Engine reads the node's resources. Without it (nil) no capacity check is
// made: tests and callers that do not know the node run unconstrained. internal/app sets DetectNode.
func (e *Engine) SetNode(node func() NodeResources) { e.capacity.node = node }

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
	n := e.capacity.node()
	c := Capacity{Node: n, Overcommit: e.cfg.Compute.OvercommitRatio()}
	c.BudgetBytes = int64(float64(n.MemoryBytes) * c.Overcommit)
	for i := range ps {
		p := &ps[i]
		if p.Ref == config.SystemRef || p.Ref == exclude || !countsAgainstNode(p.Status) {
			continue
		}
		c.CommittedBytes += projectMemory(p)
		c.Projects++
	}
	return c, true, nil
}

// Offers lists every size with whether the node can give it to ref now (ref "" is a project
// that does not exist yet). Sizes below the current one always fit.
func (e *Engine) Offers(ctx context.Context, ref string) ([]Offer, error) {
	have := ""
	if ref != "" {
		p, err := e.reg.GetProject(ctx, ref)
		if err != nil {
			return nil, err
		}
		have = p.Class
		if c, err := ClassFor(have); err == nil {
			have = c.Name
		}
	}
	cp, known, err := e.Capacity(ctx, ref)
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

// holdCapacity judges a create that names its size (CreateRequest.Class is not empty) against
// the node, and returns the release of the lock that keeps another create or resize from taking
// the room before the new row counts. Without a node (SetNode) or an explicit size it checks nothing.
func (e *Engine) holdCapacity(ctx context.Context, req CreateRequest, size Class) (release func(), err error) {
	noop := func() {}
	if req.Class == "" || e.capacity.node == nil {
		return noop, nil
	}
	e.capacity.mu.Lock()
	var once sync.Once
	release = func() { once.Do(e.capacity.mu.Unlock) }
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
