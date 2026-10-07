package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/OWNER/sbctl/internal/api"
	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/fleet"
	"github.com/OWNER/sbctl/internal/functions"
	"github.com/OWNER/sbctl/internal/lifecycle"
	"github.com/OWNER/sbctl/internal/proxy"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
	"github.com/OWNER/sbctl/internal/units"
)

var functionsCmd = &cobra.Command{
	Use:   "functions",
	Short: "Edge Functions: list, invoke, logs",
	Long: `Edge Functions run in one Edge Runtime process (unit sb-edge-runtime, started by
"sbctl fleet start" when [functions] enabled is true) with sbctl's own main service, which
routes every request to the project named by the proxy. Functions are deployed with the
Supabase CLI against this node's Management API ("supabase functions deploy"); this command
inspects what is deployed and calls it.`,
}

var (
	fnJSON      bool
	fnMethod    string
	fnData      string
	fnHeaders   []string
	fnNoAuth    bool
	fnService   bool
	fnJWT       string
	fnLogLines  int
	fnLogFollow bool
	fnLogAll    bool
	fnTokenFile string
	fnNoRuntime bool
)

func fnNode(cmd *cobra.Command, run func(n *lifecycle.Node, cfg *config.Config, store api.Store) error) error {
	n, err := openNode(cmd.Context())
	if err != nil {
		return err
	}
	defer n.Close()
	return run(n, n.Cfg, functions.NewStore(n.Registry))
}

