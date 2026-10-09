package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/registry"
)

// newOAuthService is the OAuth service for a command on the node: the registry's Store, and the audit
// events of the operator's changes on the system project, as the daemon records those of its own. It
// never mints a token (no Admit), so it serves revocation and listing only.
func newOAuthService(n *lifecycle.Node, pg *registry.Postgres) *oauth.Service {
	log := newLogger(n.Cfg)
	return &oauth.Service{
		Store:        oauth.NewPGStore(pg.Pool()),
		Issuer:       n.Cfg.APIURL(),
		DashboardURL: n.Cfg.DashboardURL(),
		Audit: func(ctx context.Context, kind string, payload map[string]any) {
			if err := n.Registry.AppendEvent(context.WithoutCancel(ctx), config.SystemRef, kind, payload); err != nil {
				log.Warn("an OAuth audit event was not recorded", "kind", kind, "error", err)
			}
		},
		Log: log.With("component", "oauth"),
	}
}

// oauthEnv is what the `oauth` commands work on: the OAuth service, and the lookups that turn the
// names an operator types (an organization's slug, a person's address) into ids. The commands are
// functions of it so that they run against a fake service.
type oauthEnv struct {
	svc oauth.Authority
	// org returns the id of the organization with this slug; registry.ErrNotFound if there is none.
	org func(ctx context.Context, slug string) (int64, error)
	// users returns the dashboard accounts (supavise-gotrue@system). It turns an address into user ids
	// and, for a listing, user ids into addresses.
	users func(ctx context.Context) ([]api.DashboardUser, error)

	in        io.Reader
	out, errw io.Writer

	accounts    []api.DashboardUser
	accountsErr error
	accountsGot bool
}

// dashboardUsers asks for the accounts once per command.
func (e *oauthEnv) dashboardUsers(ctx context.Context) ([]api.DashboardUser, error) {
	if !e.accountsGot {
		e.accounts, e.accountsErr = e.users(ctx)
		e.accountsGot = true
	}
	return e.accounts, e.accountsErr
}

// grantSelector says which grants a command acts on. A zero field does not constrain; the fields
// that are set must all match.
type grantSelector struct {
	ID   int64  // a grant id, from `oauth grants list`
	User string // a person's address, or a user id
	Org  string // an organization's slug
	App  string // an app's id (the client_id)
	All  bool   // every live grant (revoke only)
}

var (
	uuidRe      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	grantIDHint = "see `supavise oauth grants list`"
)

// parseGrantID reads the grant id argument of `grants revoke`.
func parseGrantID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a grant id (a positive number; %s)", s, grantIDHint)
	}
	return id, nil
}

// validate checks the combination of selectors. revoke is `grants revoke`, which needs one and
// refuses to guess; `grants list` takes any combination, none included.
func (s grantSelector) validate(revoke bool) error {
	filters := s.User != "" || s.Org != "" || s.App != ""
	switch {
	case s.ID != 0 && (filters || s.All):
		return errors.New("give a grant id on its own, without --user, --org, --app or --all")
	case s.All && filters:
		return errors.New("--all cannot be combined with --user, --org or --app")
	case revoke && s.ID == 0 && !filters && !s.All:
		return errors.New("name what to revoke: a grant id, or --user, --org, --app (together or alone), or --all")
	}
	if s.App != "" && !uuidRe.MatchString(s.App) {
		return fmt.Errorf("--app wants an app's id, a UUID (%s)", grantIDHint)
	}
	return nil
}

