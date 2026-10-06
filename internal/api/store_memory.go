package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store for tests.
type MemoryStore struct {
	mu        sync.Mutex
	nextUser  int64
	users     map[string]*User // by uuid
	sessions  map[string]LoginSession
	functions map[string]*Function // ref/slug
	files     map[string][]FunctionFile
	secrets   map[string]map[string]FunctionSecret
	content   map[string]*Content
	folders   map[string]*ContentFolder
	now       func() time.Time
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users: map[string]*User{}, sessions: map[string]LoginSession{}, functions: map[string]*Function{},
		files: map[string][]FunctionFile{}, secrets: map[string]map[string]FunctionSecret{},
		content: map[string]*Content{}, folders: map[string]*ContentFolder{}, now: time.Now,
	}
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (m *MemoryStore) UpsertUser(_ context.Context, u User) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.users[u.UserID]
	if !ok {
		m.nextUser++
		u.ID = m.nextUser
		u.CreatedAt = m.now()
		cur = &u
		m.users[u.UserID] = cur
	}
	cur.LastSeenAt = m.now()
	if cur.Email == "" {
		cur.Email = u.Email
	}
	out := *cur
	return &out, nil
}

func (m *MemoryStore) GetUser(_ context.Context, userID string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, ErrNotFound
	}
	out := *u
	return &out, nil
}

func (m *MemoryStore) GetUserByID(_ context.Context, id int64) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.ID == id {
			out := *u
			return &out, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) UpdateUser(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.users[u.UserID]
	if !ok {
		return ErrNotFound
	}
	cur.Username, cur.FirstName, cur.LastName = u.Username, u.FirstName, u.LastName
	return nil
}

func (m *MemoryStore) PutLoginSession(_ context.Context, s LoginSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.SessionID] = s
	return nil
}

func (m *MemoryStore) TakeLoginSession(_ context.Context, id string) (*LoginSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || !s.ExpiresAt.After(m.now()) {
		delete(m.sessions, id)
		return nil, ErrNotFound
	}
	delete(m.sessions, id)
	return &s, nil
}

func fkey(ref, slug string) string { return ref + "/" + slug }

func (m *MemoryStore) UpsertFunction(_ context.Context, f *Function, files []FunctionFile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := fkey(f.Ref, f.Slug)
	now := m.now()
	if cur, ok := m.functions[k]; ok {
		f.ID, f.CreatedAt, f.Version = cur.ID, cur.CreatedAt, cur.Version+1
	} else {
		f.ID, f.CreatedAt, f.Version = newUUID(), now, 1
	}
	f.UpdatedAt = now
	cp := *f
	m.functions[k] = &cp
	if files != nil {
		m.files[k] = append([]FunctionFile(nil), files...)
	}
	return nil
}

func (m *MemoryStore) ListFunctions(_ context.Context, ref string) ([]Function, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Function
	for _, f := range m.functions {
		if f.Ref == ref {
			out = append(out, *f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (m *MemoryStore) GetFunction(_ context.Context, ref, slug string) (*Function, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.functions[fkey(ref, slug)]
	if !ok {
		return nil, ErrNotFound
	}
	out := *f
	return &out, nil
}

func (m *MemoryStore) FunctionFiles(_ context.Context, ref, slug string) ([]FunctionFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.functions[fkey(ref, slug)]; !ok {
		return nil, ErrNotFound
	}
	return append([]FunctionFile(nil), m.files[fkey(ref, slug)]...), nil
}

func (m *MemoryStore) DeleteFunction(_ context.Context, ref, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := fkey(ref, slug)
	if _, ok := m.functions[k]; !ok {
		return ErrNotFound
	}
	delete(m.functions, k)
	delete(m.files, k)
	return nil
}

func (m *MemoryStore) PutFunctionSecrets(_ context.Context, ref string, sealed map[string][]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.secrets[ref] == nil {
		m.secrets[ref] = map[string]FunctionSecret{}
	}
	for n, b := range sealed {
		m.secrets[ref][n] = FunctionSecret{Name: n, Sealed: b, UpdatedAt: m.now()}
	}
	return nil
}

func (m *MemoryStore) ListFunctionSecrets(_ context.Context, ref string) ([]FunctionSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []FunctionSecret
	for _, s := range m.secrets[ref] {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryStore) DeleteFunctionSecrets(_ context.Context, ref string, names []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range names {
		delete(m.secrets[ref], n)
	}
	return nil
}

func (m *MemoryStore) ListContent(_ context.Context, ref string, q ContentQuery) ([]Content, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Content
	for _, c := range m.content {
		if c.Ref != ref || (q.Type != "" && c.Type != q.Type) || (q.Visibility != "" && c.Visibility != q.Visibility) ||
			(q.OwnerID != 0 && c.OwnerID != q.OwnerID) || (q.Favorite && !c.Favorite) ||
			(q.Name != "" && !strings.Contains(strings.ToLower(c.Name), strings.ToLower(q.Name))) ||
			(q.RootOnly && c.FolderID != nil) || (q.FolderID != nil && (c.FolderID == nil || *c.FolderID != *q.FolderID)) {
			continue
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *MemoryStore) GetContent(_ context.Context, ref, id string) (*Content, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.content[id]
	if !ok || c.Ref != ref {
		return nil, ErrNotFound
	}
	out := *c
	return &out, nil
}

func (m *MemoryStore) UpsertContent(_ context.Context, c *Content) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if c.ID == "" {
		c.ID, c.InsertedAt = newUUID(), now
	} else if cur, ok := m.content[c.ID]; ok && cur.Ref == c.Ref {
		c.InsertedAt = cur.InsertedAt
	} else {
		c.InsertedAt = now
	}
	c.UpdatedAt = now
	cp := *c
	m.content[c.ID] = &cp
	return nil
}

func (m *MemoryStore) DeleteContent(_ context.Context, ref string, ids []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, id := range ids {
		if c, ok := m.content[id]; ok && c.Ref == ref {
			delete(m.content, id)
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *MemoryStore) CountContent(_ context.Context, ref string, ownerID int64) (ContentCount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n ContentCount
	for _, c := range m.content {
		if c.Ref != ref {
			continue
		}
		if c.OwnerID == ownerID {
			if c.Visibility == "user" {
				n.Private++
			}
			if c.Favorite {
				n.Favorites++
			}
		}
		if c.Visibility != "user" {
			n.Shared++
		}
	}
	return n, nil
}

func (m *MemoryStore) ListFolders(_ context.Context, ref string, parentID *string) ([]ContentFolder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ContentFolder
	for _, f := range m.folders {
		if f.Ref != ref || (parentID == nil) != (f.ParentID == nil) || (parentID != nil && *parentID != *f.ParentID) {
			continue
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryStore) CreateFolder(_ context.Context, f *ContentFolder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f.ID, f.CreatedAt, f.UpdatedAt = newUUID(), m.now(), m.now()
	cp := *f
	m.folders[f.ID] = &cp
	return nil
}

func (m *MemoryStore) RenameFolder(_ context.Context, ref, id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.folders[id]
	if !ok || f.Ref != ref {
		return ErrNotFound
	}
	f.Name, f.UpdatedAt = name, m.now()
	return nil
}

func (m *MemoryStore) DeleteFolders(_ context.Context, ref string, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if f, ok := m.folders[id]; ok && f.Ref == ref {
			delete(m.folders, id)
			for _, c := range m.content {
				if c.FolderID != nil && *c.FolderID == id {
					c.FolderID = nil
				}
			}
		}
	}
	return nil
}
