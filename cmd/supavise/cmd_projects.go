package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/functions"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
	"github.com/supavise/supavise/internal/secrets"
)

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Create and manage projects",
	Long: `Projects run as units of their own: PostgreSQL, GoTrue and PostgREST. These commands
call the lifecycle library in this process and need the system cluster to be running
(supavise system init). Run them as the user that owns the state directory (supavise).`,
}

var (
	pName, pOrg, pClass, pRef, pRegion string
	pJSON, pShowKeys                   bool
	pSkipBackup                        bool
)

type projectView struct {
	Ref      string              `json:"ref"`
	Name     string              `json:"name"`
	Status   string              `json:"status"`
	Class    string              `json:"class"`
	Region   string              `json:"region"`
	APIURL   string              `json:"api_url"`
	Ports    config.ProjectPorts `json:"ports"`
	Versions map[string]string   `json:"versions"`
	Limits   config.Limits       `json:"limits"`
	Keys     *keyView            `json:"keys,omitempty"`
}

type keyView struct {
	PublishableKey string `json:"publishable_key"`
	SecretKey      string `json:"secret_key"`
	AnonKey        string `json:"anon_key"`
	ServiceRoleKey string `json:"service_role_key"`
	JWTSecret      string `json:"jwt_secret"`
	DBPassword     string `json:"db_password"`
}

func viewKeys(k *secrets.ProjectKeys) *keyView {
	return &keyView{PublishableKey: k.PublishableKey, SecretKey: k.SecretKey, AnonKey: k.AnonKey,
		ServiceRoleKey: k.ServiceRoleKey, JWTSecret: k.JWTSecret, DBPassword: k.DBPassword}
}

func viewOf(cfg *config.Config, p *registry.Project) projectView {
	scheme := "https"
	if cfg.TLS.Mode == "off" {
		scheme = "http"
	}
	return projectView{Ref: p.Ref, Name: p.Name, Status: string(p.Status), Class: p.Class, Region: p.Region,
		APIURL: scheme + "://" + cfg.ProjectHost(p.Ref), Ports: cfg.PortsFor(p.Ref, p.Seq), Versions: p.Versions, Limits: p.Limits}
}

func printProject(w io.Writer, v projectView) {
	fmt.Fprintf(w, "ref:       %s\nname:      %s\nstatus:    %s\nclass:     %s\napi url:   %s\npostgres:  127.0.0.1:%d\ngotrue:    127.0.0.1:%d\npostgrest: 127.0.0.1:%d\n",
		v.Ref, v.Name, v.Status, v.Class, v.APIURL, v.Ports.Postgres, v.Ports.GoTrue, v.Ports.PostgREST)
	if k := v.Keys; k != nil {
		fmt.Fprintf(w, "publishable key:  %s\nsecret key:       %s\nanon key:         %s\nservice_role key: %s\njwt secret:       %s\ndb password:      %s\n",
			k.PublishableKey, k.SecretKey, k.AnonKey, k.ServiceRoleKey, k.JWTSecret, k.DBPassword)
	}
}

func projectCmd(use, short string, args cobra.PositionalArgs, run func(cmd *cobra.Command, n *lifecycle.Node, args []string) error) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, a []string) error {
		n, err := openNode(cmd.Context())
		if err != nil {
			return err
		}
		defer n.Close()
		return run(cmd, n, a)
	}}
}

