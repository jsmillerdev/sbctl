package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fleet"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/sso"
)

// openSSO connects to the registry and returns the service that manages the dashboard's SAML
// identity providers, for the CLI. When the dashboard gains its first provider or loses its
// last, Studio's unit is rendered again (it offers "Continue with SSO" only while there is one)
// before the command returns.
func openSSO(cmd *cobra.Command) (*api.DashboardSSO, *api.Accounts, *config.Config, func(), error) {
	n, err := openNode(cmd.Context())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	d, acc, err := openSSOOn(cmd, n)
	if err != nil {
		n.Close()
		return nil, nil, nil, nil, err
	}
	return d, acc, n.Cfg, n.Close, nil
}

// openSSOOn builds the SSO service over an open node; the caller closes the node.
func openSSOOn(cmd *cobra.Command, n *lifecycle.Node) (*api.DashboardSSO, *api.Accounts, error) {
	pg, ok := n.Registry.(*registry.Postgres)
	if !ok {
		return nil, nil, fmt.Errorf("the registry is %T, want Postgres", n.Registry)
	}
	acc := &api.Accounts{Reg: n.Registry, Store: api.NewPGClaimStore(pg.Pool()), Keys: n.Engine.Keys, Config: n.Cfg, Log: newLogger(n.Cfg)}
	acc.EnableMembers(n.Registry, api.NewPGStore(pg.Pool()))
	// `sso remove` and `orgs delete` revoke the OAuth grants of the SSO users they remove.
	acc.OAuth = newOAuthService(n, pg)
	d := api.NewDashboardSSO(acc, api.NewPGSSOStore(pg.Pool()))
	fm, err := fleet.NewManager(fleet.Deps{Cfg: n.Cfg, Log: newLogger(n.Cfg), Registry: n.Registry, Secrets: n.Secrets, Supervisor: n.Supervisor, Artifacts: n.Artifacts})
	if err != nil {
		return nil, nil, err
	}
	d.Changed = func(ctx context.Context) {
		// Both daemon and CLI may be asked to change the providers; the one that was asked
		// renders Studio, and a unit whose files did not change is left running.
		fmt.Fprintln(cmd.ErrOrStderr(), "Updating Studio's sign-in page...")
		if err := fm.RefreshStudio(ctx); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: Studio was not updated (%v); run `supavise fleet start` to apply it\n", err)
		}
	}
	return d, acc, nil
}

// spInstructions says what to configure in the identity provider.
func spInstructions(sp sso.SPURLs) string {
	return fmt.Sprintf(`Configure the identity provider with:
  ACS URL (assertion consumer service):  %s
  Entity ID (audience):                  %s
  Service provider metadata:             %s
  NameID: persistent or email address; the user's email must be in the assertion.`, sp.ACSURL, sp.EntityID, sp.MetadataURL)
}

func providerRole(p *api.DashboardProvider) string {
	if p.DefaultRole == 0 {
		return "none (approval)"
	}
	return strings.ToLower(members.RoleName(p.DefaultRole))
}

