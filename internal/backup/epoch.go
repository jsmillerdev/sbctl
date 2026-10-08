package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/supavise/supavise/internal/registry"
)

var _ EpochMarkerStore = (*Service)(nil)

// markerMaxBytes bounds a marker read: the object is a few dozen bytes.
const markerMaxBytes = 64 << 10

// markerAttempts is how often a write starts over when the stored marker changes between its read
// and its conditional write.
const markerAttempts = 3

// ConditionalStore is implemented by a Store that can replace an object only when nobody changed
// it since it was read (S3 with If-Match and If-None-Match). The epoch marker uses it so that
// two nodes promoting at once cannot both write.
type ConditionalStore interface {
	Store
	// GetTagged reads key (at most markerMaxBytes) and returns its version tag. ErrNotFound for a missing key.
	GetTagged(ctx context.Context, key string) (data []byte, tag string, err error)
	// PutIf writes data under key when the object still has tag, or, for an empty tag, when no
	// object exists. ErrPreconditionFailed when it does not; ErrConditionalUnsupported when the
	// service ignores or refuses conditional writes, so the caller falls back to Put.
	PutIf(ctx context.Context, key string, data []byte, tag string) error
}

// ErrPreconditionFailed is returned by ConditionalStore.PutIf when the object is not in the state
// the caller read.
var ErrPreconditionFailed = errors.New("backup: the object changed since it was read")

// ErrConditionalUnsupported is returned by ConditionalStore.PutIf when the service does not
// implement conditional writes.
var ErrConditionalUnsupported = errors.New("backup: the store does not support conditional writes")

// ReadLeaderMarker implements EpochMarkerStore.
func (s *Service) ReadLeaderMarker(ctx context.Context) (*LeaderMarker, error) {
	m, _, err := readLeaderMarker(ctx, s.opt.Store)
	return m, err
}

// WriteLeaderMarker implements EpochMarkerStore. A marker with the same epoch and the same
// leader is written again (a resumed promotion); the same epoch under another leader is two
// nodes promoted at once and counts as a newer marker. A zero m.At is the service's clock.
func (s *Service) WriteLeaderMarker(ctx context.Context, m LeaderMarker) error {
	if m.At.IsZero() {
		m.At = s.opt.Now()
	}
	return writeLeaderMarker(ctx, s.opt.Store, m)
}

// MarkerStore returns the EpochMarkerStore over st, for a caller that has a Store and no Service
// (a node at boot, before anything else starts).
func MarkerStore(st Store) EpochMarkerStore { return markerStore{st} }

type markerStore struct{ st Store }

func (m markerStore) ReadLeaderMarker(ctx context.Context) (*LeaderMarker, error) {
	lm, _, err := readLeaderMarker(ctx, m.st)
	return lm, err
}

func (m markerStore) WriteLeaderMarker(ctx context.Context, lm LeaderMarker) error {
	if lm.At.IsZero() {
		lm.At = time.Now()
	}
	return writeLeaderMarker(ctx, m.st, lm)
}

// readLeaderMarker returns the stored marker and its version tag (empty unless st is a
// ConditionalStore), or nil when none was written. A marker that cannot be read is an error, not
// "none": it may hold a higher epoch.
func readLeaderMarker(ctx context.Context, st Store) (*LeaderMarker, string, error) {
	var data []byte
	var tag string
	var err error
	if cs, ok := st.(ConditionalStore); ok {
		data, tag, err = cs.GetTagged(ctx, LeaderMarkerKey)
	} else {
		var rc io.ReadCloser
		if rc, err = st.Get(ctx, LeaderMarkerKey); err == nil {
			data, err = io.ReadAll(io.LimitReader(rc, markerMaxBytes))
			rc.Close()
		}
	}
	if errors.Is(err, ErrNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("backup: read %s: %w", LeaderMarkerKey, err)
	}
	var m LeaderMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", fmt.Errorf("backup: %s is malformed: %w", LeaderMarkerKey, err)
	}
	if err := checkMarker(m); err != nil {
		return nil, "", fmt.Errorf("backup: %s is malformed: %w", LeaderMarkerKey, err)
	}
	return &m, tag, nil
}

func checkMarker(m LeaderMarker) error {
	if m.Epoch < 1 {
		return fmt.Errorf("epoch %d", m.Epoch)
	}
	if !registry.ValidNodeName(m.Leader) {
		return fmt.Errorf("leader %q is not a node name", m.Leader)
	}
	return nil
}

func writeLeaderMarker(ctx context.Context, st Store, m LeaderMarker) error {
	if err := checkMarker(m); err != nil {
		return fmt.Errorf("backup: leader marker: %w", err)
	}
	m.At = m.At.UTC()
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	cs, conditional := st.(ConditionalStore)
	for range markerAttempts {
		cur, tag, err := readLeaderMarker(ctx, st)
		if err != nil {
			return err
		}
		switch {
		case cur != nil && cur.Epoch > m.Epoch:
			return fmt.Errorf("%w: epoch %d (leader %s) is stored, this write is epoch %d", ErrMarkerNewer, cur.Epoch, cur.Leader, m.Epoch)
		case cur != nil && cur.Epoch == m.Epoch && cur.Leader != m.Leader:
			return fmt.Errorf("%w: epoch %d is already held by %s, this write is %s", ErrMarkerNewer, cur.Epoch, cur.Leader, m.Leader)
		}
		if !conditional {
			return st.Put(ctx, LeaderMarkerKey, bytes.NewReader(body))
		}
		switch err := cs.PutIf(ctx, LeaderMarkerKey, body, tag); {
		case err == nil:
			return nil
		case errors.Is(err, ErrPreconditionFailed):
			continue // somebody wrote between the read and the write: look again
		case errors.Is(err, ErrConditionalUnsupported):
			return st.Put(ctx, LeaderMarkerKey, bytes.NewReader(body))
		default:
			return fmt.Errorf("backup: write %s: %w", LeaderMarkerKey, err)
		}
	}
	return fmt.Errorf("backup: %s changed under %d attempts to write it", LeaderMarkerKey, markerAttempts)
}
