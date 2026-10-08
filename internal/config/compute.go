package config

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultOvercommit is the memory overcommit ratio of the [compute] section. A project's size
// is a cap (systemd MemoryMax), not a reservation, and an idle project uses a small fraction of
// it: docs/reference/footprint.md measured about 150 MB (PSS and page cache) for a project whose
// Micro cap is 1 GB, on top of about 1.5 GB for the node. Counting 1 GiB for the operating system,
// that fits about 37 idle projects in 8 GiB, 90 in 16 GiB and 200 in 32 GiB, which is what
// docs/guide.md tells an operator to expect. A ratio of 6 puts the budget at 48, 96 and 192 caps of
// 1 GB, so a node filled as the guide says never refuses a create, a branch, a restore or a resize.
const DefaultOvercommit = 6.0

// Compute is the [compute] config section: how the node decides which project sizes it can
// honor (internal/lifecycle/README.md "Compute sizes").
type Compute struct {
	// Overcommit is the ratio of the sum of the projects' memory caps to the node's memory that
	// a create or a resize may reach. Zero means DefaultOvercommit. A cap is a limit, not memory
	// held: a node whose projects are all busy at once at their caps would run out of memory at
	// any ratio above 1, which is the trade for hosting many mostly idle projects.
	Overcommit float64 `toml:"overcommit"`
	// NodeMemory overrides the memory the capacity checks assume ("16G", "8192M"); empty reads
	// it from the machine. For nodes whose /proc shows the host and not the VM or cgroup they run in.
	NodeMemory string `toml:"node_memory"`
	// NodeCPUs overrides the number of cores the capacity checks assume; zero reads it from the machine.
	NodeCPUs int `toml:"node_cpus"`
}

// OvercommitRatio returns Overcommit with the default applied.
func (c Compute) OvercommitRatio() float64 {
	if c.Overcommit > 0 {
		return c.Overcommit
	}
	return DefaultOvercommit
}

// Validate checks the section.
func (c Compute) Validate() error {
	if c.Overcommit < 0 {
		return fmt.Errorf("config: compute.overcommit must be positive (0 means %g)", DefaultOvercommit)
	}
	if c.NodeCPUs < 0 {
		return fmt.Errorf("config: compute.node_cpus cannot be negative")
	}
	if c.NodeMemory != "" {
		if n, err := ParseBytes(c.NodeMemory); err != nil || n <= 0 {
			return fmt.Errorf("config: compute.node_memory %q is not a size such as 16G or 8192M", c.NodeMemory)
		}
	}
	return nil
}

// ParseBytes parses a systemd-style size: digits with an optional K, M, G or T suffix (1024
// based, optionally followed by B or iB) or plain bytes. "infinity" and "" give an error.
func ParseBytes(s string) (int64, error) {
	v := strings.TrimSpace(s)
	v = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(v, "iB"), "ib"), "B")
	if v == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	}
	if mult != 1 {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	return n * mult, nil
}
