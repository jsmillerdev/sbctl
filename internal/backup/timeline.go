package backup

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/supavise/supavise/internal/pglsn"
)

// timelineFork is one line of a timeline history file: the timeline a branch left
// (Parent) and the WAL position where it did.
type timelineFork struct {
	Parent uint32
	Switch uint64 // LSN
}

// timelineHistory is what the archive says about the newest timeline.
type timelineHistory struct {
	Latest uint32 // 0 when the archive holds no history file: only timeline 1 ever existed
	Forks  []timelineFork
}

// latestHistory reads the newest <ref>/wal/*.history file. Recovery with
// recovery_target_timeline = 'latest' follows exactly this history.
func (s *Service) latestHistory(ctx context.Context, ref string) (timelineHistory, error) {
	objs, err := s.opt.Store.List(ctx, walDir(ref))
	if err != nil {
		return timelineHistory{}, err
	}
	var h timelineHistory
	for _, o := range objs {
		name := strings.TrimSuffix(strings.TrimPrefix(o.Key, walDir(ref)), ".zst")
		w, ok := parseWALName(name)
		if ok && w.Kind == "history" && w.Timeline > h.Latest {
			h.Latest = w.Timeline
		}
	}
	if h.Latest == 0 {
		return h, nil
	}
	rc, err := s.opt.Store.Get(ctx, walKey(ref, fmt.Sprintf("%08X.history", h.Latest)))
	if err != nil {
		return h, fmt.Errorf("backup: timeline history of %s: %w", ref, err)
	}
	defer rc.Close()
	dec, err := newDecoder(rc)
	if err != nil {
		return h, err
	}
	defer dec.Close()
	sc := bufio.NewScanner(dec)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return h, fmt.Errorf("backup: malformed timeline history line %q", line)
		}
		var parent uint32
		if _, err := fmt.Sscanf(f[0], "%d", &parent); err != nil {
			return h, fmt.Errorf("backup: malformed timeline history line %q", line)
		}
		lsn, err := pglsn.Parse(f[1])
		if err != nil {
			return h, fmt.Errorf("backup: malformed timeline history line %q: not an LSN: %s", line, f[1])
		}
		h.Forks = append(h.Forks, timelineFork{Parent: parent, Switch: lsn})
	}
	return h, sc.Err()
}

// onHistory reports whether recovery from backup m can reach the latest timeline: the
// backup is on that timeline, or on an ancestor and ended before that ancestor forked.
// A backup taken on timeline 1 after an in-place restore forked timeline 2 off earlier
// fails this test, and Postgres would stop with "requested timeline 2 is not a child of
// this server's history". When the archive has no history, or a position cannot be
// parsed, the backup is accepted: Postgres is the final judge.
func (h timelineHistory) onHistory(m *Manifest) bool {
	if h.Latest == 0 || uint32(m.Timeline) == h.Latest {
		return true
	}
	stop, err := pglsn.Parse(m.StopLSN)
	if err != nil {
		return true
	}
	for _, f := range h.Forks {
		if f.Parent == uint32(m.Timeline) {
			return stop <= f.Switch
		}
	}
	return false
}