func init() {
	list := &cobra.Command{
		Use:   "list <ref>",
		Short: "List the functions deployed to a project and whether the runtime serves them",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			return fnNode(cmd, func(n *lifecycle.Node, cfg *config.Config, store api.Store) error {
				if _, err := n.Registry.GetProject(cmd.Context(), a[0]); err != nil {
					return err
				}
				fns, err := store.ListFunctions(cmd.Context(), a[0])
				if err != nil {
					return err
				}
				type row struct {
					Slug      string `json:"slug"`
					Version   int    `json:"version"`
					VerifyJWT bool   `json:"verify_jwt"`
					Entry     string `json:"entrypoint_path"`
					Updated   string `json:"updated_at"`
					Live      bool   `json:"live"`
					LiveVer   int    `json:"live_version,omitempty"`
				}
				rows := make([]row, 0, len(fns))
				for _, f := range fns {
					v, ok := functions.Live(cfg, a[0], f.Slug)
					rows = append(rows, row{f.Slug, f.Version, f.VerifyJWT, f.EntrypointPath, f.UpdatedAt.UTC().Format(time.RFC3339), ok && v == f.Version, v})
				}
				if fnJSON {
					return printJSON(cmd.OutOrStdout(), rows)
				}
				t := newTable(cmd.OutOrStdout())
				fmt.Fprintln(t, "SLUG\tVERSION\tVERIFY JWT\tSTATE\tUPDATED")
				for _, r := range rows {
					state := "live"
					switch {
					case r.Live:
					case r.LiveVer > 0:
						state = fmt.Sprintf("stale (disk has v%d)", r.LiveVer)
					default:
						state = "not on disk"
					}
					fmt.Fprintf(t, "%s\t%d\t%v\t%s\t%s\n", r.Slug, r.Version, r.VerifyJWT, state, r.Updated)
				}
				return t.Flush()
			})
		},
	}
	list.Flags().BoolVar(&fnJSON, "json", false, "print JSON")

	invoke := &cobra.Command{
		Use:   "invoke <ref> <slug> [subpath]",
		Short: "Call a function through this node's own proxy, as a client would",
		Long: `Sends one request to https://<ref>.api.<domain>/functions/v1/<slug>[/subpath] by way
of the proxy's local listener (so TLS names and the Host header are the real ones), with the
project's anon key as apikey and bearer token. --service-role uses the service_role key
instead, --jwt a token of your own, --no-auth none at all. The response body goes to stdout
and the status line to stderr; the exit status is 1 for a status of 400 or more.`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, a []string) error {
			return fnNode(cmd, func(n *lifecycle.Node, cfg *config.Config, _ api.Store) error {
				ref, slug, sub := a[0], a[1], ""
				if len(a) == 3 {
					sub = "/" + strings.TrimPrefix(a[2], "/")
				}
				if _, err := n.Registry.GetProject(cmd.Context(), ref); err != nil {
					return err
				}
				return invokeFunction(cmd, n, cfg, ref, slug, sub)
			})
		},
	}
	invoke.Flags().StringVarP(&fnMethod, "method", "X", "", "HTTP method (default POST with --data, else GET)")
	invoke.Flags().StringVarP(&fnData, "data", "d", "", "request body; @file reads a file, @- stdin")
	invoke.Flags().StringArrayVarP(&fnHeaders, "header", "H", nil, "extra header, 'Name: value' (repeatable)")
	invoke.Flags().BoolVar(&fnNoAuth, "no-auth", false, "send no apikey and no Authorization header")
	invoke.Flags().BoolVar(&fnService, "service-role", false, "authenticate with the service_role key instead of the anon key")
	invoke.Flags().StringVar(&fnJWT, "jwt", "", "bearer token to send (the apikey stays the anon key)")

	logs := &cobra.Command{
		Use:   "logs [ref]",
		Short: "Show the log of the Edge Runtime (shared by all projects)",
		Long: `Prints the end of the log of sb-edge-runtime, which serves every project. With a ref
only the lines that mention it are shown: the main service tags its own messages
("<ref>/<slug>: ...") but a function's console output carries no project, so it is not
selected by the filter; use --all to see everything. Under systemd this is journalctl; with
the exec backend it is the unit's log file.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			n, err := openNode(cmd.Context())
			if err != nil {
				return err
			}
			defer n.Close()
			ref := ""
			if len(a) == 1 {
				ref = a[0]
				if fnLogAll {
					ref = ""
				}
			}
			return showRuntimeLog(cmd, n, ref)
		},
	}
	logs.Flags().IntVarP(&fnLogLines, "lines", "n", 100, "lines to show")
	logs.Flags().BoolVarP(&fnLogFollow, "follow", "f", false, "keep printing new lines (systemd only)")
	logs.Flags().BoolVar(&fnLogAll, "all", false, "do not filter by project")

	dev := &cobra.Command{
		Use:   "dev",
		Short: "Run the Management API, the proxy and the Edge Runtime in one process (development and CI)",
		Long: `Serves what "sbctl serve" will serve once it wires Edge Functions: the Management API at
api.<domain>, the proxy with /functions/v1 routed to the runtime, and the runtime itself,
with [functions] enabled forced on. It reconciles deployments to disk every
[functions] reconcile_seconds. Listen addresses and TLS come from the config, for example
SBCTL_TLS_MODE=off SBCTL_LISTEN_HTTP=127.0.0.1:40080 SBCTL_LISTEN_HTTPS=127.0.0.1:40081.
--token-file writes a fresh personal access token (0600) for the Supabase CLI. Stops the
runtime on exit.`,
		Args: cobra.NoArgs,
		RunE: runFunctionsDev,
	}
	dev.Flags().StringVar(&fnTokenFile, "token-file", "", "write a new personal access token for the Supabase CLI to this file (0600)")
	dev.Flags().BoolVar(&fnNoRuntime, "no-runtime", false, "do not start sb-edge-runtime (it runs already)")

	functionsCmd.AddCommand(list, invoke, logs, dev)
	rootCmd.AddCommand(functionsCmd)
}

// localAddr turns a listen address (":80", "0.0.0.0:443") into one that can be dialed.
func localAddr(listen, def string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return net.JoinHostPort("127.0.0.1", def)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func invokeFunction(cmd *cobra.Command, n *lifecycle.Node, cfg *config.Config, ref, slug, sub string) error {
	scheme, listen, def := "https", cfg.Listen.HTTPS, "443"
	if cfg.TLS.Mode == "off" {
		scheme, listen, def = "http", cfg.Listen.HTTP, "80"
	}
	dial := localAddr(listen, def)
	if cfg.BaseDomain() == "" {
		return errors.New("no domain configured: set domain or public_ip in config.toml")
	}
	host := cfg.ProjectHost(ref)
	_, port, _ := net.SplitHostPort(listen)
	authority := host
	if port != "" && port != def {
		authority = net.JoinHostPort(host, port)
	}
	u := fmt.Sprintf("%s://%s/functions/v1/%s%s", scheme, authority, slug, sub)

	var body io.Reader
	switch {
	case fnData == "@-":
		body = cmd.InOrStdin()
	case strings.HasPrefix(fnData, "@"):
		f, err := os.Open(fnData[1:])
		if err != nil {
			return err
		}
		defer f.Close()
		body = f
	case fnData != "":
		body = strings.NewReader(fnData)
	}
	method := fnMethod
	if method == "" {
		method = http.MethodGet
		if body != nil {
			method = http.MethodPost
		}
	}
	req, err := http.NewRequestWithContext(cmd.Context(), method, u, body)
	if err != nil {
		return err
	}
	if !fnNoAuth {
		k, err := n.Engine.Keys(cmd.Context(), ref)
		if err != nil {
			return err
		}
		key := k.AnonKey
		if fnService {
			key = k.ServiceRoleKey
		}
		req.Header.Set("apikey", key)
		token := key
		if fnJWT != "" {
			token = fnJWT
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, h := range fnHeaders {
		name, val, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("header %q: want 'Name: value'", h)
		}
		req.Header.Add(strings.TrimSpace(name), strings.TrimSpace(val))
	}
	if body != nil && req.Header.Get("Content-Type") == "" && strings.HasPrefix(strings.TrimSpace(fnData), "{") {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout: time.Duration(cfg.Functions.WallClock()+15) * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, dial)
			},
			TLSClientConfig:   &tls.Config{ServerName: host},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", resp.Status)
	if _, err := io.Copy(cmd.OutOrStdout(), resp.Body); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		cmd.SilenceUsage = true
		return fmt.Errorf("the function answered %s", resp.Status)
	}
	return nil
}

func showRuntimeLog(cmd *cobra.Command, n *lifecycle.Node, ref string) error {
	unit := units.Spec{Service: config.SvcEdgeRuntime}.Unit()
	out := cmd.OutOrStdout()
	keep := func(line string) bool { return ref == "" || strings.Contains(line, ref) }
	if t, ok := n.Supervisor.(units.LogTailer); ok {
		if fnLogFollow {
			fmt.Fprintln(cmd.ErrOrStderr(), "--follow needs the systemd backend; showing the current end of the log")
		}
		for _, line := range strings.Split(strings.TrimRight(t.Tail(unit, fnLogLines), "\n"), "\n") {
			if line != "" && keep(line) {
				fmt.Fprintln(out, line)
			}
		}
		return nil
	}
	args := []string{"-u", unit, "-n", strconv.Itoa(fnLogLines), "--no-pager", "-o", "short-iso"}
	if fnLogFollow {
		args = append(args, "-f")
	}
	c := exec.CommandContext(cmd.Context(), "journalctl", args...)
	pipe, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = cmd.ErrOrStderr()
	if err := c.Start(); err != nil {
		return fmt.Errorf("journalctl: %w", err)
	}
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if keep(sc.Text()) {
			fmt.Fprintln(out, sc.Text())
		}
	}
	return c.Wait()
}

// runFunctionsDev serves the API, the proxy and the runtime in this process.
func runFunctionsDev(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Functions.Enabled = true
	log := newLogger(cfg)
	lz := fleet.NewLazy(fleet.Deps{Cfg: cfg, Log: log})
	oo := openOptions(cfg)
	oo.Fleet = lz.Fleet()
	n, err := lifecycle.Open(cmd.Context(), cfg, oo)
	if err != nil {
		return err
	}
	defer n.Close()
	lz.Bind(n.Registry, n.Secrets)

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !fnNoRuntime {
		if f, ok := n.Artifacts.(interface {
			Fetch(context.Context, string) (string, error)
		}); ok {
			if _, err := f.Fetch(ctx, config.SvcEdgeRuntime); err != nil {
				return fmt.Errorf("fetch edge-runtime: %w", err)
			}
		}
		skip := append([]string(nil), fleet.Services...) // everything but the runtime
		m, err := fleet.NewManager(fleet.Deps{Cfg: cfg, Log: log, Registry: n.Registry, Secrets: n.Secrets,
			Supervisor: n.Supervisor, Artifacts: n.Artifacts, Skip: skip})
		if err != nil {
			return err
		}
		if err := m.Start(ctx); err != nil {
			return err
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := m.Stop(sctx); err != nil {
				log.Warn("stopping the edge runtime", "error", err)
			}
		}()
	}

	store := functions.NewStore(n.Registry)
	syncer, err := functions.New(functions.Deps{Cfg: cfg, Registry: n.Registry, Secrets: n.Secrets, Store: store, Keys: n.Engine.Keys, Log: log})
	if err != nil {
		return err
	}
	go syncer.Run(ctx)
	apiSrv, err := api.NewServer(api.Deps{Registry: n.Registry, Secrets: n.Secrets, Manager: n.Engine, Config: cfg, Logger: log, Store: store, Functions: syncer})
	if err != nil {
		return err
	}
	if fnTokenFile != "" {
		tok := secrets.NewPAT()
		if err := n.Registry.CreateAccessToken(ctx, &registry.AccessToken{UserID: "00000000-0000-4000-8000-00000000f0f0", Name: "functions dev", Hash: secrets.HashToken(tok), Prefix: tok[:8]}); err != nil {
			return err
		}
		if err := os.WriteFile(fnTokenFile, []byte(tok+"\n"), 0o600); err != nil {
			return err
		}
	}
	prx, err := proxy.New(proxy.Options{Config: cfg, Registry: n.Registry, Keys: n.Engine, APIHandler: apiSrv, FunctionsEnabled: true, Logger: log})
	if err != nil {
		return err
	}
	log.Info("functions dev: serving", "http", cfg.Listen.HTTP, "https", cfg.Listen.HTTPS, "api", cfg.APIURL())
	if err := prx.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
