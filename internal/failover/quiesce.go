package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/registry"
)

// The leader's record of a quiesce it was asked for. The registry cannot hold it: the quiesce stops
// the system cluster, and a leader whose registry is down must still know which switchover it is
// stopped for (to authorize the survivor's undo) and where each cluster stopped (to answer a
// survivor that asks again after a crash). The record is a file next to failover.json, written
// before the first cluster stops and completed when the last one has.

// quiesceRecord is <state_dir>/failover-quiesce.json.
type quiesceRecord struct {
	// To is the node the switchover goes to, and Epoch the epoch it runs at.
	To     string    `json:"to"`
	Epoch  int64     `json:"epoch"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// Refs are the project clusters the quiesce stops. They are listed from the registry at the
	// first try, because a later try may find the registry stopped.
	Refs []string `json:"refs"`
	// Announced: maintenance was announced.
	Announced bool `json:"announced,omitempty"`
	// LSNs are the final positions by ref, set when every cluster has stopped.
	LSNs map[string]string `json:"lsns,omitempty"`
}

func quiescePath(p config.Paths) string { return filepath.Join(p.Root, "failover-quiesce.json") }

// currentQuiesce reads the record. A record of an epoch the cluster has reached or passed, or one as
// old as the maintenance announcement it made, is over: it is removed and nil returned.
func (o *Orchestrator) currentQuiesce() (*quiesceRecord, error) {
	path := quiescePath(o.d.Cfg.Paths())
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failover: %w", err)
	}
	var rec quiesceRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("failover: %s is not a quiesce record: %w", path, err)
	}
	if rec.Epoch <= o.d.Members.Epoch() || o.d.Now().Sub(rec.At) > maintenanceTTL {
		_ = os.Remove(path)
		return nil, nil
	}
	return &rec, nil
}

func (o *Orchestrator) saveQuiesce(rec *quiesceRecord) error {
	return writeJSONFile(quiescePath(o.d.Cfg.Paths()), rec)
}

func (o *Orchestrator) clearQuiesce() {
	if err := os.Remove(quiescePath(o.d.Cfg.Paths())); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.d.Log.Warn("could not remove the quiesce record", "error", err)
	}
}

// matches reports whether the record is the one a request is for.
func (r *quiesceRecord) matches(req QuiesceRequest) bool {
	return r.To == req.To && r.Epoch == req.Epoch
}

// startQuiesce makes the record of a quiesce that has not started, lists the clusters it will
// stop and announces maintenance. Nothing has stopped when it fails, so it leaves no record.
func (o *Orchestrator) startQuiesce(ctx context.Context, req QuiesceRequest) (*quiesceRecord, error) {
	st := o.store()
	self := o.self()
	ps, err := st.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	rec := &quiesceRecord{To: req.To, Epoch: req.Epoch, Reason: maintenanceReason(req.To), At: o.d.Now().UTC()}
	for _, p := range ps {
		if p.Ref != config.SystemRef && p.NodeID == self.ID {
			rec.Refs = append(rec.Refs, p.Ref)
		}
	}
	if err := o.saveQuiesce(rec); err != nil {
		return nil, fmt.Errorf("recording the quiesce: %w", err)
	}
	if err := st.SetMaintenance(ctx, registry.Maintenance{Node: self.ID, Until: o.d.Now().Add(maintenanceTTL), Reason: rec.Reason}); err != nil {
		o.clearQuiesce()
		return nil, fmt.Errorf("announcing maintenance: %w", err)
	}
	rec.Announced = true
	if err := o.saveQuiesce(rec); err != nil {
		return nil, fmt.Errorf("recording the quiesce: %w", err)
	}
	return rec, nil
}
