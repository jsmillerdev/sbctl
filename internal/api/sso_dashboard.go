package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/members"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/sso"
)

// Dashboard SSO. The dashboard's users sign in through sb-gotrue@system, which speaks SAML 2.0
// to the identity providers (Okta, Entra ID, Google Workspace, ...) that an Owner or
// Administrator registers here. GoTrue keeps each provider's metadata and email domains; sbctl
// keeps what GoTrue has no place for: the organization a provider belongs to and the role its
// users get on a first sign-in (sbctl.sso_providers, sbctl.sso_default_roles).
//
// Who gets in. A dashboard session of a user whose GoTrue account came from SSO carries
// app_metadata.provider = "sso:<provider id>". It is accepted only when the provider is one
// registered here, and only for a user who belongs to an organization:
//
//   - on the first request of a user, the default role of the email's domain makes the user a
//     member of the provider's organization, provided the signing-in provider is one that
//     vouches for that domain (a second identity provider cannot claim another's domains);
//   - otherwise the user is recorded as pending and refused on every route (403) until an
//     administrator approves the user (`sbctl sso approve`, or the pending list in the API) or
//     invites or promotes them the usual way;
//   - the default role is for that first request only: a user who later loses every
//     membership becomes pending, and is not given the role again.
//
// Sign-up is closed to everyone else: sb-gotrue@system asks the daemon before it creates a
// user (before-user-created hook, serveBeforeUserCreated) and the daemon allows registered
// SSO providers and invited addresses only.

// DashboardSSO manages the SAML providers of the dashboard. The daemon builds it for the HTTP
// routes, the CLI for `sbctl sso`.
type DashboardSSO struct {
	Reg   registry.Registry
	Store SSOStore
	// Keys returns the credentials of a project; the system project's service_role key
	// authorizes GoTrue's admin API.
	Keys   func(ctx context.Context, ref string) (*secrets.ProjectKeys, error)
	Config *config.Config
	// GoTrueURL overrides http://127.0.0.1:<ports.system_gotrue>.
	GoTrueURL string
	HTTP      *http.Client
	Members   *members.Service
	// Accounts removes the user a denial refuses (the claim store's removal list).
	Accounts *Accounts
	Now      func() time.Time
	Log      *slog.Logger
	// Changed is told after the dashboard gained its first provider or lost its last one: Studio
	// offers "Continue with SSO" only while there is one, and decides that when it starts.
	Changed func(ctx context.Context)

	mu       sync.Mutex
	provs    map[string]cachedProvider
	admitted map[string]time.Time
	firstMu  sync.Mutex
}

type cachedProvider struct {
	row *SSOProviderRow // nil: not registered
	at  time.Time
}

// ssoCacheTTL bounds how long a registered provider, and an admitted user, are remembered: a
// removed provider or member loses the session within this time even when the change was made
// by another process (the CLI).
const ssoCacheTTL = 10 * time.Second

// NewDashboardSSO builds the service from the account service that shares its credentials,
// members and registry: the CLI's `sbctl sso` uses it, the server builds its own the same way.
func NewDashboardSSO(a *Accounts, store SSOStore) *DashboardSSO {
	a.SSOUsers = store
	return &DashboardSSO{Reg: a.Reg, Store: store, Keys: a.Keys, Config: a.Config, GoTrueURL: a.GoTrueURL, HTTP: a.HTTP,
		Members: a.Members, Accounts: a, Now: a.Now, Log: a.Log}
}

func (d *DashboardSSO) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// event records an audit event of the dashboard's single sign-on under the system project (never
// a secret: ids, domains and role names).
func (d *DashboardSSO) event(ctx context.Context, kind string, payload map[string]any) {
	if err := d.Reg.AppendEvent(context.WithoutCancel(ctx), config.SystemRef, kind, payload); err != nil {
		d.log().Warn("an SSO audit event was not recorded", "kind", kind, "error", err)
	}
}

func (d *DashboardSSO) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (d *DashboardSSO) clientCtx(ctx context.Context) (*sso.Client, error) {
	k, err := d.Keys(ctx, config.SystemRef)
	if err != nil {
		return nil, fmt.Errorf("system project credentials: %w", err)
	}
	base := d.GoTrueURL
	if base == "" {
		base = fmt.Sprintf("http://127.0.0.1:%d", d.Config.Ports.SystemGoTrue)
	}
	return &sso.Client{BaseURL: base, ServiceKey: k.ServiceRoleKey, HTTP: d.HTTP}, nil
}

// ServiceProvider is what an identity provider is configured with to trust this node.
func (d *DashboardSSO) ServiceProvider() sso.SPURLs {
	return sso.URLsFor(d.Config.APIURL() + "/auth/v1")
}

// DashboardProvider is a provider as the dashboard's API shows it.
type DashboardProvider struct {
	*sso.Provider
	OrgID   int64
	OrgSlug string
	// DefaultRole is the role of a first-time user (members role id, 0: none).
	DefaultRole int
	// Registered is false for a provider that exists in GoTrue but that sbctl did not register:
	// its users are refused. `sbctl sso remove` deletes it.
	Registered bool
}

