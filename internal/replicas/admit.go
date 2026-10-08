package replicas

import (
	"context"
	"fmt"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

// MaxReplicas is how many read replicas hosted allows a project of the given compute size: none
// below small, four from small to large, five above (Studio's useCheckEligibilityDeployReplica).
// A size that is not one of ours gets none.
func MaxReplicas(class string) int {
	name, ok := lifecycle.ParseSize(class)
	if !ok {
		return 0
	}
	switch name {
	case "nano", "micro":
		return 0
	case "small", "medium", "large":
		return 4
	}
	return 5
}

// diskHeadroom is what a node keeps free beyond the room for the data of a replica.
const diskHeadroom = int64(1) << 30

// diskNeed is the free space a replica seeded from a base backup of seedBytes wants: the
// extracted data is a little larger than the stored backup, and the standby writes WAL while it replays.
func diskNeed(seedBytes int64) int64 { return seedBytes + seedBytes/4 + diskHeadroom }

// AdmitRequest asks whether Node can take one more replica.
type AdmitRequest struct {
	Node    registry.Node
	Project registry.Project
	// Hosted are the projects the node already holds room for: the ones homed there and a stand-in
	// (the parent project under the replica's identifier) for each replica that was admitted.
	Hosted []registry.Project
	// SeedBytes is the size of the base backup the replica would start from; zero when unknown.
	SeedBytes int64
}

// Admitter judges whether a node has the room for a replica: memory under the node's overcommit
// budget and disk for the seed (design 2.7.2, step 1). A refusal is the reason in words.
type Admitter interface {
	Admit(ctx context.Context, req AdmitRequest) error
}

// Room is what is known of a node's resources. A field that is not known skips its check: the
// leader knows its own machine, and a remote node refuses at launch what it cannot hold.
type Room struct {
	Resources     lifecycle.NodeResources
	FreeDiskBytes int64
	DiskKnown     bool
}

// RoomAdmitter is the Admitter over what Room says of the node. It judges memory the way a
// project create does (lifecycle.Capacity.Fits), so a replica counts as much as its primary.
type RoomAdmitter struct {
	Cfg  *config.Config
	Room func(ctx context.Context, n registry.Node) Room
}

func (a RoomAdmitter) Admit(ctx context.Context, req AdmitRequest) error {
	room := a.Room(ctx, req.Node)
	cl, err := lifecycle.ClassFor(req.Project.Class)
	if err != nil {
		return err
	}
	// A cap set by hand counts as it is, as for a project that resumes.
	if n, err := config.ParseBytes(req.Project.Limits.MemoryMax); err == nil && n > 0 {
		cl.MemoryBytes = n
	}
	if err := lifecycle.ComputeCapacity(a.Cfg, room.Resources, req.Hosted, "").Fits(cl); err != nil {
		return err
	}
	if need := diskNeed(req.SeedBytes); room.DiskKnown && room.FreeDiskBytes < need {
		return fmt.Errorf("node %s has %s of free disk and a replica of %s needs about %s", req.Node.Name,
			byteText(room.FreeDiskBytes), req.Project.Ref, byteText(need))
	}
	return nil
}

// byteText formats a size for a message: "512 MB", "7.5 GB".
func byteText(b int64) string {
	const mib, gib = int64(1) << 20, int64(1) << 30
	if b >= gib {
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gib))
	}
	return fmt.Sprintf("%d MB", b/mib)
}
