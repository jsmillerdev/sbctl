package app

import (
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
	"github.com/jsmillerdev/supavise/internal/lifecycle"
)

func TestUpgradeEventMapping(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	for _, tc := range []struct {
		name     string
		n        lifecycle.UpgradeNotice
		kind     string
		severity string
		detail   []string
	}{
		{"started", lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeStarted, Ref: ref, Changes: "gotrue 2.195.0 -> 2.196.0"},
			alerts.KindUpgradeStarted, alerts.SeverityInfo, []string{"gotrue 2.195.0 -> 2.196.0", "offline for about a minute"}},
		{"succeeded", lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeSucceeded, Ref: ref, Changes: "gotrue 2.195.0 -> 2.196.0", Seconds: 61},
			alerts.KindUpgradeSucceeded, alerts.SeverityInfo, []string{"It took 61 seconds"}},
		{"settled", lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeSucceeded, Ref: ref, Settled: true},
			alerts.KindUpgradeSucceeded, alerts.SeverityInfo, []string{"no service changes recorded", "daemon closed the record"}},
		{"rolled back", lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeFailed, Ref: ref, ErrorCode: "5_data_upgrade_completion_failed", Cause: "gotrue did not become ready", Outcome: "rolled back to the previous versions", BackupID: 42},
			alerts.KindUpgradeFailed, alerts.SeverityWarning, []string{"gotrue did not become ready", "rolled back to the previous versions", "pre-upgrade backup is 42"}},
		{"the rollback failed too", lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeFailed, Ref: ref, ErrorCode: "8_upgrade_completion_failed", Cause: "health", Outcome: "the rollback failed too: no", BackupID: 42},
			alerts.KindUpgradeFailed, alerts.SeverityCritical, []string{"supavise projects versions " + ref, "backup 42", "supavise backups restore"}},
		{"interrupted", lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeFailed, Ref: ref, ErrorCode: "8_upgrade_completion_failed", Cause: "the process that ran the upgrade stopped", Outcome: "interrupted", Settled: true},
			alerts.KindUpgradeFailed, alerts.SeverityWarning, []string{"the process that ran the upgrade stopped", "interrupted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := upgradeEvent(tc.n)
			if ev.Kind != tc.kind || ev.Severity != tc.severity || ev.Ref != ref || !strings.Contains(ev.Title, ref) {
				t.Fatalf("event = %+v", ev)
			}
			for _, want := range tc.detail {
				if !strings.Contains(ev.Detail, want) {
					t.Errorf("detail lacks %q:\n%s", want, ev.Detail)
				}
			}
		})
	}
}

// The hook delivers on its own goroutine, after the caller's context ended, and wait lets it finish.
func TestUpgradeAlerterDelivers(t *testing.T) {
	var (
		mu   sync.Mutex
		kind []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct{ Kind, Ref string }
		_ = json.Unmarshal(b, &body)
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		kind = append(kind, body.Kind+" "+body.Ref)
		mu.Unlock()
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Alerts.Webhooks = []config.AlertWebhook{{URL: srv.URL}}
	a := newUpgradeAlerter(alerts.New(cfg, alerts.Options{}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	a.notify(ctx, lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeStarted, Ref: "aaaaaaaaaaaaaaaaaaaa"})
	a.notify(ctx, lifecycle.UpgradeNotice{Event: lifecycle.EventUpgradeFailed, Ref: "aaaaaaaaaaaaaaaaaaaa", Cause: "x"})
	cancel() // the request that started the upgrade is gone
	a.wait()
	mu.Lock()
	defer mu.Unlock()
	if len(kind) != 2 {
		t.Fatalf("delivered %v", kind)
	}
}