// ---- errors ------------------------------------------------------------------

// boolPtrIf returns &true for true and nil for false (GoTrue's flag is "disabled when set").
func boolPtrIf(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

// ssoError turns a failure of the sign-in service into an API error.
func ssoError(err error) error {
	var ae *sso.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == http.StatusNotFound:
			return errf(http.StatusNotFound, "%s", firstNonEmpty(ae.Message, "SSO provider not found"))
		case ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusUnauthorized && ae.Status != http.StatusForbidden:
			return errf(ae.Status, "%s", firstNonEmpty(ae.Message, "the sign-in service refused the request"))
		}
		return errf(http.StatusBadGateway, "the sign-in service answered %d", ae.Status)
	}
	return err
}

var (
	errSSOUnknownProvider = errf(http.StatusForbidden, "This identity provider is not registered with sbctl, so its users cannot use the dashboard.")
	errSSOPending         = errf(http.StatusForbidden, "Your single sign-on account has no access yet. Ask an administrator to approve it (sbctl sso approve <email>).")
)

// ---- rights ------------------------------------------------------------------

// mayGrant reports whether actor (nil: the operator) may give users the role by default: the
// default role of a provider is a grant to everybody the identity provider vouches for, so it
// follows the rule for adding members with that role (an Administrator cannot make Owners).
func mayGrant(actor *members.Access, org members.OrgRef, role int) bool {
	return role == 0 || actor == nil || actor.CanRole(org, members.ActCreate, members.ResSubjectRoles, int64(role))
}

// checkManage reports whether actor may change a provider with this default role: one that makes
// Owners can be changed (and have a domain added to it, which would make the domain's users
// Owners) by an Owner only.
func (d *DashboardSSO) checkManage(actor *members.Access, org members.OrgRef, row *SSOProviderRow, newRole int) error {
	if row != nil && !mayGrant(actor, org, row.DefaultRole) {
		return errf(http.StatusForbidden, "Your role does not allow changing a provider whose users become %s", members.RoleName(row.DefaultRole))
	}
	if !mayGrant(actor, org, newRole) {
		return errf(http.StatusForbidden, "Your role does not allow giving single sign-on users the %s role", members.RoleName(newRole))
	}
	return nil
}

// checkUsers reports whether actor may change who the identity provider vouches for (its metadata,
// attribute mapping, name id format or availability) or remove it: whoever controls the provider
// controls the accounts of its users, and removing it ends their sessions. So the actor must be
// able to grant every role the provider's users hold, in any organization and on any project. A
// provider with an Owner among its users is the business of an Owner (or the operator) only.
func (d *DashboardSSO) checkUsers(ctx context.Context, actor *members.Access, org members.OrgRef, providerID string) error {
	if actor == nil || d.Members == nil {
		return nil
	}
	us, err := d.Store.ListSSOUsers(ctx, "", []string{providerID})
	if err != nil {
		return err
	}
	for _, u := range us {
		ms, err := d.Members.Store.MembershipsOf(ctx, u.UserID)
		if err != nil {
			return err
		}
		prs, err := d.Members.Store.ProjectRolesOf(ctx, u.UserID)
		if err != nil {
			return err
		}
		roles := map[int64][]int{}
		for _, m := range ms {
			roles[m.OrgID] = append(roles[m.OrgID], m.RoleID)
		}
		for _, r := range prs {
			roles[r.OrgID] = append(roles[r.OrgID], r.BaseRoleID)
		}
		for orgID, rs := range roles {
			ref := members.OrgRef{ID: orgID}
			if orgID == org.ID {
				ref = org
			}
			for _, role := range rs {
				if !mayGrant(actor, ref, role) {
					return errf(http.StatusForbidden, "Your role does not allow changing or removing a provider whose users include a member with the %s role", members.RoleName(role))
				}
			}
		}
	}
	return nil
}

