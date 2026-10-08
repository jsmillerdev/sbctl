package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/notice"
)

// The host layer's part of the daemon (internal/hostsetup): while the converged marker is behind
// this binary's converge revision the daemon raises host_not_converged, and it tells the hooks after
// it, which enable the cluster features, so that they stay off until `supavise system converge` has
// run. The hook adds itself to the front of the list, so that the others can Get the state.
func init() {
	wireHooks = append([]struct {
		name string
		fn   func(ctx context.Context, w *Wire) error
	}{{"host", wireHost}}, wireHooks...)
}

// runsAsService says whether this process is a systemd service, which is what a server install
// is; systemd gives every service an invocation id. A daemon started by hand or by a test has no
// host to converge.
var runsAsService = func() bool { return os.Getenv("INVOCATION_ID") != "" }

// wireHost provides the converge state (Get[hostsetup.Status]) and starts the monitor.
func wireHost(_ context.Context, w *Wire) error {
	if w.Cfg.Supervisor != config.SupervisorSystemd || !runsAsService() {
		return nil
	}
	Provide[hostsetup.Status](w, hostsetup.StatusOf(w.Cfg.StateDir))
	m := &hostsetup.Monitor{
		StateDir: w.Cfg.StateDir,
		Quiet: func() bool {
			_, running := notice.UpgradeRunning(w.Cfg.Paths(), time.Now())
			return running
		},
		Raise: func(ctx context.Context, s hostsetup.Status) {
			if err := alerts.Notify(ctx, hostAlert(s)); err != nil {
				w.Log.Warn("could not send the host_not_converged alert", "error", err)
			}
		},
	}
	w.Go("host-monitor", func(ctx context.Context) error { m.Run(ctx); return nil })
	return nil
}

// hostAlert is host_not_converged while the node is behind, and its resolution once it is not.
func hostAlert(s hostsetup.Status) alerts.Event {
	ev := alerts.Event{Kind: alerts.KindHostNotConverged, Severity: alerts.SeverityWarning, Title: "This server's host setup is behind this release"}
	if !s.Behind() {
		ev.Resolved, ev.Detail = true, "This is over: the host is converged."
		return ev
	}
	ev.Detail = fmt.Sprintf("The host setup is at revision %d and this release needs %d, so the cluster features stay off. Run: sudo supavise system converge", s.Have, s.Want)
	return ev
}
