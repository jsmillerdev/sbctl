package members

import "slices"

// Restrict returns a copy of a that knows one organization only: its Memberships holds the
// user's standing in org, or nothing when the user is not a member. Every question asked of the
// copy (Can, IsMember, OrgRole, IsOwnerAnywhere, IsOperatorAnywhere, Permissions and the project
// and organization lists built from them) is answered for org alone, so a caller that holds an
// organization-bound credential, such as an OAuth grant, cannot see or act on another
// organization however many the user belongs to.
//
// The copy is computed from the user's live roles, not stored: a role change applies to the next
// request that loads Access, and the copy never holds more than the user does. a is not changed.
func (a *Access) Restrict(org int64) *Access {
	if a == nil {
		return &Access{}
	}
	out := &Access{UserID: a.UserID}
	m := a.membership(org)
	if m == nil {
		return out
	}
	mem := *m
	mem.Scoped = make([]ProjectRole, len(m.Scoped))
	for i, r := range m.Scoped {
		r.Refs = slices.Clone(r.Refs)
		mem.Scoped[i] = r
	}
	if len(m.Scoped) == 0 {
		mem.Scoped = nil
	}
	out.Memberships = []Membership{mem}
	if ids, ok := a.ownerScoped[org]; ok {
		out.ownerScoped = map[int64][]int64{org: slices.Clone(ids)}
	}
	return out
}