// ruleConflict refuses to register domains for org when another organization's default-role rule
// holds one of them, unless actor (nil: the operator) owns that organization: the domain is
// somebody else's, and a provider for it (even one that gives no role) would take over the sign-in
// of its addresses. It is the only check of who may claim a domain: claims are global on the node, first
// come first served, and not verified (see the SSO section of the README).
func (d *DashboardSSO) ruleConflict(ctx context.Context, actor *members.Access, org members.OrgRef, domains []string) error {
	if actor == nil || d.Members == nil {
		return nil
	}
	for _, dm := range domains {
		r, err := d.Members.Store.GetDomainDefault(ctx, dm)
		if errors.Is(err, members.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if r.OrgID != org.ID && actor.OrgRole(r.OrgID) != members.RoleOwner {
			return errf(http.StatusConflict, "The domain %s already has a default role of another organization", dm)
		}
	}
	return nil
}

// ---- providers ---------------------------------------------------------------

// AddProvider is what `sbctl sso add` and the API give to Add.
type AddProvider struct {
	Org      members.OrgRef
	Metadata sso.Metadata
	// Domains are the email domains the provider vouches for (at least one: the sign-in page
	// finds the provider by the domain of the email address).
	Domains []string
	// DefaultRole is the members role a first-time user of these domains gets; 0: none, the
	// user waits for approval.
	DefaultRole      int
	NameIDFormat     string
	AttributeMapping *sso.AttributeMapping
	CreatedBy        string
	// Disabled registers the provider switched off: GoTrue refuses to start a sign-in through it.
	Disabled bool
}

func (d *DashboardSSO) view(ctx context.Context, p *sso.Provider, row *SSOProviderRow) *DashboardProvider {
	v := &DashboardProvider{Provider: p, Registered: row != nil}
	if row != nil {
		v.OrgID, v.DefaultRole = row.OrgID, row.DefaultRole
		if o, err := d.Reg.GetOrganizationByID(ctx, row.OrgID); err == nil {
			v.OrgSlug = o.Slug
		}
	}
	return v
}

func (d *DashboardSSO) domainsTaken(ctx context.Context, domains []string, except string) error {
	rows, err := d.Store.ListProviders(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == except {
			continue
		}
		for _, dm := range domains {
			if slices.Contains(r.Domains, dm) {
				return errf(http.StatusConflict, "The domain %s already belongs to another identity provider (%s)", dm, r.ID)
			}
		}
	}
	return nil
}

// nameIDFormats are the values GoTrue takes for name_id_format.
var nameIDFormats = []string{
	"urn:oasis:names:tc:SAML:2.0:nameid-format:persistent",
	"urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
	"urn:oasis:names:tc:SAML:2.0:nameid-format:transient",
	"urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified",
}

func checkNameIDFormat(f string) error {
	if f == "" || slices.Contains(nameIDFormats, f) {
		return nil
	}
	return errf(http.StatusBadRequest, "name_id_format must be one of %s", strings.Join(nameIDFormats, ", "))
}

// Add registers an identity provider in sb-gotrue@system and records it for the organization.
func (d *DashboardSSO) Add(ctx context.Context, actor *members.Access, in AddProvider) (*DashboardProvider, error) {
	if in.DefaultRole != 0 && !members.ValidRoleID(in.DefaultRole) {
		return nil, errf(http.StatusBadRequest, "unknown role %d", in.DefaultRole)
	}
	if err := d.checkManage(actor, in.Org, nil, in.DefaultRole); err != nil {
		return nil, err
	}
	domains, err := sso.NormalizeDomains(in.Domains)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "%v", err)
	}
	if len(domains) == 0 {
		return nil, errf(http.StatusBadRequest, "At least one email domain is needed: the sign-in page finds the provider by the domain of the address")
	}
	if err := checkNameIDFormat(in.NameIDFormat); err != nil {
		return nil, err
	}
	if in.Metadata.URL == "" && in.Metadata.XML == "" {
		return nil, errf(http.StatusBadRequest, "The identity provider's metadata is needed")
	}
	if in.Metadata.URL != "" && in.Metadata.XML != "" {
		return nil, errf(http.StatusBadRequest, "Only one of the metadata address and the metadata document can be given")
	}
	if in.Metadata.XML != "" {
		if _, err := sso.ValidateXML(in.Metadata.XML); err != nil {
			return nil, errf(http.StatusBadRequest, "%v", err)
		}
	}
	if err := d.domainsTaken(ctx, domains, ""); err != nil {
		return nil, err
	}
	if err := d.ruleConflict(ctx, actor, in.Org, domains); err != nil {
		return nil, err
	}
	c, err := d.clientCtx(ctx)
	if err != nil {
		return nil, err
	}
	p, err := c.Create(ctx, sso.CreateBody{
		MetadataURL: in.Metadata.URL, MetadataXML: in.Metadata.XML, Domains: domains,
		AttributeMapping: in.AttributeMapping, NameIDFormat: in.NameIDFormat, Disabled: boolPtrIf(in.Disabled),
	})
	if err != nil {
		return nil, ssoError(err)
	}
	entity := ""
	if p.SAML != nil {
		entity = p.SAML.EntityID
	}
	row := SSOProviderRow{ID: p.ID, OrgID: in.Org.ID, EntityID: entity, Domains: domains, DefaultRole: in.DefaultRole, CreatedBy: in.CreatedBy}
	undo := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = d.Store.DeleteProvider(cctx, p.ID)
		if _, derr := c.Delete(cctx, p.ID); derr != nil {
			d.log().Warn("the identity provider could not be removed again after a failed registration", "provider", p.ID, "error", derr)
		}
	}
	if err := d.Store.PutProvider(ctx, row); err != nil {
		undo()
		return nil, err
	}
	// The provider is the source of truth: the domains' rules say what it says, and a rule of the
	// organization that was there before (set by an operator) does not outlive it.
	if err := d.syncRules(ctx, in.Org, domains, domains, 0, in.DefaultRole); err != nil {
		undo()
		return nil, err
	}
	d.forget(p.ID)
	d.event(ctx, "sso.provider.added", map[string]any{"provider": p.ID, "org": in.Org.Slug, "domains": domains, "default_role": members.RoleName(in.DefaultRole), "by": in.CreatedBy})
	d.changed(ctx)
	d.log().Info("dashboard SSO provider added", "provider", p.ID, "org", in.Org.Slug, "domains", domains, "default_role", members.RoleName(in.DefaultRole))
	return d.view(ctx, p, &row), nil
}

