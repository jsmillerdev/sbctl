package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/notice"
)

// The host layer's part of the daemon (internal/hostsetup): while the converged marker is behind
// this binary's converge revision the daemon raises host_not_converged, and it tells the hooks after
// it, which enable the cluster features, so that they stay off until `supavise system converge` has
// run and the daemon has restarted: the state they Get is the one the daemon started with (a hook
// that wants the marker as it is now reads hostsetup.StatusOf). The hook adds itself to the front of
// the list, so that the others can Get the state.
func init() {
	wireHooks = append([]struct {
		name string
		fn   func(ctx context.Context, w *Wire) error
	}{{"host", wireHost}}, wireHooks...)
}

// runsAsService says whether this process runs in supavise.service, which is what a server
// install is. The control group says it; an environment variable such as INVOCATION_ID would be
// inherited by every process a service starts, a test run by a CI agent included. A daemon started
// by hand or by a test has no host to converge.
var runsAsService = func() bool {
	b, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(b), "/supavise.service")
}

// wireHost provides the converge state (Get[hostsetup.Status]) and starts the monitor.
func wireHost(_ context.Context, w *Wire) error {
	if w.Cfg.Supervisor != config.SupervisorSystemd || !runsAsService() {
		return nil
	}
	started := hostsetup.StatusOf(w.Cfg.StateDir)
	Provide[hostsetup.Status](w, started)
	m := &hostsetup.Monitor{
		StateDir: w.Cfg.StateDir,
		Quiet: func() bool {
			_, running := notice.UpgradeRunning(w.Cfg.Paths(), time.Now())
			return running
		},
		Raise: func(ctx context.Context, s hostsetup.Status) {
			if err := alerts.Notify(ctx, hostAlert(s, started.Behind())); err != nil {
				w.Log.Warn("could not send the host_not_converged alert", "error", err)
			}
		},
	}
	w.Go("host-monitor", func(ctx context.Context) error { m.Run(ctx); return nil })
	return nil
}

// hostAlert is host_not_converged while the node is behind, and its resolution once it is not. A
// daemon that started behind keeps the cluster features off until it restarts, and the text says so.
func hostAlert(s hostsetup.Status, startedBehind bool) alerts.Event {
	ev := alerts.Event{Kind: alerts.KindHostNotConverged, Severity: alerts.SeverityWarning, Title: "This server's host setup is behind this release"}
	const restart = "sudo systemctl restart supavise.service"
	if !s.Behind() {
		ev.Resolved, ev.Detail = true, "This is over: the host is converged."
		if startedBehind {
			ev.Detail += " The cluster features start with the daemon: run " + restart + " to turn them on."
		}
		return ev
	}
	ev.Detail = fmt.Sprintf("The host setup is at revision %d and this release needs %d, so the cluster features stay off. Run: sudo supavise system converge", s.Have, s.Want)
	if startedBehind {
		ev.Detail += ", then " + restart
	}
	return ev
}
