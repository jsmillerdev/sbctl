package members

import (
	"context"
	"errors"
	"time"
)

// Errors of the store and the service. The API maps them to status codes.
var (
	ErrNotFound = errors.New("members: not found")
	// ErrLastOwner: the change would leave an organization without an Owner.
	ErrLastOwner = errors.New("an organization must keep at least one Owner")
	// ErrForbidden: the actor lacks the permission the change needs.
	ErrForbidden = errors.New("you do not have permission to do that")
	// ErrAlreadyMember: the invited address already belongs to the organization.
	ErrAlreadyMember = errors.New("already a member of this organization")
	// ErrInvalid is returned for a request that cannot be honored (unknown role, project
	// outside the organization); the wrapped message says why.
	ErrInvalid = errors.New("invalid request")
)

// Member is one user's membership of one organization.
type Member struct {
	OrgID  int64
	UserID string
	// RoleID is the organization-wide role (RoleOwner ...); 0 when the member holds only
	// project-scoped roles.
	RoleID    int
	CreatedAt time.Time
}

// ProjectRole is a base role a member holds on a set of projects.
type ProjectRole struct {
	ID         int64
	OrgID      int64
	UserID     string
	BaseRoleID int
	Refs       []string
}

// Invitation is an invitation of an email address to an organization.
type Invitation struct {
	ID    int64
	OrgID int64
	Email string
	// RoleID is the base role; ProjectRefs non-empty scopes it to those projects.
	RoleID      int
	ProjectRefs []string
	InvitedBy   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	AcceptedAt  *time.Time
	AcceptedBy  string
}

// ListedRoleID is the id Studio lists the invitation under: the base role id for an
// organization-wide invitation, a synthetic project-scoped role id otherwise.
func (i Invitation) ListedRoleID() int64 {
	if len(i.ProjectRefs) == 0 {
		return int64(i.RoleID)
	}
	return InvitationRoleIDBase + i.ID
}

// DomainDefault is the membership an SSO user gets on first sign-in, by email domain.
type DomainDefault struct {
	Domain  string
	OrgID   int64
	RoleID  int
	Created time.Time
}

// Ops are the store's primitive operations. They run on the pool (Store) or inside a
// transaction that holds the organization's lock (Store.Update). Every method that names a
// missing row returns ErrNotFound.
type Ops interface {
	GetMember(ctx context.Context, org int64, user string) (*Member, error)
	// ListMembers returns the members of org, oldest first.
	ListMembers(ctx context.Context, org int64) ([]Member, error)
	// MembershipsOf returns the memberships of a user in every organization.
	MembershipsOf(ctx context.Context, user string) ([]Member, error)
	// PutMember creates the membership or changes its role (RoleID 0: none), keeping
	// project-scoped roles.
	PutMember(ctx context.Context, m Member) error
	// DeleteMember removes the membership and, with it, the member's project-scoped roles.
	DeleteMember(ctx context.Context, org int64, user string) error
	// CountOwners counts the members whose organization-wide role is Owner.
	CountOwners(ctx context.Context, org int64) (int, error)

	// ProjectRoles returns the project-scoped roles of an organization (all members).
	ProjectRoles(ctx context.Context, org int64) ([]ProjectRole, error)
	// ProjectRolesOf returns the project-scoped roles of a user in every organization.
	ProjectRolesOf(ctx context.Context, user string) ([]ProjectRole, error)
	GetProjectRole(ctx context.Context, org, id int64) (*ProjectRole, error)
	// PutProjectRole creates the project-scoped role (org, user, base) or replaces its
	// projects; no refs deletes it (and returns nil). The user must be a member.
	PutProjectRole(ctx context.Context, org int64, user string, base int, refs []string) (*ProjectRole, error)
	DeleteProjectRole(ctx context.Context, org, id int64) error
	// OwnerScopedRoleIDs returns, per organization, the ids Studio lists under the Owner base
	// role with project scope: the members' project-scoped Owner roles and the pending
	// project-scoped Owner invitations (by their listed id).
	OwnerScopedRoleIDs(ctx context.Context, orgs []int64) (map[int64][]int64, error)

	// CreateInvitation stores inv with the SHA-256 of its token, replacing the pending
	// invitation of the same address and organization, and returns the stored row.
	CreateInvitation(ctx context.Context, inv Invitation, tokenHash []byte) (*Invitation, error)
	// ListInvitations returns the pending (not accepted) invitations of org, newest first.
	ListInvitations(ctx context.Context, org int64) ([]Invitation, error)
	GetInvitation(ctx context.Context, org, id int64) (*Invitation, error)
	// InvitationByHash returns the invitation whatever its state.
	InvitationByHash(ctx context.Context, hash []byte) (*Invitation, error)
	// PendingInvitationsFor returns the unexpired, unaccepted invitations of an address.
	PendingInvitationsFor(ctx context.Context, email string, now time.Time) ([]Invitation, error)
	DeleteInvitation(ctx context.Context, org, id int64) error
	// MarkInvitationAccepted records the acceptance in one conditional step; ErrNotFound when
	// the invitation is already accepted or expired at now, so a token works once.
	MarkInvitationAccepted(ctx context.Context, id int64, user string, now time.Time) error

	MFAEnforced(ctx context.Context, org int64) (bool, error)
	SetMFAEnforced(ctx context.Context, org int64, enforced bool) error

	GetDomainDefault(ctx context.Context, domain string) (*DomainDefault, error)
	ListDomainDefaults(ctx context.Context) ([]DomainDefault, error)
	SetDomainDefault(ctx context.Context, d DomainDefault) error
	DeleteDomainDefault(ctx context.Context, domain string) error

	// LegacyCutoff is when roles were introduced on this node (zero: never, nobody is a
	// legacy account). LegacyChecked and MarkLegacyChecked remember which accounts the
	// legacy rule has already looked at.
	LegacyCutoff(ctx context.Context) (time.Time, error)
	LegacyChecked(ctx context.Context, user string) (bool, error)
	MarkLegacyChecked(ctx context.Context, user string) error
}

// Store is the persistent state of the model: a Postgres implementation over the registry's
// database (registry migration 0900_members.sql) and a memory one for tests.
type Store interface {
	Ops
	// Update runs fn in a transaction that holds a lock on the organization, so two
	// concurrent changes cannot both pass a check (the last-owner rule) that either alone
	// would pass. A returned error rolls the changes back.
	Update(ctx context.Context, org int64, fn func(Ops) error) error
}