func init() {
	create := projectCmd("create", "Create a project and wait until it is healthy", cobra.NoArgs,
		func(cmd *cobra.Command, n *lifecycle.Node, _ []string) error {
			p, err := n.Engine.Create(cmd.Context(), lifecycle.CreateRequest{Name: pName, OrgSlug: pOrg, Class: pClass, Ref: pRef, Region: pRegion})
			if err != nil {
				return err
			}
			v := viewOf(n.Cfg, p)
			if pShowKeys {
				k, err := n.Engine.Keys(cmd.Context(), p.Ref)
				if err != nil {
					return err
				}
				v.Keys = viewKeys(k)
			}
			if pJSON {
				return printJSON(cmd.OutOrStdout(), v)
			}
			printProject(cmd.OutOrStdout(), v)
			return nil
		})
	create.Flags().StringVar(&pName, "name", "", "project name (default: the ref)")
	create.Flags().StringVar(&pOrg, "org", "", "organization slug (default \"default\", created on first use)")
	create.Flags().StringVar(&pClass, "size", "", fmt.Sprintf("compute size %v (default %s; a size named here must fit the node, see `projects sizes`)", lifecycle.ClassNames(), lifecycle.DefaultClass))
	create.Flags().StringVar(&pClass, "class", "", "same as --size")
	_ = create.Flags().MarkHidden("class")
	create.Flags().StringVar(&pRef, "ref", "", "force the ref (20 lowercase letters); default random")
	create.Flags().StringVar(&pRegion, "region", "", "AWS region code shown for the project (default: region in the config, us-east-1)")
	create.Flags().BoolVar(&pShowKeys, "show-keys", false, "print the project's keys and database password")
	create.Flags().BoolVar(&pJSON, "json", false, "print JSON")

	list := projectCmd("list", "List projects", cobra.NoArgs, func(cmd *cobra.Command, n *lifecycle.Node, _ []string) error {
		ps, err := n.Registry.ListProjects(cmd.Context())
		if err != nil {
			return err
		}
		if pJSON {
			vs := make([]projectView, 0, len(ps))
			for i := range ps {
				vs = append(vs, viewOf(n.Cfg, &ps[i]))
			}
			return printJSON(cmd.OutOrStdout(), vs)
		}
		t := newTable(cmd.OutOrStdout())
		fmt.Fprintln(t, "REF\tNAME\tSTATUS\tCLASS\tPOSTGRES")
		for _, p := range ps {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t127.0.0.1:%d\n", p.Ref, p.Name, p.Status, p.Class, n.Cfg.PortsFor(p.Ref, p.Seq).Postgres)
		}
		return t.Flush()
	})
	list.Flags().BoolVar(&pJSON, "json", false, "print JSON")

	get := projectCmd("get <ref>", "Show one project", cobra.ExactArgs(1), func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
		p, err := n.Registry.GetProject(cmd.Context(), a[0])
		if err != nil {
			return err
		}
		v := viewOf(n.Cfg, p)
		if pShowKeys {
			k, err := n.Engine.Keys(cmd.Context(), p.Ref)
			if err != nil {
				return err
			}
			v.Keys = viewKeys(k)
		}
		if pJSON {
			return printJSON(cmd.OutOrStdout(), v)
		}
		printProject(cmd.OutOrStdout(), v)
		return nil
	})
	get.Flags().BoolVar(&pShowKeys, "show-keys", false, "print the project's keys and database password")
	get.Flags().BoolVar(&pJSON, "json", false, "print JSON")

	pause := projectCmd("pause <ref>", "Stop GoTrue, PostgREST and PostgreSQL; keep the data", cobra.ExactArgs(1),
		func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
			if err := n.Engine.Pause(cmd.Context(), a[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s paused\n", a[0])
			return nil
		})
	resume := projectCmd("resume <ref>", "Start a paused project", cobra.ExactArgs(1),
		func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
			if err := n.Engine.Resume(cmd.Context(), a[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s resumed\n", a[0])
			return nil
		})
	del := projectCmd("delete <ref>", "Delete a project: final base backup, tenants, units, data", cobra.ExactArgs(1),
		func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
			// The final base backup waits for its WAL to be archived through the daemon's
			// relay; serve the sockets nobody answers while the daemon is down.
			_, stopRelay := app.StartWALRelay(cmd.Context(), n.Cfg, newLogger(n.Cfg), true, a[0])
			defer stopRelay()
			if err := deleteProject(cmd.Context(), n, a[0], pSkipBackup); err != nil {
				return err
			}
			// The Edge Functions tree lives in the runtime's state directory, not in the
			// project's: remove it now rather than at the next reconcile of a running daemon.
			if err := functions.RemoveFiles(n.Cfg, a[0]); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: removing the Edge Functions files of %s: %v\n", a[0], err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", a[0])
			return nil
		})
	del.Flags().BoolVar(&pSkipBackup, "skip-final-backup", false, "delete without the final base backup (the data is gone)")

	rotate := projectCmd("rotate-keys <ref>", "Issue a new JWT secret and API keys and restart GoTrue and PostgREST", cobra.ExactArgs(1),
		func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
			k, err := n.Engine.RotateKeys(cmd.Context(), a[0])
			if err != nil {
				return err
			}
			// Functions check JWTs against the project's secret from a file: write the new one
			// now instead of at the next reconcile of a running API server (up to
			// [functions] reconcile_seconds of 401s for the new keys and 200s for the old).
			if n.Cfg.Functions.Enabled {
				if err := syncFunctions(cmd.Context(), n, a[0]); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: the Edge Functions of %s still check the old JWT secret until the API server's next reconcile: %v\n", a[0], err)
				}
			}
			if pJSON {
				return printJSON(cmd.OutOrStdout(), viewKeys(k))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "keys of %s rotated\npublishable key:  %s\nsecret key:       %s\nanon key:         %s\nservice_role key: %s\n",
				a[0], k.PublishableKey, k.SecretKey, k.AnonKey, k.ServiceRoleKey)
			return nil
		})
	rotate.Flags().BoolVar(&pJSON, "json", false, "print JSON")

	health := projectCmd("health <ref>", "Check each service with a real request", cobra.ExactArgs(1),
		func(cmd *cobra.Command, n *lifecycle.Node, a []string) error {
			hs, err := n.Engine.Health(cmd.Context(), a[0])
			if err != nil {
				return err
			}
			if pJSON {
				if err := printJSON(cmd.OutOrStdout(), hs); err != nil {
					return err
				}
			} else {
				healthTable(cmd.OutOrStdout(), hs)
			}
			if !healthy(hs) {
				return fmt.Errorf("project %s is not healthy", a[0])
			}
			return nil
		})
	health.Flags().BoolVar(&pJSON, "json", false, "print JSON")

	projectsCmd.AddCommand(create, list, get, pause, resume, del, rotate, health)
	rootCmd.AddCommand(projectsCmd)
}
