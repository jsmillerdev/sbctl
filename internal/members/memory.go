package members

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"
)

// Memory is the in-memory Store used by tests and by a node without a Postgres registry.
type Memory struct {
	mu sync.Mutex
	d  *memData
	// Cutoff is returned by LegacyCutoff; the zero value means no legacy accounts.
	Cutoff time.Time
}

type memData struct {
	members    map[mkey]*Member
	roles      map[int64]*ProjectRole
	nextRole   int64
	invites    map[int64]*invRow
	nextInvite int64
	mfa        map[int64]bool
	domains    map[string]DomainDefault
	checked    map[string]bool
	cutoff     *time.Time
}

type mkey struct {
	org  int64
	user string
}

type invRow struct {
	Invitation
	hash []byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{d: &memData{
		members: map[mkey]*Member{}, roles: map[int64]*ProjectRole{}, nextRole: ProjectRoleIDBase,
		invites: map[int64]*invRow{}, nextInvite: 1, mfa: map[int64]bool{}, domains: map[string]DomainDefault{},
		checked: map[string]bool{},
	}}
}

var _ Store = (*Memory)(nil)

// Update implements Store.
func (m *Memory) Update(ctx context.Context, org int64, fn func(Ops) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := m.d.clone()
	if err := fn(&memOps{d: m.d, cutoff: m.Cutoff}); err != nil {
		m.d = snap
		return err
	}
	return nil
}

func (d *memData) clone() *memData {
	c := &memData{nextRole: d.nextRole, nextInvite: d.nextInvite, members: map[mkey]*Member{}, roles: map[int64]*ProjectRole{},
		invites: map[int64]*invRow{}, mfa: map[int64]bool{}, domains: map[string]DomainDefault{}, checked: map[string]bool{}, cutoff: d.cutoff}
	for k, v := range d.members {
		x := *v
		c.members[k] = &x
	}
	for k, v := range d.roles {
		x := *v
		x.Refs = append([]string(nil), v.Refs...)
		c.roles[k] = &x
	}
	for k, v := range d.invites {
		x := *v
		x.ProjectRefs = append([]string(nil), v.ProjectRefs...)
		c.invites[k] = &x
	}
	for k, v := range d.mfa {
		c.mfa[k] = v
	}
	for k, v := range d.domains {
		c.domains[k] = v
	}
	for k, v := range d.checked {
		c.checked[k] = v
	}
	return c
}

// locked wraps every Ops call of the plain Store in the mutex.
func (m *Memory) ops() *memOps { return &memOps{d: m.d, cutoff: m.Cutoff} }

type memOps struct {
	d      *memData
	cutoff time.Time
}

func (m *Memory) GetMember(ctx context.Context, org int64, user string) (*Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().GetMember(ctx, org, user)
}
func (m *Memory) ListMembers(ctx context.Context, org int64) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().ListMembers(ctx, org)
}
func (m *Memory) MembershipsOf(ctx context.Context, user string) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().MembershipsOf(ctx, user)
}
func (m *Memory) PutMember(ctx context.Context, mb Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().PutMember(ctx, mb)
}
func (m *Memory) DeleteMember(ctx context.Context, org int64, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().DeleteMember(ctx, org, user)
}
func (m *Memory) CountOwners(ctx context.Context, org int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().CountOwners(ctx, org)
}
func (m *Memory) ProjectRoles(ctx context.Context, org int64) ([]ProjectRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().ProjectRoles(ctx, org)
}
func (m *Memory) ProjectRolesOf(ctx context.Context, user string) ([]ProjectRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().ProjectRolesOf(ctx, user)
}
func (m *Memory) GetProjectRole(ctx context.Context, org, id int64) (*ProjectRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().GetProjectRole(ctx, org, id)
}
func (m *Memory) PutProjectRole(ctx context.Context, org int64, user string, base int, refs []string) (*ProjectRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().PutProjectRole(ctx, org, user, base, refs)
}
func (m *Memory) DeleteProjectRole(ctx context.Context, org, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().DeleteProjectRole(ctx, org, id)
}
func (m *Memory) OwnerScopedRoleIDs(ctx context.Context, orgs []int64) (map[int64][]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().OwnerScopedRoleIDs(ctx, orgs)
}
func (m *Memory) CreateInvitation(ctx context.Context, inv Invitation, h []byte) (*Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().CreateInvitation(ctx, inv, h)
}
func (m *Memory) ListInvitations(ctx context.Context, org int64) ([]Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().ListInvitations(ctx, org)
}
func (m *Memory) GetInvitation(ctx context.Context, org, id int64) (*Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().GetInvitation(ctx, org, id)
}
func (m *Memory) InvitationByHash(ctx context.Context, h []byte) (*Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().InvitationByHash(ctx, h)
}
func (m *Memory) PendingInvitationsFor(ctx context.Context, email string, now time.Time) ([]Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().PendingInvitationsFor(ctx, email, now)
}
func (m *Memory) DeleteInvitation(ctx context.Context, org, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().DeleteInvitation(ctx, org, id)
}
func (m *Memory) MarkInvitationAccepted(ctx context.Context, id int64, user string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().MarkInvitationAccepted(ctx, id, user, now)
}
func (m *Memory) MFAEnforced(ctx context.Context, org int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().MFAEnforced(ctx, org)
}
func (m *Memory) SetMFAEnforced(ctx context.Context, org int64, v bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().SetMFAEnforced(ctx, org, v)
}
func (m *Memory) GetDomainDefault(ctx context.Context, d string) (*DomainDefault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().GetDomainDefault(ctx, d)
}
func (m *Memory) ListDomainDefaults(ctx context.Context) ([]DomainDefault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().ListDomainDefaults(ctx)
}
func (m *Memory) SetDomainDefault(ctx context.Context, d DomainDefault) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().SetDomainDefault(ctx, d)
}
func (m *Memory) DeleteDomainDefault(ctx context.Context, d string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().DeleteDomainDefault(ctx, d)
}
func (m *Memory) LegacyCutoff(ctx context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().LegacyCutoff(ctx)
}
func (m *Memory) LegacyChecked(ctx context.Context, u string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().LegacyChecked(ctx, u)
}
func (m *Memory) MarkLegacyChecked(ctx context.Context, u string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops().MarkLegacyChecked(ctx, u)
}