// filters turns the selector into the filters that select its grants: one, or one per account when
// an address belongs to several (a password account and single sign-on accounts). Every filter asks
// for live grants only.
func (e *oauthEnv) filters(ctx context.Context, s grantSelector) ([]oauth.GrantFilter, error) {
	switch {
	case s.All:
		return []oauth.GrantFilter{{All: true, Live: true}}, nil
	case s.ID != 0:
		return []oauth.GrantFilter{{ID: s.ID, Live: true}}, nil
	}
	base := oauth.GrantFilter{AppID: strings.ToLower(s.App), Live: true}
	if s.Org != "" {
		id, err := e.org(ctx, s.Org)
		if errors.Is(err, registry.ErrNotFound) {
			return nil, fmt.Errorf("no organization %q (supavise orgs list)", s.Org)
		}
		if err != nil {
			return nil, err
		}
		base.OrgID = id
	}
	if s.User == "" {
		return []oauth.GrantFilter{base}, nil
	}
	ids, err := e.userIDs(ctx, s.User)
	if err != nil {
		return nil, err
	}
	fs := make([]oauth.GrantFilter, 0, len(ids))
	for _, id := range ids {
		f := base
		f.UserID = id
		fs = append(fs, f)
	}
	return fs, nil
}

// userIDs resolves --user. A user id stands for itself, which reaches the grants of an account that no
// longer exists. An address stands for every account that has it, because the person's authority is
// what an operator ends, whichever way they signed in.
func (e *oauthEnv) userIDs(ctx context.Context, user string) ([]string, error) {
	if uuidRe.MatchString(user) {
		return []string{strings.ToLower(user)}, nil
	}
	us, err := e.dashboardUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("looking up %s among the dashboard users (the system project must be running): %w", user, err)
	}
	var ids []string
	for _, u := range us {
		if strings.EqualFold(u.Email, strings.TrimSpace(user)) {
			ids = append(ids, strings.ToLower(u.ID))
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no dashboard user %s (supavise users list); a user id also works, for an account that is gone", user)
	}
	return ids, nil
}

