package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/api"
	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/branching"
	"github.com/supavise/supavise/internal/functions"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

type orgView struct {
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Projects []string `json:"projects"`
}

func init() {
	orgsCmd := &cobra.Command{
		Use:   "orgs",
		Short: "List and delete organizations",
		Long: `Organizations group projects and the people who work on them. Creating one is the dashboard's
job (Studio's "New organization") or the first project's (` + "`supavise projects create --org`" + `); these commands
list them and delete one, which the dashboard also does for an Owner. Run them as the user that owns
the state directory (supavise); the system cluster must be running.`,
	}

	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List organizations with their projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := openNode(cmd.Context())
			if err != nil {
				return err
			}
			defer n.Close()
			orgs, err := n.Registry.ListOrganizations(cmd.Context())
			if err != nil {
				return err
			}
			ps, err := n.Registry.ListProjects(cmd.Context())
			if err != nil {
				return err
			}
			views := make([]orgView, 0, len(orgs))
			for _, o := range orgs {
				v := orgView{Slug: o.Slug, Name: o.Name, Projects: []string{}}
				for _, p := range ps {
					if p.OrgID == o.ID {
						v.Projects = append(v.Projects, p.Ref)
					}
				}
				views = append(views, v)
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), views)
			}
			t := newTable(cmd.OutOrStdout())
			fmt.Fprintln(t, "SLUG\tNAME\tPROJECTS")
			for _, v := range views {
				fmt.Fprintf(t, "%s\t%s\t%d\n", v.Slug, v.Name, len(v.Projects))
			}
			return t.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var yes bool
	del := &cobra.Command{
		Use:   "delete <slug>",
		Short: "Delete an organization with its projects and everything keyed by it",
		Long: `Deletes the organization as the dashboard's "Delete organization" does, and as hosted Supabase does:
its projects are deleted (each takes its final base backup, as ` + "`supavise projects delete`" + ` does), its
single sign-on providers are removed (their users lose their seats and personal access tokens), and
its members, project roles, invitations, invite tokens, MFA setting and default-role rules go with it.
Without --yes the command lists what it would delete and stops.

The last organization of a node is never deleted: the dashboard would give the next person who opens
it a new "Default" organization to own. A run that stopped on a project (its final backup failed)
leaves the organization in place with the projects not yet reached; run it again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			ctx := cmd.Context()
			n, err := openNode(ctx)
			if err != nil {
				return err
			}
			defer n.Close()
			org, err := n.Registry.GetOrganization(ctx, a[0])
			if errors.Is(err, registry.ErrNotFound) {
				return fmt.Errorf("no organization %q (supavise orgs list)", a[0])
			}
			if err != nil {
				return err
			}
			sso, acc, err := openSSOOn(cmd, n)
			if err != nil {
				return err
			}
			var bsvc *branching.Service
			defer func() {
				if bsvc != nil {
					bsvc.Drain(context.Background())
				}
			}()
			d := &api.OrgDeleter{Reg: n.Registry, Members: acc.Members, SSO: sso, Claims: acc.Store, Log: newLogger(n.Cfg)}
			ps, err := d.Projects(ctx, org)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			refs := make([]string, 0, len(ps))
			for _, p := range ps {
				refs = append(refs, p.Ref)
			}
			providers, err := sso.Store.ListProviders(ctx)
			if err != nil {
				return err
			}
			nProviders := 0
			for _, p := range providers {
				if p.OrgID == org.ID {
					nProviders++
				}
			}
			if !yes {
				fmt.Fprintf(out, "organization %s (%s) would be deleted with %d project(s)", org.Slug, org.Name, len(ps))
				if len(refs) > 0 {
					fmt.Fprintf(out, ": %s", strings.Join(refs, ", "))
				}
				fmt.Fprintf(out, ", and %d single sign-on provider(s).\n", nProviders)
				return fmt.Errorf("nothing deleted; run it again with --yes to delete")
			}
			d.DeleteProject = func(ctx context.Context, p registry.Project) error {
				if p.Branch != nil {
					if bsvc == nil {
						b, err := newBranching(ctx, n.Cfg, n)
						if err != nil {
							return err
						}
						bsvc = b
					}
					if _, err := bsvc.Delete(ctx, p.Ref, branching.DeleteOptions{}); err != nil {
						return err
					}
				} else if err := n.Engine.DeleteWith(ctx, p.Ref, lifecycle.DeleteOptions{}); err != nil {
					return err
				}
				// The Edge Functions tree lives in the runtime's state directory, not in the
				// project's: remove it now rather than at the next reconcile of a running daemon.
				if err := functions.RemoveFiles(n.Cfg, p.Ref); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: removing the Edge Functions files of %s: %v\n", p.Ref, err)
				}
				fmt.Fprintf(out, "%s deleted\n", p.Ref)
				return nil
			}
			// The final base backups wait for their WAL to be archived through the daemon's
			// relay; serve the sockets nobody answers while the daemon is down.
			if len(refs) > 0 {
				_, stopRelay := app.StartWALRelay(ctx, n.Cfg, newLogger(n.Cfg), true, refs...)
				defer stopRelay()
			}
			if _, err := d.Delete(ctx, nil, org); err != nil {
				return err
			}
			fmt.Fprintf(out, "organization %s deleted\n", org.Slug)
			return nil
		},
	}
	del.Flags().BoolVar(&yes, "yes", false, "delete; without it the command only lists what would go")

	orgsCmd.AddCommand(list, del)
	rootCmd.AddCommand(orgsCmd)
}
