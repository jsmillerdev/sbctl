package projectconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/supavise/supavise/internal/secrets"
)

// Options configure a Manager.
type Options struct {
	// TemplateBaseURL is where GoTrue fetches a project's email templates:
	// http://127.0.0.1:7000/internal/templates, served by the daemon (see Templates).
	TemplateBaseURL string
	// Event records an audit event for a project (registry.AppendEvent). Optional.
	Event func(ctx context.Context, ref, kind string, payload any)
	// SigningKey returns the SAML signing key of a project (sso.EnsureSigningKey: created and
	// sealed on first use, never shared between projects). AuthEnv hands it to GoTrue as
	// GOTRUE_SAML_PRIVATE_KEY while the project has SAML enabled. Optional: without it a
	// project with SAML enabled renders no key, and GoTrue refuses to start.
	SigningKey func(ctx context.Context, ref string) (string, error)
	Log        *slog.Logger
}

// Manager reads, validates, saves and renders the settings of every project.
type Manager struct {
	store Store
	sec   secrets.Secrets
	opts  Options
	log   *slog.Logger
}

// NewManager returns a Manager over store; secrets seal the secret settings.
func NewManager(store Store, sec secrets.Secrets, opts Options) *Manager {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{store: store, sec: sec, opts: opts, log: log}
}

// State is one service's settings of one project.
type State struct {
	Ref     string
	Service Service
	// Version counts the saves; 0 means the settings were never changed.
	Version int64
	// Set holds the settings that were changed from their default, secrets in plain text.
	Set Values
	// Effective is Set on top of the schema defaults.
	Effective Values
	UpdatedAt time.Time
}

func schemaOf(svc Service) (*Schema, error) {
	switch svc {
	case Auth:
		return AuthSchema, nil
	case PostgREST:
		return PostgRESTSchema, nil
	case Realtime:
		return RealtimeSchema, nil
	case Storage:
		return StorageSchema, nil
	case Postgres:
		return PostgresSchema, nil
	case Pooler:
		return PoolerSchema, nil
	}
	return nil, fmt.Errorf("projectconfig: unknown service %q", svc)
}

// Get returns the settings of svc for ref.
func (m *Manager) Get(ctx context.Context, ref string, svc Service) (*State, error) {
	sch, err := schemaOf(svc)
	if err != nil {
		return nil, err
	}
	rec, err := m.store.Get(ctx, ref, svc)
	if err != nil {
		return nil, err
	}
	return m.state(sch, rec)
}

// CheckSaved validates the saved settings of svc as a whole under cx, the way a save of them
// would be: a nil error means they would be accepted. A project whose size is about to change
// uses it to find settings the new size cannot hold. For the pooler, a max_connections saved in
// the Postgres settings takes the place of cx.MaxConnections, because that is what the database
// will run with.
func (m *Manager) CheckSaved(ctx context.Context, ref string, svc Service, cx CrossContext) error {
	sch, err := schemaOf(svc)
	if err != nil {
		return err
	}
	if sch.Cross == nil {
		return nil
	}
	st, err := m.Get(ctx, ref, svc)
	if err != nil {
		return err
	}
	if svc == Pooler {
		if pg, err := m.Get(ctx, ref, Postgres); err == nil {
			if n, ok := pg.Set.Int("max_connections"); ok && n > 0 {
				cx.MaxConnections = n
			}
		}
	}
	return sch.Cross(st.Effective, st.Set, cx)
}

func (m *Manager) state(sch *Schema, rec *Record) (*State, error) {
	set, err := m.open(sch, rec)
	if err != nil {
		return nil, err
	}
	return &State{Ref: rec.Ref, Service: rec.Service, Version: rec.Version, Set: set, Effective: sch.effective(set), UpdatedAt: rec.UpdatedAt}, nil
}

// open decodes a record into plain values. A stored value that no longer satisfies the
// schema (the schema changed between releases) is skipped with a warning rather than
// keeping the project from starting.
func (m *Manager) open(sch *Schema, rec *Record) (Values, error) {
	set := Values{}
	for name, raw := range rec.Values {
		f, ok := sch.Field(name)
		if !ok || f.Secret {
			continue
		}
		v, err := f.Coerce(raw)
		if err != nil {
			m.log.Warn("projectconfig: ignoring a stored setting that is no longer valid", "ref", rec.Ref, "service", rec.Service, "setting", name, "error", err)
			continue
		}
		set[name] = v
	}
	for name, blob := range rec.Sealed {
		if _, ok := sch.Field(name); !ok {
			continue
		}
		pt, err := m.sec.Open(blob)
		if err != nil {
			return nil, fmt.Errorf("projectconfig: open secret setting %s of %s: %w", name, rec.Ref, err)
		}
		set[name] = string(pt)
	}
	return set, nil
}

func (s *Schema) effective(set Values) Values {
	eff := Values{}
	for i := range s.Fields {
		if f := &s.Fields[i]; f.Default != nil {
			eff[f.Name] = f.Default
		}
	}
	for k, v := range set {
		eff[k] = v
	}
	return eff
}

