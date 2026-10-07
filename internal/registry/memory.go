package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Memory is an in-process Registry for tests and the exec-supervisor dev mode.
type Memory struct {
	mu       sync.Mutex
	orgs     []Organization
	projects map[string]Project
	secrets  map[string]map[string][]byte
	tokens   []AccessToken
	routes   map[string]Route
	// hostnames and vanity are the domain store (domains_mem.go), by project ref.
	hostnames map[string]CustomHostname
	vanity    map[string]VanitySubdomain
	backups   []Backup
	events    []Event
	nextID    int64
	subs      map[chan Change]struct{}
}

func NewMemory() *Memory {
	return &Memory{
		projects: map[string]Project{},
		secrets:  map[string]map[string][]byte{},
		routes:   map[string]Route{},
		subs:     map[chan Change]struct{}{},
	}
}

func (m *Memory) id() int64 { m.nextID++; return m.nextID }

// notify must be called with m.mu held.
func (m *Memory) notify(table, op, key string) {
	for ch := range m.subs {
		select {
		case ch <- Change{Table: table, Op: op, Key: key}:
		default: // best effort, like LISTEN with a slow consumer
		}
	}
}

func (m *Memory) Close() {}

func (m *Memory) CreateOrganization(_ context.Context, slug, name string) (*Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orgs {
		if o.Slug == slug {
			return nil, ErrConflict
		}
	}
	o := Organization{ID: m.id(), Slug: slug, Name: name, CreatedAt: time.Now()}
	m.orgs = append(m.orgs, o)
	return &o, nil
}

