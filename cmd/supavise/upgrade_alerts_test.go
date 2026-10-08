package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/alerts"
	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/nodeupgrade"
	"github.com/jsmillerdev/supavise/internal/notice"
)

type hookBody struct {
	Kind, Severity, Title, Detail, Text string
}

// webhook is an alert endpoint that records what it is sent.
type webhook struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []hookBody
	fail bool
}

func newWebhook(t *testing.T) *webhook {
	t.Helper()
	w := &webhook{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var h hookBody
		_ = json.Unmarshal(b, &h)
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.fail {
			rw.WriteHeader(400)
			return
		}
		w.got = append(w.got, h)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *webhook) kinds() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var k []string
	for _, h := range w.got {
		k = append(k, h.Kind+"/"+h.Severity)
	}
	return strings.Join(k, ",")
}

func alertCfg(t *testing.T, urls ...string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Domain = "node.example.test"
	cfg.StateDir = t.TempDir()
	for _, u := range urls {
		cfg.Alerts.Webhooks = append(cfg.Alerts.Webhooks, config.AlertWebhook{URL: u})
	}
	return cfg
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestUpgradeEventsBecomeAlerts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ev       nodeupgrade.Event
		kind     string
		severity string
		title    string
		detail   []string
	}{
		{"started", nodeupgrade.Event{Kind: nodeupgrade.EventStarted, From: "v1.3.0", To: "v1.4.0", Projects: 12},
			alerts.KindUpgradeStarted, alerts.SeverityInfo, "Upgrade to v1.4.0 started", []string{"v1.3.0 -> v1.4.0", "12 project(s) move"}},
		{"started by the window", nodeupgrade.Event{Kind: nodeupgrade.EventStarted, From: "v1.3.0", To: "v1.4.0", Unattended: true},
			alerts.KindUpgradeStarted, alerts.SeverityInfo, "Upgrade to v1.4.0 started", []string{"by the maintenance window", "No project release moves"}},
		{"succeeded", nodeupgrade.Event{Kind: nodeupgrade.EventSucceeded, From: "v1.3.0", To: "v1.4.0", Projects: 3},
			alerts.KindUpgradeSucceeded, alerts.SeverityInfo, "Node is on v1.4.0", []string{"v1.3.0 -> v1.4.0", "3 project(s) moved"}},
		{"rolled back", nodeupgrade.Event{Kind: nodeupgrade.EventRolledBack, From: "v1.3.0", To: "v1.4.0", BackTo: "v1.3.0", Halted: "abcdefghijklmnopqrst", Cause: "the rollout of the projects stopped: boom", Unattended: true},
			alerts.KindUpgradeFailed, alerts.SeverityWarning, "Upgrade to v1.4.0 failed and was rolled back",
			[]string{"v1.3.0 -> v1.4.0", "runs v1.3.0 again", "halted at project abcdefghijklmnopqrst", "boom", "supavise update resume"}},
		{"needs the operator", nodeupgrade.Event{Kind: nodeupgrade.EventNeedsOperator, From: "v1.3.0", To: "v1.4.0", Halted: "abcdefghijklmnopqrst", Cause: "the registry is newer", Unattended: true},
			alerts.KindUpgradeFailed, alerts.SeverityCritical, "Upgrade to v1.4.0 failed: the node needs you",
			[]string{"not back on v1.3.0", "halted at project abcdefghijklmnopqrst", "the registry is newer", "paused until `sudo supavise update resume`"}},
		{"refused after the start", nodeupgrade.Event{Kind: nodeupgrade.EventRefused, From: "v1.3.0", To: "v1.4.0", Cause: "cannot keep the running binary"},
			alerts.KindUpgradeFailed, alerts.SeverityWarning, "Upgrade to v1.4.0 did not go ahead", []string{"still runs v1.3.0", "cannot keep the running binary"}},
		{"rollback started", nodeupgrade.Event{Kind: nodeupgrade.EventStarted, Rollback: true, From: "v1.4.0", To: "v1.3.0", Projects: 2},
			alerts.KindUpgradeStarted, alerts.SeverityInfo, "Rollback to v1.3.0 started", []string{"v1.4.0 -> v1.3.0", "2 project(s) go back"}},
		{"rollback done", nodeupgrade.Event{Kind: nodeupgrade.EventSucceeded, Rollback: true, From: "v1.4.0", To: "v1.3.0", Projects: 2},
			alerts.KindUpgradeSucceeded, alerts.SeverityInfo, "Node rolled back to v1.3.0", []string{"v1.4.0 -> v1.3.0", "2 project(s) went back"}},
		{"rollback failed", nodeupgrade.Event{Kind: nodeupgrade.EventNeedsOperator, Rollback: true, From: "v1.4.0", To: "v1.3.0", Cause: "the units did not render"},
			alerts.KindUpgradeFailed, alerts.SeverityCritical, "Rollback to v1.3.0 failed: the node needs you", []string{"the units did not render", "supavise status"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := upgradeAlert(tc.ev)
			if a.Kind != tc.kind || a.Severity != tc.severity || a.Title != tc.title || a.Ref != "" {
				t.Fatalf("alert = %+v", a)
			}
			for _, want := range tc.detail {
				if !strings.Contains(a.Detail, want) {
					t.Errorf("detail lacks %q:\n%s", want, a.Detail)
				}
			}
		})
	}
}

