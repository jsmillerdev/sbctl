package app

import (
	"context"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/hostsetup"
)

// The host hook is the first of the hooks, so the others can read the converge state.
func TestHostHookComesFirst(t *testing.T) {
	if wireHooks[0].name != "host" {
		t.Fatalf("the first hook is %q", wireHooks[0].name)
	}
}

func TestWireHostMonitorsAServiceOnly(t *testing.T) {
	old := runsAsService
	defer func() { runsAsService = old }()

	// A daemon that is not a systemd service has no host to converge.
	runsAsService = func() bool { return false }
	w := testWire(t)
	if err := wireHost(context.Background(), w); err != nil || len(w.runners) != 0 {
		t.Fatalf("not a service: %v, %d runners", err, len(w.runners))
	}

	runsAsService = func() bool { return true }
	w = testWire(t)
	w.Cfg.StateDir = t.TempDir()
	if err := hostsetup.WriteMarker(w.Cfg.StateDir, hostsetup.Marker{Revision: 0}); err != nil {
		t.Fatal(err)
	}
	if err := wireHost(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(w.runners) != 1 || w.runners[0].name != "host-monitor" {
		t.Fatalf("runners = %+v", w.runners)
	}
	st, ok := Get[hostsetup.Status](w)
	if !ok || !st.Behind() || st.Have != 0 || st.Want != hostsetup.Revision {
		t.Fatalf("provided state = %+v, %v", st, ok)
	}

	// An exec node (development, tests) is left alone whatever the environment says.
	w = testWire(t)
	w.Cfg.Supervisor = "exec"
	if err := wireHost(context.Background(), w); err != nil || len(w.runners) != 0 {
		t.Fatalf("exec supervisor: %v, %d runners", err, len(w.runners))
	}
}

func TestHostAlert(t *testing.T) {
	ev := hostAlert(hostsetup.Status{Have: 0, Want: 1, Known: true}, false)
	if ev.Kind != alerts.KindHostNotConverged || ev.Resolved || ev.Severity != alerts.SeverityWarning || !strings.Contains(ev.Detail, "sudo supavise system converge") || !strings.Contains(ev.Detail, "revision 0") || strings.Contains(ev.Detail, "restart") {
		t.Errorf("behind: %+v", ev)
	}
	ev2 := hostAlert(hostsetup.Status{Have: 1, Want: 1, Known: true}, false)
	if !ev2.Resolved || ev2.Kind != alerts.KindHostNotConverged || ev2.Title != ev.Title || strings.Contains(ev2.Detail, "restart") {
		t.Errorf("caught up: %+v", ev2)
	}
}

// The cluster features read the state the daemon started with, so a daemon that started behind has to
// be restarted after the converge: the alert and its resolution both say so.
func TestHostAlertSaysARestartIsNeededWhenTheDaemonStartedBehind(t *testing.T) {
	behind := hostAlert(hostsetup.Status{Have: 0, Want: 1, Known: true}, true)
	if !strings.Contains(behind.Detail, "sudo supavise system converge, then sudo systemctl restart supavise.service") {
		t.Errorf("behind: %q", behind.Detail)
	}
	done := hostAlert(hostsetup.Status{Have: 1, Want: 1, Known: true}, true)
	if !done.Resolved || !strings.Contains(done.Detail, "sudo systemctl restart supavise.service") {
		t.Errorf("caught up: %+v", done)
	}
}