func (m *Memory) GetOrganization(_ context.Context, slug string) (*Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orgs {
		if o.Slug == slug {
			return &o, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) GetOrganizationByID(_ context.Context, id int64) (*Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orgs {
		if o.ID == id {
			return &o, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListOrganizations(context.Context) ([]Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Organization(nil), m.orgs...), nil
}

func (m *Memory) UpdateOrganization(_ context.Context, o *Organization) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.orgs {
		if m.orgs[i].ID == o.ID {
			m.orgs[i].Slug, m.orgs[i].Name = o.Slug, o.Name
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) DeleteOrganization(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.projects {
		if p.OrgID == id {
			return fmt.Errorf("%w: projects_org_id_fkey", ErrConflict)
		}
	}
	for i := range m.orgs {
		if m.orgs[i].ID == id {
			m.orgs = append(m.orgs[:i], m.orgs[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func cloneProject(p Project) Project {
	v := make(map[string]string, len(p.Versions))
	for k, s := range p.Versions {
		v[k] = s
	}
	p.Versions = v
	if p.Branch != nil {
		b := *p.Branch
		p.Branch = &b
	}
	return p
}

func (m *Memory) CreateProject(_ context.Context, p *Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[p.Ref]; ok {
		return ErrConflict
	}
	if b := p.Branch; b != nil {
		if b.ParentRef == "" || b.Name == "" || b.ID == "" || b.ParentRef == p.Ref {
			return fmt.Errorf("registry: a branch needs an id, a parent and a name")
		}
		if _, ok := m.projects[b.ParentRef]; !ok {
			return fmt.Errorf("registry: parent %s: %w", b.ParentRef, ErrNotFound)
		}
		for _, q := range m.projects {
			if q.Branch != nil && (q.Branch.ID == b.ID || (q.Branch.ParentRef == b.ParentRef && q.Branch.Name == b.Name)) {
				return ErrConflict
			}
		}
	}
	used := map[int]bool{}
	for _, q := range m.projects {
		used[q.Seq] = true
	}
	if p.Seq == 0 && p.Ref != "system" {
		for s := 1; ; s++ {
			if !used[s] {
				p.Seq = s
				break
			}
		}
	} else if used[p.Seq] {
		return ErrConflict
	}
	if p.Engine == "" {
		p.Engine = EnginePostgres
	}
	if p.Region == "" {
		p.Region = "local"
	}
	if p.Class == "" {
		p.Class = "default"
	}
	if p.Status == "" {
		p.Status = StatusComingUp
	}
	if p.Versions == nil {
		p.Versions = map[string]string{}
	}
	now := time.Now()
	p.CreatedAt, p.UpdatedAt = now, now
	m.projects[p.Ref] = cloneProject(*p)
	m.notify("projects", "insert", p.Ref)
	return nil
}

func (m *Memory) GetProject(_ context.Context, ref string) (*Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[ref]
	if !ok {
		return nil, ErrNotFound
	}
	p = cloneProject(p)
	return &p, nil
}

func (m *Memory) ListProjects(context.Context) ([]Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Project, 0, len(m.projects))
	for _, p := range m.projects {
		out = append(out, cloneProject(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *Memory) UpdateProject(_ context.Context, p *Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.projects[p.Ref]
	if !ok {
		return ErrNotFound
	}
	cur.Name, cur.Region, cur.Class, cur.Status, cur.Versions, cur.Limits = p.Name, p.Region, p.Class, p.Status, p.Versions, p.Limits
	cur.UpdatedAt = time.Now()
	m.projects[p.Ref] = cloneProject(cur)
	*p = cloneProject(cur)
	m.notify("projects", "update", p.Ref)
	return nil
}

// UpdateBranch implements Registry.
func (m *Memory) UpdateBranch(_ context.Context, ref string, b *BranchInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.projects[ref]
	if !ok || cur.Branch == nil {
		return ErrNotFound
	}
	for _, q := range m.projects {
		if q.Ref != ref && q.Branch != nil && q.Branch.ParentRef == cur.Branch.ParentRef && q.Branch.Name == b.Name {
			return ErrConflict
		}
	}
	nb := *cur.Branch
	nb.Name, nb.GitBranch, nb.Persistent, nb.WithData, nb.ExpiresAt, nb.DeletionScheduledAt = b.Name, b.GitBranch, b.Persistent, b.WithData, b.ExpiresAt, b.DeletionScheduledAt
	nb.NotifyURL, nb.State, nb.Detail, nb.CloneMethod, nb.ReviewRequestedAt = b.NotifyURL, b.State, b.Detail, b.CloneMethod, b.ReviewRequestedAt
	cur.Branch, cur.UpdatedAt = &nb, time.Now()
	m.projects[ref] = cloneProject(cur)
	m.notify("projects", "update", ref)
	return nil
}

// SetBranchEgress implements Registry.
func (m *Memory) SetBranchEgress(_ context.Context, ref, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.projects[ref]
	if !ok || cur.Branch == nil {
		return ErrNotFound
	}
	if cur.Branch.Egress != from {
		return fmt.Errorf("%w: the egress policy of %s is no longer %q", ErrConflict, ref, from)
	}
	nb := *cur.Branch
	nb.Egress = to
	cur.Branch, cur.UpdatedAt = &nb, time.Now()
	m.projects[ref] = cloneProject(cur)
	m.notify("projects", "update", ref)
	return nil
}

func (m *Memory) SetProjectStatus(_ context.Context, ref string, s Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.projects[ref]
	if !ok {
		return ErrNotFound
	}
	cur.Status, cur.UpdatedAt = s, time.Now()
	m.projects[ref] = cur
	m.notify("projects", "update", ref)
	return nil
}

func (m *Memory) DeleteProject(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return ErrNotFound
	}
	for _, q := range m.projects {
		if q.Branch != nil && q.Branch.ParentRef == ref {
			return fmt.Errorf("registry: project %s still has branches: %w", ref, ErrConflict)
		}
	}
	delete(m.projects, ref)
	delete(m.secrets, ref)
	delete(m.hostnames, ref)
	delete(m.vanity, ref)
	for h, r := range m.routes {
		if r.Ref == ref {
			delete(m.routes, h)
			m.notify("routes", "delete", h)
		}
	}
	m.notify("projects", "delete", ref)
	return nil
}

func (m *Memory) PutSecret(_ context.Context, ref, name string, sealed []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return ErrNotFound
	}
	if m.secrets[ref] == nil {
		m.secrets[ref] = map[string][]byte{}
	}
	m.secrets[ref][name] = bytes.Clone(sealed)
	m.notify("project_secrets", "update", ref)
	return nil
}

// PutSecretIfAbsent implements SecretCreator.
func (m *Memory) PutSecretIfAbsent(_ context.Context, ref, name string, sealed []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[ref]; !ok {
		return false, ErrNotFound
	}
	if _, ok := m.secrets[ref][name]; ok {
		return false, nil
	}
	if m.secrets[ref] == nil {
		m.secrets[ref] = map[string][]byte{}
	}
	m.secrets[ref][name] = bytes.Clone(sealed)
	m.notify("project_secrets", "update", ref)
	return true, nil
}

func (m *Memory) GetSecret(_ context.Context, ref, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.secrets[ref][name]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(b), nil
}

func (m *Memory) GetSecrets(_ context.Context, ref string) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]byte{}
	for n, b := range m.secrets[ref] {
		out[n] = bytes.Clone(b)
	}
	return out, nil
}

func (m *Memory) CreateAccessToken(_ context.Context, t *AccessToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.tokens {
		if bytes.Equal(x.Hash, t.Hash) {
			return ErrConflict
		}
	}
	t.ID, t.CreatedAt = m.id(), time.Now()
	m.tokens = append(m.tokens, *t)
	return nil
}

func (m *Memory) GetAccessTokenByHash(_ context.Context, hash []byte) (*AccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if bytes.Equal(t.Hash, hash) {
			return &t, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListAccessTokens(_ context.Context, userID string) ([]AccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AccessToken
	for _, t := range m.tokens {
		if t.UserID == userID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *Memory) TouchAccessToken(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tokens {
		if m.tokens[i].ID == id {
			m.tokens[i].LastUsedAt = &at
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) DeleteAccessToken(_ context.Context, userID string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, t := range m.tokens {
		if t.ID == id && t.UserID == userID {
			m.tokens = append(m.tokens[:i], m.tokens[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) PutRoute(_ context.Context, r Route) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[r.Ref]; !ok {
		return ErrNotFound
	}
	if r.Kind == "" {
		r.Kind = "api"
	}
	if old, ok := m.routes[r.Host]; ok {
		r.CreatedAt = old.CreatedAt
	} else {
		r.CreatedAt = time.Now()
	}
	m.routes[r.Host] = r
	m.notify("routes", "update", r.Host)
	return nil
}

func (m *Memory) DeleteRoute(_ context.Context, host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.routes[host]; !ok {
		return ErrNotFound
	}
	delete(m.routes, host)
	m.notify("routes", "delete", host)
	return nil
}

func (m *Memory) ListRoutes(context.Context) ([]Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Route, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func (m *Memory) CreateBackup(_ context.Context, b *Backup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.Kind == "" {
		b.Kind = "base"
	}
	if b.Status == "" {
		b.Status = BackupRunning
	}
	if b.StartedAt.IsZero() {
		b.StartedAt = time.Now()
	}
	b.ID = m.id()
	m.backups = append(m.backups, *b)
	return nil
}

func (m *Memory) UpdateBackup(_ context.Context, b *Backup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.backups {
		if m.backups[i].ID == b.ID {
			ref, kind, started := m.backups[i].Ref, m.backups[i].Kind, m.backups[i].StartedAt
			m.backups[i] = *b
			m.backups[i].Ref, m.backups[i].Kind, m.backups[i].StartedAt = ref, kind, started
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) ListBackups(_ context.Context, ref string) ([]Backup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Backup
	for _, b := range m.backups {
		if b.Ref == ref {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

func (m *Memory) DeleteBackup(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, b := range m.backups {
		if b.ID == id {
			m.backups = append(m.backups[:i], m.backups[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) AppendEvent(_ context.Context, ref, kind string, payload any) error {
	b := []byte("{}")
	if payload != nil {
		var err error
		if b, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, Event{ID: m.id(), Ref: ref, Kind: kind, Payload: b, CreatedAt: time.Now()})
	return nil
}

func (m *Memory) ListEvents(_ context.Context, ref string, limit int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	var out []Event
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		if ref == "" || m.events[i].Ref == ref {
			out = append(out, m.events[i])
		}
	}
	return out, nil
}

func (m *Memory) Subscribe(ctx context.Context) (<-chan Change, error) {
	ch := make(chan Change, 64)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		delete(m.subs, ch)
		close(ch)
		m.mu.Unlock()
	}()
	return ch, nil
}
