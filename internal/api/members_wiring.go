package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/sso"
)

// NewMembers builds the roles service over the registry: Postgres for a Postgres registry
// (tables from registry migration 0900_members.sql), memory otherwise. accounts supplies the
// legacy-account rule's lookup in the dashboard's sign-in service (see members.Service).
func NewMembers(reg registry.Registry, accounts *Accounts, now func() time.Time, log *slog.Logger) *members.Service {
	var st members.Store
	if pg, ok := reg.(*registry.Postgres); ok {
		st = members.NewPG(pg.Pool())
	} else {
		st = members.NewMemory()
	}
	svc := &members.Service{Store: st, Now: now, Log: log}
	svc.Orgs = func(ctx context.Context) ([]members.OrgRef, error) {
		orgs, err := reg.ListOrganizations(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]members.OrgRef, len(orgs))
		for i, o := range orgs {
			out[i] = members.OrgRef{ID: o.ID, Slug: o.Slug, CreatedAt: o.CreatedAt}
		}
		return out, nil
	}
	if accounts != nil {
		svc.AccountCreatedAt = accounts.UserCreatedAt
	}
	return svc
}

// liveRefs keeps the refs of projects that exist, for invitations that name projects.
func (s *Server) liveRefs(ctx context.Context, refs []string) []string {
	return liveRefs(ctx, s.reg, refs)
}

// UserCreatedAt returns when the dashboard account was created, from supavise-gotrue@system.
func (a *Accounts) UserCreatedAt(ctx context.Context, userID string) (time.Time, error) {
	var u struct {
		CreatedAt time.Time `json:"created_at"`
	}
	if _, err := a.goTrue(ctx, http.MethodGet, "/admin/users/"+url.PathEscape(userID), nil, &u); err != nil {
		return time.Time{}, err
	}
	return u.CreatedAt, nil
}

// UserSelector picks one account when an email address belongs to several. GoTrue keeps email
// addresses unique among password accounts only, so an identity provider that vouches for an
// address creates a second account with it; the operator's commands name which one they mean.
type UserSelector struct {
	// UserID is the account's id (see `supavise users list --json`).
	UserID string
	// Provider is "email" (or "password") for the password account, or the id of a single
	// sign-on identity provider, with or without the "sso:" prefix.
	Provider string
}

func (s UserSelector) empty() bool { return s.UserID == "" && s.Provider == "" }

// AmbiguousUserError says that an address belongs to several accounts and the selector did not
// pick one.
type AmbiguousUserError struct {
	Email   string
	Matches []DashboardUser
}

func (e *AmbiguousUserError) Error() string {
	parts := make([]string, len(e.Matches))
	for i, u := range e.Matches {
		parts[i] = u.ID + " (" + u.kind() + ")"
	}
	return fmt.Sprintf("%s belongs to several dashboard accounts: %s; name one with --user-id <id>, or with --provider email or --provider <identity provider id>",
		e.Email, strings.Join(parts, ", "))
}

// kind describes how the account signs in.
func (u DashboardUser) kind() string {
	if u.SSOProvider != "" {
		return "single sign-on, provider " + u.SSOProvider
	}
	return "password"
}

// ResolveUser returns the dashboard account of an email address, nil when there is none.
//
// An address can belong to a password account and to single sign-on accounts at once: GoTrue's
// uniqueness index leaves SSO accounts out, and an identity provider may vouch for any address.
// So an address is never resolved by taking the first match. With a selector, the account it names
// is the one (an error when none matches). Without one, the password account is the one when there
// is exactly one: an account that an identity provider created never takes the place of the account
// supavise created for the address, and it is reached only by naming it. An address that only
// single sign-on accounts have resolves to its account when there is just one. Anything else is an
// AmbiguousUserError.
func (a *Accounts) ResolveUser(ctx context.Context, email string, sel UserSelector) (*DashboardUser, error) {
	users, err := a.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var matches, password []DashboardUser
	for _, u := range users {
		if !strings.EqualFold(u.Email, email) {
			continue
		}
		matches = append(matches, u)
		if u.SSOProvider == "" {
			password = append(password, u)
		}
	}
	if !sel.empty() {
		var picked []DashboardUser
		prov := strings.ToLower(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(sel.Provider)), "sso:"))
		for _, u := range matches {
			switch {
			case sel.UserID != "" && !strings.EqualFold(u.ID, strings.TrimSpace(sel.UserID)):
			case prov == "":
				picked = append(picked, u)
			case prov == "email" || prov == "password":
				if u.SSOProvider == "" {
					picked = append(picked, u)
				}
			case u.SSOProvider == prov:
				picked = append(picked, u)
			}
		}
		switch len(picked) {
		case 0:
			if len(matches) == 0 {
				return nil, nil
			}
			return nil, fmt.Errorf("none of the dashboard accounts of %s has that --user-id or --provider", email)
		case 1:
			return &picked[0], nil
		}
		return nil, &AmbiguousUserError{Email: email, Matches: picked}
	}
	switch {
	case len(matches) == 0:
		return nil, nil
	case len(matches) == 1:
		return &matches[0], nil
	case len(password) == 1:
		return &password[0], nil
	}
	return nil, &AmbiguousUserError{Email: email, Matches: matches}
}

