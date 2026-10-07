package members

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"
)

// OrgRef names an organization the way permission entries do.
type OrgRef struct {
	ID   int64
	Slug string
	// CreatedAt is set by Service.Orgs only: the legacy rule grants Owner on organizations that
	// existed when roles were introduced, not on later ones (zero: unknown, counts as existing).
	CreatedAt time.Time
}

// DefaultInvitationTTL is how long an invitation stays valid. Hosted keeps one for 24 hours;
// supavise keeps it for a week because without a mail server the link travels by hand.
const DefaultInvitationTTL = 7 * 24 * time.Hour

// InvitationPrefix starts every invitation token.
const InvitationPrefix = "sbo_"

// Service applies the membership rules over a Store. The API server, the supavise CLI and the
// SSO workstream share it.
type Service struct {
	Store Store
	Now   func() time.Time
	Log   *slog.Logger
	// InvitationTTL overrides DefaultInvitationTTL.
	InvitationTTL time.Duration
	// Orgs lists the organizations; needed by the legacy-account rule only.
	Orgs func(ctx context.Context) ([]OrgRef, error)
	// AccountCreatedAt returns when the dashboard account of a user was created (the
	// sign-in service's record); needed by the legacy-account rule only. Nil disables it.
	AccountCreatedAt func(ctx context.Context, userID string) (time.Time, error)

	legacyMu    sync.Mutex
	legacyRetry map[string]time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// HashToken is the stored form of an invitation token.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// ---- access --------------------------------------------------------------------

// Membership is a user's standing in one organization.
type Membership struct {
	OrgID int64
	// RoleID is the organization-wide role, 0 when none.
	RoleID int
	Scoped []ProjectRole
}

// Access is what a user may do, loaded once per request. It holds no mutable state.
type Access struct {
	UserID      string
	Memberships []Membership
	ownerScoped map[int64][]int64
}

// Access loads the memberships and project-scoped roles of a user. A dashboard account that
// predates roles and has not been looked at yet becomes Owner of every organization first
// (see the legacy rule in registry migration 0900_members.sql).
func (s *Service) Access(ctx context.Context, userID string) (*Access, error) {
	ms, err := s.Store.MembershipsOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		if err := s.legacyOwner(ctx, userID); err != nil {
			return nil, err
		}
		if ms, err = s.Store.MembershipsOf(ctx, userID); err != nil {
			return nil, err
		}
	}
	a := &Access{UserID: userID}
	if len(ms) == 0 {
		return a, nil
	}
	prs, err := s.Store.ProjectRolesOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	var needOwners []int64
	for _, m := range ms {
		mem := Membership{OrgID: m.OrgID, RoleID: m.RoleID}
		isAdmin := m.RoleID == RoleAdministrator
		for _, r := range prs {
			if r.OrgID == m.OrgID {
				mem.Scoped = append(mem.Scoped, r)
				isAdmin = isAdmin || r.BaseRoleID == RoleAdministrator
			}
		}
		if isAdmin {
			needOwners = append(needOwners, m.OrgID)
		}
		a.Memberships = append(a.Memberships, mem)
	}
	if len(needOwners) > 0 {
		if a.ownerScoped, err = s.Store.OwnerScopedRoleIDs(ctx, needOwners); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// legacyOwner applies the legacy-account rule to a user without memberships.
func (s *Service) legacyOwner(ctx context.Context, userID string) error {
	if s.AccountCreatedAt == nil {
		return nil
	}
	checked, err := s.Store.LegacyChecked(ctx, userID)
	if err != nil || checked {
		return err
	}
	s.legacyMu.Lock()
	if s.legacyRetry == nil {
		s.legacyRetry = map[string]time.Time{}
	}
	if until := s.legacyRetry[userID]; s.now().Before(until) {
		s.legacyMu.Unlock()
		return nil
	}
	s.legacyMu.Unlock()
	cutoff, err := s.Store.LegacyCutoff(ctx)
	if err != nil {
		return err
	}
	if !cutoff.IsZero() {
		created, err := s.AccountCreatedAt(ctx, userID)
		if err != nil {
			// The sign-in service did not answer: try again in a minute, deny meanwhile.
			s.legacyMu.Lock()
			s.legacyRetry[userID] = s.now().Add(time.Minute)
			s.legacyMu.Unlock()
			s.log().Warn("members: legacy account check failed; the user has no access until it succeeds", "user", userID, "error", err)
			return nil
		}
		if created.Before(cutoff) && s.Orgs != nil {
			orgs, err := s.Orgs(ctx)
			if err != nil {
				return err
			}
			for _, o := range orgs {
				if !o.CreatedAt.IsZero() && !o.CreatedAt.Before(cutoff) {
					continue // created after roles: its Owners decide who joins
				}
				if err := s.Store.PutMember(ctx, Member{OrgID: o.ID, UserID: userID, RoleID: RoleOwner}); err != nil {
					return err
				}
			}
			s.log().Info("members: account that predates roles became Owner of every organization that predates roles", "user", userID)
		}
	}
	return s.Store.MarkLegacyChecked(ctx, userID)
}

func (a *Access) membership(org int64) *Membership {
	for i := range a.Memberships {
		if a.Memberships[i].OrgID == org {
			return &a.Memberships[i]
		}
	}
	return nil
}

// Membership returns the user's standing in org (a zero Membership when not a member).
func (a *Access) Membership(org int64) Membership {
	if m := a.membership(org); m != nil {
		return *m
	}
	return Membership{OrgID: org}
}

// PermissionsOfRole returns the entries of a base role without organization or project scope.
func PermissionsOfRole(role int) []Permission { return roleEntries(role, nil) }

// IsMember reports whether the user belongs to the organization.
func (a *Access) IsMember(org int64) bool { return a.membership(org) != nil }

// OrgRole is the user's organization-wide role in org (0: none or not a member).
func (a *Access) OrgRole(org int64) int {
	if m := a.membership(org); m != nil {
		return m.RoleID
	}
	return 0
}

// IsOwnerAnywhere reports whether the user holds the Owner role organization-wide in some
// organization.
func (a *Access) IsOwnerAnywhere() bool {
	for _, m := range a.Memberships {
		if m.RoleID == RoleOwner {
			return true
		}
	}
	return false
}

// Permissions returns the permission entries of the user for the given organizations, the
// list GET /platform/profile/permissions serves.
func (a *Access) Permissions(orgs []OrgRef) []Permission {
	var out []Permission
	for _, o := range orgs {
		out = append(out, a.permissionsFor(o)...)
	}
	return out
}

func (a *Access) permissionsFor(org OrgRef) []Permission {
	m := a.membership(org.ID)
	if m == nil {
		return nil
	}
	fill := func(p Permission, refs []string) Permission {
		p.OrganizationID, p.OrganizationSlug = org.ID, org.Slug
		if len(refs) > 0 {
			p.ProjectRefs = append([]string(nil), refs...)
		}
		return p
	}
	var out []Permission
	if m.RoleID != 0 {
		for _, p := range roleEntries(m.RoleID, a.ownerScoped[org.ID]) {
			out = append(out, fill(p, nil))
		}
	} else {
		for _, p := range scopedMemberEntries() {
			out = append(out, fill(p, nil))
		}
	}
	for _, r := range m.Scoped {
		for _, p := range roleEntries(r.BaseRoleID, a.ownerScoped[org.ID]) {
			out = append(out, fill(p, r.Refs))
		}
	}
	return out
}

// Can reports whether the user may perform action on resource in org, on projectRef when it
// is not empty. data is merged into the condition data (see Check).
func (a *Access) Can(org OrgRef, projectRef, action, resource string, data map[string]any) bool {
	return Check(a.permissionsFor(org), action, resource, data, org.Slug, projectRef)
}

// CanRole is Can for the member-management resources, which are conditioned on a role id.
func (a *Access) CanRole(org OrgRef, action, resource string, roleID int64) bool {
	return a.Can(org, "", action, resource, map[string]any{"resource": map[string]any{"role_id": float64(roleID)}})
}

// ---- reading -------------------------------------------------------------------

// MemberView is a member with the role ids Studio lists: the organization-wide role and
// the ids of the project-scoped roles.
type MemberView struct {
	UserID    string
	RoleIDs   []int64
	CreatedAt time.Time
}

// Members lists the members of an organization, oldest first.
func (s *Service) Members(ctx context.Context, org int64) ([]MemberView, error) {
	ms, err := s.Store.ListMembers(ctx, org)
	if err != nil {
		return nil, err
	}
	prs, err := s.Store.ProjectRoles(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]MemberView, 0, len(ms))
	for _, m := range ms {
		v := MemberView{UserID: m.UserID, CreatedAt: m.CreatedAt, RoleIDs: []int64{}}
		if m.RoleID != 0 {
			v.RoleIDs = append(v.RoleIDs, int64(m.RoleID))
		}
		for _, r := range prs {
			if r.UserID == m.UserID {
				v.RoleIDs = append(v.RoleIDs, r.ID)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// ScopedRole is a project-scoped role as GET .../roles lists it: a member's role or a
// pending invitation.
type ScopedRole struct {
	ID         int64
	BaseRoleID int
	Refs       []string
}

// ScopedRoles lists the project-scoped roles of an organization: those of its members and
// those of its pending project-scoped invitations.
func (s *Service) ScopedRoles(ctx context.Context, org int64) ([]ScopedRole, error) {
	prs, err := s.Store.ProjectRoles(ctx, org)
	if err != nil {
		return nil, err
	}
	out := make([]ScopedRole, 0, len(prs))
	for _, r := range prs {
		out = append(out, ScopedRole{ID: r.ID, BaseRoleID: r.BaseRoleID, Refs: r.Refs})
	}
	invs, err := s.Store.ListInvitations(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, i := range invs {
		if len(i.ProjectRefs) > 0 {
			out = append(out, ScopedRole{ID: i.ListedRoleID(), BaseRoleID: i.RoleID, Refs: i.ProjectRefs})
		}
	}
	return out, nil
}

// ---- role changes --------------------------------------------------------------

func need(actor *Access, org OrgRef, action, resource string, roleID int64) error {
	if actor == nil || actor.CanRole(org, action, resource, roleID) {
		return nil
	}
	return ErrForbidden
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// mayManage checks that actor may take away every role the member holds now: changing what a
// member may do removes what they had, so an Administrator cannot touch an Owner's roles in
// any direction (hosted: Administrators manage members except Owners). It returns the member's
// project-scoped roles for the caller's use.
func mayManage(ctx context.Context, ops Ops, actor *Access, org OrgRef, m *Member) ([]ProjectRole, error) {
	all, err := ops.ProjectRoles(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	var mine []ProjectRole
	for _, r := range all {
		if r.UserID == m.UserID {
			mine = append(mine, r)
		}
	}
	if m.RoleID != 0 {
		if err := need(actor, org, ActDelete, ResSubjectRoles, int64(m.RoleID)); err != nil {
			return nil, err
		}
	}
	for _, r := range mine {
		if err := need(actor, org, ActDelete, ResSubjectRoles, r.ID); err != nil {
			return nil, err
		}
	}
	return mine, nil
}

// SetOrgRole sets the organization-wide role of a member. A nil actor is the operator (the
// CLI): no permission check, the Owner rule still holds.
func (s *Service) SetOrgRole(ctx context.Context, actor *Access, org OrgRef, userID string, roleID int) error {
	if !ValidRoleID(roleID) {
		return invalid("unknown role %d", roleID)
	}
	return s.Store.Update(ctx, org.ID, func(ops Ops) error {
		m, err := ops.GetMember(ctx, org.ID, userID)
		if err != nil {
			return err
		}
		scoped, err := mayManage(ctx, ops, actor, org, m)
		if err != nil {
			return err
		}
		if m.RoleID == roleID && len(scoped) == 0 {
			return nil
		}
		if err := need(actor, org, ActCreate, ResSubjectRoles, int64(roleID)); err != nil {
			return err
		}
		if m.RoleID == RoleOwner && roleID != RoleOwner {
			if err := keepOwner(ctx, ops, org.ID, userID); err != nil {
				return err
			}
		}
		// An organization-wide role replaces the member's project-scoped roles: a member is one
		// or the other, as in Studio's role panel (which assigns the new role and removes the old).
		for _, r := range scoped {
			if err := ops.DeleteProjectRole(ctx, org.ID, r.ID); err != nil {
				return err
			}
		}
		return ops.PutMember(ctx, Member{OrgID: org.ID, UserID: userID, RoleID: roleID})
	})
}

// StandInOwnerID is the dashboard user that `supavise functions dev --token-file` makes Owner of
// every organization for as long as the command runs. The seat is a development convenience,
// not an Owner: CountOwners leaves it out, so a seat that a crash left behind cannot stand in
// for the last real Owner, and changes to it never meet the last-owner rule.
const StandInOwnerID = "00000000-0000-4000-8000-00000000f0f0"

// keepOwner refuses the change when the member being changed (user) is the last Owner.
func keepOwner(ctx context.Context, ops Ops, org int64, user string) error {
	if user == StandInOwnerID {
		return nil
	}
	n, err := ops.CountOwners(ctx, org)
	if err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastOwner
	}
	return nil
}

// AssignProjectRole gives a member a base role on projects (added to the projects the member
// already holds that role on). A project holds one role per member, so the projects are taken
// from the member's other project-scoped roles.
func (s *Service) AssignProjectRole(ctx context.Context, actor *Access, org OrgRef, userID string, base int, refs []string) error {
	refs = dedupe(refs)
	if !ValidRoleID(base) {
		return invalid("unknown role %d", base)
	}
	if len(refs) == 0 {
		return invalid("a project-scoped role needs at least one project")
	}
	return s.Store.Update(ctx, org.ID, func(ops Ops) error {
		m, err := ops.GetMember(ctx, org.ID, userID)
		if err != nil {
			return err
		}
		// The roles the member holds now stay (Studio assigns first and removes the organization-wide
		// role afterwards), but the actor must be allowed to manage them: a scoped Read-only role
		// on an Owner would strip the Owner's permissions on those projects.
		if _, err := mayManage(ctx, ops, actor, org, m); err != nil {
			return err
		}
		if err := need(actor, org, ActCreate, ResSubjectRoles, int64(base)); err != nil {
			return err
		}
		return putScoped(ctx, ops, actor, org, userID, base, refs, true)
	})
}

// putScoped sets (union: false) or extends (union: true) the project-scoped role base of a
// member and removes the projects from the member's other project-scoped roles.
func putScoped(ctx context.Context, ops Ops, actor *Access, org OrgRef, userID string, base int, refs []string, union bool) error {
	all, err := ops.ProjectRoles(ctx, org.ID)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, r := range refs {
		want[r] = true
	}
	final := append([]string(nil), refs...)
	for _, r := range all {
		if r.UserID != userID {
			continue
		}
		if r.BaseRoleID == base {
			if union {
				final = append(final, r.Refs...)
			}
			continue
		}
		var keep []string
		for _, ref := range r.Refs {
			if !want[ref] {
				keep = append(keep, ref)
			}
		}
		if len(keep) == len(r.Refs) {
			continue
		}
		if err := need(actor, org, ActDelete, ResSubjectRoles, r.ID); err != nil {
			return err
		}
		if _, err := ops.PutProjectRole(ctx, org.ID, userID, r.BaseRoleID, keep); err != nil {
			return err
		}
	}
	_, err = ops.PutProjectRole(ctx, org.ID, userID, base, final)
	return err
}

// SetProjectRoleRefs replaces the projects of one project-scoped role of a member (no
// projects removes the role).
func (s *Service) SetProjectRoleRefs(ctx context.Context, actor *Access, org OrgRef, userID string, roleID int64, refs []string) error {
	refs = dedupe(refs)
	return s.Store.Update(ctx, org.ID, func(ops Ops) error {
		r, err := ops.GetProjectRole(ctx, org.ID, roleID)
		if err != nil || r.UserID != userID {
			return ErrNotFound
		}
		m, err := ops.GetMember(ctx, org.ID, userID)
		if err != nil {
			return err
		}
		if _, err := mayManage(ctx, ops, actor, org, m); err != nil {
			return err
		}
		if err := need(actor, org, ActCreate, ResSubjectRoles, int64(r.BaseRoleID)); err != nil {
			return err
		}
		if len(refs) == 0 {
			return ops.DeleteProjectRole(ctx, org.ID, r.ID)
		}
		return putScoped(ctx, ops, actor, org, userID, r.BaseRoleID, refs, false)
	})
}

// RemoveRole removes one role of a member: the organization-wide role (roleID below
// ProjectRoleIDBase) or a project-scoped role. The member stays, with whatever roles are left.
func (s *Service) RemoveRole(ctx context.Context, actor *Access, org OrgRef, userID string, roleID int64) error {
	return s.Store.Update(ctx, org.ID, func(ops Ops) error {
		m, err := ops.GetMember(ctx, org.ID, userID)
		if err != nil {
			return err
		}
		if err := need(actor, org, ActDelete, ResSubjectRoles, roleID); err != nil {
			return err
		}
		if roleID >= ProjectRoleIDBase {
			r, err := ops.GetProjectRole(ctx, org.ID, roleID)
			if err != nil || r.UserID != userID {
				return ErrNotFound
			}
			return ops.DeleteProjectRole(ctx, org.ID, roleID)
		}
		if int64(m.RoleID) != roleID {
			return ErrNotFound
		}
		if m.RoleID == RoleOwner {
			if err := keepOwner(ctx, ops, org.ID, userID); err != nil {
				return err
			}
		}
		return ops.PutMember(ctx, Member{OrgID: org.ID, UserID: userID, RoleID: 0})
	})
}

// RemoveMember removes a member from the organization. Members may always remove themselves
// (leave the team); removing another member needs the permission to remove every role the
// member holds.
func (s *Service) RemoveMember(ctx context.Context, actor *Access, org OrgRef, userID string) error {
	return s.Store.Update(ctx, org.ID, func(ops Ops) error {
		m, err := ops.GetMember(ctx, org.ID, userID)
		if err != nil {
			return err
		}
		if actor != nil && actor.UserID != userID {
			if m.RoleID != 0 {
				if err := need(actor, org, ActDelete, ResSubjectRoles, int64(m.RoleID)); err != nil {
					return err
				}
			}
			prs, err := ops.ProjectRoles(ctx, org.ID)
			if err != nil {
				return err
			}
			for _, r := range prs {
				if r.UserID == userID {
					if err := need(actor, org, ActDelete, ResSubjectRoles, r.ID); err != nil {
						return err
					}
				}
			}
			if m.RoleID == 0 && len(prs) == 0 {
				// Nothing to hold on to: still needs some permission over members.
				if err := need(actor, org, ActDelete, ResSubjectRoles, int64(RoleReadOnly)); err != nil {
					return err
				}
			}
		}
		if m.RoleID == RoleOwner {
			if err := keepOwner(ctx, ops, org.ID, userID); err != nil {
				return err
			}
		}
		return ops.DeleteMember(ctx, org.ID, userID)
	})
}

// EnsureOwner makes the user an Owner of the organization (the claimed first user; the
// creator of an organization).
func (s *Service) EnsureOwner(ctx context.Context, org OrgRef, userID string) error {
	return s.Store.Update(ctx, org.ID, func(ops Ops) error {
		return ops.PutMember(ctx, Member{OrgID: org.ID, UserID: userID, RoleID: RoleOwner})
	})
}

// LastOwnerError names the organization a removal would leave without an Owner.
type LastOwnerError struct{ OrgID int64 }

func (e *LastOwnerError) Error() string {
	return fmt.Sprintf("the user is the only Owner of organization %d; make another member an Owner first", e.OrgID)
}
func (e *LastOwnerError) Is(target error) bool { return target == ErrLastOwner }

// RemoveUser removes every membership of a user (the account is being deleted). It refuses
// when the user is the only Owner of an organization, unless force is set.
func (s *Service) RemoveUser(ctx context.Context, userID string, force bool) error {
	return s.RemoveUserExcept(ctx, userID, force, 0)
}

// RemoveUserExcept is RemoveUser while organization except is being deleted: the last-owner
// rule does not apply to that organization, which is going away with its Owners (0: none).
func (s *Service) RemoveUserExcept(ctx context.Context, userID string, force bool, except int64) error {
	ms, err := s.Store.MembershipsOf(ctx, userID)
	if err != nil {
		return err
	}
	// The stand-in of `functions dev` is not an Owner for the rule, so removing it never refuses.
	checked := !force && userID != StandInOwnerID
	if checked {
		for _, m := range ms {
			if m.RoleID != RoleOwner || m.OrgID == except {
				continue
			}
			if n, err := s.Store.CountOwners(ctx, m.OrgID); err != nil {
				return err
			} else if n <= 1 {
				return &LastOwnerError{OrgID: m.OrgID}
			}
		}
	}
	for _, m := range ms {
		err := s.Store.Update(ctx, m.OrgID, func(ops Ops) error {
			cur, err := ops.GetMember(ctx, m.OrgID, userID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if cur.RoleID == RoleOwner && checked && m.OrgID != except {
				if err := keepOwner(ctx, ops, m.OrgID, userID); err != nil {
					return &LastOwnerError{OrgID: m.OrgID}
				}
			}
			return ops.DeleteMember(ctx, m.OrgID, userID)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// DeleteOrganization removes everything the model keeps for an organization that is being
// deleted (see Store.DeleteOrganization) and returns the ids of its invitations.
func (s *Service) DeleteOrganization(ctx context.Context, org int64) ([]int64, error) {
	return s.Store.DeleteOrganization(ctx, org)
}

// ---- MFA -----------------------------------------------------------------------

// MFAEnforced reports whether the organization requires MFA.
func (s *Service) MFAEnforced(ctx context.Context, org int64) (bool, error) {
	return s.Store.MFAEnforced(ctx, org)
}

// SetMFAEnforced stores the organization's MFA requirement.
func (s *Service) SetMFAEnforced(ctx context.Context, org int64, v bool) error {
	return s.Store.SetMFAEnforced(ctx, org, v)
}

// ---- invitations ---------------------------------------------------------------

// InviteInput describes one invitation.
type InviteInput struct {
	Email string
	// RoleID is the base role; Refs non-empty scopes it to those projects.
	RoleID int
	Refs   []string
}

// NormalizeEmail validates an address and lower-cases it.
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || strings.ContainsAny(s, " <>") {
		return "", invalid("%q is not a valid email address", s)
	}
	return strings.ToLower(s), nil
}

// Invite stores an invitation and returns it with its token (shown once: only the hash is
// kept). Inviting an address again replaces its pending invitation.
func (s *Service) Invite(ctx context.Context, actor *Access, org OrgRef, in InviteInput) (*Invitation, string, error) {
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return nil, "", err
	}
	if !ValidRoleID(in.RoleID) {
		return nil, "", invalid("unknown role %d", in.RoleID)
	}
	if err := need(actor, org, ActCreate, ResUserInvites, int64(in.RoleID)); err != nil {
		return nil, "", err
	}
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return nil, "", err
	}
	token := InvitationPrefix + hex.EncodeToString(tok)
	ttl := s.InvitationTTL
	if ttl <= 0 {
		ttl = DefaultInvitationTTL
	}
	inv := Invitation{OrgID: org.ID, Email: email, RoleID: in.RoleID, ProjectRefs: dedupe(in.Refs), ExpiresAt: s.now().Add(ttl)}
	if actor != nil {
		inv.InvitedBy = actor.UserID
	}
	var stored *Invitation
	err = s.Store.Update(ctx, org.ID, func(ops Ops) error {
		// Inviting again replaces the pending invitation, which deletes it: the actor needs the
		// permission to revoke it, or an Administrator could cancel an Owner's invitation by
		// re-inviting the address with a lower role.
		pending, err := ops.ListInvitations(ctx, org.ID)
		if err != nil {
			return err
		}
		for _, p := range pending {
			if p.Email == email {
				if err := need(actor, org, ActDelete, ResUserInvites, int64(p.RoleID)); err != nil {
					return err
				}
			}
		}
		stored, err = ops.CreateInvitation(ctx, inv, HashToken(token))
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return stored, token, nil
}

// Invitations lists the pending invitations of an organization.
func (s *Service) Invitations(ctx context.Context, org int64) ([]Invitation, error) {
	return s.Store.ListInvitations(ctx, org)
}

// RevokeInvitation deletes a pending invitation.
func (s *Service) RevokeInvitation(ctx context.Context, actor *Access, org OrgRef, id int64) error {
	return s.Store.Update(ctx, org.ID, func(ops Ops) error {
		inv, err := ops.GetInvitation(ctx, org.ID, id)
		if err != nil {
			return err
		}
		if err := need(actor, org, ActDelete, ResUserInvites, int64(inv.RoleID)); err != nil {
			return err
		}
		return ops.DeleteInvitation(ctx, org.ID, id)
	})
}

// InviteState is what the invitation page shows about a token.
type InviteState struct {
	InviteID       int64
	TokenNotFound  bool
	Expired        bool
	Accepted       bool
	EmailMatch     bool
	OrganizationID int64
}

// InvitationState looks a token up for the signed-in user with this email. org is the
// organization of the link; a token of another organization is reported as unknown.
func (s *Service) InvitationState(ctx context.Context, org OrgRef, token, email string) (InviteState, error) {
	inv, err := s.Store.InvitationByHash(ctx, HashToken(strings.TrimSpace(token)))
	if errors.Is(err, ErrNotFound) || (err == nil && inv.OrgID != org.ID) {
		return InviteState{TokenNotFound: true}, nil
	}
	if err != nil {
		return InviteState{}, err
	}
	st := InviteState{InviteID: inv.ID, OrganizationID: inv.OrgID, Accepted: inv.AcceptedAt != nil,
		Expired: !inv.ExpiresAt.After(s.now()), EmailMatch: strings.EqualFold(strings.TrimSpace(email), inv.Email)}
	return st, nil
}

// AcceptInvitation makes the user a member with the invited role. The invitation must be
// pending, unexpired and addressed to email; it works once. liveRefs filters the invited
// projects down to those that still exist (nil keeps them all).
func (s *Service) AcceptInvitation(ctx context.Context, org OrgRef, token, userID, email string, liveRefs func([]string) []string) error {
	inv, err := s.Store.InvitationByHash(ctx, HashToken(strings.TrimSpace(token)))
	if err != nil || inv.OrgID != org.ID {
		return ErrNotFound
	}
	return s.accept(ctx, inv, userID, email, liveRefs)
}

func (s *Service) accept(ctx context.Context, inv *Invitation, userID, email string, liveRefs func([]string) []string) error {
	if !strings.EqualFold(strings.TrimSpace(email), inv.Email) {
		return fmt.Errorf("%w: this invitation was sent to a different address", ErrForbidden)
	}
	return s.Store.Update(ctx, inv.OrgID, func(ops Ops) error {
		if err := ops.MarkInvitationAccepted(ctx, inv.ID, userID, s.now()); err != nil {
			return err // used or expired
		}
		if _, err := ops.GetMember(ctx, inv.OrgID, userID); err == nil {
			return ErrAlreadyMember
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if len(inv.ProjectRefs) == 0 {
			return ops.PutMember(ctx, Member{OrgID: inv.OrgID, UserID: userID, RoleID: inv.RoleID})
		}
		refs := inv.ProjectRefs
		if liveRefs != nil {
			refs = liveRefs(refs)
		}
		if len(refs) == 0 {
			return invalid("none of the invited projects exist any more")
		}
		if err := ops.PutMember(ctx, Member{OrgID: inv.OrgID, UserID: userID, RoleID: 0}); err != nil {
			return err
		}
		_, err := ops.PutProjectRole(ctx, inv.OrgID, userID, inv.RoleID, refs)
		return err
	})
}

// PendingInvitation returns the invitation with this id when it is still pending (not
// accepted, not expired) and addressed to email; ErrNotFound otherwise. It is what a claim link
// (an account-creation token bound to one invitation) checks before it creates the account.
func (s *Service) PendingInvitation(ctx context.Context, id int64, email string) (*Invitation, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	invs, err := s.Store.PendingInvitationsFor(ctx, email, s.now())
	if err != nil {
		return nil, err
	}
	for i := range invs {
		if invs[i].ID == id {
			return &invs[i], nil
		}
	}
	return nil, ErrNotFound
}

// AcceptInvitationByID accepts one pending invitation for a user who just proved control of
// its address (an account created from a claim link bound to that invitation). It never
// accepts any other invitation of the address: each organization's invitation needs its own
// link, or the invitee's own sign-in.
func (s *Service) AcceptInvitationByID(ctx context.Context, id int64, userID, email string, liveRefs func([]string) []string) (*Invitation, error) {
	inv, err := s.PendingInvitation(ctx, id, email)
	if err != nil {
		return nil, err
	}
	if err := s.accept(ctx, inv, userID, email, liveRefs); err != nil {
		return nil, err
	}
	return inv, nil
}

// ---- SSO -----------------------------------------------------------------------

// Grant is a membership created by a default rule.
type Grant struct {
	OrgID  int64
	RoleID int
}

// DomainOf returns the lower-cased domain of an email address ("" when it has none).
func DomainOf(email string) string {
	_, d, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok {
		return ""
	}
	return strings.ToLower(d)
}

// GrantSSODefault is what the SSO workstream calls when a user signs in through SSO for the
// first time: the domain of email may have a default organization and role
// (SetDomainDefault); the user becomes a member with it. It returns nil, nil when the domain
// has no default or the user already belongs to that organization (an existing membership is
// never changed). Without a default the user has no access until an administrator invites
// them, which is the safe state.
func (s *Service) GrantSSODefault(ctx context.Context, userID, email string) (*Grant, error) {
	domain := DomainOf(email)
	if domain == "" {
		return nil, nil
	}
	d, err := s.Store.GetDomainDefault(ctx, domain)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var granted bool
	err = s.Store.Update(ctx, d.OrgID, func(ops Ops) error {
		if _, err := ops.GetMember(ctx, d.OrgID, userID); err == nil {
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		granted = true
		return ops.PutMember(ctx, Member{OrgID: d.OrgID, UserID: userID, RoleID: d.RoleID})
	})
	if err != nil || !granted {
		return nil, err
	}
	s.log().Info("members: SSO user joined with the default role of the email domain", "domain", domain, "org", d.OrgID, "role", RoleName(d.RoleID))
	return &Grant{OrgID: d.OrgID, RoleID: d.RoleID}, nil
}

// SetDomainDefault sets the default membership of an email domain.
func (s *Service) SetDomainDefault(ctx context.Context, domain string, org int64, roleID int) error {
	domain = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(domain, "@")))
	if domain == "" || strings.ContainsAny(domain, " @/") {
		return invalid("%q is not an email domain", domain)
	}
	if !ValidRoleID(roleID) {
		return invalid("unknown role %d", roleID)
	}
	return s.Store.SetDomainDefault(ctx, DomainDefault{Domain: domain, OrgID: org, RoleID: roleID})
}

// DomainDefaults lists the domain rules.
func (s *Service) DomainDefaults(ctx context.Context) ([]DomainDefault, error) {
	return s.Store.ListDomainDefaults(ctx)
}

// RemoveDomainDefault deletes a domain rule.
func (s *Service) RemoveDomainDefault(ctx context.Context, domain string) error {
	return s.Store.DeleteDomainDefault(ctx, strings.ToLower(strings.TrimSpace(strings.TrimPrefix(domain, "@"))))
}

// SortedRoleIDs returns ids ascending; a helper for stable output.
func SortedRoleIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
