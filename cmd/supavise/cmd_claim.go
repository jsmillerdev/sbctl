package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/members"
	"github.com/supavise/supavise/internal/registry"
)

// openAccounts connects to the registry and returns the account service the claim page
// uses, for the CLI. The system project must be initialized and running (the registry
// lives in it); user creation also needs supavise-gotrue@system.
func openAccounts(ctx context.Context) (*api.Accounts, *config.Config, func(), error) {
	n, err := openNode(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	pg, ok := n.Registry.(*registry.Postgres)
	if !ok {
		n.Close()
		return nil, nil, nil, fmt.Errorf("the registry is %T, want Postgres", n.Registry)
	}
	acc := &api.Accounts{Reg: n.Registry, Store: api.NewPGClaimStore(pg.Pool()), Keys: n.Engine.Keys, Config: n.Cfg, Log: newLogger(n.Cfg)}
	acc.EnableMembers(n.Registry, api.NewPGStore(pg.Pool()))
	// `users remove` forgets an SSO account's record and remembers its removal by address, as the
	// daemon's does (the daemon gets this from NewDashboardSSO).
	acc.SSOUsers = api.NewPGSSOStore(pg.Pool())
	// `users remove` also ends the user's OAuth grants, as the daemon's does.
	acc.OAuth = newOAuthService(n, pg)
	return acc, n.Cfg, n.Close, nil
}

func init() {
	claim := &cobra.Command{
		Use:   "claim",
		Short: "The token that creates the first dashboard administrator",
		Long: `supavise-gotrue@system has public sign-up disabled. The first administrator is created
with a claim token (single use, expires), on the claim page at https://api.<domain>/claim.
The installer prints one; ` + "`supavise claim token`" + ` issues another while nobody has claimed yet.
Later users come by invitation: ` + "`supavise users invite <email> --role developer`" + `.`,
	}

	var (
		ttl    time.Duration
		force  bool
		ifNone bool
		file   string
	)
	token := &cobra.Command{
		Use:   "token",
		Short: "Issue a claim token (revokes the unused ones)",
		Long: `Prints a new claim token on stdout (nothing else, so a script can capture it) and where to
use it on stderr. Fails when the first administrator already exists, unless --force
(for an administrator locked out of the dashboard). --file also writes the token to a
file readable only by its owner.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			acc, cfg, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			if ifNone {
				live, err := acc.HasLiveClaimToken(cmd.Context())
				if err != nil {
					return err
				}
				if live {
					// Nothing on stdout: the earlier token cannot be shown again, and replacing it
					// would break whatever stored it (a re-run of the installer, a secret).
					fmt.Fprintln(cmd.ErrOrStderr(), "A claim token that nobody has used and that has not expired exists; none was issued.")
					return nil
				}
			}
			tok, exp, err := acc.IssueClaimToken(cmd.Context(), ttl, force)
			if err != nil {
				return err
			}
			if file != "" {
				if err := writeSecretFile(file, []byte(tok+"\n")); err != nil {
					return err
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)
			fmt.Fprintf(cmd.ErrOrStderr(), "Claim page: %s/claim\nThe token works once and expires %s.\n", cfg.APIURL(), exp.Local().Format(time.RFC1123))
			return nil
		},
	}
	token.Flags().DurationVar(&ttl, "ttl", api.DefaultClaimTTL, "how long the token stays valid")
	token.Flags().BoolVar(&force, "force", false, "issue a token although the first administrator already exists")
	token.Flags().BoolVar(&ifNone, "if-none", false, "issue nothing (and print nothing) while an unused, unexpired claim token exists")
	token.Flags().StringVar(&file, "file", "", "also write the token to this file (mode 0600)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Print claimed or unclaimed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			ok, err := acc.Claimed(cmd.Context())
			if err != nil {
				return err
			}
			if ok {
				fmt.Fprintln(cmd.OutOrStdout(), "claimed")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "unclaimed")
			}
			return nil
		},
	}
	claim.AddCommand(token, status)
	rootCmd.AddCommand(claim)

	users := &cobra.Command{
		Use:   "users",
		Short: "Dashboard users, their roles and invitations",
		Long: `The accounts that can sign in to the dashboard and use the Management API, and what each
may do. A member has a role in an organization, as on hosted Supabase:

  owner          everything, including other owners and deleting the organization
  administrator  everything except organization settings, owners and project transfer
  developer      project content (data, schema, users, files, functions), no settings or keys
  read-only      read, and SELECT-only SQL; no service key or JWT secret

A role can also be limited to projects (` + "`--project`" + `). An organization always keeps one owner.
The dashboard's Team page and the Management API change the same roles.`,
	}
	var (
		inviteTTL     time.Duration
		inviteRole    string
		inviteOrg     string
		inviteProject []string
		inviteNoMail  bool
	)
	invite := &cobra.Command{
		Use:   "invite <email>",
		Short: "Invite an address to an organization with a role; prints the link",
		Long: `Invites an address to an organization with a role (default developer) and prints the link to
give the invitee on stdout. A new address gets the account page: the invitee picks a password
there and joins with the invited role at once. An address that already has an account gets the
dashboard's invitation page: sign in and accept. The link works once and expires after seven
days. Inviting the same address again replaces the earlier invitation.

When [mail] is set in config.toml, supavise-gotrue@system also emails the invitation (the link is
printed either way). Without it Supavise sends no email: pass the link on yourself.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			if inviteTTL > 0 {
				acc.Members.InvitationTTL = inviteTTL
			}
			acc.NoMail = inviteNoMail
			res, org, err := acc.InviteByEmail(cmd.Context(), args[0], inviteOrg, inviteRole, inviteProject)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Link())
			scope := ""
			if len(inviteProject) > 0 {
				scope = " on " + strings.Join(inviteProject, ", ")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Invited %s to %s as %s%s; the link works once and expires %s.\n",
				res.Invitation.Email, org.Slug, inviteRole, scope, res.Invitation.ExpiresAt.Local().Format(time.RFC1123))
			if res.Emailed {
				fmt.Fprintln(cmd.ErrOrStderr(), "The invitation was also sent by email.")
			} else if res.MailError != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "The email could not be sent ("+res.MailError+"); give the invitee the link.")
			}
			if res.ClaimURL != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "This address has no account yet: the link creates it and accepts the invitation.")
			}
			return nil
		},
	}
	invite.Flags().StringVar(&inviteRole, "role", "developer", "owner, administrator, developer or read-only")
	invite.Flags().StringVar(&inviteOrg, "org", "", "organization slug (not needed when there is one)")
	invite.Flags().StringSliceVar(&inviteProject, "project", nil, "limit the role to these project refs")
	invite.Flags().BoolVar(&inviteNoMail, "no-mail", false, "do not send the email even when [mail] is configured; print the link only")
	invite.Flags().DurationVar(&inviteTTL, "ttl", members.DefaultInvitationTTL, "how long the invitation stays valid")

	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List dashboard users and their roles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			us, err := acc.ListUsers(cmd.Context())
			if err != nil {
				return err
			}
			type row struct {
				api.DashboardUser
				Roles []string `json:"roles"`
			}
			rows := make([]row, 0, len(us))
			for _, u := range us {
				roles, err := acc.UserRoles(cmd.Context(), u.ID)
				if err != nil {
					return err
				}
				rows = append(rows, row{u, roles})
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), rows)
			}
			t := newTable(cmd.OutOrStdout())
			fmt.Fprintln(t, "EMAIL\tSIGN-IN\tROLES\tCREATED\tLAST SIGN-IN\tID")
			for _, u := range rows {
				last := "never"
				if u.LastSignIn != nil {
					last = u.LastSignIn.Local().Format("2006-01-02 15:04")
				}
				roles := strings.Join(u.Roles, ", ")
				if roles == "" {
					roles = "none"
				}
				signIn := "password"
				if u.SSOProvider != "" {
					signIn = "sso:" + u.SSOProvider
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", u.Email, signIn, roles, u.CreatedAt.Local().Format("2006-01-02"), last, u.ID)
			}
			return t.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var roleOrg, roleUserID, roleProvider string
	role := &cobra.Command{
		Use:   "role <email> <role>",
		Short: "Set the organization-wide role of a dashboard user",
		Long: `Sets a user's role in an organization (owner, administrator, developer or read-only) and adds
the user to the organization when they are not a member. It is how to give an organization an
owner again after the last one was removed with --force. The last owner of an organization cannot
be demoted.

An address can belong to a password account and to accounts that a single sign-on identity
provider created (the provider vouches for the address; GoTrue does not keep those unique). The
password account is the one this command changes. Name another with --user-id or --provider
(` + "`supavise users list`" + ` shows the ids and how each account signs in); an address that several
accounts share without a password account among them is refused until you do.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			org, u, err := acc.SetRoleOf(cmd.Context(), args[0], roleOrg, args[1], api.UserSelector{UserID: roleUserID, Provider: roleProvider})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s in %s\n", args[0], args[1], org.Slug)
			if u != nil && u.SSOProvider != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "The account is the single sign-on account %s of provider %s.\n", u.ID, u.SSOProvider)
			}
			return nil
		},
	}
	role.Flags().StringVar(&roleOrg, "org", "", "organization slug (not needed when there is one)")
	role.Flags().StringVar(&roleUserID, "user-id", "", "the account's id, when the address belongs to several accounts")
	role.Flags().StringVar(&roleProvider, "provider", "", "email, or a single sign-on provider id, when the address belongs to several accounts")

	var forceRemove bool
	var removeUserID, removeProvider string
	remove := &cobra.Command{
		Use:   "remove <email>",
		Short: "Delete a dashboard user, their memberships and the access tokens they created",
		Long: `Deletes the account from supavise-gotrue@system, the user's memberships and roles, and every
personal access token it created (a token is not re-checked against its owner's account, so it
would keep working). It also revokes the user's OAuth grants. Sessions already issued by GoTrue
expire within an hour.

An organization always keeps an owner: removing the only owner of an organization is refused
unless --force is given.

An address can belong to a password account and to accounts that a single sign-on identity
provider created. The password account is the one removed; name another with --user-id or
--provider (` + "`supavise users list`" + ` shows the ids).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			u, n, err := acc.RemoveUserBy(cmd.Context(), args[0], forceRemove, api.UserSelector{UserID: removeUserID, Provider: removeProvider})
			if err != nil {
				return err
			}
			what := args[0]
			if u != nil && u.SSOProvider != "" {
				what += " (single sign-on account of provider " + u.SSOProvider + ")"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s and %d access token(s)\n", what, n)
			return nil
		},
	}
	remove.Flags().BoolVar(&forceRemove, "force", false, "also remove the only owner of an organization")
	remove.Flags().StringVar(&removeUserID, "user-id", "", "the account's id, when the address belongs to several accounts")
	remove.Flags().StringVar(&removeProvider, "provider", "", "email, or a single sign-on provider id, when the address belongs to several accounts")

	defaults := &cobra.Command{
		Use:   "default-role",
		Short: "The role a user gets on the first SSO sign-in, by email domain",
		Long: `When single sign-on is set up, a user who signs in for the first time becomes a member of the
organization and role that their email domain maps to here. A domain without a rule gets no
access until an administrator invites or approves them. A rule applies only to a domain that a
registered identity provider vouches for (` + "`supavise sso add --domain`" + ` sets the rules of its domains;
this command sets one by hand).`,
	}
	var defOrg string
	defSet := &cobra.Command{
		Use:   "set <domain> <role>",
		Short: "Map an email domain to a role",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			org, err := acc.SetDomainDefault(cmd.Context(), args[0], defOrg, args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s signs in as %s in %s\n", args[0], args[1], org.Slug)
			return nil
		},
	}
	defSet.Flags().StringVar(&defOrg, "org", "", "organization slug (not needed when there is one)")
	defList := &cobra.Command{
		Use:   "list",
		Short: "List the rules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			rules, err := acc.Members.DomainDefaults(cmd.Context())
			if err != nil {
				return err
			}
			t := newTable(cmd.OutOrStdout())
			fmt.Fprintln(t, "DOMAIN\tORGANIZATION\tROLE")
			for _, r := range rules {
				slug := fmt.Sprint(r.OrgID)
				if o, err := acc.Reg.GetOrganizationByID(cmd.Context(), r.OrgID); err == nil {
					slug = o.Slug
				}
				fmt.Fprintf(t, "%s\t%s\t%s\n", r.Domain, slug, strings.ToLower(members.RoleName(r.RoleID)))
			}
			return t.Flush()
		},
	}
	defRemove := &cobra.Command{
		Use:   "remove <domain>",
		Short: "Delete a rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			return acc.Members.RemoveDomainDefault(cmd.Context(), args[0])
		},
	}
	defaults.AddCommand(defSet, defList, defRemove)
	users.AddCommand(invite, list, role, remove, defaults)
	rootCmd.AddCommand(users)
}