// The CLI builds its own Notifier over the node's config and state directory: the webhook gets the
// message, and every event of a run is sent (they are announcements, not conditions).
func TestUpgradeNotifierSendsEveryEventOfARun(t *testing.T) {
	w := newWebhook(t)
	cfg := alertCfg(t, w.srv.URL)
	notify := upgradeNotifier(cfg, quietLog())
	ctx := context.Background()
	notify(ctx, nodeupgrade.Event{Kind: nodeupgrade.EventStarted, From: "v1", To: "v2"})
	notify(ctx, nodeupgrade.Event{Kind: nodeupgrade.EventRolledBack, From: "v1", To: "v2", BackTo: "v1", Cause: "x"})
	// A second failed upgrade is news again.
	notify(ctx, nodeupgrade.Event{Kind: nodeupgrade.EventStarted, From: "v1", To: "v2"})
	notify(ctx, nodeupgrade.Event{Kind: nodeupgrade.EventRolledBack, From: "v1", To: "v2", BackTo: "v1", Cause: "x"})
	notify(ctx, nodeupgrade.Event{Kind: nodeupgrade.EventNeedsOperator, From: "v1", To: "v2", Cause: "y"})
	want := "upgrade_started/info,upgrade_failed/warning,upgrade_started/info,upgrade_failed/warning,upgrade_failed/critical"
	if got := w.kinds(); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
}

// An announced maintenance window and a running upgrade marker quiet the daemon's checker, not
// the upgrade's own events: an upgrade that fails inside the window is still told.
func TestUpgradeAlertsAreNotQuietedByAMaintenanceWindow(t *testing.T) {
	w := newWebhook(t)
	cfg := alertCfg(t, w.srv.URL)
	now := time.Now()
	paths := cfg.Paths()
	if _, err := notice.WriteMaintenance(paths, notice.Maintenance{Message: "planned", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	if err := notice.WriteUpgrade(paths, notice.Upgrade{Phase: nodeupgrade.PhaseProjects, From: "v1", To: "v2", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if m, _ := notice.ReadMaintenance(paths); m == nil || !m.Quiet(now) {
		t.Fatal("the test did not open a maintenance window")
	}
	notify := upgradeNotifier(cfg, quietLog())
	notify(context.Background(), nodeupgrade.Event{Kind: nodeupgrade.EventStarted, From: "v1", To: "v2"})
	notify(context.Background(), nodeupgrade.Event{Kind: nodeupgrade.EventRolledBack, From: "v1", To: "v2", BackTo: "v1", Cause: "x"})
	if got := w.kinds(); got != "upgrade_started/info,upgrade_failed/warning" {
		t.Fatalf("sent %s", got)
	}
}

// A webhook that is down costs the upgrade nothing but a log line.
func TestUpgradeNotifierSurvivesADeadDestination(t *testing.T) {
	w := newWebhook(t)
	w.fail = true
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	notify := upgradeNotifier(alertCfg(t, w.srv.URL), log)
	notify(context.Background(), nodeupgrade.Event{Kind: nodeupgrade.EventNeedsOperator, From: "v1", To: "v2", Cause: "x"})
	if !strings.Contains(logged.String(), "could not send the upgrade alert") {
		t.Fatalf("log: %s", logged.String())
	}
	// A node with no destination at all is not an error either.
	upgradeNotifier(alertCfg(t), quietLog())(context.Background(), nodeupgrade.Event{Kind: nodeupgrade.EventStarted, From: "v1", To: "v2"})
}

// The alert still goes when the upgrade was told to stop.
func TestUpgradeNotifierSendsAfterTheUpgradeWasCancelled(t *testing.T) {
	w := newWebhook(t)
	notify := upgradeNotifier(alertCfg(t, w.srv.URL), quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	notify(ctx, nodeupgrade.Event{Kind: nodeupgrade.EventRolledBack, From: "v1", To: "v2", BackTo: "v1", Cause: "SIGTERM"})
	if got := w.kinds(); got != "upgrade_failed/warning" {
		t.Fatalf("sent %q", got)
	}
}

func TestHaltTeeFindsTheProjectTheRolloutStoppedAt(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a rollout", "level=INFO msg=x\nsupavise: rollout halted: abcdefghijklmnopqrst failed (the new release did not start); not attempted: uvwxyzabcdefghijklmn\n", "abcdefghijklmnopqrst"},
		{"one project", "supavise: abcdefghijklmnopqrst failed: lifecycle: upgrade failed\n", "abcdefghijklmnopqrst"},
		{"no newline at the end", "supavise: rollout halted: abcdefghijklmnopqrst failed (x)", "abcdefghijklmnopqrst"},
		{"nothing", "level=ERROR msg=upgrade_failed ref=abcdefghijklmnopqrst\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			tee := &haltTee{w: &out}
			// Written in pieces, as a pipe would.
			for i := 0; i < len(tc.in); i += 7 {
				_, _ = tee.Write([]byte(tc.in[i:min(i+7, len(tc.in))]))
			}
			if got := tee.Ref(); got != tc.want {
				t.Errorf("ref = %q, want %q", got, tc.want)
			}
			if out.String() != tc.in {
				t.Errorf("the output was changed: %q", out.String())
			}
		})
	}
}