func init() {
	ssoCmd := &cobra.Command{
		Use:   "sso",
		Short: "Single sign-on (SAML 2.0) for the dashboard",
		Long: `Lets people sign in to the dashboard (and so use the Management API and the CLI) with the
identity provider of their company: Okta, Entra ID, Google Workspace or any SAML 2.0 provider.

  supavise sso add --metadata-url https://idp.example.com/metadata --domain example.com --default-role developer

registers the provider in supavise-gotrue@system and makes Studio's sign-in page offer "Continue with
SSO". A person signs in with an email address of one of the provider's domains. On the first
sign-in they become a member of the provider's organization with the default role, and only the
domains that a provider vouches for can do that. Anyone else who signs in through a registered
provider is refused on every route and listed by ` + "`supavise sso pending`" + `, until an administrator
approves them (` + "`supavise sso approve`" + `) or deletes the account (` + "`supavise sso deny`" + `). A denial, and
` + "`supavise users remove`" + ` of an SSO account, are remembered by email address: the person's next sign-in
creates a new account that waits for approval and does not get the default role again, until an
administrator approves it or runs ` + "`supavise sso allow`" + `.

Projects have identity providers of their own for their end users: use the Supabase CLI,
` + "`supabase sso add --project-ref <ref>`" + `, with the profile of this node.`,
	}

	var (
		addURL, addFile, addRole, addOrg, addNameID, addMapping string
		addDomains                                              []string
		addDisabled                                             bool
	)
	add := &cobra.Command{
		Use:   "add",
		Short: "Register a SAML identity provider",
		Long: `Registers the identity provider whose metadata is given (an https address, which GoTrue
fetches and refreshes, or a file) for the email domains named with --domain. Prints the provider
id on stdout and what to configure in the identity provider on stderr.

--default-role is the role a person gets on their first sign-in with an address of one of the
domains (owner, administrator, developer or read-only). Without it people wait for approval.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			md, err := sso.ResolveMetadata(ctx, addURL, addFile, nil)
			if err != nil {
				return err
			}
			role := 0
			if addRole != "" && strings.ToLower(addRole) != "none" {
				ro, err := members.ParseRole(addRole)
				if err != nil {
					return err
				}
				role = ro.ID
			}
			var am *sso.AttributeMapping
			if addMapping != "" {
				b, err := os.ReadFile(addMapping)
				if err != nil {
					return err
				}
				am = &sso.AttributeMapping{}
				if err := json.Unmarshal(b, am); err != nil {
					return fmt.Errorf("%s: %w", addMapping, err)
				}
			}
			d, acc, _, closeFn, err := openSSO(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			org, err := acc.Org(ctx, addOrg)
			if err != nil {
				return err
			}
			p, err := d.Add(ctx, nil, api.AddProvider{Org: org, Metadata: md, Domains: addDomains, DefaultRole: role,
				NameIDFormat: addNameID, AttributeMapping: am, Disabled: addDisabled})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p.ID)
			fmt.Fprintf(cmd.ErrOrStderr(), "Registered %s for %s in %s; first sign-in role: %s.\n%s\n",
				p.SAML.EntityID, strings.Join(p.DomainNames(), ", "), org.Slug, providerRole(p), spInstructions(d.ServiceProvider()))
			if md.URL == "" && md.XML != "" && addURL != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "The metadata was read from this machine and is not refreshed: update it with `supavise sso add` again when the provider's certificate changes.")
			}
			return nil
		},
	}
	add.Flags().StringVar(&addURL, "metadata-url", "", "https address of the identity provider's SAML metadata")
	add.Flags().StringVar(&addFile, "metadata-file", "", "file with the identity provider's SAML metadata")
	add.Flags().StringSliceVar(&addDomains, "domain", nil, "email domain the provider vouches for (repeat for several; at least one)")
	add.Flags().StringVar(&addRole, "default-role", "", "role of a first-time user: owner, administrator, developer, read-only; none: wait for approval")
	add.Flags().StringVar(&addOrg, "org", "", "organization slug (not needed when there is one)")
	add.Flags().StringVar(&addNameID, "name-id-format", "", "NameID format to ask for (a SAML URN); default persistent")
	add.Flags().StringVar(&addMapping, "attribute-mapping-file", "", "JSON file with GoTrue's attribute_mapping ({\"keys\": {\"email\": {\"name\": \"mail\"}}})")
	add.Flags().BoolVar(&addDisabled, "disabled", false, "register the provider switched off")

	var listJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List the identity providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, _, _, closeFn, err := openSSO(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			ps, err := d.List(cmd.Context(), 0)
			if err != nil {
				return err
			}
			if listJSON {
				type row struct {
					ID           string   `json:"id"`
					Organization string   `json:"organization,omitempty"`
					Domains      []string `json:"domains"`
					DefaultRole  string   `json:"default_role,omitempty"`
					EntityID     string   `json:"entity_id"`
					Registered   bool     `json:"registered"`
					Disabled     bool     `json:"disabled"`
				}
				rows := []row{}
				for _, p := range ps {
					r := row{ID: p.ID, Organization: p.OrgSlug, Domains: p.DomainNames(), Registered: p.Registered, Disabled: p.Disabled != nil && *p.Disabled}
					if p.SAML != nil {
						r.EntityID = p.SAML.EntityID
					}
					if p.DefaultRole != 0 {
						r.DefaultRole = strings.ToLower(members.RoleName(p.DefaultRole))
					}
					rows = append(rows, r)
				}
				return printJSON(cmd.OutOrStdout(), rows)
			}
			if len(ps) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "No identity providers. Add one with `supavise sso add`.")
				return nil
			}
			t := newTable(cmd.OutOrStdout())
			fmt.Fprintln(t, "ID\tORGANIZATION\tDOMAINS\tFIRST SIGN-IN\tENTITY ID\tSTATE")
			for _, p := range ps {
				state := "active"
				switch {
				case !p.Registered:
					state = "not registered with Supavise (its users are refused)"
				case p.Disabled != nil && *p.Disabled:
					state = "disabled"
				}
				entity := ""
				if p.SAML != nil {
					entity = p.SAML.EntityID
				}
				org := p.OrgSlug
				if org == "" {
					org = "-"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, org, strings.Join(p.DomainNames(), ","), providerRole(p), entity, state)
			}
			return t.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")

	info := &cobra.Command{
		Use:   "info",
		Short: "Print what an identity provider is configured with (ACS URL, entity id)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), spInstructions(sso.URLsFor(cfg.APIURL()+"/auth/v1")))
			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "remove <id|domain>",
		Short: "Remove an identity provider",
		Long: `Removes the provider from supavise-gotrue@system. The people who signed in through it lose their
dashboard sessions within seconds and their personal access tokens are revoked; their accounts
and memberships stay (` + "`supavise users remove <email>`" + ` deletes one). The role rules of its domains go.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, _, _, closeFn, err := openSSO(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			p, err := d.Find(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if _, err := d.Remove(cmd.Context(), nil, p.ID, 0); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s (%s)\n", p.ID, strings.Join(p.DomainNames(), ", "))
			return nil
		},
	}

	var pendOrg string
	var pendJSON bool
	pending := &cobra.Command{
		Use:   "pending",
		Short: "List the people who signed in through SSO and wait for approval",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, acc, _, closeFn, err := openSSO(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			var orgID int64
			if pendOrg != "" {
				org, err := acc.Org(cmd.Context(), pendOrg)
				if err != nil {
					return err
				}
				orgID = org.ID
			}
			us, err := d.Pending(cmd.Context(), orgID)
			if err != nil {
				return err
			}
			if pendJSON {
				return printJSON(cmd.OutOrStdout(), us)
			}
			if len(us) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "Nobody is waiting.")
				return nil
			}
			t := newTable(cmd.OutOrStdout())
			fmt.Fprintln(t, "EMAIL\tORGANIZATION\tFIRST SEEN\tUSER ID")
			for _, u := range us {
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", u.Email, u.OrgSlug, u.FirstSeen.Local().Format(time.DateTime), u.UserID)
			}
			return t.Flush()
		},
	}
	pending.Flags().StringVar(&pendOrg, "org", "", "only this organization")
	pending.Flags().BoolVar(&pendJSON, "json", false, "print JSON")

	var apprRole string
	approve := &cobra.Command{
		Use:   "approve <email|user id>",
		Short: "Let a waiting person in with a role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ro, err := members.ParseRole(apprRole)
			if err != nil {
				return err
			}
			d, _, _, closeFn, err := openSSO(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			u, err := d.FindPending(cmd.Context(), 0, args[0])
			if err != nil {
				return err
			}
			if err := d.Approve(cmd.Context(), nil, members.OrgRef{ID: u.OrgID, Slug: u.OrgSlug}, u.UserID, ro.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s in %s\n", u.Email, strings.ToLower(ro.Name), u.OrgSlug)
			return nil
		},
	}
	approve.Flags().StringVar(&apprRole, "role", "developer", "owner, administrator, developer or read-only")

	deny := &cobra.Command{
		Use:   "deny <email|user id>",
		Short: "Refuse a waiting person: the account is deleted",
		Long: `Deletes the account of a waiting person and ends their sessions. The refusal is kept by email
address: signing in again creates a new account, which waits for approval again and does not get the
provider's default role. Approving that account (supavise sso approve), or supavise sso allow, lifts it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, _, _, closeFn, err := openSSO(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			u, err := d.FindPending(cmd.Context(), 0, args[0])
			if err != nil {
				return err
			}
			if err := d.Deny(cmd.Context(), nil, members.OrgRef{ID: u.OrgID, Slug: u.OrgSlug}, u.UserID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "denied %s\n", u.Email)
			return nil
		},
	}

	var allowProvider string
	allow := &cobra.Command{
		Use:   "allow <email>",
		Short: "Lift the refusal of an address: the default role applies again",
		Long: `Forgets that an administrator denied an address (supavise sso deny) or removed its SSO account
(supavise users remove), so that the person's next request gets the provider's default role, if it has
one (an account of theirs that is waiting is treated as new). The provider is the one that serves the address's domain; --provider names it by id when the
domain has changed hands. Approving the person's waiting account (supavise sso approve) lifts the
refusal too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, _, _, closeFn, err := openSSO(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			who := allowProvider
			if who == "" {
				at := strings.LastIndex(args[0], "@")
				if at < 0 {
					return fmt.Errorf("%q is not an email address", args[0])
				}
				who = args[0][at+1:]
			}
			p, err := d.Find(cmd.Context(), who)
			if err != nil {
				return err
			}
			if err := d.Allow(cmd.Context(), p.ID, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is no longer refused at %s\n", strings.ToLower(args[0]), p.ID)
			return nil
		},
	}
	allow.Flags().StringVar(&allowProvider, "provider", "", "the identity provider (id or domain) instead of the address's domain")

	ssoCmd.AddCommand(add, list, info, remove, pending, approve, deny, allow)
	rootCmd.AddCommand(ssoCmd)
}