// passwordAccount returns the account supavise created for an address (one that signs in with a
// password or a magic link), nil when there is none. Accounts of identity providers do not count:
// an invitation goes to the person who owns the address, not to whoever an identity provider says
// has it.
func (a *Accounts) passwordAccount(ctx context.Context, email string) (*DashboardUser, error) {
	users, err := a.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].SSOProvider == "" && strings.EqualFold(users[i].Email, email) {
			return &users[i], nil
		}
	}
	return nil, nil
}

// InviteResult is what inviting an address to an organization produced.
type InviteResult struct {
	Invitation *members.Invitation
	// JoinURL opens the invitation in the dashboard (the user signs in and accepts).
	JoinURL string
	// ClaimURL creates the dashboard account of an address that has none; the invitation is
	// accepted when the account is created. Empty when the address already has an account.
	ClaimURL string
	// Emailed is true when supavise-gotrue@system sent the invitation through the configured SMTP
	// relay; otherwise the administrator passes Link on.
	Emailed bool
	// MailError says why a configured relay did not send.
	MailError string
}

// Link is the URL to give the invitee: the account page for a new address, the invitation
// page otherwise.
func (r *InviteResult) Link() string {
	if r.ClaimURL != "" {
		return r.ClaimURL
	}
	return r.JoinURL
}

// InviteToOrganization invites an address to an organization with a role (and optionally
// projects). actor nil is the operator (the CLI). When mail is configured ([mail] in
// config.toml) the dashboard's sign-in service sends the message: an invitation to create an
// account for a new address, a sign-in link that lands on the invitation for an existing one.
// Without mail, or when sending fails, the caller gets a link to pass on; the invitation
// itself is stored either way.
func (a *Accounts) InviteToOrganization(ctx context.Context, actor *members.Access, org members.OrgRef, in members.InviteInput) (*InviteResult, error) {
	if a.Members == nil {
		return nil, errors.New("api: invitations need the members service")
	}
	email, err := members.NormalizeEmail(in.Email)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "%v", err)
	}
	in.Email = email
	existing, err := a.passwordAccount(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if m, err := a.Members.Store.GetMember(ctx, org.ID, existing.ID); err == nil && m != nil {
			return nil, members.ErrAlreadyMember
		}
	}
	inv, token, err := a.Members.Invite(ctx, actor, org, in)
	if err != nil {
		return nil, err
	}
	res := &InviteResult{Invitation: inv}
	res.JoinURL = a.Config.DashboardURL() + "/join?" + url.Values{"token": {token}, "slug": {org.Slug}}.Encode()
	mail := a.Config.Mail.Enabled() && !a.NoMail
	if existing == nil && !mail {
		// The link that creates the account; accepting is then automatic.
		ct, _, err := a.IssueInvite(ctx, email, inv.ID, a.Members.InvitationTTL)
		if err != nil {
			return nil, err
		}
		res.ClaimURL = a.Config.APIURL() + "/claim#" + url.Values{"token": {ct}, "email": {email}}.Encode()
	}
	if mail {
		if err := a.sendInvitation(ctx, email, existing, res.JoinURL); err != nil {
			res.MailError = err.Error()
			a.log().Warn("invitation mail not sent; pass the link on", "email", email, "error", err)
			if existing == nil {
				if ct, _, cerr := a.IssueInvite(ctx, email, inv.ID, a.Members.InvitationTTL); cerr == nil {
					res.ClaimURL = a.Config.APIURL() + "/claim#" + url.Values{"token": {ct}, "email": {email}}.Encode()
				}
			}
		} else {
			res.Emailed = true
		}
	}
	if !res.Emailed {
		// The link is a credential (the claim URL creates the account with a password of the
		// holder's choosing and accepts the invitation), so it goes to the caller only, never to
		// the log. An administrator who lost it replaces the invitation to get a new one.
		a.log().Info("organization invitation created; no mail was sent, the caller has the link", "email", email, "org", org.Slug, "invitation", inv.ID)
	}
	return res, nil
}