// syncRules makes the default-role rules of the members service say what a provider of org
// says: oldDomains lose their rule (if it is the organization's), every domain of newDomains
// gets newRole, or no rule when newRole is 0.
func (d *DashboardSSO) syncRules(ctx context.Context, org members.OrgRef, oldDomains, newDomains []string, oldRole, newRole int) error {
	if d.Members == nil {
		return nil
	}
	for _, dm := range oldDomains {
		if slices.Contains(newDomains, dm) && newRole != 0 {
			continue
		}
		if r, err := d.Members.Store.GetDomainDefault(ctx, dm); err == nil && r.OrgID == org.ID {
			if err := d.Members.RemoveDomainDefault(ctx, dm); err != nil && !errors.Is(err, members.ErrNotFound) {
				return err
			}
		}
	}
	if newRole == 0 {
		return nil
	}
	for _, dm := range newDomains {
		if err := d.Members.SetDomainDefault(ctx, dm, org.ID, newRole); err != nil {
			return err
		}
	}
	return nil
}

// registeredRow returns the sbctl record of a provider, errNoProvider when there is none.
func (d *DashboardSSO) registeredRow(ctx context.Context, id string) (*SSOProviderRow, error) {
	row, err := d.Store.GetProvider(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, errNoSSOProvider
	}
	return row, err
}

var errNoSSOProvider = errf(http.StatusNotFound, "SSO provider not found")

// List returns the providers of org (0: every organization), with the unregistered ones GoTrue
// holds when org is 0.
func (d *DashboardSSO) List(ctx context.Context, orgID int64) ([]*DashboardProvider, error) {
	rows, err := d.Store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]*SSOProviderRow{}
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	c, err := d.clientCtx(ctx)
	if err != nil {
		return nil, err
	}
	all, err := c.List(ctx)
	if err != nil {
		return nil, ssoError(err)
	}
	out := []*DashboardProvider{}
	for _, p := range all {
		row := byID[p.ID]
		switch {
		case row == nil && orgID == 0:
			out = append(out, d.view(ctx, p, nil))
		case row != nil && (orgID == 0 || row.OrgID == orgID):
			out = append(out, d.view(ctx, p, row))
		}
	}
	return out, nil
}

