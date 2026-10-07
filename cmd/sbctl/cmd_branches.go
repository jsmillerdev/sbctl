package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/app"
	"github.com/OWNER/sbctl/internal/backup"
	"github.com/OWNER/sbctl/internal/branching"
	"github.com/OWNER/sbctl/internal/registry"
)

// openBranching connects to the node and builds the branching service. The backup backend
// is optional: without one, with_data needs a copy-on-write filesystem and the archive of a
// deleted branch is not cleaned up.
func openBranching(ctx context.Context) (*branching.Service, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	log := newLogger(cfg)
	n, err := openNode(ctx)
	if err != nil {
		return nil, nil, err
	}
	var bk *backup.Service
	if bk, err = app.NewBackupService(ctx, cfg, n.Registry, n.Secrets, appOptions(cfg)); err != nil {
		log.Warn("no usable backup backend: with_data needs a copy-on-write filesystem here", "err", err)
		bk = nil
	} else {
		bk.SetManager(n.Engine)
	}
	deps := branching.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Engine: n.Engine, Backup: bk, Log: log}
	if pg, ok := n.Registry.(*registry.Postgres); ok {
		deps.Functions = api.FunctionDigests(api.NewPGStore(pg.Pool()))
	}
	svc, err := branching.New(deps)
	if err != nil {
		n.Close()
		return nil, nil, err
	}
	return svc, func() { svc.Drain(context.Background()); n.Close() }, nil
}

type branchView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Ref         string     `json:"project_ref"`
	Parent      string     `json:"parent_project_ref"`
	Default     bool       `json:"is_default"`
	GitBranch   string     `json:"git_branch,omitempty"`
	Persistent  bool       `json:"persistent"`
	WithData    bool       `json:"with_data"`
	State       string     `json:"state"`
	Project     string     `json:"project_status"`
	CloneMethod string     `json:"clone_method,omitempty"`
	Egress      string     `json:"egress,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	DeleteAt    *time.Time `json:"deletion_scheduled_at,omitempty"`
}

func viewBranch(b *branching.Branch) branchView {
	return branchView{
		ID: b.ID, Name: b.Name, Ref: b.Ref, Parent: b.ParentRef, Default: b.IsDefault, GitBranch: b.GitBranch, Persistent: b.Persistent,
		WithData: b.WithData, State: string(b.State), Project: string(b.ProjectStatus), CloneMethod: b.CloneMethod, Egress: b.Egress, Detail: b.Detail,
		CreatedAt: b.CreatedAt, ExpiresAt: b.ExpiresAt, DeleteAt: b.DeletionScheduledAt,
	}
}

func printBranch(w io.Writer, b *branching.Branch) {
	v := viewBranch(b)
	fmt.Fprintf(w, "name:     %s%s\nid:       %s\nref:      %s\nparent:   %s\nstatus:   %s (project %s)\n", v.Name, map[bool]string{true: " (default)"}[v.Default], v.ID, v.Ref, v.Parent, v.State, v.Project)
	if v.Default {
		return
	}
	fmt.Fprintf(w, "data:     %s\npersist:  %v\n", orDash(v.CloneMethod), v.Persistent)
	if v.WithData {
		fmt.Fprintf(w, "egress:   %s\n", orDash(v.Egress))
	}
	if v.ExpiresAt != nil {
		fmt.Fprintf(w, "expires:  %s\n", v.ExpiresAt.Local().Format(time.RFC3339))
	}
	if v.DeleteAt != nil {
		fmt.Fprintf(w, "deleted:  scheduled for %s\n", v.DeleteAt.Local().Format(time.RFC3339))
	}
	if v.Detail != "" {
		fmt.Fprintf(w, "detail:   %s\n", v.Detail)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// selectBranch finds a branch by id or ref, or by name within --project.
func selectBranch(ctx context.Context, svc *branching.Service, project, arg string) (*branching.Branch, error) {
	if project != "" {
		return svc.Get(ctx, project, arg)
	}
	return svc.Resolve(ctx, arg)
}

// waitDone waits for the operation on ref and returns an error when it failed.
func waitDone(ctx context.Context, svc *branching.Service, ref string) (*branching.Branch, error) {
	b, err := svc.WaitIdle(ctx, ref)
	if err != nil {
		return nil, err
	}
	if b.State == registry.BranchMigrationsFailed {
		return b, fmt.Errorf("branch %s: %s", b.Name, b.Detail)
	}
	return b, nil
}

func init() {
	var (
		asJSON, noWait, withData, allowEgress, persistent bool
		gitBranch, size, notifyURL, ttl                   string
		seedFile, project, version                        string
		force, schedule, dryRun                           bool
	)
	cmd := &cobra.Command{
		Use:   "branches",
		Short: "Branches for agents: a branch is a project with its own cluster",
		Long: `A branch is a project with a parent. It has its own Postgres, GoTrue, PostgREST, keys and
host. Create one per agent or per task, merge what worked, delete the rest; a branch that is
not persistent is deleted when its lifetime ends ([branching] default_ttl).

