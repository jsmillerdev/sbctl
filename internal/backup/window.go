package backup

import (
	"context"
	"errors"
	"time"

	"github.com/supavise/supavise/internal/lifecycle"
)

// RestoreWindow is what a point-in-time restore of a project can reach: the dashboard's
// backup list and the span its recovery picker offers.
type RestoreWindow struct {
	// Backups are the base backups a restore can start from, oldest first: complete,
	// on the archive's timeline history, with the WAL file they start from still archived.
	Backups []Manifest
	// Earliest is when the oldest of them ended. A time target before it has no base backup.
	Earliest time.Time
	// Latest is the newest time a restore can reach. Both are zero without a usable backup.
	Latest time.Time
}

// RestoreWindow reports the span ref can be restored to. A running project can be restored
// to any time up to now, because a restore first writes a commit record on the source, switches
// WAL and waits for the archiver (flushArchive). Otherwise nothing flushes the archive, so the
// span ends with the newest archived object (a time between the last commit and that object
// can still be refused by the restore).
//
// The check matches PlanRestoreWith, which chooses the base backup of a restore.
func (s *Service) RestoreWindow(ctx context.Context, ref string, running bool) (RestoreWindow, error) {
	all, err := s.ListBackups(ctx, ref)
	if err != nil {
		return RestoreWindow{}, err
	}
	var w RestoreWindow
	if len(all) == 0 {
		return w, nil
	}
	hist, err := s.latestHistory(ctx, ref)
	if err != nil {
		return w, err
	}
	for i := range all {
		m := &all[i]
		if !hist.onHistory(m) {
			continue
		}
		if _, err := s.opt.Store.Stat(ctx, walKey(ref, m.StartWAL)); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return w, err
		}
		w.Backups = append(w.Backups, *m)
	}
	if len(w.Backups) == 0 {
		return w, nil
	}
	newest := w.Backups[len(w.Backups)-1].StopTime
	w.Earliest = w.Backups[0].StopTime
	if running {
		w.Latest = s.opt.Now().UTC()
	} else {
		w.Latest = newest
		objs, err := s.opt.Store.List(ctx, walDir(ref))
		if err != nil {
			return w, err
		}
		for _, o := range objs {
			if o.ModTime.After(w.Latest) {
				w.Latest = o.ModTime
			}
		}
	}
	if w.Latest.Before(newest) {
		w.Latest = newest
	}
	return w, nil
}

// RestoreInPlace implements lifecycle.InPlaceRestorer: the restore behind the Management
// API's restore routes, over the running project itself. A BackupID alone restores the state at
// the end of that base backup; with a Target it starts from that backup and replays to the
// time; without a BackupID the newest usable backup before Target is the base.
func (s *Service) RestoreInPlace(ctx context.Context, ref string, req lifecycle.RestoreRequest) error {
	opts := RestoreOptions{Force: true, BackupID: req.BackupID}
	if req.BackupID != "" && req.Target.IsZero() {
		opts.ToBackup = true
	}
	_, err := s.RestoreWith(ctx, ref, req.Target, "", opts)
	return err
}

var _ lifecycle.InPlaceRestorer = (*Service)(nil)
