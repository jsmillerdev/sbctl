package projectconfig

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"
)

// Service names the settings groups stored per project.
type Service string

// The services whose settings sbctl stores. The names are the values the
// project_settings.service column allows.
const (
	Auth      Service = "auth"
	PostgREST Service = "postgrest"
	Realtime  Service = "realtime"
	Storage   Service = "storage"
	Postgres  Service = "postgres"
	Pooler    Service = "pooler"
)

// Services lists every Service.
var Services = []Service{Auth, PostgREST, Realtime, Storage, Postgres, Pooler}

var (
	// ErrNotFound is returned when the project has no row.
	ErrNotFound = errors.New("projectconfig: not found")
	// ErrConflict is returned by Put when the stored version is not the expected one.
	ErrConflict = errors.New("projectconfig: the settings changed since they were read")
)

// Record is the stored overrides of one service of one project.
type Record struct {
	Ref     string
	Service Service
	// Version is 0 for a record that was never written.
	Version int64
	// Values are the plain overrides, by setting name.
	Values map[string]any
	// Sealed are the secret overrides, sealed.
	Sealed    map[string][]byte
	UpdatedAt time.Time
}

// Store persists Records. Implementations are safe for concurrent use.
type Store interface {
	// Get returns the record, or a zero-version record with empty maps when none exists.
	Get(ctx context.Context, ref string, svc Service) (*Record, error)
	// Put writes rec if the stored version equals expected (0: no row yet) and returns the
	// stored record with its new version. ErrConflict otherwise; ErrNotFound when the
	// project does not exist.
	Put(ctx context.Context, rec *Record, expected int64) (*Record, error)
}

// Memory is a Store in memory, for tests.
type Memory struct {
	mu   sync.Mutex
	rows map[string]*Record
	// Projects, when set, lists the refs Put accepts (mirrors the foreign key).
	Projects func(ref string) bool
	Now      func() time.Time
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory { return &Memory{rows: map[string]*Record{}} }

func key(ref string, svc Service) string { return ref + "/" + string(svc) }

func cloneRecord(r *Record) *Record {
	c := *r
	c.Values = maps.Clone(r.Values)
	c.Sealed = maps.Clone(r.Sealed)
	if c.Values == nil {
		c.Values = map[string]any{}
	}
	if c.Sealed == nil {
		c.Sealed = map[string][]byte{}
	}
	return &c
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, ref string, svc Service) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[key(ref, svc)]; ok {
		return cloneRecord(r), nil
	}
	return &Record{Ref: ref, Service: svc, Values: map[string]any{}, Sealed: map[string][]byte{}}, nil
}

// Put implements Store.
func (m *Memory) Put(_ context.Context, rec *Record, expected int64) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Projects != nil && !m.Projects(rec.Ref) {
		return nil, ErrNotFound
	}
	k := key(rec.Ref, rec.Service)
	var have int64
	if r, ok := m.rows[k]; ok {
		have = r.Version
	}
	if have != expected {
		return nil, ErrConflict
	}
	c := cloneRecord(rec)
	c.Version = expected + 1
	c.UpdatedAt = time.Now()
	if m.Now != nil {
		c.UpdatedAt = m.Now()
	}
	m.rows[k] = c
	return cloneRecord(c), nil
}