// sendInvitation has supavise-gotrue@system mail the invitee. A new address gets GoTrue's invite
// (the account is created, confirmed and marked as a dashboard user at once, the link signs the
// person in and lands on the invitation); an existing account gets a sign-in link that lands
// on it.
func (a *Accounts) sendInvitation(ctx context.Context, email string, existing *DashboardUser, joinURL string) error {
	redirect := "?redirect_to=" + url.QueryEscape(joinURL)
	if existing != nil {
		_, err := a.goTrueAnon(ctx, http.MethodPost, "/magiclink"+redirect, map[string]any{"email": email})
		return err
	}
	// GoTrue creates the user, which its before-user-created hook allows only with this
	// one-time grant, issued for this address (sso.GrantKey, serveBeforeUserCreated).
	grant := newToken("sbg_")
	gh := sha256.Sum256([]byte(grant))
	if err := a.Store.CreateSignupGrant(ctx, email, gh[:], a.now().Add(10*time.Minute)); err != nil {
		return err
	}
	var u struct {
		ID string `json:"id"`
	}
	if _, err := a.goTrue(ctx, http.MethodPost, "/invite"+redirect, map[string]any{"email": email, "data": map[string]any{sso.GrantKey: grant}}, &u); err != nil {
		return err
	}
	if u.ID == "" {
		return errors.New("gotrue did not return the invited user")
	}
	// The invited user needs the dashboard claim like any account supavise creates.
	if _, err := a.goTrue(ctx, http.MethodPut, "/admin/users/"+url.PathEscape(u.ID), map[string]any{
		"app_metadata": map[string]any{AdminClaim: true, "provider": "email", "providers": []string{"email"}},
	}, nil); err != nil {
		return fmt.Errorf("mark the invited user as a dashboard user: %w", err)
	}
	if a.Users != nil {
		_, _ = a.Users.UpsertUser(ctx, User{UserID: u.ID, Email: email, Username: strings.SplitN(email, "@", 2)[0]})
	}
	return nil
}

// goTrueAnon calls a public GoTrue endpoint (no service key).
func (a *Accounts) goTrueAnon(ctx context.Context, method, path string, body any) (int, error) {
	return a.goTrue(ctx, method, path, body, nil)
}

// EnableMembers gives accounts (built outside the server, as the CLI does) the roles service
// and the user store, so that inviting, listing and removing users work with roles.
func (a *Accounts) EnableMembers(reg registry.Registry, users Store) {
	a.Users = users
	a.Members = NewMembers(reg, a, a.Now, a.Log)
	a.LiveRefs = func(ctx context.Context, refs []string) []string { return liveRefs(ctx, reg, refs) }
}

func liveRefs(ctx context.Context, reg registry.Registry, refs []string) []string {
	var out []string
	for _, ref := range refs {
		if p, err := reg.GetProject(ctx, ref); err == nil && p.Ref != config.SystemRef {
			out = append(out, ref)
		}
	}
	return out
}

// orgBySlugOrOnly resolves --org: the slug, or the only organization when slug is empty.
func (a *Accounts) orgBySlugOrOnly(ctx context.Context, slug string) (members.OrgRef, error) {
	if slug != "" {
		o, err := a.Reg.GetOrganization(ctx, slug)
		if err != nil {
			return members.OrgRef{}, fmt.Errorf("no organization %q", slug)
		}
		return members.OrgRef{ID: o.ID, Slug: o.Slug}, nil
	}
	orgs, err := a.Reg.ListOrganizations(ctx)
	if err != nil {
		return members.OrgRef{}, err
	}
	switch len(orgs) {
	case 0:
		return members.OrgRef{}, errors.New("there is no organization yet; claim the node first (`supavise claim token`)")
	case 1:
		return members.OrgRef{ID: orgs[0].ID, Slug: orgs[0].Slug}, nil
	}
	var slugs []string
	for _, o := range orgs {
		slugs = append(slugs, o.Slug)
	}
	return members.OrgRef{}, fmt.Errorf("there are several organizations (%s); name one with --org", strings.Join(slugs, ", "))
}

// Org resolves --org: the slug, or the only organization when slug is empty.
func (a *Accounts) Org(ctx context.Context, slug string) (members.OrgRef, error) {
	return a.orgBySlugOrOnly(ctx, slug)
}

