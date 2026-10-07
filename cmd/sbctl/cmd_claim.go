package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
)

// openAccounts connects to the registry and returns the account service the claim page
// uses, for the CLI. The system project must be initialized and running (the registry
// lives in it); user creation also needs sb-gotrue@system.
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
	return acc, n.Cfg, n.Close, nil
}

func init() {
	claim := &cobra.Command{
		Use:   "claim",
		Short: "The token that creates the first dashboard administrator",
		Long: `sb-gotrue@system has public sign-up disabled. The first administrator is created
with a claim token (single use, expires), on the claim page at https://api.<domain>/claim.
The installer prints one; ` + "`sbctl claim token`" + ` issues another while nobody has claimed yet.
Later users come by invitation: ` + "`sbctl users invite <email>`" + `.`,
	}

	var (
		ttl   time.Duration
		force bool
		file  string
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
		Short: "Dashboard users",
		Long: `The accounts that can sign in to the dashboard and use the Management API. Every
account is an administrator: members and roles are a later phase.`,
	}
	var inviteTTL time.Duration
	invite := &cobra.Command{
		Use:   "invite <email>",
		Short: "Create an invite token for an address",
		Long: `Prints an invite token on stdout. The invitee opens https://api.<domain>/claim, enters the
token and picks a password; the account is created then, with that address. The token
works once and expires. Inviting the same address again revokes the earlier invite.
sbctl sends no email: pass the token on yourself.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, cfg, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			tok, exp, err := acc.IssueInvite(cmd.Context(), args[0], inviteTTL)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), tok)
			fmt.Fprintf(cmd.ErrOrStderr(), "Claim page: %s/claim\nThe invite works once and expires %s.\n", cfg.APIURL(), exp.Local().Format(time.RFC1123))
			return nil
		},
	}
	invite.Flags().DurationVar(&inviteTTL, "ttl", api.DefaultInviteTTL, "how long the invite stays valid")

	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List dashboard users",
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
			if asJSON {
				return printJSON(cmd.OutOrStdout(), us)
			}
			t := newTable(cmd.OutOrStdout())
			fmt.Fprintln(t, "EMAIL\tADMIN\tCREATED\tLAST SIGN-IN")
			for _, u := range us {
				last := "never"
				if u.LastSignIn != nil {
					last = u.LastSignIn.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(t, "%s\t%v\t%s\t%s\n", u.Email, u.Admin, u.CreatedAt.Local().Format("2006-01-02"), last)
			}
			return t.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	remove := &cobra.Command{
		Use:   "remove <email>",
		Short: "Delete a dashboard user and the access tokens they created",
		Long: `Deletes the account from sb-gotrue@system and every personal access token it created
(a token is not re-checked against its owner's account, so it would keep working).
Sessions already issued by GoTrue expire within an hour.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, _, closeFn, err := openAccounts(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			n, err := acc.RemoveUser(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s and %d access token(s)\n", args[0], n)
			return nil
		},
	}
	users.AddCommand(invite, list, remove)
	rootCmd.AddCommand(users)
}