// ---- the operations ------------------------------------------------------------

func (o *memOps) GetMember(_ context.Context, org int64, user string) (*Member, error) {
	if m, ok := o.d.members[mkey{org, user}]; ok {
		c := *m
		return &c, nil
	}
	return nil, ErrNotFound
}

func (o *memOps) ListMembers(_ context.Context, org int64) ([]Member, error) {
	var out []Member
	for k, m := range o.d.members {
		if k.org == org {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].UserID < out[j].UserID
	})
	return out, nil
}

func (o *memOps) MembershipsOf(_ context.Context, user string) ([]Member, error) {
	var out []Member
	for k, m := range o.d.members {
		if k.user == user {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrgID < out[j].OrgID })
	return out, nil
}

func (o *memOps) PutMember(_ context.Context, m Member) error {
	o.d.checked[m.UserID] = true // a user with a membership is settled; see Service.Access
	k := mkey{m.OrgID, m.UserID}
	if cur, ok := o.d.members[k]; ok {
		cur.RoleID = m.RoleID
		return nil
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	o.d.members[k] = &m
	return nil
}

func (o *memOps) DeleteMember(_ context.Context, org int64, user string) error {
	k := mkey{org, user}
	if _, ok := o.d.members[k]; !ok {
		return ErrNotFound
	}
	delete(o.d.members, k)
	for id, r := range o.d.roles {
		if r.OrgID == org && r.UserID == user {
			delete(o.d.roles, id)
		}
	}
	return nil
}

func (o *memOps) CountOwners(_ context.Context, org int64) (int, error) {
	n := 0
	for k, m := range o.d.members {
		if k.org == org && m.RoleID == RoleOwner {
			n++
		}
	}
	return n, nil
}

func sortedRoles(in []*ProjectRole) []ProjectRole {
	sort.Slice(in, func(i, j int) bool { return in[i].ID < in[j].ID })
	out := make([]ProjectRole, len(in))
	for i, r := range in {
		out[i] = *r
		out[i].Refs = append([]string(nil), r.Refs...)
	}
	return out
}

func (o *memOps) ProjectRoles(_ context.Context, org int64) ([]ProjectRole, error) {
	var in []*ProjectRole
	for _, r := range o.d.roles {
		if r.OrgID == org {
			in = append(in, r)
		}
	}
	return sortedRoles(in), nil
}

func (o *memOps) ProjectRolesOf(_ context.Context, user string) ([]ProjectRole, error) {
	var in []*ProjectRole
	for _, r := range o.d.roles {
		if r.UserID == user {
			in = append(in, r)
		}
	}
	return sortedRoles(in), nil
}

func (o *memOps) GetProjectRole(_ context.Context, org, id int64) (*ProjectRole, error) {
	if r, ok := o.d.roles[id]; ok && r.OrgID == org {
		c := *r
		c.Refs = append([]string(nil), r.Refs...)
		return &c, nil
	}
	return nil, ErrNotFound
}

func (o *memOps) PutProjectRole(_ context.Context, org int64, user string, base int, refs []string) (*ProjectRole, error) {
	if _, ok := o.d.members[mkey{org, user}]; !ok {
		return nil, ErrNotFound
	}
	var cur *ProjectRole
	for _, r := range o.d.roles {
		if r.OrgID == org && r.UserID == user && r.BaseRoleID == base {
			cur = r
		}
	}
	refs = dedupe(refs)
	if len(refs) == 0 {
		if cur != nil {
			delete(o.d.roles, cur.ID)
		}
		return nil, nil
	}
	if cur == nil {
		cur = &ProjectRole{ID: o.d.nextRole, OrgID: org, UserID: user, BaseRoleID: base}
		o.d.nextRole++
		o.d.roles[cur.ID] = cur
	}
	cur.Refs = refs
	c := *cur
	c.Refs = append([]string(nil), refs...)
	return &c, nil
}

func (o *memOps) DeleteProjectRole(_ context.Context, org, id int64) error {
	if r, ok := o.d.roles[id]; ok && r.OrgID == org {
		delete(o.d.roles, id)
		return nil
	}
	return ErrNotFound
}

func (o *memOps) OwnerScopedRoleIDs(_ context.Context, orgs []int64) (map[int64][]int64, error) {
	want := map[int64]bool{}
	for _, id := range orgs {
		want[id] = true
	}
	out := map[int64][]int64{}
	for _, r := range o.d.roles {
		if want[r.OrgID] && r.BaseRoleID == RoleOwner {
			out[r.OrgID] = append(out[r.OrgID], r.ID)
		}
	}
	for _, i := range o.d.invites {
		if want[i.OrgID] && i.AcceptedAt == nil && i.RoleID == RoleOwner && len(i.ProjectRefs) > 0 {
			out[i.OrgID] = append(out[i.OrgID], i.ListedRoleID())
		}
	}
	return out, nil
}

func (o *memOps) CreateInvitation(_ context.Context, inv Invitation, hash []byte) (*Invitation, error) {
	for id, i := range o.d.invites {
		if i.OrgID == inv.OrgID && i.Email == inv.Email && i.AcceptedAt == nil {
			delete(o.d.invites, id)
		}
	}
	inv.ID = o.d.nextInvite
	o.d.nextInvite++
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now()
	}
	inv.ProjectRefs = append([]string(nil), inv.ProjectRefs...)
	o.d.invites[inv.ID] = &invRow{Invitation: inv, hash: append([]byte(nil), hash...)}
	c := inv
	return &c, nil
}