// Change is the outcome of Patch.
type Change struct {
	State *State
	// Changed names the settings whose value changed, sorted.
	Changed []string
	// Ignored names the keys of the patch that are not settings of the service.
	Ignored []string
}

// Patch applies patch to the settings of svc for ref. A null value returns a setting to
// its default; a secret sent back as the redaction Redact gave out is left alone (clients
// that edit a form save the fields they were shown). The result is validated as a whole
// (Schema.Cross) before anything is written; a concurrent save is retried on top of the
// newer version. Nothing is written when nothing changes.
func (m *Manager) Patch(ctx context.Context, ref string, svc Service, patch map[string]any, cx CrossContext) (*Change, error) {
	sch, err := schemaOf(svc)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		rec, err := m.store.Get(ctx, ref, svc)
		if err != nil {
			return nil, err
		}
		set, err := m.open(sch, rec)
		if err != nil {
			return nil, err
		}
		before := cloneValues(set)
		var ignored []string
		touched := map[string]bool{}
		for name, raw := range patch {
			f, ok := sch.Field(name)
			if !ok {
				ignored = append(ignored, name)
				continue
			}
			if raw == nil || (f.Secret && raw == "") {
				if sch.ResetStoresDefault && f.Default != nil {
					set[name] = f.Default
				} else {
					delete(set, name)
				}
				touched[name] = true
				continue
			}
			if f.Secret {
				cur, has := set[name].(string)
				if s, ok := raw.(string); ok && has && s == Redact(cur) {
					continue // the redaction of the stored secret came back unchanged
				}
			}
			v, err := f.validate(raw)
			if err != nil {
				return nil, err
			}
			if obj, ok := v.(map[string]any); ok && f.Kind == Object {
				if cur, has := set[name].(map[string]any); has {
					v = mergeObjects(cur, obj) // a partial object updates, it does not replace
				}
			}
			set[name] = v
			touched[name] = true
		}
		if sch.Fixup != nil {
			sch.Fixup(set, touched)
		}
		eff := sch.effective(set)
		if sch.Cross != nil {
			if err := sch.Cross(eff, set, cx); err != nil {
				return nil, err
			}
		}
		if err := m.checkRenders(ref, svc, set, rec.Version+1); err != nil {
			return nil, err
		}
		changed := diff(before, set)
		sort.Strings(ignored)
		if len(changed) == 0 {
			st, err := m.state(sch, rec)
			if err != nil {
				return nil, err
			}
			return &Change{State: st, Ignored: ignored}, nil
		}
		out := &Record{Ref: ref, Service: svc, Values: map[string]any{}, Sealed: map[string][]byte{}}
		for name, v := range set {
			f, _ := sch.Field(name)
			if !f.Secret {
				out.Values[name] = v
				continue
			}
			sealed, err := m.sec.Seal([]byte(v.(string)))
			if err != nil {
				return nil, err
			}
			out.Sealed[name] = sealed
		}
		saved, err := m.store.Put(ctx, out, rec.Version)
		if errors.Is(err, ErrConflict) && attempt < 5 {
			continue
		}
		if err != nil {
			return nil, err
		}
		st, err := m.state(sch, saved)
		if err != nil {
			return nil, err
		}
		if m.opts.Event != nil {
			m.opts.Event(ctx, ref, "settings."+string(svc)+".updated", map[string]any{"version": saved.Version, "changed": changed})
		}
		return &Change{State: st, Changed: changed, Ignored: ignored}, nil
	}
}

// mergeObjects returns base with patch laid over it, recursively.
func mergeObjects(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = mergeObjects(bm, pm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// Restore writes prior back as the settings of svc (an undo: the Management API calls it when
// applying a saved change to the running service failed). The version still moves forward.
func (m *Manager) Restore(ctx context.Context, ref string, svc Service, prior *State) error {
	sch, err := schemaOf(svc)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		rec, err := m.store.Get(ctx, ref, svc)
		if err != nil {
			return err
		}
		out := &Record{Ref: ref, Service: svc, Values: map[string]any{}, Sealed: map[string][]byte{}}
		for name, v := range prior.Set {
			f, ok := sch.Field(name)
			if !ok {
				continue
			}
			if !f.Secret {
				out.Values[name] = v
				continue
			}
			sealed, err := m.sec.Seal([]byte(v.(string)))
			if err != nil {
				return err
			}
			out.Sealed[name] = sealed
		}
		_, err = m.store.Put(ctx, out, rec.Version)
		if errors.Is(err, ErrConflict) && attempt < 5 {
			continue
		}
		return err
	}
}

func cloneValues(v Values) Values {
	out := make(Values, len(v))
	for k, x := range v {
		out[k] = x
	}
	return out
}

// diff names the settings that differ between a and b.
func diff(a, b Values) []string {
	var out []string
	for k, v := range b {
		if o, ok := a[k]; !ok || !equalValue(o, v) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func equalValue(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && fmt.Sprint(x) == fmt.Sprint(y)
	}
	return a == b
}