These commands call the branching library in this process and need the system cluster to be
running, like "sbctl projects". The Management API serves the same operations to the Supabase
CLI and MCP server.`,
	}
	run := func(use, short string, args cobra.PositionalArgs, f func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, a []string) error {
			svc, closeFn, err := openBranching(cmd.Context())
			if err != nil {
				return err
			}
			defer closeFn()
			return f(cmd.Context(), cmd, svc, a)
		}}
	}

	list := run("list <project-ref>", "List the branches of a project", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		bs, err := svc.List(ctx, a[0])
		if err != nil {
			return err
		}
		if asJSON {
			vs := make([]branchView, len(bs))
			for i := range bs {
				vs[i] = viewBranch(&bs[i])
			}
			return printJSON(cmd.OutOrStdout(), vs)
		}
		t := newTable(cmd.OutOrStdout())
		fmt.Fprintln(t, "NAME\tREF\tSTATUS\tPROJECT\tDATA\tPERSISTENT\tEXPIRES")
		for _, b := range bs {
			exp := "-"
			if b.ExpiresAt != nil {
				exp = time.Until(*b.ExpiresAt).Round(time.Minute).String()
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%v\t%s\n", b.Name, b.Ref, b.State, b.ProjectStatus, orDash(b.CloneMethod), b.Persistent, exp)
		}
		return t.Flush()
	})
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	create := run("create <project-ref> <name>", "Create a branch and wait until it is ready", cobra.ExactArgs(2), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		in := branching.CreateInput{Name: a[1], GitBranch: gitBranch, Persistent: persistent, WithData: withData, AllowEgress: allowEgress, DesiredInstanceSize: size, NotifyURL: notifyURL}
		if ttl != "" {
			if ttl == "off" || ttl == "never" {
				in.TTL = -1
			} else if d, err := time.ParseDuration(ttl); err != nil || d <= 0 {
				return fmt.Errorf("--ttl %q: want a duration like 6h, or off", ttl)
			} else {
				in.TTL = d
			}
		}
		if seedFile != "" {
			b, err := os.ReadFile(seedFile)
			if err != nil {
				return err
			}
			in.Seed = string(b)
		}
		b, err := svc.Create(ctx, a[0], in)
		if err != nil {
			return err
		}
		if !noWait {
			if b, err = waitDone(ctx, svc, b.Ref); err != nil {
				if b != nil {
					printBranch(cmd.ErrOrStderr(), b)
				}
				return err
			}
		}
		if asJSON {
			return printJSON(cmd.OutOrStdout(), viewBranch(b))
		}
		printBranch(cmd.OutOrStdout(), b)
		return nil
	})
	create.Flags().BoolVar(&withData, "with-data", false, "clone the parent's data (copy-on-write when the disk supports it, else from its latest base backup)")
	create.Flags().BoolVar(&allowEgress, "allow-egress", false, "with --with-data: keep the parent's outbound side effects (no egress block, pg_cron jobs stay active); by default the branch's Postgres reaches loopback only and the cron jobs are paused")
	create.Flags().BoolVar(&persistent, "persistent", false, "never expire; take a final backup when deleted")
	create.Flags().StringVar(&gitBranch, "git-branch", "", "git branch the branch tracks")
	create.Flags().StringVar(&size, "size", "", "instance size (default: [branching] default_class)")
	create.Flags().StringVar(&ttl, "ttl", "", "lifetime of a non-persistent branch, for example 6h; off never expires (default: [branching] default_ttl)")
	create.Flags().StringVar(&seedFile, "seed-file", "", "SQL file to run after the migrations instead of the parent's stored seed")
	create.Flags().StringVar(&notifyURL, "notify-url", "", "URL that receives a POST when an operation on the branch ends")
	create.Flags().BoolVar(&noWait, "no-wait", false, "return as soon as the branch exists")
	create.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	get := run("get <id-or-ref|name>", "Show a branch", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		b, err := selectBranch(ctx, svc, project, a[0])
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(cmd.OutOrStdout(), viewBranch(b))
		}
		printBranch(cmd.OutOrStdout(), b)
		return nil
	})
	get.Flags().StringVar(&project, "project", "", "look the argument up as a branch name of this project")
	get.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	del := run("delete <id-or-ref|name>", "Delete a branch", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		b, err := selectBranch(ctx, svc, project, a[0])
		if err != nil {
			return err
		}
		got, err := svc.Delete(ctx, b.Ref, branching.DeleteOptions{Schedule: schedule})
		if err != nil {
			return err
		}
		if schedule {
			fmt.Fprintf(cmd.OutOrStdout(), "branch %s will be deleted at %s (`sbctl branches restore` cancels)\n", got.Name, got.DeletionScheduledAt.Local().Format(time.RFC3339))
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "branch %s (%s) deleted\n", b.Name, b.Ref)
		return nil
	})
	del.Flags().StringVar(&project, "project", "", "look the argument up as a branch name of this project")
	del.Flags().BoolVar(&schedule, "schedule", false, "soft delete: remove after the grace period instead of now")

	restore := run("restore <id-or-ref|name>", "Cancel a scheduled deletion", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		b, err := selectBranch(ctx, svc, project, a[0])
		if err != nil {
			return err
		}
		if _, err := svc.Restore(ctx, b.Ref); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "branch %s restored\n", b.Name)
		return nil
	})
	restore.Flags().StringVar(&project, "project", "", "look the argument up as a branch name of this project")

	action := func(name, short string, f func(context.Context, *branching.Service, string, branching.ActionInput) (string, error)) *cobra.Command {
		c := run(name+" <id-or-ref|name>", short, cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
			b, err := selectBranch(ctx, svc, project, a[0])
			if err != nil {
				return err
			}
			runID, err := f(ctx, svc, b.Ref, branching.ActionInput{MigrationVersion: version, Force: force})
			if err != nil {
				return err
			}
			if noWait {
				fmt.Fprintf(cmd.OutOrStdout(), "started run %s\n", runID)
				return nil
			}
			done, err := waitDone(ctx, svc, b.Ref)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", b.Name, done.Detail)
			return nil
		})
		c.Flags().StringVar(&project, "project", "", "look the argument up as a branch name of this project")
		c.Flags().StringVar(&version, "migration-version", "", "limit to migrations up to this version")
		c.Flags().BoolVar(&noWait, "no-wait", false, "return after the operation starts")
		return c
	}
	merge := action("merge", "Apply the branch's new migrations to its parent", func(ctx context.Context, s *branching.Service, ref string, in branching.ActionInput) (string, error) {
		return s.Merge(ctx, ref, in)
	})
	merge.Flags().BoolVar(&force, "force", false, "merge despite divergence (versions that differ in content are skipped)")
	reset := action("reset", "Recreate the branch from its parent (everything written to the branch is lost)", func(ctx context.Context, s *branching.Service, ref string, in branching.ActionInput) (string, error) {
		return s.Reset(ctx, ref, in)
	})
	push := action("push", "Apply the parent's new migrations to the branch (rebase)", func(ctx context.Context, s *branching.Service, ref string, in branching.ActionInput) (string, error) {
		return s.Push(ctx, ref, in)
	})
	push.Flags().BoolVar(&force, "force", false, "push despite versions that differ in content (they are skipped)")

	diff := run("diff <id-or-ref|name>", "Show the migrations a merge would apply", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		b, err := selectBranch(ctx, svc, project, a[0])
		if err != nil {
			return err
		}
		out, err := svc.Diff(ctx, b.Ref)
		if err != nil {
			return err
		}
		_, err = io.WriteString(cmd.OutOrStdout(), out)
		return err
	})
	diff.Flags().StringVar(&project, "project", "", "look the argument up as a branch name of this project")

	sweep := run("sweep", "Delete branches whose lifetime or scheduled deletion has lapsed", cobra.NoArgs, func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, _ []string) error {
		res, err := svc.Sweep(ctx, dryRun)
		if err != nil {
			return err
		}
		verb := "deleted"
		if dryRun {
			verb = "would delete"
		}
		for _, r := range res.Expired {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", verb, r)
		}
		for _, r := range res.Stale {
			fmt.Fprintf(cmd.OutOrStdout(), "interrupted operation on %s marked failed\n", r)
		}
		for r, e := range res.Failed {
			fmt.Fprintf(cmd.ErrOrStderr(), "could not delete %s: %s\n", r, e)
		}
		if len(res.Expired) == 0 && len(res.Stale) == 0 && len(res.Failed) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "nothing to sweep")
		}
		if len(res.Failed) > 0 {
			return errors.New("some branches could not be deleted")
		}
		return nil
	})
	sweep.Flags().BoolVar(&dryRun, "dry-run", false, "only report")

	seed := &cobra.Command{Use: "seed", Short: "The SQL that new schema-only branches of a project run after its migrations"}
	var seedPath string
	seedSet := run("set <project-ref>", "Store a seed script (from --file, or standard input)", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		var b []byte
		var err error
		if seedPath == "" || seedPath == "-" {
			b, err = io.ReadAll(cmd.InOrStdin())
		} else {
			b, err = os.ReadFile(seedPath)
		}
		if err != nil {
			return err
		}
		if err := svc.SetSeed(ctx, a[0], string(b)); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "seed of %s stored (%d bytes)\n", a[0], len(b))
		return nil
	})
	seedSet.Flags().StringVar(&seedPath, "file", "", "seed.sql to store (default: standard input)")
	seedGet := run("get <project-ref>", "Print the stored seed", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		s, err := svc.GetSeed(ctx, a[0])
		if err != nil {
			return err
		}
		_, err = io.WriteString(cmd.OutOrStdout(), s)
		return err
	})
	seedClear := run("clear <project-ref>", "Remove the stored seed", cobra.ExactArgs(1), func(ctx context.Context, cmd *cobra.Command, svc *branching.Service, a []string) error {
		return svc.SetSeed(ctx, a[0], "")
	})
	seed.AddCommand(seedSet, seedGet, seedClear)

	cmd.AddCommand(list, create, get, del, restore, merge, reset, push, diff, sweep, seed)
	rootCmd.AddCommand(cmd)
}