// list returns the live grants the selector names, newest first.
func (e *oauthEnv) list(ctx context.Context, s grantSelector) ([]oauth.GrantInfo, error) {
	fs, err := e.filters(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []oauth.GrantInfo
	seen := map[int64]bool{}
	for _, f := range fs {
		gs, err := e.svc.ListGrants(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, g := range gs {
			if !seen[g.Grant.ID] {
				seen[g.Grant.ID] = true
				out = append(out, g)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Grant, out[j].Grant
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	return out, nil
}

// grantView is a grant as `grants list --json` prints it.
type grantView struct {
	ID               int64      `json:"id"`
	AppID            string     `json:"app_id"`
	App              string     `json:"app"`
	RegistrationType string     `json:"registration_type"`
	UserID           string     `json:"user_id"`
	User             string     `json:"user,omitempty"`
	Org              string     `json:"org"`
	Scopes           []string   `json:"scopes"`
	Resource         string     `json:"resource,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
}

// views builds the display form. The address of a user comes from the dashboard accounts when they can
// be listed; if not, the id stands in and the note says why. Names that a client or an account
// chose are cleaned of control characters: they reach an operator's terminal.
func (e *oauthEnv) views(ctx context.Context, gs []oauth.GrantInfo) []grantView {
	emails := map[string]string{}
	if len(gs) > 0 {
		us, err := e.dashboardUsers(ctx)
		if err != nil {
			fmt.Fprintf(e.errw, "note: the dashboard users could not be listed (%v); users are shown by id\n", err)
		}
		for _, u := range us {
			emails[strings.ToLower(u.ID)] = u.Email
		}
	}
	vs := make([]grantView, 0, len(gs))
	for _, gi := range gs {
		g := gi.Grant
		scopes := append([]string{}, g.Scopes...)
		sort.Strings(scopes)
		vs = append(vs, grantView{
			ID: g.ID, AppID: gi.App.ID, App: printable(gi.App.Name), RegistrationType: gi.App.RegistrationType,
			UserID: g.UserID, User: printable(emails[strings.ToLower(g.UserID)]), Org: printable(gi.OrgSlug),
			Scopes: scopes, Resource: g.Resource, CreatedAt: g.CreatedAt, LastUsedAt: g.LastUsedAt,
		})
	}
	return vs
}

// printable replaces the control characters of a name that someone else chose.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// scopeSummary is the SCOPES column: the scopes themselves while they are few.
func scopeSummary(scopes []string) string {
	if len(scopes) <= 3 {
		return strings.Join(scopes, ",")
	}
	return fmt.Sprintf("%d scopes", len(scopes))
}

func writeGrantTable(w io.Writer, vs []grantView) error {
	t := newTable(w)
	fmt.Fprintln(t, "ID\tAPP\tAPP ID\tUSER\tORG\tSCOPES\tCREATED\tLAST USED")
	for _, v := range vs {
		who := v.User
		if who == "" {
			who = v.UserID
		}
		last := "-"
		if v.LastUsedAt != nil {
			last = v.LastUsedAt.Local().Format(time.DateTime)
		}
		fmt.Fprintf(t, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.App, v.AppID, who, v.Org, scopeSummary(v.Scopes),
			v.CreatedAt.Local().Format(time.DateTime), last)
	}
	return t.Flush()
}

// runOAuthGrantsList is `supavise oauth grants list`.
func runOAuthGrantsList(ctx context.Context, e *oauthEnv, s grantSelector, asJSON bool) error {
	if err := s.validate(false); err != nil {
		return err
	}
	gs, err := e.list(ctx, s)
	if err != nil {
		return err
	}
	vs := e.views(ctx, gs)
	if asJSON {
		return printJSON(e.out, vs)
	}
	if len(vs) == 0 {
		fmt.Fprintln(e.out, "no live OAuth grants")
		return nil
	}
	return writeGrantTable(e.out, vs)
}

// runOAuthGrantsRevoke is `supavise oauth grants revoke`. It shows the grants it is about to end and
// asks, unless yes is set. The service raises the audit event of each (oauth.grant_revoked, by the
// operator).
func runOAuthGrantsRevoke(ctx context.Context, e *oauthEnv, s grantSelector, yes bool) error {
	if err := s.validate(true); err != nil {
		return err
	}
	fs, err := e.filters(ctx, s)
	if err != nil {
		return err
	}
	if !yes {
		gs, err := e.list(ctx, s)
		if err != nil {
			return err
		}
		if len(gs) == 0 {
			if s.ID != 0 {
				return fmt.Errorf("no live grant %d (%s)", s.ID, grantIDHint)
			}
			fmt.Fprintln(e.out, "no live OAuth grants match; nothing revoked")
			return nil
		}
		fmt.Fprintf(e.errw, "%d live OAuth grant(s) would be revoked; each client has to be authorized again:\n", len(gs))
		if err := writeGrantTable(e.errw, e.views(ctx, gs)); err != nil {
			return err
		}
		fmt.Fprint(e.errw, "Revoke them? [y/N] ")
		line, _ := bufio.NewReader(e.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("not revoked")
		}
	}
	n := 0
	for _, f := range fs {
		k, err := e.svc.RevokeGrants(ctx, f, oauth.ReasonOperator, oauth.ActorOperator)
		n += k
		if err != nil {
			return fmt.Errorf("revoked %d grant(s) before the error: %w", n, err)
		}
	}
	if n == 0 && s.ID != 0 {
		return fmt.Errorf("no live grant %d (%s)", s.ID, grantIDHint)
	}
	fmt.Fprintf(e.out, "revoked %d grant(s)\n", n)
	return nil
}

// openOAuthEnv opens the node and builds the environment of the commands. The caller closes it.
func openOAuthEnv(cmd *cobra.Command) (*oauthEnv, func(), error) {
	n, err := openNode(cmd.Context())
	if err != nil {
		return nil, nil, err
	}
	pg, ok := n.Registry.(*registry.Postgres)
	if !ok {
		n.Close()
		return nil, nil, fmt.Errorf("the registry is %T, want Postgres", n.Registry)
	}
	acc := &api.Accounts{Reg: n.Registry, Store: api.NewPGClaimStore(pg.Pool()), Keys: n.Engine.Keys, Config: n.Cfg, Log: newLogger(n.Cfg)}
	e := &oauthEnv{
		svc: newOAuthService(n, pg),
		org: func(ctx context.Context, slug string) (int64, error) {
			o, err := n.Registry.GetOrganization(ctx, slug)
			if err != nil {
				return 0, err
			}
			return o.ID, nil
		},
		users: acc.ListUsers,
		in:    cmd.InOrStdin(), out: cmd.OutOrStdout(), errw: cmd.ErrOrStderr(),
	}
	return e, n.Close, nil
}

func init() {
	oauthCmd := &cobra.Command{
		Use:   "oauth",
		Short: "List and revoke the OAuth grants of MCP clients",
		Long: `An MCP client (Claude Code, Cursor, VS Code, Codex) that a person authorizes in the dashboard holds a
grant: that person's authority in one organization, limited to the scopes they approved. Its access
token lasts an hour and its refresh token renews it. These commands list the grants and end them, for
the cases the dashboard does not reach: everything one person holds, everything in an organization, a
client everywhere, or every grant on the node. Studio's Organization > OAuth Apps page revokes one
client in one organization.

Run them as the user that owns the state directory (supavise); the system cluster must be running.
` + "`supavise users remove`" + ` ends the grants of the user it removes by itself.`,
	}
	grants := &cobra.Command{
		Use:   "grants",
		Short: "List and revoke grants",
	}

	var (
		lsSel  grantSelector
		lsJSON bool
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List the live grants",
		Long: `Lists the grants that are live, newest first: the client, the person, the organization, the
scopes, when the grant was made and when it was last used. A grant that was revoked is not listed.
The filters combine; a grant must match all that are given.

--user takes the person's address and covers every dashboard account that has it (it needs the system
project's sign-in service), or a user id, which reaches the grants of an account that is gone.
--org takes an organization's slug and --app an app's id, the column APP ID.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, closeFn, err := openOAuthEnv(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			return runOAuthGrantsList(cmd.Context(), e, lsSel, lsJSON)
		},
	}
	list.Flags().StringVar(&lsSel.User, "user", "", "only the grants of this person (address or user id)")
	list.Flags().StringVar(&lsSel.Org, "org", "", "only the grants in this organization (slug)")
	list.Flags().StringVar(&lsSel.App, "app", "", "only the grants of this app (id)")
	list.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var (
		rmSel grantSelector
		rmYes bool
	)
	revoke := &cobra.Command{
		Use:   "revoke (<id> | --user EMAIL | --org SLUG | --app ID | --all)",
		Short: "Revoke grants",
		Long: `Revokes live grants. The access and refresh tokens of a revoked grant stop working at the next
request, and the client has to be authorized again by a person. Name what to revoke:

  <id>            one grant, by the ID column of ` + "`supavise oauth grants list`" + `
  --user EMAIL    everything one person holds (an address, or a user id)
  --org SLUG      every grant in an organization
  --app ID        an app's grants everywhere
  --all           every live grant on the node

--user, --org and --app combine, as the filters of ` + "`list`" + ` do; an id and --all stand alone. The command
lists the grants and asks before it revokes; --yes skips the question. Each revocation is recorded in
the audit log as oauth.grant_revoked by the operator.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sel := rmSel
			if len(args) == 1 {
				id, err := parseGrantID(args[0])
				if err != nil {
					return err
				}
				sel.ID = id
			}
			if err := sel.validate(true); err != nil { // before the node is opened
				return err
			}
			e, closeFn, err := openOAuthEnv(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			return runOAuthGrantsRevoke(cmd.Context(), e, sel, rmYes)
		},
	}
	revoke.Flags().StringVar(&rmSel.User, "user", "", "revoke the grants of this person (address or user id)")
	revoke.Flags().StringVar(&rmSel.Org, "org", "", "revoke the grants in this organization (slug)")
	revoke.Flags().StringVar(&rmSel.App, "app", "", "revoke the grants of this app (id)")
	revoke.Flags().BoolVar(&rmSel.All, "all", false, "revoke every live grant")
	revoke.Flags().BoolVarP(&rmYes, "yes", "y", false, "do not ask for confirmation")

	grants.AddCommand(list, revoke)
	oauthCmd.AddCommand(grants)
	rootCmd.AddCommand(oauthCmd)
}