// InviteByEmail is `supavise users invite`: the operator invites an address to an organization.
func (a *Accounts) InviteByEmail(ctx context.Context, email, orgSlug, role string, projectRefs []string) (*InviteResult, members.OrgRef, error) {
	ro, err := members.ParseRole(role)
	if err != nil {
		return nil, members.OrgRef{}, err
	}
	org, err := a.orgBySlugOrOnly(ctx, orgSlug)
	if err != nil {
		return nil, org, err
	}
	for _, ref := range projectRefs {
		p, err := a.Reg.GetProject(ctx, ref)
		if err != nil || p.OrgID != org.ID {
			return nil, org, fmt.Errorf("project %s is not in organization %s", ref, org.Slug)
		}
	}
	res, err := a.InviteToOrganization(ctx, nil, org, members.InviteInput{Email: email, RoleID: ro.ID, Refs: projectRefs})
	if errors.Is(err, members.ErrAlreadyMember) {
		return nil, org, fmt.Errorf("%s is already a member of %s; change the role with `supavise users role`", email, org.Slug)
	}
	return res, org, err
}

// SetRole is SetRoleOf for the account an address resolves to without a selector.
func (a *Accounts) SetRole(ctx context.Context, email, orgSlug, role string) (members.OrgRef, error) {
	org, _, err := a.SetRoleOf(ctx, email, orgSlug, role, UserSelector{})
	return org, err
}

// SetRoleOf is `supavise users role`: the operator sets the organization-wide role of an account,
// adding the account to the organization when it is not a member. It is how an organization
// that lost every Owner gets one back. The last Owner still cannot be demoted. When the address
// belongs to several accounts the selector names one (ResolveUser); the account that got the role
// is returned.
func (a *Accounts) SetRoleOf(ctx context.Context, email, orgSlug, role string, sel UserSelector) (members.OrgRef, *DashboardUser, error) {
	ro, err := members.ParseRole(role)
	if err != nil {
		return members.OrgRef{}, nil, err
	}
	org, err := a.orgBySlugOrOnly(ctx, orgSlug)
	if err != nil {
		return org, nil, err
	}
	email, err = normalizeEmail(email)
	if err != nil {
		return org, nil, err
	}
	u, err := a.ResolveUser(ctx, email, sel)
	if err != nil {
		return org, nil, err
	}
	if u == nil {
		return org, nil, fmt.Errorf("no dashboard user %s", email)
	}
	if _, err := a.Members.Store.GetMember(ctx, org.ID, u.ID); errors.Is(err, members.ErrNotFound) {
		return org, u, a.Members.Store.Update(ctx, org.ID, func(ops members.Ops) error {
			return ops.PutMember(ctx, members.Member{OrgID: org.ID, UserID: u.ID, RoleID: ro.ID})
		})
	} else if err != nil {
		return org, nil, err
	}
	return org, u, a.Members.SetOrgRole(ctx, nil, org, u.ID, ro.ID)
}

// UserRoles describes where a dashboard user belongs, for `supavise users list`: one entry per
// organization, "acme:owner" or "acme:developer(2 projects)".
func (a *Accounts) UserRoles(ctx context.Context, userID string) ([]string, error) {
	ms, err := a.Members.Store.MembershipsOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	prs, err := a.Members.Store.ProjectRolesOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range ms {
		o, err := a.Reg.GetOrganizationByID(ctx, m.OrgID)
		if err != nil {
			continue
		}
		parts := []string{}
		if m.RoleID != 0 {
			parts = append(parts, strings.ToLower(members.RoleName(m.RoleID)))
		}
		for _, r := range prs {
			if r.OrgID == m.OrgID {
				parts = append(parts, fmt.Sprintf("%s(%d project%s)", strings.ToLower(members.RoleName(r.BaseRoleID)), len(r.Refs), map[bool]string{true: "", false: "s"}[len(r.Refs) == 1]))
			}
		}
		out = append(out, o.Slug+":"+strings.Join(parts, "+"))
	}
	return out, nil
}

// SetDomainDefault is `supavise users default-role set`: the organization and role an SSO user
// gets on a first sign-in from this email domain.
func (a *Accounts) SetDomainDefault(ctx context.Context, domain, orgSlug, role string) (members.OrgRef, error) {
	ro, err := members.ParseRole(role)
	if err != nil {
		return members.OrgRef{}, err
	}
	org, err := a.orgBySlugOrOnly(ctx, orgSlug)
	if err != nil {
		return org, err
	}
	return org, a.Members.SetDomainDefault(ctx, domain, org.ID, ro.ID)
}
