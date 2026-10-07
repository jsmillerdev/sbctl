package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
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
			out[i] = members.OrgRef{ID: o.ID, Slug: o.Slug}
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
	var out []string
	for _, ref := range refs {
		if p, err := s.reg.GetProject(ctx, ref); err == nil && p.Ref != config.SystemRef {
			out = append(out, ref)
		}
	}
	return out
}

// UserCreatedAt returns when the dashboard account was created, from sb-gotrue@system.
func (a *Accounts) UserCreatedAt(ctx context.Context, userID string) (time.Time, error) {
	var u struct {
		CreatedAt time.Time `json:"created_at"`
	}
	if _, err := a.goTrue(ctx, http.MethodGet, "/admin/users/"+url.PathEscape(userID), nil, &u); err != nil {
		return time.Time{}, err
	}
	return u.CreatedAt, nil
}

// findUser returns the dashboard account with this address, nil when there is none.
func (a *Accounts) findUser(ctx context.Context, email string) (*DashboardUser, error) {
	users, err := a.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if strings.EqualFold(users[i].Email, email) {
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
	// Emailed is true when sb-gotrue@system sent the invitation through the configured SMTP
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
	existing, err := a.findUser(ctx, email)
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
	mail := a.Config.Mail.Enabled()
	if existing == nil && !mail {
		// The link that creates the account; accepting is then automatic.
		ct, _, err := a.IssueInvite(ctx, email, a.Members.InvitationTTL)
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
				if ct, _, cerr := a.IssueInvite(ctx, email, a.Members.InvitationTTL); cerr == nil {
					res.ClaimURL = a.Config.APIURL() + "/claim#" + url.Values{"token": {ct}, "email": {email}}.Encode()
				}
			}
		} else {
			res.Emailed = true
		}
	}
	if !res.Emailed {
		// The link is the credential-free half of the invitation (it works only for the signed-in
		// holder of the address); log it so an administrator who lost it can find it again.
		a.log().Info("organization invitation created; no mail was sent, give the invitee this link", "email", email, "org", org.Slug, "link", res.Link())
	}
	return res, nil
}

// sendInvitation has sb-gotrue@system mail the invitee. A new address gets GoTrue's invite
// (the account is created, confirmed and marked as a dashboard user at once, the link signs the
// person in and lands on the invitation); an existing account gets a sign-in link that lands
// on it.
func (a *Accounts) sendInvitation(ctx context.Context, email string, existing *DashboardUser, joinURL string) error {
	redirect := "?redirect_to=" + url.QueryEscape(joinURL)
	if existing != nil {
		_, err := a.goTrueAnon(ctx, http.MethodPost, "/magiclink"+redirect, map[string]any{"email": email})
		return err
	}
	var u struct {
		ID string `json:"id"`
	}
	if _, err := a.goTrue(ctx, http.MethodPost, "/invite"+redirect, map[string]any{"email": email}, &u); err != nil {
		return err
	}
	if u.ID == "" {
		return errors.New("gotrue did not return the invited user")
	}
	// The invited user needs the dashboard claim like any account sbctl creates.
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
