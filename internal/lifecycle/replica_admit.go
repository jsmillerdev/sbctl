package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/supavise/supavise/internal/registry"
)

// ErrReplicaDisk is returned by AdmitReplica when the disk cannot hold the replica.
var ErrReplicaDisk = errors.New("lifecycle: not enough free disk for a replica")

// diskError is the refusal of AdmitReplica for lack of disk. It matches ErrReplicaDisk and, like a
// *CapacityError, says it is a refusal for lack of room (NoRoom).
type diskError struct{ msg string }

func (e *diskError) Error() string { return ErrReplicaDisk.Error() + ": " + e.msg }
func (e *diskError) Unwrap() error { return ErrReplicaDisk }
func (e *diskError) NoRoom() bool  { return true }

// replicaReserve is the disk a new replica wants on top of the size of its base backup, which
// grows when it is extracted and again while it replays WAL.
const replicaReserve = 1 << 30

// replicaNeed is the free space a replica seeded from a base backup of size bytes wants: the
// backup's size plus a quarter, and the reserve (design 2.7.2).
func replicaNeed(size int64) int64 { return size + size/4 + replicaReserve }

// AdmitReplica says whether this node can take the replica identifier of p: the node's memory budget
// and cores hold p's size next to what is promised already (the replica's own row, which the
// controller wrote before it asked, does not count twice), and the disk holds a base backup of
// backupBytes with room to spare. It returns a *CapacityError or an error wrapping ErrReplicaDisk,
// and nil when the Engine does not know the node's resources. Where the free space cannot be read the
// disk check is skipped, as the restore's is.
func (e *Engine) AdmitReplica(ctx context.Context, p *registry.Project, identifier string, backupBytes int64) error {
	if e.capacity.node != nil {
		class, err := ClassFor(p.Class)
		if err != nil {
			return err
		}
		ps, err := e.reg.ListProjects(ctx)
		if err != nil {
			return err
		}
		var rs []registry.Replica
		if e.opts.NodeID != "" {
			all, err := e.reg.ListReplicasOn(ctx, e.opts.NodeID)
			if err != nil {
				return err
			}
			for _, r := range all {
				if r.Identifier != identifier {
					rs = append(rs, r)
				}
			}
		}
		if err := ComputeNodeCapacity(e.cfg, e.capacity.node(), e.opts.NodeID, ps, rs, "").Fits(class); err != nil {
			return err
		}
	}
	if backupBytes <= 0 {
		return nil
	}
	dir := e.cfg.StateDir
	free := e.freeBytes(dir)
	for free < 0 && filepath.Dir(dir) != dir {
		dir = filepath.Dir(dir)
		free = e.freeBytes(dir)
	}
	if free < 0 {
		e.log.Warn("replica disk check skipped: the free space could not be read", "dir", e.cfg.StateDir)
		return nil
	}
	if need := replicaNeed(backupBytes); free < need {
		return &diskError{fmt.Sprintf("the disk has %s free and a replica seeded from a base backup of %s needs about %s",
			humanBytes(free), humanBytes(backupBytes), humanBytes(need))}
	}
	return nil
}