// Get returns one provider with its metadata. orgID limits the answer to that organization
// (0: any).
func (d *DashboardSSO) Get(ctx context.Context, id string, orgID int64) (*DashboardProvider, error) {
	row, err := d.Store.GetProvider(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if row == nil && orgID != 0 || row != nil && orgID != 0 && row.OrgID != orgID {
		return nil, errNoSSOProvider
	}
	c, err := d.clientCtx(ctx)
	if err != nil {
		return nil, err
	}
	p, err := c.Get(ctx, id)
	if err != nil {
		return nil, ssoError(err)
	}
	return d.view(ctx, p, row), nil
}

// UpdateProvider is what Update changes; nil fields stay as they are.
type UpdateProvider struct {
	Metadata         *sso.Metadata
	Domains          *[]string
	DefaultRole      *int
	NameIDFormat     *string
	AttributeMapping *sso.AttributeMapping
	Disabled         *bool
}

// Update changes a registered provider.
func (d *DashboardSSO) Update(ctx context.Context, actor *members.Access, id string, orgID int64, in UpdateProvider) (*DashboardProvider, error) {
	row, err := d.registeredRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if orgID != 0 && row.OrgID != orgID {
		return nil, errNoSSOProvider
	}
	org := members.OrgRef{ID: row.OrgID}
	if o, err := d.Reg.GetOrganizationByID(ctx, row.OrgID); err == nil {
		org.Slug = o.Slug
	}
	newRole := row.DefaultRole
	if in.DefaultRole != nil {
		if *in.DefaultRole != 0 && !members.ValidRoleID(*in.DefaultRole) {
			return nil, errf(http.StatusBadRequest, "unknown role %d", *in.DefaultRole)
		}
		newRole = *in.DefaultRole
	}
	if err := d.checkManage(actor, org, row, newRole); err != nil {
		return nil, err
	}
	if in.Metadata != nil || in.AttributeMapping != nil || in.NameIDFormat != nil || (in.Disabled != nil && *in.Disabled) {
		if err := d.checkUsers(ctx, actor, org, id); err != nil {
			return nil, err
		}
	}
	if in.NameIDFormat != nil {
		if err := checkNameIDFormat(*in.NameIDFormat); err != nil {
			return nil, err
		}
	}
	body := sso.UpdateBody{AttributeMapping: in.AttributeMapping, NameIDFormat: in.NameIDFormat, Disabled: in.Disabled}
	newDomains := row.Domains
	if in.Domains != nil {
		if newDomains, err = sso.NormalizeDomains(*in.Domains); err != nil {
			return nil, errf(http.StatusBadRequest, "%v", err)
		}
		if len(newDomains) == 0 {
			return nil, errf(http.StatusBadRequest, "At least one email domain is needed")
		}
		if err := d.domainsTaken(ctx, newDomains, id); err != nil {
			return nil, err
		}
		body.Domains = &newDomains
	}
	if err := d.ruleConflict(ctx, actor, org, newDomains); err != nil {
		return nil, err
	}
	if in.Metadata != nil {
		if in.Metadata.XML != "" {
			if _, err := sso.ValidateXML(in.Metadata.XML); err != nil {
				return nil, errf(http.StatusBadRequest, "%v", err)
			}
		}
		body.MetadataURL, body.MetadataXML = in.Metadata.URL, in.Metadata.XML
	}
	c, err := d.clientCtx(ctx)
	if err != nil {
		return nil, err
	}
	p, err := c.Update(ctx, id, body)
	if err != nil {
		return nil, ssoError(err)
	}
	old := *row
	row.Domains, row.DefaultRole = newDomains, newRole
	if err := d.Store.PutProvider(ctx, *row); err != nil {
		return nil, err
	}
	if err := d.syncRules(ctx, org, old.Domains, newDomains, old.DefaultRole, newRole); err != nil {
		return nil, err
	}
	d.forget(id)
	d.event(ctx, "sso.provider.updated", map[string]any{"provider": id, "org": org.Slug, "domains": newDomains, "default_role": members.RoleName(newRole)})
	return d.view(ctx, p, row), nil
}

// Remove deletes a provider: sbctl stops accepting its users at once, their memberships and
// roles are removed, their personal access tokens are revoked (a token would otherwise outlive the identity provider that vouched for
// its owner), and the provider goes from GoTrue. It finishes a removal that stopped halfway:
// a provider that only GoTrue still has can be removed too. orgID limits the removal to that
// organization (0: any).
func (d *DashboardSSO) Remove(ctx context.Context, actor *members.Access, id string, orgID int64) (*DashboardProvider, error) {
	row, err := d.Store.GetProvider(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if row == nil && orgID != 0 || row != nil && orgID != 0 && row.OrgID != orgID {
		return nil, errNoSSOProvider
	}
	c, err := d.clientCtx(ctx)
	if err != nil {
		return nil, err
	}
	var org members.OrgRef
	if row != nil {
		org.ID = row.OrgID
		if o, err := d.Reg.GetOrganizationByID(ctx, row.OrgID); err == nil {
			org.Slug = o.Slug
		}
		if err := d.checkManage(actor, org, row, 0); err != nil {
			return nil, err
		}
		if err := d.checkUsers(ctx, actor, org, id); err != nil {
			return nil, err
		}
	} else if actor != nil && !actor.IsOwnerAnywhere() {
		return nil, errf(http.StatusForbidden, "Your role does not allow removing a provider that sbctl did not register")
	}
	if row != nil {
		// The users are unreachable once the provider is gone, and the rows that name them go with
		// it, so their memberships go first: otherwise they would stay in the member lists and
		// count as Owners for the last-owner rule. Removing the only Owner of an organization is
		// refused (nothing of the provider changes then), as `sbctl users remove` refuses it.
		if err := d.removeMemberships(ctx, id); err != nil {
			return nil, err
		}
		users, err := d.Store.DeleteProvider(ctx, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		d.forget(id)
		if err := d.syncRules(ctx, org, row.Domains, nil, row.DefaultRole, 0); err != nil {
			return nil, err
		}
		for _, u := range users {
			d.revokeTokens(ctx, u.UserID)
		}
	}
	p, err := c.Delete(ctx, id)
	if err != nil {
		var ae *sso.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound && row != nil {
			// Registered here but already gone from GoTrue: the removal is done.
			p = (&sso.Provider{ID: id, SAML: &sso.SAML{EntityID: row.EntityID}}).Normalize()
		} else {
			return nil, ssoError(err)
		}
	}
	d.event(ctx, "sso.provider.removed", map[string]any{"provider": id})
	d.changed(ctx)
	d.log().Info("dashboard SSO provider removed", "provider", id)
	return d.view(ctx, p, row), nil
}

// removeMemberships removes the memberships and project roles of every user that signed in
// through the provider, in every organization (the accounts can sign in no more). It stops at
// the first user whose removal would leave an organization without an Owner; the users removed
// before that stay removed (they wait for approval, were the provider kept), and running the
// removal again after another Owner exists finishes it.
func (d *DashboardSSO) removeMemberships(ctx context.Context, providerID string) error {
	if d.Members == nil {
		return nil
	}
	us, err := d.Store.ListSSOUsers(ctx, "", []string{providerID})
	if err != nil {
		return err
	}
	for _, u := range us {
		if err := d.Members.RemoveUser(ctx, u.UserID, false); err != nil {
			if errors.Is(err, members.ErrLastOwner) {
				return errf(http.StatusConflict, "%s signs in through this provider and is the only Owner of an organization; make another member an Owner first", u.Email)
			}
			return err
		}
	}
	return nil
}

func (d *DashboardSSO) revokeTokens(ctx context.Context, userID string) {
	ts, err := d.Reg.ListAccessTokens(ctx, userID)
	if err != nil {
		d.log().Warn("personal access tokens of a removed SSO user could not be listed", "user", userID, "error", err)
		return
	}
	for _, t := range ts {
		if err := d.Reg.DeleteAccessToken(ctx, userID, t.ID); err != nil {
			d.log().Warn("a personal access token of a removed SSO user could not be deleted", "user", userID, "error", err)
		}
	}
}

// changed tells the owner of Studio's unit that the dashboard may have gained or lost its
// first or last provider.
func (d *DashboardSSO) changed(ctx context.Context) {
	if d.Changed != nil {
		d.Changed(ctx)
	}
}

// ---- pending users -----------------------------------------------------------

// PendingUser is a user who signed in through SSO and waits for approval.
type PendingUser struct {
	UserID, Email, ProviderID string
	OrgID                     int64
	OrgSlug                   string
	FirstSeen, LastSeen       time.Time
}

// Pending lists the users waiting for approval in org (0: every organization).
func (d *DashboardSSO) Pending(ctx context.Context, orgID int64) ([]PendingUser, error) {
	rows, err := d.Store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	byID := map[string]SSOProviderRow{}
	for _, r := range rows {
		if orgID == 0 || r.OrgID == orgID {
			ids = append(ids, r.ID)
			byID[r.ID] = r
		}
	}
	if len(ids) == 0 {
		return []PendingUser{}, nil
	}
	us, err := d.Store.ListSSOUsers(ctx, SSOPending, ids)
	if err != nil {
		return nil, err
	}
	out := make([]PendingUser, 0, len(us))
	slugs := map[int64]string{}
	for _, u := range us {
		r := byID[u.ProviderID]
		slug, ok := slugs[r.OrgID]
		if !ok {
			if o, err := d.Reg.GetOrganizationByID(ctx, r.OrgID); err == nil {
				slug = o.Slug
			}
			slugs[r.OrgID] = slug
		}
		out = append(out, PendingUser{UserID: u.UserID, Email: u.Email, ProviderID: u.ProviderID, OrgID: r.OrgID, OrgSlug: slug, FirstSeen: u.FirstSeen, LastSeen: u.LastSeen})
	}
	return out, nil
}

// pendingOf returns the pending user of the organization.
func (d *DashboardSSO) pendingOf(ctx context.Context, org members.OrgRef, userID string) (*SSOUser, error) {
	u, err := d.Store.GetSSOUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return nil, errNoPending
	}
	if err != nil {
		return nil, err
	}
	row, err := d.Store.GetProvider(ctx, u.ProviderID)
	if errors.Is(err, ErrNotFound) || (err == nil && row.OrgID != org.ID) || u.State != SSOPending {
		return nil, errNoPending
	}
	return u, err
}

var errNoPending = errf(http.StatusNotFound, "No pending single sign-on user with this id")

// Approve makes a pending user a member of the organization with the role. A nil actor is the
// operator (the CLI).
func (d *DashboardSSO) Approve(ctx context.Context, actor *members.Access, org members.OrgRef, userID string, roleID int) error {
	if !members.ValidRoleID(roleID) {
		return errf(http.StatusBadRequest, "unknown role %d", roleID)
	}
	if !mayGrant(actor, org, roleID) {
		return errf(http.StatusForbidden, "Your role does not allow adding members with the %s role", members.RoleName(roleID))
	}
	if _, err := d.pendingOf(ctx, org, userID); err != nil {
		return err
	}
	err := d.Members.Store.Update(ctx, org.ID, func(ops members.Ops) error {
		if _, err := ops.GetMember(ctx, org.ID, userID); err == nil {
			return nil // already a member: the approval only settles the state
		} else if !errors.Is(err, members.ErrNotFound) {
			return err
		}
		return ops.PutMember(ctx, members.Member{OrgID: org.ID, UserID: userID, RoleID: roleID})
	})
	if err != nil {
		return err
	}
	if err := d.Store.SetSSOUserState(ctx, userID, SSOActive, d.now()); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.admitted, userID)
	d.mu.Unlock()
	d.event(ctx, "sso.user.approved", map[string]any{"user": userID, "org": org.Slug, "role": members.RoleName(roleID)})
	d.log().Info("single sign-on user approved", "user", userID, "org", org.Slug, "role", members.RoleName(roleID))
	return nil
}

// Deny refuses a pending user for good: the account is deleted from sb-gotrue@system and the
// user's sessions end. Signing in again creates a new account, which waits again. A user who has
// become a member since the last request is not denied (409): the stored state is refreshed
// only when the user makes a request, and the account may carry access elsewhere.
func (d *DashboardSSO) Deny(ctx context.Context, actor *members.Access, org members.OrgRef, userID string) error {
	u, err := d.pendingOf(ctx, org, userID)
	if err != nil {
		return err
	}
	// The stored state is as old as the user's last request: a user who joined an organization
	// since (invited, `sbctl users role`) is a member, and deleting the account would end that access too.
	if member, err := d.isMember(ctx, userID); err != nil {
		return err
	} else if member {
		if err := d.Store.SetSSOUserState(ctx, userID, SSOActive, d.now()); err != nil {
			return err
		}
		d.mu.Lock()
		delete(d.admitted, userID)
		d.mu.Unlock()
		return errf(http.StatusConflict, "This user is a member of an organization now and is not waiting for approval; remove the member instead")
	}
	if d.Accounts != nil {
		if err := d.Accounts.Store.MarkUserRemoved(ctx, u.UserID, u.Email); err != nil {
			return err
		}
		if _, err := d.Accounts.goTrue(ctx, http.MethodDelete, "/admin/users/"+url.PathEscape(u.UserID), nil, nil); err != nil {
			var ge *goTrueError
			if !errors.As(err, &ge) || ge.Status != http.StatusNotFound {
				return err
			}
		}
	}
	d.revokeTokens(ctx, u.UserID)
	d.mu.Lock()
	delete(d.admitted, userID)
	d.mu.Unlock()
	d.event(ctx, "sso.user.denied", map[string]any{"user": userID, "org": org.Slug})
	return d.Store.DeleteSSOUser(ctx, userID)
}

// ---- admission ---------------------------------------------------------------

// ssoProviderOf returns the SSO provider id of a GoTrue session (app_metadata.provider is
// "sso:<id>", and so is every entry of providers), "" for any other session. GoTrue sets
// app_metadata; a user cannot edit it.
func ssoProviderOf(claims map[string]any) string {
	app, _ := claims["app_metadata"].(map[string]any)
	prov, _ := app["provider"].(string)
	id, ok := strings.CutPrefix(prov, "sso:")
	if !ok || id == "" {
		return ""
	}
	if list, ok := app["providers"].([]any); ok {
		for _, p := range list {
			if s, _ := p.(string); !strings.HasPrefix(s, "sso:") {
				return "" // linked to another kind of identity: not a pure SSO account
			}
		}
	}
	return strings.ToLower(id)
}

func (d *DashboardSSO) provider(ctx context.Context, id string) (*SSOProviderRow, error) {
	now := d.now()
	d.mu.Lock()
	if c, ok := d.provs[id]; ok && now.Sub(c.at) < ssoCacheTTL {
		d.mu.Unlock()
		return c.row, nil
	}
	d.mu.Unlock()
	row, err := d.Store.GetProvider(ctx, id)
	if errors.Is(err, ErrNotFound) {
		row, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.provs == nil {
		d.provs = map[string]cachedProvider{}
	}
	d.provs[id] = cachedProvider{row: row, at: now}
	d.mu.Unlock()
	return row, nil
}

// forget drops what is remembered about a provider (after a change made here).
func (d *DashboardSSO) forget(id string) {
	d.mu.Lock()
	delete(d.provs, id)
	d.admitted = nil
	d.mu.Unlock()
}

func (d *DashboardSSO) isMember(ctx context.Context, userID string) (bool, error) {
	ms, err := d.Members.Store.MembershipsOf(ctx, userID)
	return len(ms) > 0, err
}

// Admit decides whether the dashboard session of an SSO user (userID, email, the provider the
// session came from) may use the API; it returns nil to let the request through and a 403
// otherwise. It is the whole of "first sign-in gets the default role, anyone else waits".
func (d *DashboardSSO) Admit(ctx context.Context, userID, email, providerID string) error {
	row, err := d.provider(ctx, providerID)
	if err != nil {
		return err
	}
	if row == nil {
		return errSSOUnknownProvider
	}
	now := d.now()
	d.mu.Lock()
	at, ok := d.admitted[userID]
	d.mu.Unlock()
	if ok && now.Sub(at) < ssoCacheTTL {
		return nil
	}
	u, err := d.Store.GetSSOUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		u, err = d.firstSight(ctx, row, userID, email)
	}
	if err != nil {
		return err
	}
	// After firstSight, which may have made the user a member.
	member, err := d.isMember(ctx, userID)
	if err != nil {
		return err
	}
	switch {
	case member && u.State == SSOActive:
	case member:
		// Approved by other means (an invitation accepted, `sbctl users role`).
		if err := d.Store.SetSSOUserState(ctx, userID, SSOActive, now); err != nil {
			return err
		}
	case u.State == SSOActive:
		// Lost every membership since: waits for approval again, and is not given the default role again.
		if err := d.Store.SetSSOUserState(ctx, userID, SSOPending, now); err != nil {
			return err
		}
		return errSSOPending
	default:
		return errSSOPending
	}
	d.mu.Lock()
	if d.admitted == nil || len(d.admitted) >= maxAdmitted {
		d.admitted = map[string]time.Time{} // entries are ten seconds old at most; a reset only costs a lookup
	}
	d.admitted[userID] = now
	d.mu.Unlock()
	return nil
}

// maxAdmitted bounds the memory of admitted users.
const maxAdmitted = 4096

// firstSight records a user seen for the first time and applies the default role of the email's
// domain. One request at a time, so that the parallel calls Studio makes on its first page do
// not race the grant.
func (d *DashboardSSO) firstSight(ctx context.Context, row *SSOProviderRow, userID, email string) (*SSOUser, error) {
	d.firstMu.Lock()
	defer d.firstMu.Unlock()
	if u, err := d.Store.GetSSOUser(ctx, userID); err == nil {
		return u, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := d.now()
	u := SSOUser{UserID: userID, ProviderID: row.ID, Email: strings.ToLower(email), State: SSOPending, FirstSeen: now, LastSeen: now}
	member, err := d.isMember(ctx, userID)
	if err != nil {
		return nil, err
	}
	if member {
		u.State = SSOActive
	} else if g, err := d.grantDefault(ctx, row, userID, email); err != nil {
		return nil, err
	} else if g != nil {
		u.State = SSOActive
	}
	if _, err := d.Store.InsertSSOUser(ctx, u); err != nil {
		return nil, err
	}
	payload := map[string]any{"user": userID, "provider": row.ID, "state": u.State}
	// An identity provider can vouch for an address that already has a password account; the two
	// stay separate accounts (GoTrue keeps SSO accounts out of its email uniqueness). Say so in the
	// audit trail: the operator's commands never take the SSO account for the password account.
	if d.Accounts != nil {
		if pw, err := d.Accounts.passwordAccount(ctx, u.Email); err != nil {
			d.log().Warn("could not check whether a single sign-on user shares an address with a password account", "user", userID, "error", err)
		} else if pw != nil {
			payload["shares_email_with"] = pw.ID
			d.log().Warn("a single sign-on user has the address of a password account; they are separate accounts", "user", userID, "provider", row.ID, "email", u.Email, "password_account", pw.ID)
		}
	}
	d.event(ctx, "sso.user.first_sign_in", payload)
	d.log().Info("single sign-on user seen for the first time", "user", userID, "email", u.Email, "provider", row.ID, "state", u.State)
	return &u, nil
}

// grantDefault gives a first-time user the provider's default role, when the provider vouches for
// the email's domain. The provider's own role is the one that counts: the domain's default-role
// rule (which Add and Update keep in step) only has to belong to the provider's organization, so a
// rule that was changed on its own, or left behind by an operator, cannot hand out more than the
// provider's registrant was allowed to give.
func (d *DashboardSSO) grantDefault(ctx context.Context, row *SSOProviderRow, userID, email string) (*members.Grant, error) {
	dm := members.DomainOf(email)
	if dm == "" || row.DefaultRole == 0 || !slices.Contains(row.Domains, dm) {
		return nil, nil
	}
	rule, err := d.Members.Store.GetDomainDefault(ctx, dm)
	if errors.Is(err, members.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rule.OrgID != row.OrgID {
		return nil, nil
	}
	if rule.RoleID == row.DefaultRole {
		return d.Members.GrantSSODefault(ctx, userID, email)
	}
	var granted bool
	err = d.Members.Store.Update(ctx, row.OrgID, func(ops members.Ops) error {
		if _, err := ops.GetMember(ctx, row.OrgID, userID); err == nil {
			return nil
		} else if !errors.Is(err, members.ErrNotFound) {
			return err
		}
		granted = true
		return ops.PutMember(ctx, members.Member{OrgID: row.OrgID, UserID: userID, RoleID: row.DefaultRole})
	})
	if err != nil || !granted {
		return nil, err
	}
	d.log().Info("single sign-on user joined with the default role of the provider", "provider", row.ID, "org", row.OrgID, "role", members.RoleName(row.DefaultRole))
	return &members.Grant{OrgID: row.OrgID, RoleID: row.DefaultRole}, nil
}

// Find resolves what an operator typed to a provider: a provider id, or an email domain it
// serves. It sees the providers GoTrue holds that sbctl did not register, too.
func (d *DashboardSSO) Find(ctx context.Context, who string) (*DashboardProvider, error) {
	all, err := d.List(ctx, 0)
	if err != nil {
		return nil, err
	}
	who = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(who, "@")))
	for _, p := range all {
		if strings.ToLower(p.ID) == who || slices.Contains(p.DomainNames(), who) {
			return p, nil
		}
	}
	return nil, errf(http.StatusNotFound, "No identity provider with the id or domain %q (see `sbctl sso list`)", who)
}

// FindPending resolves an email address or user id to a user waiting in org (0: any).
func (d *DashboardSSO) FindPending(ctx context.Context, orgID int64, who string) (*PendingUser, error) {
	us, err := d.Pending(ctx, orgID)
	if err != nil {
		return nil, err
	}
	who = strings.ToLower(strings.TrimSpace(who))
	for i := range us {
		if strings.ToLower(us[i].UserID) == who || strings.ToLower(us[i].Email) == who {
			return &us[i], nil
		}
	}
	return nil, errNoPending
}
