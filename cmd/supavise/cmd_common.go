package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/supavise/supavise/internal/app"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
)

// newLogger returns a text logger on stderr at the config's log level.
func newLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// effectiveConfigPath is the config file the process loaded, exported to Postgres units
// so that archive_command loads the same one. Empty when no file exists.
func effectiveConfigPath() string {
	p := configPath
	if p == "" {
		p = os.Getenv(config.EnvConfigPath)
	}
	if p == "" {
		p = config.DefaultPath
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// appOptions is what every command that drives projects tells the composition: the
// logger, the config file the children must read, and the fleet.
func appOptions(cfg *config.Config) app.Options {
	log := newLogger(cfg)
	fl, bind := newFleet(cfg, log)
	return app.Options{Log: log, ConfigPath: effectiveConfigPath(), Version: version, Fleet: fl, BindFleet: bind}
}

// openOptions wires the backup service into the lifecycle (archive_command, the final
// backup on delete, restore); see app.LifecycleOptions.
func openOptions(cfg *config.Config) lifecycle.OpenOptions {
	o := appOptions(cfg)
	o.Fleet, o.BindFleet = nil, nil // these commands never create, re-key or delete projects
	return app.LifecycleOptions(cfg, o)
}

// openNode loads the config and connects to an initialized node. Its Engine registers,
// re-keys and removes projects with the shared services (fleet.Lazy), so `supavise projects
// create|rotate-keys|delete` keep Supavisor, Realtime and Storage in step.
func openNode(ctx context.Context) (*lifecycle.Node, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	o := appOptions(cfg)
	n, err := openLifecycle(ctx, cfg, app.LifecycleOptions(cfg, o)) // SUPAVISE_REGISTRY_DSN overrides, as for the backups commands
	if err != nil {
		return nil, err
	}
	o.BindFleet(n.Registry, n.Secrets)
	return n, nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0) }

func healthTable(w io.Writer, hs []lifecycle.ServiceHealth) {
	t := newTable(w)
	fmt.Fprintln(t, "SERVICE\tSTATUS\tDETAIL")
	for _, h := range hs {
		fmt.Fprintf(t, "%s\t%s\t%s\n", h.Name, h.Status, h.Error)
	}
	t.Flush()
}

func healthy(hs []lifecycle.ServiceHealth) bool {
	for _, h := range hs {
		if !h.Healthy {
			return false
		}
	}
	return len(hs) > 0
}
