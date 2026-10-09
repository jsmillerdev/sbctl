package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/supavise/supavise/internal/backup"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/update"
)

// runConverge is `supavise system converge` and its alias `install-units`.
func runConverge(cmd *cobra.Command, check, asJSON bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// A unit directory other than systemd's is a test: only the unit files go there.
	sandbox := sysUnitDir != defaultUnitDir
	if !check && !sandbox && os.Geteuid() != 0 {
		return errors.New("run as root: sudo supavise system converge (--check needs no root)")
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	progress := out
	if asJSON {
		progress = errOut // stdout carries the JSON only
	}
	c, err := newConverger(cfg, sysUnitDir, sysPolkitDir, sandbox, progress, errOut)
	if err != nil {
		return err
	}
	if check {
		rs := c.Check(cmd.Context())
		if asJSON {
			return printJSON(out, rs)
		}
		renderCheck(out, rs)
		return nil
	}
	rs, err := c.Run(cmd.Context())
	if asJSON {
		if perr := printJSON(out, rs); perr != nil {
			return perr
		}
	}
	if err != nil {
		return err
	}
	if !sandbox {
		// The dashboard build this binary pins, and config.toml naming it (studio_build.go). It is
		// not a host step: it never fails converge and is not part of the revision.
		newStudioSetup(cfg, selfUpdateConfigPath(), progress, errOut).run(cmd.Context())
	}
	if !asJSON && !sandbox && len(changedLines(rs)) == 0 {
		fmt.Fprintf(out, "host is converged (revision %d)\n", hostsetup.Revision)
	}
	return nil
}

func changedLines(rs []hostsetup.Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Changed...)
	}
	return out
}

// renderCheck prints the result of --check for a person.
func renderCheck(w io.Writer, rs []hostsetup.Result) {
	t := newTable(w)
	n, unknown := 0, 0
	for _, r := range rs {
		state := "ok"
		switch {
		case r.Pending:
			state, n = "pending", n+1
		case r.Unknown:
			state, unknown = "unknown", unknown+1
		}
		fmt.Fprintf(t, "%s\t%s\t%s\n", state, r.Title, r.Detail)
	}
	_ = t.Flush()
	switch {
	case n > 0:
		fmt.Fprintf(w, "%d step(s) pending: sudo supavise system converge\n", n)
		if unknown > 0 {
			fmt.Fprintf(w, "%d step(s) could not be checked\n", unknown)
		}
	case unknown > 0:
		fmt.Fprintf(w, "nothing is pending, but %d step(s) could not be checked (revision %d)\n", unknown, hostsetup.Revision)
	default:
		fmt.Fprintf(w, "host is converged (revision %d)\n", hostsetup.Revision)
	}
}

// newConverger builds the step list of this node. sandbox keeps it to the unit files: the other
// steps look at the real host.
func newConverger(cfg *config.Config, unitDir, polkitDir string, sandbox bool, out, errOut io.Writer) (*hostsetup.Converger, error) {
	// The embedded timers carry the default schedules; the node's own come from config and are
	// written in the same pass, so a second run changes nothing.
	backupTimer, err := backup.RenderBackupTimerChecked(cfg.Backup.BaseBackupOnCalendar)
	if err != nil {
		return nil, fmt.Errorf("config backup.base_backup_on_calendar: %w", err)
	}
	upgradeTimer, err := update.RenderTimer(cfg.Update)
	if err != nil {
		return nil, fmt.Errorf("config update: %w", err)
	}
	o := hostsetup.Options{
		StateDir: cfg.StateDir, ConfigDir: filepath.Dir(selfUpdateConfigPath()), UnitDir: unitDir, PolkitDir: polkitDir,
		UnitOverrides: map[string][]byte{backup.BackupTimerUnit: []byte(backupTimer), update.TimerUnit: []byte(upgradeTimer)},
		Owner: func() (int, int, bool) {
			uid, gid := supaviseOwner()
			return uid, gid, uid >= 0
		},
		PeerPort: peerPort(cfg),
	}
	// A server in a cluster refreshes the cluster-scoped settings from its leader.
	if cs := newClusterConfigSync(cfg, selfUpdateConfigPath()); cs != nil {
		o.ConfigSync = cs
	}
	if sandbox {
		o.Owner, o.PeerPort, o.ConfigSync = nil, 0, nil
		o.MountInfo = func() ([]byte, error) { return nil, errors.New("not read in a test directory") }
	}
	// A test directory takes the unit files and nothing else, so it must not record that the host is
	// converged: the node's marker is the daemon's word on the real host.
	c := &hostsetup.Converger{Steps: hostsetup.DefaultSteps(o), StateDir: cfg.StateDir, Version: version, Out: out, NoMarker: sandbox}
	if !sandbox {
		c.Activate = func(ctx context.Context, done hostsetup.Outcome) error {
			return activateUnits(ctx, cfg, done, out, errOut)
		}
	}
	return c, nil
}

// peerPort is the TCP port of the mesh, 0 when [node] peer_listen has none.
func peerPort(cfg *config.Config) int {
	_, p, err := net.SplitHostPort(cfg.PeerListen())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// activateUnits makes systemd re-read the unit files when a step wrote any, enables the system
// project's units for boot and starts or stops the upgrade timer according to [update]. Without a
// systemd supervisor (the exec backend) it says what to do by hand and succeeds, as install-units
// always did.
func activateUnits(ctx context.Context, cfg *config.Config, done hostsetup.Outcome, out, errOut io.Writer) error {
	apply, err := lifecycle.OpenOptions{Log: newLogger(cfg)}.UnitInstaller(cfg)
	if err != nil {
		fmt.Fprintln(errOut, "run `systemctl daemon-reload` and enable the system units yourself:", err)
		return nil
	}
	if err := apply(ctx, done.Reload); err != nil {
		return err
	}
	timerChanged := false
	for _, f := range done.Files {
		timerChanged = timerChanged || strings.HasSuffix(f, "/"+update.TimerUnit)
	}
	return applyUpgradeTimer(ctx, cfg, timerChanged, out, errOut)
}