func copyInv(i *invRow) Invitation {
	c := i.Invitation
	c.ProjectRefs = append([]string(nil), i.ProjectRefs...)
	return c
}

func (o *memOps) ListInvitations(_ context.Context, org int64) ([]Invitation, error) {
	var out []Invitation
	for _, i := range o.d.invites {
		if i.OrgID == org && i.AcceptedAt == nil {
			out = append(out, copyInv(i))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (o *memOps) GetInvitation(_ context.Context, org, id int64) (*Invitation, error) {
	if i, ok := o.d.invites[id]; ok && i.OrgID == org {
		c := copyInv(i)
		return &c, nil
	}
	return nil, ErrNotFound
}

func (o *memOps) InvitationByHash(_ context.Context, hash []byte) (*Invitation, error) {
	for _, i := range o.d.invites {
		if bytes.Equal(i.hash, hash) {
			c := copyInv(i)
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (o *memOps) PendingInvitationsFor(_ context.Context, email string, now time.Time) ([]Invitation, error) {
	var out []Invitation
	for _, i := range o.d.invites {
		if i.Email == email && i.AcceptedAt == nil && i.ExpiresAt.After(now) {
			out = append(out, copyInv(i))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (o *memOps) DeleteInvitation(_ context.Context, org, id int64) error {
	if i, ok := o.d.invites[id]; ok && i.OrgID == org && i.AcceptedAt == nil {
		delete(o.d.invites, id)
		return nil
	}
	return ErrNotFound
}

func (o *memOps) MarkInvitationAccepted(_ context.Context, id int64, user string, now time.Time) error {
	i, ok := o.d.invites[id]
	if !ok || i.AcceptedAt != nil || !i.ExpiresAt.After(now) {
		return ErrNotFound
	}
	i.AcceptedAt, i.AcceptedBy = &now, user
	return nil
}

func (o *memOps) MFAEnforced(_ context.Context, org int64) (bool, error) { return o.d.mfa[org], nil }
func (o *memOps) SetMFAEnforced(_ context.Context, org int64, v bool) error {
	o.d.mfa[org] = v
	return nil
}

func (o *memOps) GetDomainDefault(_ context.Context, domain string) (*DomainDefault, error) {
	if d, ok := o.d.domains[domain]; ok {
		return &d, nil
	}
	return nil, ErrNotFound
}

func (o *memOps) ListDomainDefaults(context.Context) ([]DomainDefault, error) {
	var out []DomainDefault
	for _, d := range o.d.domains {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

func (o *memOps) SetDomainDefault(_ context.Context, d DomainDefault) error {
	if d.Created.IsZero() {
		d.Created = time.Now()
	}
	o.d.domains[d.Domain] = d
	return nil
}

func (o *memOps) DeleteDomainDefault(_ context.Context, domain string) error {
	if _, ok := o.d.domains[domain]; !ok {
		return ErrNotFound
	}
	delete(o.d.domains, domain)
	return nil
}

func (o *memOps) LegacyCutoff(context.Context) (time.Time, error) { return o.cutoff, nil }
func (o *memOps) LegacyChecked(_ context.Context, user string) (bool, error) {
	return o.d.checked[user], nil
}
func (o *memOps) MarkLegacyChecked(_ context.Context, user string) error {
	o.d.checked[user] = true
	return nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
